//go:build e2e

// scripts/e2e_approval_v2_test.go covers the new defer-by-default
// approval flow: rich envelope on every approval-required call, plus
// the tools.poll_approval / tools.poll_approvals / tools.wait_for_approval
// / tools.list_my_pending_approvals / tools.cancel_my_approval /
// tools.approval_stats meta-tools.
package scripts

import (
	"context"
	"flag"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

// callApprovalRequiredTool fires a memory.set (built-in write that
// always needs approval) and asserts the response is the new
// rich-envelope deferred shape. Returns the approval_id so subsequent
// tests can drive it.
func callApprovalRequiredTool(t *testing.T, c mcpClientLike, ctx context.Context, key, value, reason string) string {
	t.Helper()
	req := mcp.CallToolRequest{}
	req.Params.Name = "memory.set"
	req.Params.Arguments = map[string]any{
		"_reason": reason,
		"key":     key,
		"value":   value,
	}
	res, err := c.CallTool(ctx, req)
	if err != nil {
		t.Fatalf("memory.set: %v", err)
	}
	sc, _ := res.StructuredContent.(map[string]any)
	if sc == nil {
		t.Fatalf("expected structured_content on deferred response, got %s", dumpResult(res))
	}
	if status, _ := sc["status"].(string); status != "pending_approval" {
		t.Fatalf("expected status=pending_approval, got %v (full=%v)", status, sc)
	}
	apID, _ := sc["approval_id"].(string)
	if apID == "" {
		t.Fatal("deferred response missing approval_id")
	}
	// Quick spot-checks on the rich envelope so we know the new fields land.
	if _, ok := sc["expected_decision"]; !ok {
		t.Errorf("envelope missing expected_decision; full=%v", sc)
	}
	if _, ok := sc["next_steps_for_agent"]; !ok {
		t.Errorf("envelope missing next_steps_for_agent")
	}
	return apID
}

// mcpClientLike is the subset of *client.Client we use, named so the
// helper compiles without importing mcp-go internals into this file.
type mcpClientLike interface {
	CallTool(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error)
}

// TestE2EDeferredEnvelope: an approval-required call returns the rich
// envelope immediately (default in-line-wait now 0s), AI uses
// tools.poll_approval to check status, the human approves, AI re-calls
// the original tool with _approval_id and gets the result.
func TestE2EDeferredEnvelope(t *testing.T) {
	flag.Parse()
	h := &httpClient{base: *toolyardURL}
	mustLogin(t, h)
	token := enrollAgent(t, h, "deferred-envelope")
	c := mcpClient(t, *toolyardURL, token)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	apID := callApprovalRequiredTool(t, c, ctx, "k1", "v1", "writing v1 to k1 to test the new deferred-envelope path")

	// Poll without waiting — should report pending.
	pollReq := mcp.CallToolRequest{}
	pollReq.Params.Name = "tools.poll_approval"
	pollReq.Params.Arguments = map[string]any{
		"_reason":     "checking on my queued write",
		"approval_id": apID,
	}
	pollRes, err := c.CallTool(ctx, pollReq)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	pollSC, _ := pollRes.StructuredContent.(map[string]any)
	if pollSC == nil || pollSC["status"] != "pending" {
		t.Fatalf("expected poll status=pending, got %v", pollSC)
	}

	// Approve via the dashboard.
	var dummy map[string]any
	h.raw(t, "POST", "/v1/approvals/"+apID+"/decide", map[string]string{"Action": "allowed"}, &dummy)

	// Poll again — should now report allowed.
	pollRes2, err := c.CallTool(ctx, pollReq)
	if err != nil {
		t.Fatalf("poll2: %v", err)
	}
	pollSC2, _ := pollRes2.StructuredContent.(map[string]any)
	if pollSC2 == nil || pollSC2["status"] != "allowed" {
		t.Fatalf("expected poll status=allowed after approve, got %v", pollSC2)
	}

	// Re-call original tool with _approval_id to actually execute.
	resumeReq := mcp.CallToolRequest{}
	resumeReq.Params.Name = "memory.set"
	resumeReq.Params.Arguments = map[string]any{
		"_reason":      "resuming after approval landed",
		"_approval_id": apID,
	}
	resumeRes, err := c.CallTool(ctx, resumeReq)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if resumeRes.IsError {
		t.Fatalf("resume errored: %s", dumpResult(resumeRes))
	}
	if !strings.Contains(dumpResult(resumeRes), "v1") {
		t.Errorf("expected resumed call result to contain 'v1', got %s", dumpResult(resumeRes))
	}
}

// TestE2EWaitForApproval: AI fires a write, then immediately calls
// tools.wait_for_approval which blocks until the human decides (or the
// timeout elapses). We approve out-of-band and verify wait returns
// allowed.
func TestE2EWaitForApproval(t *testing.T) {
	flag.Parse()
	h := &httpClient{base: *toolyardURL}
	mustLogin(t, h)
	token := enrollAgent(t, h, "wait-for-approval")
	c := mcpClient(t, *toolyardURL, token)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	apID := callApprovalRequiredTool(t, c, ctx, "wait-key", "wait-val", "blocking wait test for approval flow")

	// Approve in the background after a short delay so wait_for_approval
	// actually has to wait briefly before the decision lands.
	go func() {
		time.Sleep(800 * time.Millisecond)
		var dummy map[string]any
		h.raw(t, "POST", "/v1/approvals/"+apID+"/decide", map[string]string{"Action": "allowed"}, &dummy)
	}()

	waitReq := mcp.CallToolRequest{}
	waitReq.Params.Name = "tools.wait_for_approval"
	waitReq.Params.Arguments = map[string]any{
		"_reason":         "blocking briefly for the human to tap allow",
		"approval_id":     apID,
		"timeout_seconds": 5,
	}
	start := time.Now()
	waitRes, err := c.CallTool(ctx, waitReq)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed > 4*time.Second {
		t.Errorf("wait_for_approval took %v — should have unblocked when approval landed at ~800ms", elapsed)
	}
	sc, _ := waitRes.StructuredContent.(map[string]any)
	if sc == nil || sc["status"] != "allowed" {
		t.Fatalf("expected wait status=allowed, got %v", sc)
	}
}

// TestE2EPollApprovalsBatch: fire 3 writes in parallel, batch-poll
// them all in one call. Confirms tools.poll_approvals returns one
// snapshot per id and the count tallies.
func TestE2EPollApprovalsBatch(t *testing.T) {
	flag.Parse()
	h := &httpClient{base: *toolyardURL}
	mustLogin(t, h)
	token := enrollAgent(t, h, "batch-poll")
	c := mcpClient(t, *toolyardURL, token)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ids := []string{}
	for i := 0; i < 3; i++ {
		key := "batch-" + string(rune('a'+i))
		ids = append(ids, callApprovalRequiredTool(t, c, ctx, key, "x", "fanning out 3 writes for batch-poll test, key="+key))
	}

	// Approve the middle one.
	var dummy map[string]any
	h.raw(t, "POST", "/v1/approvals/"+ids[1]+"/decide", map[string]string{"Action": "allowed"}, &dummy)

	// Batch poll all 3.
	idArr := make([]any, len(ids))
	for i, id := range ids {
		idArr[i] = id
	}
	pollReq := mcp.CallToolRequest{}
	pollReq.Params.Name = "tools.poll_approvals"
	pollReq.Params.Arguments = map[string]any{
		"_reason":      "batch-checking my fan-out approvals",
		"approval_ids": idArr,
	}
	res, err := c.CallTool(ctx, pollReq)
	if err != nil {
		t.Fatalf("batch poll: %v", err)
	}
	sc, _ := res.StructuredContent.(map[string]any)
	if sc == nil {
		t.Fatalf("missing structured_content; %s", dumpResult(res))
	}
	count, _ := sc["count"].(float64)
	if int(count) != 3 {
		t.Errorf("expected count=3 in batch result, got %v", sc["count"])
	}
	results, _ := sc["results"].([]any)
	statusByID := map[string]string{}
	for _, r := range results {
		row, _ := r.(map[string]any)
		id, _ := row["approval_id"].(string)
		st, _ := row["status"].(string)
		statusByID[id] = st
	}
	if statusByID[ids[1]] != "allowed" {
		t.Errorf("middle id should be allowed, got %v", statusByID[ids[1]])
	}
	if statusByID[ids[0]] != "pending" || statusByID[ids[2]] != "pending" {
		t.Errorf("flanking ids should still be pending, got %v / %v", statusByID[ids[0]], statusByID[ids[2]])
	}

	// Cleanup: deny the remaining two so they don't pile up.
	h.raw(t, "POST", "/v1/approvals/"+ids[0]+"/decide", map[string]string{"Action": "denied"}, &dummy)
	h.raw(t, "POST", "/v1/approvals/"+ids[2]+"/decide", map[string]string{"Action": "denied"}, &dummy)
}

// TestE2EListAndCancelMyPending: an agent fires 2 writes, then queries
// its own pending list, then cancels one. After cancellation the
// pending list shrinks; the cancelled approval no longer shows as
// pending in the dashboard.
func TestE2EListAndCancelMyPending(t *testing.T) {
	flag.Parse()
	h := &httpClient{base: *toolyardURL}
	mustLogin(t, h)
	token := enrollAgent(t, h, "list-cancel")
	c := mcpClient(t, *toolyardURL, token)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	id1 := callApprovalRequiredTool(t, c, ctx, "list-cancel-a", "x", "first of two pendings to test list+cancel")
	id2 := callApprovalRequiredTool(t, c, ctx, "list-cancel-b", "y", "second of two pendings to test list+cancel")

	listReq := mcp.CallToolRequest{}
	listReq.Params.Name = "tools.list_my_pending_approvals"
	listReq.Params.Arguments = map[string]any{
		"_reason": "auditing what I have queued so far",
	}
	listRes, err := c.CallTool(ctx, listReq)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	listSC, _ := listRes.StructuredContent.(map[string]any)
	if listSC == nil {
		t.Fatalf("list missing structured_content")
	}
	count, _ := listSC["count"].(float64)
	if int(count) < 2 {
		t.Errorf("expected ≥2 pendings for this agent, got %v", count)
	}

	// Cancel id1.
	cancelReq := mcp.CallToolRequest{}
	cancelReq.Params.Name = "tools.cancel_my_approval"
	cancelReq.Params.Arguments = map[string]any{
		"_reason":     "no longer needed; user changed their mind",
		"approval_id": id1,
	}
	cancelRes, err := c.CallTool(ctx, cancelReq)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	cancelSC, _ := cancelRes.StructuredContent.(map[string]any)
	if cancelSC == nil || cancelSC["status"] != "cancelled" {
		t.Fatalf("expected cancel status=cancelled, got %v", cancelSC)
	}

	// List again — should show ≥1 (id2) and id1 should be absent.
	listRes2, err := c.CallTool(ctx, listReq)
	if err != nil {
		t.Fatalf("list2: %v", err)
	}
	listSC2, _ := listRes2.StructuredContent.(map[string]any)
	pendings, _ := listSC2["pendings"].([]any)
	for _, p := range pendings {
		row, _ := p.(map[string]any)
		if row["approval_id"] == id1 {
			t.Errorf("cancelled approval %s still appears in pending list", id1)
		}
	}

	// Cleanup id2.
	var dummy map[string]any
	h.raw(t, "POST", "/v1/approvals/"+id2+"/decide", map[string]string{"Action": "denied"}, &dummy)
}

// TestE2EApprovalStats: tools.approval_stats returns the operator's
// recent decision-time histogram and the agent's queue depth without
// needing approval.
func TestE2EApprovalStats(t *testing.T) {
	flag.Parse()
	h := &httpClient{base: *toolyardURL}
	mustLogin(t, h)
	token := enrollAgent(t, h, "stats-test")
	c := mcpClient(t, *toolyardURL, token)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	statsReq := mcp.CallToolRequest{}
	statsReq.Params.Name = "tools.approval_stats"
	statsReq.Params.Arguments = map[string]any{
		"_reason": "checking queue depth at session start",
	}
	res, err := c.CallTool(ctx, statsReq)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	sc, _ := res.StructuredContent.(map[string]any)
	if sc == nil {
		t.Fatalf("stats missing structured_content")
	}
	// We always have these top-level keys.
	if _, ok := sc["min_poll_interval_seconds"]; !ok {
		t.Errorf("stats missing min_poll_interval_seconds")
	}
	if _, ok := sc["window_days"]; !ok {
		t.Errorf("stats missing window_days")
	}
	// Caller is authenticated → these should appear.
	if _, ok := sc["your_pending_count"]; !ok {
		t.Errorf("stats missing your_pending_count")
	}
	if _, ok := sc["your_budget"]; !ok {
		t.Errorf("stats missing your_budget")
	}
}

// TestE2EAgentBudgetExceeded: fire 17 writes from one agent (budget is
// 16). The 17th should bounce with status=agent_pending_budget_exceeded.
//
// Calls run in parallel so the test works regardless of the operator's
// -in-line-wait setting (with 0s the calls return immediately; with a
// non-zero wait each call holds for that long, but 16 in parallel still
// finish quickly).
func TestE2EAgentBudgetExceeded(t *testing.T) {
	flag.Parse()
	h := &httpClient{base: *toolyardURL}
	mustLogin(t, h)
	token := enrollAgent(t, h, "budget-test")
	c := mcpClient(t, *toolyardURL, token)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	type result struct {
		idx int
		id  string
		sc  map[string]any
		err error
	}
	resCh := make(chan result, 16)
	for i := 0; i < 16; i++ {
		go func(i int) {
			req := mcp.CallToolRequest{}
			req.Params.Name = "memory.set"
			req.Params.Arguments = map[string]any{
				"_reason": "filling the per-agent pending budget for the budget-exceeded test",
				"key":     "budget-" + string(rune('a'+i)),
				"value":   "x",
			}
			res, err := c.CallTool(ctx, req)
			r := result{idx: i, err: err}
			if res != nil {
				r.sc, _ = res.StructuredContent.(map[string]any)
				if r.sc != nil {
					r.id, _ = r.sc["approval_id"].(string)
				}
			}
			resCh <- r
		}(i)
	}

	ids := make([]string, 0, 16)
	for i := 0; i < 16; i++ {
		r := <-resCh
		if r.err != nil {
			t.Fatalf("call %d: %v", r.idx, r.err)
		}
		if r.id != "" {
			ids = append(ids, r.id)
		}
	}

	// 17th — should be rejected for budget.
	req17 := mcp.CallToolRequest{}
	req17.Params.Name = "memory.set"
	req17.Params.Arguments = map[string]any{
		"_reason": "this should bounce because the agent already has 16 pendings outstanding",
		"key":     "budget-overflow",
		"value":   "x",
	}
	res17, err := c.CallTool(ctx, req17)
	if err != nil {
		t.Fatalf("17th call: %v", err)
	}
	sc17, _ := res17.StructuredContent.(map[string]any)
	if sc17 == nil || sc17["status"] != "agent_pending_budget_exceeded" {
		t.Errorf("expected budget-exceeded envelope on 17th call, got %v", sc17)
	}

	// Cleanup: deny everything we queued.
	for _, id := range ids {
		var dummy map[string]any
		h.raw(t, "POST", "/v1/approvals/"+id+"/decide", map[string]string{"Action": "denied"}, &dummy)
	}
}
