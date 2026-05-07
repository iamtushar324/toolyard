// Package api exposes the dashboard's REST surface and SSE feeds.
//
// Routes (all under /v1):
//   POST /v1/auth/setup           one-time create the local user
//   POST /v1/auth/login           username + password -> session cookie
//   POST /v1/auth/logout
//   GET  /v1/auth/me              current user
//
//   GET  /v1/agents
//   POST /v1/agents/enroll        creates a code (op uses it on the agent)
//   POST /v1/agents/exchange      agent swaps code for a long-lived token
//
//   GET  /v1/approvals
//   GET  /v1/approvals/{id}
//   POST /v1/approvals/{id}/decide      {"action":"allowed|denied"}
//   POST /v1/approvals/decide-by-token  one-tap approve via signed token
//
//   GET  /v1/audit
//   GET  /v1/audit/stream         SSE
//   GET  /v1/events/stream        SSE - approval/audit fan-out
//
//   GET  /v1/memory               list (?scope=, ?prefix=)
//   POST /v1/memory               {"scope":..., "key":..., "value":...}
//   DELETE /v1/memory             ?scope=&key=
//
//   POST /v1/push/subscribe       browser PushSubscription
//   GET  /v1/push/vapid_key
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/tusharbhardwaj/toolyard/internal/approval"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/autoapproval"
	"github.com/tusharbhardwaj/toolyard/internal/gateway"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/marketplace"
	"github.com/tusharbhardwaj/toolyard/internal/memory"
	"github.com/tusharbhardwaj/toolyard/internal/metrics"
	"github.com/tusharbhardwaj/toolyard/internal/push"
	"github.com/tusharbhardwaj/toolyard/internal/realtime"
	"github.com/tusharbhardwaj/toolyard/internal/settings"
	"github.com/tusharbhardwaj/toolyard/internal/upstreams"
	"github.com/tusharbhardwaj/toolyard/internal/usage"
)

const (
	sessionCookieName = "toolyard_session"
	sessionTTL        = 24 * time.Hour
	enrollCodeTTL     = 15 * time.Minute
)

type Server struct {
	identity     *identity.Service
	approval     *approval.Bus
	audit        *audit.Logger
	memory       *memory.Service
	push         *push.Service
	hub          *realtime.Hub
	gateway      *gateway.Gateway
	upstreams    *upstreams.Service
	settings     *settings.Service
	usage        *usage.Service
	metrics      *metrics.Reader
	autoApproval *autoapproval.Service
	sessionKey   []byte
	security     SecurityOptions
	loginLimit   *loginThrottle
	unauthLimit  *loginThrottle
}

type Options struct {
	Identity     *identity.Service
	Approval     *approval.Bus
	Audit        *audit.Logger
	Memory       *memory.Service
	Push         *push.Service
	Hub          *realtime.Hub
	Gateway      *gateway.Gateway
	Upstreams    *upstreams.Service
	Settings     *settings.Service
	Usage        *usage.Service
	Metrics      *metrics.Reader
	AutoApproval *autoapproval.Service
	SessionKey   []byte
	Security     SecurityOptions
}

func New(opts Options) *Server {
	s := &Server{
		identity:     opts.Identity,
		approval:     opts.Approval,
		audit:        opts.Audit,
		memory:       opts.Memory,
		push:         opts.Push,
		hub:          opts.Hub,
		gateway:      opts.Gateway,
		upstreams:    opts.Upstreams,
		settings:     opts.Settings,
		usage:        opts.Usage,
		metrics:      opts.Metrics,
		autoApproval: opts.AutoApproval,
		sessionKey:   opts.SessionKey,
		security:     opts.Security,
		loginLimit:   newLoginThrottle(5, 15*time.Minute),
		unauthLimit:  newLoginThrottle(0, time.Hour), // max/window passed per-call via AllowN
	}
	go func() {
		t := time.NewTicker(2 * time.Minute)
		defer t.Stop()
		for range t.C {
			s.loginLimit.Sweep()
			s.unauthLimit.Sweep()
		}
	}()
	return s
}

// SecurityOpts exposes the configured options for middleware wiring outside
// the api package (cmd/gateway uses this when stitching the mux).
func (s *Server) SecurityOpts() SecurityOptions { return s.security }

