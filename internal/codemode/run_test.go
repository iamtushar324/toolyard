package codemode

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

const upsertTarget = "BkCoreServices.upsert_bifrost_virtual_key_actor"

// recHooks records what Run's hooks saw; before, when set, decides each
// call.
type recHooks struct {
	before func(c *PendingCall) (*mcp.CallToolResult, bool, error)

	mu         sync.Mutex
	seen       []PendingCall
	afterCalls []PendingCall
	after      []CallOutcome
}

func (h *recHooks) BeforeCall(_ context.Context, c *PendingCall) (*mcp.CallToolResult, bool, error) {
	h.mu.Lock()
	h.seen = append(h.seen, *c)
	h.mu.Unlock()
	if h.before != nil {
		return h.before(c)
	}
	return nil, false, nil
}

func (h *recHooks) AfterCall(_ context.Context, c PendingCall, out CallOutcome) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.afterCalls = append(h.afterCalls, c)
	h.after = append(h.after, out)
}

func (h *recHooks) outcomes() []CallOutcome {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]CallOutcome(nil), h.after...)
}

func mustRun(t *testing.T, rt *Runtime, code string, opts RunOptions) *RunResult {
	t.Helper()
	rr, err := rt.Run(context.Background(), code, opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rr == nil {
		t.Fatal("Run returned no result")
	}
	return rr
}

func callStatuses(rr *RunResult) []string {
	out := make([]string, len(rr.Calls))
	for i, c := range rr.Calls {
		out[i] = c.Status
	}
	return out
}

func TestRunSucceedsWithJSONResult(t *testing.T) {
	f := newFake()
	bothWays(t, f, Limits{}, func(t *testing.T, rt *Runtime) {
		f.reset()
		hooks := &recHooks{}
		code := "print(\"hello\")\nc = BkCoreServices.get_client(clientId=\"vai-us\")\nresult = {\"id\": c[\"data\"][\"id\"], \"n\": 2}"
		rr := mustRun(t, rt, code, RunOptions{Reason: "look up one client for the run test", Hooks: hooks})
		if rr.Status != StatusSucceeded || rr.ErrorKind != "" || rr.ErrorMessage != "" || rr.Held != nil {
			t.Fatalf("run = %+v", rr)
		}
		var got map[string]any
		if err := json.Unmarshal(rr.Result, &got); err != nil || got["id"] != "vai-us" || got["n"] != float64(2) {
			t.Fatalf("result = %s (%v)", rr.Result, err)
		}
		// Logs are print() lines only, not the [TOOL] trace.
		if !reflect.DeepEqual(rr.Logs, []string{"hello"}) {
			t.Fatalf("logs = %q", rr.Logs)
		}
		if len(rr.Calls) != 1 {
			t.Fatalf("calls = %+v", rr.Calls)
		}
		c := rr.Calls[0]
		if c.Seq != 1 || c.Tool != "BkCoreServices.get_client" || c.Target != "BkCoreServices.get_client" ||
			c.Status != StatusSucceeded || c.Replayed || c.StartedAt == 0 || c.FinishedAt < c.StartedAt {
			t.Fatalf("call record = %+v", c)
		}
		if seen := f.seen(); len(seen) != 1 || seen[0].args["_reason"] != "look up one client for the run test" || seen[0].via != Via {
			t.Fatalf("route calls = %+v", seen)
		}
		outs := hooks.outcomes()
		if len(outs) != 1 || outs[0].Record.Status != StatusSucceeded || outs[0].Message != "" ||
			!strings.Contains(outs[0].Text, `"amz_seller_id":"A1B2"`) || outs[0].Result == nil {
			t.Fatalf("after-call outcomes = %+v", outs)
		}
		if len(hooks.seen) != 1 || hooks.seen[0].ScriptArgs["clientId"] != "vai-us" || hooks.seen[0].ScriptArgs["_reason"] != nil {
			t.Fatalf("pending call = %+v", hooks.seen)
		}
		if n := rt.slotsInUse(); n != 0 {
			t.Fatalf("%d slots still held", n)
		}

		// No result and no calls is still a success.
		rr = mustRun(t, rt, "x = 1", RunOptions{})
		if rr.Status != StatusSucceeded || rr.Result != nil || len(rr.Calls) != 0 {
			t.Fatalf("empty run = %+v", rr)
		}
	})
}

func TestRunErrorKinds(t *testing.T) {
	f := newFake()
	bothWays(t, f, Limits{}, func(t *testing.T, rt *Runtime) {
		cases := []struct{ name, code, kind, want string }{
			{"syntax", "def broken(:\n  pass", ErrorScriptError, "got ':', want ')'"},
			{"runtime", "x = {}\nresult = x[\"nope\"]", ErrorScriptError, `key "nope" not in dict`},
			{"result too big", "result = \"x\" * (2 << 20)", ErrorLimit, "a returned value may be at most 1 MiB"},
		}
		for _, c := range cases {
			rr := mustRun(t, rt, c.code, RunOptions{})
			if rr.Status != StatusFailed || rr.ErrorKind != c.kind || !strings.Contains(rr.ErrorMessage, c.want) || rr.Result != nil {
				t.Errorf("%s: %+v", c.name, rr)
			}
		}
	})

	ctx := context.Background()
	// A tool error.
	f.handlers["BkCoreServices.get_client"] = func(map[string]any) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultError("upstream said no: client not found"), nil
	}
	rt := inProcess(f, Limits{})
	rr := mustRun(t, rt, "print(\"before\")\nresult = BkCoreServices.get_client(clientId=\"x\")\nprint(\"after\")", RunOptions{})
	if rr.Status != StatusFailed || rr.ErrorKind != ErrorToolError || !strings.Contains(rr.ErrorMessage, "upstream said no: client not found") ||
		!reflect.DeepEqual(callStatuses(rr), []string{StatusFailed}) || !reflect.DeepEqual(rr.Logs, []string{"before"}) {
		t.Fatalf("tool error: %+v", rr)
	}

	// The call budget, the step limit and the wall clock.
	rt = inProcess(newFake(), Limits{MaxCalls: 3})
	rr = mustRun(t, rt, "for i in range(10):\n  BkCoreServices.get_all_clients()\nresult = 1", RunOptions{})
	if rr.Status != StatusFailed || rr.ErrorKind != ErrorLimit || len(rr.Calls) != 3 {
		t.Fatalf("call limit: %+v", rr)
	}
	rt = inProcess(newFake(), Limits{MaxSteps: 10_000})
	rr = mustRun(t, rt, "i = 0\nwhile True:\n  i += 1\nresult = i", RunOptions{})
	if rr.Status != StatusFailed || rr.ErrorKind != ErrorLimit || !strings.Contains(rr.ErrorMessage, "too many steps") {
		t.Fatalf("step limit: %+v", rr)
	}
	slow := newFake()
	slow.handlers["BkCoreServices.get-all-clients"] = func(map[string]any) (*mcp.CallToolResult, error) {
		time.Sleep(50 * time.Millisecond)
		return jsonResult([]any{})
	}
	rt = inProcess(slow, Limits{ScriptTimeout: 120 * time.Millisecond})
	rr, err := rt.Run(ctx, "for i in range(100):\n  BkCoreServices.get_all_clients()\nresult = 1", RunOptions{})
	if err != nil || rr.Status != StatusFailed || rr.ErrorKind != ErrorTimeout || !strings.Contains(rr.ErrorMessage, "script stopped") {
		t.Fatalf("timeout: %+v %v", rr, err)
	}
	for _, c := range rr.Calls {
		if c.Status != StatusSucceeded {
			t.Fatalf("a call that finished is not outcome_unknown: %+v", rr.Calls)
		}
	}

	// Empty code runs nothing.
	rr = mustRun(t, rt, "  \n ", RunOptions{})
	if rr.Status != StatusFailed || rr.ErrorKind != ErrorScriptError {
		t.Fatalf("empty code: %+v", rr)
	}
}

