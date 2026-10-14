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

func TestReadyOrderAndCache(t *testing.T) {
	var pings atomic.Int32
	pingErr := error(nil)
	var mu sync.Mutex
	s := &State{}
	p := NewProber(s, func(ctx context.Context) error {
		pings.Add(1)
		if _, ok := ctx.Deadline(); !ok {
			t.Error("ping without a deadline")
		}
		mu.Lock()
		defer mu.Unlock()
		return pingErr
	})
	now := time.Unix(1000, 0)
	p.now = func() time.Time { return now }

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
	if pings.Load() != 0 {
		t.Fatal("pinged before the flags were ready")
	}
	s.MarkStarted()
	check(true, "")
	check(true, "")
	if pings.Load() != 1 {
		t.Fatalf("%d pings within the cache window, want 1", pings.Load())
	}
	// The cached answer holds until the window passes.
	mu.Lock()
	pingErr = errors.New("database is locked")
	mu.Unlock()
	check(true, "")
	now = now.Add(readyCacheTTL)
	check(false, ReasonDB)
	if pings.Load() != 2 {
		t.Fatalf("%d pings after the window, want 2", pings.Load())
	}
	// Draining wins at once.
	s.SetDraining()
	check(false, ReasonDraining)
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
	if c := call("127.0.0.1:5000", "", nil); c != http.StatusNoContent {
		t.Errorf("loopback without token: %d", c)
	}
	if c := call("[::1]:5000", "", nil); c != http.StatusNoContent {
		t.Errorf("IPv6 loopback without token: %d", c)
	}
	for _, auth := range []string{"", "Bearer wrong", "Bearer " + token + "x", "Basic " + token, token} {
		if c := call("10.0.0.7:5000", auth, nil); c != http.StatusUnauthorized {
			t.Errorf("non-loopback with %q: %d", auth, c)
		}
	}
	if c := call("10.0.0.7:5000", "Bearer "+token, nil); c != http.StatusNoContent {
		t.Errorf("non-loopback with the token: %d", c)
	}
	// A forwarded request through a local proxy is not loopback.
	if c := call("127.0.0.1:5000", "", map[string]string{"X-Forwarded-For": "203.0.113.9"}); c != http.StatusUnauthorized {
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
		resp, err := client.Get(base + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
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
