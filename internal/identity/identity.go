// Package identity manages dashboard users (the local password owner plus
// Clerk-linked staff, each with a role and a status), their sessions, and
// agent enrollment tokens.
package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/argon2"

	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// usernameRE constrains usernames to "safe" identifier characters and a
// reasonable length. Prevents 1MB usernames that DoS argon2id and stops
// control-character / shell-metachar shenanigans in display contexts.
var usernameRE = regexp.MustCompile(`^[A-Za-z0-9._-]{2,64}$`)

// agentNameRE allows a slightly broader set since agent names show up in UI
// and audit only, never in shells.
var agentNameRE = regexp.MustCompile(`^[A-Za-z0-9 ._:-]{1,64}$`)

// MinPasswordLen — picked to match the OWASP "memorized secret" baseline.
// Stronger ones welcome; we don't enforce upper cap to allow passphrases.
const MinPasswordLen = 10

var (
	ErrNoUser            = errors.New("no user configured")
	ErrUserExists        = errors.New("user already exists")
	ErrInvalidLogin      = errors.New("invalid username or password")
	ErrUserBlocked       = errors.New("user is blocked")
	ErrLastAdmin         = errors.New("cannot demote or block the last active admin")
	ErrInvalidRole       = errors.New("role must be admin or member")
	ErrInvalidStatus     = errors.New("status must be active or blocked")
	ErrEnrollNotFound    = errors.New("enrollment code not found or expired")
	ErrAgentTokenInvalid = errors.New("agent token invalid")
	// ErrIdentityAgent: the generic agent actions (rotate, disable, enable,
	// delete) refuse a user's identity agent; identitykeys owns its
	// lifecycle so the registry stays in step with the token.
	ErrIdentityAgent = errors.New("identity agent: manage it through the identity key")
)

// Agent kinds, mirroring the CHECK constraint on agents.kind. An identity
// agent is the one per user whose token is that person's identity key.
const (
	AgentKindAgent    = "agent"
	AgentKindIdentity = "identity"
)

// Roles and statuses, mirroring the CHECK constraints on users.
const (
	RoleAdmin     = "admin"
	RoleMember    = "member"
	StatusActive  = "active"
	StatusBlocked = "blocked"
	// Auth values reported on User: how the account signs in.
	AuthClerk    = "clerk"
	AuthPassword = "password"
)

type Service struct {
	db *store.DB
}

func New(db *store.DB) *Service { return &Service{db: db} }

