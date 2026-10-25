package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tusharbhardwaj/toolyard/internal/approval"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/jobs"
	"github.com/tusharbhardwaj/toolyard/internal/memory"
	"github.com/tusharbhardwaj/toolyard/internal/policy"
	"github.com/tusharbhardwaj/toolyard/internal/realtime"
	"github.com/tusharbhardwaj/toolyard/internal/store"
	"github.com/tusharbhardwaj/toolyard/internal/store/storetest"
)

// asyncRules is a map-based AsyncPolicy.
type asyncRules struct {
	mu sync.Mutex
	m  map[string]jobs.AsyncSetting
}

func (a *asyncRules) AsyncSetting(upstream string) jobs.AsyncSetting {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.m[upstream]
}

func (a *asyncRules) set(upstream string, s jobs.AsyncSetting) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.m[upstream] = s
}

// jobNotes records the jobs the job service reports finished.
type jobNotes struct {
	mu  sync.Mutex
	got []jobs.Job
}

func (n *jobNotes) JobFinished(_ context.Context, _ *sql.Tx, j jobs.Job) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.got = append(n.got, j)
	return nil
}

func (n *jobNotes) find(id string) (jobs.Job, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, j := range n.got {
		if j.ID == id {
			return j, true
		}
	}
	return jobs.Job{}, false
}

// jobsFixture is a gateway with async tools on and three alpha tools:
// get_slow blocks until released (or cancelled), get_fast answers at once,
// get_echo records the arguments it was sent.
type jobsFixture struct {
	gw    *Gateway
	svc   *jobs.Service
	db    *store.DB
	rules *asyncRules
	notes *jobNotes

	release       chan struct{}
	releaseOnce   sync.Once
	slowCancelled chan struct{}
	cancelOnce    sync.Once
	echoArgs      chan map[string]any
}

const jobsTestReason = "exercise async tool jobs in the gateway tests"

func newJobsFixture(t *testing.T) *jobsFixture {
	t.Helper()
	db := storetest.Open(t)
	bus, err := approval.New(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	f := &jobsFixture{
		db: db, rules: &asyncRules{m: map[string]jobs.AsyncSetting{}}, notes: &jobNotes{},
		release: make(chan struct{}), slowCancelled: make(chan struct{}), echoArgs: make(chan map[string]any, 8),
	}
	f.svc = jobs.New(db, jobs.Options{Notifier: f.notes, PollEvery: 20 * time.Millisecond})
	f.gw = New(Options{Policy: policy.New(db), Approval: bus, Audit: audit.New(db), Hub: realtime.NewHub(), Memory: memory.New(db), Jobs: f.svc})
	f.gw.RegisterBuiltins()
	f.gw.SetAsyncPolicy(f.rules)
	t.Cleanup(func() {
		f.releaseAll()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		f.svc.Wait(ctx)
		_ = f.gw.Close()
	})
	reg := func(short string, h directHandler) {
		f.gw.registerEntry(toolEntry{
			tool: mcp.Tool{Name: "alpha." + short, Description: descriptionBanner + short,
				InputSchema: mcp.ToolInputSchema{Type: "object", Properties: addMetaProps(map[string]any{})}},
			upstream: "alpha", originalName: short, reasonField: ReasonField, handle: h,
		})
	}
	reg("get_slow", func(ctx context.Context, _ map[string]any) (*mcp.CallToolResult, error) {
		select {
		case <-f.release:
			return mcp.NewToolResultText("slow done"), nil
		case <-ctx.Done():
			f.cancelOnce.Do(func() { close(f.slowCancelled) })
			return nil, ctx.Err()
		}
	})
	reg("get_fast", func(context.Context, map[string]any) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("fast"), nil
	})
	reg("get_echo", func(_ context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		cp := make(map[string]any, len(args))
		for k, v := range args {
			cp[k] = v
		}
		f.echoArgs <- cp
		return mcp.NewToolResultText("echo"), nil
	})
	return f
}

func (f *jobsFixture) releaseAll() { f.releaseOnce.Do(func() { close(f.release) }) }

