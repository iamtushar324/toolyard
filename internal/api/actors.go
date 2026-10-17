package api

// actors.go names the people behind API requests: the signed-in
// dashboard user as decider (approvals, inbox, grants) or raiser (the
// workbench), and the bearer agent with its owner as raiser (the CLI, a
// hook script), so every audit row, approval and inbox decision records
// who did it and how.

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/tusharbhardwaj/toolyard/internal/actor"
	"github.com/tusharbhardwaj/toolyard/internal/gateway"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
)

// Raiser.ClientKind and Via values the REST entry points record.
const (
	clientKindBrowser = "browser"
	clientKindCLI     = "cli"
	viaDashboard      = "dashboard"
	viaCLI            = "cli"
	viaHook           = "hook"
	// headerClientKind lets a REST client name itself (the CLI sends
	// "cli"): a known kind is recorded as is, anything else goes through
	// the gateway's clientInfo mapping (so "claude-code/1.2" still reads
	// claude_code, and a value we have never seen is "unknown").
	headerClientKind = "x-toolyard-client"
)

// knownClientKinds are the Raiser.ClientKind values a client may claim
// outright.
var knownClientKinds = map[string]bool{
	"t3": true, "claude_code": true, "codex": true, "cursor": true, "opencode": true, clientKindCLI: true, clientKindBrowser: true,
}

// declaredClientKind is the kind the request's x-toolyard-client header
// declares, or "" without one.
func declaredClientKind(r *http.Request) string {
	v := strings.ToLower(actor.Clean(r.Header.Get(headerClientKind)))
	switch {
	case v == "":
		return ""
	case knownClientKinds[v]:
		return v
	}
	return gateway.ClientKindOf(v)
}

// requireUserFull is requireUser returning the whole user, for handlers
// that record the person's email and name.
func (s *Server) requireUserFull(r *http.Request) (*identity.User, error) {
	u, ok := s.sessionUser(r)
	if !ok {
		return nil, errors.New("unauthorized")
	}
	return u, nil
}

// dashboardDecider is the signed-in user deciding through the dashboard.
// via says which route (dashboard, dashboard_batch, push_token with a
// session); Ref is their dashboard session id, the instrument that
// identified them.
func (s *Server) dashboardDecider(r *http.Request, u *identity.User, via string) actor.Decider {
	return actor.Decider{UserID: u.ID, Email: u.Email, Name: u.Label(), Via: via, Ref: s.currentSessionID(r)}
}

// batchDecider is dashboardDecider for one "Allow all / Deny all" click:
// Via dashboard_batch and one batch id as Ref on every row it decides.
func (s *Server) batchDecider(r *http.Request, u *identity.User) actor.Decider {
	d := s.dashboardDecider(r, u, actor.ViaDashboardBatch)
	d.Ref = "batch_" + uuid.NewString()
	return d
}

// dashboardRaiser is the signed-in user raising a call from the
// dashboard's workbench: caller "dashboard:<uid>", owner themselves,
// client their browser.
func (s *Server) dashboardRaiser(r *http.Request, u *identity.User) actor.Raiser {
	rs := actor.Raiser{
		CallerID:    "dashboard:" + u.ID,
		AgentKind:   viaDashboard,
		OwnerUserID: u.ID,
		OwnerEmail:  u.Email,
		OwnerName:   u.Label(),
		ClientIP:    s.security.ClientIP(r),
		ClientKind:  clientKindBrowser,
		Via:         viaDashboard,
	}
	// Raised by a CLI agent with an operator token: same owner and the same
	// approval path, but named as the CLI so the approval card says so.
	if t := operatorFromContext(r.Context()); t != nil {
		rs.AgentName = "operator: " + t.Name
		rs.ClientKind, rs.ClientName, rs.Via = "cli", t.ID, "cli"
	}
	return rs
}

// agentRaiser is a bearer-token agent raising a call over REST: the
// agent, its owner, the client IP, the kind the client declared (else
// fallback) and the path in.
func (s *Server) agentRaiser(r *http.Request, ag *identity.Agent, fallbackKind, via string) actor.Raiser {
	kind := declaredClientKind(r)
	if kind == "" {
		kind = fallbackKind
	}
	return actor.Raiser{
		CallerID:    ag.ID,
		AgentName:   ag.Name,
		AgentKind:   ag.Kind,
		OwnerUserID: ag.Owner,
		OwnerEmail:  ag.OwnerEmail,
		OwnerName:   ag.OwnerName(),
		ClientIP:    s.security.ClientIP(r),
		ClientKind:  kind,
		Via:         via,
	}
}

