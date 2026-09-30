package gateway

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

// Meta-tool names. They live under the synthetic upstream "tools" so the
// gateway's reserved-name check rejects user-added upstreams of the same name.
const (
	MetaSearchTool  = "tools.search"
	MetaExecuteTool = "tools.execute"
)

// metaTools returns the search/execute meta-tools as toolEntry values ready to
// register on the MCP server.
func (g *Gateway) metaTools() []toolEntry {
	search := mcp.Tool{
		Name:        MetaSearchTool,
		Description: descriptionBanner + "Search the toolyard catalog. Returns matching tools with their wrapped input schemas. Use this to discover what tools are available before calling tools.execute.",
		InputSchema: mcp.ToolInputSchema{
			Type:     "object",
			Required: []string{ReasonField},
			Properties: addMetaProps(map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": "Substring matched (case-insensitive) against tool name and description. Empty matches all.",
				},
				"upstream": map[string]any{
					"type":        "string",
					"description": "Optional: limit to tools from one upstream (e.g. 'github', 'builtin').",
				},
				"limit": map[string]any{
					"type":        "integer",
					"minimum":     1,
					"maximum":     200,
					"description": "Max results (default 50).",
				},
			}),
		},
	}

	execute := mcp.Tool{
		Name:        MetaExecuteTool,
		Description: descriptionBanner + "Proxy a call to any registered tool. The target tool's policy/approval still applies, so writes will hold for human approval just like a direct call.",
		InputSchema: mcp.ToolInputSchema{
			Type:     "object",
			Required: []string{ReasonField, "tool"},
			Properties: addMetaProps(map[string]any{
				"tool": map[string]any{
					"type":        "string",
					"description": "Catalog name of the target tool (as returned by tools.search).",
				},
				"arguments": map[string]any{
					"type":        "object",
					"description": "Arguments forwarded to the target tool. _reason on the outer call propagates if not specified here.",
				},
			}),
		},
	}

	return []toolEntry{
		{tool: search, upstream: "tools", originalName: "search",
			reasonField: ReasonField, handle: g.handleSearchTools()},
		{tool: execute, upstream: "tools", originalName: "execute",
			reasonField: ReasonField, handle: g.handleExecuteTool()},
	}
}

// handleSearchTools matches against tool name + description, oldest-loaded
// first then alphabetical. Read-only — never holds for approval.
func (g *Gateway) handleSearchTools() directHandler {
	return func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		q, _ := args["query"].(string)
		limitedTo, _ := args["upstream"].(string)
		limit := 50
		if v, ok := args["limit"].(float64); ok && int(v) > 0 {
			limit = int(v)
		}
		needle := strings.ToLower(strings.TrimSpace(q))

		// Only the tools this caller may use: a member searching for a
		// server they were never granted finds nothing, as if it weren't there.
		entries := g.CatalogFor(ctx)
		// Hide the meta-tools themselves from search results so the model can't
		// recursively call tools.execute -> tools.execute.
		filtered := entries[:0]
		for _, e := range entries {
			if e.Name == MetaSearchTool || e.Name == MetaExecuteTool {
				continue
			}
			if limitedTo != "" && e.Upstream != limitedTo {
				continue
			}
			if needle != "" {
				hay := strings.ToLower(e.Name + " " + e.Description)
				if !strings.Contains(hay, needle) {
					continue
				}
			}
			filtered = append(filtered, e)
		}
		sort.Slice(filtered, func(i, j int) bool { return filtered[i].Name < filtered[j].Name })
		if len(filtered) > limit {
			filtered = filtered[:limit]
		}

		body, err := json.MarshalIndent(map[string]any{
			"total":   len(filtered),
			"results": filtered,
		}, "", "  ")
		if err != nil {
			return mcp.NewToolResultErrorFromErr("encode results", err), nil
		}
		res := mcp.NewToolResultText(string(body))
		res.StructuredContent = map[string]any{
			"total":   len(filtered),
			"results": filtered,
		}
		return res, nil
	}
}

// handleExecuteTool proxies a call to any registered tool through the same
// routing path as a direct MCP call. The target tool's _reason is set from
// either an inner `_reason` in arguments or, if missing, the outer one.
func (g *Gateway) handleExecuteTool() directHandler {
	return func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		target, _ := args["tool"].(string)
		if strings.TrimSpace(target) == "" {
			return mcp.NewToolResultError("tool is required"), nil
		}
		inner, _ := args["arguments"].(map[string]any)
		if inner == nil {
			inner = map[string]any{}
		}
		// Forward the outer reason if the model didn't supply one for the
		// inner call. routeEntry stripped it from args before this handler
		// ran, so it is read back off the context. The router validates
		// length either way.
		if _, ok := inner["_reason"]; !ok {
			inner["_reason"] = callReasonFromContext(ctx)
		}
		if apID, ok := args["_approval_id"].(string); ok && apID != "" {
			inner["_approval_id"] = apID
		}
		return g.RouteCall(ctx, MetaExecuteTool, target, inner)
	}
}

// callReasonKey carries a call's validated _reason from dispatch to its
// handler.
type callReasonKey struct{}

func withCallReason(ctx context.Context, reason string) context.Context {
	return context.WithValue(ctx, callReasonKey{}, reason)
}

// callReasonFromContext returns the reason the current call was made with,
// or "" when it had none (a code-mode call from a Bifrost client).
func callReasonFromContext(ctx context.Context) string {
	r, _ := ctx.Value(callReasonKey{}).(string)
	return r
}
