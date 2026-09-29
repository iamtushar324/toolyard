// Package identitykeys issues each dashboard user one identity key and
// forwards it to Beknown services so their audit logs record the person.
//
// The key is the token of the user's dedicated identity agent
// (agents.kind = 'identity'): agents present it at /mcp as a bearer token
// or in x-bf-vk, exactly where a Bifrost virtual key went before. toolyard
// keeps the raw key sealed, forwards it on tool calls to upstreams whose
// identity setting names a header (x-bk-bifrost-vk), and registers its
// sha256 fingerprint in prime-service's bifrost_virtual_key_actors registry
// through each registry upstream's upsert-bifrost-virtual-key-actor tool,
// acting as the admin who provisioned it.
//
// Lifecycle (admins provision, the owner reveals once):
//
//	Issue    mint, store sealed, register the fingerprint everywhere
//	Reveal   the owner sees the raw key once (revealed_at); again needs Rotate
//	Rotate   mint anew, register the new fingerprint, delete the old one,
//	         then swap the agent token so the old key dies at once
//	Revoke   delete the fingerprint everywhere, then the key and the agent
//	Register retry the (idempotent) upsert after a failure or bootstrap
//
// A registration row's status is toolyard's best knowledge of the registry:
// "registered" (the row's hash is there; error may carry a note such as a
// stale previous fingerprint), "error" (the upsert failed; the key is kept
// and the admin retries) or "revoked" (deleted).
package identitykeys

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/gateway"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/sealbox"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

const (
	// AgentName is the identity agent's display name in the agents list.
	AgentName = "Beknown key"

	// The registry tools every registry upstream exposes; called through
	// the gateway as "<upstream>.<tool>".
	ToolUpsert = "upsert-bifrost-virtual-key-actor"
	ToolDelete = "delete-bifrost-virtual-key-actor"

	// ViaTool tags the internal registry calls in audit and metrics.
	ViaTool = "identity_key"

	// cacheTTL bounds how stale a forwarded key can be when a write skips
	// Invalidate (mirrors access.cacheTTL).
	cacheTTL = 5 * time.Second

	// maxErrorLen caps the registry error text stored per row.
	maxErrorLen = 500
	// maxLabelLen is the registry's label limit.
	maxLabelLen = 255
)

// Registration statuses (identity_key_registrations.status).
const (
	StatusRegistered = "registered"
	StatusError      = "error"
	StatusRevoked    = "revoked"
)

// Audit event types. AgentID is the actor ("user:<uid>"), ResultSummary
// names the target user; register events also carry the upstream, the
// status as Decision and the error as Reason. The key never appears.
const (
	EventIssue    = "identity_key.issue"
	EventRotate   = "identity_key.rotate"
	EventReveal   = "identity_key.reveal"
	EventRevoke   = "identity_key.revoke"
	EventRegister = "identity_key.register"
)

var (
	// ErrNoKey: the user has no identity key.
	ErrNoKey = errors.New("no identity key")
	// ErrKeyExists: Issue on a user who already has one (use Rotate).
	ErrKeyExists = errors.New("identity key already issued")
	// ErrAlreadyRevealed: the owner has seen this key; only a rotate shows
	// a new one.
	ErrAlreadyRevealed = errors.New("identity key already revealed; rotate to get a new one")
	// ErrNoClerkIdentity: registration needs the person's Clerk user id
	// and email, so the user must sign in with Google once first.
	ErrNoClerkIdentity = errors.New("user has no Clerk identity; they must sign in with Google once")
	// ErrNotWired: the registry caller or lister isn't set.
	ErrNotWired = errors.New("identity key registry not wired")
)

// RegistryLister names the upstreams where fingerprints are registered:
// servers whose identity setting has register = true.
type RegistryLister interface {
	RegistryUpstreams(ctx context.Context) ([]string, error)
}

// RegistryListerFunc adapts a function to RegistryLister.
type RegistryListerFunc func(ctx context.Context) ([]string, error)

func (f RegistryListerFunc) RegistryUpstreams(ctx context.Context) ([]string, error) { return f(ctx) }

