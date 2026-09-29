// Package hooks stores agent lifecycle hook events and optionally forwards
// high-signal interaction text into MemPalace.
package hooks

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/mempalace"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

const (
	MaxBodyBytes     = 1 << 20   // 1 MiB
	MaxTextBytes     = 128 << 10 // 128 KiB
	MaxPayloadBytes  = 512 << 10 // 512 KiB
	MaxMetadataBytes = 64 << 10  // 64 KiB
)

var (
	ErrAgentRequired = errors.New("agent_id required")
	ErrInvalidJSON   = errors.New("invalid hook JSON")
	ErrBodyTooLarge  = errors.New("hook payload too large")
)

// MemPalaceIngester is the slice of mempalace.Service used by hook ingest.
type MemPalaceIngester interface {
	Ingest(ctx context.Context, agentID, entry, topic, wing string) (*mempalace.IngestResult, error)
}

type Service struct {
	db        *store.DB
	mempalace MemPalaceIngester
}

func New(db *store.DB, mp MemPalaceIngester) *Service {
	return &Service{db: db, mempalace: mp}
}

type Event struct {
	ID             string          `json:"id"`
	TS             int64           `json:"ts"`
	CreatedAt      int64           `json:"created_at"`
	AgentID        string          `json:"agent_id"`
	Source         string          `json:"source"`
	EventName      string          `json:"event_name"`
	SessionID      string          `json:"session_id,omitempty"`
	TurnID         string          `json:"turn_id,omitempty"`
	ConversationID string          `json:"conversation_id,omitempty"`
	ToolName       string          `json:"tool_name,omitempty"`
	CWD            string          `json:"cwd,omitempty"`
	Text           string          `json:"text,omitempty"`
	Payload        json.RawMessage `json:"payload,omitempty"`
	Metadata       json.RawMessage `json:"metadata,omitempty"`
	MemoryIngested bool            `json:"memory_ingested"`
}

type Query struct {
	AgentID   string
	Source    string
	EventName string
	SessionID string
	Q         string
	Since     int64
	Until     int64
	Before    int64
	Limit     int
}

// IngestOptions tunes one ingest.
type IngestOptions struct {
	// SkipMemory records the event but never forwards it to MemPalace. The
	// API sets it for agents whose user may not use the mempalace tool
	// group: the hook trail is still kept, the memory write is not made.
	SkipMemory bool
}

// IngestRaw accepts either Toolyard's normalized hook shape or raw client hook
// JSON. For raw client JSON, the full body is stored as payload and the common
// fields are best-effort extracted from known client keys.
func (s *Service) IngestRaw(ctx context.Context, agentID string, body []byte, sourceHint string) (*Event, error) {
	return s.IngestRawOpts(ctx, agentID, body, sourceHint, IngestOptions{})
}

// IngestRawOpts is IngestRaw with options.
func (s *Service) IngestRawOpts(ctx context.Context, agentID string, body []byte, sourceHint string, opts IngestOptions) (*Event, error) {
	if strings.TrimSpace(agentID) == "" {
		return nil, ErrAgentRequired
	}
	if len(body) == 0 {
		return nil, ErrInvalidJSON
	}
	if len(body) > MaxBodyBytes {
		return nil, ErrBodyTooLarge
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidJSON, err)
	}
	now := time.Now().UnixMilli()
	ev := &Event{
		ID:        "he_" + uuid.NewString(),
		AgentID:   agentID,
		Source:    normalizeSource(firstString(sourceHint, pickString(m, "source"), pickString(m, "client"))),
		EventName: normalizeEvent(firstString(pickString(m, "event_name"), pickString(m, "hook_event_name"), pickString(m, "event"))),
		CreatedAt: now,
		TS:        parseOccurredAt(m, now),
	}
	ev.SessionID = limitString(firstString(pickString(m, "session_id"), pickString(m, "sessionId")), 512)
	ev.TurnID = limitString(firstString(pickString(m, "turn_id"), pickString(m, "turnId")), 512)
	ev.ConversationID = limitString(firstString(pickString(m, "conversation_id"), pickString(m, "conversationId")), 512)
	ev.ToolName = limitString(firstString(pickString(m, "tool_name"), pickString(m, "toolName")), 512)
	ev.CWD = limitString(firstString(pickString(m, "cwd"), pickString(m, "project_dir"), pickString(m, "workspace")), 2048)
	ev.Text = limitString(audit.RedactString(extractText(m)), MaxTextBytes)
	ev.Payload = redactAndLimitJSON(rawPayload(m, body), MaxPayloadBytes)
	ev.Metadata = redactAndLimitJSON(rawField(m, "metadata"), MaxMetadataBytes)

	if err := s.insert(ctx, ev); err != nil {
		return nil, err
	}
	if !opts.SkipMemory {
		ev.MemoryIngested = s.tryMemoryIngest(ctx, ev)
	}
	if ev.MemoryIngested {
		_, _ = s.db.ExecContext(ctx, `UPDATE hook_events SET memory_ingested = 1 WHERE id = ?`, ev.ID)
	}
	return ev, nil
}

