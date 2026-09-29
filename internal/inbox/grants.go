package inbox

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// A grant is permission for one agent to make one call to one tool, within
// the parameters the owner approved, before it expires. The token the agent
// holds is:
//
//	tyg_<grant id>.<base64url ed25519 signature over "toolyard-grant-v1\n<id>\n<agent id>">
//
// Only its sha256 is kept once the agent has collected it. The scope lives
// in the database, never in the token, so revoking or narrowing takes
// effect immediately.

// Grant statuses.
const (
	GrantActive  = "active"
	GrantUsed    = "used"
	GrantExpired = "expired"
	GrantRevoked = "revoked"
)

const grantKeyPurpose = "grant_sign"

// Grant is the stored shape, minus the token.
type Grant struct {
	ID         string                `json:"id"`
	RequestID  string                `json:"request_id"`
	ToolIndex  int                   `json:"tool_index"`
	AgentID    string                `json:"agent_id"`
	Tool       string                `json:"tool"`
	Params     map[string]Constraint `json:"params"`
	MaxUses    int                   `json:"max_uses"`
	Uses       int                   `json:"uses"`
	Status     string                `json:"status"`
	CreatedAt  int64                 `json:"created_at"`
	ExpiresAt  int64                 `json:"expires_at"`
	RevokedAt  int64                 `json:"revoked_at,omitempty"`
	LastUsedAt int64                 `json:"last_used_at,omitempty"`
}

// Redemption failures. The gateway turns these into a grant_invalid result
// that tells the agent which check failed.
var (
	ErrGrantMalformed = errors.New("the grant token is malformed")
	ErrGrantUnknown   = errors.New("no such grant")
	ErrGrantAgent     = errors.New("this grant was issued to a different agent")
	ErrGrantTool      = errors.New("this grant is for a different tool")
	ErrGrantExpired   = errors.New("this grant has expired")
	ErrGrantUsed      = errors.New("this grant has already been used")
	ErrGrantRevoked   = errors.New("your owner revoked this grant")
	ErrGrantScope     = errors.New("the call is outside the grant's parameters")
)

// RedeemError carries the grant (when known) and a human detail.
type RedeemError struct {
	Err    error
	Grant  *Grant
	Detail string
}

func (e *RedeemError) Error() string {
	if e.Detail != "" {
		return e.Err.Error() + ": " + e.Detail
	}
	return e.Err.Error()
}

func (e *RedeemError) Unwrap() error { return e.Err }

type grantSigner struct {
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey
}

func loadGrantSigner(ctx context.Context, db *store.DB) (*grantSigner, error) {
	var priv, pub []byte
	err := db.QueryRowContext(ctx, `SELECT private_key, public_key FROM server_keys WHERE purpose = ?`, grantKeyPurpose).Scan(&priv, &pub)
	if err == nil {
		return &grantSigner{priv: ed25519.PrivateKey(priv), pub: ed25519.PublicKey(pub)}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	pubKey, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO server_keys(id, purpose, private_key, public_key, created_at) VALUES(?,?,?,?,?)`,
		"key_"+uuid.NewString(), grantKeyPurpose, []byte(privKey), []byte(pubKey), time.Now().UnixMilli()); err != nil {
		return nil, err
	}
	return &grantSigner{priv: privKey, pub: pubKey}, nil
}

func grantMessage(id, agentID string) []byte {
	return []byte("toolyard-grant-v1\n" + id + "\n" + agentID)
}

func (s *grantSigner) token(id, agentID string) string {
	sig := ed25519.Sign(s.priv, grantMessage(id, agentID))
	return "tyg_" + base64.RawURLEncoding.EncodeToString([]byte(id)) + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// parseToken returns the grant ID and signature.
func parseToken(tok string) (string, []byte, error) {
	tok = strings.TrimSpace(tok)
	if !strings.HasPrefix(tok, "tyg_") {
		return "", nil, ErrGrantMalformed
	}
	body := strings.TrimPrefix(tok, "tyg_")
	dot := strings.IndexByte(body, '.')
	if dot <= 0 {
		return "", nil, ErrGrantMalformed
	}
	idb, err1 := base64.RawURLEncoding.DecodeString(body[:dot])
	sig, err2 := base64.RawURLEncoding.DecodeString(body[dot+1:])
	if err1 != nil || err2 != nil || len(sig) != ed25519.SignatureSize {
		return "", nil, ErrGrantMalformed
	}
	return string(idb), sig, nil
}

func hashToken(tok string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(tok)))
	return hex.EncodeToString(sum[:])
}

// issueGrants creates one grant per allowed tool inside tx. It returns the
// grant IDs by tool index.
func (s *Service) issueGrants(ctx context.Context, tx *sql.Tx, r *Request, now int64) (map[int]string, error) {
	out := map[int]string{}
	expires := now + int64(r.TTLSeconds)*1000
	for i, t := range r.Tools {
		if t.Decision != ToolAllowed {
			continue
		}
		id := "gr_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
		tok := s.signer.token(id, r.AgentID)
		params, err := json.Marshal(t.Params)
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO inbox_grants
			(id, request_id, tool_index, agent_id, tool, params, max_uses, uses, status, token_hash, pending_token, created_at, expires_at)
			VALUES (?,?,?,?,?,?,?,0,?,?,?,?,?)`,
			id, r.ID, i, r.AgentID, t.Tool, string(params), 1, GrantActive, hashToken(tok), tok, now, expires); err != nil {
			return nil, err
		}
		out[i] = id
	}
	return out, nil
}