// InternalCaller dispatches one gateway tool call as the ctx caller
// (gateway.WithAgentID), skipping access checks and approval but not
// identity forwarding. *gateway.Gateway satisfies it.
type InternalCaller interface {
	CallInternal(ctx context.Context, viaTool, target string, args map[string]any) (*mcp.CallToolResult, error)
}

// Registration is one registry upstream's view of the user's key.
type Registration struct {
	Upstream  string `json:"upstream"`
	Status    string `json:"status"`
	Error     string `json:"error"`
	UpdatedAt int64  `json:"updated_at"`
	// keyHash is the fingerprint the row refers to; not exposed because the
	// key's own fingerprint is on Status.
	keyHash string
}

// Status is what the dashboard shows for a user's identity key. The
// fingerprint is not secret (the registry stores it); the bootstrap admin
// registers it by hand.
type Status struct {
	HasKey        bool           `json:"has_key"`
	Revealed      bool           `json:"revealed"`
	Fingerprint   string         `json:"fingerprint,omitempty"`
	CreatedAt     int64          `json:"created_at,omitempty"`
	Registrations []Registration `json:"registrations"`
}

// Service owns identity keys and their registrations.
type Service struct {
	db         *store.DB
	cipher     *sealbox.Cipher
	audit      *audit.Logger
	registries RegistryLister
	caller     InternalCaller

	mu     sync.Mutex
	keys   map[string]cached[string] // user id -> raw key ("" = none)
	owners map[string]cached[string] // agent id -> owner user id ("" = unknown)
	now    func() time.Time
}

type cached[T any] struct {
	v   T
	exp time.Time
}

// New returns a Service that seals keys with cipher. Wire the audit log,
// the registry lister and the internal caller with the setters; without a
// lister there is nowhere to register and without a caller every
// registration records an error.
func New(db *store.DB, cipher *sealbox.Cipher) *Service {
	return &Service{
		db:     db,
		cipher: cipher,
		keys:   map[string]cached[string]{},
		owners: map[string]cached[string]{},
		now:    time.Now,
	}
}

// SetAudit records the lifecycle events on log.
func (s *Service) SetAudit(log *audit.Logger) { s.audit = log }

// SetRegistries names the registry upstreams.
func (s *Service) SetRegistries(l RegistryLister) { s.registries = l }

// SetCaller is the gateway the registry tools are called through.
func (s *Service) SetCaller(c InternalCaller) { s.caller = c }

// ---- reads -------------------------------------------------------------------

type keyRow struct {
	UserID     string
	AgentID    string
	KeyHash    string
	KeySealed  string
	RevealedAt int64
	CreatedBy  string
	CreatedAt  int64
	UpdatedAt  int64
}

