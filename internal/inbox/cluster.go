package inbox

// High availability (TEC-1630): what the Inbox needs when several replicas
// share one Postgres database. Options.Cluster names this node; without
// one (SQLite, tests) everything here keeps the single-process behaviour.

import (
	"context"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/cluster"
)

const (
	// wakeTopic carries the id of a request that left pending, so a Wait
	// on another node re-reads it.
	wakeTopic = "inbox-wake"
	// waitPoll is how often a Wait re-reads its requests on a multi-node
	// database, in case a wake-up message was missed.
	waitPoll = 2 * time.Second
	// LeaseRenewalLoop names RunLeaseRenewal in the supervised loop.
	LeaseRenewalLoop = "inbox-lease-renewal"
	// legacySyncTopic asks the replica that runs the legacy sync loop for
	// a pass (RequestClusterLegacySync).
	legacySyncTopic = "inbox-legacy-sync"
)

// multiNode reports whether other replicas may share the database.
func (s *Service) multiNode() bool { return s.opts.Cluster.MultiNode() }

// instance is this node's cluster instance ID (the owner_instance of the
// executions it runs).
func (s *Service) instance() string { return s.opts.Cluster.InstanceID() }

// subscribeCluster makes a wake-up published by any node reach this
// node's waiters. A Resync wakes every waiter: each re-reads its requests.
func (s *Service) subscribeCluster() {
	if !s.multiNode() {
		return
	}
	s.opts.Cluster.Subscribe(wakeTopic, func(id string) {
		if id == cluster.Resync {
			s.wakeAllLocal()
			return
		}
		s.wakeLocal(id)
	})
	s.opts.Cluster.Subscribe(legacySyncTopic, func(p string) {
		if p != cluster.Resync {
			s.RequestLegacySync()
		}
	})
}

// RequestClusterLegacySync is RequestLegacySync for whichever replica runs
// the legacy sync loop (one at a time, under a cluster leader lease): this
// one, and through the cluster every other. Signals still coalesce.
func (s *Service) RequestClusterLegacySync() {
	s.RequestLegacySync()
	if s.multiNode() {
		_ = s.opts.Cluster.Publish(context.Background(), legacySyncTopic, "sync")
	}
}

// wakeAllLocal wakes every waiter of this node.
func (s *Service) wakeAllLocal() {
	s.watchMu.Lock()
	chans := s.watch
	s.watch = map[string]chan struct{}{}
	s.watchMu.Unlock()
	for _, c := range chans {
		close(c)
	}
}

// lockSubmit serializes one agent's submissions. On a multi-node database
// it is a cluster lock per agent (the pending-request cap, the dry-run
// budget and the urgent budget are all per agent); otherwise it is the
// process-wide submitMu, as before.
func (s *Service) lockSubmit(ctx context.Context, agentID string) (func(), error) {
	if !s.multiNode() {
		s.submitMu.Lock()
		return s.submitMu.Unlock, nil
	}
	return s.opts.Cluster.Lock(ctx, "inbox-submit:"+agentID)
}

// lockBlob serializes the steps that adopt or delete one blob's bytes (an
// upload and the upload sweep). On a multi-node database it is a cluster
// lock per sha256, otherwise the process-wide uploadMu, as before.
func (s *Service) lockBlob(ctx context.Context, sha string) (func(), error) {
	if !s.multiNode() {
		s.uploadMu.Lock()
		return s.uploadMu.Unlock, nil
	}
	return s.opts.Cluster.Lock(ctx, "inbox-blob:"+sha)
}

// RunLeaseRenewal renews the leases of the executions this node runs until
// ctx ends. Every node runs it (not a leader task): each renews only its
// own rows. Without a multi-node cluster it returns at once.
func (s *Service) RunLeaseRenewal(ctx context.Context) {
	if !s.multiNode() {
		return
	}
	cluster.RenewLeases(ctx, LeaseRenewalLoop, s.renewLeases)
}

// renewLeases extends the lease of every running execution this node owns.
func (s *Service) renewLeases(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE inbox_executions SET lease_until=? WHERE owner_instance=? AND state=?`,
		cluster.LeaseUntil(s.now()), s.instance(), ExecutionRunning)
	return err
}
