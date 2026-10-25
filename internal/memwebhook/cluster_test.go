package memwebhook

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/cluster"
	"github.com/tusharbhardwaj/toolyard/internal/store/storetest"
)

// On a multi-node database the worker requeues only a job whose lease
// ended: a job another node's worker is still running stays running.
func TestTwoNodeLeaseRecoverJobs(t *testing.T) {
	storetest.SkipUnlessPostgres(t)
	ing := &fakeIngester{available: true}
	s := newService(t, ing)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c, err := cluster.New(ctx, s.db, cluster.Options{InstanceID: "node-1", ResyncInterval: -1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	s.SetCluster(c)
	wh, _ := mustCreate(t, s, CreateInput{Name: "ha", Wing: "w"})
	live, err := enqueue(t, s, wh, "req_live", `{"transcript":"x"}`)
	if err != nil {
		t.Fatal(err)
	}
	dead, err := enqueue(t, s, wh, "req_dead", `{"transcript":"y"}`)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for id, lease := range map[string]int64{live.ID: now.Add(time.Hour).UnixMilli(), dead.ID: 1} {
		if _, err := s.db.ExecContext(context.Background(), `UPDATE memory_webhook_jobs SET status=?, owner_instance=?, lease_until=? WHERE id=?`,
			jobRunning, "node-2", lease, id); err != nil {
			t.Fatal(err)
		}
	}
	s.recoverRunningJobs(context.Background())
	for id, want := range map[string]string{live.ID: jobRunning, dead.ID: jobQueued} {
		got, err := s.GetJob(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != want {
			t.Errorf("%s: status %q, want %q", id, got.Status, want)
		}
	}
}

// holdLease ends the job's context when the job is no longer this node's
// (another node recovered and claimed it), and when no renewal succeeds in
// time, so the job stops before it writes again.
func TestTwoNodeHoldLeaseEndsJobContext(t *testing.T) {
	storetest.SkipUnlessPostgres(t)
	ing := &fakeIngester{available: true}
	s := newService(t, ing)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c, err := cluster.New(ctx, s.db, cluster.Options{InstanceID: "node-1", ResyncInterval: -1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	s.SetCluster(c)
	wh, _ := mustCreate(t, s, CreateInput{Name: "ha", Wing: "w"})
	oldLost, oldRenew := leaseLostAfter, leaseRenewEvery
	t.Cleanup(func() { leaseLostAfter, leaseRenewEvery = oldLost, oldRenew })

	claim := func(reqID string) string {
		t.Helper()
		j, err := enqueue(t, s, wh, reqID, `{"transcript":"x"}`)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE memory_webhook_jobs SET status=?, owner_instance=?, lease_until=? WHERE id=?`,
			jobRunning, c.InstanceID(), time.Now().Add(time.Minute).UnixMilli(), j.ID); err != nil {
			t.Fatal(err)
		}
		return j.ID
	}
	wantLost := func(jobCtx context.Context, what string) {
		t.Helper()
		select {
		case <-jobCtx.Done():
			if !errors.Is(context.Cause(jobCtx), errLeaseLost) {
				t.Fatalf("%s: cause %v, want errLeaseLost", what, context.Cause(jobCtx))
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s: the job's context did not end", what)
		}
	}

	// Another node took the job: the next renewal changes no row.
	leaseLostAfter, leaseRenewEvery = time.Minute, 50*time.Millisecond
	id := claim("req_taken")
	jobCtx, stop := s.holdLease(ctx, id)
	select {
	case <-jobCtx.Done():
		t.Fatal("the job's context ended while this node owns the job")
	case <-time.After(300 * time.Millisecond):
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE memory_webhook_jobs SET owner_instance=? WHERE id=?`, "node-2", id); err != nil {
		t.Fatal(err)
	}
	wantLost(jobCtx, "job taken by another node")
	stop()

	// No renewal succeeds in time.
	leaseLostAfter, leaseRenewEvery = 200*time.Millisecond, time.Hour
	jobCtx, stop = s.holdLease(ctx, claim("req_silent"))
	wantLost(jobCtx, "no renewal")
	stop()

	// A job that ends normally: stop ends its context without a lease loss.
	leaseLostAfter, leaseRenewEvery = time.Minute, 50*time.Millisecond
	jobCtx, stop = s.holdLease(ctx, claim("req_done"))
	stop()
	if jobCtx.Err() == nil || errors.Is(context.Cause(jobCtx), errLeaseLost) {
		t.Fatalf("after stop: err %v, cause %v", jobCtx.Err(), context.Cause(jobCtx))
	}
}
