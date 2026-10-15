package store

import (
	"context"
	"fmt"
	"sort"
)

// KnownMigrations returns, sorted, every migration name a database of engine
// d records once this binary has fully migrated it: every file of
// migrations/sqlite on SQLite, PostgresMigrationNames on Postgres.
func KnownMigrations(d Dialect) ([]string, error) {
	if d.IsPostgres() {
		return PostgresMigrationNames()
	}
	return migrationNames("migrations/sqlite")
}

// MigrationStatus compares the migrations db records in schema_migrations
// with the ones this binary knows (KnownMigrations). It only reads: it
// creates nothing, not even schema_migrations, so it also works on a
// read-only session. pending lists, sorted, what Migrate would record;
// unknown lists, sorted, what the database records that this binary does
// not know (a newer toolyard migrated it). A database without
// schema_migrations has every migration pending.
func MigrationStatus(ctx context.Context, db *DB) (pending, unknown []string, err error) {
	known, err := KnownMigrations(db.Dialect)
	if err != nil {
		return nil, nil, err
	}
	has, err := TableExists(ctx, db, db.Dialect, "schema_migrations")
	if err != nil {
		return nil, nil, fmt.Errorf("store: check for schema_migrations: %w", err)
	}
	applied := map[string]bool{}
	if has {
		if applied, err = appliedMigrations(ctx, db); err != nil {
			return nil, nil, fmt.Errorf("store: read schema_migrations: %w", err)
		}
	}
	knownSet := make(map[string]bool, len(known))
	for _, name := range known {
		knownSet[name] = true
		if !applied[name] {
			pending = append(pending, name)
		}
	}
	for name := range applied {
		if !knownSet[name] {
			unknown = append(unknown, name)
		}
	}
	sort.Strings(unknown)
	return pending, unknown, nil
}

// Migrate applies the pending migrations of db's engine, as Open and
// OpenPostgres do (on Postgres on one connection that holds the migration
// advisory lock), for a database opened without them (OpenNoMigrate,
// PostgresOptions.NoMigrate). It returns, sorted, the names this call
// recorded. On Postgres they are read and written under the lock, so a call
// that waited while another process migrated returns only its own work;
// after an error they are what was committed before it.
func (db *DB) Migrate(ctx context.Context) ([]string, error) {
	var applied []string
	var err error
	if db.Dialect.IsPostgres() {
		applied, err = db.migratePostgresNames(ctx)
	} else {
		applied, err = db.migrateNames()
	}
	if err != nil {
		return applied, fmt.Errorf("migrate: %w", err)
	}
	return applied, nil
}
