package skillhub

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// usageKey is one skill on one day (days since the Unix epoch, UTC).
type usageKey struct {
	source string
	path   string
	day    int64
}

// usageBuffer counts reads in memory between flushes, so a read never
// waits for a database write.
type usageBuffer struct {
	mu     sync.Mutex
	counts map[usageKey]int64
}

// day is the UTC day number of t.
func day(t time.Time) int64 { return t.UTC().Unix() / 86400 }

// countRead counts one read of a skill.
func (s *Service) countRead(sourceID, skillPath string) {
	k := usageKey{source: sourceID, path: skillPath, day: day(s.now())}
	s.usage.mu.Lock()
	s.usage.counts[k]++
	s.usage.mu.Unlock()
}

// flushUsage writes the buffered read counts. Counts it could not write
// go back into the buffer.
func (s *Service) flushUsage(ctx context.Context) error {
	s.usage.mu.Lock()
	counts := s.usage.counts
	s.usage.counts = map[usageKey]int64{}
	s.usage.mu.Unlock()
	if len(counts) == 0 {
		return nil
	}
	var firstErr error
	for k, n := range counts {
		if firstErr == nil {
			_, err := s.db.ExecContext(ctx, `INSERT INTO skill_usage (source_id, skill_path, day, reads) VALUES (?, ?, ?, ?)
				ON CONFLICT (source_id, skill_path, day) DO UPDATE SET reads = skill_usage.reads + excluded.reads`,
				k.source, k.path, k.day, n)
			if err == nil {
				continue
			}
			firstErr = fmt.Errorf("skillhub: write read counts: %w", err)
		}
		s.usage.mu.Lock()
		s.usage.counts[k] += n
		s.usage.mu.Unlock()
	}
	return firstErr
}

// runUsageFlush writes read counts every usageFlushEvery until ctx ends,
// then once more.
func (s *Service) runUsageFlush(ctx context.Context) {
	t := time.NewTicker(usageFlushEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			fctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = s.flushUsage(fctx)
			cancel()
			return
		case <-t.C:
			if err := s.flushUsage(ctx); err != nil && ctx.Err() == nil {
				s.log.Warn("skillhub: writing read counts failed; they are kept for the next try", "err", err.Error())
			}
		}
	}
}

// usageKeepDays is how many days of read counts are kept.
const usageKeepDays = 90

// pruneUsage deletes read counts older than usageKeepDays.
func (s *Service) pruneUsage(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM skill_usage WHERE day < ?`, day(s.now())-usageKeepDays); err != nil {
		return fmt.Errorf("skillhub: prune read counts: %w", err)
	}
	return nil
}

// reads30d sums each skill's reads over the last 30 days.
func (s *Service) reads30d(ctx context.Context, sourceID string) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT skill_path, CAST(SUM(reads) AS BIGINT) FROM skill_usage
		WHERE source_id = ? AND day > ? GROUP BY skill_path`, sourceID, day(s.now())-30)
	if err != nil {
		return nil, fmt.Errorf("skillhub: read counts: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var p string
		var n int64
		if err := rows.Scan(&p, &n); err != nil {
			return nil, fmt.Errorf("skillhub: read counts: %w", err)
		}
		out[p] = int(n)
	}
	return out, rows.Err()
}
