package policy

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/store"
	"github.com/tusharbhardwaj/toolyard/internal/store/storetest"
)

// openRulesDB opens a migrated test database.
func openRulesDB(t *testing.T) *store.DB {
	t.Helper()
	db := storetest.Open(t)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// hideRules makes the tool_policies table unreadable by renaming it away.
func hideRules(t *testing.T, db *store.DB) {
	t.Helper()
	if _, err := db.Exec(`ALTER TABLE tool_policies RENAME TO tool_policies_hidden`); err != nil {
		t.Fatal(err)
	}
}

// restoreRules puts the table hideRules renamed back in place.
func restoreRules(t *testing.T, db *store.DB) {
	t.Helper()
	if _, err := db.Exec(`ALTER TABLE tool_policies_hidden RENAME TO tool_policies`); err != nil {
		t.Fatal(err)
	}
}

func TestLoadReturnsErrorWhenRulesUnreadable(t *testing.T) {
	ctx := context.Background()
	db := openRulesDB(t)
	hideRules(t, db)
	e, err := Load(ctx, db)
	if err == nil {
		t.Fatal("Load with an unreadable rules table returned no error")
	}
	if e != nil {
		t.Fatal("Load returned an engine together with its error")
	}

	restoreRules(t, db)
	e, err = Load(ctx, db)
	if err != nil {
		t.Fatalf("Load with a readable rules table: %v", err)
	}
	if err := e.Ready(); err != nil {
		t.Fatalf("Ready after a successful Load = %v", err)
	}

	if _, err := Load(ctx, nil); !errors.Is(err, ErrNoDB) {
		t.Fatalf("Load(nil) = %v, want ErrNoDB", err)
	}
}

// TestNewFailsClosedWhenRulesUnreadable: an engine whose first load failed
// knows none of the explicit rules, so it denies every call instead of
// letting the name heuristic allow read-shaped tools a rule may deny.
func TestNewFailsClosedWhenRulesUnreadable(t *testing.T) {
	ctx := context.Background()
	db := openRulesDB(t)
	hideRules(t, db)
	e := New(db)
	if e.Ready() == nil {
		t.Fatal("Ready() = nil after a failed first load")
	}
	for _, req := range []Request{
		{UpstreamName: "github", ToolName: "github.list_issues"},
		{UpstreamName: "github", ToolName: "github.create_issue"},
		{UpstreamName: "tools", ToolName: "search"},
		{UpstreamName: "tools", ToolName: "executeToolCode"},
	} {
		if d := e.Eval(req); d.Action != ActionDeny || d.RuleID != RuleUnavailable {
			t.Errorf("Eval(%s on %s) = %+v, want the %s deny", req.ToolName, req.UpstreamName, d, RuleUnavailable)
		}
	}
	if d, ok := e.ExplicitDeny("github.list_issues", "github"); !ok || d.RuleID != RuleUnavailable {
		t.Errorf("ExplicitDeny while unavailable = %+v %v, want the %s deny", d, ok, RuleUnavailable)
	}
	sim := e.WithChange(ScopeTool, "github.list_issues", "allow")
	if d := sim.Eval(Request{UpstreamName: "github", ToolName: "github.list_issues"}); d.Action != ActionDeny || d.RuleID != RuleUnavailable {
		t.Errorf("what-if copy of an unavailable engine = %+v, want the %s deny", d, RuleUnavailable)
	}

	// Writes fail while the table is unreadable, and a guarded Change does
	// not reach its guard.
	if _, err := e.Set(ctx, ScopeTool, "github.create_issue", "ask", "", false); err == nil {
		t.Fatal("Set with an unreadable rules table returned no error")
	}
	guardCalled := false
	if _, _, err := e.Change(ctx, ScopeTool, "github.create_issue", "ask", "", false, func(before, after *Engine) error {
		guardCalled = true
		return nil
	}); err == nil || guardCalled {
		t.Fatalf("Change while unavailable: err=%v guardCalled=%v, want an error before the guard", err, guardCalled)
	}
	if e.Ready() == nil {
		t.Fatal("a failed write cleared the load error")
	}

	// A later successful reload, here through Set, clears it.
	restoreRules(t, db)
	if _, err := e.Set(ctx, ScopeTool, "github.create_issue", "ask", "", false); err != nil {
		t.Fatalf("Set after the table is back: %v", err)
	}
	if err := e.Ready(); err != nil {
		t.Fatalf("Ready after a successful reload = %v", err)
	}
	if d := e.Eval(Request{UpstreamName: "github", ToolName: "github.list_issues"}); d.Action != ActionAllow || d.RuleID != "v0.1-name-heuristic" {
		t.Fatalf("Eval after recovery = %+v, want the name heuristic allow", d)
	}
}

// TestChangeLoadsRulesBeforeGuard: when the first load failed, Change reads
// the rules before its guard runs, so the guard judges the change against
// the rules in force and not against the deny-everything fallback.
func TestChangeLoadsRulesBeforeGuard(t *testing.T) {
	ctx := context.Background()
	db := openRulesDB(t)
	if _, err := New(db).Set(ctx, ScopeTool, "github.create_issue", "ask", "", false); err != nil {
		t.Fatal(err)
	}
	hideRules(t, db)
	e := New(db)
	if e.Ready() == nil {
		t.Fatal("Ready() = nil after a failed first load")
	}
	restoreRules(t, db)

	req := Request{UpstreamName: "github", ToolName: "github.create_issue"}
	var saw Decision
	if _, _, err := e.Change(ctx, ScopeTool, "github.create_issue", "deny", "", false, func(before, after *Engine) error {
		saw = before.Eval(req)
		return errString("stop")
	}); err == nil || err.Error() != "stop" {
		t.Fatalf("Change = %v, want the guard's error", err)
	}
	if saw.Action != ActionApprove || !saw.RequireHuman {
		t.Fatalf("guard saw %+v, want the stored ask rule", saw)
	}
	if err := e.Ready(); err != nil {
		t.Fatalf("Ready after Change loaded the rules = %v", err)
	}
}

// TestReloadFailureKeepsLastGoodSnapshot: once loaded, a failed reload
// leaves the cached rules in force and is returned to the writer.
func TestReloadFailureKeepsLastGoodSnapshot(t *testing.T) {
	ctx := context.Background()
	db := openRulesDB(t)
	e, err := Load(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Set(ctx, ScopeTool, "github.list_issues", "deny", "", false); err != nil {
		t.Fatal(err)
	}
	before := e.List()
	if len(before) != 1 {
		t.Fatalf("List after one Set has %d rules, want 1", len(before))
	}

	hideRules(t, db)
	if err := e.reload(ctx); err == nil {
		t.Fatal("reload with an unreadable rules table returned no error")
	}
	req := Request{UpstreamName: "github", ToolName: "github.list_issues"}
	if d := e.Eval(req); d.Action != ActionDeny || d.RuleID != before[0].ID {
		t.Fatalf("Eval after a failed reload = %+v, want the stored deny %s", d, before[0].ID)
	}
	if got := e.List(); !reflect.DeepEqual(got, before) {
		t.Fatalf("List after a failed reload = %+v, want %+v", got, before)
	}
	if err := e.Ready(); err != nil {
		t.Fatalf("a failed later reload marked the engine unavailable: %v", err)
	}

	if _, err := e.Set(ctx, ScopeTool, "github.get_repo", "deny", "", false); err == nil {
		t.Fatal("Set with an unreadable rules table returned no error")
	}
	if err := e.Delete(ctx, before[0].ID); err == nil {
		t.Fatal("Delete with an unreadable rules table returned no error")
	}
	if d := e.Eval(req); d.Action != ActionDeny || d.RuleID != before[0].ID {
		t.Fatalf("Eval after failed writes = %+v, want the stored deny %s", d, before[0].ID)
	}
	if got := e.List(); !reflect.DeepEqual(got, before) {
		t.Fatalf("List after failed writes = %+v, want %+v", got, before)
	}
}

func TestExplicitDeny(t *testing.T) {
	ctx := context.Background()
	e := newTestEngine(t)
	set := func(scope, target, action string) {
		t.Helper()
		if _, err := e.Set(ctx, scope, target, action, "", false); err != nil {
			t.Fatal(err)
		}
	}
	type call struct{ tool, upstream string }
	wantDeny := func(c call) {
		t.Helper()
		d, ok := e.ExplicitDeny(c.tool, c.upstream)
		if !ok || d.Action != ActionDeny || !strings.HasPrefix(d.RuleID, "tp_") {
			t.Errorf("ExplicitDeny(%s, %s) = %+v %v, want an explicit deny", c.tool, c.upstream, d, ok)
		}
	}
	wantNone := func(c call) {
		t.Helper()
		if d, ok := e.ExplicitDeny(c.tool, c.upstream); ok || d != (Decision{}) {
			t.Errorf("ExplicitDeny(%s, %s) = %+v %v, want none", c.tool, c.upstream, d, ok)
		}
	}

	// No rule.
	wantNone(call{"warehouse.query", "warehouse"})

	// Tool-scope deny.
	set(ScopeTool, "warehouse.query", "deny")
	wantDeny(call{"warehouse.query", "warehouse"})
	wantNone(call{"warehouse.tables", "warehouse"})

	// Upstream-scope deny covers the upstream's other tools.
	set(ScopeUpstream, "github", "deny")
	wantDeny(call{"github.list_issues", "github"})

	// A tool-scope allow beats the upstream deny.
	set(ScopeTool, "github.get_repo", "allow")
	wantNone(call{"github.get_repo", "github"})

	// A tool-scope ask beats the upstream deny and is not a deny.
	set(ScopeTool, "github.create_issue", "ask")
	wantNone(call{"github.create_issue", "github"})

	// An upstream-scope rule on the tools group yields to the meta-tool
	// shortcut, as in Eval; a tool-scope deny there still applies.
	set(ScopeUpstream, "tools", "deny")
	wantNone(call{"search", "tools"})
	set(ScopeTool, "executeToolCode", "deny")
	wantDeny(call{"executeToolCode", "tools"})

	// It agrees with Eval on every call above.
	for _, c := range []call{
		{"warehouse.query", "warehouse"}, {"warehouse.tables", "warehouse"},
		{"github.list_issues", "github"}, {"github.get_repo", "github"}, {"github.create_issue", "github"},
		{"search", "tools"}, {"executeToolCode", "tools"},
	} {
		d, ok := e.ExplicitDeny(c.tool, c.upstream)
		ev := e.Eval(Request{UpstreamName: c.upstream, ToolName: c.tool})
		if ok != (ev.Action == ActionDeny) || (ok && d != ev) {
			t.Errorf("ExplicitDeny(%s, %s) = %+v %v, but Eval = %+v", c.tool, c.upstream, d, ok, ev)
		}
	}
}
