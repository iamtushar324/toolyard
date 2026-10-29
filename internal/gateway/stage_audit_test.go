package gateway

import (
	"context"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/codemode"
	"github.com/tusharbhardwaj/toolyard/internal/metrics"
	"github.com/tusharbhardwaj/toolyard/internal/store"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
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
	q := structured(t, f.call(t, f.agent, "inbox.ask", map[string]any{"schema_version": 2, "prompt": "Wait for a decision", "question": map[string]any{"type": "free_text"}}))
	if q["ok"] != true {
		t.Fatalf("submit: %+v", q)
	}
	// Only the wait must ignore the dispatch timeout; set it after the
	// submit, which a database round trip can take longer than 1 ms.
	f.gw.upstreamCallTimeout = time.Millisecond
	start := time.Now()
	result := f.call(t, f.agent, "inbox.wait", map[string]any{"ids": []any{q["request_id"]}, "timeout_seconds": 1})
	if result.IsError || time.Since(start) < 800*time.Millisecond {
		t.Fatalf("coordination wait used dispatch timeout: %s", textOf(result))
	}
}

// TestDispatchCompletionAuditMissing: when the completion row cannot be
// saved even after its retry, a read-only call still returns its real
// result with a warning, and a write stays outcome_unknown but says what
// the upstream call itself answered (TEC-1816).
func TestDispatchCompletionAuditMissing(t *testing.T) {
	readOnly := true
	cases := []struct {
		name     string
		readOnly bool
		upstream *mcp.CallToolResult
		want     []string
	}{
		{"read succeeded", true, mcp.NewToolResultText("the answer"), []string{"the answer"}},
		{"write succeeded", false, mcp.NewToolResultText("saved"), []string{"Tool outcome unknown: the upstream call succeeded, but its audit record was not saved.", `audit.mine dispatch_id="dispatch_`}},
		{"write returned an error", false, mcp.NewToolResultError("conflict"), []string{"the upstream call returned an error", "Do not repeat the write."}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db, err := store.Open(filepath.Join(t.TempDir(), "audit.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			// Every completion row fails, the retry's too.
			if _, err = db.Exec(`CREATE TRIGGER fail_audit BEFORE INSERT ON audit_events WHEN NEW.id LIKE '%.completed' BEGIN SELECT RAISE(ABORT,'test failure'); END`); err != nil {
				t.Fatal(err)
			}
			g := New(Options{Audit: audit.New(db)})
			tool := mcp.Tool{Name: "stage.tool"}
			if c.readOnly {
				tool.Annotations.ReadOnlyHint = &readOnly
			}
			upstream := c.upstream
			entry := toolEntry{tool: tool, handle: func(context.Context, map[string]any) (*mcp.CallToolResult, error) {
				return upstream, nil
			}}
			ev := &metrics.Event{}
			res, err := g.dispatch(context.Background(), entry, map[string]any{}, "agent", "test audit", "", ev)
			if err != nil || res == nil {
				t.Fatalf("dispatch: %+v %v", res, err)
			}
			text := textOf(res)
			for _, w := range c.want {
				if !strings.Contains(text, w) {
					t.Fatalf("result lacks %q:\n%s", w, text)
				}
			}
			id, _ := res.Meta.AdditionalFields[DispatchIDMarker].(string)
			if !strings.HasPrefix(id, "dispatch_") {
				t.Fatalf("no dispatch id in meta: %v", res.Meta.AdditionalFields)
			}
			unknown, _ := res.Meta.AdditionalFields[codemode.OutcomeUnknownMarker].(bool)
			if c.readOnly {
				// The content is the upstream's, unchanged; the warning is
				// only in the metadata.
				warning, _ := res.Meta.AdditionalFields[WarningMarker].(string)
				if text != "the answer" || len(res.Content) != 1 || !strings.Contains(warning, "audit record for dispatch "+id+" was not saved") {
					t.Fatalf("read-only content %q, warning %q", text, warning)
				}
				if unknown || res.IsError || ev.ErrorClass == "audit_outcome_unknown" || res.Meta.AdditionalFields[AuditMissingMarker] != true {
					t.Fatalf("read-only result = %+v meta %v class %q", res, res.Meta.AdditionalFields, ev.ErrorClass)
				}
				if len(upstream.Content) != 1 || upstream.Meta != nil {
					t.Fatalf("the handler's own result was changed: %+v", upstream)
				}
			} else if !unknown || !res.IsError || ev.ErrorClass != "audit_outcome_unknown" || !strings.Contains(text, id) {
				t.Fatalf("write result = %+v meta %v class %q", res, res.Meta.AdditionalFields, ev.ErrorClass)
			}
			// The intent row is there; the dispatch can be looked up.
			rows, err := audit.New(db).Query(context.Background(), audit.Filter{DispatchID: id})
			if err != nil || len(rows) != 1 || rows[0].EventType != "call.dispatch" {
				t.Fatalf("rows of %s = %+v %v", id, rows, err)
			}
		})
	}
}

// TestDispatchCompletionAuditRetry: a completion row the store refuses
// once is saved by the retry, and the call answers normally.
func TestDispatchCompletionAuditRetry(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "audit.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Refuse the full summary; the retry's marker passes.
	if _, err = db.Exec(`CREATE TRIGGER fail_audit BEFORE INSERT ON audit_events WHEN NEW.result_summary = 'saved' BEGIN SELECT RAISE(ABORT,'test failure'); END`); err != nil {
		t.Fatal(err)
	}
	g := New(Options{Audit: audit.New(db)})
	entry := toolEntry{tool: mcp.Tool{Name: "stage.write"}, handle: func(context.Context, map[string]any) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("saved"), nil
	}}
	res, err := g.dispatch(context.Background(), entry, map[string]any{}, "agent", "test audit", "", &metrics.Event{})
	if err != nil || res.IsError || textOf(res) != "saved" {
		t.Fatalf("retry result = %+v %v", res, err)
	}
	var n int
	db.QueryRow(`SELECT count(*) FROM audit_events WHERE event_type='call.succeeded' AND result_summary='(summary not saved)'`).Scan(&n)
	if n != 1 {
		t.Fatalf("retry rows = %d", n)
	}
}

