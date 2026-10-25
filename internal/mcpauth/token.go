package mcpauth

// token.go is the token endpoint (authorization_code and refresh_token
// grants), RFC 7009 revocation, and the access token check the /mcp guard
// runs on every request.

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"net/url"
	"time"

	"github.com/google/uuid"

	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// TokenResponse is the RFC 6749 section 5.1 answer.
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token,omitempty"`
	Scope        string `json:"scope,omitempty"`
}

// ClientAuth is how a client authenticated at the token or revocation
// endpoint: client_secret_basic (Basic set), client_secret_post (Secret in
// the body) or none (ClientID only).
type ClientAuth struct {
	ClientID string
	Secret   string
	Basic    bool
}

// authenticate checks the client's credentials against its registration.
// A public client may not send a secret; a confidential one must.
func (s *Service) authenticate(ctx context.Context, ca ClientAuth) (*Client, *Error) {
	if ca.ClientID == "" {
		return nil, invalidClient("client authentication failed: no client_id")
	}
	c, oerr := s.Client(ctx, ca.ClientID)
	if oerr != nil {
		return nil, invalidClient("client authentication failed: unknown client")
	}
	if c.Public() {
		if ca.Secret != "" || ca.Basic {
			return nil, invalidClient("client authentication failed: this client is public and has no secret")
		}
		return c, nil
	}
	if !c.checkSecret(ca.Secret) {
		return nil, invalidClient("client authentication failed")
	}
	return c, nil
}

// Exchange runs the token endpoint for one form-encoded request.
func (s *Service) Exchange(ctx context.Context, form url.Values, ca ClientAuth) (*TokenResponse, *Error) {
	if !s.Enabled() {
		return nil, oauthErr(404, "invalid_request", "OAuth is not enabled on this toolyard")
	}
	switch form.Get("grant_type") {
	case "authorization_code":
		return s.exchangeCode(ctx, form, ca)
	case "refresh_token":
		return s.refresh(ctx, form, ca)
	case "":
		return nil, invalidRequest("grant_type is required")
	default:
		return nil, oauthErr(400, "unsupported_grant_type", "grant_type must be authorization_code or refresh_token")
	}
}

// grantState is what a token or code needs from its grant to be honoured.
type grantState struct {
	agentID, nickname, ownerStatus string
	revoked, disabled              bool
}

func (s *Service) grantStateTx(ctx context.Context, q store.Querier, grantID string) (*grantState, error) {
	var g grantState
	var revoked sql.NullInt64
	var disabled int
	// A grant whose agent was deleted (the grant is its tombstone) counts
	// as disabled until the person connects the client again.
	err := q.QueryRowContext(ctx,
		`SELECT g.agent_id, COALESCE(a.name, ''), g.revoked_at, COALESCE(a.disabled, 1), COALESCE(u.status, '')
         FROM mcp_oauth_grants g LEFT JOIN agents a ON a.id = g.agent_id LEFT JOIN users u ON u.id = g.user_id
         WHERE g.id = ?`, grantID).Scan(&g.agentID, &g.nickname, &revoked, &disabled, &g.ownerStatus)
	if err != nil {
		return nil, err
	}
	g.revoked, g.disabled = revoked.Valid, disabled != 0
	return &g, nil
}

func (g *grantState) usable() bool {
	return !g.revoked && !g.disabled && g.ownerStatus == identity.StatusActive
}

