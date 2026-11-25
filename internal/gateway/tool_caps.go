package gateway

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tusharbhardwaj/toolyard/internal/actor"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/metrics"
)

// Daily caps on successful tool calls (TEC-2060). An admin may cap any
// tool: at most PerCallerDaily successful calls by one caller and at most
// TotalDaily by everyone, each UTC day (0: no cap). dispatch, the one
// place every execution path passes, takes a slot before the tool runs
// and gives it back when the call fails, so only successful calls count
// and a cap holds exactly, across replicas too (the counters live in the
// database). internal/toolcaps implements ToolCaps.

// ToolCap is one capped tool.
type ToolCap struct {
	// Upstream is the tool's server (or built-in group) name.
	Upstream string `json:"upstream"`
	// Tool is the full catalog name, as tools/list shows it.
	Tool string `json:"tool"`
	// PerCallerDaily is how many successful calls one caller may make per
	// UTC day; 0 is no cap.
	PerCallerDaily int64 `json:"per_caller_daily"`
	// TotalDaily is how many successful calls everyone together may make
	// per UTC day; 0 is no cap.
	TotalDaily int64  `json:"total_daily"`
	Note       string `json:"note"`
	CreatedAt  int64  `json:"created_at"`
	CreatedBy  string `json:"created_by,omitempty"`
	UpdatedAt  int64  `json:"updated_at"`
	UpdatedBy  string `json:"updated_by,omitempty"`
}

// ToolCapUsage is one counter of one UTC day: Caller's calls of Tool, or
// everyone's when Caller is CapTotalCaller.
type ToolCapUsage struct {
	Day      string `json:"day"`
	Upstream string `json:"upstream"`
	Tool     string `json:"tool"`
	Caller   string `json:"caller"`
	Calls    int64  `json:"calls"`
}

// CapTotalCaller is the caller of the counter that totals every caller.
const CapTotalCaller = "*"

// Cap scopes: which limit refused a call.
const (
	CapScopePerCaller = "per_caller"
	CapScopeTotal     = "total"
)

// CapRefusal says why a call was refused: which limit, how many calls it
// allows, and when the day ends (00:00 UTC).
type CapRefusal struct {
	Upstream string
	Tool     string
	Scope    string
	Limit    int64
	ResetAt  time.Time
}

// Message is the text the caller reads.
func (r *CapRefusal) Message() string {
	who := "you have"
	if r.Scope == CapScopeTotal {
		who = "everyone together has"
	}
	kind := "per-caller"
	if r.Scope == CapScopeTotal {
		kind = "total"
	}
	return fmt.Sprintf("%s has a daily cap: %s used its %s limit of %d successful call(s) today (UTC). "+
		"The call was not made. The cap resets at %s (00:00 UTC). An admin can change the cap on toolyard's Tools page.",
		r.Tool, who, kind, r.Limit, r.ResetAt.UTC().Format(time.RFC3339))
}

// ToolCaps keeps the caps and the daily counters.
type ToolCaps interface {
	// Reserve takes one slot of every cap on (upstream, tool) for the
	// caller that caller() names; caller is called only when the tool has
	// a cap. No cap: nil, nil, nil. A cap reached: the refusal, and no slot is
	// held. Otherwise release, which gives the slots back; dispatch calls
	// it when the call does not succeed.
	Reserve(ctx context.Context, upstream, tool string, caller func() string) (release func(), refused *CapRefusal, err error)
	// List returns every cap.
	List(ctx context.Context) ([]ToolCap, error)
	// Set creates or replaces the cap on c.Upstream and c.Tool.
	Set(ctx context.Context, c ToolCap, by string) (ToolCap, error)
	// Clear removes the cap on (upstream, tool); false when there was none.
	Clear(ctx context.Context, upstream, tool string) (bool, error)
	// Usage returns the counters of one UTC day (YYYY-MM-DD).
	Usage(ctx context.Context, day string) ([]ToolCapUsage, error)
	// Today is the current UTC day (YYYY-MM-DD) and when it ends.
	Today() (string, time.Time)
}

// ErrInvalidCap is a cap that cannot be saved (negative limits, both 0,
// no tool).
var ErrInvalidCap = errors.New("invalid tool cap")

// SetToolCaps wires the caps. nil turns them off.
func (g *Gateway) SetToolCaps(c ToolCaps) {
	g.mu.Lock()
	g.toolCaps = c
	g.mu.Unlock()
}

// ToolCaps returns the caps the gateway enforces, nil when none are wired.
// The dashboard's API changes them through it.
func (g *Gateway) ToolCaps() ToolCaps { return g.caps() }

func (g *Gateway) caps() ToolCaps {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.toolCaps
}

