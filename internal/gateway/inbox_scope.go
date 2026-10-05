package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/tusharbhardwaj/toolyard/internal/inbox"
)

// ValidateScope uses server catalog metadata rather than an agent's operation
// label to require write context. It checks exact arguments against tool schemas.
func (g *Gateway) ValidateScope(ctx context.Context, agentID string, call inbox.ToolRequest) []inbox.Problem {
	g.mu.RLock()
	entry, ok := g.tools[call.Tool]
	g.mu.RUnlock()
	if !ok || !g.allowsEntry(ctx, agentID, entry) {
		return nil
	}
	var problems []inbox.Problem
	add := func(path, message string) { problems = append(problems, inbox.Problem{Path: path, Message: message}) }
	readOnly := entry.tool.Annotations.ReadOnlyHint != nil && *entry.tool.Annotations.ReadOnlyHint
	if call.Operation == "read" && !readOnly {
		add("operation", "catalog does not establish this tool as read-only; use write and supply affected_scope, material_risks and undo")
	}
	b, err := json.Marshal(entry.tool.InputSchema)
	if err != nil {
		add("params", "tool schema cannot be inspected; ask the administrator to fix the catalog")
		return problems
	}
	var schema map[string]any
	if json.Unmarshal(b, &schema) != nil {
		return problems
	}
	required, _ := schema["required"].([]any)
	filtered := []any{}
	for _, v := range required {
		if v != ReasonField && v != GrantField {
			filtered = append(filtered, v)
		}
	}
	schema["required"] = filtered
	args := map[string]any{}
	exact := true
	for name, c := range call.Params {
		if name == ReasonField || name == GrantField {
			add("params."+name, "metadata credentials are not permission scope; omit this parameter")
			continue
		}
		if c.Op != "eq" {
			exact = false
		}
		args[name] = c.Eq
	}
	props, _ := schema["properties"].(map[string]any)
	for _, v := range filtered {
		name, _ := v.(string)
		if _, ok := call.Params[name]; !ok {
			add("params."+name, "required tool parameter is missing; provide its exact value or a bounded constraint")
		}
	}
	if additional, exists := schema["additionalProperties"]; exists && additional == false {
		for name := range call.Params {
			if _, ok := props[name]; !ok {
				add("params."+name, "not a supported parameter for this tool")
			}
		}
	}
	if exact {
		if err := validateInboxSchema(schema, args); err != nil {
			add("params", fmt.Sprintf("parameters do not match the tool schema: %v", err))
		}
		return problems
	}
	for name, c := range call.Params {
		raw, ok := props[name]
		if !ok {
			continue
		}
		property, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch c.Op {
		case "eq":
			if err := validateInboxSchema(property, c.Eq); err != nil {
				add("params."+name, "exact value does not match the tool schema: "+err.Error())
			}
		case "in":
			for _, v := range c.In {
				if err := validateInboxSchema(property, v); err != nil {
					add("params."+name, "a permitted value does not match the tool schema: "+err.Error())
					break
				}
			}
		case "range":
			if c.Gte == nil || c.Lte == nil {
				add("params."+name, "provide both numeric bounds")
			}
			if property["type"] != "number" && property["type"] != "integer" {
				add("params."+name, "numeric constraint requires a numeric parameter")
			}
			if min, present := property["minimum"]; present && c.Gte != nil {
				if cmp, ok := inbox.CompareNumbers(*c.Gte, min); ok && cmp < 0 {
					add("params."+name, "lower bound is outside the tool schema")
				}
			}
			if max, present := property["maximum"]; present && c.Lte != nil {
				if cmp, ok := inbox.CompareNumbers(*c.Lte, max); ok && cmp > 0 {
					add("params."+name, "upper bound is outside the tool schema")
				}
			}
		case "prefix", "limit":
			if property["type"] != "string" {
				add("params."+name, "string constraint requires a string parameter")
			}
		}
	}
	return problems
}

func validateInboxSchema(schema map[string]any, value any) error {
	raw, err := json.Marshal(schema)
	if err != nil {
		return err
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return err
	}
	compiler := jsonschema.NewCompiler()
	const loc = "https://toolyard.invalid/inbox-schema.json"
	if err = compiler.AddResource(loc, doc); err != nil {
		return err
	}
	compiled, err := compiler.Compile(loc)
	if err != nil {
		return err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return err
	}
	return compiled.Validate(instance)
}
