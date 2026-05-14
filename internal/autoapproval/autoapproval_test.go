package autoapproval

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// newTestService spins up a temporary sqlite-backed Service. The metrics
// reader and settings service are nil — SetToolPolicy / ToolPolicies don't
// touch either of them.
func newTestService(t *testing.T) *Service {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return New(db, nil, nil)
}

func TestSetToolPolicy_CreatesEnabledRule(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()

	if err := s.SetToolPolicy(ctx, "kite_get_holdings", true); err != nil {
		t.Fatalf("SetToolPolicy on: %v", err)
	}
	p := s.ToolPolicies(ctx)
	if !p["kite_get_holdings"] {
		t.Fatalf("expected kite_get_holdings to be auto-approved, got %#v", p)
	}

	rules, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var matched int
	for _, r := range rules {
		if r.Kind == "tool" && r.ToolName == "kite_get_holdings" {
			matched++
			if !r.Enabled {
				t.Fatalf("rule should be enabled")
			}
			if r.Source != "user" {
				t.Fatalf("source = %q, want user", r.Source)
			}
		}
	}
	if matched != 1 {
		t.Fatalf("expected 1 tool rule, got %d", matched)
	}
}

func TestSetToolPolicy_OffDisablesRule(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()

	if err := s.SetToolPolicy(ctx, "lake_query", true); err != nil {
		t.Fatalf("on: %v", err)
	}
	if err := s.SetToolPolicy(ctx, "lake_query", false); err != nil {
		t.Fatalf("off: %v", err)
	}
	if p := s.ToolPolicies(ctx); p["lake_query"] {
		t.Fatalf("expected lake_query disabled, got %#v", p)
	}

	// The rule row should still exist (disabled) so toggling back on
	// re-uses it instead of creating a duplicate.
	if err := s.SetToolPolicy(ctx, "lake_query", true); err != nil {
		t.Fatalf("on again: %v", err)
	}
	rules, _ := s.List(ctx)
	var n int
	for _, r := range rules {
		if r.Kind == "tool" && r.ToolName == "lake_query" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("expected exactly 1 tool rule after toggle cycle, got %d", n)
	}
}

func TestSetToolPolicy_RejectsEmpty(t *testing.T) {
	s := newTestService(t)
	if err := s.SetToolPolicy(context.Background(), "   ", true); err == nil {
		t.Fatal("expected error for empty tool name")
	}
}

func TestSetToolPolicy_OnClearsCooloff(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()

	if err := s.SetToolPolicy(ctx, "notes_publish", true); err != nil {
		t.Fatalf("on: %v", err)
	}
	// Simulate a denial having set a cool-off on the rule.
	if _, err := s.db.ExecContext(ctx,
		`UPDATE auto_approval_rules SET cooloff_until = 99999999999999 WHERE kind='tool' AND tool_name=?`,
		"notes_publish"); err != nil {
		t.Fatalf("seed cooloff: %v", err)
	}
	_ = s.reload(ctx)

	// Toggle off, then back on — the on path must clear cool-off so the
	// operator's explicit "yes, please auto-approve" isn't silently
	// neutered by leftover state.
	if err := s.SetToolPolicy(ctx, "notes_publish", false); err != nil {
		t.Fatalf("off: %v", err)
	}
	if err := s.SetToolPolicy(ctx, "notes_publish", true); err != nil {
		t.Fatalf("on: %v", err)
	}

	rules, _ := s.List(ctx)
	for _, r := range rules {
		if r.Kind == "tool" && r.ToolName == "notes_publish" {
			if r.CoolOffUntil != 0 {
				t.Fatalf("cooloff_until = %d, want 0", r.CoolOffUntil)
			}
			if !r.Enabled {
				t.Fatalf("expected re-enabled rule")
			}
		}
	}
}
