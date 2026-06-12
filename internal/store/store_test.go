package store

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCloseCheckpointsWAL verifies that Close folds the WAL sidecar back into
// the main database file so a plain file-copy backup is complete without the
// -wal companion, and that the data is still readable after a reopen.
func TestCloseCheckpointsWAL(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "toolyard.db")
	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}

	if _, err := db.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	for i := 0; i < 100; i++ {
		if _, err := db.Exec(`INSERT INTO t(v) VALUES(?)`, "row"); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// After a TRUNCATE checkpoint the -wal file is either absent or zero-length.
	walPath := dbPath + "-wal"
	if fi, err := os.Stat(walPath); err == nil {
		if fi.Size() != 0 {
			t.Fatalf("wal file %s is %d bytes; want absent or zero-length", walPath, fi.Size())
		}
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat wal: %v", err)
	}

	// Reopen and confirm the rows survived (i.e. were checkpointed, not lost).
	db2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	defer db2.Close()

	var n int
	if err := db2.QueryRow(`SELECT COUNT(*) FROM t`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 100 {
		t.Fatalf("got %d rows after reopen; want 100", n)
	}
}
