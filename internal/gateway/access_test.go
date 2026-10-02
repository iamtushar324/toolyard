package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tusharbhardwaj/toolyard/internal/access"
	"github.com/tusharbhardwaj/toolyard/internal/approval"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/inbox"
	"github.com/tusharbhardwaj/toolyard/internal/memory"
	"github.com/tusharbhardwaj/toolyard/internal/metrics"
	"github.com/tusharbhardwaj/toolyard/internal/policy"
	"github.com/tusharbhardwaj/toolyard/internal/realtime"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// fakeResolver is a map-based access.Resolver. Unknown callers are Denied,
// like the real service.
type fakeResolver struct {
	mu     sync.Mutex
	scopes map[string]access.Scope
}

func (f *fakeResolver) ScopeFor(_ context.Context, callerID string) access.Scope {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.scopes[callerID]; ok {
		return s
	}
	return access.Scope{Denied: true}
}

func (f *fakeResolver) set(callerID string, s access.Scope) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scopes[callerID] = s
}

type fakeMetrics struct {
	mu     sync.Mutex
	events []metrics.Event
}

func (m *fakeMetrics) Record(e metrics.Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, e)
}

func (m *fakeMetrics) find(tool, outcome, class string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.events {
		if e.ToolName == tool && e.Outcome == outcome && e.ErrorClass == class {
			return true
		}
	}
	return false
}

// fakeVisibility passes every tool through but records what it was shown,
// and hides one tool from direct calls.
type fakeVisibility struct {
	mu     sync.Mutex
	seen   []string
	hidden string
}

func (v *fakeVisibility) List(_ context.Context, tools []mcp.Tool) []mcp.Tool {
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, t := range tools {
		v.seen = append(v.seen, t.Name)
	}
	return tools
}

func (v *fakeVisibility) IsVisible(_ context.Context, name string) bool { return name != v.hidden }

func groups(gs ...string) access.Scope {
	s := access.Scope{Groups: map[string]bool{}}
	for _, g := range gs {
		s.Groups[g] = true
	}
	return s
}

type accessFixture struct {
	gw      *Gateway
	svc     *inbox.Service
	bus     *approval.Bus
	aud     *audit.Logger
	res     *fakeResolver
	met     *fakeMetrics
	calls   atomic.Int32
	member  context.Context // granted alpha only
	admin   context.Context // All
	blocked context.Context // Denied
}

const (
	memberID  = "ag_member"
	adminID   = "ag_admin"
	blockedID = "ag_blocked"
)

// newAccessFixture wires a gateway with the fake resolver and two upstream
// groups: alpha.{get_status,run} and beta.{get_status,run}. get_status is a
// read (auto-allowed), run is a write (needs approval).
func newAccessFixture(t *testing.T, vis VisibilityProvider) *accessFixture {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "g.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	bus, err := approval.New(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	f := &accessFixture{
		bus: bus,
		aud: audit.New(db),
		met: &fakeMetrics{},
		res: &fakeResolver{scopes: map[string]access.Scope{
			memberID:  groups("alpha"),
			adminID:   {All: true},
			blockedID: {Denied: true},
		}},
	}
	f.gw = New(Options{
		Policy: policy.New(db), Approval: bus, Audit: f.aud, Hub: realtime.NewHub(), Memory: memory.New(db),
		Metrics: f.met, Access: f.res, Visibility: vis,
	})
	f.gw.RegisterBuiltins()
	t.Cleanup(func() { _ = f.gw.Close() })
	f.svc, err = inbox.New(context.Background(), inbox.Options{DB: db, Catalog: f.gw})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.svc.Flush)
	f.gw.SetInbox(f.svc, inbox.NewGuide(nil), func() string { return ApprovalModeExecute })
	f.gw.RegisterInboxTools()

	handler := func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		f.calls.Add(1)
		return mcp.NewToolResultText("ran"), nil
	}
	for _, up := range []string{"alpha", "beta"} {
		for _, short := range []string{"get_status", "run"} {
			f.gw.registerEntry(toolEntry{
				tool:     mcp.Tool{Name: up + "." + short, InputSchema: mcp.ToolInputSchema{Type: "object", Properties: addMetaProps(map[string]any{})}},
				upstream: up, originalName: short, reasonField: ReasonField, handle: handler,
			})
		}
	}
	f.member = WithAgentID(context.Background(), memberID)
	f.admin = WithAgentID(context.Background(), adminID)
	f.blocked = WithAgentID(context.Background(), blockedID)
	return f
}

