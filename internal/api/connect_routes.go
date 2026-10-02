package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/clerk"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/identitykeys"
	"github.com/tusharbhardwaj/toolyard/internal/logx"
)

// POST /v1/connect/t3 connects a person signed in to bkt3 (our T3 Code
// fork) to toolyard. Both apps sign people in through the same Clerk
// instance and organisation; bkt3's server posts a fresh Clerk session
// token its browser minted (azp = the bkt3 origin) and gets back that
// person's toolyard agent token, which it then sends as a bearer on /mcp.
//
//	POST /v1/connect/t3 {"token":"<clerk session JWT>"}
//	200 {"token":"<agent token>","email":"…","agent_id":"ag_…","user_id":"u_…"}
//
// verify (RS256, exp/nbf, iss, azp in -connect-azp) -> org membership ->
// local user upsert (as the dashboard sign-in does) -> identity key issued
// if missing -> create or rotate the person's one "T3 Code (bkt3)" agent.
// Errors are {"error":"<code>"}: 400 bad_request, 401 invalid_token,
// 403 not_org_member, 403 user_disabled, 403 agent_disabled (the person
// disabled that agent), 429 rate_limited, 503 clerk_unavailable, 404
// connect_disabled when no connect origins are configured.
//
// Server to server: no cookie, no Origin, no X-Requested-With; the Clerk
// token in the body is the credential, so it is exempt from the CSRF
// header and Origin checks (security.go) and needs no toolyard session.

const (
	// connectT3AgentName is the one agent per person bkt3 uses on /mcp.
	connectT3AgentName = "T3 Code (bkt3)"
	// connectT3Actor issues a missing identity key: the actor on the
	// identity_key.* audit rows and registry rows Issue writes.
	connectT3Actor = "connect:t3"
	// connectT3EventType is the audit event of every attempt that got as
	// far as reading the body: the outcome, or the refusal, in Reason.
	connectT3EventType = "connect.t3"
	// connectT3MaxBody: a Clerk session token is a couple of KiB; HardenAPI
	// already caps this route lower (perRouteBodyCap's default).
	connectT3MaxBody = 32 << 10
)

// Throttle budgets. Every call comes from bkt3's server, so one client IP
// carries the whole org: its budget is wide, and the per-person one (by
// verified Clerk subject) is the tight one. A legitimate person needs one
// call per bkt3 sign-in.
const (
	connectT3IPMaxAttempts      = 600
	connectT3SubjectMaxAttempts = 30
	connectT3Window             = 15 * time.Minute
)

// errConnectAgentDisabled: the person's bkt3 agent exists but is disabled
// (a hard revoke from the Agents page). Connect respects it rather than
// handing out a token that /mcp would refuse.
var errConnectAgentDisabled = errors.New("connect: the bkt3 agent is disabled")

// connectVerifier is the slice of *clerk.Client the connect route uses.
type connectVerifier interface {
	VerifySessionTokenFor(ctx context.Context, token string, authorizedParties []string) (clerk.Claims, error)
	OrgMembership(ctx context.Context, clerkUserID string) (clerk.Member, error)
}

// connectConfig is the state of POST /v1/connect/t3; nil on the Server when
// the endpoint is off.
type connectConfig struct {
	clerk   connectVerifier
	origins []string
	// Throttle budgets; fields so tests can shrink them.
	ipMax, subjectMax int
	window            time.Duration
	// locks serialises connects of one person so two concurrent calls
	// can't issue two identity keys or create two bkt3 agents.
	locks keyedMutex
}

// newConnectT3 returns the connect state, or nil (endpoint off) when Clerk
// is off or no connect origin is configured. Origins are normalised as
// clerk compares them: lowercase, no trailing slash.
func newConnectT3(c *clerk.Client, origins []string) *connectConfig {
	if c == nil {
		return nil
	}
	var norm []string
	seen := map[string]bool{}
	for _, o := range origins {
		if o = clerk.NormalizeOrigin(o); o != "" && !seen[o] {
			seen[o] = true
			norm = append(norm, o)
		}
	}
	if len(norm) == 0 {
		return nil
	}
	return &connectConfig{
		clerk:      c,
		origins:    norm,
		ipMax:      connectT3IPMaxAttempts,
		subjectMax: connectT3SubjectMaxAttempts,
		window:     connectT3Window,
	}
}

