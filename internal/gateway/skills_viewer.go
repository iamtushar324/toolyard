package gateway

import (
	"context"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/skillhub"
)

// skillViewer is who the ctx caller is to the skills hub, which filters
// every answer by it:
//
//   - UserID is the person whose sources the caller sees: an agent's
//     owner, or the dashboard (or voice) user themselves.
//   - AgentID is the calling agent ("ag_…"), "" for a dashboard user.
//   - Admin is true when the caller's owner reaches everything (an admin,
//     or a local unauthenticated caller on a gateway without access
//     control); an agent sees what its owner sees, so an admin's limited
//     agent is still an admin viewer. Admins see every source.
//   - OnHost is true only for an agent that runs on the T3 host (see
//     agentOnHost).
func (g *Gateway) skillViewer(ctx context.Context, hub *skillHubConfig) skillhub.Viewer {
	caller := agentIDFromContext(ctx)
	v := skillhub.Viewer{Admin: g.scopeFor(ctx, caller).OwnerIsAdmin()}
	switch kind, owner := callerKind(caller); kind {
	case agentKindDashboard, agentKindVoice:
		v.UserID = owner
	case agentKindAgent:
		v.AgentID = caller
		v.UserID = g.ownerOf(ctx, caller)
		v.OnHost = hub.agentOnHost(ctx, raiserOnCtx(ctx, caller).AgentKind, caller)
	}
	return v
}

// SkillViewer is the skills hub's view of the ctx caller (WithAgentID and
// the raiser on ctx), for the REST routes; the zero Viewer while the hub
// is off.
func (g *Gateway) SkillViewer(ctx context.Context) skillhub.Viewer {
	hub := g.skillHub.Load()
	if hub == nil {
		return skillhub.Viewer{}
	}
	return g.skillViewer(ctx, hub)
}

// Where an agent runs decides whether it sees the skills that need the T3
// host (runs: host) without asking for them. The rule:
//
//   - An agent whose credential is a T3 host connection or a federation
//     connection is on the host: toolyard minted that credential for a T3
//     environment, whose sessions run on the T3 host
//     (federation.Service.OnHost, wired as SkillHub.OnHost).
//   - Every other caller is not: an OAuth client's agent (Claude.ai,
//     ChatGPT, …; agent kind oauth_client) is never looked up, and an
//     ordinary agent key or an identity key is off the host because it
//     holds neither connection. Such a caller sees host skills only when a
//     call asks for them (include_host).
//   - What a client says about itself (MCP clientInfo, x-toolyard-client,
//     the User-Agent) is never part of the rule: it is the caller's word.
//
// A lookup that fails counts as off the host, so host skills stay hidden
// rather than shown by mistake, and is not cached.

// hostAnswer is one cached OnHost answer.
type hostAnswer struct {
	onHost bool
	at     time.Time
}

// hostCacheTTL bounds how stale a cached OnHost answer can be. A
// connection only changes when it is made or revoked, and a revoked
// credential stops authenticating at once anyway.
const hostCacheTTL = time.Minute

// maxHostCache bounds the cache; past it the cache starts over.
const maxHostCache = 4096

// agentOnHost applies the rule above to agentID, whose raiser reports
// agentKind.
func (h *skillHubConfig) agentOnHost(ctx context.Context, agentKind, agentID string) bool {
	if h.onHost == nil || agentID == "" || agentKind == identity.AgentKindOAuthClient {
		return false
	}
	now := time.Now()
	h.hostMu.Lock()
	if a, ok := h.hosts[agentID]; ok && now.Sub(a.at) < hostCacheTTL {
		h.hostMu.Unlock()
		return a.onHost
	}
	h.hostMu.Unlock()
	on, err := h.onHost(ctx, agentID)
	if err != nil {
		return false
	}
	h.hostMu.Lock()
	if len(h.hosts) >= maxHostCache {
		h.hosts = map[string]hostAnswer{}
	}
	h.hosts[agentID] = hostAnswer{onHost: on, at: now}
	h.hostMu.Unlock()
	return on
}
