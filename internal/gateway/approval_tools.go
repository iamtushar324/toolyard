package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tusharbhardwaj/toolyard/internal/approval"
)

// Approval-coordination meta-tool names. They live under the synthetic
// "tools" upstream alongside tools.search / tools.execute and are
// pinned (always visible) so an AI can always discover them from a
// deferred response.
const (
	MetaPollApproval         = "tools.poll_approval"
	MetaPollApprovals        = "tools.poll_approvals"
	MetaWaitForApproval      = "tools.wait_for_approval"
	MetaWaitForApprovals     = "tools.wait_for_approvals"
	MetaListPendingApprovals = "tools.list_my_pending_approvals"
	MetaCancelMyApproval     = "tools.cancel_my_approval"
	MetaApprovalStats        = "tools.approval_stats"
)

// approvalCoordinationTools names the tools approvalMetaTools returns.
// routeEntry does not treat _approval_id on them as a resume.
var approvalCoordinationTools = map[string]bool{
	MetaPollApproval: true, MetaPollApprovals: true,
	MetaWaitForApproval: true, MetaWaitForApprovals: true,
	MetaListPendingApprovals: true, MetaCancelMyApproval: true, MetaApprovalStats: true,
}

// approvalMetaTools returns the read-only approval coordination tools
// the gateway exposes to every agent. None of them require approval
// themselves — they only operate on the calling agent's own queue (or
// on global stats), so there's no security boundary to protect.
//
// Without these, an AI that gets a deferred response has to either
// block on a follow-up call or naively retry the original tool. With
// them, the AI can fan out, batch-poll, and free up its connection
// while the human reviews.
func (g *Gateway) approvalMetaTools() []toolEntry {
	poll := mcp.Tool{
		Name: MetaPollApproval,
		Description: descriptionBanner +
			"Fetch the status — and, when ready, the executed result — of one approval queued earlier. Non-blocking. " +
			"Auto-execute: the moment the human flips an approval to allowed, toolyard runs the original tool itself with your persisted arguments. The poll response carries result.content + result.structured_content as soon as that finishes (status='executed'). " +
			"Status flow: pending -> executed | denied | expired | cancelled. While the executor is mid-flight you'll briefly see status='allowed' with no result; just poll again. " +
			"Do NOT re-call the original tool to get the result — toolyard already ran it; this poll is the canonical way to retrieve the answer.",
		InputSchema: mcp.ToolInputSchema{
			Type:     "object",
			Required: []string{ReasonField, "approval_id"},
			Properties: addMetaProps(map[string]any{
				"approval_id": map[string]any{
					"type":        "string",
					"description": "ID returned in the deferred response (looks like ap_…)",
				},
			}),
		},
	}

	pollMany := mcp.Tool{
		Name: MetaPollApprovals,
		Description: descriptionBanner +
			"Batch-check the status of several approvals in one call. Strongly preferred over calling tools.poll_approval one at a time when multiple approvals are in flight — saves round-trips and lets you plan all next steps from a single response.",
		InputSchema: mcp.ToolInputSchema{
			Type:     "object",
			Required: []string{ReasonField, "approval_ids"},
			Properties: addMetaProps(map[string]any{
				"approval_ids": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type": "string",
					},
					"minItems":    1,
					"maxItems":    32,
					"description": "Up to 32 approval IDs to check at once.",
				},
			}),
		},
	}

	wait := mcp.Tool{
		Name: MetaWaitForApproval,
		Description: descriptionBanner +
			"Block up to timeout_seconds for one approval to reach a final state — either status='executed' (with result.content + result.structured_content populated) or one of denied/expired/cancelled. Returns as soon as that lands, or when the timeout elapses. " +
			"Auto-execute: the gateway runs the approved tool itself, so a single wait_for_approval round-trip gets the executed answer; you do not need to follow up with a re-call of the original tool. " +
			"Use this when expected_decision_in_seconds is short and you'd rather wait than poll. " +
			"Maximum timeout is 300 seconds; the connection is held open server-side. For longer waits, prefer polling.",
		InputSchema: mcp.ToolInputSchema{
			Type:     "object",
			Required: []string{ReasonField, "approval_id"},
			Properties: addMetaProps(map[string]any{
				"approval_id": map[string]any{
					"type": "string",
				},
				"timeout_seconds": map[string]any{
					"type":        "integer",
					"minimum":     1,
					"maximum":     int(WaitForApprovalMaxTimeout.Seconds()),
					"description": "How long to wait before returning the current state. Default 60.",
				},
			}),
		},
	}

	waitMany := mcp.Tool{
		Name: MetaWaitForApprovals,
		Description: descriptionBanner +
			"Block on SEVERAL approvals at once. Server-side fan-in: one connection waits on up to 32 approval_ids and returns when the requested condition is met (or timeout elapses). " +
			"mode='all' (default) returns when EVERY id has reached a terminal state (executed/denied/expired/cancelled) — preferred when you need every result before continuing. " +
			"mode='any' returns as soon as ONE id is terminal — preferred when you can act on the first available result. " +
			"This is strictly more efficient than running tools.wait_for_approval N times in parallel (one server connection vs. N) and than poll-loops via tools.poll_approvals. " +
			"Maximum timeout is 300 seconds.",
		InputSchema: mcp.ToolInputSchema{
			Type:     "object",
			Required: []string{ReasonField, "approval_ids"},
			Properties: addMetaProps(map[string]any{
				"approval_ids": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"minItems":    1,
					"maxItems":    32,
					"description": "Up to 32 approval IDs to wait on.",
				},
				"mode": map[string]any{
					"type":        "string",
					"enum":        []string{"all", "any"},
					"description": "all (default): return when every id is terminal. any: return when any one is terminal.",
				},
				"timeout_seconds": map[string]any{
					"type":        "integer",
					"minimum":     1,
					"maximum":     int(WaitForApprovalMaxTimeout.Seconds()),
					"description": "How long to wait before returning the current state. Default 60.",
				},
			}),
		},
	}

	list := mcp.Tool{
		Name: MetaListPendingApprovals,
		Description: descriptionBanner +
			"List every still-pending approval queued by THIS agent. Use this if you've lost track of approval IDs (e.g., after a restart) or want to plan a batch poll.",
		InputSchema: mcp.ToolInputSchema{
			Type:       "object",
			Required:   []string{ReasonField},
			Properties: addMetaProps(map[string]any{}),
		},
	}

	cancel := mcp.Tool{
		Name: MetaCancelMyApproval,
		Description: descriptionBanner +
			"Withdraw one of YOUR own pending approvals. Use when you've decided you no longer need the call to go through (e.g., user changed their mind). The human reviewer sees the cancellation and the row stops appearing in the queue. Only works on your own pendings.",
		InputSchema: mcp.ToolInputSchema{
			Type:     "object",
			Required: []string{ReasonField, "approval_id"},
			Properties: addMetaProps(map[string]any{
				"approval_id": map[string]any{
					"type": "string",
				},
			}),
		},
	}

	stats := mcp.Tool{
		Name: MetaApprovalStats,
		Description: descriptionBanner +
			"Returns the human reviewer's recent decision-time histogram so you can tune your polling. Mostly useful at the start of a session to set expectations; the deferred-response envelope already includes the relevant percentile inline.",
		InputSchema: mcp.ToolInputSchema{
			Type:     "object",
			Required: []string{ReasonField},
			Properties: addMetaProps(map[string]any{
				"tool_name": map[string]any{
					"type":        "string",
					"description": "Optional: restrict stats to one tool name.",
				},
				"upstream": map[string]any{
					"type":        "string",
					"description": "Optional: restrict stats to one upstream.",
				},
			}),
		},
	}

	return []toolEntry{
		{tool: poll, upstream: "tools", originalName: "poll_approval",
			reasonField: ReasonField, handle: g.handlePollApproval()},
		{tool: pollMany, upstream: "tools", originalName: "poll_approvals",
			reasonField: ReasonField, handle: g.handlePollApprovals()},
		{tool: wait, upstream: "tools", originalName: "wait_for_approval",
			reasonField: ReasonField, handle: g.handleWaitForApproval()},
		{tool: waitMany, upstream: "tools", originalName: "wait_for_approvals",
			reasonField: ReasonField, handle: g.handleWaitForApprovals()},
		{tool: list, upstream: "tools", originalName: "list_my_pending_approvals",
			reasonField: ReasonField, handle: g.handleListMyPendingApprovals()},
		{tool: cancel, upstream: "tools", originalName: "cancel_my_approval",
			reasonField: ReasonField, handle: g.handleCancelMyApproval()},
		{tool: stats, upstream: "tools", originalName: "approval_stats",
			reasonField: ReasonField, handle: g.handleApprovalStats()},
	}
}