type User struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	Email       string `json:"email,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
	AvatarURL   string `json:"avatar_url,omitempty"`
	// Role: admin sees and manages everything; member manages only their
	// own agents and uses only the tool groups granted to them.
	Role string `json:"role"`
	// Status: blocked users can't sign in, their sessions are revoked and
	// their agents stop authenticating.
	Status        string `json:"status"`
	BlockedReason string `json:"blocked_reason,omitempty"`
	// Auth is "clerk" when the row is linked to a Clerk user, else "password".
	Auth        string `json:"auth"`
	ClerkUserID string `json:"clerk_user_id,omitempty"`
	CreatedAt   int64  `json:"created_at"`
	LastSeenAt  int64  `json:"last_seen_at,omitempty"`
}

// Label is how the person is shown: their display name, else their
// username. Audit rows and approvals record it as the decider's or
// owner's name.
func (u *User) Label() string {
	if u.DisplayName != "" {
		return u.DisplayName
	}
	return u.Username
}

// ClerkProfile is what a verified Clerk sign-in tells us about the person.
type ClerkProfile struct {
	ClerkUserID string
	Email       string
	DisplayName string
	AvatarURL   string
	// OrgRole is the person's role in the Clerk organisation (org:admin,
	// org:member, …). It only matters for a brand-new user on a store with
	// no active admin; see UpsertClerkUser.
	OrgRole string
}

// IsOrgAdminRole reports whether a Clerk organisation role is an admin
// role: "org:admin", or the legacy "admin" (bkt3's rule).
func IsOrgAdminRole(role string) bool {
	r := strings.ToLower(strings.TrimSpace(role))
	return r == "org:admin" || r == "admin"
}

// UserSummary is a User plus the counts the Users admin page shows.
type UserSummary struct {
	User
	AgentCount int `json:"agent_count"`
}

// userColumns is the SELECT list every user read shares (alias u); scanUser
// reads it in the same order.
const userColumns = `u.id, u.username, COALESCE(u.email,''), COALESCE(u.display_name,''), COALESCE(u.avatar_url,''),
	u.role, u.status, COALESCE(u.blocked_reason,''), COALESCE(u.clerk_user_id,''), u.created_at, COALESCE(u.last_seen_at,0)`

type rowScanner interface {
	Scan(dest ...any) error
}

// scanUser reads userColumns (plus any extra trailing columns) from row.
func scanUser(row rowScanner, extra ...any) (*User, error) {
	var u User
	dest := []any{&u.ID, &u.Username, &u.Email, &u.DisplayName, &u.AvatarURL,
		&u.Role, &u.Status, &u.BlockedReason, &u.ClerkUserID, &u.CreatedAt, &u.LastSeenAt}
	dest = append(dest, extra...)
	if err := row.Scan(dest...); err != nil {
		return nil, err
	}
	u.Auth = AuthPassword
	if u.ClerkUserID != "" {
		u.Auth = AuthClerk
	}
	return &u, nil
}

type Agent struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Owner    string    `json:"owner"`
	LastSeen time.Time `json:"last_seen"`
	Disabled bool      `json:"disabled"`
	// Kind is AgentKindAgent for an enrolled agent, AgentKindIdentity for
	// the owner's identity key.
	Kind string `json:"kind"`
	// OwnerEmail, OwnerDisplayName and OwnerUsername describe the owner
	// row. VerifyAgentToken fills them from the same lookup that checks
	// the owner's status, so an ingress can name the person behind a call
	// without a second query. Empty when the owner row is gone.
	OwnerEmail       string `json:"owner_email,omitempty"`
	OwnerDisplayName string `json:"owner_display_name,omitempty"`
	OwnerUsername    string `json:"owner_username,omitempty"`
}

// OwnerName is how the owner is shown: their display name, else their
// username.
func (a *Agent) OwnerName() string {
	if a.OwnerDisplayName != "" {
		return a.OwnerDisplayName
	}
	return a.OwnerUsername
}

// DefaultRotateGrace is how long a rotated-away token keeps authenticating
// so a running agent isn't killed mid-task by a rotation.
const DefaultRotateGrace = 10 * time.Minute

// ---- argon2id password hashing -----------------------------------------------

// Argon2id cost. Bumped from time=2 to time=3 — within OWASP recommendations
// and on a modern desktop costs ~150ms per attempt, slow enough to throttle
// a guesser while remaining unobtrusive on legitimate logins.
const (
	argonTime    = 3
	argonMemory  = 64 * 1024
	argonThreads = 2
	argonKeyLen  = 32
	argonSaltLen = 16
)

func hashPassword(password string) string {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		panic(err)
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("argon2id$%d$%d$%d$%s$%s",
		argonTime, argonMemory, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key))
}

func verifyPassword(password, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "argon2id" {
		return false
	}
	var t, m, p uint32
	if _, err := fmt.Sscanf(parts[1]+" "+parts[2]+" "+parts[3], "%d %d %d", &t, &m, &p); err != nil {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, t, m, uint8(p), uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// ---- user management ---------------------------------------------------------

func (s *Service) HasUser(ctx context.Context) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (s *Service) CreateUser(ctx context.Context, username, password string) (*User, error) {
	if !usernameRE.MatchString(username) {
		return nil, errors.New("username must be 2-64 chars of [A-Za-z0-9._-]")
	}
	if len(password) < MinPasswordLen {
		return nil, fmt.Errorf("password must be at least %d characters", MinPasswordLen)
	}
	if len(password) > 1024 {
		return nil, errors.New("password too long")
	}
	exists, err := s.HasUser(ctx)
	if err != nil {
		return nil, err
	}
	if exists {
		return nil, ErrUserExists
	}
	id := "u_" + uuid.NewString()
	now := time.Now().UnixMilli()
	// The first (and only) password user is the owner: an admin.
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO users(id, username, password_hash, role, status, created_at, updated_at) VALUES(?,?,?,?,?,?,?)`,
		id, username, hashPassword(password), RoleAdmin, StatusActive, now, now); err != nil {
		return nil, err
	}
	return s.GetUserByID(ctx, id)
}

