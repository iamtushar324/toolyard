package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/tusharbhardwaj/toolyard/internal/approval"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/lake"
	"github.com/tusharbhardwaj/toolyard/internal/memory"
	"github.com/tusharbhardwaj/toolyard/internal/metrics"
	"github.com/tusharbhardwaj/toolyard/internal/policy"
	"github.com/tusharbhardwaj/toolyard/internal/realtime"
)

const (
	builtinUpstream = "builtin"
	// defaultInLineWait used to be 30s — block the agent's HTTP request
	// for up to half a minute hoping a human taps Allow. The new model
	// returns deferred immediately and lets the agent decide whether to
	// poll or wait via tools.poll_approval / tools.wait_for_approval.
	// Operators can opt back into the legacy flow with -in-line-wait > 0.
	defaultInLineWait  = 0
	deferredRetryAfter = 60
	// DefaultMaxPendingPerAgent caps how many approvals one agent can
	// have queued at once. With the deferred-by-default flow agents can
	// fire many in parallel; this stops a runaway agent from filling the
	// human reviewer's queue.
	DefaultMaxPendingPerAgent = 16
	// WaitForApprovalMaxTimeout caps tools.wait_for_approval so a single
	// blocking call can't hold a connection longer than common LB read
	// timeouts.
	WaitForApprovalMaxTimeout = 5 * time.Minute
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
	"tools.search":                    {},
	"tools.execute":                   {},
	"tools.poll_approval":             {},
	"tools.poll_approvals":            {},
	"tools.wait_for_approval":         {},
	"tools.wait_for_approvals":        {},
	"tools.list_my_pending_approvals": {},
	"tools.cancel_my_approval":        {},
	"tools.approval_stats":            {},
	"memory.get":                      {},
	"memory.set":                      {},
	"memory.list":                     {},
	"memory.delete":                   {},
	// Personal data lake — the additive tools are pinned so agents always
	// see them; the approval-gated mutations (lake.update, lake.delete,
	// lake.alter, lake.drop) are deliberately not pinned, keeping them off
	// the default toolbelt unless the agent reaches via tools.search.
	"lake.query":          {},
	"lake.list_tables":    {},
	"lake.describe_table": {},
	"lake.insert":         {},
	"lake.create_table":   {},
	"lake.ingest":         {},
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
	// forcedAction, when non-nil, short-circuits policy.Eval for this tool
	// and uses the provided action instead. Used by built-in tools that
	// need a deterministic policy decision regardless of the name-heuristic
	// (e.g., lake.create_table looks like a write but is intentionally
	// auto-allowed). The intent_category supplied by the agent is ignored
	// when forcedAction is set.
	forcedAction *policy.Action
}

// Gateway stitches the MCP server, policy, approval bus, memory, and upstream
// pool into one coordinated unit.
type Gateway struct {
	mcp                *server.MCPServer
	policy             *policy.Engine
	approval           *approval.Bus
	audit              *audit.Logger
	hub                *realtime.Hub
	memory             *memory.Service
	lake               *lake.Service
	visibility         VisibilityProvider
	usage              UsageRecorder
	metrics            MetricsRecorder
	metricsReader      MetricsLatencyReader
	surface            SurfaceModeProvider
	inLineWait          time.Duration
	maxPendingPerAgent  int
	upstreamCallTimeout time.Duration

	// inFlight is the live count of routeEntry calls currently executing
	// (not yet returned). Surfaced via /v1/health and the dashboard so a
	// runaway upstream is visible without dumping goroutines.
	inFlight atomic.Int64

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
	// Lake, when non-nil, gives the gateway a DuckDB-backed personal data
	// lake. The lake.* MCP tools are registered against it during
	// RegisterBuiltins. Nil keeps the gateway working without a lake (e.g.,
	// in tests that don't need it).
	Lake       *lake.Service
	InLineWait time.Duration
	// Visibility, if non-nil, decides which tools the agent sees in
	// tools/list and whether direct calls to a tool are accepted. Tools
	// that the provider hides remain registered, so meta-tool routing
	// (tools.execute) can still reach them.
	Visibility VisibilityProvider
	// Usage, if non-nil, has its Increment called on every call.succeeded
	// so top-N selections reflect real usage.
	Usage UsageRecorder
	// Metrics, if non-nil, receives one Event per terminal call outcome.
	// Events are recorded asynchronously so this never adds latency to
	// the request path.
	Metrics MetricsRecorder
	// MetricsReader, if non-nil, lets the gateway look up the human
	// reviewer's recent decision-time percentiles so deferred responses
	// can tell the agent "expected ~30s" instead of guessing. Optional —
	// when nil the deferred envelope omits the timing hint and the agent
	// gets a static fallback in next_steps_for_agent.
	MetricsReader MetricsLatencyReader
	// Surface lets the gateway tag each event with the active surface_mode
	// so analytics can correlate visibility decisions to call counts.
	Surface SurfaceModeProvider
	// MaxPendingPerAgent overrides the per-agent pending-approval cap.
	// Zero falls back to DefaultMaxPendingPerAgent.
	MaxPendingPerAgent int
	// UpstreamCallTimeout caps how long a single tool dispatch may take
	// before the gateway aborts it. Zero (the default) disables the cap;
	// any positive value applies to every dispatch (built-in, fixture,
	// and external upstreams alike). Without a cap, a hung upstream
	// pinned a goroutine forever and piled up everyone behind it.
	UpstreamCallTimeout time.Duration
}

// UsageRecorder is satisfied by *internal/usage.Service. The gateway only
// needs Increment; we keep the surface narrow for testability.
type UsageRecorder interface {
	Increment(ctx context.Context, agentID, toolName string) error
}

// MetricsRecorder is satisfied by *internal/metrics.Recorder. We declare it
// here as a small interface so test doubles don't need a DB.
type MetricsRecorder interface {
	Record(metrics.Event)
}

