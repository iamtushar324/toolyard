package gateway

import (
	"context"

	"github.com/tusharbhardwaj/toolyard/internal/policy"
	"github.com/tusharbhardwaj/toolyard/internal/toolsearch"
)

// Run statuses of a tools.search result: what a call to the tool with no
// arguments would get now.
const (
	// RunNow: the call runs at once.
	RunNow = "now"
	// RunNeedsApproval: the call needs a person's (or a grant's) yes;
	// ask with inbox.request.
	RunNeedsApproval = "needs_approval"
	// RunBlocked: a policy denies the call.
	RunBlocked = "blocked"
)

// runStatuses is the run status of each hit, in order: the routing
// decision (decide) for a call by the ctx caller with no arguments and no
// declared intent, the one Access gives inbox.check. Search already left
// out the tools the caller cannot reach, so none reads as unknown here. A
// call whose arguments escalate it, or that declares a write intent, may
// still need approval where this says now. "" when the gateway has no
// policy engine or no longer has the tool.
func (g *Gateway) runStatuses(ctx context.Context, hits []toolsearch.Hit) []string {
	out := make([]string, len(hits))
	if g.policy == nil || len(hits) == 0 {
		return out
	}
	entries := make([]toolEntry, len(hits))
	found := make([]bool, len(hits))
	g.mu.RLock()
	for i, h := range hits {
		entries[i], found[i] = g.tools[h.Tool.Name]
	}
	g.mu.RUnlock()
	agentID := agentIDFromContext(ctx)
	for i, e := range entries {
		if found[i] {
			out[i] = runStatus(g.decide(e, agentID, "", "", nil))
		}
	}
	return out
}

// runStatus is a decision's run status.
func runStatus(d policy.Decision) string {
	switch d.Action {
	case policy.ActionAllow:
		return RunNow
	case policy.ActionDeny:
		return RunBlocked
	default:
		return RunNeedsApproval
	}
}
