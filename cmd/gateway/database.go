package main

import (
	"context"
	"errors"
	"fmt"
	iofs "io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/keyguard"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

const (
	// databaseURLEnv selects Postgres: a postgres:// URL.
	databaseURLEnv = "TOOLYARD_DATABASE_URL"
	// databaseURLFile is the credentials file that may hold the same URL
	// (systemd LoadCredential, or -credentials-dir).
	databaseURLFile = "database-url"
)

// dbTarget is the database a command opens: Postgres when URL is set,
// otherwise the SQLite file at Path.
type dbTarget struct {
	URL    string // Postgres URL (holds the password: never log it)
	Path   string // SQLite file
	Source string // where the choice came from, for logs
}

func (t dbTarget) isPostgres() bool { return t.URL != "" }

// describe names the database for logs and output: the engine, and the
// host and database name or the file path. Never the URL or the password.
func (t dbTarget) describe() string {
	if !t.isPostgres() {
		return "sqlite " + t.Path
	}
	host, name, err := store.PostgresTarget(t.URL)
	if err != nil {
		return "postgres (unreadable URL)"
	}
	return fmt.Sprintf("postgres host=%s database=%s", host, name)
}

// resolveDatabase picks the database the same way for serve,
// operator-token, keys-check and db migrate:
//  1. $TOOLYARD_DATABASE_URL (a postgres:// URL), else
//  2. a file named database-url in $CREDENTIALS_DIRECTORY, then in
//     credentialsDir, holding that URL (surrounding whitespace trimmed), else
//  3. SQLite at <dataDir>/toolyard.db.
//
// A URL that is not postgres:// or postgresql:// (in any letter case) or
// that cannot be parsed, an empty or unreadable database-url file, is an
// error matching keyguard.ErrConfig (exit 78), so systemd does not restart
// a start that cannot work. No error quotes the URL.
func resolveDatabase(dataDir, credentialsDir string, getenv func(string) string) (dbTarget, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	if u := strings.TrimSpace(getenv(databaseURLEnv)); u != "" {
		if !store.IsPostgresURL(u) {
			return dbTarget{}, dbConfigErrorf("%s must be a postgres:// or postgresql:// URL", databaseURLEnv)
		}
		if err := checkPostgresURL(u); err != nil {
			return dbTarget{}, dbConfigErrorf("%s: %v", databaseURLEnv, err)
		}
		return dbTarget{URL: u, Source: databaseURLEnv}, nil
	}
	seen := map[string]bool{}
	for _, d := range []string{getenv("CREDENTIALS_DIRECTORY"), credentialsDir} {
		if d = strings.TrimSpace(d); d == "" {
			continue
		}
		p := filepath.Join(filepath.Clean(d), databaseURLFile)
		if seen[p] {
			continue
		}
		seen[p] = true
		b, err := os.ReadFile(p)
		if errors.Is(err, iofs.ErrNotExist) {
			continue
		}
		if err != nil {
			return dbTarget{}, dbConfigErrorf("%s: cannot read: %v", p, err)
		}
		u := strings.TrimSpace(string(b))
		if u == "" {
			return dbTarget{}, dbConfigErrorf("%s is empty", p)
		}
		if !store.IsPostgresURL(u) {
			return dbTarget{}, dbConfigErrorf("%s must hold a postgres:// or postgresql:// URL", p)
		}
		if err := checkPostgresURL(u); err != nil {
			return dbTarget{}, dbConfigErrorf("%s: %v", p, err)
		}
		return dbTarget{URL: u, Source: p}, nil
	}
	return dbTarget{Path: filepath.Join(dataDir, "toolyard.db"), Source: "-data"}, nil
}

// checkPostgresURL parses u as store does when it connects. Its error is
// store's fixed text (it never quotes the URL): the URL cannot be parsed,
// for example a bad port or an unescaped / in the password, or an unescaped
// @ in the password would put part of it in the host name.
func checkPostgresURL(u string) error {
	_, _, err := store.PostgresTarget(u)
	return err
}

// openDatabase opens t read-write and applies pending migrations.
// maxConns is the Postgres pool size (<= 0: store's default); SQLite always
// uses one connection. A Postgres setting a restart cannot fix (a database
// whose collation is not C) is an error matching keyguard.ErrConfig.
func openDatabase(ctx context.Context, t dbTarget, maxConns int) (*store.DB, error) {
	if t.isPostgres() {
		db, err := store.OpenPostgres(ctx, t.URL, maxConns)
		return db, asDBConfigError(err)
	}
	return store.Open(t.Path)
}

// migrateMode is serve's -migrate flag.
type migrateMode string

const (
	// migrateAuto applies pending migrations at startup (on Postgres under
	// the migration advisory lock): the behaviour before -migrate existed.
	migrateAuto migrateMode = "auto"
	// migrateOff applies none: `toolyard db migrate` does, before the
	// start, and serve refuses a database that is not at its level.
	migrateOff migrateMode = "off"
)

// parseMigrateMode parses -migrate. A value it does not know is an error
// matching keyguard.ErrConfig (exit 78).
func parseMigrateMode(s string) (migrateMode, error) {
	switch m := migrateMode(strings.ToLower(strings.TrimSpace(s))); m {
	case migrateAuto, migrateOff:
		return m, nil
	}
	return "", configErrorf("-migrate must be %q or %q, not %q", migrateAuto, migrateOff, s)
}

// openServeDatabase opens the database serve uses. Postgres opens with
// connection retries (openPostgresRetry, bounded by connectTimeout), then,
// with -migrate=auto, applies pending migrations under the advisory lock
// (what OpenPostgres does), or, with -migrate=off, applies none and checks
// the schema level (checkSchemaLevel). SQLite is a local file: it opens at
// once, migrated with auto (store.Open) and as it is with off; with off a
// file that does not exist is refused (exit 78) and not created. The second
// result lists, with off, the migrations the database records that this
// binary does not know (a newer toolyard migrated it): they do not stop the
// start, as with auto, and the caller logs them.
func openServeDatabase(ctx context.Context, t dbTarget, maxConns int, mode migrateMode, connectTimeout time.Duration, logf func(string, ...any)) (*store.DB, []string, error) {
	if !t.isPostgres() {
		if mode != migrateOff {
			db, err := store.Open(t.Path)
			return db, nil, err
		}
		db, err := store.OpenNoMigrate(t.Path)
		if errors.Is(err, iofs.ErrNotExist) {
			return nil, nil, dbConfigErrorf("-migrate=off and %s does not exist: run `toolyard db migrate` with the same -data and -credentials-dir to create it, then start again", t.Path)
		}
		if err != nil {
			return nil, nil, err
		}
		return checkSchemaLevel(ctx, db)
	}
	db, err := openPostgresRetry(ctx, t, store.PostgresOptions{MaxConns: maxConns, NoMigrate: true}, connectTimeout, logf)
	if err != nil {
		return nil, nil, err
	}
	if mode == migrateOff {
		return checkSchemaLevel(ctx, db)
	}
	if err := db.MigratePostgres(ctx); err != nil {
		_ = db.Close()
		return nil, nil, asDBConfigError(err)
	}
	return db, nil, nil
}

// openDatabaseNoMigrate is serve -migrate=off's open with no connection
// retry (openServeDatabase).
func openDatabaseNoMigrate(ctx context.Context, t dbTarget, maxConns int) (*store.DB, []string, error) {
	return openServeDatabase(ctx, t, maxConns, migrateOff, 0, nil)
}

// checkSchemaLevel is -migrate=off's check of db: when it does not record
// every migration this binary knows, db is closed and the error matches
// keyguard.ErrConfig (exit 78) and says to run toolyard db migrate. It
// returns db and the migrations db records that this binary does not know.
func checkSchemaLevel(ctx context.Context, db *store.DB) (*store.DB, []string, error) {
	pending, unknown, err := store.MigrationStatus(ctx, db)
	if err != nil {
		_ = db.Close()
		return nil, nil, fmt.Errorf("check the schema level: %w", err)
	}
	if len(pending) > 0 {
		_ = db.Close()
		return nil, nil, dbConfigErrorf("-migrate=off, but the database does not record %d migration(s) this binary needs (%s): "+
			"run `toolyard db migrate` with the same database settings, then start again", len(pending), nameList(pending, 3))
	}
	return db, unknown, nil
}

const (
	// defaultDBConnectTimeout is the default of -db-connect-timeout: an
	// Aurora failover takes 30 to 60 s.
	defaultDBConnectTimeout = 60 * time.Second
	// dbConnectAttemptTimeout bounds one connection attempt, so a server
	// that does not answer at all (packets dropped during a failover) costs
	// at most this before the next try.
	dbConnectAttemptTimeout = 15 * time.Second
	// firstConnectBackoff and maxConnectBackoff are the first and the
	// longest wait between two attempts.
	firstConnectBackoff = time.Second
	maxConnectBackoff   = 15 * time.Second
)

// dbConnectTimeoutUsage is the help text of -db-connect-timeout.
const dbConnectTimeoutUsage = "Postgres only: how long to keep trying to open the database when the server cannot be reached " +
	"(for example a restart during an Aurora failover). Attempts are 1 s apart, then twice as far apart each time, at most 15 s. " +
	"A bad URL, the wrong collation or encoding, a refused login or a database that does not exist fails at once. 0 = one attempt."

// checkConnectTimeout refuses a negative -db-connect-timeout (exit 78).
func checkConnectTimeout(d time.Duration) error {
	if d < 0 {
		return configErrorf("-db-connect-timeout must not be negative")
	}
	return nil
}

// openPostgresRetry opens t, a Postgres target, with opts. A failure to
// connect (store.ErrConnect) is tried again until connectTimeout has passed
// since the first attempt: firstConnectBackoff later, then twice as long
// each time, at most maxConnectBackoff; the last attempt starts at the
// deadline at the latest, and each is bounded by dbConnectAttemptTimeout
// (so the whole open takes at most connectTimeout plus that). Any other
// error (a bad URL, the wrong collation or encoding, a refused login, a
// database that does not exist) returns at once, and one that matches
// store.ErrConfig also matches keyguard.ErrConfig (exit 78).
// connectTimeout <= 0 makes one attempt. logf (nil: no log) gets one line
// per retry naming the database by host and name (dbTarget.describe) and
// the error, never the URL.
func openPostgresRetry(ctx context.Context, t dbTarget, opts store.PostgresOptions, connectTimeout time.Duration, logf func(string, ...any)) (*store.DB, error) {
	if connectTimeout <= 0 {
		db, err := store.OpenPostgresWith(ctx, t.URL, opts)
		return db, asDBConfigError(err)
	}
	start := time.Now()
	deadline := start.Add(connectTimeout)
	wait := firstConnectBackoff
	for attempt := 1; ; attempt++ {
		// The last attempt starts at the deadline at the latest and gets
		// its full time too.
		actx, cancel := context.WithTimeout(ctx, dbConnectAttemptTimeout)
		db, err := store.OpenPostgresWith(actx, t.URL, opts)
		cancel()
		if err == nil {
			return db, nil
		}
		if !errors.Is(err, store.ErrConnect) || ctx.Err() != nil {
			return nil, asDBConfigError(err)
		}
		left := time.Until(deadline)
		if left <= 0 {
			return nil, fmt.Errorf("gave up after %d attempts in %s (-db-connect-timeout %s): %w",
				attempt, time.Since(start).Round(100*time.Millisecond), connectTimeout, err)
		}
		if wait > left {
			wait = left
		}
		if logf != nil {
			logf("database: cannot connect to %s (attempt %d): %v; trying again in %s (-db-connect-timeout %s)",
				t.describe(), attempt, err, wait.Round(100*time.Millisecond), connectTimeout)
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, err
		case <-timer.C:
		}
		if wait *= 2; wait > maxConnectBackoff {
			wait = maxConnectBackoff
		}
	}
}

// nameList joins up to limit names, then says how many more there are.
func nameList(names []string, limit int) string {
	if len(names) <= limit {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(names[:limit], ", "), len(names)-limit)
}

// asDBConfigError makes an error matching store.ErrConfig also match
// keyguard.ErrConfig (exit 78). Other errors, nil included, pass unchanged.
func asDBConfigError(err error) error {
	if err == nil || !errors.Is(err, store.ErrConfig) || errors.Is(err, keyguard.ErrConfig) {
		return err
	}
	return &dbConfigError{msg: "database: " + err.Error(), err: err}
}

// dbConfigError is a database setting that is missing, unreadable or wrong:
// it exits 78 like a key error, since a restart will not fix it.
type dbConfigError struct {
	msg string
	err error // the underlying error, when there is one
}

func (e *dbConfigError) Error() string { return e.msg }

func (e *dbConfigError) Is(target error) bool { return target == keyguard.ErrConfig }

func (e *dbConfigError) Unwrap() error { return e.err }

func dbConfigErrorf(format string, args ...any) error {
	return &dbConfigError{msg: "database: " + fmt.Sprintf(format, args...)}
}
