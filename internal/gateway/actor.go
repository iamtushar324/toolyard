package gateway

// actor.go is where the gateway works out WHO raised a tool call and WHO
// decided it, so every audit row, approval and metric written further
// down carries the same answer.
//
// The raiser comes from three places, merged in this order: the
// actor.Raiser the MCP ingress put on the context (agent, owner, client
// IP, session ids), the MCP clientInfo the client sent at initialize
// (cached per agent because -stateless-mcp forgets sessions), and the
// optional `_session_id` argument naming a toolyard agent session. When
// the context carries no raiser at all, the caller id alone is enough to
// build one, so every path keeps working as it did.

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tusharbhardwaj/toolyard/internal/actor"
	"github.com/tusharbhardwaj/toolyard/internal/approval"
	"github.com/tusharbhardwaj/toolyard/internal/metrics"
)

// Raiser.AgentKind values the gateway derives from the caller id.
const (
	agentKindAgent     = "agent"
	agentKindDashboard = "dashboard"
	agentKindVoice     = "voice"
)

// Raiser.Via for calls the MCP handler routes itself.
const viaDirect = "direct"

// clientKinds maps a normalised MCP clientInfo name to Raiser.ClientKind.
// Keep it small: anything not listed is "unknown", which is an honest
// answer for a client we have never seen.
var clientKinds = map[string]string{
	"claude-code":      "claude_code",
	"claude-code-cli":  "claude_code",
	"codex":            "codex",
	"codex-cli":        "codex",
	"codex-mcp-client": "codex",
	"cursor":           "cursor",
	"cursor-vscode":    "cursor",
	"opencode":         "opencode",
	"t3":               "t3",
	"t3-code":          "t3",
	"t3code":           "t3",
}

// ClientKindOf maps an MCP clientInfo name to a Raiser.ClientKind.
func ClientKindOf(name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	n = strings.NewReplacer(" ", "-", "_", "-").Replace(n)
	if n == "" {
		return "unknown"
	}
	if k, ok := clientKinds[n]; ok {
		return k
	}
	// "claude-code/1.2" or "cursor-0.48": the leading token is the name.
	for _, sep := range []string{"/", "@", ":"} {
		if head, _, ok := strings.Cut(n, sep); ok {
			if k, ok := clientKinds[head]; ok {
				return k
			}
		}
	}
	return "unknown"
}

// clientInfo is what a client said about itself at initialize.
type clientInfo struct {
	name string // "name/version"
	kind string
}

// rememberClient caches the clientInfo from an initialize request under
// the agent that sent it. It is the server.Hooks AfterInitialize hook.
func (g *Gateway) rememberClient(ctx context.Context, _ any, req *mcp.InitializeRequest, _ *mcp.InitializeResult) {
	if req == nil {
		return
	}
	name := actor.Clean(req.Params.ClientInfo.Name)
	if name == "" {
		return
	}
	ci := clientInfo{name: name, kind: ClientKindOf(name)}
	if v := actor.Clean(req.Params.ClientInfo.Version); v != "" {
		ci.name = name + "/" + v
	}
	g.clientMu.Lock()
	if g.clients == nil {
		g.clients = map[string]clientInfo{}
	}
	g.clients[agentIDFromContext(ctx)] = ci
	g.clientMu.Unlock()
}

// clientFor returns the cached clientInfo for an agent.
func (g *Gateway) clientFor(agentID string) (clientInfo, bool) {
	g.clientMu.Lock()
	defer g.clientMu.Unlock()
	ci, ok := g.clients[agentID]
	return ci, ok
}

// sessionCacheTTL bounds how often a `_session_id` is re-checked against
// the inbox (and heartbeated). Calls inside the window cost no query.
const sessionCacheTTL = 30 * time.Second

type sessionOwner struct {
	agentID   string // "" when the session does not exist
	checkedAt time.Time
}

// resolveRaiser builds the raiser for a call: the context's raiser (or a
// minimal one from the caller id), the cached clientInfo, and the kind
// and owner the caller id itself implies. via, when set, is the path in
// and wins over whatever the context said.
func (g *Gateway) resolveRaiser(ctx context.Context, callerID, via string) actor.Raiser {
	r, _ := actor.RaiserFrom(ctx)
	if callerID != "" {
		r.CallerID = callerID
	}
	if via != "" {
		r.Via = via
	}
	if ci, ok := g.clientFor(r.CallerID); ok {
		r = r.Merge(actor.Raiser{ClientName: ci.name, ClientKind: ci.kind})
	} else if r.ClientKind == "" && r.ClientName != "" {
		r.ClientKind = ClientKindOf(r.ClientName)
	}
	switch kind, owner := callerKind(r.CallerID); kind {
	case agentKindDashboard, agentKindVoice:
		r.AgentKind = kind
		if r.OwnerUserID == "" {
			r.OwnerUserID = owner
		}
	case agentKindAgent:
		if r.AgentKind == "" {
			r.AgentKind = agentKindAgent
		}
	}
	return cleanRaiser(r)
}

// cleanRaiser runs every caller-supplied field through actor.Clean, so a
// header or clientInfo value is safe to store on an approval as given.
func cleanRaiser(r actor.Raiser) actor.Raiser {
	for _, p := range []*string{&r.CallerID, &r.AgentName, &r.AgentKind, &r.OwnerUserID, &r.OwnerEmail, &r.OwnerName,
		&r.MCPSessionID, &r.AgentSessionID, &r.ClientSessionID, &r.ClientKind, &r.ClientName, &r.ClientIP, &r.Via} {
		*p = actor.Clean(*p)
	}
	return r
}

