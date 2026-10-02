package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The /login document is the one page that loads Clerk, so it alone gets
// the Clerk-compatible CSP; every other path keeps the strict policy, and
// with Clerk off even /login stays strict.
func TestSecurityHeadersClerkCSPOnLoginOnly(t *testing.T) {
	cspFor := func(s *Server, path string) string {
		h := s.SecurityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec.Header().Get("Content-Security-Policy")
	}

	on := &Server{security: SecurityOptions{ClerkFrontendAPI: "clerk.example.test"}}
	for _, path := range []string{"/login", "/login.html"} {
		csp := cspFor(on, path)
		for _, want := range []string{
			"script-src 'self' https://clerk.example.test https://challenges.cloudflare.com",
			"connect-src 'self' wss: ws: https://clerk.example.test https://*.protect.clerk.com:*",
			"img-src 'self' data: https://img.clerk.com",
			"style-src 'self' 'unsafe-inline'",
			"frame-src https://challenges.cloudflare.com",
			"worker-src 'self' blob:",
			"frame-ancestors 'none'",
			"form-action 'self'",
			"base-uri 'self'",
		} {
			if !strings.Contains(csp, want) {
				t.Errorf("%s CSP missing %q: %s", path, want, csp)
			}
		}
	}
	for _, path := range []string{"/", "/index.html", "/app.js", "/login/", "/login/callback", "/v1/auth/me", "/mcp"} {
		if csp := cspFor(on, path); csp != strictCSP {
			t.Errorf("%s CSP = %s, want the strict policy", path, csp)
		}
	}
	off := &Server{}
	if csp := cspFor(off, "/login"); csp != strictCSP {
		t.Errorf("/login with Clerk off: %s", csp)
	}
	// The dashboard shows Clerk avatars, so img.clerk.com is on the strict
	// policy too — but only as an image source.
	if !strings.Contains(strictCSP, "img-src 'self' data: https://img.clerk.com;") {
		t.Errorf("strict CSP lacks the avatar host: %s", strictCSP)
	}
	for _, leak := range []string{"unsafe-inline", "challenges.cloudflare.com", "protect.clerk.com", "frame-src", "script-src 'self' https"} {
		if strings.Contains(strictCSP, leak) {
			t.Errorf("strict CSP leaked a Clerk allowance %q: %s", leak, strictCSP)
		}
	}
}

// With -public-url set, agent-side routes (called by the toolyard CLI, hooks
// and webhooks, which send no Origin) must not be refused by the Origin
// check, while cookie-authenticated dashboard routes and the login/setup
// forms still are.
func TestEnforceOriginOnMutationsAgentRoutes(t *testing.T) {
	const public = "https://toolyard.example.com"
	s := &Server{security: SecurityOptions{PublicURL: public}}
	h := s.EnforceOriginOnMutations(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	cases := []struct {
		path   string
		origin string
		want   int
	}{
		{"/v1/agents/exchange", "", http.StatusNoContent},
		{"/v1/hooks/ingest", "", http.StatusNoContent},
		{"/v1/agents/tools/run", "", http.StatusNoContent},
		{"/v1/ingest/tok", "", http.StatusNoContent},
		{"/v1/inbox/decide-by-token", "", http.StatusNoContent},
		{"/v1/connect/t3", "", http.StatusNoContent},
		{"/v1/auth/login", "", http.StatusForbidden},
		{"/v1/auth/setup", "", http.StatusForbidden},
		{"/v1/settings", "", http.StatusForbidden},
		{"/v1/settings", "https://evil.example", http.StatusForbidden},
		{"/v1/settings", public, http.StatusNoContent},
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodPost, c.path, nil)
		if c.origin != "" {
			req.Header.Set("Origin", c.origin)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("POST %s origin=%q: got %d, want %d", c.path, c.origin, rec.Code, c.want)
		}
	}
}

// /mcp authenticated by a token header skips the Origin check whether the
// token arrives as a bearer or in x-bf-vk (the header T3's Bifrost proxy
// sends, with no Origin): neither is a cookie, so neither is
// CSRF-replayable. /mcp with neither header, or a cookie, still needs a
// matching Origin.
func TestEnforceOriginOnMutationsMCPTokenHeaders(t *testing.T) {
	const public = "https://toolyard.example.com"
	s := &Server{security: SecurityOptions{PublicURL: public}}
	h := s.EnforceOriginOnMutations(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	cases := []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{"bearer", map[string]string{"Authorization": "Bearer ag_x.y"}, http.StatusNoContent},
		{"x-bf-vk", map[string]string{"x-bf-vk": "ag_x.y"}, http.StatusNoContent},
		{"empty x-bf-vk", map[string]string{"x-bf-vk": "  "}, http.StatusForbidden},
		{"no token", nil, http.StatusForbidden},
		{"cookie only", map[string]string{"Cookie": "toolyard_session=abc"}, http.StatusForbidden},
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		for k, v := range c.headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("POST /mcp %s: got %d, want %d", c.name, rec.Code, c.want)
		}
	}
}
