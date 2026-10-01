package identity

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Operator tokens let a CLI agent drive the /v1 API the dashboard uses, as
// the user who owns the token, limited to the token's scopes. They are a
// different credential from agent tokens: an agent token calls tools through
// /mcp, an operator token administers Toolyard.
//
// Token shape: "tyop_<hex id>.<base64url secret>". The row id is "op_<hex id>"
// and only the sha256 of the whole token is stored.

const (
	// OperatorTokenPrefix starts every operator token, so a bearer header
	// can be routed without a database lookup.
	OperatorTokenPrefix = "tyop_"

	// ScopeRead allows GET/HEAD on the /v1 API.
	ScopeRead = "read"
	// ScopeWrite allows mutations: servers, agents, settings, events,
	// memory, notes, and secret requests.
	ScopeWrite = "write"
	// ScopeOwner allows what decides or pre-authorizes tool calls, or sets
	// secret values: approvals, inbox decisions and grants, policies,
	// auto-approval rules, users, passkeys and secret values. It is never
	// granted by default.
	ScopeOwner = "owner"
)

// DefaultOperatorScopes is what a token gets when no scopes are asked for.
var DefaultOperatorScopes = []string{ScopeRead, ScopeWrite}

var validOperatorScopes = map[string]bool{ScopeRead: true, ScopeWrite: true, ScopeOwner: true}

var (
	ErrOperatorTokenInvalid  = errors.New("invalid operator token")
	ErrOperatorTokenNotFound = errors.New("operator token not found")
)

// OperatorToken is the value-free view of an operator token.
type OperatorToken struct {
	ID         string   `json:"id"`
	UserID     string   `json:"user_id"`
	Name       string   `json:"name"`
	Scopes     []string `json:"scopes"`
	CreatedBy  string   `json:"created_by,omitempty"`
	CreatedAt  int64    `json:"created_at"`
	ExpiresAt  int64    `json:"expires_at,omitempty"`
	LastUsedAt int64    `json:"last_used_at,omitempty"`
	RevokedAt  int64    `json:"revoked_at,omitempty"`
}

// HasScope reports whether the token carries scope.
func (t *OperatorToken) HasScope(scope string) bool {
	for _, s := range t.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// NormalizeOperatorScopes validates, de-duplicates and sorts scopes. An empty
// list means DefaultOperatorScopes. "owner" and "write" imply "read".
func NormalizeOperatorScopes(in []string) ([]string, error) {
	if len(in) == 0 {
		in = DefaultOperatorScopes
	}
	set := map[string]bool{}
	for _, raw := range in {
		for _, s := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' }) {
			s = strings.ToLower(strings.TrimSpace(s))
			if !validOperatorScopes[s] {
				return nil, fmt.Errorf("unknown scope %q (want read, write, owner)", s)
			}
			set[s] = true
		}
	}
	if len(set) == 0 {
		return nil, errors.New("no scopes")
	}
	set[ScopeRead] = true
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out, nil
}

