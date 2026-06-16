// Package memwebhook implements TEC-481: authenticated, wing-locked
// memory-ingestion webhooks for n8n automations.
//
// Each webhook is bound to exactly one MemPalace wing at creation time and
// authenticated by its own revocable bearer token (sha256-hash-only storage,
// mirroring internal/events). Callers can never override the bound wing — the
// wing is read from the webhook row keyed by the verified token, never from the
// request. Within the locked wing, each webhook carries its own PayloadSpec so
// different automations can structure their data differently.
//
// The actual write goes through a narrow MemIngester (satisfied by
// *mempalace.Service) so this package is unit-testable without a live MemPalace
// subprocess. Every ingest — success, failure, schema rejection, or
// oversized-payload rejection — is recorded in the memory_webhook_ingestions
// ledger for the dashboard panel and for audit/debugging.
package memwebhook

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
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/mempalace"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

var (
	ErrNotFound         = errors.New("memory webhook not found")
	ErrExists           = errors.New("memory webhook name already exists")
	ErrInvalid          = errors.New("invalid memory webhook")
	ErrBadToken         = errors.New("invalid or disabled webhook token")
	ErrSchemaViolation  = errors.New("payload rejected by webhook schema")
	ErrMemUnavailable   = errors.New("mempalace is not available")
	ErrUpstreamRejected = errors.New("mempalace rejected the entry")
)

// Limits.
const (
	MaxNameLen = 120
	MaxWingLen = 120
	maxDetail  = 500
)

// Ledger statuses.
const (
	statusOK       = "ok"
	statusFailed   = "failed"
	statusRejected = "rejected"
	statusTooLarge = "too_large"
)

// MemIngester is the narrow slice of mempalace.Service this package needs.
// *mempalace.Service satisfies it directly; tests supply a stub.
type MemIngester interface {
	Available() bool
	IngestTagged(ctx context.Context, entry, topic, wing, agentName string) (*mempalace.IngestResult, error)
}

// Webhook is one wing-locked ingestion webhook. The token hash is never carried
// on this struct, so it can never leak through an API response.
type Webhook struct {
	ID          string       `json:"id"`   // "mwh_"+uuid
	Name        string       `json:"name"` // unique
	Wing        string       `json:"wing"` // LOCKED at creation, immutable
	Source      string       `json:"source,omitempty"`
	Spec        *PayloadSpec `json:"payload_spec,omitempty"`
	Notes       string       `json:"notes,omitempty"`
	Enabled     bool         `json:"enabled"`
	LastUsedAt  int64        `json:"last_used_at,omitempty"`
	IngestCount int64        `json:"ingest_count"`
	FailCount   int64        `json:"fail_count"`
	CreatedAt   int64        `json:"created_at"`
	UpdatedAt   int64        `json:"updated_at"`
}

// Service owns the memory_webhooks + memory_webhook_ingestions tables.
type Service struct {
	db    *store.DB
	mem   MemIngester
	audit *audit.Logger
}

// New builds a Service. mem may be a *mempalace.Service that is currently
// disabled — ingestion then returns ErrMemUnavailable while management keeps
// working.
func New(db *store.DB, mem MemIngester, auditLog *audit.Logger) *Service {
	return &Service{db: db, mem: mem, audit: auditLog}
}

// ---- CRUD -------------------------------------------------------------------

// CreateInput is the input to Create.
type CreateInput struct {
	Name   string
	Wing   string
	Source string
	Spec   *PayloadSpec
	Notes  string
}

// Create mints a webhook bound to in.Wing and returns its bearer token in
// plaintext exactly once. The wing is fixed here and can never be changed.
func (s *Service) Create(ctx context.Context, in CreateInput) (*Webhook, string, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" || len(name) > MaxNameLen {
		return nil, "", fmt.Errorf("%w: name required (<=%d chars)", ErrInvalid, MaxNameLen)
	}
	wing := strings.TrimSpace(in.Wing)
	if wing == "" || len(wing) > MaxWingLen {
		return nil, "", fmt.Errorf("%w: wing required (<=%d chars)", ErrInvalid, MaxWingLen)
	}
	if err := validateSpec(in.Spec); err != nil {
		return nil, "", err
	}

	id := "mwh_" + uuid.NewString()
	now := time.Now().UnixMilli()
	plaintext, tokenHash, err := mintToken(id)
	if err != nil {
		return nil, "", err
	}
	specJSON, err := marshalSpec(in.Spec)
	if err != nil {
		return nil, "", err
	}

	_, err = s.db.ExecContext(ctx,
		`INSERT INTO memory_webhooks(id, name, wing, source, token_hash, payload_spec, notes, enabled, ingest_count, fail_count, created_at, updated_at)
		 VALUES(?,?,?,?,?,?,?,1,0,0,?,?)`,
		id, name, wing, nullStr(strings.TrimSpace(in.Source)), tokenHash, specJSON, nullStr(strings.TrimSpace(in.Notes)), now, now)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return nil, "", ErrExists
		}
		return nil, "", err
	}
	s.audit2(ctx, "memory_webhook.create", name, "wing="+wing)
	wh, err := s.Get(ctx, id)
	return wh, plaintext, err
}

