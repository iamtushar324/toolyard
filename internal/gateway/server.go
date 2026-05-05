package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/tusharbhardwaj/toolyard/internal/approval"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/memory"
	"github.com/tusharbhardwaj/toolyard/internal/policy"
	"github.com/tusharbhardwaj/toolyard/internal/realtime"
)

const (
	builtinUpstream    = "builtin"
	defaultInLineWait  = 30 * time.Second
	deferredRetryAfter = 60
)

// ErrUpstreamNotFound is returned when an upstream is referenced by name but
// is not registered.
var ErrUpstreamNotFound = errors.New("upstream not found")

// PinnedTools are always exposed to agents regardless of surface_mode. The
// list intentionally includes the meta-tools (so the agent can always
// discover and proxy) and the built-in memory tools (so memory stays usable
// as a baseline shared scratchpad). The set is hardcoded — there is no UI
// for removing entries.
var PinnedTools = map[string]struct{}{
	"tools.search":  {},
	"tools.execute": {},
	"memory.get":    {},
	"memory.set":    {},
	"memory.list":   {},
	"memory.delete": {},
}

// IsPinned reports whether toolName is in the always-visible set.
func IsPinned(toolName string) bool {
	_, ok := PinnedTools[toolName]
	return ok
}

// directHandler is the in-process call handler used by built-in tools and the
// fixture upstream. Upstream MCP-server-backed tools have a nil directHandler
// and instead route through the gateway's upstream pool.
type directHandler func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error)

// toolEntry is what the gateway tracks for each registered tool.
type toolEntry struct {
	tool         mcp.Tool
	upstream     string // "builtin", "fixture", or upstream config name
	originalName string // upstream-side name (without prefix)
	reasonField  string // "_reason" (or "__toolyard_reason" if a clash forced a rename)
	handle       directHandler
}

// Gateway stitches the MCP server, policy, approval bus, memory, and upstream
// pool into one coordinated unit.
type Gateway struct {
	mcp        *server.MCPServer
	policy     *policy.Engine
	approval   *approval.Bus
	audit      *audit.Logger
	hub        *realtime.Hub
	memory     *memory.Service
	visibility VisibilityProvider
	usage      UsageRecorder
	inLineWait time.Duration

	mu        sync.RWMutex
	tools     map[string]toolEntry
	upstreams map[string]*upstream
}

// VisibilityProvider gives the gateway a way to compute, per request, which
// tools are visible to the calling agent. List shapes the tools/list
// response (and may rank/truncate); IsVisible answers the cheap per-tool
// check used by direct-call gating. Implementations are allowed to share
// state — the gateway never modifies the slice it passes to List.
type VisibilityProvider interface {
	List(ctx context.Context, tools []mcp.Tool) []mcp.Tool
	IsVisible(ctx context.Context, toolName string) bool
}

type Options struct {
	Name       string
	Version    string
	Policy     *policy.Engine
	Approval   *approval.Bus
	Audit      *audit.Logger
	Hub        *realtime.Hub
	Memory     *memory.Service
	InLineWait time.Duration
	// Visibility, if non-nil, decides which tools the agent sees in
	// tools/list and whether direct calls to a tool are accepted. Tools
	// that the provider hides remain registered, so meta-tool routing
	// (tools.execute) can still reach them.
	Visibility VisibilityProvider
	// Usage, if non-nil, has its Increment called on every call.succeeded
	// so top-N selections reflect real usage.
	Usage UsageRecorder
}

// UsageRecorder is satisfied by *internal/usage.Service. The gateway only
// needs Increment; we keep the surface narrow for testability.
type UsageRecorder interface {
	Increment(ctx context.Context, agentID, toolName string) error
}

