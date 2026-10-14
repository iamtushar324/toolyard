package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// With MaxOpenConns(1), a transaction left open holds the only connection.
// The WAL checkpoint in Close used to wait for it forever; CloseContext
// must return by the time its context ends.
func TestCloseContextBoundedWithBusyConnection(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "busy.db"))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	start := time.Now()
	go func() {
		_ = db.CloseContext(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("CloseContext did not return while a connection was busy")
	}
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond {
		t.Fatalf("CloseContext returned after %s; it should have waited for the busy connection until its deadline", elapsed)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "twice.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}
