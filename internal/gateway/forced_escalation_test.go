package gateway

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tusharbhardwaj/toolyard/internal/inbox"
	"github.com/tusharbhardwaj/toolyard/internal/policy"
)

// These tests exercise the routing rules a built-in entry can carry on top
// of a forced allow: an escalate func that sends some argument sets to a
// person, and denyOverridesForced, which lets an explicit deny switch the
// tool off. They use synthetic entries, so only routing is exercised.

// actionAllow is the forced action the synthetic entries share.
var actionAllow = policy.ActionAllow

// dataUpstream tags the synthetic data tools: forced-allow entries an
// explicit deny may override.
const dataUpstream = "data"

// registerSynthIngest registers synth.ingest: a forced-allow tool whose
// escalate func sends mode=replace to approval.
func (f *inboxFixture) registerSynthIngest(t *testing.T) *atomic.Int32 {
	t.Helper()
	ran := &atomic.Int32{}
	f.gw.registerEntry(toolEntry{
		tool: mcp.Tool{Name: "synth.ingest", InputSchema: mcp.ToolInputSchema{Type: "object", Properties: addMetaProps(map[string]any{
			"mode": map[string]any{"type": "string"},
		})}},
		upstream: "synth", originalName: "ingest", reasonField: ReasonField,
		forcedAction: &actionAllow,
		escalate: func(args map[string]any) (string, bool) {
			if stringArg(args, "mode") == "replace" {
				return "synth.ingest: replace discards the target's data", true
			}
			return "", false
		},
		handle: func(context.Context, map[string]any) (*mcp.CallToolResult, error) {
			ran.Add(1)
			return mcp.NewToolResultText("ingested"), nil
		},
	})
	return ran
}

// registerData registers data.query, data.insert and data.ingest:
// forced-allow entries an explicit deny overrides, with a counting handler.
// data.ingest escalates mode=replace.
func (f *inboxFixture) registerData(t *testing.T) *atomic.Int32 {
	t.Helper()
	ran := &atomic.Int32{}
	for _, short := range []string{"query", "insert", "ingest"} {
		e := toolEntry{
			tool: mcp.Tool{Name: dataUpstream + "." + short, InputSchema: mcp.ToolInputSchema{Type: "object", Properties: addMetaProps(map[string]any{
				"mode": map[string]any{"type": "string"},
			})}},
			upstream: dataUpstream, originalName: short, reasonField: ReasonField,
			forcedAction:        &actionAllow,
			denyOverridesForced: true,
			handle: func(context.Context, map[string]any) (*mcp.CallToolResult, error) {
				ran.Add(1)
				return mcp.NewToolResultText("ran"), nil
			},
		}
		if short == "ingest" {
			e.escalate = func(args map[string]any) (string, bool) {
				if stringArg(args, "mode") == "replace" {
					return "data.ingest: replace discards the target's data", true
				}
				return "", false
			}
		}
		f.gw.registerEntry(e)
	}
	return ran
}

func (f *inboxFixture) access(t *testing.T, tool string, args map[string]any) string {
	t.Helper()
	a, _ := f.gw.Access(context.Background(), "ag_1", tool, args)
	return a
}

func (f *inboxFixture) explain(t *testing.T, tool string) map[string]any {
	t.Helper()
	res := f.call(t, f.agent, "policies.explain", map[string]any{"tool": tool})
	if res.IsError {
		t.Fatalf("policies.explain %s: %s", tool, textOf(res))
	}
	return structured(t, res)
}

func mustNeedPermission(t *testing.T, res *mcp.CallToolResult, what string) {
	t.Helper()
	if s := structured(t, res); !res.IsError || s["status"] != "permission_required" || s["executed"] != false {
		t.Fatalf("%s: want permission_required, got %v %s", what, s, textOf(res))
	}
}

func mustBeDenied(t *testing.T, res *mcp.CallToolResult, what string) {
	t.Helper()
	if !res.IsError || !strings.Contains(textOf(res), "denied by policy") {
		t.Fatalf("%s: want a policy deny, got isError=%v %s", what, res.IsError, textOf(res))
	}
}

