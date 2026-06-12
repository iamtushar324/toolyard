package audit

import (
	"bytes"
	"context"
	"encoding/csv"
	"path/filepath"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/store"
)

func newTestAudit(t *testing.T) *Logger {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return New(db)
}

func TestQueryFilterNarrowing(t *testing.T) {
	l := newTestAudit(t)
	ctx := context.Background()
	_ = l.Write(ctx, Event{EventType: "call.allowed", AgentID: "a1", ToolName: "github.x", Decision: "allow"})
	_ = l.Write(ctx, Event{EventType: "call.denied", AgentID: "a2", ToolName: "fs.y", Decision: "deny"})
	_ = l.Write(ctx, Event{EventType: "call.allowed", AgentID: "a2", ToolName: "github.z", Decision: "allow"})

	if got, _ := l.Query(ctx, Filter{AgentID: "a2"}); len(got) != 2 {
		t.Errorf("agent filter: got %d, want 2", len(got))
	}
	if got, _ := l.Query(ctx, Filter{EventType: "call.denied"}); len(got) != 1 {
		t.Errorf("event_type filter: got %d, want 1", len(got))
	}
	if got, _ := l.Query(ctx, Filter{Decision: "allow"}); len(got) != 2 {
		t.Errorf("decision filter: got %d, want 2", len(got))
	}
	if got, _ := l.Query(ctx, Filter{AgentID: "a2", EventType: "call.allowed"}); len(got) != 1 {
		t.Errorf("combined filter: got %d, want 1", len(got))
	}
}

// TestCSVEscaping confirms reasons with commas/newlines survive a CSV
// round-trip (encoding/csv quotes them; a reader recovers the original).
func TestCSVEscaping(t *testing.T) {
	l := newTestAudit(t)
	ctx := context.Background()
	tricky := "deleted repo, then\nrolled \"back\""
	_ = l.Write(ctx, Event{EventType: "call.allowed", ToolName: "t", Reason: tricky})

	rows, _ := l.Query(ctx, Filter{})
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	var buf bytes.Buffer
	cw := csv.NewWriter(&buf)
	_ = cw.Write([]string{"reason"})
	_ = cw.Write([]string{rows[0].Reason})
	cw.Flush()

	rr := csv.NewReader(bytes.NewReader(buf.Bytes()))
	recs, err := rr.ReadAll()
	if err != nil {
		t.Fatalf("csv read: %v", err)
	}
	if len(recs) != 2 || recs[1][0] != tricky {
		t.Fatalf("round-trip reason = %q, want %q", recs[1][0], tricky)
	}
}
