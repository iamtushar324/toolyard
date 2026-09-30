package gateway

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tusharbhardwaj/toolyard/internal/approval"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/codemode"
	"github.com/tusharbhardwaj/toolyard/internal/memory"
	"github.com/tusharbhardwaj/toolyard/internal/metrics"
	"github.com/tusharbhardwaj/toolyard/internal/policy"
	"github.com/tusharbhardwaj/toolyard/internal/realtime"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

const outerReason = "outer reason the client sent, long enough for the gate"

// lastEvent is the newest metric recorded for tool.
func lastEvent(m *fakeMetrics, tool string) (metrics.Event, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := len(m.events) - 1; i >= 0; i-- {
		if m.events[i].ToolName == tool {
			return m.events[i], true
		}
	}
	return metrics.Event{}, false
}

// route calls a tool through the gateway with exactly the args given: no
// _reason is added, which is how a Bifrost client calls code mode.
func route(t *testing.T, g *Gateway, ctx context.Context, tool string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := g.RouteCall(ctx, "test", tool, args)
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	return res
}

func TestCodeModeToolsAlwaysOnPinnedAndReasonOptional(t *testing.T) {
	f := newAccessFixture(t, nil)
	member := mcpToolsList(t, f.gw, f.member)
	for _, n := range CodeModeToolNames {
		if !has(member, n) {
			t.Errorf("member should see %s: %v", n, member)
		}
		if !IsPinned(n) {
			t.Errorf("%s should be pinned", n)
		}
		e := f.gw.tools[n]
		if !e.reasonOptional || e.upstream != "tools" || e.forcedAction == nil || *e.forcedAction != policy.ActionAllow {
			t.Errorf("%s entry = upstream %q optional %v forced %v", n, e.upstream, e.reasonOptional, e.forcedAction)
		}
		for _, r := range e.tool.InputSchema.Required {
			if r == ReasonField {
				t.Errorf("%s must not require _reason", n)
			}
		}
	}
	if blocked := mcpToolsList(t, f.gw, f.blocked); len(blocked) != 0 {
		t.Fatalf("blocked caller sees %v", blocked)
	}
	// The one thing a present _reason must still do is be valid.
	res := route(t, f.gw, f.member, CodeModeListToolFiles, map[string]any{ReasonField: "short"})
	if !res.IsError || !strings.Contains(textOf(res), "_reason too short") {
		t.Fatalf("a bad explicit reason should still be refused: %v %s", res.IsError, textOf(res))
	}
}

