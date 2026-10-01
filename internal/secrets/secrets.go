// Package secrets is toolyard's credential broker. Operators store API keys
// once, AES-256-GCM-encrypted (the same sealbox envelope as OAuth tokens),
// and reference them as secret://NAME in upstream env / header configs. The
// raw value is resolved only at dial time — agents, .claude.json, and API
// responses never see it.
package secrets

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/sealbox"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// RefPrefix marks a config value as a reference to a stored secret rather
// than a literal. A value is either a whole-value ref ("secret://API_KEY") or
// carries embedded refs ("Bearer ${secret://API_KEY}").
const RefPrefix = "secret://"

// embedOpen starts an embedded reference inside a larger value.
const embedOpen = "${" + RefPrefix

var (
	ErrNotFound = errors.New("secret not found")
	ErrExists   = errors.New("secret already exists")
	ErrInvalid  = errors.New("invalid secret")
	// ErrPending means the secret was requested but the owner hasn't set
	// its value yet.
	ErrPending = errors.New("secret is waiting for its value")

	// nameRe is the permitted secret-name shape: an uppercase env-var-style
	// identifier. Keeps refs unambiguous and shell-safe.
	nameRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
)

// aad binds a ciphertext to its secret name so a copied row can't be decrypted
// under another name. Rename is therefore create+delete, never an UPDATE name.
func aad(name string) []byte {
	return []byte("toolyard.secrets.v1|" + name + "|value")
}

// ValidName reports whether name matches the permitted shape.
func ValidName(name string) bool { return nameRe.MatchString(name) }

// Meta is the value-free view of a secret. It deliberately has no value field
// so it can never be accidentally serialized into an API response.
type Meta struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	CreatedAt   int64  `json:"created_at"`
	UpdatedAt   int64  `json:"updated_at"`
	LastUsedAt  int64  `json:"last_used_at,omitempty"`
	// Pending is true for a requested secret whose value the owner hasn't
	// set yet; RequestedBy names who asked for it.
	Pending     bool   `json:"pending,omitempty"`
	RequestedBy string `json:"requested_by,omitempty"`
}

// Service owns the secrets table.
type Service struct {
	db     *store.DB
	cipher *sealbox.Cipher
	audit  *audit.Logger

	// useLog rate-limits secret.use audit events to one per name per minute
	// so a hot upstream re-dial loop doesn't flood the audit trail.
	mu     sync.Mutex
	useLog map[string]time.Time
}

// New builds a Service. audit may be nil (events are then skipped).
func New(db *store.DB, cipher *sealbox.Cipher, auditLog *audit.Logger) *Service {
	return &Service{db: db, cipher: cipher, audit: auditLog, useLog: map[string]time.Time{}}
}

// Create stores a new secret. Fails if the name already exists.
func (s *Service) Create(ctx context.Context, name, value, description string) (*Meta, error) {
	name = strings.TrimSpace(name)
	if !ValidName(name) {
		return nil, fmt.Errorf("%w: name must match ^[A-Z][A-Z0-9_]{0,63}$", ErrInvalid)
	}
	if value == "" {
		return nil, fmt.Errorf("%w: value required", ErrInvalid)
	}
	enc, err := s.cipher.Seal([]byte(value), aad(name))
	if err != nil {
		return nil, err
	}
	now := time.Now().UnixMilli()
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO secrets(name, value_enc, description, created_at, updated_at)
         VALUES(?,?,?,?,?)`,
		name, enc, nullStr(description), now, now)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return nil, ErrExists
		}
		return nil, err
	}
	s.writeAudit(ctx, "secret.create", name)
	return &Meta{Name: name, Description: description, CreatedAt: now, UpdatedAt: now}, nil
}

// Request creates a named placeholder with no value. Upstreams may reference
// it straight away; it resolves (and they can connect) only after the owner
// sets the value with Update. requestedBy is recorded for the dashboard.
func (s *Service) Request(ctx context.Context, name, description, requestedBy string) (*Meta, error) {
	name = strings.TrimSpace(name)
	if !ValidName(name) {
		return nil, fmt.Errorf("%w: name must match ^[A-Z][A-Z0-9_]{0,63}$", ErrInvalid)
	}
	now := time.Now().UnixMilli()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO secrets(name, value_enc, description, created_at, updated_at, pending, requested_by)
         VALUES(?,'',?,?,?,1,?)`,
		name, nullStr(description), now, now, nullStr(requestedBy))
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return nil, ErrExists
		}
		return nil, err
	}
	s.writeAudit(ctx, "secret.request", name)
	return &Meta{Name: name, Description: description, CreatedAt: now, UpdatedAt: now, Pending: true, RequestedBy: requestedBy}, nil
}

