package notes

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// stubGateway records the last CallInternal target/args and returns a
// configurable response. has controls which tools "exist".
type stubGateway struct {
	has        map[string]bool
	calls      []call
	returnErr  error
	returnText string
	isError    bool
}

type call struct {
	target string
	args   map[string]any
}

func (g *stubGateway) HasTool(name string) bool { return g.has[name] }

func (g *stubGateway) CallInternal(_ context.Context, _, target string, args map[string]any) (*mcp.CallToolResult, error) {
	g.calls = append(g.calls, call{target: target, args: args})
	if g.returnErr != nil {
		return nil, g.returnErr
	}
	res := mcp.NewToolResultText(g.returnText)
	res.IsError = g.isError
	return res, nil
}

func tempDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func newSvc(t *testing.T, dir string, gw Dispatcher) *Service {
	t.Helper()
	return New(Options{
		DB:        tempDB(t),
		Gateway:   gw,
		NotesDir:  dir,
		Interval:  -1, // scanner off in tests; we call Sync directly
		WriteTool: "notes.write_file",
		DiaryTool: "mempalace.mempalace_diary_write",
	})
}

func TestNewReturnsNilWhenDirEmpty(t *testing.T) {
	if New(Options{NotesDir: "  "}) != nil {
		t.Errorf("expected nil for empty dir")
	}
}

func TestSafeJoinRejectsEscape(t *testing.T) {
	dir := t.TempDir()
	s := newSvc(t, dir, &stubGateway{})
	cases := []string{"../etc/passwd", "/etc/passwd", "a/../../escape", ""}
	for _, c := range cases {
		if _, _, err := s.safeJoin(c); err == nil {
			t.Errorf("safeJoin(%q) should fail", c)
		}
	}
}

func TestSafeJoinAcceptsRelative(t *testing.T) {
	dir := t.TempDir()
	s := newSvc(t, dir, &stubGateway{})
	abs, rel, err := s.safeJoin("decisions/graphql.md")
	if err != nil {
		t.Fatalf("safeJoin: %v", err)
	}
	if rel != filepath.Join("decisions", "graphql.md") {
		t.Errorf("rel = %q", rel)
	}
	// abs should resolve under the (symlink-resolved) notes dir.
	if !filepath.IsAbs(abs) || filepath.Base(abs) != "graphql.md" {
		t.Errorf("abs = %q", abs)
	}
	wantTail := filepath.Join("decisions", "graphql.md")
	if !strings.HasSuffix(abs, wantTail) {
		t.Errorf("abs = %q does not end with %q", abs, wantTail)
	}
}

func TestPublishWritesFileAndIngests(t *testing.T) {
	dir := t.TempDir()
	gw := &stubGateway{
		has:        map[string]bool{"notes.write_file": true, "mempalace.mempalace_diary_write": true},
		returnText: `{"success":true,"entry_id":"diary_abc123"}`,
	}
	s := newSvc(t, dir, gw)
	ctx := context.Background()
	res, err := s.Publish(ctx, "agent-1", "decisions/graphql.md", "# graphql\nbecause REST chatter", "architecture")
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if !res.Indexed || res.PalaceID != "diary_abc123" {
		t.Errorf("expected indexed with palace_id, got %+v", res)
	}
	if res.Path != filepath.Join("decisions", "graphql.md") {
		t.Errorf("path = %q", res.Path)
	}
	// File written by stub gateway's write_file? We routed through
	// notes.write_file (stub), so the disk file isn't actually created
	// by the stub. Instead verify the call sequence.
	if len(gw.calls) != 2 {
		t.Fatalf("expected 2 CallInternal calls, got %d", len(gw.calls))
	}
	if gw.calls[0].target != "notes.write_file" || gw.calls[1].target != "mempalace.mempalace_diary_write" {
		t.Errorf("wrong call order: %+v", gw.calls)
	}
	if gw.calls[1].args["wing"] != "notes" || gw.calls[1].args["topic"] != "architecture" {
		t.Errorf("ingest args wrong: %v", gw.calls[1].args)
	}
	if gw.calls[1].args["agent_name"] != "agent-1" {
		t.Errorf("agent_name not forwarded: %v", gw.calls[1].args)
	}
}

