package gateway

// Regression tests for the attacker review of the agent access tools:
// each test reproduces one hole as reported and asserts it is closed.

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tusharbhardwaj/toolyard/internal/codemode"
	"github.com/tusharbhardwaj/toolyard/internal/policy"
)

// registerFake adds an upstream-backed-looking tool to the fixture.
func (f *accessFixture) registerFake(t *testing.T, upstream, short string) {
	t.Helper()
	f.gw.registerEntry(toolEntry{
		tool:     mcp.Tool{Name: upstream + "." + short, InputSchema: mcp.ToolInputSchema{Type: "object", Properties: addMetaProps(map[string]any{})}},
		upstream: upstream, originalName: short, reasonField: ReasonField,
		handle: func(context.Context, map[string]any) (*mcp.CallToolResult, error) {
			f.calls.Add(1)
			return mcp.NewToolResultText("ran"), nil
		},
	})
}

// requireHuman is what the approval bus would see for a call: whether an
// explicit ask keeps the learned auto-approval rules away.
func (f *accessFixture) requireHuman(upstream, tool string) bool {
	return f.gw.policy.Eval(policy.Request{UpstreamName: upstream, ToolName: tool}).RequireHuman
}

// H1: clearing an explicit ask kept the word "ask" but dropped
// RequireHuman, so a learned auto-approval rule could decide the call.
func TestH1ClearingAnExplicitAskIsOutOfAsk(t *testing.T) {
	f := newAccessFixture(t, nil)
	ctx := context.Background()
	f.registerFake(t, "zeta", "create_x")
	f.registerFake(t, "zeta", "get_x")

	// Upstream scope: the operator's "always ask me" on zeta.
	if _, err := f.gw.policy.Set(ctx, policy.ScopeUpstream, "zeta", "ask", "", false); err != nil {
		t.Fatal(err)
	}
	if !f.requireHuman("zeta", "zeta.create_x") {
		t.Fatal("precondition: explicit upstream ask must require a human")
	}
	res := f.call(t, f.admin, "policies.clear", map[string]any{"scope": "upstream", "target": "zeta"})
	mustReject(t, res, "zeta.create_x (ask by policy → ask by default, where an auto-approval rule may decide)", "zeta.get_x (ask → allow)")
	if !f.requireHuman("zeta", "zeta.create_x") {
		t.Fatal("the explicit ask was cleared: a learned rule can now approve zeta.create_x without a person")
	}
	// Even with every tool of the server a write, so the words never move.
	f.registerFake(t, "eta", "create_y")
	if _, err := f.gw.policy.Set(ctx, policy.ScopeUpstream, "eta", "ask", "", false); err != nil {
		t.Fatal(err)
	}
	res = f.call(t, f.admin, "policies.clear", map[string]any{"scope": "upstream", "target": "eta"})
	mustReject(t, res, "eta.create_y (ask by policy → ask by default")
	if !f.requireHuman("eta", "eta.create_y") {
		t.Fatal("eta's explicit ask was cleared")
	}

	// Tool scope: an explicit ask on a write, cleared, is the same hole.
	mustApply(t, f.setPolicy(t, f.admin, "tool", "alpha.run", "ask"))
	if !f.requireHuman("alpha", "alpha.run") {
		t.Fatal("precondition: tool ask must require a human")
	}
	mustReject(t, f.call(t, f.admin, "policies.clear", map[string]any{"scope": "tool", "target": "alpha.run"}), "alpha.run (ask by policy → ask by default")
	if !f.requireHuman("alpha", "alpha.run") {
		t.Fatal("alpha.run's explicit ask was cleared")
	}
	// Replacing it with a tool-scope ask under an upstream ask keeps the
	// person, so that is fine; and policies.list says who decides.
	if _, err := f.gw.policy.Set(ctx, policy.ScopeUpstream, "alpha", "ask", "", false); err != nil {
		t.Fatal(err)
	}
	res = f.call(t, f.admin, "policies.clear", map[string]any{"scope": "tool", "target": "alpha.run"})
	if res.IsError {
		t.Fatalf("clearing a tool ask that an upstream ask still covers: %s", textOf(res))
	}
	if !f.requireHuman("alpha", "alpha.run") {
		t.Fatal("alpha.run lost its person")
	}
	res = f.call(t, f.member, "policies.list", map[string]any{"server": "alpha"})
	if !strings.Contains(textOf(res), `"require_human": true`) {
		t.Fatalf("policies.list should say a person decides:\n%s", textOf(res))
	}
	res = f.call(t, f.member, "policies.explain", map[string]any{"tool": "alpha.run"})
	if !strings.Contains(textOf(res), "learned auto-approval rules do not apply") {
		t.Fatalf("policies.explain should say a person decides:\n%s", textOf(res))
	}
}

