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
// same panic guard as a plain tools/call. A panic in a call answered
// without a job (here an inline jobs.* tool) is recorded outcome_unknown:
// it may have run.
func TestMCPTasksRecoverAPanickingTool(t *testing.T) {
	f := newJobsFixture(t)
	f.gw.registerEntry(toolEntry{
		tool: mcp.Tool{Name: "jobs.get_boom", Description: descriptionBanner + "panic",
			InputSchema: mcp.ToolInputSchema{Type: "object", Properties: addMetaProps(map[string]any{})}},
		upstream: jobsUpstream, originalName: "get_boom", reasonField: ReasonField,
		handle: func(context.Context, map[string]any) (*mcp.CallToolResult, error) { panic("boom") },
	})
	c := newTasksServer(t, f, "ag_1", false)
	c.initialize()
	out := c.result("tools/call", map[string]any{"name": "jobs.get_boom", "arguments": map[string]any{ReasonField: jobsTestReason}, "task": map[string]any{}})
	task, _ := out["task"].(map[string]any)
	id, _ := task["taskId"].(string)
	if id == "" {
		t.Fatalf("no task for a panicking tool: %v", out)
	}
	if j, err := f.svc.Get(context.Background(), id); err != nil || j.Status != jobs.StatusOutcomeUnknown {
		t.Fatalf("recorded answer of a panicking tool = %+v %v", j, err)
	}
}

// A retried POST /v1/jobs with the same Idempotency-Key gets the job its
// first request started, even though that job used the grant up.
func TestIdempotentRetryWithAGrantReturnsTheJob(t *testing.T) {
	f := newJobsFixture(t, withInbox)
	tok := f.grantFor(t)
	args := func() map[string]any {
		return map[string]any{"service": "api", "env": "prod", GrantField: tok}
	}
	ctx := context.Background()
	first, res, err := f.gw.StartJob(ctx, "ag_1", "deploy.run", args(), jobsTestReason, "", "deploy-api-1")
	if err != nil || first == nil {
		t.Fatalf("first request = %+v %s %v", first, textOf(res), err)
	}
	if w := f.waitJob(t, first.ID); w["status"] != jobs.StatusSucceeded {
		t.Fatalf("first job = %v", w)
	}
	again, res, err := f.gw.StartJob(ctx, "ag_1", "deploy.run", args(), jobsTestReason, "", "deploy-api-1")
	if err != nil || again == nil || again.ID != first.ID {
		t.Fatalf("retried request = %+v %s %v, want job %s", again, textOf(res), err, first.ID)
	}
	if f.deployCalls.Load() != 1 {
		t.Fatalf("deploy ran %d times", f.deployCalls.Load())
	}
	// A new key with the used grant is refused at once and stores no job.
	jobsBefore := f.jobCount(t)
	if j, res, err := f.gw.StartJob(ctx, "ag_1", "deploy.run", args(), jobsTestReason, "", "deploy-api-2"); err != nil || j != nil ||
		structured(t, res)["status"] != "grant_invalid" {
		t.Fatalf("new key with a used grant = %+v %s %v", j, textOf(res), err)
	}
	if n := f.jobCount(t); n != jobsBefore {
		t.Fatalf("a used grant stored %d jobs", n-jobsBefore)
	}
}

// Past the recorded-answer cap, a task call answered at once gets no task:
// a toolyard refusal (nothing ran) is a JSON-RPC error, and an answer
// that may have had an effect is the plain tool result.
func TestTasksPastTheRecordedCap(t *testing.T) {
	f := newJobsFixture(t, func(c *jobsFixtureConfig) {
		c.jobOpts = func(o *jobs.Options) { o.MaxRecordedPerAgent, o.MaxActive = 1, 1 }
	})
	c := newTasksServer(t, f, "ag_1", false)
	c.initialize()
	call := func(name string) map[string]any {
		return c.send("tools/call", map[string]any{"name": name, "arguments": map[string]any{ReasonField: jobsTestReason}, "task": map[string]any{}}, false)
	}
	// The only active job slot is taken: task calls are refused with
	// job_budget_exceeded (nothing ran).
	if out := call("alpha.get_slow"); out["result"] == nil {
		t.Fatalf("slow task = %v", out)
	}
	if out := call("alpha.get_fast"); out["result"] == nil {
		t.Fatalf("first refused task (recorded) = %v", out)
	}
	if out := call("alpha.get_fast"); out["error"] == nil || !strings.Contains(asJSON(out["error"]), "no task was stored") {
		t.Fatalf("refusal past the cap = %v", out)
	}
	out := call(MetaJobsList)
	res, _ := out["result"].(map[string]any)
	if res == nil || res["task"] != nil || res["content"] == nil {
		t.Fatalf("inline answer past the cap = %v", out)
	}
}

