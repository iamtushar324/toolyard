package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/mempalace"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

type stubMemPalace struct {
	calls     int
	lastEntry string
	err       error
	ok        bool
}

func (s *stubMemPalace) Ingest(_ context.Context, _ string, entry, _, _ string) (*mempalace.IngestResult, error) {
	s.calls++
	s.lastEntry = entry
	if s.err != nil {
		return nil, s.err
	}
	return &mempalace.IngestResult{OK: s.ok}, nil
}

func tempHooksDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "hooks.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestIngestRawNormalizesAndRedacts(t *testing.T) {
	svc := New(tempHooksDB(t), nil)
	body := []byte(`{
	  "source":"claude-code",
	  "hook_event_name":"UserPromptSubmit",
	  "session_id":"s1",
	  "text":"use sk-proj-abcdefghijklmnopqrstuvwxyz123456",
	  "payload":{"token":"ghp_abcdefghijklmnopqrstuvwxyzABCDE12345"}
	}`)
	ev, err := svc.IngestRaw(context.Background(), "ag_1", body, "")
	if err != nil {
		t.Fatalf("IngestRaw: %v", err)
	}
	if ev.Source != "claude_code" || ev.EventName != "UserPromptSubmit" || ev.SessionID != "s1" {
		t.Fatalf("unexpected normalized event: %+v", ev)
	}
	if strings.Contains(ev.Text, "sk-proj") {
		t.Fatalf("text was not redacted: %q", ev.Text)
	}
	if strings.Contains(string(ev.Payload), "ghp_") {
		t.Fatalf("payload was not redacted: %s", ev.Payload)
	}
}

func TestListFiltersAndSearch(t *testing.T) {
	svc := New(tempHooksDB(t), nil)
	ctx := context.Background()
	_, _ = svc.IngestRaw(ctx, "ag_1", []byte(`{"source":"codex","event_name":"Stop","session_id":"s1","text":"fixed billing"}`), "")
	_, _ = svc.IngestRaw(ctx, "ag_2", []byte(`{"source":"cursor","event_name":"afterMCPExecution","session_id":"s2","tool_name":"github.create_issue","text":"created issue"}`), "")

	rows, err := svc.List(ctx, Query{Source: "codex"})
	if err != nil || len(rows) != 1 || rows[0].AgentID != "ag_1" {
		t.Fatalf("source filter rows=%+v err=%v", rows, err)
	}
	rows, err = svc.List(ctx, Query{Q: "github"})
	if err != nil || len(rows) != 1 || rows[0].ToolName != "github.create_issue" {
		t.Fatalf("search rows=%+v err=%v", rows, err)
	}
	rows, err = svc.List(ctx, Query{SessionID: "s2"})
	if err != nil || len(rows) != 1 || rows[0].Source != "cursor" {
		t.Fatalf("session filter rows=%+v err=%v", rows, err)
	}
}

func TestMemoryIngestBestEffort(t *testing.T) {
	mp := &stubMemPalace{ok: true}
	svc := New(tempHooksDB(t), mp)
	ev, err := svc.IngestRaw(context.Background(), "ag_1", []byte(`{"source":"codex","event_name":"UserPromptSubmit","text":"remember this"}`), "")
	if err != nil {
		t.Fatalf("IngestRaw: %v", err)
	}
	if !ev.MemoryIngested || mp.calls != 1 {
		t.Fatalf("expected memory ingest, ev=%+v calls=%d", ev, mp.calls)
	}
	if !strings.Contains(mp.lastEntry, ev.ID) || !strings.Contains(mp.lastEntry, "remember this") {
		t.Fatalf("memory entry missing context: %q", mp.lastEntry)
	}

	mp.err = errors.New("palace unavailable")
	ev, err = svc.IngestRaw(context.Background(), "ag_1", []byte(`{"source":"codex","event_name":"Stop","text":"done"}`), "")
	if err != nil {
		t.Fatalf("best-effort ingest should still persist: %v", err)
	}
	if ev.MemoryIngested {
		t.Fatalf("memory_ingested should be false after failed mempalace call")
	}
}

func TestDirectClientPayloadStoresWholeBody(t *testing.T) {
	svc := New(tempHooksDB(t), nil)
	body := []byte(`{"hook_event_name":"PostToolUse","tool_name":"Bash","tool_response":{"stdout":"ok"}}`)
	ev, err := svc.IngestRaw(context.Background(), "ag_1", body, "claude_code")
	if err != nil {
		t.Fatalf("IngestRaw: %v", err)
	}
	if ev.Source != "claude_code" || ev.EventName != "PostToolUse" || ev.ToolName != "Bash" {
		t.Fatalf("unexpected direct event: %+v", ev)
	}
	var payload map[string]any
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		t.Fatalf("payload json: %v", err)
	}
	if payload["hook_event_name"] != "PostToolUse" {
		t.Fatalf("expected original payload, got %v", payload)
	}
}
