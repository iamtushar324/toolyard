package goroutines

import (
	"context"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

func eventually(t *testing.T, what string, cond func() bool) {
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

func TestLoopRestartsOnlyAfterPanic(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	before := Panics()
	var calls atomic.Int32
	running := make(chan struct{})
	Loop(ctx, "test-loop-panic", 5*time.Millisecond, func(ctx context.Context) {
		if calls.Add(1) <= 2 {
			panic("boom")
		}
		close(running)
		<-ctx.Done()
	})
	select {
	case <-running:
	case <-time.After(3 * time.Second):
		t.Fatalf("loop did not restart after its panics; calls=%d", calls.Load())
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("calls = %d, want 3 (two panics, then a run)", got)
	}
	if got := Panics() - before; got != 2 {
		t.Fatalf("recovered panics = %d, want 2", got)
	}
}

func TestLoopEndsOnNormalReturn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	Loop(ctx, "test-loop-return", time.Millisecond, func(context.Context) {
		calls.Add(1) // a disabled scanner returns at once
	})
	eventually(t, "first call", func() bool { return calls.Load() >= 1 })
	time.Sleep(50 * time.Millisecond)
	if got := calls.Load(); got != 1 {
		t.Fatalf("calls = %d, want 1: a normal return must end the loop", got)
	}
}

func TestLoopStopsWhenCancelledAfterPanic(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var calls atomic.Int32
	Loop(ctx, "test-loop-cancel", time.Hour, func(context.Context) {
		calls.Add(1)
		cancel()
		panic("boom")
	})
	eventually(t, "first call", func() bool { return calls.Load() >= 1 })
	time.Sleep(50 * time.Millisecond)
	if got := calls.Load(); got != 1 {
		t.Fatalf("calls = %d, want 1 after cancel", got)
	}
}

func TestGoRecoversPanic(t *testing.T) {
	before := Panics()
	Go("test-go", func() { panic("boom") })
	eventually(t, "panic recovered and counted", func() bool { return Panics() > before })
	ran := make(chan struct{})
	Go("test-go", func() { close(ran) })
	select {
	case <-ran:
	case <-time.After(3 * time.Second):
		t.Fatal("Go did not run its function")
	}
}

func TestTrackerWaitAndDeadline(t *testing.T) {
	var tr Tracker
	if left := tr.Wait(context.Background()); left != nil {
		t.Fatalf("idle tracker: Wait = %v, want nil", left)
	}
	doneA := tr.Add("a")
	doneB := tr.Add("b")
	if tr.Len() != 2 {
		t.Fatalf("Len = %d, want 2", tr.Len())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if left := tr.Wait(ctx); !reflect.DeepEqual(left, []string{"a", "b"}) {
		t.Fatalf("Wait at deadline = %v, want [a b]", left)
	}

	doneA()
	doneA() // a second call is harmless
	if tr.Len() != 1 {
		t.Fatalf("Len = %d, want 1", tr.Len())
	}
	waited := make(chan []string, 1)
	go func() { waited <- tr.Wait(context.Background()) }()
	time.Sleep(10 * time.Millisecond)
	doneB()
	select {
	case left := <-waited:
		if left != nil {
			t.Fatalf("Wait after the last done = %v, want nil", left)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Wait did not return when the tracker became idle")
	}
	// Add after a Wait has returned works (unlike a reused WaitGroup).
	doneC := tr.Add("c")
	if got := tr.Running(); !reflect.DeepEqual(got, []string{"c"}) {
		t.Fatalf("Running = %v, want [c]", got)
	}
	doneC()
}