func TestRunPermissionRequiredKeepsDraft(t *testing.T) {
	f := newFake()
	draft := map[string]any{"title": "<title>", "tools": []any{map[string]any{"call_id": "call_1", "tool": upsertTarget, "params": map[string]any{"email": "a@b"}}}}
	f.handlers[upsertTarget] = func(map[string]any) (*mcp.CallToolResult, error) {
		res := mcp.NewToolResultError(upsertTarget + " is restricted. Nothing ran and nothing was sent to your owner.")
		res.StructuredContent = map[string]any{"status": "permission_required", "executed": false, "tool": upsertTarget, "draft": draft}
		res.Meta = &mcp.Meta{AdditionalFields: map[string]any{"toolyard.permission_required": true}}
		return res, nil
	}
	bothWays(t, f, Limits{}, func(t *testing.T, rt *Runtime) {
		f.reset()
		code := "c = BkCoreServices.get_client(clientId=\"vai-us\")\nr = BkCoreServices.upsert_bifrost_virtual_key_actor(email=\"a@b\", userId=\"u\", virtualKeyHash=\"h\")\nprint(\"continued\")\nresult = r"
		rr := mustRun(t, rt, code, RunOptions{})
		if rr.Status != StatusPermissionRequired || rr.ErrorKind != "" || rr.ErrorMessage != "" || rr.Result != nil || len(rr.Logs) != 0 {
			t.Fatalf("run = %+v", rr)
		}
		if !reflect.DeepEqual(callStatuses(rr), []string{StatusSucceeded, StatusPermissionRequired}) {
			t.Fatalf("call statuses = %v", callStatuses(rr))
		}
		h := rr.Held
		if h == nil || h.Seq != 2 || h.Tool != upsertTarget || h.Target != upsertTarget || h.Status != StatusPermissionRequired ||
			h.ApprovalID != "" || !strings.Contains(h.Message, "is restricted") || h.Structured == nil {
			t.Fatalf("held = %+v", h)
		}
		var got map[string]any
		if err := json.Unmarshal(h.Draft, &got); err != nil {
			t.Fatalf("draft %s: %v", h.Draft, err)
		}
		want, _ := json.Marshal(draft)
		var wantMap map[string]any
		_ = json.Unmarshal(want, &wantMap)
		if !reflect.DeepEqual(got, wantMap) {
			t.Fatalf("draft = %s, want %s", h.Draft, want)
		}
		if n := len(f.seen()); n != 2 {
			t.Fatalf("RouteCall reached %d times, want 2", n)
		}
	})
}

