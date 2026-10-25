package coderuns

// High availability (TEC-1630): code runs carry this node's lease while
// they are queued or running, so another replica's recovery never settles
// a run that is still going here.

import (
	"context"

	"github.com/tusharbhardwaj/toolyard/internal/cluster"
)

// LeaseRenewalLoop names RunLeaseRenewal in the supervised loop.
const LeaseRenewalLoop = "code-runs-lease-renewal"

// SetCluster names this node. Call it before Recover and before the
// routes serve. Without a multi-node cluster, Recover settles every queued
// or running run (one process per database).
func (s *Service) SetCluster(c *cluster.Cluster) { s.cluster = c }

// RunLeaseRenewal renews the leases of this node's queued and running runs
// until ctx ends. Every node runs it (not a leader task): each renews only
// its own runs. Without a multi-node cluster it returns at once.
func (s *Service) RunLeaseRenewal(ctx context.Context) {
	if !s.cluster.MultiNode() {
		return
	}
	cluster.RenewLeases(ctx, LeaseRenewalLoop, func(ctx context.Context) error {
		_, err := s.cluster.ExecReserved(ctx, `UPDATE code_runs SET lease_until = `+cluster.LeaseUntilSQL(s.db.Dialect)+
			` WHERE owner_instance = ? AND status IN (?, ?)`, s.cluster.InstanceID(), StatusQueued, StatusRunning)
		return err
	})
}
