// Package visibility computes which tools each agent sees, given the
// current surface_mode setting and live usage counters. It exists as its own
// package so the gateway doesn't depend directly on usage / settings; that
// keeps the gateway test-only.
package visibility

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tusharbhardwaj/toolyard/internal/gateway"
	"github.com/tusharbhardwaj/toolyard/internal/settings"
	"github.com/tusharbhardwaj/toolyard/internal/usage"
)

// Provider implements gateway.VisibilityProvider.
type Provider struct {
	settings *settings.Service
	usage    *usage.Service
}

func New(s *settings.Service, u *usage.Service) *Provider {
	return &Provider{settings: s, usage: u}
}

// agentIDFromCtx mirrors gateway.agentIDFromContext, but is duplicated to
// avoid an import cycle. Both packages must agree on the context key shape;
// gateway exposes WithAgentID for setting it, and we read using the same
// unexported key by re-registering through that helper would be circular,
// so we accept a small amount of coupling: gateway.AgentIDFromContext is a
// public read accessor we add for this purpose.
func (p *Provider) agentID(ctx context.Context) string {
	return gateway.AgentIDFromContext(ctx)
}

// List returns the subset of tools that should be exposed to the calling
// agent. PinnedTools are always included; everything else is gated by
// surface_mode.
func (p *Provider) List(ctx context.Context, tools []mcp.Tool) []mcp.Tool {
	mode := p.surfaceMode()
	if mode == settings.SurfaceFull {
		// Full mode: nothing hidden. Return as-is so we don't mess with the
		// underlying slice headers.
		out := make([]mcp.Tool, len(tools))
		copy(out, tools)
		return out
	}

	// Always include the pinned set, regardless of mode.
	out := make([]mcp.Tool, 0, len(tools))
	pinnedSeen := map[string]struct{}{}
	for _, t := range tools {
		if gateway.IsPinned(t.Name) {
			out = append(out, t)
			pinnedSeen[t.Name] = struct{}{}
		}
	}

	if mode == settings.SurfaceRouterOnly {
		return out
	}

	// surface_mode == top_n: append up to N more tools chosen by the
	// agent's usage history (or, while cold, the overall ranking).
	allowed := p.topNNames(ctx)
	if len(allowed) == 0 {
		return out
	}
	allowSet := make(map[string]struct{}, len(allowed))
	for _, n := range allowed {
		allowSet[n] = struct{}{}
	}
	for _, t := range tools {
		if _, isPinned := pinnedSeen[t.Name]; isPinned {
			continue
		}
		if _, ok := allowSet[t.Name]; ok {
			out = append(out, t)
		}
	}
	return out
}

// IsVisible answers the cheap per-tool gating used for direct calls.
func (p *Provider) IsVisible(ctx context.Context, toolName string) bool {
	if gateway.IsPinned(toolName) {
		return true
	}
	mode := p.surfaceMode()
	switch mode {
	case settings.SurfaceFull:
		return true
	case settings.SurfaceRouterOnly:
		return false
	}
	for _, n := range p.topNNames(ctx) {
		if n == toolName {
			return true
		}
	}
	return false
}

// topNNames returns the names that the current agent should see in top_n
// mode (excluding the pinned set; List/IsVisible add those back in).
//
// Cold start rule: if the agent's total usage is below
// top_n_personalize_after, fall back to the overall top-N. Once the agent
// has crossed the threshold we use that agent's own counters.
func (p *Provider) topNNames(ctx context.Context) []string {
	if p.usage == nil {
		return nil
	}
	limit := p.settings.GetInt(settings.TopNCount, 20)
	threshold := int64(p.settings.GetInt(settings.TopNPersonalizeAfter, 100))

	agent := p.agentID(ctx)
	total, err := p.usage.TotalForAgent(ctx, agent)
	if err != nil {
		return nil
	}
	if agent == "" || total < threshold {
		names, _ := p.usage.TopOverall(ctx, limit)
		return names
	}
	names, _ := p.usage.TopForAgent(ctx, agent, limit)
	return names
}

func (p *Provider) surfaceMode() string {
	if p.settings == nil {
		return settings.SurfaceFull
	}
	return p.settings.SurfaceModeOrDefault()
}
