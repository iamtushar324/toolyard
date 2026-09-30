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