// Authenticate checks a password login. A blocked user with the right
// password gets ErrUserBlocked; a wrong password is always ErrInvalidLogin
// so the status of an account never leaks to a guesser. Clerk-only rows
// (empty password_hash) can never authenticate this way.
func (s *Service) Authenticate(ctx context.Context, username, password string) (*User, error) {
	var hash string
	u, err := scanUser(s.db.QueryRowContext(ctx,
		`SELECT `+userColumns+`, u.password_hash FROM users u WHERE u.username = ?`, username), &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrInvalidLogin
	}
	if err != nil {
		return nil, err
	}
	if hash == "" || !verifyPassword(password, hash) {
		return nil, ErrInvalidLogin
	}
	if u.Status != StatusActive {
		return nil, ErrUserBlocked
	}
	return u, nil
}

func (s *Service) GetUserByID(ctx context.Context, id string) (*User, error) {
	u, err := scanUser(s.db.QueryRowContext(ctx,
		`SELECT `+userColumns+` FROM users u WHERE u.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoUser
	}
	if err != nil {
		return nil, err
	}
	return u, nil
}

// GetUserByEmail returns the user with that email (case-insensitive), or
// ErrNoUser. Emails are not unique in the schema; the oldest row wins.
func (s *Service) GetUserByEmail(ctx context.Context, email string) (*User, error) {
	email = strings.TrimSpace(email)
	if email == "" {
		return nil, ErrNoUser
	}
	u, err := scanUser(s.db.QueryRowContext(ctx,
		`SELECT `+userColumns+` FROM users u WHERE LOWER(u.email) = LOWER(?) ORDER BY u.created_at, u.id LIMIT 1`, email))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoUser
	}
	if err != nil {
		return nil, err
	}
	return u, nil
}

// PrimaryUser is the oldest account: the local owner who set toolyard up.
func (s *Service) PrimaryUser(ctx context.Context) (*User, error) {
	u, err := scanUser(s.db.QueryRowContext(ctx,
		`SELECT `+userColumns+` FROM users u ORDER BY u.created_at, u.id LIMIT 1`))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoUser
	}
	if err != nil {
		return nil, err
	}
	return u, nil
}

// ---- roles, status and Clerk-linked users -----------------------------------

// UpsertClerkUser records a verified Clerk sign-in and returns the local
// user. A returning Clerk id updates the profile and last_seen_at. A new
// Clerk id whose email equals ownerEmail (case-insensitive) attaches to the
// primary user when that row isn't linked yet, so the owner keeps their
// admin role, agents, passkeys and push subscriptions. Anyone else becomes
// a member with a username derived from their email — except that while
// the store has no active admin at all (a fresh install where nobody ran
// the password setup, or every admin blocked), the owner or an admin of
// the Clerk organisation becomes admin, so someone can always reach the
// Users page: /v1/auth/setup closes as soon as any user exists. Blocked
// users are returned as they are; the caller decides what that means.
func (s *Service) UpsertClerkUser(ctx context.Context, p ClerkProfile, ownerEmail string) (*User, error) {
	p.ClerkUserID = strings.TrimSpace(p.ClerkUserID)
	if p.ClerkUserID == "" {
		return nil, errors.New("clerk user id is required")
	}
	p.Email = strings.TrimSpace(p.Email)
	p.DisplayName = strings.TrimSpace(p.DisplayName)
	p.AvatarURL = strings.TrimSpace(p.AvatarURL)
	ownerEmail = strings.TrimSpace(ownerEmail)
	now := time.Now().UnixMilli()

	// One transaction so two concurrent first sign-ins can't both pick the
	// same username or both attach to the owner row. Every statement goes
	// through tx: the store has a single connection and a stray s.db call
	// inside an open transaction would deadlock the process.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var id string
	err = tx.QueryRowContext(ctx, `SELECT id FROM users WHERE clerk_user_id = ?`, p.ClerkUserID).Scan(&id)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	isOwner := ownerEmail != "" && p.Email != "" && strings.EqualFold(p.Email, ownerEmail)
	if id == "" && isOwner {
		var pid string
		var linked sql.NullString
		err := tx.QueryRowContext(ctx,
			`SELECT id, clerk_user_id FROM users ORDER BY created_at, id LIMIT 1`).Scan(&pid, &linked)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if err == nil && (!linked.Valid || linked.String == "") {
			if _, err := tx.ExecContext(ctx, `UPDATE users SET clerk_user_id = ? WHERE id = ?`, p.ClerkUserID, pid); err != nil {
				return nil, err
			}
			id = pid
		}
	}
	if id != "" {
		if _, err := tx.ExecContext(ctx,
			`UPDATE users SET email = ?, display_name = ?, avatar_url = ?, last_seen_at = ?, updated_at = ?
             WHERE id = ?`,
			nullStr(p.Email), nullStr(p.DisplayName), nullStr(p.AvatarURL), now, now, id); err != nil {
			return nil, err
		}
	} else {
		username, err := freeUsername(ctx, tx, usernameFromEmail(p.Email))
		if err != nil {
			return nil, err
		}
		role := RoleMember
		if isOwner || IsOrgAdminRole(p.OrgRole) {
			bootstrap, err := noActiveAdmin(ctx, tx)
			if err != nil {
				return nil, err
			}
			if bootstrap {
				role = RoleAdmin
			}
		}
		id = "u_" + uuid.NewString()
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO users(id, username, password_hash, created_at, updated_at,
                               email, display_name, avatar_url, role, status, clerk_user_id, last_seen_at)
             VALUES(?,?,'',?,?,?,?,?,?,?,?,?)`,
			id, username, now, now,
			nullStr(p.Email), nullStr(p.DisplayName), nullStr(p.AvatarURL),
			role, StatusActive, p.ClerkUserID, now); err != nil {
			return nil, err
		}
	}
	u, err := scanUser(tx.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users u WHERE u.id = ?`, id))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return u, nil
}

// usernameFromEmail turns the local part of an email into a usernameRE-safe
// base, leaving room for a -N de-duplication suffix under the 64 cap.
func usernameFromEmail(email string) string {
	local := email
	if i := strings.IndexByte(local, '@'); i >= 0 {
		local = local[:i]
	}
	var b strings.Builder
	for _, r := range strings.ToLower(local) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), ".-_")
	if len(out) > 56 {
		out = strings.TrimRight(out[:56], ".-_")
	}
	if len(out) < 2 {
		out = "user"
	}
	return out
}

// freeUsername returns base, or base-2, base-3, … — the first not taken.
func freeUsername(ctx context.Context, tx *sql.Tx, base string) (string, error) {
	for i := 1; i < 10000; i++ {
		cand := base
		if i > 1 {
			cand = fmt.Sprintf("%s-%d", base, i)
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM users WHERE username = ?`, cand).Scan(&n); err != nil {
			return "", err
		}
		if n == 0 {
			return cand, nil
		}
	}
	return "", errors.New("no free username")
}

