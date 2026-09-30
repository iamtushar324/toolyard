package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tusharbhardwaj/toolyard/internal/approval"
	"github.com/tusharbhardwaj/toolyard/internal/metrics"
)

// raiseExecuted has the caller on ctx run t.run under an auto-approve
// rule, so the approval is allowed and its result ("ran") is persisted
// before the call returns, and returns the approval id.
func raiseExecuted(t *testing.T, f *actorFixture, ctx context.Context) string {
	t.Helper()
	f.bus.SetAutoApprover(fakeAuto{rule: "rule_1", creator: "u_creator"})
	res := f.call(t, ctx, "test", "t.run", map[string]any{"env": "prod"})
	if res.IsError || !strings.Contains(textOf(res), "ran") {
		t.Fatalf("setup: t.run did not execute: %q", textOf(res))
	}
	id := f.lastMetric(t, "t.run").ApprovalID
	if id == "" {
		t.Fatal("setup: no approval id on the t.run metric")
	}
	return id
}

func (f *actorFixture) runCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.raisers)
}

// Agent B re-calling agent A's tool with A's approval id must get what an
// unknown id gets, not A's cached result. An anonymous caller is not A
// either.
func TestResumeRefusesAnotherAgentsApproval(t *testing.T) {
	f := newActorFixture(t)
	ctxA := WithAgentID(context.Background(), "ag_a")
	id := raiseExecuted(t, f, ctxA)
	runs := f.runCount()

	unknown := fmt.Sprintf("unknown approval %q", id)
	for _, c := range []struct {
		name string
		ctx  context.Context
	}{
		{"other agent", WithAgentID(context.Background(), "ag_b")},
		{"anonymous", context.Background()},
	} {
		res := f.call(t, c.ctx, "test", "t.run", map[string]any{ApprovalIDField: id})
		if !res.IsError || textOf(res) != unknown {
			t.Fatalf("%s: got %q (error %v), want the unknown-approval error %q", c.name, textOf(res), res.IsError, unknown)
		}
		if res.StructuredContent != nil {
			t.Fatalf("%s: structured content leaked: %v", c.name, res.StructuredContent)
		}
		m := f.lastMetric(t, "t.run")
		if m.Outcome != metrics.OutcomeError || m.ErrorClass != "approval" || m.ApprovalID != id {
			t.Fatalf("%s: metric %+v, want outcome error / class approval / approval id %s", c.name, m, id)
		}
		if m.Fingerprint != "" || m.ReasonText != "" || m.ApprovalOutcome != "" {
			t.Fatalf("%s: metric carries A's approval details: %+v", c.name, m)
		}
	}
	if f.runCount() != runs {
		t.Fatal("a refused resume ran the tool")
	}

	// A still gets its own result back.
	res := f.call(t, ctxA, "test", "t.run", map[string]any{ApprovalIDField: id})
	if res.IsError || textOf(res) != "ran" {
		t.Fatalf("owner resume: got %q (error %v)", textOf(res), res.IsError)
	}
}

// An approval id only resumes the tool it was raised for: re-calling a
// different tool, or the same name on another upstream, with it reads as
// an unknown id.
func TestResumeRefusesApprovalForAnotherTool(t *testing.T) {
	f := newActorFixture(t)
	ctxA := WithAgentID(context.Background(), "ag_a")
	id := raiseExecuted(t, f, ctxA)

	t.Run("other tool", func(t *testing.T) {
		res := f.call(t, ctxA, "test", "t.get_status", map[string]any{ApprovalIDField: id})
		if want := fmt.Sprintf("unknown approval %q", id); !res.IsError || textOf(res) != want {
			t.Fatalf("got %q (error %v), want %q", textOf(res), res.IsError, want)
		}
		if m := f.lastMetric(t, "t.get_status"); m.Outcome != metrics.OutcomeError || m.ErrorClass != "approval" || m.ApprovalID != id {
			t.Fatalf("metric %+v", m)
		}
	})

	t.Run("same tool name on another upstream", func(t *testing.T) {
		other, err := f.bus.Hold(context.Background(), approval.NewRequest{
			AgentID: "ag_a", UpstreamName: "other", ToolName: "t.run",
			Arguments: map[string]any{"env": "prod"}, Reason: testReason, RequireHuman: true,
		}, 0)
		if err != nil {
			t.Fatal(err)
		}
		res := f.call(t, ctxA, "test", "t.run", map[string]any{ApprovalIDField: other.ID})
		if want := fmt.Sprintf("unknown approval %q", other.ID); !res.IsError || textOf(res) != want {
			t.Fatalf("got %q (error %v), want %q", textOf(res), res.IsError, want)
		}
	})
}

