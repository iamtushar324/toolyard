package codemode

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

// call is one nested call the fake saw.
type call struct {
	via, target string
	args        map[string]any
}

// fakeCaller answers RouteCall from a table of handlers keyed by target.
type fakeCaller struct {
	tools    []Tool
	handlers map[string]func(args map[string]any) (*mcp.CallToolResult, error)
	mu       sync.Mutex
	calls    []call
}

func (f *fakeCaller) Tools(context.Context) []Tool { return f.tools }

func (f *fakeCaller) RouteCall(_ context.Context, via, target string, args map[string]any) (*mcp.CallToolResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, call{via: via, target: target, args: args})
	f.mu.Unlock()
	h, ok := f.handlers[target]
	if !ok {
		return mcp.NewToolResultErrorf("tool %q not found in catalog", target), nil
	}
	return h(args)
}

func (f *fakeCaller) seen() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]call(nil), f.calls...)
}

func (f *fakeCaller) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}

func jsonResult(v any) (*mcp.CallToolResult, error) {
	b, _ := json.Marshal(v)
	return mcp.NewToolResultText(string(b)), nil
}

func prop(typ, desc string) map[string]any {
	p := map[string]any{"type": typ}
	if desc != "" {
		p["description"] = desc
	}
	return p
}

// The tool from the task brief, as prod renders it.
var upsertActor = Tool{
	Server: "BkCoreServices", Name: "upsert_bifrost_virtual_key_actor",
	Target:      "BkCoreServices.upsert_bifrost_virtual_key_actor",
	Description: "Create or update a Bifrost Virtual Key actor mapping. Idempotent on (userId, virtualKeyHash).",
	Properties: map[string]any{
		"email":          prop("string", "The actor's email"),
		"userId":         prop("string", "Clerk user id"),
		"virtualKeyHash": prop("string", ""),
		"isActive":       prop("boolean", "Whether the mapping is live"),
		"label":          prop("string", ""),
	},
	Required:    []string{"email", "userId", "virtualKeyHash"},
	ReasonField: "_reason",
}

func newFake() *fakeCaller {
	f := &fakeCaller{handlers: map[string]func(map[string]any) (*mcp.CallToolResult, error){}}
	f.tools = []Tool{
		upsertActor,
		{Server: "BkCoreServices", Name: "get-all-clients", Target: "BkCoreServices.get-all-clients",
			Description: "List every client.", Properties: map[string]any{}, ReasonField: "_reason"},
		{Server: "BkCoreServices", Name: "get_client", Target: "BkCoreServices.get_client",
			Description: "Fetch one client by id", ReasonField: "_reason",
			Properties: map[string]any{"clientId": prop("string", "e.g. vai-us")}, Required: []string{"clientId"}},
		{Server: "memory", Name: "get", Target: "memory.get", Description: "Read a value from toolyard shared memory by key.",
			Properties: map[string]any{"key": prop("string", ""), "scope": prop("string", "")}, Required: []string{"key"}, ReasonField: "_reason"},
	}
	f.handlers["BkCoreServices.get-all-clients"] = func(map[string]any) (*mcp.CallToolResult, error) {
		return jsonResult([]any{map[string]any{"id": "vai-us", "n": 3}, map[string]any{"id": "ols-us", "n": 12345678901234567}})
	}
	f.handlers["BkCoreServices.get_client"] = func(args map[string]any) (*mcp.CallToolResult, error) {
		return jsonResult(map[string]any{"data": map[string]any{"id": args["clientId"], "amz_seller_id": "A1B2"}})
	}
	f.handlers["memory.get"] = func(args map[string]any) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("plain text, not json"), nil
	}
	f.handlers["BkCoreServices.upsert_bifrost_virtual_key_actor"] = func(args map[string]any) (*mcp.CallToolResult, error) {
		return jsonResult(map[string]any{"ok": true, "email": args["email"]})
	}
	return f
}

// inProcess is the fast path for unit tests: same script engine, no child.
func inProcess(f Caller, l Limits) *Runtime {
	rt := New(f, l)
	rt.inProcess = true
	return rt
}

// bothWays runs a test against the in-process engine and the real worker.
func bothWays(t *testing.T, f Caller, l Limits, fn func(t *testing.T, rt *Runtime)) {
	t.Run("in-process", func(t *testing.T) { fn(t, inProcess(f, l)) })
	t.Run("worker", func(t *testing.T) {
		rt := New(f, l)
		rt.SetWorker(testWorker())
		fn(t, rt)
	})
}

func TestCanonicalAndAliasNames(t *testing.T) {
	cases := []struct{ in, canon, alias string }{
		{"get-all-clients", "get_all_clients", ""},
		{"get_client", "get_client", ""},
		{"getClient", "getclient", "getClient"},
		{"Save Issue", "save_issue", ""},
		{"list--items", "list_items", "list__items"},
		{"9lives", "_9lives", ""},
		{"foo.bar", "foobar", ""},
		{"trailing_", "trailing", "trailing_"},
		{"load", "load_", ""},
		{"Import", "import_", "Import"},
		{"", "tool", ""},
	}
	for _, c := range cases {
		if got := canonicalName(c.in); got != c.canon {
			t.Errorf("canonicalName(%q) = %q, want %q", c.in, got, c.canon)
		}
		if got := aliasName(c.in); got != c.alias {
			t.Errorf("aliasName(%q) = %q, want %q", c.in, got, c.alias)
		}
	}
}