func (f *accessFixture) call(t *testing.T, ctx context.Context, tool string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	full := map[string]any{ReasonField: testReason}
	for k, v := range args {
		full[k] = v
	}
	res, err := f.gw.RouteCall(ctx, "test", tool, full)
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	return res
}

// mcpToolsList runs tools/list through the MCP server so the registered tool
// filters apply, and returns the tool names.
func mcpToolsList(t *testing.T, g *Gateway, ctx context.Context) []string {
	t.Helper()
	raw := g.MCPServer().HandleMessage(ctx, json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	b, _ := json.Marshal(raw)
	var out struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
		Error any `json:"error"`
	}
	if err := json.Unmarshal(b, &out); err != nil || out.Error != nil {
		t.Fatalf("tools/list: %v %s", err, b)
	}
	names := make([]string, 0, len(out.Result.Tools))
	for _, tl := range out.Result.Tools {
		names = append(names, tl.Name)
	}
	return names
}

// rpcReply is a decoded raw JSON-RPC tools/call response: either a tool
// result (isError/text) or a protocol error (code/message).
type rpcReply struct {
	raw     []byte
	rpcErr  bool
	code    int
	message string
	isError bool
	text    string
}

// mcpToolsCall runs tools/call through the MCP server, as a raw client would.
func mcpToolsCall(t *testing.T, g *Gateway, ctx context.Context, tool string) rpcReply {
	t.Helper()
	msg, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": map[string]any{ReasonField: testReason}},
	})
	raw := g.MCPServer().HandleMessage(ctx, json.RawMessage(msg))
	b, _ := json.Marshal(raw)
	var out struct {
		Result *struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(b, &out); err != nil || (out.Result == nil) == (out.Error == nil) {
		t.Fatalf("tools/call %s: %v %s", tool, err, b)
	}
	r := rpcReply{raw: b}
	if out.Error != nil {
		r.rpcErr, r.code, r.message = true, out.Error.Code, out.Error.Message
		return r
	}
	r.isError = out.Result.IsError
	var sb strings.Builder
	for _, c := range out.Result.Content {
		sb.WriteString(c.Text)
	}
	r.text = sb.String()
	return r
}

// rpcNotFoundMessage is mcp-go's own message for a tools/call naming a tool
// it doesn't have (server/server.go handleToolCall, v0.52.0).
func rpcNotFoundMessage(tool string) string {
	return fmt.Sprintf("tool '%s' not found: tool not found", tool)
}

func has(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

func hasPrefix(names []string, prefix string) bool {
	for _, n := range names {
		if strings.HasPrefix(n, prefix) {
			return true
		}
	}
	return false
}

func catalogNames(entries []CatalogEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name)
	}
	sort.Strings(out)
	return out
}

func notFoundText(tool string) string { return fmt.Sprintf("tool %q not found in catalog", tool) }

func TestAccessToolsListFiltersByScope(t *testing.T) {
	vis := &fakeVisibility{hidden: "beta.get_status"}
	f := newAccessFixture(t, vis)

	member := mcpToolsList(t, f.gw, f.member)
	if !has(member, "alpha.get_status") || !has(member, "alpha.run") {
		t.Fatalf("member should see alpha.*: %v", member)
	}
	if hasPrefix(member, "beta.") {
		t.Fatalf("member must not see beta.*: %v", member)
	}
	for _, n := range []string{"tools.search", "tools.execute", "inbox.guide", "inbox.check", "session.start"} {
		if !has(member, n) {
			t.Errorf("member should see always-on %s: %v", n, member)
		}
	}
	// memory.* is the built-in group "memory", not granted to the member.
	if hasPrefix(member, "memory.") {
		t.Fatalf("member must not see ungranted built-in group memory: %v", member)
	}
	// Access runs before visibility: the visibility provider never sees beta.*.
	vis.mu.Lock()
	seen := append([]string(nil), vis.seen...)
	vis.mu.Unlock()
	if len(seen) == 0 || hasPrefix(seen, "beta.") {
		t.Fatalf("visibility filter should run after access and never see beta.*: %v", seen)
	}

	admin := mcpToolsList(t, f.gw, f.admin)
	if !has(admin, "beta.run") || !has(admin, "memory.get") || !has(admin, "alpha.run") {
		t.Fatalf("admin should see everything: %v", admin)
	}
	if blocked := mcpToolsList(t, f.gw, f.blocked); len(blocked) != 0 {
		t.Fatalf("blocked caller should see nothing, got %v", blocked)
	}
	if unknown := mcpToolsList(t, f.gw, WithAgentID(context.Background(), "ag_nobody")); len(unknown) != 0 {
		t.Fatalf("unknown caller should see nothing, got %v", unknown)
	}

	// A direct MCP call to a hidden, ungranted tool answers as an unknown
	// tool, not with the surface-mode hint (which would confirm it exists).
	if r := mcpToolsCall(t, f.gw, f.member, "beta.get_status"); !r.rpcErr || r.message != rpcNotFoundMessage("beta.get_status") {
		t.Fatalf("hidden+ungranted direct call: %s", r.raw)
	}
	// The same hidden tool for an admin still gets the surface-mode hint.
	if r := mcpToolsCall(t, f.gw, f.admin, "beta.get_status"); r.rpcErr || !r.isError || !strings.Contains(r.text, "hidden by the current agent-surface mode") {
		t.Fatalf("admin hidden direct call: %s", r.raw)
	}
	if f.calls.Load() != 0 {
		t.Fatal("no handler should have run")
	}
}

