// Package identity manages users (single-user local password) and agent
// enrollment tokens.
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
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/argon2"

	"github.com/tusharbhardwaj/toolyard/internal/store"
)

var (
	ErrNoUser            = errors.New("no user configured")
	ErrUserExists        = errors.New("user already exists")
	ErrInvalidLogin      = errors.New("invalid username or password")
	ErrEnrollNotFound    = errors.New("enrollment code not found or expired")
	ErrAgentTokenInvalid = errors.New("agent token invalid")
)

type Service struct {
	db *store.DB
}

func New(db *store.DB) *Service { return &Service{db: db} }

type User struct {
	ID       string `json:"id"`
	Username string `json:"username"`
}

type Agent struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Owner    string    `json:"owner"`
	LastSeen time.Time `json:"last_seen"`
}

// ---- argon2id password hashing -----------------------------------------------

const (
	argonTime    = 2
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
	if username == "" || len(password) < 8 {
		return nil, errors.New("username required, password must be 8+ chars")
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
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO users(id, username, password_hash, created_at, updated_at) VALUES(?,?,?,?,?)`,
		id, username, hashPassword(password), now, now); err != nil {
		return nil, err
	}
	return &User{ID: id, Username: username}, nil
}

func (s *Service) Authenticate(ctx context.Context, username, password string) (*User, error) {
	var u User
	var hash string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, username, password_hash FROM users WHERE username = ?`, username).
		Scan(&u.ID, &u.Username, &hash)
	if err == sql.ErrNoRows {
		return nil, ErrInvalidLogin
	}
	if err != nil {
		return nil, err
	}
	if !verifyPassword(password, hash) {
		return nil, ErrInvalidLogin
	}
	return &u, nil
}

func (s *Service) GetUserByID(ctx context.Context, id string) (*User, error) {
	var u User
	err := s.db.QueryRowContext(ctx,
		`SELECT id, username FROM users WHERE id = ?`, id).Scan(&u.ID, &u.Username)
	if err == sql.ErrNoRows {
		return nil, ErrNoUser
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func (s *Service) PrimaryUser(ctx context.Context) (*User, error) {
	var u User
	err := s.db.QueryRowContext(ctx,
		`SELECT id, username FROM users ORDER BY created_at LIMIT 1`).Scan(&u.ID, &u.Username)
	if err == sql.ErrNoRows {
		return nil, ErrNoUser
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// ---- agent enrollment --------------------------------------------------------

// CreateEnrollment returns a short-lived code an operator pastes into an agent's
// MCP config. The agent then exchanges it for a long-lived token.
func (s *Service) CreateEnrollment(ctx context.Context, ownerUserID, agentName string, ttl time.Duration) (string, *Agent, error) {
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
	return code, &Agent{ID: id, Name: agentName, Owner: ownerUserID}, nil
}

// ExchangeEnrollment swaps a one-time enrollment code for a long-lived token.
// The token is returned to the caller in plaintext exactly once; only its hash
// is persisted.
func (s *Service) ExchangeEnrollment(ctx context.Context, code string) (string, *Agent, error) {
	var ag Agent
	var exp int64
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, owner_user, enroll_expires
         FROM agents WHERE enroll_code = ?`, code).
		Scan(&ag.ID, &ag.Name, &ag.Owner, &exp)
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
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, owner_user, token_hash FROM agents WHERE id = ?`, id).
		Scan(&ag.ID, &ag.Name, &ag.Owner, &hash)
	if err == sql.ErrNoRows {
		return nil, ErrAgentTokenInvalid
	}
	if err != nil {
		return nil, err
	}
	if hash == "" || subtle.ConstantTimeCompare([]byte(hash), []byte(hashToken(token))) != 1 {
		return nil, ErrAgentTokenInvalid
	}
	_, _ = s.db.ExecContext(ctx, `UPDATE agents SET last_seen = ? WHERE id = ?`,
		time.Now().UnixMilli(), ag.ID)
	return &ag, nil
}

func (s *Service) ListAgents(ctx context.Context, ownerUserID string) ([]Agent, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, owner_user, COALESCE(last_seen, 0)
         FROM agents WHERE owner_user = ? ORDER BY created_at DESC`, ownerUserID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Agent
	for rows.Next() {
		var ag Agent
		var seen int64
		if err := rows.Scan(&ag.ID, &ag.Name, &ag.Owner, &seen); err != nil {
			return nil, err
		}
		if seen > 0 {
			ag.LastSeen = time.UnixMilli(seen)
		}
		out = append(out, ag)
	}
	return out, rows.Err()
}

// ---- helpers ----------------------------------------------------------------

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
