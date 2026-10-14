package gateway

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tusharbhardwaj/toolyard/internal/approval"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/inbox"
	"github.com/tusharbhardwaj/toolyard/internal/memory"
	"github.com/tusharbhardwaj/toolyard/internal/policy"
	"github.com/tusharbhardwaj/toolyard/internal/realtime"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

func assertRestarting(t *testing.T, res *mcp.CallToolResult) {
	t.Helper()
	s := structured(t, res)
	if !res.IsError || s["status"] != "server_restarting" || s["retryable"] != true || s["executed"] != false {
		t.Fatalf("want the retryable server_restarting answer, got %v: %s", s, textOf(res))
	}
	if !strings.Contains(textOf(res), "server restarting, call again") {
		t.Fatalf("text = %q", textOf(res))
	}
}

// slowDeploy replaces deploy.run with a handler that blocks until release
// (or its context ends), and reports when a call starts.
func slowDeploy(gw *Gateway) (started chan struct{}, release chan struct{}) {
	started = make(chan struct{}, 4)
	release = make(chan struct{})
	gw.registerEntry(toolEntry{
		tool:     mcp.Tool{Name: "deploy.run", InputSchema: mcp.ToolInputSchema{Type: "object", Properties: addMetaProps(map[string]any{})}},
		upstream: "deploy", originalName: "run", reasonField: ReasonField,
		handle: func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
			started <- struct{}{}
			select {
			case <-release:
				return mcp.NewToolResultText("deployed"), nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		},
	})
	return started, release
}

// During a drain a restricted call with a valid grant gets the retryable
// refusal before the grant is redeemed; after the restart the same grant
// still works, once.
func TestDrainRefusesGrantedCallWithoutUsingTheGrant(t *testing.T) {
	f := newInboxFixture(t)
	f.mode = ApprovalModeInbox
	tok := f.approvedGrant(t)
	args := map[string]any{"service": "api", "env": "prod", GrantField: tok}

	f.gw.BeginDrain()
	res := f.call(t, f.agent, "deploy.run", args)
	assertRestarting(t, res)
	if f.calls.Load() != 0 {
		t.Fatal("the tool ran during the drain")
	}
	active, err := f.svc.ListGrants(context.Background(), inbox.GrantActive, "ag_1", 0)
	if err != nil || len(active) != 1 || active[0].Uses != 0 {
		t.Fatalf("the grant must stay active and unused, got %d active (%v)", len(active), err)
	}

	// The drain flag is process state; the grant lives in the database. A
	// fresh process starts undrained.
	f.gw.drainActive.Store(false)
	res = f.call(t, f.agent, "deploy.run", args)
	if res.IsError || f.calls.Load() != 1 {
		t.Fatalf("the grant did not work after the restart: %s", textOf(res))
	}
	res = f.call(t, f.agent, "deploy.run", args)
	if s := structured(t, res); s["status"] != "grant_invalid" || f.calls.Load() != 1 {
		t.Fatalf("the grant must still be single-use: %v", s)
	}
}

