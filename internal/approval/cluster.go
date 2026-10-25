package approval

// High availability (TEC-1630): what the bus needs when several replicas
// share one Postgres database. SetCluster names this node; without a
// multi-node cluster (SQLite, tests) the bus keeps its single-process
// behaviour.

import (
	"context"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/cluster"
	"github.com/tusharbhardwaj/toolyard/internal/logx"
)

const (
	// wakeTopic carries the id of an approval whose waiters must re-read
	// it (decided, executed, cancelled), or wakeAll after an expiry sweep.
	wakeTopic = "approval-wake"
	wakeAll   = "*"
	// holdPoll is how often an in-line Hold re-reads its request on a
	// multi-node database, in case a wake-up message was missed.
	holdPoll = 2 * time.Second
	// LeaseRenewalLoop names RunLeaseRenewal in the supervised loop.
	LeaseRenewalLoop = "approval-lease-renewal"
)

// SetCluster makes the bus safe for replicas sharing the database: waiters
// wake on decisions made on any node, execution claims carry this node's
// lease, and recovery takes only claims whose lease ended. Call it before
// SweepUnexecuted.
func (b *Bus) SetCluster(c *cluster.Cluster) {
	b.cluster = c
	if !c.MultiNode() {
		return
	}
	c.Subscribe(wakeTopic, func(id string) {
		if id == cluster.Resync || id == wakeAll {
			b.signalAllLocal()
			return
		}
		b.signalLocal(id)
	})
}

// multiNode reports whether other replicas may share the database.
func (b *Bus) multiNode() bool { return b.cluster.MultiNode() }

// publishWake tells the other nodes to wake the waiters of id (or wakeAll).
func (b *Bus) publishWake(id string) {
	if !b.multiNode() {
		return
	}
	if err := b.cluster.Publish(context.Background(), wakeTopic, id); err != nil {
		logx.For("approval").Warn("approval: wake-up for other nodes failed; their waiters poll", "approval_id", id, "err", err.Error())
	}
}

// signalAllLocal wakes every waiter of this node.
func (b *Bus) signalAllLocal() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, w := range b.waiters {
		select {
		case <-w.done:
		default:
			close(w.done)
		}
	}
}

// claimArgs are the owner_instance and lease_until values of a claim this
// node takes now.
func (b *Bus) claimArgs() (string, int64) {
	return b.cluster.InstanceID(), cluster.LeaseUntil(time.Now())
}

// RunLeaseRenewal renews the leases of the execution claims this node
// holds until ctx ends. Every node runs it (not a leader task): each
// renews only its own claims. Without a multi-node cluster it returns at
// once.
func (b *Bus) RunLeaseRenewal(ctx context.Context) {
	if !b.multiNode() {
		return
	}
	cluster.RenewLeases(ctx, LeaseRenewalLoop, func(ctx context.Context) error {
		_, err := b.db.ExecContext(ctx, `UPDATE legacy_execution_claims SET lease_until=? WHERE owner_instance=? AND state='running'`,
			cluster.LeaseUntil(time.Now()), b.cluster.InstanceID())
		return err
	})
}
