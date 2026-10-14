package store

import "context"

// Test hooks for the external store_test package (which can import
// storetest; package store cannot without an import cycle).

// MigratePostgresAgain runs the Postgres migration step on an open database.
func MigratePostgresAgain(ctx context.Context, db *DB) error { return db.migratePostgres(ctx) }

// SQLiteMigrationNames lists migrations/sqlite in order.
func SQLiteMigrationNames() ([]string, error) { return migrationNames("migrations/sqlite") }