// H1 (second part): an upstream-scope ask never disabled the learned
// tool auto-approval rules of that server's tools.
func TestH1UpstreamAskDisablesToolAutoApprovalRules(t *testing.T) {
	f := newAccessFixture(t, nil)
	hook := &fakeAutoApproval{}
	f.gw.SetAutoApproval(hook)
	mustApply(t, f.setPolicy(t, f.admin, "upstream", "alpha", "ask"))
	if !reflect.DeepEqual(hook.disabled, []string{"alpha.get_status", "alpha.run"}) {
		t.Fatalf("auto-approval rules disabled = %v, want every registered alpha tool (and no probe)", hook.disabled)
	}
	hook.disabled = nil
	mustApply(t, f.setPolicy(t, f.admin, "tool", "beta.get_status", "deny"))
	if !reflect.DeepEqual(hook.disabled, []string{"beta.get_status"}) {
		t.Fatalf("auto-approval rules disabled on a tool deny = %v", hook.disabled)
	}
}

// H2: an upstream-scope allow was judged only against the tools
// registered at that moment; a write registered later ran unapproved.
func TestH2UpstreamAllowIsJudgedByProbesNotLoadedTools(t *testing.T) {
	f := newAccessFixture(t, nil)
	ctx := context.Background()
	f.registerFake(t, "delta", "get_x") // the only tool loaded right now: a read

	res := f.setPolicy(t, f.admin, "upstream", "delta", "allow")
	mustReject(t, res, "delta.probe (ask → allow)")
	if _, ok := f.gw.policy.Get(policy.ScopeUpstream, "delta"); ok {
		t.Fatal("upstream allow was written on the strength of one loaded read tool")
	}
	// The write that arrives later (a reconnect loading tools one by one)
	// still asks.
	f.registerFake(t, "delta", "create_payment")
	if d := f.gw.policy.Eval(policy.Request{UpstreamName: "delta", ToolName: "delta.create_payment"}); d.Action != policy.ActionApprove {
		t.Fatalf("delta.create_payment = %+v, want approve", d)
	}
	// deny → allow for a whole server is refused too (it would open the
	// destructive probe), so an agent can never set a server-wide allow;
	// only re-state one a person set.
	if _, err := f.gw.policy.Set(ctx, policy.ScopeUpstream, "delta", "deny", "", false); err != nil {
		t.Fatal(err)
	}
	res = f.setPolicy(t, f.admin, "upstream", "delta", "allow")
	if !res.IsError || !strings.Contains(textOf(res), "delta.delete_probe (deny → allow)") {
		t.Fatalf("deny → allow on a server: %v %s", res.IsError, textOf(res))
	}
	if p, _ := f.gw.policy.Get(policy.ScopeUpstream, "delta"); p.Action != "deny" {
		t.Fatalf("server deny was changed: %+v", p)
	}
	if _, err := f.gw.policy.Set(ctx, policy.ScopeUpstream, "delta", "allow", "", false); err != nil {
		t.Fatal(err)
	}
	mustApply(t, f.setPolicy(t, f.admin, "upstream", "delta", "allow")) // a no-op re-statement
}

