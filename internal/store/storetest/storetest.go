// Package storetest gives tests a fresh, migrated toolyard database on the
// engine the test run selects, so the same tests run on SQLite and Postgres.
//
// TOOLYARD_TEST_DB=postgres selects Postgres: TOOLYARD_TEST_PG_DSN must then
// be a postgres:// URL of a role that may create databases (for example
// postgres://postgres:pw@localhost:5432/postgres?sslmode=disable). Each test
// gets its own database, cloned from a migrated template database with C
// collation, and dropped when the test ends. Anything else (the default)
// selects SQLite in t.TempDir().
package storetest

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver

	"github.com/tusharbhardwaj/toolyard/internal/store"
)

const (
	// EnvEngine selects the engine: "postgres", or anything else for SQLite.
	EnvEngine = "TOOLYARD_TEST_DB"
	// EnvPostgresDSN is the Postgres server URL used when EnvEngine is
	// "postgres".
	EnvPostgresDSN = "TOOLYARD_TEST_PG_DSN"

	// testMaxConns is each test database's pool size: small, since many
	// test databases can be open at once on one server.
	testMaxConns = 4
	// templateLock serializes template creation across the test processes
	// of one `go test ./...` run: "tytmpl01" in ASCII.
	templateLock int64 = 0x7479746d706c3031
)

// Engine returns the engine selected by TOOLYARD_TEST_DB.
func Engine() store.Dialect {
	if strings.EqualFold(strings.TrimSpace(os.Getenv(EnvEngine)), "postgres") {
		return store.Postgres
	}
	return store.SQLite
}

// SkipUnlessSQLite skips the test on Postgres runs.
func SkipUnlessSQLite(t testing.TB) {
	t.Helper()
	if Engine() != store.SQLite {
		t.Skip("SQLite-only test (TOOLYARD_TEST_DB=postgres)")
	}
}

// SkipUnlessPostgres skips the test on SQLite runs.
func SkipUnlessPostgres(t testing.TB) {
	t.Helper()
	if Engine() != store.Postgres {
		t.Skip("Postgres-only test (set TOOLYARD_TEST_DB=postgres and TOOLYARD_TEST_PG_DSN)")
	}
}

