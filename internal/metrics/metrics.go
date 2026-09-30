// Package metrics records one fact row per tool call and exposes
// analytics queries used by the auto-approval engine, the Insights dashboard
// tab, and the anomaly detector.
//
// The Sink is async: callers hand off a populated Event and return; an
// internal goroutine batches inserts so the request path never blocks on
// disk IO. If the buffer overflows or the writer is closed, events are
// dropped on the floor — losing analytics rows is not a correctness issue
// for toolyard.
package metrics

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/tusharbhardwaj/toolyard/internal/logx"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// dropLogEvery controls how often a full-buffer drop is logged: the first
// drop, then every Nth drop thereafter. Keeps the log readable under a
// sustained overflow without going fully silent.
const dropLogEvery = 500

// Outcome values stored on call_events.outcome.
const (
	OutcomeOK       = "ok"
	OutcomeError    = "error"
	OutcomeDenied   = "denied"   // policy or human denied
	OutcomeExpired  = "expired"  // approval expired without decision
	OutcomeDeferred = "deferred" // deferred response returned
)

// ApprovalOutcome values stored on call_events.approval_outcome.
const (
	ApprovalNone     = "none"
	ApprovalAuto     = "auto"
	ApprovalApproved = "approved"
	ApprovalDenied   = "denied"
	ApprovalExpired  = "expired"
)

// Event is the per-call fact row. Fields left zero are stored as NULL so the
// dashboard can distinguish "absent" from "zero".
type Event struct {
	TS        int64
	RequestID string
	// SessionID is the toolyard agent session ("ses_…") the call was
	// tagged with, else the MCP session id.
	SessionID string
	AgentID   string
	AgentName string
	// OwnerUserID and ClientKind come from the call's actor.Raiser: the
	// person the agent works for and the client software that raised it.
	OwnerUserID string
	ClientKind  string

	Upstream      string
	ShortName     string
	ToolName      string
	IsWrite       bool
	IsDestructive bool
	PinnedTool    bool

	Via         string // direct | tools.execute | dashboard | cli | voice | auto-execute
	SurfaceMode string
	InTopN      *bool

	Fingerprint   string
	ArgsSizeBytes int
	ArgsTopKeys   []string

	ReasonText     string
	ReasonLen      int
	IntentCategory string

	ApprovalID        string
	ApprovalOutcome   string
	ApprovalLatencyMs int
	ApprovalVia       string
	ApprovalDecider   string
	CoalescedInto     string

	Outcome           string
	ErrorClass        string
	QueueLatencyMs    int
	UpstreamLatencyMs int
	TotalLatencyMs    int
	ResultSizeBytes   int
}

// Sink accepts events asynchronously. Implementations must be safe for
// concurrent calls.
type Sink interface {
	Record(Event)
	Close(ctx context.Context) error
}

// Noop is used in tests / probe binaries that don't need persistence.
type Noop struct{}

func (Noop) Record(Event)                    {}
func (Noop) Close(ctx context.Context) error { return nil }

// Recorder is the production Sink — buffered channel + background flusher.
type Recorder struct {
	db        *store.DB
	log       *slog.Logger
	in        chan Event
	stop      chan struct{}
	done      chan struct{}
	dropped   uint64
	flushSize int
	flushTick time.Duration
}

// New creates a Recorder. Capacity bounds the in-memory buffer; on overflow
// new events are dropped (with a counter that callers can inspect via
// DroppedCount). Reasonable defaults: capacity=4096, batch=200, flush=2s.
func New(db *store.DB) *Recorder {
	r := &Recorder{
		db:        db,
		log:       logx.For("metrics"),
		in:        make(chan Event, 512),
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
		flushSize: 200,
		flushTick: 2 * time.Second,
	}
	go r.run()
	return r
}

// Record hands an event to the writer goroutine. Non-blocking; events are
// dropped if the buffer is full.
func (r *Recorder) Record(e Event) {
	if r == nil {
		return
	}
	if e.TS == 0 {
		e.TS = time.Now().UnixMilli()
	}
	select {
	case r.in <- e:
	default:
		// Buffer full: drop the event (analytics loss is non-fatal) but make
		// the drop observable. Log the first drop, then every dropLogEvery'th
		// thereafter so a sustained overflow stays visible without flooding.
		dropped := atomic.AddUint64(&r.dropped, 1)
		if dropped == 1 || dropped%dropLogEvery == 0 {
			r.logger().Warn("metrics buffer full, dropping event", "dropped_total", dropped)
		}
	}
}

// logger returns the recorder's logger, falling back to the package default
// for zero-value / hand-built Recorders that skipped New (e.g. tests).
func (r *Recorder) logger() *slog.Logger {
	if r.log != nil {
		return r.log
	}
	return logx.For("metrics")
}