// approvalSnapshot is the JSON shape returned for one approval row by
// the meta-tools. Compact deliberately — the AI doesn't need the full
// argument map echoed back at it (it has its own copy from the original
// call).
//
// `status` follows the request lifecycle: pending -> allowed -> executed
// (synthesised when the bus has persisted a result_executed_at) /
// denied / expired / cancelled. The "executed" pseudo-status is the
// agent's signal that `result` is populated and the call is done.
func approvalSnapshot(req *approval.Request, includeArgs bool) map[string]any {
	status := req.Status
	if req.Status == approval.StatusAllowed && req.ResultExecutedAt > 0 {
		status = "executed"
	}
	out := map[string]any{
		"approval_id": req.ID,
		"status":      status,
		"raw_status":  req.Status,
		"tool":        req.ToolName,
		"upstream":    req.UpstreamName,
		"agent_id":    req.AgentID,
		"fingerprint": req.Fingerprint,
		"reason":      req.Reason,
		"queued_at":   time.UnixMilli(req.CreatedAt).UTC().Format(time.RFC3339),
		"expires_at":  time.UnixMilli(req.ExpiresAt).UTC().Format(time.RFC3339),
		"age_seconds": int((time.Now().UnixMilli() - req.CreatedAt) / 1000),
	}
	if req.DecidedAt > 0 {
		out["decided_at"] = time.UnixMilli(req.DecidedAt).UTC().Format(time.RFC3339)
		out["decided_by"] = req.DecidedBy
		out["decision_latency_seconds"] = int((req.DecidedAt - req.CreatedAt) / 1000)
	}
	switch req.Status {
	case approval.StatusAllowed:
		// Auto-execute: gateway runs the approved tool itself. The
		// snapshot carries the result if the executor has finished;
		// otherwise it tells the agent to keep polling rather than
		// re-call the original tool.
		if req.ResultExecutedAt > 0 {
			out["executed_at"] = time.UnixMilli(req.ResultExecutedAt).UTC().Format(time.RFC3339)
			out["execution_latency_seconds"] = int((req.ResultExecutedAt - req.DecidedAt) / 1000)
			if req.ResultError != "" {
				out["execution_error"] = req.ResultError
			}
			result, err := rehydrateApprovalResult(req.ResultEnvelope)
			if err != nil {
				out["execution_error_decode"] = err.Error()
			} else if result != nil {
				out["result"] = map[string]any{
					"is_error":           result.IsError,
					"content":            result.Content,
					"structured_content": result.StructuredContent,
				}
			}
			out["how_to_use_result"] = "result.content + result.structured_content are exactly what the original tool returned. The call is done; nothing else to do."
		} else {
			out["how_to_get_result"] = "The tool is executing right now (the human just approved). Poll again shortly with the same approval_id; the response will carry result.content + result.structured_content once the executor finishes."
		}
	case approval.StatusDenied:
		out["how_to_proceed"] = "The human reviewer rejected this call. Do not re-fire the same fingerprint; explain to the user what they declined and ask for an alternative."
	case approval.StatusExpired:
		out["how_to_proceed"] = "The approval window expired without a decision. If still relevant, fire the tool again to enqueue a fresh approval."
	case approval.StatusCancelled:
		out["how_to_proceed"] = "This approval was cancelled (by the agent or operator). If still needed, fire the tool again."
	case approval.StatusPending:
		out["how_to_proceed"] = "Still awaiting the human's decision. Once approved, toolyard runs the tool automatically — keep polling this approval_id, no re-call needed."
	}
	if includeArgs {
		out["arguments"] = req.Arguments
	}
	return out
}

