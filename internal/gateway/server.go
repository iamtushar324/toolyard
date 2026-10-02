package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/tusharbhardwaj/toolyard/internal/access"
	"github.com/tusharbhardwaj/toolyard/internal/actor"
	"github.com/tusharbhardwaj/toolyard/internal/approval"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/codemode"
	"github.com/tusharbhardwaj/toolyard/internal/inbox"
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
	// Events Hub — the common-language layer. Pinned so every agent type
	// always sees the brief/query/publish/ack entry points without reaching
	// through tools.search.
	"events.brief":   {},
	"events.query":   {},
	"events.get":     {},
	"events.ack":     {},
	"events.publish": {},
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
	// reasonOptional lets a call omit _reason. Set on the code-mode tools,
	// whose clients are configured for Bifrost and never send one; the
	// nested calls a script makes still carry a reason.
	reasonOptional bool
	// noCallTimeout exempts the dispatch from upstreamCallTimeout for a
	// tool that enforces its own, longer deadline (executeToolCode). The
	// calls it makes in turn are still capped through their own dispatch.
	noCallTimeout bool
}

// Gateway stitches the MCP server, policy, approval bus, memory, and upstream
// pool into one coordinated unit.
type Gateway struct {
	mcp                 *server.MCPServer
	policy              *policy.Engine
	approval            *approval.Bus
	audit               *audit.Logger
	hub                 *realtime.Hub
	memory              *memory.Service
	lake                *lake.Service
	visibility          VisibilityProvider
	usage               UsageRecorder
	metrics             MetricsRecorder
	metricsReader       MetricsLatencyReader
	surface             SurfaceModeProvider
	inLineWait          time.Duration
	maxPendingPerAgent  int
	upstreamCallTimeout time.Duration
	access              access.Resolver
	identity            IdentityResolver

	// inbox, when set, backs the inbox.* tools and grant redemption.
	// approvalMode returns "inbox" (restricted calls are coached towards
	// inbox.request) or anything else (the legacy queue-and-run flow).
	inbox        *inbox.Service
	guide        *inbox.Guide
	approvalMode func() string

	// codeMode backs the Bifrost-compatible listToolFiles / readToolFile /
	// getToolDocs / executeToolCode tools (codemode_tools.go).
	codeMode *codemode.Runtime

	// maxLiveUpstreams caps how many upstream connections are alive
	// simultaneously. 0 = unbounded. When the cap is hit and a new
	// upstream needs a slot, the least-recently-used live upstream is
	// suspended (catalog stays populated, re-dialed transparently on
	// next call). poolMu serializes admission decisions so two
	// concurrent resumes can't both think they have a slot.
	maxLiveUpstreams int
	// maxLivePerUser is the same cap for the per-user connections of
	// per_user upstreams, which form their own pool: people signing in
	// never evict a shared (often stdio) server, and a shared server
	// never evicts a person's session. 0 = unbounded.
	maxLivePerUser int
	poolMu         sync.Mutex

	// inFlight is the live count of routeEntry calls currently executing
	// (not yet returned). Surfaced via /v1/health and the dashboard so a
	// runaway upstream is visible without dumping goroutines.
	inFlight atomic.Int64

	// clientCache remembers each agent's MCP clientInfo; sessionCache
	// remembers which agent owns a `_session_id` (actor.go).
	clientCache
	sessionCache
	// accessToolsState backs the policies.*, servers.*, audit.* and
	// access.* tools (access_tools.go).
	accessToolsState

	// owners resolves a caller to its dashboard user for per_user
	// upstreams; publicURL is where their My connections page lives.
	owners    OwnerResolver
	publicURL string
	// connect and directory back the connections.* tools and the connect
	// links in sign-in refusals (connections_tools.go); nil until
	// SetConnect. Guarded by mu.
	connect   ConnectProvider
	directory ConnectDirectory

	mu        sync.RWMutex
	tools     map[string]toolEntry
	upstreams map[string]*upstream
	// perUser holds the per_user upstreams (peruser.go): one group per
	// upstream name, one connection per user inside it. A name is in
	// either upstreams or perUser, never both.
	perUser map[string]*perUserGroup
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
	Name     string
	Version  string
	Policy   *policy.Engine
	Approval *approval.Bus
	Audit    *audit.Logger
	Hub      *realtime.Hub
	Memory   *memory.Service
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
	// Access, when set, limits each caller to the tool groups its dashboard
	// user may use (admins: everything). nil leaves every tool reachable.
	Access access.Resolver
	// Identity, when set, supplies the caller's per-person key for
	// upstreams whose UpstreamConfig sets IdentityHeader. nil refuses every
	// call to such an upstream.
	Identity IdentityResolver
	// Owners, when set, resolves a caller to its dashboard user for
	// per_user upstreams (peruser.go). Without it the user the ingress put
	// on the raiser is used.
	Owners OwnerResolver
	// PublicURL is the dashboard's public origin, used to point an agent
	// at the My connections page when its owner has not connected a
	// per_user upstream. Optional.
	PublicURL string
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
	g := &Gateway{
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
		access:              opts.Access,
		identity:            opts.Identity,
		owners:              opts.Owners,
		publicURL:           opts.PublicURL,
		tools:               map[string]toolEntry{},
		upstreams:           map[string]*upstream{},
		perUser:             map[string]*perUserGroup{},
	}
	serverOpts := []server.ServerOption{
		server.WithToolCapabilities(true),
		server.WithLogging(),
		server.WithRecovery(),
		server.WithInstructions(buildInstructions(opts.Approval, opts.InLineWait)),
	}
	// The initialize hook remembers what each client says it is, so audit
	// rows can name the client software. Filters run in registration
	// order: access first, so the visibility provider only ever ranks
	// tools the caller may use. The request hook answers raw tools/call
	// for ungranted tools before mcp-go looks the tool up, so the wire
	// response matches an unknown tool.
	hooks := &server.Hooks{}
	hooks.AddAfterInitialize(g.rememberClient)
	if opts.Access != nil {
		serverOpts = append(serverOpts, server.WithToolFilter(g.accessToolFilter))
		hooks.AddOnRequestInitialization(g.accessRequestHook)
	}
	serverOpts = append(serverOpts, server.WithHooks(hooks))
	if opts.Visibility != nil {
		vp := opts.Visibility
		serverOpts = append(serverOpts, server.WithToolFilter(
			func(ctx context.Context, tools []mcp.Tool) []mcp.Tool {
				return vp.List(ctx, tools)
			},
		))
	}
	g.mcp = server.NewMCPServer(opts.Name, opts.Version, serverOpts...)
	return g
}

