package coderuns

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tusharbhardwaj/toolyard/internal/codemode"
	"github.com/tusharbhardwaj/toolyard/internal/inbox"
	"github.com/tusharbhardwaj/toolyard/internal/store/storetest"
)

// extraToolsCaller is fakeCaller with more tools in its catalog.
type extraToolsCaller struct {
	*fakeCaller
	extra []codemode.Tool
}

func (c *extraToolsCaller) Tools(ctx context.Context) []codemode.Tool {
	return append(c.fakeCaller.Tools(ctx), c.extra...)
}

// requestGrants is fakeGrants that can also read a request, as
// *inbox.Service can, so a resume matches grants by their parameters.
type requestGrants struct {
	*fakeGrants
	mu   sync.Mutex
	reqs map[string]*inbox.Request
}

func (g *requestGrants) Get(_ context.Context, id string) (*inbox.Request, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if r, ok := g.reqs[id]; ok {
		return r, nil
	}
	return nil, inbox.ErrNotFound
}

// newEnvWith is newEnv with extra catalog tools and a Grants of the test's
// choice.
func newEnvWith(t *testing.T, extra []codemode.Tool, grants Grants) (*env, *fakeCaller) {
	t.Helper()
	db := storetest.Open(t)
	f := newFakeCaller()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	rt := codemode.New(&extraToolsCaller{fakeCaller: f, extra: extra}, codemode.Limits{ScriptTimeout: 60 * time.Second})
	rt.SetWorker(codemode.Worker{Path: exe, Env: []string{testWorkerEnv + "=1"}, MaxConcurrent: 2, AcquireWait: 100 * time.Millisecond})
	eng := &testEngine{rt: rt}
	svc := New(db, Options{Engine: eng, Grants: grants, PollEvery: 50 * time.Millisecond})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		svc.Wait(ctx)
	})
	return &env{db: db, svc: svc, f: f, rt: rt, eng: eng}, f
}

func constraints(t *testing.T, raw string) map[string]inbox.Constraint {
	t.Helper()
	var c map[string]inbox.Constraint
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatalf("constraints %s: %v", raw, err)
	}
	return c
}

// Two grants of one request cover the same tool with different parameters,
// in the other order than the script calls them: each call gets the grant
// whose parameters cover it.
func TestResumeMatchesGrantsByParameters(t *testing.T) {
	fg := &fakeGrants{views: map[string]inbox.AgentView{}}
	rg := &requestGrants{fakeGrants: fg, reqs: map[string]*inbox.Request{}}
	e, f := newEnvWith(t, nil, rg)
	f.setHandler("demo.write", func(args map[string]any) (*mcp.CallToolResult, error) {
		x, _ := args["x"].(float64)
		switch {
		case args[grantArg] == "tok-1" && x == 1, args[grantArg] == "tok-2" && x == 2:
			return mcp.NewToolResultText(`{"ok": true}`), nil
		case args[grantArg] != nil:
			return mcp.NewToolResultError("grant_invalid: the grant does not cover this call"), nil
		}
		return permissionRequired("demo.write"), nil
	})
	script := "a = demo.write(x=2)\nb = demo.write(x=1)\nresult = {\"a\": a, \"b\": b}\n"
	held := e.start(t, "ag_a", script, "")
	if held.Status != codemode.StatusPermissionRequired || held.Held == nil || held.Held.Seq != 1 {
		t.Fatalf("held = %+v", held)
	}
	fg.set("rq_2",
		inbox.AgentToolView{CallID: "call_1", Tool: "demo.write", Decision: "allowed", Grant: "tok-1"},
		inbox.AgentToolView{CallID: "call_2", Tool: "demo.write", Decision: "allowed", Grant: "tok-2"})
	rg.reqs["rq_2"] = &inbox.Request{ID: "rq_2", AgentID: "ag_a", Tools: []inbox.ToolRequest{
		{CallID: "call_1", Tool: "demo.write", Params: constraints(t, `{"x": {"eq": 1}}`)},
		{CallID: "call_2", Tool: "demo.write", Params: constraints(t, `{"x": {"eq": 2}}`)},
	}}
	run, err := e.svc.Resume(context.Background(), ResumeRequest{AgentID: "ag_a", RunID: held.ID, RequestID: "rq_2"})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	done := e.settle(t, "ag_a", run.ID)
	if done.Status != codemode.StatusSucceeded {
		t.Fatalf("resumed = %+v", done)
	}
	writes := f.sent("demo.write")
	if len(writes) != 3 {
		t.Fatalf("demo.write sent %d times, want 3: %+v", len(writes), writes)
	}
	if writes[1].args[grantArg] != "tok-2" || writes[2].args[grantArg] != "tok-1" {
		t.Fatalf("grants went to the wrong calls: %+v", writes)
	}
}

