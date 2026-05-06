// Package gateway implements toolyard's MCP-side gateway: it speaks MCP to
// agents (stdio + streamable HTTP), proxies tools to upstream MCP servers,
// rewrites tool schemas to require an explicit `_reason`, and routes every
// call through the policy engine and approval bus.
//
// schema_wrap.go is the schema-rewriting half: inject `_reason` and
// `_intent_category` into upstream tool input schemas, and strip those fields
// before forwarding the call.
package gateway

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

const (
	ReasonField   = "_reason"
	IntentField   = "_intent_category"
	FallbackField = "__toolyard_reason"

	minReasonLen = 20
	maxReasonLen = 2000

	reasonPropDescription = "One short sentence on why you are calling this tool. Shown verbatim to the human approver. Required, 20-2000 chars."
	intentPropDescription = "Coarse intent category: read | write | destructive | external_communication | financial | privileged_admin."
	descriptionBanner     = "[toolyard-gated · _reason required · writes need approval · safe to batch with parallel tool calls so the human reviews them together] "
)

var intentEnum = []string{
	"read", "write", "destructive", "external_communication",
	"financial", "privileged_admin",
}

// wrapSchema returns a deep-copied tool with the _reason / _intent_category
// fields prepended to its input schema and a description banner. If the
// upstream schema already has a property literally called _reason, the
// gateway uses __toolyard_reason instead and reports the chosen field name.
func wrapSchema(t mcp.Tool) (mcp.Tool, string) {
	field := ReasonField
	props := map[string]any{}
	required := []string{}
	additional := any(nil)

	// Decide source schema: prefer InputSchema (parsed), fall back to
	// RawInputSchema (json.RawMessage from upstream).
	hasParsed := t.InputSchema.Type != "" || len(t.InputSchema.Properties) > 0
	if hasParsed {
		for k, v := range t.InputSchema.Properties {
			props[k] = v
		}
		required = append([]string(nil), t.InputSchema.Required...)
		additional = t.InputSchema.AdditionalProperties
	} else if len(t.RawInputSchema) > 0 {
		var raw map[string]any
		if err := json.Unmarshal(t.RawInputSchema, &raw); err == nil {
			if rp, ok := raw["properties"].(map[string]any); ok {
				for k, v := range rp {
					props[k] = v
				}
			}
			if rr, ok := raw["required"].([]any); ok {
				for _, x := range rr {
					if s, ok := x.(string); ok {
						required = append(required, s)
					}
				}
			}
			additional = raw["additionalProperties"]
		}
	}

	if _, clash := props[ReasonField]; clash {
		field = FallbackField
	}

	props[field] = map[string]any{
		"type":        "string",
		"minLength":   minReasonLen,
		"maxLength":   maxReasonLen,
		"description": reasonPropDescription,
	}
	if _, has := props[IntentField]; !has {
		props[IntentField] = map[string]any{
			"type":        "string",
			"enum":        intentEnum,
			"description": intentPropDescription,
		}
	}
	required = appendUnique(required, field)

	out := t
	out.InputSchema = mcp.ToolInputSchema{
		Type:                 "object",
		Properties:           props,
		Required:             required,
		AdditionalProperties: additional,
	}
	out.RawInputSchema = nil // we have a structured schema now
	out.Description = descriptionBanner + t.Description
	return out, field
}

func appendUnique(xs []string, s string) []string {
	for _, x := range xs {
		if x == s {
			return xs
		}
	}
	return append(xs, s)
}

// extractReason pulls _reason / _intent_category off the args map, validates
// length, and returns (reason, intentCategory, strippedArgs, error). The
// returned map is a new copy; the caller may safely forward it upstream.
func extractReason(args map[string]any, fieldName string) (string, string, map[string]any, error) {
	if args == nil {
		args = map[string]any{}
	}
	rv, ok := args[fieldName]
	if !ok {
		return "", "", nil, fmt.Errorf("%s is required: provide a one-sentence rationale (%d-%d chars)",
			fieldName, minReasonLen, maxReasonLen)
	}
	reason, ok := rv.(string)
	if !ok {
		return "", "", nil, fmt.Errorf("%s must be a string", fieldName)
	}
	reason = strings.TrimSpace(reason)
	if len(reason) < minReasonLen {
		return "", "", nil, fmt.Errorf("%s too short (got %d chars, need %d)", fieldName, len(reason), minReasonLen)
	}
	if len(reason) > maxReasonLen {
		return "", "", nil, fmt.Errorf("%s too long (got %d chars, max %d)", fieldName, len(reason), maxReasonLen)
	}

	var intent string
	if iv, has := args[IntentField]; has {
		if s, ok := iv.(string); ok {
			intent = s
		}
	}

	out := make(map[string]any, len(args))
	for k, v := range args {
		if k == fieldName || k == IntentField {
			continue
		}
		out[k] = v
	}
	return reason, intent, out, nil
}

// argsAsMap normalises CallToolRequest.Params.Arguments (which is `any`) into
// a string-keyed map.
func argsAsMap(arguments any) map[string]any {
	switch v := arguments.(type) {
	case nil:
		return map[string]any{}
	case map[string]any:
		return v
	case json.RawMessage:
		var m map[string]any
		_ = json.Unmarshal(v, &m)
		return m
	}
	// Best-effort fallback via JSON round-trip.
	b, err := json.Marshal(arguments)
	if err != nil {
		return map[string]any{}
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if m == nil {
		return map[string]any{}
	}
	return m
}
