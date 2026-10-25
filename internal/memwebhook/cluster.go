package memwebhook

// High availability (TEC-1630): the job worker runs on one replica at a
// time (cluster.RunLeader in cmd/gateway). Enqueue may run on any replica,
// so it wakes the worker through the cluster, and each running job holds
// the worker's lease so a worker that moved never runs a job twice.

import (
	"context"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/cluster"
	"github.com/tusharbhardwaj/toolyard/internal/logx"
)

// wakeTopic tells the worker, wherever it runs, that a job was queued.
const wakeTopic = "memwebhook-wake"

// SetCluster names this node and lets a job queued on any node wake the
// worker. Call it before RunWorker starts.
func (s *Service) SetCluster(c *cluster.Cluster) {
	s.cluster = c
	if c.MultiNode() {
		// A Resync drains too: it costs one query when nothing is queued.
		c.Subscribe(wakeTopic, func(string) { s.signalWake() })
	}
}

// publishWake wakes the worker on another node.
func (s *Service) publishWake(ctx context.Context) {
	if !s.cluster.MultiNode() {
		return
	}
	if err := s.cluster.Publish(context.WithoutCancel(ctx), wakeTopic, "queued"); err != nil {
		logx.For("memwebhook").Warn("memwebhook: waking the worker on another node failed; it picks the job up on its next tick", "err", err.Error())
	}
}

// holdLease renews the lease of running job id until the returned stop is
// called. Without a multi-node cluster nothing reads the lease, so it does
// nothing.
func (s *Service) holdLease(ctx context.Context, id string) (stop func()) {
	if s.db == nil || !s.cluster.MultiNode() {
		return func() {}
	}
	lctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(cluster.WorkLeaseRenew)
		defer t.Stop()
		for {
			select {
			case <-lctx.Done():
				return
			case <-t.C:
				_, _ = s.cluster.ExecReserved(lctx, `UPDATE memory_webhook_jobs SET lease_until=`+cluster.LeaseUntilSQL(s.db.Dialect)+
					` WHERE id=? AND status=? AND owner_instance=?`, id, jobRunning, s.cluster.InstanceID())
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}