// Redeem checks a grant token for one call and, if it's valid, uses it up.
// The use counter is incremented with a conditional UPDATE, so two
// concurrent calls can't both redeem a single-use grant.
func (s *Service) Redeem(ctx context.Context, token, agentID, tool string, args map[string]any) (*Grant, error) {
	id, sig, err := parseToken(token)
	if err != nil {
		return nil, &RedeemError{Err: err}
	}
	g, hash, err := s.getGrantWithHash(ctx, id)
	if err != nil {
		return nil, &RedeemError{Err: ErrGrantUnknown}
	}
	if subtle.ConstantTimeCompare([]byte(hash), []byte(hashToken(token))) != 1 ||
		!ed25519.Verify(s.signer.pub, grantMessage(g.ID, g.AgentID), sig) {
		return nil, &RedeemError{Err: ErrGrantUnknown}
	}
	if g.AgentID != agentID {
		return nil, &RedeemError{Err: ErrGrantAgent, Grant: g}
	}
	now := s.now().UnixMilli()
	switch {
	case g.Status == GrantRevoked:
		return nil, &RedeemError{Err: ErrGrantRevoked, Grant: g}
	case g.Status == GrantUsed || g.Uses >= g.MaxUses:
		return nil, &RedeemError{Err: ErrGrantUsed, Grant: g}
	case g.Status == GrantExpired || now >= g.ExpiresAt:
		return nil, &RedeemError{Err: ErrGrantExpired, Grant: g}
	}
	if g.Tool != tool {
		return nil, &RedeemError{Err: ErrGrantTool, Grant: g, Detail: fmt.Sprintf("it covers %s, not %s", g.Tool, tool)}
	}
	if ok, why := MatchArgs(g.Params, args); !ok {
		return nil, &RedeemError{Err: ErrGrantScope, Grant: g, Detail: why}
	}
	res, err := s.db.ExecContext(ctx, `UPDATE inbox_grants
		SET uses = uses + 1, last_used_at = ?, status = CASE WHEN uses + 1 >= max_uses THEN ? ELSE status END
		WHERE id = ? AND status = ? AND uses < max_uses AND expires_at > ?`,
		now, GrantUsed, g.ID, GrantActive, now)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil, &RedeemError{Err: ErrGrantUsed, Grant: g}
	}
	argsJSON, _ := json.Marshal(args)
	_, _ = s.db.ExecContext(ctx, `INSERT INTO inbox_grant_uses(id, grant_id, arguments, used_at) VALUES (?,?,?,?)`,
		"gu_"+uuid.NewString(), g.ID, string(argsJSON), now)
	g.Uses++
	if g.Uses >= g.MaxUses {
		g.Status = GrantUsed
	}
	g.LastUsedAt = now
	s.noteGrantUse(ctx, g, args)
	return g, nil
}

// noteGrantUse records the use on the request's activity timeline.
func (s *Service) noteGrantUse(ctx context.Context, g *Grant, args map[string]any) {
	_ = s.mutate(ctx, g.RequestID, func(r *Request) error {
		text := "Used " + g.Tool
		for k, c := range g.Params {
			if c.Op == "limit" {
				text += fmt.Sprintf(" (%s = %s)", k, compactJSON(normalise(args[k])))
			}
		}
		r.addActivity(s.now().UnixMilli(), text)
		return nil
	})
	s.publish("grant", map[string]any{"id": g.ID, "request_id": g.RequestID, "status": g.Status})
}