// Update rotates an existing secret's value (and optionally its description).
// Pass description == nil to leave the description untouched.
func (s *Service) Update(ctx context.Context, name, value string, description *string) (*Meta, error) {
	name = strings.TrimSpace(name)
	if !ValidName(name) {
		return nil, fmt.Errorf("%w: bad name", ErrInvalid)
	}
	if value == "" {
		return nil, fmt.Errorf("%w: value required", ErrInvalid)
	}
	enc, err := s.cipher.Seal([]byte(value), aad(name))
	if err != nil {
		return nil, err
	}
	now := time.Now().UnixMilli()
	var res sql.Result
	if description != nil {
		res, err = s.db.ExecContext(ctx,
			`UPDATE secrets SET value_enc=?, description=?, updated_at=?, pending=0 WHERE name=?`,
			enc, nullStr(*description), now, name)
	} else {
		res, err = s.db.ExecContext(ctx,
			`UPDATE secrets SET value_enc=?, updated_at=?, pending=0 WHERE name=?`,
			enc, now, name)
	}
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrNotFound
	}
	s.writeAudit(ctx, "secret.update", name)
	return s.Get(ctx, name)
}

// Delete removes a secret. Referencing upstreams are NOT checked here — the
// API layer enforces the 409-unless-force policy because it owns the
// upstreams view.
func (s *Service) Delete(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM secrets WHERE name=?`, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	s.writeAudit(ctx, "secret.delete", name)
	return nil
}

// Get returns the value-free metadata for one secret.
func (s *Service) Get(ctx context.Context, name string) (*Meta, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT name, COALESCE(description,''), created_at, updated_at, COALESCE(last_used_at,0),
                pending, COALESCE(requested_by,'')
         FROM secrets WHERE name=?`, name)
	var m Meta
	if err := row.Scan(&m.Name, &m.Description, &m.CreatedAt, &m.UpdatedAt, &m.LastUsedAt, &m.Pending, &m.RequestedBy); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &m, nil
}