func TestAccessCallsToUngrantedGroupLookLikeUnknownTools(t *testing.T) {
	f := newAccessFixture(t, nil)

	// Raw MCP tools/call: a protocol-level not-found (see
	// TestAccessRawMCPCallMatchesUnknownTool for the byte comparison).
	if r := mcpToolsCall(t, f.gw, f.member, "beta.get_status"); !r.rpcErr || r.message != rpcNotFoundMessage("beta.get_status") {
		t.Fatalf("mcp direct: %s", r.raw)
	}
	// RouteCall.
	res := f.call(t, f.member, "beta.get_status", nil)
	if !res.IsError || textOf(res) != notFoundText("beta.get_status") {
		t.Fatalf("RouteCall: %v %q", res.IsError, textOf(res))
	}
	// Same text as a genuinely unknown tool.
	if unknown := f.call(t, f.member, "nope.tool", nil); textOf(unknown) != notFoundText("nope.tool") {
		t.Fatalf("unknown tool text changed: %q", textOf(unknown))
	}
	// tools.execute.
	res = f.call(t, f.member, "tools.execute", map[string]any{"tool": "beta.get_status", "arguments": map[string]any{}})
	if !res.IsError || textOf(res) != notFoundText("beta.get_status") {
		t.Fatalf("tools.execute: %v %q", res.IsError, textOf(res))
	}
	// The check runs before _approval_id resume and before _grant.
	res = f.call(t, f.member, "beta.run", map[string]any{"_approval_id": "apr_x"})
	if !res.IsError || textOf(res) != notFoundText("beta.run") {
		t.Fatalf("_approval_id resume should be refused first: %q", textOf(res))
	}
	res = f.call(t, f.member, "beta.run", map[string]any{GrantField: "tyg_x"})
	if !res.IsError || textOf(res) != notFoundText("beta.run") {
		t.Fatalf("_grant should be refused first: %q", textOf(res))
	}
	// A write to an ungranted group must not queue an approval either.
	if n, _ := f.bus.CountPendingForAgent(context.Background(), memberID); n != 0 {
		t.Fatalf("no approval should be queued, got %d", n)
	}
	if f.calls.Load() != 0 {
		t.Fatal("handler ran for an ungranted tool")
	}

	// Granted group still works.
	if res := f.call(t, f.member, "alpha.get_status", nil); res.IsError || f.calls.Load() != 1 {
		t.Fatalf("alpha.get_status should run: %s", textOf(res))
	}
	// Admin reaches beta.
	if res := f.call(t, f.admin, "beta.get_status", nil); res.IsError || f.calls.Load() != 2 {
		t.Fatalf("admin beta.get_status should run: %s", textOf(res))
	}

	// Admins see the denial in the audit log, with the tool and upstream.
	evs, err := f.aud.Recent(context.Background(), 50)
	if err != nil {
		t.Fatal(err)
	}
	denied := 0
	for _, e := range evs {
		if e.EventType == audit.EventCallDenied && e.Reason == "access: server not granted" && e.AgentID == memberID {
			if e.UpstreamName != "beta" || !strings.HasPrefix(e.ToolName, "beta.") {
				t.Errorf("denial should name the tool and upstream: %+v", e)
			}
			denied++
		}
	}
	if denied < 3 {
		t.Fatalf("expected an access denial audit event per refused call, got %d", denied)
	}
	if !f.met.find("beta.get_status", metrics.OutcomeDenied, "access") {
		t.Fatalf("expected a denied/access metrics event, got %+v", f.met.events)
	}
}

