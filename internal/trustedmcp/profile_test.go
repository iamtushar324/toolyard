package trustedmcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTrustedMCPProfileRejectsNetworkReferencesAndCallerControlledConfiguration(t *testing.T) {
	for _, mutate := range []func(*Profile){
		func(p *Profile) { p.Endpoint = "https://public.invalid/mcp" },
		func(p *Profile) { p.Endpoint = "http://localhost:18791/mcp" },
		func(p *Profile) { p.Endpoint = "http://user:password@127.0.0.1:18791/mcp" },
		func(p *Profile) { p.TimeoutSeconds = 66 },
		func(p *Profile) { p.HeaderTimeoutSeconds = 36 },
		func(p *Profile) { p.Issuer = "" },
		func(p *Profile) { p.Tools = append(p.Tools, p.Tools[0]) },
		func(p *Profile) { p.Tools[0].Alias = "other.create" },
		func(p *Profile) { p.Tools[0].Schema["additionalProperties"] = true },
		func(p *Profile) { p.Tools[0].Schema["$ref"] = "https://public.invalid/schema" },
		func(p *Profile) { p.Tools[0].IdempotencyField = "caller_session" },
	} {
		p := testProfile(t)
		mutate(&p)
		raw, _ := json.Marshal(p)
		if _, err := ParseProfile(raw); !errors.Is(err, ErrIdentity) {
			t.Fatal("unsafe trusted profile accepted")
		}
	}
	raw, err := os.ReadFile("../../config/preview-mcp.profile.json")
	if err != nil {
		t.Fatal("profile fixture unavailable")
	}
	for _, bad := range []string{strings.Replace(string(raw), `"version": 1`, `"version": 1, "Version": 1`, 1), strings.Replace(string(raw), `"read_only": false`, `"read_only": null`, 1), strings.Replace(string(raw), `"version": 1`, `"version": 1, "version": 1`, 1)} {
		if _, err := ParseProfile([]byte(bad)); !errors.Is(err, ErrIdentity) {
			t.Fatal("ambiguous trusted profile accepted")
		}
	}
}

func TestTrustedMCPProfilePrivateFileAndSnapshotImmutability(t *testing.T) {
	raw, err := os.ReadFile("../../config/preview-mcp.profile.json")
	if err != nil {
		t.Fatal("profile fixture unavailable")
	}
	dir := t.TempDir()
	if os.Chmod(dir, 0700) != nil {
		t.Fatal("private test profile directory unavailable")
	}
	path := filepath.Join(dir, "profile.json")
	if os.WriteFile(path, raw, 0600) != nil {
		t.Fatal("test profile unavailable")
	}
	p, err := LoadProfile(path, uint32(os.Geteuid()))
	if err != nil {
		t.Fatal("private profile refused")
	}
	c, err := NewClient(signer(t, binding(agentA, sessionA)), p)
	if err != nil {
		t.Fatal("trusted client refused")
	}
	c.Check = func(context.Context) error { return nil }
	publicOperation := p.Tools[0].Operation
	p.Endpoint = "http://127.0.0.1:19999/changed"
	p.Tools[0].Operation = "changed"
	c.Profile.Tools[0].Operation = "public_changed"
	if c.Operation("create") != publicOperation {
		t.Fatal("caller changed immutable profile snapshot")
	}
	if os.Chmod(path, 0644) != nil {
		t.Fatal("test chmod failed")
	}
	if _, err := LoadProfile(path, uint32(os.Geteuid())); !errors.Is(err, ErrIdentity) {
		t.Fatal("public profile accepted")
	}
}

