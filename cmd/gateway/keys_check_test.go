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

	"github.com/tusharbhardwaj/toolyard/internal/keyguard"
	"github.com/tusharbhardwaj/toolyard/internal/sealbox"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

func noEnv(string) string { return "" }

type fileSnap struct {
	data []byte
	mod  time.Time
}

func snapDir(t *testing.T, dir string) map[string]fileSnap {
	t.Helper()
	out := map[string]fileSnap{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = fileSnap{data: b, mod: info.ModTime()}
	}
	return out
}

func sameDir(t *testing.T, before, after map[string]fileSnap) {
	t.Helper()
	if len(before) != len(after) {
		t.Fatalf("files changed: %d before, %d after", len(before), len(after))
	}
	for name, b := range before {
		a, ok := after[name]
		if !ok || !bytes.Equal(a.data, b.data) || !a.mod.Equal(b.mod) {
			t.Fatalf("%s was written", name)
		}
	}
}

// installed makes a data directory the way `serve` leaves it: a migrated
// database, four keys and their canaries.
func installed(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "toolyard.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := keyguard.LoadAll(context.Background(), keyguard.Config{
		DB: db.DB, DataDir: dir, Mode: keyguard.CreateIfFresh, Getenv: noEnv, Logf: t.Logf,
	}, keyguard.ServeKeys()); err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return dir
}

// keyLines returns the status column of each key line, by key name.
func keyLines(t *testing.T, out string) map[string]string {
	t.Helper()
	got := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && strings.HasSuffix(f[0], ".key") {
			got[f[0]] = f[1]
		}
	}
	return got
}

func TestKeysCheckPassesAndChangesNothing(t *testing.T) {
	dir := installed(t)
	before := snapDir(t, dir)
	var out bytes.Buffer
	if err := keysCheck(context.Background(), &out, dir, "", "create-if-fresh", noEnv); err != nil {
		t.Fatalf("keys-check: %v\n%s", err, out.String())
	}
	sameDir(t, before, snapDir(t, dir))
	lines := keyLines(t, out.String())
	for _, n := range []string{"identity.key", "oauth.key", "secrets.key", "session.key"} {
		if lines[n] != "ok" {
			t.Errorf("%s: %q\n%s", n, lines[n], out.String())
		}
	}
	if n := strings.Count(strings.TrimSpace(out.String()), "\n") + 1; n != 5 {
		t.Errorf("%d output lines, want the database line and one per key:\n%s", n, out.String())
	}
	// No key material in the output.
	for name, f := range before {
		if strings.HasSuffix(name, ".key") && strings.Contains(out.String(), strings.TrimSpace(string(f.data))) {
			t.Fatalf("output shows the contents of %s", name)
		}
	}
}

func TestKeysCheckFailsOnAWrongKey(t *testing.T) {
	dir := installed(t)
	p := filepath.Join(dir, "secrets.key")
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, sealbox.EncodeKey(bytes.Repeat([]byte{1}, sealbox.MasterKeySize)), 0o600); err != nil {
		t.Fatal(err)
	}
	before := snapDir(t, dir)
	var out bytes.Buffer
	err := keysCheck(context.Background(), &out, dir, "", "create-if-fresh", noEnv)
	if !errors.Is(err, keyguard.ErrConfig) {
		t.Fatalf("wrong key: %v (want an error that exits 78)\n%s", err, out.String())
	}
	sameDir(t, before, snapDir(t, dir))
	lines := keyLines(t, out.String())
	if lines["secrets.key"] != "FAIL" || lines["oauth.key"] != "ok" {
		t.Fatalf("lines: %v\n%s", lines, out.String())
	}
	if !strings.Contains(out.String(), "does not match this database") {
		t.Fatalf("no reason given:\n%s", out.String())
	}
}

func TestKeysCheckFailsOnAMissingKey(t *testing.T) {
	dir := installed(t)
	if err := os.Remove(filepath.Join(dir, "identity.key")); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := keysCheck(context.Background(), &out, dir, "", "create-if-fresh", noEnv); !errors.Is(err, keyguard.ErrConfig) {
		t.Fatalf("missing key: %v\n%s", err, out.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "identity.key")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("keys-check created a key")
	}
	if keyLines(t, out.String())["identity.key"] != "FAIL" {
		t.Fatalf("output:\n%s", out.String())
	}
}

func TestKeysCheckBeforeTheFirstStart(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	if err := keysCheck(context.Background(), &out, dir, "", "create-if-fresh", noEnv); err != nil {
		t.Fatalf("new install: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "not found: the first start creates a new one") {
		t.Fatalf("output:\n%s", out.String())
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("keys-check created %d files", len(entries))
	}
	// With -keys=require nothing would be created, so it fails.
	out.Reset()
	if err := keysCheck(context.Background(), &out, dir, "", "require", noEnv); !errors.Is(err, keyguard.ErrConfig) {
		t.Fatalf("require on a new install: %v", err)
	}
}

// A database from before key canaries (no key_canaries table) passes:
// every key is adopted on the next start. The check does not add the table.
func TestKeysCheckOnADatabaseWithoutCanaries(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "toolyard.db")
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE key_canaries`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM schema_migrations WHERE name = '0039_key_canaries.sql'`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"identity.key", "oauth.key", "secrets.key", "session.key"} {
		if _, err := sealbox.CreateKeyFile(filepath.Join(dir, n)); err != nil {
			t.Fatal(err)
		}
	}
	before := snapDir(t, dir)
	var out bytes.Buffer
	if err := keysCheck(context.Background(), &out, dir, "", "create-if-fresh", noEnv); err != nil {
		t.Fatalf("pre-canary database: %v\n%s", err, out.String())
	}
	sameDir(t, before, snapDir(t, dir))
	if strings.Count(out.String(), "adopted on the next start") != 4 {
		t.Fatalf("output:\n%s", out.String())
	}
}

func TestKeysCheckUsesTheCredentialsDirectory(t *testing.T) {
	dir := installed(t)
	creds := t.TempDir()
	for _, n := range []string{"identity.key", "oauth.key", "secrets.key", "session.key"} {
		if err := os.Rename(filepath.Join(dir, n), filepath.Join(creds, n)); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	if err := keysCheck(context.Background(), &out, dir, creds, "require", noEnv); err != nil {
		t.Fatalf("keys in the credentials dir: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), filepath.Join(creds, "oauth.key")) {
		t.Fatalf("output does not name the credentials dir:\n%s", out.String())
	}
}

func TestKeysCheckFlags(t *testing.T) {
	if err := runKeysCheck([]string{"-data", t.TempDir(), "extra"}); err == nil || !strings.Contains(err.Error(), "unexpected arguments") {
		t.Fatalf("stray argument: %v", err)
	}
	var out bytes.Buffer
	if err := keysCheck(context.Background(), &out, t.TempDir(), "", "sometimes", noEnv); !errors.Is(err, keyguard.ErrConfig) {
		t.Fatalf("bad -keys: %v", err)
	}
}
