package inbox

import (
	"context"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/cluster"
	"github.com/tusharbhardwaj/toolyard/internal/store"
	"github.com/tusharbhardwaj/toolyard/internal/store/storetest"
)

// haBase is the clock every node of a two-node test reads: the leases
// they write and compare share one time line.
var haBase = time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)

// newClusterFor starts a cluster node on db for the rest of the test.
func newClusterFor(t *testing.T, db *store.DB, id string) *cluster.Cluster {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	c, err := cluster.New(ctx, db, cluster.Options{InstanceID: id, ResyncInterval: -1})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		_ = c.Close()
	})
	return c
}

// newNode is one replica's Inbox on db, as cluster node c.
func newNode(t *testing.T, db *store.DB, c *cluster.Cluster) *testEnv {
	t.Helper()
	e := &testEnv{now: haBase}
	svc, err := New(context.Background(), Options{
		DB:      db,
		Catalog: testCatalog,
		Cluster: c,
		Now:     func() time.Time { e.mu.Lock(); defer e.mu.Unlock(); return e.now },
	})
	if err != nil {
		t.Fatal(err)
	}
	e.svc = svc
	t.Cleanup(svc.Flush)
	return e
}

// twoInboxNodes is two replicas, each with its own pool, on one Postgres
// database. It returns node 1's cluster and pool, and node 2's cluster.
func twoInboxNodes(t *testing.T) (*testEnv, *testEnv, *cluster.Cluster, *store.DB, *cluster.Cluster) {
	t.Helper()
	db1, db2 := storetest.OpenPostgresPair(t)
	c1 := newClusterFor(t, db1, "node-1")
	c2 := newClusterFor(t, db2, "node-2")
	return newNode(t, db1, c1), newNode(t, db2, c2), c1, db1, c2
}

func executionState(t *testing.T, db *store.DB, grantID string) (state, owner string) {
	t.Helper()
	var o *string
	if err := db.QueryRowContext(context.Background(), `SELECT state, owner_instance FROM inbox_executions WHERE grant_id=?`, grantID).Scan(&state, &o); err != nil {
		t.Fatalf("read execution %s: %v", grantID, err)
	}
	if o != nil {
		owner = *o
	}
	return state, owner
}

