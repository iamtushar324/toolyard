package gateway

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tusharbhardwaj/toolyard/internal/approval"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/inbox"
	"github.com/tusharbhardwaj/toolyard/internal/memory"
	"github.com/tusharbhardwaj/toolyard/internal/policy"
	"github.com/tusharbhardwaj/toolyard/internal/realtime"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

type inboxFixture struct {
	gw     *Gateway
	svc    *inbox.Service
	pol    *policy.Engine
	aud    *audit.Logger
	mode   string
	calls  atomic.Int32
	agent  context.Context
	anonym context.Context
}

const testReason = "testing the inbox permission flow end to end"

func newInboxFixture(t *testing.T) *inboxFixture {
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
	f := &inboxFixture{mode: ApprovalModeExecute}
	f.pol = policy.New(db)
	f.aud = audit.New(db)
	f.gw = New(Options{Policy: f.pol, Approval: bus, Audit: f.aud, Hub: realtime.NewHub(), Memory: memory.New(db)})
	f.gw.RegisterBuiltins()
	t.Cleanup(func() { _ = f.gw.Close() })
	f.svc, err = inbox.New(context.Background(), inbox.Options{DB: db, Catalog: f.gw})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.svc.Flush)
	f.gw.SetInbox(f.svc, inbox.NewGuide(nil), func() string { return f.mode })
	f.gw.RegisterInboxTools()

	handler := func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		f.calls.Add(1)
		return mcp.NewToolResultText("deployed"), nil
	}
	for _, name := range []string{"deploy.run", "deploy.rollback", "deploy.get_status"} {
		f.gw.registerEntry(toolEntry{
			tool:     mcp.Tool{Name: name, InputSchema: mcp.ToolInputSchema{Type: "object", Properties: addMetaProps(map[string]any{})}},
			upstream: "deploy", originalName: strings.TrimPrefix(name, "deploy."), reasonField: ReasonField, handle: handler,
		})
	}
	f.agent = WithAgentID(context.Background(), "ag_1")
	f.anonym = context.Background()
	return f
}

