package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/inbox"
)

func uploadTestPNG(seed string) []byte {
	return append([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"), []byte("upload route test "+seed+strings.Repeat("q", 40))...)
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// uploadStack is the production middleware order around the mux (minus
// security headers and Origin enforcement, which need a public URL).
func (f *inboxAPIFixture) uploadStack() http.Handler {
	var h http.Handler = f.srv.RoleGuard(f.mux)
	h = LimitBodyByPath(h, 4<<20, map[string]int64{"/v1/uploads/": inbox.MaxVideoBytes})
	return f.srv.HardenAPI(h)
}

func (f *inboxAPIFixture) put(t *testing.T, path, ct string, body []byte, mod func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, path, bytes.NewReader(body))
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	if mod != nil {
		mod(req)
	}
	rec := httptest.NewRecorder()
	f.uploadStack().ServeHTTP(rec, req)
	return rec
}

// The whole flow as an agent sees it: inbox.upload, a plain curl-style PUT
// (no cookie, no X-Requested-With, a non-JSON body), then an Inbox post
// citing the ref, which the owner can open.
func TestInboxUploadEndToEnd(t *testing.T) {
	f := newInboxAPIFixture(t)
	data := uploadTestPNG("e2e")
	sum := sha256Hex(data)
	sc := f.agentRun(t, "inbox.upload", map[string]any{"name": "after.png", "content_type": "image/png", "size": len(data), "sha256": sum})
	uploadURL, _ := sc["upload_url"].(string)
	if !strings.HasPrefix(uploadURL, "/v1/uploads/tyu_") || sc["ref"] != "upload:"+sum || sc["method"] != "PUT" {
		t.Fatalf("inbox.upload: %v", sc)
	}
	if curl, _ := sc["curl"].(string); !strings.Contains(curl, "--data-binary") || !strings.Contains(curl, uploadURL) {
		t.Fatalf("curl hint: %v", sc["curl"])
	}

	rec := f.put(t, uploadURL, "image/png", data, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["ref"] != "upload:"+sum || out["content_type"] != "image/png" {
		t.Fatalf("PUT body: %v", out)
	}
	if rec := f.put(t, uploadURL, "image/png", data, nil); rec.Code != http.StatusGone {
		t.Fatalf("replayed PUT: %d %s, want 410", rec.Code, rec.Body.String())
	}

	again := f.agentRun(t, "inbox.upload", map[string]any{"name": "after.png", "content_type": "image/png", "size": len(data), "sha256": sum})
	if again["already_uploaded"] != true || again["upload_url"] != nil {
		t.Fatalf("second inbox.upload for the same bytes: %v", again)
	}

	post := f.agentRun(t, "inbox.post", map[string]any{
		"title": "Checkout fix is live", "summary": "Shipped; screenshot attached.", "message": "I shipped the checkout fix. The screenshot shows the page after the deploy.",
		"audio": map[string]any{"script": "The checkout fix is live."}, "urgency": "fyi",
		"attachments": []any{map[string]any{"type": "image", "upload": "upload:" + sum, "alt": "Checkout page after the fix"}},
	})
	id, _ := post["request_id"].(string)
	if post["ok"] != true || id == "" {
		t.Fatalf("inbox.post with an upload: %v", post)
	}
	f.svc.Flush()
	code, one := f.owner(t, "GET", "/v1/inbox/"+id, nil)
	att := one["request"].(map[string]any)["attachments"].([]any)[0].(map[string]any)
	if code != 200 || att["blob"] != sum || att["content_type"] != "image/png" {
		t.Fatalf("owner view: %d %v", code, att)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/inbox/blobs/"+sum, nil)
	req.AddCookie(f.cookie)
	brec := httptest.NewRecorder()
	f.mux.ServeHTTP(brec, req)
	if brec.Code != http.StatusOK || !bytes.Equal(brec.Body.Bytes(), data) || brec.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("owner download: %d %s", brec.Code, brec.Header().Get("Content-Type"))
	}

	evs, err := audit.New(f.db).Query(context.Background(), audit.Filter{AgentID: f.agent, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range evs {
		if e.EventType == EventInboxUpload && strings.Contains(string(e.Arguments), sum[:16]) && strings.Contains(string(e.Arguments), "image/png") {
			found = true
		}
	}
	if !found {
		t.Fatal("no inbox.upload audit event")
	}
}

func TestInboxUploadRouteStatusCodes(t *testing.T) {
	f := newInboxAPIFixture(t)
	ctx := context.Background()
	data := uploadTestPNG("codes")
	ticket := func(spec inbox.UploadSpec) string {
		t.Helper()
		tk, err := f.svc.CreateUpload(ctx, f.agent, spec)
		if err != nil {
			t.Fatal(err)
		}
		return "/v1/uploads/" + tk.Ticket
	}
	spec := inbox.UploadSpec{Name: "a.png", ContentType: "image/png", Size: int64(len(data)), SHA256: sha256Hex(data)}

	if rec := f.put(t, "/v1/uploads/tyu_"+strings.Repeat("A", 43), "image/png", data, nil); rec.Code != http.StatusNotFound {
		t.Errorf("unknown ticket: %d", rec.Code)
	}
	if rec := f.put(t, "/v1/uploads/", "image/png", data, nil); rec.Code != http.StatusNotFound {
		t.Errorf("no ticket: %d", rec.Code)
	}
	path := ticket(spec)
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	f.uploadStack().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: %d", rec.Code)
	}
	if rec := f.put(t, path, "image/png", data[:len(data)-2], nil); rec.Code != http.StatusBadRequest {
		t.Errorf("short body: %d %s", rec.Code, rec.Body.String())
	}
	// Content-Length says one size, the body is longer: MaxBytesReader stops it.
	if rec := f.put(t, path, "image/png", append(append([]byte{}, data...), 'x'), func(r *http.Request) { r.ContentLength = -1 }); rec.Code != http.StatusBadRequest {
		t.Errorf("long body: %d %s", rec.Code, rec.Body.String())
	}
	html := []byte("<html><script>alert(1)</script></html>")
	hpath := ticket(inbox.UploadSpec{Name: "x.png", ContentType: "image/png", Size: int64(len(html)), SHA256: sha256Hex(html)})
	if rec := f.put(t, hpath, "image/png", html, nil); rec.Code != http.StatusBadRequest {
		t.Errorf("html as png: %d %s", rec.Code, rec.Body.String())
	}
	other := uploadTestPNG("other")
	if rec := f.put(t, path, "image/png", other[:len(data)], nil); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "sha256") {
		t.Errorf("hash mismatch: %d %s", rec.Code, rec.Body.String())
	}
	// Failed attempts don't use the ticket; the right bytes still work once.
	if rec := f.put(t, path, "image/png", data, nil); rec.Code != http.StatusCreated {
		t.Errorf("good PUT after failures: %d %s", rec.Code, rec.Body.String())
	}

	// An operator token can never redeem a ticket, whatever its scopes.
	owner, err := f.srv.identity.PrimaryUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	opTok, _, err := f.srv.identity.CreateOperatorToken(ctx, owner.ID, "uploader", []string{identity.ScopeRead, identity.ScopeWrite, identity.ScopeOwner}, 0, "test")
	if err != nil {
		t.Fatal(err)
	}
	opPath := ticket(inbox.UploadSpec{Name: "b.png", ContentType: "image/png", Size: int64(len(other)), SHA256: sha256Hex(other)})
	if rec := f.put(t, opPath, "image/png", other, func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+opTok) }); rec.Code != http.StatusForbidden {
		t.Errorf("operator token PUT: %d %s, want 403", rec.Code, rec.Body.String())
	}
	// The owner's cookie riding along changes nothing: the ticket decides.
	if rec := f.put(t, opPath, "image/png", other, func(r *http.Request) { r.AddCookie(f.cookie) }); rec.Code != http.StatusCreated {
		t.Errorf("PUT with a cookie: %d %s", rec.Code, rec.Body.String())
	}
}

func TestUploadRouteGuards(t *testing.T) {
	if !memberAllowed(http.MethodPut, "/v1/uploads/tyu_abc") {
		t.Error("a member's cookie must not block an upload PUT")
	}
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/v1/uploads/tyu_abc"}, {http.MethodPost, "/v1/uploads/tyu_abc"},
		{http.MethodPut, "/v1/uploads/"}, {http.MethodPut, "/v1/uploads/a/b"},
	} {
		if memberAllowed(c.method, c.path) {
			t.Errorf("member %s %s should be admin_only", c.method, c.path)
		}
	}
	if _, ok := operatorScopeFor(http.MethodPut, "/v1/uploads/tyu_abc"); ok {
		t.Error("operator tokens must never reach /v1/uploads/")
	}
	if !exemptFromCSRFHeader("/v1/uploads/tyu_abc") || !exemptFromOriginCheck("/v1/uploads/tyu_abc") {
		t.Error("an upload PUT comes from curl: no CSRF header or Origin")
	}
	if perRouteBodyCap("/v1/uploads/tyu_abc") != inbox.MaxVideoBytes {
		t.Error("upload body cap should allow a 50 MB video")
	}
	if unauthRouteLimit("/v1/uploads/tyu_abc") == nil {
		t.Error("upload PUTs need a per-IP rate limit")
	}
}