// approvalVisibleTo reports whether caller (the gateway caller id, "" for
// an anonymous connection) may read or resume req. An agent sees only the
// approvals it raised, and an anonymous caller is no exception: it sees
// only the approvals anonymous callers raised.
//
// A row with no agent_id is visible to every caller, as in the
// /v1/agents/approvals/{id} route (cliApprovalsOne). Such rows come from
// anonymous calls (a local gateway without -require-auth-on-mcp, or one
// that ran before auth was turned on), so there is no owner to compare
// against; refusing them would strand those approvals for the caller that
// raised them.
//
// Every caller of this answers a mismatch exactly as it answers an unknown
// id, so an agent can't use it to confirm that another agent's id exists.
func approvalVisibleTo(req *approval.Request, caller string) bool {
	return req.AgentID == "" || req.AgentID == caller
}

func (g *Gateway) handlePollApproval() directHandler {
	return func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		id, _ := args["approval_id"].(string)
		if strings.TrimSpace(id) == "" {
			return mcp.NewToolResultError("approval_id is required"), nil
		}
		req, err := g.approval.Get(ctx, id)
		// Scope check: agents only see their own approvals, and another
		// agent's id reads as unknown.
		if errors.Is(err, approval.ErrNotFound) || (err == nil && !approvalVisibleTo(req, agentIDFromContext(ctx))) {
			return mcp.NewToolResultError("unknown approval_id"), nil
		}
		if err != nil {
			return mcp.NewToolResultErrorFromErr("lookup approval", err), nil
		}
		return jsonResultMap(approvalSnapshot(req, false))
	}
}