// requireAgentFull resolves the bearer token to its agent, owner details
// included, writing the 401 itself when it can't.
func (s *Server) requireAgentFull(w http.ResponseWriter, r *http.Request) (*identity.Agent, bool) {
	authz := r.Header.Get("Authorization")
	if !strings.HasPrefix(authz, "Bearer ") {
		writeError(w, http.StatusUnauthorized, "bearer token required")
		return nil, false
	}
	ag, err := s.verifyAgentToken(r.Context(), strings.TrimPrefix(authz, "Bearer "))
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid agent token")
		return nil, false
	}
	return ag, true
}

func (s *Server) verifyAgentToken(ctx context.Context, token string) (*identity.Agent, error) {
	if s.federation != nil {
		return s.federation.VerifyAgent(ctx, token)
	}
	return s.identity.VerifyAgentToken(ctx, token)
}

// verifySessionToken resolves a session token ("tys_…") to its row and
// agent. With federation on it also checks the agent's connection, as
// verifyAgentToken does for agent tokens.
func (s *Server) verifySessionToken(ctx context.Context, token string) (*identity.SessionToken, *identity.Agent, error) {
	if s.federation != nil {
		return s.federation.VerifySessionToken(ctx, token)
	}
	return s.identity.VerifySessionToken(ctx, token)
}

// Errors codeCaller returns; writeCallerError maps each to its answer.
var (
	errBearerRequired      = errors.New("bearer token required")
	errOperatorToken       = errors.New("not available to operator tokens")
	errInvalidAgentToken   = errors.New("invalid agent token")
	errInvalidSessionToken = errors.New("invalid session token")
	// errCallerCheckFailed: the token could not be checked (a database
	// failure), which says nothing about the token itself.
	errCallerCheckFailed = errors.New("authentication unavailable, try again")
)

// bearerToken returns the token in r's "Authorization: Bearer" header.
func bearerToken(r *http.Request) (string, bool) {
	authz := r.Header.Get("Authorization")
	if !strings.HasPrefix(authz, "Bearer ") {
		return "", false
	}
	tok := strings.TrimSpace(strings.TrimPrefix(authz, "Bearer "))
	return tok, tok != ""
}

// callerError maps a token verification error to invalid when the token
// was refused, or to errCallerCheckFailed when the check itself failed.
func callerError(err, invalid error) error {
	if errors.Is(err, identity.ErrAgentTokenInvalid) || errors.Is(err, identity.ErrSessionTokenInvalid) {
		return invalid
	}
	return errCallerCheckFailed
}

// codeCaller resolves the bearer of a code-mode request (/v1/code/*) to its
// agent, owner details included. It accepts an agent token, or a session
// token ("tys_…") minted with one: then sessionID is the session token's
// client session id and viaSession is true. An operator token
// ("tyop_…") is errOperatorToken: the operator guard normally answers 403
// before a handler runs. writeCallerError writes the answer for any error.
func (s *Server) codeCaller(r *http.Request) (ag *identity.Agent, sessionID string, viaSession bool, err error) {
	tok, ok := bearerToken(r)
	if !ok {
		return nil, "", false, errBearerRequired
	}
	switch {
	case strings.HasPrefix(tok, identity.OperatorTokenPrefix):
		return nil, "", false, errOperatorToken
	case identity.IsSessionToken(tok):
		t, a, e := s.verifySessionToken(r.Context(), tok)
		if e != nil {
			return nil, "", false, callerError(e, errInvalidSessionToken)
		}
		return a, t.SessionID, true, nil
	}
	a, e := s.verifyAgentToken(r.Context(), tok)
	if e != nil {
		return nil, "", false, callerError(e, errInvalidAgentToken)
	}
	return a, "", false, nil
}

// writeCallerError writes the answer for a codeCaller error: 401 without a
// token or with a refused one, 403 for an operator token, 503 when the
// token could not be checked.
func writeCallerError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errOperatorToken):
		writeError(w, http.StatusForbidden, errOperatorToken.Error())
	case errors.Is(err, errBearerRequired), errors.Is(err, errInvalidAgentToken), errors.Is(err, errInvalidSessionToken):
		writeError(w, http.StatusUnauthorized, err.Error())
	default:
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusServiceUnavailable, errCallerCheckFailed.Error())
	}
}

// codeRaiser is agentRaiser for a caller codeCaller resolved. With a
// session token, its session id is recorded as a verified client session
// (ClientSessionClaimed false): toolyard minted that token for that session
// to the agent's own token, so it is not the caller's unchecked word.
func (s *Server) codeRaiser(r *http.Request, ag *identity.Agent, sessionID string, viaSession bool, fallbackKind, via string) actor.Raiser {
	rs := s.agentRaiser(r, ag, fallbackKind, via)
	if viaSession && sessionID != "" {
		rs.ClientSessionID, rs.ClientSessionClaimed = sessionID, false
	}
	return rs
}
