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
