package gateway

// access_tools.go is the agent-facing view of toolyard's own access
// control: the policies.*, servers.*, audit.* and access.* tools. They are
// internal tools (no approval for themselves), scoped to the calling
// agent's owner, and audited.
//
// The one rule that matters is enforced here, server-side, on every
// policy change an agent asks for: a change may move a tool INTO ask, or
// between allow and deny, but never OUT of ask. Before writing anything
// the gateway works out every tool the change touches, evaluates the
// policy engine before and after on a detached copy (static: no declared
// intent, no arguments), and rejects the change if any tool would go from
// ask to allow or deny. Only a person on the dashboard can relax an ask.
// Policies are global, so only an admin's agent may change them at all.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tusharbhardwaj/toolyard/internal/access"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/policy"
	"github.com/tusharbhardwaj/toolyard/internal/realtime"
)

// Synthetic upstreams for the access tools. Each is an always-on access
// group (access.alwaysOn) and a reserved upstream name.
const (
	policiesUpstream = "policies"
	serversUpstream  = "servers"
	auditUpstream    = "audit"
	accessUpstream   = "access"

	// EventPolicyChange is the audit event for a policy change an agent
	// made, or asked for and was refused (decision says which).
	EventPolicyChange = "policy.change"
	// EventServerReconnect is the audit event for servers.reconnect.
	EventServerReconnect = "server.reconnect"
	// hubEventPolicy is the realtime (SSE) event type a policy change is
	// published as, beside the audit row's own "audit" event.
	hubEventPolicy = "policy"

	// Effective access words, as policies.* report them.
	AccessAllow = "allow"
	AccessAsk   = "ask"
	AccessDeny  = "deny"
)

// AccessToolNames are always visible to agents.
var AccessToolNames = []string{
	"policies.list", "policies.explain", "policies.set", "policies.clear",
	"servers.list", "servers.reconnect", "audit.mine", "access.whoami",
}

func init() {
	for _, n := range AccessToolNames {
		PinnedTools[n] = struct{}{}
	}
}

// accessToolsState lives on the Gateway (server.go embeds it). Agent
// policy changes are serialised with every other policy write by the
// engine's own write lock (policy.Engine.Change).
type accessToolsState struct {
	servers      ServersProvider
	autoApproval AutoApprovalPolicies
}

// ServerInfo is what an agent may learn about one upstream server. It
// never carries a URL, a header, an env value or a token.
type ServerInfo struct {
	Name      string `json:"name"`
	Transport string `json:"transport,omitempty"`
	Enabled   bool   `json:"enabled"`
	// Status: ok | error | waiting_signin | disabled | idle | unknown.
	Status string `json:"status"`
	// Error is the last connection error, redacted and shortened.
	Error     string `json:"error,omitempty"`
	ToolCount int    `json:"tool_count"`
	// AuthMode: shared (one account for everyone) or per_user (each person
	// signs in from My connections).
	AuthMode string `json:"auth_mode"`
	// OAuth is set when the server authenticates with a stored sign-in
	// (OAuth or a pasted token).
	OAuth bool `json:"oauth"`
	// SignedIn, for an OAuth server: on a shared server, whether a sign-in
	// is stored; on a per_user server, whether the calling agent's owner
	// has connected it. nil when not applicable.
	SignedIn *bool `json:"signed_in,omitempty"`
}

// ServersProvider is the upstream service as the access tools see it.
// Implemented in cmd/gateway over internal/upstreams and internal/oauth
// (the gateway cannot import either).
type ServersProvider interface {
	// ListServers lists every configured server. ownerUserID is the
	// calling agent's owner, for per_user sign-in status; "" when unknown.
	ListServers(ctx context.Context, ownerUserID string) ([]ServerInfo, error)
	// ReconnectServer drops the server's connection and dials again. A
	// non-nil info beside an error carries the status the failure left.
	ReconnectServer(ctx context.Context, name string) (*ServerInfo, error)
}

// AutoApprovalPolicies is the slice of the auto-approval service the
// access tools use: an explicit ask or deny on a tool disables its learned
// auto-approval rule, as the dashboard does, so the two never fight.
type AutoApprovalPolicies interface {
	SetToolPolicy(ctx context.Context, toolName string, autoApprove bool) error
}

// SetServersProvider wires servers.list and servers.reconnect. Without it
// servers.list answers from the gateway's own connection pool and
// servers.reconnect is unavailable.
func (g *Gateway) SetServersProvider(p ServersProvider) { g.servers = p }

// SetAutoApproval wires the anti-fight hook for policies.set.
func (g *Gateway) SetAutoApproval(a AutoApprovalPolicies) { g.autoApproval = a }

func accessEntry(name, upstream, desc string, schema mcp.ToolInputSchema, h directHandler) toolEntry {
	allow := policy.ActionAllow
	return toolEntry{
		tool:         mcp.Tool{Name: name, Description: desc, InputSchema: schema},
		upstream:     upstream,
		originalName: strings.TrimPrefix(name, upstream+"."),
		reasonField:  ReasonField,
		handle:       h,
		forcedAction: &allow,
	}
}