func (f *inboxFixture) call(t *testing.T, ctx context.Context, tool string, args map[string]any) *mcp.CallToolResult {
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

func structured(t *testing.T, res *mcp.CallToolResult) map[string]any {
	t.Helper()
	b, _ := json.Marshal(res.StructuredContent)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

func deployArgs() map[string]any { return map[string]any{"service": "api", "env": "prod"} }

func TestExecuteModeKeepsLegacyFlow(t *testing.T) {
	f := newInboxFixture(t)
	res := f.call(t, f.agent, "deploy.run", deployArgs())
	if s := structured(t, res); s["status"] != "pending_approval" {
		t.Fatalf("execute mode should queue an approval, got %v", s["status"])
	}
	if f.calls.Load() != 0 {
		t.Fatal("tool ran without approval")
	}
}

// A caller-declared _intent_category can only escalate. Declaring "read"
// on a write doesn't skip the approval (execute mode) or the permission
// check (inbox mode); declaring "write" on a read makes it need one.
func TestDeclaredIntentOnlyEscalates(t *testing.T) {
	f := newInboxFixture(t)
	withIntent := func(intent string) map[string]any {
		a := deployArgs()
		a[IntentField] = intent
		return a
	}
	for _, c := range []struct{ tool, intent string }{{"deploy.run", "read"}, {"deploy.get_status", "write"}} {
		if s := structured(t, f.call(t, f.agent, c.tool, withIntent(c.intent))); s["status"] != "pending_approval" {
			t.Fatalf("execute mode: %s declared %s should queue an approval, got %v", c.tool, c.intent, s["status"])
		}
	}
	f.mode = ApprovalModeInbox
	for _, c := range []struct{ tool, intent string }{{"deploy.rollback", "read"}, {"deploy.get_status", "write"}} {
		res := f.call(t, f.agent, c.tool, withIntent(c.intent))
		if s := structured(t, res); !res.IsError || s["status"] != "permission_required" {
			t.Fatalf("inbox mode: %s declared %s should need permission, got %v", c.tool, c.intent, s)
		}
	}
	if n := f.calls.Load(); n != 0 {
		t.Fatalf("%d call(s) ran without approval", n)
	}
	// A read declared read still runs, as it would with no intent.
	if res := f.call(t, f.agent, "deploy.get_status", withIntent("read")); res.IsError || f.calls.Load() != 1 {
		t.Fatalf("read declared read should run: %+v", res)
	}
}

func TestInboxModeCoaches(t *testing.T) {
	f := newInboxFixture(t)
	f.mode = ApprovalModeInbox
	res := f.call(t, f.agent, "deploy.run", deployArgs())
	s := structured(t, res)
	if !res.IsError || s["status"] != "permission_required" || s["executed"] != false {
		t.Fatalf("want permission_required, got %v", s)
	}
	draft := s["draft"].(map[string]any)
	tool := draft["tools"].([]any)[0].(map[string]any)
	if tool["tool"] != "deploy.run" || tool["params"].(map[string]any)["env"].(map[string]any)["eq"] != "prod" {
		t.Fatalf("draft should carry the exact call: %v", tool)
	}
	nearby := s["also_restricted_nearby"].([]any)
	if len(nearby) != 1 || nearby[0] != "deploy.rollback" {
		t.Fatalf("nearby should list deploy.rollback only (get_status is a read): %v", nearby)
	}
	if f.calls.Load() != 0 {
		t.Fatal("tool ran")
	}
	list, _ := f.svc.List(context.Background(), inbox.ListFilter{})
	if len(list) != 0 {
		t.Fatal("coaching must not create a request")
	}
	evs, _ := f.aud.Recent(context.Background(), 20)
	coached := false
	for _, e := range evs {
		if e.EventType == EventCallCoached && e.ToolName == "deploy.run" {
			coached = true
		}
	}
	if !coached {
		t.Fatal("expected a call.coached audit event")
	}
	// Anonymous callers keep the legacy flow: grants need an identity.
	if s := structured(t, f.call(t, f.anonym, "deploy.run", deployArgs())); s["status"] != "pending_approval" {
		t.Fatalf("anonymous caller should get the legacy flow, got %v", s["status"])
	}
	// Reads are unaffected.
	if res := f.call(t, f.agent, "deploy.get_status", nil); res.IsError || f.calls.Load() != 1 {
		t.Fatal("read-only tool should run")
	}
}

func requestArgs(params map[string]any) map[string]any {
	return map[string]any{
		"title":   "Deploy api to prod",
		"summary": "I need one deploy.",
		"message": "The fix is merged and green, so I'd like to deploy the api service to production.",
		"facts":   map[string]any{"why_now": "Customers are hitting the bug.", "if_it_goes_wrong": "The api service.", "undo": "Roll back to the previous build."},
		"audio":   map[string]any{"script": "I'd like to deploy the api service to production. The fix is merged and all checks passed."},
		"urgency": "soon",
		"tools": []any{map[string]any{
			"tool": "deploy.run", "required": true, "summary": "Deploy api to prod.", "params": params,
		}},
	}
}

func (f *inboxFixture) approvedGrant(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	dry := requestArgs(map[string]any{"service": "api", "env": "prod"})
	dry["dry_run"] = true
	res := f.call(t, f.agent, "inbox.request", dry)
	if s := structured(t, res); s["ok"] != true || s["request_id"] != nil {
		t.Fatalf("dry run: %v", s)
	}
	res = f.call(t, f.agent, "inbox.request", requestArgs(map[string]any{"service": "api", "env": "prod"}))
	s := structured(t, res)
	id, _ := s["request_id"].(string)
	if s["ok"] != true || id == "" {
		t.Fatalf("request: %v", s)
	}
	f.svc.Flush()
	if _, err := f.svc.Decide(ctx, id, inbox.Decision{Action: "approve", Allow: []bool{true}}); err != nil {
		t.Fatal(err)
	}
	w := structured(t, f.call(t, f.agent, "inbox.wait", map[string]any{"ids": []any{id}, "timeout_seconds": 1}))
	reqs := w["requests"].([]any)
	tok := reqs[0].(map[string]any)["tools"].([]any)[0].(map[string]any)["grant"].(string)
	if !strings.HasPrefix(tok, "tyg_") {
		t.Fatalf("no grant token in wait result: %v", w)
	}
	return tok
}

func TestGrantLifecycleThroughTheGateway(t *testing.T) {
	f := newInboxFixture(t)
	f.mode = ApprovalModeInbox
	tok := f.approvedGrant(t)

	// Out of scope: nothing runs, the grant survives.
	res := f.call(t, f.agent, "deploy.run", map[string]any{"service": "api", "env": "staging", GrantField: tok})
	if s := structured(t, res); !res.IsError || s["status"] != "grant_invalid" || s["draft"] == nil {
		t.Fatalf("want grant_invalid with a draft, got %v", s)
	}
	// Another agent can't use it.
	other := WithAgentID(context.Background(), "ag_2")
	if s := structured(t, f.call(t, other, "deploy.run", map[string]any{"service": "api", "env": "prod", GrantField: tok})); s["status"] != "grant_invalid" {
		t.Fatalf("other agent used the grant: %v", s)
	}
	if f.calls.Load() != 0 {
		t.Fatal("tool ran on an invalid grant")
	}
	// In scope: runs once.
	res = f.call(t, f.agent, "deploy.run", map[string]any{"service": "api", "env": "prod", GrantField: tok})
	if res.IsError || f.calls.Load() != 1 {
		t.Fatalf("granted call failed: %+v", res)
	}
	// Used up.
	res = f.call(t, f.agent, "deploy.run", map[string]any{"service": "api", "env": "prod", GrantField: tok})
	if s := structured(t, res); s["status"] != "grant_invalid" || f.calls.Load() != 1 {
		t.Fatalf("second use should fail: %v", s)
	}
}

func TestGrantWorksInExecuteModeToo(t *testing.T) {
	f := newInboxFixture(t)
	tok := f.approvedGrant(t)
	res := f.call(t, f.agent, "deploy.run", map[string]any{"service": "api", "env": "prod", GrantField: tok})
	if res.IsError || f.calls.Load() != 1 {
		t.Fatalf("grant should work in execute mode: %+v", res)
	}
}

func TestExplicitDenyBeatsGrant(t *testing.T) {
	f := newInboxFixture(t)
	f.mode = ApprovalModeInbox
	tok := f.approvedGrant(t)
	if _, err := f.pol.Set(context.Background(), "tool", "deploy.run", "deny", "frozen", true); err != nil {
		t.Fatal(err)
	}
	res := f.call(t, f.agent, "deploy.run", map[string]any{"service": "api", "env": "prod", GrantField: tok})
	if !res.IsError || f.calls.Load() != 0 {
		t.Fatal("a deny policy must beat a grant")
	}
	// The grant wasn't consumed by the denied call.
	active, _ := f.svc.ListGrants(context.Background(), inbox.GrantActive, "ag_1", 0)
	if len(active) != 1 {
		t.Fatalf("grant should still be active, got %d", len(active))
	}
}

func TestInboxToolsNeedAnAgent(t *testing.T) {
	f := newInboxFixture(t)
	res := f.call(t, f.anonym, "inbox.request", requestArgs(map[string]any{"service": "api"}))
	if !res.IsError || !strings.Contains(textOf(res), "enrolled agent") {
		t.Fatalf("anonymous inbox.request should fail: %s", textOf(res))
	}
}

func TestInboxGuideCheckAndPinned(t *testing.T) {
	f := newInboxFixture(t)
	res := f.call(t, f.agent, "inbox.guide", map[string]any{"topic": "voice"})
	if res.IsError || !strings.Contains(textOf(res), "75 words") {
		t.Fatalf("guide voice topic: %s", textOf(res))
	}
	if res := f.call(t, f.agent, "inbox.guide", map[string]any{"topic": "nope"}); !res.IsError {
		t.Fatal("unknown topic should be an error")
	}
	c := structured(t, f.call(t, f.agent, "inbox.check", map[string]any{"calls": []any{
		map[string]any{"tool": "deploy.run"}, map[string]any{"tool": "deploy.get_status"}, map[string]any{"tool": "inbox.guide"}, map[string]any{"tool": "no.such"},
	}}))
	calls := c["calls"].([]any)
	want := []string{"restricted", "open", "open", "unknown"}
	for i, w := range want {
		if got := calls[i].(map[string]any)["status"]; got != w {
			t.Errorf("check %d: %v want %s", i, got, w)
		}
	}
	for _, n := range InboxToolNames {
		if !IsPinned(n) || !f.gw.HasTool(n) {
			t.Errorf("%s should be registered and pinned", n)
		}
	}
	if err := f.gw.AddUpstream(context.Background(), UpstreamConfig{Name: "inbox"}); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("upstream named inbox should be reserved, got %v", err)
	}
}

func TestSchemaHasGrantField(t *testing.T) {
	wrapped, _ := wrapSchema(mcp.Tool{Name: "x", InputSchema: mcp.ToolInputSchema{Type: "object", Properties: map[string]any{"a": map[string]any{"type": "string"}}}})
	if _, ok := wrapped.InputSchema.Properties[GrantField]; !ok {
		t.Fatal("_grant missing from wrapped schema")
	}
	_, _, clean, err := extractReason(map[string]any{ReasonField: testReason, GrantField: "tyg_x", "a": "b"}, ReasonField)
	if err != nil || clean[GrantField] != nil || clean["a"] != "b" {
		t.Fatalf("_grant must be stripped before dispatch: %v %v", clean, err)
	}
}

func textOf(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if t, ok := mcp.AsTextContent(c); ok {
			b.WriteString(t.Text)
		}
	}
	return b.String()
}
