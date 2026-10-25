package push

// High availability (TEC-1630): every replica signs Web Push with the
// VAPID key pair in server_keys. A rotation on one replica tells the
// others to read the new pair; the periodic cluster resync heals a missed
// message.

import (
	"context"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/cluster"
	"github.com/tusharbhardwaj/toolyard/internal/logx"
)

// clusterTopic carries "the VAPID keys were rotated".
const clusterTopic = "push-keys"

// SetCluster connects the key cache to the other replicas. Call it once,
// at startup. Without a multi-node cluster nothing changes.
func (s *Service) SetCluster(c *cluster.Cluster) {
	s.cluster = c
	if !c.MultiNode() {
		return
	}
	c.Subscribe(clusterTopic, func(string) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		pub, priv, err := loadOrCreateVapid(ctx, s.db)
		if err != nil {
			logx.For("push").Warn("push: reloading the VAPID keys failed; keeping the cached pair", "err", err.Error())
			return
		}
		s.keyMu.Lock()
		s.vapidPublic, s.vapidPriv = pub, priv
		s.keyMu.Unlock()
	})
}

// publishRotation tells the other replicas to read the new key pair.
func (s *Service) publishRotation(ctx context.Context) {
	if !s.cluster.MultiNode() {
		return
	}
	if err := s.cluster.Publish(context.WithoutCancel(ctx), clusterTopic, "rotated"); err != nil {
		logx.For("push").Warn("push: telling other nodes about the key rotation failed; they catch up at the next resync", "err", err.Error())
	}
}