func (g *Gateway) handlePollApprovals() directHandler {
	return func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		raw, _ := args["approval_ids"].([]any)
		if len(raw) == 0 {
			return mcp.NewToolResultError("approval_ids is required and non-empty"), nil
		}
		if len(raw) > 32 {
			return mcp.NewToolResultError("approval_ids: max 32 per call"), nil
		}
		callerAgent := agentIDFromContext(ctx)
		out := make([]map[string]any, 0, len(raw))
		for _, item := range raw {
			id, _ := item.(string)
			if strings.TrimSpace(id) == "" {
				out = append(out, map[string]any{"approval_id": id, "status": "invalid", "error": "empty id"})
				continue
			}
			req, err := g.approval.Get(ctx, id)
			if err != nil || !approvalVisibleTo(req, callerAgent) {
				out = append(out, map[string]any{"approval_id": id, "status": "unknown"})
				continue
			}
			out = append(out, approvalSnapshot(req, false))
		}
		return jsonResultMap(map[string]any{
			"count":   len(out),
			"results": out,
		})
	}
}

// clampToDeadline caps a wait, in seconds, at what is left of the
// caller's deadline: a wait inside a code-mode script ends with the
// script's clock, a wait under the dispatch timeout with that, and the
// answer is the normal timed-out snapshot rather than a cancelled context.
func clampToDeadline(ctx context.Context, timeoutSec int) int {
	dl, ok := ctx.Deadline()
	if !ok {
		return timeoutSec
	}
	if rem := int(time.Until(dl).Seconds()); rem < timeoutSec {
		if rem < 1 {
			return 1
		}
		return rem
	}
	return timeoutSec
}

func (g *Gateway) handleWaitForApproval() directHandler {
	return func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		id, _ := args["approval_id"].(string)
		if strings.TrimSpace(id) == "" {
			return mcp.NewToolResultError("approval_id is required"), nil
		}
		timeoutSec := 60
		if v, ok := args["timeout_seconds"].(float64); ok && v > 0 {
			timeoutSec = int(v)
		}
		if maxSec := int(WaitForApprovalMaxTimeout.Seconds()); timeoutSec > maxSec {
			timeoutSec = maxSec
		}
		timeoutSec = clampToDeadline(ctx, timeoutSec)
		req, err := g.approval.Get(ctx, id)
		if errors.Is(err, approval.ErrNotFound) || (err == nil && !approvalVisibleTo(req, agentIDFromContext(ctx))) {
			return mcp.NewToolResultError("unknown approval_id"), nil
		}
		if err != nil {
			return mcp.NewToolResultErrorFromErr("lookup approval", err), nil
		}
		// Already in a terminal state (denied/expired/cancelled, or
		// allowed-and-executed) — return immediately. Allowed-but-
		// executor-not-yet-finished still falls through so the caller
		// gets the result in the same round-trip rather than being
		// told "approved, poll again."
		if req.Status != approval.StatusPending {
			executed := req.Status == approval.StatusAllowed && req.ResultExecutedAt > 0
			if req.Status != approval.StatusAllowed || executed {
				return jsonResultMap(approvalSnapshot(req, false))
			}
		}
		// Subscribe to the bus's per-approval signal channel. If the
		// approval row is in a different process / restart, Watch returns
		// false; we still poll briefly as fallback below.
		ch, hasWatcher := g.approval.Watch(id)
		timer := time.NewTimer(time.Duration(timeoutSec) * time.Second)
		defer timer.Stop()
		if hasWatcher {
			select {
			case <-ch:
			case <-timer.C:
			case <-ctx.Done():
			}
		} else {
			// Fallback poll loop — coarse so we don't hammer SQLite.
			pollTimer := time.NewTimer(2 * time.Second)
			defer pollTimer.Stop()
			for {
				select {
				case <-timer.C:
					goto done
				case <-ctx.Done():
					goto done
				case <-pollTimer.C:
					req, err = g.approval.Get(ctx, id)
					if err == nil && req.Status != approval.StatusPending {
						goto done
					}
					pollTimer.Reset(2 * time.Second)
				}
			}
		done:
		}
		// Re-fetch authoritative state.
		req, err = g.approval.Get(ctx, id)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("lookup approval", err), nil
		}
		return jsonResultMap(approvalSnapshot(req, false))
	}
}