// A row with no agent_id (raised by an anonymous caller) has no owner to
// check, so any caller may resume it, as on /v1/agents/approvals/{id}.
func TestResumeAllowsOwnerlessApproval(t *testing.T) {
	f := newActorFixture(t)
	id := raiseExecuted(t, f, context.Background())
	res := f.call(t, WithAgentID(context.Background(), "ag_b"), "test", "t.run", map[string]any{ApprovalIDField: id})
	if res.IsError || textOf(res) != "ran" {
		t.Fatalf("ownerless resume: got %q (error %v)", textOf(res), res.IsError)
	}
}

func decodeResult(t *testing.T, res *mcp.CallToolResult) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(textOf(res)), &out); err != nil {
		t.Fatalf("decode %q: %v", textOf(res), err)
	}
	return out
}

// The polling and waiting tools answer another agent's approval id (and an
// anonymous caller asking for an agent's id) exactly as they answer an id
// that doesn't exist; the owner still gets the executed result.
func TestApprovalPollersHideOtherAgentsApprovals(t *testing.T) {
	f := newActorFixture(t)
	ctxA := WithAgentID(context.Background(), "ag_a")
	id := raiseExecuted(t, f, ctxA)
	const missing = "ap_00000000-0000-0000-0000-000000000000"

	single := map[string]map[string]any{
		MetaPollApproval:    {},
		MetaWaitForApproval: {"timeout_seconds": float64(1)},
	}
	many := map[string]map[string]any{
		MetaPollApprovals:    {},
		MetaWaitForApprovals: {"timeout_seconds": float64(1)},
	}
	with := func(base map[string]any, k string, v any) map[string]any {
		out := map[string]any{k: v}
		for bk, bv := range base {
			out[bk] = bv
		}
		return out
	}

	for _, c := range []struct {
		name string
		ctx  context.Context
	}{
		{"other agent", WithAgentID(context.Background(), "ag_b")},
		{"anonymous", context.Background()},
	} {
		for tool, args := range single {
			t.Run(c.name+" "+tool, func(t *testing.T) {
				want := f.call(t, c.ctx, "test", tool, with(args, "approval_id", missing))
				got := f.call(t, c.ctx, "test", tool, with(args, "approval_id", id))
				if !want.IsError || !got.IsError || textOf(got) != textOf(want) {
					t.Fatalf("got %q (error %v), want the unknown-id answer %q", textOf(got), got.IsError, textOf(want))
				}
			})
		}
		for tool, args := range many {
			t.Run(c.name+" "+tool, func(t *testing.T) {
				got := decodeResult(t, f.call(t, c.ctx, "test", tool, with(args, "approval_ids", []any{id, missing})))
				results, _ := got["results"].([]any)
				if len(results) != 2 {
					t.Fatalf("results %v", got["results"])
				}
				mine, theirs := results[0].(map[string]any), results[1].(map[string]any)
				if len(mine) != 2 || mine["approval_id"] != id || mine["status"] != theirs["status"] || theirs["status"] != "unknown" {
					t.Fatalf("got %v for another agent's id, want the unknown-id answer %v", mine, theirs)
				}
			})
		}
	}

	// The owner sees the executed result through every one of them.
	for tool, args := range single {
		got := decodeResult(t, f.call(t, ctxA, "test", tool, with(args, "approval_id", id)))
		if got["status"] != "executed" || !strings.Contains(fmt.Sprint(got["result"]), "ran") {
			t.Fatalf("owner %s: %v", tool, got)
		}
	}
	for tool, args := range many {
		got := decodeResult(t, f.call(t, ctxA, "test", tool, with(args, "approval_ids", []any{id})))
		results, _ := got["results"].([]any)
		if len(results) != 1 || results[0].(map[string]any)["status"] != "executed" {
			t.Fatalf("owner %s: %v", tool, got)
		}
	}
}