// callerKind reads the kind (and, for dashboard:/voice: callers, the
// owner user id) off a caller id.
func callerKind(callerID string) (kind, owner string) {
	switch {
	case strings.HasPrefix(callerID, "dashboard:"):
		return agentKindDashboard, strings.TrimPrefix(callerID, "dashboard:")
	case strings.HasPrefix(callerID, "voice:"):
		return agentKindVoice, strings.TrimPrefix(callerID, "voice:")
	case strings.HasPrefix(callerID, "ag_"):
		return agentKindAgent, ""
	}
	return "", ""
}

// attachSession applies the `_session_id` argument: the session must
// exist and belong to the calling agent, otherwise it is ignored (and
// logged) rather than trusted. A session that checks out is heartbeated.
func (g *Gateway) attachSession(ctx context.Context, r *actor.Raiser, args map[string]any) {
	sid, _ := args[SessionField].(string)
	sid = actor.Clean(sid)
	if sid == "" {
		return
	}
	if r.CallerID == "" || g.inbox == nil {
		log.Printf("session: %s ignored %s: no agent identity on the call", SessionField, sid)
		return
	}
	owner, ok := g.sessionAgent(ctx, sid)
	if !ok {
		log.Printf("session: %s ignored %s: no such session", SessionField, sid)
		return
	}
	if owner != r.CallerID {
		log.Printf("session: %s ignored %s: belongs to %s, not %s", SessionField, sid, owner, r.CallerID)
		return
	}
	r.AgentSessionID = sid
}

// sessionAgent returns the agent that owns a session, from a short-lived
// cache. A cache miss queries the inbox and heartbeats the session.
func (g *Gateway) sessionAgent(ctx context.Context, sid string) (string, bool) {
	now := time.Now()
	g.sessionMu.Lock()
	if e, ok := g.sessions[sid]; ok && now.Sub(e.checkedAt) < sessionCacheTTL {
		g.sessionMu.Unlock()
		return e.agentID, e.agentID != ""
	}
	g.sessionMu.Unlock()

	e := sessionOwner{checkedAt: now}
	if ss, err := g.inbox.GetSession(ctx, sid); err == nil && ss != nil {
		e.agentID = ss.AgentID
		g.inbox.HeartbeatSession(ctx, sid)
	}
	g.sessionMu.Lock()
	if g.sessions == nil {
		g.sessions = map[string]sessionOwner{}
	}
	if len(g.sessions) > 4096 {
		// Sessions are short-lived; a full cache just starts over.
		g.sessions = map[string]sessionOwner{}
	}
	g.sessions[sid] = e
	g.sessionMu.Unlock()
	return e.agentID, e.agentID != ""
}

// stampRaiser copies the raiser's identity onto a metrics event.
func stampRaiser(ev *metrics.Event, r actor.Raiser) {
	ev.AgentName = r.AgentName
	ev.OwnerUserID = r.OwnerUserID
	ev.ClientKind = r.ClientKind
	ev.SessionID = r.AgentSessionID
	if ev.SessionID == "" {
		ev.SessionID = r.MCPSessionID
	}
	if ev.Via == "" {
		ev.Via = r.Via
	}
}

// approvalDecider reads who decided an approval request and how
// (approval.Request.Decider maps rows written before the decider columns
// existed); an auto-rule response without one names the rule.
func approvalDecider(req *approval.Request) actor.Decider {
	d := req.Decider()
	if d.IsZero() && req.AutoDecidedBy != "" {
		d = actor.Decider{Via: actor.ViaAutoRule, Ref: req.AutoDecidedBy}
	}
	return d
}

// approvalMetrics fills the approval columns of a metrics event from the
// decided request: how it was decided, by whom, and how long it waited.
// Auto-rule decisions keep the historical "auto" via so existing analytics
// still group them.
func approvalMetrics(ev *metrics.Event, req *approval.Request) {
	d := approvalDecider(req)
	switch {
	case req.AutoDecidedBy != "":
		ev.ApprovalVia = "auto"
		ev.ApprovalDecider = req.AutoDecidedBy
	case !d.IsZero():
		ev.ApprovalVia = d.Via
		ev.ApprovalDecider = d.Legacy()
	}
	if req.DecidedAt > 0 && req.CreatedAt > 0 && req.DecidedAt >= req.CreatedAt {
		ev.ApprovalLatencyMs = int(req.DecidedAt - req.CreatedAt)
	}
}

// raiserOnCtx returns the raiser the routing path put on ctx, or a
// minimal one from the caller id when a path skipped resolveRaiser.
func raiserOnCtx(ctx context.Context, callerID string) actor.Raiser {
	if r, ok := actor.RaiserFrom(ctx); ok {
		if r.CallerID == "" {
			r.CallerID = callerID
		}
		return r
	}
	kind, owner := callerKind(callerID)
	return actor.Raiser{CallerID: callerID, AgentKind: kind, OwnerUserID: owner}
}

// clientCache and sessionCache live on the Gateway; declared here so the
// struct in server.go only has to name them.
type clientCache struct {
	clientMu sync.Mutex
	clients  map[string]clientInfo
}

type sessionCache struct {
	sessionMu sync.Mutex
	sessions  map[string]sessionOwner
}
