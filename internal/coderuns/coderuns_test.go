package coderuns

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tusharbhardwaj/toolyard/internal/codemode"
	"github.com/tusharbhardwaj/toolyard/internal/inbox"
	"github.com/tusharbhardwaj/toolyard/internal/store"
	"github.com/tusharbhardwaj/toolyard/internal/store/storetest"
)

// Scripts run in a child process: in these tests the child is this test
// binary, which runs the code-mode worker instead of the tests when it sees
// the marker variable in the bare environment the parent builds.
const testWorkerEnv = "TOOLYARD_CODERUNS_TEST_WORKER"

func TestMain(m *testing.M) {
	if os.Getenv(testWorkerEnv) == "1" {
		os.Exit(codemode.WorkerMain(os.Stdin, os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

const testReason = "run the code route test script for TEC-1627"

// fakeCaller is the gateway: two tools on server demo, answered by
// handlers keyed by target, every call recorded.
type fakeCaller struct {
	mu       sync.Mutex
	calls    []fakeCall
	handlers map[string]func(args map[string]any) (*mcp.CallToolResult, error)
}

type fakeCall struct {
	target string
	args   map[string]any
}

var testTools = []codemode.Tool{
	{Server: "demo", Name: "read", Target: "demo.read", Description: "Read a value.", ReasonField: "_reason",
		Properties: map[string]any{"q": map[string]any{"type": "string"}}, Required: []string{"q"}},
	{Server: "demo", Name: "write", Target: "demo.write", Description: "Write a value.", ReasonField: "_reason",
		Properties: map[string]any{"x": map[string]any{"type": "integer"}}, Required: []string{"x"}},
}

func newFakeCaller() *fakeCaller {
	f := &fakeCaller{handlers: map[string]func(map[string]any) (*mcp.CallToolResult, error){}}
	f.handlers["demo.read"] = func(args map[string]any) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText(`{"n": 1, "q": "` + args["q"].(string) + `"}`), nil
	}
	// demo.write runs only with the grant "tok-w"; without it the gateway
	// answers permission_required with an Inbox draft.
	f.handlers["demo.write"] = func(args map[string]any) (*mcp.CallToolResult, error) {
		if args[grantArg] == "tok-w" {
			return mcp.NewToolResultText(`{"ok": true}`), nil
		}
		return permissionRequired("demo.write"), nil
	}
	return f
}

func (f *fakeCaller) Tools(context.Context) []codemode.Tool { return testTools }

func (f *fakeCaller) RouteCall(_ context.Context, _, target string, args map[string]any) (*mcp.CallToolResult, error) {
	cp := make(map[string]any, len(args))
	for k, v := range args {
		cp[k] = v
	}
	f.mu.Lock()
	f.calls = append(f.calls, fakeCall{target: target, args: cp})
	h := f.handlers[target]
	f.mu.Unlock()
	if h == nil {
		return mcp.NewToolResultErrorf("tool %q not found", target), nil
	}
	return h(args)
}

func (f *fakeCaller) setHandler(target string, h func(map[string]any) (*mcp.CallToolResult, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers[target] = h
}

// sent returns the calls the gateway received for target.
func (f *fakeCaller) sent(target string) []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeCall
	for _, c := range f.calls {
		if c.target == target {
			out = append(out, c)
		}
	}
	return out
}

func permissionRequired(target string) *mcp.CallToolResult {
	res := mcp.NewToolResultError(target + " is restricted. Nothing ran and nothing was sent to your owner.")
	res.StructuredContent = map[string]any{"status": "permission_required", "executed": false, "tool": target,
		"draft": map[string]any{"title": "Write x", "tools": []any{map[string]any{"call_id": "call_1", "tool": target}}}}
	res.Meta = &mcp.Meta{AdditionalFields: map[string]any{"toolyard.permission_required": true}}
	return res
}

// testEngine runs scripts on a real runtime whose worker is this binary.
type testEngine struct {
	rt       *codemode.Runtime
	draining atomic.Bool
}

func (e *testEngine) Runner() Runner { return e.rt }

func (e *testEngine) RunContext(ctx context.Context, _, _ string) (context.Context, bool) {
	return ctx, !e.draining.Load()
}

// fakeGrants answers Status from views keyed by request id.
type fakeGrants struct {
	mu    sync.Mutex
	views map[string]inbox.AgentView
}

func (g *fakeGrants) Status(_ context.Context, _ string, ids []string) ([]inbox.AgentView, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]inbox.AgentView, 0, len(ids))
	for _, id := range ids {
		v, ok := g.views[id]
		if !ok {
			v = inbox.AgentView{RequestID: id, Status: "not_found"}
		}
		out = append(out, v)
	}
	return out, nil
}

func (g *fakeGrants) set(id string, tools ...inbox.AgentToolView) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.views[id] = inbox.AgentView{RequestID: id, Status: "approved", Tools: tools}
}

