package ops

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestStateFlags(t *testing.T) {
	s := &State{}
	if s.Draining() || s.Started() || s.KeysVerified() {
		t.Fatal("zero State is not all false")
	}
	s.MarkKeysVerified()
	s.MarkStarted()
	s.SetDraining()
	if !s.Draining() || !s.Started() || !s.KeysVerified() {
		t.Fatal("flags did not stick")
	}
	if Default == nil {
		t.Fatal("Default is nil")
	}
}

// waitPing waits for the Prober's running ping, if any.
func (p *Prober) waitPing() {
	p.mu.Lock()
	done := p.inflight
	p.mu.Unlock()
	if done != nil {
		<-done
	}
}

// fakeDB is a database ping whose answer a test sets, on a clock the test
// moves.
type fakeDB struct {
	mu    sync.Mutex
	err   error
	clock time.Time
	pings atomic.Int32
	t     *testing.T
}

func (f *fakeDB) ping(ctx context.Context) error {
	f.pings.Add(1)
	if dl, ok := ctx.Deadline(); !ok || time.Until(dl) > dbPingTimeout || time.Until(dl) < dbPingTimeout-time.Second {
		f.t.Errorf("ping deadline %v (set %v), want about %s", dl, ok, dbPingTimeout)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}

func (f *fakeDB) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.clock
}

func (f *fakeDB) set(err error) {
	f.mu.Lock()
	f.err = err
	f.mu.Unlock()
}

func (f *fakeDB) advance(d time.Duration) {
	f.mu.Lock()
	f.clock = f.clock.Add(d)
	f.mu.Unlock()
}

func newFakeProber(t *testing.T, s *State) (*Prober, *fakeDB) {
	f := &fakeDB{clock: time.Unix(1000, 0), t: t}
	p := NewProber(s, f.ping)
	p.now = f.now
	return p, f
}

func TestReadyOrderAndCache(t *testing.T) {
	s := &State{}
	p, db := newFakeProber(t, s)
	check := func(wantOK bool, wantReason string) {
		t.Helper()
		ok, reason := p.Ready()
		if ok != wantOK || reason != wantReason {
			t.Fatalf("Ready() = %v %q, want %v %q", ok, reason, wantOK, wantReason)
		}
	}
	check(false, ReasonKeys)
	s.MarkKeysVerified()
	check(false, ReasonStarting)
	if db.pings.Load() != 0 {
		t.Fatal("pinged before the flags were ready")
	}
	s.MarkStarted()
	check(true, "") // the first check waits for its ping
	check(true, "")
	check(true, "")
	if n := db.pings.Load(); n != 1 {
		t.Fatalf("%d pings within the cache window, want 1", n)
	}
	// Draining wins at once.
	s.SetDraining()
	check(false, ReasonDraining)
}

// A busy database (the single connection held by a long query) fails one
// ping: readiness holds. Only two failed pings in a row report "db", and
// one good ping brings it back.
func TestReadyToleratesOneFailedPing(t *testing.T) {
	s := &State{}
	s.MarkKeysVerified()
	s.MarkStarted()
	p, db := newFakeProber(t, s)
	check := func(wantOK bool) {
		t.Helper()
		ok, reason := p.Ready()
		if ok != wantOK || (!ok && reason != ReasonDB) {
			t.Fatalf("Ready() = %v %q, want %v", ok, reason, wantOK)
		}
	}
	// next moves past the cache window, lets the probe start a ping in
	// the background (answering from the old results), and waits for it.
	next := func(wantNow bool) {
		t.Helper()
		db.advance(readyCacheTTL)
		check(wantNow)
		p.waitPing()
	}
	check(true)
	db.set(errors.New("context deadline exceeded"))
	next(true)
	check(true) // one failure: still ready
	next(true)
	check(false) // two in a row: down
	if n := db.pings.Load(); n != 3 {
		t.Fatalf("%d pings, want 3", n)
	}
	db.set(nil)
	next(false)
	check(true) // one good ping: back
	// A probe never waits for a background ping.
	db.set(errors.New("busy"))
	db.advance(readyCacheTTL)
	check(true)
	p.waitPing()
}

func TestReadyIsDownWhenTheFirstPingFails(t *testing.T) {
	s := &State{}
	s.MarkKeysVerified()
	s.MarkStarted()
	p, db := newFakeProber(t, s)
	db.set(errors.New("database is closed"))
	if ok, reason := p.Ready(); ok || reason != ReasonDB {
		t.Fatalf("no successful ping yet: %v %q", ok, reason)
	}
}

