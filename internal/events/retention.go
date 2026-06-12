package events

import (
	"context"
	"time"
)

// RunRetention wakes daily and purges old events. retentionDays is read live
// each tick via getDays so a settings change applies without a restart.
// hasLake reports whether a lake sink is active (when false, the lake_synced
// condition is dropped so events aren't pinned forever waiting for a sink that
// will never run).
//
// Two tiers:
//   - Normal: delete events older than the window that are acked AND
//     (lake_synced OR no-lake).
//   - Hard floor: delete events older than 4× the window unconditionally, so a
//     never-acked / never-synced backlog can't grow without bound.
//
// Shaped as a goroutines.Supervise fn (blocking; returns on ctx cancel).
func (s *Service) RunRetention(ctx context.Context, getDays func() int, hasLake func() bool) error {
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	s.purgeOnce(ctx, getDays(), hasLake())
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			s.purgeOnce(ctx, getDays(), hasLake())
		}
	}
}

func (s *Service) purgeOnce(ctx context.Context, days int, hasLake bool) (int64, error) {
	if days <= 0 {
		days = 90
	}
	now := time.Now()
	cutoff := now.Add(-time.Duration(days) * 24 * time.Hour).UnixMilli()
	hardFloor := now.Add(-4 * time.Duration(days) * 24 * time.Hour).UnixMilli()

	var total int64
	// Normal tier.
	q := `DELETE FROM events WHERE received_at < ? AND acked_at IS NOT NULL`
	if hasLake {
		q += ` AND lake_synced = 1`
	}
	if res, err := s.db.ExecContext(ctx, q, cutoff); err == nil {
		n, _ := res.RowsAffected()
		total += n
	} else {
		return total, err
	}
	// Hard floor.
	if res, err := s.db.ExecContext(ctx, `DELETE FROM events WHERE received_at < ?`, hardFloor); err == nil {
		n, _ := res.RowsAffected()
		total += n
	} else {
		return total, err
	}
	return total, nil
}