func TestServerIdentifiers(t *testing.T) {
	cases := []struct{ in, want string }{
		{"BkCoreServices", "BkCoreServices"},
		{"memory", "memory"},
		{"bk-core", "bk_core"},
		{"9lives", "_9lives"},
		{"load", "load_"},
		{"my.server", "my_server"},
		{"Bk Core", "Bk_Core"},
		{"---", "___"},
		{"!!!", ""},
	}
	for _, c := range cases {
		if got := serverIdent(c.in); got != c.want {
			t.Errorf("serverIdent(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	ping := func(server string) Tool {
		return Tool{Server: server, Name: "ping", Target: server + ".ping", Description: "Ping.", Properties: map[string]any{}, ReasonField: "_reason"}
	}
	f := &fakeCaller{handlers: map[string]func(map[string]any) (*mcp.CallToolResult, error){}}
	f.tools = []Tool{ping("BkCoreServices"), ping("bk-core"), ping("9lives"), ping("load"), ping("!!!")}
	for _, s := range []string{"BkCoreServices", "bk-core", "9lives", "load"} {
		srv := s
		f.handlers[srv+".ping"] = func(map[string]any) (*mcp.CallToolResult, error) { return jsonResult(map[string]any{"from": srv}) }
	}
	rt := inProcess(f, Limits{})
	ctx := context.Background()

	list := rt.ListToolFiles(ctx)
	wantTree := "servers/\n  BkCoreServices/\n    ping.pyi\n  _9lives/\n    ping.pyi\n  bk_core/\n    ping.pyi\n  load_/\n    ping.pyi"
	if !strings.HasSuffix(list, wantTree) || !strings.Contains(list, `# Not callable: server "!!!" (no identifier can be made of the name)`) {
		t.Fatalf("listing:\n%s", list)
	}
	for _, name := range []string{"servers/bk_core/ping.pyi", "servers/bk-core/ping.pyi", "servers/BK-CORE/ping"} {
		stub, ok := rt.ReadToolFile(ctx, name, nil, nil)
		if !ok || !strings.Contains(stub, "# bk_core.ping tool\n# Server \"bk-core\" is called bk_core in code.\n# Usage: bk_core.tool_name(param=value)") ||
			!strings.Contains(stub, `getToolDocs(server="bk_core", tool="tool_name")`) {
			t.Fatalf("%s (ok=%v):\n%s", name, ok, stub)
		}
	}
	if stub, ok := rt.ReadToolFile(ctx, "servers/BkCoreServices/ping.pyi", nil, nil); !ok || strings.Contains(stub, "is called") {
		t.Fatalf("an identifier-shaped key gets no rename note:\n%s", stub)
	}
	if doc, ok := rt.GetToolDocs(ctx, "9lives", "ping"); !ok || !strings.Contains(doc, "# Documentation for _9lives.ping tool") || !strings.Contains(doc, "result = _9lives.ping()") {
		t.Fatalf("docs by key:\n%s", doc)
	}
	if bad, ok := rt.ReadToolFile(ctx, "servers/!!!/ping.pyi", nil, nil); ok || !strings.Contains(bad, "cannot be called from code") {
		t.Fatalf("unbindable key: ok=%v %s", ok, bad)
	}

	text, failed := rt.ExecuteToolCode(ctx, "result = [BkCoreServices.ping()[\"from\"], bk_core.ping()[\"from\"], _9lives.ping()[\"from\"], load_.ping()[\"from\"]]", "")
	if failed || !strings.Contains(text, "Available server keys: BkCoreServices, _9lives, bk_core, load_") ||
		!strings.Contains(text, "[TOOL] bk_core.ping raw response:") || !strings.Contains(text, `"9lives"`) {
		t.Fatalf("calls through sanitised identifiers (failed=%v):\n%s", failed, text)
	}
	if got := f.seen(); len(got) != 4 || got[1].target != "bk-core.ping" || got[2].target != "9lives.ping" || got[3].target != "load.ping" {
		t.Fatalf("targets = %+v", got)
	}

	// Two keys that sanitise alike are both refused.
	f = &fakeCaller{tools: []Tool{ping("bk-core"), ping("bk_core"), ping("ok")}, handlers: map[string]func(map[string]any) (*mcp.CallToolResult, error){}}
	rt = inProcess(f, Limits{})
	list = rt.ListToolFiles(ctx)
	if !strings.Contains(list, `# Not callable: server bk_core (ambiguous, could be "bk-core" or "bk_core")`) || strings.Contains(list, "  bk_core/") {
		t.Fatalf("collision listing:\n%s", list)
	}
	for _, name := range []string{"servers/bk_core/ping.pyi", "servers/bk-core/ping.pyi"} {
		if bad, ok := rt.ReadToolFile(ctx, name, nil, nil); ok || !strings.Contains(bad, "is ambiguous in code") {
			t.Fatalf("%s: ok=%v %s", name, ok, bad)
		}
	}
	if text, failed := rt.ExecuteToolCode(ctx, "result = bk_core.ping()", ""); !failed || !strings.Contains(text, "undefined: bk_core") || !strings.Contains(text, "Available server keys: ok") {
		t.Fatalf("collided key in code:\n%s", text)
	}
}

func TestCollidingNamesAreRefused(t *testing.T) {
	tools := []Tool{
		{Server: "S", Name: "get-client", Target: "S.get-client"},
		{Server: "S", Name: "get_client", Target: "S.get_client"},
		{Server: "S", Name: "Save_Issue", Target: "S.Save_Issue"},
		{Server: "S", Name: "save_issue", Target: "S.save_issue"},
		{Server: "S", Name: "list_all", Target: "S.list_all"},
	}
	cat := buildCatalog(tools)
	if len(cat.servers) != 1 {
		t.Fatalf("servers = %d", len(cat.servers))
	}
	s := cat.servers[0]
	var idents []string
	for _, b := range s.bound {
		idents = append(idents, b.ident)
	}
	// get_client is contested with no way out: neither tool is bound.
	// save_issue is contested but Save_Issue has a unique alias, so that
	// one keeps it and plain save_issue is refused.
	if want := []string{"Save_Issue", "list_all"}; strings.Join(idents, ",") != strings.Join(want, ",") {
		t.Fatalf("bound = %v, want %v", idents, want)
	}
	if got := s.ambiguous["get_client"]; len(got) != 2 {
		t.Fatalf("ambiguous get_client = %v", got)
	}
	if got := s.ambiguous["save_issue"]; len(got) != 2 {
		t.Fatalf("ambiguous save_issue = %v", got)
	}
	if b, names := s.lookup("get_client"); b != nil || len(names) != 2 {
		t.Fatalf("lookup(get_client) = %v %v", b, names)
	}
	if b, names := s.lookup("Save_Issue"); b == nil || names != nil || b.tool.Name != "Save_Issue" {
		t.Fatalf("lookup(Save_Issue) = %v %v", b, names)
	}
	if b, names := s.lookup("SAVE_ISSUE"); b != nil || len(names) != 2 {
		t.Fatalf("lookup(SAVE_ISSUE) should be ambiguous: %v %v", b, names)
	}

	f := &fakeCaller{tools: tools, handlers: map[string]func(map[string]any) (*mcp.CallToolResult, error){}}
	rt := inProcess(f, Limits{})
	text, failed := rt.ExecuteToolCode(context.Background(), `result = S.get_client(clientId="x")`, "")
	if !failed || !strings.Contains(text, `S.get_client is ambiguous: it could be "get-client" or "get_client"`) {
		t.Fatalf("ambiguous call output:\n%s", text)
	}
	if len(f.seen()) != 0 {
		t.Fatalf("an ambiguous call reached RouteCall: %+v", f.seen())
	}
	stub, ok := rt.ReadToolFile(context.Background(), "servers/S/get_client.pyi", nil, nil)
	if ok || !strings.Contains(stub, "ambiguous") {
		t.Fatalf("readToolFile of an ambiguous name: ok=%v\n%s", ok, stub)
	}
	whole, ok := rt.ReadToolFile(context.Background(), "servers/S.pyi", nil, nil)
	if !ok || !strings.Contains(whole, `# Not callable: get_client (ambiguous, could be "get-client" or "get_client")`) {
		t.Fatalf("server stub should flag the ambiguity:\n%s", whole)
	}
}

func TestListToolFilesTree(t *testing.T) {
	rt := inProcess(newFake(), Limits{})
	got := rt.ListToolFiles(context.Background())
	want := strings.Join([]string{
		"# Workflow: listToolFiles -> readToolFile -> (optional) getToolDocs -> executeToolCode",
		"# Filenames below use the exact canonical tool identifiers available in executeToolCode.",
		"# Still call readToolFile before executeToolCode to confirm parameters and return shape.",
		"",
		"servers/",
		"  BkCoreServices/",
		"    get_all_clients.pyi",
		"    get_client.pyi",
		"    upsert_bifrost_virtual_key_actor.pyi",
		"  memory/",
		"    get.pyi",
	}, "\n")
	if got != want {
		t.Fatalf("listToolFiles:\n%s\nwant:\n%s", got, want)
	}

	empty := inProcess(&fakeCaller{}, Limits{})
	if got := empty.ListToolFiles(context.Background()); got != noServersText {
		t.Fatalf("empty catalog: %q", got)
	}
}

func TestReadToolFileStub(t *testing.T) {
	rt := inProcess(newFake(), Limits{})
	ctx := context.Background()
	want := strings.Join([]string{
		"# Total lines: 10 (this is the complete file, no need to paginate)",
		"# BkCoreServices.upsert_bifrost_virtual_key_actor tool",
		"# Usage: BkCoreServices.tool_name(param=value)",
		"# The def names below are the exact callable names to use in executeToolCode.",
		"# Read this file before executeToolCode to confirm parameters and return shape.",
		`# For detailed docs: use getToolDocs(server="BkCoreServices", tool="tool_name")`,
		"# Note: Descriptions may be truncated. Use getToolDocs for full details.",
		"",
		"def upsert_bifrost_virtual_key_actor(email: str, userId: str, virtualKeyHash: str, isActive: bool = None, label: str = None) -> dict:  # Create or update a Bifrost Virtual Key actor mapping.",
		"",
	}, "\n")
	got, ok := rt.ReadToolFile(ctx, "servers/BkCoreServices/upsert_bifrost_virtual_key_actor.pyi", nil, nil)
	if !ok || got != want {
		t.Fatalf("stub (ok=%v):\n%s\nwant:\n%s", ok, got, want)
	}

	// Case-insensitive, extension optional, and the canonical form of a
	// hyphenated upstream name all find the same file.
	for _, name := range []string{"servers/bkcoreservices/GET_ALL_CLIENTS", "servers/BkCoreServices/get-all-clients.pyi", "BkCoreServices/get_all_clients"} {
		s, ok := rt.ReadToolFile(ctx, name, nil, nil)
		if !ok || !strings.Contains(s, "def get_all_clients() -> dict:  # List every client.") {
			t.Fatalf("%s (ok=%v):\n%s", name, ok, s)
		}
	}

	// A server-level file lists every tool.
	s, ok := rt.ReadToolFile(ctx, "servers/BkCoreServices.pyi", nil, nil)
	if !ok || !strings.Contains(s, "# BkCoreServices server tools") ||
		!strings.Contains(s, "def get_all_clients()") || !strings.Contains(s, "def get_client(clientId: str)") ||
		!strings.Contains(s, "def upsert_bifrost_virtual_key_actor(") {
		t.Fatalf("server-level stub (ok=%v):\n%s", ok, s)
	}

	// Partial reads are 1-based, inclusive and clamped.
	one, two := 1, 2
	part, ok := rt.ReadToolFile(ctx, "servers/BkCoreServices/get_client.pyi", &one, &two)
	if !ok || part != "# Total lines: 10 (this is the complete file, no need to paginate)\n# BkCoreServices.get_client tool" {
		t.Fatalf("lines 1-2:\n%s", part)
	}
	nine, big := 9, 500
	tail, ok := rt.ReadToolFile(ctx, "servers/BkCoreServices/get_client.pyi", &nine, &big)
	if !ok || tail != "def get_client(clientId: str) -> dict:  # Fetch one client by id\n" {
		t.Fatalf("lines 9-500:\n%q", tail)
	}
	zero := 0
	first, _ := rt.ReadToolFile(ctx, "servers/BkCoreServices/get_client.pyi", &zero, &zero)
	if !strings.HasPrefix(first, "# Total lines:") || strings.Contains(first, "\n") {
		t.Fatalf("lines 0-0 should clamp to line 1: %q", first)
	}

	// Misses explain themselves.
	miss, ok := rt.ReadToolFile(ctx, "servers/BkCoreServices/nope.pyi", nil, nil)
	if ok || !strings.HasPrefix(miss, "Tool 'nope' not found in server 'BkCoreServices'. Available tools in this server are:\n  - servers/BkCoreServices/get_all_clients.pyi") {
		t.Fatalf("missing tool:\n%s", miss)
	}
	miss, ok = rt.ReadToolFile(ctx, "servers/Nowhere/x.pyi", nil, nil)
	if ok || !strings.HasPrefix(miss, "No server found matching 'Nowhere'. Available virtual files are:\n  - servers/BkCoreServices/get_all_clients.pyi") {
		t.Fatalf("missing server:\n%s", miss)
	}
	if bad, ok := rt.ReadToolFile(ctx, "servers/../etc/passwd", nil, nil); ok || !strings.Contains(bad, "Invalid filename") {
		t.Fatalf("traversal: ok=%v %q", ok, bad)
	}
}

func TestPythonTypes(t *testing.T) {
	cases := []struct {
		prop any
		want string
	}{
		{prop("string", ""), "str"},
		{prop("integer", ""), "int"},
		{prop("number", ""), "float"},
		{prop("boolean", ""), "bool"},
		{prop("object", ""), "dict"},
		{prop("null", ""), "None"},
		{prop("array", ""), "list[Any]"},
		{map[string]any{"type": "array", "items": prop("string", "")}, "list[str]"},
		{map[string]any{"type": "string", "enum": []any{"SP", "SB"}}, `Literal["SP", "SB"]`},
		{map[string]any{"type": "string", "enum": []string{"allow", "ask", "deny"}}, `Literal["allow", "ask", "deny"]`},
		{map[string]any{"enum": []any{1.0, 2.0}}, "Literal[1, 2]"},
		{map[string]any{"const": "fixed"}, `Literal["fixed"]`},
		{map[string]any{"type": []any{"string", "null"}}, "Any"},
		{"not a schema", "Any"},
	}
	for _, c := range cases {
		if got := pythonType(c.prop); got != c.want {
			t.Errorf("pythonType(%v) = %q, want %q", c.prop, got, c.want)
		}
	}
	if got := shortDescription("Short one. Then more words that get dropped."); got != "Short one." {
		t.Errorf("first sentence: %q", got)
	}
	long := strings.Repeat("x", 100)
	if got := shortDescription(long); got != strings.Repeat("x", 77)+"..." {
		t.Errorf("truncation: %q", got)
	}
}

func TestGetToolDocs(t *testing.T) {
	rt := inProcess(newFake(), Limits{})
	ctx := context.Background()
	doc, ok := rt.GetToolDocs(ctx, "bkcoreservices", "upsert_bifrost_virtual_key_actor")
	if !ok {
		t.Fatalf("docs: %s", doc)
	}
	for _, want := range []string{
		"# Documentation for BkCoreServices.upsert_bifrost_virtual_key_actor tool",
		"def upsert_bifrost_virtual_key_actor(email: str, userId: str, virtualKeyHash: str, isActive: bool = None, label: str = None) -> dict:",
		"    Create or update a Bifrost Virtual Key actor mapping. Idempotent on (userId, virtualKeyHash).",
		"        email (str): The actor's email (required)",
		"        isActive (bool): Whether the mapping is live (optional)",
		"        label (str): label parameter (optional)",
		`        result = BkCoreServices.upsert_bifrost_virtual_key_actor(email="...")`,
		"    ...",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("docs missing %q:\n%s", want, doc)
		}
	}
	if miss, ok := rt.GetToolDocs(ctx, "BkCoreServices", "nope"); ok || !strings.HasPrefix(miss, "Tool 'nope' not found in server 'BkCoreServices'. Available tools are:\n  - get_all_clients") {
		t.Fatalf("missing tool: %s", miss)
	}
	if miss, ok := rt.GetToolDocs(ctx, "Nope", "x"); ok || !strings.HasPrefix(miss, "Server 'Nope' not found. Available servers are:\n  - BkCoreServices\n  - memory") {
		t.Fatalf("missing server: %s", miss)
	}
}

const multiCallScript = `
# pick the biggest client and read its record
clients = BkCoreServices.get_all_clients()
print("clients:", len(clients))
best = sorted(clients, key=lambda c: c["n"])[-1]
detail = BkCoreServices.get_client(clientId=best["id"])
note = memory.get(key="x")
result = {"id": detail["data"]["id"], "seller": detail["data"]["amz_seller_id"], "n": best["n"], "note": note, "ids": [c["id"] for c in clients]}
`

var multiCallWant = strings.Join([]string{
	"Print output:",
	`[TOOL] BkCoreServices.get_all_clients raw response: "[{\"id\":\"vai-us\",\"n\":3},{\"id\":\"ols-us\",\"n\":12345678901234567}]"`,
	"clients: 2",
	`[TOOL] BkCoreServices.get_client raw response: "{\"data\":{\"amz_seller_id\":\"A1B2\",\"id\":\"ols-us\"}}"`,
	`[TOOL] memory.get raw response: "plain text, not json"`,
	"",
	"Execution completed successfully.",
	"Return value: {",
	`  "id": "ols-us",`,
	`  "ids": [`,
	`    "vai-us",`,
	`    "ols-us"`,
	`  ],`,
	`  "n": 12345678901234567,`,
	`  "note": "plain text, not json",`,
	`  "seller": "A1B2"`,
	"}",
	"",
	"Environment:",
	"  Available server keys: BkCoreServices, memory",
	"Note: This is a Starlark (Python subset) environment. Use MCP tools for external interactions.",
}, "\n")

func TestExecuteMultiCallScript(t *testing.T) {
	f := newFake()
	bothWays(t, f, Limits{}, func(t *testing.T, rt *Runtime) {
		f.reset()
		text, failed := rt.ExecuteToolCode(context.Background(), multiCallScript, "outer reason from the client, long enough")
		if failed {
			t.Fatalf("script failed:\n%s", text)
		}
		if text != multiCallWant {
			t.Fatalf("output:\n%s\nwant:\n%s", text, multiCallWant)
		}
		calls := f.seen()
		if len(calls) != 3 {
			t.Fatalf("calls = %+v", calls)
		}
		for i, want := range []string{"BkCoreServices.get-all-clients", "BkCoreServices.get_client", "memory.get"} {
			if calls[i].target != want || calls[i].via != Via {
				t.Fatalf("call %d = %+v, want %s via %s", i, calls[i], want, Via)
			}
			if calls[i].args["_reason"] != "outer reason from the client, long enough" {
				t.Fatalf("call %d reason = %v", i, calls[i].args["_reason"])
			}
		}
		if calls[1].args["clientId"] != "ols-us" {
			t.Fatalf("get_client args = %v", calls[1].args)
		}
	})
}

func TestExecuteReturnValueOnlyAndNoData(t *testing.T) {
	rt := inProcess(newFake(), Limits{})
	ctx := context.Background()
	text, failed := rt.ExecuteToolCode(ctx, `result = [1, 2.5, None, True, "s", {"k": (1, 2)}]`, "")
	if failed || text != "Execution completed successfully.\nReturn value: [\n  1,\n  2.5,\n  null,\n  true,\n  \"s\",\n  {\n    \"k\": [\n      1,\n      2\n    ]\n  }\n]\n\nEnvironment:\n  Available server keys: BkCoreServices, memory\nNote: This is a Starlark (Python subset) environment. Use MCP tools for external interactions." {
		t.Fatalf("return only (failed=%v):\n%s", failed, text)
	}
	text, failed = rt.ExecuteToolCode(ctx, `x = 1`, "")
	if failed || !strings.HasPrefix(text, "Execution completed but produced no data:\n\nThe code executed without errors but returned no output (no print output and no result variable).\n\nHints:\nAdd print()") ||
		!strings.HasSuffix(text, "Environment:\n  Available server keys: BkCoreServices, memory") {
		t.Fatalf("no data (failed=%v):\n%s", failed, text)
	}
	text, failed = rt.ExecuteToolCode(ctx, `result = "<b>&</b>"`, "")
	if failed || !strings.Contains(text, `Return value: "<b>&</b>"`) {
		t.Fatalf("HTML must not be escaped:\n%s", text)
	}
	if text, failed := rt.ExecuteToolCode(ctx, "   ", ""); !failed || text != "code parameter is required and must be a non-empty string" {
		t.Fatalf("empty code: failed=%v %q", failed, text)
	}
}

func TestExecuteSyntaxError(t *testing.T) {
	rt := inProcess(newFake(), Limits{})
	text, failed := rt.ExecuteToolCode(context.Background(), "def broken(:\n  pass", "")
	if !failed || !strings.HasPrefix(text, "Execution syntax error:\n\ncode.star:1:13: got ':', want ')'") {
		t.Fatalf("syntax error:\n%s", text)
	}
	if !strings.Contains(text, "Hints:\nPython syntax error detected.") || !strings.HasSuffix(text, "Environment:\n  Available server keys: BkCoreServices, memory") {
		t.Fatalf("syntax error shape:\n%s", text)
	}
	// try/except is the classic Python habit Starlark refuses.
	text, _ = rt.ExecuteToolCode(context.Background(), "try:\n  x = 1\nexcept:\n  pass", "")
	if !strings.Contains(text, "Starlark does NOT support try/except/finally/raise") {
		t.Fatalf("try/except hint missing:\n%s", text)
	}
}

func TestExecuteRuntimeError(t *testing.T) {
	rt := inProcess(newFake(), Limits{})
	code := `
print("before")
d = BkCoreServices.get_client(clientId="vai-us")
result = d["missing"]
`
	text, failed := rt.ExecuteToolCode(context.Background(), code, "")
	if !failed || !strings.HasPrefix(text, "Execution runtime error:\n\nTraceback (most recent call last):\n  code.star:3:") {
		t.Fatalf("runtime error:\n%s", text)
	}
	for _, want := range []string{
		`Error: key "missing" not in dict`,
		"Hints:\nDictionary key not found.",
		"Print Output:\nbefore\n[TOOL] BkCoreServices.get_client raw response:",
		"\n\nEnvironment:\n  Available server keys: BkCoreServices, memory",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("runtime error missing %q:\n%s", want, text)
		}
	}
	text, _ = rt.ExecuteToolCode(context.Background(), `result = Nowhere.get()`, "")
	if !strings.Contains(text, "undefined: Nowhere") || !strings.Contains(text, "Variable 'Nowhere' is not defined.") {
		t.Fatalf("undefined server:\n%s", text)
	}
	text, _ = rt.ExecuteToolCode(context.Background(), `result = BkCoreServices.nope()`, "")
	if !strings.Contains(text, "has no .nope") || !strings.Contains(text, "You're trying to access an attribute that doesn't exist.") {
		t.Fatalf("unknown tool:\n%s", text)
	}
	text, _ = rt.ExecuteToolCode(context.Background(), `result = BkCoreServices.get_client("vai-us")`, "")
	if !strings.Contains(text, "get_client() takes keyword arguments: get_client(param=value)") {
		t.Fatalf("positional args:\n%s", text)
	}
	text, failed = rt.ExecuteToolCode(context.Background(), `load("x.star", "y")`, "")
	if !failed || !strings.Contains(text, "Execution runtime error") {
		t.Fatalf("load must not work:\n%s", text)
	}
}