// A top-level _approval_id on tools.execute resumes against the real
// target: the owner gets the result whether or not the call names the
// target, the forwarded call keeps the caller's identity, and anyone else,
// or a wrong target, gets the unknown-approval answer.
func TestResumeThroughExecute(t *testing.T) {
	f := newActorFixture(t)
	ctxA := WithAgentID(context.Background(), "ag_a")
	id := raiseExecuted(t, f, ctxA)
	runs := f.runCount()
	unknown := fmt.Sprintf("unknown approval %q", id)

	for _, c := range []struct {
		name string
		args map[string]any
	}{
		{"owner names the target", map[string]any{"tool": "t.run", ApprovalIDField: id}},
		{"owner names only the id", map[string]any{ApprovalIDField: id}},
	} {
		t.Run(c.name, func(t *testing.T) {
			res := f.call(t, ctxA, "test", MetaExecuteTool, c.args)
			if res.IsError || textOf(res) != "ran" {
				t.Fatalf("got %q (error %v), want the cached result", textOf(res), res.IsError)
			}
			if m := f.lastMetric(t, "t.run"); m.AgentID != "ag_a" || m.Via != MetaExecuteTool || m.ApprovalID != id {
				t.Fatalf("forwarded resume metric %+v, want agent ag_a via %s for %s", m, MetaExecuteTool, id)
			}
		})
	}

	for _, c := range []struct {
		name string
		ctx  context.Context
		args map[string]any
	}{
		{"owner names another target", ctxA, map[string]any{"tool": "t.get_status", ApprovalIDField: id}},
		{"other agent names the target", WithAgentID(context.Background(), "ag_b"), map[string]any{"tool": "t.run", ApprovalIDField: id}},
		{"other agent names only the id", WithAgentID(context.Background(), "ag_b"), map[string]any{ApprovalIDField: id}},
		{"anonymous names only the id", context.Background(), map[string]any{ApprovalIDField: id}},
	} {
		t.Run(c.name, func(t *testing.T) {
			res := f.call(t, c.ctx, "test", MetaExecuteTool, c.args)
			if !res.IsError || textOf(res) != unknown {
				t.Fatalf("got %q (error %v), want %q", textOf(res), res.IsError, unknown)
			}
		})
	}
	if f.runCount() != runs {
		t.Fatal("a resume through tools.execute ran the tool")
	}

	// An approval raised for tools.execute itself (an operator gated it)
	// still resumes on tools.execute, for its owner only.
	t.Run("approval raised for tools.execute", func(t *testing.T) {
		held, err := f.bus.Hold(context.Background(), approval.NewRequest{
			AgentID: "ag_a", UpstreamName: "tools", ToolName: MetaExecuteTool,
			Arguments: map[string]any{"tool": "t.run"}, Reason: testReason, RequireHuman: true,
		}, 0)
		if err != nil {
			t.Fatal(err)
		}
		res := f.call(t, ctxA, "test", MetaExecuteTool, map[string]any{ApprovalIDField: held.ID})
		sc, _ := res.StructuredContent.(map[string]any)
		if res.IsError || sc["status"] != "pending_approval" || sc["approval_id"] != held.ID {
			t.Fatalf("owner: got %q (error %v, structured %v), want the pending envelope", textOf(res), res.IsError, sc)
		}
		res = f.call(t, WithAgentID(context.Background(), "ag_b"), "test", MetaExecuteTool, map[string]any{ApprovalIDField: held.ID})
		if want := fmt.Sprintf("unknown approval %q", held.ID); !res.IsError || textOf(res) != want {
			t.Fatalf("other agent: got %q (error %v), want %q", textOf(res), res.IsError, want)
		}
	})
}