func (f *jobsFixture) callAs(t *testing.T, agent, tool string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	if args == nil {
		args = map[string]any{}
	}
	if _, ok := args[ReasonField]; !ok {
		args[ReasonField] = jobsTestReason
	}
	res, err := f.gw.RouteCall(WithAgentID(context.Background(), agent), "test", tool, args)
	if err != nil || res == nil {
		t.Fatalf("%s: %v %v", tool, res, err)
	}
	return res
}

func (f *jobsFixture) call(t *testing.T, tool string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	return f.callAs(t, "ag_1", tool, args)
}

// jobIDOf reads the job envelope of a call that became a job.
func jobIDOf(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	body := structured(t, res)
	id, _ := body["job_id"].(string)
	if id == "" || res.IsError || !metaTrue(res, "toolyard.job") {
		t.Fatalf("not a job envelope: %s %v", textOf(res), body)
	}
	if body["poll"] != MetaJobsStatus || body["wait"] != MetaJobsWait {
		t.Fatalf("envelope without poll/wait: %v", body)
	}
	if st := body["status"]; st != jobs.StatusQueued && st != jobs.StatusRunning {
		t.Fatalf("envelope status = %v", st)
	}
	return id
}

func (f *jobsFixture) waitJob(t *testing.T, id string) map[string]any {
	t.Helper()
	snap := structured(t, f.call(t, MetaJobsWait, map[string]any{"job_id": id, "timeout_seconds": 30}))
	if snap["final"] != true {
		t.Fatalf("job %s not final after jobs.wait: %v", id, snap)
	}
	return snap
}

func asJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func (f *jobsFixture) jobCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM tool_jobs`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestAsyncToolReturnsAJobAndItsResult(t *testing.T) {
	f := newJobsFixture(t)
	f.rules.set("alpha", jobs.AsyncSetting{Tools: map[string]jobs.AsyncRule{"get_slow": {Mode: jobs.AsyncAlways}}})

	started := time.Now()
	id := jobIDOf(t, f.call(t, "alpha.get_slow", nil))
	if time.Since(started) > 5*time.Second {
		t.Fatal("an async call did not answer at once")
	}
	st := structured(t, f.call(t, MetaJobsStatus, map[string]any{"job_id": id}))
	if st["final"] != false || st["tool"] != "alpha.get_slow" {
		t.Fatalf("status while running = %v", st)
	}
	if early := f.call(t, MetaJobsResult, map[string]any{"job_id": id}); early.IsError || structured(t, early)["final"] != false {
		t.Fatalf("jobs.result while running = %s", textOf(early))
	}
	// Another agent cannot see it.
	if other := f.callAs(t, "ag_2", MetaJobsStatus, map[string]any{"job_id": id}); !other.IsError || !strings.Contains(textOf(other), "unknown job_id") {
		t.Fatalf("ag_2 read ag_1's job: %s", textOf(other))
	}

	f.releaseAll()
	w := f.waitJob(t, id)
	if w["status"] != jobs.StatusSucceeded || !strings.Contains(asJSON(w["result"]), "slow done") {
		t.Fatalf("jobs.wait = %v", w)
	}
	if st := structured(t, f.call(t, MetaJobsStatus, map[string]any{"job_id": id})); st["status"] != jobs.StatusSucceeded || !strings.Contains(asJSON(st["result"]), "slow done") {
		t.Fatalf("jobs.status after = %v", st)
	}
	r := f.call(t, MetaJobsResult, map[string]any{"job_id": id})
	if r.IsError || textOf(r) != "slow done" || r.Meta == nil || r.Meta.AdditionalFields["toolyard.job"] == nil {
		t.Fatalf("jobs.result = %s %+v", textOf(r), r.Meta)
	}

	// The call has its usual audit rows, and the job's own.
	rows, err := f.db.Query(`SELECT event_type FROM audit_events WHERE tool_name = ?`, "alpha.get_slow")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for rows.Next() {
		var ev string
		if err := rows.Scan(&ev); err != nil {
			t.Fatal(err)
		}
		seen[ev] = true
	}
	rows.Close()
	for _, ev := range []string{audit.EventCallStart, audit.EventCallAllowed, "call.dispatch", audit.EventCallSucceeded, eventJobStarted} {
		if !seen[ev] {
			t.Errorf("audit row %s missing (have %v)", ev, seen)
		}
	}
	if _, ok := f.notes.find(id); !ok {
		t.Error("the finished job was not reported to the notifier")
	}
	list := structured(t, f.call(t, MetaJobsList, nil))
	if list["count"] != float64(1) || !strings.Contains(asJSON(list["jobs"]), id) {
		t.Fatalf("jobs.list = %v", list)
	}
}

func TestAsyncAfterMsAnswersFastCallsInline(t *testing.T) {
	f := newJobsFixture(t)
	f.rules.set("alpha", jobs.AsyncSetting{Mode: jobs.AsyncAfter, AfterMs: 300})

	fast := f.call(t, "alpha.get_fast", nil)
	if fast.IsError || textOf(fast) != "fast" || metaTrue(fast, "toolyard.job") {
		t.Fatalf("fast call = %s", textOf(fast))
	}
	started := time.Now()
	id := jobIDOf(t, f.call(t, "alpha.get_slow", nil))
	if waited := time.Since(started); waited < 250*time.Millisecond {
		t.Fatalf("the slow call became a job after %s, before the inline wait", waited)
	}
	f.releaseAll()
	if w := f.waitJob(t, id); w["status"] != jobs.StatusSucceeded {
		t.Fatalf("slow job = %v", w)
	}
	// Only the job handed back as a job id is reported for a callback.
	list, err := f.svc.List(context.Background(), jobs.Filter{AgentID: "ag_1"})
	if err != nil || len(list) != 2 {
		t.Fatalf("jobs = %d %v", len(list), err)
	}
	for _, j := range list {
		_, noticed := f.notes.find(j.ID)
		if j.Tool == "alpha.get_fast" && noticed {
			t.Error("a call answered inline was also reported for a callback")
		}
		if j.Tool == "alpha.get_slow" && !noticed {
			t.Error("the job handed back as a job id was not reported")
		}
	}
}

func TestJobsCancelStopsTheRunningCall(t *testing.T) {
	f := newJobsFixture(t)
	f.rules.set("alpha", jobs.AsyncSetting{Tools: map[string]jobs.AsyncRule{"get_slow": {Mode: jobs.AsyncAlways}}})
	id := jobIDOf(t, f.call(t, "alpha.get_slow", nil))
	deadline := time.Now().Add(10 * time.Second)
	for structured(t, f.call(t, MetaJobsStatus, map[string]any{"job_id": id}))["status"] != jobs.StatusRunning {
		if time.Now().After(deadline) {
			t.Fatal("the job never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	c := structured(t, f.call(t, MetaJobsCancel, map[string]any{"job_id": id}))
	if c["cancel_accepted"] != true {
		t.Fatalf("jobs.cancel = %v", c)
	}
	select {
	case <-f.slowCancelled:
	case <-time.After(10 * time.Second):
		t.Fatal("the upstream call's context was not cancelled")
	}
	w := f.waitJob(t, id)
	if w["status"] != jobs.StatusCancelled || w["may_have_run"] != true || !strings.Contains(asJSON(w), "may already have taken effect") {
		t.Fatalf("cancelled job = %v", w)
	}
	again := structured(t, f.call(t, MetaJobsCancel, map[string]any{"job_id": id}))
	if again["cancel_accepted"] != false {
		t.Fatalf("second cancel = %v", again)
	}
}

func TestCallbackRefReachesTheNotifierAndNotTheUpstream(t *testing.T) {
	f := newJobsFixture(t)
	f.rules.set("alpha", jobs.AsyncSetting{Tools: map[string]jobs.AsyncRule{"get_echo": {Mode: jobs.AsyncAlways}}})
	id := jobIDOf(t, f.call(t, "alpha.get_echo", map[string]any{callbackRefArg: "cb_test", "x": "1"}))
	f.waitJob(t, id)
	args := <-f.echoArgs
	if _, leaked := args[callbackRefArg]; leaked || args["x"] != "1" {
		t.Fatalf("upstream saw %v", args)
	}
	j, ok := f.notes.find(id)
	if !ok || j.CallbackRef != "cb_test" || j.Status != jobs.StatusSucceeded {
		t.Fatalf("notifier got %+v (%v)", j, ok)
	}
	// A call that runs inline never forwards it either.
	f.rules.set("alpha", jobs.AsyncSetting{})
	if res := f.call(t, "alpha.get_echo", map[string]any{callbackRefArg: "cb_test"}); res.IsError {
		t.Fatal(textOf(res))
	}
	if args := <-f.echoArgs; args[callbackRefArg] != nil {
		t.Fatalf("an inline call forwarded _callback_ref: %v", args)
	}
}

func TestPolicyDenyRefusesAnAsyncCallLikeASyncOne(t *testing.T) {
	f := newJobsFixture(t)
	ctx := context.Background()
	if _, err := f.gw.policy.Set(ctx, policy.ScopeTool, "alpha.get_echo", "deny", "", false); err != nil {
		t.Fatal(err)
	}
	syncRes := f.call(t, "alpha.get_echo", nil)
	if !syncRes.IsError || !strings.Contains(textOf(syncRes), "denied by policy") {
		t.Fatalf("sync call = %s", textOf(syncRes))
	}
	f.rules.set("alpha", jobs.AsyncSetting{Mode: jobs.AsyncAlways})
	asyncRes := f.call(t, "alpha.get_echo", nil)
	if !asyncRes.IsError || textOf(asyncRes) != textOf(syncRes) || metaTrue(asyncRes, "toolyard.job") {
		t.Fatalf("async call = %s, want %s", textOf(asyncRes), textOf(syncRes))
	}
	started := f.call(t, MetaJobsStart, map[string]any{"tool": "alpha.get_echo", "arguments": map[string]any{}})
	if !started.IsError || !strings.Contains(textOf(started), "denied by policy") {
		t.Fatalf("jobs.start of a denied tool = %s", textOf(started))
	}
	if n := f.jobCount(t); n != 0 {
		t.Fatalf("%d jobs stored for denied calls", n)
	}
	select {
	case args := <-f.echoArgs:
		t.Fatalf("a denied tool ran with %v", args)
	default:
	}
}

func TestJobsStartRunsAnyToolAsAJob(t *testing.T) {
	f := newJobsFixture(t)
	res := f.call(t, MetaJobsStart, map[string]any{"tool": "alpha.get_fast", "arguments": map[string]any{}})
	id := jobIDOf(t, res)
	w := f.waitJob(t, id)
	if w["status"] != jobs.StatusSucceeded || w["via"] != jobViaStart || !strings.Contains(asJSON(w["result"]), "fast") {
		t.Fatalf("jobs.start job = %v", w)
	}
	if bad := f.call(t, MetaJobsStart, map[string]any{"tool": MetaJobsWait}); !bad.IsError {
		t.Fatalf("a jobs tool ran as a job: %s", textOf(bad))
	}
	wait := structured(t, f.call(t, MetaJobsWait, map[string]any{"job_ids": []any{id, "job_missing"}, "timeout_seconds": 1}))
	if wait["count"] != float64(2) || wait["any_final"] != true || !strings.Contains(asJSON(wait["results"]), `"unknown"`) {
		t.Fatalf("multi wait = %v", wait)
	}
}

func TestJobsAreOffWithoutAJobService(t *testing.T) {
	db := storetest.Open(t)
	gw := New(Options{Policy: policy.New(db), Audit: audit.New(db), Hub: realtime.NewHub(), Memory: memory.New(db)})
	gw.RegisterBuiltins()
	t.Cleanup(func() { _ = gw.Close() })
	for _, n := range jobToolNames {
		if gw.HasTool(n) {
			t.Fatalf("%s registered without a job service", n)
		}
	}
	if !reservedUpstreamName(jobsUpstream) {
		t.Fatal("jobs must be a reserved upstream name")
	}
}
