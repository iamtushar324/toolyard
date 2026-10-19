package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
)

// Session tokens (docs/design/code-rest-route.md, "Session tokens"): an
// agent mints a short-lived token for one client session with its agent
// token, and the session uses it on the code-mode REST routes. Both routes
// are bearer-only: they never read a cookie.
//
//	POST   /v1/agents/session-tokens             agent token only
//	DELETE /v1/agents/session-tokens/{token_id}  the agent token, or the session token itself

const (
	sessionTokensPath   = "/v1/agents/session-tokens"
	sessionTokensPrefix = sessionTokensPath + "/"
)

// Audit event types for session tokens. The token itself is never logged.
const (
	eventSessionTokenMint   = "agent.session_token_mint"
	eventSessionTokenRevoke = "agent.session_token_revoke"
)

// registerSessionTokenRoutes adds the session token routes to mux. The
// paths are literals so the route scan in role_guard_test.go reads them.
func (s *Server) registerSessionTokenRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/v1/agents/session-tokens", s.sessionTokensCollection)
	mux.HandleFunc("/v1/agents/session-tokens/", s.sessionTokensItem)
}

// sessionTokenView is the 201 answer to a mint.
type sessionTokenView struct {
	Token     string `json:"token"`
	TokenID   string `json:"token_id"`
	AgentID   string `json:"agent_id"`
	SessionID string `json:"session_id"`
	ExpiresAt int64  `json:"expires_at"`
}

// POST /v1/agents/session-tokens {"session_id": "...", "ttl_seconds": 3600}
//
// Agent token only: a session token cannot mint another one (403), nor can
// an operator token (403). ttl_seconds is optional; it is clamped to
// 60..3600 and 0 means the default (3600).
func (s *Server) sessionTokensCollection(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	tok, ok := bearerToken(r)
	if !ok {
		writeCallerError(w, errBearerRequired)
		return
	}
	switch {
	case strings.HasPrefix(tok, identity.OperatorTokenPrefix):
		writeCallerError(w, errOperatorToken)
		return
	case identity.IsSessionToken(tok):
		if _, _, err := s.verifySessionToken(r.Context(), tok); err != nil {
			writeCallerError(w, callerError(err, errInvalidSessionToken))
			return
		}
		writeError(w, http.StatusForbidden, "a session token cannot mint session tokens")
		return
	}
	ag, err := s.verifyAgentToken(r.Context(), tok)
	if err != nil {
		writeCallerError(w, callerError(err, errInvalidAgentToken))
		return
	}
	var body struct {
		SessionID  string `json:"session_id"`
		TTLSeconds *int64 `json:"ttl_seconds"`
	}
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !identity.ValidSessionID(body.SessionID) {
		writeError(w, http.StatusBadRequest, identity.ErrSessionIDInvalid.Error())
		return
	}
	var ttl time.Duration // 0: the default
	if body.TTLSeconds != nil {
		secs := *body.TTLSeconds
		if secs < 0 {
			writeError(w, http.StatusBadRequest, "ttl_seconds must be between 60 and 3600")
			return
		}
		// Clamp in seconds first: a huge value would overflow a Duration.
		if maxSecs := int64(identity.MaxSessionTokenTTL / time.Second); secs > maxSecs {
			secs = maxSecs
		}
		ttl = time.Duration(secs) * time.Second
	}
	token, t, err := s.identity.MintSessionToken(r.Context(), ag.ID, tok, body.SessionID, ttl)
	switch {
	case errors.Is(err, identity.ErrSessionIDInvalid):
		writeError(w, http.StatusBadRequest, err.Error())
		return
	case errors.Is(err, identity.ErrAgentTokenInvalid):
		writeCallerError(w, errInvalidAgentToken)
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "could not mint a session token")
		return
	}
	s.auditSessionToken(r, ag, eventSessionTokenMint, t, "", false)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, sessionTokenView{
		Token: token, TokenID: t.ID, AgentID: t.AgentID, SessionID: t.SessionID, ExpiresAt: t.ExpiresAt,
	})
}

