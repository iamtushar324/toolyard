// Package goroutines wraps `go func()` with panic recovery + crash dumps.
//
// One panic in a background goroutine used to take the whole gateway
// down. Now: the recover writes a crash dump, logs a structured
// `goroutine panic` event at error level with the stack, and counts it
// (opsmetrics.IncGoroutinePanic); the process keeps serving everything
// else.
//
// Three flavors:
//
//	goroutines.Go("scanner", fn)                 // one-shot; logs & exits on panic
//	goroutines.Loop(ctx, "sweeper", max, fn)     // restarts only after a panic
//	goroutines.Supervise(ctx, "scanner", max, …) // restarts after a panic or an error
//
// Names are a fixed set of constants chosen by the caller: they become a
// metric label.
//
// Tracker counts running work so a shutdown can wait for it and name what
// is still running at its deadline.
package goroutines

import (
	"context"
	"log/slog"
	"runtime/debug"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/crashdump"
	"github.com/tusharbhardwaj/toolyard/internal/logx"
	"github.com/tusharbhardwaj/toolyard/internal/opsmetrics"
)

// panics counts every recovered goroutine panic in this process.
var panics atomic.Int64

// Panics returns how many goroutine panics this process has recovered.
func Panics() int64 { return panics.Load() }

// Go launches fn in a new goroutine with panic recovery. On panic it writes
// a crash dump, logs at error level, and returns (does not restart).
func Go(name string, fn func()) {
	go func() {
		defer recoverAndLog(name, nil)
		fn()
	}()
}

// Loop launches fn in a goroutine and restarts it ONLY after a panic, with
// exponential backoff (500ms, doubling, capped at maxBackoff), until ctx is
// cancelled. A normal return ends the loop: a scanner that returns at once
// because it is disabled must not restart forever. fn should return
// promptly when ctx is cancelled.
func Loop(ctx context.Context, name string, maxBackoff time.Duration, fn func(context.Context)) {
	if maxBackoff <= 0 {
		maxBackoff = 30 * time.Second
	}
	go func() {
		backoff := min(500*time.Millisecond, maxBackoff)
		for {
			if !runRecovered(name, func() { fn(ctx) }) {
				return
			}
			if ctx.Err() != nil {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, maxBackoff)
		}
	}()
}

// runRecovered runs fn and reports whether it panicked. The panic is
// logged and counted here.
func runRecovered(name string, fn func()) (panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			panicked = true
			report(name, r, map[string]string{"supervised": "true"})
		}
	}()
	fn()
	return false
}

// Supervise launches fn in a goroutine; if it panics or returns an error,
// it's restarted with exponential backoff (capped at maxBackoff). Stops
// when ctx is cancelled. fn should respect ctx and return promptly on
// cancellation; otherwise Supervise can't tell shutdown from work.
func Supervise(ctx context.Context, name string, maxBackoff time.Duration, fn func(context.Context) error) {
	if maxBackoff <= 0 {
		maxBackoff = 30 * time.Second
	}
	go func() {
		backoff := 500 * time.Millisecond
		for ctx.Err() == nil {
			func() {
				defer recoverAndLog(name, map[string]string{"supervised": "true"})
				if err := fn(ctx); err != nil && ctx.Err() == nil {
					logx.For("goroutines").Error("supervised exited with error",
						"name", name, "err", err.Error())
				}
			}()
			if ctx.Err() != nil {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}()
}

func recoverAndLog(name string, extra map[string]string) {
	r := recover()
	if r == nil {
		return
	}
	report(name, r, extra)
}

// report counts, dumps and logs one recovered panic. Called from the
// deferred recover, so debug.Stack still shows the panicking frames.
func report(name string, r any, extra map[string]string) {
	panics.Add(1)
	opsmetrics.IncGoroutinePanic(name)
	path := crashdump.Record("goroutine:"+name, r, extra)
	logx.For("goroutines").LogAttrs(context.Background(), slog.LevelError, "goroutine panic",
		slog.String("name", name),
		slog.Any("panic", r),
		slog.String("stack", string(debug.Stack())),
		slog.String("crash_dump", path),
	)
}

// ---- Tracker ----------------------------------------------------------------

// Tracker counts running work by label, so a shutdown can wait for it and
// name what is still running at its deadline. Unlike a sync.WaitGroup,
// Add may run while Wait is waiting. The zero value is ready to use.
type Tracker struct {
	// Executions counts this tracker's work in the process-wide
	// in-flight executions gauge (opsmetrics.SetInflightExecutions).
	// Set it before the first Add.
	Executions bool

	mu    sync.Mutex
	next  uint64
	items map[uint64]string
	idle  chan struct{} // closed and cleared when items becomes empty
}

// Add records one piece of running work and returns the func that ends
// it. Calling the returned func more than once is safe.
func (t *Tracker) Add(label string) (done func()) {
	t.mu.Lock()
	if t.items == nil {
		t.items = map[uint64]string{}
	}
	t.next++
	id := t.next
	t.items[id] = label
	t.mu.Unlock()
	if t.Executions {
		addExecutions(1)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			t.mu.Lock()
			delete(t.items, id)
			if len(t.items) == 0 && t.idle != nil {
				close(t.idle)
				t.idle = nil
			}
			t.mu.Unlock()
			if t.Executions {
				addExecutions(-1)
			}
		})
	}
}

// Len returns how many pieces of work are running.
func (t *Tracker) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.items)
}

// Running returns the labels of the work still running, sorted.
func (t *Tracker) Running() []string {
	t.mu.Lock()
	out := make([]string, 0, len(t.items))
	for _, l := range t.items {
		out = append(out, l)
	}
	t.mu.Unlock()
	sort.Strings(out)
	return out
}

// Wait blocks until no work is running or ctx ends. It returns the labels
// of the work still running when ctx ended, or nil when idle.
func (t *Tracker) Wait(ctx context.Context) []string {
	for {
		t.mu.Lock()
		if len(t.items) == 0 {
			t.mu.Unlock()
			return nil
		}
		if t.idle == nil {
			t.idle = make(chan struct{})
		}
		idle := t.idle
		t.mu.Unlock()
		select {
		case <-idle:
		case <-ctx.Done():
			if left := t.Running(); len(left) > 0 {
				return left
			}
			return nil
		}
	}
}

var (
	execMu sync.Mutex
	execN  int
)

// addExecutions moves the process-wide in-flight executions gauge. The
// lock keeps the reported values in order.
func addExecutions(delta int) {
	execMu.Lock()
	execN += delta
	opsmetrics.SetInflightExecutions(execN)
	execMu.Unlock()
}