func (s *Service) getGrantWithHash(ctx context.Context, id string) (*Grant, string, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, request_id, tool_index, agent_id, tool, params, max_uses, uses, status,
		created_at, expires_at, COALESCE(revoked_at,0), COALESCE(last_used_at,0), token_hash FROM inbox_grants WHERE id = ?`, id)
	var g Grant
	var params, hash string
	if err := row.Scan(&g.ID, &g.RequestID, &g.ToolIndex, &g.AgentID, &g.Tool, &params, &g.MaxUses, &g.Uses, &g.Status,
		&g.CreatedAt, &g.ExpiresAt, &g.RevokedAt, &g.LastUsedAt, &hash); err != nil {
		return nil, "", err
	}
	if err := json.Unmarshal([]byte(params), &g.Params); err != nil {
		return nil, "", err
	}
	return &g, hash, nil
}

// ListGrants returns grants, newest first. status "" means all.
func (s *Service) ListGrants(ctx context.Context, status, agentID string, limit int) ([]Grant, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	q := `SELECT id, request_id, tool_index, agent_id, tool, params, max_uses, uses, status, created_at, expires_at,
		COALESCE(revoked_at,0), COALESCE(last_used_at,0) FROM inbox_grants WHERE 1=1`
	var args []any
	if status != "" {
		q += ` AND status = ?`
		args = append(args, status)
	}
	if agentID != "" {
		q += ` AND agent_id = ?`
		args = append(args, agentID)
	}
	q += ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Grant
	for rows.Next() {
		var g Grant
		var params string
		if err := rows.Scan(&g.ID, &g.RequestID, &g.ToolIndex, &g.AgentID, &g.Tool, &params, &g.MaxUses, &g.Uses, &g.Status,
			&g.CreatedAt, &g.ExpiresAt, &g.RevokedAt, &g.LastUsedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(params), &g.Params)
		out = append(out, g)
	}
	return out, rows.Err()
}

// RevokeGrant revokes one active grant.
func (s *Service) RevokeGrant(ctx context.Context, id, by string) error {
	now := s.now().UnixMilli()
	res, err := s.db.ExecContext(ctx, `UPDATE inbox_grants SET status = ?, revoked_at = ?, pending_token = NULL WHERE id = ? AND status = ?`,
		GrantRevoked, now, id, GrantActive)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	g, _, err := s.getGrantWithHash(ctx, id)
	if err == nil {
		_ = s.mutate(ctx, g.RequestID, func(r *Request) error {
			r.addActivity(now, fmt.Sprintf("%s revoked the permission for %s", nonEmpty(by, "Owner"), g.Tool))
			return nil
		})
	}
	s.publish("grant", map[string]any{"id": id, "status": GrantRevoked})
	return nil
}

// RevokeAll revokes every active grant (optionally for one agent) and
// returns how many it revoked. This is the kill switch.
func (s *Service) RevokeAll(ctx context.Context, agentID string) (int, error) {
	now := s.now().UnixMilli()
	q := `UPDATE inbox_grants SET status = ?, revoked_at = ?, pending_token = NULL WHERE status = ?`
	args := []any{GrantRevoked, now, GrantActive}
	if agentID != "" {
		q += ` AND agent_id = ?`
		args = append(args, agentID)
	}
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	s.publish("grant", map[string]any{"revoked_all": n})
	return int(n), nil
}

// expireGrants marks lapsed active grants expired.
func (s *Service) expireGrants(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE inbox_grants SET status = ?, pending_token = NULL WHERE status = ? AND expires_at <= ?`,
		GrantExpired, GrantActive, s.now().UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// collectTokens returns plaintext tokens for a request's grants that the
// agent hasn't collected yet, and clears them. Tokens are handed over once.
func (s *Service) collectTokens(ctx context.Context, requestID, agentID string) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, pending_token FROM inbox_grants
		WHERE request_id = ? AND agent_id = ? AND pending_token IS NOT NULL AND status = ?`, requestID, agentID, GrantActive)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for rows.Next() {
		var id, tok string
		if err := rows.Scan(&id, &tok); err != nil {
			rows.Close()
			return nil, err
		}
		out[id] = tok
	}
	rows.Close()
	for id := range out {
		if _, err := s.db.ExecContext(ctx, `UPDATE inbox_grants SET pending_token = NULL WHERE id = ?`, id); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func nonEmpty(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
