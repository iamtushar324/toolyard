package main

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/keyguard"
	"github.com/tusharbhardwaj/toolyard/internal/store"
	"github.com/tusharbhardwaj/toolyard/internal/store/storetest"
)

func runMigrateForTest(t *testing.T, env map[string]string, args ...string) (string, string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var out, errOut bytes.Buffer
	err := runDBMigrate(ctx, args, &out, &errOut, flagEnv{lookup: mapLookup(env)})
	return out.String(), errOut.String(), err
}

// outputLines returns the lines of out whose label (first field) is label.
func outputLines(out, label string) []string {
	var got []string
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[0] == label {
			got = append(got, f[1])
		}
	}
	return got
}

// emptyTarget returns a database target with no toolyard schema on the
// engine of the test run, and the environment that selects it: SQLite in a
// new data directory (no file yet), or an empty Postgres database.
func emptyTarget(t *testing.T) (dataDir string, env map[string]string) {
	t.Helper()
	dataDir = filepath.Join(t.TempDir(), "data")
	env = map[string]string{}
	if storetest.Engine() == store.Postgres {
		dbURL, err := storetest.NewEmptyPostgresDatabase(t, "C")
		if err != nil {
			t.Fatalf("create an empty database: %v", err)
		}
		env[databaseURLEnv] = dbURL
	}
	return dataDir, env
}