// Two PUTs on one ticket: the second, arriving while the first is still
// reading its body, is refused at once (409), and exactly one stores.
func TestInboxUploadParallelPUTs(t *testing.T) {
	f := newInboxAPIFixture(t)
	ctx := context.Background()
	data := uploadTestPNG("parallel")
	tk, err := f.svc.CreateUpload(ctx, f.agent, inbox.UploadSpec{Name: "p.png", ContentType: "image/png", Size: int64(len(data)), SHA256: sha256Hex(data)})
	if err != nil {
		t.Fatal(err)
	}
	path := "/v1/uploads/" + tk.Ticket
	pr, pw := io.Pipe()
	first := make(chan int, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPut, path, pr)
		req.Header.Set("Content-Type", "image/png")
		rec := httptest.NewRecorder()
		f.uploadStack().ServeHTTP(rec, req)
		first <- rec.Code
	}()
	// Wait until the first PUT holds the claim.
	deadline := time.Now().Add(10 * time.Second)
	for {
		var claimed int
		_ = f.db.QueryRow(`SELECT COUNT(*) FROM inbox_upload_tickets WHERE claimed_until IS NOT NULL`).Scan(&claimed)
		if claimed == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the first PUT never claimed the ticket")
		}
		time.Sleep(5 * time.Millisecond)
	}
	second := f.put(t, path, "image/png", data, nil)
	if _, err := pw.Write(data); err != nil {
		t.Fatal(err)
	}
	_ = pw.Close()
	code := <-first
	if code != http.StatusCreated || second.Code != http.StatusConflict {
		t.Fatalf("first %d, second %d %s; want 201 and 409", code, second.Code, second.Body.String())
	}
	if again := f.put(t, path, "image/png", data, nil); again.Code != http.StatusGone {
		t.Fatalf("PUT after success: %d, want 410", again.Code)
	}
}