func TestExecuteToolErrorAborts(t *testing.T) {
	f := newFake()
	f.handlers["BkCoreServices.get_client"] = func(map[string]any) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultError("upstream said no: client not found"), nil
	}
	code := `
a = BkCoreServices.get_all_clients()
b = BkCoreServices.get_client(clientId="zzz")
print("never printed")
result = b
`
	bothWays(t, f, Limits{}, func(t *testing.T, rt *Runtime) {
		f.reset()
		text, failed := rt.ExecuteToolCode(context.Background(), code, "")
		if !failed || !strings.HasPrefix(text, "Execution runtime error:\n\n") {
			t.Fatalf("tool error:\n%s", text)
		}
		if !strings.Contains(text, "Error in get_client: tool call failed for BkCoreServices.get_client: upstream said no: client not found") {
			t.Fatalf("tool error message:\n%s", text)
		}
		if !strings.Contains(text, "[TOOL] BkCoreServices.get_client error result: upstream said no: client not found") || strings.Contains(text, "never printed") {
			t.Fatalf("tool error log:\n%s", text)
		}
		if len(f.seen()) != 2 {
			t.Fatalf("script continued after the failure: %+v", f.seen())
		}
	})

	// A Go error from the route is a tool error too.
	f.handlers["BkCoreServices.get_client"] = func(map[string]any) (*mcp.CallToolResult, error) {
		return nil, fmt.Errorf("transport exploded")
	}
	rt := inProcess(f, Limits{})
	text, failed := rt.ExecuteToolCode(context.Background(), `result = BkCoreServices.get_client(clientId="x")`, "")
	if !failed || !strings.Contains(text, "tool call failed for BkCoreServices.get_client: transport exploded") ||
		!strings.Contains(text, "[TOOL] BkCoreServices.get_client error: transport exploded") {
		t.Fatalf("route error:\n%s", text)
	}
}

