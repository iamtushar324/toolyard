package coderuns

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
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