type env struct {
	db     *store.DB
	svc    *Service
	f      *fakeCaller
	rt     *codemode.Runtime
	eng    *testEngine
	grants *fakeGrants
}

func newEnv(t *testing.T) *env {
	t.Helper()
	db := storetest.Open(t)
	f := newFakeCaller()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	rt := codemode.New(f, codemode.Limits{ScriptTimeout: 60 * time.Second})
	rt.SetWorker(codemode.Worker{Path: exe, Env: []string{testWorkerEnv + "=1"}, MaxConcurrent: 2, AcquireWait: 100 * time.Millisecond})
	eng := &testEngine{rt: rt}
	g := &fakeGrants{views: map[string]inbox.AgentView{}}
	svc := New(db, Options{Engine: eng, Grants: g, PollEvery: 50 * time.Millisecond})
	// Runs on before the database closes (cleanups run last-in first-out).
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		svc.Wait(ctx)
	})
	return &env{db: db, svc: svc, f: f, rt: rt, eng: eng, grants: g}
}

// settle waits for the run to leave queued and running.
func (e *env) settle(t *testing.T, agentID, id string) *Run {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		r, err := e.svc.Get(context.Background(), agentID, id, 5*time.Second)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if !InProgress(r.Status) {
			return r
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s still %s", id, r.Status)
		}
	}
}

func (e *env) start(t *testing.T, agentID, code, key string) *Run {
	t.Helper()
	run, created, err := e.svc.Start(context.Background(), StartRequest{AgentID: agentID, Code: code, Reason: testReason, IdempotencyKey: key})
	if err != nil || !created {
		t.Fatalf("start: created %v, %v", created, err)
	}
	return e.settle(t, agentID, run.ID)
}

func (e *env) count(t *testing.T, q string, args ...any) int {
	t.Helper()
	var n int
	if err := e.db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func callStatuses(r *Run) []string {
	out := make([]string, len(r.Calls))
	for i, c := range r.Calls {
		out[i] = c.Status
		if c.Replayed {
			out[i] += "+replayed"
		}
	}
	return out
}

func jsonOf(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("result %s: %v", raw, err)
	}
	return v
}

const readScript = "result = demo.read(q=\"one\")\n"
const readWriteScript = "print(\"start\")\na = demo.read(q=\"one\")\nb = demo.write(x=1)\nresult = {\"a\": a, \"b\": b}\n"

