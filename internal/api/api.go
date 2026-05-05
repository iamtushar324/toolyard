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
	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/memory"
	"github.com/tusharbhardwaj/toolyard/internal/push"
	"github.com/tusharbhardwaj/toolyard/internal/realtime"
)

const (
	sessionCookieName = "toolyard_session"
	sessionTTL        = 24 * time.Hour
	enrollCodeTTL     = 15 * time.Minute
)

type Server struct {
	identity   *identity.Service
	approval   *approval.Bus
	audit      *audit.Logger
	memory     *memory.Service
	push       *push.Service
	hub        *realtime.Hub
	sessionKey []byte
}

type Options struct {
	Identity   *identity.Service
	Approval   *approval.Bus
	Audit      *audit.Logger
	Memory     *memory.Service
	Push       *push.Service
	Hub        *realtime.Hub
	SessionKey []byte
}

func New(opts Options) *Server {
	return &Server{
		identity:   opts.Identity,
		approval:   opts.Approval,
		audit:      opts.Audit,
		memory:     opts.Memory,
		push:       opts.Push,
		hub:        opts.Hub,
		sessionKey: opts.SessionKey,
	}
}

func (s *Server) Routes(mux *http.ServeMux) {
	mux.HandleFunc("/v1/health", s.health)
	mux.HandleFunc("/v1/auth/setup", s.authSetup)
	mux.HandleFunc("/v1/auth/login", s.authLogin)
	mux.HandleFunc("/v1/auth/logout", s.authLogout)
	mux.HandleFunc("/v1/auth/me", s.authMe)

	mux.HandleFunc("/v1/agents", s.agentsList)
	mux.HandleFunc("/v1/agents/enroll", s.agentsEnroll)
	mux.HandleFunc("/v1/agents/exchange", s.agentsExchange)

	mux.HandleFunc("/v1/approvals", s.approvalsList)
	mux.HandleFunc("/v1/approvals/", s.approvalsOne)
	mux.HandleFunc("/v1/approvals/decide-by-token", s.approvalsDecideByToken)

	mux.HandleFunc("/v1/audit", s.auditList)
	mux.HandleFunc("/v1/events/stream", s.eventsStream)

	mux.HandleFunc("/v1/memory", s.memoryHandler)
	mux.HandleFunc("/v1/memory/list", s.memoryList)

	mux.HandleFunc("/v1/push/vapid_key", s.pushVapidKey)
	mux.HandleFunc("/v1/push/subscribe", s.pushSubscribe)
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

func decode(r *http.Request, dst any) error {
	if r.Body == nil {
		return errors.New("empty body")
	}
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(dst)
}

// ---- auth & session ---------------------------------------------------------

type sessionClaims struct {
	UserID string `json:"sub"`
	jwt.RegisteredClaims
}

func (s *Server) issueSession(w http.ResponseWriter, userID string) error {
	claims := sessionClaims{
		UserID:           userID,
		RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(time.Now().Add(sessionTTL))},
	}
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(s.sessionKey)
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    tok,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
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
	if c, ok := parsed.Claims.(*sessionClaims); ok {
		return c.UserID, true
	}
	return "", false
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
	if err := s.issueSession(w, u.ID); err != nil {
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
	if err := s.issueSession(w, u.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.audit.Write(r.Context(), audit.Event{EventType: audit.EventUserLogin, ResultSummary: u.Username})
	writeJSON(w, http.StatusOK, u)
}

func (s *Server) authLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
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

func (s *Server) agentsList(w http.ResponseWriter, r *http.Request) {
	uid, err := s.requireUser(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
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
	if id == "" || id == "decide-by-token" {
		http.NotFound(w, r)
		return
	}
	if strings.HasSuffix(id, "/decide") {
		s.approvalsDecide(w, r, strings.TrimSuffix(id, "/decide"))
		return
	}
	// Allow unauthenticated GET when the request carries a valid signed
	// decision token in ?token= so the phone push deep-link works without a
	// session cookie.
	if r.Method == http.MethodGet {
		if _, err := s.requireUser(r); err != nil {
			if tok := r.URL.Query().Get("token"); tok != "" {
				if req, terr := s.approval.Get(r.Context(), id); terr == nil && req.DecisionToken == tok {
					writeJSON(w, http.StatusOK, req)
					return
				}
			}
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

// ---- placeholder for context.Background usage ------------------------------

var _ = context.Background