// ListUsers returns every user, oldest first, with their agent count.
func (s *Service) ListUsers(ctx context.Context) ([]UserSummary, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+userColumns+`, (SELECT count(*) FROM agents a WHERE a.owner_user = u.id)
         FROM users u ORDER BY u.created_at, u.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UserSummary{}
	for rows.Next() {
		var n int
		u, err := scanUser(rows, &n)
		if err != nil {
			return nil, err
		}
		out = append(out, UserSummary{User: *u, AgentCount: n})
	}
	return out, rows.Err()
}

// SetRole changes a user's role. Demoting the last active admin is
// ErrLastAdmin: someone must always be able to approve.
func (s *Service) SetRole(ctx context.Context, id, role string) error {
	if role != RoleAdmin && role != RoleMember {
		return ErrInvalidRole
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	curRole, curStatus, err := roleStatus(ctx, tx, id)
	if err != nil {
		return err
	}
	if curRole == RoleAdmin && curStatus == StatusActive && role == RoleMember {
		if err := ensureOtherActiveAdmin(ctx, tx, id); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE users SET role = ?, updated_at = ? WHERE id = ?`,
		role, time.Now().UnixMilli(), id); err != nil {
		return err
	}
	return tx.Commit()
}

// SetStatus blocks or re-activates a user. Blocking records reason, revokes
// every session (the agents stop on their next token check), and refuses
// to take out the last active admin. Re-activating clears the reason but
// leaves revoked sessions revoked.
func (s *Service) SetStatus(ctx context.Context, id, status, reason string) error {
	if status != StatusActive && status != StatusBlocked {
		return ErrInvalidStatus
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	curRole, curStatus, err := roleStatus(ctx, tx, id)
	if err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	if status == StatusBlocked {
		if curRole == RoleAdmin && curStatus == StatusActive {
			if err := ensureOtherActiveAdmin(ctx, tx, id); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE users SET status = ?, blocked_reason = ?, updated_at = ? WHERE id = ?`,
			status, nullStr(strings.TrimSpace(reason)), now, id); err != nil {
			return err
		}
		if err := revokeAllForUser(ctx, tx, id, now); err != nil {
			return err
		}
	} else {
		if _, err := tx.ExecContext(ctx,
			`UPDATE users SET status = ?, blocked_reason = NULL, updated_at = ? WHERE id = ?`,
			status, now, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func roleStatus(ctx context.Context, tx *sql.Tx, id string) (role, status string, err error) {
	err = tx.QueryRowContext(ctx, `SELECT role, status FROM users WHERE id = ?`, id).Scan(&role, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrNoUser
	}
	return role, status, err
}

// noActiveAdmin reports whether the store has no admin who could sign in.
func noActiveAdmin(ctx context.Context, tx *sql.Tx) (bool, error) {
	var n int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM users WHERE role = ? AND status = ?`, RoleAdmin, StatusActive).Scan(&n); err != nil {
		return false, err
	}
	return n == 0, nil
}

// ensureOtherActiveAdmin is the last-admin rule: at least one active admin
// other than exceptID must remain.
func ensureOtherActiveAdmin(ctx context.Context, tx *sql.Tx, exceptID string) error {
	var n int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM users WHERE role = ? AND status = ? AND id != ?`,
		RoleAdmin, StatusActive, exceptID).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return ErrLastAdmin
	}
	return nil
}

// ---- agent enrollment --------------------------------------------------------

// CreateEnrollment returns a short-lived code an operator pastes into an agent's
// MCP config. The agent then exchanges it for a long-lived token.
func (s *Service) CreateEnrollment(ctx context.Context, ownerUserID, agentName string, ttl time.Duration) (string, *Agent, error) {
	if agentName == "" {
		agentName = "agent"
	}
	if !agentNameRE.MatchString(agentName) {
		return "", nil, errors.New("agent name must be 1-64 chars of [A-Za-z0-9 ._:-]")
	}
	code, err := randCode(8)
	if err != nil {
		return "", nil, err
	}
	id := "ag_" + uuid.NewString()
	now := time.Now()
	exp := now.Add(ttl).UnixMilli()
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO agents(id, name, owner_user, token_hash, enroll_code, enroll_expires, created_at)
         VALUES(?,?,?,'',?,?,?)`,
		id, agentName, ownerUserID, code, exp, now.UnixMilli()); err != nil {
		return "", nil, err
	}
	return code, &Agent{ID: id, Name: agentName, Owner: ownerUserID, Kind: AgentKindAgent}, nil
}

// CreateAgentWithToken provisions a new agent and returns its long-lived
// token in one step. Use this from the dashboard (which already
// authenticates the operator), so they don't have to do the
// enrollment-code -> exchange dance just to set up Claude Code locally.
// The plaintext token is returned exactly once; only its sha256 is stored.
func (s *Service) CreateAgentWithToken(ctx context.Context, ownerUserID, agentName string) (string, *Agent, error) {
	if agentName == "" {
		agentName = "agent"
	}
	if !agentNameRE.MatchString(agentName) {
		return "", nil, errors.New("agent name must be 1-64 chars of [A-Za-z0-9 ._:-]")
	}
	id := "ag_" + uuid.NewString()
	now := time.Now()
	rawToken, err := randCode(32)
	if err != nil {
		return "", nil, err
	}
	token := id + "." + rawToken
	hash := hashToken(token)
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO agents(id, name, owner_user, token_hash, last_seen, created_at)
         VALUES(?,?,?,?,?,?)`,
		id, agentName, ownerUserID, hash, now.UnixMilli(), now.UnixMilli()); err != nil {
		return "", nil, err
	}
	return token, &Agent{ID: id, Name: agentName, Owner: ownerUserID, Kind: AgentKindAgent}, nil
}

// ExchangeEnrollment swaps a one-time enrollment code for a long-lived token.
// The token is returned to the caller in plaintext exactly once; only its hash
// is persisted.
func (s *Service) ExchangeEnrollment(ctx context.Context, code string) (string, *Agent, error) {
	var ag Agent
	var exp int64
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, owner_user, enroll_expires, kind
         FROM agents WHERE enroll_code = ?`, code).
		Scan(&ag.ID, &ag.Name, &ag.Owner, &exp, &ag.Kind)
	if err == sql.ErrNoRows {
		return "", nil, ErrEnrollNotFound
	}
	if err != nil {
		return "", nil, err
	}
	if exp < time.Now().UnixMilli() {
		_, _ = s.db.ExecContext(ctx, `UPDATE agents SET enroll_code = NULL WHERE id = ?`, ag.ID)
		return "", nil, ErrEnrollNotFound
	}
	rawToken, err := randCode(32)
	if err != nil {
		return "", nil, err
	}
	token := ag.ID + "." + rawToken
	hash := hashToken(token)
	if _, err := s.db.ExecContext(ctx,
		`UPDATE agents SET token_hash = ?, enroll_code = NULL, enroll_expires = NULL, last_seen = ?
         WHERE id = ?`, hash, time.Now().UnixMilli(), ag.ID); err != nil {
		return "", nil, err
	}
	return token, &ag, nil
}