// accessDeniedReason is the audit reason on every call refused because the
// caller's dashboard user was never granted the tool's group.
const accessDeniedReason = "access: server not granted"

// scopeFor resolves the caller's access scope. Without a resolver every
// caller has the run of the catalog, exactly as before access existed.
func (g *Gateway) scopeFor(ctx context.Context, callerID string) access.Scope {
	if g.access == nil {
		return access.Scope{All: true}
	}
	return g.access.ScopeFor(ctx, callerID)
}

// allowsEntry reports whether callerID may see and call entry.
func (g *Gateway) allowsEntry(ctx context.Context, callerID string, entry toolEntry) bool {
	if g.access == nil {
		return true
	}
	return g.scopeFor(ctx, callerID).AllowsTool(entry.upstream, entry.tool.Name)
}

// accessToolFilter is the tools/list filter installed when a resolver is
// configured. It drops every tool whose group the caller's scope doesn't
// reach; a tool the gateway doesn't know is dropped too (fail closed).
func (g *Gateway) accessToolFilter(ctx context.Context, tools []mcp.Tool) []mcp.Tool {
	scope := g.scopeFor(ctx, agentIDFromContext(ctx))
	if scope.All {
		return tools
	}
	out := make([]mcp.Tool, 0, len(tools))
	g.mu.RLock()
	defer g.mu.RUnlock()
	for _, t := range tools {
		if e, ok := g.tools[t.Name]; ok && scope.AllowsTool(e.upstream, e.tool.Name) {
			out = append(out, t)
		}
	}
	return out
}

// notFoundResult is the answer for an unknown tool. Calls to a tool the
// caller was never granted return the same result, so probing names
// doesn't reveal which servers exist.
func notFoundResult(toolName string) *mcp.CallToolResult {
	return mcp.NewToolResultErrorf("tool %q not found in catalog", toolName)
}