func TestCodeModeListingIsScopedToCaller(t *testing.T) {
	f := newAccessFixture(t, nil)
	ctx := context.Background()

	member := textOf(route(t, f.gw, f.member, CodeModeListToolFiles, map[string]any{}))
	want := "servers/\n  alpha/\n    get_status.pyi\n    run.pyi\n  inbox/\n"
	if !strings.Contains(member, want) {
		t.Fatalf("member listing:\n%s", member)
	}
	for _, leak := range []string{"beta", "memory/", "tools/", "tools.search", "listToolFiles.pyi", "execute.pyi", "fixture"} {
		if strings.Contains(member, leak) {
			t.Errorf("member listing leaks %q:\n%s", leak, member)
		}
	}

	admin := textOf(route(t, f.gw, f.admin, CodeModeListToolFiles, map[string]any{}))
	for _, want := range []string{"  alpha/\n", "  beta/\n    get_status.pyi\n    run.pyi\n", "  memory/\n    delete.pyi\n    get.pyi\n", "  fixture/\n    echo.pyi\n"} {
		if !strings.Contains(admin, want) {
			t.Errorf("admin listing missing %q:\n%s", want, admin)
		}
	}
	if strings.Contains(admin, "tools/") {
		t.Errorf("the meta-tool group must never be bound:\n%s", admin)
	}

	// A blocked caller is refused before any handler runs, like everywhere.
	blocked := route(t, f.gw, f.blocked, CodeModeListToolFiles, map[string]any{})
	if !blocked.IsError || textOf(blocked) != notFoundText(CodeModeListToolFiles) {
		t.Fatalf("blocked listing: %v %s", blocked.IsError, textOf(blocked))
	}
	// A member with no grants sees only the always-on groups.
	f.res.set("ag_nogrants", groups())
	nogrants := textOf(route(t, f.gw, WithAgentID(context.Background(), "ag_nogrants"), CodeModeListToolFiles, map[string]any{}))
	if !strings.HasSuffix(nogrants, "servers/\n  inbox/\n    ask.pyi\n    cancel.pyi\n    check.pyi\n    guide.pyi\n    post.pyi\n    request.pyi\n    status.pyi\n    wait.pyi\n  session/\n    start.pyi\n    update.pyi") {
		t.Fatalf("no-grants listing:\n%s", nogrants)
	}

	// A member asking about an ungranted server learns nothing about it.
	res := route(t, f.gw, f.member, CodeModeReadToolFile, map[string]any{"fileName": "servers/beta/run.pyi"})
	if !res.IsError || !strings.HasPrefix(textOf(res), "No server found matching 'beta'.") || strings.Contains(textOf(res), "servers/beta") {
		t.Fatalf("member reading beta: %v %s", res.IsError, textOf(res))
	}
	res = route(t, f.gw, f.member, CodeModeGetToolDocs, map[string]any{"server": "beta", "tool": "run"})
	if !res.IsError || strings.Contains(textOf(res), "- beta") {
		t.Fatalf("member docs for beta: %s", textOf(res))
	}

	// And in code, beta is simply not a name.
	res = route(t, f.gw, f.member, CodeModeExecuteToolCode, map[string]any{"code": "result = beta.get_status()"})
	if !res.IsError || !strings.Contains(textOf(res), "undefined: beta") || !strings.Contains(textOf(res), "Available server keys: alpha, inbox, session") {
		t.Fatalf("member calling beta in code: %s", textOf(res))
	}
	if f.calls.Load() != 0 {
		t.Fatal("a handler ran for an ungranted server")
	}
	_ = ctx
}

func TestCodeModeStubsHideGatewayFields(t *testing.T) {
	f := newAccessFixture(t, nil)
	res := route(t, f.gw, f.member, CodeModeReadToolFile, map[string]any{"fileName": "servers/Alpha/Get_Status"})
	if res.IsError {
		t.Fatalf("readToolFile: %s", textOf(res))
	}
	stub := textOf(res)
	if !strings.Contains(stub, "# alpha.get_status tool\n") || !strings.Contains(stub, "\ndef get_status() -> dict\n") {
		t.Fatalf("stub:\n%s", stub)
	}
	for _, hidden := range []string{ReasonField, IntentField, ApprovalIDField, GrantField, SessionField, "[toolyard-gated"} {
		if strings.Contains(stub, hidden) {
			t.Errorf("stub shows gateway field %q:\n%s", hidden, stub)
		}
	}
	// A built-in with real parameters renders them, without the banner.
	res = route(t, f.gw, f.admin, CodeModeReadToolFile, map[string]any{"fileName": "servers/memory/get.pyi", "startLine": 9.0, "endLine": 9.0})
	if res.IsError || textOf(res) != "def get(key: str, scope: str = None) -> dict:  # Read a value from toolyard shared memory by key." {
		t.Fatalf("memory.get stub line 9: %v %q", res.IsError, textOf(res))
	}
	res = route(t, f.gw, f.admin, CodeModeGetToolDocs, map[string]any{"server": "memory", "tool": "get"})
	if res.IsError || !strings.Contains(textOf(res), "# Documentation for memory.get tool") || !strings.Contains(textOf(res), "        key (str): key parameter (required)") {
		t.Fatalf("memory.get docs: %s", textOf(res))
	}
	if res := route(t, f.gw, f.admin, CodeModeReadToolFile, map[string]any{}); !res.IsError || !strings.Contains(textOf(res), "fileName parameter is required") {
		t.Fatalf("missing fileName: %s", textOf(res))
	}
	if res := route(t, f.gw, f.admin, CodeModeGetToolDocs, map[string]any{"server": "memory"}); !res.IsError || !strings.Contains(textOf(res), "tool parameter is required") {
		t.Fatalf("missing tool: %s", textOf(res))
	}
	if res := route(t, f.gw, f.admin, CodeModeExecuteToolCode, map[string]any{}); !res.IsError || !strings.Contains(textOf(res), "code parameter is required") {
		t.Fatalf("missing code: %s", textOf(res))
	}
}

