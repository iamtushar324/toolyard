package codemode

import (
	"context"
	"os"
	"strings"
	"testing"
)

// tsFixture: a tool with nested objects, arrays of objects, enums,
// nullable types, a map, a default, a non-identifier property and an
// output schema; a tool with nothing, on a server whose key is not an
// identifier; and a plain read.
func tsFixture() []Tool {
	str := map[string]any{"type": "string"}
	return []Tool{
		{
			Server: "Linear", Name: "save_issue", Target: "Linear.save_issue",
			Description: "Create or update an issue.\n\nPass id to update.",
			Properties: map[string]any{
				"title":    map[string]any{"type": "string", "description": "Issue title"},
				"id":       str,
				"priority": map[string]any{"type": "integer", "enum": []any{0, 1, 2}, "description": "0 is none", "default": 0},
				"state":    map[string]any{"type": "string", "enum": []any{"todo", "in progress", "done"}},
				"labels":   map[string]any{"type": "array", "items": str},
				"assignee": map[string]any{"type": "object", "description": "Who owns it", "required": []any{"email"},
					"properties": map[string]any{"email": str, "name": map[string]any{"type": []any{"string", "null"}}}},
				"links": map[string]any{"type": "array", "items": map[string]any{"type": "object", "required": []any{"url"},
					"properties": map[string]any{"url": map[string]any{"type": "string", "format": "uri"}, "title": str}}},
				"meta":    map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "number"}},
				"x-trace": map[string]any{"type": "boolean"},
			},
			Required: []string{"title", "state"},
			OutputSchema: map[string]any{"type": "object", "description": "The saved issue.", "required": []any{"id", "status"},
				"properties": map[string]any{
					"id": str, "url": str,
					"status": map[string]any{"anyOf": []any{map[string]any{"const": "created"}, map[string]any{"const": "updated"}}},
				}},
			ReasonField: "_reason",
		},
		{Server: "Linear", Name: "get_issue", Target: "Linear.get_issue", Description: "Fetch one issue. By key or id.",
			Properties: map[string]any{"id": str}, Required: []string{"id"}, ReasonField: "_reason"},
		{Server: "bk-core", Name: "get-all-clients", Target: "bk-core.get-all-clients", Description: "List every client.",
			Properties: map[string]any{}, ReasonField: "_reason"},
	}
}

func TestTypeScriptStubsMatchGoldenFiles(t *testing.T) {
	cat := buildCatalog(tsFixture())
	for _, c := range []struct{ server, tool, golden string }{
		{"Linear", "save_issue", "testdata/dts/save_issue.d.ts"},
		{"bk_core", "get_all_clients", "testdata/dts/get_all_clients.d.ts"},
	} {
		srv := cat.byIdent(c.server)
		if srv == nil {
			t.Fatalf("no server %s", c.server)
		}
		b, _ := srv.lookup(c.tool)
		if b == nil {
			t.Fatalf("no tool %s.%s", c.server, c.tool)
		}
		want, err := os.ReadFile(c.golden)
		if err != nil {
			t.Fatal(err)
		}
		if got := tsTool(srv, *b); got != string(want) {
			t.Errorf("%s differs from the generated stub; got:\n%s", c.golden, got)
		}
	}
}

func TestTypeScriptTypes(t *testing.T) {
	cases := []struct {
		schema any
		want   string
	}{
		{map[string]any{"type": "string"}, "string"},
		{map[string]any{"type": "integer"}, "number"},
		{map[string]any{"type": []any{"number", "integer", "null"}}, "number | null"},
		{map[string]any{"enum": []string{"a", "b"}}, `"a" | "b"`},
		{map[string]any{"enum": []any{true, nil, 2.5}}, "true | null | 2.5"},
		{map[string]any{"const": "x"}, `"x"`},
		{map[string]any{"$ref": "#/$defs/Thing"}, "unknown"},
		{map[string]any{}, "unknown"},
		{"not a schema", "unknown"},
		{map[string]any{"type": "array"}, "unknown[]"},
		{map[string]any{"type": "array", "items": map[string]any{"enum": []any{"a", "b"}}}, `("a" | "b")[]`},
		{map[string]any{"type": "array", "items": []any{map[string]any{"type": "string"}, map[string]any{"type": "number"}}}, "[string, number]"},
		{map[string]any{"type": "object"}, "Record<string, unknown>"},
		{map[string]any{"type": "object", "additionalProperties": false}, "Record<string, never>"},
		{map[string]any{"oneOf": []any{map[string]any{"type": "string"}, map[string]any{"type": "number"}}}, "string | number"},
		{map[string]any{"allOf": []any{map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "string"}}},
			map[string]any{"type": "object", "properties": map[string]any{"b": map[string]any{"type": "number"}}}}},
			"{\n  a?: string;\n} & {\n  b?: number;\n}"},
		{map[string]any{"properties": map[string]any{"a": map[string]any{"type": "string"}}, "additionalProperties": true},
			"{\n  a?: string;\n  [key: string]: unknown;\n}"},
	}
	for _, c := range cases {
		if got := tsType(c.schema, "", 0); got != c.want {
			t.Errorf("tsType(%v) = %q, want %q", c.schema, got, c.want)
		}
	}
	// A schema nested past the depth limit ends in unknown, never loops.
	deep := map[string]any{"type": "object"}
	cur := deep
	for i := 0; i < 20; i++ {
		next := map[string]any{"type": "object"}
		cur["properties"] = map[string]any{"x": next}
		cur = next
	}
	if got := tsType(deep, "", 0); !strings.Contains(got, "x?: unknown;") {
		t.Fatalf("deep schema:\n%s", got)
	}
	// Comment closers in descriptions cannot end the JSDoc early.
	var b strings.Builder
	writeDoc(&b, "", "a */ b")
	if b.String() != "/** a *\\/ b */\n" {
		t.Fatalf("writeDoc = %q", b.String())
	}
}