func TestConcurrentProbesShareOnePing(t *testing.T) {
	s := &State{}
	s.MarkKeysVerified()
	s.MarkStarted()
	p, db := newFakeProber(t, s)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, _ := p.Ready(); !ok {
				t.Error("not ready")
			}
		}()
	}
	wg.Wait()
	if n := db.pings.Load(); n != 1 {
		t.Fatalf("%d pings for 20 concurrent first probes, want 1", n)
	}
}

func TestProbeHandlers(t *testing.T) {
	s := &State{}
	s.MarkKeysVerified()
	s.MarkStarted()
	p := NewProber(s, func(context.Context) error { return nil })
	mux := http.NewServeMux()
	p.Register(mux)
	do := func(method, path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
		return rec
	}
	for path, want := range map[string]string{
		"/v1/livez": `{"status":"live"}`, "/livez": `{"status":"live"}`, "/healthz": `{"status":"live"}`,
		"/v1/readyz": `{"status":"ready"}`, "/readyz": `{"status":"ready"}`,
	} {
		rec := do(http.MethodGet, path)
		if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != want {
			t.Errorf("GET %s: %d %s", path, rec.Code, rec.Body.String())
		}
		if rec.Header().Get("Content-Type") != "application/json" || rec.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("GET %s: headers %v", path, rec.Header())
		}
		if head := do(http.MethodHead, path); head.Code != http.StatusOK || head.Body.Len() != 0 {
			t.Errorf("HEAD %s: %d with %d body bytes", path, head.Code, head.Body.Len())
		}
		if post := do(http.MethodPost, path); post.Code != http.StatusMethodNotAllowed || post.Header().Get("Allow") != "GET, HEAD" {
			t.Errorf("POST %s: %d allow %q", path, post.Code, post.Header().Get("Allow"))
		}
		if !IsProbePath(path) {
			t.Errorf("IsProbePath(%s) = false", path)
		}
	}
	s.SetDraining()
	rec := do(http.MethodGet, "/v1/readyz")
	if rec.Code != http.StatusServiceUnavailable || strings.TrimSpace(rec.Body.String()) != `{"status":"not_ready","reason":"draining"}` {
		t.Fatalf("draining: %d %s", rec.Code, rec.Body.String())
	}
	for _, p := range []string{"/v1/health", "/v1/livez/", "/metrics", "/v1/readyz/x"} {
		if IsProbePath(p) {
			t.Errorf("IsProbePath(%s) = true", p)
		}
	}
}

func TestNilPingIsNeverReady(t *testing.T) {
	s := &State{}
	s.MarkKeysVerified()
	s.MarkStarted()
	if ok, reason := NewProber(s, nil).Ready(); ok || reason != ReasonDB {
		t.Fatalf("nil ping: %v %q", ok, reason)
	}
	if NewProber(nil, nil).state != Default {
		t.Fatal("nil state is not Default")
	}
}

