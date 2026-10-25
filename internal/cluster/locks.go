package cluster

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// processLocks is the in-process lock table of a nil Cluster.
var processLocks keyedMutex

// Lock is LockCtx without the lock's context, for a section that need not
// stop when the lock is lost.
func (c *Cluster) Lock(ctx context.Context, name string) (unlock func(), err error) {
	_, unlock, err = c.LockCtx(ctx, name)
	return unlock, err
}

// LockCtx takes the named lock across every node. It waits until the lock
// is free or ctx ends, and returns a context that ends when the lock is
// lost (its database session ended), when ctx ends, or at unlock, and the
// function that releases the lock (safe to call more than once). The lock
// is held in process and, on Postgres, as a session advisory lock on the
// cluster's own lock connection, so it holds no connection of the pool
// however long the section runs. Waiting polls pg_try_advisory_lock, at
// most every 100 ms. A section that writes after a slow step should check
// the returned context first. Use LockTx when the section is one
// transaction.
func (c *Cluster) LockCtx(ctx context.Context, name string) (context.Context, func(), error) {
	locks := &processLocks
	if c != nil {
		locks = &c.local
	}
	unlockLocal, err := locks.lock(ctx, name)
	if err != nil {
		return nil, nil, err
	}
	if !c.MultiNode() {
		lctx, cancel := context.WithCancel(ctx)
		var once sync.Once
		return lctx, func() {
			once.Do(func() {
				cancel()
				unlockLocal()
			})
		}, nil
	}
	key := LockKey("lock:" + name)
	backoff := 5 * time.Millisecond
	for {
		lctx, l, err := c.tryNamed(ctx, key)
		if err == nil && l != nil {
			var once sync.Once
			return lctx, func() {
				once.Do(func() {
					c.unlockNamed(key, l)
					unlockLocal()
				})
			}, nil
		}
		if errors.Is(err, errClosed) {
			unlockLocal()
			return nil, nil, err
		}
		// Taken elsewhere, or the connection failed (it is opened again on
		// the next try): wait and try again.
		t := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			t.Stop()
			unlockLocal()
			return nil, nil, ctx.Err()
		case <-t.C:
		}
		backoff = min(backoff*2, 100*time.Millisecond)
	}
}

// tryNamed tries once to take the named lock key on the lock connection.
// A nil lease and nil error mean another node holds it.
func (c *Cluster) tryNamed(ctx context.Context, key int64) (context.Context, *lease, error) {
	s := &c.locks
	s.mu.Lock()
	defer s.mu.Unlock()
	got, err := c.tryLockLocked(s, key)
	if err != nil || !got {
		return nil, nil, err
	}
	lctx, cancel := context.WithCancel(ctx)
	l := &lease{gen: s.gen, cancel: cancel}
	c.named[key] = l
	return lctx, l, nil
}

// unlockNamed releases a named lock. A lock lost with its connection is
// already gone.
func (c *Cluster) unlockNamed(key int64, l *lease) {
	s := &c.locks
	s.mu.Lock()
	defer s.mu.Unlock()
	l.cancel()
	if c.named[key] != l {
		return
	}
	delete(c.named, key)
	c.unlockLocked(s, key, l.gen)
}

// LockTx takes the named lock for the rest of tx: pg_advisory_xact_lock on
// Postgres, released at commit or rollback. On SQLite it does nothing: the
// database has one connection, so a transaction already excludes every
// other writer.
func LockTx(ctx context.Context, tx store.Querier, d store.Dialect, name string) error {
	if !d.IsPostgres() {
		return nil
	}
	_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(?)`, LockKey("lock:"+name))
	return err
}

// keyedMutex is a table of in-process locks by name whose waits end with
// their context.
type keyedMutex struct {
	mu sync.Mutex
	m  map[string]*keyedEntry
}

type keyedEntry struct {
	ch   chan struct{}
	refs int
}

func (k *keyedMutex) lock(ctx context.Context, name string) (func(), error) {
	k.mu.Lock()
	if k.m == nil {
		k.m = map[string]*keyedEntry{}
	}
	e := k.m[name]
	if e == nil {
		e = &keyedEntry{ch: make(chan struct{}, 1)}
		k.m[name] = e
	}
	e.refs++
	k.mu.Unlock()
	select {
	case e.ch <- struct{}{}:
		var once sync.Once
		return func() {
			once.Do(func() {
				<-e.ch
				k.put(name, e)
			})
		}, nil
	case <-ctx.Done():
		k.put(name, e)
		return nil, ctx.Err()
	}
}

func (k *keyedMutex) put(name string, e *keyedEntry) {
	k.mu.Lock()
	defer k.mu.Unlock()
	e.refs--
	if e.refs == 0 {
		delete(k.m, name)
	}
}
