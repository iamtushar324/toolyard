// Package settings is a tiny key/value store backed by SQLite for
// system-wide toolyard configuration. Values are persisted as JSON so we can
// store booleans / strings / small structs uniformly. An in-memory cache is
// kept on top so the tool-filter hot path doesn't touch the DB on every
// tools/list.
package settings

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// Known keys.
const (
	// SurfaceMode controls which tools the agent sees in tools/list.
	// One of "full" | "top_n" | "router_only". Defaults to "full".
	SurfaceMode = "surface_mode"
	// TopNCount is how many additional tools (beyond the pinned set) to
	// surface to each agent when surface_mode is "top_n". Default 20.
	TopNCount = "top_n_count"
	// TopNPersonalizeAfter is the per-agent total-call threshold that
	// switches an agent from the cold-start "overall top-N" to its own
	// "personal top-N." Default 100.
	TopNPersonalizeAfter = "top_n_personalize_after"

	// RouterOnlyMode is kept as a virtual alias for back-compat: writes are
	// translated into surface_mode = "router_only" / "full" and reads
	// reflect surface_mode == "router_only".
	RouterOnlyMode = "router_only_mode"
)

// Surface modes.
const (
	SurfaceFull       = "full"
	SurfaceTopN       = "top_n"
	SurfaceRouterOnly = "router_only"
)

type Service struct {
	db *store.DB

	mu    sync.RWMutex
	cache map[string]json.RawMessage
}

