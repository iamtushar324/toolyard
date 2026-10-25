package coderuns

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tusharbhardwaj/toolyard/internal/codemode"
)

// setWorker gives the env's runtime its worker with these caps.
func (e *env) setWorker(t *testing.T, maxConcurrent, maxPerAgent int, wait time.Duration) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	e.rt.SetWorker(codemode.Worker{Path: exe, Env: []string{testWorkerEnv + "=1"}, MaxConcurrent: maxConcurrent, MaxPerAgent: maxPerAgent, AcquireWait: wait})
}

// TestStartAnswersAtOnceWhenEverySlotIsTaken (live test, Prefer: wait=0):
// with the agent's one slot taken, a start is stored queued at once (its
// created_at is the request's time) and runs when the slot frees; the
// queue holds twice the agent's cap, and a start past that is refused
// busy at once with nothing stored.
func TestStartAnswersAtOnceWhenEverySlotIsTaken(t *testing.T) {
	e := newEnv(t)
	e.setWorker(t, 1, 1, 20*time.Second)
	release := make(chan struct{})
	var once sync.Once
	free := func() { once.Do(func() { close(release) }) }
	defer free()
	started := make(chan struct{}, 8)
	e.f.setHandler("demo.read", func(map[string]any) (*mcp.CallToolResult, error) {
		started <- struct{}{}
		<-release
		return mcp.NewToolResultText(`{"n": 1}`), nil
	})
	ctx := context.Background()
	first, _, err := e.svc.Start(ctx, StartRequest{AgentID: "ag_a", Code: readScript, Reason: testReason})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(30 * time.Second):
		t.Fatal("the first run did not start")
	}
	var queued []*Run
	for i := 0; i < 2; i++ {
		before := time.Now()
		run, created, err := e.svc.Start(ctx, StartRequest{AgentID: "ag_a", Code: readScript, Reason: testReason})
		took := time.Since(before)
		if err != nil || !created || run.Status != StatusQueued {
			t.Fatalf("start %d: %+v %v %v", i, run, created, err)
		}
		if took > 2*time.Second {
			t.Fatalf("start %d waited %s for a slot before answering", i, took)
		}
		if now := time.Now().UnixMilli(); run.CreatedAt < before.UnixMilli() || run.CreatedAt > now {
			t.Fatalf("start %d: created_at %d is not the request's time", i, run.CreatedAt)
		}
		got, err := e.svc.Get(ctx, "ag_a", run.ID, 0)
		if err != nil || got.Status != StatusQueued {
			t.Fatalf("start %d read back: %+v %v", i, got, err)
		}
		queued = append(queued, run)
	}
	// Both queued runs wait in the slot queue (their goroutines enqueue).
	for deadline := time.Now().Add(10 * time.Second); e.rt.SlotWaiters() != 2; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%d runs wait for a slot, want 2", e.rt.SlotWaiters())
		}
	}
	if _, _, err := e.svc.Start(ctx, StartRequest{AgentID: "ag_a", Code: readScript, Reason: testReason}); !errors.Is(err, codemode.ErrBusy) ||
		!strings.Contains(err.Error(), "waiting for a slot") {
		t.Fatalf("a start past the agent's queue: %v", err)
	}
	if n := e.count(t, `SELECT count(*) FROM code_runs`); n != 3 {
		t.Fatalf("runs = %d, want 3 (the refused start stores nothing)", n)
	}
	free()
	for _, id := range []string{first.ID, queued[0].ID, queued[1].ID} {
		if r := e.settle(t, "ag_a", id); r.Status != codemode.StatusSucceeded {
			t.Fatalf("run %s = %+v", id, r)
		}
	}
}

// TestDrainInterruptsRuns (live test): a run that toolyard stops while it
// drains is interrupted, not timed out: outcome_unknown once it had sent a
// tool call, failed with nothing run before that, and failed with nothing
// run while it was still queued for a slot.
func TestDrainInterruptsRuns(t *testing.T) {
	drain := func(e *env) {
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		e.svc.Wait(ctx)
	}
	t.Run("after a call", func(t *testing.T) {
		e := newEnv(t)
		called := make(chan struct{}, 1)
		e.f.setHandler("demo.read", func(map[string]any) (*mcp.CallToolResult, error) {
			called <- struct{}{}
			return mcp.NewToolResultText(`{"n": 1}`), nil
		})
		run, _, err := e.svc.Start(context.Background(), StartRequest{AgentID: "ag_a", Language: "javascript", Reason: testReason,
			Code: "await tools.demo.read({ q: \"x\" });\nlet i = 0;\nwhile (true) { i++; }"})
		if err != nil {
			t.Fatal(err)
		}
		select {
		case <-called:
		case <-time.After(30 * time.Second):
			t.Fatal("the run made no call")
		}
		drain(e)
		got := e.settle(t, "ag_a", run.ID)
		if got.Status != codemode.StatusOutcomeUnknown || got.ErrorKind != ErrorInterrupted || !strings.Contains(got.ErrorMessage, "after it sent 1 tool call(s)") ||
			HTTPStatus(got.Status) != 409 {
			t.Fatalf("interrupted after a call = %+v", got)
		}
	})
	t.Run("before any call", func(t *testing.T) {
		e := newEnv(t)
		run, _, err := e.svc.Start(context.Background(), StartRequest{AgentID: "ag_a", Language: "javascript", Reason: testReason,
			Code: "let i = 0;\nwhile (true) { i++; }"})
		if err != nil {
			t.Fatal(err)
		}
		waitStatus(t, e, run.ID, StatusRunning)
		time.Sleep(300 * time.Millisecond)
		drain(e)
		got := e.settle(t, "ag_a", run.ID)
		if got.Status != codemode.StatusFailed || got.ErrorKind != ErrorInterrupted || !strings.Contains(got.ErrorMessage, "nothing ran") {
			t.Fatalf("interrupted before a call = %+v", got)
		}
	})
	t.Run("while queued", func(t *testing.T) {
		e := newEnv(t)
		e.setWorker(t, 1, 1, 20*time.Second)
		slot, err := e.rt.AcquireSlot(codemode.WithAgentKey(context.Background(), "ag_a"))
		if err != nil {
			t.Fatal(err)
		}
		defer slot.Release()
		run, _, err := e.svc.Start(context.Background(), StartRequest{AgentID: "ag_a", Code: readScript, Reason: testReason})
		if err != nil || run.Status != StatusQueued {
			t.Fatalf("start: %+v %v", run, err)
		}
		drain(e)
		got := e.settle(t, "ag_a", run.ID)
		if got.Status != codemode.StatusFailed || got.ErrorKind != ErrorInterrupted || got.ErrorMessage != msgInterruptedQueued {
			t.Fatalf("interrupted while queued = %+v", got)
		}
		if len(e.f.sent("demo.read")) != 0 {
			t.Fatal("a queued run that never started sent a call")
		}
	})
}

// waitStatus waits until the run reads status.
func waitStatus(t *testing.T, e *env, id, status string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		r, err := e.svc.Get(context.Background(), "ag_a", id, 0)
		if err != nil {
			t.Fatal(err)
		}
		if r.Status == status {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s is %s, not %s", id, r.Status, status)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