func TestExecuteHeldResultAborts(t *testing.T) {
	for _, marker := range heldMarkers {
		f := newFake()
		f.handlers["BkCoreServices.upsert_bifrost_virtual_key_actor"] = func(map[string]any) (*mcp.CallToolResult, error) {
			res := mcp.NewToolResultText("Tool execution requires human approval. Your call has been queued.\napproval_id: ap_123")
			res.Meta = &mcp.Meta{AdditionalFields: map[string]any{marker: true}}
			return res, nil
		}
		rt := inProcess(f, Limits{})
		code := `
r = BkCoreServices.upsert_bifrost_virtual_key_actor(email="a@b", userId="u", virtualKeyHash="h")
print("continued with", r)
result = r
`
		text, failed := rt.ExecuteToolCode(context.Background(), code, "")
		if !failed || strings.Contains(text, "continued with") {
			t.Fatalf("%s: script continued past a held call:\n%s", marker, text)
		}
		if !strings.Contains(text, "tool call failed for BkCoreServices.upsert_bifrost_virtual_key_actor: Tool execution requires human approval. Your call has been queued.\napproval_id: ap_123") {
			t.Fatalf("%s: held text missing:\n%s", marker, text)
		}
	}
}

func TestExecuteLimits(t *testing.T) {
	f := newFake()
	ctx := context.Background()

	// Nested call budget.
	rt := inProcess(f, Limits{MaxCalls: 3})
	text, failed := rt.ExecuteToolCode(ctx, "for i in range(10):\n  BkCoreServices.get_all_clients()\nresult = 1", "")
	if !failed || !strings.Contains(text, "this script already made 3 tool calls, the limit for one executeToolCode run") {
		t.Fatalf("call limit:\n%s", text)
	}
	if n := len(f.seen()); n != 3 {
		t.Fatalf("RouteCall reached %d times, want 3", n)
	}

	// Output cap: the log stops growing and the response is bounded.
	rt = inProcess(f, Limits{MaxOutputBytes: 2000})
	text, failed = rt.ExecuteToolCode(ctx, "for i in range(500):\n  print(\"x\" * 100)\nresult = \"y\" * 3000", "")
	if failed {
		t.Fatalf("output cap failed the script:\n%s", text)
	}
	if !strings.Contains(text, "[print output truncated: 2000 byte limit reached]") || !strings.HasSuffix(text, "[output truncated at 2000 bytes]") || len(text) > 2100 {
		t.Fatalf("output cap (len %d):\n%s", len(text), text)
	}

	// Step limit stops a spin long before the wall clock.
	rt = inProcess(f, Limits{MaxSteps: 10_000})
	text, failed = rt.ExecuteToolCode(ctx, "i = 0\nwhile True:\n  i += 1\nresult = i", "")
	if !failed || !strings.Contains(text, "too many steps") {
		t.Fatalf("step limit:\n%s", text)
	}

	// Wall clock stops the interpreter as well as the next nested call.
	slow := newFake()
	slow.handlers["BkCoreServices.get-all-clients"] = func(map[string]any) (*mcp.CallToolResult, error) {
		time.Sleep(50 * time.Millisecond)
		return jsonResult([]any{})
	}
	rt = inProcess(slow, Limits{ScriptTimeout: 120 * time.Millisecond})
	start := time.Now()
	text, failed = rt.ExecuteToolCode(ctx, "for i in range(100):\n  BkCoreServices.get_all_clients()\nresult = 1", "")
	if !failed || !strings.Contains(text, "script stopped: context deadline exceeded (limit 120ms)") {
		t.Fatalf("timeout:\n%s", text)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("timeout took %s", time.Since(start))
	}
	// The caller's own deadline applies when shorter.
	short, cancel := context.WithTimeout(ctx, 60*time.Millisecond)
	defer cancel()
	rt = inProcess(slow, Limits{})
	text, failed = rt.ExecuteToolCode(short, "for i in range(100):\n  BkCoreServices.get_all_clients()\nresult = 1", "")
	if !failed || !strings.Contains(text, "script stopped: context deadline exceeded (the caller's deadline)") {
		t.Fatalf("caller deadline:\n%s", text)
	}
}