// TestAccessRawMCPCallMatchesUnknownTool: mcp-go answers tools/call for a
// name it doesn't have with a JSON-RPC error, not a tool result, so an
// ungranted tool must produce that same error or a member could tell the
// two apart and map which servers exist.
func TestAccessRawMCPCallMatchesUnknownTool(t *testing.T) {
	f := newAccessFixture(t, nil)

	ungranted := mcpToolsCall(t, f.gw, f.member, "beta.get_status")
	unknown := mcpToolsCall(t, f.gw, f.member, "nope.tool")
	if !ungranted.rpcErr || !unknown.rpcErr {
		t.Fatalf("both must be JSON-RPC errors:\n%s\n%s", ungranted.raw, unknown.raw)
	}
	normalised := bytes.ReplaceAll(ungranted.raw, []byte("beta.get_status"), []byte("nope.tool"))
	if !bytes.Equal(normalised, unknown.raw) {
		t.Fatalf("ungranted and unknown responses differ:\n%s\n%s", normalised, unknown.raw)
	}
	if ungranted.message != rpcNotFoundMessage("beta.get_status") {
		t.Fatalf("message should keep mcp-go's shape: %q", ungranted.message)
	}
	// A Denied caller gets the same answer for an always-on tool.
	blocked := mcpToolsCall(t, f.gw, f.blocked, "tools.search")
	if !blocked.rpcErr || blocked.code != unknown.code || blocked.message != rpcNotFoundMessage("tools.search") {
		t.Fatalf("blocked raw call: %s", blocked.raw)
	}
	if f.calls.Load() != 0 {
		t.Fatal("no handler should have run")
	}

	// The denial is still on record even though the agent saw "not found".
	evs, err := f.aud.Recent(context.Background(), 50)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range evs {
		if e.EventType == audit.EventCallDenied && e.Reason == accessDeniedReason && e.ToolName == "beta.get_status" && e.AgentID == memberID {
			found = true
		}
	}
	if !found {
		t.Fatal("raw MCP denial should be audited")
	}
	if !f.met.find("beta.get_status", metrics.OutcomeDenied, "access") {
		t.Fatalf("raw MCP denial should record a denied/access metrics event: %+v", f.met.events)
	}

	// Granted and admin callers still get real results over raw MCP.
	if r := mcpToolsCall(t, f.gw, f.member, "alpha.get_status"); r.rpcErr || r.isError || f.calls.Load() != 1 {
		t.Fatalf("member alpha.get_status: %s", r.raw)
	}
	if r := mcpToolsCall(t, f.gw, f.admin, "beta.get_status"); r.rpcErr || r.isError || f.calls.Load() != 2 {
		t.Fatalf("admin beta.get_status: %s", r.raw)
	}
}

func TestAddUpstreamReservesAccessGroupNames(t *testing.T) {
	f := newAccessFixture(t, nil)
	// The synthetic upstreams, the always-on meta-tool group, and the data
	// groups whose tools live under the "builtin" upstream.
	for _, name := range []string{"builtin", "fixture", "inbox", "session", "connections", "tools", "memory", "lake", "events"} {
		err := f.gw.AddUpstream(context.Background(), UpstreamConfig{Name: name})
		if err == nil || !strings.Contains(err.Error(), "reserved") {
			t.Errorf("upstream %q should be reserved, got %v", name, err)
		}
	}
	// notes and skills are real upstreams that startup registers under
	// those names, and any plain name, get past the reservation (and fail
	// later, on dial).
	for _, name := range []string{"notes", "skills", "deploy"} {
		if err := f.gw.AddUpstream(context.Background(), UpstreamConfig{Name: name}); err != nil && strings.Contains(err.Error(), "reserved") {
			t.Errorf("%q must not be reserved at the gateway: %v", name, err)
		}
	}
}