// DELETE /v1/agents/session-tokens/{token_id}
//
// With the agent token: 204 for a token of this agent (revoked now or
// already), and for an id that does not exist; 404 for another agent's
// token. With a session token: only that token itself (204, also when it
// has already expired or been revoked); any other id is 403. An operator
// token is 403.
func (s *Server) sessionTokensItem(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, sessionTokensPrefix)
	if id == "" || strings.Contains(id, "/") {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "DELETE only")
		return
	}
	tok, ok := bearerToken(r)
	if !ok {
		writeCallerError(w, errBearerRequired)
		return
	}
	switch {
	case strings.HasPrefix(tok, identity.OperatorTokenPrefix):
		writeCallerError(w, errOperatorToken)
		return
	case identity.IsSessionToken(tok):
		s.revokeOwnSessionToken(w, r, tok, id)
		return
	}
	ag, err := s.verifyAgentToken(r.Context(), tok)
	if err != nil {
		writeCallerError(w, callerError(err, errInvalidAgentToken))
		return
	}
	switch err := s.identity.RevokeSessionToken(r.Context(), ag.ID, id); {
	case errors.Is(err, identity.ErrSessionTokenNotFound):
		writeError(w, http.StatusNotFound, "session token not found")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "could not revoke the session token")
		return
	}
	s.auditSessionToken(r, ag, eventSessionTokenRevoke, &identity.SessionToken{ID: id}, "", false)
	w.WriteHeader(http.StatusNoContent)
}

// revokeOwnSessionToken answers a DELETE authenticated by the session token
// tok. The holder may always revoke the token itself, so a retried
// clean-up after expiry or an earlier revoke is still 204; a valid session
// token naming another id is 403.
func (s *Server) revokeOwnSessionToken(w http.ResponseWriter, r *http.Request, tok, id string) {
	ownID, _ := identity.SessionTokenID(tok)
	if ownID == "" || ownID != id {
		if _, _, err := s.verifySessionToken(r.Context(), tok); err != nil {
			writeCallerError(w, callerError(err, errInvalidSessionToken))
			return
		}
		writeError(w, http.StatusForbidden, "a session token can revoke only itself")
		return
	}
	t, err := s.identity.RevokeOwnSessionToken(r.Context(), tok)
	if err != nil {
		writeCallerError(w, callerError(err, errInvalidSessionToken))
		return
	}
	// The agent may be gone or disabled by now; the audit row still names it.
	ag, aerr := s.identity.VerifyAgentTokenHash(r.Context(), t.AgentID, t.ParentTokenHash)
	if aerr != nil {
		ag = &identity.Agent{ID: t.AgentID}
	}
	s.auditSessionToken(r, ag, eventSessionTokenRevoke, t, t.SessionID, true)
	w.WriteHeader(http.StatusNoContent)
}

// auditSessionToken records a session token mint or revoke: the agent, its
// owner and the token id (never the token). A revoke made with the session
// token itself carries its verified client session id.
func (s *Server) auditSessionToken(r *http.Request, ag *identity.Agent, eventType string, t *identity.SessionToken, sessionID string, viaSession bool) {
	if s.audit == nil || ag == nil || t == nil {
		return
	}
	ev := audit.Event{EventType: eventType, AgentID: ag.ID, ResultSummary: t.ID}
	if t.SessionID != "" {
		if args, err := json.Marshal(map[string]any{"session_id": t.SessionID, "expires_at": t.ExpiresAt}); err == nil {
			ev.Arguments = args
		}
	}
	ev.Raiser = s.codeRaiser(r, ag, sessionID, viaSession, clientKindCLI, viaCLI)
	_ = s.audit.Write(context.WithoutCancel(r.Context()), ev)
}
