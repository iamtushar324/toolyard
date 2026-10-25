package access

// High availability (TEC-1630): the scope cache lives in each replica's
// memory. A write on one replica (a scope edit, a group grant, an app
// revoke, an agent disabled or deleted) drops the matching entries on
// every replica through the cluster, so a revoke is in force everywhere at
// once, not only after cacheTTL. Without a multi-node cluster nothing is
// sent: one process holds the only cache.

import (
	"context"
	"strings"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/cluster"
	"github.com/tusharbhardwaj/toolyard/internal/logx"
)

const (
	// invalidateTopic carries a cache drop: msgAll, msgUser+id or
	// msgAgent+id.
	invalidateTopic = "access-invalidate"
	msgAll          = "all"
	msgUser         = "user:"
	msgAgent        = "agent:"
	// publishTimeout bounds the cluster send after a write; a send that
	// fails leaves the other replicas on cacheTTL.
	publishTimeout = 2 * time.Second
)

// SetCluster makes cache drops reach every replica. Call it before the
// service serves. A Resync (messages may have been missed) drops the
// whole cache: every entry is read again on next use.
func (s *Service) SetCluster(c *cluster.Cluster) {
	if !c.MultiNode() {
		return
	}
	s.cluster.Store(c)
	c.Subscribe(invalidateTopic, s.onInvalidate)
}

// onInvalidate applies a drop another replica sent. It only touches the
// maps, so it is quick enough for the cluster's delivery goroutine.
func (s *Service) onInvalidate(p string) {
	switch {
	case p == cluster.Resync || p == msgAll:
		s.dropUser("")
	case strings.HasPrefix(p, msgUser):
		s.dropUser(strings.TrimPrefix(p, msgUser)) // "" drops everything
	case strings.HasPrefix(p, msgAgent):
		s.dropAgent(strings.TrimPrefix(p, msgAgent))
	}
}

// publish sends a drop to the other replicas.
func (s *Service) publish(payload string) {
	c := s.cluster.Load()
	if c == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), publishTimeout)
	defer cancel()
	if err := c.PublishOthers(ctx, invalidateTopic, payload); err != nil {
		logx.For("access").Warn("access: telling the other replicas to drop a cached scope failed; they read it again within the cache TTL", "err", err.Error(), "ttl", cacheTTL.String())
	}
}