// When every upload slot is busy, a PUT is refused with 503 and
// Retry-After, and the ticket stays usable.
func TestInboxUploadConcurrencyCap(t *testing.T) {
	f := newInboxAPIFixture(t)
	ctx := context.Background()
	data := uploadTestPNG("capped")
	tk, err := f.svc.CreateUpload(ctx, f.agent, inbox.UploadSpec{Name: "c.png", ContentType: "image/png", Size: int64(len(data)), SHA256: sha256Hex(data)})
	if err != nil {
		t.Fatal(err)
	}
	var held []func()
	for i := 0; i < inbox.MaxConcurrentUploads; i++ {
		rel, ok := f.svc.AcquireUploadSlot()
		if !ok {
			t.Fatalf("slot %d", i)
		}
		held = append(held, rel)
	}
	rec := f.put(t, "/v1/uploads/"+tk.Ticket, "image/png", data, nil)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("PUT with every slot busy: %d %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	for _, rel := range held {
		rel()
	}
	if rec := f.put(t, "/v1/uploads/"+tk.Ticket, "image/png", data, nil); rec.Code != http.StatusCreated {
		t.Fatalf("PUT after slots free up: %d %s", rec.Code, rec.Body.String())
	}
}

// Behind a trusted proxy the client is the rightmost X-Forwarded-For hop
// that isn't a trusted proxy, so a client-set header can't pick its own IP
// (and dodge per-IP limits such as the upload limit).
func TestClientIPUsesRightmostUntrustedHop(t *testing.T) {
	_, trusted, _ := net.ParseCIDR("10.0.0.0/8")
	_, loop, _ := net.ParseCIDR("127.0.0.1/32")
	o := SecurityOptions{TrustedProxies: []*net.IPNet{trusted, loop}}
	cases := []struct {
		peer string
		xff  []string
		want string
	}{
		{"10.0.0.7:1", []string{"203.0.113.9"}, "203.0.113.9"},
		{"10.0.0.7:1", []string{"6.6.6.6, 203.0.113.9"}, "203.0.113.9"},
		{"10.0.0.7:1", []string{"127.0.0.1, 203.0.113.9, 10.0.0.5"}, "203.0.113.9"},
		{"10.0.0.7:1", []string{"6.6.6.6", "203.0.113.9"}, "203.0.113.9"},
		{"10.0.0.7:1", []string{"10.0.0.1, 10.0.0.5"}, "10.0.0.1"},
		{"10.0.0.7:1", []string{"1.2.3.4, not-an-ip"}, ""},
		{"10.0.0.7:1", nil, "10.0.0.7"},
		{"198.51.100.4:1", []string{"1.2.3.4"}, "198.51.100.4"},
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
		r.RemoteAddr = c.peer
		for _, v := range c.xff {
			r.Header.Add("X-Forwarded-For", v)
		}
		if got := o.ClientIP(r); got != c.want {
			t.Errorf("peer %s xff %q: got %s, want %s", c.peer, c.xff, got, c.want)
		}
	}
}

