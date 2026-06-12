package oauth

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"math"
	"time"
)

// RefreshLeadFraction is the fraction of access-token TTL after which we
// proactively refresh. 0.8 means refresh once 80% of the lifetime has
// elapsed (e.g. an hour-long token gets refreshed 12 minutes before
// expiry). RefreshMinLead is a minimum lead time so very short tokens
// still get refreshed before they die.
const (
	RefreshLeadFraction = 0.8
	RefreshMinLead      = 60 * time.Second
	RefresherTickEvery  = 30 * time.Second
)

// MaxRefreshFailures is how many consecutive refresh errors we tolerate
// before flipping the upstream to needs_reauth — the IdP almost certainly
// considers the refresh token dead at this point.
const MaxRefreshFailures = 4

// refresherBackoff returns the delay between retries on N consecutive
// failures. 1m, 5m, 15m, 1h, then capped.
func refresherBackoff(failures int) time.Duration {
	switch failures {
	case 0:
		return 0
	case 1:
		return 1 * time.Minute
	case 2:
		return 5 * time.Minute
	case 3:
		return 15 * time.Minute
	default:
		return time.Duration(math.Min(60, float64(15+15*(failures-3)))) * time.Minute
	}
}

// RunRefresher loops every RefresherTickEvery, scans tokens that are
// approaching expiry, and refreshes them. Failures are retried with
// exponential backoff; after MaxRefreshFailures the upstream is marked
// needs_reauth.
func (s *Service) RunRefresher(ctx context.Context) {
	t := time.NewTicker(RefresherTickEvery)
	defer t.Stop()
	// Initial pass right at start-up so anything stale gets refreshed
	// before the first incoming MCP call.
	s.refreshTick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.refreshTick(ctx)
			_, _ = s.PurgeExpiredPending(ctx)
		}
	}
}

func (s *Service) refreshTick(ctx context.Context) {
	// refresh_token_enc IS NOT NULL: long-lived tokens with no refresh
	// token (e.g. Linear) must be left alone — refreshing them can only
	// fail and would wrongly flip the upstream to needs_reauth. A real
	// 401 from the upstream still triggers reauth via MarkReauthExternal.
	rows, err := s.db.QueryContext(ctx, `
        SELECT upstream_name, COALESCE(access_expires_at,0), COALESCE(last_refresh_at,0),
               refresh_failures, state, is_pat
        FROM oauth_tokens
        WHERE state IN (?, ?) AND is_pat = 0 AND refresh_token_enc IS NOT NULL
    `, StateActive, StateRefreshing)
	if err != nil {
		log.Printf("oauth refresher: query: %v", err)
		return
	}
	type cand struct {
		upstream    string
		exp         time.Time
		lastRefresh time.Time
		failures    int
		state       string
		pat         bool
	}
	var cs []cand
	for rows.Next() {
		var c cand
		var exp, lr int64
		var pat int
		if err := rows.Scan(&c.upstream, &exp, &lr, &c.failures, &c.state, &pat); err != nil {
			rows.Close()
			log.Printf("oauth refresher: scan: %v", err)
			return
		}
		if exp > 0 {
			c.exp = time.UnixMilli(exp)
		}
		if lr > 0 {
			c.lastRefresh = time.UnixMilli(lr)
		}
		c.pat = pat == 1
		cs = append(cs, c)
	}
	if err := rows.Err(); err != nil {
		log.Printf("oauth refresher: rows: %v", err)
		return
	}
	rows.Close()

	now := time.Now()
	for _, c := range cs {
		if c.pat {
			continue
		}
		if c.exp.IsZero() {
			// IdP didn't tell us TTL — treat as 1h and refresh aggressively.
			if now.Sub(c.lastRefresh) < 30*time.Minute {
				continue
			}
		} else if !shouldRefresh(now, c.lastRefresh, c.exp) {
			continue
		}
		if d := refresherBackoff(c.failures); d > 0 && now.Sub(c.lastRefresh) < d {
			continue
		}
		if _, err := s.Refresh(ctx, c.upstream); err != nil {
			s.handleRefreshFailure(ctx, c.upstream, c.failures+1, err)
		}
	}
}

func shouldRefresh(now, lastRefresh, exp time.Time) bool {
	if exp.IsZero() {
		return true
	}
	if now.After(exp.Add(-RefreshMinLead)) {
		return true
	}
	if !lastRefresh.IsZero() {
		ttl := exp.Sub(lastRefresh)
		elapsed := now.Sub(lastRefresh)
		if float64(elapsed)/float64(ttl) >= RefreshLeadFraction {
			return true
		}
	} else {
		// We never refreshed yet; use total expected TTL from now.
		ttl := exp.Sub(now)
		if ttl <= RefreshMinLead {
			return true
		}
	}
	return false
}

// handleRefreshFailure increments the failure counter and decides whether
// to mark the upstream as needs_reauth.
func (s *Service) handleRefreshFailure(ctx context.Context, upstream string, newFailures int, err error) {
	if errors.Is(err, ErrNeedsReauth) {
		// Already marked by Refresh().
		return
	}
	if newFailures >= MaxRefreshFailures {
		s.markNeedsReauth(ctx, upstream, err.Error())
		return
	}
	_, dberr := s.db.ExecContext(ctx, `
        UPDATE oauth_tokens
        SET refresh_failures = ?, last_error = ?
        WHERE upstream_name = ?`, newFailures, err.Error(), upstream)
	if dberr != nil && !errors.Is(dberr, sql.ErrNoRows) {
		log.Printf("oauth refresher: persist failure for %s: %v", upstream, dberr)
	}
}
