// Package audit appends and queries toolyard's audit event log.
package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/tusharbhardwaj/toolyard/internal/actor"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// Event types we record.
const (
	EventCallStart      = "call.start"
	EventCallAllowed    = "call.allowed"
	EventCallDenied     = "call.denied"
	EventCallSucceeded  = "call.succeeded"
	EventCallFailed     = "call.failed"
	EventApprovalCreate = "approval.create"
	EventApprovalDecide = "approval.decide"
	EventApprovalExpire = "approval.expire"
	EventAgentEnroll    = "agent.enroll"
	EventUserLogin      = "user.login"
)

type Event struct {
	ID            string          `json:"id"`
	TS            int64           `json:"ts"`
	AgentID       string          `json:"agent_id,omitempty"`
	UpstreamName  string          `json:"upstream_name,omitempty"`
	ToolName      string          `json:"tool_name,omitempty"`
	EventType     string          `json:"event_type"`
	Decision      string          `json:"decision,omitempty"`
	Reason        string          `json:"reason,omitempty"`
	Arguments     json.RawMessage `json:"arguments,omitempty"`
	ResultSummary string          `json:"result_summary,omitempty"`
	ApprovalID    string          `json:"approval_id,omitempty"`

	// Raiser is who raised the call. Write fills empty fields from the
	// actor.Raiser on ctx, so call sites needn't repeat it.
	actor.Raiser
	// Decider is who approved or denied, and how (empty when no decision
	// is part of this event).
	DecidedByUserID string `json:"decided_by_user_id,omitempty"`
	DecidedByEmail  string `json:"decided_by_email,omitempty"`
	DecidedByName   string `json:"decided_by_name,omitempty"`
	DecidedVia      string `json:"decided_via,omitempty"`
	DeciderRef      string `json:"decider_ref,omitempty"`
}

// SetDecider copies d into the event's decider fields.
func (e *Event) SetDecider(d actor.Decider) {
	e.DecidedByUserID, e.DecidedByEmail, e.DecidedByName = d.UserID, d.Email, d.Name
	e.DecidedVia, e.DeciderRef = d.Via, d.Ref
}

type Logger struct {
	db   *store.DB
	subs subscribers
}

type subscribers struct {
	mu        sync.Mutex
	listeners map[chan Event]struct{}
}

func New(db *store.DB) *Logger {
	return &Logger{db: db, subs: subscribers{listeners: map[chan Event]struct{}{}}}
}

// Subscribe returns a channel that receives all subsequent events. Unsubscribe
// must be called when the consumer is done.
func (l *Logger) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 32)
	l.subs.mu.Lock()
	l.subs.listeners[ch] = struct{}{}
	l.subs.mu.Unlock()
	cancel := func() {
		l.subs.mu.Lock()
		delete(l.subs.listeners, ch)
		l.subs.mu.Unlock()
		close(ch)
	}
	return ch, cancel
}

func (l *Logger) fanOut(e Event) {
	l.subs.mu.Lock()
	defer l.subs.mu.Unlock()
	for ch := range l.subs.listeners {
		select {
		case ch <- e:
		default: // drop on slow consumer
		}
	}
}