// accessTools returns the policies.*, servers.*, audit.* and access.*
// entries. Registered by RegisterBuiltins.
func (g *Gateway) accessTools() []toolEntry {
	scopeProp := map[string]any{"type": "string", "enum": []string{policy.ScopeTool, policy.ScopeUpstream},
		"description": "tool: one tool by its catalog name (github.create_issue). upstream: every tool of one server, by server name (github)."}
	targetProp := map[string]any{"type": "string", "description": "The tool's catalog name (scope tool) or the server name (scope upstream)."}
	return []toolEntry{
		accessEntry("policies.list", policiesUpstream,
			"The explicit tool policies (scope, target, allow|ask|deny, note) and, for every tool you may use, its EFFECTIVE access and the rule that decides it. "+
				"Optional server limits the tool list to one server. Read-only.",
			obj(nil, map[string]any{"server": map[string]any{"type": "string", "description": "Only tools of this server (upstream name or built-in group such as memory)."}}),
			g.handlePoliciesList()),
		accessEntry("policies.explain", policiesUpstream,
			"Why a tool runs, needs approval, or is denied: its effective access (allow|ask|deny) and the rule behind it, in plain words you can repeat to a person. Read-only.",
			obj([]string{"tool"}, map[string]any{"tool": map[string]any{"type": "string", "description": "Catalog name, e.g. github.create_issue."}}),
			g.handlePoliciesExplain()),
		accessEntry("policies.set", policiesUpstream,
			"Set an explicit policy: allow, ask or deny for one tool (scope tool) or a whole server (scope upstream). Takes effect immediately. "+
				"Only an admin's agent may change policies. THE RULE: a change may put tools into ask, or move them between allow and deny, but is rejected if it would take ANY affected tool out of ask "+
				"(an ask by policy, where a person must decide, may not become an ask by default either); only a person on the dashboard can relax an ask. "+
				"allow only lifts a deny on one tool; a server-wide allow, and anything that would open a destructive-looking tool (delete, remove, drop, …), is refused.",
			obj([]string{"scope", "target", "access"}, map[string]any{
				"scope":  scopeProp,
				"target": targetProp,
				"access": map[string]any{"type": "string", "enum": []string{AccessAllow, AccessAsk, AccessDeny}},
				"note":   map[string]any{"type": "string", "description": "Why, for the people who read the policies page."},
			}),
			func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
				return g.changePolicy(ctx, args, false)
			}),
		accessEntry("policies.clear", policiesUpstream,
			"Remove an explicit policy so the tool or server falls back to the rules below it (an upstream policy, then toolyard's defaults). "+
				"Only an admin's agent; rejected if clearing it would take any tool out of ask.",
			obj([]string{"scope", "target"}, map[string]any{"scope": scopeProp, "target": targetProp}),
			func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
				return g.changePolicy(ctx, args, true)
			}),
		accessEntry("servers.list", serversUpstream,
			"The MCP servers your owner may use: name, enabled, connection status, tool count, auth mode (shared or per_user) and, for servers with a stored sign-in, whether one exists. Never URLs, headers or secrets. Read-only.",
			obj(nil, map[string]any{}),
			g.handleServersList()),
		accessEntry("servers.reconnect", serversUpstream,
			"Drop a server's connection and dial it again, e.g. after its status shows an error. Only an admin's agent; audited.",
			obj([]string{"server"}, map[string]any{"server": map[string]any{"type": "string", "description": "Server name, as servers.list shows it."}}),
			g.handleServersReconnect()),
		accessEntry("audit.mine", auditUpstream,
			"Your own recent calls and how toolyard decided them (tool, decision, reason, time). Only your rows, never another agent's; no arguments or result bodies. Read-only.",
			obj(nil, map[string]any{
				"limit":         map[string]any{"type": "integer", "minimum": 1, "maximum": auditMineMaxLimit, "description": "Default 50, max 200."},
				"since":         map[string]any{"type": "string", "description": "Only rows at or after this RFC 3339 time (or unix milliseconds as a string)."},
				"since_minutes": map[string]any{"type": "integer", "description": "Only rows from the last N minutes."},
			}),
			g.handleAuditMine()),
		accessEntry("access.whoami", accessUpstream,
			"Who toolyard thinks you are: your agent, its owner (email, role), the servers you were granted, and whether you may change policies. Read-only.",
			obj(nil, map[string]any{}),
			g.handleAccessWhoami()),
	}
}

// ---- effective access -------------------------------------------------------

// policyTarget is one tool the ask rule evaluates.
type policyTarget struct {
	name     string
	upstream string
	forced   *policy.Action
}

func targetOf(e toolEntry) policyTarget {
	return policyTarget{name: e.tool.Name, upstream: e.upstream, forced: e.forcedAction}
}

// accessWord maps an action to the word policies.* report.
func accessWord(a policy.Action) string {
	switch a {
	case policy.ActionAllow:
		return AccessAllow
	case policy.ActionDeny:
		return AccessDeny
	}
	return AccessAsk
}

// accessState is a tool's effective access as the ask rule sees it: the
// word, and whether an explicit ask policy decided it. That flag is
// Decision.RequireHuman: the approval bus then skips the learned
// auto-approval rules, so a person decides. Losing it while the word
// stays "ask" is a relaxation too.
type accessState struct {
	Word  string
	Human bool
}

