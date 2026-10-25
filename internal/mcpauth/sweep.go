package mcpauth

// sweep.go holds the rate limiter and the sweeper. Both keep their state in
// the database, so every replica sees the same counts and any replica may
// sweep.

import (
	"context"
	"log"
	"sync"
	"time"
)

// memLimiter is the in-process pre-limit in front of the database counter:
// once this node alone has seen more than max hits in a window, it refuses
// without writing a row, so a flood costs no database writes. It never
// allows what the shared counter would refuse: it only refuses earlier.
type memLimiter struct {
	mu sync.Mutex
	m  map[string]memBucket
}

type memBucket struct {
	hits  int
	reset time.Time
}

// maxMemBuckets bounds the pre-limit's memory; past it, finished windows
// are dropped, and if that is not enough the map starts over.
const maxMemBuckets = 50000

func (l *memLimiter) allow(key string, max int, window time.Duration, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.m == nil {
		l.m = map[string]memBucket{}
	}
	if len(l.m) >= maxMemBuckets {
		for k, b := range l.m {
			if !now.Before(b.reset) {
				delete(l.m, k)
			}
		}
		if len(l.m) >= maxMemBuckets {
			l.m = map[string]memBucket{}
		}
	}
	b := l.m[key]
	if !now.Before(b.reset) {
		b = memBucket{reset: now.Add(window)}
	}
	b.hits++
	l.m[key] = b
	return b.hits <= max
}

// allow counts one hit on bucket in a fixed window and reports whether the
// bucket is still within max. The in-process pre-limit answers first; a
// database error refuses (fails closed).
func (s *Service) allow(ctx context.Context, bucket string, max int, window time.Duration) bool {
	if !s.mem.allow(bucket, max, window, s.now()) {
		return false
	}
	now := s.now().UnixMilli()
	var hits int64
	err := s.db.QueryRowContext(ctx,
		`INSERT INTO mcp_oauth_rate_limits(bucket, hits, reset_at) VALUES(?, 1, ?)
         ON CONFLICT(bucket) DO UPDATE SET
             hits = CASE WHEN mcp_oauth_rate_limits.reset_at <= CAST(? AS BIGINT) THEN 1 ELSE mcp_oauth_rate_limits.hits + 1 END,
             reset_at = CASE WHEN mcp_oauth_rate_limits.reset_at <= CAST(? AS BIGINT) THEN excluded.reset_at ELSE mcp_oauth_rate_limits.reset_at END
         RETURNING hits`,
		bucket, now+window.Milliseconds(), now, now).Scan(&hits)
	if err != nil {
		log.Printf("mcpauth: WARN rate limit %s: %v", bucket, err)
		return false
	}
	return hits <= int64(max)
}

// AllowIP is allow for the HTTP layer (the consent page): one bucket per
// route and client address (IPv6 by /64, a missing address in a strict
// shared bucket).
func (s *Service) AllowIP(ctx context.Context, route, ip string, max int, window time.Duration) bool {
	key := rateKey(ip)
	return s.allow(ctx, route+":"+key, limitFor(key, max), window)
}

// remember marks key (a negative cache entry) for ttl in the shared
// counter table; recalled reports whether it is still marked.
func (s *Service) remember(ctx context.Context, key string, ttl time.Duration) {
	until := s.now().Add(ttl).UnixMilli()
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO mcp_oauth_rate_limits(bucket, hits, reset_at) VALUES(?, 0, ?)
         ON CONFLICT(bucket) DO UPDATE SET reset_at = excluded.reset_at`, key, until); err != nil {
		log.Printf("mcpauth: WARN remember %s: %v", key, err)
	}
}

func (s *Service) recalled(ctx context.Context, key string) bool {
	var until int64
	err := s.db.QueryRowContext(ctx, `SELECT reset_at FROM mcp_oauth_rate_limits WHERE bucket = ?`, key).Scan(&until)
	return err == nil && until > s.now().UnixMilli()
}

// flight is one client metadata document fetch that concurrent requests
// for the same client_id wait on instead of fetching again.
type flight struct {
	done   chan struct{}
	client *Client
	err    *Error
}

// Sweep deletes what can no longer be used: answered or expired requests,
// codes and tokens past their expiry (a used refresh token is kept until
// then, for reuse detection), finished rate-limit windows, registered
// clients nobody authorized within a week and cached metadata documents
// no grant uses. Grants are never deleted here: they name the client in
// the audit trail.
func (s *Service) Sweep(ctx context.Context) error {
	now := s.now()
	ms := now.UnixMilli()
	stmts := []struct {
		q    string
		args []any
	}{
		{`DELETE FROM mcp_oauth_requests WHERE expires_at < ?`, []any{ms - time.Hour.Milliseconds()}},
		{`DELETE FROM mcp_oauth_codes WHERE expires_at < ?`, []any{ms - time.Hour.Milliseconds()}},
		{`DELETE FROM mcp_oauth_tokens WHERE expires_at < ?`, []any{ms - 24*time.Hour.Milliseconds()}},
		{`DELETE FROM mcp_oauth_rate_limits WHERE reset_at < ?`, []any{ms}},
		{`DELETE FROM mcp_oauth_clients WHERE kind = 'dcr' AND created_at < ?
             AND NOT EXISTS (SELECT 1 FROM mcp_oauth_grants g WHERE g.client_id = mcp_oauth_clients.client_id)`,
			[]any{ms - 7*24*time.Hour.Milliseconds()}},
		{`DELETE FROM mcp_oauth_clients WHERE kind = 'cimd' AND expires_at < ?
             AND NOT EXISTS (SELECT 1 FROM mcp_oauth_grants g WHERE g.client_id = mcp_oauth_clients.client_id)`,
			[]any{ms - 24*time.Hour.Milliseconds()}},
	}
	for _, st := range stmts {
		if _, err := s.db.ExecContext(ctx, st.q, st.args...); err != nil {
			return err
		}
	}
	return nil
}

// Run sweeps every ten minutes until ctx ends. It is safe on every node at
// once: each statement deletes only rows that no node can use any more, so
// two nodes sweeping together delete the same rows and nothing else.
func (s *Service) Run(ctx context.Context) {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.Sweep(ctx); err != nil && ctx.Err() == nil {
				log.Printf("mcpauth: WARN sweep: %v", err)
			}
		}
	}
}