// Write appends an event. ID and TS are filled in if zero. Reason and
// arguments are passed through RedactString / RedactJSONBytes first so a
// fat-fingered API key in a tool argument doesn't become a permanent leak
// in the audit log.
func (l *Logger) Write(ctx context.Context, e Event) error {
	if e.ID == "" {
		e.ID = "ev_" + uuid.NewString()
	}
	if e.TS == 0 {
		e.TS = time.Now().UnixMilli()
	}
	e.Reason = RedactString(e.Reason)
	e.ResultSummary = RedactString(e.ResultSummary)
	e.Arguments = RedactJSONBytes(e.Arguments)
	_, err := l.db.ExecContext(ctx,
		`INSERT INTO audit_events(id, ts, agent_id, upstream_name, tool_name, event_type,
            decision, reason, arguments, result_summary, approval_id)
         VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		e.ID, e.TS,
		nullStr(e.AgentID), nullStr(e.UpstreamName), nullStr(e.ToolName),
		e.EventType, nullStr(e.Decision), nullStr(e.Reason),
		nullRaw(e.Arguments), nullStr(e.ResultSummary), nullStr(e.ApprovalID),
	)
	if err != nil {
		return err
	}
	l.fanOut(e)
	return nil
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

// Recent returns up to limit recent events, newest first.
func (l *Logger) Recent(ctx context.Context, limit int) ([]Event, error) {
	return l.RecentBefore(ctx, limit, 0)
}

// Filter narrows an audit Query. Zero-value fields are ignored. Timestamps
// are ms-UTC.
type Filter struct {
	Since     int64
	Until     int64
	Before    int64 // ts < Before, pagination cursor
	AgentID   string
	EventType string
	Tool      string
	Decision  string
	Limit     int
}

// Query returns events matching the filter, newest first. Backs both the
// audit export and the filtered audit list so the UI and export agree.
func (l *Logger) Query(ctx context.Context, f Filter) ([]Event, error) {
	q := `SELECT id, ts, agent_id, upstream_name, tool_name, event_type, decision,
            reason, arguments, result_summary, approval_id
         FROM audit_events WHERE 1=1`
	var args []any
	if f.Since > 0 {
		q += ` AND ts >= ?`
		args = append(args, f.Since)
	}
	if f.Until > 0 {
		q += ` AND ts <= ?`
		args = append(args, f.Until)
	}
	if f.Before > 0 {
		q += ` AND ts < ?`
		args = append(args, f.Before)
	}
	if f.AgentID != "" {
		q += ` AND agent_id = ?`
		args = append(args, f.AgentID)
	}
	if f.EventType != "" {
		q += ` AND event_type = ?`
		args = append(args, f.EventType)
	}
	if f.Tool != "" {
		q += ` AND tool_name = ?`
		args = append(args, f.Tool)
	}
	if f.Decision != "" {
		q += ` AND decision = ?`
		args = append(args, f.Decision)
	}
	q += ` ORDER BY ts DESC`
	if f.Limit > 0 {
		q += ` LIMIT ?`
		args = append(args, f.Limit)
	}
	rows, err := l.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		var agent, upstream, tool, decision, reason, eargs, summary, approval sql.NullString
		if err := rows.Scan(&e.ID, &e.TS, &agent, &upstream, &tool, &e.EventType,
			&decision, &reason, &eargs, &summary, &approval); err != nil {
			return nil, err
		}
		e.AgentID = agent.String
		e.UpstreamName = upstream.String
		e.ToolName = tool.String
		e.Decision = decision.String
		e.Reason = reason.String
		if eargs.Valid && eargs.String != "" {
			e.Arguments = json.RawMessage(eargs.String)
		}
		e.ResultSummary = summary.String
		e.ApprovalID = approval.String
		out = append(out, e)
	}
	return out, rows.Err()
}

// RecentBefore returns up to limit events older than the `before` ts_ms
// cursor, newest first. before<=0 means "from the newest". Backs the
// dashboard's "Load older" pagination; uses idx_audit_ts.
func (l *Logger) RecentBefore(ctx context.Context, limit int, before int64) ([]Event, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	const cols = `id, ts, agent_id, upstream_name, tool_name, event_type, decision,
            reason, arguments, result_summary, approval_id`
	var rows *sql.Rows
	var err error
	if before > 0 {
		rows, err = l.db.QueryContext(ctx,
			`SELECT `+cols+` FROM audit_events WHERE ts < ? ORDER BY ts DESC LIMIT ?`, before, limit)
	} else {
		rows, err = l.db.QueryContext(ctx,
			`SELECT `+cols+` FROM audit_events ORDER BY ts DESC LIMIT ?`, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var agent, upstream, tool, decision, reason, args, summary, approval sql.NullString
		if err := rows.Scan(&e.ID, &e.TS, &agent, &upstream, &tool, &e.EventType,
			&decision, &reason, &args, &summary, &approval); err != nil {
			return nil, err
		}
		e.AgentID = agent.String
		e.UpstreamName = upstream.String
		e.ToolName = tool.String
		e.Decision = decision.String
		e.Reason = reason.String
		if args.Valid && args.String != "" {
			e.Arguments = json.RawMessage(args.String)
		}
		e.ResultSummary = summary.String
		e.ApprovalID = approval.String
		out = append(out, e)
	}
	return out, rows.Err()
}