// connectAttempt is what one call's audit row says. Ids, origins and codes
// only: never the Clerk token or the agent token.
type connectAttempt struct {
	ip       string
	azp      string
	sub      string
	user     *identity.User
	agentID  string
	keyState string
}

func (s *Server) connectT3(w http.ResponseWriter, r *http.Request) {
	cfg := s.connect
	if cfg == nil {
		writeError(w, http.StatusNotFound, "connect_disabled")
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	ctx := r.Context()
	at := &connectAttempt{ip: s.security.ClientIP(r)}
	// Per-IP first, before any work; not audited, so a flood can't turn
	// into a flood of audit writes.
	if !s.unauthLimit.AllowN("connect-t3-ip:"+at.ip, cfg.ipMax, cfg.window) {
		writeError(w, http.StatusTooManyRequests, "rate_limited")
		return
	}
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, connectT3MaxBody)
	}
	var body struct {
		Token string `json:"token"`
	}
	if err := decode(r, &body); err != nil || strings.TrimSpace(body.Token) == "" {
		s.connectRefuse(ctx, w, at, http.StatusBadRequest, "bad_request", nil)
		return
	}
	claims, err := cfg.clerk.VerifySessionTokenFor(ctx, body.Token, cfg.origins)
	if err != nil {
		if errors.Is(err, clerk.ErrUnavailable) {
			s.connectRefuse(ctx, w, at, http.StatusServiceUnavailable, "clerk_unavailable", err)
			return
		}
		s.connectRefuse(ctx, w, at, http.StatusUnauthorized, "invalid_token", err)
		return
	}
	at.azp, at.sub = claims.AuthorizedParty, claims.Subject
	if !s.unauthLimit.AllowN("connect-t3-sub:"+claims.Subject, cfg.subjectMax, cfg.window) {
		s.connectRefuse(ctx, w, at, http.StatusTooManyRequests, "rate_limited", nil)
		return
	}
	member, err := cfg.clerk.OrgMembership(ctx, claims.Subject)
	if err != nil {
		if errors.Is(err, clerk.ErrNotMember) {
			s.connectRefuse(ctx, w, at, http.StatusForbidden, "not_org_member", nil)
			return
		}
		// ErrUnavailable and anything unexpected: fail closed.
		s.connectRefuse(ctx, w, at, http.StatusServiceUnavailable, "clerk_unavailable", err)
		return
	}
	u, err := s.upsertClerkMember(ctx, claims.Subject, member)
	if err != nil {
		s.connectRefuse(ctx, w, at, http.StatusInternalServerError, "internal_error", err)
		return
	}
	at.user = u
	if u.Status != identity.StatusActive {
		s.connectRefuse(ctx, w, at, http.StatusForbidden, "user_disabled", nil)
		return
	}

	unlock := cfg.locks.lock(u.ID)
	defer unlock()
	at.keyState, err = s.connectIdentityKey(ctx, u.ID)
	if err != nil {
		s.connectRefuse(ctx, w, at, http.StatusInternalServerError, "internal_error", err)
		return
	}
	token, agentID, outcome, err := s.connectAgent(ctx, u.ID)
	at.agentID = agentID
	if errors.Is(err, errConnectAgentDisabled) {
		s.connectRefuse(ctx, w, at, http.StatusForbidden, "agent_disabled", nil)
		return
	}
	if err != nil {
		s.connectRefuse(ctx, w, at, http.StatusInternalServerError, "internal_error", err)
		return
	}
	s.connectAudit(ctx, at, "", outcome)
	logx.For("connect-t3").Info("connected",
		"outcome", outcome, "user", u.ID, "agent", agentID, "azp", at.azp, "identity_key", at.keyState)
	// The body carries a credential: no cache may keep it.
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]string{
		"token":    token,
		"email":    u.Email,
		"agent_id": agentID,
		"user_id":  u.ID,
	})
}

// connectIdentityKey makes sure the person has a Beknown identity key: it
// is issued here (as connect:t3) when missing and never rotated. A
// registry failure inside Issue is recorded there and is not an error; a
// person without a Clerk email can't be registered, so their key is left
// for an admin. It reports what it did for the audit row.
func (s *Server) connectIdentityKey(ctx context.Context, userID string) (string, error) {
	if s.identityKeys == nil {
		return "off", nil
	}
	has, err := s.identityKeys.HasKey(ctx, userID)
	if err != nil {
		return "", err
	}
	if has {
		return "present", nil
	}
	_, err = s.identityKeys.Issue(ctx, connectT3Actor, userID)
	switch {
	case err == nil:
		return "issued", nil
	case errors.Is(err, identitykeys.ErrKeyExists):
		return "present", nil
	case errors.Is(err, identitykeys.ErrNoClerkIdentity):
		return "skipped_no_clerk_identity", nil
	}
	return "", err
}