type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// keyRow returns the user's key row, or nil when there is none.
func keyRowFor(ctx context.Context, q rowQuerier, userID string) (*keyRow, error) {
	var k keyRow
	var revealed sql.NullInt64
	var createdBy sql.NullString
	err := q.QueryRowContext(ctx,
		`SELECT user_id, agent_id, key_hash, key_sealed, revealed_at, created_by, created_at, updated_at
         FROM user_identity_keys WHERE user_id = ?`, userID).
		Scan(&k.UserID, &k.AgentID, &k.KeyHash, &k.KeySealed, &revealed, &createdBy, &k.CreatedAt, &k.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	k.RevealedAt = revealed.Int64
	k.CreatedBy = createdBy.String
	return &k, nil
}

// registrations lists the user's registry rows sorted by upstream. The
// cursor is drained before returning so callers may query again.
func (s *Service) registrations(ctx context.Context, userID string) ([]Registration, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT upstream, key_hash, status, COALESCE(error,''), updated_at
         FROM identity_key_registrations WHERE user_id = ? ORDER BY upstream`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Registration{}
	for rows.Next() {
		var r Registration
		if err := rows.Scan(&r.Upstream, &r.keyHash, &r.Status, &r.Error, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Status reports the user's key and where it is registered. A user with no
// key gets HasKey=false and whatever registry rows remain (revoked, or a
// stale fingerprint a failed revoke could not remove).
func (s *Service) Status(ctx context.Context, userID string) (*Status, error) {
	k, err := keyRowFor(ctx, s.db, userID)
	if err != nil {
		return nil, err
	}
	regs, err := s.registrations(ctx, userID)
	if err != nil {
		return nil, err
	}
	st := &Status{Registrations: regs}
	if k != nil {
		st.HasKey = true
		st.Revealed = k.RevealedAt > 0
		st.Fingerprint = k.KeyHash
		st.CreatedAt = k.CreatedAt
	}
	return st, nil
}

// HasKey reports whether the user holds an identity key.
func (s *Service) HasKey(ctx context.Context, userID string) (bool, error) {
	k, err := keyRowFor(ctx, s.db, userID)
	return k != nil, err
}

// userInfo is what registration needs to know about the person.
type userInfo struct {
	Username    string
	Email       string
	ClerkUserID string
	Status      string
}

func (s *Service) user(ctx context.Context, userID string) (*userInfo, error) {
	var u userInfo
	err := s.db.QueryRowContext(ctx,
		`SELECT username, COALESCE(email,''), COALESCE(clerk_user_id,''), status FROM users WHERE id = ?`, userID).
		Scan(&u.Username, &u.Email, &u.ClerkUserID, &u.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, identity.ErrNoUser
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// registrableUser is user plus the Clerk identity check every registry
// call needs.
func (s *Service) registrableUser(ctx context.Context, userID string) (*userInfo, error) {
	u, err := s.user(ctx, userID)
	if err != nil {
		return nil, err
	}
	if u.ClerkUserID == "" || u.Email == "" {
		return nil, ErrNoClerkIdentity
	}
	return u, nil
}

// ---- lifecycle ---------------------------------------------------------------

// Provision is what the admin's button does: Issue when the user has no
// key, Rotate when they do.
func (s *Service) Provision(ctx context.Context, actorID, userID string) (*Status, error) {
	has, err := s.HasKey(ctx, userID)
	if err != nil {
		return nil, err
	}
	if has {
		return s.Rotate(ctx, actorID, userID)
	}
	return s.Issue(ctx, actorID, userID)
}

// Issue mints the user's identity key, stores it sealed as the token of a
// new identity agent, and registers its fingerprint in every registry
// upstream as actorID. The raw key is never returned here: the owner
// reveals it. A registry failure keeps the key and records the error.
func (s *Service) Issue(ctx context.Context, actorID, userID string) (*Status, error) {
	u, err := s.registrableUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	if has, err := s.HasKey(ctx, userID); err != nil {
		return nil, err
	} else if has {
		return nil, ErrKeyExists
	}
	agentID := identity.NewAgentID()
	token, hash, err := identity.NewAgentToken(agentID)
	if err != nil {
		return nil, err
	}
	sealed, err := s.cipher.Seal([]byte(token), []byte(userID))
	if err != nil {
		return nil, err
	}
	now := s.now().UnixMilli()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO agents(id, name, owner_user, token_hash, kind, created_at) VALUES(?,?,?,?,?,?)`,
		agentID, AgentName, userID, hash, identity.AgentKindIdentity, now); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO user_identity_keys(user_id, agent_id, key_hash, key_sealed, revealed_at, created_by, created_at, updated_at)
         VALUES(?,?,?,?,NULL,?,?,?)`,
		userID, agentID, hash, sealed, nullStr(actorID), now, now); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	s.Invalidate(userID)
	s.event(ctx, EventIssue, actorID, userID, "", "", "", "")
	if err := s.registerAll(ctx, actorID, userID, u, hash); err != nil {
		return nil, err
	}
	return s.Status(ctx, userID)
}

// Rotate mints a new key, registers the new fingerprint, deletes the old
// one from each registry, then swaps the identity agent's token so the old
// key stops authenticating at once. The new key is unrevealed.
func (s *Service) Rotate(ctx context.Context, actorID, userID string) (*Status, error) {
	u, err := s.registrableUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	k, err := keyRowFor(ctx, s.db, userID)
	if err != nil {
		return nil, err
	}
	if k == nil {
		return nil, ErrNoKey
	}
	token, hash, err := identity.NewAgentToken(k.AgentID)
	if err != nil {
		return nil, err
	}
	sealed, err := s.cipher.Seal([]byte(token), []byte(userID))
	if err != nil {
		return nil, err
	}
	if err := s.registerAll(ctx, actorID, userID, u, hash); err != nil {
		return nil, err
	}
	now := s.now().UnixMilli()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`UPDATE agents SET token_hash = ?, prev_token_hash = NULL, prev_token_expires = NULL, disabled = 0
         WHERE id = ? AND kind = ?`, hash, k.AgentID, identity.AgentKindIdentity); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE user_identity_keys SET key_hash = ?, key_sealed = ?, revealed_at = NULL, created_by = ?, updated_at = ?
         WHERE user_id = ?`, hash, sealed, nullStr(actorID), now, userID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	s.Invalidate(userID)
	s.event(ctx, EventRotate, actorID, userID, "", "", "", "")
	return s.Status(ctx, userID)
}