func TestTrustedMCPConnectorUsesGenericProfileWithoutServiceSpecificGoCode(t *testing.T) {
	p := testProfile(t)
	p.Endpoint = "http://127.0.0.1:18812/rpc"
	p.Issuer = "another-issuer"
	p.Audience = "another-audience"
	p.KeyID = "another-key"
	p.Tools = []ToolProfile{{Alias: "status", Operation: "task_status", Description: "Read an owned task.", Schema: map[string]any{"type": "object", "properties": map[string]any{}, "required": []string{}, "additionalProperties": false}, ReadOnly: true}}
	c, err := NewClient(signer(t, binding(agentA, sessionA)), p)
	if err != nil {
		t.Fatal("generic profile refused")
	}
	c.Check = func(context.Context) error { return nil }
	posts := 0
	c.http.Transport = roundTrip(func(req *http.Request) (*http.Response, error) {
		posts++
		if req.URL.String() != p.Endpoint {
			t.Fatal("generic endpoint ignored")
		}
		claims := claims(t, strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer "))
		if claims["iss"] != p.Issuer || claims["aud"] != p.Audience {
			t.Fatal("generic assertion configuration ignored")
		}
		raw, _ := io.ReadAll(req.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		switch body["method"] {
		case "initialize":
			return response(200, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-11-25"}}`), nil
		case "notifications/initialized":
			return response(202, ""), nil
		case "tools/list":
			return response(200, `{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"task_status","inputSchema":{"type":"object","properties":{}}}]}}`), nil
		case "tools/call":
			return response(200, `{"jsonrpc":"2.0","id":3,"result":{"content":[]}}`), nil
		}
		return nil, ErrTransport
	})
	if _, err := c.Call(context.Background(), agentA, c.Operation("status"), nil); err != nil || posts != 4 {
		t.Fatal("generic authenticated discovery/call failed")
	}
}

func TestTrustedMCPApprovalIdentityPinsCanonicalFullProfile(t *testing.T) {
	p := testProfile(t)
	s := signer(t, binding(agentA, sessionA))
	c, err := NewClient(s, p)
	if err != nil {
		t.Fatal("test connector unavailable")
	}
	identity := c.ApprovalIdentity()
	if len(identity) != 64 {
		t.Fatal("approval profile identity unavailable")
	}
	c.Profile.Endpoint = "http://127.0.0.1:19999/changed"
	c.Profile.Tools[0].Operation = "public_metadata_changed"
	if c.ApprovalIdentity() != identity {
		t.Fatal("public metadata changed approval identity")
	}
	raw, _ := json.MarshalIndent(p, "", "  ")
	canonical, err := ParseProfile(raw)
	if err != nil {
		t.Fatal("canonical test profile unavailable")
	}
	other, err := NewClient(s, canonical)
	if err != nil || other.ApprovalIdentity() != identity {
		t.Fatal("JSON whitespace changed profile identity")
	}
	p.Tools[0].Operation = "changed_remote_operation"
	changed, err := NewClient(s, p)
	if err != nil || changed.ApprovalIdentity() == identity {
		t.Fatal("changed operation retained prior approval identity")
	}
}

func TestTrustedMCPIntegerArgumentsPreserveExactSignedValuesAndRejectOverflow(t *testing.T) {
	for _, tc := range []struct {
		value    string
		expected string
	}{
		{"300e0", "300"},
		{"9007199254740993e0", "9007199254740993"},
		{"9.007199254740993e15", "9007199254740993"},
		{"9223372036854775807", "9223372036854775807"},
		{"-9223372036854775808", "-9223372036854775808"},
		{"9223372036854775808", ""},
		{"-9223372036854775809", ""},
		{"300.5", ""},
	} {
		t.Run(tc.value, func(t *testing.T) {
			p := testProfile(t)
			p.Tools = []ToolProfile{{Alias: "echo", Operation: "integer_echo", Description: "Read an exact integer.", ReadOnly: true, Schema: map[string]any{"type": "object", "properties": map[string]any{"value": map[string]any{"type": "integer"}}, "required": []string{"value"}, "additionalProperties": false}}}
			c, err := NewClient(signer(t, binding(agentA, sessionA)), p)
			if err != nil {
				t.Fatal("test integer connector unavailable")
			}
			c.Check = func(context.Context) error { return nil }
			posts := 0
			c.http.Transport = roundTrip(func(req *http.Request) (*http.Response, error) {
				posts++
				raw, _ := io.ReadAll(req.Body)
				var body struct {
					Method string `json:"method"`
					Params struct {
						Arguments map[string]json.RawMessage `json:"arguments"`
					} `json:"params"`
				}
				_ = json.Unmarshal(raw, &body)
				switch body.Method {
				case "initialize":
					return response(200, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-11-25"}}`), nil
				case "notifications/initialized":
					return response(202, ""), nil
				case "tools/list":
					return response(200, `{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"integer_echo","inputSchema":{"type":"object","properties":{"value":{"type":"integer"}},"required":["value"]}}]}}`), nil
				case "tools/call":
					if string(body.Params.Arguments["value"]) != tc.expected {
						t.Fatal("integer value rounded before remote call")
					}
					return response(200, `{"jsonrpc":"2.0","id":3,"result":{"content":[]}}`), nil
				}
				return nil, ErrTransport
			})
			_, err = c.Call(context.Background(), agentA, "integer_echo", map[string]any{"value": json.Number(tc.value)})
			if tc.expected == "" {
				if !errors.Is(err, ErrArguments) || posts != 0 {
					t.Fatal("invalid integer reached upstream")
				}
			} else if err != nil || posts != 4 {
				t.Fatal("exact signed integer refused")
			}
		})
	}
}
