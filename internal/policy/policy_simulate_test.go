package policy

import (
	"context"
	"testing"
	"time"
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

// TestChangeIsGuardedAndSerialised: Change writes only when the guard
// agrees, the guard sees before and after, and no other write lands
// between the check and the write.
func TestChangeIsGuardedAndSerialised(t *testing.T) {
	ctx := context.Background()
	e := newTestEngine(t)
	req := Request{UpstreamName: "github", ToolName: "github.create_issue"}

	// A refusing guard: nothing is written and its error comes back as is.
	refused := errString("no")
	var sawBefore, sawAfter Action
	p, existed, err := e.Change(ctx, ScopeTool, "github.create_issue", "allow", "", false, func(before, after *Engine) error {
		sawBefore, sawAfter = before.Eval(req).Action, after.Eval(req).Action
		return refused
	})
	if err != refused || p != nil || existed {
		t.Fatalf("Change with a refusing guard = %v %v %v", p, existed, err)
	}
	if sawBefore != ActionApprove || sawAfter != ActionAllow {
		t.Fatalf("guard saw %v → %v", sawBefore, sawAfter)
	}
	if _, ok := e.Get(ScopeTool, "github.create_issue"); ok {
		t.Fatal("a refused change was written")
	}

	// An agreeing guard writes; a concurrent Set waits for the whole of
	// Change (check and write) to finish.
	setDone := make(chan error, 1)
	p, existed, err = e.Change(ctx, ScopeTool, "github.create_issue", "deny", "why", false, func(before, after *Engine) error {
		go func() { _, err := e.Set(ctx, ScopeTool, "github.other", "deny", "", false); setDone <- err }()
		time.Sleep(50 * time.Millisecond)
		if _, ok := before.Get(ScopeTool, "github.other"); ok {
			return errString("another write landed during the check")
		}
		return nil
	})
	if err != nil || p == nil || p.Action != "deny" || p.Note != "why" || !existed {
		t.Fatalf("Change = %+v %v %v", p, existed, err)
	}
	if err := <-setDone; err != nil {
		t.Fatal(err)
	}
	if _, ok := e.Get(ScopeTool, "github.other"); !ok {
		t.Fatal("the queued Set never landed")
	}

	// Removal reports whether there was anything to remove.
	if _, existed, err := e.Change(ctx, ScopeTool, "github.create_issue", "", "", false, nil); err != nil || !existed {
		t.Fatalf("remove = %v %v", existed, err)
	}
	if _, existed, err := e.Change(ctx, ScopeTool, "github.create_issue", "", "", false, nil); err != nil || existed {
		t.Fatalf("remove of nothing = %v %v", existed, err)
	}
	if _, _, err := e.Change(ctx, "nope", "x", "", "", false, nil); err == nil {
		t.Fatal("bad scope accepted")
	}
	if _, _, err := e.Change(ctx, ScopeTool, "x", "maybe", "", false, nil); err == nil {
		t.Fatal("bad action accepted")
	}
	// The destructive-allow check still applies through Change.
	if _, _, err := e.Change(ctx, ScopeTool, "github.delete_repo", "allow", "", false, nil); err != ErrForceRequired {
		t.Fatalf("destructive allow through Change = %v", err)
	}
}
