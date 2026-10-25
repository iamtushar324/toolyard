package memwebhook

// High availability (TEC-1630): the job worker runs on one replica at a
// time (cluster.RunLeader in cmd/gateway). Enqueue may run on any replica,
// so it wakes the worker through the cluster, and each running job holds
// the worker's lease so a worker that moved never runs a job twice.

import (
	"context"
	"errors"
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

// errLeaseLost ends a job's context when its lease can no longer be kept.
var errLeaseLost = errors.New("memwebhook: the job's lease was lost")

// leaseLostAfter is how long a job runs without a successful renewal before
// it stops: well before its lease ends (cluster.WorkLease, 60 s) and
// another node's recovery may settle it, even when a renewal that started
// just before blocks for its full query timeout. leaseRenewEvery is how
// often the lease is renewed. Tests shorten both.
var (
	leaseLostAfter  = cluster.WorkLease - 15*time.Second
	leaseRenewEvery = cluster.WorkLeaseRenew
)

// holdLease renews the lease of running job id until the returned stop is
// called, and returns the context the job must run under. That context
// ends (cause errLeaseLost) when a renewal finds the job no longer this
// node's, or when no renewal succeeded for leaseLostAfter, so the job stops
// before another node may take it. Without a multi-node cluster nothing
// reads the lease, so it does nothing and returns ctx.
func (s *Service) holdLease(ctx context.Context, id string) (jobCtx context.Context, stop func()) {
	if s.db == nil || !s.cluster.MultiNode() {
		return ctx, func() {}
	}
	jctx, cancelJob := context.WithCancelCause(ctx)
	lctx, cancel := context.WithCancel(jctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(leaseRenewEvery)
		defer t.Stop()
		lost := time.NewTimer(leaseLostAfter)
		defer lost.Stop()
		for {
			select {
			case <-lctx.Done():
				return
			case <-lost.C:
				logx.For("memwebhook").Warn("memwebhook: no lease renewal succeeded in time; stopping the job so another node may resume it", "job", id, "after", leaseLostAfter.String())
				cancelJob(errLeaseLost)
				return
			case <-t.C:
			}
			n, err := s.cluster.ExecReserved(lctx, `UPDATE memory_webhook_jobs SET lease_until=`+cluster.LeaseUntilSQL(s.db.Dialect)+
				` WHERE id=? AND status='running' AND owner_instance=?`, id, s.cluster.InstanceID())
			if lctx.Err() != nil {
				return
			}
			switch {
			case err == nil && n == 0:
				logx.For("memwebhook").Warn("memwebhook: the job is no longer this node's; stopping it", "job", id)
				cancelJob(errLeaseLost)
				return
			case err == nil:
				lost.Reset(leaseLostAfter)
			}
		}
	}()
	return jctx, func() {
		cancel()
		<-done
		cancelJob(nil)
	}
}
