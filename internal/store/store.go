// Package store wraps the SQLite database used by toolyard.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

//go:embed migrations/*.sql
var migrations embed.FS

type DB struct {
	*sql.DB
	Path string
}

// Open opens the SQLite database at path (creating it if missing) and applies
// any pending migrations.
func Open(path string) (*DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	dsn := "file:" + abs + "?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=on"
	sdb, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	sdb.SetMaxOpenConns(1) // SQLite + concurrent writes; serialize to avoid lock errors.
	if err := sdb.Ping(); err != nil {
		return nil, err
	}
	db := &DB{DB: sdb, Path: abs}
	if err := db.migrate(); err != nil {
		_ = sdb.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return db, nil
}

// OpenReadOnly opens the database at path for reading only, for checks
// that must change nothing (toolyard keys-check): no migrations and no
// writes (SQLite mode=ro and query_only). When nothing has the database
// open (no -shm file, and no -wal file or an empty one, as Close leaves
// it) the main file holds everything, so it is opened immutable as well:
// SQLite then takes no locks and creates no -wal or -shm file. While a
// server has it open, the sidecars exist and are read as they are. A
// missing database is an error wrapping fs.ErrNotExist and is never
// created. The caller closes the returned *sql.DB.
func OpenReadOnly(path string) (*sql.DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(abs); err != nil {
		return nil, err
	}
	dsn := "file:" + abs + "?mode=ro&_query_only=1&_busy_timeout=5000"
	if idleDatabase(abs) {
		dsn += "&immutable=1"
	}
	sdb, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	sdb.SetMaxOpenConns(1)
	if err := sdb.Ping(); err != nil {
		_ = sdb.Close()
		return nil, err
	}
	return sdb, nil
}

// idleDatabase reports that no connection has the WAL database at abs
// open: no -shm file, and the -wal file is absent or empty.
func idleDatabase(abs string) bool {
	if _, err := os.Stat(abs + "-shm"); !errors.Is(err, fs.ErrNotExist) {
		return false
	}
	fi, err := os.Stat(abs + "-wal")
	if errors.Is(err, fs.ErrNotExist) {
		return true
	}
	return err == nil && fi.Size() == 0
}

// DefaultCloseTimeout bounds Close. CloseContext takes the caller's bound.
const DefaultCloseTimeout = 10 * time.Second

// Close is CloseContext bounded by DefaultCloseTimeout.
func (db *DB) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), DefaultCloseTimeout)
	defer cancel()
	return db.CloseContext(ctx)
}

// CloseContext checkpoints the WAL (best-effort) and closes the underlying
// database, and returns by the time ctx ends even when a connection is
// still busy. The TRUNCATE checkpoint folds the -wal sidecar back into the
// main file so a file-copy backup of the .db is complete on its own. A
// checkpoint failure is non-fatal: we still close so callers don't leak the
// handle. With MaxOpenConns(1), a checkpoint without a context would wait
// forever for a connection held by a stuck query.
func (db *DB) CloseContext(ctx context.Context) error {
	if _, err := db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		// Best-effort; the WAL is replayed on next open regardless.
		_ = err
	}
	done := make(chan error, 1)
	go func() { done <- db.DB.Close() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("close database: %w", ctx.Err())
	}
}

func (db *DB) migrate() error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
        name TEXT PRIMARY KEY,
        applied_at INTEGER NOT NULL
    )`); err != nil {
		return err
	}
	entries, err := migrations.ReadDir("migrations")
	if err != nil {
		return err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		var seen string
		err := db.QueryRow(`SELECT name FROM schema_migrations WHERE name = ?`, name).Scan(&seen)
		if err == nil {
			continue
		}
		if err != sql.ErrNoRows {
			return err
		}
		body, err := migrations.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(string(body)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply %s: %w", name, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations(name, applied_at) VALUES(?, strftime('%s','now'))`, name); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}
