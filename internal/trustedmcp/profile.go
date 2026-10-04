package trustedmcp

import (
	"bytes"
	"encoding/json"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Profile is reviewed, service-owned configuration, never caller metadata.
// Runtime code must pin its exact file digest and reject changes until restart.
type Profile struct {
	Version              int           `json:"version"`
	Endpoint             string        `json:"endpoint"`
	Issuer               string        `json:"issuer"`
	Audience             string        `json:"audience"`
	KeyID                string        `json:"key_id"`
	ProtocolVersion      string        `json:"protocol_version"`
	TimeoutSeconds       int           `json:"timeout_seconds"`
	HeaderTimeoutSeconds int           `json:"header_timeout_seconds"`
	Tools                []ToolProfile `json:"tools"`
}

// ToolProfile limits a remote operation and its exposed alias and schema.
type ToolProfile struct {
	Alias            string         `json:"alias"`
	Operation        string         `json:"operation"`
	Description      string         `json:"description"`
	Schema           map[string]any `json:"schema"`
	ReadOnly         bool           `json:"read_only"`
	Destructive      bool           `json:"destructive"`
	IdempotencyField string         `json:"idempotency_field,omitempty"`
}

var aliasRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,63}$`)
var operationRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]{0,127}$`)
var assertionConfigRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:._/-]{0,159}$`)

// LoadProfile accepts only bounded private regular files in a trusted hierarchy.
func LoadProfile(path string, ownerUID uint32) (Profile, error) {
	raw, err := PrivateFile(path, ownerUID, 65536)
	if err != nil {
		return Profile{}, ErrIdentity
	}
	return ParseProfile(raw)
}

// ParseProfile performs no network request or credential lookup.
func ParseProfile(raw []byte) (Profile, error) {
	var p Profile
	if len(raw) > 65536 || !uniqueJSONKeys(raw) {
		return p, ErrIdentity
	}
	var shape map[string]json.RawMessage
	if json.Unmarshal(raw, &shape) != nil {
		return p, ErrIdentity
	}
	allowed := map[string]bool{"version": true, "endpoint": true, "issuer": true, "audience": true, "key_id": true, "protocol_version": true, "timeout_seconds": true, "header_timeout_seconds": true, "tools": true}
	for field := range shape {
		if !allowed[field] {
			return p, ErrIdentity
		}
	}
	var tools []map[string]json.RawMessage
	if json.Unmarshal(shape["tools"], &tools) != nil {
		return p, ErrIdentity
	}
	allowedTool := map[string]bool{"alias": true, "operation": true, "description": true, "schema": true, "read_only": true, "destructive": true, "idempotency_field": true}
	for _, tool := range tools {
		for _, field := range []string{"read_only", "destructive"} {
			value := string(bytes.TrimSpace(tool[field]))
			if value != "true" && value != "false" {
				return p, ErrIdentity
			}
		}
		for field := range tool {
			if !allowedTool[field] {
				return p, ErrIdentity
			}
		}
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&p) != nil || d.Decode(new(any)) != io.EOF || validateProfile(p) != nil {
		return Profile{}, ErrIdentity
	}
	return p, nil
}

type denySchemaLoader struct{}

func (denySchemaLoader) Load(string) (any, error) { return nil, ErrIdentity }

func compileSchema(schema map[string]any) (*jsonschema.Schema, error) {
	raw, err := json.Marshal(schema)
	if err != nil || len(raw) > 16384 {
		return nil, ErrIdentity
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, ErrIdentity
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.UseLoader(denySchemaLoader{})
	const location = "mem:///trusted-mcp-schema.json"
	if compiler.AddResource(location, doc) != nil {
		return nil, ErrIdentity
	}
	compiled, err := compiler.Compile(location)
	if err != nil {
		return nil, ErrIdentity
	}
	return compiled, nil
}

func validateProfile(p Profile) error {
	u, err := url.Parse(p.Endpoint)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" || u.Path == "" || u.Path[0] != '/' || u.String() != p.Endpoint {
		return ErrIdentity
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 || strconv.Itoa(port) != u.Port() {
		return ErrIdentity
	}
	if p.Version != 1 || !assertionConfigRE.MatchString(p.Issuer) || !assertionConfigRE.MatchString(p.Audience) || !assertionConfigRE.MatchString(p.KeyID) || p.TimeoutSeconds < 5 || p.TimeoutSeconds > 65 || p.HeaderTimeoutSeconds < 1 || p.HeaderTimeoutSeconds > 35 || p.HeaderTimeoutSeconds >= p.TimeoutSeconds || len(p.Tools) < 1 || len(p.Tools) > 32 {
		return ErrIdentity
	}
	if _, err := time.Parse("2006-01-02", p.ProtocolVersion); err != nil {
		return ErrIdentity
	}
	aliases, operations := map[string]bool{}, map[string]bool{}
	for _, tool := range p.Tools {
		if !aliasRE.MatchString(tool.Alias) || !operationRE.MatchString(tool.Operation) || aliases[tool.Alias] || operations[tool.Operation] || len(tool.Description) > 2000 || tool.Schema["type"] != "object" || tool.Schema["additionalProperties"] != false {
			return ErrIdentity
		}
		properties, ok := tool.Schema["properties"].(map[string]any)
		if !ok || len(properties) > 32 {
			return ErrIdentity
		}
		if _, err := compileSchema(tool.Schema); err != nil {
			return ErrIdentity
		}
		if tool.IdempotencyField != "" {
			property, ok := properties[tool.IdempotencyField].(map[string]any)
			if !ok || property["type"] != "string" {
				return ErrIdentity
			}
			found := false
			raw, _ := json.Marshal(tool.Schema["required"])
			var required []string
			_ = json.Unmarshal(raw, &required)
			for _, field := range required {
				if field == tool.IdempotencyField {
					found = true
				}
			}
			if !found {
				return ErrIdentity
			}
		}
		aliases[tool.Alias] = true
		operations[tool.Operation] = true
	}
	return nil
}

// Catalog is only reviewed bootstrap metadata. It is not remote discovery.
func (c *Client) Catalog() []mcp.Tool {
	if c == nil {
		return nil
	}
	tools := make([]mcp.Tool, 0, len(c.profile.Tools))
	for _, entry := range c.profile.Tools {
		raw, _ := json.Marshal(entry.Schema)
		tools = append(tools, mcp.Tool{Name: entry.Alias, Description: entry.Description, RawInputSchema: raw, Annotations: mcp.ToolAnnotation{ReadOnlyHint: mcp.ToBoolPtr(entry.ReadOnly), DestructiveHint: mcp.ToBoolPtr(entry.Destructive), IdempotentHint: mcp.ToBoolPtr(entry.IdempotencyField != ""), OpenWorldHint: mcp.ToBoolPtr(false)}})
	}
	return tools
}

func (c *Client) Operation(alias string) string {
	if c == nil {
		return ""
	}
	for _, tool := range c.profile.Tools {
		if tool.Alias == alias {
			return tool.Operation
		}
	}
	return ""
}