// String is the state as a rejection names it.
func (s accessState) String() string {
	if s.Word == AccessAsk && s.Human {
		return "ask by policy"
	}
	return s.Word
}

// effectiveAccess is a tool's static access under engine: a built-in's
// forced action, else the engine's decision for a call with no declared
// intent and no arguments, exactly as routeEntry would start from.
func effectiveAccess(engine *policy.Engine, t policyTarget) (accessState, policy.Decision) {
	if t.forced != nil {
		d := policy.Decision{Action: *t.forced, Reason: "built-in tool policy", RuleID: "builtin-forced-" + string(*t.forced)}
		return accessState{Word: accessWord(d.Action)}, d
	}
	d := engine.Eval(policy.Request{UpstreamName: t.upstream, ToolName: t.name})
	return accessState{Word: accessWord(d.Action), Human: d.RequireHuman}, d
}

// explainRule says in plain words which rule decided d.
func explainRule(d policy.Decision) string {
	switch {
	case strings.HasPrefix(d.RuleID, "builtin-forced-"):
		return "built-in toolyard tool: always " + accessWord(d.Action) + ", no policy applies"
	case strings.HasPrefix(d.RuleID, "tp_"):
		scope := "tool"
		if strings.Contains(d.Reason, "upstream-policy") {
			scope = "server"
		}
		return fmt.Sprintf("an explicit %s policy (%s) says %s", scope, d.RuleID, accessWord(d.Action))
	case d.RuleID == "v0.1-meta-tool":
		return "the tools.* group is open unless a tool policy says otherwise"
	case d.RuleID == "v0.1-name-heuristic":
		return "no explicit policy; its name starts with a read verb (get, list, search, …), so toolyard lets it run without approval"
	case d.RuleID == "v0.1-default-write":
		return "no explicit policy; its name does not look read-only, so toolyard's default applies: writes need approval"
	}
	return d.Reason
}

func accessPhrase(word string) string {
	switch word {
	case AccessAllow:
		return "runs without approval (allow)"
	case AccessDeny:
		return "is denied (deny)"
	}
	return "needs approval (ask)"
}

// upstreamProbes are the synthetic tools every upstream-scope change is
// judged by, beside the tools registered right now: a read (allow by
// default), a write (ask by default) and a destructive write. They stand
// for the tools the server does not have yet, or has not re-registered
// yet (a reconnect loads tools one by one), so a server-wide rule can
// never be relaxed on the strength of what happens to be loaded.
var upstreamProbes = []string{"get_probe", "probe", "delete_probe"}