func TestCodeModeNestedCallsRouteViaCodeModeWithReason(t *testing.T) {
	f := newAccessFixture(t, nil)

	// Outer _reason given: every nested call carries it.
	res := route(t, f.gw, f.member, CodeModeExecuteToolCode, map[string]any{
		"code": "a = alpha.get_status()\nresult = {\"status\": a}", ReasonField: outerReason,
	})
	if res.IsError {
		t.Fatalf("script failed:\n%s", textOf(res))
	}
	if !strings.Contains(textOf(res), "Print output:\n[TOOL] alpha.get_status raw response: \"ran\"\n\nExecution completed successfully.\nReturn value: {\n  \"status\": \"ran\"\n}") {
		t.Fatalf("output:\n%s", textOf(res))
	}
	if f.calls.Load() != 1 {
		t.Fatalf("handler ran %d times", f.calls.Load())
	}
	ev, ok := lastEvent(f.met, "alpha.get_status")
	if !ok || ev.Via != codemode.Via || ev.ReasonText != outerReason || ev.Outcome != metrics.OutcomeOK || ev.AgentID != memberID {
		t.Fatalf("nested call metric = %+v", ev)
	}
	outer, ok := lastEvent(f.met, CodeModeExecuteToolCode)
	if !ok || outer.Via != "test" || outer.Outcome != metrics.OutcomeOK || outer.ReasonText != outerReason {
		t.Fatalf("outer call metric = %+v", outer)
	}

	// No outer _reason (a Bifrost client): the nested call carries one
	// derived from the script, and the audit trail shows the same.
	res = route(t, f.gw, f.member, CodeModeExecuteToolCode, map[string]any{
		"code": "# checking alpha status before the sync\nresult = alpha.get_status()",
	})
	if res.IsError {
		t.Fatalf("script failed:\n%s", textOf(res))
	}
	ev, _ = lastEvent(f.met, "alpha.get_status")
	if ev.ReasonText != "code mode: checking alpha status before the sync" || ev.Via != codemode.Via {
		t.Fatalf("derived reason metric = %+v", ev)
	}
	outer, _ = lastEvent(f.met, CodeModeExecuteToolCode)
	if outer.ReasonText != "" || outer.Outcome != metrics.OutcomeOK {
		t.Fatalf("outer call without reason = %+v", outer)
	}
	found := false
	evs, err := f.aud.Recent(context.Background(), 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		if e.ToolName == "alpha.get_status" && e.EventType == audit.EventCallSucceeded && e.Reason == "code mode: checking alpha status before the sync" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no audit row with the derived reason: %+v", evs)
	}

	// Two different scripts, two different reasons.
	route(t, f.gw, f.member, CodeModeExecuteToolCode, map[string]any{"code": "result = alpha.get_status()"})
	ev, _ = lastEvent(f.met, "alpha.get_status")
	if ev.ReasonText != "code mode: result = alpha.get_status()" {
		t.Fatalf("second derived reason = %q", ev.ReasonText)
	}

	// The MCP wire path is the same route, and needs no _reason either.
	msg, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 9, "method": "tools/call",
		"params": map[string]any{"name": CodeModeExecuteToolCode, "arguments": map[string]any{"code": "result = alpha.get_status()"}},
	})
	raw := f.gw.MCPServer().HandleMessage(f.member, json.RawMessage(msg))
	b, _ := json.Marshal(raw)
	if !strings.Contains(string(b), "Execution completed successfully") || strings.Contains(string(b), `"isError":true`) {
		t.Fatalf("raw MCP executeToolCode = %s", b)
	}
}

