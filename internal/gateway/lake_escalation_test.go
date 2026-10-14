package gateway

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tusharbhardwaj/toolyard/internal/inbox"
)

// None of these tests need a ClickHouse: the lake entries are built with a
// nil lake.Service (lakeToolEntries) and their handlers replaced by a
// counter, so only routing is exercised.

// registerLake registers the real lake.* entries on the fixture gateway with
// a counting handler in place of each lake handler.
func (f *inboxFixture) registerLake(t *testing.T) *atomic.Int32 {
	t.Helper()
	ran := &atomic.Int32{}
	for _, e := range lakeToolEntries(nil) {
		e.handle = func(context.Context, map[string]any) (*mcp.CallToolResult, error) {
			ran.Add(1)
			return mcp.NewToolResultText("ran"), nil
		}
		f.gw.registerEntry(e)
	}
	return ran
}

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

func TestLakeEntriesAreOverridableAndEscalate(t *testing.T) {
	for _, e := range lakeToolEntries(nil) {
		if !e.denyOverridesForced {
			t.Errorf("%s: an explicit deny must be able to switch every lake tool off", e.tool.Name)
		}
		wantEscalate := e.tool.Name == "lake.create_table" || e.tool.Name == "lake.ingest"
		if (e.escalate != nil) != wantEscalate {
			t.Errorf("%s: escalate set = %v, want %v", e.tool.Name, e.escalate != nil, wantEscalate)
		}
		if strings.Contains(e.tool.Description, "OR REPLACE freely") {
			t.Errorf("%s: description still invites CREATE OR REPLACE", e.tool.Name)
		}
	}
}

func TestLakeCreateEscalation(t *testing.T) {
	cases := []struct {
		sql  string
		want bool
	}{
		{"CREATE TABLE app.t (x Int64) ENGINE = MergeTree ORDER BY x", false},
		{"CREATE VIEW app.v AS SELECT 1", false},
		{"CREATE TABLE app.t (c String DEFAULT 'or replace') ENGINE = Memory", false},
		{"CREATE OR REPLACE TABLE app.t (x Int64) ENGINE = MergeTree ORDER BY x", true},
		{"create /* c */ or replace view app.v as select 1", true},
		{"REPLACE TABLE app.t ENGINE = Memory AS SELECT 1", true},
		{"", false},
	}
	for _, c := range cases {
		why, got := escalateLakeCreate(map[string]any{"sql": c.sql})
		if got != c.want {
			t.Errorf("escalateLakeCreate(%q) = %v, want %v", c.sql, got, c.want)
		}
		if got && why == "" {
			t.Errorf("escalateLakeCreate(%q): an escalation needs a reason", c.sql)
		}
	}
	if _, got := escalateLakeCreate(nil); got {
		t.Error("no arguments must not escalate")
	}

	// Through the router, on the real entry.
	f := newInboxFixture(t)
	ran := f.registerLake(t)
	replace := map[string]any{"sql": "CREATE OR REPLACE TABLE app.t (x Int64) ENGINE = Memory"}
	mustNeedPermission(t, f.call(t, f.agent, "lake.create_table", replace), "CREATE OR REPLACE")
	if ran.Load() != 0 {
		t.Fatal("CREATE OR REPLACE ran without approval")
	}
	if a := f.access(t, "lake.create_table", replace); a != inbox.AccessRestricted {
		t.Errorf("Access(CREATE OR REPLACE) = %s, want restricted", a)
	}
	plain := map[string]any{"sql": "CREATE TABLE app.t (x Int64) ENGINE = Memory"}
	if res := f.call(t, f.agent, "lake.create_table", plain); res.IsError || ran.Load() != 1 {
		t.Fatalf("plain CREATE TABLE should run without approval: %s", textOf(res))
	}
	if a := f.access(t, "lake.create_table", plain); a != inbox.AccessOpen {
		t.Errorf("Access(CREATE TABLE) = %s, want open", a)
	}
}