// MetricsLatencyReader returns the human reviewer's recent
// decision-time percentiles for use in deferred-response envelopes.
// Implemented by *internal/metrics.Reader; gateway treats nil as "no
// hint available."
type MetricsLatencyReader interface {
	ApprovalLatency(ctx context.Context, fingerprint, toolName, upstream string) *metrics.ApprovalLatencyEstimate
}

// SurfaceModeProvider returns the surface mode that was in effect when a
// call was routed. internal/visibility implements this.
type SurfaceModeProvider interface {
	SurfaceMode(ctx context.Context) string
}

func New(opts Options) *Gateway {
	if opts.Name == "" {
		opts.Name = "toolyard"
	}
	if opts.Version == "" {
		opts.Version = "0.1.0"
	}
	// InLineWait of 0 is the new default: deferred response is returned
	// immediately. We treat any negative value as "use the default."
	if opts.InLineWait < 0 {
		opts.InLineWait = defaultInLineWait
	}
	if opts.MaxPendingPerAgent <= 0 {
		opts.MaxPendingPerAgent = DefaultMaxPendingPerAgent
	}
	serverOpts := []server.ServerOption{
		server.WithToolCapabilities(true),
		server.WithLogging(),
		server.WithRecovery(),
		server.WithInstructions(buildInstructions(opts.Approval, opts.InLineWait)),
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
		mcp:                 mcpSrv,
		policy:              opts.Policy,
		approval:            opts.Approval,
		audit:               opts.Audit,
		hub:                 opts.Hub,
		memory:              opts.Memory,
		lake:                opts.Lake,
		visibility:          opts.Visibility,
		usage:               opts.Usage,
		metrics:             opts.Metrics,
		metricsReader:       opts.MetricsReader,
		surface:             opts.Surface,
		inLineWait:          opts.InLineWait,
		maxPendingPerAgent:  opts.MaxPendingPerAgent,
		upstreamCallTimeout: opts.UpstreamCallTimeout,
		tools:               map[string]toolEntry{},
		upstreams:           map[string]*upstream{},
	}
}

