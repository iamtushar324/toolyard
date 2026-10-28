package mcpauth

// sweep.go holds the rate limiter and the sweeper. Both keep their state in
// the database, so every replica sees the same counts and any replica may
// sweep.

import (
	"context"
	"log"
	"sort"
	"sync"
	"time"
)

// memLimiter is the in-process pre-limit in front of the database counter:
// once this node alone has seen more than max hits in a window, it refuses
// without writing a row, so a flood costs no database writes. It never
// allows what the shared counter would refuse: it only refuses earlier.
type memLimiter struct {
	mu sync.Mutex
	m  map[string]*memBucket
}

type memBucket struct {
	hits   int
	reset  time.Time
	last   time.Time // last use, for evicting per-address buckets
	global bool      // never evicted
}

// maxMemBuckets bounds the pre-limit's memory. Past it, finished windows
// are dropped first, then the least recently used per-address buckets (a
// tenth at a time); global buckets are never evicted, so a flood of fresh
// addresses cannot reset a global count.
const maxMemBuckets = 50000

// admit checks every bucket of one request without counting, and counts
// them all only when every one has room, so a request refused by one
// bucket (an address over its limit) uses up none of the others (a global
// bucket) and creates no bucket. It reports whether all had room.
func (l *memLimiter) admit(now time.Time, limits []limit) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.m == nil {
		l.m = map[string]*memBucket{}
	}
	for _, lim := range limits {
		hits := 0
		if b, ok := l.m[lim.bucket]; ok && now.Before(b.reset) {
			hits = b.hits
		}
		if hits >= lim.max {
			return false
		}
	}
	for _, lim := range limits {
		b, ok := l.m[lim.bucket]
		if !ok {
			l.makeRoom(now)
			b = &memBucket{global: lim.global}
			l.m[lim.bucket] = b
		}
		if !now.Before(b.reset) {
			b.hits, b.reset = 0, now.Add(lim.window)
		}
		b.hits++
		b.last = now
	}
	return true
}

// makeRoom keeps the map under maxMemBuckets before a new bucket goes in.
func (l *memLimiter) makeRoom(now time.Time) {
	if len(l.m) < maxMemBuckets {
		return
	}
	for k, b := range l.m {
		if !now.Before(b.reset) && !b.global {
			delete(l.m, k)
		}
	}
	if len(l.m) < maxMemBuckets {
		return
	}
	type aged struct {
		key  string
		last time.Time
	}
	var perAddress []aged
	for k, b := range l.m {
		if !b.global {
			perAddress = append(perAddress, aged{k, b.last})
		}
	}
	sort.Slice(perAddress, func(i, j int) bool { return perAddress[i].last.Before(perAddress[j].last) })
	for i := 0; i < len(perAddress) && i < maxMemBuckets/10; i++ {
		delete(l.m, perAddress[i].key)
	}
}

// limit is one fixed-window bucket. A global bucket counts every caller.
type limit struct {
	bucket string
	max    int
	window time.Duration
	global bool
}

// allowAll counts one hit on every bucket and reports whether all are
// still within their limit. Callers list per-address buckets first and
// global buckets last. The in-process pre-limit first checks every bucket
// without counting and counts them only if all have room, so a refused
// request writes nothing to the database and uses up nothing. Then the
// shared counters are written in order and the first one over its limit
// refuses, before the buckets after it (the global ones) are touched: one
// address over its limit never uses up a global budget. A database error
// refuses (fails closed).
func (s *Service) allowAll(ctx context.Context, limits ...limit) bool {
	if !s.mem.admit(s.now(), limits) {
		return false
	}
	for _, l := range limits {
		if !s.dbAllow(ctx, l) {
			return false
		}
	}
	return true
}

// allow is allowAll for one bucket.
func (s *Service) allow(ctx context.Context, bucket string, max int, window time.Duration) bool {
	return s.allowAll(ctx, limit{bucket: bucket, max: max, window: window})
}

// dbAllow counts one hit on the shared counter.
func (s *Service) dbAllow(ctx context.Context, l limit) bool {
	now := s.now().UnixMilli()
	var hits int64
	err := s.db.QueryRowContext(ctx,
		`INSERT INTO mcp_oauth_rate_limits(bucket, hits, reset_at) VALUES(?, 1, ?)
         ON CONFLICT(bucket) DO UPDATE SET
             hits = CASE WHEN mcp_oauth_rate_limits.reset_at <= CAST(? AS BIGINT) THEN 1 ELSE mcp_oauth_rate_limits.hits + 1 END,
             reset_at = CASE WHEN mcp_oauth_rate_limits.reset_at <= CAST(? AS BIGINT) THEN excluded.reset_at ELSE mcp_oauth_rate_limits.reset_at END
         RETURNING hits`,
		l.bucket, now+l.window.Milliseconds(), now, now).Scan(&hits)
	if err != nil {
		log.Printf("mcpauth: WARN rate limit %s: %v", l.bucket, err)
		return false
	}
	return hits <= int64(l.max)
}

// ipLimits are the per-address and per-network buckets of route for ip:
// the address (IPv6 by /64) with max, and its network (IPv4 /24, IPv6
// /48) with networkFactor times max, so one network cannot spread a flood
// over many addresses. A missing address shares one strict bucket.
func ipLimits(route, ip string, max int, window time.Duration) []limit {
	key := rateKey(ip)
	out := []limit{{bucket: route + ":" + key, max: limitFor(key, max), window: window}}
	if net := netKey(ip); net != "" {
		out = append(out, limit{bucket: route + ":net:" + net, max: max * networkFactor, window: window})
	}
	return out
}

// networkFactor: a network may make this many times one address's hits.
const networkFactor = 4

// AllowIP is the per-address and per-network limit for the HTTP layer
// (the consent page).
func (s *Service) AllowIP(ctx context.Context, route, ip string, max int, window time.Duration) bool {
	return s.allowAll(ctx, ipLimits(route, ip, max, window)...)
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
// return links, codes and tokens past their expiry (a used refresh token
// is kept until then, for reuse detection), finished rate-limit windows,
// registered clients nobody authorized within a day and cached metadata
// documents no grant uses. Grants are never deleted here: they name the
// client in the audit trail.
func (s *Service) Sweep(ctx context.Context) error {
	now := s.now()
	ms := now.UnixMilli()
	stmts := []struct {
		q    string
		args []any
	}{
		{`DELETE FROM mcp_oauth_requests WHERE expires_at < ?`, []any{ms - time.Hour.Milliseconds()}},
		{`DELETE FROM mcp_oauth_return_tickets WHERE expires_at < ?`, []any{ms - time.Hour.Milliseconds()}},
		{`DELETE FROM mcp_oauth_codes WHERE expires_at < ?`, []any{ms - time.Hour.Milliseconds()}},
		{`DELETE FROM mcp_oauth_tokens WHERE expires_at < ?`, []any{ms - 24*time.Hour.Milliseconds()}},
		{`DELETE FROM mcp_oauth_rate_limits WHERE reset_at < ?`, []any{ms}},
		{`DELETE FROM mcp_oauth_clients WHERE kind = 'dcr' AND created_at < ?
             AND NOT EXISTS (SELECT 1 FROM mcp_oauth_grants g WHERE g.client_id = mcp_oauth_clients.client_id)`,
			[]any{ms - unusedClientTTL.Milliseconds()}},
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