func New(opts Options) *Gateway {
	if opts.Name == "" {
		opts.Name = "toolyard"
	}
	if opts.Version == "" {
		opts.Version = "0.1.0"
	}
	if opts.InLineWait == 0 {
		opts.InLineWait = defaultInLineWait
	}
	serverOpts := []server.ServerOption{
		server.WithToolCapabilities(true),
		server.WithLogging(),
		server.WithRecovery(),
		server.WithInstructions("toolyard gateway. All tool calls require an explicit _reason; writes go through human approval."),
	}
	if opts.Visibility != nil {
		vp := opts.Visibility
		serverOpts = append(serverOpts, server.WithToolFilter(
			func(ctx context.Context, tools []mcp.Tool) []mcp.Tool {
				return vp.List(ctx, tools)
			},
		))
	}
	mcpSrv := server.NewMCPServer(opts.Name, opts.Version, serverOpts...)
	return &Gateway{
		mcp:        mcpSrv,
		policy:     opts.Policy,
		approval:   opts.Approval,
		audit:      opts.Audit,
		hub:        opts.Hub,
		memory:     opts.Memory,
		visibility: opts.Visibility,
		usage:      opts.Usage,
		inLineWait: opts.InLineWait,
		tools:      map[string]toolEntry{},
		upstreams:  map[string]*upstream{},
	}
}

// NotifyToolListChanged sends notifications/tools/list_changed to every
// connected MCP session. Call this when something that affects what tools
// agents see has changed (e.g., the router_only_mode setting flipped, an
// upstream connected/disconnected). Without this, MCP clients keep using
// the cached tool list from their initial connect.
func (g *Gateway) NotifyToolListChanged() {
	if g == nil || g.mcp == nil {
		return
	}
	g.mcp.SendNotificationToAllClients(mcp.MethodNotificationToolsListChanged, nil)
}

func (g *Gateway) MCPServer() *server.MCPServer { return g.mcp }

// RegisterBuiltins wires the built-in memory tools, the meta-tools
// (tools.search / tools.execute), and the fixture echo tool into the MCP
// server.
func (g *Gateway) RegisterBuiltins() {
	entries := g.builtinMemoryTools()
	entries = append(entries, g.staticFixtureTool())
	entries = append(entries, g.metaTools()...)
	for _, e := range entries {
		g.registerEntry(e)
	}
}

// AddUpstream connects to one upstream MCP server, fetches its tool list, and
// wraps each one into the gateway's catalog.
func (g *Gateway) AddUpstream(ctx context.Context, cfg UpstreamConfig) error {
	if cfg.Name == "" {
		return errors.New("upstream needs a name")
	}
	if cfg.Name == builtinUpstream || cfg.Name == "fixture" {
		return fmt.Errorf("name %q is reserved", cfg.Name)
	}
	u, err := newUpstream(ctx, cfg)
	if err != nil {
		return err
	}
	tools, err := u.listTools(ctx)
	if err != nil {
		_ = u.close()
		return err
	}
	g.mu.Lock()
	g.upstreams[cfg.Name] = u
	g.mu.Unlock()

	upstreamRef := u
	for _, t := range tools {
		original := t.Name
		wrapped, field := wrapSchema(t)
		wrapped.Name = cfg.Name + "." + original
		entry := toolEntry{
			tool:         wrapped,
			upstream:     cfg.Name,
			originalName: original,
			reasonField:  field,
			handle: func(name string) directHandler {
				return func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
					return upstreamRef.callTool(ctx, name, args)
				}
			}(original),
		}
		g.registerEntry(entry)
	}
	return nil
}

// Close shuts down all upstream connections.
func (g *Gateway) Close() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, u := range g.upstreams {
		_ = u.close()
	}
	return nil
}

// RemoveUpstream disconnects an upstream and removes all of its registered
// tools from the gateway catalog (and from the underlying MCP server).
func (g *Gateway) RemoveUpstream(name string) error {
	g.mu.Lock()
	u, ok := g.upstreams[name]
	if !ok {
		g.mu.Unlock()
		return ErrUpstreamNotFound
	}
	delete(g.upstreams, name)
	// Collect the wrapped tool names to remove.
	var toRemove []string
	for k, e := range g.tools {
		if e.upstream == name {
			toRemove = append(toRemove, k)
		}
	}
	for _, k := range toRemove {
		delete(g.tools, k)
	}
	g.mu.Unlock()

	g.mcp.DeleteTools(toRemove...)
	_ = u.close()
	return nil
}

// UpstreamToolCount returns the number of registered tools for the named
// upstream (0 if unknown).
func (g *Gateway) UpstreamToolCount(name string) int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	n := 0
	for _, e := range g.tools {
		if e.upstream == name {
			n++
		}
	}
	return n
}

