package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/dbcopy"
	"github.com/tusharbhardwaj/toolyard/internal/keyguard"
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
