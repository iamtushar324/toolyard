package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	MetaListPendingApprovals = "tools.list_my_pending_approvals"
	MetaCancelMyApproval     = "tools.cancel_my_approval"
	MetaApprovalStats        = "tools.approval_stats"
)

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
			"Check the status of one approval queued earlier. Non-blocking — returns the current state immediately. " +
			"Use this when you got a deferred response from a tool call and want to know whether the human has decided yet. " +
			"If the response says status=allowed, re-call the original tool with _approval_id to execute and get the result.",
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
			"Block up to timeout_seconds for a single approval to be decided. Returns as soon as a decision lands, or when the timeout elapses (whichever comes first). " +
			"Use this when you've decided you want to wait synchronously rather than poll — for example, the deferred response says expected_decision_in_seconds=10 and you'd rather wait than check back. " +
			"Maximum timeout is 300 seconds. The connection is held open server-side; for longer waits, prefer polling.",
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
func approvalSnapshot(req *approval.Request, includeArgs bool) map[string]any {
	out := map[string]any{
		"approval_id": req.ID,
		"status":      req.Status,
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
	if req.Status == approval.StatusAllowed {
		out["result_via"] = fmt.Sprintf("re-call %s with _approval_id=%q to execute", req.ToolName, req.ID)
	}
	if includeArgs {
		out["arguments"] = req.Arguments
	}
	return out
}

func (g *Gateway) handlePollApproval() directHandler {
	return func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		id, _ := args["approval_id"].(string)
		if strings.TrimSpace(id) == "" {
			return mcp.NewToolResultError("approval_id is required"), nil
		}
		req, err := g.approval.Get(ctx, id)
		if err != nil {
			if errors.Is(err, approval.ErrNotFound) {
				return mcp.NewToolResultError("unknown approval_id"), nil
			}
			return mcp.NewToolResultErrorFromErr("lookup approval", err), nil
		}
		// Scope check: agents only see their own approvals.
		if agentID := agentIDFromContext(ctx); agentID != "" && req.AgentID != "" && agentID != req.AgentID {
			return mcp.NewToolResultError("approval_id does not belong to the calling agent"), nil
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
			if err != nil {
				out = append(out, map[string]any{"approval_id": id, "status": "unknown"})
				continue
			}
			if callerAgent != "" && req.AgentID != "" && callerAgent != req.AgentID {
				out = append(out, map[string]any{"approval_id": id, "status": "forbidden"})
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
		req, err := g.approval.Get(ctx, id)
		if err != nil {
			if errors.Is(err, approval.ErrNotFound) {
				return mcp.NewToolResultError("unknown approval_id"), nil
			}
			return mcp.NewToolResultErrorFromErr("lookup approval", err), nil
		}
		if callerAgent := agentIDFromContext(ctx); callerAgent != "" && req.AgentID != "" && callerAgent != req.AgentID {
			return mcp.NewToolResultError("approval_id does not belong to the calling agent"), nil
		}
		// Already decided — return immediately, no waiting.
		if req.Status != approval.StatusPending {
			return jsonResultMap(approvalSnapshot(req, false))
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