func New(ctx context.Context, db *store.DB) (*Service, error) {
	s := &Service{db: db, cache: map[string]json.RawMessage{}}
	if err := s.reloadAll(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Service) reloadAll(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM system_settings`)
	if err != nil {
		return err
	}
	defer rows.Close()
	cache := map[string]json.RawMessage{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return err
		}
		cache[k] = json.RawMessage(v)
	}
	s.mu.Lock()
	s.cache = cache
	s.mu.Unlock()
	return rows.Err()
}

// All returns a snapshot of every setting (decoded to interface{}). Used by
// the dashboard's GET /v1/settings. The result also fills in defaults +
// the virtual router_only_mode alias derived from surface_mode.
func (s *Service) All(ctx context.Context) (map[string]any, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]any, len(s.cache))
	for k, v := range s.cache {
		var x any
		if err := json.Unmarshal(v, &x); err != nil {
			out[k] = string(v)
			continue
		}
		out[k] = x
	}
	// Defaults for the keys the UI expects to be present.
	if _, ok := out[SurfaceMode]; !ok {
		out[SurfaceMode] = SurfaceFull
	}
	if _, ok := out[TopNCount]; !ok {
		out[TopNCount] = float64(20)
	}
	if _, ok := out[TopNPersonalizeAfter]; !ok {
		out[TopNPersonalizeAfter] = float64(100)
	}
	// Virtual alias: keep router_only_mode boolean in sync for older callers.
	if mode, _ := out[SurfaceMode].(string); mode == SurfaceRouterOnly {
		out[RouterOnlyMode] = true
	} else {
		out[RouterOnlyMode] = false
	}
	return out, nil
}

// GetString reads a string setting from the in-memory cache, returning
// fallback if missing or wrong type.
func (s *Service) GetString(key, fallback string) string {
	s.mu.RLock()
	v, ok := s.cache[key]
	s.mu.RUnlock()
	if !ok {
		return fallback
	}
	var str string
	if err := json.Unmarshal(v, &str); err != nil {
		return fallback
	}
	return str
}

// GetInt reads an integer setting from the in-memory cache.
func (s *Service) GetInt(key string, fallback int) int {
	s.mu.RLock()
	v, ok := s.cache[key]
	s.mu.RUnlock()
	if !ok {
		return fallback
	}
	var f float64
	if err := json.Unmarshal(v, &f); err != nil {
		return fallback
	}
	return int(f)
}

// SurfaceModeOrDefault returns the surface_mode value, with a
// router_only_mode alias fallback for back-compat. When surface_mode is set,
// it always wins.
func (s *Service) SurfaceModeOrDefault() string {
	mode := s.GetString(SurfaceMode, "")
	if mode == SurfaceFull || mode == SurfaceTopN || mode == SurfaceRouterOnly {
		return mode
	}
	if s.GetBool(RouterOnlyMode) {
		return SurfaceRouterOnly
	}
	return SurfaceFull
}

// GetBool reads a boolean setting from the in-memory cache.
func (s *Service) GetBool(key string) bool {
	s.mu.RLock()
	v, ok := s.cache[key]
	s.mu.RUnlock()
	if !ok {
		return false
	}
	var b bool
	if err := json.Unmarshal(v, &b); err != nil {
		return false
	}
	return b
}

// Set persists value (must be JSON-serializable) and updates the cache.
func (s *Service) Set(ctx context.Context, key string, value any) error {
	if key == "" {
		return errors.New("key required")
	}
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO system_settings(key, value, updated_at) VALUES(?,?,?)
         ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
		key, string(body), time.Now().UnixMilli()); err != nil {
		return err
	}
	s.mu.Lock()
	s.cache[key] = body
	s.mu.Unlock()
	return nil
}

// Patch merges a map of {key: value} updates in one go, rolled back on error.
// router_only_mode is normalised into surface_mode at write time so older
// callers continue working.
func (s *Service) Patch(ctx context.Context, updates map[string]any) error {
	updates = normalise(updates)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	type row struct {
		k string
		b []byte
	}
	rows := make([]row, 0, len(updates))
	for k, v := range updates {
		body, err := json.Marshal(v)
		if err != nil {
			_ = tx.Rollback()
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO system_settings(key, value, updated_at) VALUES(?,?,?)
             ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
			k, string(body), now); err != nil {
			_ = tx.Rollback()
			return err
		}
		rows = append(rows, row{k, body})
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.mu.Lock()
	for _, r := range rows {
		s.cache[r.k] = r.b
	}
	s.mu.Unlock()
	return nil
}

// normalise rewrites virtual / aliased keys into their canonical form.
//   - router_only_mode: true  -> surface_mode = router_only
//   - router_only_mode: false -> surface_mode = full (only when surface_mode
//     isn't already in the same patch)
//   - top_n_count / top_n_personalize_after: clamped to sane ranges so a
//     bad input can't brick the UI.
func normalise(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	// router_only_mode aliasing.
	if _, sm := out[SurfaceMode]; !sm {
		if v, ok := out[RouterOnlyMode]; ok {
			if b, _ := v.(bool); b {
				out[SurfaceMode] = SurfaceRouterOnly
			} else {
				out[SurfaceMode] = SurfaceFull
			}
		}
	}
	delete(out, RouterOnlyMode)
	// Clamp surface_mode to a known value.
	if v, ok := out[SurfaceMode]; ok {
		s, _ := v.(string)
		switch s {
		case SurfaceFull, SurfaceTopN, SurfaceRouterOnly:
		default:
			out[SurfaceMode] = SurfaceFull
		}
	}
	// Clamp top_n_count to [1, 200].
	if v, ok := out[TopNCount]; ok {
		n := toInt(v)
		if n < 1 {
			n = 1
		}
		if n > 200 {
			n = 200
		}
		out[TopNCount] = n
	}
	// Clamp top_n_personalize_after to [0, 1_000_000].
	if v, ok := out[TopNPersonalizeAfter]; ok {
		n := toInt(v)
		if n < 0 {
			n = 0
		}
		if n > 1_000_000 {
			n = 1_000_000
		}
		out[TopNPersonalizeAfter] = n
	}
	return out
}

func toInt(v any) int {
	switch x := v.(type) {
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	case json.Number:
		i, _ := x.Int64()
		return int(i)
	}
	return 0
}

// Compile-time check that the package compiles even if sql.ErrNoRows isn't
// referenced anywhere.
var _ = sql.ErrNoRows
