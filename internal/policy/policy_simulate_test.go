package policy

import (
	"context"
	"testing"
)

// TestWithChangeIsDetached: the what-if copy evaluates the change and
// leaves the engine, and its database, exactly as they were.
func TestWithChangeIsDetached(t *testing.T) {
	ctx := context.Background()
	e := newTestEngine(t)
	if _, err := e.Set(ctx, ScopeUpstream, "github", "ask", "", false); err != nil {
		t.Fatal(err)
	}
	req := Request{UpstreamName: "github", ToolName: "github.create_issue"}

	// Upsert on the copy.
	sim := e.WithChange(ScopeTool, "github.create_issue", "allow")
	if d := sim.Eval(req); d.Action != ActionAllow || d.RuleID != "tp_simulated" {
		t.Fatalf("simulated allow = %+v", d)
	}
	if d := e.Eval(req); d.Action != ActionApprove || !d.RequireHuman {
		t.Fatalf("the real engine changed: %+v", d)
	}
	if _, ok := e.Get(ScopeTool, "github.create_issue"); ok {
		t.Fatal("the simulated rule leaked into the engine")
	}
	if n := len(e.List()); n != 1 {
		t.Fatalf("engine has %d rules, want 1", n)
	}

	// Removal on the copy: back to the default for a write.
	sim = e.WithChange(ScopeUpstream, "github", "")
	if d := sim.Eval(req); d.Action != ActionApprove || d.RuleID != "v0.1-default-write" {
		t.Fatalf("simulated clear = %+v", d)
	}
	if d := e.Eval(req); d.RuleID == "v0.1-default-write" {
		t.Fatalf("the real upstream rule is gone: %+v", d)
	}

	// Changing an existing rule keeps its id on the copy.
	real, _ := e.Get(ScopeUpstream, "github")
	sim = e.WithChange(ScopeUpstream, "github", "deny")
	if d := sim.Eval(req); d.Action != ActionDeny || d.RuleID != real.ID {
		t.Fatalf("simulated change of an existing rule = %+v, want id %s", d, real.ID)
	}
	// The copy cannot write.
	if _, err := sim.Set(ctx, ScopeTool, "x.y", "deny", "", false); err != ErrNoDB {
		t.Fatalf("Set on a copy = %v, want ErrNoDB", err)
	}
}