// policyTargets lists the tools a (scope,target) change touches. Tool
// scope: that tool; an unregistered name is judged by the name alone, as
// the engine would judge it once it appears. Upstream scope: every tool
// registered for that upstream, plus the probes.
func (g *Gateway) policyTargets(scope, target string) []policyTarget {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if scope == policy.ScopeTool {
		if e, ok := g.tools[target]; ok {
			return []policyTarget{targetOf(e)}
		}
		up := target
		if i := strings.IndexByte(target, '.'); i > 0 {
			up = target[:i]
		}
		return []policyTarget{{name: target, upstream: up}}
	}
	var out []policyTarget
	seen := map[string]bool{}
	for _, e := range g.tools {
		if e.upstream == target {
			out = append(out, targetOf(e))
			seen[e.tool.Name] = true
		}
	}
	for _, p := range upstreamProbes {
		if name := target + "." + p; !seen[name] {
			out = append(out, policyTarget{name: name, upstream: target})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// policyDelta is one tool whose effective access a change moves.
// RequireHuman says an explicit ask decided the state.
type policyDelta struct {
	Tool               string `json:"tool"`
	Before             string `json:"before"`
	After              string `json:"after"`
	RequireHumanBefore bool   `json:"require_human_before,omitempty"`
	RequireHumanAfter  bool   `json:"require_human_after,omitempty"`
}

// errPolicyChangeRejected is what the ask-rule guard returns; the
// message is the agent's answer.
type errPolicyChangeRejected struct {
	msg     string
	reason  string
	changes []policyDelta
}

func (e *errPolicyChangeRejected) Error() string { return e.msg }

// policyChangeGuard is the rule an agent's change must pass, evaluated
// under the engine's write lock against the engine as it is and as it
// would be. It returns the per-tool changes and the before states for
// the audit row, or an *errPolicyChangeRejected naming the tools.
//
//   - Out of ask is never allowed: a tool at ask (by default or by
//     policy) may not end at allow or deny.
//   - An ask decided by a policy may not become an ask by default: the
//     word is the same, but a person no longer has to decide (learned
//     auto-approval rules apply again).
//   - No destructive-looking tool (delete, remove, drop, …) may be
//     opened: one that was not allow may not end at allow, whatever
//     verb and scope got it there.
//   - A tool-scope allow only lifts a deny. On a tool that already runs
//     it would change nothing today and pin the tool open against a
//     later server-wide ask.
func policyChangeGuard(verb, scope, want string, targets []policyTarget) (
	guard func(before, after *policy.Engine) error, changes *[]policyDelta, before map[string]string) {
	changes = &[]policyDelta{}
	before = map[string]string{}
	guard = func(cur, next *policy.Engine) error {
		var leaving, opened []string
		*changes = (*changes)[:0]
		for _, t := range targets {
			b, _ := effectiveAccess(cur, t)
			a, _ := effectiveAccess(next, t)
			before[t.name] = b.String()
			if scope == policy.ScopeTool && want == AccessAllow && b.Word == AccessAllow {
				return &errPolicyChangeRejected{
					reason: "allow_only_lifts_deny",
					msg: fmt.Sprintf("%s rejected: %s is %s already, so an explicit allow would change nothing today and only pin it open against a later server-wide ask. "+
						"Agents use allow only to lift a deny; leave a tool that runs alone.", verb, t.name, b),
				}
			}
			if b == a {
				continue
			}
			*changes = append(*changes, policyDelta{Tool: t.name, Before: b.Word, After: a.Word, RequireHumanBefore: b.Human, RequireHumanAfter: a.Human})
			switch {
			case b.Word == AccessAsk && a.Word != AccessAsk:
				leaving = append(leaving, fmt.Sprintf("%s (ask → %s)", t.name, a.Word))
			case b.Word == AccessAsk && b.Human && !a.Human:
				leaving = append(leaving, fmt.Sprintf("%s (ask by policy → ask by default, where an auto-approval rule may decide)", t.name))
			}
			if policy.IsDestructiveName(t.name) && a.Word == AccessAllow && b.Word != AccessAllow {
				opened = append(opened, fmt.Sprintf("%s (%s → allow)", t.name, b))
			}
		}
		if len(leaving) > 0 {
			return &errPolicyChangeRejected{
				reason:  "ask_is_final_for_agents",
				changes: append([]policyDelta(nil), *changes...),
				msg: fmt.Sprintf("%s rejected: it would take %d tool(s) out of ask: %s. "+
					"Agents may put tools into ask, or move them between allow and deny; only a person on the dashboard can relax an ask.",
					verb, len(leaving), strings.Join(leaving, ", ")),
			}
		}
		if len(opened) > 0 {
			return &errPolicyChangeRejected{
				reason:  "destructive_never_opened",
				changes: append([]policyDelta(nil), *changes...),
				msg: fmt.Sprintf("%s rejected: it would open %d destructive-looking tool(s) (delete, remove, drop, …): %s. "+
					"Agents never allow such a tool, by any rule; a person can do it from the dashboard.",
					verb, len(opened), strings.Join(opened, ", ")),
			}
		}
		return nil
	}
	return guard, changes, before
}

// ---- policies.* -------------------------------------------------------------

type effectiveRow struct {
	Tool   string `json:"tool"`
	Server string `json:"server"`
	Access string `json:"access"`
	// RequireHuman: an explicit ask decided it, so a person must decide
	// each call (learned auto-approval rules do not apply).
	RequireHuman bool   `json:"require_human,omitempty"`
	RuleID       string `json:"rule_id"`
	Rule         string `json:"rule"`
}

const policyChangeHint = "An admin's agent can put a tool into ask, move it between allow and deny, or lift a deny on one tool with policies.set; only a person on the dashboard can take a tool out of ask, allow a whole server, or open a destructive-looking tool."

func (g *Gateway) handlePoliciesList() directHandler {
	return func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		if g.policy == nil {
			return mcp.NewToolResultError("the policy engine is not available on this gateway"), nil
		}
		server := strings.TrimSpace(stringArg(args, "server"))
		scope := g.scopeFor(ctx, agentIDFromContext(ctx))

		g.mu.RLock()
		entries := make([]toolEntry, 0, len(g.tools))
		for _, e := range g.tools {
			entries = append(entries, e)
		}
		g.mu.RUnlock()
		known := map[string]toolEntry{}
		rows := make([]effectiveRow, 0, len(entries))
		for _, e := range entries {
			if !scope.AllowsTool(e.upstream, e.tool.Name) {
				continue
			}
			known[e.tool.Name] = e
			group := access.GroupOf(e.upstream, e.tool.Name)
			if server != "" && server != e.upstream && server != group {
				continue
			}
			st, d := effectiveAccess(g.policy, targetOf(e))
			rows = append(rows, effectiveRow{Tool: e.tool.Name, Server: group, Access: st.Word, RequireHuman: st.Human, RuleID: d.RuleID, Rule: explainRule(d)})
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i].Tool < rows[j].Tool })

		// Explicit policies: a member sees only those about tools and
		// servers in their scope, so the list never names a server they
		// were not granted.
		pols := make([]policy.ToolPolicy, 0)
		for _, p := range g.policy.List() {
			switch {
			case scope.All:
			case p.Scope == policy.ScopeTool:
				if _, ok := known[p.Target]; !ok {
					continue
				}
			default:
				if !scope.Allows(p.Target) {
					continue
				}
			}
			if server != "" && p.Target != server && !strings.HasPrefix(p.Target, server+".") {
				continue
			}
			pols = append(pols, p)
		}
		sort.Slice(pols, func(i, j int) bool {
			if pols[i].Scope != pols[j].Scope {
				return pols[i].Scope < pols[j].Scope
			}
			return pols[i].Target < pols[j].Target
		})
		out := map[string]any{
			"policies": pols,
			"tools":    rows,
			"rule":     policyChangeHint,
		}
		if server != "" {
			out["server"] = server
		}
		return inboxJSON(out), nil
	}
}

