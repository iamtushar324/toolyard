package memory

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/store"
)

func newTestMemory(t *testing.T) *Service {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return New(db)
}

func TestExportImportRoundTrip(t *testing.T) {
	s := newTestMemory(t)
	ctx := context.Background()
	if _, err := s.Set(ctx, "global", "a", "1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Set(ctx, "proj", "b", "two"); err != nil {
		t.Fatal(err)
	}

	dump, err := s.ExportAll(ctx)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if len(dump) != 2 {
		t.Fatalf("export len = %d, want 2", len(dump))
	}

	// Fresh store, import the dump → identical content.
	s2 := newTestMemory(t)
	n, err := s2.Import(ctx, dump, false)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if n != 2 {
		t.Errorf("imported %d, want 2", n)
	}
	got, _ := s2.ExportAll(ctx)
	if len(got) != 2 {
		t.Fatalf("after import len = %d, want 2", len(got))
	}
}

func TestImportMergeVsReplace(t *testing.T) {
	s := newTestMemory(t)
	ctx := context.Background()
	if _, err := s.Set(ctx, "global", "keep", "old"); err != nil {
		t.Fatal(err)
	}

	// Merge: existing "keep" stays, "new" added, "keep" value updated.
	if _, err := s.Import(ctx, []Entry{{Scope: "global", Key: "keep", Value: "updated"}, {Scope: "global", Key: "new", Value: "x"}}, false); err != nil {
		t.Fatal(err)
	}
	all, _ := s.ExportAll(ctx)
	if len(all) != 2 {
		t.Fatalf("after merge len = %d, want 2", len(all))
	}
	if e, _ := s.Get(ctx, "global", "keep"); e.Value != "updated" {
		t.Errorf("merged value = %q, want updated", e.Value)
	}

	// Replace: only the imported set survives.
	if _, err := s.Import(ctx, []Entry{{Scope: "global", Key: "only", Value: "z"}}, true); err != nil {
		t.Fatal(err)
	}
	all, _ = s.ExportAll(ctx)
	if len(all) != 1 || all[0].Key != "only" {
		t.Fatalf("after replace = %+v, want only [only]", all)
	}
}
