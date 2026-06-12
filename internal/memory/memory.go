// Package memory is toolyard's built-in shared memory MCP. v0.1: KV only.
//
// The store sits behind the gateway, so calls inherit policy + audit just like
// any upstream tool. Keys are scoped (default: "global"); v0.2 will add
// per-actor ACLs.
package memory

import (
	"context"
	"errors"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/store"
)

const defaultScope = "global"

type Service struct {
	db *store.DB
}

func New(db *store.DB) *Service { return &Service{db: db} }

type Entry struct {
	Scope     string `json:"scope"`
	Key       string `json:"key"`
	Value     string `json:"value"`
	UpdatedAt int64  `json:"updated_at"`
}

func (s *Service) Set(ctx context.Context, scope, key, value string) (*Entry, error) {
	if key == "" {
		return nil, errors.New("key required")
	}
	if scope == "" {
		scope = defaultScope
	}
	now := time.Now().UnixMilli()
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO memory_entries(scope, key, value, updated_at) VALUES(?,?,?,?)
         ON CONFLICT(scope, key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
		scope, key, value, now); err != nil {
		return nil, err
	}
	return &Entry{Scope: scope, Key: key, Value: value, UpdatedAt: now}, nil
}

// ExportAll returns every entry across all scopes, uncapped — for backup.
func (s *Service) ExportAll(ctx context.Context) ([]Entry, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT scope, key, value, updated_at FROM memory_entries ORDER BY scope, key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Entry{}
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.Scope, &e.Key, &e.Value, &e.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Import upserts the given entries in a single transaction. When replace is
// true, all existing entries are deleted first, so the result is exactly the
// imported set. Returns the number of entries written.
func (s *Service) Import(ctx context.Context, entries []Entry, replace bool) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if replace {
		if _, err := tx.ExecContext(ctx, `DELETE FROM memory_entries`); err != nil {
			return 0, err
		}
	}
	n := 0
	for _, e := range entries {
		if e.Key == "" {
			continue
		}
		scope := e.Scope
		if scope == "" {
			scope = defaultScope
		}
		updated := e.UpdatedAt
		if updated == 0 {
			updated = time.Now().UnixMilli()
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO memory_entries(scope, key, value, updated_at) VALUES(?,?,?,?)
             ON CONFLICT(scope, key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
			scope, e.Key, e.Value, updated); err != nil {
			return 0, err
		}
		n++
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}

func (s *Service) Get(ctx context.Context, scope, key string) (*Entry, error) {
	if scope == "" {
		scope = defaultScope
	}
	var e Entry
	err := s.db.QueryRowContext(ctx,
		`SELECT scope, key, value, updated_at FROM memory_entries WHERE scope = ? AND key = ?`,
		scope, key).Scan(&e.Scope, &e.Key, &e.Value, &e.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &e, nil
}

func (s *Service) Delete(ctx context.Context, scope, key string) error {
	if scope == "" {
		scope = defaultScope
	}
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM memory_entries WHERE scope = ? AND key = ?`, scope, key)
	return err
}

func (s *Service) List(ctx context.Context, scope, prefix string) ([]Entry, error) {
	if scope == "" {
		scope = defaultScope
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT scope, key, value, updated_at FROM memory_entries
         WHERE scope = ? AND key LIKE ? ORDER BY key ASC LIMIT 500`,
		scope, prefix+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.Scope, &e.Key, &e.Value, &e.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
