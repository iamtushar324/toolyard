package mempalace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// stubGateway pretends to be the gateway for tests. has tracks which tools
// are "registered"; the last call's args are stashed so assertions can
// inspect them.
type stubGateway struct {
	has        map[string]bool
	lastTool   string
	lastArgs   map[string]any
	returnErr  error
	returnText string
	isError    bool
}

func (g *stubGateway) HasTool(name string) bool { return g.has[name] }

func (g *stubGateway) CallInternal(_ context.Context, _, name string, args map[string]any) (*mcp.CallToolResult, error) {
	g.lastTool = name
	g.lastArgs = args
	if g.returnErr != nil {
		return nil, g.returnErr
	}
	res := mcp.NewToolResultText(g.returnText)
	res.IsError = g.isError
	return res, nil
}

func tempDB(t *testing.T) *store.DB {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestParseMode(t *testing.T) {
	cases := map[string]Mode{
		"":         ModeAuto,
		"auto":     ModeAuto,
		"on":       ModeOn,
		"required": ModeOn,
		"off":      ModeOff,
		"0":        ModeOff,
		"bogus":    ModeAuto,
	}
	for in, want := range cases {
		if got := ParseMode(in); got != want {
			t.Errorf("ParseMode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEnsureInitializedCreatesPalaceDir(t *testing.T) {
	dir := t.TempDir()
	svc := New(nil, nil, nil, dir, ModeAuto)
	if err := svc.EnsureInitialized(context.Background()); err != nil {
		t.Fatalf("EnsureInitialized: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, "palace"))
	if err != nil {
		t.Fatalf("palace dir not created: %v", err)
	}
	if !info.IsDir() {
		t.Errorf("palace path is not a directory")
	}
}

func TestEnsureInitializedOffIsNoOp(t *testing.T) {
	dir := t.TempDir()
	svc := New(nil, nil, nil, dir, ModeOff)
	if err := svc.EnsureInitialized(context.Background()); err != nil {
		t.Fatalf("EnsureInitialized: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "palace")); !os.IsNotExist(err) {
		t.Errorf("ModeOff should leave palace dir absent, got err=%v", err)
	}
}

func TestEnsureInstalledShortCircuitsWhenBinaryFound(t *testing.T) {
	// Point at an absolute-path binary that exists and is executable.
	// /bin/sh is universally available on darwin/linux.
	t.Setenv("TOOLYARD_MEMPALACE_BIN", "/bin/sh")
	svc := New(nil, nil, nil, t.TempDir(), ModeAuto)
	if err := svc.EnsureInstalled(context.Background()); err != nil {
		t.Fatalf("EnsureInstalled: %v", err)
	}
	if !svc.installed {
		t.Errorf("expected installed=true after short-circuit")
	}
}

func TestIngestRejectsEmptyEntry(t *testing.T) {
	gw := &stubGateway{has: map[string]bool{DiaryWriteTool: true}}
	svc := New(tempDB(t), gw, audit.New(tempDB(t)), t.TempDir(), ModeAuto)
	_, err := svc.Ingest(context.Background(), "agent-1", "  ", "", "")
	if !errors.Is(err, ErrEntryRequired) {
		t.Errorf("want ErrEntryRequired, got %v", err)
	}
	if gw.lastTool != "" {
		t.Errorf("gateway should not have been called: %q", gw.lastTool)
	}
}

func TestIngestRejectsWhenDisabled(t *testing.T) {
	gw := &stubGateway{has: map[string]bool{DiaryWriteTool: true}}
	svc := New(tempDB(t), gw, nil, t.TempDir(), ModeOff)
	_, err := svc.Ingest(context.Background(), "agent-1", "hi", "", "")
	if !errors.Is(err, ErrDisabled) {
		t.Errorf("want ErrDisabled, got %v", err)
	}
}

func TestIngestRejectsWhenNotReady(t *testing.T) {
	gw := &stubGateway{has: map[string]bool{}} // diary_write not registered
	svc := New(tempDB(t), gw, nil, t.TempDir(), ModeAuto)
	_, err := svc.Ingest(context.Background(), "agent-1", "hi", "", "")
	if !errors.Is(err, ErrNotReady) {
		t.Errorf("want ErrNotReady, got %v", err)
	}
}

func TestIngestPassesAgentNameAndUpdatesIndex(t *testing.T) {
	db := tempDB(t)
	gw := &stubGateway{
		has:        map[string]bool{DiaryWriteTool: true},
		returnText: "ok",
	}
	svc := New(db, gw, nil, t.TempDir(), ModeAuto)
	res, err := svc.Ingest(context.Background(), "agent-42", "we picked GraphQL", "architecture", "decisions")
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if !res.OK {
		t.Errorf("expected OK=true, got %+v", res)
	}
	if gw.lastTool != DiaryWriteTool {
		t.Errorf("wrong tool dispatched: %q", gw.lastTool)
	}
	if gw.lastArgs["agent_name"] != "agent-42" {
		t.Errorf("agent_name not forwarded: %v", gw.lastArgs)
	}
	if gw.lastArgs["entry"] != "we picked GraphQL" {
		t.Errorf("entry not forwarded: %v", gw.lastArgs["entry"])
	}
	if gw.lastArgs["topic"] != "architecture" {
		t.Errorf("topic not forwarded: %v", gw.lastArgs["topic"])
	}
	if gw.lastArgs["wing"] != "decisions" {
		t.Errorf("wing not forwarded: %v", gw.lastArgs["wing"])
	}

	// Index row should exist.
	var count int64
	if err := db.QueryRowContext(context.Background(),
		`SELECT entry_count FROM mempalace_agents WHERE agent_id = 'agent-42'`).Scan(&count); err != nil {
		t.Fatalf("index lookup: %v", err)
	}
	if count != 1 {
		t.Errorf("entry_count = %d, want 1", count)
	}

	// Second call should bump the counter rather than insert.
	if _, err := svc.Ingest(context.Background(), "agent-42", "another", "", ""); err != nil {
		t.Fatalf("second Ingest: %v", err)
	}
	if err := db.QueryRowContext(context.Background(),
		`SELECT entry_count FROM mempalace_agents WHERE agent_id = 'agent-42'`).Scan(&count); err != nil {
		t.Fatalf("index lookup 2: %v", err)
	}
	if count != 2 {
		t.Errorf("entry_count = %d, want 2", count)
	}
}

func TestIngestPropagatesUpstreamError(t *testing.T) {
	gw := &stubGateway{
		has:        map[string]bool{DiaryWriteTool: true},
		isError:    true,
		returnText: "palace closed",
	}
	svc := New(tempDB(t), gw, nil, t.TempDir(), ModeAuto)
	res, err := svc.Ingest(context.Background(), "a", "hi", "", "")
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	if res.OK {
		t.Errorf("expected OK=false on upstream IsError")
	}
	if res.Detail != "palace closed" {
		t.Errorf("detail = %q", res.Detail)
	}
}

func TestSnapshotReportsBinary(t *testing.T) {
	t.Setenv("TOOLYARD_MEMPALACE_BIN", "/bin/sh")
	gw := &stubGateway{has: map[string]bool{DiaryWriteTool: true}}
	svc := New(tempDB(t), gw, nil, t.TempDir(), ModeAuto)
	st := svc.Snapshot(context.Background())
	if !st.Enabled {
		t.Errorf("expected enabled")
	}
	if !st.Installed {
		t.Errorf("expected installed (TOOLYARD_MEMPALACE_BIN=/bin/sh)")
	}
	if !st.Available {
		t.Errorf("expected available (DiaryWriteTool registered)")
	}
	if st.Binary != "/bin/sh" {
		t.Errorf("binary = %q", st.Binary)
	}
}

func TestTopAgentsOrdersByEntryCount(t *testing.T) {
	db := tempDB(t)
	gw := &stubGateway{has: map[string]bool{DiaryWriteTool: true}, returnText: "ok"}
	svc := New(db, gw, nil, t.TempDir(), ModeAuto)
	ctx := context.Background()
	_, _ = svc.Ingest(ctx, "alice", "x", "", "")
	_, _ = svc.Ingest(ctx, "alice", "x", "", "")
	_, _ = svc.Ingest(ctx, "alice", "x", "", "")
	_, _ = svc.Ingest(ctx, "bob", "x", "", "")
	rows, err := svc.TopAgents(ctx, 5)
	if err != nil {
		t.Fatalf("TopAgents: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	if rows[0].AgentID != "alice" || rows[0].EntryCount != 3 {
		t.Errorf("first row %+v", rows[0])
	}
	if rows[1].AgentID != "bob" || rows[1].EntryCount != 1 {
		t.Errorf("second row %+v", rows[1])
	}
}