func (s *Server) Routes(mux *http.ServeMux) {
	mux.HandleFunc("/v1/health", s.health)
	mux.HandleFunc("/v1/auth/setup", s.authSetup)
	mux.HandleFunc("/v1/auth/login", s.authLogin)
	mux.HandleFunc("/v1/auth/logout", s.authLogout)
	mux.HandleFunc("/v1/auth/me", s.authMe)

	mux.HandleFunc("/v1/agents", s.agentsCollection)
	mux.HandleFunc("/v1/agents/enroll", s.agentsEnroll)
	mux.HandleFunc("/v1/agents/exchange", s.agentsExchange)
	mux.HandleFunc("/v1/agents/", s.agentsItem)

	mux.HandleFunc("/v1/approvals", s.approvalsList)
	mux.HandleFunc("/v1/approvals/", s.approvalsOne)
	mux.HandleFunc("/v1/approvals/decide-by-token", s.approvalsDecideByToken)
	mux.HandleFunc("/v1/approvals/decide-batch", s.approvalsDecideBatch)

	mux.HandleFunc("/v1/audit", s.auditList)
	mux.HandleFunc("/v1/events/stream", s.eventsStream)

	mux.HandleFunc("/v1/memory", s.memoryHandler)
	mux.HandleFunc("/v1/memory/list", s.memoryList)

	mux.HandleFunc("/v1/push/vapid_key", s.pushVapidKey)
	mux.HandleFunc("/v1/push/subscribe", s.pushSubscribe)

	mux.HandleFunc("/v1/servers", s.serversCollection)
	mux.HandleFunc("/v1/servers/", s.serversItem)
	mux.HandleFunc("/v1/tools", s.toolsList)
	mux.HandleFunc("/v1/tools/run", s.toolsRun)
	mux.HandleFunc("/v1/marketplace", s.marketplaceList)
	mux.HandleFunc("/v1/settings", s.settingsHandler)
	mux.HandleFunc("/v1/usage", s.usageHandler)

	mux.HandleFunc("/v1/insights/overview", s.insightsOverview)
	mux.HandleFunc("/v1/insights/tools", s.insightsTools)
	mux.HandleFunc("/v1/insights/agents", s.insightsAgents)
	mux.HandleFunc("/v1/insights/agents/", s.insightsAgentDetail)
	mux.HandleFunc("/v1/insights/anomalies", s.insightsAnomalies)
	mux.HandleFunc("/v1/insights/anomalies/", s.insightsAnomalyAction)
	mux.HandleFunc("/v1/insights/cost", s.insightsCost)
	mux.HandleFunc("/v1/insights/auto/rules", s.autoRulesCollection)
	mux.HandleFunc("/v1/insights/auto/rules/", s.autoRulesItem)
	mux.HandleFunc("/v1/insights/purge-agent", s.insightsPurgeAgent)
	mux.HandleFunc("/v1/insights/export", s.insightsExport)
}

// ---- helpers ----------------------------------------------------------------

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "service": "toolyard", "version": "0.1.0"})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if body != nil {
		_ = json.NewEncoder(w).Encode(body)
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// maxJSONBody caps every control-plane JSON body. The middleware also
// applies http.MaxBytesReader to the raw stream — this is a second belt for
// callers that bypass the middleware (none today, but cheap).
const maxJSONBody = 1 << 20 // 1 MiB

func decode(r *http.Request, dst any) error {
	if r.Body == nil {
		return errors.New("empty body")
	}
	defer r.Body.Close()
	limited := http.MaxBytesReader(nil, r.Body, maxJSONBody)
	dec := json.NewDecoder(limited)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	// Reject trailing garbage so a caller can't smuggle a second JSON value.
	if dec.More() {
		return errors.New("body has trailing JSON tokens")
	}
	return nil
}

// ---- auth & session ---------------------------------------------------------

type sessionClaims struct {
	UserID    string `json:"sub"`
	SessionID string `json:"sid"`
	jwt.RegisteredClaims
}

func (s *Server) issueSession(w http.ResponseWriter, r *http.Request, userID string) error {
	// Persist a session anchor so we can revoke this JWT before its expiry.
	sid, err := s.identity.CreateSession(r.Context(), userID, r.UserAgent(), s.security.ClientIP(r), sessionTTL)
	if err != nil {
		return err
	}
	claims := sessionClaims{
		UserID:    userID,
		SessionID: sid,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(sessionTTL)),
			ID:        sid,
		},
	}
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(s.sessionKey)
	if err != nil {
		return err
	}
	// Secure flag only when behind real HTTPS; otherwise local dev (plain HTTP)
	// would lose the cookie on every request.
	secure := s.security.IsBehindHTTPS(r)
	sameSite := http.SameSiteLaxMode
	if secure {
		// Strict gives the strongest CSRF posture once we're confident
		// every cross-tab transition still works under HTTPS. Lax in dev.
		sameSite = http.SameSiteStrictMode
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    tok,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: sameSite,
		Expires:  time.Now().Add(sessionTTL),
	})
	return nil
}

