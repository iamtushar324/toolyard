package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tusharbhardwaj/toolyard/internal/actor"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/codemode"
	"github.com/tusharbhardwaj/toolyard/internal/inbox"
	"github.com/tusharbhardwaj/toolyard/internal/policy"
)

// setPolicy is policies.set through the gateway, with the test reason.
func (f *accessFixture) setPolicy(t *testing.T, ctx context.Context, scope, target, access string) *mcp.CallToolResult {
	t.Helper()
	return f.call(t, ctx, "policies.set", map[string]any{"scope": scope, "target": target, "access": access})
}

// mustReject asserts the ask rule refused a change naming the tool(s).
func mustReject(t *testing.T, res *mcp.CallToolResult, want ...string) {
	t.Helper()
	if !res.IsError || !strings.Contains(textOf(res), "out of ask") {
		t.Fatalf("expected the ask rule to reject: isError=%v %s", res.IsError, textOf(res))
	}
	for _, w := range want {
		if !strings.Contains(textOf(res), w) {
			t.Errorf("rejection should name %q:\n%s", w, textOf(res))
		}
	}
	sc, _ := res.StructuredContent.(map[string]any)
	if sc["status"] != "rejected" || sc["reason"] != "ask_is_final_for_agents" {
		t.Errorf("structured = %v", sc)
	}
}

func mustApply(t *testing.T, res *mcp.CallToolResult) map[string]any {
	t.Helper()
	if res.IsError {
		t.Fatalf("expected the change to apply: %s", textOf(res))
	}
	sc, _ := res.StructuredContent.(map[string]any)
	if sc["status"] != "applied" {
		t.Fatalf("structured = %v", sc)
	}
	return sc
}

