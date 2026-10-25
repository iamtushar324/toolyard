package memwebhook

import (
	"context"
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
