package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/tusharbhardwaj/toolyard/internal/access"
	"github.com/tusharbhardwaj/toolyard/internal/actor"
	"github.com/tusharbhardwaj/toolyard/internal/approval"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/cluster"
	"github.com/tusharbhardwaj/toolyard/internal/codemode"
	"github.com/tusharbhardwaj/toolyard/internal/goroutines"
	"github.com/tusharbhardwaj/toolyard/internal/inbox"
	"github.com/tusharbhardwaj/toolyard/internal/jobs"
	"github.com/tusharbhardwaj/toolyard/internal/logx"
	"github.com/tusharbhardwaj/toolyard/internal/memory"
	"github.com/tusharbhardwaj/toolyard/internal/metrics"
	"github.com/tusharbhardwaj/toolyard/internal/opsmetrics"
	"github.com/tusharbhardwaj/toolyard/internal/policy"
	"github.com/tusharbhardwaj/toolyard/internal/realtime"
	"github.com/tusharbhardwaj/toolyard/internal/toolsearch"
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
	// restartRetryAfterSeconds is the retry hint in a drain refusal.
	restartRetryAfterSeconds = 5
	// persistTimeout bounds recording an executed approval's outcome on a
	// context that outlives the executor's.
	persistTimeout = 5 * time.Second
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
	personalGitHub bool
	requireHuman   bool
	tool           mcp.Tool
	upstream       string // "builtin", "fixture", or upstream config name
	originalName   string // upstream-side name (without prefix)
	reasonField    string // "_reason" (or "__toolyard_reason" if a clash forced a rename)
	handle         directHandler
	// forcedAction, when non-nil, short-circuits policy.Eval for this tool
	// and uses the provided action instead. Used by built-in tools that
	// need a deterministic policy decision regardless of the name-heuristic
	// (e.g., notes.publish looks like a write but is intentionally
	// auto-allowed). The intent_category supplied by the agent is ignored
	// when forcedAction is set.
	forcedAction *policy.Action
	// denyOverridesForced lets an explicit deny policy, on the tool or on
	// its upstream, beat forcedAction (policy.Engine.ExplicitDeny). For
	// data tools an owner must be able to switch off; no built-in sets it
	// today. Control-plane tools (inbox.*, policies.*, ...) keep ignoring
	// deny, so one rule can never lock every agent out.
	denyOverridesForced bool
	// escalate, when non-nil, reads a call's arguments after the policy
	// decision: if that decision is allow and escalate returns true, the
	// call needs approval instead, with the returned reason. It sends the
	// data-replacing or externally reaching forms of an open tool to a
	// person.
	escalate func(args map[string]any) (string, bool)
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
	githubHTTP    *http.Client
	githubWriteMu sync.Mutex
	// cluster, when it spans several nodes, extends githubWriteMu and the
	// client cache to every replica (SetCluster, actor.go).
	cluster             *cluster.Cluster
	mcp                 *server.MCPServer
	policy              *policy.Engine
	approval            *approval.Bus
	audit               *audit.Logger
	hub                 *realtime.Hub
	memory              *memory.Service
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
	upstreamProtocol    string

	// safeToolNames publishes OpenAI-safe tool names in tools/list and
	// the texts a client reads (tool_names.go). catalogGen counts changes
	// to tools (catalogChanged); toolNameCache holds the published names
	// of the generation it was built from.
	safeToolNames bool
	catalogGen    atomic.Uint64
	toolNameCache atomic.Pointer[toolNameTable]

	// inbox, when set, backs the inbox.* tools and grant redemption.
	// approvalMode returns "inbox" (restricted calls are coached towards
	// inbox.request) or anything else (the legacy queue-and-run flow).
	inbox        *inbox.Service
	guide        *inbox.Guide
	approvalMode func() string

	// codeMode backs the Bifrost-compatible listToolFiles / readToolFile /
	// getToolDocs / executeToolCode tools (codemode_tools.go).
	codeMode *codemode.Runtime
	// search ranks the catalog for tools.search and GET /v1/code/search
	// (meta.go); nil until first use or SetToolSearch.
	search atomic.Pointer[toolsearch.Engine]

	// maxLiveUpstreams caps how many upstream connections are alive
	// simultaneously. 0 = unbounded. When the cap is hit and a new
	// upstream needs a slot, the least-recently-used live upstream is
	// suspended (catalog stays populated, re-dialed transparently on
	// next call). Active RPCs remain pinned: simultaneous calls can exceed
	// this idle-pool cap, and release trims the excess idle transports.
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

	// Shutdown (TEC-1626). drainActive makes routeEntry refuse new calls
	// with a retryable answer (BeginDrain). drainRelease, closed by
	// ReleaseWaiters, ends the approval wait loops. grantCalls tracks
	// granted (Inbox) executions so shutdown can wait for them.
	drainActive   atomic.Bool
	drainMu       sync.Mutex
	drainRelease  chan struct{}
	drainReleased bool
	grantCalls    goroutines.Tracker

	// clientCache remembers each agent's MCP clientInfo; sessionCache
	// remembers which agent owns a `_session_id` (actor.go).
	clientCache
	sessionCache
	// accessToolsState backs the policies.*, servers.*, audit.* and
	// access.* tools (access_tools.go).
	accessToolsState
	// jobsState backs async tools and the jobs.* tools (jobs_tools.go).
	jobsState
	// skillHub backs the skills.* tools and the MCP Skills extension
	// (builtin_skills.go); nil while the skills hub is off.
	skillHub atomic.Pointer[skillHubConfig]

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
	// serverDescs is the admin's description of each upstream, by name
	// (SetServerDescription, server_descriptions.go).
	serverDescs map[string]string

	// catalogMu serialises the steps that install or drop a shared
	// upstream's whole catalog: AddUpstream's register, RemoveUpstream and
	// a catalog refresh. A refresh read from a connection that was removed
	// or replaced meanwhile is then dropped, not applied. Taken before mu,
	// never while holding it.
	catalogMu sync.Mutex
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
	// UpstreamProtocol is "" (every upstream speaks 2025-11-25, as before
	// 2026-07-28 support) or ProtocolAuto (an http upstream that offers
	// 2026-07-28 is used statelessly; see requestedProtocol).
	UpstreamProtocol string
	// Jobs, when set, turns on async tools: the jobs.* tools, MCP Tasks
	// and the async setting of upstream servers (jobs_tools.go). nil
	// leaves every call inline, as before.
	Jobs *jobs.Service
	// SafeToolNames publishes every tool in tools/list under a name that
	// matches ^[a-zA-Z0-9_-]{1,64}$ ("inbox.request" is listed as
	// "inbox_request"), and the server instructions and the descriptions
	// of the listed built-in tools name tools that way (tool_names.go).
	// Calls by a published name work whether or not it is set. serve sets
	// it from -mcp-safe-tool-names (default on); false lists the canonical
	// (dotted) names, as before.
	SafeToolNames bool
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
		upstreamProtocol:    opts.UpstreamProtocol,
		safeToolNames:       opts.SafeToolNames,
		jobsState:           jobsState{jobs: opts.Jobs},
		tools:               map[string]toolEntry{},
		upstreams:           map[string]*upstream{},
		perUser:             map[string]*perUserGroup{},
	}
	g.grantCalls.Executions = true
	// No logging capability: the gateway never sends log notifications, so
	// it no longer advertises them. tools/list is filtered per caller, so
	// its cache hint (sent to 2026-07-28 clients only) is private.
	serverOpts := []server.ServerOption{
		server.WithToolCapabilities(true),
		server.WithToolHandlerMiddleware(recoverToolPanics),
		server.WithInstructions(buildInstructions(opts.Approval, opts.InLineWait, opts.SafeToolNames)),
		server.WithMethodCacheHints(mcp.MethodToolsList, toolsListCacheTTL.Milliseconds(), mcp.CacheScopePrivate),
	}
	// The initialize hook remembers what each client says it is, so audit
	// rows can name the client software (a 2026-07-28 client sends no
	// initialize; resolveRaiser reads its clientInfo from the request).
	// The request hook answers raw tools/call for ungranted tools before
	// mcp-go looks the tool up, so the wire response matches an unknown
	// tool.
	hooks := &server.Hooks{}
	hooks.AddAfterInitialize(g.rememberClient)
	if opts.Access != nil {
		hooks.AddOnRequestInitialization(g.accessRequestHook)
	}
	// A tools/call may name a tool by its published name (tool_names.go):
	// it becomes the canonical name before mcp-go looks the tool up, so
	// the registered tool runs and every later step sees the canonical
	// name. Always on: a client that listed published names keeps working
	// when an operator turns -mcp-safe-tool-names off.
	hooks.AddBeforeCallTool(g.resolveCallName)
	// The access and visibility filters shape tools/list only. They are a
	// list hook, not server.WithToolFilter: mcp-go v1 also runs tool
	// filters on every tools/call, which would refuse a tool the
	// visibility mode hides from the list (router_only, top_n) before
	// handlerFor can answer or audit it. Access first, so the visibility
	// provider only ever ranks tools the caller may use. The tool list
	// (tool_list.go) comes next: an agent on the small list (the default)
	// sees only SmallToolList, of what access kept, and reaches the rest
	// through search and code mode; the hidden tools stay callable, which
	// is why this too is a list hook. The gateway sets no pagination
	// limit, so the hook sees the whole filtered list.
	filterAccess, vp := opts.Access != nil, opts.Visibility
	if filterAccess || vp != nil {
		hooks.AddAfterListTools(func(ctx context.Context, _ any, _ *mcp.ListToolsRequest, res *mcp.ListToolsResult) {
			if res == nil {
				return
			}
			if filterAccess {
				res.Tools = g.accessToolFilter(ctx, res.Tools)
				res.Tools = g.toolListFilter(ctx, res.Tools)
			}
			if vp != nil {
				res.Tools = vp.List(ctx, res.Tools)
			}
		})
	}
	// Async tools: MCP Tasks are answered from the jobs table by
	// MCPTasks (jobs_tasks.go), which also marks async tools in tools/list.
	if opts.Jobs != nil {
		serverOpts = append(serverOpts, server.WithTaskCapabilities(true, true, true))
		hooks.AddAfterListTools(g.markTaskSupport)
		g.attachJobs(opts.Jobs)
	}
	// Published names come last: every hook above works on canonical
	// names (access groups, the small list, visibility, the jobs.* prefix
	// markTaskSupport skips); only the answer the client reads changes.
	if opts.SafeToolNames {
		hooks.AddAfterListTools(g.publishToolNames)
	}
	serverOpts = append(serverOpts, server.WithHooks(hooks))
	g.mcp = server.NewMCPServer(opts.Name, opts.Version, serverOpts...)
	return g
}