// A call that no grant covers goes without one, so the run is held again
// (resumable), not failed.
func TestResumeWithoutACoveringGrantHoldsAgain(t *testing.T) {
	fg := &fakeGrants{views: map[string]inbox.AgentView{}}
	rg := &requestGrants{fakeGrants: fg, reqs: map[string]*inbox.Request{}}
	e, f := newEnvWith(t, nil, rg)
	held := e.start(t, "ag_a", "result = demo.write(x=5)\n", "")
	if held.Status != codemode.StatusPermissionRequired {
		t.Fatalf("held = %+v", held)
	}
	fg.set("rq_3", inbox.AgentToolView{CallID: "call_1", Tool: "demo.write", Decision: "allowed", Grant: "tok-w"})
	rg.reqs["rq_3"] = &inbox.Request{ID: "rq_3", AgentID: "ag_a", Tools: []inbox.ToolRequest{
		{CallID: "call_1", Tool: "demo.write", Params: constraints(t, `{"x": {"eq": 1}}`)},
	}}
	run, err := e.svc.Resume(context.Background(), ResumeRequest{AgentID: "ag_a", RunID: held.ID, RequestID: "rq_3"})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	done := e.settle(t, "ag_a", run.ID)
	if done.Status != codemode.StatusPermissionRequired || HTTPStatus(done.Status) != 403 {
		t.Fatalf("resumed = %+v", done)
	}
	if writes := f.sent("demo.write"); len(writes) != 2 || writes[1].args[grantArg] != nil {
		t.Fatalf("demo.write calls = %+v", writes)
	}
}

// Grant tokens in a call's answer, the result or a log line are never
// stored in plain text.
func TestStoredRunsRedactGrantTokens(t *testing.T) {
	e := newEnv(t)
	e.f.setHandler("demo.read", func(map[string]any) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText(`{"grant": "tyg_aWQ.c2ln-_x"}`), nil
	})
	r := e.start(t, "ag_a", "a = demo.read(q=\"one\")\nprint(a)\nresult = a\n", "")
	if r.Status != codemode.StatusSucceeded {
		t.Fatalf("run = %+v", r)
	}
	if strings.Contains(string(r.Result), "tyg_aWQ") || !strings.Contains(string(r.Result), "tyg_[redacted]") {
		t.Fatalf("result = %s", r.Result)
	}
	for _, l := range r.Logs {
		if strings.Contains(l, "tyg_aWQ") {
			t.Fatalf("log = %q", l)
		}
	}
	if n := e.count(t, `SELECT count(*) FROM code_run_calls WHERE replay_text LIKE ?`, "%tyg_aWQ%"); n != 0 {
		t.Fatal("a call record holds a grant token")
	}
	if n := e.count(t, `SELECT count(*) FROM code_runs WHERE result LIKE ? OR logs LIKE ?`, "%tyg_aWQ%", "%tyg_aWQ%"); n != 0 {
		t.Fatal("the run row holds a grant token")
	}
}

// A resume sends Inbox status reads again instead of replaying them: their
// answers can carry live grant tokens, which the record never keeps.
func TestResumeRerunsInboxReads(t *testing.T) {
	inboxStatus := codemode.Tool{Server: "toolyard", Name: "inbox_status", Target: "inbox.status", Description: "Read requests.",
		ReasonField: "_reason", Properties: map[string]any{"ids": map[string]any{"type": "array"}}, Required: []string{"ids"}}
	fg := &fakeGrants{views: map[string]inbox.AgentView{}}
	e, f := newEnvWith(t, []codemode.Tool{inboxStatus}, fg)
	f.setHandler("inbox.status", func(map[string]any) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText(`{"requests": [{"status": "approved", "tools": [{"grant": "tyg_aWQ.c2ln"}]}]}`), nil
	})
	script := "s = toolyard.inbox_status(ids=[\"rq_1\"])\nb = demo.write(x=1)\nresult = {\"b\": b}\n"
	held := e.start(t, "ag_a", script, "")
	if held.Status != codemode.StatusPermissionRequired || held.Held == nil || held.Held.Seq != 2 {
		t.Fatalf("held = %+v", held)
	}
	// replay_text holds a JSON string: the Inbox read's is the empty one.
	if n := e.count(t, `SELECT count(*) FROM code_run_calls WHERE run_id = ? AND seq = 1 AND COALESCE(replay_text, '""') = '""'`, held.ID); n != 1 {
		t.Fatal("the Inbox read's answer was stored for replay")
	}
	fg.set("rq_1", inbox.AgentToolView{CallID: "call_1", Tool: "demo.write", Decision: "allowed", Grant: "tok-w"})
	run, err := e.svc.Resume(context.Background(), ResumeRequest{AgentID: "ag_a", RunID: held.ID, RequestID: "rq_1"})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	done := e.settle(t, "ag_a", run.ID)
	if done.Status != codemode.StatusSucceeded {
		t.Fatalf("resumed = %+v", done)
	}
	if n := len(f.sent("inbox.status")); n != 2 {
		t.Fatalf("inbox.status sent %d times, want 2 (sent again on resume)", n)
	}
}

func TestCheckReasonRejectsInvalidUTF8(t *testing.T) {
	bad := "a reason with a broken byte \xff in the middle of it"
	if _, err := checkReason(bad); !isInput(err) {
		t.Fatalf("checkReason(invalid UTF-8) = %v, want an InputError", err)
	}
	if _, err := checkReason("a reason that is valid UTF-8, ünïcödé included"); err != nil {
		t.Fatalf("checkReason(valid) = %v", err)
	}
}
