package coderuns

import (
	"context"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tusharbhardwaj/toolyard/internal/codemode"
	"github.com/tusharbhardwaj/toolyard/internal/inbox"
)

// startJS starts a JavaScript run and waits for it to settle.
func (e *env) startJS(t *testing.T, agentID, code string) *Run {
	t.Helper()
	run, created, err := e.svc.Start(context.Background(), StartRequest{AgentID: agentID, Code: code, Reason: testReason, Language: "javascript"})
	if err != nil || !created {
		t.Fatalf("start: created %v, %v", created, err)
	}
	return e.settle(t, agentID, run.ID)
}

// resume resumes a held run and waits for it to settle.
func (e *env) resume(t *testing.T, agentID, runID, requestID string) *Run {
	t.Helper()
	if _, err := e.svc.Resume(context.Background(), ResumeRequest{AgentID: agentID, RunID: runID, RequestID: requestID}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	return e.settle(t, agentID, runID)
}

// TestResumeNeverSendsACallOnAShiftedPath (B2 P1-A): an Inbox read is
// sent again on a resume and answers differently, so the script takes
// another path and its calls shift: the parallel reads no longer match
// calls 2 and 3, and the post that lands past the record (call 4, new by
// its number) is never sent, though it started beside them.
func TestResumeNeverSendsACallOnAShiftedPath(t *testing.T) {
	inboxStatus := codemode.Tool{Server: "toolyard", Name: "inbox_status", Target: "inbox.status", Description: "Read requests.",
		ReasonField: "_reason", Properties: map[string]any{"ids": map[string]any{"type": "array"}}, Required: []string{"ids"}}
	fg := &fakeGrants{views: map[string]inbox.AgentView{}}
	e, f := newEnvWith(t, []codemode.Tool{inboxStatus}, fg)
	posts := e.countPosts(t)
	var reads atomic.Int32
	f.setHandler("inbox.status", func(map[string]any) (*mcp.CallToolResult, error) {
		if reads.Add(1) == 1 {
			return mcp.NewToolResultText(`{"state": "first"}`), nil
		}
		return mcp.NewToolResultText(`{"state": "second"}`), nil
	})
	script := `const s = await tools.toolyard.inbox_status({ ids: ["rq_1"] });
if (s.state === "first") {
  await tools.demo.post({ text: "early" });
  await tools.demo.write({ x: 1 });
} else {
  await tools.parallel([
    () => tools.demo.read({ q: "a" }),
    () => tools.demo.read({ q: "b" }),
    () => tools.demo.post({ text: "late" }),
  ], { limit: 3 });
}
return "done";`
	held := e.startJS(t, "ag_a", script)
	if held.Status != codemode.StatusPermissionRequired || held.Held == nil || held.Held.Seq != 3 || posts.Load() != 1 {
		t.Fatalf("held = %+v posts %d", held, posts.Load())
	}
	fg.set("rq_1", inbox.AgentToolView{CallID: "call_1", Tool: "demo.write", Decision: "allowed", Grant: "tok-w"})
	done := e.resume(t, "ag_a", held.ID, "rq_1")
	if done.Status != codemode.StatusFailed || done.ErrorKind != codemode.ErrorScriptDiverged {
		t.Fatalf("resumed = %+v", done)
	}
	if n := posts.Load(); n != 1 {
		t.Fatalf("demo.post ran %d times, want 1 (the shifted call 4 must not be sent)", n)
	}
	if sent := f.sent("demo.post"); len(sent) != 1 || sent[0].args["text"] != "early" {
		t.Fatalf("demo.post calls = %+v", sent)
	}
	if n := len(f.sent("demo.read")) + len(f.sent("demo.write")); n != 1 {
		t.Fatalf("%d reads and writes reached the gateway, want only the first write", n)
	}
}

// TestResumeKeepsRecordsPastAnEarlierHold (B2 P1-B): a resume that holds at
// an earlier call than before keeps the records of the calls after it, so
// the next resume answers the post from its record instead of sending it
// as new. W1 is held and caught first; the first resume sends it with a
// grant and it is deferred (uncaught this time), holding the run at call
// 1; the second resume runs it under its approval and replays the post.
func TestResumeKeepsRecordsPastAnEarlierHold(t *testing.T) {
	e := newEnv(t)
	posts := e.countPosts(t)
	e.f.setHandler("demo.write", func(args map[string]any) (*mcp.CallToolResult, error) {
		x, _ := args["x"].(float64)
		switch {
		case x == 1 && args[approvalIDArg] == "ap_9":
			return mcp.NewToolResultText(`{"ok": true}`), nil
		case x == 1 && args[grantArg] == "tok-1":
			res := mcp.NewToolResultError("demo.write is waiting for approval ap_9")
			res.StructuredContent = map[string]any{"status": "deferred", "approval_id": "ap_9"}
			res.Meta = &mcp.Meta{AdditionalFields: map[string]any{"toolyard.deferred": true}}
			return res, nil
		case x == 2 && args[grantArg] == "tok-2":
			return mcp.NewToolResultText(`{"ok": true}`), nil
		}
		return permissionRequired("demo.write"), nil
	})
	script := `try { await tools.demo.write({ x: 1 }); } catch (e) { if (e.status !== "permission_required") throw e; }
const p = await tools.demo.post({ text: "once" });
const w = await tools.demo.write({ x: 2 });
return { posted: p.posted, wrote: w.ok };`
	held := e.startJS(t, "ag_a", script)
	if held.Status != codemode.StatusPermissionRequired || held.Held == nil || held.Held.Seq != 3 || posts.Load() != 1 ||
		!reflect.DeepEqual(callStatuses(held), []string{"permission_required", "succeeded", "permission_required"}) {
		t.Fatalf("held = %+v calls %v posts %d", held, callStatuses(held), posts.Load())
	}
	e.grants.set("rq_1",
		inbox.AgentToolView{CallID: "call_1", Tool: "demo.write", Decision: "allowed", Grant: "tok-1"},
		inbox.AgentToolView{CallID: "call_2", Tool: "demo.write", Decision: "allowed", Grant: "tok-2"})
	first := e.resume(t, "ag_a", held.ID, "rq_1")
	if first.Status != codemode.StatusDeferred || first.Held == nil || first.Held.Seq != 1 || first.Held.ApprovalID != "ap_9" {
		t.Fatalf("first resume = %+v held %+v", first, first.Held)
	}
	// Calls 2 and 3 keep their records though this execution stopped at 1.
	if !reflect.DeepEqual(callStatuses(first), []string{"deferred", "succeeded", "permission_required"}) {
		t.Fatalf("records after the first resume = %v", callStatuses(first))
	}
	second := e.resume(t, "ag_a", held.ID, "")
	// The post is replayed; the second write, which no grant covers on
	// this resume, gets its hold again.
	if second.Status != codemode.StatusPermissionRequired || second.Held == nil || second.Held.Seq != 3 ||
		!reflect.DeepEqual(callStatuses(second), []string{"succeeded", "succeeded+replayed", "permission_required+replayed"}) {
		t.Fatalf("second resume = %+v calls %v", second, callStatuses(second))
	}
	if n := posts.Load(); n != 1 {
		t.Fatalf("demo.post ran %d times, want 1", n)
	}
	writes := e.f.sent("demo.write")
	if len(writes) != 4 || writes[3].args[approvalIDArg] != "ap_9" || writes[3].args[grantArg] != nil {
		t.Fatalf("demo.write calls = %+v", writes)
	}
}

// TestResumeRefusesAMissingRecordBelowTheMark (B2 P1-B): a call with no
// record at a number the run reached before means the script took another
// path; it is refused, never sent as new.
func TestResumeRefusesAMissingRecordBelowTheMark(t *testing.T) {
	e := newEnv(t)
	posts := e.countPosts(t)
	script := "p = demo.post(text=\"x\")\nr = demo.read(q=\"one\")\nw = demo.write(x=1)\nresult = w\n"
	held := e.start(t, "ag_a", script, "")
	if held.Status != codemode.StatusPermissionRequired || held.Held == nil || held.Held.Seq != 3 {
		t.Fatalf("held = %+v", held)
	}
	if _, err := e.db.Exec(`DELETE FROM code_run_calls WHERE run_id = ? AND seq = 2`, held.ID); err != nil {
		t.Fatal(err)
	}
	e.grants.set("rq_1", inbox.AgentToolView{CallID: "call_1", Tool: "demo.write", Decision: "allowed", Grant: "tok-w"})
	done := e.resume(t, "ag_a", held.ID, "rq_1")
	if done.Status != codemode.StatusFailed || done.ErrorKind != codemode.ErrorScriptDiverged || !strings.Contains(done.ErrorMessage, "has no record") {
		t.Fatalf("resumed = %+v", done)
	}
	if posts.Load() != 1 || len(e.f.sent("demo.read")) != 1 || len(e.f.sent("demo.write")) != 1 {
		t.Fatalf("a call was sent: posts %d, reads %d, writes %d", posts.Load(), len(e.f.sent("demo.read")), len(e.f.sent("demo.write")))
	}
}

// TestResumeReplaysARecordedFailure (B2 P1-C): an upstream error may come
// after the write took effect, so a resume gives the script the same
// failure (which it caught before) and never sends the call again.
func TestResumeReplaysARecordedFailure(t *testing.T) {
	e := newEnv(t)
	var posts atomic.Int32
	e.f.setHandler("demo.post", func(map[string]any) (*mcp.CallToolResult, error) {
		posts.Add(1)
		return mcp.NewToolResultError("upstream answered 502 after the post was queued"), nil
	})
	script := "r = try_call(demo.post, text=\"maybe\")\nw = demo.write(x=1)\nresult = {\"first\": r[\"ok\"], \"error\": r[\"error\"], \"w\": w[\"ok\"]}\n"
	held := e.start(t, "ag_a", script, "")
	if held.Status != codemode.StatusPermissionRequired || !reflect.DeepEqual(callStatuses(held), []string{"failed", "permission_required"}) {
		t.Fatalf("held = %+v calls %v", held, callStatuses(held))
	}
	if n := e.count(t, `SELECT count(*) FROM code_run_calls WHERE run_id = ? AND seq = 1 AND error_kind = ? AND error_text LIKE ? AND sent = 1`,
		held.ID, codemode.ErrorToolError, "%502 after the post%"); n != 1 {
		t.Fatal("the failure was not recorded with its kind and text")
	}
	e.grants.set("rq_1", inbox.AgentToolView{CallID: "call_1", Tool: "demo.write", Decision: "allowed", Grant: "tok-w"})
	done := e.resume(t, "ag_a", held.ID, "rq_1")
	if done.Status != codemode.StatusSucceeded || !reflect.DeepEqual(callStatuses(done), []string{"failed+replayed", "succeeded"}) {
		t.Fatalf("resumed = %+v calls %v", done, callStatuses(done))
	}
	got := jsonOf(t, done.Result)
	if got["first"] != false || got["error"] != "tool call failed for demo.post: upstream answered 502 after the post was queued" || got["w"] != true {
		t.Fatalf("result = %s", done.Result)
	}
	if n := posts.Load(); n != 1 {
		t.Fatalf("demo.post ran %d times, want 1", n)
	}
}

// TestResumeReplaysAHoldNoGrantCovers (B2 P1-C): a caught
// permission_required call goes again only with a grant that covers it;
// without one the script gets the same hold, so it takes the same path.
func TestResumeReplaysAHoldNoGrantCovers(t *testing.T) {
	fg := &fakeGrants{views: map[string]inbox.AgentView{}}
	rg := &requestGrants{fakeGrants: fg, reqs: map[string]*inbox.Request{}}
	e, f := newEnvWith(t, nil, rg)
	posts := e.countPosts(t)
	f.setHandler("demo.write", func(args map[string]any) (*mcp.CallToolResult, error) {
		if x, _ := args["x"].(float64); x == 2 && args[grantArg] == "tok-2" {
			return mcp.NewToolResultText(`{"ok": true}`), nil
		}
		return permissionRequired("demo.write"), nil
	})
	script := "r = try_call(demo.write, x=1)\np = demo.post(text=\"x\")\nw = demo.write(x=2)\nresult = {\"first\": r[\"status\"], \"w\": w[\"ok\"]}\n"
	held := e.start(t, "ag_a", script, "")
	if held.Status != codemode.StatusPermissionRequired || held.Held == nil || held.Held.Seq != 3 {
		t.Fatalf("held = %+v", held)
	}
	fg.set("rq_2", inbox.AgentToolView{CallID: "call_2", Tool: "demo.write", Decision: "allowed", Grant: "tok-2"})
	rg.reqs["rq_2"] = &inbox.Request{ID: "rq_2", AgentID: "ag_a", Tools: []inbox.ToolRequest{
		{CallID: "call_2", Tool: "demo.write", Params: constraints(t, `{"x": {"eq": 2}}`)},
	}}
	done := e.resume(t, "ag_a", held.ID, "rq_2")
	if done.Status != codemode.StatusSucceeded || string(done.Result) != `{"first":"permission_required","w":true}` ||
		!reflect.DeepEqual(callStatuses(done), []string{"permission_required+replayed", "succeeded+replayed", "succeeded"}) {
		t.Fatalf("resumed = %+v calls %v", done, callStatuses(done))
	}
	var ones int
	for _, w := range f.sent("demo.write") {
		if x, _ := w.args["x"].(float64); x == 1 {
			ones++
		}
	}
	if ones != 1 || posts.Load() != 1 {
		t.Fatalf("demo.write x=1 sent %d times, demo.post %d times; want 1 each", ones, posts.Load())
	}
}

// TestResumeAsksADeferredCallWithItsOwnApproval (B2 P1-C): a caught call
// that a legacy approval deferred is asked again with that approval's id,
// which answers its result, instead of being sent bare (a second
// approval).
func TestResumeAsksADeferredCallWithItsOwnApproval(t *testing.T) {
	e := newEnv(t)
	e.f.setHandler("demo.write", func(args map[string]any) (*mcp.CallToolResult, error) {
		x, _ := args["x"].(float64)
		switch {
		case x == 1 && args[approvalIDArg] == "ap_5":
			return mcp.NewToolResultText(`{"ok": true, "via": "approval"}`), nil
		case x == 1:
			res := mcp.NewToolResultError("demo.write is waiting for approval ap_5")
			res.StructuredContent = map[string]any{"status": "deferred", "approval_id": "ap_5"}
			res.Meta = &mcp.Meta{AdditionalFields: map[string]any{"toolyard.deferred": true}}
			return res, nil
		case args[grantArg] == "tok-w":
			return mcp.NewToolResultText(`{"ok": true}`), nil
		}
		return permissionRequired("demo.write"), nil
	})
	script := "r = try_call(demo.write, x=1)\nw = demo.write(x=2)\nresult = {\"first\": r[\"ok\"], \"w\": w[\"ok\"]}\n"
	held := e.start(t, "ag_a", script, "")
	if held.Status != codemode.StatusPermissionRequired || !reflect.DeepEqual(callStatuses(held), []string{"deferred", "permission_required"}) {
		t.Fatalf("held = %+v calls %v", held, callStatuses(held))
	}
	if n := e.count(t, `SELECT count(*) FROM code_run_calls WHERE run_id = ? AND seq = 1 AND approval_id = ?`, held.ID, "ap_5"); n != 1 {
		t.Fatal("the deferred call's approval id was not recorded")
	}
	e.grants.set("rq_1", inbox.AgentToolView{CallID: "call_1", Tool: "demo.write", Decision: "allowed", Grant: "tok-w"})
	done := e.resume(t, "ag_a", held.ID, "rq_1")
	if done.Status != codemode.StatusSucceeded || string(done.Result) != `{"first":true,"w":true}` {
		t.Fatalf("resumed = %+v (%s)", done, done.Result)
	}
	writes := e.f.sent("demo.write")
	if len(writes) != 4 || writes[2].args[approvalIDArg] != "ap_5" || writes[2].args[grantArg] != nil || writes[3].args[grantArg] != "tok-w" {
		t.Fatalf("demo.write calls = %+v", writes)
	}
}

// TestResumeReplaysASizeLimitedCall (B2 P2-5): a result over the per-call
// limit is recorded failed with kind limit; a resume gives the script that
// failure again instead of an empty value or a second call.
func TestResumeReplaysASizeLimitedCall(t *testing.T) {
	e := newEnv(t)
	var reads atomic.Int32
	e.f.setHandler("demo.read", func(map[string]any) (*mcp.CallToolResult, error) {
		reads.Add(1)
		return mcp.NewToolResultText(strings.Repeat("r", 7<<20)), nil
	})
	script := "r = try_call(demo.read, q=\"big\")\nw = demo.write(x=1)\nresult = {\"read\": r[\"ok\"], \"w\": w[\"ok\"]}\n"
	held := e.start(t, "ag_a", script, "")
	if held.Status != codemode.StatusPermissionRequired || !reflect.DeepEqual(callStatuses(held), []string{"failed", "permission_required"}) {
		t.Fatalf("held = %+v calls %v", held, callStatuses(held))
	}
	if n := e.count(t, `SELECT count(*) FROM code_run_calls WHERE run_id = ? AND seq = 1 AND error_kind = ?`, held.ID, codemode.ErrorLimit); n != 1 {
		t.Fatal("the size-limited call was not recorded failed with kind limit")
	}
	e.grants.set("rq_1", inbox.AgentToolView{CallID: "call_1", Tool: "demo.write", Decision: "allowed", Grant: "tok-w"})
	done := e.resume(t, "ag_a", held.ID, "rq_1")
	if done.Status != codemode.StatusSucceeded || string(done.Result) != `{"read":false,"w":true}` {
		t.Fatalf("resumed = %+v (%s)", done, done.Result)
	}
	if n := reads.Load(); n != 1 {
		t.Fatalf("demo.read ran %d times, want 1", n)
	}
}

// TestRequestIDResumeOfADeferredRunUsesItsApproval (B2 P2-6): a deferred
// run resumed with a request_id still resumes its held call with the
// legacy approval's own id, never a grant (which would run the tool a
// second time).
func TestRequestIDResumeOfADeferredRunUsesItsApproval(t *testing.T) {
	e := newEnv(t)
	e.f.setHandler("demo.write", func(args map[string]any) (*mcp.CallToolResult, error) {
		if args[approvalIDArg] == "ap_1" {
			return mcp.NewToolResultText(`{"ok": true, "via": "approval"}`), nil
		}
		if args[grantArg] != nil {
			return mcp.NewToolResultText(`{"ok": true, "via": "grant"}`), nil
		}
		res := mcp.NewToolResultError("demo.write is waiting for approval ap_1")
		res.StructuredContent = map[string]any{"status": "deferred", "approval_id": "ap_1"}
		res.Meta = &mcp.Meta{AdditionalFields: map[string]any{"toolyard.deferred": true}}
		return res, nil
	})
	held := e.start(t, "ag_a", readWriteScript, "")
	if held.Status != codemode.StatusDeferred || held.Held == nil || held.Held.ApprovalID != "ap_1" {
		t.Fatalf("held = %+v", held)
	}
	e.grants.set("rq_1", inbox.AgentToolView{CallID: "call_1", Tool: "demo.write", Decision: "allowed", Grant: "tok-w"})
	done := e.resume(t, "ag_a", held.ID, "rq_1")
	if done.Status != codemode.StatusSucceeded || !strings.Contains(string(done.Result), `"via":"approval"`) {
		t.Fatalf("resumed = %+v (%s)", done, done.Result)
	}
	writes := e.f.sent("demo.write")
	if len(writes) != 2 || writes[1].args[approvalIDArg] != "ap_1" || writes[1].args[grantArg] != nil {
		t.Fatalf("demo.write calls = %+v", writes)
	}
}

// TestArgsHashKeepsLargeIntegersApart (B2 P2-9): two integers above 2^53
// that float64 would round to one value hash differently; an int and a
// float64 of the same small value hash the same.
func TestArgsHashKeepsLargeIntegersApart(t *testing.T) {
	if argsHash(map[string]any{"n": int64(9007199254740993)}) == argsHash(map[string]any{"n": int64(9007199254740992)}) {
		t.Fatal("2^53+1 and 2^53 hash the same")
	}
	if argsHash(map[string]any{"n": 5, "m": []any{1.5}}) != argsHash(map[string]any{"m": []any{1.5}, "n": 5.0}) {
		t.Fatal("5 and 5.0 hash differently")
	}
}
