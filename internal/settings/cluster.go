package settings

// High availability (TEC-1630): every replica reads settings from its own
// cache. A write on one replica publishes on the cluster and every other
// replica reloads the table; the periodic cluster resync heals a missed
// message.

import (
	"context"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/cluster"
	"github.com/tusharbhardwaj/toolyard/internal/logx"
)

// clusterTopic carries "the stored settings changed".
const clusterTopic = "settings"

// SetCluster connects the cache to the other replicas. Call it once, at
// startup. Without a multi-node cluster the cache works as before.
func (s *Service) SetCluster(c *cluster.Cluster) {
	s.cluster = c
	if !c.MultiNode() {
		return
	}
	c.Subscribe(clusterTopic, func(string) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := s.reloadAll(ctx); err != nil {
			logx.For("settings").Warn("settings: reload after a change on another node failed; keeping the cached values", "err", err.Error())
		}
	})
}

// publishChange tells the other replicas to reload.
func (s *Service) publishChange(ctx context.Context) {
	if !s.cluster.MultiNode() {
		return
	}
	if err := s.cluster.Publish(context.WithoutCancel(ctx), clusterTopic, "changed"); err != nil {
		logx.For("settings").Warn("settings: telling other nodes about a change failed; they catch up at the next resync", "err", err.Error())
	}
}
