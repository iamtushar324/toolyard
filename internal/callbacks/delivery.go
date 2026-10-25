package callbacks

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/logx"
)

// Delivery limits. Each process runs at most deliveryWorkers deliveries at
// once, and every process together keeps at most deliveryPerReceiver
// events of one receiver in flight, so one slow or hanging destination
// holds a worker, not the queue. Claims stay atomic in the database (the
// fenced UPDATE in deliver), so the loop is safe on every node.
const (
	deliveryWorkers     = 8
	deliveryPerReceiver = 2
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

// pickDue returns up to limit due push events, oldest first, without more
// than deliveryPerReceiver in flight for any receiver (counting the claims
// every node holds).
func (s *Service) pickDue(ctx context.Context, limit int) ([]string, error) {
	now := s.now().UnixMilli()
	inflight := map[string]int{}
	rows, err := s.db.QueryContext(ctx, `SELECT receiver_id,count(*) FROM callback_outbox WHERE status='delivering' AND claim_until>? GROUP BY receiver_id`, now)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var ref string
		var n int
		if err := rows.Scan(&ref, &n); err != nil {
			rows.Close()
			return nil, err
		}
		inflight[ref] = n
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	rows, err = s.db.QueryContext(ctx, `SELECT id,receiver_id FROM callback_outbox WHERE receiver_id IN (SELECT id FROM callback_receivers WHERE transport='push') AND ((status='pending' AND next_attempt_at<=?) OR (status='delivering' AND claim_until<=?)) ORDER BY next_attempt_at,id LIMIT ?`, now, now, limit*8)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() && len(out) < limit {
		var id, ref string
		if err := rows.Scan(&id, &ref); err != nil {
			return nil, err
		}
		if inflight[ref] >= deliveryPerReceiver {
			continue
		}
		inflight[ref]++
		out = append(out, id)
	}
	return out, rows.Err()
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
	rows, err := tx.QueryContext(ctx, `SELECT id FROM callback_outbox WHERE status IN ('delivered','failed') AND created_at<? ORDER BY created_at LIMIT ?`, cutoff, sweepBatch)
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