// UpdateInput carries optional updates; nil fields are left unchanged. The wing
// is intentionally absent — it is immutable after creation.
type UpdateInput struct {
	Enabled *bool
	Source  *string
	Notes   *string
	Spec    *PayloadSpec // when non-nil, replaces the stored spec
}

// Update applies partial updates. It can never change the bound wing.
func (s *Service) Update(ctx context.Context, id string, in UpdateInput) (*Webhook, error) {
	if _, err := s.Get(ctx, id); err != nil {
		return nil, err
	}
	sets := []string{"updated_at=?"}
	args := []any{time.Now().UnixMilli()}
	if in.Enabled != nil {
		sets = append(sets, "enabled=?")
		args = append(args, boolToInt(*in.Enabled))
	}
	if in.Source != nil {
		sets = append(sets, "source=?")
		args = append(args, nullStr(strings.TrimSpace(*in.Source)))
	}
	if in.Notes != nil {
		sets = append(sets, "notes=?")
		args = append(args, nullStr(strings.TrimSpace(*in.Notes)))
	}
	if in.Spec != nil {
		if err := validateSpec(in.Spec); err != nil {
			return nil, err
		}
		specJSON, err := marshalSpec(in.Spec)
		if err != nil {
			return nil, err
		}
		sets = append(sets, "payload_spec=?")
		args = append(args, specJSON)
	}
	args = append(args, id)
	if _, err := s.db.ExecContext(ctx,
		"UPDATE memory_webhooks SET "+strings.Join(sets, ", ")+" WHERE id=?", args...); err != nil {
		return nil, err
	}
	s.audit2(ctx, "memory_webhook.update", id, "")
	return s.Get(ctx, id)
}

// Delete removes a webhook. Its ingestion ledger rows are intentionally kept.
func (s *Service) Delete(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM memory_webhooks WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	s.audit2(ctx, "memory_webhook.delete", id, "")
	return nil
}

// RotateToken mints a fresh token, invalidating the previous one immediately.
func (s *Service) RotateToken(ctx context.Context, id string) (string, error) {
	if _, err := s.Get(ctx, id); err != nil {
		return "", err
	}
	plaintext, hash, err := mintToken(id)
	if err != nil {
		return "", err
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE memory_webhooks SET token_hash=?, updated_at=? WHERE id=?`,
		hash, time.Now().UnixMilli(), id); err != nil {
		return "", err
	}
	s.audit2(ctx, "memory_webhook.rotate", id, "")
	return plaintext, nil
}

// List returns all webhooks (never their token hashes).
func (s *Service) List(ctx context.Context) ([]Webhook, error) {
	rows, err := s.db.QueryContext(ctx, selectCols+` FROM memory_webhooks ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Webhook{}
	for rows.Next() {
		wh, err := scanWebhook(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *wh)
	}
	return out, rows.Err()
}

// Get returns one webhook by id.
func (s *Service) Get(ctx context.Context, id string) (*Webhook, error) {
	row := s.db.QueryRowContext(ctx, selectCols+` FROM memory_webhooks WHERE id=?`, id)
	wh, err := scanWebhook(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return wh, err
}

// VerifyToken resolves a presented bearer token to its webhook, rejecting
// disabled webhooks and using a constant-time hash compare.
func (s *Service) VerifyToken(ctx context.Context, token string) (*Webhook, error) {
	id, _, ok := splitToken(strings.TrimSpace(token))
	if !ok {
		return nil, ErrBadToken
	}
	var hash string
	var enabled int
	row := s.db.QueryRowContext(ctx,
		`SELECT token_hash, enabled FROM memory_webhooks WHERE id=?`, id)
	if err := row.Scan(&hash, &enabled); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrBadToken
		}
		return nil, err
	}
	if enabled == 0 {
		return nil, ErrBadToken
	}
	want := hashToken(strings.TrimSpace(token))
	if hash == "" || subtle.ConstantTimeCompare([]byte(hash), []byte(want)) != 1 {
		return nil, ErrBadToken
	}
	return s.Get(ctx, id)
}