func TestRunDeferredKeepsApprovalID(t *testing.T) {
	for _, marker := range []string{"toolyard.deferred", "toolyard.executing"} {
		f := newFake()
		f.handlers[upsertTarget] = func(map[string]any) (*mcp.CallToolResult, error) {
			res := mcp.NewToolResultText("Tool execution requires human approval. Your call has been queued.\napproval_id: ap_123")
			res.StructuredContent = map[string]any{"status": "pending_approval", "approval_id": "ap_123", "tool": upsertTarget}
			res.Meta = &mcp.Meta{AdditionalFields: map[string]any{marker: true}}
			return res, nil
		}
		rt := inProcess(f, Limits{})
		rr := mustRun(t, rt, "r = BkCoreServices.upsert_bifrost_virtual_key_actor(email=\"a@b\", userId=\"u\", virtualKeyHash=\"h\")\nprint(\"continued\")", RunOptions{})
		if rr.Status != StatusDeferred || rr.ErrorKind != "" || len(rr.Logs) != 0 {
			t.Fatalf("%s: run = %+v", marker, rr)
		}
		h := rr.Held
		if h == nil || h.Seq != 1 || h.Status != StatusDeferred || h.ApprovalID != "ap_123" || h.Draft != nil || !strings.Contains(h.Message, "approval_id: ap_123") {
			t.Fatalf("%s: held = %+v", marker, h)
		}
		if !reflect.DeepEqual(callStatuses(rr), []string{StatusDeferred}) {
			t.Fatalf("%s: call statuses = %v", marker, callStatuses(rr))
		}
	}
}

