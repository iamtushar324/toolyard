package trustedmcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestTrustedMCPDiscoveryFiltersAllowlistAndAcceptsActualSDKNullableArguments(t *testing.T) {
	c := newTestClient(t, signer(t, binding(agentA, sessionA)))
	posts := 0
	c.http.Transport = roundTrip(func(req *http.Request) (*http.Response, error) {
		posts++
		r := goodResponse(req)
		if posts != 3 {
			return r, nil
		}
		raw, _ := io.ReadAll(r.Body)
		var envelope map[string]any
		_ = json.Unmarshal(raw, &envelope)
		list := envelope["result"].(map[string]any)
		tools := list["tools"].([]any)
		for _, raw := range tools {
			tool := raw.(map[string]any)
			schema := tool["inputSchema"].(map[string]any)
			delete(schema, "additionalProperties")
			schema["title"] = "SDK parameter schema"
			for field, raw := range schema["properties"].(map[string]any) {
				property := raw.(map[string]any)
				for _, constraint := range []string{"pattern", "enum", "minimum", "maximum", "minLength", "maxLength"} {
					delete(property, constraint)
				}
				property["title"] = field
				if field == "job_id" {
					delete(property, "type")
					property["anyOf"] = []any{map[string]any{"type": "string"}, map[string]any{"type": "null"}}
					property["default"] = nil
				}
			}
		}
		list["tools"] = append(tools, map[string]any{"name": "unreviewed_remote_tool", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{}}})
		out, _ := json.Marshal(envelope)
		r.Body = io.NopCloser(bytes.NewReader(out))
		return r, nil
	})
	tools, err := c.Discover(context.Background(), agentA)
	if err != nil || posts != 3 || len(tools) != 6 {
		t.Fatal("authenticated SDK discovery failed")
	}
	for _, tool := range tools {
		if strings.HasPrefix(tool.Name, "preview_") || tool.Name == "unreviewed_remote_tool" {
			t.Fatal("unreviewed name escaped alias projection")
		}
	}
}

func TestTrustedMCPDiscoveryRejectsDuplicatesMissingToolsAndSchemaDriftBeforeCall(t *testing.T) {
	for _, mode := range []string{"duplicate", "missing", "identity-field", "changed-required", "wrong-type", "changed-default", "missing-default", "external-schema", "pagination"} {
		t.Run(mode, func(t *testing.T) {
			c := newTestClient(t, signer(t, binding(agentA, sessionA)))
			posts := 0
			c.http.Transport = roundTrip(func(req *http.Request) (*http.Response, error) {
				posts++
				r := goodResponse(req)
				if posts != 3 {
					return r, nil
				}
				raw, _ := io.ReadAll(r.Body)
				var envelope map[string]any
				_ = json.Unmarshal(raw, &envelope)
				list := envelope["result"].(map[string]any)
				tools := list["tools"].([]any)
				first := tools[0].(map[string]any)
				schema := first["inputSchema"].(map[string]any)
				properties := schema["properties"].(map[string]any)
				switch mode {
				case "duplicate":
					list["tools"] = append(tools, tools[0])
				case "missing":
					list["tools"] = tools[1:]
				case "identity-field":
					properties["session_id"] = map[string]any{"type": "string"}
				case "changed-required":
					schema["required"] = []string{"source_ref"}
				case "wrong-type":
					properties["source_ref"].(map[string]any)["type"] = "integer"
				case "changed-default":
					properties["pool"].(map[string]any)["default"] = "medium"
				case "missing-default":
					delete(properties["pool"].(map[string]any), "default")
				case "external-schema":
					schema["$ref"] = "https://public.invalid/schema"
				case "pagination":
					list["nextCursor"] = "unreviewed-extra-page"
				}
				out, _ := json.Marshal(envelope)
				r.Body = io.NopCloser(bytes.NewReader(out))
				return r, nil
			})
			result, err := c.Call(context.Background(), agentA, "preview_snapshots", nil)
			if !errors.Is(err, ErrTransport) || posts != 3 || len(result) != 0 {
				t.Fatal("unreviewed discovery reached tools/call")
			}
		})
	}
}

func TestTrustedMCPDiscoveryNeverUsesAnonymousCallerOrMissingCheck(t *testing.T) {
	c := newTestClient(t, signer(t, binding(agentA, sessionA)))
	posts := 0
	c.http.Transport = roundTrip(func(req *http.Request) (*http.Response, error) { posts++; return goodResponse(req), nil })
	if _, err := c.Discover(context.Background(), ""); !errors.Is(err, ErrIdentity) || posts != 0 {
		t.Fatal("anonymous discovery reached upstream")
	}
	c.Check = nil
	if _, err := c.Discover(context.Background(), agentA); !errors.Is(err, ErrIdentity) || posts != 0 {
		t.Fatal("unchecked discovery reached upstream")
	}
}

func TestTrustedMCPConfigurationRevocationBetweenPostsStopsSubmission(t *testing.T) {
	for _, after := range []int{0, 1, 2, 3} {
		c := newTestClient(t, signer(t, binding(agentA, sessionA)))
		posts := 0
		c.Check = func(context.Context) error {
			if posts >= after {
				return ErrIdentity
			}
			return nil
		}
		c.http.Transport = roundTrip(func(req *http.Request) (*http.Response, error) { posts++; return goodResponse(req), nil })
		if _, err := c.Call(context.Background(), agentA, "preview_snapshots", nil); !errors.Is(err, ErrIdentity) || posts != after {
			t.Fatal("revoked configuration continued HTTP submission")
		}
	}
}

func TestTrustedMCPDeadlineDuringInitializePreventsLaterPosts(t *testing.T) {
	c := newTestClient(t, signer(t, binding(agentA, sessionA)))
	posts := 0
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.http.Transport = roundTrip(func(req *http.Request) (*http.Response, error) { posts++; cancel(); return goodResponse(req), nil })
	if _, err := c.Call(ctx, agentA, "preview_snapshots", nil); !errors.Is(err, ErrTransport) || posts != 1 {
		t.Fatal("expired initialization continued to tools/call")
	}
}

type testTimeout struct{}

func (testTimeout) Error() string   { return "synthetic timeout diagnostic" }
func (testTimeout) Timeout() bool   { return true }
func (testTimeout) Temporary() bool { return true }

func TestTrustedMCPToolTimeoutHasUnknownOutcomeAndNeverRetries(t *testing.T) {
	for _, timeoutPost := range []int{1, 4} {
		c := newTestClient(t, signer(t, binding(agentA, sessionA)))
		posts := 0
		c.http.Transport = roundTrip(func(req *http.Request) (*http.Response, error) {
			posts++
			if posts == timeoutPost {
				return nil, testTimeout{}
			}
			return goodResponse(req), nil
		})
		_, err := c.Call(context.Background(), agentA, "preview_snapshots", nil)
		want := ErrTransport
		if timeoutPost == 4 {
			want = ErrUnknownOutcome
		}
		if !errors.Is(err, want) || posts != timeoutPost || SafeError(err) != want.Error() {
			t.Fatal("timeout outcome or no-retry contract changed")
		}
	}
}