func TestAccessSearchAndCatalogFor(t *testing.T) {
	f := newAccessFixture(t, nil)

	res := f.call(t, f.member, "tools.search", map[string]any{"query": ""})
	if res.IsError {
		t.Fatalf("tools.search: %s", textOf(res))
	}
	var names []string
	for _, r := range structured(t, res)["results"].([]any) {
		names = append(names, r.(map[string]any)["name"].(string))
	}
	if !has(names, "alpha.run") || hasPrefix(names, "beta.") || hasPrefix(names, "memory.") {
		t.Fatalf("tools.search for member: %v", names)
	}
	if res := f.call(t, f.member, "tools.search", map[string]any{"upstream": "beta"}); structured(t, res)["total"].(float64) != 0 {
		t.Fatalf("tools.search upstream=beta should be empty for member: %s", textOf(res))
	}

	member := catalogNames(f.gw.CatalogFor(f.member))
	if !has(member, "alpha.run") || !has(member, "tools.search") || !has(member, "inbox.request") || hasPrefix(member, "beta.") {
		t.Fatalf("CatalogFor member: %v", member)
	}
	admin := catalogNames(f.gw.CatalogFor(f.admin))
	if len(admin) != len(f.gw.Catalog()) || !has(admin, "beta.run") {
		t.Fatalf("CatalogFor admin should equal Catalog: %v", admin)
	}
	if blocked := f.gw.CatalogFor(f.blocked); len(blocked) != 0 {
		t.Fatalf("CatalogFor blocked should be empty: %v", catalogNames(blocked))
	}
}

