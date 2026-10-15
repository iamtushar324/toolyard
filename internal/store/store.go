// Package store wraps the database used by toolyard: SQLite by default
// (Open), or Postgres (OpenPostgres). Both run the same SQL with ?
// placeholders: on Postgres the driver rewrites them (Rebind).
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
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// migrations holds migrations/sqlite/*.sql (every SQLite migration) and
// migrations/postgres/*.sql (the Postgres baseline, then the Postgres twin
// of every later SQLite migration, under the same file name).
//
//go:embed migrations/sqlite/*.sql migrations/postgres/*.sql
var migrations embed.FS

// DB is an open toolyard database.
type DB struct {
	*sql.DB
	// Path is the SQLite file path; "" on Postgres.
	Path string
	// Dialect is the engine: SQLite or Postgres.
	Dialect Dialect
}

// Open opens the SQLite database at path (creating it if missing) and applies
// any pending migrations.
func Open(path string) (*DB, error) { return openSQLite(path, true) }

// OpenNoMigrate opens the existing SQLite database at path read-write, as
// Open does, but applies no migration: the database is used as it is
// (toolyard serve -migrate=off; MigrationStatus then tells whether its
// schema is at this binary's level, and Migrate applies what is pending).
// A missing database is an error wrapping fs.ErrNotExist and is never
// created.
func OpenNoMigrate(path string) (*DB, error) { return openSQLite(path, false) }

func openSQLite(path string, migrate bool) (*DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	dsn := "file:" + abs + "?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=on"
	if !migrate {
		if _, err := os.Stat(abs); err != nil {
			return nil, err
		}
		// mode=rw: read-write, but never create the file.
		dsn += "&mode=rw"
	}
	sdb, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	sdb.SetMaxOpenConns(1) // SQLite + concurrent writes; serialize to avoid lock errors.
	if err := sdb.Ping(); err != nil {
		_ = sdb.Close()
		return nil, err
	}
	db := &DB{DB: sdb, Path: abs, Dialect: SQLite}
	if !migrate {
		return db, nil
	}
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

// CloseContext checkpoints the WAL on SQLite (best-effort) and closes the
// underlying database, and returns by the time ctx ends even when a
// connection is still busy. The TRUNCATE checkpoint folds the -wal sidecar
// back into the main file so a file-copy backup of the .db is complete on
// its own. A checkpoint failure is non-fatal: we still close so callers
// don't leak the handle. With MaxOpenConns(1), a checkpoint without a
// context would wait forever for a connection held by a stuck query.
// Postgres has no checkpoint to run here.
func (db *DB) CloseContext(ctx context.Context) error {
	if !db.Dialect.IsPostgres() {
		if _, err := db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
			// Best-effort; the WAL is replayed on next open regardless.
			_ = err
		}
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

// migrate applies the SQLite migrations (migrations/sqlite) that
// schema_migrations does not record yet, in file-name order, each in its own
// transaction.
func (db *DB) migrate() error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
        name TEXT PRIMARY KEY,
        applied_at INTEGER NOT NULL
    )`); err != nil {
		return err
	}
	names, err := migrationNames("migrations/sqlite")
	if err != nil {
		return err
	}
	for _, name := range names {
		var seen string
		err := db.QueryRow(`SELECT name FROM schema_migrations WHERE name = ?`, name).Scan(&seen)
		if err == nil {
			continue
		}
		if err != sql.ErrNoRows {
			return err
		}
		body, err := migrations.ReadFile("migrations/sqlite/" + name)
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
		if _, err := tx.Exec(`INSERT INTO schema_migrations(name, applied_at) VALUES(?, ?)`, name, time.Now().Unix()); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}
