package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

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