// policyEvents are the policy.change audit rows for one agent, newest first.
func policyEvents(t *testing.T, f *accessFixture, agentID string) []audit.Event {
	t.Helper()
	evs, err := f.aud.Query(context.Background(), audit.Filter{EventType: EventPolicyChange, AgentID: agentID, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

func TestAccessToolsAlwaysOnAndPinned(t *testing.T) {
	f := newAccessFixture(t, nil)
	member := mcpToolsList(t, f.gw, f.member)
	for _, n := range AccessToolNames {
		if !has(member, n) {
			t.Errorf("member should see %s: %v", n, member)
		}
		if !IsPinned(n) {
			t.Errorf("%s should be pinned", n)
		}
		if e := f.gw.tools[n]; e.forcedAction == nil || *e.forcedAction != policy.ActionAllow {
			t.Errorf("%s must never need approval itself", n)
		}
	}
	if blocked := mcpToolsList(t, f.gw, f.blocked); len(blocked) != 0 {
		t.Fatalf("blocked caller sees %v", blocked)
	}
	for _, n := range []string{"policies", "servers", "audit", "access", "toolyard"} {
		if !reservedUpstreamName(n) {
			t.Errorf("%q must be a reserved upstream name", n)
		}
	}
}

func TestPoliciesSetAskRuleToolScope(t *testing.T) {
	f := newAccessFixture(t, nil)
	ctx := context.Background()

	// alpha.run is a write: ask by default. Out of ask is refused either way.
	mustReject(t, f.setPolicy(t, f.admin, "tool", "alpha.run", "allow"), "alpha.run (ask → allow)", "would take 1 tool(s) out of ask")
	mustReject(t, f.setPolicy(t, f.admin, "tool", "alpha.run", "deny"), "alpha.run (ask → deny)")
	if _, ok := f.gw.policy.Get(policy.ScopeTool, "alpha.run"); ok {
		t.Fatal("a rejected change must not be written")
	}

	// Into ask is fine (an explicit ask), and still cannot leave afterwards.
	sc := mustApply(t, f.setPolicy(t, f.admin, "tool", "alpha.run", "ask"))
	if p, ok := f.gw.policy.Get(policy.ScopeTool, "alpha.run"); !ok || p.Action != "ask" {
		t.Fatalf("policy after set = %+v %v", p, ok)
	}
	if sc["affected_tools"] != 1 || sc["changed"] == nil {
		t.Fatalf("applied structured = %v", sc)
	}
	mustReject(t, f.setPolicy(t, f.admin, "tool", "alpha.run", "deny"), "alpha.run (ask → deny)")

	// allow ↔ deny moves freely, and takes effect immediately.
	mustApply(t, f.setPolicy(t, f.admin, "tool", "alpha.get_status", "deny"))
	if a, _ := f.gw.Access(ctx, adminID, "alpha.get_status", nil); a != inbox.AccessDenied {
		t.Fatalf("alpha.get_status after deny = %s", a)
	}
	mustApply(t, f.setPolicy(t, f.admin, "tool", "alpha.get_status", "allow"))
	if a, _ := f.gw.Access(ctx, adminID, "alpha.get_status", nil); a != inbox.AccessOpen {
		t.Fatalf("alpha.get_status after allow = %s", a)
	}
	// allow → ask is fine; clearing it would be ask → allow: refused.
	mustApply(t, f.setPolicy(t, f.admin, "tool", "alpha.get_status", "ask"))
	mustReject(t, f.call(t, f.admin, "policies.clear", map[string]any{"scope": "tool", "target": "alpha.get_status"}), "alpha.get_status (ask → allow)")
	if p, ok := f.gw.policy.Get(policy.ScopeTool, "alpha.get_status"); !ok || p.Action != "ask" {
		t.Fatalf("clear must not have run: %+v %v", p, ok)
	}
	// Clearing a deny (deny → ask) is fine.
	if _, err := f.gw.policy.Set(ctx, policy.ScopeTool, "alpha.run", "deny", "", false); err != nil {
		t.Fatal(err)
	}
	res := f.call(t, f.admin, "policies.clear", map[string]any{"scope": "tool", "target": "alpha.run"})
	if res.IsError || !strings.Contains(textOf(res), `"message": "policies.clear tool alpha.run: 1 of 1 affected tool(s) change."`) {
		t.Fatalf("clear deny: %v %s", res.IsError, textOf(res))
	}
	if _, ok := f.gw.policy.Get(policy.ScopeTool, "alpha.run"); ok {
		t.Fatal("clear did not remove the policy")
	}
	res = f.call(t, f.admin, "policies.clear", map[string]any{"scope": "tool", "target": "alpha.run"})
	if res.IsError || !strings.Contains(textOf(res), "nothing to clear") {
		t.Fatalf("clear of nothing: %v %s", res.IsError, textOf(res))
	}

	// deny → allow is fine except on a destructive-looking name: agents
	// never force it, even when a person had set the deny.
	f.gw.registerEntry(toolEntry{
		tool:     mcp.Tool{Name: "alpha.delete_thing", InputSchema: mcp.ToolInputSchema{Type: "object", Properties: addMetaProps(map[string]any{})}},
		upstream: "alpha", originalName: "delete_thing", reasonField: ReasonField,
		handle: func(context.Context, map[string]any) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText("x"), nil
		},
	})
	if _, err := f.gw.policy.Set(ctx, policy.ScopeTool, "alpha.delete_thing", "deny", "", false); err != nil {
		t.Fatal(err)
	}
	res = f.setPolicy(t, f.admin, "tool", "alpha.delete_thing", "allow")
	if !res.IsError || !strings.Contains(textOf(res), "destructive-looking tool(s)") || !strings.Contains(textOf(res), "alpha.delete_thing (deny → allow)") {
		t.Fatalf("destructive allow: %v %s", res.IsError, textOf(res))
	}
	if p, _ := f.gw.policy.Get(policy.ScopeTool, "alpha.delete_thing"); p.Action != "deny" {
		t.Fatalf("destructive deny was changed: %+v", p)
	}

	// A tool that is not registered (yet) is judged by its name, as the
	// engine will judge it once it appears.
	mustReject(t, f.setPolicy(t, f.admin, "tool", "gamma.create_x", "allow"), "gamma.create_x (ask → allow)")
	mustApply(t, f.setPolicy(t, f.admin, "tool", "gamma.list_x", "deny"))

	// Bad input is a plain error, not a policy write.
	for _, args := range []map[string]any{
		{"scope": "server", "target": "alpha", "access": "ask"},
		{"scope": "tool", "target": "", "access": "ask"},
		{"scope": "tool", "target": "alpha.run", "access": "maybe"},
	} {
		if res := f.call(t, f.admin, "policies.set", args); !res.IsError {
			t.Errorf("args %v should be refused: %s", args, textOf(res))
		}
	}
}

func TestPoliciesSetAskRuleUpstreamScope(t *testing.T) {
	f := newAccessFixture(t, nil)
	ctx := context.Background()

	// Relaxing a whole server would take its default-ask write out of ask.
	res := f.setPolicy(t, f.admin, "upstream", "alpha", "allow")
	mustReject(t, res, "alpha.run (ask → allow)")
	if strings.Contains(textOf(res), "alpha.get_status") {
		t.Fatalf("get_status does not leave ask and must not be blamed:\n%s", textOf(res))
	}
	mustReject(t, f.setPolicy(t, f.admin, "upstream", "alpha", "deny"), "alpha.run (ask → deny)")

	// The whole server into ask: fine, and the read moves allow → ask.
	sc := mustApply(t, f.setPolicy(t, f.admin, "upstream", "alpha", "ask"))
	// Two registered tools and the three probes are judged; the read moves
	// allow → ask, and every ask is now one a person decides.
	changed, _ := json.Marshal(sc["changed"])
	if !strings.Contains(string(changed), `{"tool":"alpha.get_status","before":"allow","after":"ask","require_human_after":true}`) || sc["affected_tools"] != 5 {
		t.Fatalf("upstream ask: changed=%s affected=%v", changed, sc["affected_tools"])
	}
	if a, _ := f.gw.Access(ctx, adminID, "alpha.get_status", nil); a != inbox.AccessRestricted {
		t.Fatalf("alpha.get_status under upstream ask = %s", a)
	}
	// Clearing it would put the read back to allow: refused.
	mustReject(t, f.call(t, f.admin, "policies.clear", map[string]any{"scope": "upstream", "target": "alpha"}), "alpha.get_status (ask → allow)")
	// A tool rule on top is judged against what the upstream rule does:
	// the read is ask now, so neither allow nor deny may be set on it.
	mustReject(t, f.setPolicy(t, f.admin, "tool", "alpha.get_status", "allow"), "alpha.get_status (ask → allow)")
	mustReject(t, f.setPolicy(t, f.admin, "tool", "alpha.get_status", "deny"), "alpha.get_status (ask → deny)")
	mustApply(t, f.setPolicy(t, f.admin, "tool", "alpha.get_status", "ask"))

	// An upstream nobody has registered tools for is judged by a read and
	// a write probe: allow would relax the write, ask is harmless.
	mustReject(t, f.setPolicy(t, f.admin, "upstream", "gamma", "allow"), "gamma.probe (ask → allow)")
	mustApply(t, f.setPolicy(t, f.admin, "upstream", "gamma", "ask"))

	// beta is untouched by all of this.
	if a, _ := f.gw.Access(ctx, adminID, "beta.get_status", nil); a != inbox.AccessOpen {
		t.Fatalf("beta.get_status = %s", a)
	}
}

// sseSink is a goroutine-safe ResponseWriter+Flusher for Hub.ServeSSE.
type sseSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
	h   http.Header
}

func (s *sseSink) Header() http.Header { return s.h }
func (s *sseSink) WriteHeader(int)     {}
func (s *sseSink) Flush()              {}
func (s *sseSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}
func (s *sseSink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func TestPoliciesChangeIsScopedToAdminsAndAudited(t *testing.T) {
	f := newAccessFixture(t, nil)

	// A member's agent is refused in plain words, and the attempt is on record.
	res := f.setPolicy(t, f.member, "tool", "alpha.get_status", "deny")
	if !res.IsError || !strings.Contains(textOf(res), "only an admin's agent may change them") {
		t.Fatalf("member set: %v %s", res.IsError, textOf(res))
	}
	if _, ok := f.gw.policy.Get(policy.ScopeTool, "alpha.get_status"); ok {
		t.Fatal("a member's change was written")
	}
	evs := policyEvents(t, f, memberID)
	if len(evs) != 1 || evs[0].Decision != "refused" || evs[0].ToolName != "alpha.get_status" || evs[0].Reason != testReason {
		t.Fatalf("member audit rows = %+v", evs)
	}
	res = f.call(t, f.member, "policies.clear", map[string]any{"scope": "tool", "target": "alpha.get_status"})
	if !res.IsError || !strings.Contains(textOf(res), "policies.clear refused") {
		t.Fatalf("member clear: %s", textOf(res))
	}
	// A blocked caller never reaches the tool.
	if res := f.call(t, f.blocked, "policies.set", map[string]any{"scope": "tool", "target": "alpha.run", "access": "ask"}); textOf(res) != notFoundText("policies.set") {
		t.Fatalf("blocked set: %s", textOf(res))
	}

	// Live feed: the change is published as a "policy" event.
	sink := &sseSink{h: http.Header{}}
	sseCtx, cancelSSE := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.gw.hub.ServeSSE(sink, httptest.NewRequest(http.MethodGet, "/v1/stream", nil).WithContext(sseCtx))
	}()
	for i := 0; i < 100 && !strings.Contains(sink.String(), "event: open"); i++ {
		time.Sleep(10 * time.Millisecond)
	}

	// An admin's agent, with the owner the ingress named: who, what,
	// before and after are on the audit row.
	adminCtx := actor.WithRaiser(f.admin, actor.Raiser{CallerID: adminID, AgentName: "ops-bot", OwnerUserID: "u_admin", OwnerEmail: "ada@example.com"})
	mustReject(t, f.setPolicy(t, adminCtx, "tool", "alpha.run", "allow"))
	mustApply(t, f.setPolicy(t, adminCtx, "tool", "alpha.run", "ask"))
	evs = policyEvents(t, f, adminID)
	if len(evs) != 2 {
		t.Fatalf("admin audit rows = %d", len(evs))
	}
	// Both rows can share a millisecond, so pick them by decision.
	applied, rejected := evs[0], evs[1]
	if applied.Decision == "rejected" {
		applied, rejected = rejected, applied
	}
	if applied.Decision != "ask" || applied.ToolName != "alpha.run" || applied.UpstreamName != "alpha" || applied.Reason != testReason ||
		applied.AgentName != "ops-bot" || applied.OwnerUserID != "u_admin" || applied.OwnerEmail != "ada@example.com" {
		t.Fatalf("applied row = %+v", applied)
	}
	if !strings.Contains(string(applied.Arguments), `"scope":"tool"`) || !strings.Contains(string(applied.Arguments), `"target":"alpha.run"`) ||
		!strings.Contains(applied.ResultSummary, "policies.set tool alpha.run → ask: 1 of 1 affected tool(s) change") {
		t.Fatalf("applied row details = %s / %s", applied.Arguments, applied.ResultSummary)
	}
	// (The audit redactor re-encodes arguments, so keys come back sorted.)
	if rejected.Decision != "rejected" || !strings.Contains(string(rejected.Arguments), `{"after":"allow","before":"ask","tool":"alpha.run"}`) {
		t.Fatalf("rejected row = %+v %s", rejected, rejected.Arguments)
	}
	for i := 0; i < 100 && !strings.Contains(sink.String(), "event: policy"); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	cancelSSE()
	<-done
	feed := sink.String()
	if !strings.Contains(feed, "event: policy\ndata: ") || !strings.Contains(feed, `"target":"alpha.run"`) || !strings.Contains(feed, `"owner_email":"ada@example.com"`) {
		t.Fatalf("live feed:\n%s", feed)
	}
	// The audit row itself reaches the feed too (main.go bridges the
	// audit subscription); here the tool's own event is what we check.

	// The anti-fight hook fires for ask/deny on a tool, like the dashboard.
	hook := &fakeAutoApproval{}
	f.gw.SetAutoApproval(hook)
	mustApply(t, f.setPolicy(t, f.admin, "tool", "alpha.get_status", "deny"))
	mustApply(t, f.setPolicy(t, f.admin, "tool", "alpha.get_status", "allow"))
	if !reflect.DeepEqual(hook.disabled, []string{"alpha.get_status"}) {
		t.Fatalf("auto-approval hook calls = %v", hook.disabled)
	}
}

type fakeAutoApproval struct{ disabled []string }

func (f *fakeAutoApproval) SetToolPolicy(_ context.Context, tool string, auto bool) error {
	if !auto {
		f.disabled = append(f.disabled, tool)
	}
	return nil
}

func TestPoliciesExplainAndList(t *testing.T) {
	f := newAccessFixture(t, nil)
	ctx := context.Background()

	res := f.call(t, f.member, "policies.explain", map[string]any{"tool": "alpha.run"})
	if res.IsError || !strings.HasPrefix(textOf(res), "alpha.run needs approval (ask): no explicit policy; its name does not look read-only, so toolyard's default applies: writes need approval.") ||
		!strings.Contains(textOf(res), "only a person on the dashboard can take a tool out of ask") {
		t.Fatalf("explain alpha.run: %v %s", res.IsError, textOf(res))
	}
	sc, _ := res.StructuredContent.(map[string]any)
	if sc["access"] != AccessAsk || sc["rule_id"] != "v0.1-default-write" || sc["server"] != "alpha" {
		t.Fatalf("explain structured = %v", sc)
	}
	res = f.call(t, f.member, "policies.explain", map[string]any{"tool": "alpha.get_status"})
	if !strings.HasPrefix(textOf(res), "alpha.get_status runs without approval (allow): no explicit policy; its name starts with a read verb") ||
		!strings.Contains(textOf(res), "_intent_category write, destructive") {
		t.Fatalf("explain alpha.get_status: %s", textOf(res))
	}
	res = f.call(t, f.member, "policies.explain", map[string]any{"tool": "inbox.guide"})
	if !strings.Contains(textOf(res), "built-in toolyard tool: always allow, no policy applies") {
		t.Fatalf("explain inbox.guide: %s", textOf(res))
	}
	// A server the member was never granted reads as an unknown tool.
	if res := f.call(t, f.member, "policies.explain", map[string]any{"tool": "beta.run"}); textOf(res) != notFoundText("beta.run") {
		t.Fatalf("member explaining beta.run: %s", textOf(res))
	}
	if res := f.call(t, f.member, "policies.explain", map[string]any{}); !res.IsError || !strings.Contains(textOf(res), "tool is required") {
		t.Fatalf("explain without tool: %s", textOf(res))
	}
	// An explicit rule is named as such.
	mustApply(t, f.setPolicy(t, f.admin, "tool", "alpha.run", "ask"))
	res = f.call(t, f.member, "policies.explain", map[string]any{"tool": "alpha.run"})
	if !strings.Contains(textOf(res), "an explicit tool policy (tp_") || res.StructuredContent.(map[string]any)["tool_policy"] == nil {
		t.Fatalf("explain explicit: %s", textOf(res))
	}

	// policies.list: a member sees the tools and policies in their scope only.
	if _, err := f.gw.policy.Set(ctx, policy.ScopeUpstream, "beta", "deny", "members can't see this", false); err != nil {
		t.Fatal(err)
	}
	res = f.call(t, f.member, "policies.list", nil)
	if res.IsError {
		t.Fatalf("list: %s", textOf(res))
	}
	var listed struct {
		Policies []policy.ToolPolicy `json:"policies"`
		Tools    []effectiveRow      `json:"tools"`
	}
	if err := json.Unmarshal([]byte(textOf(res)), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Policies) != 1 || listed.Policies[0].Target != "alpha.run" {
		t.Fatalf("member policies = %+v", listed.Policies)
	}
	if strings.Contains(textOf(res), "beta") {
		t.Fatalf("member list leaks beta:\n%s", textOf(res))
	}
	byTool := map[string]effectiveRow{}
	for _, r := range listed.Tools {
		byTool[r.Tool] = r
	}
	if byTool["alpha.run"].Access != AccessAsk || byTool["alpha.get_status"].Access != AccessAllow || byTool["inbox.guide"].Access != AccessAllow || byTool["inbox.guide"].Server != "inbox" {
		t.Fatalf("member effective rows = %+v", byTool)
	}
	if _, ok := byTool["memory.get"]; ok {
		t.Fatal("member sees an ungranted built-in group")
	}
	// Filtered by server, and the admin sees everything.
	res = f.call(t, f.member, "policies.list", map[string]any{"server": "alpha"})
	if err := json.Unmarshal([]byte(textOf(res)), &listed); err != nil {
		t.Fatal(err)
	}
	for _, r := range listed.Tools {
		if r.Server != "alpha" {
			t.Fatalf("server filter leaked %+v", r)
		}
	}
	res = f.call(t, f.admin, "policies.list", nil)
	if err := json.Unmarshal([]byte(textOf(res)), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Policies) != 2 || !strings.Contains(textOf(res), "beta.run") || !strings.Contains(textOf(res), "memory.get") {
		t.Fatalf("admin list: %d policies\n%s", len(listed.Policies), textOf(res))
	}
}

// fakeServers is a ServersProvider that records what it was asked.
type fakeServers struct {
	mu          sync.Mutex
	owners      []string
	reconnected []string
	failOn      string
	rows        []ServerInfo
}

func (s *fakeServers) ListServers(_ context.Context, owner string) ([]ServerInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.owners = append(s.owners, owner)
	out := make([]ServerInfo, len(s.rows))
	copy(out, s.rows)
	return out, nil
}

func (s *fakeServers) ReconnectServer(_ context.Context, name string) (*ServerInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reconnected = append(s.reconnected, name)
	if name == s.failOn {
		return &ServerInfo{Name: name, Status: "error", Error: "dial tcp: connection refused"}, errors.New("dial tcp: connection refused")
	}
	return &ServerInfo{Name: name, Status: "ok", ToolCount: 3}, nil
}

func TestServersListIsScopedAndCarriesNoSecrets(t *testing.T) {
	f := newAccessFixture(t, nil)

	// Without an upstream service the gateway answers from its own pool,
	// which in this fixture holds nothing.
	res := f.call(t, f.admin, "servers.list", nil)
	if res.IsError || !strings.Contains(textOf(res), `"count": 0`) {
		t.Fatalf("pool fallback: %v %s", res.IsError, textOf(res))
	}

	yes, no := true, false
	fake := &fakeServers{rows: []ServerInfo{
		{Name: "alpha", Transport: "http", Enabled: true, Status: "ok", ToolCount: 2, AuthMode: "shared", OAuth: true, SignedIn: &yes},
		{Name: "beta", Transport: "http", Enabled: true, Status: "waiting_signin", AuthMode: "per_user", OAuth: true, SignedIn: &no},
		{Name: "gamma", Transport: "http", Enabled: true, Status: "error", Error: "POST https://mcp.example.com/mcp?api_key=sk-SECRET-VALUE failed: 401 Unauthorized", AuthMode: "shared"},
	}}
	f.gw.SetServersProvider(fake)

	memberCtx := actor.WithRaiser(f.member, actor.Raiser{CallerID: memberID, OwnerUserID: "u_member"})
	res = f.call(t, memberCtx, "servers.list", nil)
	if res.IsError {
		t.Fatalf("member list: %s", textOf(res))
	}
	var out struct {
		Servers []ServerInfo `json:"servers"`
		Count   int          `json:"count"`
	}
	if err := json.Unmarshal([]byte(textOf(res)), &out); err != nil {
		t.Fatal(err)
	}
	if out.Count != 1 || out.Servers[0].Name != "alpha" || out.Servers[0].SignedIn == nil || !*out.Servers[0].SignedIn || out.Servers[0].AuthMode != "shared" {
		t.Fatalf("member servers = %+v", out)
	}
	if !reflect.DeepEqual(fake.owners, []string{"u_member"}) {
		t.Fatalf("provider asked for owners %v", fake.owners)
	}

	res = f.call(t, f.admin, "servers.list", nil)
	if err := json.Unmarshal([]byte(textOf(res)), &out); err != nil {
		t.Fatal(err)
	}
	if out.Count != 3 || out.Servers[1].Name != "beta" || out.Servers[1].Status != StatusWaitingSignIn || *out.Servers[1].SignedIn {
		t.Fatalf("admin servers = %+v", out)
	}
	text := textOf(res)
	if strings.Contains(text, "SECRET") || strings.Contains(text, "api_key") || strings.Contains(text, "/mcp/") || !strings.Contains(out.Servers[2].Error, "POST https://mcp.example.com failed: 401 Unauthorized") {
		t.Fatalf("error text not sanitised: %s", out.Servers[2].Error)
	}
	for _, forbidden := range []string{`"url"`, `"headers"`, `"env"`, `"command"`, `"token"`, "Authorization"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("servers.list carries %s:\n%s", forbidden, text)
		}
	}
}

