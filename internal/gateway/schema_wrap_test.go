package gateway

import (
	"encoding/json"
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

func TestWrapSchemaKeepsDefs(t *testing.T) {
	filter := map[string]any{"type": "object", "properties": map[string]any{"threadId": map[string]any{"type": "string"}}}
	parsed := mcp.Tool{
		Name: "brief",
		InputSchema: mcp.ToolInputSchema{
			Type:       "object",
			Properties: map[string]any{"filter": map[string]any{"$ref": "#/$defs/Filter"}},
			Defs:       map[string]any{"Filter": filter},
		},
	}
	out, _ := wrapSchema(parsed)
	if out.InputSchema.Defs["Filter"] == nil {
		t.Fatalf("parsed schema lost $defs: %#v", out.InputSchema.Defs)
	}

	raw := mcp.Tool{
		Name:           "brief_raw",
		RawInputSchema: []byte(`{"type":"object","properties":{"filter":{"$ref":"#/$defs/Filter"}},"$defs":{"Filter":{"type":"object"}}}`),
	}
	out, _ = wrapSchema(raw)
	if out.InputSchema.Defs["Filter"] == nil {
		t.Fatalf("raw schema lost $defs: %#v", out.InputSchema.Defs)
	}

	draft07 := mcp.Tool{
		Name:           "brief_draft07",
		RawInputSchema: []byte(`{"type":"object","properties":{"filter":{"anyOf":[{"$ref":"#/definitions/Filter"},{"type":"null"}]}},"definitions":{"Filter":{"type":"object"}}}`),
	}
	out, _ = wrapSchema(draft07)
	if out.InputSchema.Defs["Filter"] == nil {
		t.Fatalf("draft-07 schema lost definitions: %#v", out.InputSchema.Defs)
	}
	b, err := json.Marshal(out.InputSchema)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"$ref":"#/$defs/Filter"`) || !strings.Contains(string(b), `"$defs":{"Filter"`) {
		t.Errorf("draft-07 refs not rewritten to $defs: %s", b)
	}
}

// TestWrapSchemaRewritesOnlySchemaRefs: a draft-07 "$ref" inside const,
// enum, default or examples is data and stays as it is.
func TestWrapSchemaRewritesOnlySchemaRefs(t *testing.T) {
	raw := mcp.Tool{
		Name: "data_refs",
		RawInputSchema: []byte(`{"type":"object","definitions":{"Filter":{"type":"object","properties":{"next":{"$ref":"#/definitions/Filter"}}}},
			"properties":{
				"filter":{"type":"array","items":{"$ref":"#/definitions/Filter"}},
				"marker":{"const":{"$ref":"#/definitions/Filter"}},
				"kind":{"enum":[{"$ref":"#/definitions/Filter"}],"default":{"$ref":"#/definitions/Filter"},"examples":[{"$ref":"#/definitions/Filter"}]}}}`),
	}
	out, _ := wrapSchema(raw)
	props := out.InputSchema.Properties
	if got := props["filter"].(map[string]any)["items"].(map[string]any)["$ref"]; got != "#/$defs/Filter" {
		t.Errorf("schema ref not rewritten: %v", got)
	}
	next := out.InputSchema.Defs["Filter"].(map[string]any)["properties"].(map[string]any)["next"].(map[string]any)
	if next["$ref"] != "#/$defs/Filter" {
		t.Errorf("ref inside $defs not rewritten: %v", next["$ref"])
	}
	kind := props["kind"].(map[string]any)
	for name, data := range map[string]any{
		"const":    props["marker"].(map[string]any)["const"],
		"enum":     kind["enum"].([]any)[0],
		"default":  kind["default"],
		"examples": kind["examples"].([]any)[0],
	} {
		if got := data.(map[string]any)["$ref"]; got != "#/definitions/Filter" {
			t.Errorf("%s data changed to %v", name, got)
		}
	}
}