// exchangeCode is the authorization_code grant. The code is claimed first,
// so a failed check still uses it up; a code presented twice revokes the
// tokens it produced (RFC 6749 section 4.1.2, OAuth 2.1 section 4.1.3).
func (s *Service) exchangeCode(ctx context.Context, form url.Values, ca ClientAuth) (*TokenResponse, *Error) {
	code := form.Get("code")
	if code == "" {
		return nil, invalidRequest("code is required")
	}
	client, oerr := s.authenticate(ctx, ca)
	if oerr != nil {
		return nil, oerr
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, serverError()
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.lockGrantOf(ctx, tx, `SELECT grant_id FROM mcp_oauth_codes WHERE code_hash = ?`, hashSecret(code)); err != nil {
		return nil, serverError()
	}
	now := s.now().UnixMilli()
	var grantID, clientID, redirect, challenge, resource, scope, family string
	var expires, consentedAt int64
	err = tx.QueryRowContext(ctx,
		`UPDATE mcp_oauth_codes SET used_at = ? WHERE code_hash = ? AND used_at IS NULL
         RETURNING grant_id, client_id, redirect_uri, code_challenge, resource, scope, family_id, expires_at, created_at`,
		now, hashSecret(code)).Scan(&grantID, &clientID, &redirect, &challenge, &resource, &scope, &family, &expires, &consentedAt)
	if errors.Is(err, sql.ErrNoRows) {
		var usedFamily, usedGrant string
		e := tx.QueryRowContext(ctx, `SELECT family_id, grant_id FROM mcp_oauth_codes WHERE code_hash = ?`, hashSecret(code)).Scan(&usedFamily, &usedGrant)
		if e != nil {
			return nil, invalidGrant("the authorization code is unknown or expired")
		}
		if _, e = tx.ExecContext(ctx, `UPDATE mcp_oauth_tokens SET revoked_at = ? WHERE family_id = ? AND revoked_at IS NULL`, now, usedFamily); e != nil {
			return nil, serverError()
		}
		_ = s.auditTx(ctx, tx, auditRow{event: "mcp_oauth.code_reused", clientID: client.ID, grantID: usedGrant,
			details: map[string]any{"action": "tokens issued from the code were revoked"}})
		if e = tx.Commit(); e != nil {
			return nil, serverError()
		}
		return nil, invalidGrant("the authorization code was already used")
	}
	if err != nil {
		return nil, serverError()
	}
	// The code is used up from here on, whatever the checks say.
	fail := func(e *Error) (*TokenResponse, *Error) {
		if err := tx.Commit(); err != nil {
			return nil, serverError()
		}
		return nil, e
	}
	switch {
	case expires <= now:
		return fail(invalidGrant("the authorization code expired; sign in again"))
	case clientID != client.ID:
		return fail(invalidGrant("the authorization code was issued to another client"))
	case form.Get("redirect_uri") != redirect:
		return fail(invalidGrant("redirect_uri does not match the authorization request"))
	}
	verifier := form.Get("code_verifier")
	if !verifierRE.MatchString(verifier) {
		return fail(invalidGrant("code_verifier is missing or malformed"))
	}
	if subtle.ConstantTimeCompare([]byte(pkceS256(verifier)), []byte(challenge)) != 1 {
		return fail(invalidGrant("PKCE verification failed: code_verifier does not match code_challenge"))
	}
	if r := form.Get("resource"); r != "" && !s.sameResource(r) {
		return fail(oauthErr(400, "invalid_target", "resource must be "+s.Resource()))
	}
	g, err := s.grantStateTx(ctx, tx, grantID)
	if err != nil {
		return nil, serverError()
	}
	if !g.usable() {
		return fail(invalidGrant("this connection was revoked or its owner can no longer sign in"))
	}
	resp, err := s.issueTokensTx(ctx, tx, grantID, client.ID, family, scope, resource, consentedAt, "")
	if errors.Is(err, errSessionTooOld) {
		return fail(invalidGrant("this sign-in reached its maximum age; sign in again"))
	}
	if err != nil {
		return nil, serverError()
	}
	if _, err = tx.ExecContext(ctx, `UPDATE mcp_oauth_grants SET last_used_at = ? WHERE id = ?`, now, grantID); err != nil {
		return nil, serverError()
	}
	if err = tx.Commit(); err != nil {
		return nil, serverError()
	}
	return resp, nil
}

// errSessionTooOld: the grant's sign-in passed the absolute session limit.
var errSessionTooOld = errors.New("mcpauth: the sign-in reached its maximum age")

// refresh is the refresh_token grant: the presented token is claimed
// (used once) and a new access and refresh token are issued. A refresh
// token presented after it was used revokes the whole grant: one of the
// two holders is not the client. The one exception is a retry: within
// refreshReuseGrace of its use, while the token that replaced it is still
// unused, it is refused without revoking anything.
func (s *Service) refresh(ctx context.Context, form url.Values, ca ClientAuth) (*TokenResponse, *Error) {
	raw := form.Get("refresh_token")
	if raw == "" {
		return nil, invalidRequest("refresh_token is required")
	}
	client, oerr := s.authenticate(ctx, ca)
	if oerr != nil {
		return nil, oerr
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, serverError()
	}
	defer func() { _ = tx.Rollback() }()
	now := s.now().UnixMilli()
	hash := hashSecret(raw)
	if err := s.lockGrantOf(ctx, tx, `SELECT grant_id FROM mcp_oauth_tokens WHERE token_hash = ? AND kind = 'refresh'`, hash); err != nil {
		return nil, serverError()
	}
	var grantID, family, scope, resource string
	var authTime int64
	err = tx.QueryRowContext(ctx,
		`UPDATE mcp_oauth_tokens SET used_at = ?
         WHERE token_hash = ? AND kind = 'refresh' AND client_id = ? AND used_at IS NULL AND revoked_at IS NULL AND expires_at > ?
         RETURNING grant_id, family_id, scope, resource, auth_time`,
		now, hash, client.ID, now).Scan(&grantID, &family, &scope, &resource, &authTime)
	if errors.Is(err, sql.ErrNoRows) {
		var usedGrant, usedClient string
		var used, revoked sql.NullInt64
		e := tx.QueryRowContext(ctx, `SELECT grant_id, client_id, used_at, revoked_at FROM mcp_oauth_tokens WHERE token_hash = ? AND kind = 'refresh'`, hash).
			Scan(&usedGrant, &usedClient, &used, &revoked)
		if e != nil || usedClient != client.ID {
			return nil, invalidGrant("the refresh token is unknown, expired or revoked")
		}
		if used.Valid && !revoked.Valid {
			if used.Int64 >= now-refreshReuseGrace.Milliseconds() {
				var live int
				if e = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM mcp_oauth_tokens
                    WHERE parent_hash = ? AND kind = 'refresh' AND used_at IS NULL AND revoked_at IS NULL`, hash).Scan(&live); e != nil {
					return nil, serverError()
				}
				if live > 0 {
					return nil, invalidGrant("the refresh token was just used; use the newest refresh token")
				}
			}
			agentID, e := s.revokeGrantTx(ctx, tx, usedGrant, "refresh_token_reused")
			if e != nil {
				return nil, serverError()
			}
			_ = s.auditTx(ctx, tx, auditRow{event: "mcp_oauth.refresh_reused", clientID: client.ID, grantID: usedGrant,
				details: map[string]any{"action": "the grant was revoked"}})
			if e = tx.Commit(); e != nil {
				return nil, serverError()
			}
			s.afterRevoke(ctx, agentID)
			return nil, invalidGrant("the refresh token was already used, so toolyard revoked this connection; sign in again")
		}
		return nil, invalidGrant("the refresh token is unknown, expired or revoked")
	}
	if err != nil {
		return nil, serverError()
	}
	// From here a refusal rolls the claim back (the deferred Rollback), so
	// a request the server refuses does not cost the client its token.
	if want := form.Get("scope"); want != "" {
		if !scopeSubset(want, scope) {
			return nil, oauthErr(400, "invalid_scope", "scope may only narrow the original grant")
		}
		scope = grantedScope(want)
	}
	if r := form.Get("resource"); r != "" && !s.sameResource(r) {
		return nil, oauthErr(400, "invalid_target", "resource must be "+s.Resource())
	}
	g, err := s.grantStateTx(ctx, tx, grantID)
	if err != nil {
		return nil, serverError()
	}
	if !g.usable() {
		return nil, invalidGrant("this connection was revoked or its owner can no longer sign in")
	}
	resp, err := s.issueTokensTx(ctx, tx, grantID, client.ID, family, scope, resource, authTime, hash)
	if errors.Is(err, errSessionTooOld) {
		return nil, invalidGrant("this sign-in reached its maximum age; sign in again")
	}
	if err != nil {
		return nil, serverError()
	}
	if _, err = tx.ExecContext(ctx, `UPDATE mcp_oauth_grants SET last_used_at = ? WHERE id = ?`, now, grantID); err != nil {
		return nil, serverError()
	}
	if err = tx.Commit(); err != nil {
		return nil, serverError()
	}
	return resp, nil
}

// issueTokensTx stores a new access and refresh token for the grant.
// authTime is when the person consented: no token outlives authTime plus
// MaxSession, however often it is refreshed (errSessionTooOld once that
// has passed). parent is the refresh token this one replaces ("" for a
// code exchange).
func (s *Service) issueTokensTx(ctx context.Context, q store.Querier, grantID, clientID, family, scope, resource string, authTime int64, parent string) (*TokenResponse, error) {
	now := s.now()
	limit := time.UnixMilli(authTime).Add(s.MaxSession())
	if !now.Before(limit) {
		return nil, errSessionTooOld
	}
	until := func(ttl time.Duration) time.Time {
		if t := now.Add(ttl); t.Before(limit) {
			return t
		}
		return limit
	}
	access, err := newSecret(accessPrefix)
	if err != nil {
		return nil, err
	}
	refresh, err := newSecret(refreshPrefix)
	if err != nil {
		return nil, err
	}
	accessUntil := until(s.AccessTTL())
	for _, t := range []struct {
		raw, kind, parent string
		expires           time.Time
	}{{access, "access", "", accessUntil}, {refresh, "refresh", parent, until(s.RefreshTTL())}} {
		if _, err := q.ExecContext(ctx,
			`INSERT INTO mcp_oauth_tokens(token_hash, kind, grant_id, client_id, family_id, parent_hash, scope, resource, auth_time, created_at, expires_at)
             VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
			hashSecret(t.raw), t.kind, grantID, clientID, family, nullStr(t.parent), scope, resource, authTime, now.UnixMilli(), t.expires.UnixMilli()); err != nil {
			return nil, err
		}
	}
	return &TokenResponse{AccessToken: access, TokenType: "Bearer", ExpiresIn: int64(accessUntil.Sub(now) / time.Second), RefreshToken: refresh, Scope: scope}, nil
}

// revokeGrantTx ends a grant: its tokens and unused codes stop working and
// its agent is disabled. The rows stay, so the audit trail keeps naming
// the client. Connecting the client again re-enables the same agent. It
// returns the agent, for afterRevoke once the transaction commits.
func (s *Service) revokeGrantTx(ctx context.Context, q store.Querier, grantID, reason string) (string, error) {
	now := s.now().UnixMilli()
	var agentID string
	if err := q.QueryRowContext(ctx, `SELECT agent_id FROM mcp_oauth_grants WHERE id = ?`, grantID).Scan(&agentID); err != nil {
		return "", err
	}
	if _, err := q.ExecContext(ctx, `UPDATE mcp_oauth_grants SET revoked_at = ?, revoked_reason = ?, updated_at = ? WHERE id = ? AND revoked_at IS NULL`,
		now, reason, now, grantID); err != nil {
		return "", err
	}
	if _, err := q.ExecContext(ctx, `UPDATE mcp_oauth_tokens SET revoked_at = ? WHERE grant_id = ? AND revoked_at IS NULL`, now, grantID); err != nil {
		return "", err
	}
	if _, err := q.ExecContext(ctx, `UPDATE mcp_oauth_codes SET used_at = ? WHERE grant_id = ? AND used_at IS NULL`, now, grantID); err != nil {
		return "", err
	}
	if _, err := q.ExecContext(ctx, `UPDATE agents SET disabled = 1 WHERE id = ?`, agentID); err != nil {
		return "", err
	}
	return agentID, nil
}

// afterRevoke runs once a revocation committed: the agent's cached scope
// is dropped (a disabled agent reaches nothing, so nothing it queued can
// run) and OnRevoke cancels what it still has pending, on a context the
// caller's request ending cannot cancel.
func (s *Service) afterRevoke(ctx context.Context, agentID string) {
	if agentID == "" {
		return
	}
	if s.access != nil {
		s.access.InvalidateAgent(agentID)
	}
	s.cancelPending(ctx, agentID)
}

// cancelPending runs OnRevoke for agentID on a context the caller's
// request ending cannot cancel.
func (s *Service) cancelPending(ctx context.Context, agentID string) {
	if s.onRevoke != nil && agentID != "" {
		s.onRevoke(context.WithoutCancel(ctx), agentID)
	}
}

// AgentDeleted runs the revocation follow-up for an agent deleted through
// the generic agent routes (identity.DeleteAgent revoked its grant in the
// same transaction): its cached scope goes and OnRevoke cancels what it
// had pending. Nothing happens for an agent that was not an OAuth client.
func (s *Service) AgentDeleted(ctx context.Context, agentID string) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM mcp_oauth_grants WHERE agent_id = ?`, agentID).Scan(&n); err != nil || n == 0 {
		return
	}
	s.afterRevoke(ctx, agentID)
}

// lockGrantOf locks the grant of the code or refresh token hash in tx
// (SELECT ... FOR UPDATE on Postgres) before the code or token row is
// claimed, so every transaction that touches a grant and its tokens or
// codes takes the grant first, as Approve and revokeGrantTx do: no two of
// them can lock the same rows in opposite orders (a deadlock).
func (s *Service) lockGrantOf(ctx context.Context, tx store.Querier, query, hash string) error {
	var grantID string
	err := tx.QueryRowContext(ctx, query, hash).Scan(&grantID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // the claim below finds nothing either
	}
	if err != nil {
		return err
	}
	var locked string
	return tx.QueryRowContext(ctx, `SELECT id FROM mcp_oauth_grants WHERE id = ?`+s.db.Dialect.ForUpdate(), grantID).Scan(&locked)
}

// Revoke is RFC 7009 revocation. An unknown token, or one issued to
// another client, is answered as a success that changes nothing. Revoking
// an access token ends that token; revoking a refresh token ends the whole
// grant (the client signed out).
func (s *Service) Revoke(ctx context.Context, token string, ca ClientAuth) *Error {
	if !s.Enabled() {
		return oauthErr(404, "invalid_request", "OAuth is not enabled on this toolyard")
	}
	if token == "" {
		return invalidRequest("token is required")
	}
	client, oerr := s.authenticate(ctx, ca)
	if oerr != nil {
		return oerr
	}
	var kind, grantID, clientID string
	err := s.db.QueryRowContext(ctx, `SELECT kind, grant_id, client_id FROM mcp_oauth_tokens WHERE token_hash = ?`, hashSecret(token)).
		Scan(&kind, &grantID, &clientID)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && clientID != client.ID) {
		return nil
	}
	if err != nil {
		return serverError()
	}
	now := s.now().UnixMilli()
	if kind == "access" {
		if _, err := s.db.ExecContext(ctx, `UPDATE mcp_oauth_tokens SET revoked_at = ? WHERE token_hash = ? AND revoked_at IS NULL`, now, hashSecret(token)); err != nil {
			return serverError()
		}
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return serverError()
	}
	defer func() { _ = tx.Rollback() }()
	agentID, err := s.revokeGrantTx(ctx, tx, grantID, "revoked_by_client")
	if err != nil {
		return serverError()
	}
	if err := s.auditTx(ctx, tx, auditRow{event: "mcp_oauth.revoked", client: client, grantID: grantID,
		details: map[string]any{"by": "client"}}); err != nil {
		return serverError()
	}
	if err := tx.Commit(); err != nil {
		return serverError()
	}
	s.afterRevoke(ctx, agentID)
	return nil
}

// VerifyAccessToken is the /mcp check for an OAuth access token: unknown,
// expired, revoked, for another resource, or whose agent or owner can no
// longer act is identity.ErrAgentTokenInvalid; any other error means the
// database could not answer. The agent comes back with Kind
// AgentKindOAuthClient, its nickname as Name, and its owner's details.
func (s *Service) VerifyAccessToken(ctx context.Context, token string) (*identity.Agent, error) {
	if !s.Enabled() || !IsAccessToken(token) {
		return nil, identity.ErrAgentTokenInvalid
	}
	var grantID, resource string
	var expires int64
	var tokRevoked, grantRevoked, lastUsed sql.NullInt64
	var disabled int
	var status string
	ag := identity.Agent{Kind: AgentKindOAuthClient}
	err := s.db.QueryRowContext(ctx,
		`SELECT t.grant_id, t.expires_at, t.revoked_at, t.resource, g.revoked_at, g.last_used_at,
                a.id, a.name, a.owner_user, COALESCE(a.disabled, 0),
                COALESCE(u.status, ''), COALESCE(u.email, ''), COALESCE(u.display_name, ''), COALESCE(u.username, '')
         FROM mcp_oauth_tokens t
         JOIN mcp_oauth_grants g ON g.id = t.grant_id
         JOIN agents a ON a.id = g.agent_id
         LEFT JOIN users u ON u.id = a.owner_user
         WHERE t.token_hash = ? AND t.kind = 'access'`, hashSecret(token)).
		Scan(&grantID, &expires, &tokRevoked, &resource, &grantRevoked, &lastUsed,
			&ag.ID, &ag.Name, &ag.Owner, &disabled, &status, &ag.OwnerEmail, &ag.OwnerDisplayName, &ag.OwnerUsername)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, identity.ErrAgentTokenInvalid
	}
	if err != nil {
		return nil, err
	}
	now := s.now()
	if tokRevoked.Valid || grantRevoked.Valid || expires <= now.UnixMilli() || disabled != 0 ||
		status != identity.StatusActive || !s.sameResource(resource) {
		return nil, identity.ErrAgentTokenInvalid
	}
	ag.LastSeen = now
	if !lastUsed.Valid || lastUsed.Int64 < now.Add(-lastUsedEvery).UnixMilli() {
		// Best effort: a failed touch must not fail the call.
		_, _ = s.db.ExecContext(ctx, `UPDATE mcp_oauth_grants SET last_used_at = ? WHERE id = ?`, now.UnixMilli(), grantID)
		_, _ = s.db.ExecContext(ctx, `UPDATE agents SET last_seen = ? WHERE id = ?`, now.UnixMilli(), ag.ID)
	}
	return &ag, nil
}

// newFamily names the tokens of one code exchange.
func newFamily() string { return "fam_" + uuid.NewString() }