// Reveal returns the raw key and its fingerprint to the owner exactly once.
func (s *Service) Reveal(ctx context.Context, userID string) (key, fingerprint string, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = tx.Rollback() }()
	k, err := keyRowFor(ctx, tx, userID)
	if err != nil {
		return "", "", err
	}
	if k == nil {
		return "", "", ErrNoKey
	}
	if k.RevealedAt > 0 {
		return "", "", ErrAlreadyRevealed
	}
	raw, err := s.cipher.Open(k.KeySealed, []byte(userID))
	if err != nil {
		return "", "", fmt.Errorf("unseal identity key: %w", err)
	}
	now := s.now().UnixMilli()
	if _, err := tx.ExecContext(ctx,
		`UPDATE user_identity_keys SET revealed_at = ?, updated_at = ? WHERE user_id = ?`, now, now, userID); err != nil {
		return "", "", err
	}
	if err := tx.Commit(); err != nil {
		return "", "", err
	}
	s.event(ctx, EventReveal, userID, userID, "", "", "", "")
	return string(raw), k.KeyHash, nil
}

// Revoke deletes the fingerprint from every registry it is in, then
// removes the key and the identity agent. The local removal happens even
// when a registry delete fails: the key must die now. Such a row keeps
// status "registered" with the failure in error, and the next Issue for
// the user removes the stale fingerprint before registering the new one.
func (s *Service) Revoke(ctx context.Context, actorID, userID string) (*Status, error) {
	k, err := keyRowFor(ctx, s.db, userID)
	if err != nil {
		return nil, err
	}
	if k == nil {
		return nil, ErrNoKey
	}
	regs, err := s.registrations(ctx, userID)
	if err != nil {
		return nil, err
	}
	callCtx := gateway.WithAgentID(ctx, "dashboard:"+actorID)
	failed := 0
	for _, r := range regs {
		switch r.Status {
		case StatusRevoked:
			continue
		case StatusError:
			// Never registered as far as we know: nothing to delete.
			if _, err := s.db.ExecContext(ctx,
				`DELETE FROM identity_key_registrations WHERE user_id = ? AND upstream = ?`, userID, r.Upstream); err != nil {
				return nil, err
			}
			continue
		}
		if derr := s.call(callCtx, r.Upstream+"."+ToolDelete, map[string]any{
			"virtualKeyHash": r.keyHash, "confirm": true,
		}); derr != nil {
			failed++
			if err := s.setRegistration(ctx, userID, r.Upstream, r.keyHash, StatusRegistered, "revoke: "+derr.Error(), actorID); err != nil {
				return nil, err
			}
			s.event(ctx, EventRegister, actorID, userID, r.Upstream, StatusRegistered, derr.Error(), "revoke failed")
			continue
		}
		if err := s.setRegistration(ctx, userID, r.Upstream, r.keyHash, StatusRevoked, "", actorID); err != nil {
			return nil, err
		}
		s.event(ctx, EventRegister, actorID, userID, r.Upstream, StatusRevoked, "", "")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM user_identity_keys WHERE user_id = ?`, userID); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM agents WHERE id = ? AND kind = ?`, k.AgentID, identity.AgentKindIdentity); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	s.Invalidate(userID)
	s.event(ctx, EventRevoke, actorID, userID, "", "", "", fmt.Sprintf("registry_failures=%d", failed))
	return s.Status(ctx, userID)
}

