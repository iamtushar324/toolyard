package gateway

import (
	"context"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tusharbhardwaj/toolyard/internal/access"
	"github.com/tusharbhardwaj/toolyard/internal/codemode"
)

// Code-mode tool names. They are Bifrost's, letter for letter, with no
// group prefix: a client configured for the Bifrost MCP gateway calls them
// by these names and never sends _reason. They are registered under the
// always-on "tools" group so every caller has them, and they are always
// allowed themselves: every nested call a script makes runs the full
// pipeline (access, policy and approval, identity forwarding, audit and
// metrics) under via "code_mode".
const (
	CodeModeListToolFiles   = "listToolFiles"
	CodeModeReadToolFile    = "readToolFile"
	CodeModeGetToolDocs     = "getToolDocs"
	CodeModeExecuteToolCode = "executeToolCode"

	// codeModeGroup is the access group the code-mode tools live in, and
	// the one group a script cannot reach: tools.search, tools.execute, the
	// approval pollers and code mode itself would only recurse.
	codeModeGroup = "tools"
)

// CodeModeToolNames are always visible to agents.
var CodeModeToolNames = []string{CodeModeListToolFiles, CodeModeReadToolFile, CodeModeGetToolDocs, CodeModeExecuteToolCode}

func init() {
	for _, n := range CodeModeToolNames {
		PinnedTools[n] = struct{}{}
	}
}

// defaultCodeModeWorker is the process scripts run in: this binary's
// codemode-worker subcommand. Tests point it at the test binary.
var defaultCodeModeWorker = codemode.DefaultWorker

// SetCodeModeLimits replaces the limits one executeToolCode run gets. Call
// it before RegisterBuiltins so the executeToolCode description advertises
// the limits that are enforced; the handlers read the runtime through the
// gateway on every call either way.
func (g *Gateway) SetCodeModeLimits(l codemode.Limits) {
	worker := g.codeModeRuntime().Worker()
	g.codeMode = codemode.New(codeModeCaller{g}, l)
	g.codeMode.SetWorker(worker)
}

// SetCodeModeWorker replaces how scripts are run.
func (g *Gateway) SetCodeModeWorker(w codemode.Worker) {
	g.codeModeRuntime().SetWorker(w)
}

func (g *Gateway) codeModeRuntime() *codemode.Runtime {
	if g.codeMode == nil {
		g.codeMode = codemode.New(codeModeCaller{g}, codemode.DefaultLimits())
		g.codeMode.SetWorker(defaultCodeModeWorker())
	}
	return g.codeMode
}

