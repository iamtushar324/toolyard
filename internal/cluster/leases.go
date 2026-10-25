package cluster

import (
	"context"
	"hash/fnv"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/logx"
)

// RunLeader runs fn while this process holds the named lease
// (pg_try_advisory_lock on a dedicated connection on Postgres). fn's ctx is
// cancelled when the lease is lost; RunLeader then retries until ctx ends.
// It blocks until ctx ends or fn returns nil while it still holds the lease
// (the work is finished); an error from fn is logged and fn runs again
// after the retry interval, under a fresh lease. A panic in fn releases the
// lease and propagates (run RunLeader under goroutines.Loop to restart it).
// Within one process, two RunLeader calls with the same name also take
// turns. After Drain no lease is taken any more. On a single node the
// lease is granted at once.
func (c *Cluster) RunLeader(ctx context.Context, name string, fn func(ctx context.Context) error) {
	if c == nil {
		runUnleased(ctx, name, fn)
		return
	}
	key := LockKey("leader:" + name)
	lg := c.log.With("lease", name)
	var wait time.Duration // the first try is immediate
	for {
		if wait > 0 && !sleepCtx(ctx, jitter(wait)) {
			return
		}
		wait = c.opts.RetryInterval
		lctx, release, ok := c.acquire(ctx, key)
		if !ok {
			if ctx.Err() != nil {
				return
			}
			continue
		}
		if c.pg {
			lg.Info("cluster: leader lease taken")
		}
		lost, err := runHeld(lctx, fn, release)
		if ctx.Err() != nil {
			return
		}
		switch {
		case lost && c.draining.Load():
			lg.Info("cluster: leader lease released for the drain; another node takes it")
		case lost:
			lg.Warn("cluster: leader lease lost; the task stopped and will take the lease again when it can")
		case err != nil:
			lg.Error("cluster: leader task failed; retrying", "err", err.Error())
		default:
			return
		}
	}
}

// Drain releases every leader lease this node holds and takes none any
// more, so another node takes over the leader tasks at once while this one
// finishes its requests (shutdown drain step 2). Each lease's task sees its
// context end. Safe to call more than once.
func (c *Cluster) Drain() {
	if c == nil {
		return
	}
	c.draining.Store(true)
	c.leases.mu.Lock()
	defer c.leases.mu.Unlock()
	// Cancelling makes each task return; RunLeader then releases its lease
	// (pg_advisory_unlock) on its way out.
	for _, l := range c.held {
		l.cancel()
	}
}

// runHeld runs fn under a held lease and releases it, also on a panic.
// lost reports that the lease ended before fn returned.
func runHeld(lctx context.Context, fn func(context.Context) error, release func()) (lost bool, err error) {
	defer release()
	err = fn(lctx)
	return lctx.Err() != nil, err
}

// runUnleased is RunLeader for a nil Cluster: fn runs at once, again after
// an error.
func runUnleased(ctx context.Context, name string, fn func(context.Context) error) {
	for {
		err := fn(ctx)
		if ctx.Err() != nil || err == nil {
			return
		}
		logx.For("cluster").Error("cluster: task failed; retrying", "lease", name, "err", err.Error())
		if !sleepCtx(ctx, jitter(DefaultRetryInterval)) {
			return
		}
	}
}

// acquire takes the lease key if no one holds it. It returns a context
// that ends when the lease is lost or ctx ends, and the func that releases
// the lease.
func (c *Cluster) acquire(ctx context.Context, key int64) (context.Context, func(), bool) {
	s := &c.leases
	s.mu.Lock()
	defer s.mu.Unlock()
	if c.closed.Load() || c.ctx.Err() != nil || c.draining.Load() {
		return nil, nil, false
	}
	if _, ok := c.held[key]; ok {
		return nil, nil, false
	}
	if c.pg {
		got, err := c.tryLockLocked(s, key)
		if err != nil || !got {
			return nil, nil, false
		}
	}
	lctx, cancel := context.WithCancel(ctx)
	l := &lease{gen: s.gen, cancel: cancel}
	c.held[key] = l
	return lctx, func() { c.releaseLease(key, l) }, true
}

// releaseLease gives up one leader lease. A lease lost with its connection
// is already gone: nothing is sent then.
func (c *Cluster) releaseLease(key int64, l *lease) {
	s := &c.leases
	s.mu.Lock()
	defer s.mu.Unlock()
	l.cancel()
	if c.held[key] != l {
		return
	}
	delete(c.held, key)
	if c.pg {
		c.unlockLocked(s, key, l.gen)
	}
}

// tryLockLocked tries once to take the session advisory lock key on s's
// connection. s.mu is held.
func (c *Cluster) tryLockLocked(s *slot, key int64) (bool, error) {
	conn, err := c.connLocked(s)
	if err != nil {
		return false, err
	}
	// The cluster's context, not the caller's: a query cancelled half-way
	// would leave the connection unusable, and with it everything it holds.
	qctx, cancel := context.WithTimeout(c.ctx, queryTimeout)
	defer cancel()
	var got bool
	if err := conn.QueryRow(qctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&got); err != nil {
		c.dropLocked(s, "take a lock", err)
		return false, err
	}
	return got, nil
}

// unlockLocked releases the session advisory lock key that s's connection
// generation gen took. A lock lost with its connection is already gone.
// s.mu is held.
func (c *Cluster) unlockLocked(s *slot, key int64, gen uint64) {
	if s.conn == nil || gen != s.gen || c.ctx.Err() != nil {
		// Close closes the connection, which releases the lock.
		return
	}
	qctx, cancel := context.WithTimeout(c.ctx, queryTimeout)
	defer cancel()
	var ok bool
	if err := s.conn.QueryRow(qctx, `SELECT pg_advisory_unlock($1)`, key).Scan(&ok); err != nil {
		c.dropLocked(s, "release a lock", err)
	}
}

// LockKey maps a lock name to the 64-bit key of a Postgres advisory lock.
func LockKey(name string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("toolyard:" + name))
	return int64(h.Sum64())
}
