// Package audit appends and queries toolyard's audit event log.
package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"sync"
	"time"

	"github.com/google/uuid"

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
}

type Logger struct {
	db   *store.DB
	subs subscribers
}

type subscribers struct {
	mu      sync.Mutex
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
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := l.db.QueryContext(ctx,
		`SELECT id, ts, agent_id, upstream_name, tool_name, event_type, decision,
            reason, arguments, result_summary, approval_id
         FROM audit_events ORDER BY ts DESC LIMIT ?`, limit)
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