func (g *Gateway) handlePoliciesExplain() directHandler {
	return func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		if g.policy == nil {
			return mcp.NewToolResultError("the policy engine is not available on this gateway"), nil
		}
		tool := strings.TrimSpace(stringArg(args, "tool"))
		if tool == "" {
			return mcp.NewToolResultError("tool is required"), nil
		}
		g.mu.RLock()
		e, ok := g.tools[tool]
		g.mu.RUnlock()
		// Outside the caller's scope reads as unknown, like everywhere.
		if !ok || !g.allowsEntry(ctx, agentIDFromContext(ctx), e) {
			return notFoundResult(tool), nil
		}
		st, d := effectiveAccess(g.policy, targetOf(e))
		var b strings.Builder
		fmt.Fprintf(&b, "%s %s: %s.", tool, accessPhrase(st.Word), explainRule(d))
		switch {
		case st.Word == AccessAllow && e.forcedAction == nil:
			b.WriteString(" A call that declares _intent_category write, destructive, external_communication, financial or privileged_admin still needs approval.")
		case st.Word == AccessAsk && st.Human:
			b.WriteString(" In inbox mode, ask your owner with inbox.request; in execute mode the call is queued and runs only when a person allows it (learned auto-approval rules do not apply).")
		case st.Word == AccessAsk:
			b.WriteString(" In inbox mode, ask your owner with inbox.request; in execute mode the call is queued and runs when a person, or a learned auto-approval rule, allows it.")
		}
		b.WriteString(" " + policyChangeHint)
		out := map[string]any{
			"tool":          tool,
			"server":        access.GroupOf(e.upstream, e.tool.Name),
			"access":        st.Word,
			"require_human": st.Human,
			"rule_id":       d.RuleID,
			"rule":          explainRule(d),
			"explanation":   b.String(),
		}
		if p, ok := g.policy.Get(policy.ScopeTool, tool); ok {
			out["tool_policy"] = p
		}
		if p, ok := g.policy.Get(policy.ScopeUpstream, e.upstream); ok {
			out["server_policy"] = p
		}
		res := mcp.NewToolResultText(b.String())
		res.StructuredContent = out
		return res, nil
	}
}

// changePolicy is policies.set (clear=false) and policies.clear.
func (g *Gateway) changePolicy(ctx context.Context, args map[string]any, clear bool) (*mcp.CallToolResult, error) {
	if g.policy == nil {
		return mcp.NewToolResultError("the policy engine is not available on this gateway"), nil
	}
	agentID := agentIDFromContext(ctx)
	scope := strings.TrimSpace(stringArg(args, "scope"))
	target := strings.TrimSpace(stringArg(args, "target"))
	want := strings.TrimSpace(stringArg(args, "access"))
	note := strings.TrimSpace(stringArg(args, "note"))
	reason := callReasonFromContext(ctx)
	verb := "policies.set"
	if clear {
		verb, want, note = "policies.clear", "", ""
	}
	if scope != policy.ScopeTool && scope != policy.ScopeUpstream {
		return mcp.NewToolResultError("scope must be tool or upstream"), nil
	}
	if target == "" {
		return mcp.NewToolResultError("target is required: a tool's catalog name (scope tool) or a server name (scope upstream)"), nil
	}
	if !clear && want != AccessAllow && want != AccessAsk && want != AccessDeny {
		return mcp.NewToolResultError("access must be allow, ask or deny"), nil
	}
	change := map[string]any{"scope": scope, "target": target, "access": want, "note": note}
	refuse := func(reasonCode, msg string) (*mcp.CallToolResult, error) {
		g.auditPolicyChange(ctx, agentID, scope, target, "refused", reason, msg, change, nil)
		res := mcp.NewToolResultError(msg)
		res.StructuredContent = map[string]any{"status": "refused", "reason": reasonCode}
		return res, nil
	}

	// Who may change: an enrolled agent (a local unauthenticated caller
	// and the dashboard's own callers have the Policies page), and only
	// one whose owner is an admin, since policies are global to this
	// toolyard. A member's agent is told so, and the attempt is on record.
	if !enrolledAgent(agentID) {
		return refuse("agent_required", fmt.Sprintf("%s needs an enrolled agent: connect with an agent token (Authorization: Bearer …). People change policies on the dashboard's Policies page.", verb))
	}
	if !g.scopeFor(ctx, agentID).All {
		return refuse("admin_only", fmt.Sprintf("%s refused: policies apply to every agent on this toolyard, so only an admin's agent may change them, and your owner is a member. "+
			"Ask an admin, or use policies.explain to tell your owner why a call needs approval.", verb))
	}

	// What the change does, tool by tool, judged and written under the
	// engine's write lock so no dashboard edit lands in between.
	targets := g.policyTargets(scope, target)
	guard, changes, before := policyChangeGuard(verb, scope, want, targets)
	pol, existed, err := g.policy.Change(ctx, scope, target, want, note, false, guard)
	change["changes"] = *changes
	if err != nil {
		var rej *errPolicyChangeRejected
		if errors.As(err, &rej) {
			g.auditPolicyChange(ctx, agentID, scope, target, "rejected", reason, rej.msg, change, rej.changes)
			res := mcp.NewToolResultError(rej.msg)
			res.StructuredContent = map[string]any{"status": "rejected", "reason": rej.reason, "would_change": rej.changes}
			return res, nil
		}
		msg := verb + " failed: " + err.Error()
		if errors.Is(err, policy.ErrForceRequired) {
			msg = fmt.Sprintf("%s refused: %q looks destructive (delete, remove, drop, …) and agents never force-allow such a tool. A person can do it from the dashboard.", verb, target)
		}
		g.auditPolicyChange(ctx, agentID, scope, target, "refused", reason, msg, change, *changes)
		return mcp.NewToolResultError(msg), nil
	}
	if clear && !existed {
		res := mcp.NewToolResultText(fmt.Sprintf("no explicit %s policy for %s; nothing to clear", scope, target))
		res.StructuredContent = map[string]any{"status": "unchanged", "scope": scope, "target": target}
		return res, nil
	}
	// Anti-fight, as the dashboard does: an explicit ask or deny disables
	// the learned auto-approval rule of every tool it covers.
	if !clear && want != AccessAllow && g.autoApproval != nil {
		for _, t := range targets {
			if !strings.HasSuffix(t.name, ".get_probe") && !strings.HasSuffix(t.name, ".probe") && !strings.HasSuffix(t.name, ".delete_probe") {
				_ = g.autoApproval.SetToolPolicy(ctx, t.name, false)
			}
		}
	}

	decision := want
	if clear {
		decision = "clear"
	}
	summary := fmt.Sprintf("%s %s %s", verb, scope, target)
	if !clear {
		summary += " → " + want
	}
	summary += fmt.Sprintf(": %d of %d affected tool(s) change", len(*changes), len(targets))
	g.auditPolicyChange(ctx, agentID, scope, target, decision, reason, summary, change, *changes)
	if g.hub != nil {
		raiser := raiserOnCtx(ctx, agentID)
		g.hub.Publish(realtime.Event{Type: hubEventPolicy, Data: map[string]any{
			"ts": time.Now().UnixMilli(), "scope": scope, "target": target, "access": decision,
			"policy": pol, "changes": *changes, "agent_id": agentID, "agent_name": raiser.AgentName,
			"owner_user_id": raiser.OwnerUserID, "owner_email": raiser.OwnerEmail, "reason": reason,
		}})
	}
	text := summary + "."
	if len(*changes) == 0 {
		text += " Every affected tool keeps the access it had; the explicit policy is on record."
	}
	out := map[string]any{
		"status": "applied", "scope": scope, "target": target, "access": decision, "message": text,
		"changed": *changes, "affected_tools": len(targets), "before": before,
	}
	if pol != nil {
		out["policy"] = pol
	}
	return inboxJSON(out), nil
}