func TestAccessExecuteRechecksScope(t *testing.T) {
	f := newAccessFixture(t, nil)
	f.res.set(memberID, groups("alpha", "beta"))

	queue := func() *approval.Request {
		res := f.call(t, f.member, "beta.run", map[string]any{"env": "prod"})
		s := structured(t, res)
		id, _ := s["approval_id"].(string)
		if s["status"] != "pending_approval" || id == "" {
			t.Fatalf("expected a queued approval, got %v", s)
		}
		req, err := f.bus.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		return req
	}

	// Grant removed while the approval waited: nothing runs, the row gets
	// an error result so pollers see why.
	req := queue()
	f.res.set(memberID, groups("alpha"))
	f.gw.Execute(context.Background(), req)
	if f.calls.Load() != 0 {
		t.Fatal("Execute ran a tool whose group was removed")
	}
	after, err := f.bus.Get(context.Background(), req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.ResultExecutedAt == 0 || !after.ResultIsError || !strings.Contains(after.ResultEnvelope, "access to beta was removed") {
		t.Fatalf("expected a persisted access error result, got %+v", after)
	}
	if !f.met.find("beta.run", metrics.OutcomeDenied, "access") {
		t.Fatalf("expected a denied/access metrics event for Execute")
	}

	// Grant still present: Execute runs it.
	f.res.set(memberID, groups("alpha", "beta"))
	req2 := queue()
	f.gw.Execute(context.Background(), req2)
	if f.calls.Load() != 1 {
		t.Fatalf("Execute should run a still-granted tool, calls=%d", f.calls.Load())
	}
}

func TestAccessInboxReportsUngrantedAsUnknown(t *testing.T) {
	f := newAccessFixture(t, nil)
	ctx := context.Background()

	if a, up := f.gw.Access(ctx, memberID, "beta.run", nil); a != inbox.AccessUnknown || up != "" {
		t.Fatalf("beta.run for member: %s %q", a, up)
	}
	if a, _ := f.gw.Access(ctx, memberID, "alpha.run", nil); a != inbox.AccessRestricted {
		t.Fatalf("alpha.run for member: %s", a)
	}
	if a, _ := f.gw.Access(ctx, memberID, "alpha.get_status", nil); a != inbox.AccessOpen {
		t.Fatalf("alpha.get_status for member: %s", a)
	}
	if a, _ := f.gw.Access(ctx, adminID, "beta.run", nil); a != inbox.AccessRestricted {
		t.Fatalf("beta.run for admin: %s", a)
	}
	if a, _ := f.gw.Access(ctx, blockedID, "inbox.guide", nil); a != inbox.AccessUnknown {
		t.Fatalf("blocked caller should get unknown for everything: %s", a)
	}

	// inbox.check and inbox.request go through the same answer.
	c := structured(t, f.call(t, f.member, "inbox.check", map[string]any{"calls": []any{
		map[string]any{"tool": "beta.run"}, map[string]any{"tool": "alpha.run"},
	}}))
	calls := c["calls"].([]any)
	if calls[0].(map[string]any)["status"] != "unknown" || calls[1].(map[string]any)["status"] != "restricted" {
		t.Fatalf("inbox.check: %v", calls)
	}
	req := requestArgs(map[string]any{"env": "prod"})
	req["tools"] = []any{map[string]any{"tool": "beta.run", "required": true, "summary": "Run beta.", "params": map[string]any{"env": "prod"}}}
	req["dry_run"] = true
	s := structured(t, f.call(t, f.member, "inbox.request", req))
	probs, _ := s["problems"].([]any)
	found := false
	for _, p := range probs {
		if m := p.(map[string]any); m["path"] == "tools[0].tool" && strings.Contains(m["message"].(string), "isn't a toolyard tool") {
			found = true
		}
	}
	if s["ok"] != false || !found {
		t.Fatalf("inbox.request for an ungranted tool should fail validation as unknown: %v", s)
	}
}

func TestAccessAlwaysOnAndDeniedScopes(t *testing.T) {
	f := newAccessFixture(t, nil)

	// Always-on groups work for a member with no explicit grant for them.
	if res := f.call(t, f.member, "tools.search", map[string]any{"query": "alpha"}); res.IsError {
		t.Fatalf("tools.search: %s", textOf(res))
	}
	if res := f.call(t, f.member, "inbox.guide", map[string]any{"topic": "voice"}); res.IsError {
		t.Fatalf("inbox.guide: %s", textOf(res))
	}
	if res := f.call(t, f.member, "session.start", map[string]any{"title": "Checking access"}); res.IsError {
		t.Fatalf("session.start: %s", textOf(res))
	}
	// Built-in data groups are not always-on: memory needs a grant.
	if res := f.call(t, f.member, "memory.list", nil); !res.IsError || textOf(res) != notFoundText("memory.list") {
		t.Fatalf("memory.list for member without grant: %q", textOf(res))
	}
	f.res.set(memberID, groups("alpha", "memory"))
	if res := f.call(t, f.member, "memory.list", nil); res.IsError {
		t.Fatalf("memory.list with grant: %s", textOf(res))
	}

	// Denied reaches nothing, including the meta-tools.
	for _, tool := range []string{"tools.search", "tools.execute", "inbox.guide", "session.start", "alpha.get_status"} {
		res := f.call(t, f.blocked, tool, map[string]any{"query": "", "tool": "alpha.get_status", "title": "x"})
		if !res.IsError || textOf(res) != notFoundText(tool) {
			t.Errorf("blocked %s: isError=%v %q", tool, res.IsError, textOf(res))
		}
	}
	if f.calls.Load() != 0 {
		t.Fatal("blocked caller ran a tool")
	}
}

func TestAccessNilResolverIsUnrestricted(t *testing.T) {
	f := newInboxFixture(t) // no Access option
	unknown := WithAgentID(context.Background(), "ag_nobody")

	names := mcpToolsList(t, f.gw, unknown)
	if !has(names, "deploy.run") || !has(names, "memory.get") || !has(names, "tools.search") {
		t.Fatalf("without a resolver tools/list must be unfiltered: %v", names)
	}
	if got, want := len(f.gw.CatalogFor(unknown)), len(f.gw.Catalog()); got != want {
		t.Fatalf("CatalogFor without a resolver: %d want %d", got, want)
	}
	if res := f.call(t, unknown, "deploy.get_status", nil); res.IsError || f.calls.Load() != 1 {
		t.Fatalf("without a resolver every caller may call: %s", textOf(res))
	}
	if a, up := f.gw.Access(context.Background(), "ag_nobody", "deploy.run", nil); a != inbox.AccessRestricted || up != "deploy" {
		t.Fatalf("without a resolver inbox Access is unchanged: %s %s", a, up)
	}
	// Raw MCP keeps mcp-go's own unknown-tool answer (INVALID_PARAMS).
	if r := mcpToolsCall(t, f.gw, unknown, "nope.tool"); !r.rpcErr || r.code != mcp.INVALID_PARAMS || r.message != rpcNotFoundMessage("nope.tool") {
		t.Fatalf("without a resolver unknown tools are mcp-go's business: %s", r.raw)
	}
	if r := mcpToolsCall(t, f.gw, unknown, "deploy.get_status"); r.rpcErr || r.isError {
		t.Fatalf("without a resolver raw MCP calls run: %s", r.raw)
	}
}