func TestCyclicAndDeepValuesAreErrorsNotCrashes(t *testing.T) {
	f := newFake()
	bothWays(t, f, Limits{}, func(t *testing.T, rt *Runtime) {
		ctx := context.Background()
		cases := []struct{ name, code, want string }{
			{"self list as result", "l = []\nl.append(l)\nresult = l", "result cannot be returned: value contains a cycle"},
			{"self dict as argument", "d = {}\nd[\"x\"] = d\nresult = BkCoreServices.get_client(clientId=d)", "argument clientId: value contains a cycle"},
			{"mutual lists", "a = []\nb = [a]\na.append(b)\nresult = {\"a\": a}", "value contains a cycle"},
			{"too deep", "v = 1\nfor i in range(80):\n  v = [v]\nresult = v", "nests deeper than 64 levels"},
			{"too deep argument", "v = {}\nfor i in range(80):\n  v = {\"k\": v}\nresult = BkCoreServices.get_client(clientId=v)", "nests deeper than 64 levels"},
		}
		for _, c := range cases {
			text, failed := rt.ExecuteToolCode(ctx, c.code, "")
			if !failed || !strings.Contains(text, c.want) {
				t.Errorf("%s: failed=%v\n%s", c.name, failed, text)
			}
		}
		// Sharing without a cycle is fine, and printing a cycle is Starlark's
		// own business.
		text, failed := rt.ExecuteToolCode(ctx, "a = [1]\nl = []\nl.append(l)\nprint(l)\nresult = [a, a, {\"x\": a}]", "")
		if failed || !strings.Contains(text, "[[...]]") || !strings.Contains(text, "Return value: [") {
			t.Fatalf("shared values (failed=%v):\n%s", failed, text)
		}
	})
}