// enrolledAgent reports whether callerID is an enrolled agent ("ag_…"), as
// opposed to a local unauthenticated caller or a dashboard/voice session.
func enrolledAgent(callerID string) bool {
	return strings.HasPrefix(callerID, "ag_") && len(callerID) > len("ag_")
}

// auditPolicyChange records who asked for what and what it did to every
// affected tool. The raiser on ctx (agent, owner, client) is merged in by
// audit.Write; the audit subscription publishes the row to the dashboard's
// live feed.
func (g *Gateway) auditPolicyChange(ctx context.Context, agentID, scope, target, decision, reason, summary string, change map[string]any, changes []policyDelta) {
	if changes != nil {
		change["changes"] = changes
	}
	argsJSON, _ := json.Marshal(change)
	ev := audit.Event{
		EventType:     EventPolicyChange,
		AgentID:       agentID,
		Decision:      decision,
		Reason:        reason,
		Arguments:     argsJSON,
		ResultSummary: summary,
	}
	if scope == policy.ScopeTool {
		ev.ToolName = target
		ev.UpstreamName = target
		if i := strings.IndexByte(target, '.'); i > 0 {
			ev.UpstreamName = target[:i]
		}
		g.mu.RLock()
		if e, ok := g.tools[target]; ok {
			ev.UpstreamName = e.upstream
		}
		g.mu.RUnlock()
	} else {
		ev.UpstreamName = target
	}
	_ = g.audit.Write(ctx, ev)
}

// ---- servers.* --------------------------------------------------------------

// ownerOf is the dashboard user behind the caller: the ingress raiser's
// owner, else the owner resolver's answer, else "".
func (g *Gateway) ownerOf(ctx context.Context, callerID string) string {
	if r := raiserOnCtx(ctx, callerID); r.OwnerUserID != "" {
		return r.OwnerUserID
	}
	if g.owners != nil && callerID != "" {
		if uid, err := g.owners.OwnerUser(ctx, callerID); err == nil {
			return uid
		}
	}
	return ""
}

// urlRE matches a URL: scheme, optional userinfo, host (with port), and
// the rest up to whitespace, leaving trailing punctuation (": refused",
// "." at a sentence end) to the surrounding text.
var urlRE = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.\-]*://)(?:[^\s/@"'<>]*@)?([^\s/?#"'<>]+)(?:[^\s"'<>:.,;)]|[:.,;)]+[^\s"'<>:.,;)])*`)

