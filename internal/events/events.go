// Package events is toolyard's Events Hub: a common natural-language activity
// layer across heterogeneous agents and external systems.
//
//   - Webhook sources push events in via per-source bearer tokens.
//   - Poller sources watch a URL (whole-body hash or a JSON field) and emit
//     an event when it changes.
//   - Agent sources let any MCP client publish NL events to the shared feed.
//
// Every event carries a `summary` — a natural-language sentence — so the
// events.brief MCP tool can give any agent the same readable digest of "what
// happened." Events land in the SQLite hot store, sink async into the
// ClickHouse lake, and fan out to push/SSE.
package events

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/tusharbhardwaj/toolyard/internal/store"
)

var (
	ErrNotFound      = errors.New("event source not found")
	ErrExists        = errors.New("event source name already exists")
	ErrInvalid       = errors.New("invalid event source")
	ErrBadToken      = errors.New("invalid or disabled source token")
	ErrPayloadTooBig = errors.New("event payload too large")
)

// Source kinds.
const (
	KindWebhook = "webhook"
	KindPoller  = "poller"
	KindAgent   = "agent"
)

// Poller modes.
const (
	ModeHash      = "hash"
	ModeJSONField = "json_field"
)

// Limits.
const (
	MaxPayloadBytes = 64 << 10 // 64 KiB
	MaxTitleLen     = 300
	MaxTypeLen      = 64
	MaxSummaryLen   = 1000
	MinPollInterval = 60 // seconds
	clampPast       = 7 * 24 * time.Hour
	clampFuture     = 5 * time.Minute
)

// PollerConfig is the persisted poller spec.
type PollerConfig struct {
	URL         string            `json:"url"`
	IntervalSec int               `json:"interval_sec"`
	Mode        string            `json:"mode"` // hash | json_field
	JSONPath    string            `json:"json_path,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
}

// PollerState is the persisted runtime state.
type PollerState struct {
	LastHash            string `json:"last_hash,omitempty"`
	LastValue           string `json:"last_value,omitempty"`
	LastPolledAt        int64  `json:"last_polled_at,omitempty"`
	ConsecutiveFailures int    `json:"consecutive_failures,omitempty"`
}

// Source is one event source (never carrying its token hash to callers).
type Source struct {
	ID          string        `json:"id"`
	Name        string        `json:"name"`
	Kind        string        `json:"kind"`
	PollerCfg   *PollerConfig `json:"poller_config,omitempty"`
	PollerState *PollerState  `json:"poller_state,omitempty"`
	LastError   string        `json:"last_error,omitempty"`
	LastEventAt int64         `json:"last_event_at,omitempty"`
	Notify      bool          `json:"notify"`
	NotifyTypes []string      `json:"notify_types,omitempty"`
	Enabled     bool          `json:"enabled"`
	CreatedAt   int64         `json:"created_at"`
	UpdatedAt   int64         `json:"updated_at"`
}

// Event is one stored event.
type Event struct {
	ID         string          `json:"id"`
	SourceID   string          `json:"source_id"`
	SourceName string          `json:"source_name,omitempty"`
	Type       string          `json:"type"`
	Title      string          `json:"title,omitempty"`
	Summary    string          `json:"summary"`
	Payload    json.RawMessage `json:"payload,omitempty"`
	DedupKey   string          `json:"dedup_key,omitempty"`
	CreatedAt  int64           `json:"created_at"`
	ReceivedAt int64           `json:"received_at"`
	AckedAt    int64           `json:"acked_at,omitempty"`
	AckedBy    string          `json:"acked_by,omitempty"`
}

// NotifierFunc is a fan-out target invoked once per ingested (non-deduped)
// event. Mirrors approval.Bus's AddNotifierFunc.
type NotifierFunc func(ctx context.Context, ev *Event, src *Source)

// Service owns the event_sources + events tables.
type Service struct {
	db *store.DB

	mu        sync.Mutex
	notifiers []NotifierFunc
}

// New builds a Service.
func New(db *store.DB) *Service { return &Service{db: db} }

// AddNotifierFunc registers a fan-out target. Called with no lock held.
func (s *Service) AddNotifierFunc(f NotifierFunc) {
	s.mu.Lock()
	s.notifiers = append(s.notifiers, f)
	s.mu.Unlock()
}

func (s *Service) fanOut(ctx context.Context, ev *Event, src *Source) {
	s.mu.Lock()
	fns := append([]NotifierFunc(nil), s.notifiers...)
	s.mu.Unlock()
	for _, f := range fns {
		f(ctx, ev, src)
	}
}

// ---- source CRUD ------------------------------------------------------------

// CreateSourceInput is the input to CreateSource.
type CreateSourceInput struct {
	Name        string
	Kind        string
	PollerCfg   *PollerConfig
	Notify      bool
	NotifyTypes []string
}

// CreateSource creates a source. For webhook sources it mints a bearer token
// (identity-style) returned in plaintext exactly once. For poller sources it
// validates and stores the poller config.
func (s *Service) CreateSource(ctx context.Context, in CreateSourceInput) (*Source, string, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, "", fmt.Errorf("%w: name required", ErrInvalid)
	}
	if in.Kind != KindWebhook && in.Kind != KindPoller && in.Kind != KindAgent {
		return nil, "", fmt.Errorf("%w: kind must be webhook|poller|agent", ErrInvalid)
	}
	id := "evs_" + uuid.NewString()
	now := time.Now().UnixMilli()

	var tokenHash, plaintext string
	if in.Kind == KindWebhook {
		var err error
		plaintext, tokenHash, err = mintToken(id)
		if err != nil {
			return nil, "", err
		}
	}
	var cfgJSON any
	if in.Kind == KindPoller {
		if err := validatePollerConfig(in.PollerCfg); err != nil {
			return nil, "", err
		}
		b, _ := json.Marshal(in.PollerCfg)
		cfgJSON = string(b)
	}
	notifyTypesJSON, _ := json.Marshal(in.NotifyTypes)

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO event_sources(id, name, kind, token_hash, poller_config, notify, notify_types, enabled, created_at, updated_at)
         VALUES(?,?,?,?,?,?,?,1,?,?)`,
		id, name, in.Kind, nullStr(tokenHash), cfgJSON, boolToInt(in.Notify), string(notifyTypesJSON), now, now)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return nil, "", ErrExists
		}
		return nil, "", err
	}
	src, err := s.GetSource(ctx, id)
	return src, plaintext, err
}