// capCaller is the caller a per-caller cap counts, and the person behind
// it: the person (every agent of one person shares their cap), else the
// gateway caller id itself, "anonymous" for a local unauthenticated
// caller.
func (g *Gateway) capCaller(ctx context.Context, callerID string) (caller, owner string) {
	if uid, err := g.ownerUser(ctx, callerID); err == nil && uid != "" {
		return "user:" + uid, uid
	}
	if callerID == "" {
		return "anonymous", ""
	}
	return callerID, ""
}

// reserveCap is dispatch's cap step. It returns the release func (nil
// when nothing was reserved) or the refusal result, already audited.
func (g *Gateway) reserveCap(ctx context.Context, entry toolEntry, agentID, reason, approvalID string, ev *metrics.Event) (func(), *mcp.CallToolResult) {
	c := g.caps()
	if c == nil {
		return nil, nil
	}
	var caller, owner string
	release, refused, err := c.Reserve(ctx, entry.upstream, entry.tool.Name, func() string {
		caller, owner = g.capCaller(ctx, agentID)
		return caller
	})
	if err != nil {
		log.Printf("tool-cap: tool=%s upstream=%s agent=%s: %v", entry.tool.Name, entry.upstream, agentID, err)
		g.writeCapRefusal(ctx, entry, agentID, reason, approvalID, owner, "cap: counter unavailable")
		ev.Outcome = metrics.OutcomeError
		ev.ErrorClass = "cap_unavailable"
		return nil, mcp.NewToolResultError(fmt.Sprintf("%s has a daily cap and toolyard could not check it now; the call was not made. Try again shortly.", entry.tool.Name))
	}
	if refused != nil {
		g.writeCapRefusal(ctx, entry, agentID, reason, approvalID, owner,
			fmt.Sprintf("cap: %s daily limit %d reached for %s; resets %s", refused.Scope, refused.Limit, caller, refused.ResetAt.UTC().Format(time.RFC3339)))
		ev.Outcome = metrics.OutcomeError
		ev.ErrorClass = "cap"
		res := mcp.NewToolResultError(refused.Message())
		res.StructuredContent = map[string]any{
			"status":   "cap_reached",
			"tool":     refused.Tool,
			"scope":    refused.Scope,
			"limit":    refused.Limit,
			"reset_at": refused.ResetAt.UTC().Format(time.RFC3339),
		}
		return nil, res
	}
	return release, nil
}

func (g *Gateway) writeCapRefusal(ctx context.Context, entry toolEntry, agentID, reason, approvalID, owner, summary string) {
	_ = g.audit.Write(ctx, audit.Event{
		EventType:     audit.EventCallFailed,
		AgentID:       agentID,
		UpstreamName:  entry.upstream,
		ToolName:      entry.tool.Name,
		Decision:      "refused",
		Reason:        reason,
		ApprovalID:    approvalID,
		ResultSummary: summary,
		Raiser:        actor.Raiser{OwnerUserID: owner},
	})
}

// EventToolCapChange is the audit event of a cap change an agent made, or
// asked for and was refused (decision says which). The dashboard's API
// writes the same event (internal/api).
const EventToolCapChange = "tool_cap.change"

// capTools are policies.list_caps, policies.set_cap and policies.clear_cap.
// Registered with the access tools.
func (g *Gateway) capTools() []toolEntry {
	toolProp := map[string]any{"type": "string", "description": "The tool's catalog name, as tools/list or tools.search shows it (e.g. bk.helium10-cerebro-keywords)."}
	return []toolEntry{
		accessEntry("policies.list_caps", policiesUpstream,
			"The daily caps on tools you may use: for each capped tool its per-caller and total limits of successful calls per UTC day (0: no cap), "+
				"how many successful calls everyone and your owner made today, and when the day resets (00:00 UTC). Read-only.",
			obj(nil, map[string]any{}),
			g.handleListCaps()),
		accessEntry("policies.set_cap", policiesUpstream,
			"Set a daily cap on one tool: per_caller_daily (successful calls one person may make per UTC day) and/or total_daily (successful calls everyone together may make per UTC day); 0 or absent means no cap of that kind. "+
				"Only successful calls count. A call over a cap is refused before the server is contacted, until 00:00 UTC. Replaces the tool's cap. Only an admin's agent; audited.",
			obj([]string{"tool"}, map[string]any{
				"tool":             toolProp,
				"per_caller_daily": map[string]any{"type": "integer", "minimum": 0},
				"total_daily":      map[string]any{"type": "integer", "minimum": 0},
				"note":             map[string]any{"type": "string", "description": "Why, for the people who read the Tools page."},
			}),
			func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
				return g.changeCap(ctx, args, false)
			}),
		accessEntry("policies.clear_cap", policiesUpstream,
			"Remove a tool's daily cap. Only an admin's agent; audited.",
			obj([]string{"tool"}, map[string]any{"tool": toolProp}),
			func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
				return g.changeCap(ctx, args, true)
			}),
	}
}