// codeModeTools returns the four code-mode entries.
func (g *Gateway) codeModeTools() []toolEntry {
	rt := g.codeModeRuntime()
	optional := func(props map[string]any, required ...string) mcp.ToolInputSchema {
		return mcp.ToolInputSchema{Type: "object", Required: required, Properties: addMetaProps(props)}
	}
	list := mcp.Tool{
		Name: CodeModeListToolFiles,
		Description: "Returns a tree structure listing all virtual .pyi stub files available for the servers you may use, organized by individual tool. " +
			"Each tool has a corresponding file (servers/<serverName>/<toolName>.pyi) with its compact Python signature; the <toolName> in the filename is the exact identifier to call in executeToolCode. " +
			"Workflow: listToolFiles -> readToolFile -> (optional) getToolDocs -> executeToolCode. " +
			"In code, access tools via: server_name.tool_name(param=value). " +
			"CALL THIS TOOL FIRST whenever a server, tool or capability is not visible in your current tool list; do not tell the user something is unavailable until you have called listToolFiles and confirmed it is absent. " +
			"toolyard code mode: _reason is optional here; every call your code makes still goes through toolyard's access, policy and approval rules.",
		InputSchema: optional(map[string]any{}),
	}
	read := mcp.Tool{
		Name: CodeModeReadToolFile,
		Description: "Reads a virtual .pyi stub file for a specific tool (servers/<serverName>/<toolName>.pyi) or a whole server (servers/<serverName>.pyi), returning compact Python function signatures. " +
			"Matching is case-insensitive and the .pyi extension is optional. This is the authoritative source for the exact callable name and arguments to use in executeToolCode: serverName.tool_name(param=value). " +
			"If the compact signature is not enough, use getToolDocs. " +
			"IMPORTANT: if the response header shows 'Total lines: X (this is the complete file)', do NOT call this tool again with startLine/endLine.",
		InputSchema: optional(map[string]any{
			"fileName": map[string]any{
				"type":        "string",
				"description": "The virtual filename from listToolFiles, servers/<serverName>/<toolName>.pyi (e.g. 'servers/BkCoreServices/get_client.pyi').",
			},
			"startLine": map[string]any{
				"type":        "number",
				"description": "Optional 1-based starting line for a partial read. Usually not needed; files are small.",
			},
			"endLine": map[string]any{
				"type":        "number",
				"description": "Optional 1-based ending line for a partial read. Clamped to the file size.",
			},
		}, "fileName"),
	}
	docs := mcp.Tool{
		Name: CodeModeGetToolDocs,
		Description: "Get detailed documentation for a specific tool: full parameter descriptions, types and a usage example. " +
			"Use this when the compact signature from readToolFile is not sufficient. Requires both the server name and the tool name.",
		InputSchema: optional(map[string]any{
			"server": map[string]any{"type": "string", "description": "The server name (e.g. 'BkCoreServices'). Use listToolFiles to see available servers."},
			"tool":   map[string]any{"type": "string", "description": "The tool name (e.g. 'get_client'). Use readToolFile to see the tools of a server."},
		}, "server", "tool"),
	}
	exec := mcp.Tool{
		Name: CodeModeExecuteToolCode,
		Description: "Executes Python code in a sandboxed Starlark interpreter with tool access. Servers are global objects: result = serverName.toolName(param=\"value\"). " +
			"Final step of the workflow listToolFiles -> readToolFile -> (optional) getToolDocs -> executeToolCode; read a tool's .pyi stub before calling it and never guess callable names. " +
			"STARLARK DIFFERENCES FROM PYTHON: no try/except/finally/raise (a failed tool call aborts the script); no classes; no imports, network or filesystem; no `is`; no f-strings (use % formatting); " +
			"each call runs in a FRESH ISOLATED SCOPE with no state carried between calls. " +
			"SYNTAX: synchronous calls, keyword arguments (server.tool(param=\"value\")), dict access with brackets (result[\"key\"]), print() for logging, assign the value to return to `result`. " +
			"toolyard code mode: each tool call your code makes goes through toolyard's access, policy and approval rules; a call that is held for approval or needs a permission request aborts the script with the gateway's answer, so poll or ask as it says, then rerun. " +
			"Your code runs in an isolated process with no credentials and no network; it can only call tools. " +
			rt.LimitsText(),
		InputSchema: optional(map[string]any{
			"code": map[string]any{
				"type": "string",
				"description": "Python (Starlark) code to execute. Tool calls are synchronous: result = server.tool(param=\"value\"). " +
					"Use print() for logging. Assign to 'result' to return a value. Before rerunning code that already made tool calls, inspect prior output and avoid replaying stateful operations.",
			},
		}, "code"),
	}
	// Policy evaluates these like any tool: the "tools" group is allowed by
	// default, and an explicit tool-scope rule (deny, or ask to hold the
	// whole run for approval) still applies. executeToolCode keeps its own
	// clock: the per-dispatch upstream timeout would cut the advertised
	// script limit short, and every nested call gets that timeout through
	// its own dispatch anyway.
	entry := func(t mcp.Tool, h directHandler, ownClock bool) toolEntry {
		return toolEntry{
			tool: t, upstream: codeModeGroup, originalName: t.Name, reasonField: ReasonField,
			handle: h, reasonOptional: true, noCallTimeout: ownClock,
		}
	}
	return []toolEntry{
		entry(list, g.handleListToolFiles(), false),
		entry(read, g.handleReadToolFile(), false),
		entry(docs, g.handleGetToolDocs(), false),
		entry(exec, g.handleExecuteToolCode(), true),
	}
}

func (g *Gateway) handleListToolFiles() directHandler {
	return func(ctx context.Context, _ map[string]any) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText(g.codeModeRuntime().ListToolFiles(ctx)), nil
	}
}

