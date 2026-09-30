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
	ReasonField     = "_reason"
	IntentField     = "_intent_category"
	ApprovalIDField = "_approval_id"
	GrantField      = "_grant"
	// SessionField names the toolyard agent session (session.start) a
	// call belongs to. Stripped before the tool sees it; honoured only
	// when the session belongs to the calling agent.
	SessionField  = "_session_id"
	FallbackField = "__toolyard_reason"

	minReasonLen = 20
	maxReasonLen = 2000

	reasonPropDescription     = "One short sentence on why you are calling this tool. Shown verbatim to the human approver. Required, 20-2000 chars."
	intentPropDescription     = "Coarse intent category: read | write | destructive | external_communication | financial | privileged_admin. Declaring write, destructive, external_communication, financial or privileged_admin makes the call need approval unless an explicit rule allows it; declaring read never lets a call skip approval."
	approvalIDPropDescription = "If a previous call to this tool returned a deferred response with an `approval_id`, set this to that value to resume the held call instead of creating a new approval."
	grantPropDescription      = "A grant token (tyg_…) your owner issued for this exact call through inbox.request. Restricted tools run only with a valid grant; the call must stay within the parameters you asked for."
	sessionPropDescription    = "Optional: the id session.start gave you (ses_…), so this call is recorded under that piece of work."
	descriptionBanner         = "[toolyard-gated · _reason required · restricted tools need your owner's permission: check with inbox.check, ask with inbox.request, then pass the grant as _grant] "
)

var intentEnum = []string{
	"read", "write", "destructive", "external_communication",
	"financial", "privileged_admin",
}

// metaProps returns the toolyard-injected schema properties: _reason
// (required), _intent_category, _approval_id, _grant and _session_id.
// Built-in tool schemas merge these into their hand-rolled property maps
// so direct upstream calls and meta-tools stay consistent.
func metaProps() map[string]any {
	return map[string]any{
		ReasonField: map[string]any{
			"type": "string", "minLength": minReasonLen, "maxLength": maxReasonLen,
			"description": reasonPropDescription,
		},
		IntentField: map[string]any{
			"type": "string", "enum": intentEnum, "description": intentPropDescription,
		},
		ApprovalIDField: map[string]any{
			"type": "string", "description": approvalIDPropDescription,
		},
		GrantField: map[string]any{
			"type": "string", "description": grantPropDescription,
		},
		SessionField: map[string]any{
			"type": "string", "description": sessionPropDescription,
		},
	}
}

// addMetaProps merges metaProps into props (without overwriting existing
// keys). Used by built-in tool definitions.
func addMetaProps(props map[string]any) map[string]any {
	for k, v := range metaProps() {
		if _, has := props[k]; !has {
			props[k] = v
		}
	}
	return props
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
	if _, has := props[ApprovalIDField]; !has {
		props[ApprovalIDField] = map[string]any{
			"type":        "string",
			"description": approvalIDPropDescription,
		}
	}
	if _, has := props[GrantField]; !has {
		props[GrantField] = map[string]any{
			"type":        "string",
			"description": grantPropDescription,
		}
	}
	if _, has := props[SessionField]; !has {
		props[SessionField] = map[string]any{
			"type":        "string",
			"description": sessionPropDescription,
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
	return extractReasonOpt(args, fieldName, false)
}

// extractReasonOpt is extractReason for a tool whose reason may be omitted:
// with optional set, a missing field yields an empty reason instead of an
// error. A reason that is present is validated either way.
func extractReasonOpt(args map[string]any, fieldName string, optional bool) (string, string, map[string]any, error) {
	if args == nil {
		args = map[string]any{}
	}
	var reason string
	rv, ok := args[fieldName]
	switch {
	case !ok && optional:
	case !ok:
		return "", "", nil, fmt.Errorf("%s is required: provide a one-sentence rationale (%d-%d chars)",
			fieldName, minReasonLen, maxReasonLen)
	default:
		s, isStr := rv.(string)
		if !isStr {
			return "", "", nil, fmt.Errorf("%s must be a string", fieldName)
		}
		reason = strings.TrimSpace(s)
		if len(reason) < minReasonLen {
			return "", "", nil, fmt.Errorf("%s too short (got %d chars, need %d)", fieldName, len(reason), minReasonLen)
		}
		if len(reason) > maxReasonLen {
			return "", "", nil, fmt.Errorf("%s too long (got %d chars, max %d)", fieldName, len(reason), maxReasonLen)
		}
	}

	var intent string
	if iv, has := args[IntentField]; has {
		if s, ok := iv.(string); ok {
			intent = s
		}
	}

	out := make(map[string]any, len(args))
	for k, v := range args {
		if k == fieldName || k == IntentField || k == ApprovalIDField || k == GrantField || k == SessionField {
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