func (s *Server) verifySession(r *http.Request) (string, bool) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return "", false
	}
	parsed, err := jwt.ParseWithClaims(cookie.Value, &sessionClaims{}, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method")
		}
		return s.sessionKey, nil
	})
	if err != nil || !parsed.Valid {
		return "", false
	}
	c, ok := parsed.Claims.(*sessionClaims)
	if !ok {
		return "", false
	}
	// Server-side anchor check: a JWT survives the cookie clear, but a
	// revoked sid stops authenticating immediately. SID is empty for any
	// JWT minted before this change — those are accepted only until they
	// naturally expire (24h grace).
	if c.SessionID != "" && !s.identity.IsSessionValid(r.Context(), c.SessionID) {
		return "", false
	}
	return c.UserID, true
}

// currentSessionID extracts the sid claim if the cookie is present and
// valid. Used by logout so we revoke this JWT specifically.
func (s *Server) currentSessionID(r *http.Request) string {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return ""
	}
	parsed, err := jwt.ParseWithClaims(cookie.Value, &sessionClaims{}, func(t *jwt.Token) (any, error) {
		return s.sessionKey, nil
	})
	if err != nil {
		return ""
	}
	if c, ok := parsed.Claims.(*sessionClaims); ok {
		return c.SessionID
	}
	return ""
}

func (s *Server) requireUser(r *http.Request) (string, error) {
	id, ok := s.verifySession(r)
	if !ok {
		return "", errors.New("unauthorized")
	}
	return id, nil
}

func (s *Server) authSetup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	// Bootstrap is only safe over the loopback (or trusted-proxy localhost),
	// otherwise an attacker on the public internet can race the legit
	// operator and claim the admin account.
	if !s.security.IsLoopback(r) {
		writeError(w, http.StatusForbidden, "first-time setup must be performed from localhost")
		return
	}
	// One-shot: once any user exists the route closes entirely.
	if exists, err := s.identity.HasUser(r.Context()); err == nil && exists {
		http.NotFound(w, r)
		return
	}
	var body struct{ Username, Password string }
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	u, err := s.identity.CreateUser(r.Context(), body.Username, body.Password)
	if errors.Is(err, identity.ErrUserExists) {
		writeError(w, http.StatusConflict, "user already configured")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.issueSession(w, r, u.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, u)
}

func (s *Server) authLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	// Per-IP throttle. We don't let an attacker walk past argon2id at full speed.
	ip := s.security.ClientIP(r)
	if !s.loginLimit.Allow(ip) {
		writeError(w, http.StatusTooManyRequests, "too many login attempts; try again later")
		return
	}
	var body struct{ Username, Password string }
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	u, err := s.identity.Authenticate(r.Context(), body.Username, body.Password)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	// Successful login resets the bucket so later legit requests aren't blocked.
	s.loginLimit.Reset(ip)
	if err := s.issueSession(w, r, u.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.audit.Write(r.Context(), audit.Event{EventType: audit.EventUserLogin, ResultSummary: u.Username})
	writeJSON(w, http.StatusOK, u)
}

