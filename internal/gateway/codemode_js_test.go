package gateway

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/access"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/codemode"
)

// TestCodeModeJavaScriptThroughTheGateway: a JavaScript script runs in the
// worker, its nested calls take the full pipeline (via code_mode), and the
// outer call's audit rows and every nested call's share one run id, which
// the result's _meta also carries with the call ledger.
func TestCodeModeJavaScriptThroughTheGateway(t *testing.T) {
	rec := &recorder{answers: map[string]func(map[string]any) any{
		"BkCoreServices.get_client": func(args map[string]any) any { return map[string]any{"id": args["clientId"]} },
	}}
	gw := newReplayGateway(t, rec)
	ctx := context.Background()
	res := route(t, gw, ctx, CodeModeExecuteToolCode, map[string]any{
		"language": "javascript",
		"code": `const c = await tools.BkCoreServices.get_client({ clientId: "vai-us" });
const r = await tools.tryCall("LinearForT3Code.get_issue", { id: "TEC-1" });
console.log("issue ok:", r.ok);
return { id: c.id, ok: r.ok };`,
		ReasonField: outerReason,
	})
	text := textOf(res)
	if res.IsError {
		t.Fatalf("script failed:\n%s", text)
	}
	for _, want := range []string{`"id": "vai-us"`, `"ok": true`, "issue ok: true", "  Tool calls: 2\n    1. BkCoreServices.get_client succeeded "} {
		if !strings.Contains(text, want) {
			t.Errorf("output missing %q:\n%s", want, text)
		}
	}
	if got := rec.toolNames(); len(got) != 2 || got[0] != "BkCoreServices.get_client" || got[1] != "LinearForT3Code.get_issue" {
		t.Fatalf("nested calls %v", got)
	}
	if args := rec.argsOf(0); args["clientId"] != "vai-us" {
		t.Fatalf("nested args %v", args)
	}
	if res.Meta == nil {
		t.Fatal("no _meta on the result")
	}
	runID, _ := res.Meta.AdditionalFields["toolyard.run_id"].(string)
	calls, _ := res.Meta.AdditionalFields["toolyard.calls"].([]codemode.LedgerEntry)
	if !strings.HasPrefix(runID, "cm_") || len(calls) != 2 || calls[1].Tool != "LinearForT3Code.get_issue" || calls[1].Status != codemode.StatusSucceeded {
		t.Fatalf("_meta = %+v", res.Meta.AdditionalFields)
	}
	evs, err := gw.audit.Query(ctx, audit.Filter{RunID: runID, Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, e := range evs {
		if e.RunID != runID {
			t.Fatalf("row %+v has run id %q", e, e.RunID)
		}
		seen[e.ToolName] = true
	}
	for _, tool := range []string{CodeModeExecuteToolCode, "BkCoreServices.get_client", "LinearForT3Code.get_issue"} {
		if !seen[tool] {
			t.Errorf("no audit row of run %s for %s: %+v", runID, tool, evs)
		}
	}
	// A second run gets its own id; a call outside code mode gets none.
	res2 := route(t, gw, ctx, CodeModeExecuteToolCode, map[string]any{"code": "result = 1"})
	if id2, _ := res2.Meta.AdditionalFields["toolyard.run_id"].(string); id2 == "" || id2 == runID {
		t.Fatalf("second run id %q", id2)
	}
	route(t, gw, ctx, "BkCoreServices.get_client", map[string]any{"clientId": "direct", ReasonField: outerReason})
	all, err := gw.audit.Query(ctx, audit.Filter{Tool: "BkCoreServices.get_client", Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	direct := 0
	for _, e := range all {
		if strings.Contains(string(e.Arguments), "direct") {
			direct++
			if e.RunID != "" {
				t.Fatalf("a direct call has run id %q", e.RunID)
			}
		}
	}
	if direct == 0 {
		t.Fatal("no audit row for the direct call")
	}

	// An unknown language is a clear error; echo none keeps tool answers
	// out of the text.
	if res := route(t, gw, ctx, CodeModeExecuteToolCode, map[string]any{"code": "x", "language": "lua"}); !res.IsError || !strings.Contains(textOf(res), `unknown language "lua"`) {
		t.Fatalf("lua: %s", textOf(res))
	}
	res = route(t, gw, ctx, CodeModeExecuteToolCode, map[string]any{"code": `result = BkCoreServices.get_client(clientId="x")`, "echo": "none"})
	if res.IsError || strings.Contains(textOf(res), "[TOOL]") || !strings.Contains(textOf(res), "  Tool calls: 1") {
		t.Fatalf("echo none:\n%s", textOf(res))
	}
}

func TestCodeModeJavaScriptStubs(t *testing.T) {
	gw := newReplayGateway(t, &recorder{})
	ctx := context.Background()
	list := textOf(route(t, gw, ctx, CodeModeListToolFiles, map[string]any{"language": "javascript"}))
	for _, want := range []string{"  BkCoreServices/\n    get_client.d.ts\n", "  index.d.ts", "  toolyard/\n"} {
		if !strings.Contains(list, want) {
			t.Errorf("listing missing %q:\n%s", want, list)
		}
	}
	if py := textOf(route(t, gw, ctx, CodeModeListToolFiles, map[string]any{})); !strings.Contains(py, "get_client.pyi") || strings.Contains(py, ".d.ts") {
		t.Errorf("the default listing is still .pyi:\n%s", py)
	}
	if res := route(t, gw, ctx, CodeModeListToolFiles, map[string]any{"language": "cobol"}); !res.IsError {
		t.Errorf("cobol listing: %s", textOf(res))
	}
	stub := route(t, gw, ctx, CodeModeReadToolFile, map[string]any{"fileName": "servers/BkCoreServices/get_client.d.ts"})
	if stub.IsError || !strings.Contains(textOf(stub), "export interface Input") || strings.Contains(textOf(stub), "_reason") {
		t.Fatalf("stub:\n%s", textOf(stub))
	}
	index := route(t, gw, ctx, CodeModeReadToolFile, map[string]any{"fileName": "servers/index.d.ts"})
	if index.IsError || !strings.Contains(textOf(index), "BkCoreServices: import(\"./BkCoreServices\").Tools;") {
		t.Fatalf("index:\n%s", textOf(index))
	}
}

func TestToolsSearchModes(t *testing.T) {
	gw := newReplayGateway(t, &recorder{})
	ctx := context.Background()
	search := func(args map[string]any) map[string]any {
		t.Helper()
		args[ReasonField] = outerReason
		res := route(t, gw, ctx, MetaSearchTool, args)
		if res.IsError {
			t.Fatalf("tools.search %v: %s", args, textOf(res))
		}
		return structured(t, res)
	}
	refused := func(args map[string]any, want string) {
		t.Helper()
		args[ReasonField] = outerReason
		res := route(t, gw, ctx, MetaSearchTool, args)
		if !res.IsError || !strings.Contains(textOf(res), want) {
			t.Errorf("tools.search %v: %v %s", args, res.IsError, textOf(res))
		}
	}
	names := func(body map[string]any) []string {
		var out []string
		for _, r := range body["results"].([]any) {
			out = append(out, r.(map[string]any)["name"].(string))
		}
		return out
	}

	body := search(map[string]any{"query": "save issue t3code"})
	got := names(body)
	if len(got) == 0 || got[0] != "LinearForT3Code.save_issue" || body["mode"] != "keyword" {
		t.Fatalf("keyword: %v %v", body["mode"], got)
	}
	first := body["results"].([]any)[0].(map[string]any)
	// The MCP tool keeps returning input schemas by default (full).
	if first["code"] != "LinearForT3Code.save_issue" || first["upstream"] != "LinearForT3Code" || first["input_schema"] == nil || first["score"] == nil {
		t.Fatalf("default (full) entry %v", first)
	}
	summary := search(map[string]any{"query": "save issue t3code", "detail": "summary"})["results"].([]any)[0].(map[string]any)
	if summary["input_schema"] != nil || summary["name"] != "LinearForT3Code.save_issue" {
		t.Fatalf("summary entry %v", summary)
	}
	for _, n := range got {
		if n == MetaSearchTool || n == MetaExecuteTool {
			t.Fatalf("the meta-tools must not appear: %v", got)
		}
	}

	body = search(map[string]any{"query": `^BkCoreServices\.get_`, "mode": "regex"})
	if got := names(body); len(got) != 2 || got[0] != "BkCoreServices.get_client" || got[1] != "BkCoreServices.get_data_insight" {
		t.Fatalf("regex: %v", got)
	}
	body = search(map[string]any{"query": "client", "detail": "full", "limit": 1})
	entry := body["results"].([]any)[0].(map[string]any)
	if body["total"].(float64) != 1 || body["matched"].(float64) < 1 || entry["input_schema"] == nil {
		t.Fatalf("full detail: %v", body)
	}
	body = search(map[string]any{"query": "client", "detail": "name"})
	for _, r := range body["results"].([]any) {
		if len(r.(map[string]any)) != 1 {
			t.Fatalf("name detail carries more than the name: %v", r)
		}
	}
	body = search(map[string]any{"query": "issue", "upstream": "LinearForUsers"})
	if got := names(body); len(got) != 1 || got[0] != "LinearForUsers.save_issue" {
		t.Fatalf("upstream filter: %v", got)
	}
	if search(map[string]any{"query": "issue", "mode": "auto"})["mode"] != "keyword" {
		t.Fatal("auto should run keyword")
	}
	refused(map[string]any{"query": "x", "mode": "fuzzy"}, "unknown search mode")
	refused(map[string]any{"query": "x", "mode": "semantic"}, "semantic search is not configured")
	refused(map[string]any{"query": strings.Repeat("a", 201), "mode": "regex"}, "at most 200")
	refused(map[string]any{"query": "(", "mode": "regex"}, "not a valid RE2")
	refused(map[string]any{"query": "x", "detail": "everything"}, "unknown detail")
}

// TestCodeModeCannotNestAtAnyDepth (review P1-2): a script reaches the
// code-mode tools neither directly through tools.execute nor through
// tools.execute calling tools.execute; every hop below a script refuses
// them, whatever via it records.
func TestCodeModeCannotNestAtAnyDepth(t *testing.T) {
	gw := newReplayGateway(t, &recorder{})
	ctx := context.Background()
	for _, code := range []string{
		`result = toolyard.tools_execute(tool="executeToolCode", arguments={"code": "result = 1"})`,
		`result = toolyard.tools_execute(tool="tools.execute", arguments={"tool": "executeToolCode", "arguments": {"code": "result = 1"}})`,
		`r = try_call("toolyard.tools_execute", {"tool": "listToolFiles", "arguments": {}})
result = r`,
	} {
		res := route(t, gw, ctx, CodeModeExecuteToolCode, map[string]any{"code": code})
		text := textOf(res)
		if !strings.Contains(text, "cannot be called from code mode") && !strings.Contains(text, "cannot target itself") {
			t.Errorf("nested code mode was not refused:\n%s\n%s", code, text)
		}
	}
	for _, tool := range CodeModeToolNames {
		res, err := gw.RouteCall(withinCodeMode(ctx), MetaExecuteTool, tool, map[string]any{"code": "result = 1", "fileName": "servers/x/y.pyi", "server": "x", "tool": "y"})
		if err != nil || !res.IsError || !strings.Contains(textOf(res), "cannot be called from code mode") {
			t.Errorf("%s below a script: %v %v %s", tool, err, res.IsError, textOf(res))
		}
	}
	if res := route(t, gw, ctx, CodeModeListToolFiles, map[string]any{}); res.IsError {
		t.Fatalf("outside a script listToolFiles works: %s", textOf(res))
	}
}

// TestToolsSearchSharesCatalogForScope (review P1-6): search shows exactly
// the tools CatalogFor lists for the caller, the agent's own denies
// included.
func TestToolsSearchSharesCatalogForScope(t *testing.T) {
	f := newAccessFixture(t, nil)
	f.res.set(memberID, access.Scope{Groups: map[string]bool{"alpha": true},
		Limits: access.AgentScope{DenyTools: []access.ToolRef{{Server: "alpha", Tool: "alpha.run"}}}.Compile()})
	res := f.call(t, f.member, MetaSearchTool, map[string]any{"query": "", "limit": 200, "detail": "name"})
	if res.IsError {
		t.Fatalf("search: %s", textOf(res))
	}
	got := map[string]bool{}
	for _, r := range structured(t, res)["results"].([]any) {
		got[r.(map[string]any)["name"].(string)] = true
	}
	want := map[string]bool{}
	for _, c := range f.gw.CatalogFor(f.member) {
		if c.Name != MetaSearchTool && c.Name != MetaExecuteTool {
			want[c.Name] = true
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("search and CatalogFor differ:\n search %v\n catalog %v", got, want)
	}
	if got["alpha.run"] || !got["alpha.get_status"] || got["beta.run"] {
		t.Fatalf("scope not applied: %v", got)
	}
	regex := f.call(t, f.member, MetaSearchTool, map[string]any{"query": `^alpha\.`, "mode": "regex", "detail": "name"})
	for _, r := range structured(t, regex)["results"].([]any) {
		if r.(map[string]any)["name"] == "alpha.run" {
			t.Fatal("regex search shows a denied tool")
		}
	}
}