// Register (re)registers the user's current fingerprint in every registry
// upstream as actorID. The upsert is idempotent, so this is the retry
// after an error and the bootstrap step after the first admin's
// fingerprint was registered by hand.
func (s *Service) Register(ctx context.Context, actorID, userID string) (*Status, error) {
	u, err := s.registrableUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	k, err := keyRowFor(ctx, s.db, userID)
	if err != nil {
		return nil, err
	}
	if k == nil {
		return nil, ErrNoKey
	}
	if err := s.registerAll(ctx, actorID, userID, u, k.KeyHash); err != nil {
		return nil, err
	}
	return s.Status(ctx, userID)
}

// registerAll upserts hash in every registry upstream as actorID and
// records one row per upstream. Where the upstream still holds a different
// fingerprint of this user's (a rotate, or a stale one from a failed
// revoke), that one is deleted after the upsert succeeds; a failed delete
// leaves a note in error but the row stays registered. Only store errors
// are returned; registry failures are recorded on the rows.
func (s *Service) registerAll(ctx context.Context, actorID, userID string, u *userInfo, hash string) error {
	var upstreams []string
	if s.registries != nil {
		var err error
		upstreams, err = s.registries.RegistryUpstreams(ctx)
		if err != nil {
			return err
		}
	}
	sort.Strings(upstreams)
	regs, err := s.registrations(ctx, userID)
	if err != nil {
		return err
	}
	prev := make(map[string]Registration, len(regs))
	for _, r := range regs {
		prev[r.Upstream] = r
	}
	callCtx := gateway.WithAgentID(ctx, "dashboard:"+actorID)
	args := map[string]any{
		"virtualKeyHash": hash,
		"userId":         u.ClerkUserID,
		"email":          u.Email,
		"label":          label(u.Username),
		"isActive":       true,
	}
	for _, up := range upstreams {
		if uerr := s.call(callCtx, up+"."+ToolUpsert, args); uerr != nil {
			if err := s.setRegistration(ctx, userID, up, hash, StatusError, uerr.Error(), actorID); err != nil {
				return err
			}
			s.event(ctx, EventRegister, actorID, userID, up, StatusError, uerr.Error(), "")
			continue
		}
		note := ""
		if p, ok := prev[up]; ok && p.Status == StatusRegistered && p.keyHash != hash {
			if derr := s.call(callCtx, up+"."+ToolDelete, map[string]any{
				"virtualKeyHash": p.keyHash, "confirm": true,
			}); derr != nil {
				note = "previous fingerprint not removed: " + derr.Error()
			}
		}
		if err := s.setRegistration(ctx, userID, up, hash, StatusRegistered, note, actorID); err != nil {
			return err
		}
		s.event(ctx, EventRegister, actorID, userID, up, StatusRegistered, note, "")
	}
	return nil
}