// handleWaitForApprovals fans in on N approvals at once. Returns when
// the requested condition (mode=any|all) holds across the supplied IDs
// or timeout elapses, whichever comes first. Subscribes to each id's
// in-memory waiter; falls back to a 2 s polling ceiling for ids
// without one (e.g., rows surfaced from a previous process restart).
func (g *Gateway) handleWaitForApprovals() directHandler {
	return func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		raw, _ := args["approval_ids"].([]any)
		if len(raw) == 0 {
			return mcp.NewToolResultError("approval_ids is required and non-empty"), nil
		}
		if len(raw) > 32 {
			return mcp.NewToolResultError("approval_ids: max 32 per call"), nil
		}
		mode := "all"
		if m, ok := args["mode"].(string); ok && m != "" {
			if m != "all" && m != "any" {
				return mcp.NewToolResultError(`mode must be "all" or "any"`), nil
			}
			mode = m
		}
		timeoutSec := 60
		if v, ok := args["timeout_seconds"].(float64); ok && v > 0 {
			timeoutSec = int(v)
		}
		if maxSec := int(WaitForApprovalMaxTimeout.Seconds()); timeoutSec > maxSec {
			timeoutSec = maxSec
		}
		timeoutSec = clampToDeadline(ctx, timeoutSec)
		ids := make([]string, 0, len(raw))
		for _, item := range raw {
			id, _ := item.(string)
			id = strings.TrimSpace(id)
			if id == "" {
				return mcp.NewToolResultError("approval_ids contains an empty string"), nil
			}
			ids = append(ids, id)
		}
		callerAgent := agentIDFromContext(ctx)
		deadline := time.Now().Add(time.Duration(timeoutSec) * time.Second)

		// Each loop iteration: snapshot every id, evaluate condition,
		// return if met or timed out, otherwise reflect.Select on every
		// in-memory watcher channel + a polling tick + timeout + ctx.
		for {
			snapshots := make([]map[string]any, 0, len(ids))
			anyTerm, allTerm := false, true
			for _, id := range ids {
				req, err := g.approval.Get(ctx, id)
				if errors.Is(err, approval.ErrNotFound) || (err == nil && !approvalVisibleTo(req, callerAgent)) {
					// Another agent's id reads as unknown too.
					snapshots = append(snapshots, map[string]any{
						"approval_id": id, "status": "unknown",
					})
					// "unknown" is terminal — there's nothing to wait for.
					anyTerm = true
					continue
				}
				if err != nil {
					return mcp.NewToolResultErrorFromErr("lookup approval", err), nil
				}
				if isTerminal(req) {
					anyTerm = true
				} else {
					allTerm = false
				}
				snapshots = append(snapshots, approvalSnapshot(req, false))
			}

			finished := (mode == "all" && allTerm) || (mode == "any" && anyTerm)
			now := time.Now()
			if finished || !now.Before(deadline) {
				return jsonResultMap(map[string]any{
					"count":        len(snapshots),
					"mode":         mode,
					"all_terminal": allTerm,
					"any_terminal": anyTerm,
					"timed_out":    !finished,
					"results":      snapshots,
				})
			}

			// Build a dynamic select over every available watcher,
			// plus a polling cap (for IDs without an in-memory waiter)
			// plus the deadline plus ctx.Done.
			remaining := time.Until(deadline)
			pollCap := 2 * time.Second
			if remaining < pollCap {
				pollCap = remaining
			}
			cases := make([]reflect.SelectCase, 0, len(ids)+3)
			for _, id := range ids {
				if ch, ok := g.approval.Watch(id); ok {
					cases = append(cases, reflect.SelectCase{
						Dir:  reflect.SelectRecv,
						Chan: reflect.ValueOf(ch),
					})
				}
			}
			cases = append(cases,
				reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(time.After(pollCap))},
				reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(time.After(remaining))},
				reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(ctx.Done())},
			)
			_, _, _ = reflect.Select(cases)
			// Loop back and re-snapshot. The deadline check at the top
			// handles the "timer fired" path uniformly.
		}
	}
}

