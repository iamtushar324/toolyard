package store_test

import (
	"context"
	"errors"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/store"
	"github.com/tusharbhardwaj/toolyard/internal/store/storetest"
)

// A fully migrated database has nothing pending and nothing unknown.
func TestMigrationStatusOfAMigratedDatabase(t *testing.T) {
	db := storetest.Open(t)
	pending, unknown, err := store.MigrationStatus(context.Background(), db)
	if err != nil || len(pending) != 0 || len(unknown) != 0 {
		t.Fatalf("status = pending %v, unknown %v, %v; want nothing", pending, unknown, err)
	}
	known, err := store.KnownMigrations(db.Dialect)
	if err != nil || len(known) == 0 {
		t.Fatalf("known migrations: %v %v", known, err)
	}
}

// A migration the database does not record is pending; one it records that
// this binary does not know is unknown. Neither changes the database.
func TestMigrationStatusSeesMissingAndUnknownMigrations(t *testing.T) {
	ctx := context.Background()
	db := storetest.Open(t)
	known, err := store.KnownMigrations(db.Dialect)
	if err != nil {
		t.Fatal(err)
	}
	last := known[len(known)-1]
	if _, err := db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE name = ?`, last); err != nil {
		t.Fatal(err)
	}
	future := "9999_from_a_newer_toolyard.sql"
	if _, err := db.ExecContext(ctx, `INSERT INTO schema_migrations(name, applied_at) VALUES(?, ?)`, future, 1); err != nil {
		t.Fatal(err)
	}
	pending, unknown, err := store.MigrationStatus(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(pending, ",") != last || strings.Join(unknown, ",") != future {
		t.Fatalf("status = pending %v, unknown %v; want [%s], [%s]", pending, unknown, last, future)
	}
}

// An empty database (no schema_migrations) has every migration pending, and
// reading its status creates nothing; Migrate then applies them all. On
// Postgres the status is read on a read-only session first.
func TestMigrateAnEmptyDatabase(t *testing.T) {
	ctx := context.Background()
	var db *store.DB
	if storetest.Engine() == store.Postgres {
		dbURL, err := storetest.NewEmptyPostgresDatabase(t, "C")
		if err != nil {
			t.Fatalf("create an empty database: %v", err)
		}
		ro, err := store.OpenPostgresWith(ctx, dbURL, store.PostgresOptions{MaxConns: 1, ReadOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		pending, _, err := store.MigrationStatus(ctx, ro)
		_ = ro.Close()
		known, kerr := store.KnownMigrations(store.Postgres)
		if err != nil || kerr != nil || len(pending) != len(known) {
			t.Fatalf("read-only status of an empty database: %d pending of %d (%v, %v)", len(pending), len(known), err, kerr)
		}
		if db, err = store.OpenPostgresWith(ctx, dbURL, store.PostgresOptions{MaxConns: 2, NoMigrate: true}); err != nil {
			t.Fatal(err)
		}
	} else {
		path := filepath.Join(t.TempDir(), "toolyard.db")
		// An empty file is an empty SQLite database.
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		var err error
		if db, err = store.OpenNoMigrate(path); err != nil {
			t.Fatal(err)
		}
	}
	defer db.Close()

	known, err := store.KnownMigrations(db.Dialect)
	if err != nil {
		t.Fatal(err)
	}
	pending, unknown, err := store.MigrationStatus(ctx, db)
	if err != nil || len(pending) != len(known) || len(unknown) != 0 {
		t.Fatalf("empty database: %d pending of %d, unknown %v (%v)", len(pending), len(known), unknown, err)
	}
	if has, err := store.TableExists(ctx, db, db.Dialect, "schema_migrations"); err != nil || has {
		t.Fatalf("MigrationStatus created schema_migrations (%v)", err)
	}

	applied, err := db.Migrate(ctx)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if strings.Join(applied, ",") != strings.Join(known, ",") {
		t.Fatalf("Migrate reported %d applied, want all %d: %v", len(applied), len(known), applied)
	}
	pending, unknown, err = store.MigrationStatus(ctx, db)
	if err != nil || len(pending) != 0 || len(unknown) != 0 {
		t.Fatalf("after Migrate: pending %v, unknown %v (%v)", pending, unknown, err)
	}
	// Migrating again changes nothing and reports nothing.
	applied, err = db.Migrate(ctx)
	if err != nil || len(applied) != 0 {
		t.Fatalf("migrate again: applied %v (%v)", applied, err)
	}
}

// OpenNoMigrate never creates a database and never migrates one.
func TestOpenNoMigrate(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	missing := filepath.Join(dir, "toolyard.db")
	if _, err := store.OpenNoMigrate(missing); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("open a missing database: %v, want fs.ErrNotExist", err)
	}
	if _, err := os.Stat(missing); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("OpenNoMigrate created %s", missing)
	}

	// A migrated database opens read-write as it is.
	db, err := store.Open(missing)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = store.OpenNoMigrate(missing)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if db.Dialect != store.SQLite || db.Path == "" {
		t.Fatalf("OpenNoMigrate: dialect %v, path %q", db.Dialect, db.Path)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE written (v TEXT)`); err != nil {
		t.Fatalf("OpenNoMigrate is not read-write: %v", err)
	}
	pending, _, err := store.MigrationStatus(ctx, db)
	if err != nil || len(pending) != 0 {
		t.Fatalf("status: %v %v", pending, err)
	}
}

// A server that cannot be reached is store.ErrConnect (worth a retry); a
// URL that cannot be parsed is ErrConfig and never ErrConnect.
func TestOpenPostgresConnectErrors(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := store.OpenPostgresWith(ctx, "postgres://toolyard:s3cret-pw@127.0.0.1:1/toolyard?sslmode=disable&connect_timeout=5",
		store.PostgresOptions{MaxConns: 1, NoMigrate: true})
	if !errors.Is(err, store.ErrConnect) || errors.Is(err, store.ErrConfig) {
		t.Fatalf("unreachable server: %v, want ErrConnect", err)
	}
	if strings.Contains(err.Error(), "s3cret-pw") {
		t.Fatalf("the error shows the password: %v", err)
	}
	_, err = store.OpenPostgresWith(ctx, "postgres://toolyard:s3cret-pw@127.0.0.1:99999/toolyard", store.PostgresOptions{MaxConns: 1, NoMigrate: true})
	if errors.Is(err, store.ErrConnect) || !errors.Is(err, store.ErrConfig) {
		t.Fatalf("bad port: %v, want ErrConfig and not ErrConnect", err)
	}
}

// A database that does not exist is no ErrConnect: a retry cannot create it.
func TestOpenPostgresMissingDatabaseIsNotRetryable(t *testing.T) {
	storetest.SkipUnlessPostgres(t)
	u, err := url.Parse(storetest.NewPostgresDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/toolyard_no_such_database"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err = store.OpenPostgresWith(ctx, u.String(), store.PostgresOptions{MaxConns: 1, NoMigrate: true})
	if err == nil || errors.Is(err, store.ErrConnect) {
		t.Fatalf("missing database: %v, want an error that is not ErrConnect", err)
	}
}