// jobs.wait takes one of the agent's wait slots only when it will block.
func TestJobsWaitOnFinishedJobsTakesNoSlot(t *testing.T) {
	f := newJobsFixture(t, func(c *jobsFixtureConfig) {
		c.jobOpts = func(o *jobs.Options) { o.MaxWaitersPerAgent = 1 }
	})
	f.rules.set("alpha", jobs.AsyncSetting{Tools: map[string]jobs.AsyncRule{"get_slow": {Mode: jobs.AsyncAlways}}})
	done := jobIDOf(t, f.call(t, MetaJobsStart, map[string]any{"tool": "alpha.get_fast", "arguments": map[string]any{}}))
	f.waitJob(t, done)
	running := jobIDOf(t, f.call(t, "alpha.get_slow", nil))
	release, err := f.svc.BeginWait("ag_1") // the agent's only slot
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if w := f.call(t, MetaJobsWait, map[string]any{"job_id": done, "timeout_seconds": 5}); w.IsError || structured(t, w)["final"] != true {
		t.Fatalf("jobs.wait on a finished job without a free slot = %s", textOf(w))
	}
	if w := f.call(t, MetaJobsWait, map[string]any{"job_id": running, "timeout_seconds": 5}); !w.IsError || !strings.Contains(textOf(w), "too many waits") {
		t.Fatalf("jobs.wait on a running job without a free slot = %s", textOf(w))
	}
}

// A job's outcome reads the result: an error result that says its work
// may have run is outcome_unknown, and one that proves nothing ran says
// so: a code-mode run that sent no call, or one of toolyard's own
// refusals. An upstream's structured content proves nothing.
func TestJobOutcomeReadsTheResult(t *testing.T) {
	ledger := []codemode.LedgerEntry{{Seq: 1, Tool: "alpha.x", Status: codemode.StatusFailed}}
	interrupted := mcp.NewToolResultError("script interrupted")
	interrupted.Meta = &mcp.Meta{AdditionalFields: map[string]any{"toolyard.calls": ledger, "toolyard.status": codemode.StatusOutcomeUnknown, "toolyard.sent_calls": 1}}
	if out := jobOutcome(interrupted, nil); out.Status != jobs.StatusOutcomeUnknown {
		t.Fatalf("an interrupted run that sent a call = %+v", out)
	}
	unknownCall := mcp.NewToolResultError("script failed")
	unknownCall.Meta = &mcp.Meta{AdditionalFields: map[string]any{"toolyard.calls": []codemode.LedgerEntry{{Seq: 1, Tool: "alpha.x", Status: codemode.StatusOutcomeUnknown}}}}
	if out := jobOutcome(unknownCall, nil); out.Status != jobs.StatusOutcomeUnknown {
		t.Fatalf("a run with an outcome_unknown call = %+v", out)
	}
	structuredUnknown := mcp.NewToolResultError("interrupted")
	structuredUnknown.StructuredContent = map[string]any{"status": "outcome_unknown"}
	if out := jobOutcome(structuredUnknown, nil); out.Status != jobs.StatusOutcomeUnknown {
		t.Fatalf("structured outcome_unknown = %+v", out)
	}
	// A call the script asked for but toolyard refused before sending is
	// in the ledger, not in the sent count: nothing ran.
	refusedOnly := mcp.NewToolResultError("script failed")
	refusedOnly.Meta = &mcp.Meta{AdditionalFields: map[string]any{"toolyard.calls": ledger, "toolyard.status": codemode.StatusFailed, "toolyard.sent_calls": 0}}
	if out := jobOutcome(refusedOnly, nil); out.Status != jobs.StatusFailed || !out.NothingRan {
		t.Fatalf("a run that sent no call = %+v", out)
	}
	sent := mcp.NewToolResultError("script failed")
	sent.Meta = &mcp.Meta{AdditionalFields: map[string]any{"toolyard.calls": ledger, "toolyard.status": codemode.StatusFailed, "toolyard.sent_calls": 1}}
	if out := jobOutcome(sent, nil); out.Status != jobs.StatusFailed || out.NothingRan {
		t.Fatalf("a failed run that sent a call = %+v", out)
	}
	// The same through codeModeResult, as executeToolCode answers.
	if out := jobOutcome(codeModeResult(codemode.ExecResult{Text: "stopped", Failed: true, Status: codemode.StatusFailed,
		ErrorKind: codemode.ErrorInterrupted, Calls: ledger, SentCalls: 0}, "cm_1"), nil); !out.NothingRan {
		t.Fatalf("executeToolCode result with no sent call = %+v", out)
	}
	refused := mcp.NewToolResultError("refused")
	refused.StructuredContent = map[string]any{"status": "job_budget_exceeded", "executed": false}
	if out := jobOutcome(refused, nil); !out.NothingRan {
		t.Fatalf("a toolyard refusal = %+v", out)
	}
	upstream := mcp.NewToolResultError("the upstream says so")
	upstream.StructuredContent = map[string]any{"status": "rejected", "executed": false}
	if out := jobOutcome(upstream, nil); out.NothingRan {
		t.Fatalf("an upstream's executed: false was trusted: %+v", out)
	}
}