// A resume through tools.execute runs the target's access check: a member
// whose approval is on a server it can no longer reach is told the tool
// doesn't exist, as on a direct call.
func TestResumeThroughExecuteChecksTargetAccess(t *testing.T) {
	f := newAccessFixture(t, nil)
	hold := func(up string) string {
		t.Helper()
		req, err := f.bus.Hold(context.Background(), approval.NewRequest{
			AgentID: memberID, UpstreamName: up, ToolName: up + ".run",
			Arguments: map[string]any{}, Reason: testReason, RequireHuman: true,
		}, 0)
		if err != nil {
			t.Fatal(err)
		}
		return req.ID
	}
	beta := hold("beta")
	for _, args := range []map[string]any{
		{ApprovalIDField: beta},
		{"tool": "beta.run", ApprovalIDField: beta},
	} {
		res := f.call(t, f.member, MetaExecuteTool, args)
		if !res.IsError || textOf(res) != notFoundText("beta.run") {
			t.Fatalf("ungranted target %v: got %q (error %v)", args, textOf(res), res.IsError)
		}
	}
	alpha := hold("alpha")
	res := f.call(t, f.member, MetaExecuteTool, map[string]any{ApprovalIDField: alpha})
	if sc, _ := res.StructuredContent.(map[string]any); res.IsError || sc["approval_id"] != alpha {
		t.Fatalf("granted target: got %q (error %v)", textOf(res), res.IsError)
	}
	if f.calls.Load() != 0 {
		t.Fatal("no handler should have run")
	}
}

// The approval coordination tools read approvals by their own arguments,
// so a stray _approval_id next to them is ignored rather than treated as a
// resume of the coordination call. Their own owner check still applies.
func TestApprovalToolsIgnoreStrayApprovalIDField(t *testing.T) {
	f := newActorFixture(t)
	ctxA := WithAgentID(context.Background(), "ag_a")
	ctxB := WithAgentID(context.Background(), "ag_b")
	id := raiseExecuted(t, f, ctxA)

	for _, tool := range []string{MetaPollApproval, MetaWaitForApproval} {
		t.Run(tool, func(t *testing.T) {
			args := map[string]any{"approval_id": id, ApprovalIDField: id, "timeout_seconds": float64(1)}
			got := decodeResult(t, f.call(t, ctxA, "test", tool, args))
			if got["status"] != "executed" || !strings.Contains(fmt.Sprint(got["result"]), "ran") {
				t.Fatalf("owner: %v", got)
			}
			if res := f.call(t, ctxB, "test", tool, args); !res.IsError || textOf(res) != "unknown approval_id" {
				t.Fatalf("other agent: got %q (error %v)", textOf(res), res.IsError)
			}
		})
	}
	for _, tool := range []string{MetaPollApprovals, MetaWaitForApprovals} {
		t.Run(tool, func(t *testing.T) {
			args := map[string]any{"approval_ids": []any{id}, ApprovalIDField: id, "timeout_seconds": float64(1)}
			results, _ := decodeResult(t, f.call(t, ctxA, "test", tool, args))["results"].([]any)
			if len(results) != 1 || results[0].(map[string]any)["status"] != "executed" {
				t.Fatalf("owner: %v", results)
			}
		})
	}
	for _, tool := range []string{MetaListPendingApprovals, MetaCancelMyApproval, MetaApprovalStats} {
		t.Run(tool, func(t *testing.T) {
			res := f.call(t, ctxA, "test", tool, map[string]any{"approval_id": id, ApprovalIDField: id})
			if res.IsError {
				t.Fatalf("got error %q", textOf(res))
			}
		})
	}
}
