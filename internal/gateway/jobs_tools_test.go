package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tusharbhardwaj/toolyard/internal/access"
	"github.com/tusharbhardwaj/toolyard/internal/actor"
	"github.com/tusharbhardwaj/toolyard/internal/approval"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/codemode"
	"github.com/tusharbhardwaj/toolyard/internal/inbox"
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
	inbox *inbox.Service
	// deployCalls counts deploy.run calls (Inbox fixtures only).
	deployCalls atomic.Int32

	release       chan struct{}
	releaseOnce   sync.Once
	slowStarted   chan struct{}
	startedOnce   sync.Once
	slowCancelled chan struct{}
	cancelOnce    sync.Once
	echoArgs      chan map[string]any
}

const jobsTestReason = "exercise async tool jobs in the gateway tests"

// jobsFixtureConfig varies newJobsFixture: an access resolver, and an
// Inbox with a restricted deploy.run tool.
type jobsFixtureConfig struct {
	resolver access.Resolver
	inbox    bool
	jobOpts  func(*jobs.Options)
	// safeNames turns on safe tool names (tool_names.go).
	safeNames bool
}

func newJobsFixture(t *testing.T, opts ...func(*jobsFixtureConfig)) *jobsFixture {
	t.Helper()
	var cfg jobsFixtureConfig
	for _, o := range opts {
		o(&cfg)
	}
	db := storetest.Open(t)
	bus, err := approval.New(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	f := &jobsFixture{
		db: db, rules: &asyncRules{m: map[string]jobs.AsyncSetting{}}, notes: &jobNotes{},
		release: make(chan struct{}), slowStarted: make(chan struct{}), slowCancelled: make(chan struct{}), echoArgs: make(chan map[string]any, 8),
	}
	jopts := jobs.Options{Notifier: f.notes, PollEvery: 20 * time.Millisecond}
	if cfg.jobOpts != nil {
		cfg.jobOpts(&jopts)
	}
	f.svc = jobs.New(db, jopts)
	gopts := Options{Policy: policy.New(db), Approval: bus, Audit: audit.New(db), Hub: realtime.NewHub(), Memory: memory.New(db), Jobs: f.svc}
	if cfg.resolver != nil {
		gopts.Access = cfg.resolver
	}
	gopts.SafeToolNames = cfg.safeNames
	f.gw = New(gopts)
	f.gw.RegisterBuiltins()
	f.gw.SetAsyncPolicy(f.rules)
	if cfg.inbox {
		f.inbox, err = inbox.New(context.Background(), inbox.Options{DB: db, Catalog: f.gw})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(f.inbox.Flush)
		f.gw.SetInbox(f.inbox, inbox.NewGuide(nil), func() string { return ApprovalModeExecute })
		f.gw.RegisterInboxTools()
		f.gw.registerEntry(toolEntry{
			tool:     mcp.Tool{Name: "deploy.run", InputSchema: mcp.ToolInputSchema{Type: "object", Properties: addMetaProps(map[string]any{})}},
			upstream: "deploy", originalName: "run", reasonField: ReasonField,
			handle: func(context.Context, map[string]any) (*mcp.CallToolResult, error) {
				f.deployCalls.Add(1)
				return mcp.NewToolResultText("deployed"), nil
			},
		})
	}
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
		f.startedOnce.Do(func() { close(f.slowStarted) })
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
	f.gw.registerEntry(toolEntry{
		tool: mcp.Tool{Name: "beta.get_secret", Description: descriptionBanner + "secret",
			InputSchema: mcp.ToolInputSchema{Type: "object", Properties: addMetaProps(map[string]any{})}},
		upstream: "beta", originalName: "get_secret", reasonField: ReasonField,
		handle: func(context.Context, map[string]any) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText("secret"), nil
		},
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

// callOwned calls as agent, owned by the dashboard user owner.
func (f *jobsFixture) callOwned(t *testing.T, agent, owner, tool string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	if args == nil {
		args = map[string]any{}
	}
	if _, ok := args[ReasonField]; !ok {
		args[ReasonField] = jobsTestReason
	}
	ctx := actor.WithRaiser(WithAgentID(context.Background(), agent), actor.Raiser{CallerID: agent, OwnerUserID: owner})
	res, err := f.gw.RouteCall(ctx, "test", tool, args)
	if err != nil || res == nil {
		t.Fatalf("%s: %v %v", tool, res, err)
	}
	return res
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
	// A fast call stores no job at all; the slow one is a job, reported
	// for its callback.
	list, err := f.svc.List(context.Background(), jobs.Filter{AgentID: "ag_1"})
	if err != nil || len(list) != 1 || list[0].ID != id {
		t.Fatalf("jobs = %+v %v", list, err)
	}
	if _, noticed := f.notes.find(id); !noticed {
		t.Error("the job handed back as a job id was not reported")
	}
}

func TestJobsCancelStopsTheRunningCall(t *testing.T) {
	f := newJobsFixture(t)
	f.rules.set("alpha", jobs.AsyncSetting{Tools: map[string]jobs.AsyncRule{"get_slow": {Mode: jobs.AsyncAlways}}})
	id := jobIDOf(t, f.call(t, "alpha.get_slow", nil))
	// The upstream call is in flight before the cancel.
	select {
	case <-f.slowStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("the job's call never started")
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

func withInbox(c *jobsFixtureConfig) { c.inbox = true }

func withResolver(r access.Resolver) func(*jobsFixtureConfig) {
	return func(c *jobsFixtureConfig) { c.resolver = r }
}

// grantFor asks for and approves one deploy.run call for ag_1, and
// returns its grant token.
func (f *jobsFixture) grantFor(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	s := structured(t, f.call(t, "inbox.request", requestArgs(map[string]any{"service": "api", "env": "prod"})))
	id, _ := s["request_id"].(string)
	if s["ok"] != true || id == "" {
		t.Fatalf("request: %v", s)
	}
	f.inbox.Flush()
	if _, err := f.inbox.Decide(ctx, id, inbox.Decision{Action: "submit", RequestRevision: 1, SubmissionID: "jobs_test",
		Verdicts: map[string]inbox.CallVerdict{"deploy_api": {Verdict: inbox.VerdictAccepted}}}); err != nil {
		t.Fatal(err)
	}
	w := structured(t, f.call(t, "inbox.wait", map[string]any{"ids": []any{id}, "timeout_seconds": 1}))
	tok, _ := w["requests"].([]any)[0].(map[string]any)["tools"].([]any)[0].(map[string]any)["grant"].(string)
	if !strings.HasPrefix(tok, "tyg_") {
		t.Fatalf("no grant: %v", w)
	}
	return tok
}

// An async tool under an approve policy answers permission_required and
// starts no job; with a grant, the grant is redeemed once, inside the job.
func TestAsyncToolNeedsPermissionAndRedeemsItsGrantInTheJob(t *testing.T) {
	f := newJobsFixture(t, withInbox)
	f.rules.set("deploy", jobs.AsyncSetting{Mode: jobs.AsyncAlways})
	deploy := map[string]any{"service": "api", "env": "prod"}

	res := f.call(t, "deploy.run", map[string]any{"service": "api", "env": "prod"})
	if s := structured(t, res); s["status"] != "permission_required" || metaTrue(res, "toolyard.job") {
		t.Fatalf("restricted async call = %v", s)
	}
	if n := f.jobCount(t); n != 0 || f.deployCalls.Load() != 0 {
		t.Fatalf("restricted call stored %d jobs, ran %d times", n, f.deployCalls.Load())
	}

	tok := f.grantFor(t)
	granted := map[string]any{GrantField: tok}
	for k, v := range deploy {
		granted[k] = v
	}
	id := jobIDOf(t, f.call(t, "deploy.run", granted))
	if w := f.waitJob(t, id); w["status"] != jobs.StatusSucceeded || !strings.Contains(asJSON(w["result"]), "deployed") {
		t.Fatalf("granted job = %v", w)
	}
	if f.deployCalls.Load() != 1 {
		t.Fatalf("deploy ran %d times", f.deployCalls.Load())
	}
	// The grant is checked before a job is stored: a used one, or a
	// made-up one, answers grant_invalid at once, starts no job and runs
	// nothing.
	jobsBefore := f.jobCount(t)
	for _, bad := range []string{tok, "tyg_not.a-real-grant"} {
		again := map[string]any{GrantField: bad}
		for k, v := range deploy {
			again[k] = v
		}
		res := f.call(t, "deploy.run", again)
		if s := structured(t, res); s["status"] != "grant_invalid" || metaTrue(res, "toolyard.job") {
			t.Fatalf("call with grant %q = %v", bad, s)
		}
	}
	if n := f.jobCount(t); n != jobsBefore {
		t.Fatalf("invalid grants stored %d jobs", n-jobsBefore)
	}
	if f.deployCalls.Load() != 1 {
		t.Fatalf("a used grant ran the tool again (%d calls)", f.deployCalls.Load())
	}
}

func scopeResolver() *fakeResolver {
	return &fakeResolver{scopes: map[string]access.Scope{
		"ag_1":           {Groups: map[string]bool{"alpha": true}},
		"ag_2":           {Groups: map[string]bool{"alpha": true}},
		"dashboard:u_1":  {Groups: map[string]bool{"alpha": true}},
		"dashboard:boss": {All: true},
	}}
}

// jobs.start, an MCP task and a direct call on a tool the caller was
// never granted answer like an unknown tool and store nothing.
func TestUngrantedToolsStartNoJob(t *testing.T) {
	f := newJobsFixture(t, withResolver(scopeResolver()))
	f.rules.set("beta", jobs.AsyncSetting{Mode: jobs.AsyncAlways})
	if res := f.call(t, MetaJobsStart, map[string]any{"tool": "beta.get_secret", "arguments": map[string]any{}}); !res.IsError || !strings.Contains(textOf(res), "not found") {
		t.Fatalf("jobs.start of an ungranted tool = %s", textOf(res))
	}
	if res := f.call(t, "beta.get_secret", nil); !res.IsError || !strings.Contains(textOf(res), "not found") {
		t.Fatalf("async call of an ungranted tool = %s", textOf(res))
	}
	c := newTasksServer(t, f, "ag_1", false)
	c.initialize()
	out := c.send("tools/call", map[string]any{"name": "beta.get_secret", "arguments": map[string]any{ReasonField: jobsTestReason}, "task": map[string]any{}}, false)
	if errObj, _ := out["error"].(map[string]any); errObj == nil || !strings.Contains(asJSON(errObj), "not found") {
		t.Fatalf("task for an ungranted tool = %v", out)
	}
	if n := f.jobCount(t); n != 0 {
		t.Fatalf("%d jobs stored for ungranted tools", n)
	}
}

// Agent B reads, waits on and cancels agent A's job as an unknown id,
// over the jobs.* tools and over MCP Tasks.
func TestAnotherAgentCannotTouchAJob(t *testing.T) {
	f := newJobsFixture(t)
	f.rules.set("alpha", jobs.AsyncSetting{Tools: map[string]jobs.AsyncRule{"get_slow": {Mode: jobs.AsyncAlways}}})
	id := jobIDOf(t, f.call(t, "alpha.get_slow", nil))
	for _, tool := range []string{MetaJobsStatus, MetaJobsResult, MetaJobsWait, MetaJobsCancel} {
		args := map[string]any{"job_id": id}
		if tool == MetaJobsWait {
			args["timeout_seconds"] = 1
		}
		if res := f.callAs(t, "ag_2", tool, args); !res.IsError || !strings.Contains(textOf(res), "unknown job_id") {
			t.Errorf("ag_2 %s = %s", tool, textOf(res))
		}
	}
	if list := structured(t, f.callAs(t, "ag_2", MetaJobsList, nil)); list["count"] != float64(0) {
		t.Errorf("ag_2 lists ag_1's jobs: %v", list)
	}
	c := newTasksServer(t, f, "ag_2", false)
	c.initialize()
	for _, method := range []string{"tasks/get", "tasks/cancel", "tasks/result"} {
		if out := c.send(method, map[string]any{"taskId": id}, false); out["error"] == nil {
			t.Errorf("ag_2 %s = %v", method, out)
		}
	}
	if j, _ := f.svc.Get(context.Background(), id); j.Final() || j.CancelRequested {
		t.Fatalf("ag_2 changed ag_1's job: %+v", j)
	}
}

// A dashboard member sees only their own agents' jobs; an admin sees all.
func TestDashboardCallersSeeTheirAgentsJobs(t *testing.T) {
	f := newJobsFixture(t, withResolver(scopeResolver()))
	mine := jobIDOf(t, f.callOwned(t, "ag_1", "u_1", MetaJobsStart, map[string]any{"tool": "alpha.get_fast", "arguments": map[string]any{}}))
	theirs := jobIDOf(t, f.callOwned(t, "ag_2", "u_2", MetaJobsStart, map[string]any{"tool": "alpha.get_fast", "arguments": map[string]any{}}))
	f.waitJob(t, mine)
	if w := structured(t, f.callAs(t, "ag_2", MetaJobsWait, map[string]any{"job_id": theirs, "timeout_seconds": 30})); w["final"] != true {
		t.Fatalf("ag_2's job = %v", w)
	}
	list := structured(t, f.callAs(t, "dashboard:u_1", MetaJobsList, nil))
	if list["count"] != float64(1) || !strings.Contains(asJSON(list["jobs"]), mine) {
		t.Fatalf("member's jobs.list = %v", list)
	}
	if st := f.callAs(t, "dashboard:u_1", MetaJobsStatus, map[string]any{"job_id": theirs}); !st.IsError {
		t.Fatalf("member read another owner's job: %s", textOf(st))
	}
	if st := f.callAs(t, "dashboard:u_1", MetaJobsStatus, map[string]any{"job_id": mine}); st.IsError {
		t.Fatalf("member cannot read their agent's job: %s", textOf(st))
	}
	if all := structured(t, f.callAs(t, "dashboard:boss", MetaJobsList, nil)); all["count"] != float64(2) {
		t.Fatalf("admin's jobs.list = %v", all)
	}
}

// A queued job checks access and policy again when it starts.
func TestQueuedJobRechecksPolicyWhenItStarts(t *testing.T) {
	f := newJobsFixture(t)
	ctx := context.Background()
	f.rules.set("alpha", jobs.AsyncSetting{Mode: jobs.AsyncAlways})
	c := jobCall{entry: f.gw.tools["alpha.get_echo"], args: map[string]any{}, agentID: "ag_1", reason: jobsTestReason}
	if r := f.gw.recheckJob(ctx, c); r != "" {
		t.Fatalf("recheck of an allowed call = %q", r)
	}
	if _, err := f.gw.policy.Set(ctx, policy.ScopeTool, "alpha.get_echo", "deny", "", false); err != nil {
		t.Fatal(err)
	}
	if r := f.gw.recheckJob(ctx, c); !strings.Contains(r, "denied by policy") {
		t.Fatalf("recheck after a deny = %q", r)
	}
	if _, err := f.gw.policy.Set(ctx, policy.ScopeTool, "alpha.get_echo", "ask", "", false); err != nil {
		t.Fatal(err)
	}
	if r := f.gw.recheckJob(ctx, c); !strings.Contains(r, "needs approval") {
		t.Fatalf("recheck after an ask = %q", r)
	}
	c.grant = true
	if r := f.gw.recheckJob(ctx, c); r != "" {
		t.Fatalf("a granted call refused after an ask: %q", r)
	}
}

// When the job budget is full, a tool the administrator made async runs
// inline instead of failing.
func TestFullBudgetRunsAsyncToolsInline(t *testing.T) {
	db := storetest.Open(t)
	bus, err := approval.New(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	svc := jobs.New(db, jobs.Options{MaxActive: 1, PollEvery: 20 * time.Millisecond})
	gw := New(Options{Policy: policy.New(db), Approval: bus, Audit: audit.New(db), Hub: realtime.NewHub(), Memory: memory.New(db), Jobs: svc})
	gw.RegisterBuiltins()
	release := make(chan struct{})
	t.Cleanup(func() {
		close(release)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		svc.Wait(ctx)
		_ = gw.Close()
	})
	rules := &asyncRules{m: map[string]jobs.AsyncSetting{"alpha": {Mode: jobs.AsyncAlways}}}
	gw.SetAsyncPolicy(rules)
	for _, short := range []string{"get_slow", "get_fast"} {
		short := short
		gw.registerEntry(toolEntry{
			tool:     mcp.Tool{Name: "alpha." + short, InputSchema: mcp.ToolInputSchema{Type: "object", Properties: addMetaProps(map[string]any{})}},
			upstream: "alpha", originalName: short, reasonField: ReasonField,
			handle: func(ctx context.Context, _ map[string]any) (*mcp.CallToolResult, error) {
				if short == "get_slow" {
					select {
					case <-release:
					case <-ctx.Done():
					}
				}
				return mcp.NewToolResultText(short + " done"), nil
			},
		})
	}
	ctx := WithAgentID(context.Background(), "ag_1")
	first, _ := gw.RouteCall(ctx, "test", "alpha.get_slow", map[string]any{ReasonField: jobsTestReason})
	if !metaTrue(first, "toolyard.job") {
		t.Fatalf("first call = %s", textOf(first))
	}
	second, _ := gw.RouteCall(ctx, "test", "alpha.get_fast", map[string]any{ReasonField: jobsTestReason})
	if second.IsError || metaTrue(second, "toolyard.job") || textOf(second) != "get_fast done" {
		t.Fatalf("call over the budget = %s", textOf(second))
	}
	// An explicit job request over the budget is refused.
	explicit, _ := gw.RouteCall(ctx, "test", MetaJobsStart, map[string]any{ReasonField: jobsTestReason, "tool": "alpha.get_fast", "arguments": map[string]any{}})
	if !explicit.IsError || structured(t, explicit)["status"] != "job_budget_exceeded" {
		t.Fatalf("jobs.start over the budget = %s", textOf(explicit))
	}
}

// A job a code-mode script starts keeps the script's mark and run id, in
// both async modes: the job cannot start code mode, and its audit rows
// name the run. jobs.start of a code-mode tool from a script is refused.
func TestJobsFromCodeModeKeepTheScriptContext(t *testing.T) {
	f := newJobsFixture(t)
	nested := make(chan *mcp.CallToolResult, 4)
	f.gw.registerEntry(toolEntry{
		tool: mcp.Tool{Name: "alpha.get_nested", Description: descriptionBanner + "nested",
			InputSchema: mcp.ToolInputSchema{Type: "object", Properties: addMetaProps(map[string]any{})}},
		upstream: "alpha", originalName: "get_nested", reasonField: ReasonField,
		handle: func(ctx context.Context, _ map[string]any) (*mcp.CallToolResult, error) {
			res, _ := f.gw.RouteCall(ctx, "test", CodeModeExecuteToolCode, map[string]any{"code": "result = 1"})
			nested <- res
			time.Sleep(100 * time.Millisecond) // past the `after` wait below
			return mcp.NewToolResultText("nested done"), nil
		},
	})
	script := func(runID string) context.Context {
		ctx := actor.WithRaiser(WithAgentID(context.Background(), "ag_1"), actor.Raiser{CallerID: "ag_1", Via: codemode.Via})
		return audit.WithRunID(withinCodeMode(ctx), runID)
	}
	for _, mode := range []jobs.AsyncRule{{Mode: jobs.AsyncAlways}, {Mode: jobs.AsyncAfter, AfterMs: 10}} {
		runID := "run_" + mode.Mode
		f.rules.set("alpha", jobs.AsyncSetting{Tools: map[string]jobs.AsyncRule{"get_nested": mode}})
		res, err := f.gw.RouteCall(script(runID), codemode.Via, "alpha.get_nested", map[string]any{ReasonField: jobsTestReason})
		if err != nil {
			t.Fatal(err)
		}
		id := jobIDOf(t, res)
		select {
		case got := <-nested:
			if !got.IsError || !strings.Contains(textOf(got), "cannot be called from code mode") {
				t.Fatalf("%s: executeToolCode inside a script's job = %s", mode.Mode, textOf(got))
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s: the job's call never ran", mode.Mode)
		}
		if w := f.waitJob(t, id); w["status"] != jobs.StatusSucceeded {
			t.Fatalf("%s: job = %v", mode.Mode, w)
		}
		var n int
		if err := f.db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE tool_name = ? AND event_type = 'call.dispatch' AND run_id = ?`,
			"alpha.get_nested", runID).Scan(&n); err != nil || n != 1 {
			t.Fatalf("%s: dispatch audit rows with the script's run id = %d (%v)", mode.Mode, n, err)
		}
	}
	started, err := f.gw.RouteCall(script("run_start"), codemode.Via, MetaJobsStart, map[string]any{
		ReasonField: jobsTestReason, "tool": CodeModeExecuteToolCode, "arguments": map[string]any{"code": "result = 1"}})
	if err != nil || !started.IsError || !strings.Contains(textOf(started), "cannot be called from code mode") {
		t.Fatalf("jobs.start of executeToolCode from a script = %s %v", textOf(started), err)
	}
}
