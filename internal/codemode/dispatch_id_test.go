package codemode

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"go.starlark.net/starlark"
)

// TestTryValueDispatchID: try_call's dict names the dispatch on success
// and on failure, and leaves the key out when there was none.
func TestTryValueDispatchID(t *testing.T) {
	get := func(v starlark.Value, key string) (starlark.Value, bool) {
		t.Helper()
		got, found, err := v.(*starlark.Dict).Get(starlark.String(key))
		if err != nil {
			t.Fatal(err)
		}
		return got, found
	}
	ok := tryValue(parentMsg{Status: StatusSucceeded, Text: `"x"`, DispatchID: "dispatch_ok"})
	if v, found := get(ok, "dispatch_id"); !found || v != starlark.String("dispatch_ok") {
		t.Fatalf("success dict = %s", ok)
	}
	failed := tryValue(parentMsg{Status: StatusOutcomeUnknown, Error: "tool call failed for demo.write: Tool outcome unknown", DispatchID: "dispatch_w"})
	if v, found := get(failed, "dispatch_id"); !found || v != starlark.String("dispatch_w") {
		t.Fatalf("failure dict = %s", failed)
	}
	if v, _ := get(failed, "status"); v != starlark.String(StatusOutcomeUnknown) {
		t.Fatalf("failure status = %s", failed)
	}
	if _, found := get(tryValue(parentMsg{Status: StatusSucceeded, Text: `"x"`}), "dispatch_id"); found {
		t.Fatal("dispatch_id without a dispatch")
	}
}

// TestTryCallCarriesDispatchID: the gateway's dispatch id in a result's
// metadata reaches try_call (Starlark) and tools.tryCall (JavaScript).
func TestTryCallCarriesDispatchID(t *testing.T) {
	f := newJSFake()
	withID := func(res *mcp.CallToolResult, id string) *mcp.CallToolResult {
		res.Meta = &mcp.Meta{AdditionalFields: map[string]any{DispatchIDMarker: id}}
		return res
	}
	f.handlers["demo.read"] = func(map[string]any) (*mcp.CallToolResult, error) {
		return withID(mcp.NewToolResultText(`{"n":1}`), "dispatch_read"), nil
	}
	f.handlers["demo.write"] = func(map[string]any) (*mcp.CallToolResult, error) {
		return withID(mcp.NewToolResultError("upstream said no"), "dispatch_write"), nil
	}
	rt := inProcess(f, Limits{ScriptTimeout: 20 * time.Second})

	rr := mustRun(t, rt, "a = try_call(demo.read, q=\"x\")\nb = try_call(demo.write, x=1)\nresult = [a[\"dispatch_id\"], b[\"dispatch_id\"], b[\"ok\"]]\n", RunOptions{})
	if rr.Status != StatusSucceeded || !strings.Contains(string(rr.Result), `"dispatch_read"`) || !strings.Contains(string(rr.Result), `"dispatch_write"`) {
		t.Fatalf("starlark = %+v (result %s)", rr, rr.Result)
	}

	rr = jsRun(t, rt, `const a = await tools.tryCall("demo.read", { q: "x" });
const b = await tools.tryCall("demo.write", { x: 1 });
return [a.dispatch_id, b.dispatch_id, b.ok];`, RunOptions{})
	if rr.Status != StatusSucceeded || !strings.Contains(string(rr.Result), `"dispatch_read"`) || !strings.Contains(string(rr.Result), `"dispatch_write"`) {
		t.Fatalf("javascript = %+v (result %s)", rr, rr.Result)
	}
}

// TestAuditMissingWarningKeepsValue: a read whose audit row is missing
// keeps its value in both engines (JSON text and structured content);
// the warning reaches try_call and the run's output, with echo off.
func TestAuditMissingWarningKeepsValue(t *testing.T) {
	f := newJSFake()
	warned := func(res *mcp.CallToolResult) *mcp.CallToolResult {
		res.Meta = &mcp.Meta{AdditionalFields: map[string]any{
			AuditMissingMarker: true, WarningMarker: "Warning: W1", DispatchIDMarker: "dispatch_r"}}
		return res
	}
	f.handlers["demo.read"] = func(map[string]any) (*mcp.CallToolResult, error) {
		return warned(mcp.NewToolResultText(`{"n":1}`)), nil
	}
	f.handlers["demo.facts"] = func(map[string]any) (*mcp.CallToolResult, error) {
		res := mcp.NewToolResultText("facts as text")
		res.StructuredContent = map[string]any{"facts": []any{"a", "b"}}
		return warned(res), nil
	}
	rt := inProcess(f, Limits{ScriptTimeout: 20 * time.Second})
	hasWarning := func(logs []string, tool string) bool {
		for _, l := range logs {
			if l == "[TOOL] "+tool+" warning: Warning: W1" {
				return true
			}
		}
		return false
	}

	rr := mustRun(t, rt, "a = demo.read(q=\"x\")\nb = try_call(demo.read, q=\"y\")\nresult = [a, b[\"result\"], b[\"warning\"]]\n", RunOptions{})
	if rr.Status != StatusSucceeded || canonical(t, rr.Result) != `[{"n":1},{"n":1},"Warning: W1"]` || !hasWarning(rr.Logs, "demo.read") {
		t.Fatalf("starlark = %+v (result %s, logs %q)", rr, rr.Result, rr.Logs)
	}

	rr = jsRun(t, rt, `const a = await tools.demo.read({ q: "x" });
const f = await tools.demo.facts();
const t = await tools.tryCall("demo.facts", {});
return { a, f, tf: t.result, w: t.warning };`, RunOptions{})
	if rr.Status != StatusSucceeded || canonical(t, rr.Result) != `{"a":{"n":1},"f":{"facts":["a","b"]},"tf":{"facts":["a","b"]},"w":"Warning: W1"}` ||
		!hasWarning(rr.Logs, "demo.read") || !hasWarning(rr.Logs, "demo.facts") {
		t.Fatalf("javascript = %+v (result %s, logs %q)", rr, rr.Result, rr.Logs)
	}

	// A plain call with echo none: the warning is still in the output.
	out := rt.Execute(context.Background(), "a = demo.read(q=\"x\")\nprint(a[\"n\"])\n", ExecOptions{Echo: EchoNone})
	if out.Failed || !strings.Contains(out.Text, "demo.read warning: Warning: W1") {
		t.Fatalf("echo none output:\n%s", out.Text)
	}
}
