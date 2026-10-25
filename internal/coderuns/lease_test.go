package coderuns

import (
	"context"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/cluster"
	"github.com/tusharbhardwaj/toolyard/internal/codemode"
	"github.com/tusharbhardwaj/toolyard/internal/store/storetest"
)

// A rolling restart of node 1 (Recover at its start) leaves node 2's
// queued and running code runs alone; it settles the runs of a node whose
// lease ended and runs from before leases existed.
func TestRecoverKeepsOtherNodesRuns(t *testing.T) {
	storetest.SkipUnlessPostgres(t)
	db := storetest.Open(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c1, err := cluster.New(ctx, db, cluster.Options{InstanceID: "node-1", ResyncInterval: -1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c1.Close() })

	now := time.Now()
	insert := func(id, status string, owner, lease any) {
		t.Helper()
		if _, err := db.ExecContext(context.Background(), `INSERT INTO code_runs
			(id, agent_id, fingerprint, code, reason, status, created_at, updated_at, owner_instance, lease_until)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, "ag_a", "fp", "print(1)", testReason, status, now.UnixMilli(), now.UnixMilli(), owner, lease); err != nil {
			t.Fatal(err)
		}
	}
	live := cluster.LeaseUntil(now)
	insert("cr_live_running", StatusRunning, "node-2", live)
	insert("cr_live_queued", StatusQueued, "node-2", live)
	insert("cr_dead_running", StatusRunning, "node-dead", now.Add(-time.Second).UnixMilli())
	insert("cr_dead_queued", StatusQueued, "node-dead", now.Add(-time.Second).UnixMilli())
	insert("cr_old_running", StatusRunning, nil, nil)

	svc := New(db, Options{})
	svc.SetCluster(c1)
	failed, unknown, err := svc.Recover(context.Background())
	if err != nil || failed != 1 || unknown != 2 {
		t.Fatalf("recover = %d failed, %d unknown, %v; want 1 and 2", failed, unknown, err)
	}
	for id, want := range map[string]string{
		"cr_live_running": StatusRunning,
		"cr_live_queued":  StatusQueued,
		"cr_dead_running": codemode.StatusOutcomeUnknown,
		"cr_dead_queued":  codemode.StatusFailed,
		"cr_old_running":  codemode.StatusOutcomeUnknown,
	} {
		var got string
		if err := db.QueryRowContext(context.Background(), `SELECT status FROM code_runs WHERE id = ?`, id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%s: status %q, want %q", id, got, want)
		}
	}
}

// A run's row names the node that runs it, with a lease.
func TestRunCarriesLease(t *testing.T) {
	e := newEnv(t)
	now := time.Now()
	e.svc.now = func() time.Time { return now }
	e.insertState(t, "cr_lease", StatusQueued, now.UnixMilli())
	var owner string
	var lease int64
	if err := e.db.QueryRowContext(context.Background(), `SELECT owner_instance, lease_until FROM code_runs WHERE id = ?`, "cr_lease").Scan(&owner, &lease); err != nil {
		t.Fatal(err)
	}
	if owner != e.svc.cluster.InstanceID() || lease != cluster.LeaseUntil(now) {
		t.Fatalf("owner %q lease %d, want %q %d", owner, lease, e.svc.cluster.InstanceID(), cluster.LeaseUntil(now))
	}
}
