package gateway

import (
	"context"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/metrics"
	"github.com/tusharbhardwaj/toolyard/internal/store"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStageDispatchAuditDurability(t *testing.T) {
	for _, mode := range []string{"logical_failure", "intent_failure", "completion_failure"} {
		t.Run(mode, func(t *testing.T) {
			db, err := store.Open(filepath.Join(t.TempDir(), "audit.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			g := New(Options{Audit: audit.New(db)})
			calls := 0
			entry := toolEntry{tool: mcp.Tool{Name: "stage.write"}, handle: func(context.Context, map[string]any) (*mcp.CallToolResult, error) {
				calls++
				if mode == "logical_failure" {
					return mcp.NewToolResultError("fixture failure"), nil
				}
				return mcp.NewToolResultText("saved"), nil
			}}
			if mode != "logical_failure" {
				condition := "NEW.event_type = 'call.dispatch'"
				if mode == "completion_failure" {
					condition = "NEW.event_type = 'call.succeeded'"
				}
				if _, err = db.Exec(`CREATE TRIGGER fail_audit BEFORE INSERT ON audit_events WHEN ` + condition + ` BEGIN SELECT RAISE(ABORT,'test failure'); END`); err != nil {
					t.Fatal(err)
				}
			}
			res, err := g.dispatch(context.Background(), entry, map[string]any{}, "agent", "test audit", "", &metrics.Event{})
			if err != nil || !res.IsError {
				t.Fatalf("expected explicit error: %+v %v", res, err)
			}
			if mode == "intent_failure" && calls != 0 {
				t.Fatal("tool ran without durable intent")
			}
			if mode == "completion_failure" && (calls != 1 || !strings.Contains(textOf(res), "outcome unknown")) {
				t.Fatalf("unsafe completion result %s", textOf(res))
			}
			if mode == "logical_failure" {
				var n int
				db.QueryRow(`SELECT count(*) FROM audit_events WHERE event_type='call.succeeded'`).Scan(&n)
				if n != 0 {
					t.Fatal("logical failure recorded as success")
				}
			}
		})
	}
}

func TestStageWaitUsesOwnDeadline(t *testing.T) {
	f := newInboxFixture(t)
	f.gw.upstreamCallTimeout = time.Millisecond
	q := structured(t, f.call(t, f.agent, "inbox.ask", map[string]any{"schema_version": 2, "prompt": "Wait for a decision", "question": map[string]any{"type": "free_text"}}))
	if q["ok"] != true {
		t.Fatalf("submit: %+v", q)
	}
	start := time.Now()
	result := f.call(t, f.agent, "inbox.wait", map[string]any{"ids": []any{q["request_id"]}, "timeout_seconds": 1})
	if result.IsError || time.Since(start) < 800*time.Millisecond {
		t.Fatalf("coordination wait used dispatch timeout: %s", textOf(result))
	}
}