// synthRequest is an access request for one synth.ingest call.
func synthRequest(mode string) map[string]any {
	args := requestArgs(map[string]any{"mode": mode})
	args["tools"].([]any)[0].(map[string]any)["tool"] = "synth.ingest"
	return args
}

func warningsMention(s map[string]any, text string) bool {
	ws, _ := s["warnings"].([]any)
	for _, w := range ws {
		if m, ok := w.(map[string]any); ok && strings.Contains(m["message"].(string), text) {
			return true
		}
	}
	return false
}

func TestForcedAllowEscalatesThroughRouter(t *testing.T) {
	f := newInboxFixture(t)
	ran := f.registerSynthIngest(t)

	// replace: permission_required, nothing runs, nothing is queued.
	mustNeedPermission(t, f.call(t, f.agent, "synth.ingest", map[string]any{"mode": "replace"}), "replace")
	if ran.Load() != 0 {
		t.Fatal("handler ran for an escalated call")
	}
	// append: runs at once.
	if res := f.call(t, f.agent, "synth.ingest", map[string]any{"mode": "append"}); res.IsError || ran.Load() != 1 {
		t.Fatalf("append should run: %s", textOf(res))
	}
	// Access (inbox.check) agrees with the router for each argument set.
	for _, c := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"mode": "replace"}, inbox.AccessRestricted},
		{map[string]any{"mode": "append"}, inbox.AccessOpen},
		{nil, inbox.AccessOpen},
	} {
		if a := f.access(t, "synth.ingest", c.args); a != c.want {
			t.Errorf("Access(%v) = %s, want %s", c.args, a, c.want)
		}
	}
	// Request validation judges the exact parameters: a replace request is
	// not told it "doesn't need permission"; an append request is.
	dry := synthRequest("replace")
	dry["dry_run"] = true
	if s := structured(t, f.call(t, f.agent, "inbox.request", dry)); s["ok"] != true || warningsMention(s, "doesn't need permission") {
		t.Fatalf("replace dry run: %v", s)
	}
	dry = synthRequest("append")
	dry["dry_run"] = true
	if s := structured(t, f.call(t, f.agent, "inbox.request", dry)); !warningsMention(s, "doesn't need permission") {
		t.Fatalf("append dry run should say it needs no permission: %v", s)
	}

	// A grant for the replace call runs it exactly once.
	ctx := context.Background()
	res := f.call(t, f.agent, "inbox.request", synthRequest("replace"))
	s := structured(t, res)
	id, _ := s["request_id"].(string)
	if s["ok"] != true || id == "" {
		t.Fatalf("request: %v", s)
	}
	f.svc.Flush()
	if _, err := f.svc.Decide(ctx, id, inbox.Decision{Action: "submit", RequestRevision: 1, SubmissionID: "test_approval", Verdicts: map[string]inbox.CallVerdict{"deploy_api": {Verdict: inbox.VerdictAccepted}}}); err != nil {
		t.Fatal(err)
	}
	w := structured(t, f.call(t, f.agent, "inbox.wait", map[string]any{"ids": []any{id}, "timeout_seconds": 1}))
	tok, _ := w["requests"].([]any)[0].(map[string]any)["tools"].([]any)[0].(map[string]any)["grant"].(string)
	if !strings.HasPrefix(tok, "tyg_") {
		t.Fatalf("no grant token in wait result: %v", w)
	}
	if res := f.call(t, f.agent, "synth.ingest", map[string]any{"mode": "replace", GrantField: tok}); res.IsError || ran.Load() != 2 {
		t.Fatalf("granted replace should run once: %s", textOf(res))
	}
	if s := structured(t, f.call(t, f.agent, "synth.ingest", map[string]any{"mode": "replace", GrantField: tok})); s["status"] != "grant_invalid" || ran.Load() != 2 {
		t.Fatalf("a used grant must not run the call again: %v (ran %d)", s, ran.Load())
	}
}