func TestCodeModeHeldCallAbortsScript(t *testing.T) {
	// Execute mode: a write is queued for approval and the script stops
	// with the deferred envelope's text, approval id included.
	f := newAccessFixture(t, nil)
	res := route(t, f.gw, f.member, CodeModeExecuteToolCode, map[string]any{
		"code": "r = alpha.run()\nprint(\"after the write\")\nresult = r",
	})
	text := textOf(res)
	// The print never ran: the words appear only inside the derived reason
	// the reviewer is shown, never as a print line of their own.
	if !res.IsError || strings.Contains(text, "\nafter the write\n") {
		t.Fatalf("script continued past a held call:\n%s", text)
	}
	if !strings.Contains(text, "Error in run: tool call failed for alpha.run: Tool execution requires human approval. Your call has been queued.") ||
		!strings.Contains(text, "approval_id: ap_") {
		t.Fatalf("held text missing:\n%s", text)
	}
	if f.calls.Load() != 0 {
		t.Fatal("the held tool ran")
	}
	pending, err := f.bus.ListPendingByAgent(context.Background(), memberID)
	if err != nil || len(pending) != 1 || pending[0].ToolName != "alpha.run" || pending[0].RaisedBy == nil || pending[0].RaisedBy.Via != codemode.Via {
		t.Fatalf("pending approvals = %+v (err %v)", pending, err)
	}
	ev, _ := lastEvent(f.met, "alpha.run")
	if ev.Outcome != metrics.OutcomeDeferred || ev.Via != codemode.Via {
		t.Fatalf("held call metric = %+v", ev)
	}

	// Inbox mode: the coaching answer aborts the script the same way.
	in := newInboxFixture(t)
	in.mode = ApprovalModeInbox
	res, err = in.gw.RouteCall(in.agent, "test", CodeModeExecuteToolCode, map[string]any{
		"code": "r = deploy.run(service=\"api\", env=\"prod\")\nprint(\"after\")\nresult = r",
	})
	if err != nil {
		t.Fatal(err)
	}
	text = textOf(res)
	if !res.IsError || strings.Contains(text, "\nafter\n") {
		t.Fatalf("coached script continued:\n%s", text)
	}
	if !strings.Contains(text, "tool call failed for deploy.run: deploy.run is restricted. Nothing ran and nothing was sent to your owner.") {
		t.Fatalf("coaching text missing:\n%s", text)
	}
	if in.calls.Load() != 0 {
		t.Fatal("the restricted tool ran")
	}
}

// TestToolsExecuteForwardsOuterReason covers the fix for tools.execute:
// its outer _reason was stripped before the handler ran, so the inner call
// used to fail "_reason too short" unless arguments._reason was set.
func TestToolsExecuteForwardsOuterReason(t *testing.T) {
	f := newAccessFixture(t, nil)
	res := route(t, f.gw, f.member, MetaExecuteTool, map[string]any{
		ReasonField: outerReason, "tool": "alpha.get_status", "arguments": map[string]any{},
	})
	if res.IsError || textOf(res) != "ran" {
		t.Fatalf("tools.execute without an inner reason: isError=%v %q", res.IsError, textOf(res))
	}
	ev, ok := lastEvent(f.met, "alpha.get_status")
	if !ok || ev.ReasonText != outerReason || ev.Via != MetaExecuteTool {
		t.Fatalf("inner call metric = %+v", ev)
	}
	// An inner reason still wins.
	res = route(t, f.gw, f.member, MetaExecuteTool, map[string]any{
		ReasonField: outerReason, "tool": "alpha.get_status",
		"arguments": map[string]any{ReasonField: "the inner reason, spelt out for this call"},
	})
	if res.IsError {
		t.Fatalf("tools.execute with an inner reason: %s", textOf(res))
	}
	if ev, _ := lastEvent(f.met, "alpha.get_status"); ev.ReasonText != "the inner reason, spelt out for this call" {
		t.Fatalf("inner reason metric = %+v", ev)
	}
	// Missing arguments entirely.
	res = route(t, f.gw, f.member, MetaExecuteTool, map[string]any{ReasonField: outerReason, "tool": "alpha.get_status"})
	if res.IsError || textOf(res) != "ran" {
		t.Fatalf("tools.execute with no arguments: %s", textOf(res))
	}
}

