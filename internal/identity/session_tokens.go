package identity

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

// Session tokens are short-lived bearers an agent mints with its agent token
// for one client session (a T3 thread), so the session can call code mode
// over REST (/v1/code/*) without holding the long-lived agent token. A
// session token cannot mint another one.
//
// Token shape: "tys_st_<hex id>.<base64url secret>". The row id is
// "st_<hex id>" and only the sha256 of the whole token is stored. The row
// also keeps the sha256 of the agent token that minted it: the session
// token works only while that agent token is still accepted for its agent
// (the current token, or the previous one inside its rotation grace).

const (
	// SessionTokenPrefix starts every session token, so a bearer header
	// can be routed without a database lookup.
	SessionTokenPrefix = "tys_"
	// sessionTokenIDPrefix starts every session token row id.
	sessionTokenIDPrefix = "st_"

	// MinSessionTokenTTL and MaxSessionTokenTTL bound a session token's
	// lifetime; DefaultSessionTokenTTL is used when none is asked for.
	MinSessionTokenTTL     = 60 * time.Second
	MaxSessionTokenTTL     = time.Hour
	DefaultSessionTokenTTL = MaxSessionTokenTTL

	// MaxSessionIDLen caps a session token's client session id.
	MaxSessionIDLen = 128

	// sessionTokenTouch is how often last_used_at (and the agent's
	// last_seen) is written for a session token in use: once a minute, so
	// a busy session does not turn every call into a write.
	sessionTokenTouch = time.Minute
)

var (
	// ErrSessionTokenInvalid: the session token is unknown, malformed,
	// expired or revoked, or the agent token it was minted with is no
	// longer accepted.
	ErrSessionTokenInvalid = errors.New("invalid session token")
	// ErrSessionTokenNotFound: the session token id belongs to another
	// agent.
	ErrSessionTokenNotFound = errors.New("session token not found")
	// ErrSessionIDInvalid: the client session id is not 1-128 printable
	// ASCII characters.
	ErrSessionIDInvalid = errors.New("session_id must be 1-128 printable ASCII characters")
)

// SessionToken is the value-free view of a session token row.
type SessionToken struct {
	ID         string `json:"token_id"`
	AgentID    string `json:"agent_id"`
	SessionID  string `json:"session_id"`
	CreatedAt  int64  `json:"created_at"`
	ExpiresAt  int64  `json:"expires_at"`
	RevokedAt  int64  `json:"revoked_at,omitempty"`
	LastUsedAt int64  `json:"last_used_at,omitempty"`
	// ParentTokenHash is the sha256 of the agent token that minted this
	// session token. Never sent to a client.
	ParentTokenHash string `json:"-"`
}

// IsSessionToken reports whether token has the session token shape (the
// prefix only; VerifySessionToken checks the rest).
func IsSessionToken(token string) bool { return strings.HasPrefix(token, SessionTokenPrefix) }

// SessionTokenID returns the row id ("st_…") a session token names, or
// ok=false when token does not have the session token shape.
func SessionTokenID(token string) (id string, ok bool) {
	rest, found := strings.CutPrefix(token, SessionTokenPrefix)
	if !found {
		return "", false
	}
	id, secret, found := strings.Cut(rest, ".")
	if !found || secret == "" || !validSessionTokenID(id) {
		return "", false
	}
	return id, true
}

