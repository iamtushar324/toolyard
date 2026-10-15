package memory

import (
	"context"
	"strings"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/store/storetest"
)

func newTestMemory(t *testing.T) *Service {
	t.Helper()
	return New(storetest.Open(t))
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

// TestListPrefixIsCaseInsensitive pins List's prefix match: case-insensitive
// (SQLite's LIKE always was; Postgres uses ILIKE), scoped, and a backslash in
// the prefix is a plain character on both engines.
func TestListPrefixIsCaseInsensitive(t *testing.T) {
	s := newTestMemory(t)
	ctx := context.Background()
	for _, kv := range [][3]string{
		{"global", "Project/Alpha", "1"},
		{"global", "project/beta", "2"},
		{"global", "PROJECT/gamma", "3"},
		{"global", "other", "4"},
		{"team", "project/delta", "5"},
		{"global", `dir\sub`, "6"},
		{"global", "dirXsub", "7"},
	} {
		if _, err := s.Set(ctx, kv[0], kv[1], kv[2]); err != nil {
			t.Fatal(err)
		}
	}
	keys := func(scope, prefix string) string {
		t.Helper()
		got, err := s.List(ctx, scope, prefix)
		if err != nil {
			t.Fatalf("List(%q, %q): %v", scope, prefix, err)
		}
		var out []string
		for _, e := range got {
			out = append(out, e.Key)
		}
		return strings.Join(out, ",")
	}
	// ORDER BY key is byte order on both engines (SQLite BINARY, Postgres C).
	if got, want := keys("global", "project/"), "PROJECT/gamma,Project/Alpha,project/beta"; got != want {
		t.Fatalf("List(global, project/) = %q, want %q", got, want)
	}
	if got, want := keys("global", "PrOjEcT/A"), "Project/Alpha"; got != want {
		t.Fatalf("List(global, PrOjEcT/A) = %q, want %q", got, want)
	}
	if got, want := keys("team", "PROJECT"), "project/delta"; got != want {
		t.Fatalf("List(team, PROJECT) = %q, want %q", got, want)
	}
	if got, want := keys("global", `dir\`), `dir\sub`; got != want {
		t.Fatalf("List(global, dir\\) = %q, want %q", got, want)
	}
}