// ---- ingest -----------------------------------------------------------------

// IngestRequest is one inbound webhook call. RequestID is pre-generated by the
// handler so it is returned even when mapping fails early. Payload is the raw
// (already size-bounded) request body; Source is the optional per-request
// X-Toolyard-Source override.
type IngestRequest struct {
	RequestID   string
	Payload     json.RawMessage
	PayloadSize int64
	Source      string
}

// Result is the ingest outcome envelope returned to the caller.
type Result struct {
	RequestID string `json:"request_id"`
	Wing      string `json:"wing"`
	Bytes     int64  `json:"bytes"`
	Status    string `json:"status"`
	Detail    string `json:"detail,omitempty"`
	OK        bool   `json:"ok"`
}

// Ingest maps the payload per the webhook's spec and writes it into MemPalace
// under the webhook's LOCKED wing. The wing comes only from wh.Wing; the
// request body/headers are never consulted for it. Every outcome is recorded in
// the ledger and audited.
func (s *Service) Ingest(ctx context.Context, wh *Webhook, in IngestRequest) (*Result, error) {
	source := strings.TrimSpace(in.Source)
	if source == "" {
		source = wh.Source
	}
	res := &Result{RequestID: in.RequestID, Wing: wh.Wing, Bytes: in.PayloadSize}

	entry, topic, err := buildEntry(wh.Spec, in.Payload, entryMeta{
		WebhookName: wh.Name,
		RequestID:   in.RequestID,
		Source:      source,
		Bytes:       in.PayloadSize,
	})
	if err != nil {
		res.Status = statusRejected
		res.Detail = err.Error()
		s.recordIngestion(ctx, wh, in.RequestID, source, in.PayloadSize, statusRejected, err.Error(), false)
		s.bumpCounters(ctx, wh.ID, false)
		s.audit2(ctx, "memory_webhook.ingest_failed", wh.Name, "rejected req="+in.RequestID)
		return res, err
	}

	if s.mem == nil || !s.mem.Available() {
		res.Status = statusFailed
		res.Detail = "mempalace unavailable"
		s.recordIngestion(ctx, wh, in.RequestID, source, in.PayloadSize, statusFailed, res.Detail, false)
		s.bumpCounters(ctx, wh.ID, false)
		s.audit2(ctx, "memory_webhook.ingest_failed", wh.Name, "mempalace_unavailable req="+in.RequestID)
		return res, ErrMemUnavailable
	}

	ir, err := s.mem.IngestTagged(ctx, entry, topic, wh.Wing, safeAgentName("webhook", wh.Name))
	if err != nil {
		res.Status = statusFailed
		res.Detail = err.Error()
		s.recordIngestion(ctx, wh, in.RequestID, source, in.PayloadSize, statusFailed, err.Error(), false)
		s.bumpCounters(ctx, wh.ID, false)
		s.audit2(ctx, "memory_webhook.ingest_failed", wh.Name, "req="+in.RequestID)
		if errors.Is(err, mempalace.ErrDisabled) || errors.Is(err, mempalace.ErrNotReady) {
			return res, ErrMemUnavailable
		}
		return res, err
	}

	ok := ir != nil && ir.OK
	if ir != nil {
		res.Detail = ir.Detail
	}
	if !ok {
		res.Status = statusFailed
		s.recordIngestion(ctx, wh, in.RequestID, source, in.PayloadSize, statusFailed, res.Detail, false)
		s.bumpCounters(ctx, wh.ID, false)
		s.audit2(ctx, "memory_webhook.ingest_failed", wh.Name, "upstream_error req="+in.RequestID)
		return res, ErrUpstreamRejected
	}

	res.Status = statusOK
	res.OK = true
	s.recordIngestion(ctx, wh, in.RequestID, source, in.PayloadSize, statusOK, res.Detail, true)
	s.bumpCounters(ctx, wh.ID, true)
	s.audit2(ctx, "memory_webhook.ingest_ok", wh.Name,
		fmt.Sprintf("wing=%s req=%s bytes=%d", wh.Wing, in.RequestID, in.PayloadSize))
	return res, nil
}

