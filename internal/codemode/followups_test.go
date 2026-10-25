package codemode

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

// funcHooks are CallHooks from two functions; after may be nil.
type funcHooks struct {
	before func(ctx context.Context, c *PendingCall) (*mcp.CallToolResult, bool, error)
	after  func(c PendingCall, out CallOutcome)
}

func (h funcHooks) BeforeCall(ctx context.Context, c *PendingCall) (*mcp.CallToolResult, bool, error) {
	return h.before(ctx, c)
}

func (h funcHooks) AfterCall(_ context.Context, c PendingCall, out CallOutcome) {
	if h.after != nil {
		h.after(c, out)
	}
}

// TestCallIsNotSentAfterTheScriptStopped (B2 P1-A): a call whose hook let
// it go only after another call stopped the script (a diverged one) is
// not sent; the hook's context ends as soon as the script stops, not at
// the clock.
func TestCallIsNotSentAfterTheScriptStopped(t *testing.T) {
	f := newJSFake()
	rt := inProcess(f, Limits{ScriptTimeout: 20 * time.Second})
	hooks := funcHooks{before: func(ctx context.Context, c *PendingCall) (*mcp.CallToolResult, bool, error) {
		if c.Seq == 2 {
			return nil, false, &CallError{Kind: ErrorScriptDiverged, Message: "call 2 took another path"}
		}
		// Call 1 waits for the run to stop, then lets the call go anyway:
		// the runtime must still not send it.
		<-ctx.Done()
		return nil, false, nil
	}}
	start := time.Now()
	rr := mustRun(t, rt, `await tools.parallel([() => tools.demo.facts({}), () => tools.demo.read({ q: "x" })], { limit: 2 });
return 1;`, RunOptions{Language: LanguageJavaScript, Hooks: hooks})
	if rr.Status != StatusFailed || rr.ErrorKind != ErrorScriptDiverged {
		t.Fatalf("run = %+v", rr)
	}
	if n := len(f.seen()); n != 0 {
		t.Fatalf("RouteCall reached %d times after the script stopped: %+v", n, f.seen())
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("the waiting hook was released after %s, at the clock, not when the script stopped", took)
	}
	for _, c := range rr.Calls {
		if c.Sent {
			t.Fatalf("call %d is recorded as sent: %+v", c.Seq, rr.Calls)
		}
	}
}

// TestHeldBesideARunningCallIsOutcomeUnknown (B2 P2-7): a run held while
// another of its calls was still running cannot be resumed safely; it
// reports outcome_unknown and names both calls.
func TestHeldBesideARunningCallIsOutcomeUnknown(t *testing.T) {
	s := &session{record: true, deferred: map[int]*callStop{}}
	held := &HeldCall{Seq: 1, Tool: "demo.write", Status: StatusPermissionRequired}
	s.records = []*callState{
		{rec: CallRecord{Seq: 1, Tool: "demo.write", Status: StatusPermissionRequired}, sent: true, done: true},
		{rec: CallRecord{Seq: 2, Tool: "demo.post"}, sent: true},
	}
	s.stop = &callStop{status: StatusPermissionRequired, held: held}
	rr := s.result(scriptResult{})
	if rr.Status != StatusOutcomeUnknown || rr.ErrorKind != ErrorOutcomeUnknown || rr.Held != nil ||
		!strings.Contains(rr.ErrorMessage, "call 1 (demo.write) held the run") || !strings.Contains(rr.ErrorMessage, "call 2 (demo.post) was still running") {
		t.Fatalf("result = %+v", rr)
	}
	if rr.Calls[1].Status != StatusOutcomeUnknown || !rr.Calls[1].Sent {
		t.Fatalf("calls = %+v", rr.Calls)
	}
	// Held with nothing running is still held.
	s = &session{record: true, deferred: map[int]*callStop{}}
	s.records = []*callState{{rec: CallRecord{Seq: 1, Tool: "demo.write", Status: StatusPermissionRequired}, sent: true, done: true}}
	s.stop = &callStop{status: StatusPermissionRequired, held: held}
	if rr := s.result(scriptResult{}); rr.Status != StatusPermissionRequired || rr.Held == nil {
		t.Fatalf("held alone = %+v", rr)
	}
}