func TestRunReplayIsNotSent(t *testing.T) {
	f := newFake()
	bothWays(t, f, Limits{}, func(t *testing.T, rt *Runtime) {
		f.reset()
		hooks := &recHooks{before: func(c *PendingCall) (*mcp.CallToolResult, bool, error) {
			if c.Seq == 1 {
				return mcp.NewToolResultText(`{"data": {"id": "from-record"}}`), true, nil
			}
			return nil, false, nil
		}}
		code := "a = BkCoreServices.get_client(clientId=\"vai-us\")\nb = BkCoreServices.get_all_clients()\nresult = [a[\"data\"][\"id\"], len(b)]"
		rr := mustRun(t, rt, code, RunOptions{Hooks: hooks})
		if rr.Status != StatusSucceeded {
			t.Fatalf("run = %+v", rr)
		}
		var got []any
		if err := json.Unmarshal(rr.Result, &got); err != nil || !reflect.DeepEqual(got, []any{"from-record", float64(2)}) {
			t.Fatalf("result = %s (%v)", rr.Result, err)
		}
		seen := f.seen()
		if len(seen) != 1 || seen[0].target != "BkCoreServices.get-all-clients" {
			t.Fatalf("the replayed call reached RouteCall: %+v", seen)
		}
		if len(rr.Calls) != 2 || !rr.Calls[0].Replayed || rr.Calls[0].Status != StatusSucceeded || rr.Calls[1].Replayed ||
			rr.Calls[1].Tool != "BkCoreServices.get-all-clients" || rr.Calls[1].Seq != 2 {
			t.Fatalf("calls = %+v", rr.Calls)
		}
		// AfterCall sees only the call the gateway answered.
		outs := hooks.outcomes()
		if len(outs) != 1 || outs[0].Record.Seq != 2 || outs[0].Record.Replayed {
			t.Fatalf("after-call outcomes = %+v", outs)
		}
		// Replaying the recorded Text gives the script the same value.
		rr2 := mustRun(t, rt, "b = BkCoreServices.get_all_clients()\nresult = len(b)", RunOptions{Hooks: &recHooks{
			before: func(*PendingCall) (*mcp.CallToolResult, bool, error) {
				return mcp.NewToolResultText(outs[0].Text), true, nil
			}}})
		if rr2.Status != StatusSucceeded || string(rr2.Result) != "2" || len(f.seen()) != 1 {
			t.Fatalf("replay of recorded text: %+v", rr2)
		}
	})
}

func TestRunHookAddsGrant(t *testing.T) {
	f := newFake()
	bothWays(t, f, Limits{}, func(t *testing.T, rt *Runtime) {
		f.reset()
		hooks := &recHooks{before: func(c *PendingCall) (*mcp.CallToolResult, bool, error) {
			c.Args["_grant"] = "gt_test_token"
			return nil, false, nil
		}}
		rr := mustRun(t, rt, "result = BkCoreServices.upsert_bifrost_virtual_key_actor(email=\"a@b\", userId=\"u\", virtualKeyHash=\"h\")", RunOptions{Hooks: hooks})
		if rr.Status != StatusSucceeded {
			t.Fatalf("run = %+v", rr)
		}
		seen := f.seen()
		if len(seen) != 1 || seen[0].args["_grant"] != "gt_test_token" || seen[0].args["email"] != "a@b" {
			t.Fatalf("route args = %+v", seen)
		}
		sa := hooks.seen[0].ScriptArgs
		if sa["email"] != "a@b" || sa["_grant"] != nil || sa["_reason"] != nil {
			t.Fatalf("script args = %v", sa)
		}

		// A grant the script passes itself is sent, and never recorded.
		f.reset()
		hooks = &recHooks{}
		rr = mustRun(t, rt, "result = BkCoreServices.upsert_bifrost_virtual_key_actor(email=\"a@b\", userId=\"u\", virtualKeyHash=\"h\", _grant=\"gt_from_script\")", RunOptions{Hooks: hooks})
		if rr.Status != StatusSucceeded || f.seen()[0].args["_grant"] != "gt_from_script" {
			t.Fatalf("script grant: %+v %+v", rr, f.seen())
		}
		if _, ok := hooks.seen[0].ScriptArgs["_grant"]; ok {
			t.Fatalf("script args keep the grant: %v", hooks.seen[0].ScriptArgs)
		}
	})
}