// InFlight returns the number of tool calls currently being routed. Used
// by /v1/health and the dashboard to spot pile-ups; it is also a quick
// signal during a hang ("how many calls are stuck").
func (g *Gateway) InFlight() int64 {
	if g == nil {
		return 0
	}
	return g.inFlight.Load()
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
// (tools.search / tools.execute), the fixture echo tool, and (if a Lake is
// configured) the lake.* personal-data-warehouse tools into the MCP server.
func (g *Gateway) RegisterBuiltins() {
	entries := g.builtinMemoryTools()
	entries = append(entries, g.staticFixtureTool())
	entries = append(entries, g.metaTools()...)
	entries = append(entries, g.approvalMetaTools()...)
	if g.lake != nil {
		entries = append(entries, g.lakeTools()...)
	}
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

// CallInternal dispatches a registered tool by name, bypassing the
// approval/policy gate. It is intended for toolyard-internal call paths
// (e.g. /v1/mempalace/ingest) where the call comes from a trusted code path
// rather than an agent, so blocking on a human tap would defeat the
// integration's purpose. The audit trail still records the call so it is
// reviewable after the fact, with `viaTool` recorded as the dispatcher.
//
// Approval, policy.Eval, and the per-agent budget are all skipped — callers
// must already be confident the operation is safe. Schema-wrap reason
// extraction is also skipped; the caller is responsible for whatever shape
// the underlying tool expects.
//
// Returns the tool result (possibly with IsError=true) just like a regular
// call. Errors surface dispatch / upstream connection failures only.
func (g *Gateway) CallInternal(ctx context.Context, viaTool, targetName string, args map[string]any) (*mcp.CallToolResult, error) {
	g.mu.RLock()
	entry, ok := g.tools[targetName]
	g.mu.RUnlock()
	if !ok {
		return mcp.NewToolResultErrorf("tool %q not found in catalog", targetName), nil
	}

	started := time.Now()
	agentID := agentIDFromContext(ctx)
	ev := &metrics.Event{
		TS:         started.UnixMilli(),
		AgentID:    agentID,
		Upstream:   entry.upstream,
		ShortName:  entry.originalName,
		ToolName:   entry.tool.Name,
		IsWrite:    !policy.IsReadOnlyName(entry.tool.Name),
		PinnedTool: IsPinned(entry.tool.Name),
		Via:        viaTool,
	}
	if g.surface != nil {
		ev.SurfaceMode = g.surface.SurfaceMode(ctx)
	}
	// We log the dispatch as already-allowed so the dashboard's audit feed
	// shows the call. We deliberately omit the arguments blob because the
	// ingest path treats whatever the agent posted as opaque — the redactor
	// runs in audit.Write anyway, but skipping the marshal keeps the hot
	// path cheap.
	_ = g.audit.Write(ctx, audit.Event{
		EventType:    audit.EventCallAllowed,
		AgentID:      agentID,
		UpstreamName: entry.upstream,
		ToolName:     entry.tool.Name,
		Decision:     "internal",
		Reason:       "internal:" + viaTool,
	})
	ev.ApprovalOutcome = metrics.ApprovalNone
	res, err := g.dispatch(ctx, entry, args, agentID, "internal:"+viaTool, "", ev)
	ev.TotalLatencyMs = int(time.Since(started).Milliseconds())
	if g.metrics != nil {
		g.metrics.Record(*ev)
	}
	return res, err
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
// tools.execute. It assumes entry is a registered toolEntry. The metric
// Event captured here is emitted to the metrics sink (if any) once the
// terminal outcome of this call is known.
func (g *Gateway) routeEntry(ctx context.Context, entry toolEntry, args map[string]any) (res *mcp.CallToolResult, err error) {
	started := time.Now()
	agentID := agentIDFromContext(ctx)

	// In-flight tracking + slow-call watchdog. The watchdog goroutine
	// logs at 5s / 30s / 2m elapsed if the call is still routing, so a
	// hang shows up in the journal naming the offending tool / upstream
	// / agent instead of just "everything is stuck."
	g.inFlight.Add(1)
	doneWatch := make(chan struct{})
	defer func() {
		g.inFlight.Add(-1)
		close(doneWatch)
	}()
	go watchSlowCall(doneWatch, started, entry.tool.Name, entry.upstream, agentID)
	ev := metrics.Event{
		TS:         started.UnixMilli(),
		AgentID:    agentID,
		Upstream:   entry.upstream,
		ShortName:  entry.originalName,
		ToolName:   entry.tool.Name,
		IsWrite:    !policy.IsReadOnlyName(entry.tool.Name),
		PinnedTool: IsPinned(entry.tool.Name),
		Via:        "direct",
	}
	if g.surface != nil {
		ev.SurfaceMode = g.surface.SurfaceMode(ctx)
	}
	if g.visibility != nil {
		v := g.visibility.IsVisible(ctx, entry.tool.Name)
		ev.InTopN = &v
	}
	defer func() {
		ev.TotalLatencyMs = int(time.Since(started).Milliseconds())
		if ev.Outcome == "" {
			switch {
			case err != nil:
				ev.Outcome = metrics.OutcomeError
			case res != nil && res.IsError:
				ev.Outcome = metrics.OutcomeError
			default:
				ev.Outcome = metrics.OutcomeOK
			}
		}
		if res != nil {
			ev.ResultSizeBytes = approxResultSize(res)
		}
		if g.metrics != nil {
			g.metrics.Record(ev)
		}
	}()

	// Deferred-resume short-circuit.
	if approvalID, ok := args["_approval_id"].(string); ok && approvalID != "" {
		ev.ApprovalID = approvalID
		return g.resumeDeferred(ctx, entry, approvalID, &ev)
	}

	reason, intent, cleanArgs, rerr := extractReason(args, entry.reasonField)
	if rerr != nil {
		_ = g.audit.Write(ctx, audit.Event{
			EventType:     audit.EventCallFailed,
			UpstreamName:  entry.upstream,
			ToolName:      entry.tool.Name,
			ResultSummary: "rejected: " + rerr.Error(),
		})
		ev.Outcome = metrics.OutcomeError
		ev.ErrorClass = "schema"
		return mcp.NewToolResultError(rerr.Error()), nil
	}

	keys, sizeBytes := metrics.ArgsShape(cleanArgs)
	ev.ArgsTopKeys = keys
	ev.ArgsSizeBytes = sizeBytes
	ev.ReasonText = reason
	ev.ReasonLen = len(reason)
	ev.IntentCategory = intent
	if g.approval != nil {
		ev.Fingerprint = approval.ComputeFingerprint(agentID, entry.upstream, entry.tool.Name, cleanArgs)
	}
	argsJSON, _ := json.Marshal(cleanArgs)
	_ = g.audit.Write(ctx, audit.Event{
		EventType:    audit.EventCallStart,
		AgentID:      agentID,
		UpstreamName: entry.upstream,
		ToolName:     entry.tool.Name,
		Reason:       reason,
		Arguments:    argsJSON,
	})

	var decision policy.Decision
	if entry.forcedAction != nil {
		decision = policy.Decision{
			Action: *entry.forcedAction,
			Reason: "built-in tool policy",
			RuleID: "builtin-forced-" + string(*entry.forcedAction),
		}
	} else {
		decision = g.policy.Eval(policy.Request{
			AgentID:        agentID,
			UpstreamName:   entry.upstream,
			ToolName:       entry.originalName,
			IntentCategory: intent,
			Arguments:      cleanArgs,
			UserReason:     reason,
		})
	}

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
		ev.ApprovalOutcome = metrics.ApprovalNone
		return g.dispatch(ctx, entry, cleanArgs, agentID, reason, "", &ev)
	case policy.ActionDeny:
		_ = g.audit.Write(ctx, audit.Event{
			EventType:    audit.EventCallDenied,
			AgentID:      agentID,
			UpstreamName: entry.upstream,
			ToolName:     entry.tool.Name,
			Decision:     string(decision.Action),
			Reason:       reason,
		})
		ev.Outcome = metrics.OutcomeDenied
		ev.ErrorClass = "policy"
		return mcp.NewToolResultError("denied by policy: " + decision.Reason), nil
	case policy.ActionApprove:
		return g.holdAndWait(ctx, entry, cleanArgs, agentID, reason, intent, &ev)
	default:
		ev.Outcome = metrics.OutcomeError
		ev.ErrorClass = "policy"
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
	agentID, reason, intent string, ev *metrics.Event) (*mcp.CallToolResult, error) {

	// Per-agent budget: too many concurrent pendings from one agent can
	// drown the human reviewer. Reject before persisting so a runaway
	// agent doesn't fill the queue.
	if agentID != "" && g.approval != nil {
		if n, err := g.approval.CountPendingForAgent(ctx, agentID); err == nil && n >= g.maxPendingPerAgent {
			ev.Outcome = metrics.OutcomeError
			ev.ErrorClass = "budget"
			return budgetExceededResponse(agentID, n, g.maxPendingPerAgent), nil
		}
	}

	// holdCtx ensures Hold() unblocks even if the caller's context is
	// long. With inLineWait=0 (the new default) Hold returns immediately
	// after the row is committed; we still give a tiny buffer for the
	// auto-approval evaluator and SQLite write.
	holdTimeout := g.inLineWait + 5*time.Second
	if holdTimeout < 5*time.Second {
		holdTimeout = 5 * time.Second
	}
	holdCtx, cancel := context.WithTimeout(ctx, holdTimeout)
	defer cancel()

	holdStart := time.Now()
	req, err := g.approval.Hold(holdCtx, approval.NewRequest{
		AgentID:        agentID,
		UpstreamName:   entry.upstream,
		ToolName:       entry.tool.Name,
		Arguments:      args,
		Reason:         reason,
		IntentCategory: intent,
	}, g.inLineWait)
	if err != nil {
		ev.Outcome = metrics.OutcomeError
		ev.ErrorClass = "approval"
		return mcp.NewToolResultErrorFromErr("approval hold failed", err), nil
	}
	ev.ApprovalID = req.ID
	if req.AutoDecidedBy != "" {
		ev.ApprovalVia = "auto"
		ev.ApprovalDecider = req.AutoDecidedBy
	}
	if req.Coalesced {
		ev.CoalescedInto = req.ID
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
		ev.ApprovalLatencyMs = int(time.Since(holdStart).Milliseconds())
		if req.AutoDecidedBy != "" {
			ev.ApprovalOutcome = metrics.ApprovalAuto
		} else {
			ev.ApprovalOutcome = metrics.ApprovalApproved
		}
		return g.dispatch(ctx, entry, args, agentID, reason, req.ID, ev)
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
		ev.ApprovalOutcome = metrics.ApprovalDenied
		ev.ApprovalLatencyMs = int(time.Since(holdStart).Milliseconds())
		ev.Outcome = metrics.OutcomeDenied
		return mcp.NewToolResultError("denied by human reviewer"), nil
	case approval.StatusExpired:
		ev.ApprovalOutcome = metrics.ApprovalExpired
		ev.Outcome = metrics.OutcomeExpired
		return mcp.NewToolResultError("approval window expired without a decision"), nil
	default:
		// Still pending after in-line window — return deferred response.
		ev.Outcome = metrics.OutcomeDeferred
		return g.deferredResponse(ctx, req, entry, args), nil
	}
}

// resumeDeferred is the legacy `_approval_id` re-call path. With
// auto-execute, the gateway has already dispatched the tool the moment
// the human tapped Allow — so this path NEVER re-dispatches. It only:
//
//   - returns the cached result if one is already persisted;
//   - waits briefly (up to inLineWait) for the result if the request
//     is approved-but-still-executing, then returns it (or an
//     "executing" envelope telling the agent to poll);
//   - returns the appropriate denied/expired/cancelled message.
//
// The agent doesn't have to use this path anymore — tools.poll_approval
// and tools.wait_for_approval surface the same cached result. We keep
// `_approval_id` working only for backwards-compat with already-deployed
// agent code.
func (g *Gateway) resumeDeferred(ctx context.Context, entry toolEntry, approvalID string, ev *metrics.Event) (*mcp.CallToolResult, error) {
	req, err := g.approval.Get(ctx, approvalID)
	if err != nil {
		ev.Outcome = metrics.OutcomeError
		ev.ErrorClass = "approval"
		return mcp.NewToolResultErrorf("unknown approval %q", approvalID), nil
	}
	// Wait briefly if either the human hasn't decided yet, or we're
	// approved-but-the-executor-hasn't-finished. Either way the
	// signal channel closes when the row reaches its terminal state.
	awaitingDecision := req.Status == approval.StatusPending
	awaitingResult := req.Status == approval.StatusAllowed && req.ResultExecutedAt == 0
	if awaitingDecision || awaitingResult {
		if waitCh, ok := g.approval.Watch(approvalID); ok {
			wait := g.inLineWait
			if wait <= 0 {
				wait = 5 * time.Second // give the executor a brief chance even in deferred-by-default mode
			}
			select {
			case <-waitCh:
			case <-time.After(wait):
			case <-ctx.Done():
			}
		}
		req, err = g.approval.Get(ctx, approvalID)
		if err != nil {
			ev.Outcome = metrics.OutcomeError
			ev.ErrorClass = "approval"
			return mcp.NewToolResultErrorFromErr("approval lookup", err), nil
		}
	}
	ev.Fingerprint = req.Fingerprint
	ev.ReasonText = req.Reason
	ev.ReasonLen = len(req.Reason)
	switch req.Status {
	case approval.StatusAllowed:
		if req.AutoDecidedBy != "" {
			ev.ApprovalOutcome = metrics.ApprovalAuto
			ev.ApprovalVia = "auto"
			ev.ApprovalDecider = req.AutoDecidedBy
		} else {
			ev.ApprovalOutcome = metrics.ApprovalApproved
		}
		ev.ApprovalLatencyMs = int(time.Now().UnixMilli() - req.CreatedAt)
		if req.ResultExecutedAt > 0 {
			res, rerr := rehydrateApprovalResult(req.ResultEnvelope)
			if rerr != nil {
				return mcp.NewToolResultErrorFromErr("decode cached result", rerr), nil
			}
			if req.ResultError != "" {
				return mcp.NewToolResultErrorf("auto-execute failed: %s", req.ResultError), nil
			}
			if res == nil {
				return mcp.NewToolResultText("(approved tool returned no content)"), nil
			}
			return res, nil
		}
		// Approved but executor still running — tell the agent to poll
		// the approval_id rather than re-call the original tool.
		ev.Outcome = metrics.OutcomeDeferred
		return executingResponse(req, entry), nil
	case approval.StatusDenied:
		ev.ApprovalOutcome = metrics.ApprovalDenied
		ev.Outcome = metrics.OutcomeDenied
		return mcp.NewToolResultError("denied by human reviewer"), nil
	case approval.StatusExpired:
		ev.ApprovalOutcome = metrics.ApprovalExpired
		ev.Outcome = metrics.OutcomeExpired
		return mcp.NewToolResultError("approval window expired without a decision"), nil
	case approval.StatusCancelled:
		ev.ApprovalOutcome = metrics.ApprovalDenied
		ev.Outcome = metrics.OutcomeDenied
		return mcp.NewToolResultError("approval was cancelled by the agent"), nil
	default:
		ev.Outcome = metrics.OutcomeDeferred
		return g.deferredResponse(ctx, req, entry, req.Arguments), nil
	}
}

// executingResponse is the envelope returned when the agent re-calls
// with `_approval_id` after Allow but before the executor has written
// the result. It steers the agent to poll the approval ID rather than
// firing another resume call.
func executingResponse(req *approval.Request, entry toolEntry) *mcp.CallToolResult {
	text := fmt.Sprintf(
		"Approval %s is approved and the tool is executing now. "+
			"Poll tools.poll_approval(approval_id=%q) (or block via tools.wait_for_approval) "+
			"to receive the result — re-calling %s with _approval_id will only spin until the "+
			"same poll surfaces the cached result.",
		req.ID, req.ID, entry.tool.Name)
	res := mcp.NewToolResultText(text)
	res.StructuredContent = map[string]any{
		"status":      "executing",
		"approval_id": req.ID,
		"tool":        entry.tool.Name,
		"upstream":    entry.upstream,
		"next_steps_for_agent": map[string]any{
			"poll_status": map[string]any{
				"tool": "tools.poll_approval",
				"args": map[string]any{"approval_id": req.ID, "_reason": "(your reason)"},
			},
			"wait_then_check": map[string]any{
				"tool": "tools.wait_for_approval",
				"args": map[string]any{"approval_id": req.ID, "timeout_seconds": 60, "_reason": "(your reason)"},
			},
		},
	}
	res.Meta = &mcp.Meta{AdditionalFields: map[string]any{"toolyard.executing": true}}
	return res
}

// budgetExceededResponse explains the per-agent pending cap to the AI
// in the same self-describing envelope shape as a deferred response, so
// the client knows it should poll/cancel its existing pendings before
// firing more.
func budgetExceededResponse(agentID string, current, max int) *mcp.CallToolResult {
	text := fmt.Sprintf(
		"Per-agent pending-approval budget exceeded: you have %d pendings (max %d). "+
			"Resolve or cancel some before firing new approval-required calls. "+
			"Use tools.list_my_pending_approvals to see them, "+
			"tools.poll_approvals to check status, or tools.cancel_my_approval to withdraw.",
		current, max)
	res := mcp.NewToolResultError(text)
	res.StructuredContent = map[string]any{
		"status":           "agent_pending_budget_exceeded",
		"agent_id":         agentID,
		"current_pendings": current,
		"budget":           max,
		"recovery_options": []map[string]any{
			{"tool": "tools.list_my_pending_approvals", "args": map[string]any{"_reason": "(your reason)"}},
			{"tool": "tools.poll_approvals", "args": map[string]any{"approval_ids": []string{"…"}, "_reason": "(your reason)"}},
			{"tool": "tools.cancel_my_approval", "args": map[string]any{"approval_id": "…", "_reason": "(your reason)"}},
		},
	}
	return res
}

func (g *Gateway) dispatch(ctx context.Context, entry toolEntry, args map[string]any,
	agentID, reason, approvalID string, ev *metrics.Event) (*mcp.CallToolResult, error) {

	if entry.handle == nil {
		// Upstream-backed tool — route through the upstream pool.
		g.mu.RLock()
		u := g.upstreams[entry.upstream]
		g.mu.RUnlock()
		if u == nil {
			ev.Outcome = metrics.OutcomeError
			ev.ErrorClass = "upstream_not_connected"
			return mcp.NewToolResultErrorf("upstream %q not connected", entry.upstream), nil
		}
		entry.handle = func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
			return u.callTool(ctx, entry.originalName, args)
		}
	}
	upstreamStart := time.Now()
	callCtx := ctx
	if g.upstreamCallTimeout > 0 {
		var cancel context.CancelFunc
		callCtx, cancel = context.WithTimeout(ctx, g.upstreamCallTimeout)
		defer cancel()
	}
	res, err := entry.handle(callCtx, args)
	ev.UpstreamLatencyMs = int(time.Since(upstreamStart).Milliseconds())
	if errors.Is(err, context.DeadlineExceeded) {
		log.Printf("upstream-timeout: tool=%s upstream=%s agent=%s elapsed=%s timeout=%s",
			entry.tool.Name, entry.upstream, agentID,
			time.Since(upstreamStart).Round(time.Millisecond), g.upstreamCallTimeout)
	}
	if err != nil {
		_ = g.audit.Write(ctx, audit.Event{
			EventType:     audit.EventCallFailed,
			AgentID:       agentID,
			UpstreamName:  entry.upstream,
			ToolName:      entry.tool.Name,
			Reason:        reason,
			ApprovalID:    approvalID,
			ResultSummary: err.Error(),
		})
		ev.Outcome = metrics.OutcomeError
		ev.ErrorClass = "upstream"
		return mcp.NewToolResultErrorFromErr("tool failed", err), nil
	}
	_ = g.audit.Write(ctx, audit.Event{
		EventType:     audit.EventCallSucceeded,
		AgentID:       agentID,
		UpstreamName:  entry.upstream,
		ToolName:      entry.tool.Name,
		Reason:        reason,
		ApprovalID:    approvalID,
		ResultSummary: summariseResult(res),
	})
	if g.usage != nil && !res.IsError {
		// We treat IsError=true (e.g., "key not found") as a logical
		// failure even though the call dispatched cleanly, so it does
		// not feed top-N. Outright transport/protocol failures already
		// short-circuited above.
		_ = g.usage.Increment(ctx, agentID, entry.tool.Name)
	}
	if res.IsError {
		ev.Outcome = metrics.OutcomeError
		if ev.ErrorClass == "" {
			ev.ErrorClass = "tool"
		}
	}
	return res, nil
}

// approxResultSize estimates the byte size of a tool result for the cost
// panel and args-shape analysis. It walks text content and adds raw lengths;
// non-text content contributes a flat 64-byte fudge.
func approxResultSize(res *mcp.CallToolResult) int {
	if res == nil {
		return 0
	}
	n := 0
	for _, c := range res.Content {
		if t, ok := mcp.AsTextContent(c); ok {
			n += len(t.Text)
		} else {
			n += 64
		}
	}
	return n
}

// deferredResponse builds the "approval queued, here's how to track it"
// envelope. Both the human-readable text and the structured_content carry
// enough context for any MCP client to react intelligently — the AI
// learns the meta-tool names and recommended polling cadence from the
// response itself, no out-of-band documentation required.
func (g *Gateway) deferredResponse(ctx context.Context, req *approval.Request, entry toolEntry, originalArgs map[string]any) *mcp.CallToolResult {
	expires := time.UnixMilli(req.ExpiresAt).UTC().Format(time.RFC3339)

	// Latency hint: the most-specific bucket with enough samples wins.
	var latencyHint *metrics.ApprovalLatencyEstimate
	if g.metricsReader != nil {
		latencyHint = g.metricsReader.ApprovalLatency(ctx, req.Fingerprint, entry.tool.Name, entry.upstream)
	}
	expected := defaultExpectedDecisionSeconds
	expectedBasis := "default-static-guess"
	expectedSamples := 0
	expectedP90 := 0
	if latencyHint != nil && latencyHint.BasedOn != "default" && latencyHint.P50Seconds > 0 {
		expected = latencyHint.P50Seconds
		expectedBasis = latencyHint.BasedOn
		expectedSamples = latencyHint.Samples
		expectedP90 = latencyHint.P90Seconds
	}

	// Snapshot of this agent's other in-flight pendings so the AI can plan
	// batch polling. Best-effort — failure here doesn't break the deferred path.
	var otherPendings []string
	if g.approval != nil && req.AgentID != "" {
		if peers, err := g.approval.ListPendingByAgent(ctx, req.AgentID); err == nil {
			for _, p := range peers {
				if p.ID == req.ID {
					continue
				}
				otherPendings = append(otherPendings, p.ID)
			}
		}
	}

	// Plain-text body — visible to any MCP client that doesn't read structured_content.
	text := buildDeferredText(req, expires, expected)

	// Structured content — the protocol guide for AI clients that DO read it.
	envelope := map[string]any{
		// "pending_approval" is the canonical status the dashboard's
		// workbench filters on. We keep the value stable so existing
		// consumers keep working; the rich metadata around it is what's
		// new in the deferred-by-default flow.
		"status":          "pending_approval",
		"approval_id":     req.ID,
		"fingerprint":     req.Fingerprint,
		"agent_id":        req.AgentID,
		"tool":            entry.tool.Name,
		"upstream":        entry.upstream,
		"reason_to_human": req.Reason,
		"queued_at":       time.UnixMilli(req.CreatedAt).UTC().Format(time.RFC3339),
		"expires_at":      expires,
		"expected_decision": map[string]any{
			"in_seconds_p50": expected,
			"in_seconds_p90": expectedP90,
			"based_on":       expectedBasis,
			"samples":        expectedSamples,
			"window_days":    defaultExpectedDecisionWindowDays,
		},
		"your_other_pending":        otherPendings,
		"min_poll_interval_seconds": minPollIntervalSeconds,
		// auto_execute_on_approve: the gateway dispatches the original
		// tool itself the moment the human (or an auto-rule) flips this
		// approval to allowed. The agent's job is ONLY to fetch the
		// result via tools.poll_approval / tools.wait_for_approval.
		// Re-calling the original tool with _approval_id still works as
		// a backwards-compat path but is strictly slower and offers no
		// new behaviour.
		"auto_execute_on_approve": true,
		"how_to_get_the_result": "Poll this approval_id with tools.poll_approval (or block with tools.wait_for_approval). The response will carry the executed tool's result under `result` once the executor finishes. Do NOT re-call the original tool — toolyard already runs it for you on approve.",
		"next_steps_for_agent": map[string]any{
			"poll_status_and_result": map[string]any{
				"tool":     "tools.poll_approval",
				"args":     map[string]any{"approval_id": req.ID, "_reason": "(your reason)"},
				"blocking": false,
				"hint":     "preferred — non-blocking; response carries the executed tool's result once status='executed'",
			},
			"poll_many_at_once": map[string]any{
				"tool":     "tools.poll_approvals",
				"args":     map[string]any{"approval_ids": []string{req.ID}, "_reason": "(your reason)"},
				"blocking": false,
				"hint":     "preferred when several approvals are in flight; one round-trip returns each one's status + result",
			},
			"wait_then_check": map[string]any{
				"tool":                "tools.wait_for_approval",
				"args":                map[string]any{"approval_id": req.ID, "timeout_seconds": 60, "_reason": "(your reason)"},
				"blocking":            true,
				"max_timeout_seconds": int(WaitForApprovalMaxTimeout.Seconds()),
				"hint":                "blocks server-side until the executor writes the result, then returns it",
			},
			"cancel_if_no_longer_needed": map[string]any{
				"tool": "tools.cancel_my_approval",
				"args": map[string]any{"approval_id": req.ID, "_reason": "(your reason)"},
			},
			"list_all_my_pending": map[string]any{
				"tool": "tools.list_my_pending_approvals",
				"args": map[string]any{"_reason": "(your reason)"},
			},
			"backwards_compat_resume": map[string]any{
				"tool": entry.tool.Name,
				"args": map[string]any{"_approval_id": req.ID, "_reason": "(your reason)"},
				"hint": "DEPRECATED — re-calling with _approval_id returns the same cached result that the polling tools surface. Use the polling tools instead; they're cheaper and match the auto-execute model.",
			},
		},
		"concurrency_advice": "You may fire other approval-required tools in parallel; each returns its own approval_id without holding any connection. Polling all at once via tools.poll_approvals is more efficient than serially.",
	}
	res := mcp.NewToolResultText(text)
	res.StructuredContent = envelope
	res.Meta = &mcp.Meta{AdditionalFields: map[string]any{"toolyard.deferred": true}}
	return res
}

// buildDeferredText is the plain-text version of the envelope. Every MCP
// client renders text content; this paragraph is what humans / older AI
// clients see when they don't parse structured_content.
func buildDeferredText(req *approval.Request, expires string, expectedSeconds int) string {
	var b strings.Builder
	b.WriteString("Tool execution requires human approval. Your call has been queued.\n")
	b.WriteString("On approve, toolyard fires the tool itself — you do NOT re-call the original tool. ")
	b.WriteString("Just poll this approval_id and the response carries the executed tool's result.\n\n")
	fmt.Fprintf(&b, "approval_id: %s\n", req.ID)
	if expectedSeconds > 0 {
		fmt.Fprintf(&b, "expected_decision_in: ~%ds (based on the human reviewer's recent average)\n", expectedSeconds)
	}
	fmt.Fprintf(&b, "expires_at: %s\n\n", expires)
	b.WriteString("To proceed:\n")
	b.WriteString("- Continue with other work; you may fire more approval-required tools in parallel.\n")
	fmt.Fprintf(&b, "- Get the result: tools.poll_approval(approval_id=%q) — when status='executed', the result is in the response.\n", req.ID)
	fmt.Fprintf(&b, "- Or block server-side: tools.wait_for_approval(approval_id=%q, timeout_seconds=60) — returns as soon as the executor finishes.\n", req.ID)
	if req.Reason != "" {
		b.WriteString("\nThe human reviewer sees your reason: ")
		b.WriteString(strings.TrimSpace(req.Reason))
	}
	return b.String()
}

const (
	// defaultExpectedDecisionSeconds is the static fallback when we have
	// no historical data about this user's decision latency. 30s is a
	// reasonable typical "look at phone, tap Allow."
	defaultExpectedDecisionSeconds    = 30
	defaultExpectedDecisionWindowDays = 30
	// minPollIntervalSeconds is what the deferred envelope advertises to
	// agents as a polite floor between poll attempts.
	minPollIntervalSeconds = 3
)

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

// buildInstructions composes the MCP server's `instructions` field. It tells
// the agent how reasons + approvals + batching work so it knows it can fire
// several writes in parallel and let the human approve them together.
func buildInstructions(bus *approval.Bus, inLineWait time.Duration) string {
	ttl := approval.DefaultTTL
	if bus != nil {
		ttl = bus.TTL()
	}
	var b strings.Builder
	b.WriteString("toolyard gateway. ")
	b.WriteString("Every tool call REQUIRES a `_reason` field (20-2000 chars) explaining why you are calling it; this string is shown verbatim to the human reviewer. ")
	b.WriteString("Reads pass through silently; writes hold for human approval. ")
	b.WriteString(fmt.Sprintf("Approvals expire after %s if no decision arrives. ", ttl.Round(time.Minute)))
	b.WriteString("AUTO-EXECUTE ON APPROVE: when a write needs human review the gateway returns a deferred response containing `approval_id`. The moment the human (or an auto-approval rule) flips the request to allowed, toolyard fires the original tool itself with your persisted arguments and stashes the result. ")
	b.WriteString("Collecting that result is OPTIONAL — the tool runs (and its side effect happens) regardless of whether you fetch the result. Skip the collect step for fire-and-forget writes (logging, notifications, anything you don't need to read back). ")
	b.WriteString("If you DO need the result, the canonical way to retrieve it is to poll the approval_id, NOT to re-call the original tool: ")
	b.WriteString("• `tools.poll_approval(approval_id=…)` — non-blocking; response carries the executed tool's `result` once `status='executed'`. ")
	b.WriteString("• `tools.poll_approvals(approval_ids=[…])` — same shape, batched (up to 32 at once). ")
	b.WriteString("• `tools.wait_for_approval(approval_id=…, timeout_seconds=60)` — block server-side until the executor finishes (single id). ")
	b.WriteString("• `tools.wait_for_approvals(approval_ids=[…], mode='all'|'any', timeout_seconds=60)` — block server-side on several ids at once. mode='all' (default) returns when every id is terminal; mode='any' returns as soon as one is. Strictly more efficient than firing N parallel `tools.wait_for_approval` calls. ")
	b.WriteString("Re-calling the original tool with `_approval_id` still works for backwards compat, but returns the same cached result the polling tools already surface — strictly slower, no extra capability. ")
	b.WriteString("BATCHING: when a task needs several writes (e.g. create issue + comment + assign), invoke them in parallel from one turn rather than serially. The dashboard groups concurrent calls from the same agent into a single approval card so the human approves the whole batch with one tap. Per-call `_reason` strings are surfaced in that summary, so write each one to be readable on its own. After approving, toolyard executes each tool independently — `tools.wait_for_approvals(mode='all')` is the natural way to collect all the results in one round-trip. ")
	b.WriteString("Use `tools.search` and `tools.execute` to discover and proxy tools that aren't directly visible in your catalog.")
	return b.String()
}

// SetUpstreamLogger wires log output for upstream errors to the supplied logger.
func (g *Gateway) SetUpstreamLogger(l *log.Logger) {
	// (placeholder) Future: pass into upstream client options.
}

// Execute is the bus-driven auto-execute hook. It runs the approved
// tool with the persisted arguments and stashes the result on the
// approval row via bus.SetResult. The bus calls Execute on its own
// background context, so a slow upstream doesn't tie up the dashboard
// request that flipped the approval to allowed.
//
// We bypass policy/approval here because the row is already in
// Status=allowed — the gating decision is final. We still go through
// dispatch() so audit, metrics, the upstream-call timeout, and the
// slow-call watchdog all apply just like a normal call.
func (g *Gateway) Execute(ctx context.Context, req *approval.Request) {
	if g == nil || req == nil || g.approval == nil {
		return
	}
	g.mu.RLock()
	entry, ok := g.tools[req.ToolName]
	g.mu.RUnlock()
	if !ok {
		_ = g.approval.SetResult(ctx, req.ID, "", false,
			fmt.Sprintf("tool %q is no longer registered (was the upstream removed?)", req.ToolName))
		return
	}
	// Stamp the agent ID so audit / usage rows attribute the call to
	// the originating agent rather than to a phantom anonymous caller.
	execCtx := WithAgentID(ctx, req.AgentID)

	// Build a metrics.Event and run dispatch directly. The original
	// routeEntry already evaluated policy and consumed the human's
	// reason; here we just need to fire the tool with the recorded
	// arguments.
	started := time.Now()
	ev := &metrics.Event{
		TS:              started.UnixMilli(),
		AgentID:         req.AgentID,
		Upstream:        entry.upstream,
		ShortName:       entry.originalName,
		ToolName:        entry.tool.Name,
		IsWrite:         !policy.IsReadOnlyName(entry.tool.Name),
		PinnedTool:      IsPinned(entry.tool.Name),
		Via:             "auto-execute",
		ApprovalID:      req.ID,
		Fingerprint:     req.Fingerprint,
		ReasonText:      req.Reason,
		ReasonLen:       len(req.Reason),
		IntentCategory:  req.IntentCategory,
		ApprovalOutcome: metrics.ApprovalApproved,
	}
	res, dispatchErr := g.dispatch(execCtx, entry, req.Arguments, req.AgentID, req.Reason, req.ID, ev)
	ev.TotalLatencyMs = int(time.Since(started).Milliseconds())
	if g.metrics != nil {
		g.metrics.Record(*ev)
	}

	envelope, encErr := encodeApprovalResult(res)
	if encErr != nil {
		_ = g.approval.SetResult(ctx, req.ID, "", false, "encode result: "+encErr.Error())
		return
	}
	var execErr string
	if dispatchErr != nil {
		execErr = dispatchErr.Error()
	}
	isErr := res != nil && res.IsError
	if err := g.approval.SetResult(ctx, req.ID, envelope, isErr, execErr); err != nil {
		log.Printf("auto-execute: persist result %s: %v", req.ID, err)
	}
}

// approvalResultEnvelope is the JSON shape persisted on the approval
// row and rehydrated by both the legacy `_approval_id` re-call path
// and the polling meta-tools. We persist the text content separately
// from structured_content so a client that only reads text content
// still gets a usable answer.
type approvalResultEnvelope struct {
	IsError           bool   `json:"is_error"`
	TextContent       string `json:"text_content,omitempty"`
	StructuredContent any    `json:"structured_content,omitempty"`
	Meta              any    `json:"meta,omitempty"`
}

func encodeApprovalResult(res *mcp.CallToolResult) (string, error) {
	if res == nil {
		return "", nil
	}
	env := approvalResultEnvelope{IsError: res.IsError, StructuredContent: res.StructuredContent}
	if res.Meta != nil {
		env.Meta = res.Meta.AdditionalFields
	}
	var text strings.Builder
	for _, c := range res.Content {
		if t, ok := mcp.AsTextContent(c); ok {
			text.WriteString(t.Text)
		}
	}
	env.TextContent = text.String()
	out, err := json.Marshal(env)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// rehydrateApprovalResult turns a stored envelope back into a
// CallToolResult so the agent's polling/_approval_id path receives an
// answer indistinguishable from running the tool inline.
func rehydrateApprovalResult(envelope string) (*mcp.CallToolResult, error) {
	if envelope == "" {
		return nil, nil
	}
	var env approvalResultEnvelope
	if err := json.Unmarshal([]byte(envelope), &env); err != nil {
		return nil, err
	}
	res := &mcp.CallToolResult{IsError: env.IsError}
	if env.TextContent != "" {
		res = mcp.NewToolResultText(env.TextContent)
		res.IsError = env.IsError
	}
	if env.StructuredContent != nil {
		res.StructuredContent = env.StructuredContent
	}
	if m, ok := env.Meta.(map[string]any); ok && len(m) > 0 {
		res.Meta = &mcp.Meta{AdditionalFields: m}
	}
	return res, nil
}

// slowCallTiers are the absolute elapsed-time thresholds at which the
// watchdog logs that a call is still in flight. The list is short on
// purpose — at the 2m mark the upstream-call-timeout (default 120s) has
// usually already aborted the call; anything past that is a stuck
// built-in or a missing timeout, both worth a noisy log.
var slowCallTiers = []time.Duration{
	5 * time.Second,
	30 * time.Second,
	2 * time.Minute,
	5 * time.Minute,
}

// watchSlowCall logs a "slow-call" line each time the elapsed routing
// time crosses one of slowCallTiers, until done is closed. The lines
// are intentionally structured (key=value) so journalctl | grep
// slow-call gives a clean diagnostic timeline.
func watchSlowCall(done <-chan struct{}, started time.Time, tool, upstream, agentID string) {
	for _, tier := range slowCallTiers {
		wait := tier - time.Since(started)
		if wait <= 0 {
			continue
		}
		t := time.NewTimer(wait)
		select {
		case <-done:
			t.Stop()
			return
		case <-t.C:
			log.Printf("slow-call still in flight: tool=%s upstream=%s agent=%q elapsed=%s tier=%s",
				tool, upstream, agentID,
				time.Since(started).Round(time.Millisecond), tier)
		}
	}
}