// H3: only policy.Set guarded destructive names, and only for a
// tool-scope allow; a clear under an upstream allow, or an upstream
// deny → allow, opened delete_* tools.
func TestH3DestructiveToolsAreNeverOpened(t *testing.T) {
	f := newAccessFixture(t, nil)
	ctx := context.Background()

	// A person allowed beta server-wide and pinned beta.delete_repo to deny.
	f.registerFake(t, "beta", "delete_repo")
	if _, err := f.gw.policy.Set(ctx, policy.ScopeUpstream, "beta", "allow", "", false); err != nil {
		t.Fatal(err)
	}
	if _, err := f.gw.policy.Set(ctx, policy.ScopeTool, "beta.delete_repo", "deny", "", false); err != nil {
		t.Fatal(err)
	}
	res := f.call(t, f.admin, "policies.clear", map[string]any{"scope": "tool", "target": "beta.delete_repo"})
	if !res.IsError || !strings.Contains(textOf(res), "would open 1 destructive-looking tool(s)") || !strings.Contains(textOf(res), "beta.delete_repo (deny → allow)") {
		t.Fatalf("clear under upstream allow: %v %s", res.IsError, textOf(res))
	}
	if d := f.gw.policy.Eval(policy.Request{UpstreamName: "beta", ToolName: "beta.delete_repo"}); d.Action != policy.ActionDeny {
		t.Fatalf("beta.delete_repo opened: %+v", d)
	}
	sc, _ := res.StructuredContent.(map[string]any)
	if sc["reason"] != "destructive_never_opened" {
		t.Fatalf("structured = %v", sc)
	}

	// A server denied by a person: an agent may not allow it, because its
	// destructive tool (and the destructive probe) would open.
	f.registerFake(t, "omega", "delete_repo")
	f.registerFake(t, "omega", "get_x")
	if _, err := f.gw.policy.Set(ctx, policy.ScopeUpstream, "omega", "deny", "", false); err != nil {
		t.Fatal(err)
	}
	res = f.setPolicy(t, f.admin, "upstream", "omega", "allow")
	if !res.IsError || !strings.Contains(textOf(res), "omega.delete_repo (deny → allow)") || !strings.Contains(textOf(res), "omega.delete_probe (deny → allow)") {
		t.Fatalf("upstream deny → allow: %v %s", res.IsError, textOf(res))
	}
	if d := f.gw.policy.Eval(policy.Request{UpstreamName: "omega", ToolName: "omega.delete_repo"}); d.Action != policy.ActionDeny {
		t.Fatalf("omega.delete_repo opened: %+v", d)
	}
	// Tool scope deny → allow on the destructive tool itself: refused
	// before policy.Set's own force check is even reached.
	res = f.setPolicy(t, f.admin, "tool", "omega.delete_repo", "allow")
	if !res.IsError || !strings.Contains(textOf(res), "omega.delete_repo (deny → allow)") {
		t.Fatalf("tool deny → allow on destructive: %v %s", res.IsError, textOf(res))
	}
	// A destructive tool a person force-allowed is not the agent's doing:
	// an unrelated change that leaves it allow is still fine.
	if _, err := f.gw.policy.Set(ctx, policy.ScopeTool, "omega.get_x", "deny", "", false); err != nil {
		t.Fatal(err)
	}
	if _, err := f.gw.policy.Set(ctx, policy.ScopeTool, "omega.delete_repo", "allow", "", true); err != nil {
		t.Fatal(err)
	}
	mustApply(t, f.setPolicy(t, f.admin, "tool", "omega.get_x", "allow"))
}