func TestServersReconnectIsAdminOnlyAndAudited(t *testing.T) {
	f := newAccessFixture(t, nil)
	reconnects := func(agentID string) []audit.Event {
		evs, err := f.aud.Query(context.Background(), audit.Filter{EventType: EventServerReconnect, AgentID: agentID, Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		return evs
	}

	res := f.call(t, f.member, "servers.reconnect", map[string]any{"server": "alpha"})
	if !res.IsError || !strings.Contains(textOf(res), "only an admin's agent may reconnect") {
		t.Fatalf("member reconnect: %v %s", res.IsError, textOf(res))
	}
	if evs := reconnects(memberID); len(evs) != 1 || evs[0].Decision != "refused" || evs[0].UpstreamName != "alpha" {
		t.Fatalf("member audit = %+v", evs)
	}
	res = f.call(t, f.admin, "servers.reconnect", map[string]any{"server": "alpha"})
	if !res.IsError || !strings.Contains(textOf(res), "not available on this gateway") {
		t.Fatalf("reconnect without a provider: %s", textOf(res))
	}

	fake := &fakeServers{failOn: "gamma"}
	f.gw.SetServersProvider(fake)
	res = f.call(t, f.admin, "servers.reconnect", map[string]any{"server": "alpha"})
	if res.IsError || !strings.Contains(textOf(res), `"status": "reconnected"`) || !strings.Contains(textOf(res), `"tool_count": 3`) {
		t.Fatalf("admin reconnect: %v %s", res.IsError, textOf(res))
	}
	res = f.call(t, f.admin, "servers.reconnect", map[string]any{"server": "gamma"})
	if !res.IsError || !strings.Contains(textOf(res), "connection refused") {
		t.Fatalf("failed reconnect: %v %s", res.IsError, textOf(res))
	}
	if !reflect.DeepEqual(fake.reconnected, []string{"alpha", "gamma"}) {
		t.Fatalf("provider reconnects = %v", fake.reconnected)
	}
	evs := reconnects(adminID)
	byServer := map[string]audit.Event{}
	for _, e := range evs {
		byServer[e.UpstreamName] = e
	}
	if len(evs) != 2 || byServer["gamma"].Decision != "error" || byServer["alpha"].Decision != "ok" ||
		byServer["alpha"].Reason != testReason || !strings.Contains(byServer["alpha"].ResultSummary, "3 tool(s)") {
		t.Fatalf("admin audit = %+v", evs)
	}
	if res := f.call(t, f.admin, "servers.reconnect", nil); !res.IsError || !strings.Contains(textOf(res), "server is required") {
		t.Fatalf("missing server: %s", textOf(res))
	}
}

func TestAuditMineOnlyOwnRowsWithoutBodies(t *testing.T) {
	f := newAccessFixture(t, nil)
	// Rows for two agents.
	for i := 0; i < 3; i++ {
		if res := f.call(t, f.member, "alpha.get_status", map[string]any{"n": i}); res.IsError {
			t.Fatal(textOf(res))
		}
	}
	if res := f.call(t, f.admin, "alpha.get_status", nil); res.IsError {
		t.Fatal(textOf(res))
	}
	f.call(t, f.admin, "alpha.run", nil) // held for approval: a decision row too
	logged, err := f.aud.Query(context.Background(), audit.Filter{AgentID: memberID, Limit: 200})
	if err != nil {
		t.Fatal(err)
	}

	res := f.call(t, f.member, "audit.mine", nil)
	if res.IsError {
		t.Fatalf("audit.mine: %s", textOf(res))
	}
	var out struct {
		AgentID string         `json:"agent_id"`
		Count   int            `json:"count"`
		Calls   []auditMineRow `json:"calls"`
	}
	if err := json.Unmarshal([]byte(textOf(res)), &out); err != nil {
		t.Fatal(err)
	}
	if out.AgentID != memberID || out.Count == 0 || len(out.Calls) != out.Count {
		t.Fatalf("mine = %+v", out)
	}
	// Everything logged so far, plus the start of the audit.mine call itself.
	if out.Count < len(logged) || out.Count > len(logged)+3 {
		t.Fatalf("mine has %d rows, the log had %d for this agent", out.Count, len(logged))
	}
	seenTool := false
	for _, c := range out.Calls {
		if c.Tool == "alpha.run" || c.Event == EventPolicyChange {
			t.Fatalf("another agent's row leaked: %+v", c)
		}
		if c.Tool == "alpha.get_status" && c.Event == audit.EventCallSucceeded && c.Decision == "" && c.Reason == testReason {
			seenTool = true
		}
	}
	if !seenTool {
		t.Fatalf("own calls missing: %+v", out.Calls)
	}
	for _, forbidden := range []string{`"arguments"`, `"result_summary"`, `"n": 1`, `"ran"`} {
		if strings.Contains(textOf(res), forbidden) {
			t.Errorf("audit.mine carries %s:\n%s", forbidden, textOf(res))
		}
	}
	// limit and since bound the query; an impossible since finds nothing.
	res = f.call(t, f.member, "audit.mine", map[string]any{"limit": 1.0})
	if err := json.Unmarshal([]byte(textOf(res)), &out); err != nil || out.Count != 1 {
		t.Fatalf("limit 1: %v %s", err, textOf(res))
	}
	res = f.call(t, f.member, "audit.mine", map[string]any{"since": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
	if err := json.Unmarshal([]byte(textOf(res)), &out); err != nil || out.Count != 0 {
		t.Fatalf("future since: %v %s", err, textOf(res))
	}
	if res := f.call(t, f.member, "audit.mine", map[string]any{"since": "yesterday"}); !res.IsError {
		t.Fatalf("bad since accepted: %s", textOf(res))
	}
	res = f.call(t, f.member, "audit.mine", map[string]any{"since_minutes": 5.0})
	if err := json.Unmarshal([]byte(textOf(res)), &out); err != nil || out.Count == 0 {
		t.Fatalf("since_minutes: %v %s", err, textOf(res))
	}

	// Anonymous local callers have no rows of their own to show.
	in := newInboxFixture(t)
	anon, err := in.gw.RouteCall(in.anonym, "test", "audit.mine", map[string]any{ReasonField: testReason})
	if err != nil || !anon.IsError || !strings.Contains(textOf(anon), "needs an enrolled agent") {
		t.Fatalf("anonymous audit.mine: %v %s", err, textOf(anon))
	}
}

func TestAccessWhoami(t *testing.T) {
	f := newAccessFixture(t, nil)
	whoami := func(ctx context.Context) map[string]any {
		t.Helper()
		res := f.call(t, ctx, "access.whoami", nil)
		if res.IsError {
			t.Fatalf("whoami: %s", textOf(res))
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(textOf(res)), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	memberCtx := actor.WithRaiser(f.member, actor.Raiser{CallerID: memberID, AgentName: "writer", AgentKind: "agent", OwnerUserID: "u_member", OwnerEmail: "mia@example.com", ClientKind: "claude_code"})
	m := whoami(memberCtx)
	owner, _ := m["owner"].(map[string]any)
	if m["caller_id"] != memberID || m["agent_name"] != "writer" || owner["role"] != "member" || owner["email"] != "mia@example.com" ||
		m["can_change_policies"] != false || !reflect.DeepEqual(m["granted_servers"], []any{"alpha"}) {
		t.Fatalf("member whoami = %v", m)
	}
	if client, _ := m["client"].(map[string]any); client["kind"] != "claude_code" {
		t.Fatalf("client = %v", client)
	}
	always, _ := m["always_on"].([]any)
	if !reflect.DeepEqual(always, []any{"access", "audit", "connections", "inbox", "policies", "servers", "session", "tools"}) {
		t.Fatalf("always_on = %v", always)
	}
	a := whoami(f.admin)
	owner, _ = a["owner"].(map[string]any)
	if owner["role"] != "admin" || a["granted_servers"] != "all" || a["can_change_policies"] != true || a["can_reconnect_servers"] != true {
		t.Fatalf("admin whoami = %v", a)
	}
}

// TestCodeModeToolyardNamespace: every internal tool is callable from a
// script as toolyard.<group>_<name>, with the same scoping, policy rule,
// audit and result as a direct call.
func TestCodeModeToolyardNamespace(t *testing.T) {
	f := newAccessFixture(t, nil)
	ctx := context.Background()

	// The stub shows the tool's real signature.
	res := route(t, f.gw, f.member, CodeModeReadToolFile, map[string]any{"fileName": "servers/toolyard/policies_set.pyi"})
	if res.IsError {
		t.Fatalf("readToolFile: %s", textOf(res))
	}
	if !strings.Contains(textOf(res), "# toolyard.policies_set tool\n") ||
		!strings.Contains(textOf(res), `def policies_set(access: Literal["allow", "ask", "deny"], scope: Literal["tool", "upstream"], target: str, note: str = None) -> dict:`) {
		t.Fatalf("policies_set stub:\n%s", textOf(res))
	}
	whole := textOf(route(t, f.gw, f.admin, CodeModeReadToolFile, map[string]any{"fileName": "servers/toolyard.pyi"}))
	for _, want := range []string{"def inbox_request(", "def memory_get(key: str", "def tools_poll_approval(approval_id: str", "def session_start(title: str", "def access_whoami() -> dict", "def audit_mine("} {
		if !strings.Contains(whole, want) {
			t.Errorf("toolyard.pyi missing %q", want)
		}
	}
	for _, never := range []string{"def tools_executeToolCode", "def tools_listToolFiles", "executetoolcode", "def tools_readToolFile", "def tools_getToolDocs"} {
		if strings.Contains(whole, never) {
			t.Errorf("code mode must not bind itself: %q\n%s", never, whole)
		}
	}
	docs := textOf(route(t, f.gw, f.member, CodeModeGetToolDocs, map[string]any{"server": "toolyard", "tool": "policies_explain"}))
	if !strings.Contains(docs, "# Documentation for toolyard.policies_explain tool") || !strings.Contains(docs, "tool (str): Catalog name, e.g. github.create_issue. (required)") {
		t.Fatalf("docs:\n%s", docs)
	}

	// policies.set from a script: the ask rule, word for word.
	direct := f.setPolicy(t, f.admin, "tool", "alpha.run", "allow")
	res = route(t, f.gw, f.admin, CodeModeExecuteToolCode, map[string]any{
		"code": `result = toolyard.policies_set(scope="tool", target="alpha.run", access="allow")`, ReasonField: testReason,
	})
	if !res.IsError || !strings.Contains(textOf(res), "tool call failed for toolyard.policies_set: "+textOf(direct)) {
		t.Fatalf("script rejection should carry the direct text %q:\n%s", textOf(direct), textOf(res))
	}
	if _, ok := f.gw.policy.Get(policy.ScopeTool, "alpha.run"); ok {
		t.Fatal("a rejected change was written via code mode")
	}
	// A member's agent is refused the same way.
	direct = f.setPolicy(t, f.member, "tool", "alpha.get_status", "deny")
	res = route(t, f.gw, f.member, CodeModeExecuteToolCode, map[string]any{
		"code": `result = toolyard.policies_set(scope="tool", target="alpha.get_status", access="deny")`, ReasonField: testReason,
	})
	if !res.IsError || !strings.Contains(textOf(res), "tool call failed for toolyard.policies_set: "+textOf(direct)) {
		t.Fatalf("member script refusal:\n%s", textOf(res))
	}
	// An allowed change applies, is audited under via code_mode, and is live.
	res = route(t, f.gw, f.admin, CodeModeExecuteToolCode, map[string]any{
		"code": "# deny alpha.get_status for everyone\nresult = toolyard.policies_set(scope=\"tool\", target=\"alpha.get_status\", access=\"deny\")",
	})
	if res.IsError || !strings.Contains(textOf(res), `"status": "applied"`) || !strings.Contains(textOf(res), `"message": "policies.set tool alpha.get_status → deny: 1 of 1 affected tool(s) change."`) {
		t.Fatalf("script set:\n%s", textOf(res))
	}
	if a, _ := f.gw.Access(ctx, adminID, "alpha.get_status", nil); a != inbox.AccessDenied {
		t.Fatalf("alpha.get_status after script = %s", a)
	}
	evs := policyEvents(t, f, adminID)
	if len(evs) == 0 || evs[0].Decision != "deny" || evs[0].Via != codemode.Via || evs[0].Reason != "code mode: deny alpha.get_status for everyone" {
		t.Fatalf("code-mode policy audit row = %+v", evs)
	}
	if ev, ok := lastEvent(f.met, "policies.set"); !ok || ev.Via != codemode.Via || ev.AgentID != adminID {
		t.Fatalf("policies.set metric = %+v", ev)
	}

	// A read-only tool returns exactly what the direct call returns.
	directWho := f.call(t, f.member, "access.whoami", nil)
	res = route(t, f.gw, f.member, CodeModeExecuteToolCode, map[string]any{"code": "result = toolyard.access_whoami()"})
	if res.IsError {
		t.Fatalf("whoami script: %s", textOf(res))
	}
	_, after, ok := strings.Cut(textOf(res), "Return value: ")
	scriptJSON, _, _ := strings.Cut(after, "\n\nEnvironment:")
	if !ok {
		t.Fatalf("no return value:\n%s", textOf(res))
	}
	var fromScript, fromDirect map[string]any
	if err := json.Unmarshal([]byte(scriptJSON), &fromScript); err != nil {
		t.Fatalf("script json: %v\n%s", err, scriptJSON)
	}
	if err := json.Unmarshal([]byte(textOf(directWho)), &fromDirect); err != nil {
		t.Fatal(err)
	}
	// The path in differs (via), nothing else.
	delete(fromScript, "client")
	delete(fromDirect, "client")
	if !reflect.DeepEqual(fromScript, fromDirect) {
		t.Fatalf("whoami via code mode %v\n!= direct %v", fromScript, fromDirect)
	}
	if ev, ok := lastEvent(f.met, "access.whoami"); !ok || ev.Via != codemode.Via {
		t.Fatalf("whoami metric = %+v", ev)
	}

	// Scope still applies in the namespace: a member has no memory tools.
	res = route(t, f.gw, f.member, CodeModeExecuteToolCode, map[string]any{"code": `result = toolyard.memory_get(key="k")`})
	if !res.IsError || !strings.Contains(textOf(res), "has no .memory_get") {
		t.Fatalf("member calling memory via toolyard: %s", textOf(res))
	}
	// And code mode cannot nest itself through tools.execute.
	res = route(t, f.gw, f.admin, CodeModeExecuteToolCode, map[string]any{
		"code": `result = toolyard.tools_execute(tool="executeToolCode", arguments={"code": "result = 1"})`,
	})
	if !res.IsError || !strings.Contains(textOf(res), "executeToolCode cannot be called from code mode") {
		t.Fatalf("nested code mode: %s", textOf(res))
	}
	// tools.execute to an ordinary tool still works from a script (the
	// policy on its target still applies: alpha.get_status is deny now).
	res = route(t, f.gw, f.member, CodeModeExecuteToolCode, map[string]any{
		"code": `result = toolyard.tools_execute(tool="alpha.get_status", arguments={})`,
	})
	if !res.IsError || !strings.Contains(textOf(res), "denied by policy: explicit tool-policy: deny") {
		t.Fatalf("tools.execute to a denied tool from a script: %s", textOf(res))
	}
	res = route(t, f.gw, f.member, CodeModeExecuteToolCode, map[string]any{
		"code": `result = toolyard.tools_execute(tool="access.whoami", arguments={})`,
	})
	if res.IsError || !strings.Contains(textOf(res), `"caller_id": "ag_member"`) {
		t.Fatalf("tools.execute from a script: %s", textOf(res))
	}
}
