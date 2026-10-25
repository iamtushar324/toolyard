package coderuns

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tusharbhardwaj/toolyard/internal/codemode"
	"github.com/tusharbhardwaj/toolyard/internal/inbox"
)

// TestJavaScriptRunIsStoredAndResumed: a JavaScript run keeps its language
// in the row, the view and the idempotency fingerprint, and a resume runs
// it again as JavaScript.
func TestJavaScriptRunIsStoredAndResumed(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	script := `const r = await tools.demo.read({ q: "js" });
console.log("read", r.q);
const w = await tools.demo.write({ x: 2 });
return { q: r.q, ok: w.ok };`
	run, created, err := e.svc.Start(ctx, StartRequest{AgentID: "ag_a", Code: script, Reason: testReason, Language: "javascript", IdempotencyKey: "js-1"})
	if err != nil || !created || run.Language != codemode.LanguageJavaScript {
		t.Fatalf("start: %+v %v %v", run, created, err)
	}
	held := e.settle(t, "ag_a", run.ID)
	if held.Status != codemode.StatusPermissionRequired || held.Language != codemode.LanguageJavaScript || held.Held == nil || held.Held.Tool != "demo.write" {
		t.Fatalf("held = %+v", held)
	}
	if v := held.View(); v.Language != codemode.LanguageJavaScript || len(v.Calls) != 2 {
		t.Fatalf("view = %+v", v)
	}
	if n := e.count(t, `SELECT count(*) FROM code_runs WHERE id = ? AND language = ?`, run.ID, "javascript"); n != 1 {
		t.Fatalf("stored language rows = %d", n)
	}
	// The same key with the same script as Starlark is another request.
	if _, _, err := e.svc.Start(ctx, StartRequest{AgentID: "ag_a", Code: script, Reason: testReason, IdempotencyKey: "js-1"}); !errors.Is(err, ErrIdempotencyKeyReused) {
		t.Fatalf("same key, other language: %v", err)
	}
	e.grants.set("rq_js", inbox.AgentToolView{CallID: "call_1", Tool: "demo.write", Decision: "allowed", Grant: "tok-w"})
	if _, err := e.svc.Resume(ctx, ResumeRequest{AgentID: "ag_a", RunID: run.ID, RequestID: "rq_js"}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	done := e.settle(t, "ag_a", run.ID)
	if done.Status != codemode.StatusSucceeded || string(done.Result) != `{"q":"js","ok":true}` ||
		!reflect.DeepEqual(callStatuses(done), []string{"succeeded+replayed", "succeeded"}) {
		t.Fatalf("resumed = %+v calls %v", done, callStatuses(done))
	}
	if !reflect.DeepEqual(done.Logs, []string{"read js"}) {
		t.Fatalf("logs = %q", done.Logs)
	}
	if _, _, err := e.svc.Start(ctx, StartRequest{AgentID: "ag_a", Code: "x", Reason: testReason, Language: "lua"}); err == nil {
		t.Fatal("an unknown language should be refused")
	} else if ie := (*InputError)(nil); !errors.As(err, &ie) || ie.Field != "language" {
		t.Fatalf("lua: %v", err)
	}
}

// TestPerAgentCapRefusesTheThirdRunOnly: with room for four scripts, one
// agent runs two at once; its third start is refused busy (HTTP 429 at
// the route) and creates nothing, while another agent starts at once.
func TestPerAgentCapRefusesTheThirdRunOnly(t *testing.T) {
	e := newEnv(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	e.rt.SetWorker(codemode.Worker{Path: exe, Env: []string{testWorkerEnv + "=1"}, MaxConcurrent: 4, MaxPerAgent: 2, AcquireWait: 100 * time.Millisecond})
	release := make(chan struct{})
	started := make(chan struct{}, 8)
	e.f.setHandler("demo.read", func(args map[string]any) (*mcp.CallToolResult, error) {
		started <- struct{}{}
		<-release
		return mcp.NewToolResultText(`{"n": 1}`), nil
	})
	defer close(release)
	ctx := context.Background()
	var runs []string
	for i := 0; i < 2; i++ {
		run, _, err := e.svc.Start(ctx, StartRequest{AgentID: "ag_a", Code: readScript, Reason: testReason})
		if err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
		runs = append(runs, run.ID)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(30 * time.Second):
			t.Fatal("agent a's runs did not start")
		}
	}
	before := e.count(t, `SELECT count(*) FROM code_runs`)
	_, _, err = e.svc.Start(ctx, StartRequest{AgentID: "ag_a", Code: readScript, Reason: testReason})
	if !errors.Is(err, codemode.ErrBusy) || !strings.Contains(err.Error(), "this caller already runs 2 scripts") {
		t.Fatalf("third start for agent a: %v", err)
	}
	if n := e.count(t, `SELECT count(*) FROM code_runs`); n != before {
		t.Fatalf("a refused start stored a run (%d -> %d)", before, n)
	}
	run, _, err := e.svc.Start(ctx, StartRequest{AgentID: "ag_b", Code: readScript, Reason: testReason})
	if err != nil {
		t.Fatalf("agent b was held up by agent a: %v", err)
	}
	select {
	case <-started:
	case <-time.After(30 * time.Second):
		t.Fatal("agent b's run did not start")
	}
	_ = run
	_ = runs
}

// demo.post is a write that always runs: a resume must never send it twice.
func init() {
	testTools = append(testTools, codemode.Tool{Server: "demo", Name: "post", Target: "demo.post", Description: "Post a message.",
		ReasonField: "_reason", Properties: map[string]any{"text": map[string]any{"type": "string"}}, Required: []string{"text"}})
}

func (e *env) countPosts(t *testing.T) *atomic.Int32 {
	t.Helper()
	var n atomic.Int32
	e.f.setHandler("demo.post", func(args map[string]any) (*mcp.CallToolResult, error) {
		n.Add(1)
		return mcp.NewToolResultText(`{"posted": "` + args["text"].(string) + `"}`), nil
	})
	return &n
}

// TestResumeNeverRepeatsACallAfterTheHeldOne (review P0-1): with
// tools.parallel the held write is call 1 and the post that ran is call 2.
// The resume sends the write with its grant and answers the post from the
// record, so the post runs once.
func TestResumeNeverRepeatsACallAfterTheHeldOne(t *testing.T) {
	e := newEnv(t)
	posts := e.countPosts(t)
	ctx := context.Background()
	script := `const [w, p] = await tools.parallel([
  () => tools.demo.write({ x: 1 }),
  () => tools.demo.post({ text: "hi" }),
], { limit: 2 });
return { wrote: w.ok, posted: p.posted };`
	run, _, err := e.svc.Start(ctx, StartRequest{AgentID: "ag_a", Code: script, Reason: testReason, Language: "javascript"})
	if err != nil {
		t.Fatal(err)
	}
	held := e.settle(t, "ag_a", run.ID)
	if held.Status != codemode.StatusPermissionRequired || held.Held == nil || held.Held.Seq != 1 ||
		!reflect.DeepEqual(callStatuses(held), []string{"permission_required", "succeeded"}) || posts.Load() != 1 {
		t.Fatalf("held = %+v calls %v posts %d", held, callStatuses(held), posts.Load())
	}
	e.grants.set("rq_p", inbox.AgentToolView{CallID: "call_1", Tool: "demo.write", Decision: "allowed", Grant: "tok-w"})
	if _, err := e.svc.Resume(ctx, ResumeRequest{AgentID: "ag_a", RunID: run.ID, RequestID: "rq_p"}); err != nil {
		t.Fatal(err)
	}
	done := e.settle(t, "ag_a", run.ID)
	if done.Status != codemode.StatusSucceeded || string(done.Result) != `{"wrote":true,"posted":"hi"}` ||
		!reflect.DeepEqual(callStatuses(done), []string{"succeeded", "succeeded+replayed"}) {
		t.Fatalf("resumed = %+v calls %v", done, callStatuses(done))
	}
	if n := posts.Load(); n != 1 {
		t.Fatalf("the post after the held write ran %d times", n)
	}
	if len(e.f.sent("demo.post")) != 1 {
		t.Fatalf("demo.post reached the gateway %d times", len(e.f.sent("demo.post")))
	}

	// A recorded call with an unknown outcome after the held one is never
	// sent again either: the resume stops with outcome_unknown.
	run, _, err = e.svc.Start(ctx, StartRequest{AgentID: "ag_a", Code: script, Reason: testReason, Language: "javascript"})
	if err != nil {
		t.Fatal(err)
	}
	held = e.settle(t, "ag_a", run.ID)
	if _, err := e.db.Exec(`UPDATE code_run_calls SET status = ? WHERE run_id = ? AND seq = ?`, codemode.StatusOutcomeUnknown, run.ID, 2); err != nil {
		t.Fatal(err)
	}
	before := posts.Load()
	e.grants.set("rq_q", inbox.AgentToolView{CallID: "call_1", Tool: "demo.write", Decision: "allowed", Grant: "tok-w"})
	if _, err := e.svc.Resume(ctx, ResumeRequest{AgentID: "ag_a", RunID: run.ID, RequestID: "rq_q"}); err != nil {
		t.Fatal(err)
	}
	done = e.settle(t, "ag_a", run.ID)
	if done.Status != codemode.StatusOutcomeUnknown || done.ErrorKind != codemode.ErrorOutcomeUnknown || posts.Load() != before {
		t.Fatalf("unknown-outcome record: %+v posts %d -> %d", done, before, posts.Load())
	}
}

// TestResumeAfterACaughtHeldCall (review P1-4): a held call the script
// caught (try_call) is recorded permission_required; the resume sends it
// again with the request's grant instead of calling the script diverged.
func TestResumeAfterACaughtHeldCall(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	script := `first = try_call(demo.write, x=1)
r = demo.read(q="between")
w = demo.write(x=2)
result = {"first": first["ok"], "read": r["q"], "second": w["ok"]}
`
	held := e.start(t, "ag_a", script, "")
	if held.Status != codemode.StatusPermissionRequired || held.Held == nil || held.Held.Seq != 3 ||
		!reflect.DeepEqual(callStatuses(held), []string{"permission_required", "succeeded", "permission_required"}) {
		t.Fatalf("held = %+v calls %v", held, callStatuses(held))
	}
	e.grants.set("rq_c",
		inbox.AgentToolView{CallID: "call_1", Tool: "demo.write", Decision: "allowed", Grant: "tok-w"},
		inbox.AgentToolView{CallID: "call_2", Tool: "demo.write", Decision: "allowed", Grant: "tok-w"})
	if _, err := e.svc.Resume(ctx, ResumeRequest{AgentID: "ag_a", RunID: held.ID, RequestID: "rq_c"}); err != nil {
		t.Fatal(err)
	}
	done := e.settle(t, "ag_a", held.ID)
	if done.Status != codemode.StatusSucceeded || string(done.Result) != `{"first":true,"read":"between","second":true}` ||
		!reflect.DeepEqual(callStatuses(done), []string{"succeeded", "succeeded+replayed", "succeeded"}) {
		t.Fatalf("resumed = %+v calls %v (%s)", done, callStatuses(done), done.ErrorMessage)
	}
	if len(e.f.sent("demo.read")) != 1 {
		t.Fatalf("the read between was sent %d times", len(e.f.sent("demo.read")))
	}
}