// toolsListCacheTTL is the freshness hint on tools/list for 2026-07-28
// clients. The catalog changes when servers or settings change, and the
// gateway also announces that with notifications/tools/list_changed.
const toolsListCacheTTL = time.Minute

// accessDeniedReason is the audit reason on every call refused because the
// caller's dashboard user was never granted the tool's group.
const accessDeniedReason = "access: server not granted"

// agentLimitedReason is the audit reason on a call refused by the agent's
// own scope (agent_scopes): its owner may use the tool, the agent may not.
const agentLimitedReason = "access: not allowed for this agent"

// agentLimited reports whether callerID's owner may use entry but the
// agent's own limits refuse it.
func (g *Gateway) agentLimited(ctx context.Context, callerID string, entry toolEntry) bool {
	if g.access == nil {
		return false
	}
	return g.scopeFor(ctx, callerID).LimitedOut(entry.upstream, entry.tool.Name)
}

// agentLimitedMessage tells an agent plainly that its owner limited it:
// the owner chose the limit and can see the tool, so nothing is hidden.
func agentLimitedMessage(toolName string) string {
	return fmt.Sprintf("tool %q is not allowed for this agent: its owner limited what it may use. "+
		"The owner can change that on toolyard's Connected apps page.", toolName)
}

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
			Name string          `json:"name"`
			Meta json.RawMessage `json:"_meta"`
		} `json:"params"`
	}
	if err := json.Unmarshal(raw, &req); err != nil || req.Method != string(mcp.MethodToolsCall) {
		return nil
	}
	// mcp-go runs this hook before it puts a 2026-07-28 request's protocol
	// info on the context, so the client a denied call names comes from
	// the request's own _meta.
	ctx = withRequestMeta(ctx, req.Params.Meta)
	// A published name means its canonical tool here too (resolveCallName
	// rewrites it for mcp-go after this hook); the answers keep the name
	// the client sent.
	name := g.resolveToolName(req.Params.Name)
	g.mu.RLock()
	entry, known := g.tools[name]
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
		if entry.personalGitHub {
			ev.ReasonText = ""
		}
		if g.metrics != nil {
			g.metrics.Record(ev)
		}
		if g.agentLimited(ctx, agentID, entry) {
			return errors.New(agentLimitedMessage(req.Params.Name))
		}
	}
	// Same text mcp-go uses in handleToolCall for a name it doesn't have.
	return fmt.Errorf("tool '%s' not found: %w", req.Params.Name, server.ErrToolNotFound)
}