func TestPublishFallsBackToDirectWriteWhenUpstreamMissing(t *testing.T) {
	dir := t.TempDir()
	gw := &stubGateway{
		has:        map[string]bool{"mempalace.mempalace_diary_write": true}, // no notes.write_file
		returnText: `{"entry_id":"x"}`,
	}
	s := newSvc(t, dir, gw)
	_, err := s.Publish(context.Background(), "a", "hello.md", "# hello\n", "")
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	// File should have been written directly to disk.
	body, err := os.ReadFile(filepath.Join(dir, "hello.md"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(body) != "# hello\n" {
		t.Errorf("body = %q", string(body))
	}
}

func TestPublishRejectsEmptyContent(t *testing.T) {
	dir := t.TempDir()
	gw := &stubGateway{has: map[string]bool{"notes.write_file": true, "mempalace.mempalace_diary_write": true}}
	s := newSvc(t, dir, gw)
	if _, err := s.Publish(context.Background(), "", "x.md", "  ", ""); !errors.Is(err, ErrEmptyContent) {
		t.Errorf("want ErrEmptyContent, got %v", err)
	}
}

func TestSyncIngestsOnlyChangedFiles(t *testing.T) {
	dir := t.TempDir()
	// Three notes on disk, two of them in subdirs.
	mustWrite := func(rel, body string) {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("a.md", "alpha")
	mustWrite("decisions/b.md", "bravo")
	mustWrite("decisions/c.md", "charlie")
	// A non-markdown file should be skipped.
	mustWrite("ignore.txt", "skip me")
	// A hidden file should be skipped.
	mustWrite(".secret.md", "ignored")
	// A hidden dir should be skipped wholesale.
	mustWrite(".git/objects.md", "ignored")

	gw := &stubGateway{
		has:        map[string]bool{"mempalace.mempalace_diary_write": true},
		returnText: `{"entry_id":"e"}`,
	}
	s := newSvc(t, dir, gw)
	ctx := context.Background()
	n, err := s.Sync(ctx)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if n != 3 {
		t.Errorf("first Sync ingested %d, want 3", n)
	}
	// Topic for subdir files should default to the dir name.
	var sawDecisionsTopic bool
	for _, c := range gw.calls {
		if c.args["topic"] == "decisions" {
			sawDecisionsTopic = true
		}
	}
	if !sawDecisionsTopic {
		t.Errorf("expected at least one call with topic=decisions: %+v", gw.calls)
	}

	// Second sync — nothing changed → 0 ingests.
	gw.calls = nil
	n, err = s.Sync(ctx)
	if err != nil {
		t.Fatalf("Sync 2: %v", err)
	}
	if n != 0 {
		t.Errorf("second Sync ingested %d, want 0", n)
	}

	// Bump mtime of one file; expect exactly 1 ingest.
	bumpTime := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "a.md"), bumpTime, bumpTime); err != nil {
		t.Fatal(err)
	}
	n, err = s.Sync(ctx)
	if err != nil {
		t.Fatalf("Sync 3: %v", err)
	}
	if n != 1 {
		t.Errorf("third Sync ingested %d, want 1", n)
	}
}

func TestSyncShortCircuitsWhenDiaryToolMissing(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	gw := &stubGateway{has: map[string]bool{}} // no mempalace tool
	s := newSvc(t, dir, gw)
	n, err := s.Sync(context.Background())
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if n != 0 {
		t.Errorf("ingested %d when diary tool missing; want 0", n)
	}
}

func TestSnapshotReportsTrackedCount(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.md"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	gw := &stubGateway{
		has:        map[string]bool{"mempalace.mempalace_diary_write": true},
		returnText: `{"entry_id":"e"}`,
	}
	s := newSvc(t, dir, gw)
	if _, err := s.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	st := s.Snapshot(context.Background())
	if !st.Enabled || st.Tracked != 1 {
		t.Errorf("snapshot = %+v", st)
	}
}

func TestNilServiceIsSafe(t *testing.T) {
	var s *Service
	if s.Enabled() {
		t.Errorf("nil should not be enabled")
	}
	if dir := s.NotesDir(); dir != "" {
		t.Errorf("nil NotesDir = %q", dir)
	}
	st := s.Snapshot(context.Background())
	if st.Enabled {
		t.Errorf("nil Snapshot.Enabled = true")
	}
}