func TestStartRunsAndRecords(t *testing.T) {
	e := newEnv(t)
	r := e.start(t, "ag_a", readScript, "")
	if r.Status != codemode.StatusSucceeded || HTTPStatus(r.Status) != 200 {
		t.Fatalf("run = %+v", r)
	}
	if got := jsonOf(t, r.Result); got["n"] != float64(1) || got["q"] != "one" {
		t.Fatalf("result = %s", r.Result)
	}
	if !reflect.DeepEqual(callStatuses(r), []string{"succeeded"}) || r.Calls[0].Tool != "demo.read" || r.Calls[0].ArgsHash == "" {
		t.Fatalf("calls = %+v", r.Calls)
	}
	if r.FinishedAt == 0 || r.CreatedAt == 0 || !strings.HasPrefix(r.ID, "cr_") {
		t.Fatalf("times or id: %+v", r)
	}
	v := r.View()
	if v.Error != nil || v.Held != nil || v.Links.Self != "/v1/code/runs/"+r.ID || v.Links.Resume != "" || len(v.Calls) != 1 || v.Calls[0].FinishedAt == nil {
		t.Fatalf("view = %+v", v)
	}
	// Another agent cannot see it.
	if _, err := e.svc.Get(context.Background(), "ag_b", r.ID, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other agent: %v", err)
	}
	// A failing script is failed, with its kind.
	r = e.start(t, "ag_a", "fail(\"boom\")\n", "")
	if r.Status != codemode.StatusFailed || r.ErrorKind != codemode.ErrorScriptError || !strings.Contains(r.ErrorMessage, "boom") ||
		HTTPStatus(r.Status) != 200 || r.View().Error == nil {
		t.Fatalf("failed run = %+v", r)
	}
}

func TestIdempotencyKeyReplaysTheRun(t *testing.T) {
	e := newEnv(t)
	first := e.start(t, "ag_a", readScript, "key-1")
	if first.Status != codemode.StatusSucceeded || len(e.f.sent("demo.read")) != 1 {
		t.Fatalf("first = %+v, sent %d", first, len(e.f.sent("demo.read")))
	}
	again, created, err := e.svc.Start(context.Background(), StartRequest{AgentID: "ag_a", Code: readScript, Reason: "  " + testReason + " ", IdempotencyKey: "key-1"})
	if err != nil || created || again.ID != first.ID || again.Status != codemode.StatusSucceeded {
		t.Fatalf("replay = %+v created %v err %v", again, created, err)
	}
	time.Sleep(100 * time.Millisecond)
	if n := len(e.f.sent("demo.read")); n != 1 {
		t.Fatalf("the replay sent %d calls upstream, want 1 in total", n)
	}
	// The same key with another script, or another reason, is refused.
	for _, req := range []StartRequest{
		{AgentID: "ag_a", Code: readScript + "x = 1\n", Reason: testReason, IdempotencyKey: "key-1"},
		{AgentID: "ag_a", Code: readScript, Reason: testReason + " again", IdempotencyKey: "key-1"},
	} {
		if _, _, err := e.svc.Start(context.Background(), req); !errors.Is(err, ErrIdempotencyKeyReused) {
			t.Fatalf("reused key: %v", err)
		}
	}
	// Keys are scoped to the agent.
	other := e.start(t, "ag_b", readScript, "key-1")
	if other.ID == first.ID {
		t.Fatal("another agent's key replayed agent a's run")
	}
	if n := e.count(t, `SELECT count(*) FROM code_runs`); n != 2 {
		t.Fatalf("runs = %d, want 2", n)
	}
}