func TestTypeScriptFilesThroughTheRuntime(t *testing.T) {
	f := &fakeCaller{tools: tsFixture()}
	rt := inProcess(f, Limits{})
	ctx := context.Background()
	list := rt.ListFiles(ctx, LanguageJavaScript)
	for _, want := range []string{"// Workflow (JavaScript):", "servers/\n", "  Linear/\n    get_issue.d.ts\n    save_issue.d.ts\n", "  bk_core/\n    get_all_clients.d.ts\n", "  index.d.ts"} {
		if !strings.Contains(list, want) {
			t.Errorf("listing missing %q:\n%s", want, list)
		}
	}
	if strings.Contains(list, ".pyi") {
		t.Errorf("JavaScript listing names .pyi files:\n%s", list)
	}
	if py := rt.ListFiles(ctx, LanguageStarlark); py != rt.ListToolFiles(ctx) || !strings.Contains(py, "save_issue.pyi") {
		t.Errorf("Starlark listing:\n%s", py)
	}

	stub, ok := rt.ReadToolFile(ctx, "servers/Linear/save_issue.d.ts", nil, nil)
	golden, _ := os.ReadFile("testdata/dts/save_issue.d.ts")
	if !ok || !strings.HasPrefix(stub, "// Total lines: ") || !strings.HasSuffix(stub, string(golden)) {
		t.Fatalf("read save_issue.d.ts (ok=%v):\n%s", ok, stub)
	}
	// Case-insensitive, and by the server's own key.
	if s, ok := rt.ReadToolFile(ctx, "servers/BK-CORE/get-all-clients.d.ts", nil, nil); !ok || !strings.Contains(s, "tools.bk_core.get_all_clients") {
		t.Fatalf("by key (ok=%v):\n%s", ok, s)
	}
	server, ok := rt.ReadToolFile(ctx, "servers/Linear.d.ts", nil, nil)
	if !ok || !strings.Contains(server, "export interface Tools {\n  /** Fetch one issue. */\n  get_issue(args: import(\"./Linear/get_issue\").Input): Promise<import(\"./Linear/get_issue\").Output>;\n") ||
		!strings.Contains(server, "  /** Create or update an issue. */\n  save_issue(args: import(\"./Linear/save_issue\").Input)") {
		t.Fatalf("server file (ok=%v):\n%s", ok, server)
	}
	index, ok := rt.ReadToolFile(ctx, "servers/index.d.ts", nil, nil)
	for _, want := range []string{"export type TryResult<T = unknown>", "export interface SearchOptions", "parallel<T>(fns: Array<() => Promise<T>>",
		"export interface Tools extends ToolHelpers {\n  Linear: import(\"./Linear\").Tools;\n  bk_core: import(\"./bk_core\").Tools;\n}", "declare global {\n  const tools: Tools;"} {
		if !ok || !strings.Contains(index, want) {
			t.Errorf("index missing %q (ok=%v):\n%s", want, ok, index)
		}
	}
	if s, ok := rt.ReadToolFile(ctx, "index.d.ts", nil, nil); !ok || s != index {
		t.Errorf("index.d.ts without servers/ (ok=%v)", ok)
	}
	// Line slicing works as for .pyi.
	one := 1
	if s, ok := rt.ReadToolFile(ctx, "servers/Linear/save_issue.d.ts", &one, &one); !ok || !strings.HasPrefix(s, "// Total lines: ") || strings.Contains(s, "\n") {
		t.Fatalf("line 1: %q", s)
	}
	for name, want := range map[string]string{
		"servers/Nope/x.d.ts":       "No server found matching 'Nope'",
		"servers/Linear/nope.d.ts":  "Tool 'nope' not found in server 'Linear'",
		"servers/../etc/x.d.ts":     "Invalid filename",
		"servers/Linear/a/b/c.d.ts": "Invalid filename",
	} {
		if s, ok := rt.ReadToolFile(ctx, name, nil, nil); ok || !strings.Contains(s, want) {
			t.Errorf("%s (ok=%v): %s", name, ok, s)
		}
	}
	if s := (&Runtime{caller: &fakeCaller{}}).listTSFiles(ctx); !strings.Contains(s, "No servers are available to you") {
		t.Errorf("empty listing: %s", s)
	}
}
