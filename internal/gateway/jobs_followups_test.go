package gateway

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tusharbhardwaj/toolyard/internal/codemode"
	"github.com/tusharbhardwaj/toolyard/internal/jobs"
)

// jobs.wait never reports a live job as unknown: a released wait (a
// restart) answers the job's last state, and a failed read answers a
// retryable error.
func TestJobsWaitNeverCallsALiveJobUnknown(t *testing.T) {
	f := newJobsFixture(t)
	f.rules.set("alpha", jobs.AsyncSetting{Tools: map[string]jobs.AsyncRule{"get_slow": {Mode: jobs.AsyncAlways}}})
	id := jobIDOf(t, f.call(t, "alpha.get_slow", nil))
	go func() {
		time.Sleep(150 * time.Millisecond)
		f.svc.ReleaseWaiters()
	}()
	w := structured(t, f.call(t, MetaJobsWait, map[string]any{"job_id": id, "timeout_seconds": 30}))
	if w["status"] != jobs.StatusRunning || w["final"] != false || w["timed_out"] != true {
		t.Fatalf("jobs.wait released by a restart = %v", w)
	}
	multi := structured(t, f.call(t, MetaJobsWait, map[string]any{"job_ids": []any{id}, "timeout_seconds": 30}))
	if strings.Contains(asJSON(multi["results"]), `"unknown"`) || multi["timed_out"] != true {
		t.Fatalf("multi jobs.wait released by a restart = %v", multi)
	}

	// The caller's context ended (a script's deadline): the read fails,
	// and the answer says to call again, not that the job is unknown.
	ended, cancel := context.WithCancel(WithAgentID(context.Background(), "ag_1"))
	cancel()
	res, err := f.gw.handleJobsWait()(ended, map[string]any{"job_id": id, "timeout_seconds": 5})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || strings.Contains(textOf(res), "unknown job_id") || structured(t, res)["retryable"] != true {
		t.Fatalf("jobs.wait on an ended context = %s %v", textOf(res), res.StructuredContent)
	}
	// Another agent's id still reads as unknown.
	if other := f.callAs(t, "ag_2", MetaJobsWait, map[string]any{"job_id": id, "timeout_seconds": 1}); !other.IsError || !strings.Contains(textOf(other), "unknown job_id") {
		t.Fatalf("ag_2 waited on ag_1's job: %s", textOf(other))
	}
}

// In `after` mode a caller that gives up before after_ms leaves a job
// that can be found and waited on; the call goes on inside it.
func TestAfterModeKeepsTheJobWhenTheCallerGivesUp(t *testing.T) {
	f := newJobsFixture(t)
	f.rules.set("alpha", jobs.AsyncSetting{Mode: jobs.AsyncAfter, AfterMs: 60000})
	ctx, cancel := context.WithCancel(WithAgentID(context.Background(), "ag_1"))
	defer cancel()
	go func() {
		<-f.slowStarted
		cancel()
	}()
	started := time.Now()
	res, err := f.gw.RouteCall(ctx, "test", "alpha.get_slow", map[string]any{ReasonField: jobsTestReason})
	if err != nil {
		t.Fatal(err)
	}
	if waited := time.Since(started); waited > 10*time.Second {
		t.Fatalf("the handler held on for %s after its caller left", waited)
	}
	id := jobIDOf(t, res)
	select {
	case <-f.slowCancelled:
		t.Fatal("the call stopped with its caller although it became a job")
	case <-time.After(100 * time.Millisecond):
	}
	list, err := f.svc.List(context.Background(), jobs.Filter{AgentID: "ag_1", Status: "active"})
	if err != nil || len(list) != 1 || list[0].ID != id {
		t.Fatalf("active jobs = %+v %v", list, err)
	}
	f.releaseAll()
	if w := f.waitJob(t, id); w["status"] != jobs.StatusSucceeded {
		t.Fatalf("job = %v", w)
	}
}

// In `after` mode a call that cannot become a job (here: the job service
// drains) is an inline call again: it stops at the per-call timeout,
// counted from its start, and when its caller goes away.
func TestAfterModeWithoutAJobKeepsTheInlineLimits(t *testing.T) {
	f := newJobsFixture(t)
	f.rules.set("alpha", jobs.AsyncSetting{Mode: jobs.AsyncAfter, AfterMs: 50})
	f.svc.BeginDrain()
	f.gw.upstreamCallTimeout = 400 * time.Millisecond
	started := time.Now()
	res := f.call(t, "alpha.get_slow", nil)
	if waited := time.Since(started); waited > 10*time.Second {
		t.Fatalf("the call ran %s, past the per-call timeout", waited)
	}
	if !res.IsError || metaTrue(res, "toolyard.job") || !strings.Contains(textOf(res), "per-call time limit") {
		t.Fatalf("call past the per-call timeout = %s", textOf(res))
	}
	select {
	case <-f.slowCancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("the upstream call was not stopped")
	}
	if n := f.jobCount(t); n != 0 {
		t.Fatalf("%d jobs stored while draining", n)
	}
}

