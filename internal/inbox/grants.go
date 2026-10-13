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

	"github.com/tusharbhardwaj/toolyard/internal/actor"
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
	Execution  *Execution            `json:"execution,omitempty"`
	CallID     string                `json:"call_id"`
	AttemptID  string                `json:"attempt_id,omitempty"`
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
	// IssuedBy and RevokedBy are the user ids of the owner who allowed the
	// request and who revoked the grant (empty when not a person, or for
	// grants from before they were recorded).
	IssuedBy  string `json:"issued_by,omitempty"`
	RevokedBy string `json:"revoked_by,omitempty"`
	// IssuedByEmail and IssuedByName are the issuing owner's email and
	// name as recorded on the request they decided, the record the
	// inbox.decide audit row names. Only Redeem fills them.
	IssuedByEmail string `json:"-"`
	IssuedByName  string `json:"-"`
}

// Decider is who authorised a call run under g: the grant is the
// instrument and the owner who issued it is the person.
func (g *Grant) Decider() actor.Decider {
	return actor.Decider{UserID: g.IssuedBy, Email: g.IssuedByEmail, Name: g.IssuedByName, Via: actor.ViaInboxGrant, Ref: g.ID}
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
// grant IDs by tool index. r.DeciderUserID (set by Decide before it calls
// this) is stored as issued_by.
func (s *Service) issueGrants(ctx context.Context, tx *sql.Tx, r *Request, now int64) (map[int]string, error) {
	out := map[int]string{}
	if r.ExecutionMode == "legacy" {
		return out, nil
	}
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
			(id, request_id, tool_index, call_id, agent_id, tool, params, max_uses, uses, status, token_hash, pending_token, created_at, expires_at, issued_by)
			VALUES (?,?,?,?,?,?,?,?,0,?,?,?,?,?,?)`,
			id, r.ID, i, t.CallID, r.AgentID, t.Tool, string(params), 1, GrantActive, hashToken(tok), nil, now, expires, nullStr(r.DeciderUserID)); err != nil {
			return nil, err
		}
		out[i] = id
	}
	return out, nil
}

// Redeem checks a grant token for one call and, if it's valid, uses it up.
// The use counter is incremented with a conditional UPDATE, so two
// concurrent calls can't both redeem a single-use grant. The returned
// grant carries the issuer's email and name (see Grant.Decider).
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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE inbox_grants
 SET uses = uses + 1, last_used_at = ?, pending_token=NULL, status = CASE WHEN uses + 1 >= max_uses THEN ? ELSE status END
 WHERE id = ? AND status = ? AND uses < max_uses AND expires_at > ?`, now, GrantUsed, g.ID, GrantActive, now)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil, &RedeemError{Err: ErrGrantUsed, Grant: g}
	}
	argsJSON, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	g.AttemptID = "gu_" + uuid.NewString()
	if _, err = tx.ExecContext(ctx, `INSERT INTO inbox_grant_uses(id,grant_id,arguments,used_at) VALUES(?,?,?,?)`, g.AttemptID, g.ID, string(argsJSON), now); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO inbox_executions(grant_id,request_id,call_id,agent_id,state,attempt_id,arguments,started_at) VALUES(?,?,?,?,?,?,?,?)`, g.ID, g.RequestID, g.CallID, g.AgentID, ExecutionRunning, g.AttemptID, string(argsJSON), now); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	g.Uses++
	if g.Uses >= g.MaxUses {
		g.Status = GrantUsed
	}
	g.LastUsedAt = now
	s.noteGrantUse(ctx, g, args)
	return g, nil
}

// noteGrantUse records the use on the request's activity timeline, and
// copies the issuer's email and name onto g from the request it loads for
// that, so naming the decider costs no extra query.
func (s *Service) noteGrantUse(ctx context.Context, g *Grant, args map[string]any) {
	_ = s.mutate(ctx, g.RequestID, func(r *Request) error {
		// issued_by was stamped from this request's decider; a different
		// user id means the request no longer names the grant's issuer, so
		// leave the fields empty rather than name someone else.
		if d := r.Decider(); d.UserID == g.IssuedBy {
			g.IssuedByEmail, g.IssuedByName = d.Email, d.Name
		}
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
	row := s.db.QueryRowContext(ctx, `SELECT id, request_id, tool_index, call_id, agent_id, tool, params, max_uses, uses, status,
		created_at, expires_at, COALESCE(revoked_at,0), COALESCE(last_used_at,0), COALESCE(issued_by,''), COALESCE(revoked_by,''), token_hash
		FROM inbox_grants WHERE id = ?`, id)
	var g Grant
	var params, hash string
	if err := row.Scan(&g.ID, &g.RequestID, &g.ToolIndex, &g.CallID, &g.AgentID, &g.Tool, &params, &g.MaxUses, &g.Uses, &g.Status,
		&g.CreatedAt, &g.ExpiresAt, &g.RevokedAt, &g.LastUsedAt, &g.IssuedBy, &g.RevokedBy, &hash); err != nil {
		return nil, "", err
	}
	if err := json.Unmarshal([]byte(params), &g.Params); err != nil {
		return nil, "", err
	}
	if r, e := s.Get(ctx, g.RequestID); g.CallID == "" && e == nil && g.ToolIndex >= 0 && g.ToolIndex < len(r.Tools) {
		g.CallID = r.Tools[g.ToolIndex].CallID
	}
	return &g, hash, nil
}

// ListGrants returns grants, newest first. status "" means all.
func (s *Service) ListGrants(ctx context.Context, status, agentID string, limit int) ([]Grant, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	q := `SELECT id, request_id, tool_index, call_id, agent_id, tool, params, max_uses, uses, status, created_at, expires_at,
		COALESCE(revoked_at,0), COALESCE(last_used_at,0), COALESCE(issued_by,''), COALESCE(revoked_by,'') FROM inbox_grants WHERE 1=1`
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
		if err := rows.Scan(&g.ID, &g.RequestID, &g.ToolIndex, &g.CallID, &g.AgentID, &g.Tool, &params, &g.MaxUses, &g.Uses, &g.Status,
			&g.CreatedAt, &g.ExpiresAt, &g.RevokedAt, &g.LastUsedAt, &g.IssuedBy, &g.RevokedBy); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(params), &g.Params)
		out = append(out, g)
	}
	return out, rows.Err()
}

// RevokeGrant revokes one active grant; by is the display label for the
// activity line. RevokeGrantBy also records who did it.
func (s *Service) RevokeGrant(ctx context.Context, id, by string) error {
	return s.RevokeGrantBy(ctx, id, actor.Decider{Name: strings.TrimSpace(by), Via: actor.ViaDashboard})
}

// RevokeGrantBy revokes one active grant and records the revoking user
// (by.UserID) as revoked_by. The activity line names them.
func (s *Service) RevokeGrantBy(ctx context.Context, id string, by actor.Decider) error {
	now := s.now().UnixMilli()
	res, err := s.db.ExecContext(ctx, `UPDATE inbox_grants SET status = ?, revoked_at = ?, revoked_by = ?, pending_token = NULL
		WHERE id = ? AND status = ?`,
		GrantRevoked, now, nullStr(by.UserID), id, GrantActive)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	g, _, err := s.getGrantWithHash(ctx, id)
	if err == nil {
		who := by.Name
		if who == "" {
			who = by.Email
		}
		_ = s.mutate(ctx, g.RequestID, func(r *Request) error {
			r.addActivity(now, fmt.Sprintf("%s revoked the permission for %s", nonEmpty(who, "Owner"), g.Tool))
			return nil
		})
	}
	s.publish("grant", map[string]any{"id": id, "status": GrantRevoked})
	return nil
}

// RevokeAll revokes every active grant (optionally for one agent) and
// returns how many it revoked. This is the kill switch. RevokeAllBy also
// records who pulled it.
func (s *Service) RevokeAll(ctx context.Context, agentID string) (int, error) {
	return s.RevokeAllBy(ctx, agentID, actor.Decider{})
}

// RevokeAllBy is RevokeAll recording by.UserID as revoked_by.
func (s *Service) RevokeAllBy(ctx context.Context, agentID string, by actor.Decider) (int, error) {
	now := s.now().UnixMilli()
	q := `UPDATE inbox_grants SET status = ?, revoked_at = ?, revoked_by = ?, pending_token = NULL WHERE status = ?`
	args := []any{GrantRevoked, now, nullStr(by.UserID), GrantActive}
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

// collectTokens deterministically recovers the original token for each valid,
// unused permission. Reads never consume permission or mint additional uses.
func (s *Service) collectTokens(ctx context.Context, requestID, agentID string) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM inbox_grants WHERE request_id=? AND agent_id=? AND status=? AND uses<max_uses AND expires_at>?`, requestID, agentID, GrantActive, s.now().UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = s.signer.token(id, agentID)
	}
	return out, rows.Err()
}

func nonEmpty(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