// VerifyAgentToken returns the agent identified by token, or ErrAgentTokenInvalid.
func (s *Service) VerifyAgentToken(ctx context.Context, token string) (*Agent, error) {
	idx := strings.IndexByte(token, '.')
	if idx <= 0 {
		return nil, ErrAgentTokenInvalid
	}
	id := token[:idx]
	var ag Agent
	var hash string
	var prevHash sql.NullString
	var prevExp sql.NullInt64
	var disabled int
	var ownerStatus string
	// LEFT JOIN: an agent whose owner row is gone keeps working (other
	// packages' tests enrol agents under fake owners); an agent whose owner
	// is blocked stops with the owner. The owner's email and names come
	// back in the same row so the caller can attribute the call to the
	// person without a second lookup.
	err := s.db.QueryRowContext(ctx,
		`SELECT a.id, a.name, a.owner_user, a.kind, a.token_hash, COALESCE(a.disabled,0), a.prev_token_hash, a.prev_token_expires,
                COALESCE(u.status, ?), COALESCE(u.email,''), COALESCE(u.display_name,''), COALESCE(u.username,'')
         FROM agents a LEFT JOIN users u ON u.id = a.owner_user WHERE a.id = ?`, StatusActive, id).
		Scan(&ag.ID, &ag.Name, &ag.Owner, &ag.Kind, &hash, &disabled, &prevHash, &prevExp, &ownerStatus,
			&ag.OwnerEmail, &ag.OwnerDisplayName, &ag.OwnerUsername)
	if err == sql.ErrNoRows {
		return nil, ErrAgentTokenInvalid
	}
	if err != nil {
		return nil, err
	}
	if disabled != 0 || ownerStatus != StatusActive {
		return nil, ErrAgentTokenInvalid
	}
	want := hashToken(token)
	ok := hash != "" && subtle.ConstantTimeCompare([]byte(hash), []byte(want)) == 1
	if !ok && prevHash.Valid && prevHash.String != "" {
		// Accept the previous token within its grace window.
		withinGrace := !prevExp.Valid || prevExp.Int64 == 0 || time.Now().UnixMilli() < prevExp.Int64
		if withinGrace && subtle.ConstantTimeCompare([]byte(prevHash.String), []byte(want)) == 1 {
			ok = true
		}
	}
	if !ok {
		return nil, ErrAgentTokenInvalid
	}
	_, _ = s.db.ExecContext(ctx, `UPDATE agents SET last_seen = ? WHERE id = ?`,
		time.Now().UnixMilli(), ag.ID)
	return &ag, nil
}

