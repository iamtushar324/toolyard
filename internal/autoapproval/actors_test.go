package autoapproval

import (
	"context"
	"testing"
)

func findRule(t *testing.T, s *Service, id string) Rule {
	t.Helper()
	r, err := s.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	return *r
}

func TestRuleCreateOrUpdate_RecordsCreatorAndEnabler(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()

	r, err := s.CreateOrUpdate(ctx, Rule{Kind: "static", Fingerprint: "fp1", Enabled: true, CreatedBy: "u_admin"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got := findRule(t, s, r.ID)
	if got.CreatedBy != "u_admin" || got.EnabledBy != "u_admin" {
		t.Fatalf("created_by=%q enabled_by=%q, want u_admin for both (enabled at create)", got.CreatedBy, got.EnabledBy)
	}
	// The cache (List) carries the same columns.
	rules, _ := s.List(ctx)
	if len(rules) != 1 || rules[0].CreatedBy != "u_admin" {
		t.Fatalf("List lost created_by: %+v", rules)
	}

	// A proposer rule has no person behind it.
	p, err := s.CreateOrUpdate(ctx, Rule{Kind: "tool", ToolName: "t", Enabled: false, Source: "proposer"})
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	if got := findRule(t, s, p.ID); got.CreatedBy != "" || got.EnabledBy != "" {
		t.Fatalf("proposer rule has actors: %+v", got)
	}

	// Updating an existing rule keeps its creator; enabling it names the enabler.
	if _, err := s.CreateOrUpdate(ctx, Rule{ID: r.ID, Kind: "static", Fingerprint: "fp1", Enabled: true, CreatedBy: "u_other", EnabledBy: "u_other"}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if got := findRule(t, s, r.ID); got.CreatedBy != "u_admin" || got.EnabledBy != "u_other" {
		t.Fatalf("after update: created_by=%q enabled_by=%q, want u_admin/u_other", got.CreatedBy, got.EnabledBy)
	}
}

func TestRuleEnable_RecordsEnabler(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	r, err := s.CreateOrUpdate(ctx, Rule{Kind: "tool", ToolName: "t", Enabled: false, Source: "proposer"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.Enable(ctx, r.ID, "u_ok"); err != nil {
		t.Fatalf("enable: %v", err)
	}
	got := findRule(t, s, r.ID)
	if !got.Enabled || got.EnabledBy != "u_ok" {
		t.Fatalf("enabled=%v enabled_by=%q, want true/u_ok", got.Enabled, got.EnabledBy)
	}
	if err := s.Disable(ctx, r.ID); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if got := findRule(t, s, r.ID); got.Enabled || got.EnabledBy != "u_ok" {
		t.Fatalf("disable must keep the last enabler for the record: %+v", got)
	}
}

func TestRuleSetToolPolicyBy_RecordsActor(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	if err := s.SetToolPolicyBy(ctx, "kite_get_holdings", true, "u_one"); err != nil {
		t.Fatalf("on: %v", err)
	}
	rules, _ := s.List(ctx)
	if len(rules) != 1 || rules[0].CreatedBy != "u_one" || rules[0].EnabledBy != "u_one" {
		t.Fatalf("created rule actors: %+v", rules)
	}
	if err := s.SetToolPolicyBy(ctx, "kite_get_holdings", false, "u_one"); err != nil {
		t.Fatalf("off: %v", err)
	}
	if err := s.SetToolPolicyBy(ctx, "kite_get_holdings", true, "u_two"); err != nil {
		t.Fatalf("on again: %v", err)
	}
	got := findRule(t, s, rules[0].ID)
	if got.CreatedBy != "u_one" || got.EnabledBy != "u_two" {
		t.Fatalf("re-enable: created_by=%q enabled_by=%q, want u_one/u_two", got.CreatedBy, got.EnabledBy)
	}
	// The actor-less wrapper still works and leaves the columns alone.
	if err := s.SetToolPolicy(ctx, "lake_query", true); err != nil {
		t.Fatalf("legacy on: %v", err)
	}
}

func TestRuleMatch_CarriesCreator(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	if err := s.SetToolPolicyBy(ctx, "notes_publish", true, "u_creator"); err != nil {
		t.Fatalf("on: %v", err)
	}
	m := s.Match("ag_1", "up", "notes_publish", "fp", false)
	if m == nil {
		t.Fatal("expected a match")
	}
	if m.CreatedBy != "u_creator" || m.Kind != "tool" {
		t.Fatalf("match = %+v, want created_by u_creator", m)
	}
}