// Open returns a fresh, migrated database for one test and registers its
// cleanup. TOOLYARD_TEST_DB=postgres uses TOOLYARD_TEST_PG_DSN (a fresh
// database per test, created from a migrated template with C collation);
// anything else (default) uses SQLite in t.TempDir().
func Open(t testing.TB) *store.DB {
	t.Helper()
	if Engine() == store.Postgres {
		return openPostgres(t)
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "toolyard.db"))
	if err != nil {
		t.Fatalf("storetest: open SQLite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

var (
	setupOnce sync.Once
	setupErr  error
	adminDB   *sql.DB // the server's maintenance database (from the DSN)
	tplName   string
)

func openPostgres(t testing.TB) *store.DB {
	t.Helper()
	dbURL := NewPostgresDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db, err := store.OpenPostgres(ctx, dbURL, testMaxConns)
	if err != nil {
		t.Fatalf("storetest: open the test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// NewPostgresDatabase creates a fresh, migrated Postgres database for one
// test (cloned from the template) and returns its postgres:// URL, for tests
// that open the database themselves (read-only opens, commands that take a
// URL). The database is dropped when the test ends; close what you open on
// it before then. It fails the test unless TOOLYARD_TEST_DB=postgres.
func NewPostgresDatabase(t testing.TB) string {
	t.Helper()
	if Engine() != store.Postgres {
		t.Fatalf("storetest: NewPostgresDatabase needs %s=postgres", EnvEngine)
	}
	dsn := strings.TrimSpace(os.Getenv(EnvPostgresDSN))
	if dsn == "" {
		t.Fatalf("storetest: %s=postgres needs %s", EnvEngine, EnvPostgresDSN)
	}
	setupOnce.Do(func() { setupErr = setup(dsn) })
	if setupErr != nil {
		t.Fatalf("storetest: prepare the Postgres template database: %v", setupErr)
	}

	name := "t_" + randomHex(t, 8)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := execRetryInUse(ctx, adminDB, `CREATE DATABASE `+quoteIdent(name)+` TEMPLATE `+quoteIdent(tplName)); err != nil {
		t.Fatalf("storetest: create database %s: %v", name, err)
	}
	// Registered first, so it runs after the cleanups that close pools on
	// this database (cleanups run last-registered first); FORCE ends any
	// connection still left.
	t.Cleanup(func() {
		dctx, dcancel := context.WithTimeout(context.Background(), time.Minute)
		defer dcancel()
		if _, err := adminDB.ExecContext(dctx, `DROP DATABASE IF EXISTS `+quoteIdent(name)+` WITH (FORCE)`); err != nil {
			t.Logf("storetest: drop database %s: %v", name, err)
		}
	})
	dbURL, err := withDatabase(dsn, name)
	if err != nil {
		t.Fatalf("storetest: %v", err)
	}
	return dbURL
}

// NewEmptyPostgresDatabase creates an empty Postgres database for one test:
// no toolyard table and no migration, built from template0 with LC_COLLATE
// and LC_CTYPE set to locale (for example "C"). It returns its postgres://
// URL, and drops it when the test ends; close what you open on it before
// then. The error is CREATE DATABASE's, for example when the server does
// not know locale, so a test can skip. It fails the test unless
// TOOLYARD_TEST_DB=postgres.
func NewEmptyPostgresDatabase(t testing.TB, locale string) (string, error) {
	t.Helper()
	if Engine() != store.Postgres {
		t.Fatalf("storetest: NewEmptyPostgresDatabase needs %s=postgres", EnvEngine)
	}
	dsn := strings.TrimSpace(os.Getenv(EnvPostgresDSN))
	if dsn == "" {
		t.Fatalf("storetest: %s=postgres needs %s", EnvEngine, EnvPostgresDSN)
	}
	setupOnce.Do(func() { setupErr = setup(dsn) })
	if setupErr != nil {
		t.Fatalf("storetest: prepare the Postgres template database: %v", setupErr)
	}
	name := "t_" + randomHex(t, 8)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	lit := "'" + strings.ReplaceAll(locale, "'", "''") + "'"
	if err := execRetryInUse(ctx, adminDB, `CREATE DATABASE `+quoteIdent(name)+
		` TEMPLATE template0 ENCODING 'UTF8' LC_COLLATE `+lit+` LC_CTYPE `+lit); err != nil {
		return "", err
	}
	t.Cleanup(func() {
		dctx, dcancel := context.WithTimeout(context.Background(), time.Minute)
		defer dcancel()
		if _, err := adminDB.ExecContext(dctx, `DROP DATABASE IF EXISTS `+quoteIdent(name)+` WITH (FORCE)`); err != nil {
			t.Logf("storetest: drop database %s: %v", name, err)
		}
	})
	dbURL, err := withDatabase(dsn, name)
	if err != nil {
		t.Fatalf("storetest: %v", err)
	}
	return dbURL, nil
}

// setup connects to the maintenance database named in dsn and makes sure
// the template database for the current migrations exists. It runs once
// per test process; a pg_advisory_lock makes the test processes of one run
// take turns, so only the first builds the template.
func setup(dsn string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	// Plain pgx driver (no ? rewriting): this SQL uses $1 directly.
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		return err
	}
	admin.SetMaxOpenConns(4)
	if err := admin.PingContext(ctx); err != nil {
		_ = admin.Close()
		return err
	}
	name := "toolyard_tpl_" + store.MigrationsDigest()[:16]
	if err := ensureTemplate(ctx, admin, dsn, name); err != nil {
		_ = admin.Close()
		return err
	}
	adminDB, tplName = admin, name
	return nil
}

func ensureTemplate(ctx context.Context, admin *sql.DB, dsn, name string) error {
	conn, err := admin.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, templateLock); err != nil {
		return fmt.Errorf("take the template lock: %w", err)
	}
	defer func() {
		uctx, ucancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer ucancel()
		_, _ = conn.ExecContext(uctx, `SELECT pg_advisory_unlock($1)`, templateLock)
	}()

	var exists bool
	if err := conn.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname = $1)`, name).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	// Build under a temporary name and rename at the end, so a run that
	// dies half-way never leaves a broken template under the real name.
	build := name + "_build"
	if _, err := conn.ExecContext(ctx, `DROP DATABASE IF EXISTS `+quoteIdent(build)+` WITH (FORCE)`); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `CREATE DATABASE `+quoteIdent(build)+
		` TEMPLATE template0 ENCODING 'UTF8' LC_COLLATE 'C' LC_CTYPE 'C'`); err != nil {
		return fmt.Errorf("create %s: %w", build, err)
	}
	buildURL, err := withDatabase(dsn, build)
	if err != nil {
		return err
	}
	db, err := store.OpenPostgres(ctx, buildURL, 1)
	if err != nil {
		return fmt.Errorf("migrate %s: %w", build, err)
	}
	if err := db.Close(); err != nil {
		return fmt.Errorf("close %s: %w", build, err)
	}
	// A template must have no open connection when it is renamed or
	// cloned; the closed session can take a moment to end.
	return execRetryInUse(ctx, conn, `ALTER DATABASE `+quoteIdent(build)+` RENAME TO `+quoteIdent(name))
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// execRetryInUse runs query, retrying for a few seconds while Postgres
// reports the database in use (SQLSTATE 55006), which happens right after
// the last connection to a template closes.
func execRetryInUse(ctx context.Context, q execer, query string) error {
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, err := q.ExecContext(ctx, query)
		var pe *pgconn.PgError
		if err == nil || !errors.As(err, &pe) || pe.Code != "55006" || time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// withDatabase returns dsn (a postgres:// URL) with its database replaced
// by name.
func withDatabase(dsn, name string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		// The DSN may hold a password: do not quote it.
		return "", fmt.Errorf("%s must be a postgres:// URL", EnvPostgresDSN)
	}
	u.Path = "/" + name
	u.RawPath = ""
	return u.String(), nil
}

func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func randomHex(t testing.TB, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("storetest: random name: %v", err)
	}
	return hex.EncodeToString(b)
}