// RotateAgentToken issues a fresh token for an existing agent and invalidates
// the old hash. Returned plaintext token must be re-distributed by the
// operator; the old token is dead the instant this returns.
// RotateAgentToken issues a fresh token. When grace > 0 the previous token
// keeps authenticating until now+grace (so a running agent isn't killed
// mid-task); grace <= 0 kills the old token immediately.
func (s *Service) RotateAgentToken(ctx context.Context, ownerUserID, agentID string, grace time.Duration) (string, error) {
	if err := s.requirePlainAgent(ctx, ownerUserID, agentID); err != nil {
		return "", err
	}
	token, hash, err := NewAgentToken(agentID)
	if err != nil {
		return "", err
	}
	now := time.Now().UnixMilli()
	var res sql.Result
	if grace > 0 {
		res, err = s.db.ExecContext(ctx,
			`UPDATE agents SET prev_token_hash = token_hash, prev_token_expires = ?, token_hash = ?, last_seen = ?
             WHERE id = ? AND owner_user = ?`,
			now+grace.Milliseconds(), hash, now, agentID, ownerUserID)
	} else {
		res, err = s.db.ExecContext(ctx,
			`UPDATE agents SET prev_token_hash = NULL, prev_token_expires = NULL, token_hash = ?, last_seen = ?
             WHERE id = ? AND owner_user = ?`,
			hash, now, agentID, ownerUserID)
	}
	if err != nil {
		return "", err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return "", ErrAgentTokenInvalid
	}
	return token, nil
}

