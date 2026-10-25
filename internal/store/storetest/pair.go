package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// OpenPostgresPair returns two pools on one fresh, migrated Postgres test
// database, as two gateway replicas would open it, and closes both when
// the test ends. It skips the test unless TOOLYARD_TEST_DB=postgres.
func OpenPostgresPair(t testing.TB) (*store.DB, *store.DB) {
	t.Helper()
	SkipUnlessPostgres(t)
	url := NewPostgresDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	open := func() *store.DB {
		db, err := store.OpenPostgres(ctx, url, testMaxConns)
		if err != nil {
			t.Fatalf("storetest: open a pool on the test database: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return db
	}
	return open(), open()
}
