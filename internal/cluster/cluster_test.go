package cluster

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/store"
	"github.com/tusharbhardwaj/toolyard/internal/store/storetest"
)

// fastOptions keeps the tests quick: leases are retried and checked every
// 50 ms.
func fastOptions(id string) Options {
	return Options{InstanceID: id, RetryInterval: 50 * time.Millisecond, CheckInterval: 50 * time.Millisecond, ResyncInterval: -1}
}

func newCluster(t *testing.T, db *store.DB, opts Options) *Cluster {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := New(ctx, db, opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// twoNodes opens two connections pools on one Postgres test database, as
// two gateways would, and a Cluster on each.
func twoNodes(t *testing.T) (*Cluster, *Cluster, *store.DB, *store.DB) {
	t.Helper()
	storetest.SkipUnlessPostgres(t)
	url := storetest.NewPostgresDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	db1, err := store.OpenPostgres(ctx, url, 4)
	if err != nil {
		t.Fatalf("open node 1: %v", err)
	}
	t.Cleanup(func() { _ = db1.Close() })
	db2, err := store.OpenPostgres(ctx, url, 4)
	if err != nil {
		t.Fatalf("open node 2: %v", err)
	}
	t.Cleanup(func() { _ = db2.Close() })
	return newCluster(t, db1, fastOptions("node-1")), newCluster(t, db2, fastOptions("node-2")), db1, db2
}

func waitFor(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", within, what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestInstanceID(t *testing.T) {
	c := newCluster(t, nil, Options{})
	if c.InstanceID() == "" || c.InstanceID() != c.InstanceID() {
		t.Fatalf("InstanceID %q is not stable", c.InstanceID())
	}
	other := newCluster(t, nil, Options{})
	if other.InstanceID() == c.InstanceID() {
		t.Fatalf("two clusters share instance ID %q", c.InstanceID())
	}
	named := newCluster(t, nil, Options{InstanceID: "  pod-a  "})
	if named.InstanceID() != "pod-a" {
		t.Fatalf("InstanceID = %q, want pod-a", named.InstanceID())
	}
	var nilCluster *Cluster
	if nilCluster.InstanceID() == "" || nilCluster.InstanceID() != nilCluster.InstanceID() {
		t.Fatal("a nil Cluster has no stable instance ID")
	}
	if nilCluster.MultiNode() || c.MultiNode() {
		t.Fatal("MultiNode is true without Postgres")
	}
}

func TestSingleNodePublishSubscribe(t *testing.T) {
	db := storetest.Open(t)
	c := newCluster(t, db, fastOptions("solo"))
	if c.MultiNode() != db.Dialect.IsPostgres() {
		t.Fatalf("MultiNode = %v on %s", c.MultiNode(), db.Dialect)
	}
	got := make(chan string, 4)
	cancel := c.Subscribe("policy", func(p string) { got <- p })
	other := make(chan string, 4)
	c.Subscribe("settings", func(p string) { other <- p })
	if err := c.Publish(context.Background(), "policy", "reload"); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	select {
	case p := <-got:
		if p != "reload" {
			t.Fatalf("payload %q, want reload", p)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no local delivery")
	}
	select {
	case p := <-other:
		t.Fatalf("a subscriber of another topic got %q", p)
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	cancel() // twice is fine
	if err := c.Publish(context.Background(), "policy", "again"); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	select {
	case p := <-got:
		t.Fatalf("delivered %q after cancel", p)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestPublishLimits(t *testing.T) {
	c := newCluster(t, nil, Options{})
	ctx := context.Background()
	if err := c.Publish(ctx, "t", strings.Repeat("x", MaxPayload)); err != nil {
		t.Fatalf("a 1 KiB payload: %v", err)
	}
	if err := c.Publish(ctx, "t", strings.Repeat("x", MaxPayload+1)); !errors.Is(err, ErrPayloadTooLarge) {
		t.Fatalf("an oversized payload: %v", err)
	}
	for _, topic := range []string{"", strings.Repeat("t", MaxTopic+1), "a\x00b"} {
		if err := c.Publish(ctx, topic, "x"); !errors.Is(err, ErrBadTopic) {
			t.Fatalf("topic %q: %v", topic, err)
		}
	}
	if err := c.Publish(ctx, "t", Resync); err == nil {
		t.Fatal("published the Resync payload")
	}
	var nilCluster *Cluster
	if err := nilCluster.Publish(ctx, "t", "x"); err != nil {
		t.Fatalf("nil Publish: %v", err)
	}
	nilCluster.Subscribe("t", func(string) {})()
}

func TestSingleNodeRunLeader(t *testing.T) {
	db := storetest.Open(t)
	c := newCluster(t, db, fastOptions("solo"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// fn runs at once; a second task under the same name waits its turn.
	var running, maxRunning atomic.Int32
	release := make(chan struct{})
	var wg sync.WaitGroup
	task := func(ctx context.Context) error {
		n := running.Add(1)
		for {
			m := maxRunning.Load()
			if n <= m || maxRunning.CompareAndSwap(m, n) {
				break
			}
		}
		defer running.Add(-1)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil
	}
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.RunLeader(ctx, "solo-task", task)
		}()
	}
	waitFor(t, 5*time.Second, "the first task to start", func() bool { return running.Load() == 1 })
	time.Sleep(200 * time.Millisecond)
	if maxRunning.Load() != 1 {
		t.Fatalf("%d tasks ran at once under one lease", maxRunning.Load())
	}
	close(release)
	wg.Wait() // both returned nil: RunLeader returns
	if maxRunning.Load() != 1 {
		t.Fatalf("%d tasks ran at once under one lease", maxRunning.Load())
	}

	// An error runs fn again; a nil return ends RunLeader.
	var calls atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.RunLeader(ctx, "flaky", func(context.Context) error {
			if calls.Add(1) < 3 {
				return errors.New("not yet")
			}
			return nil
		})
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunLeader did not return after fn succeeded")
	}
	if calls.Load() != 3 {
		t.Fatalf("fn ran %d times, want 3", calls.Load())
	}

	// A nil Cluster runs fn at once.
	var nilCluster *Cluster
	ran := false
	nilCluster.RunLeader(ctx, "nil", func(context.Context) error { ran = true; return nil })
	if !ran {
		t.Fatal("a nil Cluster did not run fn")
	}
}

func TestLockSingleNode(t *testing.T) {
	for _, c := range []*Cluster{nil, newCluster(t, nil, Options{})} {
		unlock, err := c.Lock(context.Background(), "k")
		if err != nil {
			t.Fatalf("Lock: %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		if _, err := c.Lock(ctx, "k"); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("a held lock was taken again: %v", err)
		}
		cancel()
		other, err := c.Lock(context.Background(), "other")
		if err != nil {
			t.Fatalf("another name: %v", err)
		}
		other()
		unlock()
		unlock() // twice is fine
		again, err := c.Lock(context.Background(), "k")
		if err != nil {
			t.Fatalf("Lock after unlock: %v", err)
		}
		again()
	}
}

func TestLockTxSQLiteIsNoOp(t *testing.T) {
	storetest.SkipUnlessSQLite(t)
	db := storetest.Open(t)
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := LockTx(context.Background(), tx, db.Dialect, "x"); err != nil {
		t.Fatalf("LockTx: %v", err)
	}
}

// ---- two nodes on one Postgres database ---------------------------------------

func TestLeaderLeaseTwoNodes(t *testing.T) {
	c1, c2, _, _ := twoNodes(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	var holders []string // node IDs currently running the task
	var overlap atomic.Bool
	task := func(node string) func(context.Context) error {
		return func(ctx context.Context) error {
			mu.Lock()
			holders = append(holders, node)
			if len(holders) > 1 {
				overlap.Store(true)
			}
			mu.Unlock()
			<-ctx.Done()
			mu.Lock()
			for i, h := range holders {
				if h == node {
					holders = append(holders[:i], holders[i+1:]...)
					break
				}
			}
			mu.Unlock()
			return nil
		}
	}
	current := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), holders...)
	}

	ctx1, stop1 := context.WithCancel(ctx)
	done1 := make(chan struct{})
	go func() { defer close(done1); c1.RunLeader(ctx1, "sweeper", task("node-1")) }()
	waitFor(t, 10*time.Second, "node 1 to lead", func() bool { return len(current()) == 1 })

	done2 := make(chan struct{})
	go func() { defer close(done2); c2.RunLeader(ctx, "sweeper", task("node-2")) }()
	time.Sleep(500 * time.Millisecond) // node 2 tries about ten times
	if h := current(); len(h) != 1 || h[0] != "node-1" {
		t.Fatalf("holders %v, want only node-1", h)
	}

	// Node 1 stops: node 2 takes over.
	stop1()
	<-done1
	waitFor(t, 10*time.Second, "node 2 to take over", func() bool {
		h := current()
		return len(h) == 1 && h[0] == "node-2"
	})
	if overlap.Load() {
		t.Fatal("both nodes held the lease at once")
	}

	// Another lease name is independent: node 1 takes it while node 2
	// leads the first.
	ctx3, stop3 := context.WithCancel(ctx)
	defer stop3()
	ran := make(chan struct{})
	go c1.RunLeader(ctx3, "other", func(ctx context.Context) error { close(ran); <-ctx.Done(); return nil })
	select {
	case <-ran:
	case <-time.After(10 * time.Second):
		t.Fatal("node 1 could not take an unrelated lease")
	}
	cancel()
	<-done2
}

func TestLeaderLeaseLostWithConnection(t *testing.T) {
	c1, c2, _, db2 := twoNodes(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Node 1 stops trying once it lost the lease, so node 2 surely takes
	// it next.
	ctx1, stop1 := context.WithCancel(ctx)
	defer stop1()
	lost := make(chan struct{})
	var once sync.Once
	started1 := make(chan struct{}, 8)
	go c1.RunLeader(ctx1, "refresher", func(lctx context.Context) error {
		started1 <- struct{}{}
		<-lctx.Done()
		if ctx1.Err() == nil {
			once.Do(func() { close(lost); stop1() })
		}
		return nil
	})
	select {
	case <-started1:
	case <-time.After(10 * time.Second):
		t.Fatal("node 1 did not lead")
	}
	started2 := make(chan struct{}, 8)
	go c2.RunLeader(ctx, "refresher", func(lctx context.Context) error {
		started2 <- struct{}{}
		<-lctx.Done()
		return nil
	})

	// The server ends node 1's lease session (a failover, a network cut).
	c1.lockMu.Lock()
	pid := c1.lockConn.PgConn().PID()
	c1.lockMu.Unlock()
	if _, err := db2.ExecContext(ctx, `SELECT pg_terminate_backend(?)`, int64(pid)); err != nil {
		t.Fatalf("terminate node 1's lease session: %v", err)
	}
	select {
	case <-lost:
	case <-time.After(10 * time.Second):
		t.Fatal("node 1's task kept running after its lease session ended")
	}
	select {
	case <-started2:
	case <-time.After(10 * time.Second):
		t.Fatal("node 2 did not take the lease")
	}
}

func TestPublishAcrossNodes(t *testing.T) {
	c1, c2, _, _ := twoNodes(t)
	got1 := make(chan string, 8)
	got2 := make(chan string, 8)
	c1.Subscribe("policy", func(p string) {
		if p != Resync {
			got1 <- p
		}
	})
	c2.Subscribe("policy", func(p string) {
		if p != Resync {
			got2 <- p
		}
	})
	if err := c1.Publish(context.Background(), "policy", "changed"); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	for name, ch := range map[string]chan string{"node 1 (local)": got1, "node 2": got2} {
		select {
		case p := <-ch:
			if p != "changed" {
				t.Fatalf("%s got %q", name, p)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s got nothing", name)
		}
	}
	// Our own NOTIFY comes back on node 1's channel too; it is not
	// delivered twice.
	select {
	case p := <-got1:
		t.Fatalf("node 1 got its own message twice (%q)", p)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestListenReconnectResyncs(t *testing.T) {
	c1, c2, db1, _ := twoNodes(t)
	resync := make(chan struct{}, 8)
	got := make(chan string, 8)
	c2.Subscribe("settings", func(p string) {
		if p == Resync {
			resync <- struct{}{}
			return
		}
		got <- p
	})
	c2.listenMu.Lock()
	pid := c2.listenConn.PgConn().PID()
	c2.listenMu.Unlock()
	if _, err := db1.ExecContext(context.Background(), `SELECT pg_terminate_backend(?)`, int64(pid)); err != nil {
		t.Fatalf("terminate node 2's message session: %v", err)
	}
	select {
	case <-resync:
	case <-time.After(15 * time.Second):
		t.Fatal("no Resync after the message connection came back")
	}
	if err := c1.Publish(context.Background(), "settings", "after"); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	select {
	case p := <-got:
		if p != "after" {
			t.Fatalf("got %q", p)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no delivery after the reconnect")
	}
}

func TestPeriodicResync(t *testing.T) {
	storetest.SkipUnlessPostgres(t)
	db := storetest.Open(t)
	opts := fastOptions("solo")
	opts.ResyncInterval = 100 * time.Millisecond
	c := newCluster(t, db, opts)
	resync := make(chan struct{}, 8)
	c.Subscribe("anything", func(p string) {
		if p == Resync {
			resync <- struct{}{}
		}
	})
	select {
	case <-resync:
	case <-time.After(10 * time.Second):
		t.Fatal("no periodic Resync")
	}
}

func TestLockTwoNodes(t *testing.T) {
	c1, c2, db1, _ := twoNodes(t)
	ctx := context.Background()
	unlock, err := c1.Lock(ctx, "policy-write")
	if err != nil {
		t.Fatalf("node 1 Lock: %v", err)
	}
	short, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	if _, err := c2.Lock(short, "policy-write"); err == nil {
		t.Fatal("node 2 took a lock node 1 holds")
	}
	cancel()
	unlock()
	unlock2, err := c2.Lock(ctx, "policy-write")
	if err != nil {
		t.Fatalf("node 2 Lock after node 1 released: %v", err)
	}
	unlock2()

	// LockTx: a transaction on node 1 holds it until commit.
	tx, err := db1.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := LockTx(ctx, tx, db1.Dialect, "policy-write"); err != nil {
		t.Fatalf("LockTx: %v", err)
	}
	short, cancel = context.WithTimeout(ctx, 300*time.Millisecond)
	if _, err := c2.Lock(short, "policy-write"); err == nil {
		t.Fatal("node 2 took a lock a node 1 transaction holds")
	}
	cancel()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	unlock3, err := c2.Lock(ctx, "policy-write")
	if err != nil {
		t.Fatalf("node 2 Lock after the commit: %v", err)
	}
	unlock3()
}
