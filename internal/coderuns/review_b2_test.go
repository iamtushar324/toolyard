package coderuns

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tusharbhardwaj/toolyard/internal/codemode"
	"github.com/tusharbhardwaj/toolyard/internal/inbox"
)

// TestUnsentCallNeverReplacesARecordThatRan (B2 review P1-1): a call that
// never reached the gateway does not overwrite the record of one that did
// (a deferred call's approval id, a held call's draft, a failed call's
// error), only another unsent one; a sent call replaces any record.
func TestUnsentCallNeverReplacesARecordThatRan(t *testing.T) {
	e := newEnv(t)
	r := e.start(t, "ag_a", readScript, "")
	ctx := context.Background()
	ran := []Call{
		{Seq: 11, Tool: "demo.write", Target: "demo.write", ArgsHash: "h1", Status: codemode.StatusDeferred, sent: true, approvalID: "ap_1", errorText: "waiting for ap_1"},
		{Seq: 12, Tool: "demo.write", Target: "demo.write", ArgsHash: "h2", Status: codemode.StatusPermissionRequired, sent: true, hold: `{"draft":{"title":"w"}}`, errorText: "restricted"},
		{Seq: 13, Tool: "demo.write", Target: "demo.write", ArgsHash: "h3", Status: codemode.StatusFailed, sent: true, errorKind: codemode.ErrorToolError, errorText: "boom"},
		{Seq: 14, Tool: "demo.write", Target: "demo.write", ArgsHash: "h4", Status: codemode.StatusFailed},
	}
	for _, c := range ran {
		if err := upsertCall(ctx, e.db, r.ID, c); err != nil {
			t.Fatal(err)
		}
	}
	for seq := 11; seq <= 14; seq++ {
		if err := upsertCall(ctx, e.db, r.ID, Call{Seq: seq, Tool: "demo.read", Target: "demo.read", ArgsHash: "other", Status: codemode.StatusFailed}); err != nil {
			t.Fatal(err)
		}
	}
	got := map[int]Call{}
	calls, err := e.svc.loadCalls(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range calls {
		got[c.Seq] = c
	}
	if c := got[11]; c.Status != codemode.StatusDeferred || c.approvalID != "ap_1" || !c.sent || c.Tool != "demo.write" {
		t.Errorf("deferred record = %+v", c)
	}
	if c := got[12]; c.Status != codemode.StatusPermissionRequired || c.hold != `{"draft":{"title":"w"}}` || !c.sent {
		t.Errorf("held record = %+v", c)
	}
	if c := got[13]; c.Status != codemode.StatusFailed || c.errorText != "boom" || !c.sent || c.Tool != "demo.write" {
		t.Errorf("failed record = %+v", c)
	}
	if c := got[14]; c.Tool != "demo.read" || c.ArgsHash != "other" {
		t.Errorf("an unsent record was not replaced: %+v", c)
	}
	if err := upsertCall(ctx, e.db, r.ID, Call{Seq: 11, Tool: "demo.write", Target: "demo.write", ArgsHash: "h1", Status: codemode.StatusSucceeded, sent: true, replayText: `{"ok": true}`}); err != nil {
		t.Fatal(err)
	}
	if calls, _ = e.svc.loadCalls(ctx, r.ID); calls[1].Seq != 11 || calls[1].Status != codemode.StatusSucceeded || calls[1].replayText != `{"ok": true}` {
		t.Errorf("a sent call did not replace the record: %+v", calls[1])
	}
}

// TestConcurrentResumesRunOnce (B2 review P1-2): of two resumes that read a
// run at the same time, only the first to claim it runs, even when it ends
// held again with the same status before the second one claims; the
// second starts nothing and says so.
func TestConcurrentResumesRunOnce(t *testing.T) {
	fg := &fakeGrants{views: map[string]inbox.AgentView{}}
	rg := &requestGrants{fakeGrants: fg, reqs: map[string]*inbox.Request{}}
	e, f := newEnvWith(t, nil, rg)
	ctx := context.Background()
	held := e.start(t, "ag_a", readWriteScript, "")
	if held.Status != codemode.StatusPermissionRequired || held.Held == nil || held.Held.Seq != 2 {
		t.Fatalf("held = %+v", held)
	}
	// The request's grant covers no call, so a resume holds again with the
	// same status.
	fg.set("rq_1", inbox.AgentToolView{CallID: "call_1", Tool: "demo.write", Decision: "allowed", Grant: "tok-w"})
	rg.reqs["rq_1"] = &inbox.Request{ID: "rq_1", AgentID: "ag_a", Tools: []inbox.ToolRequest{
		{CallID: "call_1", Tool: "demo.write", Params: constraints(t, `{"x": {"eq": 99}}`)},
	}}
	loaded, proceed := make(chan struct{}), make(chan struct{})
	var first atomic.Bool
	e.svc.beforeClaim = func() {
		if first.CompareAndSwap(false, true) {
			close(loaded)
			<-proceed
		}
	}
	errA := make(chan error, 1)
	go func() {
		_, err := e.svc.Resume(ctx, ResumeRequest{AgentID: "ag_a", RunID: held.ID, RequestID: "rq_1"})
		errA <- err
	}()
	<-loaded
	// The second resume runs to its end while the first waits between its
	// read and its claim.
	b := e.resume(t, "ag_a", held.ID, "rq_1")
	if b.Status != codemode.StatusPermissionRequired || b.Held == nil || b.Held.Seq != 2 {
		t.Fatalf("second resume = %+v", b)
	}
	close(proceed)
	if err := <-errA; !errors.Is(err, ErrResumeConflict) {
		t.Fatalf("the first resume claimed a run another resume had claimed since: %v", err)
	}
	if n := len(f.sent("demo.write")); n != 2 {
		t.Fatalf("demo.write sent %d times, want 2 (the run, and one resume)", n)
	}
	if n := e.count(t, `SELECT resume_gen FROM code_runs WHERE id = ?`, held.ID); n != 1 {
		t.Fatalf("resume_gen = %d, want 1", n)
	}
}

// TestResumeSendsACaughtGrantInvalidCallWithANewGrant (B2 review P2-4): a
// call whose grant the gateway refused (caught by the script) did not run;
// a later resume sends it with a grant of its request that covers it,
// instead of replaying the refusal forever.
func TestResumeSendsACaughtGrantInvalidCallWithANewGrant(t *testing.T) {
	fg := &fakeGrants{views: map[string]inbox.AgentView{}}
	rg := &requestGrants{fakeGrants: fg, reqs: map[string]*inbox.Request{}}
	e, f := newEnvWith(t, nil, rg)
	f.setHandler("demo.write", func(args map[string]any) (*mcp.CallToolResult, error) {
		x, _ := args["x"].(float64)
		switch {
		case x == 1 && args[grantArg] == "good1", x == 2 && args[grantArg] == "good2":
			return mcp.NewToolResultText(`{"ok": true}`), nil
		case args[grantArg] == "bad":
			res := mcp.NewToolResultError("the grant was already used")
			res.Meta = &mcp.Meta{AdditionalFields: map[string]any{"toolyard.grant_invalid": true}}
			return res, nil
		}
		return permissionRequired("demo.write"), nil
	})
	script := "r = try_call(demo.write, x=1)\nw = demo.write(x=2)\nresult = {\"first\": r[\"ok\"], \"w\": w[\"ok\"]}\n"
	held := e.start(t, "ag_a", script, "")
	if held.Status != codemode.StatusPermissionRequired || held.Held == nil || held.Held.Seq != 2 {
		t.Fatalf("held = %+v", held)
	}
	fg.set("rq_1", inbox.AgentToolView{CallID: "call_1", Tool: "demo.write", Decision: "allowed", Grant: "bad"})
	rg.reqs["rq_1"] = &inbox.Request{ID: "rq_1", AgentID: "ag_a", Tools: []inbox.ToolRequest{
		{CallID: "call_1", Tool: "demo.write", Params: constraints(t, `{"x": {"eq": 1}}`)},
	}}
	first := e.resume(t, "ag_a", held.ID, "rq_1")
	if first.Status != codemode.StatusPermissionRequired || !reflect.DeepEqual(callStatuses(first), []string{"failed", "permission_required"}) {
		t.Fatalf("first resume = %+v calls %v", first, callStatuses(first))
	}
	if n := e.count(t, `SELECT count(*) FROM code_run_calls WHERE run_id = ? AND seq = 1 AND error_kind = ?`, held.ID, codemode.ErrorGrantInvalid); n != 1 {
		t.Fatal("the refused grant was not recorded as grant_invalid")
	}
	fg.set("rq_2",
		inbox.AgentToolView{CallID: "call_1", Tool: "demo.write", Decision: "allowed", Grant: "good1"},
		inbox.AgentToolView{CallID: "call_2", Tool: "demo.write", Decision: "allowed", Grant: "good2"})
	rg.reqs["rq_2"] = &inbox.Request{ID: "rq_2", AgentID: "ag_a", Tools: []inbox.ToolRequest{
		{CallID: "call_1", Tool: "demo.write", Params: constraints(t, `{"x": {"eq": 1}}`)},
		{CallID: "call_2", Tool: "demo.write", Params: constraints(t, `{"x": {"eq": 2}}`)},
	}}
	done := e.resume(t, "ag_a", held.ID, "rq_2")
	if done.Status != codemode.StatusSucceeded || string(done.Result) != `{"first":true,"w":true}` {
		t.Fatalf("second resume = %+v (%s)", done, done.Result)
	}
	var ones []any
	for _, w := range f.sent("demo.write") {
		if x, _ := w.args["x"].(float64); x == 1 {
			ones = append(ones, w.args[grantArg])
		}
	}
	if !reflect.DeepEqual(ones, []any{nil, "bad", "good1"}) {
		t.Fatalf("demo.write x=1 grants = %v", ones)
	}
}