// connectAgent rotates the person's "T3 Code (bkt3)" agent, or creates it
// when there is none, and returns the plaintext token once. The rotation
// has no grace: the previous token stops working on /mcp at once, since
// bkt3 only ever holds the latest one. With several agents of that name
// (one a person made by hand), the newest is the bkt3 agent.
func (s *Server) connectAgent(ctx context.Context, userID string) (token, agentID, outcome string, err error) {
	agents, err := s.identity.ListAgents(ctx, userID)
	if err != nil {
		return "", "", "", err
	}
	for _, a := range agents { // newest first
		if a.Kind != identity.AgentKindAgent || a.Name != connectT3AgentName {
			continue
		}
		if a.Disabled {
			return "", a.ID, "", errConnectAgentDisabled
		}
		tok, err := s.identity.RotateAgentToken(ctx, userID, a.ID, 0)
		if err != nil {
			return "", a.ID, "", err
		}
		return tok, a.ID, "rotated", nil
	}
	tok, ag, err := s.identity.CreateAgentWithToken(ctx, userID, connectT3AgentName)
	if err != nil {
		return "", "", "", err
	}
	return tok, ag.ID, "created", nil
}

// connectRefuse audits and logs a refusal, then writes the error code.
// cause is logged (it says why a token failed, e.g. an azp mismatch) but
// never sent to the caller; it never contains the token.
func (s *Server) connectRefuse(ctx context.Context, w http.ResponseWriter, at *connectAttempt, status int, code string, cause error) {
	s.connectAudit(ctx, at, "denied", code)
	args := []any{"reason", code, "status", status, "client_ip", at.ip}
	if at.azp != "" {
		args = append(args, "azp", at.azp, "clerk_user", at.sub)
	}
	if at.user != nil {
		args = append(args, "user", at.user.ID)
	}
	if cause != nil {
		args = append(args, "err", cause.Error())
	}
	logx.For("connect-t3").Warn("refused", args...)
	writeError(w, status, code)
}

// connectAudit writes the attempt's one connect.t3 row: decision "denied"
// with the refusal code, or no decision with the outcome (created,
// rotated) as the reason. The bkt3 agent is the caller when known, else
// the user.
func (s *Server) connectAudit(ctx context.Context, at *connectAttempt, decision, reason string) {
	if s.audit == nil {
		return
	}
	ev := audit.Event{EventType: connectT3EventType, Decision: decision, Reason: reason}
	ev.ClientIP, ev.ClientKind, ev.Via = at.ip, "t3", "connect"
	var summary []string
	if at.azp != "" {
		summary = append(summary, "azp="+at.azp, "clerk_user="+at.sub)
	}
	if u := at.user; u != nil {
		ev.AgentID = "user:" + u.ID
		ev.OwnerUserID, ev.OwnerEmail, ev.OwnerName = u.ID, u.Email, u.Label()
	}
	if at.agentID != "" {
		ev.AgentID, ev.AgentName, ev.AgentKind = at.agentID, connectT3AgentName, identity.AgentKindAgent
		summary = append(summary, "agent="+at.agentID)
	}
	if at.keyState != "" {
		summary = append(summary, "identity_key="+at.keyState)
	}
	ev.ResultSummary = strings.Join(summary, " ")
	_ = s.audit.Write(context.WithoutCancel(ctx), ev)
}

// keyedMutex is one mutex per key, dropped when nobody holds or waits on
// it.
type keyedMutex struct {
	mu    sync.Mutex
	locks map[string]*keyedLock
}

type keyedLock struct {
	sync.Mutex
	refs int
}

// lock blocks until key is free and returns its unlock.
func (k *keyedMutex) lock(key string) func() {
	k.mu.Lock()
	if k.locks == nil {
		k.locks = map[string]*keyedLock{}
	}
	l := k.locks[key]
	if l == nil {
		l = &keyedLock{}
		k.locks[key] = l
	}
	l.refs++
	k.mu.Unlock()
	l.Lock()
	return func() {
		l.Unlock()
		k.mu.Lock()
		if l.refs--; l.refs == 0 {
			delete(k.locks, key)
		}
		k.mu.Unlock()
	}
}