func TestUnsendableValuesAreErrors(t *testing.T) {
	rt := inProcess(newFake(), Limits{})
	ctx := context.Background()
	// Starlark integers are unbounded (no ** operator, so literals).
	cases := []struct{ code, want string }{
		{"result = 1180591620717411303424", "integer 1180591620717411303424 is too large to send as JSON (64-bit limit)"},
		{"result = {\"n\": -18446744073709551616}", "too large to send as JSON"},
		{"result = BkCoreServices.get_client(clientId=1180591620717411303424)", "argument clientId: integer 1180591620717411303424 is too large"},
		{"result = b\"abc\"", "bytes values cannot be sent as JSON"},
		{"result = BkCoreServices.get_client(clientId=b\"x\")", "argument clientId: bytes values cannot be sent as JSON"},
	}
	for _, c := range cases {
		text, failed := rt.ExecuteToolCode(ctx, c.code, "")
		if !failed || !strings.Contains(text, c.want) {
			t.Errorf("%s: failed=%v\n%s", c.code, failed, text)
		}
	}
	// The 64-bit edges still travel.
	text, failed := rt.ExecuteToolCode(ctx, "result = [9223372036854775807, -9223372036854775808, 18446744073709551615]", "")
	if failed || !strings.Contains(text, "9223372036854775807") || !strings.Contains(text, "-9223372036854775808") || !strings.Contains(text, "18446744073709551615") {
		t.Fatalf("64-bit edges:\n%s", text)
	}
}

