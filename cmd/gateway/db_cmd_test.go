package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/dbcopy"
	"github.com/tusharbhardwaj/toolyard/internal/keyguard"
	"github.com/tusharbhardwaj/toolyard/internal/store"
	"github.com/tusharbhardwaj/toolyard/internal/store/storetest"
)

// dbCmdSecretURL points at a port where nothing listens; its password must
// never appear in any output.
const dbCmdSecretURL = "postgres://toolyard:s3cret-pw@127.0.0.1:1/toolyard?sslmode=disable&connect_timeout=5"

func dbCmdEnv(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func runDBForTest(t *testing.T, sub string, env map[string]string, args ...string) (string, string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var out, errOut bytes.Buffer
	err := runDBCommand(ctx, sub, args, &out, &errOut, dbCmdEnv(env))
	return out.String(), errOut.String(), err
}

func TestDBNeedsASubcommand(t *testing.T) {
	if err := runDB(nil); err == nil {
		t.Fatal("toolyard db with no subcommand succeeded")
	}
	if err := runDB([]string{"migrate"}); err == nil || !strings.Contains(err.Error(), "unknown subcommand") {
		t.Fatalf("unknown subcommand: %v", err)
	}
	if err := runDB([]string{"help"}); err != nil {
		t.Fatalf("help: %v", err)
	}
}

func TestDBFlags(t *testing.T) {
	for _, sub := range []string{"copy", "verify"} {
		if _, _, err := runDBForTest(t, sub, nil); err == nil || !strings.Contains(err.Error(), "-from-data") {
			t.Fatalf("%s without -from-data: %v", sub, err)
		}
	}
	// -truncate-target belongs to copy only.
	if _, _, err := runDBForTest(t, "verify", nil, "-from-data", t.TempDir(), "-truncate-target"); err == nil {
		t.Fatal("verify accepted -truncate-target")
	}
	if _, _, err := runDBForTest(t, "copy", nil, "-from-data", t.TempDir()); err == nil || !strings.Contains(err.Error(), "source database") {
		t.Fatalf("copy from a directory without toolyard.db: %v", err)
	}
	if _, _, err := runDBForTest(t, "copy", nil, "-from-data", t.TempDir(), "extra"); err == nil {
		t.Fatal("copy accepted a stray argument")
	}
}

// A source with a -shm file or a non-empty -wal file is open somewhere:
// the command refuses it unless -allow-live-source.
func TestDBRefusesALiveSource(t *testing.T) {
	dir := installed(t)
	dbPath := filepath.Join(dir, "toolyard.db")
	if why := dbSourceInUse(dbPath); why != "" {
		t.Fatalf("a closed database looks in use: %s", why)
	}
	env := map[string]string{"TOOLYARD_DATABASE_URL": dbCmdSecretURL}
	for _, side := range []struct{ suffix, body string }{{"-shm", "x"}, {"-wal", "not empty"}} {
		p := dbPath + side.suffix
		if err := os.WriteFile(p, []byte(side.body), 0o600); err != nil {
			t.Fatal(err)
		}
		_, _, err := runDBForTest(t, "copy", env, "-from-data", dir)
		if err == nil || !strings.Contains(err.Error(), "in use") || !strings.Contains(err.Error(), "-allow-live-source") {
			t.Fatalf("%s: %v", side.suffix, err)
		}
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
	}
	// An empty -wal file is what a clean close can leave: not in use.
	if err := os.WriteFile(dbPath+"-wal", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if why := dbSourceInUse(dbPath); why != "" {
		t.Fatalf("an empty -wal file counts as in use: %s", why)
	}
}

func TestDBNeedsAPostgresTarget(t *testing.T) {
	dir := installed(t)
	_, _, err := runDBForTest(t, "copy", nil, "-from-data", dir, "-credentials-dir", t.TempDir())
	if !errors.Is(err, keyguard.ErrConfig) || !strings.Contains(err.Error(), "TOOLYARD_DATABASE_URL") {
		t.Fatalf("copy with no target: %v", err)
	}
}

// Neither the error nor any output shows the target URL or its password,
// even when the target cannot be reached.
func TestDBNeverShowsTheTargetURL(t *testing.T) {
	dir := installed(t)
	env := map[string]string{"TOOLYARD_DATABASE_URL": dbCmdSecretURL}
	out, errOut, err := runDBForTest(t, "copy", env, "-from-data", dir)
	if err == nil {
		t.Fatal("copy into an unreachable target succeeded")
	}
	for _, s := range []string{err.Error(), out, errOut} {
		if strings.Contains(s, "s3cret-pw") || strings.Contains(s, dbCmdSecretURL) {
			t.Fatalf("the URL or its password was shown: %q", s)
		}
	}
}

// verify reads the master keys (never creating one) before it opens the
// target.
func TestDBVerifyNeedsTheKeys(t *testing.T) {
	dir := installed(t)
	for _, n := range []string{keyguard.IdentityKey, keyguard.OAuthKey, keyguard.SecretsKey, keyguard.SessionKey} {
		if err := os.Remove(filepath.Join(dir, n)); err != nil {
			t.Fatal(err)
		}
	}
	env := map[string]string{"TOOLYARD_DATABASE_URL": dbCmdSecretURL}
	_, _, err := runDBForTest(t, "verify", env, "-from-data", dir, "-credentials-dir", t.TempDir())
	if !errors.Is(err, keyguard.ErrConfig) || !strings.Contains(err.Error(), keyguard.IdentityKey) {
		t.Fatalf("verify without keys: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, keyguard.IdentityKey)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("verify created a key file")
	}
}

func TestPrintAndWriteDBReport(t *testing.T) {
	rep := &dbcopy.Report{
		Command: "verify",
		Tables: []dbcopy.TableReport{
			{Name: "users", SourceRows: 2, TargetRows: 2, Match: true},
			{Name: "system_settings", SourceRows: 3, TargetRows: 3},
		},
		Problems: []string{"table system_settings differs: 3 source rows, 3 target rows"},
	}
	var out bytes.Buffer
	printDBReport(&out, rep)
	for _, want := range []string{"1 of 2 match", "DIFF   system_settings", "1 problems"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("summary does not mention %q:\n%s", want, out.String())
		}
	}
	path := filepath.Join(t.TempDir(), "report.json")
	if err := writeDBReport(path, rep); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"ok": false`) || !strings.Contains(string(b), `"command": "verify"`) {
		t.Fatalf("report JSON:\n%s", b)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("report file mode %v, want 0600", info.Mode().Perm())
	}
}

// writeDBReport never keeps an existing file's mode and never writes
// through a symlink: it renames a new 0600 file over the path.
func TestWriteDBReportReplacesTheFile(t *testing.T) {
	rep := &dbcopy.Report{Command: "copy"}
	dir := t.TempDir()

	path := filepath.Join(dir, "report.json")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeDBReport(path, rep); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("an existing 0644 report: %v %v, want mode 0600", info, err)
	}
	if b, _ := os.ReadFile(path); !strings.Contains(string(b), `"command": "copy"`) {
		t.Fatalf("report JSON:\n%s", b)
	}

	elsewhere := filepath.Join(t.TempDir(), "elsewhere.txt")
	if err := os.WriteFile(elsewhere, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "linked.json")
	if err := os.Symlink(elsewhere, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := writeDBReport(link, rep); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(link); err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("the symlink was not replaced by a 0600 file: %v %v", info, err)
	}
	if b, err := os.ReadFile(elsewhere); err != nil || string(b) != "keep" {
		t.Fatalf("the report was written through the symlink: %q %v", b, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("temporary files left behind: %v", entries)
	}
}

// The source guard sees a source file that grew, got a new modification
// time, or a sidecar that appeared or disappeared, and a database that
// came into use; with -allow-live-source it only warns.
func TestSourceGuardSeesChanges(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "toolyard.db")
	if err := os.WriteFile(dbPath, []byte("sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}
	stat := func() []sourceFile {
		t.Helper()
		s, err := statSource(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	expect := func(before []sourceFile, want ...string) {
		t.Helper()
		got := sourceChanges(before, stat())
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Fatalf("changes %q, want %q", got, want)
		}
	}

	before := stat()
	expect(before)
	if err := os.WriteFile(dbPath, []byte("sqlite, grown"), 0o600); err != nil {
		t.Fatal(err)
	}
	expect(before, "toolyard.db changed")

	before = stat()
	later := before[0].mod.Add(3 * time.Second)
	if err := os.Chtimes(dbPath, later, later); err != nil {
		t.Fatal(err)
	}
	expect(before, "toolyard.db changed") // same size, new time

	before = stat()
	if err := os.WriteFile(dbPath+"-wal", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	expect(before, "toolyard.db-wal appeared")
	before = stat()
	if err := os.Remove(dbPath + "-wal"); err != nil {
		t.Fatal(err)
	}
	expect(before, "toolyard.db-wal disappeared")

	guard, err := newSourceGuard(dbPath, false, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	var warned bytes.Buffer
	live, err := newSourceGuard(dbPath, true, &warned)
	if err != nil {
		t.Fatal(err)
	}
	if err := guard(); err != nil {
		t.Fatalf("an unchanged source: %v", err)
	}
	// A toolyard starts on it: a -shm file appears.
	if err := os.WriteFile(dbPath+"-shm", []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = guard()
	if err == nil || !strings.Contains(err.Error(), "toolyard.db-shm appeared") || !strings.Contains(err.Error(), "in use") {
		t.Fatalf("a source that came into use: %v", err)
	}
	if err := live(); err != nil || !strings.Contains(warned.String(), "WARNING") {
		t.Fatalf("-allow-live-source: %v, warning %q", err, warned.String())
	}
}

// db verify opens the target read-only and never migrates it: a target one
// migration behind stays one migration behind, and verify reports the
// difference.
func TestDBVerifyNeverMigratesTheTarget(t *testing.T) {
	storetest.SkipUnlessPostgres(t)
	ctx := context.Background()
	dir := installed(t)
	dbURL := storetest.NewPostgresDatabase(t)
	names, err := store.PostgresMigrationNames()
	if err != nil || len(names) == 0 {
		t.Fatalf("migration names: %v %v", names, err)
	}
	last := names[len(names)-1]
	db, err := store.OpenPostgresWith(ctx, dbURL, store.PostgresOptions{MaxConns: 1, NoMigrate: true})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE name = ?`, last); err != nil {
		t.Fatal(err)
	}
	countMigrations := func() int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := countMigrations()

	env := map[string]string{"TOOLYARD_DATABASE_URL": dbURL}
	out, _, err := runDBForTest(t, "verify", env, "-from-data", dir, "-credentials-dir", t.TempDir())
	if err == nil {
		t.Fatal("verify passed against a target one migration behind")
	}
	if !strings.Contains(out, "migrations differ") {
		t.Fatalf("verify did not report the migration difference:\n%s", out)
	}
	if n := countMigrations(); n != before {
		t.Fatalf("verify changed schema_migrations: %d rows before, %d after", before, n)
	}
	var has bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE name = ?)`, last).Scan(&has); err != nil || has {
		t.Fatalf("verify applied %s (%v)", last, err)
	}
}

// db copy into a database that holds another application's tables (and
// records no toolyard migration) is refused, and creates no toolyard table.
func TestDBCopyRefusesADatabaseWithOtherTables(t *testing.T) {
	storetest.SkipUnlessPostgres(t)
	ctx := context.Background()
	dir := installed(t)
	dbURL, err := storetest.NewEmptyPostgresDatabase(t, "C")
	if err != nil {
		t.Fatalf("create an empty database: %v", err)
	}
	db, err := store.OpenPostgresWith(ctx, dbURL, store.PostgresOptions{MaxConns: 1, NoMigrate: true})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `CREATE TABLE other_app_users (id BIGINT PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}

	env := map[string]string{"TOOLYARD_DATABASE_URL": dbURL}
	_, _, err = runDBForTest(t, "copy", env, "-from-data", dir)
	if err == nil || !strings.Contains(err.Error(), "not an empty database") {
		t.Fatalf("copy into another application's database: %v", err)
	}
	for _, table := range []string{"schema_migrations", "users", "system_settings", "inbox_blob_objects"} {
		if has, err := store.TableExists(ctx, db, store.Postgres, table); err != nil || has {
			t.Fatalf("the refused copy created %s (%v)", table, err)
		}
	}
}