// EnsureAgentSource returns the agent-kind source named after the agent,
// creating it on first use. Used by events.publish.
func (s *Service) EnsureAgentSource(ctx context.Context, agentName string) (*Source, error) {
	agentName = strings.TrimSpace(agentName)
	if agentName == "" {
		agentName = "agent"
	}
	name := "agent:" + agentName
	if src, err := s.getSourceByName(ctx, name); err == nil {
		return src, nil
	}
	src, _, err := s.CreateSource(ctx, CreateSourceInput{Name: name, Kind: KindAgent})
	if errors.Is(err, ErrExists) {
		return s.getSourceByName(ctx, name)
	}
	return src, err
}

// RotateSourceToken mints a fresh token for a webhook source.
func (s *Service) RotateSourceToken(ctx context.Context, id string) (string, error) {
	src, err := s.GetSource(ctx, id)
	if err != nil {
		return "", err
	}
	if src.Kind != KindWebhook {
		return "", fmt.Errorf("%w: only webhook sources have tokens", ErrInvalid)
	}
	plaintext, hash, err := mintToken(id)
	if err != nil {
		return "", err
	}
	_, err = s.db.ExecContext(ctx,
		`UPDATE event_sources SET token_hash=?, updated_at=? WHERE id=?`,
		hash, time.Now().UnixMilli(), id)
	if err != nil {
		return "", err
	}
	return plaintext, nil
}

// UpdateSourceInput carries optional updates; nil fields are left unchanged.
type UpdateSourceInput struct {
	Enabled     *bool
	Notify      *bool
	NotifyTypes *[]string
	PollerCfg   *PollerConfig
}

// UpdateSource applies partial updates. Changing a poller's URL or mode resets
// the poller state so the new target re-seeds silently.
func (s *Service) UpdateSource(ctx context.Context, id string, in UpdateSourceInput) (*Source, error) {
	src, err := s.GetSource(ctx, id)
	if err != nil {
		return nil, err
	}
	sets := []string{"updated_at=?"}
	args := []any{time.Now().UnixMilli()}
	if in.Enabled != nil {
		sets = append(sets, "enabled=?")
		args = append(args, boolToInt(*in.Enabled))
	}
	if in.Notify != nil {
		sets = append(sets, "notify=?")
		args = append(args, boolToInt(*in.Notify))
	}
	if in.NotifyTypes != nil {
		b, _ := json.Marshal(*in.NotifyTypes)
		sets = append(sets, "notify_types=?")
		args = append(args, string(b))
	}
	if in.PollerCfg != nil {
		if src.Kind != KindPoller {
			return nil, fmt.Errorf("%w: not a poller source", ErrInvalid)
		}
		if err := validatePollerConfig(in.PollerCfg); err != nil {
			return nil, err
		}
		b, _ := json.Marshal(in.PollerCfg)
		sets = append(sets, "poller_config=?")
		args = append(args, string(b))
		// Reset poller state on URL/mode change so the new target re-seeds.
		if src.PollerCfg == nil || src.PollerCfg.URL != in.PollerCfg.URL || src.PollerCfg.Mode != in.PollerCfg.Mode {
			sets = append(sets, "poller_state=?", "last_error=?")
			args = append(args, nil, nil)
		}
	}
	args = append(args, id)
	_, err = s.db.ExecContext(ctx,
		"UPDATE event_sources SET "+strings.Join(sets, ", ")+" WHERE id=?", args...)
	if err != nil {
		return nil, err
	}
	return s.GetSource(ctx, id)
}