func TestSummariseResultIsSafeForPostgres(t *testing.T) {
	// A three-byte rune that starts at byte 199 is cut whole, not split.
	text := strings.Repeat("a", 199) + "€" + strings.Repeat("b", 50)
	got := summariseResult(mcp.NewToolResultText(text))
	if !utf8.ValidString(got) || !strings.HasPrefix(got, strings.Repeat("a", 199)) || strings.Contains(got, "€") || !strings.HasSuffix(got, "…") {
		t.Fatalf("multi-byte cut = %q", got)
	}
	// NUL bytes and invalid UTF-8 are dropped.
	got = summariseResult(mcp.NewToolResultText("a\x00b\xffc"))
	if got != "abc" {
		t.Fatalf("NUL and invalid bytes = %q", got)
	}
	// Thousands of blocks make a bounded summary that counts the rest.
	res := &mcp.CallToolResult{}
	for i := 0; i < 5000; i++ {
		res.Content = append(res.Content, mcp.NewTextContent(strings.Repeat("x", 300)))
	}
	got = summariseResult(res)
	if len(got) > summaryMaxBytes+64 || !utf8.ValidString(got) || !strings.Contains(got, " blocks)") {
		t.Fatalf("5000 blocks: %d bytes, tail %q", len(got), got[max(0, len(got)-40):])
	}
	// An error result keeps its prefix.
	if got := summariseResult(mcp.NewToolResultError("boom")); got != "ERROR: boom" {
		t.Fatalf("error = %q", got)
	}
}
