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
	// asyncLoading is non-nil while one reload runs; asyncGen counts local
	// writes, so a reload that read before a write does not undo it.
	asyncLoading chan struct{}
	asyncGen     uint64
}

// AsyncSetting is the named server's async setting (zero when it has
// none). It implements gateway.AsyncPolicy and is read on every routed
// call, from a copy reloaded at most every asyncCacheTTL. The reload runs
// outside the lock, one at a time, and the stale copy answers meanwhile;
// only the very first read waits for it.
func (s *Service) AsyncSetting(name string) jobs.AsyncSetting {
	s.asyncMu.Lock()
	stale := s.asyncByName == nil || time.Since(s.asyncLoaded) > asyncCacheTTL
	if stale && s.asyncLoading == nil {
		ch := make(chan struct{})
		s.asyncLoading = ch
		gen := s.asyncGen
		if s.asyncByName != nil {
			s.asyncMu.Unlock()
			go s.reloadAsync(ch, gen)
			s.asyncMu.Lock()
		} else {
			s.asyncMu.Unlock()
			s.reloadAsync(ch, gen)
			s.asyncMu.Lock()
		}
	}
	if s.asyncByName == nil && s.asyncLoading != nil {
		ch := s.asyncLoading
		s.asyncMu.Unlock()
		select {
		case <-ch:
		case <-time.After(3 * time.Second):
		}
		s.asyncMu.Lock()
	}
	set := s.asyncByName[name]
	s.asyncMu.Unlock()
	return set
}

// reloadAsync reads the settings and installs them unless a local write
// happened meanwhile (then the next read reloads).
func (s *Service) reloadAsync(done chan struct{}, gen uint64) {
	defer close(done)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	m, err := s.loadAsync(ctx)
	cancel()
	s.asyncMu.Lock()
	defer s.asyncMu.Unlock()
	s.asyncLoading = nil
	switch {
	case err == nil && gen == s.asyncGen:
		s.asyncByName, s.asyncLoaded = m, time.Now()
	case s.asyncByName == nil:
		// Keep answering (nothing async) and try again after the TTL.
		s.asyncByName, s.asyncLoaded = map[string]jobs.AsyncSetting{}, time.Now()
	case err != nil:
		s.asyncLoaded = time.Now()
	}
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
	s.asyncGen++
	if s.asyncByName != nil {
		// Copy on write: readers hold the old map without the lock.
		next := make(map[string]jobs.AsyncSetting, len(s.asyncByName)+1)
		for k, v := range s.asyncByName {
			next[k] = v
		}
		if out != nil {
			next[name] = *out
		} else {
			delete(next, name)
		}
		s.asyncByName = next
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
