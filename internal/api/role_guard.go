package api

import (
	"context"
	"net/http"
	pathpkg "path"
	"strings"

	"github.com/tusharbhardwaj/toolyard/internal/identity"
)

// RoleGuard is the deny-by-default gate for members. When a /v1 request
// carries a session cookie that resolves to an active member, only the
// routes in memberAllowed go through; everything else — including routes
// added later that forget to check the role — gets 403 admin_only. Admins
// pass. Requests without a valid session cookie pass untouched (bearer
// routes, login, and handlers that answer 401 themselves).
//
// The resolved user rides along in the request context so handlers'
// requireUser/requireAdmin don't hit the database a second time.
func (s *Server) RoleGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1/") {
			next.ServeHTTP(w, r)
			return
		}
		// An operator token (no cookie) acts as its user, within its scopes.
		if isOperatorRequest(r) {
			r2, ok := s.operatorGuard(w, r)
			if !ok {
				return
			}
			s.serveOperator(next, w, r2)
			return
		}
		if _, err := r.Cookie(sessionCookieName); err != nil {
			next.ServeHTTP(w, r)
			return
		}
		u, ok := s.sessionUser(r)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), ctxUserKey, u))
		if u.Role != identity.RoleAdmin && !memberAllowed(r.Method, r.URL.Path) {
			writeError(w, http.StatusForbidden, "admin_only")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// memberRoutes: exact path -> space-separated methods a member may use,
// or "*" for any. Keep this list short and obvious; it is the whole of
// what a member can do through the API.
var memberRoutes = map[string]string{
	"/v1/push/vapid_key":            http.MethodGet,
	"/v1/push/subscribe":            http.MethodPost + " " + http.MethodDelete,
	"/v1/approvals":                 http.MethodGet,
	"/v1/approvals/decide-batch":    http.MethodPost,
	"/v1/approvals/decide-by-token": http.MethodPost,
	"/v1/events/stream":             http.MethodGet,
	"/v1/health":                    http.MethodGet,
	"/v1/auth/me":                   http.MethodGet,
	"/v1/auth/config":               http.MethodGet,
	"/v1/auth/logout":               http.MethodPost,
	"/v1/auth/login":                http.MethodPost,
	"/v1/auth/clerk/session":        http.MethodPost,
	// Authenticated by the Clerk token in its body; a cookie riding along
	// must not change the answer.
	"/v1/connect/t3": http.MethodPost,
	// Own agents: list, create, enrolment code. Per-agent actions are the
	// pattern below; identity scopes them to the caller's own agents.
	"/v1/agents":        http.MethodGet + " " + http.MethodPost,
	"/v1/agents/enroll": http.MethodPost,
	// Which servers and data groups this member may use.
	"/v1/me/servers": http.MethodGet,
	// The member's own Beknown key: see its status, reveal it once.
	// Provisioning, rotating and revoking are admin routes under /v1/users/.
	"/v1/me/identity-key":        http.MethodGet,
	"/v1/me/identity-key/reveal": http.MethodPost,
	// The member's own sign-ins to per_user servers (My connections); the
	// per-server begin/disconnect actions are the pattern below. The OAuth
	// return routes must be reachable too: the provider sends the member's
	// browser back to the callback, and the handler itself checks that the
	// flow is theirs (a shared flow stays admin-only there).
	"/v1/me/connections":     http.MethodGet,
	"/v1/mcp-oauth/callback": http.MethodGet,
	"/v1/mcp-oauth/paste":    http.MethodPost,
	// Bearer-token agent routes ignore cookies, so a member's browser
	// cookie riding along must not lock them out.
	"/v1/agents/exchange":  "*",
	"/v1/agents/whoami":    "*",
	"/v1/agents/tools":     "*",
	"/v1/agents/tools/run": "*",
}

// reservedAgentSegments are the fixed routes under /v1/agents/ that must
// never be mistaken for an agent id by the pattern match below.
var reservedAgentSegments = map[string]bool{
	"enroll": true, "exchange": true, "whoami": true, "tools": true, "approvals": true,
}

// memberAllowed reports whether a member may call method on path.
func memberAllowed(method, path string) bool {
	// ServeMux redirects unclean paths before routing; a member gets no
	// benefit of the doubt on the raw form.
	if path != pathpkg.Clean(path) {
		return false
	}
	if methods, ok := memberRoutes[path]; ok {
		if methods == "*" {
			return true
		}
		for _, m := range strings.Fields(methods) {
			if m == method {
				return true
			}
		}
		return false
	}
	// Bearer-token approval polling for the CLI.
	if strings.HasPrefix(path, "/v1/agents/approvals/") {
		return true
	}
	// A one-time connect link an agent gave the member: GET shows the
	// confirm page, POST (from that page) redeems the ticket and starts
	// only that member's own sign-in (connect_link_routes.go).
	if ticket, ok := strings.CutPrefix(path, connectLinkPath); ok {
		return (method == http.MethodGet || method == http.MethodPost) && ticket != "" && !strings.Contains(ticket, "/")
	}
	if rest, ok := strings.CutPrefix(path, "/v1/approvals/"); ok {
		id, sub, _ := strings.Cut(rest, "/")
		if id == "" || id == "export" || id == "decide-batch" || id == "decide-by-token" {
			return false
		}
		return (sub == "" && method == http.MethodGet) || (sub == "decide" && method == http.MethodPost)
	}
	// Own connections:
	//   POST   /v1/me/connections/{server}/begin
	//   DELETE /v1/me/connections/{server}
	if rest, ok := strings.CutPrefix(path, "/v1/me/connections/"); ok {
		server, sub, _ := strings.Cut(rest, "/")
		if server == "" {
			return false
		}
		switch {
		case sub == "" && method == http.MethodDelete:
			return true
		case sub == "begin" && method == http.MethodPost:
			return true
		}
		return false
	}
	// Own-agent actions:
	//   POST   /v1/agents/{id}/rotate|disable|enable
	//   DELETE /v1/agents/{id}
	if rest, ok := strings.CutPrefix(path, "/v1/agents/"); ok {
		id, sub, _ := strings.Cut(rest, "/")
		if id == "" || reservedAgentSegments[id] {
			return false
		}
		switch {
		case sub == "" && method == http.MethodDelete:
			return true
		case (sub == "rotate" || sub == "disable" || sub == "enable") && method == http.MethodPost:
			return true
		}
	}
	return false
}
