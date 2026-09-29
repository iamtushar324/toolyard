package voice

import (
	"context"
	"log/slog"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"google.golang.org/genai"

	"github.com/tusharbhardwaj/toolyard/internal/gateway"
)

// fakeBackend implements ToolBackend for tests.
type fakeBackend struct {
	catalog []gateway.CatalogEntry

	gotVia    string
	gotTarget string
	gotArgs   map[string]any
	result    *mcp.CallToolResult
	err       error
}

func (f *fakeBackend) CatalogFor(context.Context) []gateway.CatalogEntry { return f.catalog }

func (f *fakeBackend) RouteCall(_ context.Context, viaTool, targetName string, args map[string]any) (*mcp.CallToolResult, error) {
	f.gotVia, f.gotTarget, f.gotArgs = viaTool, targetName, args
	return f.result, f.err
}

func TestBuildGenaiTools(t *testing.T) {
	catalog := []gateway.CatalogEntry{
		{
			Name:        "memory.set",
			Description: "Store a value",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"key":     map[string]any{"type": "string", "description": "the key"},
					"value":   map[string]any{"type": "string"},
					"_reason": map[string]any{"type": "string"},
					"tags":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"mode":    map[string]any{"type": "string", "enum": []any{"set", "append"}},
				},
				"required": []any{"key", "value", "_reason"},
			},
		},
		{
			// Name too long for Gemini (>64 chars) — must be skipped, not break.
			Name:        "upstream.this_tool_name_is_way_too_long_for_gemini_function_declarations_x",
			Description: "skipped",
			InputSchema: map[string]any{"type": "object"},
		},
	}

	tools := buildGenaiTools(catalog, slog.Default())
	if len(tools) != 1 {
		t.Fatalf("want 1 genai.Tool, got %d", len(tools))
	}
	decls := tools[0].FunctionDeclarations
	if len(decls) != 1 {
		t.Fatalf("want 1 declaration (long name skipped), got %d", len(decls))
	}
	d := decls[0]
	if d.Name != "memory.set" {
		t.Errorf("name = %q", d.Name)
	}
	p := d.Parameters
	if p == nil || p.Type != genai.TypeObject {
		t.Fatalf("parameters type = %+v, want object", p)
	}
	if len(p.Required) != 3 || p.Required[2] != "_reason" {
		t.Errorf("required = %v, want [key value _reason]", p.Required)
	}
	key := p.Properties["key"]
	if key == nil || key.Type != genai.TypeString || key.Description != "the key" {
		t.Errorf("key schema = %+v", key)
	}
	tags := p.Properties["tags"]
	if tags == nil || tags.Type != genai.TypeArray || tags.Items == nil || tags.Items.Type != genai.TypeString {
		t.Errorf("tags schema = %+v", tags)
	}
	mode := p.Properties["mode"]
	if mode == nil || len(mode.Enum) != 2 || mode.Enum[0] != "set" {
		t.Errorf("mode schema = %+v", mode)
	}
}

func TestMapToGenaiSchemaBackfillsObjectType(t *testing.T) {
	// Catalog schemas sometimes omit "type" at the top level; Gemini wants
	// an explicit object when properties exist.
	s := mapToGenaiSchema(map[string]any{
		"properties": map[string]any{"a": map[string]any{"type": "string"}},
	})
	if s.Type != genai.TypeObject {
		t.Errorf("type = %v, want object", s.Type)
	}
}

func TestDispatchTool(t *testing.T) {
	fb := &fakeBackend{result: mcp.NewToolResultText("stored ok")}
	out, err := dispatchTool(context.Background(), fb, "memory.set", map[string]any{
		"key": "k", "_reason": "operator asked me to remember this on the call",
	})
	if err != nil {
		t.Fatalf("dispatchTool: %v", err)
	}
	if out != "stored ok" {
		t.Errorf("out = %q", out)
	}
	if fb.gotVia != "voice" || fb.gotTarget != "memory.set" {
		t.Errorf("routed via=%q target=%q", fb.gotVia, fb.gotTarget)
	}
	if fb.gotArgs["key"] != "k" {
		t.Errorf("args = %v", fb.gotArgs)
	}
}

func TestDispatchToolErrorResult(t *testing.T) {
	fb := &fakeBackend{result: mcp.NewToolResultError("denied by policy: nope")}
	_, err := dispatchTool(context.Background(), fb, "github.create_issue", map[string]any{})
	if err == nil {
		t.Fatal("want error for IsError result")
	}
	if got := err.Error(); got != "denied by policy: nope" {
		t.Errorf("err = %q", got)
	}
}

func TestDispatchToolNilArgs(t *testing.T) {
	fb := &fakeBackend{result: mcp.NewToolResultText("ok")}
	if _, err := dispatchTool(context.Background(), fb, "tools.approval_stats", nil); err != nil {
		t.Fatalf("dispatchTool with nil args: %v", err)
	}
	if fb.gotArgs == nil {
		t.Error("nil args must be normalised to an empty map")
	}
}