// DeleteSource removes a source (and cascades to its events).
func (s *Service) DeleteSource(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM event_sources WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// VerifySourceToken returns the source for a presented bearer token, rejecting
// disabled sources. Constant-time compare against the stored hash.
func (s *Service) VerifySourceToken(ctx context.Context, token string) (*Source, error) {
	token = strings.TrimSpace(token)
	id, _, ok := splitToken(token)
	if !ok {
		return nil, ErrBadToken
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(token_hash,''), enabled FROM event_sources WHERE id=?`, id)
	var hash string
	var enabled int
	if err := row.Scan(&hash, &enabled); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrBadToken
		}
		return nil, err
	}
	if enabled == 0 {
		return nil, ErrBadToken
	}
	want := hashToken(token)
	if hash == "" || subtle.ConstantTimeCompare([]byte(hash), []byte(want)) != 1 {
		return nil, ErrBadToken
	}
	return s.GetSource(ctx, id)
}

// ListSources returns all sources (never token hashes).
func (s *Service) ListSources(ctx context.Context) ([]Source, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, kind, COALESCE(poller_config,''), COALESCE(poller_state,''),
                COALESCE(last_error,''), COALESCE(last_event_at,0), notify,
                COALESCE(notify_types,''), enabled, created_at, updated_at
         FROM event_sources ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Source{}
	for rows.Next() {
		src, err := scanSource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *src)
	}
	return out, rows.Err()
}

// GetSource returns one source by id.
func (s *Service) GetSource(ctx context.Context, id string) (*Source, error) {
	return s.querySource(ctx, `WHERE id=?`, id)
}

func (s *Service) getSourceByName(ctx context.Context, name string) (*Source, error) {
	return s.querySource(ctx, `WHERE name=?`, name)
}

func (s *Service) querySource(ctx context.Context, where string, arg any) (*Source, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, name, kind, COALESCE(poller_config,''), COALESCE(poller_state,''),
                COALESCE(last_error,''), COALESCE(last_event_at,0), notify,
                COALESCE(notify_types,''), enabled, created_at, updated_at
         FROM event_sources `+where, arg)
	src, err := scanSource(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return src, err
}

type scanner interface {
	Scan(dest ...any) error
}

func scanSource(sc scanner) (*Source, error) {
	var src Source
	var cfgRaw, stateRaw, notifyTypesRaw string
	var notify, enabled int
	if err := sc.Scan(&src.ID, &src.Name, &src.Kind, &cfgRaw, &stateRaw,
		&src.LastError, &src.LastEventAt, &notify, &notifyTypesRaw, &enabled,
		&src.CreatedAt, &src.UpdatedAt); err != nil {
		return nil, err
	}
	src.Notify = notify != 0
	src.Enabled = enabled != 0
	if cfgRaw != "" {
		_ = json.Unmarshal([]byte(cfgRaw), &src.PollerCfg)
	}
	if stateRaw != "" {
		_ = json.Unmarshal([]byte(stateRaw), &src.PollerState)
	}
	if notifyTypesRaw != "" {
		_ = json.Unmarshal([]byte(notifyTypesRaw), &src.NotifyTypes)
	}
	return &src, nil
}

// ---- validation + token helpers --------------------------------------------

func validatePollerConfig(cfg *PollerConfig) error {
	if cfg == nil {
		return fmt.Errorf("%w: poller config required", ErrInvalid)
	}
	u, err := url.Parse(strings.TrimSpace(cfg.URL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%w: poller url must be http(s)", ErrInvalid)
	}
	if cfg.IntervalSec < MinPollInterval {
		cfg.IntervalSec = MinPollInterval
	}
	switch cfg.Mode {
	case ModeHash:
	case ModeJSONField:
		if strings.TrimSpace(cfg.JSONPath) == "" {
			return fmt.Errorf("%w: json_field mode requires json_path", ErrInvalid)
		}
	default:
		return fmt.Errorf("%w: mode must be hash|json_field", ErrInvalid)
	}
	return nil
}

// mintToken builds an identity-style token "<id>.<random>" and its sha256 hex.
func mintToken(id string) (plaintext, hash string, err error) {
	code, err := randCode(32)
	if err != nil {
		return "", "", err
	}
	plaintext = id + "." + code
	return plaintext, hashToken(plaintext), nil
}

func splitToken(token string) (id, code string, ok bool) {
	i := strings.IndexByte(token, '.')
	if i <= 0 || i == len(token)-1 {
		return "", "", false
	}
	return token[:i], token[i+1:], true
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func randCode(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
