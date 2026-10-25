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

// twoInboxNodes is two replicas on one Postgres database.
func twoInboxNodes(t *testing.T) (*testEnv, *testEnv, *cluster.Cluster, *store.DB) {
	t.Helper()
	storetest.SkipUnlessPostgres(t)
	db := storetest.Open(t)
	c1 := newClusterFor(t, db, "node-1")
	c2 := newClusterFor(t, db, "node-2")
	return newNode(t, db, c1), newNode(t, db, c2), c1, db
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

// A rolling restart of node 1 (its startup recovery runs) leaves node 2's
// in-flight execution alone, and still settles the executions of a node
// that stopped (lease ended) and of a pre-lease row.
func TestRollingRestartKeepsOtherNodesExecutions(t *testing.T) {
	_, node2, c1, db := twoInboxNodes(t)
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
	if state, owner := executionState(t, db, live.ID); state != ExecutionRunning || owner != "node-2" {
		t.Fatalf("node 2's execution: state %q owner %q", state, owner)
	}

	// A node that stopped mid-call, and a row from before leases existed.
	now := haBase.UnixMilli()
	for _, row := range []struct {
		grant string
		owner any
		lease any
	}{
		{"gr_dead", "node-dead", now - 1000},
		{"gr_old", nil, nil},
	} {
		if _, err := db.ExecContext(ctx, `INSERT INTO inbox_executions(grant_id,request_id,call_id,agent_id,state,attempt_id,arguments,started_at,owner_instance,lease_until)
			VALUES(?,?,?,?,?,?,?,?,?,?)`, row.grant, r.ID, "file_beta", "ag_1", ExecutionRunning, "gu_"+row.grant, "{}", now-60000, row.owner, row.lease); err != nil {
			t.Fatal(err)
		}
	}

	// Node 1 restarts: a new Inbox runs its startup recovery.
	restarted := newNode(t, db, c1)
	if state, owner := executionState(t, db, live.ID); state != ExecutionRunning || owner != "node-2" {
		t.Fatalf("node 1's restart changed node 2's in-flight execution: state %q owner %q", state, owner)
	}
	for _, g := range []string{"gr_dead", "gr_old"} {
		if state, _ := executionState(t, db, g); state != ExecutionUnknown {
			t.Fatalf("%s: state %q, want %q", g, state, ExecutionUnknown)
		}
	}

	// Recovery again (the leader's periodic pass) still leaves it alone.
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

	// Node 2 renews its lease; once node 2 stops renewing and the lease
	// ends, node 1's recovery takes the execution.
	if err := node2.svc.renewLeases(ctx); err != nil {
		t.Fatal(err)
	}
	restarted.advance(cluster.WorkLease + time.Second)
	if n, err := restarted.svc.RecoverExecutions(ctx); err != nil || n != 1 {
		t.Fatalf("recovery after node 2's lease ended: %d %v", n, err)
	}
	if state, _ := executionState(t, db, live.ID); state != ExecutionUnknown {
		t.Fatalf("expired execution: state %q", state)
	}
}

// Wait on node 2 wakes when node 1 records the decision.
func TestWaitWakesOnOtherNodesDecision(t *testing.T) {
	node1, node2, _, _ := twoInboxNodes(t)
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
		t.Logf("node 2's wait returned %s after node 1's decision", res.took)
	case <-time.After(10 * time.Second):
		t.Fatal("node 2's wait did not wake after node 1 decided")
	}
}

// Submissions of one agent take turns across nodes: the per-agent cap
// holds when both nodes submit at once.
func TestSubmitLockSpansNodes(t *testing.T) {
	node1, node2, _, _ := twoInboxNodes(t)
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
func TestPushClaimedOnce(t *testing.T) {
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
