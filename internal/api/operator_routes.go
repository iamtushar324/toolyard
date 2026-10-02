package api

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/tusharbhardwaj/toolyard/docs"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/settings"
)

// Operator tokens let a CLI agent use every /v1 route the dashboard uses,
// as the token's user and within its scopes:
//
//	read   GET on the API
//	write  the rest of the dashboard: servers, agents, settings, events,
//	       memory, notes, tool runs (which still go through approval), and
//	       secret *requests*
//	owner  what decides or pre-authorizes tool calls or holds credentials:
//	       approvals, inbox decisions and grants, policies, auto-approval
//	       rules, per-tool policy, users, passkeys, push subscriptions,
//	       approval-related settings, secret values, audit purges
//
// Nothing that returns a secret value (/reveal routes) or manages browser
// sessions is reachable with an operator token, whatever its scopes.

const ctxOperatorKey ctxKey = 100

// operatorFromContext returns the operator token that authenticated r, if any.
func operatorFromContext(ctx context.Context) *identity.OperatorToken {
	t, _ := ctx.Value(ctxOperatorKey).(*identity.OperatorToken)
	return t
}

// operatorBearer returns the operator token in r's Authorization header.
func operatorBearer(r *http.Request) (string, bool) {
	authz := r.Header.Get("Authorization")
	if !strings.HasPrefix(authz, "Bearer ") {
		return "", false
	}
	tok := strings.TrimSpace(strings.TrimPrefix(authz, "Bearer "))
	if !strings.HasPrefix(tok, identity.OperatorTokenPrefix) {
		return "", false
	}
	return tok, true
}

// isOperatorRequest reports whether r authenticates with an operator token
// rather than a cookie. Such requests are not CSRF-replayable.
func isOperatorRequest(r *http.Request) bool {
	if _, err := r.Cookie(sessionCookieName); err == nil {
		return false
	}
	_, ok := operatorBearer(r)
	return ok
}

// operatorNeverPaths can't be reached with an operator token at all.
var operatorNeverPaths = map[string]bool{
	"/v1/auth/setup":         true,
	"/v1/auth/login":         true,
	"/v1/auth/logout":        true,
	"/v1/auth/clerk/session": true,
	"/v1/connect/t3":         true,
}

// operatorOwnerPrefixes need the owner scope for any non-GET method.
var operatorOwnerPrefixes = []string{
	"/v1/approvals",
	"/v1/policies",
	"/v1/insights/auto/rules",
	"/v1/insights/tools/",
	"/v1/insights/purge-agent",
	"/v1/users",
	"/v1/passkeys",
	"/v1/push/subscribe",
	"/v1/push/rotate-vapid",
	"/v1/settings/rotate",
	"/v1/chat/telegram/",
	"/v1/inbox/batch",
	"/v1/inbox/grants",
	"/v1/secrets/",
}

// operatorScopeFor returns the scope an operator token needs for method on
// path, or ok=false when operator tokens may never call it.
func operatorScopeFor(method, path string) (scope string, ok bool) {
	if operatorNeverPaths[path] || strings.Contains(path, "/reveal") {
		return "", false
	}
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return identity.ScopeRead, true
	}
	for _, p := range operatorOwnerPrefixes {
		if strings.HasPrefix(path, p) {
			return identity.ScopeOwner, true
		}
	}
	// /v1/inbox/{id}/decide approves or denies an agent's request.
	if strings.HasPrefix(path, "/v1/inbox/") && strings.HasSuffix(path, "/decide") {
		return identity.ScopeOwner, true
	}
	return identity.ScopeWrite, true
}

// ownerSettingKeys are settings that change how calls get approved, where
// approval prompts go, how long the audit trail lives, or hold credentials.
// Patching any of them with an operator token needs the owner scope.
var ownerSettingKeys = map[string]bool{
	settings.ApprovalMode:                    true,
	settings.AutoApprovalEnabled:             true,
	settings.AutoApprovalPatternMinApprovals: true,
	settings.AutoApprovalCooloffDays:         true,
	settings.AutoApprovalRateLimitPerHour:    true,
	settings.TelegramEnabled:                 true,
	settings.TelegramBotToken:                true,
	settings.TelegramBotUsername:             true,
	settings.TelegramChatID:                  true,
	settings.TelegramUserID:                  true,
	settings.MetricsRetentionDays:            true,
	settings.EventsRetentionDays:             true,
	settings.ClickhousePassword:              true,
}

