// Package audit appends and queries toolyard's audit event log.
package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/tusharbhardwaj/toolyard/internal/actor"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// Event types we record.
const (
	EventCallStart        = "call.start"
	EventCallAllowed      = "call.allowed"
	EventCallDenied       = "call.denied"
	EventCallSucceeded    = "call.succeeded"
	EventCallFailed       = "call.failed"
	EventApprovalCreate   = "approval.create"
	EventApprovalDecide   = "approval.decide"
	EventApprovalExpire   = "approval.expire"
	EventApprovalCancel   = "approval.cancel"
	EventApprovalExecuted = "approval.executed"
	EventAgentEnroll      = "agent.enroll"
	EventUserLogin        = "user.login"
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
	// actor.Raiser on ctx, so call sites needn't repeat it. CallerID is
	// stored in, and read back from, the agent_id column.
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

// eventColumns is the one column list for INSERT and SELECT on
// audit_events. eventArgs and scanEvent follow its order exactly; a
// column added here is added to both, and the round-trip test catches a
// slip.
const eventColumns = `id, ts, agent_id, upstream_name, tool_name, event_type, decision,
            reason, arguments, result_summary, approval_id,
            agent_name, agent_kind, owner_user_id, owner_email, owner_name,
            mcp_session_id, agent_session_id, client_session_id, client_session_claimed,
            client_kind, client_name, client_ip, via,
            decided_by_user_id, decided_by_email, decided_by_name, decided_via, decider_ref`

var eventPlaceholders = strings.TrimSuffix(strings.Repeat("?,", strings.Count(eventColumns, ",")+1), ",")

// Write appends an event. ID and TS are filled in if zero. Empty Raiser
// fields are filled from the actor.Raiser on ctx, so a call site that
// only names the agent still records owner, client and session. Identity
// strings go through actor.Clean; they are ids and names, never secrets,
// and the redactor would wipe any 40-hex id. Reason, result summary and
// arguments are passed through RedactString / RedactJSONBytes so a
// fat-fingered API key in a tool argument doesn't become a permanent leak
// in the audit log.
func (l *Logger) Write(ctx context.Context, e Event) error {
	if e.ID == "" {
		e.ID = "ev_" + uuid.NewString()
	}
	if e.TS == 0 {
		e.TS = time.Now().UnixMilli()
	}
	if r, ok := actor.RaiserFrom(ctx); ok {
		e.Raiser = e.Raiser.Merge(r)
	}
	e.Raiser = cleanRaiser(e.Raiser)
	// agent_id is the caller id column; Raiser.CallerID has no column of
	// its own. An explicit AgentID wins, else the caller on the Raiser.
	if e.AgentID == "" {
		e.AgentID = e.CallerID
	}
	for _, p := range []*string{&e.DecidedByUserID, &e.DecidedByEmail, &e.DecidedByName, &e.DecidedVia, &e.DeciderRef} {
		*p = actor.Clean(*p)
	}
	e.Reason = RedactString(e.Reason)
	e.ResultSummary = RedactString(e.ResultSummary)
	e.Arguments = RedactJSONBytes(e.Arguments)
	_, err := l.db.ExecContext(ctx,
		`INSERT INTO audit_events(`+eventColumns+`) VALUES(`+eventPlaceholders+`)`,
		eventArgs(e)...)
	if err != nil {
		return err
	}
	l.fanOut(e)
	return nil
}

// eventArgs returns e's column values in eventColumns order.
func eventArgs(e Event) []any {
	return []any{
		e.ID, e.TS, nullStr(e.AgentID), nullStr(e.UpstreamName), nullStr(e.ToolName),
		e.EventType, nullStr(e.Decision), nullStr(e.Reason),
		nullRaw(e.Arguments), nullStr(e.ResultSummary), nullStr(e.ApprovalID),
		nullStr(e.AgentName), nullStr(e.AgentKind), nullStr(e.OwnerUserID), nullStr(e.OwnerEmail), nullStr(e.OwnerName),
		nullStr(e.MCPSessionID), nullStr(e.AgentSessionID), nullStr(e.ClientSessionID), boolInt(e.ClientSessionClaimed),
		nullStr(e.ClientKind), nullStr(e.ClientName), nullStr(e.ClientIP), nullStr(e.Via),
		nullStr(e.DecidedByUserID), nullStr(e.DecidedByEmail), nullStr(e.DecidedByName), nullStr(e.DecidedVia), nullStr(e.DeciderRef),
	}
}

// rowScanner is the subset shared by *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanEvent reads one row SELECTed with eventColumns.
func scanEvent(s rowScanner) (Event, error) {
	var e Event
	var args string
	var claimed int
	if err := s.Scan(&e.ID, &e.TS, nstr{&e.AgentID}, nstr{&e.UpstreamName}, nstr{&e.ToolName},
		&e.EventType, nstr{&e.Decision}, nstr{&e.Reason},
		nstr{&args}, nstr{&e.ResultSummary}, nstr{&e.ApprovalID},
		nstr{&e.AgentName}, nstr{&e.AgentKind}, nstr{&e.OwnerUserID}, nstr{&e.OwnerEmail}, nstr{&e.OwnerName},
		nstr{&e.MCPSessionID}, nstr{&e.AgentSessionID}, nstr{&e.ClientSessionID}, &claimed,
		nstr{&e.ClientKind}, nstr{&e.ClientName}, nstr{&e.ClientIP}, nstr{&e.Via},
		nstr{&e.DecidedByUserID}, nstr{&e.DecidedByEmail}, nstr{&e.DecidedByName}, nstr{&e.DecidedVia}, nstr{&e.DeciderRef},
	); err != nil {
		return Event{}, err
	}
	if args != "" {
		e.Arguments = json.RawMessage(args)
	}
	e.ClientSessionClaimed = claimed != 0
	e.CallerID = e.AgentID
	return e, nil
}

// nstr scans a nullable TEXT column into a string, NULL as "".
type nstr struct{ s *string }

func (n nstr) Scan(v any) error {
	switch x := v.(type) {
	case nil:
		*n.s = ""
	case string:
		*n.s = x
	case []byte:
		*n.s = string(x)
	default:
		return fmt.Errorf("audit: cannot scan %T into string", v)
	}
	return nil
}

// cleanRaiser applies actor.Clean to every Raiser field.
func cleanRaiser(r actor.Raiser) actor.Raiser {
	for _, p := range []*string{
		&r.CallerID, &r.AgentName, &r.AgentKind, &r.OwnerUserID, &r.OwnerEmail, &r.OwnerName,
		&r.MCPSessionID, &r.AgentSessionID, &r.ClientSessionID, &r.ClientKind, &r.ClientName,
		&r.ClientIP, &r.Via,
	} {
		*p = actor.Clean(*p)
	}
	return r
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

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
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
	// Actor filters: the owner of the calling agent, the person who
	// decided, the agent session the call was tagged with, the approval
	// the event belongs to, and the client kind (t3, claude_code, cli…).
	OwnerUserID     string
	DecidedByUserID string
	AgentSessionID  string
	ApprovalID      string
	ClientKind      string
	Limit           int
}

// Query returns events matching the filter, newest first. Backs both the
// audit export and the filtered audit list so the UI and export agree.
func (l *Logger) Query(ctx context.Context, f Filter) ([]Event, error) {
	q := `SELECT ` + eventColumns + ` FROM audit_events WHERE 1=1`
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
	for _, eq := range []struct{ col, val string }{
		{"agent_id", f.AgentID},
		{"event_type", f.EventType},
		{"tool_name", f.Tool},
		{"decision", f.Decision},
		{"owner_user_id", f.OwnerUserID},
		{"decided_by_user_id", f.DecidedByUserID},
		{"agent_session_id", f.AgentSessionID},
		{"approval_id", f.ApprovalID},
		{"client_kind", f.ClientKind},
	} {
		if eq.val != "" {
			q += ` AND ` + eq.col + ` = ?`
			args = append(args, eq.val)
		}
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
		e, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
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
	var rows *sql.Rows
	var err error
	if before > 0 {
		rows, err = l.db.QueryContext(ctx,
			`SELECT `+eventColumns+` FROM audit_events WHERE ts < ? ORDER BY ts DESC LIMIT ?`, before, limit)
	} else {
		rows, err = l.db.QueryContext(ctx,
			`SELECT `+eventColumns+` FROM audit_events ORDER BY ts DESC LIMIT ?`, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