func TestRunHookErrorStopsRun(t *testing.T) {
	f := newFake()
	code := "a = BkCoreServices.get_client(clientId=\"vai-us\")\nb = BkCoreServices.get_all_clients()\nprint(\"after\")\nresult = 1"
	cases := []struct {
		name         string
		err          error
		status, kind string
	}{
		{"diverged", &CallError{Kind: ErrorScriptDiverged, Message: "call 2 is BkCoreServices.get-all-clients but the record has BkCoreServices.get_client"}, StatusFailed, ErrorScriptDiverged},
		{"grant", &CallError{Kind: ErrorGrantInvalid, Message: "the approved request has no grant for this call"}, StatusFailed, ErrorGrantInvalid},
		{"recorded unknown", &CallError{Kind: ErrorOutcomeUnknown, Message: "call 2 has an unknown outcome in the record"}, StatusOutcomeUnknown, ErrorOutcomeUnknown},
		{"plain error", errors.New("record store unavailable"), StatusFailed, ErrorInternal},
	}
	bothWays(t, f, Limits{}, func(t *testing.T, rt *Runtime) {
		for _, c := range cases {
			f.reset()
			hooks := &recHooks{before: func(pc *PendingCall) (*mcp.CallToolResult, bool, error) {
				if pc.Seq == 2 {
					return nil, false, c.err
				}
				return nil, false, nil
			}}
			rr := mustRun(t, rt, code, RunOptions{Hooks: hooks})
			if rr.Status != c.status || rr.ErrorKind != c.kind || !strings.Contains(rr.ErrorMessage, c.err.Error()) || len(rr.Logs) != 0 {
				t.Errorf("%s: run = %+v", c.name, rr)
				continue
			}
			wantCall := StatusFailed
			if c.status == StatusOutcomeUnknown {
				wantCall = StatusOutcomeUnknown
			}
			if !reflect.DeepEqual(callStatuses(rr), []string{StatusSucceeded, wantCall}) {
				t.Errorf("%s: call statuses = %v", c.name, callStatuses(rr))
			}
			if n := len(f.seen()); n != 1 {
				t.Errorf("%s: RouteCall reached %d times, want 1", c.name, n)
			}
			if outs := hooks.outcomes(); len(outs) != 1 || outs[0].Record.Seq != 1 {
				t.Errorf("%s: after-call outcomes = %+v", c.name, outs)
			}
		}
	})
}

func TestRunBusy(t *testing.T) {
	ctx := context.Background()
	rt := inProcess(newFake(), Limits{})
	rt.SetWorker(Worker{MaxConcurrent: 1, AcquireWait: 30 * time.Millisecond})
	first, err := rt.AcquireSlot(ctx)
	if err != nil {
		t.Fatalf("first slot: %v", err)
	}
	if rr, err := rt.Run(ctx, "result = 1", RunOptions{}); !errors.Is(err, ErrBusy) || rr != nil {
		t.Fatalf("busy run = %+v, %v", rr, err)
	}
	if _, err := rt.AcquireSlot(ctx); !errors.Is(err, ErrBusy) {
		t.Fatalf("second slot: %v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := rt.Run(cancelled, "result = 1", RunOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled wait: %v", err)
	}
	first.Release()
	first.Release() // harmless
	if rr := mustRun(t, rt, "result = 1", RunOptions{}); rr.Status != StatusSucceeded {
		t.Fatalf("after release: %+v", rr)
	}
	// A slot taken first is used and released by Run.
	slot, err := rt.AcquireSlot(ctx)
	if err != nil {
		t.Fatalf("slot: %v", err)
	}
	if rr := mustRun(t, rt, "result = 2", RunOptions{Slot: slot}); rr.Status != StatusSucceeded || string(rr.Result) != "2" {
		t.Fatalf("run on a taken slot: %+v", rr)
	}
	if n := rt.slotsInUse(); n != 0 {
		t.Fatalf("%d slots still held", n)
	}
}

