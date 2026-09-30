package metrics

import (
	"context"
	"database/sql"
	"sort"
	"time"
)

// ApprovalLatencyEstimate is the rolled-up p50/p90 of how long the human
// reviewer took to decide approvals matching a given (fingerprint, tool,
// upstream) tuple. The most-specific bucket with at least MinSamples
// observations wins; we fall back fingerprint -> tool -> upstream ->
// global so the AI always gets a usable hint instead of zero.
type ApprovalLatencyEstimate struct {
	P50Seconds int    `json:"p50_seconds"`
	P90Seconds int    `json:"p90_seconds"`
	Samples    int    `json:"samples"`
	BasedOn    string `json:"based_on"` // "fingerprint" | "tool" | "upstream" | "global" | "default"
	WindowDays int    `json:"window_days"`
}

// MinApprovalLatencySamples is the minimum number of decided approvals
// in a bucket before we'll trust its percentiles. Below that we widen.
const MinApprovalLatencySamples = 5

// DefaultApprovalLatencyWindowDays is how far back we look for samples.
// 30 days is a good balance of "enough samples to stabilize" and "still
// reflects current reviewer behaviour."
const DefaultApprovalLatencyWindowDays = 30

// ApprovalLatency picks the tightest bucket with enough samples and
// returns p50/p90 in seconds. Always returns a non-nil estimate; if no
// data exists at all, BasedOn=="default" and percentiles are zeroed
// (callers should fall back to a sensible static guess).
func (r *Reader) ApprovalLatency(ctx context.Context, fingerprint, toolName, upstream string) *ApprovalLatencyEstimate {
	now := time.Now().UnixMilli()
	from := now - int64(DefaultApprovalLatencyWindowDays)*24*60*60*1000
	if est := r.approvalLatencyByFingerprint(ctx, fingerprint, from, now); est != nil {
		return est
	}
	if est := r.approvalLatencyByTool(ctx, toolName, from, now); est != nil {
		return est
	}
	if est := r.approvalLatencyByUpstream(ctx, upstream, from, now); est != nil {
		return est
	}
	if est := r.approvalLatencyGlobal(ctx, from, now); est != nil {
		return est
	}
	return &ApprovalLatencyEstimate{BasedOn: "default", WindowDays: DefaultApprovalLatencyWindowDays}
}

func (r *Reader) approvalLatencyByFingerprint(ctx context.Context, fp string, from, to int64) *ApprovalLatencyEstimate {
	if fp == "" {
		return nil
	}
	return r.approvalLatencyQuery(ctx,
		`SELECT approval_latency_ms FROM call_events
         WHERE approval_latency_ms IS NOT NULL
           AND approval_latency_ms > 0
           AND approval_outcome = 'approved'
           AND ts >= ? AND ts < ?
           AND fingerprint = ?`,
		"fingerprint", []any{from, to, fp})
}

func (r *Reader) approvalLatencyByTool(ctx context.Context, tool string, from, to int64) *ApprovalLatencyEstimate {
	if tool == "" {
		return nil
	}
	return r.approvalLatencyQuery(ctx,
		`SELECT approval_latency_ms FROM call_events
         WHERE approval_latency_ms IS NOT NULL
           AND approval_latency_ms > 0
           AND approval_outcome = 'approved'
           AND ts >= ? AND ts < ?
           AND tool_name = ?`,
		"tool", []any{from, to, tool})
}

func (r *Reader) approvalLatencyByUpstream(ctx context.Context, upstream string, from, to int64) *ApprovalLatencyEstimate {
	if upstream == "" {
		return nil
	}
	return r.approvalLatencyQuery(ctx,
		`SELECT approval_latency_ms FROM call_events
         WHERE approval_latency_ms IS NOT NULL
           AND approval_latency_ms > 0
           AND approval_outcome = 'approved'
           AND ts >= ? AND ts < ?
           AND upstream = ?`,
		"upstream", []any{from, to, upstream})
}

func (r *Reader) approvalLatencyGlobal(ctx context.Context, from, to int64) *ApprovalLatencyEstimate {
	return r.approvalLatencyQuery(ctx,
		`SELECT approval_latency_ms FROM call_events
         WHERE approval_latency_ms IS NOT NULL
           AND approval_latency_ms > 0
           AND approval_outcome = 'approved'
           AND ts >= ? AND ts < ?`,
		"global", []any{from, to})
}

func (r *Reader) approvalLatencyQuery(ctx context.Context, q, basedOn string, args []any) *ApprovalLatencyEstimate {
	rows, err := r.db.QueryContext(ctx, q+" ORDER BY approval_latency_ms ASC", args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var samples []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			if errIs(err, sql.ErrNoRows) {
				break
			}
			return nil
		}
		samples = append(samples, v)
	}
	if len(samples) < MinApprovalLatencySamples {
		return nil
	}
	sort.Ints(samples)
	p50 := samples[len(samples)/2]
	idx90 := (len(samples) * 90) / 100
	if idx90 >= len(samples) {
		idx90 = len(samples) - 1
	}
	p90 := samples[idx90]
	return &ApprovalLatencyEstimate{
		P50Seconds: p50 / 1000,
		P90Seconds: p90 / 1000,
		Samples:    len(samples),
		BasedOn:    basedOn,
		WindowDays: DefaultApprovalLatencyWindowDays,
	}
}

func errIs(err, target error) bool { return err == target }