// A rolling restart of node 1 leaves node 2's in-flight execution alone:
// the restarted node does not recover at start, and recovery (the
// leader's) settles only the executions of a node that stopped (lease
// ended) and pre-lease rows.
func TestTwoNodeLeaseRollingRestartKeepsExecutions(t *testing.T) {
	_, node2, c1, db, c2 := twoInboxNodes(t)
	ctx := context.Background()

	r := submitBatch(t, node2)
	if _, err := node2.svc.Decide(ctx, r.ID, batchDecision(r, "approved", VerdictAccepted, VerdictRejected)); err != nil {
		t.Fatal(err)
	}
	views, err := node2.svc.Status(ctx, "ag_1", []string{r.ID})
	if err != nil || views[0].Tools[0].Grant == "" {
		t.Fatalf("no grant: %+v %v", views, err)
	}
	live, err := node2.svc.Redeem(ctx, views[0].Tools[0].Grant, "ag_1", "db.migrate", map[string]any{"env": "stage", "migration": "0042"})
	if err != nil {
		t.Fatalf("redeem on node 2: %v", err)
	}
	if state, owner := executionState(t, db, live.ID); state != ExecutionRunning || owner != c2.InstanceID() {
		t.Fatalf("node 2's execution: state %q owner %q", state, owner)
	}

	// A node that stopped mid-call (its lease ended long ago), and a row
	// from before leases existed.
	now := haBase.UnixMilli()
	for _, row := range []struct {
		grant string
		owner any
		lease any
	}{
		{"gr_dead", "node-dead", int64(1)},
		{"gr_old", nil, nil},
	} {
		if _, err := db.ExecContext(ctx, `INSERT INTO inbox_executions(grant_id,request_id,call_id,agent_id,state,attempt_id,arguments,started_at,owner_instance,lease_until)
			VALUES(?,?,?,?,?,?,?,?,?,?)`, row.grant, r.ID, "file_beta", "ag_1", ExecutionRunning, "gu_"+row.grant, "{}", now-60000, row.owner, row.lease); err != nil {
			t.Fatal(err)
		}
	}

	// Node 1 restarts: a new Inbox starts without settling anything.
	restarted := newNode(t, db, c1)
	for _, g := range []string{live.ID, "gr_dead", "gr_old"} {
		if state, _ := executionState(t, db, g); state != ExecutionRunning {
			t.Fatalf("a multi-node start settled %s: state %q", g, state)
		}
	}
	// Recovery (the leader's pass) settles only what no live node renews.
	if n, err := restarted.svc.RecoverExecutions(ctx); err != nil || n != 2 {
		t.Fatalf("recovery: %d %v, want 2", n, err)
	}
	if state, owner := executionState(t, db, live.ID); state != ExecutionRunning || owner != c2.InstanceID() {
		t.Fatalf("node 1's recovery changed node 2's in-flight execution: state %q owner %q", state, owner)
	}
	for _, g := range []string{"gr_dead", "gr_old"} {
		if state, _ := executionState(t, db, g); state != ExecutionUnknown {
			t.Fatalf("%s: state %q, want %q", g, state, ExecutionUnknown)
		}
	}
	if n, err := restarted.svc.RecoverExecutions(ctx); err != nil || n != 0 {
		t.Fatalf("second recovery: %d %v", n, err)
	}
	// Each node names only its own running executions at shutdown.
	if ids, err := node2.svc.RunningExecutions(ctx); err != nil || len(ids) != 1 || ids[0] != live.ID {
		t.Fatalf("node 2 running executions: %v %v", ids, err)
	}
	if ids, err := restarted.svc.RunningExecutions(ctx); err != nil || len(ids) != 0 {
		t.Fatalf("node 1 running executions: %v %v", ids, err)
	}

	// Node 2 renews its lease (by the database's clock, on its reserved
	// connection); once node 2 stops renewing and the lease ends, node 1's
	// recovery takes the execution.
	if _, err := db.ExecContext(ctx, `UPDATE inbox_executions SET lease_until = 1 WHERE grant_id = ?`, live.ID); err != nil {
		t.Fatal(err)
	}
	if err := node2.svc.renewLeases(ctx); err != nil {
		t.Fatal(err)
	}
	var lease int64
	if err := db.QueryRowContext(ctx, `SELECT lease_until FROM inbox_executions WHERE grant_id = ?`, live.ID).Scan(&lease); err != nil {
		t.Fatal(err)
	}
	if time.Until(time.UnixMilli(lease)) < cluster.WorkLease/2 {
		t.Fatalf("renewal set lease_until %d, not about a lease from now", lease)
	}
	if _, err := db.ExecContext(ctx, `UPDATE inbox_executions SET lease_until = 1 WHERE grant_id = ?`, live.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := restarted.svc.RecoverExecutions(ctx); err != nil || n != 1 {
		t.Fatalf("recovery after node 2's lease ended: %d %v", n, err)
	}
	if state, _ := executionState(t, db, live.ID); state != ExecutionUnknown {
		t.Fatalf("expired execution: state %q", state)
	}
}

// Wait on node 2 wakes when node 1 records the decision.
func TestTwoNodeWaitWakesOnDecision(t *testing.T) {
	// A poll far longer than the test's bound: only the cluster message
	// can wake the wait in time.
	old := waitPoll
	waitPoll = 30 * time.Second
	t.Cleanup(func() { waitPoll = old })
	node1, node2, _, _, _ := twoInboxNodes(t)
	ctx := context.Background()
	r := submitDeploy(t, node1, "ag_1")

	type result struct {
		views []AgentView
		err   error
		took  time.Duration
	}
	done := make(chan result, 1)
	go func() {
		start := time.Now()
		views, err := node2.svc.Wait(ctx, "ag_1", []string{r.ID}, "any", 30*time.Second)
		done <- result{views, err, time.Since(start)}
	}()
	// The waiter is registered on node 2 before node 1 decides.
	deadline := time.Now().Add(5 * time.Second)
	for {
		node2.svc.watchMu.Lock()
		_, waiting := node2.svc.watch[r.ID]
		node2.svc.watchMu.Unlock()
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("node 2 never started waiting")
		}
		time.Sleep(10 * time.Millisecond)
	}
	decidedAt := time.Now()
	if _, err := node1.svc.Decide(ctx, r.ID, Decision{Action: "deny", Note: "Not this week"}); err != nil {
		t.Fatalf("decide on node 1: %v", err)
	}
	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("wait: %v", res.err)
		}
		if len(res.views) != 1 || res.views[0].Status == StatusPending {
			t.Fatalf("wait returned %+v", res.views)
		}
		// The fallback poll runs every 30 s from the start of the wait: a
		// wake-up within 5 s came through the cluster message.
		if took := time.Since(decidedAt); took >= 5*time.Second {
			t.Fatalf("node 2's wait returned %s after the decision: the cluster wake-up did not arrive, only the poll", took)
		}
		t.Logf("node 2's wait returned %s after node 1's decision", res.took)
	case <-time.After(10 * time.Second):
		t.Fatal("node 2's wait did not wake after node 1 decided")
	}
}

// Submissions of one agent take turns across nodes: the per-agent cap
// holds when both nodes submit at once.
func TestTwoNodeSubmitLock(t *testing.T) {
	node1, node2, _, _, _ := twoInboxNodes(t)
	ctx := context.Background()
	unlock, err := node1.svc.lockSubmit(ctx, "ag_1")
	if err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	if _, err := node2.svc.lockSubmit(short, "ag_1"); err == nil {
		t.Fatal("node 2 took the submit lock node 1 holds")
	}
	other, err := node2.svc.lockSubmit(ctx, "ag_2")
	if err != nil {
		t.Fatalf("another agent's lock: %v", err)
	}
	other()
	unlock()
	again, err := node2.svc.lockSubmit(ctx, "ag_1")
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	again()
}

// A due push read by two dispatchers goes out once.
func TestClusterPushClaimedOnce(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.svc.db.ExecContext(ctx, `INSERT INTO inbox_pushes(request_id, agent_id, group_key, reason, urgency, due_at) VALUES (?,?,?,?,?,?)`,
		"rq_x", "ag_1", "rq_x", "new", UrgencyNow, e.svc.now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := e.svc.db.QueryRowContext(ctx, `SELECT id FROM inbox_pushes WHERE request_id = ?`, "rq_x").Scan(&id); err != nil {
		t.Fatal(err)
	}
	if !e.svc.claimPush(ctx, id) {
		t.Fatal("first claim failed")
	}
	if e.svc.claimPush(ctx, id) {
		t.Fatal("a claimed push was claimed again")
	}
}