// sanitizeServerError shortens a connection error for an agent: every URL
// is reduced to scheme://host (no userinfo, path, query or fragment, where
// a key or token would sit), the redactor runs, and the text is capped.
func sanitizeServerError(s string) string {
	s = strings.TrimSpace(urlRE.ReplaceAllString(s, "${1}${2}"))
	s = audit.RedactString(s)
	if rs := []rune(s); len(rs) > 240 {
		s = string(rs[:237]) + "…"
	}
	return s
}

// poolServers answers servers.list from the gateway's own pool when no
// ServersProvider is wired: what is connected, with its live state.
func (g *Gateway) poolServers(ctx context.Context, owner string) []ServerInfo {
	g.mu.RLock()
	counts := map[string]int{}
	for _, e := range g.tools {
		counts[e.upstream]++
	}
	var out []ServerInfo
	for name, u := range g.upstreams {
		info := ServerInfo{Name: name, Transport: u.cfg.Transport, Enabled: true, ToolCount: counts[name], AuthMode: "shared"}
		u.mu.Lock()
		dialErr := u.lastDialErr
		u.mu.Unlock()
		switch {
		case u.inBackoff() && dialErr != nil:
			info.Status, info.Error = "error", sanitizeServerError(dialErr.Error())
		case u.suspended():
			info.Status = "idle"
		default:
			info.Status = "ok"
		}
		out = append(out, info)
	}
	groups := make([]*perUserGroup, 0, len(g.perUser))
	for _, pu := range g.perUser {
		groups = append(groups, pu)
	}
	g.mu.RUnlock()
	for _, pu := range groups {
		info := ServerInfo{Name: pu.cfg.Name, Transport: pu.cfg.Transport, Enabled: true, ToolCount: counts[pu.cfg.Name], AuthMode: "per_user", OAuth: true, Status: "ok"}
		if counts[pu.cfg.Name] == 0 {
			info.Status = StatusWaitingSignIn
		}
		if owner != "" && pu.cfg.PerUserAuth != nil {
			if ok, err := pu.cfg.PerUserAuth.UserConnected(ctx, pu.cfg.Name, owner); err == nil {
				info.SignedIn = &ok
			}
		}
		out = append(out, info)
	}
	return out
}

func (g *Gateway) handleServersList() directHandler {
	return func(ctx context.Context, _ map[string]any) (*mcp.CallToolResult, error) {
		agentID := agentIDFromContext(ctx)
		scope := g.scopeFor(ctx, agentID)
		owner := g.ownerOf(ctx, agentID)
		var rows []ServerInfo
		if g.servers != nil {
			list, err := g.servers.ListServers(ctx, owner)
			if err != nil {
				return mcp.NewToolResultErrorFromErr("servers.list", err), nil
			}
			rows = list
		} else {
			rows = g.poolServers(ctx, owner)
		}
		out := make([]ServerInfo, 0, len(rows))
		for _, r := range rows {
			if !scope.Allows(r.Name) {
				continue
			}
			r.Error = sanitizeServerError(r.Error)
			out = append(out, r)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
		return inboxJSON(map[string]any{"servers": out, "count": len(out)}), nil
	}
}

func (g *Gateway) handleServersReconnect() directHandler {
	return func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		agentID := agentIDFromContext(ctx)
		name := strings.TrimSpace(stringArg(args, "server"))
		reason := callReasonFromContext(ctx)
		if name == "" {
			return mcp.NewToolResultError("server is required"), nil
		}
		refuse := func(msg string) (*mcp.CallToolResult, error) {
			_ = g.audit.Write(ctx, audit.Event{EventType: EventServerReconnect, AgentID: agentID, UpstreamName: name, Decision: "refused", Reason: reason, ResultSummary: msg})
			return mcp.NewToolResultError(msg), nil
		}
		if !enrolledAgent(agentID) {
			return refuse("servers.reconnect needs an enrolled agent: connect with an agent token (Authorization: Bearer …). People reconnect servers on the dashboard's Servers page.")
		}
		if !g.scopeFor(ctx, agentID).All {
			return refuse("servers.reconnect refused: only an admin's agent may reconnect a server, and your owner is a member. Tell your owner the server's status from servers.list instead.")
		}
		if g.servers == nil {
			return mcp.NewToolResultError("servers.reconnect is not available on this gateway (no upstream service wired)"), nil
		}
		info, err := g.servers.ReconnectServer(ctx, name)
		ev := audit.Event{EventType: EventServerReconnect, AgentID: agentID, UpstreamName: name, Reason: reason}
		if err != nil {
			ev.Decision, ev.ResultSummary = "error", sanitizeServerError(err.Error())
			_ = g.audit.Write(ctx, ev)
			res := mcp.NewToolResultError(fmt.Sprintf("servers.reconnect %s: %s", name, sanitizeServerError(err.Error())))
			if info != nil {
				info.Error = sanitizeServerError(info.Error)
				res.StructuredContent = map[string]any{"server": info, "error": sanitizeServerError(err.Error())}
			}
			return res, nil
		}
		ev.Decision = "ok"
		if info != nil {
			info.Error = sanitizeServerError(info.Error)
			ev.ResultSummary = fmt.Sprintf("status %s, %d tool(s)", info.Status, info.ToolCount)
		}
		_ = g.audit.Write(ctx, ev)
		return inboxJSON(map[string]any{"status": "reconnected", "server": info}), nil
	}
}

// ---- audit.mine -------------------------------------------------------------