// operatorGuard authenticates an operator-token request and checks its scope.
// It returns the request carrying the user and token, or false after writing
// the error itself.
func (s *Server) operatorGuard(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
	tok, _ := operatorBearer(r)
	t, u, err := s.identity.VerifyOperatorToken(r.Context(), tok)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid operator token")
		return nil, false
	}
	need, ok := operatorScopeFor(r.Method, r.URL.Path)
	if !ok {
		writeError(w, http.StatusForbidden, "not available to operator tokens")
		return nil, false
	}
	if !t.HasScope(need) {
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error":          "operator_scope",
			"required_scope": need,
			"hint":           "ask the owner to run this from the dashboard, or to mint a token with this scope",
		})
		return nil, false
	}
	if u.Role != identity.RoleAdmin && !memberAllowed(r.Method, r.URL.Path) {
		writeError(w, http.StatusForbidden, "admin_only")
		return nil, false
	}
	ctx := context.WithValue(r.Context(), ctxUserKey, u)
	ctx = context.WithValue(ctx, ctxOperatorKey, t)
	return r.WithContext(ctx), true
}

// statusRecorder remembers the status code so operator mutations can be
// audited with their outcome.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (w *statusRecorder) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusRecorder) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// serveOperator runs next for an authenticated operator request and records
// every mutation in the audit log (method, path and status; never bodies).
func (s *Server) serveOperator(next http.Handler, w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		next.ServeHTTP(w, r)
		return
	}
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	next.ServeHTTP(rec, r)
	t := operatorFromContext(r.Context())
	u, _ := r.Context().Value(ctxUserKey).(*identity.User)
	if s.audit == nil || t == nil || u == nil {
		return
	}
	ev := audit.Event{
		EventType:     "operator.request",
		AgentID:       "operator:" + t.ID,
		ResultSummary: r.Method + " " + r.URL.Path + " -> " + http.StatusText(rec.status),
	}
	ev.AgentName = t.Name
	ev.AgentKind = "cli"
	ev.OwnerUserID, ev.OwnerEmail, ev.OwnerName = u.ID, u.Email, u.Label()
	ev.ClientIP = s.security.ClientIP(r)
	ev.ClientKind = "cli"
	ev.Via = "operator"
	_ = s.audit.Write(context.WithoutCancel(r.Context()), ev)
}

func (s *Server) operatorRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/v1/operator-tokens", s.operatorTokensCollection)
	mux.HandleFunc("/v1/operator-tokens/", s.operatorTokensItem)
	mux.HandleFunc("/v1/operator/routes", s.operatorRoutesCatalog)
	mux.HandleFunc("/v1/operator/guide", s.operatorGuide)
}

// GET  /v1/operator-tokens        the caller's tokens (?all=1: every user's)
// POST /v1/operator-tokens        {"name","scopes":["read","write"],"ttl_hours":0}
//
// The plaintext token is returned once. A request authenticated by an
// operator token can only mint scopes it holds itself.
func (s *Server) operatorTokensCollection(w http.ResponseWriter, r *http.Request) {
	u, err := s.requireAdmin(r)
	if err != nil {
		writeAuthError(w, err)
		return
	}
	switch r.Method {
	case http.MethodGet:
		owner := u.ID
		if r.URL.Query().Get("all") == "1" {
			owner = ""
		}
		toks, err := s.identity.ListOperatorTokens(r.Context(), owner)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, toks)
	case http.MethodPost:
		var body struct {
			Name     string   `json:"name"`
			Scopes   []string `json:"scopes"`
			TTLHours float64  `json:"ttl_hours"`
		}
		if err := decode(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		scopes, err := identity.NormalizeOperatorScopes(body.Scopes)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		createdBy := "dashboard"
		if parent := operatorFromContext(r.Context()); parent != nil {
			createdBy = parent.ID
			for _, sc := range scopes {
				if !parent.HasScope(sc) {
					writeJSON(w, http.StatusForbidden, map[string]any{
						"error": "operator_scope", "required_scope": sc,
						"hint": "a token can only mint scopes it holds",
					})
					return
				}
			}
		}
		ttl := time.Duration(body.TTLHours * float64(time.Hour))
		tok, t, err := s.identity.CreateOperatorToken(r.Context(), u.ID, body.Name, scopes, ttl, createdBy)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		_ = s.audit.Write(r.Context(), audit.Event{
			EventType: "operator_token.create", AgentID: "user:" + u.ID,
			ResultSummary: t.ID + " " + t.Name + " [" + strings.Join(t.Scopes, " ") + "]",
		})
		writeJSON(w, http.StatusOK, map[string]any{"token": tok, "operator_token": t})
	default:
		writeError(w, http.StatusMethodNotAllowed, "GET or POST")
	}
}