// TestFramesAfterTheFinalOneAreIgnored (B2 P2-4): once the engine reported
// its outcome, a call frame (written through qjs:std here) is not acted
// on, and the first outcome stands.
func TestFramesAfterTheFinalOneAreIgnored(t *testing.T) {
	f := newJSFake()
	rt := inProcess(f, Limits{ScriptTimeout: 20 * time.Second})
	rr := mustRun(t, rt, `const std = await import("qjs:std");
std.out.puts('{"t":"done","has":true,"result":1}\n');
std.out.flush();
try { await tools.demo.read({ q: "after" }); } catch (e) {}
return 2;`, RunOptions{Language: LanguageJavaScript})
	if rr.Status != StatusSucceeded || string(rr.Result) != "1" {
		t.Fatalf("run = %+v (result %s)", rr, rr.Result)
	}
	if n := len(f.seen()); n != 0 {
		t.Fatalf("a call after the final frame reached RouteCall: %+v", f.seen())
	}
}

// TestEngineFrameLimitAbortIsClear (B2 P2-8): wazero's call-frame limit,
// which deep recursion inside the engine's C code reaches before QuickJS's
// own check, reads as a limit the script cannot catch.
func TestEngineFrameLimitAbortIsClear(t *testing.T) {
	h := &jsHost{ids: map[int]int{}}
	res := h.outcome(context.Background(), errors.New("wasm error: stack overflow\nwasm stack trace:\n\tqjs.JS_CallInternal"), "", "", 88)
	if res.cause != ErrorLimit || !strings.Contains(res.errMsg, "cannot be caught with try/catch") || !strings.Contains(res.errMsg, "nested JSON") {
		t.Fatalf("outcome = %+v", res)
	}
	// Deeply nested JSON: QuickJS parses it, or catches the depth (an
	// error about the stack), or the engine stops with the message above;
	// never a crash, and the engine still runs the next script.
	rt := inProcess(newJSFake(), Limits{ScriptTimeout: 20 * time.Second})
	out := rt.Execute(context.Background(), `return JSON.parse("[".repeat(200000) + "]".repeat(200000)).length;`, ExecOptions{Language: LanguageJavaScript})
	t.Logf("deep JSON: failed=%v\n%.600s", out.Failed, out.Text)
	if out.Failed && !(strings.Contains(out.Text, "stack") || strings.Contains(out.Text, "nested deeper")) {
		t.Fatalf("deep JSON:\n%.1500s", out.Text)
	}
	if out := rt.Execute(context.Background(), "return 2;", ExecOptions{Language: LanguageJavaScript}); out.Failed {
		t.Fatalf("after the deep JSON:\n%s", out.Text)
	}
}

