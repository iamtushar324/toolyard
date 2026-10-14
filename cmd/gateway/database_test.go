package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/keyguard"
	"github.com/tusharbhardwaj/toolyard/internal/store"
	"github.com/tusharbhardwaj/toolyard/internal/store/storetest"
)

func dbEnvMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func writeCredFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

const testPGURL = "postgres://toolyard:s3cret-pw@db.example.invalid:5432/toolyard?sslmode=require"

func TestResolveDatabaseOrder(t *testing.T) {
	data := t.TempDir()
	systemd := t.TempDir()
	creds := t.TempDir()

	// Nothing set: SQLite in the data directory.
	got, err := resolveDatabase(data, creds, noEnv)
	if err != nil || got.isPostgres() || got.Path != filepath.Join(data, "toolyard.db") {
		t.Fatalf("default = %+v, %v; want SQLite at <data>/toolyard.db", got, err)
	}

	// -credentials-dir holds database-url (whitespace trimmed).
	writeCredFile(t, filepath.Join(creds, databaseURLFile), "  "+testPGURL+"\n")
	got, err = resolveDatabase(data, creds, noEnv)
	if err != nil || got.URL != testPGURL || got.Source != filepath.Join(creds, databaseURLFile) {
		t.Fatalf("credentials dir = %+v, %v", got, err)
	}

	// $CREDENTIALS_DIRECTORY comes before -credentials-dir.
	systemdURL := "postgresql://u:p@systemd.example.invalid/ty"
	writeCredFile(t, filepath.Join(systemd, databaseURLFile), systemdURL)
	got, err = resolveDatabase(data, creds, dbEnvMap(map[string]string{"CREDENTIALS_DIRECTORY": systemd}))
	if err != nil || got.URL != systemdURL {
		t.Fatalf("$CREDENTIALS_DIRECTORY = %+v, %v", got, err)
	}

	// $TOOLYARD_DATABASE_URL wins over both files.
	envURL := "postgres://u:p@env.example.invalid/ty"
	got, err = resolveDatabase(data, creds, dbEnvMap(map[string]string{"CREDENTIALS_DIRECTORY": systemd, databaseURLEnv: envURL}))
	if err != nil || got.URL != envURL || got.Source != databaseURLEnv {
		t.Fatalf("env = %+v, %v", got, err)
	}
}

func TestResolveDatabaseRejectsBadSettings(t *testing.T) {
	data := t.TempDir()
	for name, setup := range map[string]func(t *testing.T, creds string) func(string) string{
		"env is not a postgres URL": func(*testing.T, string) func(string) string {
			return dbEnvMap(map[string]string{databaseURLEnv: "/var/lib/toolyard/s3cret-pw.db"})
		},
		"empty file": func(t *testing.T, creds string) func(string) string {
			writeCredFile(t, filepath.Join(creds, databaseURLFile), " \n")
			return noEnv
		},
		"file is not a postgres URL": func(t *testing.T, creds string) func(string) string {
			writeCredFile(t, filepath.Join(creds, databaseURLFile), "mysql://u:s3cret-pw@h/db")
			return noEnv
		},
		// pgx takes the first @ as the end of the password, so the rest of
		// the password would land in the host name (and in the logs).
		"env has an unescaped @ in the password": func(*testing.T, string) func(string) string {
			return dbEnvMap(map[string]string{databaseURLEnv: "postgres://toolyard:s3cret@pw@db.example.invalid:5432/toolyard"})
		},
		"env has an unescaped / in the password": func(*testing.T, string) func(string) string {
			return dbEnvMap(map[string]string{databaseURLEnv: "postgres://toolyard:s3cret/pw@db.example.invalid:5432/toolyard"})
		},
		"env has a bad port": func(*testing.T, string) func(string) string {
			return dbEnvMap(map[string]string{databaseURLEnv: "postgres://toolyard:s3cret-pw@db.example.invalid:54x2/toolyard"})
		},
		"file has a bad port": func(t *testing.T, creds string) func(string) string {
			writeCredFile(t, filepath.Join(creds, databaseURLFile), "postgresql://toolyard:s3cret-pw@db.example.invalid:99999/toolyard")
			return noEnv
		},
	} {
		t.Run(name, func(t *testing.T) {
			creds := t.TempDir()
			getenv := setup(t, creds)
			_, err := resolveDatabase(data, creds, getenv)
			if !errors.Is(err, keyguard.ErrConfig) {
				t.Fatalf("err = %v, want an error that exits 78", err)
			}
			if strings.Contains(err.Error(), "s3cret-pw") {
				t.Fatalf("the error quotes the setting: %v", err)
			}
		})
	}
}

// An upper-case scheme is a Postgres URL for every step: resolve, describe
// and (in store) connect.
func TestResolveDatabaseAcceptsAnUpperCaseScheme(t *testing.T) {
	upper := "POSTGRES://toolyard:s3cret-pw@db.example.invalid:5432/toolyard?sslmode=require"
	got, err := resolveDatabase(t.TempDir(), "", dbEnvMap(map[string]string{databaseURLEnv: upper}))
	if err != nil || !got.isPostgres() {
		t.Fatalf("upper-case scheme = %+v, %v", got, err)
	}
	if d := got.describe(); d != "postgres host=db.example.invalid database=toolyard" {
		t.Fatalf("describe = %q", d)
	}
}

func TestDescribeNeverShowsTheURL(t *testing.T) {
	d := dbTarget{URL: testPGURL}.describe()
	if strings.Contains(d, "s3cret-pw") || strings.Contains(d, "toolyard:") || d != "postgres host=db.example.invalid database=toolyard" {
		t.Fatalf("describe = %q", d)
	}
	if d := (dbTarget{Path: "/data/toolyard.db"}).describe(); d != "sqlite /data/toolyard.db" {
		t.Fatalf("describe = %q", d)
	}
}

// TestKeysCheckOnPostgres: keys-check opens the Postgres database read-only
// (no migrations, no writes) and checks the keys against its canaries.
func TestKeysCheckOnPostgres(t *testing.T) {
	storetest.SkipUnlessPostgres(t)
	ctx := context.Background()
	dbURL := storetest.NewPostgresDatabase(t)
	dir := t.TempDir()

	db, err := store.OpenPostgres(ctx, dbURL, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := keyguard.LoadAll(ctx, keyguard.Config{
		DB: db.DB, Dialect: db.Dialect, DataDir: dir, Mode: keyguard.CreateIfFresh, Getenv: noEnv, Logf: t.Logf,
	}, keyguard.ServeKeys()); err != nil {
		t.Fatalf("load: %v", err)
	}
	countCanaries := func() int {
		var n int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM key_canaries`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	canaries := countCanaries()
	before := snapDir(t, dir)

	var out bytes.Buffer
	getenv := dbEnvMap(map[string]string{databaseURLEnv: dbURL})
	if err := keysCheck(ctx, &out, dir, "", "require", getenv); err != nil {
		t.Fatalf("keys-check: %v\n%s", err, out.String())
	}
	sameDir(t, before, snapDir(t, dir))
	if n := countCanaries(); n != canaries {
		t.Fatalf("keys-check changed the canaries: %d before, %d after", canaries, n)
	}
	lines := keyLines(t, out.String())
	for _, n := range []string{"identity.key", "oauth.key", "secrets.key", "session.key"} {
		if lines[n] != "ok" {
			t.Errorf("%s: %q\n%s", n, lines[n], out.String())
		}
	}
	if !strings.Contains(out.String(), "database     postgres host=") || strings.Contains(out.String(), dbURL) {
		t.Fatalf("database line should name the engine and host, never the URL:\n%s", out.String())
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}
