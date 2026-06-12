package events

import (
	"context"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/lake"
)

const lakeDDL = `CREATE TABLE IF NOT EXISTS raw.toolyard_events (
  id String,
  source_name String,
  type String,
  title String,
  summary String,
  created_at DateTime64(3),
  received_at DateTime64(3)
) ENGINE = MergeTree ORDER BY (source_name, type, received_at)`

const sinkTick = 30 * time.Second

// RunLakeSink streams events into the ClickHouse lake. Only run when a lake is
// configured. On start it ensures the target table; every 30s it selects up to
// 500 un-synced events, batch-inserts them, and marks them synced. A rare
// double-insert window (mark fails after insert) is acceptable for an analytics
// table.
//
// Shaped as a goroutines.Supervise fn (blocking; returns on ctx cancel).
func (s *Service) RunLakeSink(ctx context.Context, lk *lake.Service) error {
	if lk == nil {
		<-ctx.Done()
		return ctx.Err()
	}
	if _, err := lk.Exec(ctx, lakeDDL); err != nil {
		// Non-fatal: retry on next tick (Supervise also restarts on return).
		return err
	}
	t := time.NewTicker(sinkTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			if err := s.sinkBatch(ctx, lk); err != nil {
				return err
			}
		}
	}
}

func (s *Service) sinkBatch(ctx context.Context, lk *lake.Service) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT e.id, s.name, e.type, COALESCE(e.title,''), e.summary, e.created_at, e.received_at
         FROM events e JOIN event_sources s ON s.id = e.source_id
         WHERE e.lake_synced = 0 ORDER BY e.received_at LIMIT 500`)
	if err != nil {
		return err
	}
	type row struct {
		id, name, typ, title, summary string
		createdAt, receivedAt         int64
	}
	var batch []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.name, &r.typ, &r.title, &r.summary, &r.createdAt, &r.receivedAt); err != nil {
			rows.Close()
			return err
		}
		batch = append(batch, r)
	}
	rows.Close()
	if len(batch) == 0 {
		return nil
	}

	lakeRows := make([]map[string]any, 0, len(batch))
	ids := make([]string, 0, len(batch))
	for _, r := range batch {
		lakeRows = append(lakeRows, map[string]any{
			"id":          r.id,
			"source_name": r.name,
			"type":        r.typ,
			"title":       r.title,
			"summary":     r.summary,
			"created_at":  time.UnixMilli(r.createdAt),
			"received_at": time.UnixMilli(r.receivedAt),
		})
		ids = append(ids, r.id)
	}
	if _, err := lk.InsertRows(ctx, "raw.toolyard_events", lakeRows); err != nil {
		return err
	}
	return s.markEventSynced(ctx, ids)
}
