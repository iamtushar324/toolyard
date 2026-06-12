package events

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Filter narrows an event Query. Zero-value fields are ignored.
type Filter struct {
	SourceID    string
	SourceName  string
	Type        string
	Since       int64  // received_at >= Since
	Before      int64  // received_at < Before (pagination cursor)
	Q           string // substring match on summary/title/type
	UnackedOnly bool
	Limit       int
}

// Query returns events matching the filter, newest first.
func (s *Service) Query(ctx context.Context, f Filter) ([]Event, error) {
	q := `SELECT e.id, e.source_id, s.name, e.type, COALESCE(e.title,''), e.summary,
                 COALESCE(e.payload,''), COALESCE(e.dedup_key,''), e.created_at, e.received_at,
                 COALESCE(e.acked_at,0), COALESCE(e.acked_by,'')
          FROM events e JOIN event_sources s ON s.id = e.source_id WHERE 1=1`
	var args []any
	if f.SourceID != "" {
		q += ` AND e.source_id = ?`
		args = append(args, f.SourceID)
	}
	if f.SourceName != "" {
		q += ` AND s.name = ?`
		args = append(args, f.SourceName)
	}
	if f.Type != "" {
		q += ` AND e.type = ?`
		args = append(args, f.Type)
	}
	if f.Since > 0 {
		q += ` AND e.received_at >= ?`
		args = append(args, f.Since)
	}
	if f.Before > 0 {
		q += ` AND e.received_at < ?`
		args = append(args, f.Before)
	}
	if f.UnackedOnly {
		q += ` AND e.acked_at IS NULL`
	}
	if f.Q != "" {
		like := "%" + f.Q + "%"
		q += ` AND (e.summary LIKE ? OR e.title LIKE ? OR e.type LIKE ?)`
		args = append(args, like, like, like)
	}
	q += ` ORDER BY e.received_at DESC, e.id DESC`
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	q += ` LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEvents(rows)
}

// Get returns one event by id (with full payload).
func (s *Service) Get(ctx context.Context, id string) (*Event, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT e.id, e.source_id, s.name, e.type, COALESCE(e.title,''), e.summary,
                COALESCE(e.payload,''), COALESCE(e.dedup_key,''), e.created_at, e.received_at,
                COALESCE(e.acked_at,0), COALESCE(e.acked_by,'')
         FROM events e JOIN event_sources s ON s.id = e.source_id WHERE e.id = ?`, id)
	ev, err := scanEvent(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return ev, err
}

// Ack marks events acked (only those still NULL), returning the count flipped.
func (s *Service) Ack(ctx context.Context, ids []string, actor string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	q := `UPDATE events SET acked_at=?, acked_by=? WHERE acked_at IS NULL AND id IN (` + placeholders(len(ids)) + `)`
	args := []any{time.Now().UnixMilli(), nullStr(actor)}
	for _, id := range ids {
		args = append(args, id)
	}
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// UnackedCount returns the number of unacked events.
func (s *Service) UnackedCount(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM events WHERE acked_at IS NULL`).Scan(&n)
	return n, err
}

// Brief renders a markdown digest of unacked events since `since` (zero = all
// unacked), grouped by source then type. It is the common-language entry point
// used by the events.brief MCP tool.
func (s *Service) Brief(ctx context.Context, since int64) (string, error) {
	f := Filter{UnackedOnly: true, Since: since, Limit: 500}
	evs, err := s.Query(ctx, f)
	if err != nil {
		return "", err
	}
	if len(evs) == 0 {
		return "No unacked events. You're all caught up.", nil
	}
	// Group by source, then type.
	type group struct {
		count  int
		recent []Event
	}
	bySource := map[string]map[string]*group{}
	sourceOrder := []string{}
	for _, ev := range evs {
		if _, ok := bySource[ev.SourceName]; !ok {
			bySource[ev.SourceName] = map[string]*group{}
			sourceOrder = append(sourceOrder, ev.SourceName)
		}
		g := bySource[ev.SourceName][ev.Type]
		if g == nil {
			g = &group{}
			bySource[ev.SourceName][ev.Type] = g
		}
		g.count++
		if len(g.recent) < 3 {
			g.recent = append(g.recent, ev)
		}
	}
	sort.Strings(sourceOrder)

	var b strings.Builder
	fmt.Fprintf(&b, "# Events brief — %d unacked\n", len(evs))
	for _, srcName := range sourceOrder {
		fmt.Fprintf(&b, "\n## %s\n", srcName)
		types := bySource[srcName]
		typeNames := make([]string, 0, len(types))
		for t := range types {
			typeNames = append(typeNames, t)
		}
		sort.Strings(typeNames)
		for _, t := range typeNames {
			g := types[t]
			fmt.Fprintf(&b, "- **%s** (%d)\n", t, g.count)
			for _, ev := range g.recent {
				fmt.Fprintf(&b, "  - %s — _%s_\n", ev.Summary, relTime(ev.ReceivedAt))
			}
		}
	}
	b.WriteString("\nAck events you've handled with events.ack so this brief stays focused.")
	return b.String(), nil
}

func relTime(ms int64) string {
	d := time.Since(time.UnixMilli(ms))
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func scanEvents(rows *sql.Rows) ([]Event, error) {
	out := []Event{}
	for rows.Next() {
		ev, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *ev)
	}
	return out, rows.Err()
}

func scanEvent(sc scanner) (*Event, error) {
	var ev Event
	var payload, dedup, ackedBy string
	var ackedAt int64
	if err := sc.Scan(&ev.ID, &ev.SourceID, &ev.SourceName, &ev.Type, &ev.Title, &ev.Summary,
		&payload, &dedup, &ev.CreatedAt, &ev.ReceivedAt, &ackedAt, &ackedBy); err != nil {
		return nil, err
	}
	if payload != "" {
		ev.Payload = json.RawMessage(payload)
	}
	ev.DedupKey = dedup
	ev.AckedAt = ackedAt
	ev.AckedBy = ackedBy
	return &ev, nil
}
