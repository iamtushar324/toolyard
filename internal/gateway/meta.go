package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tusharbhardwaj/toolyard/internal/access"
	"github.com/tusharbhardwaj/toolyard/internal/actor"
	"github.com/tusharbhardwaj/toolyard/internal/codemode"
	"github.com/tusharbhardwaj/toolyard/internal/toolsearch"
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
		Name: MetaSearchTool,
		Description: descriptionBanner + "Search the toolyard catalog for the tools you may use, ranked. " +
			"mode \"keyword\" (default) ranks by BM25 over tool names (split at dots, underscores and camelCase), descriptions, parameter names and parameter descriptions: query \"create a linear issue\". " +
			"mode \"regex\" matches an RE2 pattern (at most 200 characters, case-insensitive unless the pattern turns that off) against the same fields: query \"^github\\\\..*pull\". " +
			"mode \"auto\" is keyword; \"semantic\" works only where an operator configured it. " +
			"detail \"summary\" (default) returns name, upstream, description, the code-mode name and score; \"name\" only names; \"full\" adds the input and output schemas. " +
			"Use it before tools.execute, or in code mode: JavaScript `await tools.search(\"query\", { limit: 5 })`, Starlark `toolyard.tools_search(query=\"...\")`.",
		InputSchema: mcp.ToolInputSchema{
			Type:     "object",
			Required: []string{ReasonField},
			Properties: addMetaProps(map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": "Words to rank by (keyword), or an RE2 pattern (regex). Empty lists every tool, by name.",
				},
				"mode": map[string]any{
					"type":        "string",
					"enum":        []string{string(toolsearch.ModeKeyword), string(toolsearch.ModeRegex), string(toolsearch.ModeSemantic), string(toolsearch.ModeAuto)},
					"description": "keyword (default), regex, semantic (only where configured) or auto (keyword).",
				},
				"detail": map[string]any{
					"type":        "string",
					"enum":        []string{string(toolsearch.DetailName), string(toolsearch.DetailSummary), string(toolsearch.DetailFull)},
					"description": "name, summary (default) or full (with input and output schemas).",
				},
				"upstream": map[string]any{
					"type":        "string",
					"description": "Optional: limit to tools from one upstream (e.g. 'github', 'builtin').",
				},
				"limit": map[string]any{
					"type":        "integer",
					"minimum":     1,
					"maximum":     toolsearch.MaxLimit,
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

// SearchRequest is one catalog search (tools.search, GET /v1/code/search).
type SearchRequest struct {
	Query    string
	Mode     string
	Detail   string
	Upstream string
	// Limit is 0 for the default (50); more than 200 is 200.
	Limit int
}

// SearchError is a request the search refuses (a bad mode, pattern or
// limit, or semantic search where none is configured).
type SearchError struct{ Message string }

func (e *SearchError) Error() string { return e.Message }

// SetToolSearch replaces the search engine (a semantic provider, say).
// Without it the gateway uses keyword and regex search.
func (g *Gateway) SetToolSearch(e *toolsearch.Engine) { g.search.Store(e) }

func (g *Gateway) toolSearch() *toolsearch.Engine {
	if e := g.search.Load(); e != nil {
		return e
	}
	g.search.CompareAndSwap(nil, toolsearch.New(toolsearch.Options{}))
	return g.search.Load()
}

// SearchTools ranks the tools the ctx caller may use (CatalogFor's scope)
// and returns the body tools.search answers with: total (results
// returned), matched (before the limit), mode and results. The meta-tools
// themselves are left out, so a model can't recurse tools.execute ->
// tools.execute.
func (g *Gateway) SearchTools(ctx context.Context, req SearchRequest) (map[string]any, error) {
	mode, err := toolsearch.ParseMode(req.Mode)
	if err != nil {
		return nil, &SearchError{Message: err.Error()}
	}
	detail, err := toolsearch.ParseDetail(req.Detail)
	if err != nil {
		return nil, &SearchError{Message: err.Error()}
	}
	if req.Limit < 0 {
		return nil, &SearchError{Message: "limit must be at least 1"}
	}
	res, err := g.toolSearch().Search(ctx, g.searchCatalog(ctx), toolsearch.Query{
		Text: req.Query, Mode: mode, Limit: req.Limit, Server: req.Upstream,
	})
	var qe *toolsearch.QueryError
	switch {
	case errors.Is(err, toolsearch.ErrSemanticOff), errors.As(err, &qe):
		return nil, &SearchError{Message: err.Error()}
	case err != nil:
		return nil, err
	}
	results := toolsearch.Render(res.Hits, detail)
	return map[string]any{
		"total":   len(results),
		"matched": res.Matched,
		"mode":    string(res.Mode),
		"results": results,
	}, nil
}

// searchCatalog is the ctx caller's tools as search sees them: the schema
// without the gateway's injected fields, the output schema, and the name a
// script calls each tool by.
func (g *Gateway) searchCatalog(ctx context.Context) []toolsearch.Tool {
	scope := g.scopeFor(ctx, agentIDFromContext(ctx))
	g.mu.RLock()
	entries := make([]toolEntry, 0, len(g.tools))
	for _, e := range g.tools {
		if e.tool.Name == MetaSearchTool || e.tool.Name == MetaExecuteTool {
			continue
		}
		if g.access != nil && !scope.All && !scope.AllowsTool(e.upstream, e.tool.Name) {
			continue
		}
		entries = append(entries, e)
	}
	g.mu.RUnlock()
	codeTools := make([]codemode.Tool, 0, len(entries))
	out := make([]toolsearch.Tool, 0, len(entries))
	for _, e := range entries {
		props, required := codeModeSchema(e)
		desc := strings.TrimPrefix(e.tool.Description, descriptionBanner)
		if !isCodeModeTool(e.tool.Name) {
			group := access.GroupOf(e.upstream, e.tool.Name)
			server, name := group, strings.TrimPrefix(e.tool.Name, group+".")
			if internalEntry(e) {
				server, name = toolyardServer, group+"_"+name
			}
			codeTools = append(codeTools, codemode.Tool{Server: server, Name: name, Target: e.tool.Name})
		}
		reqAny := make([]any, len(required))
		for i, r := range required {
			reqAny[i] = r
		}
		out = append(out, toolsearch.Tool{
			Name:         e.tool.Name,
			Server:       e.upstream,
			Description:  desc,
			InputSchema:  map[string]any{"type": "object", "properties": props, "required": reqAny},
			OutputSchema: outputSchemaOf(e.tool),
		})
	}
	names := codemode.CodeNames(codeTools)
	for i := range out {
		out[i].CodeName = names[out[i].Name]
	}
	return out
}

// handleSearchTools answers tools.search. Read-only: never holds for
// approval.
func (g *Gateway) handleSearchTools() directHandler {
	return func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		req := SearchRequest{}
		req.Query, _ = args["query"].(string)
		req.Mode, _ = args["mode"].(string)
		req.Detail, _ = args["detail"].(string)
		req.Upstream, _ = args["upstream"].(string)
		if v, ok := asInt(args["limit"]); ok && v > 0 {
			req.Limit = v
		}
		body, err := g.SearchTools(ctx, req)
		var se *SearchError
		switch {
		case errors.As(err, &se):
			return mcp.NewToolResultError(se.Message), nil
		case err != nil:
			return mcp.NewToolResultErrorFromErr("search", err), nil
		}
		text, err := json.MarshalIndent(body, "", "  ")
		if err != nil {
			return mcp.NewToolResultErrorFromErr("encode results", err), nil
		}
		res := mcp.NewToolResultText(string(text))
		res.StructuredContent = body
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
		// A script may reach tools.execute (toolyard.tools_execute) but
		// not code mode itself: a nested executeToolCode would only
		// recurse, one worker slot per level.
		if r, ok := actor.RaiserFrom(ctx); ok && r.Via == codemode.Via && isCodeModeTool(target) {
			return mcp.NewToolResultErrorf("%s cannot be called from code mode: call the tools you need directly in the script", target), nil
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
		// A top-level _approval_id never gets here: routeEntry resumes it
		// against the target (resumeViaExecute) and strips it from args.
		// One inside arguments reaches the target with the rest of inner.
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