// CreateOperatorToken mints a token for userID. ttl 0 means no expiry. The
// plaintext is returned once and never stored.
func (s *Service) CreateOperatorToken(ctx context.Context, userID, name string, scopes []string, ttl time.Duration, createdBy string) (string, *OperatorToken, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", nil, errors.New("name required")
	}
	if len(name) > 80 {
		return "", nil, errors.New("name too long")
	}
	scopes, err := NormalizeOperatorScopes(scopes)
	if err != nil {
		return "", nil, err
	}
	u, err := s.GetUserByID(ctx, userID)
	if err != nil {
		return "", nil, fmt.Errorf("user %q: %w", userID, err)
	}
	if u.Status != StatusActive {
		return "", nil, fmt.Errorf("user %q is not active", userID)
	}
	idBytes := make([]byte, 12)
	if _, err := rand.Read(idBytes); err != nil {
		return "", nil, err
	}
	hexID := hex.EncodeToString(idBytes)
	secret, err := randCode(32)
	if err != nil {
		return "", nil, err
	}
	token := OperatorTokenPrefix + hexID + "." + secret
	now := time.Now().UnixMilli()
	t := &OperatorToken{
		ID: "op_" + hexID, UserID: userID, Name: name, Scopes: scopes,
		CreatedBy: createdBy, CreatedAt: now,
	}
	var expires any
	if ttl > 0 {
		t.ExpiresAt = time.Now().Add(ttl).UnixMilli()
		expires = t.ExpiresAt
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO operator_tokens(id, user_id, name, token_hash, scopes, created_by, created_at, expires_at)
         VALUES(?,?,?,?,?,?,?,?)`,
		t.ID, userID, name, hashToken(token), strings.Join(scopes, " "), nullIfEmpty(createdBy), now, expires)
	if err != nil {
		return "", nil, err
	}
	return token, t, nil
}

// VerifyOperatorToken resolves a token to its row and active user. Revoked,
// expired, unknown and blocked-owner tokens all return ErrOperatorTokenInvalid.
func (s *Service) VerifyOperatorToken(ctx context.Context, token string) (*OperatorToken, *User, error) {
	if !strings.HasPrefix(token, OperatorTokenPrefix) {
		return nil, nil, ErrOperatorTokenInvalid
	}
	rest := strings.TrimPrefix(token, OperatorTokenPrefix)
	dot := strings.IndexByte(rest, '.')
	if dot <= 0 {
		return nil, nil, ErrOperatorTokenInvalid
	}
	t, hash, err := s.getOperatorToken(ctx, "op_"+rest[:dot])
	if err != nil {
		return nil, nil, ErrOperatorTokenInvalid
	}
	if subtle.ConstantTimeCompare([]byte(hash), []byte(hashToken(token))) != 1 {
		return nil, nil, ErrOperatorTokenInvalid
	}
	now := time.Now().UnixMilli()
	if t.RevokedAt != 0 || (t.ExpiresAt != 0 && now >= t.ExpiresAt) {
		return nil, nil, ErrOperatorTokenInvalid
	}
	u, err := s.GetUserByID(ctx, t.UserID)
	if err != nil || u.Status != StatusActive {
		return nil, nil, ErrOperatorTokenInvalid
	}
	// last_used_at is coarse (one write per minute) so a busy CLI doesn't
	// turn every read into a write.
	if now-t.LastUsedAt > int64(time.Minute/time.Millisecond) {
		_, _ = s.db.ExecContext(ctx, `UPDATE operator_tokens SET last_used_at=? WHERE id=?`, now, t.ID)
		t.LastUsedAt = now
	}
	return t, u, nil
}

// ListOperatorTokens returns the tokens owned by userID, or every token when
// userID is empty. Hashes are never returned.
func (s *Service) ListOperatorTokens(ctx context.Context, userID string) ([]OperatorToken, error) {
	q := `SELECT id, user_id, name, scopes, COALESCE(created_by,''), created_at,
                 COALESCE(expires_at,0), COALESCE(last_used_at,0), COALESCE(revoked_at,0)
          FROM operator_tokens`
	args := []any{}
	if userID != "" {
		q += ` WHERE user_id = ?`
		args = append(args, userID)
	}
	q += ` ORDER BY created_at, id`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []OperatorToken{}
	for rows.Next() {
		var t OperatorToken
		var scopes string
		if err := rows.Scan(&t.ID, &t.UserID, &t.Name, &scopes, &t.CreatedBy, &t.CreatedAt,
			&t.ExpiresAt, &t.LastUsedAt, &t.RevokedAt); err != nil {
			return nil, err
		}
		t.Scopes = strings.Fields(scopes)
		out = append(out, t)
	}
	return out, rows.Err()
}

// RevokeOperatorToken revokes id. When userID is non-empty the token must
// belong to that user. Revoking twice is not an error.
func (s *Service) RevokeOperatorToken(ctx context.Context, userID, id string) error {
	q := `UPDATE operator_tokens SET revoked_at = COALESCE(revoked_at, ?) WHERE id = ?`
	args := []any{time.Now().UnixMilli(), id}
	if userID != "" {
		q += ` AND user_id = ?`
		args = append(args, userID)
	}
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrOperatorTokenNotFound
	}
	return nil
}

func (s *Service) getOperatorToken(ctx context.Context, id string) (*OperatorToken, string, error) {
	var t OperatorToken
	var scopes, hash string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, user_id, name, token_hash, scopes, COALESCE(created_by,''), created_at,
                COALESCE(expires_at,0), COALESCE(last_used_at,0), COALESCE(revoked_at,0)
         FROM operator_tokens WHERE id = ?`, id).
		Scan(&t.ID, &t.UserID, &t.Name, &hash, &scopes, &t.CreatedBy, &t.CreatedAt,
			&t.ExpiresAt, &t.LastUsedAt, &t.RevokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", ErrOperatorTokenNotFound
	}
	if err != nil {
		return nil, "", err
	}
	t.Scopes = strings.Fields(scopes)
	return &t, hash, nil
}

// FindUserForOperator resolves a username, email or user id to a user, for
// the local bootstrap command. An empty ref picks the oldest active admin.
func (s *Service) FindUserForOperator(ctx context.Context, ref string) (*User, error) {
	ref = strings.TrimSpace(ref)
	var id string
	var err error
	if ref == "" {
		err = s.db.QueryRowContext(ctx,
			`SELECT id FROM users WHERE role = ? AND status = ? ORDER BY created_at, id LIMIT 1`,
			RoleAdmin, StatusActive).Scan(&id)
	} else {
		err = s.db.QueryRowContext(ctx,
			`SELECT id FROM users WHERE id = ? OR username = ? OR lower(email) = lower(?) LIMIT 1`,
			ref, ref, ref).Scan(&id)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("no matching user %q", ref)
	}
	if err != nil {
		return nil, err
	}
	return s.GetUserByID(ctx, id)
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
