package main

// TEC-1630 high availability: how the gateway's background loops run when
// several replicas share one Postgres database. A loop that must run on
// one replica at a time runs under a cluster leader lease (leaderLoop,
// leaderSupervise); on SQLite, or with one replica, the lease is granted
// at once and the loop runs as before. A loop that is safe, or needed, on
// every replica is started as before and says why where it starts.

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/approval"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/cluster"
	"github.com/tusharbhardwaj/toolyard/internal/coderuns"
	"github.com/tusharbhardwaj/toolyard/internal/goroutines"
	"github.com/tusharbhardwaj/toolyard/internal/inbox"
	"github.com/tusharbhardwaj/toolyard/internal/realtime"
)

// workRecoveryEvery is how often the leader of a multi-node database looks
// for durable work whose owner stopped (its lease ended).
const workRecoveryEvery = 30 * time.Second

// leaderLoop is goroutines.Loop for a loop that must run on one replica
// at a time: fn runs while this replica holds the lease name (its context
// ends when the lease is lost), and the lease is taken again until ctx
// ends. A normal return of fn ends the loop, as with goroutines.Loop.
func leaderLoop(ctx context.Context, c *cluster.Cluster, name string, maxBackoff time.Duration, fn func(context.Context)) {
	goroutines.Loop(ctx, name, maxBackoff, func(ctx context.Context) {
		c.RunLeader(ctx, name, func(lctx context.Context) error {
			fn(lctx)
			return nil
		})
	})
}

// leaderSupervise is goroutines.Supervise for a loop that must run on one
// replica at a time (see leaderLoop). An error from fn is retried under
// the lease.
func leaderSupervise(ctx context.Context, c *cluster.Cluster, name string, maxBackoff time.Duration, fn func(context.Context) error) {
	goroutines.Supervise(ctx, name, maxBackoff, func(ctx context.Context) error {
		c.RunLeader(ctx, name, fn)
		return nil
	})
}

// registerRealtimeTypes lets live events from other replicas reach a
// dashboard stream with the data type its filter checks
// (api.personalEventVisible): approvals and audit rows.
func registerRealtimeTypes(hub *realtime.Hub) {
	hub.RegisterType("approval", func(raw json.RawMessage) (any, error) {
		var r approval.Request
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, err
		}
		return &r, nil
	})
	hub.RegisterType("audit", func(raw json.RawMessage) (any, error) {
		var e audit.Event
		if err := json.Unmarshal(raw, &e); err != nil {
			return nil, err
		}
		return e, nil
	})
}

// runWorkRecovery settles, on a multi-node database, the durable work of
// a replica that stopped: Inbox executions and legacy claims become
// outcome_unknown, allowed legacy calls nobody claimed run (under a claim,
// so once), and code runs settle, each only once its lease ended. The
// leader runs it; a single node recovers at startup only, as before.
func runWorkRecovery(ctx context.Context, c *cluster.Cluster, inboxSvc *inbox.Service, bus *approval.Bus, codeRuns *coderuns.Service) {
	if !c.MultiNode() {
		return
	}
	t := time.NewTicker(workRecoveryEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if n, err := inboxSvc.RecoverExecutions(ctx); err != nil {
			log.Printf("work recovery: inbox executions: %v", err)
		} else if n > 0 {
			log.Printf("work recovery: %d Inbox execution(s) of a stopped replica marked outcome_unknown", n)
		}
		if n, err := bus.SweepUnexecuted(ctx); err != nil {
			log.Printf("work recovery: legacy executions: %v", err)
		} else if n > 0 {
			log.Printf("work recovery: started %d unclaimed legacy execution(s)", n)
		}
		if failed, unknown, err := codeRuns.Recover(ctx); err != nil {
			log.Printf("work recovery: code runs: %v", err)
		} else if failed+unknown > 0 {
			log.Printf("work recovery: %d code run(s) of a stopped replica failed, %d marked outcome_unknown", failed, unknown)
		}
		loopDone(loopWorkRecovery)
	}
}
