package policy

// High availability (TEC-1630): every replica evaluates calls from its own
// cache of tool_policies. A write on one replica publishes on the cluster,
// and every replica (this one included) reloads, so a deny set through
// any replica is enforced by all of them within moments; the periodic
// cluster resync heals a missed message.

import (
	"context"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/cluster"
	"github.com/tusharbhardwaj/toolyard/internal/logx"
)

// clusterTopic carries "the stored policies changed".
const clusterTopic = "policy"

// SetCluster connects the engine to the other replicas. Call it once, at
// startup, before the engine serves. Without a multi-node cluster (SQLite,
// tests) the engine works as before.
func (e *Engine) SetCluster(c *cluster.Cluster) {
	e.cluster = c
	if !c.MultiNode() || e.db == nil {
		return
	}
	// The reload reads the database: it runs on a goroutine of its own,
	// off the cluster's delivery goroutine.
	c.SubscribeReload(clusterTopic, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := e.reload(ctx); err != nil {
			logx.For("policy").Warn("policy: reload after a change on another node failed; keeping the last good rules", "err", err.Error())
		}
	})
}

// lockWrite takes the write lock: writeMu, and on a multi-node database a
// cluster lock, after which the cache is reloaded so a guarded Change
// judges the rules another replica may have just written.
func (e *Engine) lockWrite(ctx context.Context) (func(), error) {
	e.writeMu.Lock()
	if !e.cluster.MultiNode() {
		return e.writeMu.Unlock, nil
	}
	unlock, err := e.cluster.Lock(ctx, "policy-write")
	if err != nil {
		e.writeMu.Unlock()
		return nil, err
	}
	if err := e.reload(ctx); err != nil {
		unlock()
		e.writeMu.Unlock()
		return nil, err
	}
	return func() {
		unlock()
		e.writeMu.Unlock()
	}, nil
}

// publishChange tells every replica to reload. A failure is logged: the
// other replicas then catch up at the next resync.
func (e *Engine) publishChange(ctx context.Context) {
	if !e.cluster.MultiNode() {
		return
	}
	if err := e.cluster.Publish(context.WithoutCancel(ctx), clusterTopic, "changed"); err != nil {
		logx.For("policy").Warn("policy: telling other nodes about a change failed; they catch up at the next resync", "err", err.Error())
	}
}