// migratedTarget returns a fully migrated database on the engine of the
// test run, as a dbTarget, and the environment that selects it.
func migratedTarget(t *testing.T) (dbTarget, string, map[string]string) {
	t.Helper()
	dataDir := t.TempDir()
	if storetest.Engine() == store.Postgres {
		dbURL := storetest.NewPostgresDatabase(t)
		return dbTarget{URL: dbURL, Source: databaseURLEnv}, dataDir, map[string]string{databaseURLEnv: dbURL}
	}
	path := filepath.Join(dataDir, "toolyard.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return dbTarget{Path: path, Source: "-data"}, dataDir, map[string]string{}
}

// openAsIs opens target without migrating, for a test to change it.
func openAsIs(t *testing.T, target dbTarget) *store.DB {
	t.Helper()
	var db *store.DB
	var err error
	if target.isPostgres() {
		db, err = store.OpenPostgresWith(context.Background(), target.URL, store.PostgresOptions{MaxConns: 1, NoMigrate: true})
	} else {
		db, err = store.OpenNoMigrate(target.Path)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// db migrate on an empty database: -dry-run lists every migration and
// creates nothing; migrate applies them all; a second run is up to date.
func TestDBMigrateAnEmptyDatabase(t *testing.T) {
	dataDir, env := emptyTarget(t)
	known, err := store.KnownMigrations(storetest.Engine())
	if err != nil {
		t.Fatal(err)
	}

	out, _, err := runMigrateForTest(t, env, "-data", dataDir, "-dry-run")
	if err != nil {
		t.Fatalf("dry run: %v\n%s", err, out)
	}
	if got := outputLines(out, "pending"); strings.Join(got, ",") != strings.Join(known, ",") {
		t.Fatalf("dry run lists %d pending, want all %d:\n%s", len(got), len(known), out)
	}
	if len(outputLines(out, "applied")) != 0 || !strings.Contains(out, "nothing applied") {
		t.Fatalf("dry run output:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "toolyard.db")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the dry run created a database file (%v)", err)
	}
	// The dry run did not migrate: a second one lists the same.
	out2, _, err := runMigrateForTest(t, env, "-data", dataDir, "-dry-run")
	if err != nil || len(outputLines(out2, "pending")) != len(known) {
		t.Fatalf("second dry run: %v\n%s", err, out2)
	}

	out, _, err = runMigrateForTest(t, env, "-data", dataDir)
	if err != nil {
		t.Fatalf("migrate: %v\n%s", err, out)
	}
	if got := outputLines(out, "applied"); strings.Join(got, ",") != strings.Join(known, ",") {
		t.Fatalf("migrate applied %d, want all %d:\n%s", len(got), len(known), out)
	}

	out, _, err = runMigrateForTest(t, env, "-data", dataDir)
	if err != nil || !strings.Contains(out, "up to date") || len(outputLines(out, "applied")) != 0 {
		t.Fatalf("second migrate: %v\n%s", err, out)
	}
	out, _, err = runMigrateForTest(t, env, "-data", dataDir, "-dry-run")
	if err != nil || !strings.Contains(out, "up to date") || len(outputLines(out, "pending")) != 0 {
		t.Fatalf("dry run after migrate: %v\n%s", err, out)
	}
}

// A database one migration behind: -dry-run names that migration, and
// serve -migrate=off refuses it (exit 78) with a pointer to db migrate.
func TestDBMigrateSeesAMissingMigration(t *testing.T) {
	ctx := context.Background()
	target, dataDir, env := migratedTarget(t)
	known, err := store.KnownMigrations(storeDialect(target))
	if err != nil {
		t.Fatal(err)
	}
	last := known[len(known)-1]
	db := openAsIs(t, target)
	if _, err := db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE name = ?`, last); err != nil {
		t.Fatal(err)
	}

	out, _, err := runMigrateForTest(t, env, "-data", dataDir, "-dry-run")
	if err != nil || strings.Join(outputLines(out, "pending"), ",") != last {
		t.Fatalf("dry run: %v, want %s pending:\n%s", err, last, out)
	}

	_, _, err = openDatabaseNoMigrate(ctx, target, 1)
	if !errors.Is(err, keyguard.ErrConfig) {
		t.Fatalf("-migrate=off on a database one migration behind: %v, want exit 78", err)
	}
	if !strings.Contains(err.Error(), "toolyard db migrate") || !strings.Contains(err.Error(), last) {
		t.Fatalf("the refusal does not name db migrate and %s: %v", last, err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE name = ?`, last).Scan(&n); err != nil || n != 0 {
		t.Fatalf("-migrate=off or the dry run applied %s (%d, %v)", last, n, err)
	}
}

// serve -migrate=off opens a database at this binary's level without
// migrating it; a migration it does not know does not stop it, and is
// returned for the log.
func TestOpenDatabaseNoMigrate(t *testing.T) {
	ctx := context.Background()
	target, _, _ := migratedTarget(t)
	db, unknown, err := openDatabaseNoMigrate(ctx, target, 2)
	if err != nil {
		t.Fatalf("a migrated database: %v", err)
	}
	if len(unknown) != 0 {
		t.Fatalf("unknown migrations %v", unknown)
	}
	future := "9999_from_a_newer_toolyard.sql"
	if _, err := db.ExecContext(ctx, `INSERT INTO schema_migrations(name, applied_at) VALUES(?, ?)`, future, 1); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, unknown, err = openDatabaseNoMigrate(ctx, target, 2)
	if err != nil {
		t.Fatalf("a database a newer toolyard migrated: %v", err)
	}
	defer db.Close()
	if strings.Join(unknown, ",") != future {
		t.Fatalf("unknown migrations %v, want [%s]", unknown, future)
	}
}

// serve -migrate=off with no SQLite file refuses (exit 78) and creates none.
func TestOpenDatabaseNoMigrateNeedsAnExistingSQLiteFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "toolyard.db")
	_, _, err := openDatabaseNoMigrate(context.Background(), dbTarget{Path: path}, 1)
	if !errors.Is(err, keyguard.ErrConfig) || !strings.Contains(err.Error(), "toolyard db migrate") {
		t.Fatalf("-migrate=off without a database: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("-migrate=off created %s (%v)", path, err)
	}
	// An empty file is a database with no schema: every migration missing.
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err = openDatabaseNoMigrate(context.Background(), dbTarget{Path: path}, 1)
	if !errors.Is(err, keyguard.ErrConfig) || !strings.Contains(err.Error(), "toolyard db migrate") {
		t.Fatalf("-migrate=off on an empty database: %v", err)
	}
}

// db migrate selects the database as serve does, refuses a bad setting
// with exit 78, and never shows the URL or its password.
func TestDBMigrateSettings(t *testing.T) {
	data := t.TempDir()
	_, _, err := runMigrateForTest(t, map[string]string{databaseURLEnv: "mysql://u:s3cret-pw@h/db"}, "-data", data)
	if !errors.Is(err, keyguard.ErrConfig) || strings.Contains(err.Error(), "s3cret-pw") {
		t.Fatalf("not a postgres URL: %v", err)
	}
	out, errOut, err := runMigrateForTest(t, map[string]string{databaseURLEnv: dbCmdSecretURL}, "-data", data, "-timeout", "20s", "-db-connect-timeout", "0")
	if err == nil {
		t.Fatal("migrate against an unreachable database succeeded")
	}
	for _, s := range []string{err.Error(), out, errOut} {
		if strings.Contains(s, "s3cret-pw") || strings.Contains(s, dbCmdSecretURL) {
			t.Fatalf("the URL or its password was shown: %q", s)
		}
	}
	if !strings.Contains(out, "postgres host=127.0.0.1") {
		t.Fatalf("the database line does not name the host:\n%s", out)
	}
	if _, _, err := runMigrateForTest(t, nil, "-data", data, "extra"); err == nil || !strings.Contains(err.Error(), "unexpected arguments") {
		t.Fatalf("stray argument: %v", err)
	}
	if _, _, err := runMigrateForTest(t, nil, "-data", data, "-timeout", "-1s"); !errors.Is(err, keyguard.ErrConfig) {
		t.Fatalf("negative -timeout: %v", err)
	}
	if _, _, err := runMigrateForTest(t, nil, "-data", data, "-db-connect-timeout", "-1s"); !errors.Is(err, keyguard.ErrConfig) {
		t.Fatalf("negative -db-connect-timeout: %v", err)
	}
}

// TOOLYARD_DATA and TOOLYARD_CREDENTIALS_DIR choose the database when the
// flags are not given: the database-url file in the credentials directory.
func TestDBMigrateReadsTheEnvironment(t *testing.T) {
	creds := t.TempDir()
	writeCredFile(t, filepath.Join(creds, databaseURLFile), dbCmdSecretURL)
	data := t.TempDir()
	out, _, err := runMigrateForTest(t, map[string]string{"TOOLYARD_CREDENTIALS_DIR": creds, "TOOLYARD_DATA": data}, "-timeout", "20s", "-db-connect-timeout", "0")
	if err == nil || !strings.Contains(out, "from "+filepath.Join(creds, databaseURLFile)) {
		t.Fatalf("TOOLYARD_CREDENTIALS_DIR: %v\n%s", err, out)
	}

	// SQLite under TOOLYARD_DATA (dry run: nothing is created).
	out, _, err = runMigrateForTest(t, map[string]string{"TOOLYARD_DATA": data}, "-dry-run")
	if err != nil || !strings.Contains(out, "sqlite "+filepath.Join(data, "toolyard.db")) {
		t.Fatalf("TOOLYARD_DATA: %v\n%s", err, out)
	}
	if entries, _ := os.ReadDir(data); len(entries) != 0 {
		t.Fatalf("the dry run wrote %v", entries)
	}
}

// db migrate keeps trying a server it cannot reach for -db-connect-timeout,
// logging each retry on stderr without the URL; TOOLYARD_DB_CONNECT_TIMEOUT
// sets it too (0: one attempt, no retry).
func TestDBMigrateRetriesAnUnreachableDatabase(t *testing.T) {
	data := t.TempDir()
	env := map[string]string{databaseURLEnv: dbCmdSecretURL}
	start := time.Now()
	out, errOut, err := runMigrateForTest(t, env, "-data", data, "-db-connect-timeout", "1500ms")
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "gave up") {
		t.Fatalf("migrate against an unreachable database: %v", err)
	}
	if elapsed < time.Second || elapsed > 20*time.Second {
		t.Fatalf("gave up after %s, want about 1.5 s", elapsed)
	}
	if n := strings.Count(errOut, "trying again"); n < 1 {
		t.Fatalf("no retry logged:\n%s", errOut)
	}
	for _, s := range []string{err.Error(), out, errOut} {
		if strings.Contains(s, "s3cret-pw") || strings.Contains(s, dbCmdSecretURL) {
			t.Fatalf("the URL or its password was shown: %q", s)
		}
	}

	env["TOOLYARD_DB_CONNECT_TIMEOUT"] = "0"
	start = time.Now()
	_, errOut, err = runMigrateForTest(t, env, "-data", data)
	if err == nil || strings.Contains(errOut, "trying again") || time.Since(start) > 10*time.Second {
		t.Fatalf("TOOLYARD_DB_CONNECT_TIMEOUT=0: %v after %s\n%s", err, time.Since(start), errOut)
	}
}
