package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// stepLog records shutdown steps in the order they ran.
type stepLog struct {
	mu    sync.Mutex
	steps []string
}

func (l *stepLog) add(step string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.steps = append(l.steps, step)
}

func (l *stepLog) get() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.steps...)
}

func TestShutdownStepsRunInOrder(t *testing.T) {
	var l stepLog
	var refusedAt, releasedAt time.Time
	closer := func(name string) shutdownCloser {
		return shutdownCloser{name: name, close: func() error { l.add("close-" + name); return nil }}
	}
	plan := &shutdownPlan{
		drainDelay:   30 * time.Millisecond,
		drainTimeout: 2 * time.Second,
		grace:        100 * time.Millisecond,
		markDraining: func() { l.add("mark-draining") },
		refuseNew:    func() { refusedAt = time.Now(); l.add("refuse-new") },
		releaseWaiters: func() {
			releasedAt = time.Now()
			l.add("release-waiters")
		},
		stopHTTP:  func(context.Context) error { l.add("stop-http"); return nil },
		forceHTTP: func() error { l.add("force-http"); return nil },
		waitExecutions: []func(context.Context){func(ctx context.Context) {
			if _, ok := ctx.Deadline(); !ok {
				l.add("wait-without-deadline")
			}
			l.add("wait-executions")
		}},
		cancelRoot: func() { l.add("cancel-root") },
		closers:    []shutdownCloser{closer("inbox"), closer("metrics"), closer("gateway"), closer("database")},
	}
	plan.run()
	want := []string{
		"mark-draining", "refuse-new", "release-waiters", "stop-http", "wait-executions",
		"cancel-root", "close-inbox", "close-metrics", "close-gateway", "close-database",
	}
	if got := l.get(); !reflect.DeepEqual(got, want) {
		t.Fatalf("steps = %v\nwant    %v", got, want)
	}
	if d := releasedAt.Sub(refusedAt); d < 30*time.Millisecond {
		t.Fatalf("waiters released %s after the refusal began; want at least the 30ms drain delay", d)
	}
}

// Steps that ignore their context cannot hold the shutdown past the drain
// deadline plus grace plus each close's own bound.
func TestShutdownHangingStepsAreBoundedByTheDeadline(t *testing.T) {
	var l stepLog
	hang := make(chan struct{})
	defer close(hang)
	plan := &shutdownPlan{
		drainTimeout: 150 * time.Millisecond,
		grace:        50 * time.Millisecond,
		stopHTTP:     func(context.Context) error { l.add("stop-http"); <-hang; return nil },
		forceHTTP:    func() error { l.add("force-http"); return nil },
		waitExecutions: []func(context.Context){func(context.Context) {
			l.add("wait-executions")
			<-hang
		}},
		cancelRoot: func() { l.add("cancel-root") },
		closers: []shutdownCloser{{
			name:  "database",
			close: closeOnce("database", 50*time.Millisecond, func(context.Context) error { <-hang; return nil }),
		}},
	}
	start := time.Now()
	finished := make(chan struct{})
	go func() {
		plan.run()
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not return: a hanging step was not bounded")
	}
	elapsed := time.Since(start)
	if elapsed < 150*time.Millisecond {
		t.Fatalf("shutdown returned after %s, before the drain deadline", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("shutdown took %s; want about deadline + grace + close bound (250ms)", elapsed)
	}
	want := []string{"stop-http", "force-http", "wait-executions", "cancel-root"}
	if got := l.get(); !reflect.DeepEqual(got, want) {
		t.Fatalf("steps = %v, want %v", got, want)
	}
}

func TestCloseOnceRunsOnceAndIsBounded(t *testing.T) {
	var calls atomic.Int32
	c := closeOnce("inbox", time.Second, func(context.Context) error {
		calls.Add(1)
		return errors.New("boom")
	})
	err1, err2 := c(), c()
	if calls.Load() != 1 {
		t.Fatalf("close ran %d times, want 1", calls.Load())
	}
	if err1 == nil || !strings.Contains(err1.Error(), "inbox: boom") || err2 != err1 {
		t.Fatalf("errors = %v, %v", err1, err2)
	}

	hang := make(chan struct{})
	defer close(hang)
	slow := closeOnce("database", 30*time.Millisecond, func(context.Context) error { <-hang; return nil })
	start := time.Now()
	err := slow()
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("a hanging close took %s", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("hanging close error = %v, want deadline exceeded", err)
	}
}