func TestOverridableEntryEscalatesThroughRouter(t *testing.T) {
	f := newInboxFixture(t)
	ran := f.registerData(t)
	mustNeedPermission(t, f.call(t, f.agent, "data.ingest", map[string]any{"mode": "replace"}), "ingest replace")
	if ran.Load() != 0 {
		t.Fatal("an escalated ingest ran without approval")
	}
	if a := f.access(t, "data.ingest", map[string]any{"mode": "replace"}); a != inbox.AccessRestricted {
		t.Errorf("Access(ingest replace) = %s, want restricted", a)
	}
	if res := f.call(t, f.agent, "data.ingest", map[string]any{"mode": "append"}); res.IsError || ran.Load() != 1 {
		t.Fatalf("append should run without approval: %s", textOf(res))
	}
	if a := f.access(t, "data.ingest", map[string]any{"mode": "append"}); a != inbox.AccessOpen {
		t.Errorf("Access(ingest append) = %s, want open", a)
	}
}

func TestExplicitDenyBeatsForcedAllowWhenOverridable(t *testing.T) {
	t.Run("tool deny", func(t *testing.T) {
		f := newInboxFixture(t)
		ran := f.registerData(t)
		p, err := f.pol.Set(context.Background(), "tool", "data.insert", "deny", "frozen", false)
		if err != nil {
			t.Fatal(err)
		}
		mustBeDenied(t, f.call(t, f.agent, "data.insert", nil), "tool deny")
		if ran.Load() != 0 {
			t.Fatal("a denied data tool ran")
		}
		if a := f.access(t, "data.insert", nil); a != inbox.AccessDenied {
			t.Errorf("Access = %s, want denied", a)
		}
		if e := f.explain(t, "data.insert"); e["access"] != "deny" || e["rule_id"] != p.ID {
			t.Errorf("explain = %v, want deny by %s", e, p.ID)
		}
		// The other data tools keep their forced allow.
		if res := f.call(t, f.agent, "data.query", nil); res.IsError || ran.Load() != 1 {
			t.Fatalf("data.query should still run: %s", textOf(res))
		}
		if e := f.explain(t, "data.query"); e["access"] != "allow" || !strings.Contains(e["rule"].(string), "unless an explicit deny") {
			t.Errorf("explain data.query = %v", e)
		}
	})

	t.Run("upstream deny", func(t *testing.T) {
		f := newInboxFixture(t)
		ran := f.registerData(t)
		if _, err := f.pol.Set(context.Background(), "upstream", dataUpstream, "deny", "data off", false); err != nil {
			t.Fatal(err)
		}
		mustBeDenied(t, f.call(t, f.agent, "data.query", nil), "upstream deny, query")
		mustBeDenied(t, f.call(t, f.agent, "data.insert", nil), "upstream deny, insert")
		if ran.Load() != 0 {
			t.Fatal("a denied data tool ran")
		}
		for _, tool := range []string{"data.query", "data.insert", "data.ingest"} {
			if a := f.access(t, tool, nil); a != inbox.AccessDenied {
				t.Errorf("Access(%s) = %s, want denied", tool, a)
			}
			if e := f.explain(t, tool); e["access"] != "deny" {
				t.Errorf("explain %s = %v, want deny", tool, e)
			}
		}
	})

	t.Run("upstream deny with tool allow", func(t *testing.T) {
		f := newInboxFixture(t)
		ran := f.registerData(t)
		ctx := context.Background()
		if _, err := f.pol.Set(ctx, "upstream", dataUpstream, "deny", "data off", false); err != nil {
			t.Fatal(err)
		}
		if _, err := f.pol.Set(ctx, "tool", "data.query", "allow", "reads stay on", false); err != nil {
			t.Fatal(err)
		}
		if res := f.call(t, f.agent, "data.query", nil); res.IsError || ran.Load() != 1 {
			t.Fatalf("a tool allow beats the upstream deny: %s", textOf(res))
		}
		if a := f.access(t, "data.query", nil); a != inbox.AccessOpen {
			t.Errorf("Access(data.query) = %s, want open", a)
		}
		if e := f.explain(t, "data.query"); e["access"] != "allow" {
			t.Errorf("explain data.query = %v, want allow", e)
		}
		// The rest of the group stays denied.
		mustBeDenied(t, f.call(t, f.agent, "data.insert", nil), "insert under upstream deny")
		if a := f.access(t, "data.insert", nil); a != inbox.AccessDenied {
			t.Errorf("Access(data.insert) = %s, want denied", a)
		}
	})
}