func (g *Gateway) handleReadToolFile() directHandler {
	return func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		fileName, _ := args["fileName"].(string)
		if strings.TrimSpace(fileName) == "" {
			return mcp.NewToolResultError("fileName parameter is required and must be a string"), nil
		}
		text, ok := g.codeModeRuntime().ReadToolFile(ctx, fileName, intArg(args, "startLine"), intArg(args, "endLine"))
		res := mcp.NewToolResultText(text)
		res.IsError = !ok
		return res, nil
	}
}

func (g *Gateway) handleGetToolDocs() directHandler {
	return func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		server, _ := args["server"].(string)
		tool, _ := args["tool"].(string)
		if strings.TrimSpace(server) == "" {
			return mcp.NewToolResultError("server parameter is required and must be a string"), nil
		}
		if strings.TrimSpace(tool) == "" {
			return mcp.NewToolResultError("tool parameter is required and must be a string"), nil
		}
		text, ok := g.codeModeRuntime().GetToolDocs(ctx, server, tool)
		res := mcp.NewToolResultText(text)
		res.IsError = !ok
		return res, nil
	}
}

// handleExecuteToolCode runs the script. The outer _reason, when the
// client sent one, is on ctx (dispatch put it there); the runtime derives
// one from the script otherwise.
func (g *Gateway) handleExecuteToolCode() directHandler {
	return func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		code, _ := args["code"].(string)
		if strings.TrimSpace(code) == "" {
			return mcp.NewToolResultError("code parameter is required and must be a non-empty string"), nil
		}
		text, failed := g.codeModeRuntime().ExecuteToolCode(ctx, code, callReasonFromContext(ctx))
		res := mcp.NewToolResultText(text)
		res.IsError = failed
		return res, nil
	}
}

// intArg reads an optional integer argument however JSON or an in-process
// caller spelt it.
func intArg(args map[string]any, key string) *int {
	var n int
	switch v := args[key].(type) {
	case float64:
		n = int(v)
	case int:
		n = v
	case int64:
		n = int(v)
	default:
		return nil
	}
	return &n
}

// codeModeCaller is the runtime's view of the gateway: the caller's
// catalog, and RouteCall.
type codeModeCaller struct{ g *Gateway }

// Tools lists what the ctx caller may use, as code mode sees it: grouped
// by server key (the upstream name, or a built-in group such as memory),
// without the meta-tool group, and with the gateway's injected schema
// fields removed so stubs show the tool's own parameters.
func (c codeModeCaller) Tools(ctx context.Context) []codemode.Tool {
	g := c.g
	scope := g.scopeFor(ctx, agentIDFromContext(ctx))
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := make([]codemode.Tool, 0, len(g.tools))
	for _, e := range g.tools {
		group := access.GroupOf(e.upstream, e.tool.Name)
		if group == codeModeGroup || !scope.AllowsTool(e.upstream, e.tool.Name) {
			continue
		}
		props, required := codeModeSchema(e)
		out = append(out, codemode.Tool{
			Server:      group,
			Name:        strings.TrimPrefix(e.tool.Name, group+"."),
			Target:      e.tool.Name,
			Description: strings.TrimPrefix(e.tool.Description, descriptionBanner),
			Properties:  props,
			Required:    required,
			ReasonField: e.reasonField,
		})
	}
	return out
}

// RouteCall is the gateway's pipeline; via is codemode.Via.
func (c codeModeCaller) RouteCall(ctx context.Context, via, target string, args map[string]any) (*mcp.CallToolResult, error) {
	return c.g.RouteCall(ctx, via, target, args)
}

// codeModeSchema is a tool's input schema without the fields the gateway
// injects (its reason field, _intent_category, _approval_id, _grant,
// _session_id). An upstream's own _reason survives when the gateway had to
// fall back to __toolyard_reason.
func codeModeSchema(e toolEntry) (map[string]any, []string) {
	drop := map[string]bool{e.reasonField: true, IntentField: true, ApprovalIDField: true, GrantField: true, SessionField: true}
	props := make(map[string]any, len(e.tool.InputSchema.Properties))
	for k, v := range e.tool.InputSchema.Properties {
		if !drop[k] {
			props[k] = v
		}
	}
	required := make([]string, 0, len(e.tool.InputSchema.Required))
	for _, r := range e.tool.InputSchema.Required {
		if !drop[r] {
			required = append(required, r)
		}
	}
	return props, required
}
