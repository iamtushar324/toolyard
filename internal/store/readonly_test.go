package store

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

func TestOpenReadOnlyChangesNothing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "toolyard.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE t (v TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO t(v) VALUES('kept')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	before := dirNames(t, dir)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatalf("open read-only: %v", err)
	}
	var v string
	if err := ro.QueryRow(`SELECT v FROM t`).Scan(&v); err != nil || v != "kept" {
		t.Fatalf("read: %q %v", v, err)
	}
	if _, err := ro.Exec(`INSERT INTO t(v) VALUES('written')`); err == nil {
		t.Fatal("a read-only connection accepted a write")
	}
	if _, err := ro.Exec(`CREATE TABLE u (v TEXT)`); err == nil {
		t.Fatal("a read-only connection accepted a schema change")
	}
	if err := ro.Close(); err != nil {
		t.Fatalf("close read-only: %v", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, after) {
		t.Fatal("the database file changed")
	}
	if got := dirNames(t, dir); len(got) != len(before) {
		t.Fatalf("files before %v, after %v: a read-only open created a file", before, got)
	}
}

func TestOpenReadOnlyNeverCreatesADatabase(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "toolyard.db")
	if _, err := OpenReadOnly(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing database: %v, want fs.ErrNotExist", err)
	}
	if names := dirNames(t, dir); len(names) != 0 {
		t.Fatalf("created %v", names)
	}
}

func TestOpenReadOnlySkipsMigrations(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "toolyard.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// Make the newest migration look unapplied: a read-only open must not
	// apply it again.
	if _, err := db.Exec(`DELETE FROM schema_migrations WHERE name = '0039_key_canaries.sql'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE key_canaries`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatalf("open read-only: %v", err)
	}
	defer ro.Close()
	var n int
	if err := ro.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'key_canaries'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("key_canaries after a read-only open: %d %v", n, err)
	}
}

func TestIdleDatabase(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "toolyard.db")
	if !idleDatabase(p) {
		t.Fatal("no sidecars: not idle")
	}
	if err := os.WriteFile(p+"-wal", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if !idleDatabase(p) {
		t.Fatal("empty -wal: not idle")
	}
	if err := os.WriteFile(p+"-wal", []byte("pending"), 0o600); err != nil {
		t.Fatal(err)
	}
	if idleDatabase(p) {
		t.Fatal("non-empty -wal: idle")
	}
	if err := os.WriteFile(p+"-wal", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p+"-shm", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if idleDatabase(p) {
		t.Fatal("-shm present: idle")
	}
}