// M1: an explicit allow on a tool that already runs changed nothing
// today and pinned it open against a later server-wide ask.
func TestM1AllowOnlyLiftsADeny(t *testing.T) {
	f := newAccessFixture(t, nil)
	ctx := context.Background()
	res := f.setPolicy(t, f.admin, "tool", "alpha.get_status", "allow")
	if !res.IsError || !strings.Contains(textOf(res), "alpha.get_status is allow already") || !strings.Contains(textOf(res), "pin it open") {
		t.Fatalf("allow on an open tool: %v %s", res.IsError, textOf(res))
	}
	if sc, _ := res.StructuredContent.(map[string]any); sc["reason"] != "allow_only_lifts_deny" {
		t.Fatalf("structured = %v", sc)
	}
	if _, ok := f.gw.policy.Get(policy.ScopeTool, "alpha.get_status"); ok {
		t.Fatal("the pinning allow was written")
	}
	// So a later operator server-wide ask reaches it.
	if _, err := f.gw.policy.Set(ctx, policy.ScopeUpstream, "alpha", "ask", "", false); err != nil {
		t.Fatal(err)
	}
	if d := f.gw.policy.Eval(policy.Request{UpstreamName: "alpha", ToolName: "alpha.get_status"}); d.Action != policy.ActionApprove {
		t.Fatalf("alpha.get_status after the operator's ask = %+v", d)
	}
	// Lifting a deny is what allow is for.
	if err := f.gw.policy.DeleteTarget(ctx, policy.ScopeUpstream, "alpha"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.gw.policy.Set(ctx, policy.ScopeTool, "alpha.get_status", "deny", "", false); err != nil {
		t.Fatal(err)
	}
	mustApply(t, f.setPolicy(t, f.admin, "tool", "alpha.get_status", "allow"))
	// And ask → allow is still the ask rule's refusal, not this one.
	mustReject(t, f.setPolicy(t, f.admin, "tool", "alpha.run", "allow"), "alpha.run (ask → allow)")
}

// M2: server error text carried URL paths and userinfo.
func TestM2ServerErrorsKeepOnlySchemeAndHost(t *testing.T) {
	cases := map[string]string{
		"POST https://user:pw@mcp.example.com/mcp/secret-path?api_key=sk-SECRET failed: 401 Unauthorized": "POST https://mcp.example.com failed: 401 Unauthorized",
		"dial http://10.0.0.1:8080/v1/tools#frag: connection refused":                                     "dial http://10.0.0.1:8080: connection refused",
		"dial tcp 10.0.0.1:443: i/o timeout":                                                              "dial tcp 10.0.0.1:443: i/o timeout",
		"two https://a.example/x?k=1 and sse://b.example/y":                                               "two https://a.example and sse://b.example",
	}
	for in, want := range cases {
		if got := sanitizeServerError(in); got != want {
			t.Errorf("sanitizeServerError(%q)\n got %q\nwant %q", in, got, want)
		}
	}
	// Through servers.list and servers.reconnect alike.
	f := newAccessFixture(t, nil)
	fake := &fakeServers{rows: []ServerInfo{{Name: "alpha", Enabled: true, Status: "error", Error: "GET https://tok3n@h.example/p/q?x=1: 500"}}}
	f.gw.SetServersProvider(fake)
	res := f.call(t, f.admin, "servers.list", nil)
	if strings.Contains(textOf(res), "tok3n") || strings.Contains(textOf(res), "/p/q") || !strings.Contains(textOf(res), "GET https://h.example: 500") {
		t.Fatalf("servers.list error text: %s", textOf(res))
	}
}

// L1: a local unauthenticated caller (ScopeFor("") is All) and the
// dashboard's own callers could change policies through the tools.
func TestL1WritersNeedAnEnrolledAgent(t *testing.T) {
	in := newInboxFixture(t) // no access resolver: every caller is All
	for _, tool := range []string{"policies.set", "policies.clear", "servers.reconnect"} {
		args := map[string]any{ReasonField: testReason, "scope": "tool", "target": "deploy.get_status", "access": "deny", "server": "deploy"}
		for name, ctx := range map[string]context.Context{"anonymous": in.anonym, "dashboard": WithAgentID(context.Background(), "dashboard:u_1")} {
			res, err := in.gw.RouteCall(ctx, "test", tool, args)
			if err != nil || !res.IsError || !strings.Contains(textOf(res), "needs an enrolled agent") {
				t.Errorf("%s as %s: %v %s", tool, name, err, textOf(res))
			}
		}
	}
	if _, ok := in.pol.Get(policy.ScopeTool, "deploy.get_status"); ok {
		t.Fatal("an unauthenticated caller wrote a policy")
	}
	// An enrolled agent in the same gateway is fine (All without a resolver).
	res, err := in.gw.RouteCall(in.agent, "test", "policies.set", map[string]any{ReasonField: testReason, "scope": "tool", "target": "deploy.get_status", "access": "deny"})
	if err != nil || res.IsError {
		t.Fatalf("enrolled agent: %v %s", err, textOf(res))
	}
	// The readers stay open to everyone.
	for _, tool := range []string{"policies.list", "policies.explain", "servers.list", "access.whoami"} {
		res, err := in.gw.RouteCall(in.anonym, "test", tool, map[string]any{ReasonField: testReason, "tool": "deploy.run"})
		if err != nil || res.IsError {
			t.Errorf("%s anonymous: %v %s", tool, err, textOf(res))
		}
	}
}

// L3: an approval wait inside a script is capped by the script's clock
// (and answers with the normal timed-out snapshot).
func TestL3ApprovalWaitInCodeModeEndsWithTheScript(t *testing.T) {
	f := newLegacyAccessFixture(t)
	held := f.call(t, f.member, "alpha.run", nil)
	sc, _ := held.StructuredContent.(map[string]any)
	id, _ := sc["approval_id"].(string)
	if id == "" {
		t.Fatalf("no pending approval: %s", textOf(held))
	}
	f.gw.SetCodeModeLimits(codemode.Limits{ScriptTimeout: 4 * time.Second})
	started := time.Now()
	res := route(t, f.gw, f.member, CodeModeExecuteToolCode, map[string]any{
		"code": `result = toolyard.tools_wait_for_approval(approval_id="` + id + `", timeout_seconds=300)`,
	})
	if el := time.Since(started); el > 10*time.Second {
		t.Fatalf("the wait outlived the script clock: %s", el)
	}
	if res.IsError || !strings.Contains(textOf(res), `"status": "pending"`) {
		t.Fatalf("wait result: %v %s", res.IsError, textOf(res))
	}
	// Direct calls under a deadline get the same cap.
	ctx, cancel := context.WithTimeout(f.member, 2*time.Second)
	defer cancel()
	if got := clampToDeadline(ctx, 300); got < 1 || got > 2 {
		t.Fatalf("clampToDeadline = %d", got)
	}
	if got := clampToDeadline(context.Background(), 300); got != 300 {
		t.Fatalf("clampToDeadline without a deadline = %d", got)
	}
	var snap map[string]any
	_, after, _ := strings.Cut(textOf(res), "Return value: ")
	body, _, _ := strings.Cut(after, "\n\nEnvironment:")
	if err := json.Unmarshal([]byte(body), &snap); err != nil || snap["approval_id"] != id {
		t.Fatalf("snapshot: %v %s", err, body)
	}
}
