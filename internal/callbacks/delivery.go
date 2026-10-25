package callbacks

import (
	"context"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/logx"
)

// Delivery limits. Each process runs at most deliveryWorkers deliveries at
// once. Every process together keeps at most deliveryPerReceiver events of
// one receiver in flight: the claim (claim in service.go) checks it
// atomically. pickDue also keeps at most deliveryPerAgent events of one
// agent and deliveryPerHost of one destination host in flight; it counts
// the claims every node holds when it picks, so two nodes picking at the
// same moment can pass those two caps by a few. One slow or hanging
// destination, agent or host holds a few workers, not the queue.
const (
	deliveryWorkers     = 8
	deliveryPerReceiver = 2
	deliveryPerAgent    = 4
	deliveryPerHost     = 4
	// OutboxRetention is how long delivered and failed events (and their
	// attempt history) are kept.
	OutboxRetention = 30 * 24 * time.Hour
	sweepBatch      = 500
)

// Run delivers due push events and sweeps old ones until ctx ends. Every
// second it starts as many deliveries as there are free workers; a
// delivery runs in its own goroutine and frees its worker when it ends.
// The hourly sweep is safe on every node: it deletes in batches what any
// node may delete.
func (s *Service) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	sweep := time.NewTicker(time.Hour)
	defer sweep.Stop()
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.dispatchDue(ctx, &wg)
		case <-sweep.C:
			if _, err := s.Sweep(ctx); err != nil && ctx.Err() == nil {
				logx.For("callbacks").Warn("callback outbox sweep failed", "err", err.Error())
			}
		}
	}
}

// dispatchDue starts a delivery for each due event it may take now.
func (s *Service) dispatchDue(ctx context.Context, wg *sync.WaitGroup) {
	free := cap(s.slots) - len(s.slots)
	if free <= 0 {
		return
	}
	ids, err := s.pickDue(ctx, free)
	if err != nil {
		return
	}
	for _, id := range ids {
		select {
		case s.slots <- struct{}{}:
		default:
			return
		}
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			defer func() { <-s.slots }()
			_ = s.deliver(ctx, id)
		}(id)
	}
}

// pickDue returns up to limit due push events, oldest first, within the
// in-flight caps per receiver, agent and destination host (counting the
// claims every node holds). The due window holds at most
// deliveryPerReceiver events of a receiver, deliveryPerAgent of an agent
// and deliveryPerHost of a host, so a receiver, an agent or a shared host
// with a long backlog cannot fill the window and starve the others (no
// head-of-line blocking). A receiver without a host (registered by an
// older version during a rolling upgrade) counts its destination as its
// host.
func (s *Service) pickDue(ctx context.Context, limit int) ([]string, error) {
	now := s.now().UnixMilli()
	perReceiver, perAgent, perHost := map[string]int{}, map[string]int{}, map[string]int{}
	rows, err := s.db.QueryContext(ctx, `SELECT o.receiver_id,r.agent_id,`+hostKey+`,count(*) FROM callback_outbox o JOIN callback_receivers r ON r.id=o.receiver_id WHERE o.status='delivering' AND o.claim_until>? GROUP BY o.receiver_id,r.agent_id,`+hostKey, now)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var ref, agent, host string
		var n int
		if err := rows.Scan(&ref, &agent, &host, &n); err != nil {
			rows.Close()
			return nil, err
		}
		perReceiver[ref] += n
		perAgent[agent] += n
		perHost[host] += n
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	rows, err = s.db.QueryContext(ctx, `SELECT id,receiver_id,agent_id,host_key FROM (
 SELECT id,receiver_id,agent_id,host_key,next_attempt_at,
  ROW_NUMBER() OVER (PARTITION BY host_key ORDER BY next_attempt_at,id) AS host_rank
 FROM (
  SELECT id,receiver_id,agent_id,host_key,next_attempt_at,
   ROW_NUMBER() OVER (PARTITION BY agent_id ORDER BY next_attempt_at,id) AS agent_rank
  FROM (
   SELECT o.id,o.receiver_id,r.agent_id,`+hostKey+` AS host_key,o.next_attempt_at,
    ROW_NUMBER() OVER (PARTITION BY o.receiver_id ORDER BY o.next_attempt_at,o.id) AS receiver_rank
   FROM callback_outbox o JOIN callback_receivers r ON r.id=o.receiver_id
   WHERE r.transport='push' AND ((o.status='pending' AND o.next_attempt_at<=?) OR (o.status='delivering' AND o.claim_until<=?))
  ) per_receiver WHERE receiver_rank<=?
 ) per_agent WHERE agent_rank<=?
) per_host WHERE host_rank<=? ORDER BY next_attempt_at,id LIMIT ?`, now, now, deliveryPerReceiver, deliveryPerAgent, deliveryPerHost, limit*8)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() && len(out) < limit {
		var id, ref, agent, host string
		if err := rows.Scan(&id, &ref, &agent, &host); err != nil {
			return nil, err
		}
		if perReceiver[ref] >= deliveryPerReceiver || perAgent[agent] >= deliveryPerAgent || perHost[host] >= deliveryPerHost {
			continue
		}
		perReceiver[ref]++
		perAgent[agent]++
		perHost[host]++
		out = append(out, id)
	}
	return out, rows.Err()
}

// hostKey is the SQL for a receiver r's host: callback_receivers.host, or
// its destination when host is empty.
const hostKey = `COALESCE(NULLIF(r.host,''),r.destination)`

// destinationHost is the lowercase host name of a receiver's destination,
// stored in callback_receivers.host: an IPv6 literal without brackets, ""
// for a pull receiver, the destination itself when it does not parse.
// Migration 0062 derives the same value for older receivers.
func destinationHost(destination string) string {
	u, err := url.Parse(destination)
	if err != nil || u.Hostname() == "" {
		return strings.ToLower(destination)
	}
	return strings.ToLower(u.Hostname())
}

// Sweep deletes delivered and failed events older than OutboxRetention,
// with their attempt history, and old approval subscriptions, in batches.
// It returns the number of events deleted. Concurrent sweeps on several
// nodes delete disjoint or already-deleted rows and are harmless.
func (s *Service) Sweep(ctx context.Context) (int, error) {
	cutoff := s.now().Add(-OutboxRetention).UnixMilli()
	total := 0
	for {
		n, err := s.sweepBatch(ctx, cutoff)
		total += n
		if err != nil || n < sweepBatch {
			if err == nil {
				_, err = s.db.ExecContext(ctx, `DELETE FROM callback_subject_subscriptions WHERE created_at<?`, cutoff)
			}
			return total, err
		}
	}
}

func (s *Service) sweepBatch(ctx context.Context, cutoff int64) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	// A row an older node queued during a rolling upgrade has created_at
	// 0: its next attempt time stands in, so it is neither swept at once
	// nor kept for ever. Each branch can use an index.
	rows, err := tx.QueryContext(ctx, `SELECT id FROM callback_outbox WHERE status IN ('delivered','failed') AND ((created_at>0 AND created_at<?) OR (created_at=0 AND next_attempt_at<?)) ORDER BY created_at,next_attempt_at LIMIT ?`, cutoff, cutoff, sweepBatch)
	if err != nil {
		return 0, err
	}
	var ids []any
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, nil
	}
	in := "(" + strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",") + ")"
	if _, err := tx.ExecContext(ctx, `DELETE FROM callback_attempts WHERE event_id IN `+in, ids...); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM callback_outbox WHERE status IN ('delivered','failed') AND id IN `+in, ids...); err != nil {
		return 0, err
	}
	return len(ids), tx.Commit()
}