func (g *Gateway) handleListCaps() directHandler {
	return func(ctx context.Context, _ map[string]any) (*mcp.CallToolResult, error) {
		c := g.caps()
		if c == nil {
			return mcp.NewToolResultError("tool caps are not available on this gateway"), nil
		}
		agentID := agentIDFromContext(ctx)
		caps, err := c.List(ctx)
		if err != nil {
			return mcp.NewToolResultErrorf("list caps: %v", err), nil
		}
		day, reset := c.Today()
		usage, err := c.Usage(ctx, day)
		if err != nil {
			return mcp.NewToolResultErrorf("read today's use: %v", err), nil
		}
		mine, _ := g.capCaller(ctx, agentID)
		used := map[[3]string]int64{}
		for _, u := range usage {
			used[[3]string{u.Upstream, u.Tool, u.Caller}] = u.Calls
		}
		out := []map[string]any{}
		for _, cp := range caps {
			g.mu.RLock()
			e, ok := g.tools[cp.Tool]
			g.mu.RUnlock()
			if !ok || e.upstream != cp.Upstream || !g.allowsEntry(ctx, agentID, e) {
				continue
			}
			out = append(out, map[string]any{
				"tool":             cp.Tool,
				"server":           cp.Upstream,
				"per_caller_daily": cp.PerCallerDaily,
				"total_daily":      cp.TotalDaily,
				"note":             cp.Note,
				"used_today_total": used[[3]string{cp.Upstream, cp.Tool, CapTotalCaller}],
				"used_today_mine":  used[[3]string{cp.Upstream, cp.Tool, mine}],
			})
		}
		return inboxJSON(map[string]any{"day": day, "reset_at": reset.UTC().Format(time.RFC3339), "caps": out}), nil
	}
}

// changeCap is policies.set_cap (clear=false) and policies.clear_cap.
func (g *Gateway) changeCap(ctx context.Context, args map[string]any, clear bool) (*mcp.CallToolResult, error) {
	verb := "policies.set_cap"
	if clear {
		verb = "policies.clear_cap"
	}
	agentID := agentIDFromContext(ctx)
	reason := callReasonFromContext(ctx)
	name := strings.TrimSpace(stringArg(args, "tool"))
	audited := func(decision, upstream, summary string) {
		_ = g.audit.Write(ctx, audit.Event{EventType: EventToolCapChange, AgentID: agentID, UpstreamName: upstream, ToolName: name,
			Decision: decision, Reason: reason, ResultSummary: summary})
	}
	refuse := func(msg string) (*mcp.CallToolResult, error) {
		audited("refused", "", msg)
		res := mcp.NewToolResultError(msg)
		res.StructuredContent = map[string]any{"status": "refused"}
		return res, nil
	}
	if !enrolledAgent(agentID) {
		return refuse(verb + " needs an enrolled agent: connect with an agent token (Authorization: Bearer …). People change caps on the dashboard's Tools page.")
	}
	if sc := g.scopeFor(ctx, agentID); !sc.CanAdminister() {
		return refuse(verb + " refused: caps apply to every caller on this toolyard, so only an admin's agent that may manage toolyard can change them. Ask an admin.")
	}
	c := g.caps()
	if c == nil {
		return mcp.NewToolResultError("tool caps are not available on this gateway"), nil
	}
	if name == "" {
		return mcp.NewToolResultError("tool is required: the tool's catalog name"), nil
	}
	name = g.resolveToolName(name)
	g.mu.RLock()
	e, ok := g.tools[name]
	g.mu.RUnlock()
	if !ok {
		return mcp.NewToolResultErrorf("unknown tool %q: use the catalog name tools.search shows (its server must be connected)", name), nil
	}
	if clear {
		existed, err := c.Clear(ctx, e.upstream, name)
		if err != nil {
			return mcp.NewToolResultErrorf("clear cap: %v", err), nil
		}
		if !existed {
			return mcp.NewToolResultErrorf("%s has no cap", name), nil
		}
		audited("cleared", e.upstream, "cap removed")
		return inboxJSON(map[string]any{"status": "cleared", "tool": name, "server": e.upstream}), nil
	}
	per, _ := asInt(args["per_caller_daily"])
	total, _ := asInt(args["total_daily"])
	saved, err := c.Set(ctx, ToolCap{Upstream: e.upstream, Tool: name, PerCallerDaily: int64(per), TotalDaily: int64(total),
		Note: strings.TrimSpace(stringArg(args, "note"))}, g.ownerOf(ctx, agentID))
	if errors.Is(err, ErrInvalidCap) {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if err != nil {
		return mcp.NewToolResultErrorf("set cap: %v", err), nil
	}
	audited("applied", e.upstream, fmt.Sprintf("per_caller_daily=%d total_daily=%d", saved.PerCallerDaily, saved.TotalDaily))
	return inboxJSON(map[string]any{"status": "applied", "cap": saved}), nil
}