func TestCodeModeLimitsAreConfigurable(t *testing.T) {
	f := newAccessFixture(t, nil)
	f.gw.SetCodeModeLimits(codemode.Limits{MaxCalls: 2})
	res := route(t, f.gw, f.admin, CodeModeExecuteToolCode, map[string]any{
		"code": "for i in range(5):\n  alpha.get_status()\nresult = 1",
	})
	if !res.IsError || !strings.Contains(textOf(res), "this script already made 2 tool calls") || f.calls.Load() != 2 {
		t.Fatalf("call limit: calls=%d\n%s", f.calls.Load(), textOf(res))
	}
	if l := f.gw.codeModeRuntime().Limits(); l.MaxCalls != 2 || l.ScriptTimeout != codemode.DefaultLimits().ScriptTimeout {
		t.Fatalf("limits = %+v", l)
	}
}

// recorder is a fake upstream for the replay: every call is kept with the
// arguments the tool saw, and answered from a table.
type recorder struct {
	mu      sync.Mutex
	calls   []recordedCall
	answers map[string]func(args map[string]any) any
}

type recordedCall struct {
	tool string
	args map[string]any
}

func (r *recorder) handler(tool string) directHandler {
	return func(_ context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		r.mu.Lock()
		r.calls = append(r.calls, recordedCall{tool: tool, args: args})
		answer := r.answers[tool]
		r.mu.Unlock()
		var v any = map[string]any{"ok": true}
		if answer != nil {
			v = answer(args)
		}
		b, _ := json.Marshal(v)
		return mcp.NewToolResultText(string(b)), nil
	}
}

func (r *recorder) toolNames() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.calls))
	for i, c := range r.calls {
		out[i] = c.tool
	}
	return out
}

func (r *recorder) argsOf(i int) map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Through JSON so Starlark's int64/[]any shapes compare as the wire
	// would carry them.
	b, _ := json.Marshal(r.calls[i].args)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

// newReplayGateway registers fake tools under the exact server names the
// skills hardcode. Writes are force-allowed: the replay proves parsing,
// dispatch and argument fidelity; approval behaviour has its own tests.
func newReplayGateway(t *testing.T, rec *recorder) *Gateway {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "replay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	bus, err := approval.New(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	gw := New(Options{Policy: policy.New(db), Approval: bus, Audit: audit.New(db), Hub: realtime.NewHub(), Memory: memory.New(db), Metrics: &fakeMetrics{}})
	gw.RegisterBuiltins()
	t.Cleanup(func() { _ = gw.Close() })
	servers := map[string][]string{
		"BkCoreServices":  {"get_client", "get_data_insight", "trigger_extraction", "seller_central_proxy"},
		"LinearForUsers":  {"save_issue"},
		"LinearForT3Code": {"get_issue", "list_comments", "save_comment", "save_issue"},
		"BkDocsServices":  {"log_recommendation"},
	}
	for up, tools := range servers {
		for _, short := range tools {
			name := up + "." + short
			gw.registerEntry(toolEntry{
				tool:     mcp.Tool{Name: name, Description: descriptionBanner + short, InputSchema: mcp.ToolInputSchema{Type: "object", Properties: addMetaProps(map[string]any{})}},
				upstream: up, originalName: short, reasonField: ReasonField, handle: rec.handler(name), forcedAction: &actionAllow,
			})
		}
	}
	return gw
}