func TestRunOutcomeUnknown(t *testing.T) {
	snapshot := func(map[string]any) (*mcp.CallToolResult, error) {
		payload := map[string]any{"approval_id": "ap_1", "status": "outcome_unknown", "execution_state": "outcome_unknown"}
		res, _ := jsonResult(payload)
		res.StructuredContent = payload
		return res, nil
	}
	cases := []struct {
		name         string
		extra        string
		handler      func(map[string]any) (*mcp.CallToolResult, error)
		status, kind string
		callStatus   string
	}{
		{"audit outcome unknown", "", func(map[string]any) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultError("Tool outcome unknown: completion audit failed. Do not retry. Review dispatch dispatch_1 before another action."), nil
		}, StatusOutcomeUnknown, ErrorOutcomeUnknown, StatusOutcomeUnknown},
		{"marker", "", func(map[string]any) (*mcp.CallToolResult, error) {
			res := mcp.NewToolResultError("tool failed: connection reset by peer")
			res.Meta = &mcp.Meta{AdditionalFields: map[string]any{OutcomeUnknownMarker: true}}
			return res, nil
		}, StatusOutcomeUnknown, ErrorOutcomeUnknown, StatusOutcomeUnknown},
		{"route error", "", func(map[string]any) (*mcp.CallToolResult, error) {
			return nil, errors.New("transport broke")
		}, StatusOutcomeUnknown, ErrorOutcomeUnknown, StatusOutcomeUnknown},
		{"legacy resume of an unknown approval", `, _approval_id="ap_1"`, snapshot, StatusOutcomeUnknown, ErrorOutcomeUnknown, StatusOutcomeUnknown},
		{"grant invalid", "", func(map[string]any) (*mcp.CallToolResult, error) {
			res := mcp.NewToolResultError(upsertTarget + ": grant expired. Nothing ran.")
			res.Meta = &mcp.Meta{AdditionalFields: map[string]any{"toolyard.grant_invalid": true}}
			return res, nil
		}, StatusFailed, ErrorGrantInvalid, StatusFailed},
		{"plain tool error", "", func(map[string]any) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultError("validation failed: email is malformed"), nil
		}, StatusFailed, ErrorToolError, StatusFailed},
	}
	for _, c := range cases {
		f := newFake()
		f.handlers[upsertTarget] = c.handler
		rt := inProcess(f, Limits{})
		code := "r = BkCoreServices.upsert_bifrost_virtual_key_actor(email=\"a@b\", userId=\"u\", virtualKeyHash=\"h\"" + c.extra + ")\nprint(\"after\")\nresult = 1"
		rr := mustRun(t, rt, code, RunOptions{})
		if rr.Status != c.status || rr.ErrorKind != c.kind || rr.ErrorMessage == "" || len(rr.Logs) != 0 {
			t.Errorf("%s: run = %+v", c.name, rr)
			continue
		}
		if !reflect.DeepEqual(callStatuses(rr), []string{c.callStatus}) {
			t.Errorf("%s: call statuses = %v", c.name, callStatuses(rr))
		}
	}

	// The same snapshot read as data (a poll, no _approval_id) is a
	// successful call.
	f := newFake()
	f.handlers[upsertTarget] = snapshot
	rr := mustRun(t, inProcess(f, Limits{}), "r = BkCoreServices.upsert_bifrost_virtual_key_actor(email=\"a@b\", userId=\"u\", virtualKeyHash=\"h\")\nresult = r[\"status\"]", RunOptions{})
	if rr.Status != StatusSucceeded || string(rr.Result) != `"outcome_unknown"` {
		t.Fatalf("snapshot as data: %+v", rr)
	}

	// The text output of executeToolCode is unchanged by the classification.
	f = newFake()
	f.handlers[upsertTarget] = cases[0].handler
	text, failed := inProcess(f, Limits{}).ExecuteToolCode(context.Background(), "r = BkCoreServices.upsert_bifrost_virtual_key_actor(email=\"a@b\", userId=\"u\", virtualKeyHash=\"h\")", "")
	if !failed || !strings.Contains(text, "tool call failed for BkCoreServices.upsert_bifrost_virtual_key_actor: Tool outcome unknown: completion audit failed.") {
		t.Fatalf("executeToolCode text:\n%s", text)
	}
}

