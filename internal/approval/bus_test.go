package approval

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// newTestBus spins up a temporary sqlite-backed Bus. Auto-approval is left
// nil so Hold()'d requests stay pending until explicitly decided.
func newTestBus(t *testing.T) *Bus {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	bus, err := New(context.Background(), db)
	if err != nil {
		t.Fatalf("new bus: %v", err)
	}
	return bus
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

// execFunc adapts a function to the Executor interface.
type execFunc func(ctx context.Context, req *Request)

func (f execFunc) Execute(ctx context.Context, req *Request) { f(ctx, req) }

func holdPending(t *testing.T, bus *Bus, tool string) *Request {
	t.Helper()
	req, err := bus.Hold(context.Background(), NewRequest{
		AgentID:      "agent-1",
		UpstreamName: "up",
		ToolName:     tool,
		Arguments:    map[string]any{"k": tool},
	}, 0)
	if err != nil {
		t.Fatalf("hold %s: %v", tool, err)
	}
	if req.Status != StatusPending {
		t.Fatalf("hold %s: status = %q, want pending", tool, req.Status)
	}
	return req
}

// TestRunExecutorPanic_PersistsErrorResult verifies a panicking executor
// doesn't leave the row unexecuted forever — the deferred recover records a
// terminal error result so pollers stop spinning.
func TestRunExecutorPanic_PersistsErrorResult(t *testing.T) {
	bus := newTestBus(t)
	ctx := context.Background()
	bus.SetExecutor(execFunc(func(ctx context.Context, req *Request) {
		panic("boom")
	}))

	req := holdPending(t, bus, "tool.panic")
	if _, err := bus.Decide(ctx, req.ID, StatusAllowed, "tester"); err != nil {
		t.Fatalf("decide: %v", err)
	}

	waitFor(t, "panic result persisted", func() bool {
		cur, err := bus.Get(ctx, req.ID)
		return err == nil && cur.ResultExecutedAt > 0
	})
	cur, _ := bus.Get(ctx, req.ID)
	if cur.ResultError == "" || !contains(cur.ResultError, "panic") {
		t.Errorf("ResultError = %q, want it to mention the panic", cur.ResultError)
	}
}

// TestSetResult_Idempotent verifies the result-executed-at gate: the first
// SetResult wins and a second is a no-op.
func TestSetResult_Idempotent(t *testing.T) {
	bus := newTestBus(t)
	ctx := context.Background()

	req := holdPending(t, bus, "tool.idem")
	// No executor registered → Decide leaves the row allowed + unexecuted.
	if _, err := bus.Decide(ctx, req.ID, StatusAllowed, "tester"); err != nil {
		t.Fatalf("decide: %v", err)
	}

	if err := bus.SetResult(ctx, req.ID, "first", false, ""); err != nil {
		t.Fatalf("first SetResult: %v", err)
	}
	if err := bus.SetResult(ctx, req.ID, "second", true, "override"); err != nil {
		t.Fatalf("second SetResult: %v", err)
	}
	cur, err := bus.Get(ctx, req.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if cur.ResultEnvelope != "first" {
		t.Errorf("ResultEnvelope = %q, want %q (second SetResult must be a no-op)", cur.ResultEnvelope, "first")
	}
	if cur.ResultIsError {
		t.Errorf("ResultIsError = true, want false (override must not apply)")
	}
	if cur.ResultError != "" {
		t.Errorf("ResultError = %q, want empty", cur.ResultError)
	}
}

// TestSweepUnexecuted_FiresOnceSkipsExecuted verifies the restart-recovery
// sweep re-fires the executor exactly once for an allowed-but-unexecuted row
// and never touches a row that already has a result.
func TestSweepUnexecuted_FiresOnceSkipsExecuted(t *testing.T) {
	bus := newTestBus(t)
	ctx := context.Background()

	// Row A: allowed, unexecuted (no executor at decision time).
	reqA := holdPending(t, bus, "tool.A")
	if _, err := bus.Decide(ctx, reqA.ID, StatusAllowed, "tester"); err != nil {
		t.Fatalf("decide A: %v", err)
	}
	// Row B: allowed AND already executed.
	reqB := holdPending(t, bus, "tool.B")
	if _, err := bus.Decide(ctx, reqB.ID, StatusAllowed, "tester"); err != nil {
		t.Fatalf("decide B: %v", err)
	}
	if err := bus.SetResult(ctx, reqB.ID, "already-done", false, ""); err != nil {
		t.Fatalf("preset B result: %v", err)
	}

	// Recorder executor that persists a result so a second sweep skips it.
	var mu sync.Mutex
	executed := map[string]int{}
	bus.SetExecutor(execFunc(func(ctx context.Context, req *Request) {
		mu.Lock()
		executed[req.ID]++
		mu.Unlock()
		_ = bus.SetResult(ctx, req.ID, "swept", false, "")
	}))

	n, err := bus.SweepUnexecuted(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 {
		t.Errorf("first sweep dispatched %d rows, want 1 (only the unexecuted A)", n)
	}
	waitFor(t, "A executed by sweep", func() bool {
		cur, err := bus.Get(ctx, reqA.ID)
		return err == nil && cur.ResultExecutedAt > 0
	})

	n2, err := bus.SweepUnexecuted(ctx)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if n2 != 0 {
		t.Errorf("second sweep dispatched %d rows, want 0", n2)
	}

	mu.Lock()
	defer mu.Unlock()
	if executed[reqA.ID] != 1 {
		t.Errorf("A executed %d times, want exactly 1", executed[reqA.ID])
	}
	if executed[reqB.ID] != 0 {
		t.Errorf("B executed %d times, want 0 (it already had a result)", executed[reqB.ID])
	}
}

// alwaysAuto is an AutoApprover that matches every request.
type alwaysAuto struct{}

func (alwaysAuto) Match(agentID, upstream, toolName, fingerprint string, isDestructive bool) *AutoMatch {
	return &AutoMatch{ID: "rule-always", Kind: "tool"}
}
func (alwaysAuto) MarkHit(ctx context.Context, ruleID, agentID string)                   {}
func (alwaysAuto) MarkDenial(ctx context.Context, agentID, toolName, fingerprint string) {}
func (alwaysAuto) IsDestructive(ctx context.Context, toolName string) bool               { return false }

// TestRequireHuman_SkipsAutoApprover verifies an explicit `ask` policy
// (RequireHuman) keeps the request pending even when a learned auto rule
// would otherwise match.
func TestRequireHuman_SkipsAutoApprover(t *testing.T) {
	bus := newTestBus(t)
	bus.SetAutoApprover(alwaysAuto{})
	ctx := context.Background()

	// Without RequireHuman the always-matching auto-approver decides it.
	auto, err := bus.Hold(ctx, NewRequest{AgentID: "a", UpstreamName: "u", ToolName: "t.auto", Arguments: map[string]any{"x": 1}}, 0)
	if err != nil {
		t.Fatalf("hold auto: %v", err)
	}
	if auto.Status != StatusAllowed {
		t.Errorf("without RequireHuman: status = %q, want allowed (auto-approved)", auto.Status)
	}

	// With RequireHuman the auto-approver is skipped → stays pending.
	human, err := bus.Hold(ctx, NewRequest{AgentID: "a", UpstreamName: "u", ToolName: "t.human", Arguments: map[string]any{"x": 2}, RequireHuman: true}, 0)
	if err != nil {
		t.Fatalf("hold human: %v", err)
	}
	if human.Status != StatusPending {
		t.Errorf("with RequireHuman: status = %q, want pending (auto-approver skipped)", human.Status)
	}
}

// TestCreate_ConcurrentIdenticalHoldsCoalesce verifies the coalescing-race
// fix: many parallel identical Holds collapse to a single non-coalesced row.
func TestCreate_ConcurrentIdenticalHoldsCoalesce(t *testing.T) {
	bus := newTestBus(t)
	ctx := context.Background()

	const n = 8
	var wg sync.WaitGroup
	results := make([]*Request, n)
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			req, err := bus.Hold(ctx, NewRequest{
				AgentID:      "agent-x",
				UpstreamName: "up",
				ToolName:     "tool.same",
				Arguments:    map[string]any{"a": 1, "b": 2},
			}, 0)
			results[i] = req
			errs[i] = err
		}(i)
	}
	close(start)
	wg.Wait()

	nonCoalesced := 0
	ids := map[string]struct{}{}
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("hold %d errored (raw constraint leak?): %v", i, errs[i])
		}
		if results[i] == nil {
			t.Fatalf("hold %d returned nil req", i)
		}
		ids[results[i].ID] = struct{}{}
		if !results[i].Coalesced {
			nonCoalesced++
		}
	}
	if nonCoalesced != 1 {
		t.Errorf("non-coalesced rows = %d, want exactly 1", nonCoalesced)
	}
	if len(ids) != 1 {
		t.Errorf("distinct approval IDs = %d, want 1 (all coalesced onto one row)", len(ids))
	}
}