// denyUngranted answers a call to a tool outside the caller's scope. The
// agent sees an unknown-tool result; the audit log and metrics record the
// real reason so an admin can grant the server if that was the intent.
//
// A tool the owner may use but the agent's own scope refuses is answered
// plainly instead (agentLimitedMessage), with its own audit reason.
func (g *Gateway) denyUngranted(ctx context.Context, entry toolEntry, agentID, approvalID string, ev *metrics.Event) *mcp.CallToolResult {
	limited := g.agentLimited(ctx, agentID, entry)
	reason := accessDeniedReason
	if limited {
		reason = agentLimitedReason
	}
	_ = g.audit.Write(ctx, audit.Event{
		EventType:    audit.EventCallDenied,
		AgentID:      agentID,
		UpstreamName: entry.upstream,
		ToolName:     entry.tool.Name,
		Decision:     "deny",
		Reason:       reason,
		ApprovalID:   approvalID,
	})
	ev.Outcome = metrics.OutcomeDenied
	ev.ErrorClass = "access"
	// The answer names the tool as the caller did (a published name stays
	// published), so it never reveals the canonical name of a tool the
	// caller may not use; the audit row above keeps the canonical name.
	name := callerToolName(ctx, entry.tool.Name)
	if limited {
		return mcp.NewToolResultError(agentLimitedMessage(name))
	}
	return notFoundResult(name)
}

// BeginDrain makes every new routed tool call answer with a retryable
// "server restarting, call again" result before any grant is redeemed,
// approval resumed or tool dispatched, so no permission is used up. Calls
// already running continue.
func (g *Gateway) BeginDrain() { g.drainActive.Store(true) }

// Draining reports whether BeginDrain was called.
func (g *Gateway) Draining() bool { return g.drainActive.Load() }

// ReleaseWaiters ends running tools.wait_for_approval(s) calls with the
// retryable answer, so they do not hold the HTTP server open. Safe to call
// more than once.
func (g *Gateway) ReleaseWaiters() {
	g.drainMu.Lock()
	defer g.drainMu.Unlock()
	if g.drainRelease == nil {
		g.drainRelease = make(chan struct{})
	}
	if !g.drainReleased {
		g.drainReleased = true
		close(g.drainRelease)
	}
}

// drainReleaseCh returns the channel ReleaseWaiters closes.
func (g *Gateway) drainReleaseCh() <-chan struct{} {
	g.drainMu.Lock()
	defer g.drainMu.Unlock()
	if g.drainRelease == nil {
		g.drainRelease = make(chan struct{})
	}
	return g.drainRelease
}

// WaitGrantedCalls waits until no granted (Inbox) execution is running or
// ctx ends, and returns how many were still running.
func (g *Gateway) WaitGrantedCalls(ctx context.Context) int {
	return len(g.grantCalls.Wait(ctx))
}

// recoverToolPanics is mcp-go's WithRecovery (a panicking tool handler
// becomes an error answer, the process keeps serving) that also logs the
// panic at ERROR with its stack and counts it in the HTTP panic counter.
// Without it the panic left no trace beyond the agent's error text.
func recoverToolPanics(next server.ToolHandlerFunc) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (result *mcp.CallToolResult, err error) {
		defer func() {
			if r := recover(); r != nil {
				opsmetrics.IncHTTPPanic()
				logx.For("gateway").Error("panic in tool handler",
					"tool", request.Params.Name, "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
				err = fmt.Errorf("panic recovered in %s tool handler: %v", request.Params.Name, r)
			}
		}()
		return next(ctx, request)
	}
}

// drainAdmittedKey marks the context of a call that passed the drain
// check. Everything the call does runs on contexts derived from it (the
// dispatch, an executeToolCode script's nested calls, tools.execute's
// inner call), so its nested calls are not refused when a drain begins
// half-way: refusing them would abort a script after some of its writes
// ran, and the agent's rerun would repeat them. Admitted calls finish
// under the drain's trackers and the HTTP server's graceful shutdown.
type drainAdmittedKey struct{}

func withDrainAdmitted(ctx context.Context) context.Context {
	if drainAdmitted(ctx) {
		return ctx
	}
	return context.WithValue(ctx, drainAdmittedKey{}, true)
}

func drainAdmitted(ctx context.Context) bool {
	v, _ := ctx.Value(drainAdmittedKey{}).(bool)
	return v
}

// restartingResult is the answer to a call refused or a wait ended by a
// drain. Nothing ran and no grant or approval was used, so the same call
// works again in a few seconds (the next process answers it).
func restartingResult(tool, detail string) *mcp.CallToolResult {
	msg := "server restarting, call again: " + detail
	res := mcp.NewToolResultError(msg)
	res.StructuredContent = map[string]any{
		"status":              "server_restarting",
		"executed":            false,
		"retryable":           true,
		"retry_after_seconds": restartRetryAfterSeconds,
		"tool":                tool,
		"message":             msg,
	}
	res.Meta = &mcp.Meta{AdditionalFields: map[string]any{"toolyard.retryable": true}}
	return res
}

// toolCallLabel maps a call's metrics outcome to the tool-call counter's
// fixed labels.
func toolCallLabel(outcome string) string {
	switch outcome {
	case metrics.OutcomeOK:
		return "ok"
	case metrics.OutcomeDenied, metrics.OutcomeExpired:
		return "denied"
	case metrics.OutcomeDeferred:
		return "approval"
	default:
		return "error"
	}
}