// DELETE /v1/operator-tokens/{id} revokes one of the caller's tokens.
func (s *Server) operatorTokensItem(w http.ResponseWriter, r *http.Request) {
	u, err := s.requireAdmin(r)
	if err != nil {
		writeAuthError(w, err)
		return
	}
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "DELETE only")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/v1/operator-tokens/")
	if err := s.identity.RevokeOperatorToken(r.Context(), u.ID, id); err != nil {
		if errors.Is(err, identity.ErrOperatorTokenNotFound) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.audit.Write(r.Context(), audit.Event{
		EventType: "operator_token.revoke", AgentID: "user:" + u.ID, ResultSummary: id,
	})
	writeJSON(w, http.StatusOK, map[string]any{"revoked": id})
}

// GET /v1/operator/routes — the machine-readable API map, with the scope an
// operator token needs for each method (and whether it may call it at all).
func (s *Server) operatorRoutesCatalog(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireUser(r); err != nil {
		writeAuthError(w, err)
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	type method struct {
		Method string `json:"method"`
		Scope  string `json:"scope,omitempty"`
		Denied bool   `json:"operator_denied,omitempty"`
	}
	type route struct {
		Path    string   `json:"path"`
		Summary string   `json:"summary"`
		Methods []method `json:"methods"`
	}
	out := make([]route, 0, len(operatorCatalog))
	for _, c := range operatorCatalog {
		rt := route{Path: c.Path, Summary: c.Summary}
		for _, m := range strings.Fields(c.Methods) {
			sc, ok := operatorScopeFor(m, strings.ReplaceAll(c.Path, "{id}", "x"))
			rt.Methods = append(rt.Methods, method{Method: m, Scope: sc, Denied: !ok})
		}
		out = append(out, rt)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	writeJSON(w, http.StatusOK, out)
}

// GET /v1/operator/guide — the operator guide (markdown).
func (s *Server) operatorGuide(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireUser(r); err != nil {
		writeAuthError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	_, _ = w.Write([]byte(docs.OperatorGuide))
}

// operatorSettingsCheck rejects an operator-token PATCH that touches an
// owner-only setting without the owner scope. It reports whether the
// handler may go on.
func (s *Server) operatorSettingsCheck(w http.ResponseWriter, r *http.Request, body map[string]any) bool {
	t := operatorFromContext(r.Context())
	if t == nil || t.HasScope(identity.ScopeOwner) {
		return true
	}
	var keys []string
	for k := range body {
		if ownerSettingKeys[k] || settings.IsSecretKey(k) {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return true
	}
	sort.Strings(keys)
	writeJSON(w, http.StatusForbidden, map[string]any{
		"error": "operator_scope", "required_scope": identity.ScopeOwner, "keys": keys,
		"hint": "these settings control approvals, approval channels, retention or credentials; ask the owner to change them in the dashboard",
	})
	return false
}

// operatorCatalogEntry documents one registered path for agents.
type operatorCatalogEntry struct {
	Path    string
	Methods string
	Summary string
}

// operatorCatalog lists every /v1 path the server registers (a test keeps it
// complete). {id} marks a path segment.
var operatorCatalog = []operatorCatalogEntry{
	{"/v1/health", "GET", "liveness"},
	{"/v1/auth/setup", "POST", "first-user bootstrap (browser only)"},
	{"/v1/auth/login", "POST", "password login (browser only)"},
	{"/v1/auth/logout", "POST", "end the browser session"},
	{"/v1/auth/me", "GET", "the authenticated user"},
	{"/v1/auth/config", "GET", "login methods"},
	{"/v1/auth/clerk/session", "POST", "Clerk login (browser only)"},
	{"/v1/connect/t3", "POST", "bkt3 server: Clerk session token -> the person's agent token (never via operator token)"},
	{"/v1/operator-tokens", "GET POST", "list or mint operator tokens {name, scopes, ttl_hours}"},
	{"/v1/operator-tokens/{id}", "DELETE", "revoke an operator token"},
	{"/v1/operator/routes", "GET", "this catalog"},
	{"/v1/operator/guide", "GET", "operator guide (markdown)"},
	{"/v1/users", "GET POST", "users: list, create"},
	{"/v1/users/{id}", "GET PATCH DELETE POST", "one user: role, status, access, identity key"},
	{"/v1/me/servers", "GET", "servers and data groups the caller may use"},
	{"/v1/me/connections", "GET", "per-user servers the caller may connect, with their sign-in state (never tokens)"},
	{"/v1/me/connections/{id}", "POST DELETE", "POST /begin starts the caller's own sign-in -> {authorize_url}; DELETE disconnects their account"},
	{"/v1/me/identity-key", "GET POST", "the caller's identity key status"},
	{"/v1/me/identity-key/reveal", "POST", "reveal the identity key once (never via operator token)"},
	{"/v1/agents", "GET POST", "agents: list, create {name} (returns the agent token once)"},
	{"/v1/agents/{id}", "DELETE POST", "agent actions: /rotate, /disable, /enable; DELETE removes"},
	{"/v1/agents/enroll", "POST", "one-time enrollment code for `toolyard auth login`"},
	{"/v1/agents/exchange", "POST", "agent: swap enrollment code for token"},
	{"/v1/agents/whoami", "GET", "agent: token identity"},
	{"/v1/agents/tools", "GET", "agent: tool catalog"},
	{"/v1/agents/tools/run", "POST", "agent: run a tool"},
	{"/v1/agents/approvals/{id}", "GET", "agent: poll its own approval"},
	{"/v1/approvals", "GET", "approval queue"},
	{"/v1/approvals/{id}", "GET POST", "one approval; POST decides it"},
	{"/v1/approvals/decide-batch", "POST", "decide many approvals"},
	{"/v1/approvals/decide-by-token", "POST", "push-tap decision (signed token)"},
	{"/v1/approvals/export", "GET", "approval history export"},
	{"/v1/audit", "GET", "audit log"},
	{"/v1/audit/export", "GET", "audit export"},
	{"/v1/tools", "GET", "full tool catalog"},
	{"/v1/tools/run", "POST", "run a tool as the user {tool, arguments}; writes still need approval"},
	{"/v1/servers", "GET POST", "MCP upstream servers: list, add {name, transport, command, args, url, env, headers, identity, auth_mode: shared|per_user, enabled}"},
	{"/v1/servers/{id}", "GET PATCH DELETE POST", "one server; PATCH {url, headers, env, identity, auth_mode, enabled}; POST /reconnect, /convert-env, /oauth/*; GET /connections (who signed in to a per_user server)"},
	{"/v1/mcp-oauth/callback", "GET", "OAuth redirect target for upstream servers"},
	{"/v1/mcp-oauth/paste", "POST", "paste an OAuth code for an upstream server"},
	{"/v1/marketplace", "GET", "curated server recipes"},
	{"/v1/secrets", "GET POST", "secrets metadata; POST {name, description, request:true} asks the owner for a value"},
	{"/v1/secrets/{id}", "PUT DELETE", "set/rotate (?reconnect=1) or delete a secret value"},
	{"/v1/policies", "GET POST", "tool policies"},
	{"/v1/policies/{id}", "PATCH DELETE", "one policy"},
	{"/v1/settings", "GET PATCH", "settings (secret values masked)"},
	{"/v1/settings/reveal", "POST", "reveal a secret setting (never via operator token)"},
	{"/v1/settings/rotate", "POST", "rotate a generated secret setting"},
	{"/v1/usage", "GET", "usage summary"},
	{"/v1/insights/overview", "GET", "insights overview"},
	{"/v1/insights/agents", "GET", "per-agent insights"},
	{"/v1/insights/agents/{id}", "GET", "one agent's insights"},
	{"/v1/insights/tools", "GET", "per-tool insights"},
	{"/v1/insights/tools/{id}", "GET POST PATCH", "per-tool policy"},
	{"/v1/insights/anomalies", "GET", "anomalies"},
	{"/v1/insights/anomalies/{id}", "POST", "acknowledge/dismiss an anomaly"},
	{"/v1/insights/auto/rules", "GET POST", "auto-approval rules"},
	{"/v1/insights/auto/rules/{id}", "POST DELETE", "enable/disable/delete an auto-approval rule"},
	{"/v1/insights/cost", "GET", "cost estimates"},
	{"/v1/insights/export", "GET", "insights export"},
	{"/v1/insights/purge-agent", "POST", "delete an agent's metrics"},
	{"/v1/inbox", "GET", "owner inbox"},
	{"/v1/inbox/{id}", "GET POST", "inbox item; /decide, /summarize, /explain, grants, sessions, batch"},
	{"/v1/inbox/decide-by-token", "POST", "push-tap inbox decision (signed token)"},
	{"/v1/guide", "GET", "agent protocol"},
	{"/v1/guide/skill", "GET", "toolyard-inbox skill"},
	{"/v1/events", "GET", "event feed"},
	{"/v1/events/{id}", "GET POST", "one event"},
	{"/v1/events/ack", "POST", "acknowledge events"},
	{"/v1/events/stream", "GET", "event stream (SSE)"},
	{"/v1/event-sources", "GET POST", "webhook event sources"},
	{"/v1/event-sources/{id}", "GET PATCH DELETE POST", "one event source"},
	{"/v1/ingest", "POST", "webhook ingest (source token)"},
	{"/v1/ingest/{id}", "POST", "webhook ingest with path token"},
	{"/v1/hooks/ingest", "POST", "agent hook ingest"},
	{"/v1/hooks/events", "GET", "agent hook events"},
	{"/v1/hooks/export", "GET", "hook events export"},
	{"/v1/memory", "GET POST DELETE", "memory store"},
	{"/v1/memory/list", "GET", "list memories"},
	{"/v1/memory/export", "GET", "export memories"},
	{"/v1/memory/import", "POST", "import memories"},
	{"/v1/memory/metrics", "GET", "memory metrics"},
	{"/v1/memory/webhooks", "GET POST", "memory webhooks"},
	{"/v1/memory/webhooks/{id}", "GET PATCH DELETE POST", "one memory webhook, or its ingest"},
	{"/v1/memory/webhooks/wings", "GET", "MemPalace wings for webhooks"},
	{"/v1/memory/webhooks/ingest", "POST", "memory webhook ingest (webhook token)"},
	{"/v1/memory/webhooks/ingest/jobs/{id}", "GET", "memory webhook ingest job status"},
	{"/v1/mempalace/status", "GET", "MemPalace status"},
	{"/v1/mempalace/agents", "GET", "MemPalace agents"},
	{"/v1/mempalace/ingest", "POST", "MemPalace ingest (agent token)"},
	{"/v1/notes/status", "GET", "notes workspace status"},
	{"/v1/notes/sync", "POST", "sync notes"},
	{"/v1/notes/publish", "POST", "publish a note"},
	{"/v1/passkeys", "GET", "passkeys"},
	{"/v1/passkeys/{id}", "POST DELETE", "register or remove a passkey"},
	{"/v1/push/vapid_key", "GET", "web-push public key"},
	{"/v1/push/subscribe", "POST DELETE", "web-push subscription (receives approval prompts)"},
	{"/v1/push/test", "POST", "send a test push"},
	{"/v1/push/diag", "GET", "push diagnostics"},
	{"/v1/push/jwt-preview", "GET", "push JWT preview"},
	{"/v1/push/rotate-vapid", "POST", "rotate web-push keys"},
	{"/v1/chat/status", "GET", "chat channel status"},
	{"/v1/chat/telegram/{id}", "GET POST", "Telegram approval channel setup"},
	{"/v1/voice/sessions", "GET POST", "voice sessions"},
	{"/v1/voice/ws", "GET", "voice websocket"},
	{"/v1/voice/hangup", "POST", "end a voice session"},
	{"/v1/diagnostics/crashes", "GET", "crash dumps"},
}