// CatalogEntry summarises one wrapped tool for the dashboard or the
// tools.search meta-tool. The schema is the upstream-facing schema (after
// schema-wrap).
type CatalogEntry struct {
	Name        string         `json:"name"`
	Upstream    string         `json:"upstream"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

// Catalog returns the flat list of registered tools.
func (g *Gateway) Catalog() []CatalogEntry {
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := make([]CatalogEntry, 0, len(g.tools))
	for _, e := range g.tools {
		schema := map[string]any{
			"type":       e.tool.InputSchema.Type,
			"properties": e.tool.InputSchema.Properties,
			"required":   e.tool.InputSchema.Required,
		}
		out = append(out, CatalogEntry{
			Name:        e.tool.Name,
			Upstream:    e.upstream,
			Description: e.tool.Description,
			InputSchema: schema,
		})
	}
	return out
}

// HasTool returns true if a tool with the given name is registered.
func (g *Gateway) HasTool(name string) bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	_, ok := g.tools[name]
	return ok
}

// RouteCall is the same call routing used by every registered MCP tool, but
// callable directly: the meta-tool tools.execute uses it to dispatch a call to
// any tool in the catalog without having to round-trip through the MCP server
// stack. The args map should already have _reason / _intent_category fields.
//
// Behaviour:
//   - Unknown tool -> error CallToolResult.
//   - Reads pass straight through.
//   - Writes hold for approval (in-line then deferred), exactly like a direct
//     call would.
//
// `viaTool` is the meta-tool name we record in the audit log so it's clear
// the call was reached through tools.execute.
func (g *Gateway) RouteCall(ctx context.Context, viaTool, targetName string, args map[string]any) (*mcp.CallToolResult, error) {
	g.mu.RLock()
	entry, ok := g.tools[targetName]
	g.mu.RUnlock()
	if !ok {
		return mcp.NewToolResultErrorf("tool %q not found in catalog", targetName), nil
	}
	if targetName == viaTool {
		return mcp.NewToolResultError("tools.execute cannot target itself"), nil
	}
	return g.routeEntry(ctx, entry, args)
}

// routeEntry is the shared path used by both the MCP-side handler and
// tools.execute. It assumes entry is a registered toolEntry.
func (g *Gateway) routeEntry(ctx context.Context, entry toolEntry, args map[string]any) (*mcp.CallToolResult, error) {
	// Deferred-resume short-circuit.
	if approvalID, ok := args["_approval_id"].(string); ok && approvalID != "" {
		return g.resumeDeferred(ctx, entry, approvalID)
	}

	reason, intent, cleanArgs, err := extractReason(args, entry.reasonField)
	if err != nil {
		_ = g.audit.Write(ctx, audit.Event{
			EventType:     audit.EventCallFailed,
			UpstreamName:  entry.upstream,
			ToolName:      entry.tool.Name,
			ResultSummary: "rejected: " + err.Error(),
		})
		return mcp.NewToolResultError(err.Error()), nil
	}

	agentID := agentIDFromContext(ctx)
	argsJSON, _ := json.Marshal(cleanArgs)
	_ = g.audit.Write(ctx, audit.Event{
		EventType:    audit.EventCallStart,
		AgentID:      agentID,
		UpstreamName: entry.upstream,
		ToolName:     entry.tool.Name,
		Reason:       reason,
		Arguments:    argsJSON,
	})

	decision := g.policy.Eval(policy.Request{
		AgentID:        agentID,
		UpstreamName:   entry.upstream,
		ToolName:       entry.originalName,
		IntentCategory: intent,
		Arguments:      cleanArgs,
		UserReason:     reason,
	})

	switch decision.Action {
	case policy.ActionAllow:
		_ = g.audit.Write(ctx, audit.Event{
			EventType:    audit.EventCallAllowed,
			AgentID:      agentID,
			UpstreamName: entry.upstream,
			ToolName:     entry.tool.Name,
			Decision:     string(decision.Action),
			Reason:       reason,
		})
		return g.dispatch(ctx, entry, cleanArgs, agentID, reason, "")
	case policy.ActionDeny:
		_ = g.audit.Write(ctx, audit.Event{
			EventType:    audit.EventCallDenied,
			AgentID:      agentID,
			UpstreamName: entry.upstream,
			ToolName:     entry.tool.Name,
			Decision:     string(decision.Action),
			Reason:       reason,
		})
		return mcp.NewToolResultError("denied by policy: " + decision.Reason), nil
	case policy.ActionApprove:
		return g.holdAndWait(ctx, entry, cleanArgs, agentID, reason, intent)
	default:
		return mcp.NewToolResultErrorf("policy returned unknown action %q", decision.Action), nil
	}
}

func (g *Gateway) registerEntry(e toolEntry) {
	g.mu.Lock()
	g.tools[e.tool.Name] = e
	g.mu.Unlock()
	g.mcp.AddTool(e.tool, g.handlerFor(e.tool.Name))
}

// handlerFor returns the ToolHandlerFunc that performs schema-wrap unwrap,
// policy eval, optional approval, and dispatch. It delegates to routeEntry
// so the same routing logic backs the meta-tool tools.execute.
//
// Direct calls to a tool that the filter has hidden (e.g., everything except
// tools.search/execute when router_only_mode is on) are rejected here with
// a clear pointer at tools.execute. RouteCall — which is what tools.execute
// itself uses — bypasses this check, so the meta-tool can still reach the
// hidden tool.
func (g *Gateway) handlerFor(toolName string) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		g.mu.RLock()
		entry, ok := g.tools[toolName]
		g.mu.RUnlock()
		if !ok {
			return mcp.NewToolResultErrorf("tool %q not registered", toolName), nil
		}
		if g.visibility != nil && !g.visibility.IsVisible(ctx, entry.tool.Name) {
			return mcp.NewToolResultErrorf(
				"tool %q is hidden by the current agent-surface mode. Use tools.execute with tool=%q to invoke it.",
				toolName, toolName,
			), nil
		}
		args := argsAsMap(request.Params.Arguments)
		return g.routeEntry(ctx, entry, args)
	}
}

func (g *Gateway) holdAndWait(ctx context.Context, entry toolEntry, args map[string]any,
	agentID, reason, intent string) (*mcp.CallToolResult, error) {

	holdCtx, cancel := context.WithTimeout(ctx, g.inLineWait+5*time.Second)
	defer cancel()

	req, err := g.approval.Hold(holdCtx, approval.NewRequest{
		AgentID:        agentID,
		UpstreamName:   entry.upstream,
		ToolName:       entry.tool.Name,
		Arguments:      args,
		Reason:         reason,
		IntentCategory: intent,
	}, g.inLineWait)
	if err != nil {
		return mcp.NewToolResultErrorFromErr("approval hold failed", err), nil
	}

	switch req.Status {
	case approval.StatusAllowed:
		_ = g.audit.Write(ctx, audit.Event{
			EventType:    audit.EventCallAllowed,
			AgentID:      agentID,
			UpstreamName: entry.upstream,
			ToolName:     entry.tool.Name,
			Decision:     "allow",
			ApprovalID:   req.ID,
			Reason:       reason,
		})
		return g.dispatch(ctx, entry, args, agentID, reason, req.ID)
	case approval.StatusDenied:
		_ = g.audit.Write(ctx, audit.Event{
			EventType:    audit.EventCallDenied,
			AgentID:      agentID,
			UpstreamName: entry.upstream,
			ToolName:     entry.tool.Name,
			Decision:     "deny",
			ApprovalID:   req.ID,
			Reason:       reason,
		})
		return mcp.NewToolResultError("denied by human reviewer"), nil
	case approval.StatusExpired:
		return mcp.NewToolResultError("approval window expired without a decision"), nil
	default:
		// Still pending after in-line window — return deferred response.
		return deferredResponse(req), nil
	}
}

func (g *Gateway) resumeDeferred(ctx context.Context, entry toolEntry, approvalID string) (*mcp.CallToolResult, error) {
	req, err := g.approval.Get(ctx, approvalID)
	if err != nil {
		return mcp.NewToolResultErrorf("unknown approval %q", approvalID), nil
	}
	if req.Status == approval.StatusPending {
		// Agent is polling early — block briefly, then re-emit deferred.
		if waitCh, ok := g.approval.Watch(approvalID); ok {
			select {
			case <-waitCh:
			case <-time.After(g.inLineWait):
			case <-ctx.Done():
			}
		}
		req, err = g.approval.Get(ctx, approvalID)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("approval lookup", err), nil
		}
	}
	switch req.Status {
	case approval.StatusAllowed:
		return g.dispatch(ctx, entry, req.Arguments, req.AgentID, req.Reason, req.ID)
	case approval.StatusDenied:
		return mcp.NewToolResultError("denied by human reviewer"), nil
	case approval.StatusExpired:
		return mcp.NewToolResultError("approval window expired without a decision"), nil
	default:
		return deferredResponse(req), nil
	}
}

func (g *Gateway) dispatch(ctx context.Context, entry toolEntry, args map[string]any,
	agentID, reason, approvalID string) (*mcp.CallToolResult, error) {

	if entry.handle == nil {
		// Upstream-backed tool — route through the upstream pool.
		g.mu.RLock()
		u := g.upstreams[entry.upstream]
		g.mu.RUnlock()
		if u == nil {
			return mcp.NewToolResultErrorf("upstream %q not connected", entry.upstream), nil
		}
		entry.handle = func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
			return u.callTool(ctx, entry.originalName, args)
		}
	}
	res, err := entry.handle(ctx, args)
	if err != nil {
		_ = g.audit.Write(ctx, audit.Event{
			EventType:    audit.EventCallFailed,
			AgentID:      agentID,
			UpstreamName: entry.upstream,
			ToolName:     entry.tool.Name,
			Reason:       reason,
			ApprovalID:   approvalID,
			ResultSummary: err.Error(),
		})
		return mcp.NewToolResultErrorFromErr("tool failed", err), nil
	}
	_ = g.audit.Write(ctx, audit.Event{
		EventType:    audit.EventCallSucceeded,
		AgentID:      agentID,
		UpstreamName: entry.upstream,
		ToolName:     entry.tool.Name,
		Reason:       reason,
		ApprovalID:   approvalID,
		ResultSummary: summariseResult(res),
	})
	if g.usage != nil && !res.IsError {
		// We treat IsError=true (e.g., "key not found") as a logical
		// failure even though the call dispatched cleanly, so it does
		// not feed top-N. Outright transport/protocol failures already
		// short-circuited above.
		_ = g.usage.Increment(ctx, agentID, entry.tool.Name)
	}
	return res, nil
}

// deferredResponse formats the "approval pending — retry with _approval_id"
// body described in the plan.
func deferredResponse(req *approval.Request) *mcp.CallToolResult {
	expires := time.UnixMilli(req.ExpiresAt).UTC().Format(time.RFC3339)
	text := fmt.Sprintf(
		"Approval pending. Re-call this tool with _approval_id=%q to resume. Expires at %s.",
		req.ID, expires,
	)
	res := mcp.NewToolResultText(text)
	res.StructuredContent = map[string]any{
		"status":              "pending_approval",
		"approval_id":         req.ID,
		"retry_after_seconds": deferredRetryAfter,
		"expires_at":          expires,
	}
	res.Meta = &mcp.Meta{AdditionalFields: map[string]any{"toolyard.deferred": true}}
	return res
}

func summariseResult(res *mcp.CallToolResult) string {
	if res == nil {
		return ""
	}
	var b strings.Builder
	for _, c := range res.Content {
		if t, ok := mcp.AsTextContent(c); ok {
			if b.Len() > 0 {
				b.WriteString(" | ")
			}
			snippet := t.Text
			if len(snippet) > 200 {
				snippet = snippet[:200] + "…"
			}
			b.WriteString(snippet)
		}
	}
	if res.IsError {
		return "ERROR: " + b.String()
	}
	return b.String()
}

// agentIDFromContext extracts the agent ID set by the HTTP transport's auth
// context func. Returns "" for unauthenticated stdio sessions.
type agentIDKey struct{}

func WithAgentID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, agentIDKey{}, id)
}

func agentIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(agentIDKey{}).(string); ok {
		return v
	}
	return ""
}

// AgentIDFromContext is the public read accessor for the agent ID stashed
// by WithAgentID. Used by the visibility provider; equivalent to
// agentIDFromContext but exported.
func AgentIDFromContext(ctx context.Context) string {
	return agentIDFromContext(ctx)
}

// SetUpstreamLogger wires log output for upstream errors to the supplied logger.
func (g *Gateway) SetUpstreamLogger(l *log.Logger) {
	// (placeholder) Future: pass into upstream client options.
}