// auditWriteFailed logs a failed dispatch audit write at ERROR, with the
// dispatch id, tool and upstream (never the arguments), and counts it.
func auditWriteFailed(stage, dispatchID string, entry toolEntry, err error) {
	opsmetrics.IncAuditWriteFailure(stage)
	logx.For("gateway").Error("dispatch audit write failed",
		"stage", stage, "dispatch_id", dispatchID,
		"tool", entry.tool.Name, "upstream", entry.upstream, "err", err.Error())
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
// the access and connections tools, and the fixture echo tool into the MCP
// server.
func (g *Gateway) RegisterBuiltins() {
	entries := g.builtinMemoryTools()
	entries = append(entries, g.staticFixtureTool())
	entries = append(entries, g.metaTools()...)
	entries = append(entries, g.approvalMetaTools()...)
	entries = append(entries, g.codeModeTools()...)
	entries = append(entries, g.accessTools()...)
	entries = append(entries, g.connectionsTools()...)
	entries = append(entries, g.jobsTools()...)
	for _, e := range entries {
		g.registerEntry(e)
	}
}

// reservedUpstreamName reports whether name belongs to the gateway itself:
// the synthetic upstreams (builtin, fixture, inbox, session, policies,
// servers, audit, access, connections, jobs, callbacks, skills), the
// meta-tool group "tools", the built-in data groups whose tools are
// registered under the "builtin" upstream (memory, events), and
// "toolyard", the code-mode server every internal tool is bound under. An
// upstream with one of these names would register tools under the same
// prefix as the built-ins and share their access group: "tools" and
// "skills" are always-on, so a server called tools or skills would be
// reachable by every member without a grant. "lake", the retired data
// group, stays reserved: old grants for it may still be stored.
//
// notes is deliberately not here: it is a real upstream that startup
// registers under that name (upstreams.UpsertBuiltin), and its access
// group is the upstream name like any other server's.
func reservedUpstreamName(name string) bool {
	switch name {
	case builtinUpstream, "fixture", inboxUpstream, sessionUpstream, "tools", "memory", "lake", "events",
		policiesUpstream, serversUpstream, auditUpstream, accessUpstream, toolyardServer, connectionsGroup, jobsUpstream, callbacksUpstream,
		skillsUpstream:
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
	if cfg.Transport == "github" {
		return g.addGitHubUpstream(cfg)
	}
	// per_user connections stay on 2025-11-25 even in auto mode: each
	// person's connection is created suspended and never lists tools, so a
	// 2026-07-28 client would know none of the x-mcp-header parameters it
	// must send, and a stateless connection would leave the per-user cap.
	if cfg.PerUser {
		return g.addPerUserUpstream(ctx, cfg)
	}
	if cfg.ProtocolVersion == "" && g.upstreamProtocol == ProtocolAuto {
		cfg.ProtocolVersion = ProtocolAuto
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
	g.installUpstream(cfg.Name, tools, u)
	// Concurrent adds dial through untracked probes. Once their catalogs
	// are registered, reclaim any idle overflow in the shared pool.
	g.trimIdlePool(u)
	return nil
}

// installUpstream puts a dialled shared upstream and its catalog in place,
// under catalogMu.
func (g *Gateway) installUpstream(name string, tools []mcp.Tool, u *upstream) {
	g.catalogMu.Lock()
	defer g.catalogMu.Unlock()
	g.mu.Lock()
	g.upstreams[name] = u
	g.mu.Unlock()
	g.registerUpstreamTools(name, tools, u)
}

// registerUpstreamTools wraps an upstream's tools into the catalog under
// "<name>.<tool>" and returns the names it registered. With ref set, each
// tool's handler calls that connection; with ref nil (a per_user upstream)
// the handler is left empty and dispatch picks the caller's connection.
// The catalog goes to mcp-go in one batch, so clients get one
// notifications/tools/list_changed, not one per tool.
func (g *Gateway) registerUpstreamTools(name string, tools []mcp.Tool, ref *upstream) []string {
	entries := make([]toolEntry, 0, len(tools))
	for _, t := range tools {
		original := t.Name
		wrapped, field := wrapSchema(t)
		wrapped.Name = name + "." + original
		// mcp-go's AddTool panics on a tool whose x-mcp-header annotations
		// break SEP-2243. An upstream's catalog is outside our control, so
		// such a tool is left out rather than taking the gateway down.
		if err := mcp.ValidateParamHeaderAnnotations(&wrapped); err != nil {
			log.Printf("gateway: upstream %s: skipping tool %q: invalid x-mcp-header annotations: %v", name, original, err)
			continue
		}
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
		entries = append(entries, entry)
	}
	if len(entries) == 0 {
		return nil
	}
	registered := make([]string, len(entries))
	batch := make([]server.ServerTool, len(entries))
	g.mu.Lock()
	for i, e := range entries {
		g.tools[e.tool.Name] = e
		registered[i] = e.tool.Name
		batch[i] = server.ServerTool{Tool: e.tool, Handler: g.handlerFor(e.tool.Name)}
	}
	g.catalogChanged()
	g.mu.Unlock()
	g.mcp.AddTools(batch...)
	return registered
}

// catalogRefreshTimeout bounds one upstream's tools/list during a
// catalog refresh.
const catalogRefreshTimeout = 30 * time.Second

// RefreshUpstreamCatalogs reads the tool list of every live shared
// upstream again and applies a catalog that changed: new and changed
// tools are registered, and tools the upstream no longer lists are
// removed (mcp-go sends notifications/tools/list_changed for both). A
// suspended upstream is skipped, because reading it would dial it and
// undo the idle sweep; the next refresh while it is live reads it.
// per_user upstreams are skipped too: their catalog comes from one
// person's connection. A refresh read is not use, so it never keeps a
// connection from going idle. It returns how many catalogs changed.
func (g *Gateway) RefreshUpstreamCatalogs(ctx context.Context) int {
	g.mu.RLock()
	ups := make(map[string]*upstream, len(g.upstreams))
	for name, u := range g.upstreams {
		ups[name] = u
	}
	g.mu.RUnlock()
	changed := 0
	for name, u := range ups {
		if ctx.Err() != nil {
			break
		}
		before := u.toolsSnapshot()
		rctx, cancel := context.WithTimeout(ctx, catalogRefreshTimeout)
		tools, live, err := u.refreshTools(rctx)
		cancel()
		if !live {
			continue
		}
		if err != nil {
			log.Printf("catalog-refresh: %s: %v", u.label(), err)
			continue
		}
		if sameTools(before, tools) {
			continue
		}
		if !g.replaceUpstreamTools(name, tools, u) {
			continue
		}
		log.Printf("catalog-refresh: %s: catalog changed (%d tools)", u.label(), len(tools))
		changed++
	}
	return changed
}

// replaceUpstreamTools registers an upstream's new catalog, then removes
// its tools that the new catalog no longer has (registering first leaves
// no moment without the tools that stay). It applies nothing, and returns
// false, when ref is no longer the connection registered under name (the
// upstream was removed or reconnected while its catalog was read).
func (g *Gateway) replaceUpstreamTools(name string, tools []mcp.Tool, ref *upstream) bool {
	g.catalogMu.Lock()
	defer g.catalogMu.Unlock()
	g.mu.RLock()
	current := g.upstreams[name] == ref
	g.mu.RUnlock()
	if !current || ref.retired.Load() {
		return false
	}
	keep := map[string]bool{}
	for _, n := range g.registerUpstreamTools(name, tools, ref) {
		keep[n] = true
	}
	var gone []string
	g.mu.Lock()
	for k, e := range g.tools {
		if e.upstream == name && !keep[k] {
			gone = append(gone, k)
			delete(g.tools, k)
		}
	}
	g.catalogChanged()
	g.mu.Unlock()
	if len(gone) > 0 {
		g.mcp.DeleteTools(gone...)
	}
	return true
}

// sameTools reports whether two catalogs are the same, tool for tool. The
// order does not count: an upstream that lists in a different order each
// time is not re-registered and announced on every refresh.
func sameTools(a, b []mcp.Tool) bool {
	if len(a) != len(b) {
		return false
	}
	ja, errA := json.Marshal(sortedByName(a))
	jb, errB := json.Marshal(sortedByName(b))
	return errA == nil && errB == nil && bytes.Equal(ja, jb)
}

func sortedByName(tools []mcp.Tool) []mcp.Tool {
	out := slices.Clone(tools)
	slices.SortFunc(out, func(x, y mcp.Tool) int { return strings.Compare(x.Name, y.Name) })
	return out
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
	u, pu, err := g.dropUpstream(name)
	if err != nil {
		return err
	}
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

// dropUpstream takes an upstream (shared or per_user) and all of its tools
// out of the catalog, under catalogMu, and returns what the caller must
// close.
func (g *Gateway) dropUpstream(name string) (*upstream, *perUserGroup, error) {
	g.catalogMu.Lock()
	defer g.catalogMu.Unlock()
	g.mu.Lock()
	u, ok := g.upstreams[name]
	pu, okPU := g.perUser[name]
	if !ok && !okPU {
		g.mu.Unlock()
		return nil, nil, ErrUpstreamNotFound
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
	g.catalogChanged()
	g.mu.Unlock()
	g.mcp.DeleteTools(toRemove...)
	return u, pu, nil
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
		if idle >= idleAfter && u.suspend() {
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
// Only idle transports are eligible. Concurrent active admissions may
// exceed the cap until their leases finish; no in-flight RPC is canceled.
func (g *Gateway) acquireSlot(self *upstream) {
	victim, live, limit := g.pickEvictionCandidate(self)
	if victim != nil && victim.suspend() {
		log.Printf("lru-evict: suspended %q to make room for %q (live=%d cap=%d)",
			victim.label(), self.label(), live, limit)
	}
}

// trimIdlePool reclaims temporary active overflow when a lease finishes.
// A concurrent new lease can make a selected victim busy; suspend checks
// again under the victim's lock, and the next release completes cleanup.
func (g *Gateway) trimIdlePool(self *upstream) {
	for {
		victim, _, _ := g.pickPoolCandidate(self, false)
		if victim == nil || !victim.suspend() {
			return
		}
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
	return g.pickPoolCandidate(self, true)
}

func (g *Gateway) pickPoolCandidate(self *upstream, admission bool) (*upstream, int, int) {
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
	// A 2026-07-28 connection is outside the pool: it is not counted and
	// is never the one suspended (upstream.pooled).
	var candidates []*upstream
	for _, u := range g.allUpstreams() {
		if (!admission || u != self) && (u.userID != "") == perUser && u.pooled() {
			candidates = append(candidates, u)
		}
	}

	var lru *upstream
	live := 0
	var oldest int64 = math.MaxInt64
	for _, u := range candidates {
		u.mu.Lock()
		liveClient, active := u.client != nil, u.active
		u.mu.Unlock()
		if !liveClient {
			continue
		}
		live++
		t := u.lastUsed.Load()
		if active == 0 && t < oldest {
			oldest = t
			lru = u
		}
	}
	if (admission && live >= limit || !admission && live > limit) && lru != nil {
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
	if entry.personalGitHub {
		ev.ReasonText = ""
	}
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
	// tools.execute (and the REST callers) may name the target by its
	// published name; everything below sees the canonical one, and a
	// refusal for a tool outside the caller's scope echoes the supplied
	// name (denyUngranted), as for a tool that does not exist.
	supplied := targetName
	targetName = g.resolveToolName(targetName)
	g.mu.RLock()
	entry, ok := g.tools[targetName]
	g.mu.RUnlock()
	if !ok {
		return notFoundResult(supplied), nil
	}
	if targetName == viaTool {
		return mcp.NewToolResultError("tools.execute cannot target itself"), nil
	}
	return g.routeEntry(withSuppliedToolName(ctx, targetName, supplied), entry, viaTool, args)
}

// routeEntry is the shared path used by both the MCP-side handler and
// tools.execute. It assumes entry is a registered toolEntry. The metric
// Event captured here is emitted to the metrics sink (if any) once the
// terminal outcome of this call is known. via is the path in ("direct"
// from the MCP handler).
func (g *Gateway) routeEntry(ctx context.Context, entry toolEntry, via string, args map[string]any) (res *mcp.CallToolResult, err error) {
	started := time.Now()
	agentID := agentIDFromContext(ctx)
	refusedDraining := false

	// Who is calling: the ingress raiser (or the caller id alone), the
	// client that introduced itself at initialize, and the agent session
	// the call names. It rides on ctx so every audit row below, the
	// approval and the upstream handler see the same answer.
	raiser := g.resolveRaiser(ctx, agentID, via)
	g.attachSession(ctx, &raiser, args)
	ctx = actor.WithRaiser(ctx, raiser)
	if res := refuseNestedCodeMode(ctx, entry); res != nil {
		return res, nil
	}
	ctx = withCodeModeRunID(ctx, entry)

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
		if refusedDraining {
			opsmetrics.IncToolCall("refused_draining")
		} else {
			opsmetrics.IncToolCall(toolCallLabel(ev.Outcome))
		}
	}()

	// Per-user access comes first: a member whose admin never granted this
	// tool's server can't resume, redeem a grant for, or even be told about
	// a tool there. The result matches an unknown tool.
	if !g.allowsEntry(ctx, agentID, entry) {
		return g.denyUngranted(ctx, entry, agentID, "", &ev), nil
	}

	// A drain refuses a new call before anything is used up: no grant is
	// redeemed, no approval resumed or created, nothing dispatched. A call
	// made by a call admitted earlier (a nested executeToolCode or
	// tools.execute call, a resume through tools.execute) is part of that
	// call and finishes under the drain.
	if g.Draining() && !drainAdmitted(ctx) {
		refusedDraining = true
		ev.Outcome = metrics.OutcomeError
		ev.ErrorClass = "draining"
		return restartingResult(entry.tool.Name, entry.tool.Name+
			" did not run and no permission was used. Call it again with the same arguments in a few seconds."), nil
	}
	ctx = withDrainAdmitted(ctx)

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
	ctx = withCallbackRefFor(ctx, entry, args)
	reason, intent, cleanArgs, rerr := extractReasonOpt(args, entry.reasonField, entry.reasonOptional)
	cleanArgs = restoreOwnCallbackRef(entry, args, cleanArgs)
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

	if entry.personalGitHub {
		if err := g.prepareGitHub(ctx, entry, cleanArgs); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
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

	decision := g.decide(entry, agentID, intent, reason, cleanArgs)

	// A grant only matters for calls that would otherwise need approval:
	// an explicit deny still wins, and an open tool doesn't use one up.
	if !entry.personalGitHub && grantToken != "" && decision.Action == policy.ActionApprove && g.inbox != nil {
		// Tracked so shutdown waits for the granted execution to finish
		// and record its outcome. An async tool redeems and runs in its
		// job (jobs_tools.go).
		call := jobCall{entry: entry, args: cleanArgs, agentID: agentID, reason: reason, intent: intent, callbackRef: callbackRefFrom(ctx), grant: true, grantToken: grantToken}
		return g.runOrStartJob(ctx, call, &ev, func(ctx context.Context, e toolEntry, ev *metrics.Event) (*mcp.CallToolResult, error) {
			done := g.grantCalls.Add(e.tool.Name)
			defer done()
			return g.redeemAndDispatch(ctx, e, cleanArgs, agentID, reason, grantToken, ev)
		})
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
		// An async tool runs as a job (jobs_tools.go).
		call := jobCall{entry: entry, args: cleanArgs, agentID: agentID, reason: reason, intent: intent, callbackRef: callbackRefFrom(ctx)}
		return g.runOrStartJob(ctx, call, &ev, func(ctx context.Context, e toolEntry, ev *metrics.Event) (*mcp.CallToolResult, error) {
			return g.dispatch(ctx, e, cleanArgs, agentID, reason, "", ev)
		})
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
		if !entry.personalGitHub && g.inbox != nil {
			return g.coach(ctx, entry, cleanArgs, agentID, reason, &ev), nil
		}
		return g.holdAndWait(ctx, entry, cleanArgs, agentID, reason, intent, decision.RequireHuman, &ev)
	default:
		ev.Outcome = metrics.OutcomeError
		ev.ErrorClass = "policy"
		return mcp.NewToolResultErrorf("policy returned unknown action %q", decision.Action), nil
	}
}

// ruleEscalated is the RuleID of the approval an entry's escalate func
// asks for.
const ruleEscalated = "builtin-escalated"

// decide is the routing decision for one call to entry: baseDecision (an
// overridable explicit deny, else the forced action or policy.Eval, then
// the entry's escalate func), then the requireHuman and personal-GitHub
// rules. routeEntry and Access (inbox.check, request validation) use it,
// and effectiveAccess (policies.*, the agent policy guard) starts from the
// same baseDecision, so all of them agree on what a call needs.
func (g *Gateway) decide(entry toolEntry, agentID, intent, reason string, args map[string]any) policy.Decision {
	decision := baseDecision(g.policy, targetOf(entry), policy.Request{
		AgentID:        agentID,
		IntentCategory: intent,
		Arguments:      args,
		UserReason:     reason,
	})
	if entry.requireHuman && decision.Action != policy.ActionDeny {
		decision.Action = policy.ActionApprove
		decision.RequireHuman = true
	}
	if entry.personalGitHub && decision.Action == policy.ActionApprove {
		decision.RequireHuman = true
	}
	return decision
}

// baseDecision is the policy decision for a call to t under engine, in
// this order:
//  1. with denyOverridesForced, an explicit deny on the tool or its
//     upstream (a tool rule beats an upstream rule, as in Eval) wins;
//  2. otherwise the forced action, if any, else engine.Eval;
//  3. an allow becomes an approval when t.escalate says this call's
//     arguments need one. A person must decide it (RequireHuman), so a
//     learned auto-approval rule never runs a data-replacing call.
//
// req.UpstreamName and req.ToolName are filled in from t: the wrapped
// catalog name, which matches tool_policies targets and the
// IsReadOnlyName/IsWrite computation used everywhere else.
func baseDecision(engine *policy.Engine, t policyTarget, req policy.Request) policy.Decision {
	req.UpstreamName = t.upstream
	req.ToolName = t.name
	if t.denyOverridesForced {
		if d, denied := engine.ExplicitDeny(t.name, t.upstream); denied {
			return d
		}
	}
	var decision policy.Decision
	if t.forced != nil {
		decision = policy.Decision{
			Action: *t.forced,
			Reason: "built-in tool policy",
			RuleID: "builtin-forced-" + string(*t.forced),
		}
	} else {
		decision = engine.Eval(req)
	}
	if decision.Action == policy.ActionAllow && t.escalate != nil {
		if why, ok := t.escalate(req.Arguments); ok {
			decision = policy.Decision{Action: policy.ActionApprove, Reason: why, RuleID: ruleEscalated, RequireHuman: true}
		}
	}
	return decision
}

func (g *Gateway) registerEntry(e toolEntry) {
	g.mu.Lock()
	g.tools[e.tool.Name] = e
	g.catalogChanged()
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
		// A 2026-07-28 call's Mcp-Param-* headers must match the tool that
		// runs, also when the call named it by its published name, which
		// mcp-go's own check cannot look up (checkParamHeaders).
		if err := g.checkParamHeaders(ctx, entry, request); err != nil {
			return nil, err
		}
		if g.visibility != nil && !g.visibility.IsVisible(ctx, entry.tool.Name) &&
			g.allowsEntry(ctx, agentIDFromContext(ctx), entry) {
			return mcp.NewToolResultErrorf(
				"tool %q is hidden by the current agent-surface mode. Use tools.execute with tool=%q to invoke it.",
				toolName, toolName,
			), nil
		}
		args := g.exactArguments(ctx, toolName, argsAsMap(request.Params.Arguments))
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
		CallbackRef:    callbackRefFrom(ctx),
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
		if req.DecidedInline {
			// Inline auto-approve: this Hold's own decideInline decided
			// it (runExec=false, under its execution claim), so NO
			// background executor was scheduled — we dispatch here, and
			// only here: a coalesced call, or one that woke within
			// -in-line-wait after another call's auto decision, sees a
			// rule as decider but not this mark, and takes resumeDeferred
			// below. We MUST then persist the result, on a context the
			// client's disconnect does not cancel (as Execute does);
			// otherwise the row stays allowed with result_executed_at IS
			// NULL until recovery records it outcome_unknown.
			res, derr := g.dispatch(ctx, entry, args, agentID, reason, req.ID, ev)
			persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistTimeout)
			defer cancel()
			g.persistApprovalResult(persistCtx, req.ID, res, derr, ev.ErrorClass == "upstream" || ev.ErrorClass == "audit_outcome_unknown")
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
		return withCallbackWarning(g.deferredResponse(ctx, req, entry, args), req.CallbackWarning), nil
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
	if err != nil || !g.approvalVisible(ctx, req) || !approvalRaisedFor(req, entry) {
		ev.Outcome = metrics.OutcomeError
		ev.ErrorClass = "approval"
		return mcp.NewToolResultErrorf("unknown approval %q", approvalID), nil
	}
	if req.ExecutionState == "outcome_unknown" {
		ev.Outcome = metrics.OutcomeError
		ev.ErrorClass = "outcome_unknown"
		return jsonResultMap(approvalSnapshot(req, false))
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
			case <-g.drainReleaseCh(): // shutdown: answer with the state as it stands
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
	if err != nil || !g.approvalVisible(ctx, req) || approvalRaisedFor(req, entry) {
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
	if entry.personalGitHub {
		asUser, _ = g.ownerUser(ctx, agentID)
	}
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
	// Each dispatch has a durable intent before the handler can change anything.
	dispatchID := "dispatch_" + uuid.NewString()
	auditArgs, _ := json.Marshal(args)
	intent := audit.Event{ID: dispatchID + ".started", EventType: "call.dispatch", AgentID: agentID, UpstreamName: entry.upstream, ToolName: entry.tool.Name, Reason: reason, ApprovalID: approvalID, Arguments: auditArgs, Raiser: actor.Raiser{OwnerUserID: asUser}}
	if err := g.audit.Write(ctx, intent); err != nil {
		auditWriteFailed("intent", dispatchID, entry, err)
		ev.Outcome = metrics.OutcomeError
		ev.ErrorClass = "audit_unavailable"
		return mcp.NewToolResultError("Audit unavailable. The tool did not run."), nil
	}
	res, err := entry.handle(callCtx, args)
	ev.UpstreamLatencyMs = int(time.Since(upstreamStart).Milliseconds())
	if errors.Is(err, context.DeadlineExceeded) {
		log.Printf("upstream-timeout: tool=%s upstream=%s agent=%s elapsed=%s timeout=%s",
			entry.tool.Name, entry.upstream, agentID,
			time.Since(upstreamStart).Round(time.Millisecond), g.upstreamCallTimeout)
	}
	if res == nil && err == nil {
		err = errors.New("upstream returned no result")
	}
	outcome := intent
	outcome.ID = dispatchID + ".completed"
	outcome.Arguments = nil
	outcome.EventType = audit.EventCallSucceeded
	if err != nil {
		outcome.EventType = audit.EventCallFailed
		outcome.ResultSummary = err.Error()
	} else {
		outcome.ResultSummary = summariseResult(res)
		if res.IsError {
			outcome.EventType = audit.EventCallFailed
		}
	}
	// A caller disconnect must not erase the final audit record.
	auditCtx, auditCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	auditErr := g.audit.Write(auditCtx, outcome)
	auditCancel()
	if auditErr != nil {
		auditWriteFailed("outcome", dispatchID, entry, auditErr)
		ev.Outcome = metrics.OutcomeError
		ev.ErrorClass = "audit_outcome_unknown"
		return outcomeUnknown(mcp.NewToolResultError("Tool outcome unknown: completion audit failed. Do not retry. Review dispatch " + dispatchID + " before another action.")), nil
	}
	if err != nil {
		ev.Outcome = metrics.OutcomeError
		ev.ErrorClass = "upstream"
		if u != nil && isUnauthorized(err) {
			if result := g.refuseSharedSignIn(ctx, u, entry, agentID, reason, approvalID, err, ev); result != nil {
				return result, nil
			}
		}
		return outcomeUnknown(mcp.NewToolResultErrorFromErr("tool failed", err)), nil
	}

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

// buildInstructions is the MCP initialize instructions: the five-tool flow
// (find with tools.search, read with getToolDocs, run in executeToolCode,
// ask with inbox.request and read the decision with inbox.status) and the
// Inbox contract. They are server-wide, so every sentence also holds for an
// agent switched to the full tool list. With safe tool names (safe) they
// name every MCP-listed tool by its published name (tools_search), say
// once that the dotted names work too, and leave code-mode names
// (server.tool, toolyard.<group>_<name>, the JavaScript helpers) as they
// are.
func buildInstructions(bus *approval.Bus, inLineWait time.Duration, safe bool) string {
	n := func(canonical string) string { return listedToolName(safe, canonical) }
	var b strings.Builder
	b.WriteString("toolyard gateway. Every tool call requires `_reason` (20-2000 chars) for the audit record; inside an executeToolCode script it is optional. A short reason never authorizes a restricted call. ")
	b.WriteString("TOOLS: by default you see five tools: " + n(MetaSearchTool) + ", getToolDocs, executeToolCode, " + n("inbox.request") + " and " + n("inbox.status") + ". They reach every other tool you may use; an agent its owner switched to the full tool list sees more tools, and this flow works the same. ")
	resultFields := "the tool's name, its code-mode name (code)"
	if safe {
		b.WriteString("NAMES: tools/list shows each tool's name with every character other than letters, digits, _ and - replaced by _ (" +
			n(MetaSearchTool) + " is " + MetaSearchTool + ", " + n("inbox.request") + " is inbox.request); the dotted names work as well. " +
			"Code-mode names (server.tool, toolyard.<group>_<name>) do not change. ")
		resultFields = "the tool's name, the name to call it by over MCP (mcp_name), its code-mode name (code)"
	}
	b.WriteString("FIND: call " + n(MetaSearchTool) + " with a few words about the task. Each result gives " + resultFields + " and whether it runs now, needs approval or is blocked (run: now, needs_approval or blocked). " +
		"READ: call getToolDocs with the two halves of a result's code (server before the dot, tool after it) for its full parameters. " +
		"RUN: call tools inside executeToolCode: Starlark `result = server.tool(param=\"value\")`, or JavaScript with language \"javascript\": `return await tools.server.tool({ param: \"value\" })`. " +
		"toolyard's own tools are the server toolyard, called as toolyard.<group>_<name>: the tools named below that you do not see (" +
		n("inbox.guide") + ", " + n("inbox.upload") + ", " + n("inbox.wait") + ", " + n("tools.poll_approval") + ", " + n("session.start") +
		") run that way, e.g. toolyard.inbox_guide(topic=\"requests\"). " +
		"Every call a script makes goes through the same access, policy and approval rules as a direct call. ")
	b.WriteString("PERMISSIONS: " + n("inbox.request") + " is the one way to get permission. Plan the round first, then send ONE " + n("inbox.request") + " with every restricted call (run: needs_approval) and every question you have for your owner in this round: tools[] with stable unique call_id values (repeated calls to the same tool are supported) and questions[] with schema_version 3. Start another round only for what the answers open. A round with only questions and no tool call uses " + n("inbox.ask") + ", run in a script as toolyard.inbox_ask (same questions[]). " +
		"Supply task.objective, message, facts.why_now, each call's summary relating its purpose to the task, target, exact params or explicit bounded constraints, operation and expected_effects. Writes also require affected_scope, material_risks and a concrete undo path or an explicit irreversible statement. Include useful evidence without secrets; for local screenshots, diagrams or recordings call " + n("inbox.upload") + ", PUT the bytes to its single-use URL, and cite the returned upload:<sha256> ref in an attachment. Server validation rejects missing context and placeholders; it cannot establish that an explanation is true. Send it with dry_run: true first. Use " + n("inbox.guide") + " for schemas and examples. " + n("inbox.check") + " stays available but is optional: " + n(MetaSearchTool) + " already says what runs now. ")
	b.WriteString("Restricted calls without grants return permission_required and run nothing. In a script, tools.tryCall (JavaScript) or try_call (Starlark) returns that status instead of stopping: collect every such call of the script into the one " + n("inbox.request") + ", then run the script again with the grants. New requests always use agent execution. The human selects accepted and rejected calls, adds optional per-call reasons and an overall note, and explicitly submits one complete decision. required is a planning hint; its rejection and all-rejected batches are valid. Rejected calls receive no grant. ")
	b.WriteString("CALLBACKS: obtain an authorized receiver or an available T3 session webhook, then pass its opaque callback_ref to " + n("inbox.request") + ". Continue unrelated work or end the turn. The callback announces the complete committed decision and contains no grant tokens. It is not permission. Read " + n("inbox.status") + " for authoritative verdicts, notes, execution state and grants. " + n("inbox.wait") + " remains optional for callers without callbacks. Pending decision deadline defaults to 24 hours, bounded by administrator limits; grant lifetime defaults to 30 minutes and is separate. ")
	b.WriteString("EXECUTION: call each accepted tool with its scoped single-use token in _grant and exactly the approved parameters; in a script _grant is one more argument: Starlark `server.tool(param=\"value\", _grant=\"<token>\")`, JavaScript `await tools.server.tool({ param: \"value\", _grant: \"<token>\" })`. Read-only " + n("inbox.status") + " can recover the same still-valid unused token after a lost response; it does not consume or renew permission. Never log tokens. Follow per-call reasons and owner_note. Revoked or expired grants never revive automatically. A used grant has one execution claim. Inspect its recorded outcome before any recovery. If a write times out after dispatch, outcome_unknown means it may already have committed; reconcile it and never blindly repeat it. ")
	b.WriteString("LEGACY: already-pending approvals retain their original execution mode and Toolyard can execute them after an Inbox decision. Inspect " + n("tools.poll_approval") + " for their saved result; never reissue the original write. This compatibility path does not apply to new requests. ")
	b.WriteString("GITHUB: GitHub writes always require the account owner's explicit approval and never run on inbox grants or automatic policy rules. Call the GitHub tool (directly, or with tools.tryCall in a script) to create the request; it returns a deferred approval_id. Collect the result with " + n("tools.poll_approval") + " or " + n("tools.wait_for_approval") + ", and never re-issue the write. Agents may propose COMMENT, REQUEST_CHANGES, or APPROVE reviews; Toolyard submits each review only after the account owner explicitly allows the request. ")
	b.WriteString("FALLBACK: if your MCP connection to toolyard fails, run the same code over HTTPS under the same identity, policy and approvals: POST /v1/code/run with your toolyard token from TOOLYARD_TOKEN_FILE, or the ty command from GET /v1/code/ty. " + n("inbox.guide") + " topic fallback explains it. ")
	b.WriteString("After " + n("session.start") + ", pass its id as _session_id for attribution; session identifiers do not stream a full session or grant permission.")
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
	if owner := req.PersonalOwner(); owner != "" && (req.Status != approval.StatusAllowed || req.DecidedBy != owner || req.ExpiresAt <= time.Now().UnixMilli()) {
		g.persistApprovalResult(ctx, req.ID, mcp.NewToolResultError("GitHub approval is no longer valid; request fresh approval"), nil)
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
	// Nobody is reading this result live: it is persisted and polled, so a
	// connect link minted here would sit in a row admins and operator
	// tokens can read. Refusals name the dashboard instead.
	execCtx := WithoutConnectLinks(actor.WithRaiser(WithAgentID(ctx, req.AgentID), raiser))
	// The bus starts an executor only outside a drain, so its nested calls
	// (an approved script's) are admitted work and finish under it.
	execCtx = withDrainAdmitted(execCtx)
	if req.PersonalOwner() != "" {
		var cancel context.CancelFunc
		execCtx, cancel = context.WithDeadline(execCtx, time.UnixMilli(req.ExpiresAt))
		defer cancel()
	}

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
	// The outcome is recorded even when ctx was cancelled mid-dispatch
	// (the bus cancels it at the drain deadline): the dispatch error then
	// makes it outcome_unknown.
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistTimeout)
	defer cancel()
	g.persistApprovalResult(persistCtx, req.ID, res, dispatchErr, ev.ErrorClass == "upstream" || ev.ErrorClass == "audit_outcome_unknown")
}

// persistApprovalResult encodes a dispatched tool result and stores it on
// the approval row via bus.SetResult, so pollers (and the legacy
// `_approval_id` re-call) can rehydrate it and SweepUnexecuted won't
// re-fire the executor after a restart. Shared by the bus-driven Execute
// hook and holdAndWait's inline auto-approve path. SetResult is gated on
// result_executed_at IS NULL, so a racing second call is a harmless no-op.
func (g *Gateway) persistApprovalResult(ctx context.Context, approvalID string, res *mcp.CallToolResult, dispatchErr error, uncertain ...bool) {
	if g.approval == nil {
		return
	}
	envelope, encErr := encodeApprovalResult(res)
	if encErr != nil {
		if err := g.approval.SetResultState(ctx, approvalID, "", true, "encode result: "+encErr.Error(), "outcome_unknown"); err != nil {
			log.Printf("approval-persist: %s: %v", approvalID, err)
		}
		return
	}
	var execErr string
	if dispatchErr != nil {
		execErr = dispatchErr.Error()
	}
	isErr := res != nil && res.IsError
	state := "succeeded"
	if isErr {
		state = "failed"
	}
	if (len(uncertain) > 0 && uncertain[0]) || dispatchErr != nil || (isErr && strings.Contains(strings.ToLower(summariseResult(res)), "outcome unknown")) {
		state = "outcome_unknown"
	}
	if err := g.approval.SetResultState(ctx, approvalID, envelope, isErr, execErr, state); err != nil {
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

// outcomeUnknown marks a result whose call may or may not have run (the
// upstream failed after dispatch, or the completion audit could not be
// written), so a code-mode run ends outcome_unknown instead of failed and
// nobody repeats a write blindly (TEC-1627). MCP clients see the same text.
func outcomeUnknown(res *mcp.CallToolResult) *mcp.CallToolResult {
	if res.Meta == nil {
		res.Meta = &mcp.Meta{}
	}
	if res.Meta.AdditionalFields == nil {
		res.Meta.AdditionalFields = map[string]any{}
	}
	res.Meta.AdditionalFields[codemode.OutcomeUnknownMarker] = true
	return res
}
