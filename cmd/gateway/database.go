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
// operator-token and keys-check:
//  1. $TOOLYARD_DATABASE_URL (a postgres:// URL), else
//  2. a file named database-url in $CREDENTIALS_DIRECTORY, then in
//     credentialsDir, holding that URL (surrounding whitespace trimmed), else
//  3. SQLite at <dataDir>/toolyard.db.
//
// A URL that is not postgres:// or postgresql://, an empty or unreadable
// database-url file, is an error matching keyguard.ErrConfig (exit 78). No
// error quotes the URL.
func resolveDatabase(dataDir, credentialsDir string, getenv func(string) string) (dbTarget, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	if u := strings.TrimSpace(getenv(databaseURLEnv)); u != "" {
		if !store.IsPostgresURL(u) {
			return dbTarget{}, dbConfigErrorf("%s must be a postgres:// or postgresql:// URL", databaseURLEnv)
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
		return dbTarget{URL: u, Source: p}, nil
	}
	return dbTarget{Path: filepath.Join(dataDir, "toolyard.db"), Source: "-data"}, nil
}

// openDatabase opens t read-write and applies pending migrations.
// maxConns is the Postgres pool size (<= 0: store's default); SQLite always
// uses one connection.
func openDatabase(ctx context.Context, t dbTarget, maxConns int) (*store.DB, error) {
	if t.isPostgres() {
		return store.OpenPostgres(ctx, t.URL, maxConns)
	}
	return store.Open(t.Path)
}

// dbConfigError is a database setting that is missing, unreadable or wrong:
// it exits 78 like a key error, since a restart will not fix it.
type dbConfigError struct{ msg string }

func (e *dbConfigError) Error() string { return e.msg }

func (e *dbConfigError) Is(target error) bool { return target == keyguard.ErrConfig }

func dbConfigErrorf(format string, args ...any) error {
	return &dbConfigError{msg: "database: " + fmt.Sprintf(format, args...)}
}