func (s *Server) authLogout(w http.ResponseWriter, r *http.Request) {
	if sid := s.currentSessionID(r); sid != "" {
		_ = s.identity.RevokeSession(r.Context(), sid)
	}
	secure := s.security.IsBehindHTTPS(r)
	sameSite := http.SameSiteLaxMode
	if secure {
		sameSite = http.SameSiteStrictMode
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   secure,
		SameSite: sameSite,
	})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) authMe(w http.ResponseWriter, r *http.Request) {
	id, err := s.requireUser(r)
	if err != nil {
		// Distinguish "no user yet" so the dashboard knows to show setup screen.
		ok, herr := s.identity.HasUser(r.Context())
		if herr == nil && !ok {
			writeJSON(w, http.StatusOK, map[string]any{"setup_required": true})
			return
		}
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	u, err := s.identity.GetUserByID(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	writeJSON(w, http.StatusOK, u)
}

// ---- agents ----------------------------------------------------------------

// agentsCollection handles GET (list) and POST (create-direct). The POST
// path is the one-step "name -> token" used by the dashboard so the
// operator gets a copy-pasteable agent token without doing the
// enrollment-code dance.
func (s *Server) agentsCollection(w http.ResponseWriter, r *http.Request) {
	uid, err := s.requireUser(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	switch r.Method {
	case http.MethodGet:
		agents, err := s.identity.ListAgents(r.Context(), uid)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		out := make([]map[string]any, 0, len(agents))
		for _, a := range agents {
			row := map[string]any{"id": a.ID, "name": a.Name}
			if !a.LastSeen.IsZero() {
				row["last_seen"] = a.LastSeen.UnixMilli()
			}
			out = append(out, row)
		}
		writeJSON(w, http.StatusOK, out)
	case http.MethodPost:
		var body struct{ Name string }
		if err := decode(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		token, ag, err := s.identity.CreateAgentWithToken(r.Context(), uid, strings.TrimSpace(body.Name))
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		_ = s.audit.Write(r.Context(), audit.Event{EventType: audit.EventAgentEnroll, AgentID: ag.ID, ResultSummary: ag.Name})
		writeJSON(w, http.StatusOK, map[string]any{
			"agent_id": ag.ID,
			"name":     ag.Name,
			"token":    token,
		})
	default:
		writeError(w, http.StatusMethodNotAllowed, "GET or POST")
	}
}

func (s *Server) agentsEnroll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	uid, err := s.requireUser(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var body struct{ Name string }
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Name == "" {
		body.Name = "agent"
	}
	code, _, err := s.identity.CreateEnrollment(r.Context(), uid, body.Name, enrollCodeTTL)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enrollment_code": code,
		"expires_in":      int(enrollCodeTTL.Seconds()),
	})
}

// agentsItem handles per-agent actions:
//
//	POST   /v1/agents/{id}/rotate   issue a fresh token, invalidate old hash
//	DELETE /v1/agents/{id}          remove the agent entirely
//
// Owner-scoped via identity.{Rotate,Delete}AgentToken — callers can't reach
// agents owned by other users (relevant the day toolyard goes multi-user).
func (s *Server) agentsItem(w http.ResponseWriter, r *http.Request) {
	uid, err := s.requireUser(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	tail := strings.TrimPrefix(r.URL.Path, "/v1/agents/")
	parts := strings.SplitN(tail, "/", 2)
	id := parts[0]
	if id == "" || id == "enroll" || id == "exchange" {
		http.NotFound(w, r)
		return
	}
	subpath := ""
	if len(parts) > 1 {
		subpath = parts[1]
	}
	switch {
	case subpath == "rotate" && r.Method == http.MethodPost:
		tok, err := s.identity.RotateAgentToken(r.Context(), uid, id)
		if err != nil {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		_ = s.audit.Write(r.Context(), audit.Event{
			EventType: "agent.rotate", AgentID: id, ResultSummary: "user:" + uid,
		})
		writeJSON(w, http.StatusOK, map[string]any{"agent_id": id, "token": tok})
	case subpath == "" && r.Method == http.MethodDelete:
		if err := s.identity.DeleteAgent(r.Context(), uid, id); err != nil {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		_ = s.audit.Write(r.Context(), audit.Event{
			EventType: "agent.delete", AgentID: id, ResultSummary: "user:" + uid,
		})
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		writeError(w, http.StatusMethodNotAllowed, "POST /rotate or DELETE")
	}
}

func (s *Server) agentsExchange(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var body struct{ Code string }
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	tok, ag, err := s.identity.ExchangeEnrollment(r.Context(), strings.TrimSpace(body.Code))
	if err != nil {
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}
	_ = s.audit.Write(r.Context(), audit.Event{EventType: audit.EventAgentEnroll, AgentID: ag.ID, ResultSummary: ag.Name})
	writeJSON(w, http.StatusOK, map[string]any{
		"agent_id": ag.ID,
		"token":    tok,
	})
}

// ---- approvals -------------------------------------------------------------

func (s *Server) approvalsList(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireUser(r); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	switch r.URL.Query().Get("status") {
	case "pending":
		out, err := s.approval.ListPending(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, out)
	default:
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		out, err := s.approval.Recent(r.Context(), limit)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, out)
	}
}

func (s *Server) approvalsOne(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/approvals/")
	if id == "" || id == "decide-by-token" || id == "decide-batch" {
		http.NotFound(w, r)
		return
	}
	if strings.HasSuffix(id, "/decide") {
		s.approvalsDecide(w, r, strings.TrimSuffix(id, "/decide"))
		return
	}
	// Authenticated GET only — the previous ?token= URL fallback was
	// dropped because query strings end up in proxy / browser-history
	// logs, leaking both the approval ID and a key that decides it. The
	// push notification flow uses POST /v1/approvals/decide-by-token
	// which keeps the token in the body.
	if r.Method == http.MethodGet {
		if _, err := s.requireUser(r); err != nil {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		req, err := s.approval.Get(r.Context(), id)
		if err != nil {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, req)
		return
	}
	writeError(w, http.StatusMethodNotAllowed, "GET or POST /decide")
}

func (s *Server) approvalsDecide(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	uid, err := s.requireUser(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var body struct{ Action string }
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	req, err := s.approval.Decide(r.Context(), id, body.Action, uid)
	if err != nil && !errors.Is(err, approval.ErrNotPending) {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, req)
}

// approvalsDecideBatch resolves several pending approvals in one shot. The
// dashboard's "Allow all / Deny all" buttons use it. Returns a per-id status
// array so the UI can show which ones flipped vs which were already
// resolved/expired.
func (s *Server) approvalsDecideBatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	uid, err := s.requireUser(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var body struct {
		IDs    []string `json:"ids"`
		Action string   `json:"action"`
	}
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(body.IDs) == 0 {
		writeError(w, http.StatusBadRequest, "ids is required")
		return
	}
	if body.Action != approval.StatusAllowed && body.Action != approval.StatusDenied {
		writeError(w, http.StatusBadRequest, "action must be allowed or denied")
		return
	}
	type result struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Error  string `json:"error,omitempty"`
	}
	out := make([]result, 0, len(body.IDs))
	for _, id := range body.IDs {
		req, err := s.approval.Decide(r.Context(), id, body.Action, uid)
		switch {
		case err == nil:
			out = append(out, result{ID: id, Status: req.Status})
		case errors.Is(err, approval.ErrNotPending) && req != nil:
			out = append(out, result{ID: id, Status: req.Status, Error: "not pending"})
		default:
			msg := "error"
			if err != nil {
				msg = err.Error()
			}
			out = append(out, result{ID: id, Error: msg})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) approvalsDecideByToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var body struct{ Token, Action string }
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	req, err := s.approval.DecideByToken(r.Context(), body.Token, body.Action)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, req)
}

// ---- audit -----------------------------------------------------------------

func (s *Server) auditList(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireUser(r); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	out, err := s.audit.Recent(r.Context(), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// eventsStream pushes both approval lifecycle events and audit rows. The
// dashboard subscribes once and demuxes by event type.
func (s *Server) eventsStream(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireUser(r); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	s.hub.ServeSSE(w, r)
}

// ---- memory ----------------------------------------------------------------

func (s *Server) memoryHandler(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireUser(r); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	q := r.URL.Query()
	switch r.Method {
	case http.MethodGet:
		if k := q.Get("key"); k != "" {
			e, err := s.memory.Get(r.Context(), q.Get("scope"), k)
			if err != nil {
				writeError(w, http.StatusNotFound, err.Error())
				return
			}
			writeJSON(w, http.StatusOK, e)
			return
		}
		entries, err := s.memory.List(r.Context(), q.Get("scope"), q.Get("prefix"))
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, entries)
	case http.MethodPost:
		var body struct{ Scope, Key, Value string }
		if err := decode(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		e, err := s.memory.Set(r.Context(), body.Scope, body.Key, body.Value)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, e)
	case http.MethodDelete:
		if err := s.memory.Delete(r.Context(), q.Get("scope"), q.Get("key")); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		writeError(w, http.StatusMethodNotAllowed, "GET, POST, DELETE")
	}
}

func (s *Server) memoryList(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireUser(r); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	q := r.URL.Query()
	entries, err := s.memory.List(r.Context(), q.Get("scope"), q.Get("prefix"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, entries)
}

// ---- push ------------------------------------------------------------------

func (s *Server) pushVapidKey(w http.ResponseWriter, r *http.Request) {
	if s.push == nil {
		writeJSON(w, http.StatusOK, map[string]string{})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"public_key": s.push.PublicKey()})
}

func (s *Server) pushSubscribe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	uid, err := s.requireUser(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var body struct {
		Endpoint string `json:"endpoint"`
		Keys     struct {
			P256dh string `json:"p256dh"`
			Auth   string `json:"auth"`
		} `json:"keys"`
	}
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	sub, err := s.push.Subscribe(r.Context(), uid, body.Endpoint, body.Keys.P256dh, body.Keys.Auth, r.UserAgent())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sub)
}

// ---- servers (upstream MCP) ------------------------------------------------

func (s *Server) serversCollection(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireUser(r); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	switch r.Method {
	case http.MethodGet:
		out, err := s.upstreams.List(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if out == nil {
			out = []upstreams.Server{}
		}
		// annotate live tool counts so the dashboard does not need a second call
		for i := range out {
			if s.gateway != nil {
				if c := s.gateway.UpstreamToolCount(out[i].Name); c > 0 {
					out[i].ToolCount = c
				}
			}
		}
		writeJSON(w, http.StatusOK, out)
	case http.MethodPost:
		var body upstreams.Server
		if err := decode(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		srv, err := s.upstreams.Add(r.Context(), body)
		if err != nil {
			// If the row was created but the connect failed, return 207 with the
			// row body so the dashboard shows the persisted error. Otherwise 400.
			if srv != nil && (errors.Is(err, gateway.ErrUpstreamNotFound) || !errors.Is(err, upstreams.ErrInvalid)) && !errors.Is(err, upstreams.ErrAlreadyHere) && !errors.Is(err, upstreams.ErrReserved) {
				writeJSON(w, http.StatusAccepted, map[string]any{
					"server":     srv,
					"warning":    err.Error(),
				})
				return
			}
			status := http.StatusBadRequest
			if errors.Is(err, upstreams.ErrAlreadyHere) {
				status = http.StatusConflict
			}
			writeError(w, status, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, srv)
	default:
		writeError(w, http.StatusMethodNotAllowed, "GET or POST")
	}
}

func (s *Server) serversItem(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireUser(r); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	tail := strings.TrimPrefix(r.URL.Path, "/v1/servers/")
	if tail == "" {
		http.NotFound(w, r)
		return
	}
	parts := strings.SplitN(tail, "/", 2)
	name := parts[0]
	subpath := ""
	if len(parts) > 1 {
		subpath = parts[1]
	}
	if subpath == "reconnect" {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "POST")
			return
		}
		srv, err := s.upstreams.Reconnect(r.Context(), name)
		if err != nil {
			if srv != nil {
				writeJSON(w, http.StatusAccepted, map[string]any{"server": srv, "warning": err.Error()})
				return
			}
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, srv)
		return
	}
	if r.Method == http.MethodDelete {
		if err := s.upstreams.Remove(r.Context(), name); err != nil {
			if errors.Is(err, upstreams.ErrNotFound) {
				writeError(w, http.StatusNotFound, err.Error())
				return
			}
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	writeError(w, http.StatusMethodNotAllowed, "DELETE")
}

// marketplaceList returns the curated MCP recipes. We require a logged-in
// user so we don't accidentally expose the catalog (small, but still
// confirms the operator's identity before suggesting installs that may want
// secrets).
func (s *Server) marketplaceList(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireUser(r); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	writeJSON(w, http.StatusOK, marketplace.Catalog())
}

func (s *Server) toolsList(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireUser(r); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if s.gateway == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	writeJSON(w, http.StatusOK, s.gateway.Catalog())
}

// toolsRun lets the dashboard's "workbench" UI try any registered tool.
// Body: {"tool":"<name>", "arguments":{...}}. Calls go through the same
// policy + approval path as a regular MCP call; if a write needs approval
// the response is the deferred CallToolResult payload, and the dashboard's
// Approvals tab will show the pending request.
func (s *Server) toolsRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	uid, err := s.requireUser(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if s.gateway == nil {
		writeError(w, http.StatusServiceUnavailable, "gateway not wired")
		return
	}
	var body struct {
		Tool      string         `json:"tool"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(body.Tool) == "" {
		writeError(w, http.StatusBadRequest, "tool is required")
		return
	}
	if body.Arguments == nil {
		body.Arguments = map[string]any{}
	}
	// Tag the call as coming from the dashboard so audit/approval rows show
	// the actual operator instead of an empty agent_id.
	ctx := gateway.WithAgentID(r.Context(), "dashboard:"+uid)
	res, err := s.gateway.RouteCall(ctx, "dashboard", body.Tool, body.Arguments)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Marshal the CallToolResult: we mirror MCP's shape so the UI can
	// handle errors / deferred responses uniformly with what an agent sees.
	out := map[string]any{
		"is_error":           res.IsError,
		"content":            res.Content,
		"structured_content": res.StructuredContent,
	}
	if res.Meta != nil {
		out["_meta"] = res.Meta.AdditionalFields
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- settings -------------------------------------------------------------

func (s *Server) settingsHandler(w http.ResponseWriter, r *http.Request) {
	uid, err := s.requireUser(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if s.settings == nil {
		writeJSON(w, http.StatusOK, map[string]any{})
		return
	}
	switch r.Method {
	case http.MethodGet:
		out, err := s.settings.All(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, out)
	case http.MethodPatch, http.MethodPost:
		var body map[string]any
		if err := decode(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := s.settings.Patch(r.Context(), body); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		// Forensic trail: who changed what. Keys recorded; values omitted
		// so we don't end up logging an API-rate fragment as an event row.
		keys := make([]string, 0, len(body))
		for k := range body {
			keys = append(keys, k)
		}
		_ = s.audit.Write(r.Context(), audit.Event{
			EventType:     "settings.patch",
			AgentID:       "user:" + uid,
			ResultSummary: strings.Join(keys, ","),
		})
		// If the change affects what tools agents can see, push a
		// notifications/tools/list_changed so connected MCP clients
		// re-fetch instead of relying on their cached tool list.
		if s.gateway != nil {
			for _, k := range []string{"router_only_mode", "surface_mode", "top_n_count", "top_n_personalize_after"} {
				if _, touched := body[k]; touched {
					s.gateway.NotifyToolListChanged()
					break
				}
			}
		}
		out, _ := s.settings.All(r.Context())
		writeJSON(w, http.StatusOK, out)
	default:
		writeError(w, http.StatusMethodNotAllowed, "GET, PATCH")
	}
}

// ---- usage ----------------------------------------------------------------

func (s *Server) usageHandler(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireUser(r); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if s.usage == nil {
		writeJSON(w, http.StatusOK, map[string]any{"per_tool": map[string]int64{}, "rows": []any{}})
		return
	}
	agentID := r.URL.Query().Get("agent_id")
	rows, err := s.usage.All(r.Context(), agentID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if rows == nil {
		rows = []usage.Row{}
	}
	perTool, err := s.usage.AggregatePerTool(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"per_tool": perTool,
		"rows":     rows,
	})
}

// ---- placeholder for context.Background usage ------------------------------

var _ = context.Background