func TestDeriveReason(t *testing.T) {
	cases := []struct{ code, want string }{
		{"# In mcp__bifrost__executeToolCode\nresult = X.y()", "code mode: In mcp__bifrost__executeToolCode"},
		{"x = 1\n\n   ## pick the biggest client\nresult = x", "code mode: pick the biggest client"},
		{"result = BkCoreServices.get_client(\n    clientId=\"vai-us\",\n)", `code mode: result = BkCoreServices.get_client( clientId="vai-us", )`},
		{"x = 1", "code mode: x = 1 (executeToolCode script)"},
		{"#\n#   \nresult = 2", "code mode: result = 2"},
		{"#\n#   \nx=2", "code mode: x=2 (executeToolCode script)"},
	}
	for _, c := range cases {
		if got := deriveReason(c.code); got != c.want {
			t.Errorf("deriveReason(%q) = %q, want %q", c.code, got, c.want)
		}
		if got := deriveReason(c.code); len(got) < 20 || len(got) > 2000 {
			t.Errorf("deriveReason(%q) length %d out of range", c.code, len(got))
		}
	}
	long := strings.Repeat("a", 300)
	got := deriveReason(long)
	if len(got) != len("code mode: ")+120 || !strings.HasSuffix(got, "...") {
		t.Errorf("long script reason = %q (len %d)", got, len(got))
	}

	// The derived reason rides on nested calls; a per-call _reason wins.
	f := newFake()
	rt := inProcess(f, Limits{})
	code := "# resolve vai-us before the sync\na = BkCoreServices.get_client(clientId=\"vai-us\")\nb = BkCoreServices.get_client(clientId=\"ols-us\", _reason=\"the script's own reason for this one\")\nresult = 1"
	if text, failed := rt.ExecuteToolCode(context.Background(), code, ""); failed {
		t.Fatalf("script failed:\n%s", text)
	}
	calls := f.seen()
	if calls[0].args["_reason"] != "code mode: resolve vai-us before the sync" {
		t.Fatalf("derived reason = %v", calls[0].args["_reason"])
	}
	if calls[1].args["_reason"] != "the script's own reason for this one" {
		t.Fatalf("per-call reason = %v", calls[1].args["_reason"])
	}
	// A custom reason field name is honoured.
	f.tools[2].ReasonField = "__toolyard_reason"
	if text, failed := rt.ExecuteToolCode(context.Background(), `result = BkCoreServices.get_client(clientId="x")`, "outer reason, long enough for the gate"); failed {
		t.Fatalf("script failed:\n%s", text)
	}
	last := f.seen()[len(f.seen())-1]
	if last.args["__toolyard_reason"] != "outer reason, long enough for the gate" || last.args["_reason"] != nil {
		t.Fatalf("fallback field args = %v", last.args)
	}
}