// RecordTooLarge logs an oversized-payload rejection against a verified webhook.
// Called by the HTTP handler when the body exceeds the configured limit.
func (s *Service) RecordTooLarge(ctx context.Context, wh *Webhook, requestID, source string, size int64) {
	if strings.TrimSpace(source) == "" {
		source = wh.Source
	}
	s.recordIngestion(ctx, wh, requestID, source, size, statusTooLarge, "payload exceeds configured limit", false)
	s.bumpCounters(ctx, wh.ID, false)
	s.audit2(ctx, "memory_webhook.payload_too_large", wh.Name,
		fmt.Sprintf("req=%s bytes=%d", requestID, size))
}

// AuditAuthFailure records a webhook auth failure (no webhook resolved).
func (s *Service) AuditAuthFailure(ctx context.Context, detail string) {
	s.audit2(ctx, "memory_webhook.auth_failed", "", detail)
}

func (s *Service) recordIngestion(ctx context.Context, wh *Webhook, requestID, source string, size int64, status, detail string, mempalaceOK bool) {
	if s.db == nil {
		return
	}
	_, _ = s.db.ExecContext(ctx,
		`INSERT INTO memory_webhook_ingestions(id, webhook_id, webhook_name, wing, source, received_at, payload_size, status, detail, mempalace_ok)
		 VALUES(?,?,?,?,?,?,?,?,?,?)`,
		requestID, wh.ID, wh.Name, wh.Wing, nullStr(source), time.Now().UnixMilli(), size, status, nullStr(truncate(detail, maxDetail)), boolToInt(mempalaceOK))
}

func (s *Service) bumpCounters(ctx context.Context, id string, ok bool) {
	if s.db == nil {
		return
	}
	now := time.Now().UnixMilli()
	if ok {
		_, _ = s.db.ExecContext(ctx,
			`UPDATE memory_webhooks SET ingest_count=ingest_count+1, last_used_at=?, updated_at=? WHERE id=?`,
			now, now, id)
		return
	}
	_, _ = s.db.ExecContext(ctx,
		`UPDATE memory_webhooks SET ingest_count=ingest_count+1, fail_count=fail_count+1, last_used_at=?, updated_at=? WHERE id=?`,
		now, now, id)
}

func (s *Service) audit2(ctx context.Context, eventType, toolName, summary string) {
	if s.audit == nil {
		return
	}
	_ = s.audit.Write(ctx, audit.Event{
		EventType:     eventType,
		UpstreamName:  mempalace.ToolPrefix,
		ToolName:      toolName,
		ResultSummary: summary,
	})
}

// ---- token helpers (mirrors internal/events) --------------------------------

func safeAgentName(prefix, name string) string {
	var b strings.Builder
	writeSafe := func(s string) {
		lastUnderscore := false
		for _, r := range s {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
				b.WriteRune(r)
				lastUnderscore = false
				continue
			}
			if !lastUnderscore && b.Len() > 0 {
				b.WriteByte('_')
				lastUnderscore = true
			}
		}
	}
	writeSafe(strings.TrimSpace(prefix))
	if b.Len() > 0 {
		b.WriteByte('_')
	}
	writeSafe(strings.TrimSpace(name))
	out := strings.Trim(b.String(), "_")
	if out == "" {
		return "webhook"
	}
	return out
}

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

// ---- scan + small helpers ---------------------------------------------------

const selectCols = `SELECT id, name, wing, COALESCE(source,''), COALESCE(payload_spec,''),
	COALESCE(notes,''), enabled, COALESCE(last_used_at,0), ingest_count, fail_count,
	created_at, updated_at`

type scanner interface {
	Scan(dest ...any) error
}

func scanWebhook(sc scanner) (*Webhook, error) {
	var wh Webhook
	var source, specRaw, notes string
	var enabled int
	if err := sc.Scan(&wh.ID, &wh.Name, &wh.Wing, &source, &specRaw, &notes,
		&enabled, &wh.LastUsedAt, &wh.IngestCount, &wh.FailCount, &wh.CreatedAt, &wh.UpdatedAt); err != nil {
		return nil, err
	}
	wh.Source = source
	wh.Notes = notes
	wh.Enabled = enabled != 0
	if specRaw != "" {
		_ = json.Unmarshal([]byte(specRaw), &wh.Spec)
	}
	return &wh, nil
}

func marshalSpec(spec *PayloadSpec) (any, error) {
	if spec == nil {
		return nil, nil
	}
	b, err := json.Marshal(spec)
	if err != nil {
		return nil, fmt.Errorf("%w: payload_spec not serializable", ErrInvalid)
	}
	return string(b), nil
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

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