const (
	auditMineDefaultLimit = 50
	auditMineMaxLimit     = 200
)

type auditMineRow struct {
	TS         string `json:"ts"`
	Tool       string `json:"tool,omitempty"`
	Server     string `json:"server,omitempty"`
	Event      string `json:"event"`
	Decision   string `json:"decision,omitempty"`
	Reason     string `json:"reason,omitempty"`
	Via        string `json:"via,omitempty"`
	ApprovalID string `json:"approval_id,omitempty"`
	DecidedVia string `json:"decided_via,omitempty"`
	DecidedBy  string `json:"decided_by,omitempty"`
}

// parseSince reads `since` (RFC 3339, or unix milliseconds) and
// `since_minutes`; the later of the two wins. 0 means no bound.
func parseSince(args map[string]any) (int64, error) {
	var since int64
	if s := strings.TrimSpace(stringArg(args, "since")); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			since = t.UnixMilli()
		} else if ms, err := parseUnixMillis(s); err == nil {
			since = ms
		} else {
			return 0, fmt.Errorf("since: %q is not an RFC 3339 time", s)
		}
	}
	if m, ok := asInt(args["since_minutes"]); ok && m > 0 {
		if t := time.Now().Add(-time.Duration(m) * time.Minute).UnixMilli(); t > since {
			since = t
		}
	}
	return since, nil
}

func parseUnixMillis(s string) (int64, error) {
	var ms int64
	if _, err := fmt.Sscanf(s, "%d", &ms); err != nil || ms <= 0 {
		return 0, fmt.Errorf("not unix milliseconds")
	}
	return ms, nil
}

func (g *Gateway) handleAuditMine() directHandler {
	return func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		agentID := agentIDFromContext(ctx)
		if agentID == "" {
			return mcp.NewToolResultError("audit.mine lists your own calls, so it needs an enrolled agent: connect with an agent token (Authorization: Bearer …)"), nil
		}
		limit := auditMineDefaultLimit
		if n, ok := asInt(args["limit"]); ok && n > 0 {
			limit = n
		}
		if limit > auditMineMaxLimit {
			limit = auditMineMaxLimit
		}
		since, err := parseSince(args)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		// The filter is the caller's own id: no argument can widen it.
		evs, err := g.audit.Query(ctx, audit.Filter{AgentID: agentID, Since: since, Limit: limit})
		if err != nil {
			return mcp.NewToolResultErrorFromErr("audit.mine", err), nil
		}
		rows := make([]auditMineRow, 0, len(evs))
		for _, e := range evs {
			if e.AgentID != agentID {
				continue // belt and braces: never another agent's row
			}
			r := auditMineRow{
				TS: time.UnixMilli(e.TS).UTC().Format(time.RFC3339Nano), Tool: e.ToolName, Server: e.UpstreamName,
				Event: e.EventType, Decision: e.Decision, Reason: e.Reason, Via: e.Via, ApprovalID: e.ApprovalID,
				DecidedVia: e.DecidedVia, DecidedBy: e.DecidedByEmail,
			}
			if r.DecidedBy == "" {
				r.DecidedBy = e.DecidedByName
			}
			rows = append(rows, r)
		}
		out := map[string]any{"agent_id": agentID, "count": len(rows), "calls": rows}
		if since > 0 {
			out["since"] = time.UnixMilli(since).UTC().Format(time.RFC3339)
		}
		return inboxJSON(out), nil
	}
}

// ---- access.whoami ----------------------------------------------------------

func (g *Gateway) handleAccessWhoami() directHandler {
	return func(ctx context.Context, _ map[string]any) (*mcp.CallToolResult, error) {
		agentID := agentIDFromContext(ctx)
		raiser := raiserOnCtx(ctx, agentID)
		scope := g.scopeFor(ctx, agentID)
		role := "member"
		switch {
		case scope.Denied:
			role = "blocked"
		case scope.All:
			role = "admin"
		}
		var granted any = "all"
		if !scope.All {
			names := make([]string, 0, len(scope.Groups))
			for gname := range scope.Groups {
				names = append(names, gname)
			}
			sort.Strings(names)
			granted = names
		}
		owner := map[string]any{"user_id": raiser.OwnerUserID, "email": raiser.OwnerEmail, "name": raiser.OwnerName, "role": role}
		if owner["user_id"] == "" {
			owner["user_id"] = g.ownerOf(ctx, agentID)
		}
		out := map[string]any{
			"caller_id":             agentID,
			"agent_name":            raiser.AgentName,
			"agent_kind":            raiser.AgentKind,
			"owner":                 owner,
			"granted_servers":       granted,
			"always_on":             access.AlwaysOnGroups(),
			"can_change_policies":   scope.All,
			"can_reconnect_servers": scope.All,
			"client":                map[string]any{"kind": raiser.ClientKind, "name": raiser.ClientName, "via": raiser.Via},
		}
		if raiser.AgentSessionID != "" {
			out["session_id"] = raiser.AgentSessionID
		}
		switch {
		case agentID == "":
			out["note"] = "unauthenticated local caller: no agent token on this connection"
		case g.access == nil:
			out["note"] = "this gateway has no per-user access control configured; every caller may use every tool"
		}
		return inboxJSON(out), nil
	}
}
