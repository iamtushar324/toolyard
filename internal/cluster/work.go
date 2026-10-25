package cluster

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/logx"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// Work leases (the "Durable work" rule of docs/design/v0.1.md): a row a
// node executes names the node (owner_instance) and holds a lease
// (lease_until, Unix milliseconds) the node renews while the work runs.
// Lease times come from the database's clock, never a node's, so clock
// skew between nodes cannot end a live lease early.
const (
	// WorkLease is how long a row a node executes (an Inbox execution, a
	// legacy execution claim, a code run, a memory-webhook job) stays
	// claimed without a renewal. Recovery takes a row only once its lease
	// ended.
	WorkLease = 60 * time.Second
	// WorkLeaseRenew is how often a node renews the leases of the work it
	// runs.
	WorkLeaseRenew = 15 * time.Second
)

// NowMillisSQL is an SQL expression for the database's current time in
// Unix milliseconds.
func NowMillisSQL(d store.Dialect) string {
	if d.IsPostgres() {
		return `CAST(EXTRACT(EPOCH FROM clock_timestamp()) * 1000 AS BIGINT)`
	}
	return `CAST((julianday('now') - 2440587.5) * 86400000 AS INTEGER)`
}

// LeaseUntilSQL is an SQL expression for the end of a work lease taken or
// renewed now: the database's time plus WorkLease.
func LeaseUntilSQL(d store.Dialect) string {
	return "(" + NowMillisSQL(d) + " + " + strconv.FormatInt(WorkLease.Milliseconds(), 10) + ")"
}

// LeaseEndedSQL is an SQL condition, true when the lease in column col
// ended by the database's clock or the row never had one (it predates
// leases). col is trusted SQL.
func LeaseEndedSQL(d store.Dialect, col string) string {
	return "(" + col + " IS NULL OR " + col + " < " + NowMillisSQL(d) + ")"
}

// RenewLeases runs renew every WorkLeaseRenew until ctx ends, logging a
// failure under name. Every node runs one for each kind of work it
// executes, outside RunLeader: each renews only its own rows.
func RenewLeases(ctx context.Context, name string, renew func(ctx context.Context) error) {
	t := time.NewTicker(WorkLeaseRenew)
	defer t.Stop()
	var lg logOnce
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := renew(ctx); err != nil {
			if ctx.Err() == nil {
				lg.warn(logx.For("cluster").With("work", name), "cluster: renewing work leases failed; another node may recover this work once the leases end", err)
			}
			continue
		}
		lg.reset()
	}
}

// ExecReserved runs one statement (? placeholders) on a connection the
// cluster keeps for lease renewals, outside the pool, so a busy pool never
// delays a renewal past the lease. It returns the rows affected. It needs
// a multi-node cluster (a single node renews nothing).
func (c *Cluster) ExecReserved(ctx context.Context, query string, args ...any) (int64, error) {
	if !c.MultiNode() {
		return 0, errors.New("cluster: ExecReserved needs a multi-node cluster")
	}
	s := &c.work
	s.mu.Lock()
	defer s.mu.Unlock()
	conn, err := c.connLocked(s)
	if err != nil {
		return 0, err
	}
	qctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	tag, err := conn.Exec(qctx, store.Rebind(query), args...)
	if err != nil {
		// The connection may be mid-protocol: open a fresh one next time.
		c.dropLocked(s, "a reserved statement", err)
		return 0, err
	}
	return tag.RowsAffected(), nil
}