func TestLakeIngestEscalation(t *testing.T) {
	local := "/home/u/.toolyard/inbox/a.csv"
	cases := []struct {
		name string
		args map[string]any
		want bool
	}{
		{"append local", map[string]any{"source_type": "csv", "source_uri": local, "mode": "append"}, false},
		{"default mode local", map[string]any{"source_type": "jsonl", "source_uri": local}, false},
		{"local path that mentions http", map[string]any{"source_type": "csv", "source_uri": "/home/u/.toolyard/inbox/http-dump.csv"}, false},
		{"replace", map[string]any{"source_type": "csv", "source_uri": local, "mode": "replace"}, true},
		{"replace spelled differently", map[string]any{"source_type": "csv", "source_uri": local, "mode": " Replace "}, true},
		{"http source type", map[string]any{"source_type": "http", "source_uri": local}, true},
		{"http source type upper-case", map[string]any{"source_type": "HTTP", "source_uri": local}, true},
		{"https URI", map[string]any{"source_type": "csv", "source_uri": "https://example.invalid/a.csv"}, true},
		{"http URI upper-case", map[string]any{"source_type": "csv", "source_uri": "HTTP://example.invalid/a.csv"}, true},
		{"no arguments", nil, false},
	}
	for _, c := range cases {
		why, got := escalateLakeIngest(c.args)
		if got != c.want {
			t.Errorf("%s: escalateLakeIngest = %v, want %v", c.name, got, c.want)
		}
		if got && why == "" {
			t.Errorf("%s: an escalation needs a reason", c.name)
		}
	}

	// Through the router, on the real entry.
	f := newInboxFixture(t)
	ran := f.registerLake(t)
	base := func(extra map[string]any) map[string]any {
		a := map[string]any{"source_type": "csv", "source_uri": local, "target_table": "app.t"}
		for k, v := range extra {
			a[k] = v
		}
		return a
	}
	mustNeedPermission(t, f.call(t, f.agent, "lake.ingest", base(map[string]any{"mode": "replace"})), "ingest replace")
	mustNeedPermission(t, f.call(t, f.agent, "lake.ingest", base(map[string]any{"source_type": "http", "source_uri": "https://example.invalid/a.csv"})), "ingest URL")
	if ran.Load() != 0 {
		t.Fatal("an escalated ingest ran without approval")
	}
	if res := f.call(t, f.agent, "lake.ingest", base(map[string]any{"mode": "append"})); res.IsError || ran.Load() != 1 {
		t.Fatalf("append from a local file should run without approval: %s", textOf(res))
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

func TestExplicitDenyBeatsForcedAllowWhenOverridable(t *testing.T) {
	createSQL := map[string]any{"sql": "CREATE TABLE app.t (x Int64) ENGINE = Memory"}
	querySQL := map[string]any{"sql": "SELECT 1"}

	t.Run("tool deny", func(t *testing.T) {
		f := newInboxFixture(t)
		ran := f.registerLake(t)
		p, err := f.pol.Set(context.Background(), "tool", "lake.create_table", "deny", "frozen", false)
		if err != nil {
			t.Fatal(err)
		}
		mustBeDenied(t, f.call(t, f.agent, "lake.create_table", createSQL), "tool deny")
		if ran.Load() != 0 {
			t.Fatal("a denied lake tool ran")
		}
		if a := f.access(t, "lake.create_table", createSQL); a != inbox.AccessDenied {
			t.Errorf("Access = %s, want denied", a)
		}
		if e := f.explain(t, "lake.create_table"); e["access"] != "deny" || e["rule_id"] != p.ID {
			t.Errorf("explain = %v, want deny by %s", e, p.ID)
		}
		// Other lake tools keep their forced allow.
		if res := f.call(t, f.agent, "lake.query", querySQL); res.IsError || ran.Load() != 1 {
			t.Fatalf("lake.query should still run: %s", textOf(res))
		}
		if e := f.explain(t, "lake.query"); e["access"] != "allow" || !strings.Contains(e["rule"].(string), "unless an explicit deny") {
			t.Errorf("explain lake.query = %v", e)
		}
	})

	t.Run("upstream deny", func(t *testing.T) {
		f := newInboxFixture(t)
		ran := f.registerLake(t)
		if _, err := f.pol.Set(context.Background(), "upstream", lakeUpstream, "deny", "lake off", false); err != nil {
			t.Fatal(err)
		}
		mustBeDenied(t, f.call(t, f.agent, "lake.query", querySQL), "upstream deny, query")
		mustBeDenied(t, f.call(t, f.agent, "lake.create_table", createSQL), "upstream deny, create_table")
		if ran.Load() != 0 {
			t.Fatal("a denied lake tool ran")
		}
		for _, tool := range []string{"lake.query", "lake.insert", "lake.ingest"} {
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
		ran := f.registerLake(t)
		ctx := context.Background()
		if _, err := f.pol.Set(ctx, "upstream", lakeUpstream, "deny", "lake off", false); err != nil {
			t.Fatal(err)
		}
		if _, err := f.pol.Set(ctx, "tool", "lake.query", "allow", "reads stay on", false); err != nil {
			t.Fatal(err)
		}
		if res := f.call(t, f.agent, "lake.query", querySQL); res.IsError || ran.Load() != 1 {
			t.Fatalf("a tool allow beats the upstream deny: %s", textOf(res))
		}
		if a := f.access(t, "lake.query", querySQL); a != inbox.AccessOpen {
			t.Errorf("Access(lake.query) = %s, want open", a)
		}
		if e := f.explain(t, "lake.query"); e["access"] != "allow" {
			t.Errorf("explain lake.query = %v, want allow", e)
		}
		// The rest of the lake stays denied.
		mustBeDenied(t, f.call(t, f.agent, "lake.create_table", createSQL), "create_table under upstream deny")
		if a := f.access(t, "lake.create_table", nil); a != inbox.AccessDenied {
			t.Errorf("Access(lake.create_table) = %s, want denied", a)
		}
	})
}

func TestControlPlaneForcedToolsIgnoreDeny(t *testing.T) {
	f := newInboxFixture(t)
	f.registerLake(t)
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

	// Only lake entries let a deny override their forced action.
	f.gw.mu.RLock()
	defer f.gw.mu.RUnlock()
	for name, e := range f.gw.tools {
		if e.denyOverridesForced != (e.upstream == lakeUpstream) {
			t.Errorf("%s (upstream %s): denyOverridesForced = %v", name, e.upstream, e.denyOverridesForced)
		}
	}
}

func TestAgentPolicyGuardSeesOverridableDeny(t *testing.T) {
	f := newInboxFixture(t)
	f.registerLake(t)
	set := func(target, access string) map[string]any {
		t.Helper()
		return structured(t, f.call(t, f.agent, "policies.set", map[string]any{"scope": "tool", "target": target, "access": access}))
	}

	// An agent's deny on a lake tool takes effect, and the guard records the
	// move from allow to deny.
	s := set("lake.create_table", "deny")
	if s["status"] != "applied" {
		t.Fatalf("deny on lake.create_table: %v", s)
	}
	moved := false
	changed, _ := s["changed"].([]any)
	for _, c := range changed {
		m := c.(map[string]any)
		if m["tool"] == "lake.create_table" && m["before"] == "allow" && m["after"] == "deny" {
			moved = true
		}
	}
	if !moved {
		t.Fatalf("the guard should see lake.create_table move allow -> deny: %v", s)
	}
	mustBeDenied(t, f.call(t, f.agent, "lake.create_table", map[string]any{"sql": "CREATE TABLE app.t (x Int64) ENGINE = Memory"}), "after the agent's deny")

	// Lifting it is allowed because the guard sees the deny it lifts.
	s = set("lake.create_table", "allow")
	if s["status"] != "applied" {
		t.Fatalf("allow should lift the deny: %v", s)
	}
	if before, _ := s["before"].(map[string]any); before["lake.create_table"] != "deny" {
		t.Errorf("before = %v, want lake.create_table deny", s["before"])
	}
	if a := f.access(t, "lake.create_table", nil); a != inbox.AccessOpen {
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
