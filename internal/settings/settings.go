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
	// RouterOnlyMode hides every tool except tools.search and tools.execute
	// from agents' tools/list, so an agent's prompt isn't bloated by the
	// schemas of every tool you have connected. Underlying tools remain
	// callable through tools.execute.
	RouterOnlyMode = "router_only_mode"
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
// the dashboard's GET /v1/settings.
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
	// Ensure the well-known keys are always present so the UI can render a
	// stable shape.
	if _, ok := out[RouterOnlyMode]; !ok {
		out[RouterOnlyMode] = false
	}
	return out, nil
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
func (s *Service) Patch(ctx context.Context, updates map[string]any) error {
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

// Compile-time check that the package compiles even if sql.ErrNoRows isn't
// referenced anywhere.
var _ = sql.ErrNoRows