func TestNonStringReasonIsRejected(t *testing.T) {
	f := newFake()
	bothWays(t, f, Limits{}, func(t *testing.T, rt *Runtime) {
		f.reset()
		for _, code := range []string{
			`result = BkCoreServices.get_client(clientId="x", _reason=5)`,
			`result = BkCoreServices.get_client(clientId="x", _reason=["a"])`,
			`result = BkCoreServices.get_client(clientId="x", _reason=None)`,
		} {
			text, failed := rt.ExecuteToolCode(context.Background(), code, "")
			if code == `result = BkCoreServices.get_client(clientId="x", _reason=None)` {
				// None reads as absent: the derived reason is used.
				if failed {
					t.Fatalf("None reason:\n%s", text)
				}
				continue
			}
			if !failed || !strings.Contains(text, "tool call failed for BkCoreServices.get_client: _reason must be a string") ||
				!strings.Contains(text, "[TOOL] BkCoreServices.get_client error: _reason must be a string") {
				t.Fatalf("%s: failed=%v\n%s", code, failed, text)
			}
		}
		if n := len(f.seen()); n != 1 {
			t.Fatalf("RouteCall reached %d times, want 1 (the None case)", n)
		}
	})
}

func TestConversions(t *testing.T) {
	cases := []struct{ text, want string }{
		{`{"a": 1, "b": [true, null, 2.5], "c": {"d": "e"}}`, `{"a": 1, "b": [True, None, 2.5], "c": {"d": "e"}}`},
		{`12345678901234567890`, `12345678901234567890`},
		{`"just a string"`, `"just a string"`},
		{`not json at all`, `"not json at all"`},
		{`{"a": 1} trailing`, `"{\"a\": 1} trailing"`},
		{``, `""`},
	}
	for _, c := range cases {
		if got := decodeResult(c.text).String(); got != c.want {
			t.Errorf("decodeResult(%q) = %s, want %s", c.text, got, c.want)
		}
	}
	// Round trip: Starlark -> Go -> JSON keeps ints exact and sets as lists.
	rt := inProcess(newFake(), Limits{})
	text, failed := rt.ExecuteToolCode(context.Background(), `result = {"big": 12345678901234567890, "set": sorted(set([3, 1])), "f": 1.5, "t": (1, "a")}`, "")
	if failed || !strings.Contains(text, `"big": 12345678901234567890`) || !strings.Contains(text, "\"set\": [\n    1,\n    3\n  ]") {
		t.Fatalf("round trip:\n%s", text)
	}
}
