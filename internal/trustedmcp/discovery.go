package trustedmcp

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

// Discover authenticates the actual enrolled caller. It never runs at startup
// or under an anonymous service identity. Metadata comes only from the profile.
func (c *Client) Discover(ctx context.Context, id string) ([]mcp.Tool, error) {
	if c == nil || c.Signer == nil {
		return nil, ErrIdentity
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(c.profile.TimeoutSeconds)*time.Second)
	defer cancel()
	if c.Check == nil || c.Check(ctx) != nil {
		return nil, ErrIdentity
	}
	if err := c.Signer.Preflight(ctx, id); err != nil {
		return nil, err
	}
	assertions := make([]string, 0, 3)
	return c.discover(ctx, id, &assertions)
}

func (c *Client) discover(ctx context.Context, id string, assertions *[]string) ([]mcp.Tool, error) {
	initial, err := c.post(ctx, id, 1, "initialize", map[string]any{"protocolVersion": c.profile.ProtocolVersion, "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "toolyard-trusted-mcp", "version": "0.1.0"}}, false, assertions)
	if err != nil {
		return nil, err
	}
	if ctx.Err() != nil {
		return nil, ErrTransport
	}
	var initialized map[string]json.RawMessage
	var version string
	if json.Unmarshal(initial, &initialized) != nil || json.Unmarshal(initialized["protocolVersion"], &version) != nil || version != c.profile.ProtocolVersion {
		return nil, ErrTransport
	}
	if _, err = c.post(ctx, id, 0, "notifications/initialized", nil, true, assertions); err != nil {
		return nil, err
	}
	result, err := c.post(ctx, id, 2, "tools/list", map[string]any{}, false, assertions)
	if err != nil {
		return nil, err
	}
	if ctx.Err() != nil {
		return nil, ErrTransport
	}
	var list map[string]json.RawMessage
	if json.Unmarshal(result, &list) != nil {
		return nil, ErrTransport
	}
	if next, exists := list["nextCursor"]; exists && string(bytes.TrimSpace(next)) != "null" && string(bytes.TrimSpace(next)) != `""` {
		return nil, ErrTransport
	}
	var remote []map[string]json.RawMessage
	if json.Unmarshal(list["tools"], &remote) != nil || remote == nil || len(remote) > 128 {
		return nil, ErrTransport
	}
	reviewed := map[string]ToolProfile{}
	for _, tool := range c.profile.Tools {
		reviewed[tool.Operation] = tool
	}
	seen := map[string]bool{}
	for _, tool := range remote {
		var name string
		if json.Unmarshal(tool["name"], &name) != nil || !operationRE.MatchString(name) || seen[name] {
			return nil, ErrTransport
		}
		seen[name] = true
		approved, ok := reviewed[name]
		if !ok {
			continue
		}
		var schema map[string]any
		if json.Unmarshal(tool["inputSchema"], &schema) != nil || !compatibleSchema(approved.Schema, schema) {
			return nil, ErrTransport
		}
		delete(reviewed, name)
	}
	if len(reviewed) != 0 {
		return nil, ErrTransport
	}
	return c.Catalog(), nil
}

// A reviewed profile may tighten SDK-generated schemas. The remote operation
// must preserve the exact fields, required set, primitive types and defaults.
// New remote constraints or references require an explicit profile review.
func compatibleSchema(reviewed, remote map[string]any) bool {
	if remote["type"] != "object" || reviewed["type"] != "object" {
		return false
	}
	if additional, ok := remote["additionalProperties"]; ok && additional != false {
		return false
	}
	localProperties, ok := reviewed["properties"].(map[string]any)
	if !ok {
		return false
	}
	remoteProperties, ok := remote["properties"].(map[string]any)
	if !ok || len(remoteProperties) != len(localProperties) {
		return false
	}
	localRequired, ok := requiredSet(reviewed["required"])
	if !ok {
		return false
	}
	remoteRequired, ok := requiredSet(remote["required"])
	if !ok || !reflect.DeepEqual(localRequired, remoteRequired) {
		return false
	}
	for field, localRaw := range localProperties {
		local, ok := localRaw.(map[string]any)
		if !ok {
			return false
		}
		candidate, ok := remoteProperties[field].(map[string]any)
		if !ok {
			return false
		}
		localType, ok := local["type"].(string)
		if !ok {
			return false
		}
		if !compatibleType(localType, candidate, !localRequired[field]) {
			return false
		}
		if expectedDefault, reviewedDefault := local["default"]; reviewedDefault {
			actualDefault, remoteDefault := candidate["default"]
			if !remoteDefault || !reflect.DeepEqual(expectedDefault, actualDefault) {
				return false
			}
		}
		for keyword, value := range candidate {
			switch keyword {
			case "type", "title", "description":
				continue
			case "anyOf":
				if _, exists := candidate["type"]; exists {
					return false
				}
				continue
			case "default":
				if value == nil && !localRequired[field] {
					continue
				}
			}
			if !reflect.DeepEqual(local[keyword], value) {
				return false
			}
		}
	}
	for field := range remote {
		if field != "type" && field != "properties" && field != "required" && field != "additionalProperties" && field != "title" && field != "description" {
			return false
		}
	}
	return true
}

func requiredSet(raw any) (map[string]bool, bool) {
	if raw == nil {
		return map[string]bool{}, true
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, false
	}
	var fields []string
	if json.Unmarshal(encoded, &fields) != nil {
		return nil, false
	}
	set := map[string]bool{}
	for _, field := range fields {
		if set[field] {
			return nil, false
		}
		set[field] = true
	}
	return set, true
}

func compatibleType(expected string, property map[string]any, optional bool) bool {
	if primitive, ok := property["type"].(string); ok {
		return primitive == expected
	}
	union, ok := property["anyOf"].([]any)
	if !ok || !optional || len(union) != 2 {
		return false
	}
	seen := map[string]bool{}
	for _, raw := range union {
		branch, ok := raw.(map[string]any)
		if !ok || len(branch) != 1 {
			return false
		}
		name, ok := branch["type"].(string)
		if !ok || seen[name] || (name != expected && name != "null") {
			return false
		}
		seen[name] = true
	}
	return seen[expected] && seen["null"]
}
