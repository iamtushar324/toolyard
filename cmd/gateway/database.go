package main

import (
	"context"
	"errors"
	"fmt"
	iofs "io/fs"
	"os"
	"path/filepath"
	"strings"

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

// openDatabaseNoMigrate opens t read-write and applies no migration (serve
// -migrate=off), then checks the schema level: when the database does not
// record every migration this binary knows, it is closed and the error
// matches keyguard.ErrConfig (exit 78) and says to run toolyard db
// migrate. A SQLite file that does not exist is the same error, and is not
// created. The second result lists the migrations the database records
// that this binary does not know (a newer toolyard migrated it): they do
// not stop the start, as with -migrate=auto, and the caller logs them.
func openDatabaseNoMigrate(ctx context.Context, t dbTarget, maxConns int) (*store.DB, []string, error) {
	var db *store.DB
	var err error
	if t.isPostgres() {
		db, err = store.OpenPostgresWith(ctx, t.URL, store.PostgresOptions{MaxConns: maxConns, NoMigrate: true})
		if err != nil {
			return nil, nil, asDBConfigError(err)
		}
	} else {
		db, err = store.OpenNoMigrate(t.Path)
		if errors.Is(err, iofs.ErrNotExist) {
			return nil, nil, dbConfigErrorf("-migrate=off and %s does not exist: run `toolyard db migrate` with the same -data and -credentials-dir to create it, then start again", t.Path)
		}
		if err != nil {
			return nil, nil, err
		}
	}
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
