package cluster

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// Counter counts requests in the rate_limit_buckets table, so a limit
// holds across every replica of a multi-node database instead of once per
// process. Each bucket is one row. Its methods are safe for concurrent
// use; each is one statement.
type Counter struct {
	db *store.DB
}

// Counter returns the shared counter of a multi-node database, or nil
// (SQLite, or a nil Cluster), where a limit counts in process memory.
func (c *Cluster) Counter() *Counter {
	if !c.MultiNode() {
		return nil
	}
	return &Counter{db: c.db}
}

// NewCounter returns a Counter on db, for tests and single-purpose tools.
func NewCounter(db *store.DB) *Counter { return &Counter{db: db} }

// Allow counts one request against bucket when the bucket's window holds
// fewer than max requests, and reports whether it did. A window starts at
// the first request after the previous one ended and lasts window, as the
// in-process limiters count. A refused request is not counted and does
// not extend the window.
func (k *Counter) Allow(ctx context.Context, bucket string, max int, window time.Duration, now time.Time) (bool, error) {
	nowMs := now.UnixMilli()
	cutoff := now.Add(-window).UnixMilli()
	expires := now.Add(2 * window).UnixMilli()
	var hits int64
	// Every SET expression reads the row as it was before the update, on
	// both engines. hits ends one past max once a request was refused.
	err := k.db.QueryRowContext(ctx, `INSERT INTO rate_limit_buckets(bucket, window_start, hits, expires_at) VALUES (?, ?, 1, ?)
		ON CONFLICT (bucket) DO UPDATE SET
			hits = CASE WHEN rate_limit_buckets.window_start < CAST(? AS BIGINT) THEN 1
				WHEN rate_limit_buckets.hits < CAST(? AS BIGINT) THEN rate_limit_buckets.hits + 1
				ELSE CAST(? AS BIGINT) END,
			expires_at = CASE WHEN rate_limit_buckets.window_start < CAST(? AS BIGINT) THEN excluded.expires_at ELSE rate_limit_buckets.expires_at END,
			window_start = CASE WHEN rate_limit_buckets.window_start < CAST(? AS BIGINT) THEN excluded.window_start ELSE rate_limit_buckets.window_start END
		RETURNING hits`,
		bucket, nowMs, expires, cutoff, int64(max), int64(max)+1, cutoff, cutoff).Scan(&hits)
	if err != nil {
		return false, err
	}
	return hits <= int64(max), nil
}

// Count returns how many requests bucket counted (0 for no row). Use it
// with Add for a window that the bucket name itself fixes (one name per
// hour, say).
func (k *Counter) Count(ctx context.Context, bucket string) (int, error) {
	var hits int64
	err := k.db.QueryRowContext(ctx, `SELECT hits FROM rate_limit_buckets WHERE bucket = ?`, bucket).Scan(&hits)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return int(hits), err
}

// Add counts one request in bucket, without a limit. The row may be
// deleted from expires on.
func (k *Counter) Add(ctx context.Context, bucket string, now, expires time.Time) error {
	_, err := k.db.ExecContext(ctx, `INSERT INTO rate_limit_buckets(bucket, window_start, hits, expires_at) VALUES (?, ?, 1, ?)
		ON CONFLICT (bucket) DO UPDATE SET hits = rate_limit_buckets.hits + 1`,
		bucket, now.UnixMilli(), expires.UnixMilli())
	return err
}

// Reset forgets bucket.
func (k *Counter) Reset(ctx context.Context, bucket string) error {
	_, err := k.db.ExecContext(ctx, `DELETE FROM rate_limit_buckets WHERE bucket = ?`, bucket)
	return err
}

// Sweep deletes the buckets whose rows expired.
func (k *Counter) Sweep(ctx context.Context, now time.Time) error {
	_, err := k.db.ExecContext(ctx, `DELETE FROM rate_limit_buckets WHERE expires_at < ?`, now.UnixMilli())
	return err
}