// accessRequestHook runs before mcp-go dispatches any request. For a raw
// tools/call it answers "tool not found" when the caller's scope doesn't
// reach the tool, and for a genuinely unknown tool too, so both come back
// as the same JSON-RPC error and a member can't map which servers exist by
// probing names. mcp-go's own unknown-tool answer is a protocol error, not
// a tool result, and the only pre-lookup seam it offers is this hook (the
// error is rendered by createErrorResponse with INVALID_REQUEST), so the
// hook has to own both cases. Only installed when a resolver is configured;
// without one mcp-go answers unknown tools itself, as before.
func (g *Gateway) accessRequestHook(ctx context.Context, _ any, message any) error {
	raw, ok := message.(json.RawMessage)
	if !ok {
		return nil
	}
	var req struct {
		Method string `json:"method"`
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	if err := json.Unmarshal(raw, &req); err != nil || req.Method != string(mcp.MethodToolsCall) {
		return nil
	}
	g.mu.RLock()
	entry, known := g.tools[req.Params.Name]
	g.mu.RUnlock()
	if known {
		agentID := agentIDFromContext(ctx)
		if g.allowsEntry(ctx, agentID, entry) {
			return nil
		}
		started := time.Now()
		raiser := g.resolveRaiser(ctx, agentID, viaDirect)
		ctx = actor.WithRaiser(ctx, raiser)
		ev := metrics.Event{
			TS:         started.UnixMilli(),
			AgentID:    agentID,
			Upstream:   entry.upstream,
			ShortName:  entry.originalName,
			ToolName:   entry.tool.Name,
			IsWrite:    !policy.IsReadOnlyName(entry.tool.Name),
			PinnedTool: IsPinned(entry.tool.Name),
			Via:        viaDirect,
		}
		stampRaiser(&ev, raiser)
		if g.surface != nil {
			ev.SurfaceMode = g.surface.SurfaceMode(ctx)
		}
		g.denyUngranted(ctx, entry, agentID, "", &ev)
		ev.TotalLatencyMs = int(time.Since(started).Milliseconds())
		if g.metrics != nil {
			g.metrics.Record(ev)
		}
	}
	// Same text mcp-go uses in handleToolCall for a name it doesn't have.
	return fmt.Errorf("tool '%s' not found: %w", req.Params.Name, server.ErrToolNotFound)
}

// denyUngranted answers a call to a tool outside the caller's scope. The
// agent sees an unknown-tool result; the audit log and metrics record the
// real reason so an admin can grant the server if that was the intent.
func (g *Gateway) denyUngranted(ctx context.Context, entry toolEntry, agentID, approvalID string, ev *metrics.Event) *mcp.CallToolResult {
	_ = g.audit.Write(ctx, audit.Event{
		EventType:    audit.EventCallDenied,
		AgentID:      agentID,
		UpstreamName: entry.upstream,
		ToolName:     entry.tool.Name,
		Decision:     "deny",
		Reason:       accessDeniedReason,
		ApprovalID:   approvalID,
	})
	ev.Outcome = metrics.OutcomeDenied
	ev.ErrorClass = "access"
	return notFoundResult(entry.tool.Name)
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
// (tools.search / tools.execute), the Bifrost-compatible code-mode tools,
// the fixture echo tool, and (if a Lake is configured) the lake.*
// personal-data-warehouse tools into the MCP server.
func (g *Gateway) RegisterBuiltins() {
	entries := g.builtinMemoryTools()
	entries = append(entries, g.staticFixtureTool())
	entries = append(entries, g.metaTools()...)
	entries = append(entries, g.approvalMetaTools()...)
	entries = append(entries, g.codeModeTools()...)
	entries = append(entries, g.accessTools()...)
	entries = append(entries, g.connectionsTools()...)
	if g.lake != nil {
		entries = append(entries, g.lakeTools()...)
	}
	for _, e := range entries {
		g.registerEntry(e)
	}
}

// reservedUpstreamName reports whether name belongs to the gateway itself:
// the synthetic upstreams (builtin, fixture, inbox, session, policies,
// servers, audit, access, connections), the meta-tool group "tools", the built-in data
// groups whose tools are registered under the "builtin" upstream (memory,
// lake, events), and "toolyard", the code-mode server every internal tool
// is bound under. An upstream with one of these names would register tools
// under the same prefix as the built-ins and share their access group:
// "tools" is always-on, so a server called tools would be reachable by
// every member without a grant.
//
// notes and skills are deliberately not here: they are real upstreams that
// startup registers under those names (upstreams.UpsertBuiltin), and their
// access group is the upstream name like any other server's.
func reservedUpstreamName(name string) bool {
	switch name {
	case builtinUpstream, "fixture", inboxUpstream, sessionUpstream, "tools", "memory", "lake", "events",
		policiesUpstream, serversUpstream, auditUpstream, accessUpstream, toolyardServer, connectionsGroup:
		return true
	}
	return false
}

// AddUpstream connects to one upstream MCP server, fetches its tool list, and
// wraps each one into the gateway's catalog.
func (g *Gateway) AddUpstream(ctx context.Context, cfg UpstreamConfig) error {
	if cfg.Name == "" {
		return errors.New("upstream needs a name")
	}
	if reservedUpstreamName(cfg.Name) {
		return fmt.Errorf("name %q is reserved", cfg.Name)
	}
	if cfg.PerUser {
		return g.addPerUserUpstream(ctx, cfg)
	}
	// Pre-allocate a pool slot: if the live cap is full this suspends the
	// LRU upstream first so the new one doesn't push us over.
	probe := &upstream{cfg: cfg, pool: g}
	g.acquireSlot(probe)
	u, err := newUpstream(ctx, cfg)
	if err != nil {
		return err
	}
	u.pool = g // so resume() after future idle-kill knows the pool
	tools, err := u.listTools(ctx)
	if err != nil {
		_ = u.close()
		return err
	}
	g.mu.Lock()
	g.upstreams[cfg.Name] = u
	g.mu.Unlock()
	g.registerUpstreamTools(cfg.Name, tools, u)
	return nil
}

// registerUpstreamTools wraps an upstream's tools into the catalog under
// "<name>.<tool>". With ref set, each tool's handler calls that
// connection; with ref nil (a per_user upstream) the handler is left
// empty and dispatch picks the caller's connection.
func (g *Gateway) registerUpstreamTools(name string, tools []mcp.Tool, ref *upstream) {
	for _, t := range tools {
		original := t.Name
		wrapped, field := wrapSchema(t)
		wrapped.Name = name + "." + original
		entry := toolEntry{
			tool:         wrapped,
			upstream:     name,
			originalName: original,
			reasonField:  field,
		}
		if ref != nil {
			entry.handle = func(name string) directHandler {
				return func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
					return ref.callTool(ctx, name, args)
				}
			}(original)
		}
		g.registerEntry(entry)
	}
}

// allUpstreams snapshots every connection in the pool: the shared
// upstreams and each per_user upstream's per-person connections. Idle
// sweeps, the live cap and the health counters treat them alike.
func (g *Gateway) allUpstreams() []*upstream {
	g.mu.RLock()
	out := make([]*upstream, 0, len(g.upstreams))
	for _, u := range g.upstreams {
		out = append(out, u)
	}
	groups := make([]*perUserGroup, 0, len(g.perUser))
	for _, pu := range g.perUser {
		groups = append(groups, pu)
	}
	g.mu.RUnlock()
	for _, pu := range groups {
		out = append(out, pu.all()...)
	}
	return out
}

// Close shuts down all upstream connections for good: each is retired, so
// a dial still in flight closes what it opens instead of keeping it.
func (g *Gateway) Close() error {
	for _, u := range g.allUpstreams() {
		u.retire()
	}
	return nil
}

// RemoveUpstream disconnects an upstream and removes all of its registered
// tools from the gateway catalog (and from the underlying MCP server). For
// a per_user upstream every person's connection is closed.
func (g *Gateway) RemoveUpstream(name string) error {
	g.mu.Lock()
	u, ok := g.upstreams[name]
	pu, okPU := g.perUser[name]
	if !ok && !okPU {
		g.mu.Unlock()
		return ErrUpstreamNotFound
	}
	delete(g.upstreams, name)
	delete(g.perUser, name)
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
	if u != nil {
		// Retire, not just close: a resume dialling right now (an idle
		// stdio server waking up) would otherwise install a client on an
		// upstream nothing tracks any more.
		u.retire()
	}
	if pu != nil {
		pu.closeAll()
	}
	return nil
}

// SweepIdleStdioUpstreams suspends any upstream (stdio or http) whose last
// tool call is older than idleAfter. The upstream's tool catalog entry
// remains intact so agents can still discover the tools; the transport
// restarts automatically on the next callTool. Returns the number of
// upstreams suspended.
//
// Despite the name, this covers HTTP too — kept for backwards compat with
// the old flag (-stdio-idle-timeout). New callers should treat it as
// "SweepIdleUpstreams".
func (g *Gateway) SweepIdleStdioUpstreams(idleAfter time.Duration) int {
	// Snapshot the upstream slice under a short read-lock to avoid holding
	// g.mu while performing the (potentially slow) suspend.
	candidates := g.allUpstreams()

	count := 0
	for _, u := range candidates {
		idle := u.idleSince()
		if !u.suspended() && idle >= idleAfter {
			u.suspend()
			log.Printf("idle-kill: suspended %q (idle %s, transport=%s)",
				u.label(), idle.Round(time.Second), u.cfg.Transport)
			count++
		}
	}
	return count
}

// SetMaxLiveUpstreams configures the cap on simultaneously-live upstreams.
// 0 = unbounded. Safe to call before or after AddUpstream — eviction
// applies on the next acquireSlot.
func (g *Gateway) SetMaxLiveUpstreams(n int) {
	if n < 0 {
		n = 0
	}
	g.poolMu.Lock()
	g.maxLiveUpstreams = n
	g.poolMu.Unlock()
}

// MaxLiveUpstreams returns the configured cap (0 = unbounded). Useful for
// /v1/health output.
func (g *Gateway) MaxLiveUpstreams() int {
	g.poolMu.Lock()
	defer g.poolMu.Unlock()
	return g.maxLiveUpstreams
}

// SetMaxLivePerUser configures the cap on simultaneously-live per-user
// connections (the per_user upstreams' pool). 0 = unbounded.
func (g *Gateway) SetMaxLivePerUser(n int) {
	if n < 0 {
		n = 0
	}
	g.poolMu.Lock()
	g.maxLivePerUser = n
	g.poolMu.Unlock()
}

// MaxLivePerUser returns the per-user pool's cap (0 = unbounded).
func (g *Gateway) MaxLivePerUser() int {
	g.poolMu.Lock()
	defer g.poolMu.Unlock()
	return g.maxLivePerUser
}

// PerUserLiveCount returns how many per-user connections currently hold a
// live transport. Surfaced via /v1/health.
func (g *Gateway) PerUserLiveCount() int {
	n := 0
	for _, u := range g.allUpstreams() {
		if u.userID != "" && !u.suspended() {
			n++
		}
	}
	return n
}

// LiveUpstreamCount returns how many upstreams currently hold a live
// transport. Counterpart to SuspendedUpstreamCount.
func (g *Gateway) LiveUpstreamCount() int {
	n := 0
	for _, u := range g.allUpstreams() {
		if !u.suspended() {
			n++
		}
	}
	return n
}

// SuspendedUpstreamCount returns how many upstreams are currently
// idle-killed or LRU-evicted (catalog still served from cache).
func (g *Gateway) SuspendedUpstreamCount() int {
	n := 0
	for _, u := range g.allUpstreams() {
		if u.suspended() {
			n++
		}
	}
	return n
}

// UpstreamsInBackoff returns how many upstreams are currently in
// circuit-breaker backoff — a recent dial failed and the retry window
// hasn't elapsed, so calls to them fail fast. Surfaced via /v1/health so
// a wedged upstream is visible without reading logs.
func (g *Gateway) UpstreamsInBackoff() int {
	n := 0
	for _, u := range g.allUpstreams() {
		if u.inBackoff() {
			n++
		}
	}
	return n
}

// SessionRecoveries is the number of times an upstream forgot the shared
// session and it was re-established under a tool call, summed over the
// live catalog. Surfaced via /v1/health.
func (g *Gateway) SessionRecoveries() int64 {
	var n int64
	for _, u := range g.allUpstreams() {
		n += u.recoveries.Load()
	}
	return n
}

// acquireSlot is called immediately before an upstream opens (or
// reopens) a transport. If the live cap would be exceeded by counting
// `self` in, the least-recently-used OTHER live upstream is suspended.
// Holding poolMu serializes admission across concurrent resumes; the
// actual suspend() takes only u.mu so there is no deadlock risk.
func (g *Gateway) acquireSlot(self *upstream) {
	victim, live, limit := g.pickEvictionCandidate(self)
	if victim != nil {
		victim.suspend()
		log.Printf("lru-evict: suspended %q to make room for %q (live=%d cap=%d)",
			victim.label(), self.label(), live, limit)
	}
}

// pickEvictionCandidate returns the LRU upstream that should be suspended
// to admit `self`, along with the observed live count and cap. Returns
// (nil, _, _) when no eviction is needed (cap=0 or count<cap). A per-user
// connection (self.userID set) is admitted against the per-user pool and
// its cap, a shared upstream against the shared pool: neither ever evicts
// from the other. Separated from acquireSlot so unit tests can exercise
// the selection without needing real mcp-go clients to .Close().
func (g *Gateway) pickEvictionCandidate(self *upstream) (*upstream, int, int) {
	perUser := self.userID != ""
	g.poolMu.Lock()
	limit := g.maxLiveUpstreams
	if perUser {
		limit = g.maxLivePerUser
	}
	g.poolMu.Unlock()
	if limit <= 0 {
		return nil, 0, 0
	}
	var candidates []*upstream
	for _, u := range g.allUpstreams() {
		if u != self && (u.userID != "") == perUser {
			candidates = append(candidates, u)
		}
	}

	var lru *upstream
	live := 0
	var oldest int64 = math.MaxInt64
	for _, u := range candidates {
		if u.suspended() {
			continue
		}
		live++
		t := u.lastUsed.Load()
		if t < oldest {
			oldest = t
			lru = u
		}
	}
	if live >= limit && lru != nil {
		return lru, live, limit
	}
	return nil, live, limit
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

// Catalog returns the flat list of registered tools, unfiltered. It is for
// admin surfaces (the dashboard's /v1/tools); anything an agent or member
// reads goes through CatalogFor.
func (g *Gateway) Catalog() []CatalogEntry {
	return g.catalog(func(toolEntry) bool { return true })
}

// CatalogFor returns the catalog limited to the tool groups the ctx caller
// (agentIDFromContext) may use. Without a resolver it equals Catalog.
func (g *Gateway) CatalogFor(ctx context.Context) []CatalogEntry {
	if g.access == nil {
		return g.Catalog()
	}
	scope := g.scopeFor(ctx, agentIDFromContext(ctx))
	if scope.All {
		return g.Catalog()
	}
	return g.catalog(func(e toolEntry) bool { return scope.AllowsTool(e.upstream, e.tool.Name) })
}

func (g *Gateway) catalog(keep func(toolEntry) bool) []CatalogEntry {
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := make([]CatalogEntry, 0, len(g.tools))
	for _, e := range g.tools {
		if !keep(e) {
			continue
		}
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
// Approval, policy.Eval, the per-agent budget and the per-user access check
// are all skipped — callers must already be confident the operation is
// safe, and the ctx here carries toolyard's own identity, not a member's.
// Schema-wrap reason extraction is also skipped; the caller is responsible
// for whatever shape the underlying tool expects.
//
// Returns the tool result (possibly with IsError=true) just like a regular
// call. Errors surface dispatch / upstream connection failures only.
func (g *Gateway) CallInternal(ctx context.Context, viaTool, targetName string, args map[string]any) (*mcp.CallToolResult, error) {
	g.mu.RLock()
	entry, ok := g.tools[targetName]
	g.mu.RUnlock()
	if !ok {
		return notFoundResult(targetName), nil
	}

	started := time.Now()
	agentID := agentIDFromContext(ctx)
	raiser := g.resolveRaiser(ctx, agentID, viaTool)
	ctx = actor.WithRaiser(ctx, raiser)
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
	stampRaiser(ev, raiser)
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
//   - Unknown tool, or a tool outside the caller's access scope -> the same
//     not-found error CallToolResult.
//   - Reads pass straight through.
//   - Writes hold for approval (in-line then deferred), exactly like a direct
//     call would.
//
// `viaTool` is the path in, recorded on the audit rows, the approval and
// the metric as Via: the meta-tool name for tools.execute, "dashboard",
// "cli" or "voice" for those callers.
func (g *Gateway) RouteCall(ctx context.Context, viaTool, targetName string, args map[string]any) (*mcp.CallToolResult, error) {
	g.mu.RLock()
	entry, ok := g.tools[targetName]
	g.mu.RUnlock()
	if !ok {
		return notFoundResult(targetName), nil
	}
	if targetName == viaTool {
		return mcp.NewToolResultError("tools.execute cannot target itself"), nil
	}
	return g.routeEntry(ctx, entry, viaTool, args)
}

// routeEntry is the shared path used by both the MCP-side handler and
// tools.execute. It assumes entry is a registered toolEntry. The metric
// Event captured here is emitted to the metrics sink (if any) once the
// terminal outcome of this call is known. via is the path in ("direct"
// from the MCP handler).
func (g *Gateway) routeEntry(ctx context.Context, entry toolEntry, via string, args map[string]any) (res *mcp.CallToolResult, err error) {
	started := time.Now()
	agentID := agentIDFromContext(ctx)

	// Who is calling: the ingress raiser (or the caller id alone), the
	// client that introduced itself at initialize, and the agent session
	// the call names. It rides on ctx so every audit row below, the
	// approval and the upstream handler see the same answer.
	raiser := g.resolveRaiser(ctx, agentID, via)
	g.attachSession(ctx, &raiser, args)
	ctx = actor.WithRaiser(ctx, raiser)

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
		Via:        via,
	}
	stampRaiser(&ev, raiser)
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

	// Per-user access comes first: a member whose admin never granted this
	// tool's server can't resume, redeem a grant for, or even be told about
	// a tool there. The result matches an unknown tool.
	if !g.allowsEntry(ctx, agentID, entry) {
		return g.denyUngranted(ctx, entry, agentID, "", &ev), nil
	}

	// Deferred-resume short-circuit. The approval coordination tools take
	// approval ids as their own arguments and have no held call of their
	// own to resume, so a stray _approval_id there (a model mixing it up
	// with approval_id) is dropped with the other meta fields and the tool
	// runs, checking the caller against the ids it was given.
	if approvalID, ok := args[ApprovalIDField].(string); ok && approvalID != "" && !approvalCoordinationTools[entry.tool.Name] {
		ev.ApprovalID = approvalID
		if entry.tool.Name == MetaExecuteTool {
			return g.resumeViaExecute(ctx, entry, args, approvalID, &ev)
		}
		return g.resumeDeferred(ctx, entry, approvalID, &ev)
	}

	grantToken, _ := args[GrantField].(string)
	reason, intent, cleanArgs, rerr := extractReasonOpt(args, entry.reasonField, entry.reasonOptional)
	if rerr != nil {
		_ = g.audit.Write(ctx, audit.Event{
			EventType:     audit.EventCallFailed,
			AgentID:       agentID,
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
			AgentID:      agentID,
			UpstreamName: entry.upstream,
			// Wrapped catalog name — matches tool_policies targets and the
			// IsReadOnlyName/IsWrite computation used everywhere else.
			ToolName:       entry.tool.Name,
			IntentCategory: intent,
			Arguments:      cleanArgs,
			UserReason:     reason,
		})
	}

	// A grant only matters for calls that would otherwise need approval:
	// an explicit deny still wins, and an open tool doesn't use one up.
	if grantToken != "" && decision.Action == policy.ActionApprove && g.inbox != nil {
		return g.redeemAndDispatch(ctx, entry, cleanArgs, agentID, reason, grantToken, &ev)
	}

	// The policy rule is the decider of an allow or deny row.
	policyDecider := actor.Decider{Via: actor.ViaPolicy, Ref: decision.RuleID}
	switch decision.Action {
	case policy.ActionAllow:
		allowed := audit.Event{
			EventType:    audit.EventCallAllowed,
			AgentID:      agentID,
			UpstreamName: entry.upstream,
			ToolName:     entry.tool.Name,
			Decision:     string(decision.Action),
			Reason:       reason,
		}
		allowed.SetDecider(policyDecider)
		_ = g.audit.Write(ctx, allowed)
		ev.ApprovalOutcome = metrics.ApprovalNone
		return g.dispatch(ctx, entry, cleanArgs, agentID, reason, "", &ev)
	case policy.ActionDeny:
		denied := audit.Event{
			EventType:    audit.EventCallDenied,
			AgentID:      agentID,
			UpstreamName: entry.upstream,
			ToolName:     entry.tool.Name,
			Decision:     string(decision.Action),
			Reason:       reason,
		}
		denied.SetDecider(policyDecider)
		_ = g.audit.Write(ctx, denied)
		ev.Outcome = metrics.OutcomeDenied
		ev.ErrorClass = "policy"
		return mcp.NewToolResultError("denied by policy: " + decision.Reason), nil
	case policy.ActionApprove:
		// Inbox mode needs an agent identity: grants are bound to it.
		// Anonymous callers keep the legacy flow.
		if g.inboxMode() && agentID != "" {
			return g.coach(ctx, entry, cleanArgs, agentID, reason, &ev), nil
		}
		return g.holdAndWait(ctx, entry, cleanArgs, agentID, reason, intent, decision.RequireHuman, &ev)
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
// hidden tool. A caller outside the tool's access scope skips the hint and
// falls through to routeEntry's not-found answer, so the hint never
// confirms that an ungranted server exists.
func (g *Gateway) handlerFor(toolName string) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		g.mu.RLock()
		entry, ok := g.tools[toolName]
		g.mu.RUnlock()
		if !ok {
			return mcp.NewToolResultErrorf("tool %q not registered", toolName), nil
		}
		if g.visibility != nil && !g.visibility.IsVisible(ctx, entry.tool.Name) &&
			g.allowsEntry(ctx, agentIDFromContext(ctx), entry) {
			return mcp.NewToolResultErrorf(
				"tool %q is hidden by the current agent-surface mode. Use tools.execute with tool=%q to invoke it.",
				toolName, toolName,
			), nil
		}
		args := argsAsMap(request.Params.Arguments)
		return g.routeEntry(ctx, entry, viaDirect, args)
	}
}

func (g *Gateway) holdAndWait(ctx context.Context, entry toolEntry, args map[string]any,
	agentID, reason, intent string, requireHuman bool, ev *metrics.Event) (*mcp.CallToolResult, error) {

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
		RequireHuman:   requireHuman,
		RaisedBy:       raiserOnCtx(ctx, agentID),
	}, g.inLineWait)
	if err != nil {
		ev.Outcome = metrics.OutcomeError
		ev.ErrorClass = "approval"
		return mcp.NewToolResultErrorFromErr("approval hold failed", err), nil
	}
	ev.ApprovalID = req.ID
	if req.Coalesced {
		ev.CoalescedInto = req.ID
	}
	// Decided during the hold (an auto-rule, or a person within the
	// in-line wait): the row and the metric say by whom.
	decider := approvalDecider(req)
	if req.Status == approval.StatusAllowed || req.Status == approval.StatusDenied {
		approvalMetrics(ev, req)
	}

	switch req.Status {
	case approval.StatusAllowed:
		allowed := audit.Event{
			EventType:    audit.EventCallAllowed,
			AgentID:      agentID,
			UpstreamName: entry.upstream,
			ToolName:     entry.tool.Name,
			Decision:     "allow",
			ApprovalID:   req.ID,
			Reason:       reason,
		}
		allowed.SetDecider(decider)
		_ = g.audit.Write(ctx, allowed)
		ev.ApprovalLatencyMs = int(time.Since(holdStart).Milliseconds())
		if req.AutoDecidedBy != "" {
			ev.ApprovalOutcome = metrics.ApprovalAuto
		} else {
			ev.ApprovalOutcome = metrics.ApprovalApproved
		}
		if req.AutoDecidedBy != "" {
			// Inline auto-approve: decideInline ran with runExec=false, so
			// NO background executor was scheduled — we dispatch here. We
			// MUST then persist the result, otherwise the row stays
			// allowed with result_executed_at IS NULL and SweepUnexecuted
			// re-fires the executor (re-running the write) on the next
			// gateway restart.
			res, derr := g.dispatch(ctx, entry, args, agentID, reason, req.ID, ev)
			g.persistApprovalResult(ctx, req.ID, res, derr)
			return res, derr
		}
		// Legacy `-in-line-wait > 0` path: a human tapped Allow during the
		// wait, so Decide already fired the background executor, which
		// dispatches AND persists. Dispatching again here would double-run
		// the tool. Surface the executor's result (or an "executing"
		// envelope if it hasn't finished) via the same rehydrate path the
		// deferred `_approval_id` re-call uses.
		return g.resumeDeferred(ctx, entry, req.ID, ev)
	case approval.StatusDenied:
		denied := audit.Event{
			EventType:    audit.EventCallDenied,
			AgentID:      agentID,
			UpstreamName: entry.upstream,
			ToolName:     entry.tool.Name,
			Decision:     "deny",
			ApprovalID:   req.ID,
			Reason:       reason,
		}
		denied.SetDecider(decider)
		_ = g.audit.Write(ctx, denied)
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
//
// The approval must belong to the caller (approvalVisibleTo) and must
// have been raised for this tool on this upstream. Otherwise any agent
// that can reach one tool could read another agent's cached result, or
// the result of a tool it can't reach, by id. Both mismatches answer
// exactly like an unknown id, so ids can't be probed.
func (g *Gateway) resumeDeferred(ctx context.Context, entry toolEntry, approvalID string, ev *metrics.Event) (*mcp.CallToolResult, error) {
	req, err := g.approval.Get(ctx, approvalID)
	if err != nil || !approvalVisibleTo(req, agentIDFromContext(ctx)) || !approvalRaisedFor(req, entry) {
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
		} else {
			ev.ApprovalOutcome = metrics.ApprovalApproved
		}
		ev.ApprovalLatencyMs = int(time.Now().UnixMilli() - req.CreatedAt)
		approvalMetrics(ev, req)
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

// approvalRaisedFor reports whether req was raised for entry: the same
// upstream and the same wrapped catalog name holdAndWait stores.
func approvalRaisedFor(req *approval.Request, entry toolEntry) bool {
	return req.UpstreamName == entry.upstream && req.ToolName == entry.tool.Name
}

// resumeViaExecute handles a top-level _approval_id on tools.execute.
// tools.execute only proxies, so the id is normally one its target raised:
// the resume is routed to that target, where the target's access check and
// resumeDeferred's owner and tool checks apply against the real tool. The
// target is the call's `tool`, or the approval's own tool when the call
// names only the id, as the e2e helper and older agents re-call
// tools.execute. An id raised for tools.execute itself (an operator gated
// it with a tool rule) resumes here, and so does an unknown id or another
// agent's, which resumeDeferred answers as unknown.
func (g *Gateway) resumeViaExecute(ctx context.Context, entry toolEntry, args map[string]any, approvalID string, ev *metrics.Event) (*mcp.CallToolResult, error) {
	req, err := g.approval.Get(ctx, approvalID)
	if err != nil || !approvalVisibleTo(req, agentIDFromContext(ctx)) || approvalRaisedFor(req, entry) {
		return g.resumeDeferred(ctx, entry, approvalID, ev)
	}
	target, _ := args["tool"].(string)
	if strings.TrimSpace(target) == "" {
		target = req.ToolName
	}
	// ctx still carries the caller's agent id and raiser.
	return g.RouteCall(ctx, MetaExecuteTool, target, map[string]any{ApprovalIDField: approvalID})
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

	// u is the live upstream behind this entry; nil for built-ins, the
	// fixture and anything else that isn't in the pool. pu is set instead
	// when the entry's upstream is per_user: the connection is the
	// caller's own.
	g.mu.RLock()
	u := g.upstreams[entry.upstream]
	pu := g.perUser[entry.upstream]
	g.mu.RUnlock()
	// asUser is the person whose account a per_user call runs as. The
	// audit rows below carry it as the owner, so the trail says whose
	// token did the work even when the ingress named nobody.
	var asUser string
	var cfg *UpstreamConfig
	switch {
	case u != nil:
		cfg = &u.cfg
	case pu != nil:
		cfg = &pu.cfg
	}
	// A shared OAuth server with no usable token is refused before the
	// dial, with a connect link for an admin owner (connections_tools.go).
	if u != nil {
		if res := g.refuseSharedSignIn(ctx, u, entry, agentID, reason, approvalID, nil, ev); res != nil {
			return res, nil
		}
	}
	if entry.handle == nil {
		// Upstream-backed tool — route through the upstream pool.
		switch {
		case pu != nil:
			// Per-user: the owner's connection or nothing. Never a shared
			// token, never somebody else's.
			handle, uid, err := g.perUserHandle(ctx, pu, entry, agentID)
			if err != nil {
				return g.refusePerUser(ctx, entry, agentID, reason, approvalID, uid, err, ev), nil
			}
			entry.handle = handle
			asUser = uid
		case u == nil:
			ev.Outcome = metrics.OutcomeError
			ev.ErrorClass = "upstream_not_connected"
			return mcp.NewToolResultErrorf("upstream %q not connected", entry.upstream), nil
		default:
			entry.handle = func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
				return u.callTool(ctx, entry.originalName, args)
			}
		}
	}
	upstreamStart := time.Now()
	callCtx := ctx
	if g.upstreamCallTimeout > 0 && !entry.noCallTimeout {
		var cancel context.CancelFunc
		callCtx, cancel = context.WithTimeout(ctx, g.upstreamCallTimeout)
		defer cancel()
	}
	// Identity forwarding: a tool on an upstream that names an identity
	// header runs as the caller or not at all. This is the one place every
	// execution path (direct call, tools.execute, auto-approve, inbox
	// grant, the approval bus and CallInternal) passes through, so it is
	// the one place the rule lives. The key travels on callCtx to the
	// transport's per-request header function; only this tools/call sees
	// it.
	if cfg != nil && cfg.IdentityHeader != "" {
		key, err := g.forwardKey(ctx, agentID)
		if err != nil {
			return g.refuseWithoutIdentity(ctx, entry, agentID, reason, approvalID, err, ev), nil
		}
		callCtx = WithForwardedKey(callCtx, key)
	}
	// The validated reason rides along for handlers that make calls of
	// their own (tools.execute, executeToolCode): extractReason stripped it
	// from args, and the nested call must carry it again.
	callCtx = withCallReason(callCtx, reason)
	res, err := entry.handle(callCtx, args)
	ev.UpstreamLatencyMs = int(time.Since(upstreamStart).Milliseconds())
	if errors.Is(err, context.DeadlineExceeded) {
		log.Printf("upstream-timeout: tool=%s upstream=%s agent=%s elapsed=%s timeout=%s",
			entry.tool.Name, entry.upstream, agentID,
			time.Since(upstreamStart).Round(time.Millisecond), g.upstreamCallTimeout)
	}
	if err != nil {
		// A shared OAuth server that answered 401: the token is dead, so
		// the agent gets the sign-in guidance instead of a bare failure.
		if u != nil && isUnauthorized(err) {
			if res := g.refuseSharedSignIn(ctx, u, entry, agentID, reason, approvalID, err, ev); res != nil {
				return res, nil
			}
		}
		_ = g.audit.Write(ctx, audit.Event{
			EventType:     audit.EventCallFailed,
			AgentID:       agentID,
			UpstreamName:  entry.upstream,
			ToolName:      entry.tool.Name,
			Reason:        reason,
			ApprovalID:    approvalID,
			ResultSummary: err.Error(),
			Raiser:        actor.Raiser{OwnerUserID: asUser},
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
		Raiser:        actor.Raiser{OwnerUserID: asUser},
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

// forwardKey resolves the caller's identity key for an identity-forwarding
// upstream. Without a resolver there is no key for anyone, so every such
// call is refused rather than made under the gateway's shared credentials.
func (g *Gateway) forwardKey(ctx context.Context, callerID string) (string, error) {
	if g.identity == nil {
		return "", errors.New("identity forwarding is not configured on this gateway")
	}
	return g.identity.ForwardKey(ctx, callerID)
}

// refuseWithoutIdentity is the terminal outcome of a call to an
// identity-forwarding upstream whose caller has no usable key: the
// upstream is never contacted, the audit row and the metric say why, and
// the agent gets a message it can act on. Key material never appears in
// any of them.
func (g *Gateway) refuseWithoutIdentity(ctx context.Context, entry toolEntry, agentID, reason, approvalID string,
	cause error, ev *metrics.Event) *mcp.CallToolResult {
	var msg string
	if errors.Is(cause, ErrNoIdentityKey) {
		msg = fmt.Sprintf("%s records who made each change, and your toolyard user has no Beknown key yet. "+
			"Ask a toolyard admin to provision one (Users page).", entry.upstream)
	} else {
		msg = fmt.Sprintf("%s needs your identity key and toolyard could not resolve it; the call was not made.", entry.upstream)
		log.Printf("identity-forward: tool=%s upstream=%s agent=%s refused: %v", entry.tool.Name, entry.upstream, agentID, cause)
	}
	_ = g.audit.Write(ctx, audit.Event{
		EventType:     audit.EventCallFailed,
		AgentID:       agentID,
		UpstreamName:  entry.upstream,
		ToolName:      entry.tool.Name,
		Reason:        reason,
		ApprovalID:    approvalID,
		ResultSummary: "identity: " + cause.Error(),
	})
	ev.Outcome = metrics.OutcomeError
	ev.ErrorClass = "identity"
	return mcp.NewToolResultError(msg)
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
		"how_to_get_the_result":   "Poll this approval_id with tools.poll_approval (or block with tools.wait_for_approval). The response will carry the executed tool's result under `result` once the executor finishes. Do NOT re-call the original tool — toolyard already runs it for you on approve.",
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
	b.WriteString("PERMISSIONS: some tools are restricted and need your owner's approval. Before a task, run `inbox.check` on the calls you plan, then ask for every restricted tool in ONE `inbox.request` (call `inbox.guide` first to learn the format: a first-person message, a short voice-note script, evidence attachments, and each tool with its parameters). When approved, call each tool with `_grant` set to its token. If a call returns status `permission_required`, nothing ran: fill in the draft it gives you and send it with inbox.request. ")
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
	b.WriteString("Use `tools.search` and `tools.execute` to discover and proxy tools that aren't directly visible in your catalog. ")
	b.WriteString("SESSIONS: after `session.start`, pass its id as `_session_id` on your tool calls so the audit log ties each call to that piece of work; it is stripped before the tool sees it and ignored if the session isn't yours.")
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
// Status=allowed — the gating decision is final. Per-user access is the
// one check that runs again: an admin may have withdrawn the agent's
// owner from this server while the request waited, and the approval must
// not outlive that. We still go through dispatch() so audit, metrics, the
// upstream-call timeout, and the slow-call watchdog all apply just like a
// normal call.
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
	// Stamp the agent ID and the raiser snapshot the request kept, so
	// audit / usage rows attribute the call to the originating agent,
	// owner, client and session rather than to a phantom anonymous
	// caller on the bus's background context.
	var raiser actor.Raiser
	if req.RaisedBy != nil {
		raiser = *req.RaisedBy
	}
	raiser = g.resolveRaiser(actor.WithRaiser(ctx, raiser), req.AgentID, "")
	execCtx := actor.WithRaiser(WithAgentID(ctx, req.AgentID), raiser)

	// Build a metrics.Event and run dispatch directly. The original
	// routeEntry already evaluated policy and consumed the human's
	// reason; here we just need to fire the tool with the recorded
	// arguments.
	started := time.Now()
	if !g.allowsEntry(execCtx, req.AgentID, entry) {
		ev := &metrics.Event{
			TS: started.UnixMilli(), AgentID: req.AgentID, Upstream: entry.upstream, ShortName: entry.originalName,
			ToolName: entry.tool.Name, IsWrite: !policy.IsReadOnlyName(entry.tool.Name), PinnedTool: IsPinned(entry.tool.Name),
			Via: "auto-execute", ApprovalID: req.ID, Fingerprint: req.Fingerprint, ApprovalOutcome: metrics.ApprovalApproved,
		}
		stampRaiser(ev, raiser)
		approvalMetrics(ev, req)
		g.denyUngranted(execCtx, entry, req.AgentID, req.ID, ev)
		ev.TotalLatencyMs = int(time.Since(started).Milliseconds())
		if g.metrics != nil {
			g.metrics.Record(*ev)
		}
		res := mcp.NewToolResultErrorf("access to %s was removed while this approval waited; nothing ran",
			access.GroupOf(entry.upstream, entry.tool.Name))
		g.persistApprovalResult(ctx, req.ID, res, nil)
		return
	}
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
	stampRaiser(ev, raiser)
	approvalMetrics(ev, req)
	res, dispatchErr := g.dispatch(execCtx, entry, req.Arguments, req.AgentID, req.Reason, req.ID, ev)
	ev.TotalLatencyMs = int(time.Since(started).Milliseconds())
	if g.metrics != nil {
		g.metrics.Record(*ev)
	}
	g.persistApprovalResult(ctx, req.ID, res, dispatchErr)
}

// persistApprovalResult encodes a dispatched tool result and stores it on
// the approval row via bus.SetResult, so pollers (and the legacy
// `_approval_id` re-call) can rehydrate it and SweepUnexecuted won't
// re-fire the executor after a restart. Shared by the bus-driven Execute
// hook and holdAndWait's inline auto-approve path. SetResult is gated on
// result_executed_at IS NULL, so a racing second call is a harmless no-op.
func (g *Gateway) persistApprovalResult(ctx context.Context, approvalID string, res *mcp.CallToolResult, dispatchErr error) {
	if g.approval == nil {
		return
	}
	envelope, encErr := encodeApprovalResult(res)
	if encErr != nil {
		if err := g.approval.SetResult(ctx, approvalID, "", false, "encode result: "+encErr.Error()); err != nil {
			log.Printf("approval-persist: %s: %v", approvalID, err)
		}
		return
	}
	var execErr string
	if dispatchErr != nil {
		execErr = dispatchErr.Error()
	}
	isErr := res != nil && res.IsError
	if err := g.approval.SetResult(ctx, approvalID, envelope, isErr, execErr); err != nil {
		log.Printf("approval-persist: %s: %v", approvalID, err)
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
	env := approvalResultEnvelope{IsError: res.IsError}
	// The row is read by admins and operator tokens and replayed on every
	// poll, so a connect link (minted for the one person the call was
	// refused for) is taken out; everything else is stored verbatim.
	if res.StructuredContent != nil {
		if raw, err := json.Marshal(res.StructuredContent); err == nil {
			env.StructuredContent = json.RawMessage(audit.RedactConnectLinks(string(raw)))
		} else {
			env.StructuredContent = res.StructuredContent
		}
	}
	if res.Meta != nil {
		env.Meta = res.Meta.AdditionalFields
	}
	var text strings.Builder
	for _, c := range res.Content {
		if t, ok := mcp.AsTextContent(c); ok {
			text.WriteString(t.Text)
		}
	}
	env.TextContent = audit.RedactConnectLinks(text.String())
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
