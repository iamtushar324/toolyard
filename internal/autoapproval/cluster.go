package autoapproval

// High availability (TEC-1630): every replica matches calls against its
// own cache of auto_approval_rules. A change on one replica publishes on
// the cluster and every replica reloads; the periodic cluster resync heals
// a missed message. The per-agent hourly budget counts in the database
// (rate_limit_buckets), so replicas share it instead of each granting the
// full budget.

import (
	"context"
	"strconv"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/cluster"
	"github.com/tusharbhardwaj/toolyard/internal/logx"
	"github.com/tusharbhardwaj/toolyard/internal/settings"
)

const (
	// clusterTopic carries "the stored rules changed".
	clusterTopic = "autoapproval"
	// rateTimeout bounds one shared budget read or write: the budget is
	// checked on the call path.
	rateTimeout = 2 * time.Second
)

// SetCluster connects the rules cache and the budget to the other
// replicas. Call it once, at startup. Without a multi-node cluster both
// stay in process, as before.
func (s *Service) SetCluster(c *cluster.Cluster) {
	s.cluster = c
	s.shared = c.Counter()
	if !c.MultiNode() {
		return
	}
	c.Subscribe(clusterTopic, func(string) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := s.reload(ctx); err != nil {
			logx.For("autoapproval").Warn("autoapproval: reload after a change on another node failed; keeping the cached rules", "err", err.Error())
		}
	})
}

// reloadChanged reloads the cache after a write and tells the other
// replicas to reload theirs.
func (s *Service) reloadChanged(ctx context.Context) error {
	if s.cluster.MultiNode() {
		if err := s.cluster.Publish(context.WithoutCancel(ctx), clusterTopic, "changed"); err != nil {
			logx.For("autoapproval").Warn("autoapproval: telling other nodes about a change failed; they catch up at the next resync", "err", err.Error())
		}
	}
	return s.reload(ctx)
}

// rateBucketName is the shared budget row of one agent for the hour that
// holds now.
func rateBucketName(agentID string, now time.Time) string {
	return "autoapproval:" + agentID + ":" + strconv.FormatInt(now.Truncate(time.Hour).UnixMilli(), 10)
}

// takeSharedBudget takes one auto-approval from the agent's shared hourly
// budget, in one conditional increment, and reports whether it did. When
// the budget cannot be read the call goes to a person: a database fault
// never grants more auto-approvals than the cap.
func (s *Service) takeSharedBudget(agentID string) bool {
	if s.settings == nil {
		return true
	}
	cap := s.settings.GetInt(settings.AutoApprovalRateLimitPerHour, 100)
	if cap <= 0 {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), rateTimeout)
	defer cancel()
	now := time.Now()
	ok, err := s.shared.Allow(ctx, rateBucketName(agentID, now), cap, 2*time.Hour, now)
	if err != nil {
		logx.For("autoapproval").Warn("autoapproval: the shared budget cannot be read; the call waits for a person", "err", err.Error())
		return false
	}
	return ok
}