// DroppedCount returns the cumulative number of events dropped due to full
// buffer. Surfaced on /v1/insights/overview as a health signal.
func (r *Recorder) DroppedCount() uint64 {
	if r == nil {
		return 0
	}
	return atomic.LoadUint64(&r.dropped)
}

// Close drains the buffer and stops the writer goroutine. Safe to call once.
func (r *Recorder) Close(ctx context.Context) error {
	if r == nil {
		return nil
	}
	close(r.stop)
	select {
	case <-r.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

func (r *Recorder) run() {
	defer close(r.done)
	buf := make([]Event, 0, r.flushSize)
	tick := time.NewTicker(r.flushTick)
	defer tick.Stop()
	flush := func() {
		if len(buf) == 0 {
			return
		}
		if err := r.insertBatch(buf); err != nil {
			log.Printf("metrics: batch insert: %v", err)
		}
		buf = buf[:0]
	}
	for {
		select {
		case <-r.stop:
			// Drain remaining events.
			for {
				select {
				case e := <-r.in:
					buf = append(buf, e)
					if len(buf) >= r.flushSize {
						flush()
					}
				default:
					flush()
					return
				}
			}
		case e := <-r.in:
			buf = append(buf, e)
			if len(buf) >= r.flushSize {
				flush()
			}
		case <-tick.C:
			flush()
		}
	}
}

func (r *Recorder) insertBatch(evs []Event) error {
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(`INSERT INTO call_events(
        ts, request_id, session_id, agent_id, agent_name,
        upstream, short_name, tool_name, is_write, is_destructive, pinned_tool,
        via, surface_mode, in_top_n,
        fingerprint, args_size_bytes, args_top_keys,
        reason_text, reason_len, intent_category,
        approval_id, approval_outcome, approval_latency_ms, approval_via,
        approval_decider, coalesced_into,
        outcome, error_class, queue_latency_ms, upstream_latency_ms,
        total_latency_ms, result_size_bytes,
        token_estimate_in, token_estimate_out,
        owner_user_id, client_kind
    ) VALUES(?,?,?,?,?, ?,?,?,?,?,?, ?,?,?, ?,?,?, ?,?,?, ?,?,?,?,?,?, ?,?,?,?, ?,?, ?,?, ?,?)`)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	defer stmt.Close()
	for _, e := range evs {
		topKeys := ""
		if len(e.ArgsTopKeys) > 0 {
			b, _ := json.Marshal(e.ArgsTopKeys)
			topKeys = string(b)
		}
		_, err := stmt.Exec(
			e.TS, nullStr(e.RequestID), nullStr(e.SessionID), nullStr(e.AgentID), nullStr(e.AgentName),
			e.Upstream, e.ShortName, e.ToolName, boolInt(e.IsWrite), boolInt(e.IsDestructive), boolInt(e.PinnedTool),
			nullStr(e.Via), nullStr(e.SurfaceMode), nullBool(e.InTopN),
			nullStr(e.Fingerprint), nullIntZero(e.ArgsSizeBytes), nullStr(topKeys),
			nullStr(e.ReasonText), nullIntZero(e.ReasonLen), nullStr(e.IntentCategory),
			nullStr(e.ApprovalID), nullStr(e.ApprovalOutcome), nullIntZero(e.ApprovalLatencyMs), nullStr(e.ApprovalVia),
			nullStr(e.ApprovalDecider), nullStr(e.CoalescedInto),
			e.Outcome, nullStr(e.ErrorClass), nullIntZero(e.QueueLatencyMs), nullIntZero(e.UpstreamLatencyMs),
			nullIntZero(e.TotalLatencyMs), nullIntZero(e.ResultSizeBytes),
			tokenEstimate(e.ArgsSizeBytes), tokenEstimate(e.ResultSizeBytes),
			nullStr(e.OwnerUserID), nullStr(e.ClientKind),
		)
		if err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullIntZero(n int) any {
	if n == 0 {
		return nil
	}
	return n
}

func nullBool(b *bool) any {
	if b == nil {
		return nil
	}
	if *b {
		return 1
	}
	return 0
}

// tokenEstimate returns a rough "1 token ~ 4 bytes" proxy. Cheap and good
// enough for relative ranking in the cost panel.
func tokenEstimate(bytes int) any {
	if bytes <= 0 {
		return nil
	}
	return (bytes + 3) / 4
}

// ArgsShape extracts top-level keys (sorted, capped at 32) and JSON-encoded
// size in bytes. Used by the gateway when populating an Event so we don't
// have to keep raw arg values around.
func ArgsShape(args map[string]any) (keys []string, size int) {
	if len(args) == 0 {
		return nil, 0
	}
	keys = make([]string, 0, len(args))
	for k := range args {
		// Skip the schema-wrap fields — they are gateway plumbing, not real
		// arguments and they bloat the keys list.
		if k == "_reason" || k == "_intent_category" || k == "_approval_id" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) > 32 {
		keys = keys[:32]
	}
	if b, err := json.Marshal(args); err == nil {
		size = len(b)
	}
	return keys, size
}

// ToBoolPtr is a small helper for callers that have a plain bool but need
// the Event's optional pointer form.
func ToBoolPtr(b bool) *bool { return &b }

// --- Read side / analytics ---------------------------------------------------

// Reader runs analytics queries. It does not own the DB connection lifecycle;
// callers pass in the same *store.DB used elsewhere.
type Reader struct{ db *store.DB }

func NewReader(db *store.DB) *Reader { return &Reader{db: db} }

// Range describes a closed-open time interval for queries.
type Range struct {
	From time.Time // inclusive
	To   time.Time // exclusive; zero means now
}

func (r Range) bounds() (int64, int64) {
	from := r.From.UnixMilli()
	if r.From.IsZero() {
		from = 0
	}
	to := r.To.UnixMilli()
	if r.To.IsZero() {
		to = time.Now().UnixMilli()
	}
	return from, to
}

// Overview is the headline numbers panel.
type Overview struct {
	Calls            int64   `json:"calls"`
	Errors           int64   `json:"errors"`
	WriteCalls       int64   `json:"write_calls"`
	Approvals        int64   `json:"approvals"`
	AutoApprovals    int64   `json:"auto_approvals"`
	Denials          int64   `json:"denials"`
	ExpiredApprovals int64   `json:"expired_approvals"`
	DistinctAgents   int64   `json:"distinct_agents"`
	DistinctTools    int64   `json:"distinct_tools"`
	BytesIn          int64   `json:"bytes_in"`
	BytesOut         int64   `json:"bytes_out"`
	P50LatencyMs     int     `json:"p50_latency_ms"`
	P95LatencyMs     int     `json:"p95_latency_ms"`
	ErrorRate        float64 `json:"error_rate"`
}

// Overview returns the Overview block for the given range.
func (r *Reader) Overview(ctx context.Context, rg Range) (*Overview, error) {
	from, to := rg.bounds()
	row := r.db.QueryRowContext(ctx, `SELECT
        COUNT(*),
        COALESCE(SUM(CASE WHEN outcome='error' THEN 1 ELSE 0 END),0),
        COALESCE(SUM(CASE WHEN is_write=1 THEN 1 ELSE 0 END),0),
        COALESCE(SUM(CASE WHEN approval_outcome='approved' THEN 1 ELSE 0 END),0),
        COALESCE(SUM(CASE WHEN approval_outcome='auto' THEN 1 ELSE 0 END),0),
        COALESCE(SUM(CASE WHEN approval_outcome='denied' THEN 1 ELSE 0 END),0),
        COALESCE(SUM(CASE WHEN approval_outcome='expired' THEN 1 ELSE 0 END),0),
        COUNT(DISTINCT agent_id),
        COUNT(DISTINCT tool_name),
        COALESCE(SUM(args_size_bytes),0),
        COALESCE(SUM(result_size_bytes),0)
        FROM call_events WHERE ts >= ? AND ts < ?`, from, to)
	o := &Overview{}
	if err := row.Scan(&o.Calls, &o.Errors, &o.WriteCalls, &o.Approvals, &o.AutoApprovals,
		&o.Denials, &o.ExpiredApprovals, &o.DistinctAgents, &o.DistinctTools,
		&o.BytesIn, &o.BytesOut); err != nil {
		return nil, err
	}
	if o.Calls > 0 {
		o.ErrorRate = float64(o.Errors) / float64(o.Calls)
	}
	o.P50LatencyMs, o.P95LatencyMs, _ = r.latencyPercentiles(ctx, "", from, to)
	return o, nil
}

// ToolRow summarises one tool's behaviour for the Per-tool table.
type ToolRow struct {
	ToolName       string  `json:"tool_name"`
	Upstream       string  `json:"upstream"`
	Calls          int64   `json:"calls"`
	Errors         int64   `json:"errors"`
	DistinctAgents int64   `json:"distinct_agents"`
	ApprovalRatio  float64 `json:"approval_ratio"`
	ErrorRate      float64 `json:"error_rate"`
	P50LatencyMs   int     `json:"p50_latency_ms"`
	P95LatencyMs   int     `json:"p95_latency_ms"`
	BytesIn        int64   `json:"bytes_in"`
	BytesOut       int64   `json:"bytes_out"`
	LastSeen       int64   `json:"last_seen"`
}

// Tools returns one row per tool seen in the range, sorted by call count desc.
//
// The aggregation cursor is fully drained before any per-tool latency
// percentile query runs — running both nested under SetMaxOpenConns(1)
// would deadlock waiting for the outer cursor to release the connection.
func (r *Reader) Tools(ctx context.Context, rg Range) ([]ToolRow, error) {
	from, to := rg.bounds()
	rows, err := r.db.QueryContext(ctx, `SELECT
        tool_name, upstream,
        COUNT(*) AS calls,
        COALESCE(SUM(CASE WHEN outcome='error' THEN 1 ELSE 0 END),0) AS errors,
        COUNT(DISTINCT agent_id) AS agents,
        COALESCE(SUM(CASE WHEN approval_outcome IN ('approved','auto') THEN 1 ELSE 0 END),0) AS ok_approvals,
        COALESCE(SUM(CASE WHEN approval_outcome IN ('approved','auto','denied','expired') THEN 1 ELSE 0 END),0) AS total_decided,
        COALESCE(SUM(args_size_bytes),0),
        COALESCE(SUM(result_size_bytes),0),
        MAX(ts)
        FROM call_events WHERE ts >= ? AND ts < ?
        GROUP BY tool_name, upstream
        ORDER BY calls DESC`, from, to)
	if err != nil {
		return nil, err
	}
	var out []ToolRow
	for rows.Next() {
		var t ToolRow
		var totalDecided, okApprovals int64
		if err := rows.Scan(&t.ToolName, &t.Upstream, &t.Calls, &t.Errors, &t.DistinctAgents,
			&okApprovals, &totalDecided, &t.BytesIn, &t.BytesOut, &t.LastSeen); err != nil {
			rows.Close()
			return nil, err
		}
		if t.Calls > 0 {
			t.ErrorRate = float64(t.Errors) / float64(t.Calls)
		}
		if totalDecided > 0 {
			t.ApprovalRatio = float64(okApprovals) / float64(totalDecided)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	for i := range out {
		out[i].P50LatencyMs, out[i].P95LatencyMs, _ = r.latencyPercentiles(ctx, out[i].ToolName, from, to)
	}
	return out, nil
}

// AgentRow summarises one agent's behaviour for the Per-agent panel.
type AgentRow struct {
	AgentID       string  `json:"agent_id"`
	AgentName     string  `json:"agent_name"`
	Calls         int64   `json:"calls"`
	Errors        int64   `json:"errors"`
	DistinctTools int64   `json:"distinct_tools"`
	WriteCalls    int64   `json:"write_calls"`
	ApprovalRatio float64 `json:"approval_ratio"`
	ErrorRate     float64 `json:"error_rate"`
	P95LatencyMs  int     `json:"p95_latency_ms"`
	LastSeen      int64   `json:"last_seen"`
}

// Agents returns one row per agent seen in the range. Same drain-first
// discipline as Tools: outer aggregation cursor closes before per-agent
// latency queries run.
func (r *Reader) Agents(ctx context.Context, rg Range) ([]AgentRow, error) {
	from, to := rg.bounds()
	rows, err := r.db.QueryContext(ctx, `SELECT
        COALESCE(agent_id,''), COALESCE(MAX(agent_name),''),
        COUNT(*) AS calls,
        COALESCE(SUM(CASE WHEN outcome='error' THEN 1 ELSE 0 END),0) AS errors,
        COUNT(DISTINCT tool_name) AS tools,
        COALESCE(SUM(CASE WHEN is_write=1 THEN 1 ELSE 0 END),0) AS writes,
        COALESCE(SUM(CASE WHEN approval_outcome IN ('approved','auto') THEN 1 ELSE 0 END),0) AS ok_approvals,
        COALESCE(SUM(CASE WHEN approval_outcome IN ('approved','auto','denied','expired') THEN 1 ELSE 0 END),0) AS total_decided,
        MAX(ts)
        FROM call_events WHERE ts >= ? AND ts < ?
        GROUP BY agent_id
        ORDER BY calls DESC`, from, to)
	if err != nil {
		return nil, err
	}
	var out []AgentRow
	for rows.Next() {
		var a AgentRow
		var totalDecided, okApprovals int64
		if err := rows.Scan(&a.AgentID, &a.AgentName, &a.Calls, &a.Errors, &a.DistinctTools,
			&a.WriteCalls, &okApprovals, &totalDecided, &a.LastSeen); err != nil {
			rows.Close()
			return nil, err
		}
		if a.Calls > 0 {
			a.ErrorRate = float64(a.Errors) / float64(a.Calls)
		}
		if totalDecided > 0 {
			a.ApprovalRatio = float64(okApprovals) / float64(totalDecided)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	for i := range out {
		out[i].P95LatencyMs = r.agentP95(ctx, out[i].AgentID, from, to)
	}
	return out, nil
}

// FingerprintStat is the per-(fingerprint, agent) summary used by the auto-
// approval engine and the rule proposer.
type FingerprintStat struct {
	Fingerprint   string `json:"fingerprint"`
	AgentID       string `json:"agent_id"`
	ToolName      string `json:"tool_name"`
	FirstSeen     int64  `json:"first_seen"`
	LastSeen      int64  `json:"last_seen"`
	LastDecidedTs int64  `json:"last_decided_ts"`
	TotalSeen     int64  `json:"total_seen"`
	Approved      int64  `json:"approved"`
	AutoApproved  int64  `json:"auto_approved"`
	Denied        int64  `json:"denied"`
	Expired       int64  `json:"expired"`
}

// FingerprintStats returns per-(agent, fingerprint) stats over the given
// window. Used by the auto-approval rule proposer (and by the engine itself
// for the pattern-rule check).
func (r *Reader) FingerprintStats(ctx context.Context, rg Range) ([]FingerprintStat, error) {
	from, to := rg.bounds()
	rows, err := r.db.QueryContext(ctx, `SELECT
        fingerprint, COALESCE(agent_id,''), MAX(tool_name),
        MIN(ts), MAX(ts),
        COALESCE(MAX(CASE WHEN approval_outcome IN ('approved','auto','denied','expired') THEN ts END), 0) AS last_decided,
        COUNT(*) AS total,
        COALESCE(SUM(CASE WHEN approval_outcome='approved' THEN 1 ELSE 0 END),0),
        COALESCE(SUM(CASE WHEN approval_outcome='auto' THEN 1 ELSE 0 END),0),
        COALESCE(SUM(CASE WHEN approval_outcome='denied' THEN 1 ELSE 0 END),0),
        COALESCE(SUM(CASE WHEN approval_outcome='expired' THEN 1 ELSE 0 END),0)
        FROM call_events
        WHERE fingerprint IS NOT NULL AND fingerprint != ''
          AND ts >= ? AND ts < ?
        GROUP BY fingerprint, agent_id
        HAVING SUM(CASE WHEN approval_outcome IN ('approved','auto','denied','expired') THEN 1 ELSE 0 END) > 0
        ORDER BY total DESC`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FingerprintStat
	for rows.Next() {
		var s FingerprintStat
		if err := rows.Scan(&s.Fingerprint, &s.AgentID, &s.ToolName, &s.FirstSeen, &s.LastSeen,
			&s.LastDecidedTs, &s.TotalSeen, &s.Approved, &s.AutoApproved, &s.Denied, &s.Expired); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// FingerprintStat returns a single (fingerprint, agent) record. agentID
// blank returns aggregate across agents.
func (r *Reader) FingerprintStat(ctx context.Context, fp, agentID string, rg Range) (*FingerprintStat, error) {
	from, to := rg.bounds()
	q := `SELECT
        fingerprint, COALESCE(agent_id,''), COALESCE(MAX(tool_name),''),
        COALESCE(MIN(ts),0), COALESCE(MAX(ts),0),
        COALESCE(MAX(CASE WHEN approval_outcome IN ('approved','auto','denied','expired') THEN ts END), 0),
        COUNT(*),
        COALESCE(SUM(CASE WHEN approval_outcome='approved' THEN 1 ELSE 0 END),0),
        COALESCE(SUM(CASE WHEN approval_outcome='auto' THEN 1 ELSE 0 END),0),
        COALESCE(SUM(CASE WHEN approval_outcome='denied' THEN 1 ELSE 0 END),0),
        COALESCE(SUM(CASE WHEN approval_outcome='expired' THEN 1 ELSE 0 END),0)
        FROM call_events
        WHERE fingerprint = ? AND ts >= ? AND ts < ?`
	args := []any{fp, from, to}
	if agentID != "" {
		q += " AND agent_id = ?"
		args = append(args, agentID)
	}
	q += " GROUP BY fingerprint, agent_id"
	row := r.db.QueryRowContext(ctx, q, args...)
	var s FingerprintStat
	if err := row.Scan(&s.Fingerprint, &s.AgentID, &s.ToolName, &s.FirstSeen, &s.LastSeen,
		&s.LastDecidedTs, &s.TotalSeen, &s.Approved, &s.AutoApproved, &s.Denied, &s.Expired); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &s, nil
}

// ToolHealth is the global per-tool aggregate over the configured window
// (defaults to 30d). Used by the rule proposer to decide which tools earn a
// "tool-wide auto-approve" suggestion.
type ToolHealth struct {
	ToolName       string  `json:"tool_name"`
	IsDestructive  bool    `json:"is_destructive"`
	Calls          int64   `json:"calls"`
	DistinctAgents int64   `json:"distinct_agents"`
	ApprovalRatio  float64 `json:"approval_ratio"`
	Denials        int64   `json:"denials"`
	LastSeen       int64   `json:"last_seen"`
}

// ToolHealth returns the per-tool aggregate for the supplied window.
func (r *Reader) ToolHealth(ctx context.Context, rg Range) ([]ToolHealth, error) {
	from, to := rg.bounds()
	rows, err := r.db.QueryContext(ctx, `SELECT
        ce.tool_name,
        COALESCE(MAX(td.is_destructive), MAX(ce.is_destructive)) AS destructive,
        COUNT(*),
        COUNT(DISTINCT ce.agent_id),
        COALESCE(SUM(CASE WHEN approval_outcome IN ('approved','auto') THEN 1 ELSE 0 END),0),
        COALESCE(SUM(CASE WHEN approval_outcome='denied' THEN 1 ELSE 0 END),0),
        COALESCE(SUM(CASE WHEN approval_outcome IN ('approved','auto','denied','expired') THEN 1 ELSE 0 END),0),
        MAX(ce.ts)
        FROM call_events ce
        LEFT JOIN tool_dim td ON td.tool_name = ce.tool_name
        WHERE ce.ts >= ? AND ce.ts < ?
        GROUP BY ce.tool_name
        ORDER BY 3 DESC`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ToolHealth
	for rows.Next() {
		var t ToolHealth
		var destructive int
		var ok, totalDecided int64
		if err := rows.Scan(&t.ToolName, &destructive, &t.Calls, &t.DistinctAgents,
			&ok, &t.Denials, &totalDecided, &t.LastSeen); err != nil {
			return nil, err
		}
		t.IsDestructive = destructive == 1
		if totalDecided > 0 {
			t.ApprovalRatio = float64(ok) / float64(totalDecided)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// HourBucket is one hour-of-day cell in an agent activity heatmap.
type HourBucket struct {
	Day    int `json:"day"`  // unix-millis at start of day, UTC
	Hour   int `json:"hour"` // 0..23
	Calls  int `json:"calls"`
	Errors int `json:"errors"`
}

// AgentHeatmap returns hour-of-day call buckets for the given agent over the
// last 14 days (UTC). Used by the per-agent dashboard panel.
func (r *Reader) AgentHeatmap(ctx context.Context, agentID string, days int) ([]HourBucket, error) {
	if days <= 0 || days > 60 {
		days = 14
	}
	from := time.Now().Add(-time.Duration(days) * 24 * time.Hour).UnixMilli()
	to := time.Now().UnixMilli()
	rows, err := r.db.QueryContext(ctx, `SELECT
        (ts/86400000)*86400000 AS day_ms,
        ((ts/3600000) % 24)    AS hour,
        COUNT(*),
        SUM(CASE WHEN outcome='error' THEN 1 ELSE 0 END)
        FROM call_events
        WHERE COALESCE(agent_id,'') = ? AND ts >= ? AND ts < ?
        GROUP BY day_ms, hour
        ORDER BY day_ms ASC, hour ASC`, agentID, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HourBucket
	for rows.Next() {
		var b HourBucket
		var dayMs int64
		var errors sql.NullInt64
		if err := rows.Scan(&dayMs, &b.Hour, &b.Calls, &errors); err != nil {
			return nil, err
		}
		b.Day = int(dayMs)
		if errors.Valid {
			b.Errors = int(errors.Int64)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// CostRow is one row in the cost panel: per-tool token estimate × rate.
type CostRow struct {
	ToolName     string  `json:"tool_name"`
	Upstream     string  `json:"upstream"`
	TokensIn     int64   `json:"tokens_in"`
	TokensOut    int64   `json:"tokens_out"`
	UsdEstimated float64 `json:"usd_estimated"`
}

// Cost returns the per-tool token totals over the range, multiplied by the
// supplied per-million rates (any default falls back to 0).
func (r *Reader) Cost(ctx context.Context, rg Range, inUsdPerM, outUsdPerM float64) ([]CostRow, error) {
	from, to := rg.bounds()
	rows, err := r.db.QueryContext(ctx, `SELECT tool_name, upstream,
        COALESCE(SUM(token_estimate_in),0),
        COALESCE(SUM(token_estimate_out),0)
        FROM call_events WHERE ts >= ? AND ts < ?
        GROUP BY tool_name, upstream
        ORDER BY 3 + 4 DESC`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CostRow
	for rows.Next() {
		var c CostRow
		if err := rows.Scan(&c.ToolName, &c.Upstream, &c.TokensIn, &c.TokensOut); err != nil {
			return nil, err
		}
		c.UsdEstimated = float64(c.TokensIn)*inUsdPerM/1e6 + float64(c.TokensOut)*outUsdPerM/1e6
		out = append(out, c)
	}
	return out, rows.Err()
}

func (r *Reader) latencyPercentiles(ctx context.Context, toolName string, from, to int64) (int, int, error) {
	q := `SELECT total_latency_ms FROM call_events
          WHERE total_latency_ms IS NOT NULL AND ts >= ? AND ts < ?`
	args := []any{from, to}
	if toolName != "" {
		q += ` AND tool_name = ?`
		args = append(args, toolName)
	}
	q += ` ORDER BY total_latency_ms ASC`
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()
	var samples []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return 0, 0, err
		}
		samples = append(samples, v)
	}
	if len(samples) == 0 {
		return 0, 0, nil
	}
	p50 := samples[len(samples)/2]
	p95 := samples[(len(samples)*95)/100]
	if p95 == 0 && len(samples) > 0 {
		p95 = samples[len(samples)-1]
	}
	return p50, p95, nil
}

func (r *Reader) agentP95(ctx context.Context, agentID string, from, to int64) int {
	rows, err := r.db.QueryContext(ctx, `SELECT total_latency_ms FROM call_events
        WHERE total_latency_ms IS NOT NULL AND COALESCE(agent_id,'') = ?
          AND ts >= ? AND ts < ?
        ORDER BY total_latency_ms ASC`, agentID, from, to)
	if err != nil {
		return 0
	}
	defer rows.Close()
	var samples []int
	for rows.Next() {
		var v int
		if rows.Scan(&v) == nil {
			samples = append(samples, v)
		}
	}
	if len(samples) == 0 {
		return 0
	}
	return samples[(len(samples)*95)/100]
}

// AnomalyEvent mirrors the anomaly_events row.
type AnomalyEvent struct {
	ID         string `json:"id"`
	TS         int64  `json:"ts"`
	Kind       string `json:"kind"`
	Severity   string `json:"severity"`
	AgentID    string `json:"agent_id,omitempty"`
	ToolName   string `json:"tool_name,omitempty"`
	Summary    string `json:"summary"`
	DetailJSON string `json:"detail_json,omitempty"`
	Dismissed  bool   `json:"dismissed"`
}

// Anomalies returns the most recent anomaly events. Limit defaults to 50.
func (r *Reader) Anomalies(ctx context.Context, limit int, includeDismissed bool) ([]AnomalyEvent, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	q := `SELECT id, ts, kind, severity, COALESCE(agent_id,''), COALESCE(tool_name,''),
        summary, COALESCE(detail_json,''), COALESCE(dismissed_at,0)
        FROM anomaly_events`
	if !includeDismissed {
		q += ` WHERE dismissed_at IS NULL`
	}
	q += ` ORDER BY ts DESC LIMIT ?`
	rows, err := r.db.QueryContext(ctx, q, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AnomalyEvent
	for rows.Next() {
		var a AnomalyEvent
		var dismissed int64
		if err := rows.Scan(&a.ID, &a.TS, &a.Kind, &a.Severity, &a.AgentID, &a.ToolName,
			&a.Summary, &a.DetailJSON, &dismissed); err != nil {
			return nil, err
		}
		a.Dismissed = dismissed > 0
		out = append(out, a)
	}
	return out, rows.Err()
}

// DismissAnomaly marks an anomaly_events row as dismissed.
func (r *Reader) DismissAnomaly(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `UPDATE anomaly_events SET dismissed_at = ? WHERE id = ?`,
		time.Now().UnixMilli(), id)
	return err
}

// RecordAnomaly is used by the detector. Public so other internal packages
// can append.
func (r *Reader) RecordAnomaly(ctx context.Context, kind, severity, agentID, toolName, summary string, detail any) error {
	id := "an_" + uuid.NewString()
	var detailBytes []byte
	if detail != nil {
		detailBytes, _ = json.Marshal(detail)
	}
	_, err := r.db.ExecContext(ctx, `INSERT INTO anomaly_events(id, ts, kind, severity, agent_id, tool_name, summary, detail_json)
        VALUES(?,?,?,?,?,?,?,?)`,
		id, time.Now().UnixMilli(), kind, severity, nullStr(agentID), nullStr(toolName), summary, nullStr(string(detailBytes)))
	return err
}

// PurgeAgent removes all metrics rows for the given agent. Used by the
// privacy "forget this agent" button.
func (r *Reader) PurgeAgent(ctx context.Context, agentID string) error {
	if strings.TrimSpace(agentID) == "" {
		return fmt.Errorf("agent_id is required")
	}
	_, err := r.db.ExecContext(ctx, `DELETE FROM call_events WHERE agent_id = ?`, agentID)
	if err != nil {
		return err
	}
	_, err = r.db.ExecContext(ctx, `DELETE FROM approval_events WHERE agent_id = ?`, agentID)
	if err != nil {
		return err
	}
	_, err = r.db.ExecContext(ctx, `DELETE FROM mv_fingerprint_state WHERE agent_id = ?`, agentID)
	if err != nil {
		return err
	}
	return nil
}

// ExportCSV streams a CSV dump of call_events in the supplied range to w.
// One header row, then one data row per event. Memory usage is O(1) — we
// stream straight from the cursor. Capped at 200k rows so a long-range +
// big-DB request can't tie up the connection indefinitely.
const exportRowCap = 200_000

func (r *Reader) ExportCSV(ctx context.Context, w interface{ Write(p []byte) (int, error) }, rg Range) error {
	from, to := rg.bounds()
	rows, err := r.db.QueryContext(ctx, `SELECT
        event_id, ts, COALESCE(agent_id,''), COALESCE(agent_name,''),
        upstream, short_name, tool_name,
        is_write, is_destructive, pinned_tool,
        COALESCE(via,''), COALESCE(surface_mode,''),
        COALESCE(fingerprint,''), COALESCE(args_size_bytes,0),
        COALESCE(reason_text,''), COALESCE(reason_len,0), COALESCE(reason_quality,0),
        COALESCE(approval_id,''), COALESCE(approval_outcome,''),
        COALESCE(approval_latency_ms,0), COALESCE(approval_via,''),
        outcome, COALESCE(error_class,''), COALESCE(total_latency_ms,0),
        COALESCE(result_size_bytes,0)
        FROM call_events WHERE ts >= ? AND ts < ? ORDER BY ts ASC LIMIT ?`,
		from, to, exportRowCap)
	if err != nil {
		return err
	}
	defer rows.Close()
	header := "event_id,ts,agent_id,agent_name,upstream,short_name,tool_name," +
		"is_write,is_destructive,pinned_tool,via,surface_mode,fingerprint," +
		"args_size_bytes,reason_text,reason_len,reason_quality,approval_id," +
		"approval_outcome,approval_latency_ms,approval_via,outcome,error_class," +
		"total_latency_ms,result_size_bytes\n"
	if _, err := w.Write([]byte(header)); err != nil {
		return err
	}
	for rows.Next() {
		var (
			eventID, ts                                       int64
			agentID, agentName, upstream, shortName, toolName string
			isWrite, isDestructive, pinned                    int
			via, surfaceMode, fp                              string
			argsSize                                          int
			reason                                            string
			reasonLen                                         int
			reasonQuality                                     float64
			approvalID, approvalOutcome                       string
			approvalLatency                                   int
			approvalVia                                       string
			outcome, errClass                                 string
			totalLatency, resultSize                          int
		)
		if err := rows.Scan(&eventID, &ts, &agentID, &agentName, &upstream, &shortName, &toolName,
			&isWrite, &isDestructive, &pinned, &via, &surfaceMode, &fp, &argsSize,
			&reason, &reasonLen, &reasonQuality, &approvalID, &approvalOutcome,
			&approvalLatency, &approvalVia, &outcome, &errClass, &totalLatency, &resultSize); err != nil {
			return err
		}
		fields := []string{
			fmt.Sprintf("%d", eventID), fmt.Sprintf("%d", ts), agentID, agentName,
			upstream, shortName, toolName,
			fmt.Sprintf("%d", isWrite), fmt.Sprintf("%d", isDestructive), fmt.Sprintf("%d", pinned),
			via, surfaceMode, fp, fmt.Sprintf("%d", argsSize),
			reason, fmt.Sprintf("%d", reasonLen), fmt.Sprintf("%.3f", reasonQuality),
			approvalID, approvalOutcome, fmt.Sprintf("%d", approvalLatency), approvalVia,
			outcome, errClass, fmt.Sprintf("%d", totalLatency), fmt.Sprintf("%d", resultSize),
		}
		if _, err := w.Write([]byte(csvEscape(fields))); err != nil {
			return err
		}
	}
	return rows.Err()
}

// csvEscape returns a CSV row terminated with \n. Fields containing comma,
// double-quote, or newline are wrapped in quotes with internal quotes
// doubled.
func csvEscape(fields []string) string {
	var b strings.Builder
	for i, f := range fields {
		if i > 0 {
			b.WriteByte(',')
		}
		needsQuote := strings.ContainsAny(f, ",\"\n\r")
		if !needsQuote {
			b.WriteString(f)
			continue
		}
		b.WriteByte('"')
		b.WriteString(strings.ReplaceAll(f, `"`, `""`))
		b.WriteByte('"')
	}
	b.WriteByte('\n')
	return b.String()
}

// PurgeOlderThan deletes call_events rows older than `before`. Returns the
// number of rows removed. Used by the retention compactor.
func (r *Reader) PurgeOlderThan(ctx context.Context, before time.Time) (int64, error) {
	res, err := r.db.ExecContext(ctx, `DELETE FROM call_events WHERE ts < ?`, before.UnixMilli())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}
