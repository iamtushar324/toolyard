package upstreams

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/jobs"
)

// Async tools: an administrator marks a server, or single tools of it,
// async (docs/guidelines/async-tools.md). The setting lives in
// upstream_servers.async_json and changing it never reconnects the
// server: it only decides whether the gateway hands an agent a job id
// instead of waiting for the result.

// asyncCacheTTL bounds how stale the gateway's copy of the settings can
// be. A change made on this process applies at once; one made on another
// replica within this time (cluster.Subscribe can make it immediate once
// slice A lands).
const asyncCacheTTL = 5 * time.Second

// asyncCache is the gateway's copy of every server's async setting,
// embedded in Service.
type asyncCache struct {
	asyncMu     sync.Mutex
	asyncLoaded time.Time
	asyncByName map[string]jobs.AsyncSetting
}

// AsyncSetting is the named server's async setting (zero when it has
// none). It implements gateway.AsyncPolicy and is read on every routed
// call, from a copy reloaded at most every asyncCacheTTL.
func (s *Service) AsyncSetting(name string) jobs.AsyncSetting {
	s.asyncMu.Lock()
	defer s.asyncMu.Unlock()
	if s.asyncByName == nil || time.Since(s.asyncLoaded) > asyncCacheTTL {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		m, err := s.loadAsync(ctx)
		cancel()
		switch {
		case err == nil:
			s.asyncByName = m
		case s.asyncByName == nil:
			// Keep answering (nothing async) and try again after the TTL.
			s.asyncByName = map[string]jobs.AsyncSetting{}
		}
		s.asyncLoaded = time.Now()
	}
	return s.asyncByName[name]
}

// loadAsync reads every stored async setting.
func (s *Service) loadAsync(ctx context.Context) (map[string]jobs.AsyncSetting, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, async_json FROM upstream_servers WHERE async_json IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]jobs.AsyncSetting{}
	for rows.Next() {
		var name, raw string
		if err := rows.Scan(&name, &raw); err != nil {
			return nil, err
		}
		if set, ok := decodeAsync(raw); ok {
			out[name] = set
		}
	}
	return out, rows.Err()
}

// decodeAsync reads one async_json value; a bad or empty one is no setting.
func decodeAsync(raw string) (jobs.AsyncSetting, bool) {
	var set jobs.AsyncSetting
	if raw == "" || raw == "null" || json.Unmarshal([]byte(raw), &set) != nil || set.Validate() != nil || set.IsZero() {
		return jobs.AsyncSetting{}, false
	}
	return set.Normalized(), true
}

// SetAsync replaces the named server's async setting. A setting that makes
// nothing async and names no tool clears it. The server stays connected.
// It returns the stored setting (nil when cleared).
func (s *Service) SetAsync(ctx context.Context, name string, set jobs.AsyncSetting) (*jobs.AsyncSetting, error) {
	if err := set.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	set = set.Normalized()
	var stored any
	var out *jobs.AsyncSetting
	if !set.IsZero() {
		raw, err := json.Marshal(set)
		if err != nil {
			return nil, err
		}
		stored = string(raw)
		out = &set
	}
	res, err := s.db.ExecContext(ctx, `UPDATE upstream_servers SET async_json = ?, updated_at = ? WHERE name = ?`,
		stored, time.Now().UnixMilli(), name)
	if err != nil {
		return nil, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return nil, err
	} else if n == 0 {
		return nil, ErrNotFound
	}
	s.asyncMu.Lock()
	if s.asyncByName != nil {
		if out != nil {
			s.asyncByName[name] = *out
		} else {
			delete(s.asyncByName, name)
		}
	}
	s.asyncMu.Unlock()
	// tools/list marks async tools for MCP Tasks clients.
	if s.gw != nil {
		s.gw.NotifyToolListChanged()
	}
	return out, nil
}

// attachAsync fills Async on servers for the API.
func (s *Service) attachAsync(servers []Server) {
	for i := range servers {
		if set := s.AsyncSetting(servers[i].Name); !set.IsZero() {
			cp := set
			servers[i].Async = &cp
		}
	}
}
