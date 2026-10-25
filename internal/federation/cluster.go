package federation

// High availability (TEC-1630): the connection writes (connect, renew,
// host requests and decisions, revocation) read, check and write several
// rows, and Service.mu kept two of them from interleaving in one process.
// With several replicas on one Postgres database a cluster lock does the
// same across replicas.

import (
	"context"

	"github.com/tusharbhardwaj/toolyard/internal/cluster"
)

// SetCluster makes the connection writes take turns across every replica.
// Call it once, at startup. Without a multi-node cluster Service.mu
// serializes them, as before.
func (s *Service) SetCluster(c *cluster.Cluster) { s.cluster = c }

// lock serializes one connection write: Service.mu on a single node, a
// cluster lock with several.
func (s *Service) lock(ctx context.Context) (func(), error) {
	if !s.cluster.MultiNode() {
		s.mu.Lock()
		return s.mu.Unlock, nil
	}
	return s.cluster.Lock(ctx, "federation")
}