// SetAgentDisabled hard-revokes (or re-enables) an agent. A disabled agent's
// token — current and previous — stops authenticating immediately. Owner-scoped.
func (s *Service) SetAgentDisabled(ctx context.Context, ownerUserID, agentID string, disabled bool) error {
	if err := s.requirePlainAgent(ctx, ownerUserID, agentID); err != nil {
		return err
	}
	d := 0
	if disabled {
		d = 1
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE agents SET disabled = ? WHERE id = ? AND owner_user = ?`, d, agentID, ownerUserID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrAgentTokenInvalid
	}
	return nil
}

// DeleteAgent removes the agent row entirely so any outstanding bearer
// token stops authenticating. Owner-scoped — callers can't delete agents
// belonging to other users.
func (s *Service) DeleteAgent(ctx context.Context, ownerUserID, agentID string) error {
	if err := s.requirePlainAgent(ctx, ownerUserID, agentID); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM agents WHERE id = ? AND owner_user = ?`, agentID, ownerUserID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrAgentTokenInvalid
	}
	return nil
}

// requirePlainAgent is the guard in front of the generic agent actions:
// the agent must exist under ownerUserID (else ErrAgentTokenInvalid, the
// same answer as for an unknown id so nothing leaks) and must not be the
// owner's identity agent (ErrIdentityAgent).
func (s *Service) requirePlainAgent(ctx context.Context, ownerUserID, agentID string) error {
	if agentID == "" {
		return ErrAgentTokenInvalid
	}
	var kind string
	err := s.db.QueryRowContext(ctx,
		`SELECT kind FROM agents WHERE id = ? AND owner_user = ?`, agentID, ownerUserID).Scan(&kind)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrAgentTokenInvalid
	}
	if err != nil {
		return err
	}
	if kind == AgentKindIdentity {
		return ErrIdentityAgent
	}
	return nil
}

