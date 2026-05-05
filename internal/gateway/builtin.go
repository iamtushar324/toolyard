package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tusharbhardwaj/toolyard/internal/memory"
)

// builtinMemoryTools returns the four memory.* tools as MCP-style ServerTools
// (with ALREADY-WRAPPED schemas — they're built-in so we avoid round-tripping
// through the schema rewriter).
func (g *Gateway) builtinMemoryTools() []toolEntry {
	mem := g.memory

	get := mcp.Tool{
		Name:        "memory.get",
		Description: descriptionBanner + "Read a value from toolyard shared memory by key.",
		InputSchema: mcp.ToolInputSchema{
			Type:     "object",
			Required: []string{ReasonField, "key"},
			Properties: map[string]any{
				ReasonField: map[string]any{
					"type": "string", "minLength": minReasonLen, "maxLength": maxReasonLen,
					"description": reasonPropDescription,
				},
				IntentField: map[string]any{
					"type": "string", "enum": intentEnum, "description": intentPropDescription,
				},
				"scope": map[string]any{"type": "string"},
				"key":   map[string]any{"type": "string"},
			},
		},
	}

	set := mcp.Tool{
		Name:        "memory.set",
		Description: descriptionBanner + "Write a value to toolyard shared memory.",
		InputSchema: mcp.ToolInputSchema{
			Type:     "object",
			Required: []string{ReasonField, "key", "value"},
			Properties: map[string]any{
				ReasonField: map[string]any{
					"type": "string", "minLength": minReasonLen, "maxLength": maxReasonLen,
					"description": reasonPropDescription,
				},
				IntentField: map[string]any{
					"type": "string", "enum": intentEnum, "description": intentPropDescription,
				},
				"scope": map[string]any{"type": "string"},
				"key":   map[string]any{"type": "string"},
				"value": map[string]any{"type": "string"},
			},
		},
	}

	list := mcp.Tool{
		Name:        "memory.list",
		Description: descriptionBanner + "List keys in toolyard shared memory matching a prefix.",
		InputSchema: mcp.ToolInputSchema{
			Type:     "object",
			Required: []string{ReasonField},
			Properties: map[string]any{
				ReasonField: map[string]any{
					"type": "string", "minLength": minReasonLen, "maxLength": maxReasonLen,
					"description": reasonPropDescription,
				},
				IntentField: map[string]any{
					"type": "string", "enum": intentEnum, "description": intentPropDescription,
				},
				"scope":  map[string]any{"type": "string"},
				"prefix": map[string]any{"type": "string"},
			},
		},
	}

	del := mcp.Tool{
		Name:        "memory.delete",
		Description: descriptionBanner + "Delete a key from toolyard shared memory.",
		InputSchema: mcp.ToolInputSchema{
			Type:     "object",
			Required: []string{ReasonField, "key"},
			Properties: map[string]any{
				ReasonField: map[string]any{
					"type": "string", "minLength": minReasonLen, "maxLength": maxReasonLen,
					"description": reasonPropDescription,
				},
				IntentField: map[string]any{
					"type": "string", "enum": intentEnum, "description": intentPropDescription,
				},
				"scope": map[string]any{"type": "string"},
				"key":   map[string]any{"type": "string"},
			},
		},
	}

	return []toolEntry{
		{tool: get, upstream: builtinUpstream, originalName: "memory.get", reasonField: ReasonField,
			handle: handleMemoryGet(mem)},
		{tool: set, upstream: builtinUpstream, originalName: "memory.set", reasonField: ReasonField,
			handle: handleMemorySet(mem)},
		{tool: list, upstream: builtinUpstream, originalName: "memory.list", reasonField: ReasonField,
			handle: handleMemoryList(mem)},
		{tool: del, upstream: builtinUpstream, originalName: "memory.delete", reasonField: ReasonField,
			handle: handleMemoryDelete(mem)},
	}
}

func handleMemoryGet(mem *memory.Service) directHandler {
	return func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		key, _ := args["key"].(string)
		scope, _ := args["scope"].(string)
		if strings.TrimSpace(key) == "" {
			return mcp.NewToolResultError("key is required"), nil
		}
		e, err := mem.Get(ctx, scope, key)
		if err != nil {
			return mcp.NewToolResultError("not found: " + err.Error()), nil
		}
		body, _ := json.Marshal(e)
		return mcp.NewToolResultText(string(body)), nil
	}
}

func handleMemorySet(mem *memory.Service) directHandler {
	return func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		key, _ := args["key"].(string)
		val, _ := args["value"].(string)
		scope, _ := args["scope"].(string)
		if strings.TrimSpace(key) == "" {
			return mcp.NewToolResultError("key is required"), nil
		}
		e, err := mem.Set(ctx, scope, key, val)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("memory.set failed", err), nil
		}
		body, _ := json.Marshal(e)
		return mcp.NewToolResultText(string(body)), nil
	}
}

func handleMemoryList(mem *memory.Service) directHandler {
	return func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		prefix, _ := args["prefix"].(string)
		scope, _ := args["scope"].(string)
		entries, err := mem.List(ctx, scope, prefix)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("memory.list failed", err), nil
		}
		body, _ := json.Marshal(entries)
		return mcp.NewToolResultText(string(body)), nil
	}
}

func handleMemoryDelete(mem *memory.Service) directHandler {
	return func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		key, _ := args["key"].(string)
		scope, _ := args["scope"].(string)
		if strings.TrimSpace(key) == "" {
			return mcp.NewToolResultError("key is required"), nil
		}
		if err := mem.Delete(ctx, scope, key); err != nil {
			return mcp.NewToolResultErrorFromErr("memory.delete failed", err), nil
		}
		return mcp.NewToolResultText("ok"), nil
	}
}

// staticEchoTool is a deterministic upstream stand-in we register so the
// gateway has a non-builtin "upstream" to demonstrate the schema-wrap rewrite
// path in tests / first-run.
func (g *Gateway) staticFixtureTool() toolEntry {
	t := mcp.Tool{
		Name:        "fixture.echo",
		Description: descriptionBanner + "Fixture upstream: echoes back the message you provide. Useful for end-to-end gateway tests.",
		InputSchema: mcp.ToolInputSchema{
			Type:     "object",
			Required: []string{ReasonField, "message"},
			Properties: map[string]any{
				ReasonField: map[string]any{
					"type": "string", "minLength": minReasonLen, "maxLength": maxReasonLen,
					"description": reasonPropDescription,
				},
				IntentField: map[string]any{
					"type": "string", "enum": intentEnum, "description": intentPropDescription,
				},
				"message": map[string]any{"type": "string"},
			},
		},
	}
	handler := directHandler(func(_ context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		msg, _ := args["message"].(string)
		if msg == "" {
			return nil, errors.New("message required")
		}
		return mcp.NewToolResultText("echo: " + msg), nil
	})
	return toolEntry{tool: t, upstream: "fixture", originalName: "fixture.echo",
		reasonField: ReasonField, handle: handler}
}