func (s *Service) insert(ctx context.Context, ev *Event) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO hook_events(id, ts, created_at, agent_id, source, event_name,
            session_id, turn_id, conversation_id, tool_name, cwd, text, payload,
            metadata, memory_ingested)
         VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		ev.ID, ev.TS, ev.CreatedAt, ev.AgentID, ev.Source, ev.EventName,
		nullStr(ev.SessionID), nullStr(ev.TurnID), nullStr(ev.ConversationID),
		nullStr(ev.ToolName), nullStr(ev.CWD), nullStr(ev.Text),
		nullRaw(ev.Payload), nullRaw(ev.Metadata), boolInt(ev.MemoryIngested),
	)
	return err
}

func (s *Service) List(ctx context.Context, q Query) ([]Event, error) {
	sqlq := `SELECT id, ts, created_at, agent_id, source, event_name,
            COALESCE(session_id,''), COALESCE(turn_id,''), COALESCE(conversation_id,''),
            COALESCE(tool_name,''), COALESCE(cwd,''), COALESCE(text,''),
            COALESCE(payload,''), COALESCE(metadata,''), memory_ingested
         FROM hook_events WHERE 1=1`
	args := []any{}
	if q.AgentID != "" {
		sqlq += ` AND agent_id = ?`
		args = append(args, q.AgentID)
	}
	if q.Source != "" {
		sqlq += ` AND source = ?`
		args = append(args, normalizeSource(q.Source))
	}
	if q.EventName != "" {
		sqlq += ` AND event_name = ?`
		args = append(args, strings.TrimSpace(q.EventName))
	}
	if q.SessionID != "" {
		sqlq += ` AND session_id = ?`
		args = append(args, q.SessionID)
	}
	if q.Since > 0 {
		sqlq += ` AND ts >= ?`
		args = append(args, q.Since)
	}
	if q.Until > 0 {
		sqlq += ` AND ts <= ?`
		args = append(args, q.Until)
	}
	if q.Before > 0 {
		sqlq += ` AND ts < ?`
		args = append(args, q.Before)
	}
	if strings.TrimSpace(q.Q) != "" {
		sqlq += ` AND (text LIKE ? OR tool_name LIKE ? OR event_name LIKE ? OR session_id LIKE ?)`
		like := "%" + strings.TrimSpace(q.Q) + "%"
		args = append(args, like, like, like, like)
	}
	sqlq += ` ORDER BY ts DESC`
	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 10000 {
		limit = 10000
	}
	sqlq += ` LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, sqlq, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		ev, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

type scanner interface {
	Scan(dest ...any) error
}

func scanEvent(row scanner) (Event, error) {
	var ev Event
	var payload, metadata string
	var mem int
	err := row.Scan(&ev.ID, &ev.TS, &ev.CreatedAt, &ev.AgentID, &ev.Source, &ev.EventName,
		&ev.SessionID, &ev.TurnID, &ev.ConversationID, &ev.ToolName, &ev.CWD, &ev.Text,
		&payload, &metadata, &mem)
	if err != nil {
		return ev, err
	}
	if payload != "" {
		ev.Payload = json.RawMessage(payload)
	}
	if metadata != "" {
		ev.Metadata = json.RawMessage(metadata)
	}
	ev.MemoryIngested = mem != 0
	return ev, nil
}

func (s *Service) tryMemoryIngest(ctx context.Context, ev *Event) bool {
	if s.mempalace == nil || !shouldIngestMemory(ev) {
		return false
	}
	entry := memoryEntry(ev)
	if strings.TrimSpace(entry) == "" {
		return false
	}
	res, err := s.mempalace.Ingest(ctx, ev.AgentID, entry, "agent-hooks", "interactions")
	return err == nil && res != nil && res.OK
}

func shouldIngestMemory(ev *Event) bool {
	name := strings.ToLower(ev.EventName)
	if strings.Contains(name, "prompt") || strings.Contains(name, "message") ||
		strings.Contains(name, "stop") || strings.Contains(name, "response") {
		return true
	}
	if strings.Contains(name, "tool") && ev.ToolName != "" {
		return true
	}
	return false
}

func memoryEntry(ev *Event) string {
	parts := []string{
		"toolyard hook event",
		"event_id: " + ev.ID,
		"source: " + ev.Source,
		"event: " + ev.EventName,
	}
	if ev.SessionID != "" {
		parts = append(parts, "session_id: "+ev.SessionID)
	}
	if ev.TurnID != "" {
		parts = append(parts, "turn_id: "+ev.TurnID)
	}
	if ev.ToolName != "" {
		parts = append(parts, "tool: "+ev.ToolName)
	}
	text := strings.TrimSpace(ev.Text)
	if text == "" && strings.Contains(strings.ToLower(ev.EventName), "tool") {
		text = summarizePayload(ev.Payload)
	}
	if text != "" {
		parts = append(parts, "text:\n"+limitString(text, 8<<10))
	}
	return strings.Join(parts, "\n")
}

func summarizePayload(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return limitString(string(raw), 4<<10)
	}
	for _, key := range []string{"tool_response", "tool_result", "result", "output", "stdout", "stderr"} {
		if s := anyToString(m[key]); s != "" {
			return limitString(s, 4<<10)
		}
	}
	return limitString(string(raw), 4<<10)
}

func extractText(m map[string]any) string {
	for _, key := range []string{"text", "prompt", "message", "content", "assistant_message", "response"} {
		if s := anyToString(m[key]); s != "" {
			return s
		}
	}
	for _, key := range []string{"tool_response", "tool_result", "result"} {
		if s := anyToString(m[key]); s != "" {
			return s
		}
	}
	return ""
}

func rawPayload(m map[string]any, original []byte) json.RawMessage {
	if raw := rawField(m, "payload"); len(raw) > 0 {
		return raw
	}
	return json.RawMessage(original)
}

func rawField(m map[string]any, key string) json.RawMessage {
	v, ok := m[key]
	if !ok || v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

func redactAndLimitJSON(in json.RawMessage, max int) json.RawMessage {
	if len(in) == 0 {
		return nil
	}
	out := audit.RedactJSONBytes(in)
	if len(out) <= max {
		return out
	}
	truncated := map[string]any{
		"truncated": true,
		"bytes":     len(out),
		"preview":   limitString(string(out), max/2),
	}
	b, _ := json.Marshal(truncated)
	return b
}

func parseOccurredAt(m map[string]any, fallback int64) int64 {
	v, ok := m["occurred_at"]
	if !ok {
		v = m["timestamp"]
	}
	switch x := v.(type) {
	case float64:
		if x > 0 {
			return int64(x)
		}
	case string:
		if x == "" {
			return fallback
		}
		if t, err := time.Parse(time.RFC3339Nano, x); err == nil {
			return t.UnixMilli()
		}
	}
	return fallback
}

func normalizeSource(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "claude", "claude-code", "claude_code":
		return "claude_code"
	case "codex", "openai_codex":
		return "codex"
	case "cursor":
		return "cursor"
	case "":
		return "generic"
	default:
		return "generic"
	}
}

func normalizeEvent(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "unknown"
	}
	return limitString(s, 256)
}

func pickString(m map[string]any, key string) string {
	return anyToString(m[key])
}

func firstString(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func anyToString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case fmt.Stringer:
		return x.String()
	case map[string]any, []any:
		b, _ := json.Marshal(x)
		return string(b)
	default:
		return ""
	}
}

func limitString(s string, maxBytes int) string {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s
	}
	out := s[:maxBytes]
	for !utf8.ValidString(out) && len(out) > 0 {
		out = out[:len(out)-1]
	}
	return out + "\n[truncated]"
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullRaw(b json.RawMessage) any {
	if len(b) == 0 {
		return nil
	}
	return string(b)
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

// Ensure sql.Rows keeps satisfying the tiny scanner interface.
var _ scanner = (*sql.Rows)(nil)