// ReleaseWaiters ends running tools.wait_for_approval(s) calls with the
// retryable answer.
func TestDrainEndsApprovalWaits(t *testing.T) {
	f := newInboxFixture(t)
	ctx := context.Background()
	req, err := f.gw.approval.Hold(ctx, approval.NewRequest{
		AgentID: "ag_1", UpstreamName: "deploy", ToolName: "deploy.run",
		Arguments: map[string]any{"service": "api"}, Reason: testReason,
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan *mcp.CallToolResult, 2)
	errs := make(chan error, 2)
	run := func(tool string, args map[string]any) {
		args[ReasonField] = testReason
		res, err := f.gw.RouteCall(f.agent, "test", tool, args)
		if err != nil {
			errs <- err
			return
		}
		results <- res
	}
	go run(MetaWaitForApproval, map[string]any{"approval_id": req.ID, "timeout_seconds": float64(60)})
	go run(MetaWaitForApprovals, map[string]any{"approval_ids": []any{req.ID}, "timeout_seconds": float64(60)})
	waitUntil(t, "both waits in flight", func() bool { return f.gw.InFlight() == 2 })
	time.Sleep(20 * time.Millisecond) // let both reach their select

	f.gw.BeginDrain()
	f.gw.ReleaseWaiters()
	for i := 0; i < 2; i++ {
		select {
		case res := <-results:
			assertRestarting(t, res)
		case err := <-errs:
			t.Fatal(err)
		case <-time.After(3 * time.Second):
			t.Fatal("a wait did not end on the drain")
		}
	}
	if cur, _ := f.gw.approval.Get(ctx, req.ID); cur.Status != approval.StatusPending {
		t.Fatalf("the approval changed: %s", cur.Status)
	}
}

// Integration: an approved write is in flight on a slow upstream when the
// drain starts (root context cancelled, as the old shutdown did first).
// The write finishes during the drain and its recorded result is the real
// one, not outcome_unknown.
func TestDrainMidDispatchKeepsTheLegacyOutcome(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "drain.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	root, cancelRoot := context.WithCancel(context.Background())
	defer cancelRoot()
	bus, err := approval.New(root, db)
	if err != nil {
		t.Fatal(err)
	}
	gw := New(Options{Policy: policy.New(db), Approval: bus, Audit: audit.New(db), Hub: realtime.NewHub(), Memory: memory.New(db)})
	gw.RegisterBuiltins()
	t.Cleanup(func() { _ = gw.Close() })
	bus.SetExecutor(gw)
	started, release := slowDeploy(gw)

	ctx := context.Background()
	req, err := bus.Hold(ctx, approval.NewRequest{
		AgentID: "ag_1", UpstreamName: "deploy", ToolName: "deploy.run",
		Arguments: map[string]any{"service": "api"}, Reason: testReason,
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bus.Decide(ctx, req.ID, approval.StatusAllowed, "tester"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("the approved write did not start")
	}

	// The shutdown order, while the write is in flight.
	gw.BeginDrain()
	bus.BeginDrain()
	gw.ReleaseWaiters()
	bus.ReleaseWaiters()
	cancelRoot()
	go func() {
		time.Sleep(50 * time.Millisecond)
		close(release) // the upstream answers during the drain
	}()
	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if left := bus.WaitExecutions(waitCtx, time.Second); len(left) != 0 {
		t.Fatalf("executions still running at the deadline: %v", left)
	}
	cur, err := bus.Get(ctx, req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cur.ExecutionState != "succeeded" || cur.ResultExecutedAt == 0 || cur.ResultIsError {
		t.Fatalf("state=%q executed_at=%d is_error=%v error=%q, want the real successful result",
			cur.ExecutionState, cur.ResultExecutedAt, cur.ResultIsError, cur.ResultError)
	}
}

// Integration, Inbox path: a granted write is in flight when the drain
// starts. New calls are refused, the running one finishes, and its
// execution records succeeded, not outcome_unknown.
func TestDrainMidDispatchKeepsTheGrantedOutcome(t *testing.T) {
	f := newInboxFixture(t)
	f.mode = ApprovalModeInbox
	tok := f.approvedGrant(t)
	started, release := slowDeploy(f.gw)

	type result struct {
		res *mcp.CallToolResult
		err error
	}
	done := make(chan result, 1)
	go func() {
		res, err := f.gw.RouteCall(f.agent, "test", "deploy.run",
			map[string]any{ReasonField: testReason, "service": "api", "env": "prod", GrantField: tok})
		done <- result{res, err}
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("the granted write did not start")
	}

	f.gw.BeginDrain()
	f.gw.ReleaseWaiters()
	f.svc.ReleaseWaiters()
	assertRestarting(t, f.call(t, f.agent, "deploy.run", map[string]any{"service": "api", "env": "prod"}))

	go func() {
		time.Sleep(50 * time.Millisecond)
		close(release)
	}()
	waitCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if left := f.gw.WaitGrantedCalls(waitCtx); left != 0 {
		t.Fatalf("granted calls still running at the deadline: %d", left)
	}
	r := <-done
	if r.err != nil || r.res == nil {
		t.Fatalf("granted call: %v", r.err)
	}
	if r.res.IsError {
		t.Fatalf("granted call failed: %s", textOf(r.res))
	}
	used, err := f.svc.ListGrants(context.Background(), inbox.GrantUsed, "ag_1", 0)
	if err != nil || len(used) != 1 {
		t.Fatalf("used grants = %d (%v), want 1", len(used), err)
	}
	ex, err := f.svc.Execution(context.Background(), used[0].ID, "ag_1")
	if err != nil {
		t.Fatal(err)
	}
	if ex.State != inbox.ExecutionSucceeded {
		t.Fatalf("execution state = %q, want succeeded", ex.State)
	}
}