// List returns every secret's metadata (never values), name-sorted.
func (s *Service) List(ctx context.Context) ([]Meta, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, COALESCE(description,''), created_at, updated_at, COALESCE(last_used_at,0),
                pending, COALESCE(requested_by,'')
         FROM secrets ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Meta{}
	for rows.Next() {
		var m Meta
		if err := rows.Scan(&m.Name, &m.Description, &m.CreatedAt, &m.UpdatedAt, &m.LastUsedAt, &m.Pending, &m.RequestedBy); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Exists reports whether a secret with this name is stored. Used by the
// upstreams validator so a secret:// typo fails at save, not at dial.
func (s *Service) Exists(ctx context.Context, name string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM secrets WHERE name=?`, name).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// Resolve decrypts and returns the raw value for name, bumping last_used_at
// and (rate-limited) writing a secret.use audit event. This is the only path
// that ever returns plaintext.
func (s *Service) Resolve(ctx context.Context, name string) (string, error) {
	row := s.db.QueryRowContext(ctx, `SELECT value_enc, pending FROM secrets WHERE name=?`, name)
	var enc string
	var pending bool
	if err := row.Scan(&enc, &pending); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", err
	}
	if pending {
		return "", fmt.Errorf("%w: ask the owner to set %s in Toolyard (Settings → Secrets)", ErrPending, name)
	}
	plain, err := s.cipher.Open(enc, aad(name))
	if err != nil {
		return "", fmt.Errorf("secret %q: decrypt failed: %w", name, err)
	}
	_, _ = s.db.ExecContext(ctx, `UPDATE secrets SET last_used_at=? WHERE name=?`,
		time.Now().UnixMilli(), name)
	s.maybeAuditUse(ctx, name)
	return string(plain), nil
}

// ParseRef returns (name, true) when value is a whole-value secret reference
// ("secret://API_KEY"). Embedded refs are handled by Refs and Expand.
func ParseRef(value string) (string, bool) {
	if !strings.HasPrefix(value, RefPrefix) {
		return "", false
	}
	name := strings.TrimPrefix(value, RefPrefix)
	if !ValidName(name) {
		return "", false
	}
	return name, true
}

// IsRef reports whether value refers to a secret in any shape, even a
// malformed one: whole-value or embedded. Masking uses it so a value that
// names a secret is shown as written (the ref isn't sensitive) and is
// resolved at dial time rather than sent literally.
func IsRef(value string) bool {
	return strings.HasPrefix(value, RefPrefix) || strings.Contains(value, embedOpen)
}

// Refs returns every secret name value refers to. ok is false when a ref is
// malformed (bad name, or an unclosed "${secret://").
func Refs(value string) (names []string, ok bool) {
	if strings.HasPrefix(value, RefPrefix) {
		name, ok := ParseRef(value)
		if !ok {
			return nil, false
		}
		return []string{name}, true
	}
	rest := value
	for {
		i := strings.Index(rest, embedOpen)
		if i < 0 {
			return names, true
		}
		rest = rest[i+len(embedOpen):]
		j := strings.IndexByte(rest, '}')
		if j < 0 || !ValidName(rest[:j]) {
			return nil, false
		}
		names = append(names, rest[:j])
		rest = rest[j+1:]
	}
}

// Expand returns value with every secret reference replaced by the secret's
// plaintext. Values without refs pass through unchanged. Errors name the
// reference, never a value.
func (s *Service) Expand(ctx context.Context, value string) (string, error) {
	if name, ok := ParseRef(value); ok {
		return s.Resolve(ctx, name)
	}
	if !strings.Contains(value, embedOpen) {
		return value, nil
	}
	var b strings.Builder
	rest := value
	for {
		i := strings.Index(rest, embedOpen)
		if i < 0 {
			b.WriteString(rest)
			return b.String(), nil
		}
		b.WriteString(rest[:i])
		rest = rest[i+len(embedOpen):]
		j := strings.IndexByte(rest, '}')
		if j < 0 || !ValidName(rest[:j]) {
			return "", fmt.Errorf("%w: malformed secret reference", ErrInvalid)
		}
		plain, err := s.Resolve(ctx, rest[:j])
		if err != nil {
			return "", err
		}
		b.WriteString(plain)
		rest = rest[j+1:]
	}
}

// ResolveMap returns a copy of in where every secret reference is replaced
// by its decrypted value. Non-ref values pass through verbatim. Errors name
// the offending key but never echo any value.
func (s *Service) ResolveMap(ctx context.Context, in map[string]string) (map[string]string, error) {
	if len(in) == 0 {
		return map[string]string{}, nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		if !IsRef(v) {
			out[k] = v
			continue
		}
		resolved, err := s.Expand(ctx, v)
		if err != nil {
			names, _ := Refs(v)
			refs := make([]string, len(names))
			for i, n := range names {
				refs[i] = RefPrefix + n
			}
			return nil, fmt.Errorf("resolving %s for key %q: %w", strings.Join(refs, ", "), k, err)
		}
		out[k] = resolved
	}
	return out, nil
}

func (s *Service) writeAudit(ctx context.Context, eventType, name string) {
	if s.audit == nil {
		return
	}
	_ = s.audit.Write(ctx, audit.Event{EventType: eventType, ResultSummary: name})
}

// maybeAuditUse writes a secret.use event at most once per name per minute.
func (s *Service) maybeAuditUse(ctx context.Context, name string) {
	if s.audit == nil {
		return
	}
	s.mu.Lock()
	last := s.useLog[name]
	now := time.Now()
	if now.Sub(last) < time.Minute {
		s.mu.Unlock()
		return
	}
	s.useLog[name] = now
	s.mu.Unlock()
	_ = s.audit.Write(ctx, audit.Event{EventType: "secret.use", ResultSummary: name})
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
