package codemode

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// limiter caps the scripts running at once: global in all, perAgent for
// any one caller. A script that cannot start waits in one queue in
// arrival order; a freed slot goes to the first waiter that may take it
// (whose caller is under its own cap), so one caller's backlog never
// holds another caller up, and among callers who may run, first come is
// first served. The limiter is per process, shared by MCP and REST.
type limiter struct {
	mu       sync.Mutex
	global   int
	perAgent int
	running  int
	byAgent  map[string]int
	queue    []*waiter
}

// waiter is one script waiting for a slot.
type waiter struct {
	agent   string
	ready   chan struct{}
	granted bool
}

func newLimiter(global, perAgent int) *limiter {
	return &limiter{global: global, perAgent: perAgent, byAgent: map[string]int{}}
}

// busyError is ErrBusy with the reason: every slot taken, or this caller
// at its own cap.
type busyError struct{ msg string }

func (e *busyError) Error() string        { return e.msg }
func (e *busyError) Is(target error) bool { return target == ErrBusy }

// fits reports whether agent may start a script now; l.mu is held.
func (l *limiter) fits(agent string) bool {
	return l.running < l.global && l.byAgent[agent] < l.perAgent
}

// take counts a started script; l.mu is held.
func (l *limiter) take(agent string) {
	l.running++
	l.byAgent[agent]++
}

// acquire takes a slot for agent, waiting up to wait in the queue: a
// busyError when none came free, ctx's error when ctx ended first. The
// returned func releases the slot.
func (l *limiter) acquire(ctx context.Context, agent string, wait time.Duration) (func(), error) {
	l.mu.Lock()
	if len(l.queue) == 0 && l.fits(agent) {
		l.take(agent)
		l.mu.Unlock()
		return l.releaser(agent), nil
	}
	w := &waiter{agent: agent, ready: make(chan struct{})}
	l.queue = append(l.queue, w)
	// The waiters ahead may all be callers at their own cap.
	l.dispatchLocked()
	l.mu.Unlock()

	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-w.ready:
		return l.releaser(agent), nil
	case <-t.C:
		l.mu.Lock()
		defer l.mu.Unlock()
		if w.granted {
			// Granted as the wait ran out: keep it.
			return l.releaser(agent), nil
		}
		l.removeLocked(w)
		return nil, l.busyLocked(agent)
	case <-ctx.Done():
		l.mu.Lock()
		granted := w.granted
		if !granted {
			l.removeLocked(w)
		}
		l.mu.Unlock()
		if granted {
			l.releaser(agent)()
		}
		return nil, ctx.Err()
	}
}

// busyLocked explains a wait that ran out; l.mu is held.
func (l *limiter) busyLocked(agent string) error {
	if l.running >= l.global {
		return &busyError{fmt.Sprintf("code mode is busy: %d scripts are already running; try again in a moment", l.running)}
	}
	return &busyError{fmt.Sprintf("code mode is busy: this caller already runs %d scripts, its limit (%d at once per caller); wait for one to finish, then try again", l.byAgent[agent], l.perAgent)}
}

// releaser frees agent's slot once, and hands it on.
func (l *limiter) releaser(agent string) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.running--
			if l.byAgent[agent]--; l.byAgent[agent] <= 0 {
				delete(l.byAgent, agent)
			}
			l.dispatchLocked()
		})
	}
}

// dispatchLocked grants free slots to waiters in queue order, skipping
// those whose caller is at its cap; l.mu is held.
func (l *limiter) dispatchLocked() {
	for i := 0; i < len(l.queue) && l.running < l.global; {
		w := l.queue[i]
		if !l.fits(w.agent) {
			i++
			continue
		}
		l.take(w.agent)
		w.granted = true
		close(w.ready)
		l.queue = append(l.queue[:i], l.queue[i+1:]...)
	}
}

// removeLocked drops w from the queue; l.mu is held.
func (l *limiter) removeLocked(w *waiter) {
	for i, q := range l.queue {
		if q == w {
			l.queue = append(l.queue[:i], l.queue[i+1:]...)
			return
		}
	}
}

// inUse is how many scripts hold a slot.
func (l *limiter) inUse() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.running
}