// validSessionTokenID reports whether id looks like a session token row id:
// "st_" and 1-64 lower-case hex digits.
func validSessionTokenID(id string) bool {
	h, ok := strings.CutPrefix(id, sessionTokenIDPrefix)
	if !ok || h == "" || len(h) > 64 {
		return false
	}
	for i := 0; i < len(h); i++ {
		c := h[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// ValidSessionID reports whether id is a valid client session id: 1-128
// printable ASCII characters (space through tilde).
func ValidSessionID(id string) bool {
	if id == "" || len(id) > MaxSessionIDLen {
		return false
	}
	for i := 0; i < len(id); i++ {
		if id[i] < 0x20 || id[i] > 0x7e {
			return false
		}
	}
	return true
}

// ClampSessionTokenTTL returns ttl bounded to MinSessionTokenTTL ..
// MaxSessionTokenTTL; ttl <= 0 means DefaultSessionTokenTTL.
func ClampSessionTokenTTL(ttl time.Duration) time.Duration {
	switch {
	case ttl <= 0:
		return DefaultSessionTokenTTL
	case ttl < MinSessionTokenTTL:
		return MinSessionTokenTTL
	case ttl > MaxSessionTokenTTL:
		return MaxSessionTokenTTL
	}
	return ttl
}

// MintSessionToken issues a session token for sessionID, minted with
// parentToken, the agent token of agentID the caller has already verified.
// The parent must be an agent token of that agent that is still accepted:
// a session token or an operator token cannot mint one. ttl is clamped
// (ClampSessionTokenTTL). The plaintext is returned once and never stored.
func (s *Service) MintSessionToken(ctx context.Context, agentID, parentToken, sessionID string, ttl time.Duration) (string, *SessionToken, error) {
	if !ValidSessionID(sessionID) {
		return "", nil, ErrSessionIDInvalid
	}
	if agentID == "" || IsSessionToken(parentToken) || strings.HasPrefix(parentToken, OperatorTokenPrefix) {
		return "", nil, ErrAgentTokenInvalid
	}
	if id, _, ok := strings.Cut(parentToken, "."); !ok || id != agentID {
		return "", nil, ErrAgentTokenInvalid
	}
	parentHash := hashToken(parentToken)
	if _, err := s.VerifyAgentTokenHash(ctx, agentID, parentHash); err != nil {
		return "", nil, err
	}
	idBytes := make([]byte, 12)
	if _, err := rand.Read(idBytes); err != nil {
		return "", nil, err
	}
	secret, err := randCode(32)
	if err != nil {
		return "", nil, err
	}
	id := sessionTokenIDPrefix + hex.EncodeToString(idBytes)
	token := SessionTokenPrefix + id + "." + secret
	now := time.Now()
	t := &SessionToken{
		ID: id, AgentID: agentID, SessionID: sessionID,
		CreatedAt: now.UnixMilli(), ExpiresAt: now.Add(ClampSessionTokenTTL(ttl)).UnixMilli(),
		ParentTokenHash: parentHash,
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO agent_session_tokens(id, agent_id, token_hash, session_id, parent_token_hash, created_at, expires_at)
         VALUES(?,?,?,?,?,?,?)`,
		t.ID, t.AgentID, hashToken(token), t.SessionID, t.ParentTokenHash, t.CreatedAt, t.ExpiresAt); err != nil {
		return "", nil, err
	}
	return token, t, nil
}

// VerifySessionToken resolves a session token to its row and its agent.
// Malformed, unknown, expired and revoked tokens, and tokens whose agent is
// disabled, deleted or has a blocked owner, or whose parent agent token is
// no longer accepted (rotated past its grace period), all return
// ErrSessionTokenInvalid. Any other error is a database failure.
//
// Connection state (federation, host and local connections) is not checked
// here: with federation on, use federation.Service.VerifySessionToken, as
// agent tokens use federation.Service.VerifyAgent.
func (s *Service) VerifySessionToken(ctx context.Context, token string) (*SessionToken, *Agent, error) {
	id, ok := SessionTokenID(token)
	if !ok {
		return nil, nil, ErrSessionTokenInvalid
	}
	t, hash, err := s.getSessionToken(ctx, id)
	if errors.Is(err, ErrSessionTokenNotFound) {
		return nil, nil, ErrSessionTokenInvalid
	}
	if err != nil {
		return nil, nil, err
	}
	if subtle.ConstantTimeCompare([]byte(hash), []byte(hashToken(token))) != 1 {
		return nil, nil, ErrSessionTokenInvalid
	}
	now := time.Now().UnixMilli()
	if t.RevokedAt != 0 || now >= t.ExpiresAt {
		return nil, nil, ErrSessionTokenInvalid
	}
	ag, err := s.VerifyAgentTokenHash(ctx, t.AgentID, t.ParentTokenHash)
	if errors.Is(err, ErrAgentTokenInvalid) {
		return nil, nil, ErrSessionTokenInvalid
	}
	if err != nil {
		return nil, nil, err
	}
	if now-t.LastUsedAt >= sessionTokenTouch.Milliseconds() {
		_, _ = s.db.ExecContext(ctx, `UPDATE agent_session_tokens SET last_used_at=? WHERE id=?`, now, t.ID)
		_, _ = s.db.ExecContext(ctx, `UPDATE agents SET last_seen=? WHERE id=?`, now, ag.ID)
		t.LastUsedAt = now
	}
	return t, ag, nil
}

// RevokeSessionToken revokes the session token tokenID of agentID. Revoking
// twice, or an id that does not exist (malformed, never issued or already
// purged), is not an error. An id that belongs to another agent is
// ErrSessionTokenNotFound.
func (s *Service) RevokeSessionToken(ctx context.Context, agentID, tokenID string) error {
	if !validSessionTokenID(tokenID) {
		return nil
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE agent_session_tokens SET revoked_at = COALESCE(revoked_at, ?) WHERE id = ? AND agent_id = ?`,
		time.Now().UnixMilli(), tokenID, agentID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n > 0 {
		return err
	}
	var owner string
	err = s.db.QueryRowContext(ctx, `SELECT agent_id FROM agent_session_tokens WHERE id = ?`, tokenID).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if owner != agentID {
		return ErrSessionTokenNotFound
	}
	return nil
}

// RevokeOwnSessionToken revokes the session token presented as token: the
// holder of a session token may always revoke it, even once it has expired
// or was revoked already (revoking twice is not an error), so a client can
// retry its clean-up. A malformed or unknown token, or a wrong secret, is
// ErrSessionTokenInvalid.
func (s *Service) RevokeOwnSessionToken(ctx context.Context, token string) (*SessionToken, error) {
	id, ok := SessionTokenID(token)
	if !ok {
		return nil, ErrSessionTokenInvalid
	}
	t, hash, err := s.getSessionToken(ctx, id)
	if errors.Is(err, ErrSessionTokenNotFound) {
		return nil, ErrSessionTokenInvalid
	}
	if err != nil {
		return nil, err
	}
	if subtle.ConstantTimeCompare([]byte(hash), []byte(hashToken(token))) != 1 {
		return nil, ErrSessionTokenInvalid
	}
	now := time.Now().UnixMilli()
	if _, err := s.db.ExecContext(ctx,
		`UPDATE agent_session_tokens SET revoked_at = COALESCE(revoked_at, ?) WHERE id = ?`, now, t.ID); err != nil {
		return nil, err
	}
	if t.RevokedAt == 0 {
		t.RevokedAt = now
	}
	return t, nil
}

// PurgeExpiredSessionTokens deletes session tokens that expired more than
// grace ago, revoked or not, so the table does not grow forever. Called by
// a sweeper; returns the number of rows deleted.
func (s *Service) PurgeExpiredSessionTokens(ctx context.Context, grace time.Duration) (int64, error) {
	cut := time.Now().Add(-grace).UnixMilli()
	res, err := s.db.ExecContext(ctx, `DELETE FROM agent_session_tokens WHERE expires_at < ?`, cut)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// VerifyAgentTokenHash reports whether tokenHash (the sha256 of an agent
// token, as HashToken returns it) is still an accepted credential of agent
// agentID, without the raw token: the agent exists, is not disabled, its
// owner is not blocked (an agent whose owner row is gone keeps working, as
// in VerifyAgentToken), and tokenHash is its current token hash or its
// previous one inside the rotation grace window. It returns the agent with
// its owner's details, or ErrAgentTokenInvalid. It writes nothing.
//
// Like VerifyAgentToken it does not check connection state: federation,
// host and local connections are checked by the federation package.
func (s *Service) VerifyAgentTokenHash(ctx context.Context, agentID, tokenHash string) (*Agent, error) {
	if agentID == "" || tokenHash == "" {
		return nil, ErrAgentTokenInvalid
	}
	var ag Agent
	var hash string
	var prevHash sql.NullString
	var prevExp sql.NullInt64
	var disabled int
	var ownerStatus string
	err := s.db.QueryRowContext(ctx,
		`SELECT a.id, a.name, a.owner_user, a.kind, a.token_hash, COALESCE(a.disabled,0), a.prev_token_hash, a.prev_token_expires,
                COALESCE(u.status, ?), COALESCE(u.email,''), COALESCE(u.display_name,''), COALESCE(u.username,'')
         FROM agents a LEFT JOIN users u ON u.id = a.owner_user WHERE a.id = ?`, StatusActive, agentID).
		Scan(&ag.ID, &ag.Name, &ag.Owner, &ag.Kind, &hash, &disabled, &prevHash, &prevExp, &ownerStatus,
			&ag.OwnerEmail, &ag.OwnerDisplayName, &ag.OwnerUsername)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrAgentTokenInvalid
	}
	if err != nil {
		return nil, err
	}
	if disabled != 0 || ownerStatus != StatusActive {
		return nil, ErrAgentTokenInvalid
	}
	ok := hash != "" && subtle.ConstantTimeCompare([]byte(hash), []byte(tokenHash)) == 1
	if !ok && prevHash.Valid && prevHash.String != "" {
		withinGrace := !prevExp.Valid || prevExp.Int64 == 0 || time.Now().UnixMilli() < prevExp.Int64
		if withinGrace && subtle.ConstantTimeCompare([]byte(prevHash.String), []byte(tokenHash)) == 1 {
			ok = true
		}
	}
	if !ok {
		return nil, ErrAgentTokenInvalid
	}
	return &ag, nil
}

// getSessionToken reads one session token row and its token hash, or
// ErrSessionTokenNotFound.
func (s *Service) getSessionToken(ctx context.Context, id string) (*SessionToken, string, error) {
	var t SessionToken
	var hash string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, agent_id, token_hash, session_id, parent_token_hash, created_at, expires_at,
                COALESCE(revoked_at,0), COALESCE(last_used_at,0)
         FROM agent_session_tokens WHERE id = ?`, id).
		Scan(&t.ID, &t.AgentID, &hash, &t.SessionID, &t.ParentTokenHash, &t.CreatedAt, &t.ExpiresAt,
			&t.RevokedAt, &t.LastUsedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", ErrSessionTokenNotFound
	}
	if err != nil {
		return nil, "", err
	}
	return &t, hash, nil
}
