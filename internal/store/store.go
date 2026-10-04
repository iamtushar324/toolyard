// Package store wraps the SQLite database used by toolyard.
package store

import (
	"database/sql"
	"embed"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

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

// Close checkpoints the WAL (best-effort) and closes the underlying database.
// The TRUNCATE checkpoint folds the -wal sidecar back into the main file so a
// file-copy backup of the .db is complete on its own. A checkpoint failure is
// non-fatal: we still close so callers don't leak the handle.
func (db *DB) Close() error {
	if _, err := db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		// Best-effort; the WAL is replayed on next open regardless.
		_ = err
	}
	return db.DB.Close()
}

// legacyMigrationNames maps migration files that were renumbered to their
// current names. Migrations 0009 and 0013 were each used twice, so
// everything from 0009_mempalace on moved up to give every file a unique
// number (order unchanged). Databases that applied the old names get their
// schema_migrations rows rewritten so nothing is re-applied.
var legacyMigrationNames = map[string]string{
	"0009_mempalace.sql":              "0010_mempalace.sql",
	"0010_approval_results.sql":       "0011_approval_results.sql",
	"0011_notes_sync.sql":             "0012_notes_sync.sql",
	"0012_skills_sync.sql":            "0013_skills_sync.sql",
	"0013_oauth_authorize_params.sql": "0014_oauth_authorize_params.sql",
	"0013_tool_policies.sql":          "0015_tool_policies.sql",
	"0014_agent_revocation.sql":       "0016_agent_revocation.sql",
	"0015_hook_events.sql":            "0017_hook_events.sql",
	"0016_secrets.sql":                "0018_secrets.sql",
	"0017_chat_messages.sql":          "0019_chat_messages.sql",
	"0018_events.sql":                 "0020_events.sql",
	"0019_memory_webhooks.sql":        "0021_memory_webhooks.sql",
	"0020_memory_webhook_jobs.sql":    "0022_memory_webhook_jobs.sql",
}

// renameLegacyMigrations rewrites old migration names in schema_migrations to
// their renumbered names in one transaction. Old and new names never collide
// (each keeps its slug), so the updates are order-independent.
func (db *DB) renameLegacyMigrations() error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	for oldName, newName := range legacyMigrationNames {
		if _, err := tx.Exec(`UPDATE OR IGNORE schema_migrations SET name = ? WHERE name = ?`, newName, oldName); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("rename migration %s: %w", oldName, err)
		}
		if _, err := tx.Exec(`DELETE FROM schema_migrations WHERE name = ?`, oldName); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("rename migration %s: %w", oldName, err)
		}
	}
	return tx.Commit()
}

func (db *DB) migrate() error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
        name TEXT PRIMARY KEY,
        applied_at INTEGER NOT NULL
    )`); err != nil {
		return err
	}
	if err := db.renameLegacyMigrations(); err != nil {
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
