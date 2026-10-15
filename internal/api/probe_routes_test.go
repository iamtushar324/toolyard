package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/ops"
	"github.com/tusharbhardwaj/toolyard/internal/store"
	"github.com/tusharbhardwaj/toolyard/internal/store/storetest"
)

// newProbeTestHandler is the public handler chain (security headers,
// origin check, API hardening, role guard, mux) around a Server whose
// probes read state and ping with ping.
func newProbeTestHandler(t *testing.T, state *ops.State, ping func(context.Context) error) http.Handler {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	srv := New(ctx, Options{
		SessionKey: []byte("0123456789abcdef0123456789abcdef"),
		Security:   SecurityOptions{PublicURL: "https://toolyard.example.com"},
		Probes:     ops.NewProber(state, ping),
	})
	mux := http.NewServeMux()
	srv.Routes(mux)
	var h http.Handler = srv.RoleGuard(mux)
	h = srv.HardenAPI(h)
	h = srv.EnforceOriginOnMutations(h)
	return srv.SecurityHeaders(h)
}

func probe(t *testing.T, h http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

func readyState() *ops.State {
	s := &ops.State{}
	s.MarkKeysVerified()
	s.MarkStarted()
	return s
}

func openProbeDB(t *testing.T) *store.DB {
	t.Helper()
	db := storetest.Open(t)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestLivezNeverTouchesTheDatabase(t *testing.T) {
	var pings atomic.Int32
	h := newProbeTestHandler(t, &ops.State{}, func(context.Context) error {
		pings.Add(1)
		return nil
	})
	for _, p := range []string{"/v1/livez", "/livez", "/healthz"} {
		rec := probe(t, h, http.MethodGet, p)
		if rec.Code != http.StatusOK || decodeJSON(t, rec)["status"] != "live" {
			t.Errorf("GET %s: %d %s", p, rec.Code, rec.Body.String())
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("GET %s: Content-Type %q", p, ct)
		}
		head := probe(t, h, http.MethodHead, p)
		if head.Code != http.StatusOK || head.Body.Len() != 0 {
			t.Errorf("HEAD %s: %d, %d body bytes", p, head.Code, head.Body.Len())
		}
	}
	if n := pings.Load(); n != 0 {
		t.Fatalf("livez pinged the database %d times", n)
	}
}

func TestReadyzReportsEachReason(t *testing.T) {
	db := openProbeDB(t)
	state := &ops.State{}
	h := newProbeTestHandler(t, state, db.PingContext)
	want := func(code int, status, reason string) {
		t.Helper()
		for _, p := range []string{"/v1/readyz", "/readyz"} {
			rec := probe(t, h, http.MethodGet, p)
			body := decodeJSON(t, rec)
			if rec.Code != code || body["status"] != status || (reason != "" && body["reason"] != reason) {
				t.Fatalf("GET %s: %d %s, want %d %s/%s", p, rec.Code, rec.Body.String(), code, status, reason)
			}
			if reason == "" && body["reason"] != nil {
				t.Fatalf("GET %s: ready answer carries a reason: %s", p, rec.Body.String())
			}
			if len(body) > 2 {
				t.Fatalf("GET %s: body has more than status and reason: %s", p, rec.Body.String())
			}
		}
	}
	want(http.StatusServiceUnavailable, "not_ready", "keys")
	state.MarkKeysVerified()
	want(http.StatusServiceUnavailable, "not_ready", "starting")
	state.MarkStarted()
	want(http.StatusOK, "ready", "")
	if rec := probe(t, h, http.MethodHead, "/v1/readyz"); rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatalf("HEAD /v1/readyz: %d, %d body bytes", rec.Code, rec.Body.Len())
	}
	// Draining takes effect at once, whatever the cached ping says.
	state.SetDraining()
	want(http.StatusServiceUnavailable, "not_ready", "draining")
}

func TestReadyzNotReadyWhenTheDatabaseIsClosed(t *testing.T) {
	db := openProbeDB(t)
	h := newProbeTestHandler(t, readyState(), db.PingContext)
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	rec := probe(t, h, http.MethodGet, "/v1/readyz")
	body := decodeJSON(t, rec)
	if rec.Code != http.StatusServiceUnavailable || body["status"] != "not_ready" || body["reason"] != "db" {
		t.Fatalf("closed db: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "closed") || strings.Contains(rec.Body.String(), "sql") {
		t.Fatalf("readyz leaks the database error: %s", rec.Body.String())
	}
}

func TestReadyzWithoutAPingerIsNeverReady(t *testing.T) {
	h := newProbeTestHandler(t, readyState(), nil)
	rec := probe(t, h, http.MethodGet, "/v1/readyz")
	if rec.Code != http.StatusServiceUnavailable || decodeJSON(t, rec)["reason"] != "db" {
		t.Fatalf("no pinger: %d %s", rec.Code, rec.Body.String())
	}
}

func TestReadyzCachesTheDatabasePing(t *testing.T) {
	var pings atomic.Int32
	h := newProbeTestHandler(t, readyState(), func(context.Context) error {
		pings.Add(1)
		return nil
	})
	for i := 0; i < 50; i++ {
		if rec := probe(t, h, http.MethodGet, "/v1/readyz"); rec.Code != http.StatusOK {
			t.Fatalf("request %d: %d %s", i, rec.Code, rec.Body.String())
		}
	}
	if n := pings.Load(); n != 1 {
		t.Fatalf("50 probes within the cache window pinged %d times, want 1", n)
	}
}

func TestProbeRoutesNeverServeHTML(t *testing.T) {
	db := openProbeDB(t)
	h := newProbeTestHandler(t, readyState(), db.PingContext)
	for _, p := range []string{"/v1/livez", "/v1/readyz", "/livez", "/readyz", "/healthz"} {
		for _, m := range []string{http.MethodGet, http.MethodHead, http.MethodPost} {
			rec := probe(t, h, m, p)
			if ct := rec.Header().Get("Content-Type"); strings.Contains(ct, "text/html") || strings.Contains(rec.Body.String(), "<html") {
				t.Errorf("%s %s served HTML (%s)", m, p, ct)
			}
		}
	}
}

// Probes skip the role guard, the operator-token guard, the CSRF check and
// the rate limits for GET and HEAD only.
func TestProbeRoutesSkipGuardsForReadsOnly(t *testing.T) {
	e := newAccessTestServer(t)
	m := e.member(t, "user_m", "m@beknown.work")
	memberCookie := e.cookieFor(t, m.ID)
	call := func(method, path string, mod func(*http.Request)) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		if mod != nil {
			mod(req)
		}
		rec := httptest.NewRecorder()
		e.handler.ServeHTTP(rec, req)
		return rec
	}
	withMember := func(r *http.Request) { r.AddCookie(memberCookie) }
	withOperator := func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+identity.OperatorTokenPrefix+"bogus") }

	for _, mod := range []func(*http.Request){nil, withMember, withOperator} {
		if rec := call(http.MethodGet, "/v1/livez", mod); rec.Code != http.StatusOK {
			t.Errorf("GET /v1/livez: %d %s", rec.Code, rec.Body.String())
		}
		// This server's probes have no pinger: not ready, but answered
		// by the probe, not refused by a guard.
		rec := call(http.MethodGet, "/v1/readyz", mod)
		if rec.Code != http.StatusServiceUnavailable || decodeJSON(t, rec)["status"] != "not_ready" {
			t.Errorf("GET /v1/readyz: %d %s", rec.Code, rec.Body.String())
		}
	}
	// Other methods get no exemption: without the CSRF header HardenAPI
	// refuses, and with it a member is refused by the role guard.
	if rec := call(http.MethodPost, "/v1/livez", withMember); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "X-Requested-With") {
		t.Errorf("member POST /v1/livez without the CSRF header: %d %s", rec.Code, rec.Body.String())
	}
	withMemberCSRF := func(r *http.Request) {
		withMember(r)
		r.Header.Set("X-Requested-With", "toolyard")
	}
	if rec := call(http.MethodPost, "/v1/livez", withMemberCSRF); rec.Code != http.StatusForbidden || decodeJSON(t, rec)["error"] != "admin_only" {
		t.Errorf("member POST /v1/livez: %d %s", rec.Code, rec.Body.String())
	}
	// No rate limit: an orchestrator polling hard is never refused.
	for i := 0; i < 300; i++ {
		if rec := call(http.MethodGet, "/v1/livez", nil); rec.Code != http.StatusOK {
			t.Fatalf("poll %d: %d %s", i, rec.Code, rec.Body.String())
		}
	}
}

func TestHealthBodyUnchangedByProbes(t *testing.T) {
	e := newAccessTestServer(t)
	rec := e.do(t, nil, http.MethodGet, "/v1/health", "")
	body := decodeJSON(t, rec)
	if rec.Code != http.StatusOK || body["ok"] != true || body["service"] != "toolyard" {
		t.Fatalf("/v1/health: %d %s", rec.Code, rec.Body.String())
	}
	if _, ok := body["status"]; ok {
		t.Fatalf("/v1/health gained a probe status field: %s", rec.Body.String())
	}
}
