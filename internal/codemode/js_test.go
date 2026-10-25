package codemode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

// jsFake is a gateway for the JavaScript tests: a read tool (JSON text),
// a tool that answers with structured content, a restricted write, and
// toolyard's tools.search. read tracks how many calls are in flight at
// once.
type jsFake struct {
	*fakeCaller
	inFlight, maxInFlight atomic.Int32
}

func newJSFake() *jsFake {
	f := &jsFake{fakeCaller: &fakeCaller{handlers: map[string]func(map[string]any) (*mcp.CallToolResult, error){}}}
	f.tools = []Tool{
		{Server: "demo", Name: "read", Target: "demo.read", Description: "Read a value.", ReasonField: "_reason",
			Properties: map[string]any{"q": prop("string", "")}, Required: []string{"q"}},
		{Server: "demo", Name: "facts", Target: "demo.facts", Description: "Facts, as structured content.", ReasonField: "_reason",
			Properties: map[string]any{}},
		{Server: "demo", Name: "write", Target: "demo.write", Description: "Write a value.", ReasonField: "_reason",
			Properties: map[string]any{"x": prop("integer", "")}, Required: []string{"x"}},
		{Server: "demo", Name: "big", Target: "demo.big", Description: "A large answer.", ReasonField: "_reason",
			Properties: map[string]any{}},
		{Server: "toolyard", Name: "tools_search", Target: "tools.search", Description: "Search the catalog.", ReasonField: "_reason",
			Properties: map[string]any{"query": prop("string", ""), "limit": prop("integer", "")}},
	}
	f.handlers["demo.read"] = func(args map[string]any) (*mcp.CallToolResult, error) {
		n := f.inFlight.Add(1)
		defer f.inFlight.Add(-1)
		for {
			m := f.maxInFlight.Load()
			if n <= m || f.maxInFlight.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(60 * time.Millisecond)
		return jsonResult(map[string]any{"q": args["q"], "n": 1})
	}
	f.handlers["demo.facts"] = func(map[string]any) (*mcp.CallToolResult, error) {
		res := mcp.NewToolResultText("facts as text")
		res.StructuredContent = map[string]any{"facts": []any{"a", "b"}}
		return res, nil
	}
	f.handlers["demo.write"] = func(map[string]any) (*mcp.CallToolResult, error) {
		res := mcp.NewToolResultError("demo.write is restricted. Nothing ran and nothing was sent to your owner.")
		res.StructuredContent = map[string]any{"status": "permission_required", "executed": false,
			"draft": map[string]any{"title": "Write x", "tools": []any{map[string]any{"call_id": "call_1", "tool": "demo.write"}}}}
		res.Meta = &mcp.Meta{AdditionalFields: map[string]any{"toolyard.permission_required": true}}
		return res, nil
	}
	f.handlers["demo.big"] = func(map[string]any) (*mcp.CallToolResult, error) {
		return jsonResult(map[string]any{"blob": strings.Repeat("z", 200_000)})
	}
	f.handlers["tools.search"] = func(args map[string]any) (*mcp.CallToolResult, error) {
		body := map[string]any{"total": 1, "mode": "keyword", "results": []any{map[string]any{"name": "demo.read", "code": "demo.read", "query": args["query"]}}}
		res, _ := jsonResult(body)
		res.StructuredContent = body
		return res, nil
	}
	return f
}

// jsScript is the acceptance script: two tools (one by name), a held call
// through tryCall that the script carries on past, parallel with a limit,
// structured content, a search, a print and a returned value.
const jsScript = `
const a = await tools.demo.read({ q: "one" });
const b = await tools.call("demo.read", { q: "two" });
const held = await tools.tryCall("demo.write", { x: 1 });
const all = await tools.parallel([1, 2, 3].map((i) => () => tools.demo.read({ q: "p" + i })), { limit: 2 });
const facts = await tools.demo.facts();
const found = await tools.search("read a value", { limit: 3 });
console.log("held:", held.status, held.ok);
return { a: a.q, b: b.q, held: held.ok, status: held.status, all: all.map((r) => r.q), facts: facts.facts, found: found.map((r) => r.name) };
`

const jsScriptWant = `{"a":"one","all":["p1","p2","p3"],"b":"two","facts":["a","b"],"found":["demo.read"],"held":false,"status":"permission_required"}`

// canonical re-encodes JSON with sorted keys.
func canonical(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("not JSON: %s", raw)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func jsRun(t *testing.T, rt *Runtime, code string, opts RunOptions) *RunResult {
	t.Helper()
	opts.Language = LanguageJavaScript
	start := time.Now()
	rr, err := rt.Run(context.Background(), code, opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	t.Logf("JavaScript run took %s", time.Since(start))
	return rr
}

func TestJavaScriptAcceptanceScript(t *testing.T) {
	f := newJSFake()
	bothWays(t, f, Limits{}, func(t *testing.T, rt *Runtime) {
		f.reset()
		f.maxInFlight.Store(0)
		start := time.Now()
		out := rt.Execute(context.Background(), jsScript, ExecOptions{Language: LanguageJavaScript})
		t.Logf("JavaScript script took %s", time.Since(start))
		if out.Failed {
			t.Fatalf("script failed:\n%s", out.Text)
		}
		for _, want := range []string{"Print output:\n[TOOL] demo.read raw response: ", "\nheld: permission_required false\n", `"status": "permission_required"`,
			`"facts": [`, "Note: This is a JavaScript (QuickJS) environment", "  Tool calls: 8\n    1. demo.read succeeded "} {
			if !strings.Contains(out.Text, want) {
				t.Errorf("output missing %q:\n%s", want, out.Text)
			}
		}
		// The held call's failure is echoed like any other.
		if !strings.Contains(out.Text, "[TOOL] demo.write error result: demo.write is restricted") {
			t.Errorf("held call not echoed:\n%s", out.Text)
		}
		// parallel kept two calls in flight, never three.
		if got := f.maxInFlight.Load(); got != 2 {
			t.Errorf("parallel with limit 2 had %d calls in flight at most", got)
		}
		if len(out.Calls) != 8 || out.Calls[2].Tool != "demo.write" || out.Calls[2].Status != StatusPermissionRequired {
			t.Errorf("ledger = %+v", out.Calls)
		}
		var seen []string
		for _, c := range f.seen() {
			seen = append(seen, c.target)
			if c.via != Via || c.args["_reason"] == "" {
				t.Errorf("nested call %+v", c)
			}
		}
		if len(seen) != 8 {
			t.Errorf("gateway saw %v", seen)
		}

		// The same script as a structured run: succeeded, with every call
		// recorded, the held one as permission_required.
		rr := jsRun(t, rt, jsScript, RunOptions{Reason: "the JavaScript acceptance script, run structured"})
		if rr.Status != StatusSucceeded || canonical(t, rr.Result) != jsScriptWant {
			t.Fatalf("structured run = %+v (result %s)", rr, rr.Result)
		}
		if got := callStatuses(rr); len(got) != 8 || got[2] != StatusPermissionRequired {
			t.Fatalf("calls = %v", got)
		}
		if !reflect.DeepEqual(rr.Logs, []string{"held: permission_required false"}) {
			t.Fatalf("logs = %q", rr.Logs)
		}
	})
}

func TestJavaScriptUncaughtHeldCallHoldsTheRun(t *testing.T) {
	f := newJSFake()
	bothWays(t, f, Limits{}, func(t *testing.T, rt *Runtime) {
		rr := jsRun(t, rt, `await tools.demo.read({ q: "x" });
await tools.demo.write({ x: 1 });
return 1;`, RunOptions{})
		if rr.Status != StatusPermissionRequired || rr.Held == nil || rr.Held.Tool != "demo.write" || len(rr.Held.Draft) == 0 || rr.Result != nil {
			t.Fatalf("held run = %+v (held %+v)", rr, rr.Held)
		}
		if got := callStatuses(rr); !reflect.DeepEqual(got, []string{StatusSucceeded, StatusPermissionRequired}) {
			t.Fatalf("calls = %v", got)
		}
		// Caught, the same failure lets the script go on.
		rr = jsRun(t, rt, `try {
  await tools.demo.write({ x: 1 });
} catch (e) {
  console.log(e.name, e.status, e instanceof Error);
}
return await tools.demo.read({ q: "after" });`, RunOptions{})
		if rr.Status != StatusSucceeded || canonical(t, rr.Result) != `{"n":1,"q":"after"}` {
			t.Fatalf("caught run = %+v (result %s)", rr, rr.Result)
		}
		if !reflect.DeepEqual(rr.Logs, []string{"ToolCallError permission_required true"}) {
			t.Fatalf("logs = %q", rr.Logs)
		}
		// executeToolCode reports the uncaught one as a failed script.
		out := rt.Execute(context.Background(), `await tools.demo.write({ x: 1 });`, ExecOptions{Language: LanguageJavaScript})
		if !out.Failed || !strings.Contains(out.Text, "Execution runtime error:\n\ntool call failed for demo.write: demo.write is restricted") ||
			!strings.Contains(out.Text, "tools.tryCall") {
			t.Fatalf("uncaught held call:\n%s", out.Text)
		}
	})
}

func TestJavaScriptErrors(t *testing.T) {
	f := newJSFake()
	rt := inProcess(f, Limits{})
	cases := []struct {
		code string
		want []string
	}{
		{"const = 1;", []string{"Execution syntax error:", "SyntaxError"}},
		{"const x = null;\nreturn x.y;", []string{"Execution runtime error:", "TypeError", "<evalScript>:2"}},
		{"return undefinedThing + 1;", []string{"Execution runtime error:", "ReferenceError", "is not defined", "A name is not defined"}},
		{"throw new Error('boom');", []string{"Error: boom"}},
		{"throw { code: 7 };", []string{`Uncaught {"code":7}`}},
		{"await tools.nope.x({});", []string{"Execution runtime error:", "TypeError"}},
		{`await tools.call("demo.missing", {});`, []string{`no tool "demo.missing"`}},
		{"return tools.demo.read('not an object');", []string{"tool arguments must be an object"}},
		{"await new Promise(() => {});", []string{"never settles"}},
		{"Promise.reject(new Error('lost'));\nawait tools.demo.read({ q: 'x' });", []string{"Possibly unhandled promise rejection", "lost"}},
	}
	for _, c := range cases {
		out := rt.Execute(context.Background(), c.code, ExecOptions{Language: LanguageJavaScript})
		if !out.Failed {
			t.Errorf("%q did not fail:\n%s", c.code, out.Text)
			continue
		}
		for _, w := range c.want {
			if !strings.Contains(out.Text, w) {
				t.Errorf("%q: output missing %q:\n%s", c.code, w, out.Text)
			}
		}
	}
	// No output and no value.
	out := rt.Execute(context.Background(), "const x = 1;", ExecOptions{Language: LanguageJavaScript})
	if out.Failed || !strings.Contains(out.Text, "no console.log output and no returned value") {
		t.Fatalf("no data:\n%s", out.Text)
	}
	// Values that are not JSON.
	if out := rt.Execute(context.Background(), "return 10n;", ExecOptions{Language: LanguageJavaScript}); !out.Failed || !strings.Contains(out.Text, "cannot be encoded as JSON") {
		t.Fatalf("a BigInt result:\n%s", out.Text)
	}
	if out := rt.Execute(context.Background(), "const o = {}; o.o = o; return o;", ExecOptions{Language: LanguageJavaScript}); !out.Failed || !strings.Contains(out.Text, "cannot be encoded as JSON") {
		t.Fatalf("a cyclic result:\n%s", out.Text)
	}
	// An unknown language is refused before anything runs.
	if out := rt.Execute(context.Background(), "x", ExecOptions{Language: "ruby"}); !out.Failed || !strings.Contains(out.Text, `unknown language "ruby"`) {
		t.Fatalf("ruby:\n%s", out.Text)
	}
	if rr, err := rt.Run(context.Background(), "x", RunOptions{Language: "ruby"}); err != nil || rr.Status != StatusFailed || rr.ErrorKind != ErrorScriptError {
		t.Fatalf("Run ruby = %+v, %v", rr, err)
	}
}

func TestJavaScriptValuesAndConsole(t *testing.T) {
	f := newJSFake()
	rt := inProcess(f, Limits{})
	rr := jsRun(t, rt, `console.log("s", 1, true, null, undefined, [1, "a"], { k: 2 });
console.error(new Error("shown"));
const big = await tools.demo.big();
console.log(typeof big.blob, big.blob.length);
print("print works");
return { n: 12345678901234, f: 1.5, s: "é", nil: null, list: [1, [2]], when: typeof Date.now() };`, RunOptions{})
	if rr.Status != StatusSucceeded {
		t.Fatalf("run = %+v", rr)
	}
	if got := canonical(t, rr.Result); got != `{"f":1.5,"list":[1,[2]],"n":12345678901234,"nil":null,"s":"é","when":"number"}` {
		t.Fatalf("result = %s", got)
	}
	if len(rr.Logs) != 4 || rr.Logs[0] != `s 1 true null undefined [1,"a"] {"k":2}` || !strings.HasPrefix(rr.Logs[1], "Error: shown") ||
		rr.Logs[2] != "string 200000" || rr.Logs[3] != "print works" {
		t.Fatalf("logs = %q", rr.Logs)
	}
	// A value that is not an object or array still comes back.
	if rr := jsRun(t, rt, `return "text";`, RunOptions{}); string(rr.Result) != `"text"` {
		t.Fatalf("string result = %s", rr.Result)
	}
	// undefined and null are no value.
	if rr := jsRun(t, rt, `return null;`, RunOptions{}); rr.Status != StatusSucceeded || rr.Result != nil {
		t.Fatalf("null result = %+v", rr)
	}
}

func TestJavaScriptTimeout(t *testing.T) {
	f := newJSFake()
	bothWays(t, f, Limits{ScriptTimeout: 4 * time.Second}, func(t *testing.T, rt *Runtime) {
		start := time.Now()
		out := rt.Execute(context.Background(), "let i = 0;\nwhile (true) { i++; }", ExecOptions{Language: LanguageJavaScript})
		if !out.Failed || !strings.Contains(out.Text, "script stopped: context deadline exceeded (limit 4s)") {
			t.Fatalf("spin:\n%s", out.Text)
		}
		if took := time.Since(start); took > 15*time.Second {
			t.Fatalf("the spin took %s to stop", took)
		}
	})
}

func TestJavaScriptMemoryLimitIsCleanError(t *testing.T) {
	rt := workerRuntime(newJSFake(), Limits{ScriptTimeout: 60 * time.Second}, nil)
	out := rt.Execute(context.Background(), `const parts = [];
console.log("about to allocate");
for (;;) parts.push("x".repeat(1 << 20) + parts.length);`, ExecOptions{Language: LanguageJavaScript})
	if !out.Failed || !strings.Contains(out.Text, "out of memory") || !strings.Contains(out.Text, "about to allocate") {
		t.Fatalf("allocation past the heap limit:\n%.2000s", out.Text)
	}
	expectClean(t, rt, 0)
	// The next script runs.
	if out := rt.Execute(context.Background(), "return 1;", ExecOptions{Language: LanguageJavaScript}); out.Failed {
		t.Fatalf("after the allocation:\n%s", out.Text)
	}
}

func TestJavaScriptDecodesLargeResults(t *testing.T) {
	f := newFake()
	var rows []any
	for i := 0; i < 128000; i++ {
		rows = append(rows, map[string]any{"id": 1234567 + i, "name": "abcdefghijklmnop"})
	}
	f.handlers["BkCoreServices.get-all-clients"] = func(map[string]any) (*mcp.CallToolResult, error) {
		return jsonResult(rows)
	}
	rt := workerRuntime(f, Limits{}, nil)
	start := time.Now()
	out := rt.Execute(context.Background(), `const rows = await tools.BkCoreServices.get_all_clients();
return { rows: rows.length, last: rows[rows.length - 1].id };`, ExecOptions{Language: LanguageJavaScript, Echo: EchoNone})
	t.Logf("a 5 MiB result took %s", time.Since(start))
	if out.Failed || !strings.Contains(out.Text, `"rows": 128000`) || !strings.Contains(out.Text, `"last": 1362566`) {
		t.Fatalf("5 MiB result:\n%.1000s", out.Text)
	}
}

func TestEchoModes(t *testing.T) {
	f := newJSFake()
	rt := inProcess(f, Limits{})
	ctx := context.Background()
	starlark := `r = demo.big()
result = len(r["blob"])`
	full := rt.Execute(ctx, starlark, ExecOptions{})
	if full.Failed || len(full.Text) < 200_000 || !strings.Contains(full.Text, "[TOOL] demo.big raw response: ") ||
		strings.Contains(full.Text, "Tool calls:") {
		t.Fatalf("echo full (len %d):\n%.400s", len(full.Text), full.Text)
	}
	none := rt.Execute(ctx, starlark, ExecOptions{Echo: EchoNone})
	if none.Failed || len(none.Text) >= 5<<10 || strings.Contains(none.Text, "[TOOL]") ||
		!strings.Contains(none.Text, "Return value: 200000") || !strings.Contains(none.Text, "  Tool calls: 1\n    1. demo.big succeeded ") {
		t.Fatalf("echo none (len %d):\n%s", len(none.Text), none.Text)
	}
	preview := rt.Execute(ctx, starlark, ExecOptions{Echo: EchoPreview})
	if preview.Failed || len(preview.Text) >= 5<<10 || !strings.Contains(preview.Text, "[TOOL] demo.big preview (200011 bytes): {") {
		t.Fatalf("echo preview (len %d):\n%s", len(preview.Text), preview.Text)
	}
	js := rt.Execute(ctx, `const r = await tools.demo.big(); return r.blob.length;`, ExecOptions{Language: LanguageJavaScript, Echo: EchoNone})
	if js.Failed || len(js.Text) >= 5<<10 || !strings.Contains(js.Text, "Return value: 200000") {
		t.Fatalf("JavaScript echo none (len %d):\n%s", len(js.Text), js.Text)
	}
	if out := rt.Execute(ctx, "result = 1", ExecOptions{Echo: "loud"}); !out.Failed || !strings.Contains(out.Text, `unknown echo "loud"`) {
		t.Fatalf("bad echo:\n%s", out.Text)
	}
}

// TestEchoShareKeepsPrintsAndResult: tool answers that would fill the
// output stop being echoed at their share, so the script's own prints
// after them and its result still show.
func TestEchoShareKeepsPrintsAndResult(t *testing.T) {
	f := newJSFake()
	rt := inProcess(f, Limits{MaxOutputBytes: 64 << 10})
	out := rt.Execute(context.Background(), `for i in range(3):
  demo.big()
print("still here")
result = "done"`, ExecOptions{})
	if out.Failed || !strings.Contains(out.Text, "[TOOL] echo stopped: tool answers used their 48 KiB share of the output") ||
		!strings.Contains(out.Text, "still here") || !strings.Contains(out.Text, `Return value: "done"`) {
		t.Fatalf("echo share (len %d):\n%.3000s", len(out.Text), out.Text)
	}
}

func TestStarlarkTryCallAndParallel(t *testing.T) {
	f := newJSFake()
	bothWays(t, f, Limits{}, func(t *testing.T, rt *Runtime) {
		f.maxInFlight.Store(0)
		code := `held = try_call(demo.write, x=1)
ok = try_call("demo.read", {"q": "z"})
many = parallel([(demo.read, {"q": "a"}), ("demo.read", {"q": "b"}), demo.facts, ["demo.read", {"q": "c"}]], limit=2)
print("held:", held["status"])
result = {"held": held["status"], "ok": ok["result"]["q"], "many": [m["ok"] for m in many], "facts": many[2]["result"], "c": many[3]["result"]["q"]}`
		out := rt.Execute(context.Background(), code, ExecOptions{})
		if out.Failed {
			t.Fatalf("script failed:\n%s", out.Text)
		}
		for _, want := range []string{`"held": "permission_required"`, `"ok": "z"`, `"facts": "facts as text"`, `"c": "c"`} {
			if !strings.Contains(out.Text, want) {
				t.Errorf("output missing %q:\n%s", want, out.Text)
			}
		}
		if got := f.maxInFlight.Load(); got != 2 {
			t.Errorf("parallel with limit 2 had %d calls in flight at most", got)
		}
		// A structured run that caught its held call succeeds.
		rr, err := rt.Run(context.Background(), code, RunOptions{})
		if err != nil || rr.Status != StatusSucceeded || rr.Held != nil {
			t.Fatalf("structured = %+v, %v", rr, err)
		}
		// try_call refuses what it cannot call; parallel needs a list.
		for code, want := range map[string]string{
			`result = try_call(42)`:                       "name the tool as Server.tool",
			`result = parallel("demo.read")`:              "parallel takes a list of calls",
			`result = parallel([("demo.read", 1, 2, 3)])`: "must be a tool, or (tool, {args})",
			`result = parallel([], limit=0)`:              "limit must be at least 1",
		} {
			out := rt.Execute(context.Background(), code, ExecOptions{})
			if !out.Failed || !strings.Contains(out.Text, want) {
				t.Errorf("%s:\n%s", code, out.Text)
			}
		}
		// An unknown tool named to try_call is data, not an abort.
		out = rt.Execute(context.Background(), `r = try_call("demo.nope", {})
result = r`, ExecOptions{})
		if out.Failed || !strings.Contains(out.Text, `"ok": false`) || !strings.Contains(out.Text, `no tool \"demo.nope\"`) {
			t.Fatalf("unknown tool via try_call:\n%s", out.Text)
		}
	})
}

// TestStarlarkHelpersYieldToServerNames: a server named like a helper keeps
// its binding.
func TestStarlarkHelpersYieldToServerNames(t *testing.T) {
	f := &fakeCaller{handlers: map[string]func(map[string]any) (*mcp.CallToolResult, error){}}
	f.tools = []Tool{{Server: "parallel", Name: "go", Target: "parallel.go", Properties: map[string]any{}, ReasonField: "_reason"}}
	f.handlers["parallel.go"] = func(map[string]any) (*mcp.CallToolResult, error) { return jsonResult("went") }
	rt := inProcess(f, Limits{})
	if out := rt.Execute(context.Background(), `result = parallel.go()`, ExecOptions{}); out.Failed || !strings.Contains(out.Text, `Return value: "went"`) {
		t.Fatalf("server named parallel:\n%s", out.Text)
	}
}

func TestEmbeddedQuickJS(t *testing.T) {
	sum := sha256.Sum256(quickJSWasm)
	if got := hex.EncodeToString(sum[:]); got != quickJSWasmSHA256 {
		t.Fatalf("quickjs/qjs-wasi.wasm sha256 %s, want %s (%s); update js.go and quickjs/README.md together with the file", got, quickJSWasmSHA256, QuickJSVersion)
	}
	if !strings.Contains(jsPrelude, "evalScript") || strings.ContainsRune(jsPrelude, 0) {
		t.Fatal("the prelude must evaluate the script and hold no NUL (it travels as an argument)")
	}
}

func TestResolveName(t *testing.T) {
	cat := buildCatalog(newJSFake().tools)
	for name, want := range map[string]string{
		"demo.read": "demo.read", "DEMO.READ": "demo.read", "toolyard.tools_search": "tools.search", "tools.search": "tools.search",
	} {
		_, b, msg := cat.resolveName(name)
		if msg != "" || b == nil || b.tool.Target != want {
			t.Errorf("resolveName(%q) = %v, %q", name, b, msg)
		}
	}
	if _, _, msg := cat.resolveName("demo"); !strings.Contains(msg, `no tool "demo"`) {
		t.Errorf("resolveName(demo) = %q", msg)
	}
	names := CodeNames(newJSFake().tools)
	if names["tools.search"] != "toolyard.tools_search" || names["demo.read"] != "demo.read" {
		t.Fatalf("CodeNames = %v", names)
	}
}

func TestParseLanguageAndEcho(t *testing.T) {
	for in, want := range map[string]string{"": LanguageStarlark, "Starlark": LanguageStarlark, "python": LanguageStarlark, "javascript": LanguageJavaScript, "JS": LanguageJavaScript} {
		if got, err := ParseLanguage(in); err != nil || got != want {
			t.Errorf("ParseLanguage(%q) = %q, %v", in, got, err)
		}
	}
	for in, want := range map[string]string{"": EchoFull, "full": EchoFull, "Preview": EchoPreview, "none": EchoNone} {
		if got, err := ParseEcho(in); err != nil || got != want {
			t.Errorf("ParseEcho(%q) = %q, %v", in, got, err)
		}
	}
	if id := NewRunID(); !strings.HasPrefix(id, "cm_") || len(id) != 35 {
		t.Fatalf("NewRunID = %q", id)
	}
}

// TestFifthScriptOfOneAgentWaitsWhileAnotherAgentRuns: with the global
// cap at 4 and the per-caller cap at 2, five scripts from one caller run
// two at a time; the other three wait their turn and then give up busy,
// while another caller's script starts at once.
func TestFifthScriptOfOneAgentWaitsWhileAnotherAgentRuns(t *testing.T) {
	f := newFake()
	release := make(chan struct{})
	started := make(chan string, 16)
	f.handlers["BkCoreServices.get_client"] = func(args map[string]any) (*mcp.CallToolResult, error) {
		started <- args["clientId"].(string)
		<-release
		return jsonResult(map[string]any{"ok": true})
	}
	rt := inProcess(f, Limits{})
	rt.SetWorker(Worker{MaxConcurrent: 4, MaxPerAgent: 2, AcquireWait: 300 * time.Millisecond})
	ctx := context.Background()
	run := func(agent, id string) error {
		_, err := rt.Run(WithAgentKey(ctx, agent), `result = BkCoreServices.get_client(clientId="`+id+`")`, RunOptions{})
		return err
	}
	errs := make(chan error, 8)
	for _, id := range []string{"a1", "a2", "a3", "a4", "a5"} {
		go func(id string) { errs <- run("agent-a", id) }(id)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("agent a's first two scripts did not start")
		}
	}
	busy := 0
	for i := 0; i < 3; i++ {
		select {
		case err := <-errs:
			if errors.Is(err, ErrBusy) && strings.Contains(err.Error(), "this caller already runs 2 scripts") {
				busy++
			} else {
				t.Errorf("a waiting script ended with %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the waiting scripts did not give up")
		}
	}
	if busy != 3 {
		t.Fatalf("%d of agent a's extra scripts were refused busy, want 3", busy)
	}
	// Agent b is not held up by agent a's queue.
	go func() { errs <- run("agent-b", "b1") }()
	select {
	case id := <-started:
		if id != "b1" {
			t.Fatalf("started %s", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("agent b's script waited behind agent a")
	}
	close(release)
	for i := 0; i < 3; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("a running script: %v", err)
		}
	}
	if n := rt.slotsInUse(); n != 0 {
		t.Fatalf("%d slots held", n)
	}
}

// TestLimiterQueueIsFair: a freed slot goes to the earliest waiter that
// may take it, skipping waiters whose caller is at its cap.
func TestLimiterQueueIsFair(t *testing.T) {
	l := newLimiter(2, 1)
	ctx := context.Background()
	relA, err := l.acquire(ctx, "a", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	relB, err := l.acquire(ctx, "b", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var order []string
	var wg sync.WaitGroup
	for _, agent := range []string{"a", "c"} {
		wg.Add(1)
		go func(agent string) {
			defer wg.Done()
			rel, err := l.acquire(ctx, agent, 3*time.Second)
			if err != nil {
				t.Errorf("%s: %v", agent, err)
				return
			}
			mu.Lock()
			order = append(order, agent)
			mu.Unlock()
			rel()
		}(agent)
		time.Sleep(50 * time.Millisecond) // a queues before c
	}
	// b's slot frees: a is still at its cap, so c (behind a) takes it.
	relB()
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	first := append([]string(nil), order...)
	mu.Unlock()
	if !reflect.DeepEqual(first, []string{"c"}) {
		t.Fatalf("after b released: %v, want [c]", first)
	}
	relA()
	relA() // releasing twice is harmless
	wg.Wait()
	if !reflect.DeepEqual(order, []string{"c", "a"}) {
		t.Fatalf("order %v", order)
	}
	if n := l.inUse(); n != 0 {
		t.Fatalf("%d in use", n)
	}
	// A cancelled wait leaves nothing behind.
	r1, _ := l.acquire(ctx, "x", time.Second)
	r2, _ := l.acquire(ctx, "y", time.Second)
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := l.acquire(cctx, "z", time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
	if _, err := l.acquire(ctx, "z", 50*time.Millisecond); !errors.Is(err, ErrBusy) || !strings.Contains(err.Error(), "2 scripts are already running") {
		t.Fatalf("full: %v", err)
	}
	r1()
	r2()
	if n := l.inUse(); n != 0 || len(l.queue) != 0 {
		t.Fatalf("%d in use, %d queued", n, len(l.queue))
	}
}