func (s *Service) setRegistration(ctx context.Context, userID, upstream, hash, status, errText, actorID string) error {
	errText = truncate(errText, maxErrorLen)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO identity_key_registrations(user_id, upstream, key_hash, status, error, by_user, updated_at)
         VALUES(?,?,?,?,?,?,?)
         ON CONFLICT(user_id, upstream) DO UPDATE SET
           key_hash = excluded.key_hash, status = excluded.status, error = excluded.error,
           by_user = excluded.by_user, updated_at = excluded.updated_at`,
		userID, upstream, hash, status, nullStr(errText), nullStr(actorID), s.now().UnixMilli())
	return err
}

// call runs one registry tool through the gateway and turns a tool-level
// error (isError) into a Go error carrying the result text.
func (s *Service) call(ctx context.Context, target string, args map[string]any) error {
	if s.caller == nil {
		return ErrNotWired
	}
	res, err := s.caller.CallInternal(ctx, ViaTool, target, args)
	if err != nil {
		return err
	}
	if res == nil {
		return errors.New("empty result")
	}
	if res.IsError {
		msg := strings.TrimSpace(resultText(res))
		if msg == "" {
			msg = "tool returned an error"
		}
		return errors.New(msg)
	}
	return nil
}

func resultText(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if t, ok := mcp.AsTextContent(c); ok {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(t.Text)
		}
	}
	return b.String()
}

// label is the registry's human label for the key.
func label(username string) string {
	return truncate("toolyard: "+username, maxLabelLen)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// event writes one audit row: the actor as AgentID ("user:<uid>"), the
// target user (plus any note) as ResultSummary. Never the key.
func (s *Service) event(ctx context.Context, typ, actorID, targetID, upstream, decision, reason, note string) {
	if s.audit == nil {
		return
	}
	summary := "user:" + targetID
	if note != "" {
		summary += " " + note
	}
	_ = s.audit.Write(ctx, audit.Event{
		EventType:     typ,
		AgentID:       "user:" + actorID,
		UpstreamName:  upstream,
		Decision:      decision,
		Reason:        truncate(reason, maxErrorLen),
		ResultSummary: summary,
	})
}

// ---- forwarding --------------------------------------------------------------

// ForwardKey implements gateway.IdentityResolver: the raw identity key of
// the caller's user, from a short cache. An agent caller resolves through
// its owner; "dashboard:<uid>" and "voice:<uid>" name the user directly.
// A blocked user, an unknown caller or a user without a key is
// gateway.ErrNoIdentityKey.
func (s *Service) ForwardKey(ctx context.Context, callerID string) (string, error) {
	uid, err := s.userFor(ctx, callerID)
	if err != nil {
		return "", err
	}
	if uid == "" {
		return "", gateway.ErrNoIdentityKey
	}
	s.mu.Lock()
	if c, ok := s.keys[uid]; ok && s.now().Before(c.exp) {
		s.mu.Unlock()
		if c.v == "" {
			return "", gateway.ErrNoIdentityKey
		}
		return c.v, nil
	}
	s.mu.Unlock()

	key, err := s.loadKey(ctx, uid)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	s.keys[uid] = cached[string]{v: key, exp: s.now().Add(cacheTTL)}
	s.mu.Unlock()
	if key == "" {
		return "", gateway.ErrNoIdentityKey
	}
	return key, nil
}

// loadKey returns the raw key, or "" when the user has none or is not
// active. Store errors are returned as such (never cached).
func (s *Service) loadKey(ctx context.Context, userID string) (string, error) {
	var sealed, status string
	err := s.db.QueryRowContext(ctx,
		`SELECT k.key_sealed, u.status FROM user_identity_keys k JOIN users u ON u.id = k.user_id
         WHERE k.user_id = ?`, userID).Scan(&sealed, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if status != identity.StatusActive {
		return "", nil
	}
	raw, err := s.cipher.Open(sealed, []byte(userID))
	if err != nil {
		return "", fmt.Errorf("unseal identity key: %w", err)
	}
	return string(raw), nil
}

// userFor resolves a gateway caller id to a user id ("" when it can't).
func (s *Service) userFor(ctx context.Context, callerID string) (string, error) {
	switch {
	case strings.HasPrefix(callerID, "dashboard:"):
		return strings.TrimPrefix(callerID, "dashboard:"), nil
	case strings.HasPrefix(callerID, "voice:"):
		return strings.TrimPrefix(callerID, "voice:"), nil
	case strings.HasPrefix(callerID, "ag_"):
		return s.agentOwner(ctx, callerID)
	}
	return "", nil
}

func (s *Service) agentOwner(ctx context.Context, agentID string) (string, error) {
	s.mu.Lock()
	if c, ok := s.owners[agentID]; ok && s.now().Before(c.exp) {
		s.mu.Unlock()
		return c.v, nil
	}
	s.mu.Unlock()

	var owner string
	err := s.db.QueryRowContext(ctx, `SELECT owner_user FROM agents WHERE id = ?`, agentID).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		owner, err = "", nil
	}
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	s.owners[agentID] = cached[string]{v: owner, exp: s.now().Add(cacheTTL)}
	s.mu.Unlock()
	return owner, nil
}

// Invalidate drops the cached key for userID ("" drops everything) so the
// next ForwardKey reads the store. Call it after a block or unblock too.
func (s *Service) Invalidate(userID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if userID == "" {
		s.keys = map[string]cached[string]{}
		s.owners = map[string]cached[string]{}
		return
	}
	delete(s.keys, userID)
}