func TestControlPlaneForcedToolsIgnoreDeny(t *testing.T) {
	f := newInboxFixture(t)
	f.registerData(t)
	ctx := context.Background()
	for _, p := range [][2]string{{"tool", "inbox.check"}, {"upstream", inboxUpstream}, {"tool", "policies.explain"}, {"upstream", policiesUpstream}} {
		if _, err := f.pol.Set(ctx, p[0], p[1], "deny", "should not lock agents out", false); err != nil {
			t.Fatal(err)
		}
	}
	res := f.call(t, f.agent, "inbox.check", map[string]any{"calls": []any{map[string]any{"tool": "deploy.get_status"}}})
	if res.IsError {
		t.Fatalf("inbox.check must ignore a deny: %s", textOf(res))
	}
	if calls, _ := structured(t, res)["calls"].([]any); len(calls) != 1 || calls[0].(map[string]any)["status"] != "open" {
		t.Errorf("inbox.check result = %v", structured(t, res))
	}
	if e := f.explain(t, "inbox.check"); e["access"] != "allow" {
		t.Errorf("explain inbox.check = %v, want allow", e)
	}
	for _, tool := range []string{"inbox.check", "inbox.request", "policies.explain"} {
		if a := f.access(t, tool, nil); a != inbox.AccessOpen {
			t.Errorf("Access(%s) = %s, want open", tool, a)
		}
	}

	// No built-in entry lets a deny override its forced action; only the
	// synthetic data entries registered above do.
	f.gw.mu.RLock()
	defer f.gw.mu.RUnlock()
	for name, e := range f.gw.tools {
		if e.denyOverridesForced != (e.upstream == dataUpstream) {
			t.Errorf("%s (upstream %s): denyOverridesForced = %v", name, e.upstream, e.denyOverridesForced)
		}
	}
}

func TestAgentPolicyGuardSeesOverridableDeny(t *testing.T) {
	f := newInboxFixture(t)
	f.registerData(t)
	set := func(target, access string) map[string]any {
		t.Helper()
		return structured(t, f.call(t, f.agent, "policies.set", map[string]any{"scope": "tool", "target": target, "access": access}))
	}

	// An agent's deny on an overridable tool takes effect, and the guard
	// records the move from allow to deny.
	s := set("data.insert", "deny")
	if s["status"] != "applied" {
		t.Fatalf("deny on data.insert: %v", s)
	}
	moved := false
	changed, _ := s["changed"].([]any)
	for _, c := range changed {
		m := c.(map[string]any)
		if m["tool"] == "data.insert" && m["before"] == "allow" && m["after"] == "deny" {
			moved = true
		}
	}
	if !moved {
		t.Fatalf("the guard should see data.insert move allow -> deny: %v", s)
	}
	mustBeDenied(t, f.call(t, f.agent, "data.insert", nil), "after the agent's deny")

	// Lifting it is allowed because the guard sees the deny it lifts.
	s = set("data.insert", "allow")
	if s["status"] != "applied" {
		t.Fatalf("allow should lift the deny: %v", s)
	}
	if before, _ := s["before"].(map[string]any); before["data.insert"] != "deny" {
		t.Errorf("before = %v, want data.insert deny", s["before"])
	}
	if a := f.access(t, "data.insert", nil); a != inbox.AccessOpen {
		t.Errorf("Access after the lift = %s, want open", a)
	}

	// A control-plane tool ignores the deny, so the guard sees no change,
	// and an allow on it is refused as pinning an open tool.
	s = set("policies.list", "deny")
	if c, _ := s["changed"].([]any); s["status"] != "applied" || len(c) != 0 {
		t.Fatalf("deny on policies.list should change nothing: %v", s)
	}
	res := f.call(t, f.agent, "policies.set", map[string]any{"scope": "tool", "target": "policies.list", "access": "allow"})
	if s := structured(t, res); !res.IsError || s["reason"] != "allow_only_lifts_deny" {
		t.Fatalf("allow on policies.list should be refused: %v %s", s, textOf(res))
	}
}
