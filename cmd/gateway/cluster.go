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
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/approval"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/cluster"
	"github.com/tusharbhardwaj/toolyard/internal/coderuns"
	"github.com/tusharbhardwaj/toolyard/internal/goroutines"
	"github.com/tusharbhardwaj/toolyard/internal/inbox"
	"github.com/tusharbhardwaj/toolyard/internal/keyguard"
	"github.com/tusharbhardwaj/toolyard/internal/realtime"
	"github.com/tusharbhardwaj/toolyard/internal/store"
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
// replica at a time (see leaderLoop). An error from fn gives up the lease
// and goes back to Supervise, which logs it and starts fn again after its
// growing backoff, as before; a lost lease is taken again by RunLeader.
func leaderSupervise(ctx context.Context, c *cluster.Cluster, name string, maxBackoff time.Duration, fn func(context.Context) error) {
	goroutines.Supervise(ctx, name, maxBackoff, func(ctx context.Context) error {
		var failed error
		c.RunLeader(ctx, name, func(lctx context.Context) error {
			if err := fn(lctx); err != nil && lctx.Err() == nil {
				failed = err
			}
			return nil
		})
		return failed
	})
}

// clusterMode is serve's -cluster setting.
type clusterMode string

const (
	clusterAuto clusterMode = "auto"
	clusterOn   clusterMode = "on"
	clusterOff  clusterMode = "off"
)

// parseClusterMode reads -cluster: auto is on for Postgres and off for
// SQLite; on is refused on SQLite (one process per database file).
func parseClusterMode(v string, d store.Dialect) (clusterMode, error) {
	switch m := clusterMode(strings.ToLower(strings.TrimSpace(v))); m {
	case clusterAuto, "":
		if d.IsPostgres() {
			return clusterOn, nil
		}
		return clusterOff, nil
	case clusterOn:
		if !d.IsPostgres() {
			return "", fmt.Errorf("-cluster=on needs Postgres: a SQLite database serves one gateway (%w)", keyguard.ErrConfig)
		}
		return clusterOn, nil
	case clusterOff:
		return clusterOff, nil
	default:
		return "", fmt.Errorf("-cluster %q: want auto, on or off (%w)", v, keyguard.ErrConfig)
	}
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
//
// It waits one WorkLease after it takes over and after the database last
// failed to answer: a live replica that could not renew during an outage
// (or while no leader ran) gets that long to renew before its work could
// read as abandoned.
//
// Startup starts its leader loop where a single node runs its startup
// recovery, once the upstream servers (the tool list a re-run legacy
// execution needs) and the Inbox hooks are loaded.
func runWorkRecovery(ctx context.Context, c *cluster.Cluster, ping func(context.Context) error, inboxSvc *inbox.Service, bus *approval.Bus, codeRuns *coderuns.Service) {
	if !c.MultiNode() {
		return
	}
	resumeAt := time.Now().Add(cluster.WorkLease)
	t := time.NewTicker(workRecoveryEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := ping(ctx); err != nil {
			resumeAt = time.Now().Add(cluster.WorkLease)
			continue
		}
		if time.Now().Before(resumeAt) {
			continue
		}
		failed := false
		if n, err := inboxSvc.RecoverExecutions(ctx); err != nil {
			log.Printf("work recovery: inbox executions: %v", err)
			failed = true
		} else if n > 0 {
			log.Printf("work recovery: %d Inbox execution(s) of a stopped replica marked outcome_unknown", n)
		}
		if n, err := bus.SweepUnexecuted(ctx); err != nil {
			log.Printf("work recovery: legacy executions: %v", err)
			failed = true
		} else if n > 0 {
			log.Printf("work recovery: started %d unclaimed legacy execution(s)", n)
		}
		if nFailed, unknown, err := codeRuns.Recover(ctx); err != nil {
			log.Printf("work recovery: code runs: %v", err)
			failed = true
		} else if nFailed+unknown > 0 {
			log.Printf("work recovery: %d code run(s) of a stopped replica failed, %d marked outcome_unknown", nFailed, unknown)
		}
		if failed {
			resumeAt = time.Now().Add(cluster.WorkLease)
			continue
		}
		loopDone(loopWorkRecovery)
	}
}
