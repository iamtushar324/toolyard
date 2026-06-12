package policy

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/store"
)

func newDBEngine(t *testing.T) *Engine {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return New(db)
}

// TestNoRulesRegression: a DB-backed engine with no policies behaves exactly
// like the heuristic-only engine (the pre-feature contract).
func TestNoRulesRegression(t *testing.T) {
	e := newDBEngine(t)
	if d := e.Eval(Request{UpstreamName: "github", ToolName: "github.get_repo"}); d.Action != ActionAllow {
		t.Errorf("read tool: got %v, want allow", d.Action)
	}
	if d := e.Eval(Request{UpstreamName: "github", ToolName: "github.create_issue"}); d.Action != ActionApprove {
		t.Errorf("write tool: got %v, want approve", d.Action)
	}
}

func TestExplicitPolicyPrecedence(t *testing.T) {
	e := newDBEngine(t)
	ctx := context.Background()

	// tool-scope allow overrides the default-write heuristic.
	if _, err := e.Set(ctx, ScopeTool, "github.create_issue", "allow", "", false); err != nil {
		t.Fatalf("set tool allow: %v", err)
	}
	if d := e.Eval(Request{UpstreamName: "github", ToolName: "github.create_issue"}); d.Action != ActionAllow {
		t.Errorf("tool allow: got %v, want allow", d.Action)
	}

	// upstream-scope deny applies to OTHER tools on that upstream...
	if _, err := e.Set(ctx, ScopeUpstream, "github", "deny", "", false); err != nil {
		t.Fatalf("set upstream deny: %v", err)
	}
	if d := e.Eval(Request{UpstreamName: "github", ToolName: "github.get_repo"}); d.Action != ActionDeny {
		t.Errorf("upstream deny on read tool: got %v, want deny", d.Action)
	}
	// ...but the tool-scope allow still wins for its specific tool.
	if d := e.Eval(Request{UpstreamName: "github", ToolName: "github.create_issue"}); d.Action != ActionAllow {
		t.Errorf("tool allow beats upstream deny: got %v, want allow", d.Action)
	}

	// ask → approve + RequireHuman.
	if _, err := e.Set(ctx, ScopeTool, "fs.write_file", "ask", "", false); err != nil {
		t.Fatalf("set tool ask: %v", err)
	}
	d := e.Eval(Request{UpstreamName: "fs", ToolName: "fs.write_file"})
	if d.Action != ActionApprove || !d.RequireHuman {
		t.Errorf("ask policy: got action=%v requireHuman=%v, want approve+true", d.Action, d.RequireHuman)
	}
}

func TestAllowDestructiveRequiresForce(t *testing.T) {
	e := newDBEngine(t)
	ctx := context.Background()
	if _, err := e.Set(ctx, ScopeTool, "github.delete_repo", "allow", "", false); !errors.Is(err, ErrForceRequired) {
		t.Fatalf("allow destructive without force: got %v, want ErrForceRequired", err)
	}
	if _, err := e.Set(ctx, ScopeTool, "github.delete_repo", "allow", "", true); err != nil {
		t.Fatalf("allow destructive with force: %v", err)
	}
	if d := e.Eval(Request{UpstreamName: "github", ToolName: "github.delete_repo"}); d.Action != ActionAllow {
		t.Errorf("forced allow: got %v, want allow", d.Action)
	}
}

func TestPolicyUpsertAndDelete(t *testing.T) {
	e := newDBEngine(t)
	ctx := context.Background()
	if _, err := e.Set(ctx, ScopeTool, "x.tool", "ask", "first", false); err != nil {
		t.Fatal(err)
	}
	// Upsert (same scope/target) updates action in place — still one entry.
	if _, err := e.Set(ctx, ScopeTool, "x.tool", "deny", "second", false); err != nil {
		t.Fatal(err)
	}
	if got := len(e.List()); got != 1 {
		t.Fatalf("after upsert, List len = %d, want 1", got)
	}
	p, ok := e.Get(ScopeTool, "x.tool")
	if !ok || p.Action != "deny" {
		t.Fatalf("Get after upsert = %+v ok=%v, want action=deny", p, ok)
	}
	if err := e.Delete(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.Get(ScopeTool, "x.tool"); ok {
		t.Fatal("policy still present after delete")
	}
}
