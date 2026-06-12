package events

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// IngestInput is one event to ingest.
type IngestInput struct {
	Type      string          `json:"type"`
	Title     string          `json:"title"`
	Summary   string          `json:"summary"`
	Payload   json.RawMessage `json:"payload"`
	Timestamp int64           `json:"timestamp"` // optional, ms-UTC, producer-claimed
	DedupKey  string          `json:"dedup_key"`
}

// Ingest validates and stores one event for src. Returns (event, deduped,
// err). deduped=true means an event with the same (source, dedup_key) already
// existed — the call is a successful idempotent no-op, so callers return 200.
func (s *Service) Ingest(ctx context.Context, src *Source, in IngestInput) (*Event, bool, error) {
	typ := strings.TrimSpace(in.Type)
	if typ == "" {
		return nil, false, fmt.Errorf("%w: type required", ErrInvalid)
	}
	if len(typ) > MaxTypeLen {
		return nil, false, fmt.Errorf("%w: type too long", ErrInvalid)
	}
	title := in.Title
	if len(title) > MaxTitleLen {
		title = title[:MaxTitleLen]
	}

	// Re-marshal the payload to normalize + enforce the cap.
	var payload string
	if len(in.Payload) > 0 {
		var tmp any
		if err := json.Unmarshal(in.Payload, &tmp); err != nil {
			return nil, false, fmt.Errorf("%w: payload is not valid JSON", ErrInvalid)
		}
		b, _ := json.Marshal(tmp)
		if len(b) > MaxPayloadBytes {
			return nil, false, ErrPayloadTooBig
		}
		payload = string(b)
	}

	// Summary: caller-supplied or template-generated.
	summary := strings.TrimSpace(in.Summary)
	if summary == "" {
		summary = templateSummary(src, typ, title)
	}
	if len(summary) > MaxSummaryLen {
		summary = summary[:MaxSummaryLen]
	}

	received := time.Now().UnixMilli()
	created := clampTimestamp(in.Timestamp, received)

	id := "ev_" + uuid.NewString()
	dedup := strings.TrimSpace(in.DedupKey)

	res, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO events(id, source_id, type, title, summary, payload, dedup_key, created_at, received_at)
         VALUES(?,?,?,?,?,?,?,?,?)`,
		id, src.ID, typ, nullStr(title), summary, nullStr(payload), nullStr(dedup), created, received)
	if err != nil {
		return nil, false, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// Deduped (idempotent retry).
		return nil, true, nil
	}

	_, _ = s.db.ExecContext(ctx,
		`UPDATE event_sources SET last_event_at=?, updated_at=? WHERE id=?`,
		received, received, src.ID)

	ev := &Event{
		ID: id, SourceID: src.ID, SourceName: src.Name, Type: typ, Title: title,
		Summary: summary, DedupKey: dedup, CreatedAt: created, ReceivedAt: received,
	}
	if payload != "" {
		ev.Payload = json.RawMessage(payload)
	}
	s.fanOut(ctx, ev, src)
	return ev, false, nil
}

// templateSummary builds a natural-language summary when the producer didn't
// supply one.
func templateSummary(src *Source, typ, title string) string {
	switch src.Kind {
	case KindWebhook, KindAgent:
		if title != "" {
			return fmt.Sprintf("New %s from %s: %s", typ, src.Name, title)
		}
		return fmt.Sprintf("New %s from %s", typ, src.Name)
	default:
		if title != "" {
			return fmt.Sprintf("%s from %s: %s", typ, src.Name, title)
		}
		return fmt.Sprintf("%s from %s", typ, src.Name)
	}
}

// clampTimestamp bounds a producer-claimed timestamp to [received-7d,
// received+5min]. Zero/absent means "use the gateway clock."
func clampTimestamp(claimed, received int64) int64 {
	if claimed <= 0 {
		return received
	}
	min := received - clampPast.Milliseconds()
	max := received + clampFuture.Milliseconds()
	if claimed < min {
		return min
	}
	if claimed > max {
		return max
	}
	return claimed
}

// ingestInternal is used by the poller/agent paths that already hold a *Source
// and want the same validation/fan-out without an HTTP envelope.
func (s *Service) ingestInternal(ctx context.Context, src *Source, in IngestInput) (*Event, error) {
	ev, _, err := s.Ingest(ctx, src, in)
	if err != nil {
		return nil, err
	}
	return ev, nil
}

// markEventSynced flags an event as lake-synced. Used by the sink.
func (s *Service) markEventSynced(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	q := `UPDATE events SET lake_synced=1 WHERE id IN (` + placeholders(len(ids)) + `)`
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	_, err := s.db.ExecContext(ctx, q, args...)
	return err
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}
