package gateway

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tusharbhardwaj/toolyard/internal/access"
)

// SmallToolList is every tool an agent on the small tool list (the
// default, access.ToolListSmall) sees in tools/list: find a tool with
// tools.search, read its schema with getToolDocs, run it inside
// executeToolCode, and ask for permission with inbox.request and read the
// decision with inbox.status. Every other tool the agent may use stays
// callable (tools/call, tools.execute, code mode); only the list is
// shorter. An owner switches one agent to the full list in its scope
// (access.AgentScope.ToolList).
var SmallToolList = []string{MetaSearchTool, CodeModeGetToolDocs, CodeModeExecuteToolCode, "inbox.request", "inbox.status"}

// smallToolListSet is SmallToolList as a set.
var smallToolListSet = func() map[string]bool {
	out := make(map[string]bool, len(SmallToolList))
	for _, n := range SmallToolList {
		out[n] = true
	}
	return out
}()

// ToolListResolver is the optional part of an access.Resolver that says
// which tool list a caller's tools/list shows (access.ToolListSmall or
// access.ToolListFull). *access.Service implements it: an agent's own
// setting, and the full list for every caller without an agent scope. A
// resolver without it (or no resolver) lists every tool, as before.
type ToolListResolver interface {
	ToolListFor(ctx context.Context, callerID string) string
}

// toolListFilter keeps only the small tool list for a caller whose
// resolver says small. It runs after the access filter, so it only ever
// drops tools: one of the five that the caller's scope removed stays
// removed.
func (g *Gateway) toolListFilter(ctx context.Context, tools []mcp.Tool) []mcp.Tool {
	r, ok := g.access.(ToolListResolver)
	if !ok || r.ToolListFor(ctx, agentIDFromContext(ctx)) != access.ToolListSmall {
		return tools
	}
	out := make([]mcp.Tool, 0, len(SmallToolList))
	for _, t := range tools {
		if smallToolListSet[t.Name] {
			out = append(out, t)
		}
	}
	return out
}
