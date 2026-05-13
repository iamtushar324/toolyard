package gateway

import (
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

func TestWrapSchemaInjectsReason(t *testing.T) {
	in := mcp.Tool{
		Name:        "create_issue",
		Description: "Create a GitHub issue",
		InputSchema: mcp.ToolInputSchema{
			Type: "object",
			Properties: map[string]any{
				"repo":  map[string]any{"type": "string"},
				"title": map[string]any{"type": "string"},
			},
			Required: []string{"repo", "title"},
		},
	}
	out, field := wrapSchema(in)
	if field != ReasonField {
		t.Fatalf("expected default reason field, got %q", field)
	}
	if _, ok := out.InputSchema.Properties[ReasonField]; !ok {
		t.Fatal("missing _reason in wrapped schema")
	}
	if _, ok := out.InputSchema.Properties[IntentField]; !ok {
		t.Fatal("missing _intent_category in wrapped schema")
	}
	hasRequired := false
	for _, r := range out.InputSchema.Required {
		if r == ReasonField {
			hasRequired = true
		}
	}
	if !hasRequired {
		t.Fatal("_reason should be required")
	}
	if !strings.Contains(out.Description, "toolyard-gated") {
		t.Fatal("missing description banner")
	}
	if _, ok := out.InputSchema.Properties["repo"]; !ok {
		t.Fatal("upstream property repo dropped")
	}
}

func TestWrapSchemaCollisionFallback(t *testing.T) {
	in := mcp.Tool{
		Name: "tool_with_clash",
		InputSchema: mcp.ToolInputSchema{
			Type:       "object",
			Properties: map[string]any{ReasonField: map[string]any{"type": "string"}},
		},
	}
	out, field := wrapSchema(in)
	if field != FallbackField {
		t.Errorf("expected %q fallback when upstream uses _reason, got %q", FallbackField, field)
	}
	if _, ok := out.InputSchema.Properties[FallbackField]; !ok {
		t.Fatal("missing fallback field")
	}
}

func TestExtractReason(t *testing.T) {
	args := map[string]any{
		"_reason":          "this is a 21-char reason string",
		"_intent_category": "read",
		"key":              "x",
	}
	reason, intent, clean, err := extractReason(args, ReasonField)
	if err != nil {
		t.Fatal(err)
	}
	if reason != "this is a 21-char reason string" {
		t.Errorf("reason got %q", reason)
	}
	if intent != "read" {
		t.Errorf("intent got %q", intent)
	}
	if _, ok := clean["_reason"]; ok {
		t.Error("_reason should be stripped")
	}
	if _, ok := clean["_intent_category"]; ok {
		t.Error("_intent_category should be stripped")
	}
	if clean["key"] != "x" {
		t.Error("upstream args not preserved")
	}

	// Too short
	if _, _, _, err := extractReason(map[string]any{"_reason": "short"}, ReasonField); err == nil {
		t.Error("expected error for short reason")
	}
	// Missing
	if _, _, _, err := extractReason(map[string]any{}, ReasonField); err == nil {
		t.Error("expected error for missing reason")
	}
}
