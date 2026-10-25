package gateway

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tusharbhardwaj/toolyard/internal/access"
	"github.com/tusharbhardwaj/toolyard/internal/actor"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
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

	// codeModeGroup is the access group the code-mode tools live in. A
	// script reaches the group's other tools (toolyard.tools_search,
	// toolyard.tools_execute, the approval pollers) but never the four
	// code-mode tools themselves, which would only recurse.
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

// SetCodeModeConcurrency sets how many scripts run at once in all
// (-codemode-max-concurrent) and for one caller (-codemode-max-per-agent),
// and how long a script past either waits for a slot
// (-codemode-queue-wait); 0 keeps a setting's default (4, 3 and 30s).
// Call it at start, before any script runs.
func (g *Gateway) SetCodeModeConcurrency(maxConcurrent, maxPerAgent int, queueWait time.Duration) {
	w := g.codeModeRuntime().Worker()
	if maxConcurrent > 0 {
		w.MaxConcurrent = maxConcurrent
	}
	if maxPerAgent > 0 {
		w.MaxPerAgent = maxPerAgent
	}
	if queueWait > 0 {
		w.AcquireWait = queueWait
	}
	g.SetCodeModeWorker(w)
}

// withCodeModeRunID gives an executeToolCode call its run id before any of
// its audit rows is written, so the outer call's rows and every nested
// call's share it (audit_events.run_id).
func withCodeModeRunID(ctx context.Context, entry toolEntry) context.Context {
	if entry.tool.Name != CodeModeExecuteToolCode || audit.RunIDFrom(ctx) != "" {
		return ctx
	}
	return audit.WithRunID(ctx, codemode.NewRunID())
}

// SetCodeModeSandbox changes only how script workers are confined. Call
// it at start, before any script runs.
func (g *Gateway) SetCodeModeSandbox(m codemode.SandboxMode) {
	w := g.codeModeRuntime().Worker()
	w.Sandbox = m
	g.SetCodeModeWorker(w)
}

func (g *Gateway) codeModeRuntime() *codemode.Runtime {
	if g.codeMode == nil {
		g.codeMode = codemode.New(codeModeCaller{g}, codemode.DefaultLimits())
		g.codeMode.SetWorker(defaultCodeModeWorker())
	}
	return g.codeMode
}

// CodeMode returns the code-mode runtime the MCP tools use. The REST code
// routes run scripts with its Run (taking a slot first with AcquireSlot
// to answer busy) and render stubs with its ListToolFiles, ReadToolFile
// and GetToolDocs, on a context from CodeModeContext. Call it for each
// run rather than keeping the result: SetCodeModeLimits replaces the
// runtime.
func (g *Gateway) CodeMode() *codemode.Runtime { return g.codeModeRuntime() }