// TestCodeModeReplaysBifrostSkillSnippets runs code bodies lifted from
// bk-docs skills, as written for Bifrost, against fakes registered under the
// server names those skills hardcode.
func TestCodeModeReplaysBifrostSkillSnippets(t *testing.T) {
	rec := &recorder{answers: map[string]func(map[string]any) any{}}
	rec.answers["BkCoreServices.get_client"] = func(args map[string]any) any {
		return map[string]any{"data": map[string]any{"client_id": args["clientId"], "amz_seller_id": "A2XSELLER", "chat_id": "-100123"}}
	}
	rec.answers["BkCoreServices.get_data_insight"] = func(map[string]any) any {
		return []any{map[string]any{"child_asin": "B0001", "parent_id": "P1"}, map[string]any{"child_asin": "B0002", "parent_id": "P1"}}
	}
	rec.answers["BkCoreServices.trigger_extraction"] = func(map[string]any) any {
		return map[string]any{"traceId": "tr_9f2", "status": "created"}
	}
	rec.answers["LinearForUsers.save_issue"] = func(args map[string]any) any {
		return map[string]any{"id": args["id"], "state": args["state"]}
	}
	rec.answers["BkDocsServices.log_recommendation"] = func(args map[string]any) any {
		if args["dryRun"] == true {
			return map[string]any{"status": "dry_run", "validation": map[string]any{"ok": true}}
		}
		return map[string]any{"status": "written", "rows": 1}
	}
	var saved []any
	rec.answers["LinearForT3Code.get_issue"] = func(args map[string]any) any {
		return map[string]any{"id": args["id"], "state": "Triage"}
	}
	rec.answers["LinearForT3Code.list_comments"] = func(map[string]any) any { return append([]any{}, saved...) }
	rec.answers["LinearForT3Code.save_comment"] = func(args map[string]any) any {
		saved = append(saved, map[string]any{"id": "c1", "body": args["body"]})
		return map[string]any{"id": "c1"}
	}
	rec.answers["LinearForT3Code.save_issue"] = func(args map[string]any) any {
		return map[string]any{"id": args["id"], "state": args["state"]}
	}
	gw := newReplayGateway(t, rec)
	ctx := WithAgentID(context.Background(), "ag_replay")
	run := func(t *testing.T, code string) string {
		t.Helper()
		res, err := gw.RouteCall(ctx, "test", CodeModeExecuteToolCode, map[string]any{"code": code})
		if err != nil {
			t.Fatal(err)
		}
		if res.IsError {
			t.Fatalf("snippet failed:\n%s", textOf(res))
		}
		return textOf(res)
	}
	reset := func() {
		rec.mu.Lock()
		rec.calls = nil
		rec.mu.Unlock()
		saved = nil
	}

	// Skills/resolvers/res-account/SKILL.md
	t.Run("res-account get_client", func(t *testing.T) {
		reset()
		out := run(t, `result = BkCoreServices.get_client(clientId="vai-us")`)
		if got := rec.toolNames(); !reflect.DeepEqual(got, []string{"BkCoreServices.get_client"}) {
			t.Fatalf("calls = %v", got)
		}
		if !reflect.DeepEqual(rec.argsOf(0), map[string]any{"clientId": "vai-us"}) {
			t.Fatalf("args = %v", rec.argsOf(0))
		}
		if !strings.Contains(out, "\"chat_id\": \"-100123\"") {
			t.Fatalf("output:\n%s", out)
		}
	})

	// Skills/navigators/skills/nav-mcp/skills/nav-mcp-bk-data-insights/SKILL.md
	t.Run("data insights get_data_insight", func(t *testing.T) {
		reset()
		out := run(t, `result = BkCoreServices.get_data_insight(
    queryId="get-child-asins",
    parameters={"accountId": "ols-us"},
)`)
		if !reflect.DeepEqual(rec.argsOf(0), map[string]any{"queryId": "get-child-asins", "parameters": map[string]any{"accountId": "ols-us"}}) {
			t.Fatalf("args = %v", rec.argsOf(0))
		}
		if !strings.Contains(out, "Return value: [\n  {\n    \"child_asin\": \"B0001\"") {
			t.Fatalf("output:\n%s", out)
		}
	})

	// Skills/beknown-growth/2-qualify-fast/bev-seller-metrics/SKILL.md
	t.Run("seller metrics trigger_extraction", func(t *testing.T) {
		reset()
		out := run(t, `# In mcp__bifrost__executeToolCode
result = BkCoreServices.trigger_extraction(
    reportId="helium10_blackbox_asin_data",
    workerId="helium10",
    parameters={"seller_name": "<SELLER NAME>", "marketplace": "<MARKETPLACE>"},
    priority=99
)
# Capture result["traceId"] for status polling`)
		want := map[string]any{
			"reportId": "helium10_blackbox_asin_data", "workerId": "helium10",
			"parameters": map[string]any{"seller_name": "<SELLER NAME>", "marketplace": "<MARKETPLACE>"}, "priority": float64(99),
		}
		if !reflect.DeepEqual(rec.argsOf(0), want) {
			t.Fatalf("args = %v", rec.argsOf(0))
		}
		if !strings.Contains(out, "\"traceId\": \"tr_9f2\"") {
			t.Fatalf("output:\n%s", out)
		}
		if ev, _ := lastEvent(gw.metrics.(*fakeMetrics), "BkCoreServices.trigger_extraction"); ev.ReasonText != "code mode: In mcp__bifrost__executeToolCode" || ev.Via != codemode.Via {
			t.Fatalf("nested metric = %+v", ev)
		}
	})

	// Skills/ops-collaboration/linear/skills/workspace/SKILL.md
	t.Run("linear workspace save_issue", func(t *testing.T) {
		reset()
		run(t, `result = LinearForUsers.save_issue(id="TEAM-123", state="Exact Live State Name")`)
		if got := rec.toolNames(); !reflect.DeepEqual(got, []string{"LinearForUsers.save_issue"}) {
			t.Fatalf("calls = %v", got)
		}
		if !reflect.DeepEqual(rec.argsOf(0), map[string]any{"id": "TEAM-123", "state": "Exact Live State Name"}) {
			t.Fatalf("args = %v", rec.argsOf(0))
		}
	})

	// Skills/decisions/d-ads/skills/d-ad-cst-stop-negative-target/SKILL.md
	// (dry run, then the real call on a clean validation)
	t.Run("negative target log_recommendation", func(t *testing.T) {
		reset()
		out := run(t, `rows = [{"client_id": "rou-uk", "entity_id": "kw-77", "decision": "negate", "match_type": "exact"}]
context = {"runId": "run-2026-09-30-01", "skill": "d-ad-cst-stop-negative-target"}
res = BkDocsServices.log_recommendation(
    contract="negative_target_decision",
    rows=rows,
    reasonForChange="Recommend negating 'cocoa butter' (exact) on rou-uk — 41 clicks, 0 orders in 30 days",
    context=context,
    dryRun=True,
)
# res["status"] == "dry_run"; proceed only when res["validation"]["ok"] is true.
if res["status"] == "dry_run" and res["validation"]["ok"]:
    res = BkDocsServices.log_recommendation(contract="negative_target_decision", rows=rows, reasonForChange="Recommend negating 'cocoa butter' (exact) on rou-uk — 41 clicks, 0 orders in 30 days", context=context, dryRun=False)
result = res`)
		if got := rec.toolNames(); !reflect.DeepEqual(got, []string{"BkDocsServices.log_recommendation", "BkDocsServices.log_recommendation"}) {
			t.Fatalf("calls = %v", got)
		}
		first, second := rec.argsOf(0), rec.argsOf(1)
		if first["dryRun"] != true || second["dryRun"] != false || first["contract"] != "negative_target_decision" {
			t.Fatalf("dryRun flags = %v / %v", first["dryRun"], second["dryRun"])
		}
		rows, _ := first["rows"].([]any)
		if len(rows) != 1 || rows[0].(map[string]any)["entity_id"] != "kw-77" || first["context"].(map[string]any)["runId"] != "run-2026-09-30-01" {
			t.Fatalf("payload = %v", first)
		}
		if !strings.Contains(out, "\"status\": \"written\"") {
			t.Fatalf("output:\n%s", out)
		}
	})

	// Skills/ops-collaboration/linear/references/state-transition-audit.md
	// (the full template: a function, % formatting, triple-quoted strings,
	// repr, six calls with control flow between them)
	t.Run("linear state transition audit", func(t *testing.T) {
		reset()
		out := run(t, `def main():
    issue = "ISSUE_IDENTIFIER"
    destination = "EXACT_LIVE_DESTINATION"
    marker = "<!-- linear-state-transition:v1:ISSUE_IDENTIFIER:FROM_SLUG:TO_SLUG:RUN_KEY -->"
    triage_destination = "NONE"
    readiness = "NONE"
    triage_fields = ""
    triage_marker = ""
    if triage_destination != "NONE":
        triage_fields = """**Proposed destination:** `+"`%s`"+`
**Readiness:** %s
""" % (triage_destination, readiness)
        triage_marker = "<!-- linear-triage-decision:v1:ISSUE_IDENTIFIER:RUN_KEY -->"
    body = """## State transition

**From:** `+"`EXACT_LIVE_SOURCE`"+`
**To:** `+"`EXACT_LIVE_DESTINATION`"+`
**Type:** `+"`EXACT_TEAM_TYPE`"+`
**Reason:** REASON
**Evidence:** EVIDENCE
%s
**Approval:** APPROVAL_OR_GOVERNING_RULE
**Automation:** `+"`LinearForT3Code`"+`

%s
%s""" % (triage_fields, marker, triage_marker)

    before = LinearForT3Code.get_issue(id=issue)
    existing = LinearForT3Code.list_comments(issueId=issue, limit=250)
    existing_text = repr(existing)
    complete = marker in existing_text
    if triage_marker != "" and triage_marker not in existing_text:
        complete = False
    if not complete:
        LinearForT3Code.save_comment(issueId=issue, body=body)
    verified = LinearForT3Code.list_comments(issueId=issue, limit=250)
    verified_text = repr(verified)
    verified_complete = marker in verified_text
    if triage_marker != "" and triage_marker not in verified_text:
        verified_complete = False
    if not verified_complete:
        return {"outcome": "audit_missing", "before": before}
    moved = LinearForT3Code.save_issue(id=issue, state=destination)
    after = LinearForT3Code.get_issue(id=issue)
    return {"outcome": "moved", "before": before, "moved": moved, "after": after}

result = main()`)
		want := []string{
			"LinearForT3Code.get_issue", "LinearForT3Code.list_comments", "LinearForT3Code.save_comment",
			"LinearForT3Code.list_comments", "LinearForT3Code.save_issue", "LinearForT3Code.get_issue",
		}
		if got := rec.toolNames(); !reflect.DeepEqual(got, want) {
			t.Fatalf("calls = %v\nwant  %v", got, want)
		}
		comment := rec.argsOf(2)
		body, _ := comment["body"].(string)
		if comment["issueId"] != "ISSUE_IDENTIFIER" || !strings.Contains(body, "<!-- linear-state-transition:v1:ISSUE_IDENTIFIER:FROM_SLUG:TO_SLUG:RUN_KEY -->") ||
			!strings.Contains(body, "**Automation:** `LinearForT3Code`") {
			t.Fatalf("save_comment args = %v", comment)
		}
		if rec.argsOf(1)["limit"] != float64(250) || rec.argsOf(4)["state"] != "EXACT_LIVE_DESTINATION" {
			t.Fatalf("list_comments/save_issue args = %v / %v", rec.argsOf(1), rec.argsOf(4))
		}
		if !strings.Contains(out, "\"outcome\": \"moved\"") || strings.Contains(out, "audit_missing") {
			t.Fatalf("output:\n%s", out)
		}
	})

	// Skills/decisions/d-pricing/skills/creating-a-discount/skills/listing-sale-price/SKILL.md
	// (no result assigned: the tool logs are the whole output)
	t.Run("listing sale price read", func(t *testing.T) {
		reset()
		out := run(t, `# seller id (the {sellerId} path part) — from the client record
client = BkCoreServices.get_client(clientId="njg-us")          # -> data.amz_seller_id
# read a listing
BkCoreServices.seller_central_proxy(accountId="njg-us",
    endpoint="/listings/2021-08-01/items/{sellerId}/{sku}", method="GET",
    queryParams={"includedData": "attributes,summaries,offers"})`)
		if got := rec.toolNames(); !reflect.DeepEqual(got, []string{"BkCoreServices.get_client", "BkCoreServices.seller_central_proxy"}) {
			t.Fatalf("calls = %v", got)
		}
		want := map[string]any{"accountId": "njg-us", "endpoint": "/listings/2021-08-01/items/{sellerId}/{sku}", "method": "GET", "queryParams": map[string]any{"includedData": "attributes,summaries,offers"}}
		if !reflect.DeepEqual(rec.argsOf(1), want) {
			t.Fatalf("proxy args = %v", rec.argsOf(1))
		}
		if !strings.HasPrefix(out, "Print output:\n[TOOL] BkCoreServices.get_client raw response:") || strings.Contains(out, "Return value:") {
			t.Fatalf("output:\n%s", out)
		}
	})
}
