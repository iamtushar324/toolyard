package approval

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/cluster"
	"github.com/tusharbhardwaj/toolyard/internal/store"
	"github.com/tusharbhardwaj/toolyard/internal/store/storetest"
)

// twoBuses is two replicas' buses on one Postgres database.
func twoBuses(t *testing.T) (*Bus, *Bus, *store.DB) {
	t.Helper()
	storetest.SkipUnlessPostgres(t)
	db := storetest.Open(t)
	mk := func(id string) *Bus {
		ctx, cancel := context.WithCancel(context.Background())
		c, err := cluster.New(ctx, db, cluster.Options{InstanceID: id, ResyncInterval: -1})
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		t.Cleanup(func() {
			cancel()
			_ = c.Close()
		})
		b, err := New(context.Background(), db)
		if err != nil {
			t.Fatal(err)
		}
		b.SetCluster(c)
		return b
	}
	return mk("node-1"), mk("node-2"), db
}

// A rolling restart of node 1 (its startup recovery, SweepUnexecuted) does
// not touch node 2's running legacy execution and never runs it again; a
// claim whose owner stopped (lease ended) becomes outcome_unknown.
func TestRollingRestartKeepsOtherNodesClaims(t *testing.T) {
	bus1, bus2, db := twoBuses(t)
	ctx := context.Background()

	started := make(chan struct{})
	release := make(chan struct{})
	bus2.SetExecutor(execFunc(func(ctx context.Context, req *Request) {
		close(started)
		<-release
		_ = bus2.SetResult(ctx, req.ID, `{"is_error":false}`, false, "")
	}))
	live := holdPending(t, bus2, "tool.live")
	if _, err := bus2.Decide(ctx, live.ID, StatusAllowed, "tester"); err != nil {
		t.Fatalf("decide on node 2: %v", err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("node 2's executor did not start")
	}
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()

	// A request a stopped node had claimed (its lease ended).
	dead := holdPending(t, bus1, "tool.dead")
	if _, err := bus1.Decide(ctx, dead.ID, StatusAllowed, "tester"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO legacy_execution_claims(request_id,state,claimed_at,owner_instance,lease_until) VALUES(?,'running',?,?,?)`,
		dead.ID, time.Now().Add(-2*time.Minute).UnixMilli(), "node-dead", time.Now().Add(-time.Minute).UnixMilli()); err != nil {
		t.Fatal(err)
	}

	// Node 1 restarts: its recovery sweep runs with an executor installed.
	var ran atomic.Int32
	bus1.SetExecutor(execFunc(func(ctx context.Context, req *Request) { ran.Add(1) }))
	n, err := bus1.SweepUnexecuted(ctx)
	if err != nil {
		t.Fatalf("sweep on node 1: %v", err)
	}
	if n != 0 {
		t.Fatalf("node 1 started %d executors, want 0", n)
	}
	time.Sleep(100 * time.Millisecond)
	if ran.Load() != 0 {
		t.Fatal("node 1 ran a claimed execution again")
	}
	cur, err := bus1.Get(ctx, live.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cur.ExecutionState != "running" || cur.ResultExecutedAt != 0 {
		t.Fatalf("node 2's running execution after node 1's restart: state %q executed_at %d", cur.ExecutionState, cur.ResultExecutedAt)
	}
	gone, err := bus1.Get(ctx, dead.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gone.ExecutionState != "outcome_unknown" || gone.ResultExecutedAt == 0 {
		t.Fatalf("stopped node's claim: state %q executed_at %d, want outcome_unknown", gone.ExecutionState, gone.ResultExecutedAt)
	}

	// Node 2 finishes and records its result.
	close(release)
	waitFor(t, "node 2's result", func() bool {
		cur, err := bus1.Get(ctx, live.ID)
		return err == nil && cur.ResultExecutedAt > 0 && cur.ExecutionState == "succeeded"
	})
}

// An in-line Hold on node 1 wakes when node 2 decides.
func TestHoldWakesOnOtherNodesDecision(t *testing.T) {
	bus1, bus2, _ := twoBuses(t)
	ctx := context.Background()
	type result struct {
		req  *Request
		err  error
		took time.Duration
	}
	done := make(chan result, 1)
	go func() {
		start := time.Now()
		req, err := bus1.Hold(ctx, NewRequest{AgentID: "agent-1", UpstreamName: "up", ToolName: "tool.wait", Arguments: map[string]any{"k": 1}}, 30*time.Second)
		done <- result{req, err, time.Since(start)}
	}()
	var id string
	waitFor(t, "the request to exist", func() bool {
		pending, err := bus2.ListPending(ctx)
		if err != nil || len(pending) != 1 {
			return false
		}
		id = pending[0].ID
		return true
	})
	// Hold registers its waiter right after it creates the row.
	waitFor(t, "node 1's waiter", func() bool {
		_, ok := bus1.Watch(id)
		return ok
	})
	if _, err := bus2.Decide(ctx, id, StatusDenied, "tester"); err != nil {
		t.Fatalf("decide on node 2: %v", err)
	}
	select {
	case res := <-done:
		if res.err != nil || res.req == nil || res.req.Status != StatusDenied {
			t.Fatalf("hold returned %+v %v", res.req, res.err)
		}
		t.Logf("node 1's hold returned %s after node 2's decision", res.took)
	case <-time.After(10 * time.Second):
		t.Fatal("node 1's hold did not wake after node 2 decided")
	}
}

// The in-line auto-approval path claims the execution, so a recovery
// sweep never runs it a second time.
func TestInlineAutoApprovalIsClaimed(t *testing.T) {
	bus := newTestBus(t)
	bus.SetAutoApprover(alwaysAuto{})
	ctx := context.Background()
	req, err := bus.Hold(ctx, NewRequest{AgentID: "a", UpstreamName: "u", ToolName: "t.inline", Arguments: map[string]any{"x": 1}}, 0)
	if err != nil || req.Status != StatusAllowed {
		t.Fatalf("hold: %+v %v", req, err)
	}
	var ran atomic.Int32
	bus.SetExecutor(execFunc(func(ctx context.Context, r *Request) { ran.Add(1) }))
	if n, err := bus.SweepUnexecuted(ctx); err != nil || n != 0 {
		t.Fatalf("sweep: %d %v", n, err)
	}
	time.Sleep(50 * time.Millisecond)
	if ran.Load() != 0 {
		t.Fatal("the sweep ran an in-line call again")
	}
	cur, err := bus.Get(ctx, req.ID)
	if err != nil {
		t.Fatal(err)
	}
	// A single node's sweep treats the claim as a stopped process's.
	if cur.ExecutionState != "outcome_unknown" {
		t.Fatalf("execution state %q, want outcome_unknown", cur.ExecutionState)
	}
}
