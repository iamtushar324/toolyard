package approval

import (
	"context"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/goroutines"
	"github.com/tusharbhardwaj/toolyard/internal/logx"
	"github.com/tusharbhardwaj/toolyard/internal/store"
	"github.com/tusharbhardwaj/toolyard/internal/store/storetest"
)

// newDrainBus opens a bus whose root context the test controls, over db
// (a fresh one when db is nil).
func newDrainBus(t *testing.T, ctx context.Context, db *store.DB) (*Bus, *store.DB) {
	t.Helper()
	if db == nil {
		db = storetest.Open(t)
	}
	bus, err := New(ctx, db)
	if err != nil {
		t.Fatalf("new bus: %v", err)
	}
	return bus, db
}

// Cancelling the root context (as every deploy used to do first) must not
// abort a write that is in flight on the executor.
func TestRootCancelDoesNotAbortInFlightExecutor(t *testing.T) {
	root, cancelRoot := context.WithCancel(context.Background())
	defer cancelRoot()
	bus, _ := newDrainBus(t, root, nil)
	started := make(chan context.Context, 1)
	release := make(chan struct{})
	bus.SetExecutor(execFunc(func(ctx context.Context, req *Request) {
		started <- ctx
		select {
		case <-release:
			_ = bus.SetResult(ctx, req.ID, `{"is_error":false}`, false, "")
		case <-ctx.Done():
		}
	}))
	req := holdPending(t, bus, "tool.slow")
	if _, err := bus.Decide(context.Background(), req.ID, StatusAllowed, "tester"); err != nil {
		t.Fatalf("decide: %v", err)
	}
	var execCtx context.Context
	select {
	case execCtx = <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("executor did not start")
	}
	cancelRoot()
	time.Sleep(20 * time.Millisecond)
	if err := execCtx.Err(); err != nil {
		t.Fatalf("executor context ended with the root context: %v", err)
	}
	close(release)
	waitFor(t, "result persisted", func() bool {
		cur, err := bus.Get(context.Background(), req.ID)
		return err == nil && cur.ResultExecutedAt > 0
	})
	cur, _ := bus.Get(context.Background(), req.ID)
	if cur.ExecutionState != "succeeded" {
		t.Fatalf("execution state = %q, want succeeded", cur.ExecutionState)
	}
	if left := bus.WaitExecutions(context.Background(), time.Second); len(left) != 0 {
		t.Fatalf("executors still running after the result: %v", left)
	}
}

// An execution still running at the drain deadline is cancelled and
// recorded outcome_unknown, and WaitExecutions names it.
func TestDrainDeadlineRecordsOutcomeUnknown(t *testing.T) {
	bus, _ := newDrainBus(t, context.Background(), nil)
	started := make(chan struct{})
	bus.SetExecutor(execFunc(func(ctx context.Context, req *Request) {
		close(started)
		<-ctx.Done() // a hung upstream; returns without a result
	}))
	req := holdPending(t, bus, "tool.hung")
	if _, err := bus.Decide(context.Background(), req.ID, StatusAllowed, "tester"); err != nil {
		t.Fatalf("decide: %v", err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("executor did not start")
	}
	bus.BeginDrain()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	left := bus.WaitExecutions(ctx, 2*time.Second)
	if len(left) != 1 || left[0] != req.ID {
		t.Fatalf("WaitExecutions = %v, want [%s]", left, req.ID)
	}
	cur, err := bus.Get(context.Background(), req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cur.ExecutionState != "outcome_unknown" || cur.ResultExecutedAt == 0 || !strings.Contains(cur.ResultError, "drain deadline") {
		t.Fatalf("state=%q executed_at=%d error=%q, want outcome_unknown at the drain deadline",
			cur.ExecutionState, cur.ResultExecutedAt, cur.ResultError)
	}
}

// During a drain a new allow is not claimed, so nothing is used up; the
// next process's startup sweep runs it exactly once.
func TestDrainRefusesNewClaimsAndNextStartRunsThem(t *testing.T) {
	bus, db := newDrainBus(t, context.Background(), nil)
	var calls atomic.Int32
	bus.SetExecutor(execFunc(func(ctx context.Context, req *Request) { calls.Add(1) }))
	req := holdPending(t, bus, "tool.later")
	bus.BeginDrain()
	if _, err := bus.Decide(context.Background(), req.ID, StatusAllowed, "tester"); err != nil {
		t.Fatalf("decide: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	if calls.Load() != 0 {
		t.Fatal("executor ran during the drain")
	}
	var claims int
	if err := db.QueryRow(`SELECT count(*) FROM legacy_execution_claims WHERE request_id=?`, req.ID).Scan(&claims); err != nil {
		t.Fatal(err)
	}
	if claims != 0 {
		t.Fatalf("claims = %d, want 0", claims)
	}

	// "Restart": a new bus over the same database.
	next, _ := newDrainBus(t, context.Background(), db)
	next.SetExecutor(execFunc(func(ctx context.Context, r *Request) {
		calls.Add(1)
		_ = next.SetResult(ctx, r.ID, `{"is_error":false}`, false, "")
	}))
	n, err := next.SweepUnexecuted(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("SweepUnexecuted = %d, %v; want 1", n, err)
	}
	waitFor(t, "executed after restart", func() bool {
		cur, err := next.Get(context.Background(), req.ID)
		return err == nil && cur.ResultExecutedAt > 0
	})
	if calls.Load() != 1 {
		t.Fatalf("executor calls = %d, want 1", calls.Load())
	}
}

// fanOut hands notifiers a context that outlives the caller's.
func TestFanOutSurvivesCallerCancel(t *testing.T) {
	bus, _ := newDrainBus(t, context.Background(), nil)
	type seen struct {
		err         error
		hasDeadline bool
	}
	got := make(chan seen, 1)
	bus.AddNotifierFunc(func(ctx context.Context, _ *Request, _ string) {
		_, ok := ctx.Deadline()
		got <- seen{err: ctx.Err(), hasDeadline: ok}
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	bus.fanOut(ctx, &Request{ID: "ap_cancelled"}, "approval.test")
	select {
	case s := <-got:
		if s.err != nil {
			t.Fatalf("notifier context already ended: %v", s.err)
		}
		if !s.hasDeadline {
			t.Fatal("notifier context has no timeout")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("notifier not called")
	}
	if left := bus.WaitExecutions(context.Background(), time.Second); len(left) != 0 {
		t.Fatalf("unexpected executors: %v", left)
	}
}

// A panicking notifier is logged at ERROR and counted; the others still run.
func TestNotifierPanicIsLoggedAndCounted(t *testing.T) {
	logx.Init(logx.FormatJSON, slog.LevelInfo)
	bus, _ := newDrainBus(t, context.Background(), nil)
	before := goroutines.Panics()
	bus.AddNotifierFunc(func(context.Context, *Request, string) { panic("notifier boom") })
	other := make(chan struct{}, 1)
	bus.AddNotifierFunc(func(context.Context, *Request, string) { other <- struct{}{} })
	bus.fanOut(context.Background(), &Request{ID: "ap_panic"}, "approval.test")
	select {
	case <-other:
	case <-time.After(3 * time.Second):
		t.Fatal("the second notifier did not run")
	}
	waitFor(t, "panic counted", func() bool { return goroutines.Panics() > before })
	waitFor(t, "panic logged", func() bool {
		snap := logx.RingSnapshot()
		return strings.Contains(snap, "goroutine panic") && strings.Contains(snap, notifierLoop) &&
			strings.Contains(snap, `"level":"ERROR"`)
	})
}