// TestLimiterBoundsTheQueue (B2 P2-3): a caller may have twice its own cap
// waiting, all callers four times the global cap (16 at least); past that
// a script is refused busy at once.
func TestLimiterBoundsTheQueue(t *testing.T) {
	l := newLimiter(1, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rel, err := l.acquire(ctx, "a", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = l.acquire(ctx, "a", 30*time.Second)
		}()
	}
	waitFor(t, func() bool { l.mu.Lock(); defer l.mu.Unlock(); return l.waitingBy["a"] == 2 })
	start := time.Now()
	if _, err := l.acquire(ctx, "a", 30*time.Second); !errors.Is(err, ErrBusy) || !strings.Contains(err.Error(), "already has 2 scripts waiting") {
		t.Fatalf("third waiter of a: %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("the refused waiter waited")
	}
	// Other callers queue up to the total bound (16 here).
	for i := range 14 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = l.acquire(ctx, string(rune('b'+i)), 30*time.Second)
		}(i)
	}
	waitFor(t, func() bool { l.mu.Lock(); defer l.mu.Unlock(); return len(l.queue) == 16 })
	if _, err := l.acquire(ctx, "z", 30*time.Second); !errors.Is(err, ErrBusy) || !strings.Contains(err.Error(), "16 scripts are already waiting") {
		t.Fatalf("waiter past the total bound: %v", err)
	}
	cancel()
	wg.Wait()
	rel()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.queue) != 0 || len(l.waitingBy) != 0 || l.running != 0 {
		t.Fatalf("left behind: queue %d, waiting %v, running %d", len(l.queue), l.waitingBy, l.running)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in 5s")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestCarryScriptMark: a job context that does not derive from a nested
// call's keeps the script mark when it is carried over.
func TestCarryScriptMark(t *testing.T) {
	call := WithinScript(context.Background())
	job := context.Background()
	if IsWithinScript(job) || !IsWithinScript(CarryScriptMark(job, call)) {
		t.Fatal("the mark was not carried")
	}
	if IsWithinScript(CarryScriptMark(job, context.Background())) {
		t.Fatal("an unmarked source marked the job")
	}
	if !IsWithinScript(context.WithoutCancel(call)) {
		t.Fatal("WithoutCancel dropped the mark")
	}
}

// TestSizeLimitedCallRecordsFailed (B2 P2-5): a result over the per-call
// limit is a failed call with kind limit, and AfterCall gets the text
// the script saw, so a resume can give it the same failure.
func TestSizeLimitedCallRecordsFailed(t *testing.T) {
	f := newJSFake()
	f.handlers["demo.big"] = func(map[string]any) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText(strings.Repeat("z", 7<<20)), nil
	}
	rt := inProcess(f, Limits{ScriptTimeout: 20 * time.Second})
	var mu sync.Mutex
	var outs []CallOutcome
	hooks := funcHooks{
		before: func(context.Context, *PendingCall) (*mcp.CallToolResult, bool, error) { return nil, false, nil },
		after: func(_ PendingCall, out CallOutcome) {
			mu.Lock()
			outs = append(outs, out)
			mu.Unlock()
		},
	}
	rr := mustRun(t, rt, "r = try_call(demo.big)\nresult = r[\"ok\"]\n", RunOptions{Hooks: hooks})
	if rr.Status != StatusSucceeded || string(rr.Result) != "false" || len(rr.Calls) != 1 || rr.Calls[0].Status != StatusFailed {
		t.Fatalf("run = %+v calls %+v", rr, rr.Calls)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(outs) != 1 || outs[0].Kind != ErrorLimit || !strings.Contains(outs[0].FailureText, "more than code mode passes to a script") ||
		outs[0].Text != "" || !strings.HasSuffix(outs[0].Message, outs[0].FailureText) {
		t.Fatalf("after = %+v", outs)
	}
}

// TestReplayedFailureAndHold: BeforeCall answers from a record with
// ReplayFailure and ReplayHold. The script can catch both, as before;
// uncaught, the failure keeps its kind and the hold holds the run with
// its draft.
func TestReplayedFailureAndHold(t *testing.T) {
	f := newJSFake()
	rt := inProcess(f, Limits{ScriptTimeout: 20 * time.Second})
	draft := json.RawMessage(`{"status":"permission_required","draft":{"title":"Write x"}}`)
	hooks := funcHooks{before: func(_ context.Context, c *PendingCall) (*mcp.CallToolResult, bool, error) {
		switch c.Tool {
		case "demo.big":
			return ReplayFailure(ErrorLimit, "the result is too big"), true, nil
		case "demo.facts":
			return ReplayFailure(ErrorToolError, "upstream said 502, outcome unknown to it"), true, nil
		case "demo.write":
			return ReplayHold(StatusPermissionRequired, "demo.write is restricted", draft), true, nil
		}
		return nil, false, nil
	}}
	rr := mustRun(t, rt, "a = try_call(demo.facts)\nb = try_call(demo.write, x=1)\nresult = [a[\"ok\"], a[\"error\"], b[\"status\"]]\n", RunOptions{Hooks: hooks})
	if rr.Status != StatusSucceeded || !strings.Contains(string(rr.Result), "tool call failed for demo.facts: upstream said 502") ||
		!strings.Contains(string(rr.Result), `"permission_required"`) {
		t.Fatalf("caught = %+v (result %s)", rr, rr.Result)
	}
	if len(rr.Calls) != 2 || rr.Calls[0].Status != StatusFailed || !rr.Calls[0].Replayed || rr.Calls[1].Status != StatusPermissionRequired || !rr.Calls[1].Replayed {
		t.Fatalf("calls = %+v", rr.Calls)
	}
	if n := len(f.seen()); n != 0 {
		t.Fatalf("a replayed call reached RouteCall: %+v", f.seen())
	}
	rr = mustRun(t, rt, "result = demo.big()\n", RunOptions{Hooks: hooks})
	if rr.Status != StatusFailed || rr.ErrorKind != ErrorLimit || !strings.Contains(rr.ErrorMessage, "the result is too big") {
		t.Fatalf("uncaught failure = %+v", rr)
	}
	rr = mustRun(t, rt, "result = demo.write(x=1)\n", RunOptions{Hooks: hooks})
	if rr.Status != StatusPermissionRequired || rr.Held == nil || rr.Held.Seq != 1 || string(rr.Held.Draft) != `{"title":"Write x"}` {
		t.Fatalf("uncaught hold = %+v held %+v", rr, rr.Held)
	}
}