func TestAfterModeWithoutAJobStopsWithItsCaller(t *testing.T) {
	f := newJobsFixture(t)
	f.rules.set("alpha", jobs.AsyncSetting{Mode: jobs.AsyncAfter, AfterMs: 50})
	f.svc.BeginDrain()
	ctx, cancel := context.WithCancel(WithAgentID(context.Background(), "ag_1"))
	defer cancel()
	go func() {
		<-f.slowStarted
		time.Sleep(200 * time.Millisecond) // past after_ms
		cancel()
	}()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = f.gw.RouteCall(ctx, "test", "alpha.get_slow", map[string]any{ReasonField: jobsTestReason})
	}()
	select {
	case <-f.slowCancelled:
	case <-time.After(10 * time.Second):
		t.Fatal("the inline call outlived its caller")
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the handler did not return after its caller left")
	}
}

// jobs.start's own _callback_ref names the job's receiver, like
// callback_ref.
func TestJobsStartTakesItsOwnCallbackRef(t *testing.T) {
	f := newJobsFixture(t)
	id := jobIDOf(t, f.call(t, MetaJobsStart, map[string]any{"tool": "alpha.get_fast", "arguments": map[string]any{}, callbackRefArg: "cb_meta"}))
	f.waitJob(t, id)
	if j, ok := f.notes.find(id); !ok || j.CallbackRef != "cb_meta" {
		t.Fatalf("notifier got %+v (%v)", j, ok)
	}
	// callback_ref wins over it.
	id2 := jobIDOf(t, f.call(t, MetaJobsStart, map[string]any{"tool": "alpha.get_fast", "arguments": map[string]any{},
		"callback_ref": "cb_param", callbackRefArg: "cb_meta"}))
	f.waitJob(t, id2)
	if j, ok := f.notes.find(id2); !ok || j.CallbackRef != "cb_param" {
		t.Fatalf("notifier got %+v (%v)", j, ok)
	}
}

// A task-augmented call whose handler panics still gets an answer: the
// same panic guard as a plain tools/call.
func TestMCPTasksRecoverAPanickingTool(t *testing.T) {
	f := newJobsFixture(t)
	f.gw.registerEntry(toolEntry{
		tool: mcp.Tool{Name: "alpha.get_panic", Description: descriptionBanner + "panic",
			InputSchema: mcp.ToolInputSchema{Type: "object", Properties: addMetaProps(map[string]any{})}},
		upstream: "alpha", originalName: "get_panic", reasonField: ReasonField,
		handle: func(context.Context, map[string]any) (*mcp.CallToolResult, error) { panic("boom") },
	})
	c := newTasksServer(t, f, "ag_1", false)
	c.initialize()
	out := c.send("tools/call", map[string]any{"name": "alpha.get_panic", "arguments": map[string]any{ReasonField: jobsTestReason}, "task": map[string]any{}}, false)
	if out["result"] == nil && out["error"] == nil {
		t.Fatalf("no JSON-RPC answer for a panicking tool: %v", out)
	}
}

// A job's outcome reads the result: an error result that says its work
// may have run is outcome_unknown, and one that proves nothing ran says
// so.
func TestJobOutcomeReadsTheResult(t *testing.T) {
	interrupted := mcp.NewToolResultError("script interrupted")
	interrupted.Meta = &mcp.Meta{AdditionalFields: map[string]any{"toolyard.calls": []codemode.LedgerEntry{{Seq: 1, Tool: "alpha.x", Status: codemode.StatusOutcomeUnknown}}}}
	if out := jobOutcome(interrupted, nil); out.Status != jobs.StatusOutcomeUnknown {
		t.Fatalf("a run with an outcome_unknown call = %+v", out)
	}
	structuredUnknown := mcp.NewToolResultError("interrupted")
	structuredUnknown.StructuredContent = map[string]any{"status": "outcome_unknown"}
	if out := jobOutcome(structuredUnknown, nil); out.Status != jobs.StatusOutcomeUnknown {
		t.Fatalf("structured outcome_unknown = %+v", out)
	}
	noCalls := mcp.NewToolResultError("script failed")
	noCalls.Meta = &mcp.Meta{AdditionalFields: map[string]any{"toolyard.calls": []codemode.LedgerEntry{}}}
	if out := jobOutcome(noCalls, nil); out.Status != jobs.StatusFailed || !out.NothingRan {
		t.Fatalf("a run that made no call = %+v", out)
	}
	madeCalls := mcp.NewToolResultError("script failed")
	madeCalls.Meta = &mcp.Meta{AdditionalFields: map[string]any{"toolyard.calls": []codemode.LedgerEntry{{Seq: 1, Tool: "alpha.x", Status: "succeeded"}}}}
	if out := jobOutcome(madeCalls, nil); out.Status != jobs.StatusFailed || out.NothingRan {
		t.Fatalf("a failed run that made a call = %+v", out)
	}
	refused := mcp.NewToolResultError("refused")
	refused.StructuredContent = map[string]any{"status": "job_budget_exceeded", "executed": false}
	if out := jobOutcome(refused, nil); !out.NothingRan {
		t.Fatalf("a refusal = %+v", out)
	}
}