// isTerminal reports whether the request is in a state that won't
// change further: the human refused / the window expired / the agent
// withdrew, or the call is allowed AND the executor has persisted a
// result. Allowed-without-result is NOT terminal — the executor is
// still mid-flight.
func isTerminal(req *approval.Request) bool {
	switch req.Status {
	case approval.StatusDenied, approval.StatusExpired, approval.StatusCancelled:
		return true
	case approval.StatusAllowed:
		return req.ResultExecutedAt > 0
	}
	return false
}

func (g *Gateway) handleListMyPendingApprovals() directHandler {
	return func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		callerAgent := agentIDFromContext(ctx)
		if callerAgent == "" {
			return mcp.NewToolResultError("only authenticated agents can list their pending approvals"), nil
		}
		rows, err := g.approval.ListPendingByAgent(ctx, callerAgent)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("list pending", err), nil
		}
		snapshots := make([]map[string]any, 0, len(rows))
		for i := range rows {
			snapshots = append(snapshots, approvalSnapshot(&rows[i], false))
		}
		return jsonResultMap(map[string]any{
			"count":    len(snapshots),
			"agent_id": callerAgent,
			"budget":   g.maxPendingPerAgent,
			"pendings": snapshots,
		})
	}
}

func (g *Gateway) handleCancelMyApproval() directHandler {
	return func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		callerAgent := agentIDFromContext(ctx)
		if callerAgent == "" {
			return mcp.NewToolResultError("only authenticated agents can cancel approvals"), nil
		}
		id, _ := args["approval_id"].(string)
		if strings.TrimSpace(id) == "" {
			return mcp.NewToolResultError("approval_id is required"), nil
		}
		req, err := g.approval.CancelByAgent(ctx, id, callerAgent)
		if err != nil {
			if errors.Is(err, approval.ErrNotFound) {
				return mcp.NewToolResultError("unknown approval_id (or not owned by this agent)"), nil
			}
			if errors.Is(err, approval.ErrNotPending) {
				return jsonResultMap(approvalSnapshot(req, false))
			}
			return mcp.NewToolResultErrorFromErr("cancel approval", err), nil
		}
		return jsonResultMap(approvalSnapshot(req, false))
	}
}

func (g *Gateway) handleApprovalStats() directHandler {
	return func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		toolName, _ := args["tool_name"].(string)
		upstream, _ := args["upstream"].(string)
		out := map[string]any{
			"window_days":               defaultExpectedDecisionWindowDays,
			"min_poll_interval_seconds": minPollIntervalSeconds,
		}
		if g.metricsReader != nil {
			est := g.metricsReader.ApprovalLatency(ctx, "", toolName, upstream)
			if est != nil {
				out["expected_decision"] = map[string]any{
					"in_seconds_p50": est.P50Seconds,
					"in_seconds_p90": est.P90Seconds,
					"based_on":       est.BasedOn,
					"samples":        est.Samples,
				}
			}
		}
		// Also include the caller's own queue depth, since they're going
		// to ask anyway.
		if callerAgent := agentIDFromContext(ctx); callerAgent != "" {
			if n, err := g.approval.CountPendingForAgent(ctx, callerAgent); err == nil {
				out["your_pending_count"] = n
				out["your_budget"] = g.maxPendingPerAgent
			}
		}
		return jsonResultMap(out)
	}
}

// jsonResultMap marshals a payload into both the text content (for MCP
// clients that don't read structured_content) and structured_content.
func jsonResultMap(payload map[string]any) (*mcp.CallToolResult, error) {
	body, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return mcp.NewToolResultErrorFromErr("encode payload", err), nil
	}
	res := mcp.NewToolResultText(string(body))
	res.StructuredContent = payload
	return res, nil
}