// CodeModeContext is the context a code-mode run made outside MCP (the
// REST code route) runs on. agentID is the caller every nested call is
// scoped, checked and audited as. sessionID, when not "", is the verified
// client session id their audit rows carry (a session token's own id,
// not a caller's claim). The run is admitted past a drain the way an MCP
// executeToolCode call is, so a drain that begins half-way does not
// refuse the script's later calls after earlier writes ran. ok is false
// while the gateway drains: start no new run then. Build it from a
// context that outlives the HTTP request when the run continues after
// the response.
func (g *Gateway) CodeModeContext(ctx context.Context, agentID, sessionID string) (context.Context, bool) {
	if g.Draining() {
		return ctx, false
	}
	ctx = WithAgentID(ctx, agentID)
	if sessionID != "" {
		r, _ := actor.RaiserFrom(ctx)
		r.ClientSessionID, r.ClientSessionClaimed = sessionID, false
		ctx = actor.WithRaiser(ctx, r)
	}
	return withDrainAdmitted(ctx), true
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
			"toolyard code mode: _reason is optional here; every call your code makes still goes through toolyard's access, policy and approval rules. " +
			"toolyard's own tools (inbox.*, callbacks.*, jobs.*, memory.*, events.*, session.*, tools.*, policies.*, servers.*, audit.*, access.*) are one server, toolyard, called as toolyard.<group>_<name>(...), e.g. toolyard.inbox_request(...) or toolyard.policies_explain(tool=\"github.create_issue\"). " +
			"For JavaScript scripts pass language=\"javascript\": the tree lists TypeScript stubs instead (servers/<serverName>/<toolName>.d.ts, and servers/index.d.ts for the tools object). " +
			"To find a tool by what it does, call toolyard's tools.search (ranked keyword or regex search) instead of reading every stub.",
		InputSchema: optional(map[string]any{
			"language": map[string]any{
				"type":        "string",
				"enum":        []string{codemode.LanguageStarlark, codemode.LanguageJavaScript},
				"description": "Which stubs to list: starlark (default, .pyi) or javascript (.d.ts).",
			},
		}),
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
				"description": "The virtual filename from listToolFiles, servers/<serverName>/<toolName>.pyi (e.g. 'servers/BkCoreServices/get_client.pyi'), or a TypeScript stub for JavaScript: servers/<serverName>/<toolName>.d.ts, servers/<serverName>.d.ts or servers/index.d.ts.",
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
			"STARLARK DIFFERENCES FROM PYTHON: no try/except/finally/raise (a failed tool call aborts the script; use try_call(server.tool, param=value) to get {ok, status, result | error} instead); no classes; no imports, network or filesystem; no `is`; no f-strings (use % formatting); " +
			"each call runs in a FRESH ISOLATED SCOPE with no state carried between calls. " +
			"SYNTAX: synchronous calls, keyword arguments (server.tool(param=\"value\")), dict access with brackets (result[\"key\"]), print() for logging, assign the value to return to `result`. " +
			"parallel([(server.tool, {\"param\": 1}), (\"server.other\", {})], limit=4) makes several calls at once and returns their try_call results in order. " +
			"JAVASCRIPT: pass language=\"javascript\" to run JavaScript (QuickJS, ES2023) instead; the code is the body of an async function: " +
			"const hits = await tools.search(\"create a linear issue\", { limit: 3 }); " +
			"const issue = await tools.Linear.get_issue({ id: \"TEC-1\" }); " +
			"const r = await tools.tryCall(\"Linear.save_issue\", { title: \"x\" }); // {ok, status, result | error}, never throws; " +
			"const all = await tools.parallel([() => tools.A.x({}), () => tools.B.y({})], { limit: 4 }); " +
			"console.log(...) prints; return { summary } returns. Results are the tool's structuredContent when it sends one, else its JSON text parsed, else the text. Read the .d.ts stubs (listToolFiles with language=\"javascript\") first. " +
			"toolyard code mode: each tool call your code makes goes through toolyard's access, policy and approval rules; a call that is held for approval or needs a permission request aborts a Starlark script (and rejects in JavaScript) with the gateway's answer, so poll or ask as it says, then rerun. " +
			"echo=\"preview\" or \"none\" keeps tool answers out of the text (only the first 500 bytes, or nothing) and lists the calls instead; the default \"full\" echoes each answer as a [TOOL] line. " +
			"Your code runs in an isolated process with no credentials and no network; it can only call tools. " +
			rt.LimitsText(),
		InputSchema: optional(map[string]any{
			"code": map[string]any{
				"type": "string",
				"description": "Python (Starlark) code to execute, or JavaScript with language=\"javascript\". Starlark tool calls are synchronous: result = server.tool(param=\"value\"). " +
					"Use print() for logging. Assign to 'result' to return a value. In JavaScript await tools.Server.tool({param: value}), console.log() and return. " +
					"Before rerunning code that already made tool calls, inspect prior output and avoid replaying stateful operations.",
			},
			"language": map[string]any{
				"type":        "string",
				"enum":        []string{codemode.LanguageStarlark, codemode.LanguageJavaScript},
				"description": "starlark (default) or javascript.",
			},
			"echo": map[string]any{
				"type":        "string",
				"enum":        []string{codemode.EchoFull, codemode.EchoPreview, codemode.EchoNone},
				"description": "How much of each tool answer the output repeats as a [TOOL] line: full (default), preview (the first 500 bytes) or none. The call ledger (tool, status, ms) is in the output with preview and none.",
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
	return func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		raw, _ := args["language"].(string)
		lang, err := codemode.ParseLanguage(raw)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultText(g.codeModeRuntime().ListFiles(ctx, lang)), nil
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
// one from the script otherwise. The text is the runtime's; the result's
// _meta carries the run id (also on every audit row of the run) and the
// call ledger, which clients that read only the text never see.
func (g *Gateway) handleExecuteToolCode() directHandler {
	return func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		code, _ := args["code"].(string)
		if strings.TrimSpace(code) == "" {
			return mcp.NewToolResultError("code parameter is required and must be a non-empty string"), nil
		}
		lang, _ := args["language"].(string)
		echo, _ := args["echo"].(string)
		runID := audit.RunIDFrom(ctx)
		if runID == "" {
			runID = codemode.NewRunID()
			ctx = audit.WithRunID(ctx, runID)
		}
		out := g.codeModeRuntime().Execute(ctx, code, codemode.ExecOptions{
			Reason: callReasonFromContext(ctx), Language: lang, Echo: echo,
		})
		return codeModeResult(out, runID), nil
	}
}

// codeModeResult is executeToolCode's answer for one run: the text, and in
// _meta the run id, the call ledger, the run's status and error kind. A
// run whose outcome is unknown (a call was cut short, or the script was
// interrupted after it sent one) also carries the toolyard.outcome_unknown
// flag and structured content {"status": "outcome_unknown", "error_kind",
// "run_id"}, which is how an async job built on executeToolCode tells it
// from a plain failure; other runs keep text only, as Bifrost answers.
func codeModeResult(out codemode.ExecResult, runID string) *mcp.CallToolResult {
	res := mcp.NewToolResultText(out.Text)
	res.IsError = out.Failed
	calls := out.Calls
	if calls == nil {
		calls = []codemode.LedgerEntry{}
	}
	meta := map[string]any{"toolyard.run_id": runID, "toolyard.calls": calls, "toolyard.status": out.Status}
	if out.ErrorKind != "" {
		meta["toolyard.error_kind"] = out.ErrorKind
	}
	if out.Status == codemode.StatusOutcomeUnknown {
		meta[codemode.OutcomeUnknownMarker] = true
		res.StructuredContent = map[string]any{"status": codemode.StatusOutcomeUnknown, "error_kind": out.ErrorKind, "run_id": runID}
	}
	res.Meta = &mcp.Meta{AdditionalFields: meta}
	return res
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

// toolyardServer is the one code-mode server every internal toolyard tool
// is bound under, as toolyard.<group>_<name>: toolyard.inbox_request,
// toolyard.memory_get, toolyard.tools_poll_approval, toolyard.policies_set.
// Upstream servers keep their own key (BkCoreServices.get_client). The
// binding is derived from the registered entries, so an internal tool
// added later appears without a change here.
const toolyardServer = "toolyard"

// internalEntry reports whether e is a toolyard tool rather than an
// upstream's: it is registered under one of the gateway's reserved
// synthetic upstreams. The fixture echo tool is left out (it stands in
// for an upstream in tests).
func internalEntry(e toolEntry) bool {
	return e.upstream != "fixture" && reservedUpstreamName(e.upstream)
}

// isCodeModeTool reports whether name is one of the four code-mode tools.
func isCodeModeTool(name string) bool {
	for _, n := range CodeModeToolNames {
		if n == name {
			return true
		}
	}
	return false
}

// Tools lists what the ctx caller may use, as code mode sees it: upstream
// tools under their server key (the upstream name), every internal tool
// under toolyardServer as <group>_<name>, and the gateway's injected
// schema fields removed so stubs show the tool's own parameters. The
// code-mode tools themselves are the one thing a script cannot reach:
// they would only recurse.
func (c codeModeCaller) Tools(ctx context.Context) []codemode.Tool {
	g := c.g
	scope := g.scopeFor(ctx, agentIDFromContext(ctx))
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := make([]codemode.Tool, 0, len(g.tools))
	for _, e := range g.tools {
		if isCodeModeTool(e.tool.Name) || !scope.AllowsTool(e.upstream, e.tool.Name) {
			continue
		}
		group := access.GroupOf(e.upstream, e.tool.Name)
		server, name := group, strings.TrimPrefix(e.tool.Name, group+".")
		if internalEntry(e) {
			server, name = toolyardServer, group+"_"+name
		}
		props, required := codeModeSchema(e)
		out = append(out, codemode.Tool{
			Server:       server,
			Name:         name,
			Target:       e.tool.Name,
			Description:  strings.TrimPrefix(e.tool.Description, descriptionBanner),
			Properties:   props,
			Required:     required,
			OutputSchema: outputSchemaOf(e.tool),
			ReasonField:  e.reasonField,
		})
	}
	return out
}

// AgentKey names the ctx caller for code mode's per-caller script cap.
func (c codeModeCaller) AgentKey(ctx context.Context) string { return agentIDFromContext(ctx) }

// outputSchemaOf is a tool's declared output schema as a map, nil when it
// declares none.
func outputSchemaOf(t mcp.Tool) map[string]any {
	raw := []byte(t.RawOutputSchema)
	if len(raw) == 0 {
		if t.OutputSchema.Type == "" && len(t.OutputSchema.Properties) == 0 {
			return nil
		}
		var err error
		if raw, err = json.Marshal(t.OutputSchema); err != nil {
			return nil
		}
	}
	var out map[string]any
	if json.Unmarshal(raw, &out) != nil || len(out) == 0 {
		return nil
	}
	return out
}

// RouteCall is the gateway's pipeline; via is codemode.Via. The context
// is marked as a script's, and the mark stays on every hop below (a
// script's tools.execute, and anything that routes on from there), so
// routeEntry refuses the code-mode tools however a script reaches them.
func (c codeModeCaller) RouteCall(ctx context.Context, via, target string, args map[string]any) (*mcp.CallToolResult, error) {
	return c.g.RouteCall(withinCodeMode(ctx), via, target, args)
}

// withinCodeMode marks ctx as a code-mode script's nested call
// (codemode.WithinScript: work that moves to a context of its own keeps
// the mark with codemode.CarryScriptMark).
func withinCodeMode(ctx context.Context) context.Context { return codemode.WithinScript(ctx) }

// isWithinCodeMode reports a context withinCodeMode marked.
func isWithinCodeMode(ctx context.Context) bool { return codemode.IsWithinScript(ctx) }

// refuseNestedCodeMode answers a code-mode tool called from inside a
// script, at any depth, with a refusal (nil otherwise): a nested script
// would only recurse, one worker slot per level.
func refuseNestedCodeMode(ctx context.Context, entry toolEntry) *mcp.CallToolResult {
	if !isWithinCodeMode(ctx) || !isCodeModeTool(entry.tool.Name) {
		return nil
	}
	return mcp.NewToolResultErrorf("%s cannot be called from code mode: call the tools you need directly in the script", entry.tool.Name)
}

// codeModeSchema is a tool's input schema without the fields the gateway
// injects (its reason field, _intent_category, _approval_id, _grant,
// _session_id, and _callback_ref unless the tool declares its own). An
// upstream's own _reason survives when the gateway had to
// fall back to __toolyard_reason.
func codeModeSchema(e toolEntry) (map[string]any, []string) {
	drop := map[string]bool{e.reasonField: true, IntentField: true, ApprovalIDField: true, GrantField: true, SessionField: true, callbackRefArg: !ownCallbackRef(e)}
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