// ctxCaller is a fakeCaller whose get-all-clients waits for the run's
// context, as an upstream call cut short by the clock does, then fails
// with the context's error.
type ctxCaller struct{ *fakeCaller }

func (c ctxCaller) RouteCall(ctx context.Context, via, target string, args map[string]any) (*mcp.CallToolResult, error) {
	if target != "BkCoreServices.get-all-clients" {
		return c.fakeCaller.RouteCall(ctx, via, target, args)
	}
	<-ctx.Done()
	return mcp.NewToolResultErrorFromErr("tool failed", ctx.Err()), nil
}

func TestRunCallCutByClockIsOutcomeUnknown(t *testing.T) {
	check := func(t *testing.T, rt *Runtime) {
		rr := mustRun(t, rt, "a = BkCoreServices.get_client(clientId=\"vai-us\")\nb = BkCoreServices.get_all_clients()\nresult = 1", RunOptions{Hooks: &recHooks{}})
		if rr.Status != StatusOutcomeUnknown || rr.ErrorKind != ErrorTimeout {
			t.Fatalf("run = %+v", rr)
		}
		if !reflect.DeepEqual(callStatuses(rr), []string{StatusSucceeded, StatusOutcomeUnknown}) {
			t.Fatalf("call statuses = %v", callStatuses(rr))
		}
		if n := rt.slotsInUse(); n != 0 {
			t.Fatalf("%d slots still held", n)
		}
	}
	t.Run("in-process", func(t *testing.T) {
		check(t, inProcess(ctxCaller{newFake()}, Limits{ScriptTimeout: 150 * time.Millisecond}))
	})
	t.Run("worker", func(t *testing.T) {
		check(t, workerRuntime(ctxCaller{newFake()}, Limits{ScriptTimeout: 2 * time.Second}, nil))
	})
}

func TestStoppedSessionRefusesMoreCalls(t *testing.T) {
	// A worker that ignores a held call's error and keeps calling (or
	// reports success) gets nothing sent and changes nothing.
	f := newFake()
	f.handlers[upsertTarget] = func(map[string]any) (*mcp.CallToolResult, error) {
		res := mcp.NewToolResultText("queued\napproval_id: ap_9")
		res.StructuredContent = map[string]any{"approval_id": "ap_9"}
		res.Meta = &mcp.Meta{AdditionalFields: map[string]any{"toolyard.deferred": true}}
		return res, nil
	}
	rt := inProcess(f, Limits{})
	s := &session{r: rt, ctx: context.Background(), reason: "a reason long enough for the gate", cat: buildCatalog(f.tools), record: true}
	if r := s.handleCall("BkCoreServices", "upsert_bifrost_virtual_key_actor", map[string]any{"email": "a@b"}); r.Error == "" {
		t.Fatalf("held call reply = %+v", r)
	}
	if r := s.handleCall("BkCoreServices", "get_client", map[string]any{"clientId": "x"}); !strings.Contains(r.Error, "the script already stopped") {
		t.Fatalf("call after the stop = %+v", r)
	}
	if n := len(f.seen()); n != 1 {
		t.Fatalf("RouteCall reached %d times, want 1", n)
	}
	rr := s.result(scriptResult{result: json.RawMessage(`1`), hasResult: true})
	if rr.Status != StatusDeferred || rr.Result != nil || rr.Held == nil || rr.Held.ApprovalID != "ap_9" || len(rr.Calls) != 1 {
		t.Fatalf("result = %+v", rr)
	}
	// A sealed session sends nothing either.
	if r := s.handleCall("BkCoreServices", "get_client", map[string]any{"clientId": "y"}); r.Error == "" || len(f.seen()) != 1 {
		t.Fatalf("call after the seal = %+v", r)
	}
}