// CreateSession persists a server-side session anchor so the JWT carrying
// this id can be invalidated (logout, password change, admin revoke) ahead
// of its TTL. Returns the new session id.
func (s *Service) CreateSession(ctx context.Context, userID, userAgent, clientIP string, ttl time.Duration) (string, error) {
	id := "sess_" + uuid.NewString()
	now := time.Now()
	exp := now.Add(ttl)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions(id, user_id, created_at, expires_at, user_agent, client_ip)
         VALUES(?,?,?,?,?,?)`,
		id, userID, now.UnixMilli(), exp.UnixMilli(), nullStr(userAgent), nullStr(clientIP))
	if err != nil {
		return "", err
	}
	return id, nil
}

// IsSessionValid reports whether a session row is non-revoked, unexpired,
// and belongs to a user who isn't blocked.
func (s *Service) IsSessionValid(ctx context.Context, sessionID string) bool {
	if sessionID == "" {
		return false
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT s.expires_at, COALESCE(s.revoked_at,0), COALESCE(u.status, ?)
         FROM sessions s LEFT JOIN users u ON u.id = s.user_id WHERE s.id = ?`, StatusActive, sessionID)
	var exp, revoked int64
	var status string
	if err := row.Scan(&exp, &revoked, &status); err != nil {
		return false
	}
	if revoked > 0 || status != StatusActive {
		return false
	}
	return exp > time.Now().UnixMilli()
}

// RevokeSession marks one session revoked (logout).
func (s *Service) RevokeSession(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET revoked_at = ? WHERE id = ?`,
		time.Now().UnixMilli(), sessionID)
	return err
}

// RevokeAllForUser is the "log out of every device" affordance — used after
// password change, an admin's revoke, or blocking.
func (s *Service) RevokeAllForUser(ctx context.Context, userID string) error {
	return revokeAllForUser(ctx, s.db, userID, time.Now().UnixMilli())
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func revokeAllForUser(ctx context.Context, db execer, userID string, now int64) error {
	_, err := db.ExecContext(ctx,
		`UPDATE sessions SET revoked_at = ?
         WHERE user_id = ? AND revoked_at IS NULL`,
		now, userID)
	return err
}

// PurgeExpiredSessions wipes rows past expiry+grace so the table doesn't
// grow forever. Called from a goroutine.
func (s *Service) PurgeExpiredSessions(ctx context.Context, grace time.Duration) (int64, error) {
	cut := time.Now().Add(-grace).UnixMilli()
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM sessions WHERE expires_at < ?`, cut)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (s *Service) ListAgents(ctx context.Context, ownerUserID string) ([]Agent, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, owner_user, COALESCE(last_seen, 0), COALESCE(disabled, 0), kind
         FROM agents WHERE owner_user = ? ORDER BY created_at DESC`, ownerUserID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Agent
	for rows.Next() {
		var ag Agent
		var seen int64
		var disabled int
		if err := rows.Scan(&ag.ID, &ag.Name, &ag.Owner, &seen, &disabled, &ag.Kind); err != nil {
			return nil, err
		}
		if seen > 0 {
			ag.LastSeen = time.UnixMilli(seen)
		}
		ag.Disabled = disabled != 0
		out = append(out, ag)
	}
	return out, rows.Err()
}

// ---- helpers ----------------------------------------------------------------

// NewAgentToken mints a token for agentID without touching the store:
// "<agent id>.<base64url 32 random bytes>" and the sha256 hex that
// agents.token_hash stores for it. identitykeys uses it to mint a user's
// identity key and register the hash before the token goes live.
func NewAgentToken(agentID string) (token, hash string, err error) {
	if agentID == "" {
		return "", "", ErrAgentTokenInvalid
	}
	raw, err := randCode(32)
	if err != nil {
		return "", "", err
	}
	token = agentID + "." + raw
	return token, hashToken(token), nil
}

// NewAgentID returns a fresh "ag_<uuid>" agent id.
func NewAgentID() string { return "ag_" + uuid.NewString() }

// HashToken is the stored form of an agent token: sha256 hex. For an
// identity key it is the fingerprint registered with Beknown services.
func HashToken(token string) string { return hashToken(token) }

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func randCode(nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