// /debug/pprof is gated on the TCP peer itself: production trusts a docker
// range (172.23.0.0/16) where other containers live, so a forwarded
// 127.0.0.1 from any of them must not count as loopback.
func TestDirectLoopbackIgnoresForwardedHeaders(t *testing.T) {
	_, dockerNet, _ := net.ParseCIDR("172.23.0.0/16")
	o := SecurityOptions{TrustedProxies: []*net.IPNet{dockerNet}}
	cases := []struct {
		peer    string
		headers map[string]string
		want    bool
	}{
		{"127.0.0.1:5000", nil, true},
		{"[::1]:5000", nil, true},
		{"172.23.0.9:5000", map[string]string{"X-Forwarded-For": "127.0.0.1"}, false},
		{"172.23.0.9:5000", nil, false},
		{"127.0.0.1:5000", map[string]string{"X-Forwarded-For": "203.0.113.9"}, false},
		{"127.0.0.1:5000", map[string]string{"X-Forwarded-For": "127.0.0.1"}, false},
		{"127.0.0.1:5000", map[string]string{"Forwarded": "for=203.0.113.9"}, false},
		{"127.0.0.1:5000", map[string]string{"X-Real-IP": "203.0.113.9"}, false},
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodGet, "/debug/pprof/", nil)
		r.RemoteAddr = c.peer
		for k, v := range c.headers {
			r.Header.Set(k, v)
		}
		if got := DirectLoopback(r); got != c.want {
			t.Errorf("peer %s headers %v: DirectLoopback %v, want %v", c.peer, c.headers, got, c.want)
		}
		if got := o.IsLoopback(r); got != c.want {
			t.Errorf("peer %s headers %v: IsLoopback %v, want %v", c.peer, c.headers, got, c.want)
		}
	}
}

// One agent may read at most two upload bodies at once; a third PUT gets
// 503 and its ticket stays usable.
func TestInboxUploadPerAgentCap(t *testing.T) {
	f := newInboxAPIFixture(t)
	ctx := context.Background()
	data := uploadTestPNG("peragent")
	tk, err := f.svc.CreateUpload(ctx, f.agent, inbox.UploadSpec{Name: "a.png", ContentType: "image/png", Size: int64(len(data)), SHA256: sha256Hex(data)})
	if err != nil {
		t.Fatal(err)
	}
	var held []func()
	for i := 0; i < inbox.MaxConcurrentUploadsPerAgent; i++ {
		rel, ok := f.svc.AcquireAgentUploadSlot(f.agent)
		if !ok {
			t.Fatalf("agent slot %d", i)
		}
		held = append(held, rel)
	}
	rec := f.put(t, "/v1/uploads/"+tk.Ticket, "image/png", data, nil)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("third body for one agent: %d %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	for _, rel := range held {
		rel()
	}
	if rec := f.put(t, "/v1/uploads/"+tk.Ticket, "image/png", data, nil); rec.Code != http.StatusCreated {
		t.Fatalf("PUT after the agent's slots free up: %d %s", rec.Code, rec.Body.String())
	}
}