func TestResumeReplaysEarlierCallsAndSendsTheGrant(t *testing.T) {
	e := newEnv(t)
	held := e.start(t, "ag_a", readWriteScript, "")
	if held.Status != codemode.StatusPermissionRequired || HTTPStatus(held.Status) != 403 {
		t.Fatalf("held = %+v", held)
	}
	if !reflect.DeepEqual(callStatuses(held), []string{"succeeded", "permission_required"}) {
		t.Fatalf("calls = %v", callStatuses(held))
	}
	v := held.View()
	if v.Held == nil || v.Held.Seq != 2 || v.Held.Tool != "demo.write" || v.Held.Status != "permission_required" ||
		v.Held.ApprovalID != nil || len(v.Held.Draft) == 0 || v.Links.Resume == "" || v.Result != nil {
		t.Fatalf("held view = %+v", v.Held)
	}
	if !reflect.DeepEqual(held.Logs, []string{"start"}) {
		t.Fatalf("logs = %q", held.Logs)
	}

	// No request_id, or one that is not the agent's, starts nothing.
	if _, err := e.svc.Resume(context.Background(), ResumeRequest{AgentID: "ag_a", RunID: held.ID}); !isInput(err) {
		t.Fatalf("resume without request_id: %v", err)
	}
	if _, err := e.svc.Resume(context.Background(), ResumeRequest{AgentID: "ag_a", RunID: held.ID, RequestID: "rq_nope"}); !isInput(err) {
		t.Fatalf("resume with an unknown request: %v", err)
	}
	if _, err := e.svc.Resume(context.Background(), ResumeRequest{AgentID: "ag_b", RunID: held.ID, RequestID: "rq_1"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another agent's resume: %v", err)
	}

	e.grants.set("rq_1", inbox.AgentToolView{CallID: "call_1", Tool: "demo.write", Decision: "allowed", Grant: "tok-w"})
	run, err := e.svc.Resume(context.Background(), ResumeRequest{AgentID: "ag_a", RunID: held.ID, RequestID: "rq_1"})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	done := e.settle(t, "ag_a", run.ID)
	if done.Status != codemode.StatusSucceeded {
		t.Fatalf("resumed = %+v", done)
	}
	got := jsonOf(t, done.Result)
	if a, _ := got["a"].(map[string]any); a["n"] != float64(1) {
		t.Fatalf("result = %s", done.Result)
	}
	if b, _ := got["b"].(map[string]any); b["ok"] != true {
		t.Fatalf("result = %s", done.Result)
	}
	if !reflect.DeepEqual(callStatuses(done), []string{"succeeded+replayed", "succeeded"}) {
		t.Fatalf("calls = %v", callStatuses(done))
	}
	// Call 1 was answered from the record, not sent again; call 2 went
	// with the grant.
	if n := len(e.f.sent("demo.read")); n != 1 {
		t.Fatalf("demo.read sent %d times, want 1", n)
	}
	writes := e.f.sent("demo.write")
	if len(writes) != 2 || writes[0].args[grantArg] != nil || writes[1].args[grantArg] != "tok-w" {
		t.Fatalf("demo.write calls = %+v", writes)
	}
	// The grant token is never stored.
	if n := e.count(t, `SELECT count(*) FROM code_run_calls WHERE replay_text LIKE ? OR args_hash LIKE ?`, "%tok-w%", "%tok-w%"); n != 0 {
		t.Fatal("a call record holds the grant token")
	}
	// A run that is no longer held cannot be resumed.
	if r, err := e.svc.Resume(context.Background(), ResumeRequest{AgentID: "ag_a", RunID: held.ID, RequestID: "rq_1"}); !errors.Is(err, ErrNotHeld) || r == nil || r.Status != codemode.StatusSucceeded {
		t.Fatalf("resume of a finished run: %v", err)
	}
}

func TestResumeStopsAtARecordedUnknownOutcome(t *testing.T) {
	e := newEnv(t)
	held := e.start(t, "ag_a", readWriteScript, "")
	if held.Status != codemode.StatusPermissionRequired {
		t.Fatalf("held = %+v", held)
	}
	if _, err := e.db.Exec(`UPDATE code_run_calls SET status = ? WHERE run_id = ? AND seq = ?`, codemode.StatusOutcomeUnknown, held.ID, 1); err != nil {
		t.Fatal(err)
	}
	e.grants.set("rq_1", inbox.AgentToolView{Tool: "demo.write", Grant: "tok-w"})
	run, err := e.svc.Resume(context.Background(), ResumeRequest{AgentID: "ag_a", RunID: held.ID, RequestID: "rq_1"})
	if err != nil {
		t.Fatal(err)
	}
	done := e.settle(t, "ag_a", run.ID)
	if done.Status != codemode.StatusOutcomeUnknown || done.ErrorKind != codemode.ErrorOutcomeUnknown || HTTPStatus(done.Status) != 409 {
		t.Fatalf("resumed = %+v", done)
	}
	if done.View().Error == nil || !strings.Contains(done.ErrorMessage, "unknown outcome") {
		t.Fatalf("error = %q", done.ErrorMessage)
	}
	if len(e.f.sent("demo.read")) != 1 || len(e.f.sent("demo.write")) != 1 {
		t.Fatalf("a call was repeated: read %d, write %d", len(e.f.sent("demo.read")), len(e.f.sent("demo.write")))
	}
}

func TestResumeRefusesADivergedScript(t *testing.T) {
	e := newEnv(t)
	held := e.start(t, "ag_a", readWriteScript, "")
	// The record says call 1 had other arguments.
	if _, err := e.db.Exec(`UPDATE code_run_calls SET args_hash = ? WHERE run_id = ? AND seq = ?`, "0000", held.ID, 1); err != nil {
		t.Fatal(err)
	}
	e.grants.set("rq_1", inbox.AgentToolView{Tool: "demo.write", Grant: "tok-w"})
	run, err := e.svc.Resume(context.Background(), ResumeRequest{AgentID: "ag_a", RunID: held.ID, RequestID: "rq_1"})
	if err != nil {
		t.Fatal(err)
	}
	done := e.settle(t, "ag_a", run.ID)
	if done.Status != codemode.StatusFailed || done.ErrorKind != codemode.ErrorScriptDiverged {
		t.Fatalf("resumed = %+v", done)
	}
	if !reflect.DeepEqual(callStatuses(done), []string{"failed"}) {
		t.Fatalf("calls = %v", callStatuses(done))
	}
	if len(e.f.sent("demo.read")) != 1 || len(e.f.sent("demo.write")) != 1 {
		t.Fatal("a diverged resume sent a call")
	}
}

func TestResumeOfADeferredApproval(t *testing.T) {
	e := newEnv(t)
	e.f.setHandler("demo.write", func(args map[string]any) (*mcp.CallToolResult, error) {
		if args[approvalIDArg] == "ap_1" {
			return mcp.NewToolResultText(`{"ok": true, "via": "approval"}`), nil
		}
		res := mcp.NewToolResultError("demo.write is waiting for approval ap_1")
		res.StructuredContent = map[string]any{"status": "deferred", "approval_id": "ap_1"}
		res.Meta = &mcp.Meta{AdditionalFields: map[string]any{"toolyard.deferred": true}}
		return res, nil
	})
	held := e.start(t, "ag_a", readWriteScript, "")
	if held.Status != codemode.StatusDeferred || HTTPStatus(held.Status) != 202 || held.Held == nil || held.Held.ApprovalID != "ap_1" {
		t.Fatalf("deferred = %+v held %+v", held, held.Held)
	}
	if v := held.View(); v.Held == nil || v.Held.ApprovalID == nil || *v.Held.ApprovalID != "ap_1" {
		t.Fatalf("view held = %+v", v.Held)
	}
	run, err := e.svc.Resume(context.Background(), ResumeRequest{AgentID: "ag_a", RunID: held.ID})
	if err != nil {
		t.Fatal(err)
	}
	done := e.settle(t, "ag_a", run.ID)
	if done.Status != codemode.StatusSucceeded || !reflect.DeepEqual(callStatuses(done), []string{"succeeded+replayed", "succeeded"}) {
		t.Fatalf("resumed = %+v calls %v", done, callStatuses(done))
	}
	writes := e.f.sent("demo.write")
	if len(writes) != 2 || writes[1].args[approvalIDArg] != "ap_1" || writes[1].args[grantArg] != nil {
		t.Fatalf("writes = %+v", writes)
	}
}

func TestBusyAndDrainingCreateNoRun(t *testing.T) {
	e := newEnv(t)
	var slots []*codemode.Slot
	for i := 0; i < 2; i++ {
		s, err := e.rt.AcquireSlot(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		slots = append(slots, s)
	}
	_, _, err := e.svc.Start(context.Background(), StartRequest{AgentID: "ag_a", Code: readScript, Reason: testReason, IdempotencyKey: "k"})
	if !errors.Is(err, codemode.ErrBusy) {
		t.Fatalf("busy start: %v", err)
	}
	for _, s := range slots {
		s.Release()
	}
	e.eng.draining.Store(true)
	if _, _, err := e.svc.Start(context.Background(), StartRequest{AgentID: "ag_a", Code: readScript, Reason: testReason}); !errors.Is(err, ErrDraining) {
		t.Fatalf("draining start: %v", err)
	}
	if n := e.count(t, `SELECT count(*) FROM code_runs`); n != 0 {
		t.Fatalf("runs = %d, want none", n)
	}
	if len(e.f.sent("demo.read")) != 0 {
		t.Fatal("a refused start ran a call")
	}
	// The busy start left its key unused: it works now.
	e.eng.draining.Store(false)
	if r := e.start(t, "ag_a", readScript, "k"); r.Status != codemode.StatusSucceeded {
		t.Fatalf("after busy: %+v", r)
	}
}

func TestStartValidates(t *testing.T) {
	e := newEnv(t)
	big := strings.Repeat("x", MaxScriptBytes+1)
	for name, c := range map[string]struct {
		req  StartRequest
		want func(error) bool
	}{
		"short reason": {StartRequest{AgentID: "ag_a", Code: readScript, Reason: "too short"}, isInput},
		"long reason":  {StartRequest{AgentID: "ag_a", Code: readScript, Reason: strings.Repeat("r", MaxReasonLen+1)}, isInput},
		"empty code":   {StartRequest{AgentID: "ag_a", Code: " \n", Reason: testReason}, isInput},
		"NUL in code":  {StartRequest{AgentID: "ag_a", Code: "x = 1\x00", Reason: testReason}, isInput},
		"too large":    {StartRequest{AgentID: "ag_a", Code: big, Reason: testReason}, func(err error) bool { return errors.Is(err, ErrTooLarge) }},
		"bad key":      {StartRequest{AgentID: "ag_a", Code: readScript, Reason: testReason, IdempotencyKey: "a\nb"}, isInput},
		"long key":     {StartRequest{AgentID: "ag_a", Code: readScript, Reason: testReason, IdempotencyKey: strings.Repeat("k", 129)}, isInput},
		"no agent":     {StartRequest{Code: readScript, Reason: testReason}, isInput},
	} {
		if _, _, err := e.svc.Start(context.Background(), c.req); !c.want(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if n := e.count(t, `SELECT count(*) FROM code_runs`); n != 0 {
		t.Fatalf("runs = %d, want none", n)
	}
}

func isInput(err error) bool {
	var ie *InputError
	return errors.As(err, &ie)
}

// insertState stores a run in a given state, as a stopped process left it.
func (e *env) insertState(t *testing.T, id, status string, createdAt int64) {
	t.Helper()
	r := &Run{ID: id, AgentID: "ag_a", Fingerprint: "fp", Reason: testReason, Status: status, CreatedAt: createdAt, UpdatedAt: createdAt, code: readScript}
	if err := e.svc.insertRun(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if err := upsertCall(context.Background(), e.db, id, Call{Seq: 1, Tool: "demo.read", Target: "demo.read", ArgsHash: "h", Status: codemode.StatusSucceeded,
		StartedAt: createdAt, FinishedAt: createdAt, replayText: "{}"}); err != nil {
		t.Fatal(err)
	}
}

func TestRecover(t *testing.T) {
	e := newEnv(t)
	now := time.Now().UnixMilli()
	e.insertState(t, "cr_queued", StatusQueued, now)
	e.insertState(t, "cr_running", StatusRunning, now)
	e.insertState(t, "cr_done", codemode.StatusSucceeded, now)
	e.insertState(t, "cr_held", codemode.StatusPermissionRequired, now)
	failed, unknown, err := e.svc.Recover(context.Background())
	if err != nil || failed != 1 || unknown != 1 {
		t.Fatalf("recover = %d %d %v", failed, unknown, err)
	}
	for id, want := range map[string][2]string{
		"cr_queued":  {codemode.StatusFailed, ErrorInterrupted},
		"cr_running": {codemode.StatusOutcomeUnknown, ErrorInterrupted},
		"cr_done":    {codemode.StatusSucceeded, ""},
		"cr_held":    {codemode.StatusPermissionRequired, ""},
	} {
		r, err := e.svc.Get(context.Background(), "ag_a", id, 0)
		if err != nil || r.Status != want[0] || r.ErrorKind != want[1] {
			t.Errorf("%s = %+v %v, want %v", id, r, err, want)
			continue
		}
		if want[1] != "" && (r.FinishedAt == 0 || r.ErrorMessage == "") {
			t.Errorf("%s: finished_at %d message %q", id, r.FinishedAt, r.ErrorMessage)
		}
	}
	if HTTPStatus(codemode.StatusOutcomeUnknown) != 409 {
		t.Fatal("outcome_unknown must map to 409")
	}
}

func TestPurge(t *testing.T) {
	e := newEnv(t)
	now := time.Now()
	e.svc.now = func() time.Time { return now }
	old := now.Add(-Retention - time.Hour).UnixMilli()
	e.insertState(t, "cr_old_done", codemode.StatusSucceeded, old)
	e.insertState(t, "cr_old_held", codemode.StatusPermissionRequired, old)
	e.insertState(t, "cr_old_running", StatusRunning, old)
	e.insertState(t, "cr_new", codemode.StatusSucceeded, now.Add(-time.Hour).UnixMilli())
	n, err := e.svc.Purge(context.Background())
	if err != nil || n != 2 {
		t.Fatalf("purge = %d %v, want 2", n, err)
	}
	var left []string
	rows, err := e.db.Query(`SELECT id FROM code_runs ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		left = append(left, id)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(left, []string{"cr_new", "cr_old_running"}) {
		t.Fatalf("left = %v", left)
	}
	if c := e.count(t, `SELECT count(*) FROM code_run_calls WHERE run_id IN (?, ?)`, "cr_old_done", "cr_old_held"); c != 0 {
		t.Fatalf("%d call records of purged runs remain", c)
	}
	if c := e.count(t, `SELECT count(*) FROM code_run_calls`); c != 2 {
		t.Fatalf("call records = %d, want 2", c)
	}
}

func TestGetWaitsAndReleases(t *testing.T) {
	e := newEnv(t)
	gate := make(chan struct{})
	e.f.setHandler("demo.read", func(map[string]any) (*mcp.CallToolResult, error) {
		<-gate
		return mcp.NewToolResultText(`{"n": 2}`), nil
	})
	run, _, err := e.svc.Start(context.Background(), StartRequest{AgentID: "ag_a", Code: readScript, Reason: testReason})
	if err != nil {
		t.Fatal(err)
	}
	// A short wait ends with the run still in progress.
	r, err := e.svc.Get(context.Background(), "ag_a", run.ID, 200*time.Millisecond)
	if err != nil || !InProgress(r.Status) || HTTPStatus(r.Status) != 202 {
		t.Fatalf("short wait = %+v %v", r, err)
	}
	// A long wait answers as soon as the run is done.
	go func() {
		time.Sleep(200 * time.Millisecond)
		close(gate)
	}()
	began := time.Now()
	r, err = e.svc.Get(context.Background(), "ag_a", run.ID, 30*time.Second)
	if err != nil || r.Status != codemode.StatusSucceeded {
		t.Fatalf("long wait = %+v %v", r, err)
	}
	if time.Since(began) > 20*time.Second {
		t.Fatal("the wait did not end when the run finished")
	}
	// After ReleaseWaiters a wait answers at once.
	e.svc.ReleaseWaiters()
	gate2 := make(chan struct{})
	defer close(gate2)
	e.f.setHandler("demo.read", func(map[string]any) (*mcp.CallToolResult, error) {
		<-gate2
		return mcp.NewToolResultText(`{}`), nil
	})
	run2, _, err := e.svc.Start(context.Background(), StartRequest{AgentID: "ag_a", Code: readScript, Reason: testReason})
	if err != nil {
		t.Fatal(err)
	}
	began = time.Now()
	if r, err := e.svc.Get(context.Background(), "ag_a", run2.ID, 30*time.Second); err != nil || !InProgress(r.Status) || time.Since(began) > 5*time.Second {
		t.Fatalf("released wait = %+v %v after %s", r, err, time.Since(began))
	}
}
