package store_test

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/store"
	"github.com/tusharbhardwaj/toolyard/internal/store/storetest"
)

// Processes that create a server key at once all end with the stored one.
func TestLoadOrCreateServerKeyRace(t *testing.T) {
	db := storetest.Open(t)
	ctx := context.Background()
	const n = 8
	var wg sync.WaitGroup
	privs := make([][]byte, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			privs[i], _, errs[i] = store.LoadOrCreateServerKey(ctx, db, "test_key", func() ([]byte, []byte, error) {
				return []byte(fmt.Sprintf("priv-%d", i)), []byte(fmt.Sprintf("pub-%d", i)), nil
			})
		}()
	}
	wg.Wait()
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if !bytes.Equal(privs[i], privs[0]) {
			t.Fatalf("caller %d got %q, caller 0 %q: two keys", i, privs[i], privs[0])
		}
	}
	var rows int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM server_keys WHERE purpose = 'test_key'`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("rows %d %v", rows, err)
	}
	// Later calls read it and never call gen.
	priv, _, err := store.LoadOrCreateServerKey(ctx, db, "test_key", func() ([]byte, []byte, error) {
		t.Fatal("gen called for an existing key")
		return nil, nil, nil
	})
	if err != nil || !bytes.Equal(priv, privs[0]) {
		t.Fatalf("reload: %q %v", priv, err)
	}
}