func TestPrivateHost(t *testing.T) {
	for host, want := range map[string]bool{
		"127.0.0.1": true, "::1": true, "localhost": true, "LOCALHOST": true,
		"10.1.2.3": true, "172.16.0.9": true, "192.168.1.1": true, "fd00::1": true,
		"100.64.0.1": true, "100.127.255.254": true, "169.254.1.1": true, "fe80::1%eth0": true,
		"": false, "0.0.0.0": false, "::": false, "8.8.8.8": false, "100.128.0.1": false,
		"2001:4860::8888": false, "example.com": false, "metrics.internal": false,
	} {
		if got := PrivateHost(host); got != want {
			t.Errorf("PrivateHost(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestCheckAddrRefusesPublicWithoutToken(t *testing.T) {
	for _, addr := range []string{":9090", "0.0.0.0:9090", "[::]:9090", "203.0.113.5:9090", "example.com:9090"} {
		if err := CheckAddr(addr, false); err == nil {
			t.Errorf("CheckAddr(%s) without a token accepted", addr)
		}
		if err := CheckAddr(addr, true); err != nil {
			t.Errorf("CheckAddr(%s) with a token: %v", addr, err)
		}
	}
	for _, addr := range []string{"127.0.0.1:9090", "[::1]:9090", "localhost:9090", "10.0.0.5:9090"} {
		if err := CheckAddr(addr, false); err != nil {
			t.Errorf("CheckAddr(%s): %v", addr, err)
		}
	}
	for _, addr := range []string{"127.0.0.1", "127.0.0.1:", "nonsense"} {
		if err := CheckAddr(addr, true); err == nil {
			t.Errorf("CheckAddr(%q) accepted a malformed address", addr)
		}
	}
}

func TestListenRefusesPublicAddressWithoutToken(t *testing.T) {
	srvs, err := Listen(ListenerConfig{Addrs: "127.0.0.1:0, 0.0.0.0:0", Handler: http.NotFoundHandler(), Logf: t.Logf})
	if err == nil || srvs != nil {
		t.Fatalf("Listen bound a public address without a token: %v", err)
	}
	if srvs, err := Listen(ListenerConfig{Logf: t.Logf}); err != nil || srvs != nil {
		t.Fatalf("empty -ops-addr: %v %v", srvs, err)
	}
	if _, err := Listen(ListenerConfig{TokenFile: "/x", Logf: t.Logf}); err == nil {
		t.Fatal("-ops-token-file without -ops-addr accepted")
	}
}

func TestReadToken(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) string {
		p := filepath.Join(dir, "token")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if tok, err := ReadToken(""); err != nil || tok != "" {
		t.Fatalf("no file: %q %v", tok, err)
	}
	if tok, err := ReadToken(write("  0123456789abcdef-token\n")); err != nil || tok != "0123456789abcdef-token" {
		t.Fatalf("token: %q %v", tok, err)
	}
	for _, body := range []string{"", " \n", "abc123"} {
		p := write(body)
		if _, err := ReadToken(p); err == nil {
			t.Errorf("token %q accepted", body)
		} else if body != "" && strings.Contains(err.Error(), "abc123") {
			t.Errorf("error shows the token: %v", err)
		}
	}
	if _, err := ReadToken(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing token file accepted")
	}
}

func TestRequireToken(t *testing.T) {
	const token = "0123456789abcdef-secret"
	h := RequireToken(token, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	call := func(remote, auth string, hdr map[string]string) int {
		req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
		req.RemoteAddr = remote
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	// Every peer needs the token, loopback included: a relay on the same
	// host would otherwise let any remote caller in.
	for _, peer := range []string{"127.0.0.1:5000", "[::1]:5000", "10.0.0.7:5000", "203.0.113.9:5000"} {
		for _, auth := range []string{"", "Bearer wrong", "Bearer " + token + "x", "Basic " + token, token} {
			if c := call(peer, auth, nil); c != http.StatusUnauthorized {
				t.Errorf("%s with %q: %d", peer, auth, c)
			}
		}
		if c := call(peer, "Bearer "+token, nil); c != http.StatusNoContent {
			t.Errorf("%s with the token: %d", peer, c)
		}
	}
	// Proxy headers change nothing.
	if c := call("127.0.0.1:5000", "", map[string]string{"X-Forwarded-For": "127.0.0.1"}); c != http.StatusUnauthorized {
		t.Errorf("forwarded loopback without token: %d", c)
	}
	// No token: the handler is returned as is.
	plain := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	if got := RequireToken("", plain); got == nil {
		t.Fatal("RequireToken without a token returned nil")
	}
}

func TestListenServesTheOpsMux(t *testing.T) {
	s := &State{}
	s.MarkKeysVerified()
	s.MarkStarted()
	p := NewProber(s, func(context.Context) error { return nil })
	metrics := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "toolyard_up 1\n")
	})
	tokenFile := filepath.Join(t.TempDir(), "ops.token")
	if err := os.WriteFile(tokenFile, []byte("0123456789abcdef-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	srvs, err := Listen(ListenerConfig{Addrs: "127.0.0.1:0", TokenFile: tokenFile, Handler: Mux(metrics, p), Logf: t.Logf})
	if err != nil || len(srvs) != 1 {
		t.Fatalf("listen: %v %v", srvs, err)
	}
	t.Cleanup(func() { _ = srvs[0].Close() })
	if strings.HasSuffix(srvs[0].Addr, ":0") {
		t.Fatalf("server Addr %q is not the bound address", srvs[0].Addr)
	}
	base := "http://" + srvs[0].Addr
	client := &http.Client{Timeout: 3 * time.Second}
	get := func(path string) (int, string) {
		req, err := http.NewRequest(http.MethodGet, base+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer 0123456789abcdef-secret")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	// Loopback without the token is refused when a token is set.
	resp, err := client.Get(base + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("loopback /metrics without the token: %d", resp.StatusCode)
	}
	if code, body := get("/metrics"); code != 200 || body != "toolyard_up 1\n" {
		t.Errorf("/metrics: %d %q", code, body)
	}
	if code, body := get("/v1/readyz"); code != 200 || !strings.Contains(body, `"ready"`) {
		t.Errorf("/v1/readyz: %d %q", code, body)
	}
	code, body := get("/v1/health")
	var out map[string]string
	if code != 404 || json.Unmarshal([]byte(body), &out) != nil || out["error"] != "not_found" {
		t.Errorf("/v1/health on the ops listener: %d %q", code, body)
	}
}
