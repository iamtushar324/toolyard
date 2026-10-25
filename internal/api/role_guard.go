package api

import (
	"context"
	"net/http"
	pathpkg "path"
	"strings"

	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/ops"
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
//
// A session token ("tys_…") is refused with 403 on every /v1 route
// outside its own (sessionTokenAllowed), whatever else the request
// carries: it is a short-lived child of an agent token for code mode, not
// a general agent credential.
func (s *Server) RoleGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1/") {
			next.ServeHTTP(w, r)
			return
		}
		// Liveness and readiness probes read no user data and answer
		// anyone: a cookie or token riding along changes nothing.
		if isProbeRead(r) {
			next.ServeHTTP(w, r)
			return
		}
		if tok, ok := bearerToken(r); ok && identity.IsSessionToken(tok) && !sessionTokenAllowed(r.URL.Path) {
			writeError(w, http.StatusForbidden, "not available to session tokens")
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

// sessionTokenAllowed reports whether a session token may call path: the
// code routes, the job and agent skills routes, whoami, the agent's
// approval polling, and the session token routes (whose handlers allow
// only revoking the token itself). Methods are left to the handlers. A
// path that is not clean gets no benefit of the doubt.
func sessionTokenAllowed(path string) bool {
	if path != pathpkg.Clean(path) {
		return false
	}
	return strings.HasPrefix(path, codePathPrefix) ||
		path == jobsPath || strings.HasPrefix(path, jobsPrefix) || skillsAgentPath(path) || path == skillsSourcesPath ||
		path == "/v1/agents/whoami" ||
		strings.HasPrefix(path, "/v1/agents/approvals/") ||
		path == sessionTokensPath || strings.HasPrefix(path, sessionTokensPrefix)
}

// isProbeRead reports a GET or HEAD of a probe route (ops.IsProbePath).
// Any other method on those paths gets no exemption.
func isProbeRead(r *http.Request) bool {
	return (r.Method == http.MethodGet || r.Method == http.MethodHead) && ops.IsProbePath(r.URL.Path)
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
	"/v1/health":                    http.MethodGet,
	"/v1/auth/me":                   http.MethodGet,
	"/v1/auth/config":               http.MethodGet,
	"/v1/auth/logout":               http.MethodPost,
	"/v1/auth/login":                http.MethodPost,
	"/v1/auth/clerk/session":        http.MethodPost,
	// Authenticated by the Clerk token in its body; a cookie riding along
	// must not change the answer.
	"/v1/connect/t3":               http.MethodPost,
	"/v1/inbox":                    http.MethodGet,
	"/v1/inbox/sessions":           http.MethodGet,
	"/v1/inbox/grants":             http.MethodGet,
	"/v1/inbox/info":               http.MethodGet,
	"/v1/inbox/batch":              http.MethodPost,
	"/v1/inbox/grants/revoke-all":  http.MethodPost,
	"/v1/passkeys":                 http.MethodGet,
	"/v1/passkeys/register/begin":  http.MethodPost,
	"/v1/passkeys/register/finish": http.MethodPost,
	// Own agents: list, create, enrolment code. Per-agent actions are the
	// pattern below; identity scopes them to the caller's own agents.
	"/v1/agents":        http.MethodGet + " " + http.MethodPost,
	"/v1/agents/enroll": http.MethodPost,
	// Which servers and data groups this member may use.
	"/v1/me/servers": http.MethodGet,
	// Connected apps (OAuth clients) and per-agent scopes: the handlers
	// act only on the member's own apps and agents.
	"/v1/me/apps":           http.MethodGet,
	"/v1/me/apps/config":    http.MethodGet,
	"/v1/me/apps/rename":    http.MethodPost,
	"/v1/me/apps/scope":     http.MethodPost,
	"/v1/me/apps/revoke":    http.MethodPost,
	"/v1/me/apps/catalog":   http.MethodGet,
	"/v1/me/apps/audit":     http.MethodGet,
	"/v1/me/agent-scopes":   http.MethodGet + " " + http.MethodPost,
	"/v1/connections/hosts": http.MethodGet,
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
	"/v1/agents/exchange":       "*",
	"/v1/agents/whoami":         "*",
	"/v1/agents/tools":          "*",
	"/v1/agents/tools/run":      "*",
	"/v1/agents/session-tokens": "*",
}

// reservedAgentSegments are the fixed routes under /v1/agents/ that must
// never be mistaken for an agent id by the pattern match below.
var reservedAgentSegments = map[string]bool{
	"enroll": true, "exchange": true, "whoami": true, "tools": true, "approvals": true, "session-tokens": true,
}

// memberAllowed reports whether a member may call method on path.
func memberAllowed(method, path string) bool {
	if hostProtocolPath(path) {
		return method == http.MethodPost
	}
	if rest, ok := strings.CutPrefix(path, "/v1/connections/hosts/"); ok {
		parts := strings.Split(rest, "/")
		return method == http.MethodPost && len(parts) == 2 && parts[0] != "" && parts[1] == "revoke"
	}
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
	// All Inbox item handlers check the caller's owned agents before acting.
	if rest, ok := strings.CutPrefix(path, "/v1/inbox/"); ok {
		parts := strings.Split(rest, "/")
		if len(parts) == 1 {
			return method == http.MethodGet && parts[0] != "voice-key" && parts[0] != "decide-by-token"
		}
		if len(parts) == 2 {
			if parts[0] == "blobs" {
				return method == http.MethodGet || method == http.MethodHead
			}
			if method == http.MethodPost {
				switch parts[1] {
				case "decide", "passkey", "summarize", "explain":
					return true
				}
			}
		}
		if len(parts) == 3 && parts[0] == "grants" && parts[2] == "revoke" {
			return method == http.MethodPost
		}
		if len(parts) == 4 && parts[1] == "callbacks" && parts[3] == "retry" {
			return method == http.MethodPost
		}
		return false
	}
	if rest, ok := strings.CutPrefix(path, "/v1/passkeys/"); ok {
		parts := strings.Split(rest, "/")
		return method == http.MethodPost && ((len(parts) == 2 && parts[1] == "remove") || (len(parts) == 3 && parts[1] == "remove" && parts[2] == "begin"))
	}
	// Bearer-token approval polling for the CLI.
	if strings.HasPrefix(path, "/v1/agents/approvals/") {
		return true
	}
	// Bearer-only routes (agent or session token) that never read a
	// cookie: session tokens, code mode over REST, async jobs and the
	// agent skills routes, whose bundle download is authenticated by the
	// single-use ticket in its path (skills_routes.go). Not
	// /v1/skills/sources, which the admin skill routes share.
	if strings.HasPrefix(path, sessionTokensPrefix) || strings.HasPrefix(path, codePathPrefix) ||
		path == jobsPath || strings.HasPrefix(path, jobsPrefix) || skillsAgentPath(path) {
		return true
	}
	// A native Inbox upload: the single-use ticket in the path is the
	// credential (upload_routes.go); a cookie riding along changes nothing.
	if ticket, ok := strings.CutPrefix(path, uploadsPathPrefix); ok {
		return method == http.MethodPut && ticket != "" && !strings.Contains(ticket, "/")
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
