package oauth

// High availability (TEC-1630): every replica keeps its own cache of the
// bearers it sends upstream. A replica that writes a token (exchange,
// refresh, PAT, reauth, disconnect) tells the others to drop their cached
// copy; their next call reads the database. Refreshes and the per-person
// read-modify-write sections take cluster locks, so two replicas never
// spend one rotating refresh token.

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/cluster"
	"github.com/tusharbhardwaj/toolyard/internal/logx"
)

// bearerTopic carries bearerMsg: a bearer changed on another node.
const bearerTopic = "oauth-bearer"

// userLockWait bounds the wait for a person's cluster lock; past it the
// in-process lock alone applies (and the write's refresh-token check still
// refuses a stale write).
const userLockWait = 45 * time.Second

// bearerMsg names the cache entry to drop: the shared bearer of Upstream
// (User ""), one person's (User set), or every bearer of Upstream (All).
type bearerMsg struct {
	Instance string `json:"n"`
	Upstream string `json:"u"`
	User     string `json:"p,omitempty"`
	All      bool   `json:"a,omitempty"`
}

// SetCluster connects the bearer caches to the other replicas. Call it
// once, at startup, before the refresher runs. Without a multi-node
// cluster the service works as before.
func (s *Service) SetCluster(c *cluster.Cluster) {
	s.cluster = c
	if !c.MultiNode() {
		return
	}
	c.Subscribe(bearerTopic, func(payload string) {
		if payload == cluster.Resync {
			// Messages may have been missed: every cached bearer is read
			// again on its next use, and served as it is while the
			// database cannot answer.
			s.mu.Lock()
			for k := range s.bearers {
				s.staleBearers[k] = true
			}
			for k := range s.userBearers {
				s.staleBearers[k] = true
			}
			s.mu.Unlock()
			return
		}
		var m bearerMsg
		if json.Unmarshal([]byte(payload), &m) != nil || m.Instance == c.InstanceID() {
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		switch {
		case m.All:
			s.dropBearerLocked(m.Upstream)
			prefix := m.Upstream + "\x00"
			for k := range s.userBearers {
				if strings.HasPrefix(k, prefix) {
					s.dropUserBearerLocked(k)
				}
			}
		case m.User != "":
			s.dropUserBearerLocked(userKey(m.Upstream, m.User))
		default:
			s.dropBearerLocked(m.Upstream)
		}
	})
}

// dropBearerLocked forgets one upstream's shared bearer; a database read
// already under way is not cached (bearerGen). s.mu is held.
func (s *Service) dropBearerLocked(upstream string) {
	delete(s.bearers, upstream)
	delete(s.staleBearers, upstream)
	s.bearerGen[upstream]++
}

// dropUserBearerLocked forgets one person's bearer (k is userKey). s.mu is
// held.
func (s *Service) dropUserBearerLocked(k string) {
	delete(s.userBearers, k)
	delete(s.staleBearers, k)
	s.bearerGen[k]++
}

// publishBearer tells the other replicas that a bearer this node wrote
// changed.
func (s *Service) publishBearer(upstream, userID string, all bool) {
	if !s.cluster.MultiNode() {
		return
	}
	body, err := json.Marshal(bearerMsg{Instance: s.cluster.InstanceID(), Upstream: upstream, User: userID, All: all})
	if err == nil {
		err = s.cluster.Publish(context.Background(), bearerTopic, string(body))
	}
	if err != nil {
		logx.For("oauth").Warn("oauth: telling other nodes about a token change failed; they catch up at the next resync", "upstream", upstream, "err", err.Error())
	}
}

// lockUserCluster takes the cluster lock for one person's token row (key
// is userKey). ok is false without a multi-node cluster, or when the lock
// could not be taken: lockUser then falls back to its in-process lock.
func (s *Service) lockUserCluster(key string) (func(), bool) {
	if !s.cluster.MultiNode() {
		return nil, false
	}
	// Bounded: a refresh holds the lock for one provider round trip.
	ctx, cancel := context.WithTimeout(context.Background(), userLockWait)
	defer cancel()
	unlock, err := s.cluster.Lock(ctx, "oauth-user:"+key)
	if err != nil {
		logx.For("oauth").Warn("oauth: cluster lock for a person's token failed; locking in process only", "err", err.Error())
		return nil, false
	}
	return unlock, true
}

// lockShared serializes refreshes of one upstream's shared token across
// replicas. Without a multi-node cluster it does nothing: the refresher is
// the only caller, one at a time.
func (s *Service) lockShared(ctx context.Context, upstream string) (func(), error) {
	if !s.cluster.MultiNode() {
		return func() {}, nil
	}
	return s.cluster.Lock(ctx, "oauth-shared:"+upstream)
}
