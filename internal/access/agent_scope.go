package access

// agent_scope.go is the per-agent half of access: an agent may be narrowed
// below its owner's scope with its own allow and deny lists (agent_scopes).
// Every agent can have them: an OAuth client (mcpauth) gets them from its
// consent screen, an ordinary agent from the Connected apps page. An agent
// with no row keeps exactly its owner's scope, as before.
//
// The lists name access groups (servers: the GroupOf an upstream tool) and
// tools (full tool names, the names tools/list shows). A deny always wins.
// The always-on groups (meta-tools, inbox, session, ...) stay reachable
// unless denied by name, so a limited agent can still search, ask for
// approval and tag its session.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// Agent scope modes.
const (
	// ScopeModeAll: everything the owner may use, minus the denies.
	ScopeModeAll = "all"
	// ScopeModeListed: only the allowed servers and tools (plus the
	// always-on groups), minus the denies.
	ScopeModeListed = "listed"
)

// maxScopeEntries bounds each list of one agent's scope.
const maxScopeEntries = 1000

// ErrInvalidScope is returned for a malformed agent scope.
var ErrInvalidScope = errors.New("invalid agent scope")

// ToolRef names one tool: its access group and its full name.
type ToolRef struct {
	Server string `json:"server"`
	Tool   string `json:"tool"`
}

// AgentScope is one agent's limits as stored and as the REST API shows
// them. The zero value (mode "" or "all", no denies) is "no limits".
type AgentScope struct {
	Mode         string    `json:"mode"`
	AllowServers []string  `json:"allow_servers"`
	AllowTools   []ToolRef `json:"allow_tools"`
	DenyServers  []string  `json:"deny_servers"`
	DenyTools    []ToolRef `json:"deny_tools"`
	UpdatedAt    int64     `json:"updated_at,omitempty"`
	UpdatedBy    string    `json:"updated_by,omitempty"`
}

// Unlimited reports whether sc changes nothing: everything the owner may
// use, nothing denied.
func (sc AgentScope) Unlimited() bool {
	return sc.Mode != ScopeModeListed && len(sc.DenyServers) == 0 && len(sc.DenyTools) == 0
}

// Summary is a short, plain description of sc for lists and audit rows.
func (sc AgentScope) Summary() string {
	var parts []string
	if sc.Mode == ScopeModeListed {
		parts = append(parts, "only "+countOf(len(sc.AllowServers), "server")+" and "+countOf(len(sc.AllowTools), "tool"))
	} else {
		parts = append(parts, "everything its owner can use")
	}
	if n := len(sc.DenyServers) + len(sc.DenyTools); n > 0 {
		parts = append(parts, "except "+countOf(len(sc.DenyServers), "denied server")+" and "+countOf(len(sc.DenyTools), "denied tool"))
	}
	return strings.Join(parts, ", ")
}

func countOf(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// Limits is the compiled form of an AgentScope that Scope checks against.
// A nil *Limits allows everything (no agent limits). Every OAuth client
// has Limits, empty when its scope is "everything", so it never inherits
// its owner's admin control unless Manage says so (CanAdminister).
type Limits struct {
	// OAuthClient: the agent is an OAuth client (it has a grant).
	OAuthClient bool
	// Manage: the owner let this OAuth client manage toolyard.
	Manage       bool
	Listed       bool
	AllowServers map[string]bool
	AllowTools   map[string]map[string]bool // group -> tool names
	DenyServers  map[string]bool
	DenyTools    map[string]bool // full tool names
}

// allowsGroup reports whether the limits leave any part of group usable.
func (l *Limits) allowsGroup(group string) bool {
	if l == nil {
		return true
	}
	if l.DenyServers[group] {
		return false
	}
	if alwaysOn[group] || !l.Listed {
		return true
	}
	return l.AllowServers[group] || len(l.AllowTools[group]) > 0
}

// allowsTool reports whether the limits allow one tool of group.
func (l *Limits) allowsTool(group, toolName string) bool {
	if l == nil {
		return true
	}
	if l.DenyServers[group] || l.DenyTools[toolName] {
		return false
	}
	if alwaysOn[group] || !l.Listed {
		return true
	}
	return l.AllowServers[group] || l.AllowTools[group][toolName]
}

// Describe lists the limits for an agent to read (access.whoami): the
// mode and the sorted allow and deny lists.
func (l *Limits) Describe() map[string]any {
	if l == nil {
		return nil
	}
	keys := func(m map[string]bool) []string {
		out := make([]string, 0, len(m))
		for k := range m {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	allowTools := []string{}
	for _, tools := range l.AllowTools {
		allowTools = append(allowTools, keys(tools)...)
	}
	sort.Strings(allowTools)
	mode := ScopeModeAll
	if l.Listed {
		mode = ScopeModeListed
	}
	return map[string]any{
		"mode":          mode,
		"allow_servers": keys(l.AllowServers),
		"allow_tools":   allowTools,
		"deny_servers":  keys(l.DenyServers),
		"deny_tools":    keys(l.DenyTools),
		"oauth_client":  l.OAuthClient,
		"may_manage":    l.Manage,
	}
}

// Compile turns sc into the Limits Scope checks, or nil when sc limits
// nothing.
func (sc AgentScope) Compile() *Limits {
	if sc.Unlimited() {
		return nil
	}
	l := &Limits{
		Listed:       sc.Mode == ScopeModeListed,
		AllowServers: map[string]bool{},
		AllowTools:   map[string]map[string]bool{},
		DenyServers:  map[string]bool{},
		DenyTools:    map[string]bool{},
	}
	for _, g := range sc.AllowServers {
		l.AllowServers[g] = true
	}
	for _, t := range sc.AllowTools {
		if l.AllowTools[t.Server] == nil {
			l.AllowTools[t.Server] = map[string]bool{}
		}
		l.AllowTools[t.Server][t.Tool] = true
	}
	for _, g := range sc.DenyServers {
		l.DenyServers[g] = true
	}
	for _, t := range sc.DenyTools {
		l.DenyTools[t.Tool] = true
	}
	return l
}

// Normalize checks sc and returns it cleaned: a known mode, trimmed and
// de-duplicated sorted lists, every server a valid group name and every
// tool a plain name of at most 200 characters.
func (sc AgentScope) Normalize() (AgentScope, error) {
	out := AgentScope{Mode: strings.TrimSpace(sc.Mode)}
	switch out.Mode {
	case "":
		out.Mode = ScopeModeAll
	case ScopeModeAll, ScopeModeListed:
	default:
		return AgentScope{}, fmt.Errorf("%w: mode must be all or listed", ErrInvalidScope)
	}
	var err error
	if out.AllowServers, err = cleanServers(sc.AllowServers); err != nil {
		return AgentScope{}, err
	}
	if out.DenyServers, err = cleanServers(sc.DenyServers); err != nil {
		return AgentScope{}, err
	}
	if out.AllowTools, err = cleanTools(sc.AllowTools); err != nil {
		return AgentScope{}, err
	}
	if out.DenyTools, err = cleanTools(sc.DenyTools); err != nil {
		return AgentScope{}, err
	}
	return out, nil
}

func cleanServers(in []string) ([]string, error) {
	if len(in) > maxScopeEntries {
		return nil, fmt.Errorf("%w: at most %d servers", ErrInvalidScope, maxScopeEntries)
	}
	seen := map[string]bool{}
	out := []string{}
	for _, g := range in {
		g = strings.TrimSpace(g)
		if !groupRE.MatchString(g) {
			return nil, fmt.Errorf("%w: server %q", ErrInvalidScope, g)
		}
		if !seen[g] {
			seen[g] = true
			out = append(out, g)
		}
	}
	sort.Strings(out)
	return out, nil
}

func cleanTools(in []ToolRef) ([]ToolRef, error) {
	if len(in) > maxScopeEntries {
		return nil, fmt.Errorf("%w: at most %d tools", ErrInvalidScope, maxScopeEntries)
	}
	seen := map[ToolRef]bool{}
	out := []ToolRef{}
	for _, t := range in {
		t.Server, t.Tool = strings.TrimSpace(t.Server), strings.TrimSpace(t.Tool)
		if t.Server == "" {
			// A tool's group is its prefix for built-ins and its upstream
			// otherwise; the caller knows which, and we refuse to guess.
			return nil, fmt.Errorf("%w: tool %q needs its server", ErrInvalidScope, t.Tool)
		}
		if !groupRE.MatchString(t.Server) || !plainToolName(t.Tool) {
			return nil, fmt.Errorf("%w: tool %q on %q", ErrInvalidScope, t.Tool, t.Server)
		}
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Server != out[j].Server {
			return out[i].Server < out[j].Server
		}
		return out[i].Tool < out[j].Tool
	})
	return out, nil
}

// plainToolName: 1-200 printable characters, no spaces.
func plainToolName(name string) bool {
	if name == "" || len(name) > 200 {
		return false
	}
	for _, r := range name {
		if !unicode.IsPrint(r) || unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

// storedRules is the JSON in agent_scopes.rules.
type storedRules struct {
	AllowServers []string  `json:"allow_servers,omitempty"`
	AllowTools   []ToolRef `json:"allow_tools,omitempty"`
	DenyServers  []string  `json:"deny_servers,omitempty"`
	DenyTools    []ToolRef `json:"deny_tools,omitempty"`
}

// AgentScope returns agentID's limits; an agent without a row gets the
// unlimited scope (mode "all", empty lists).
func (s *Service) AgentScope(ctx context.Context, agentID string) (AgentScope, error) {
	return ReadAgentScope(ctx, s.db, agentID)
}

// ReadAgentScope is AgentScope on any querier (a transaction included).
func ReadAgentScope(ctx context.Context, q store.Querier, agentID string) (AgentScope, error) {
	sc := AgentScope{Mode: ScopeModeAll, AllowServers: []string{}, AllowTools: []ToolRef{}, DenyServers: []string{}, DenyTools: []ToolRef{}}
	var mode, rules string
	var updatedAt int64
	var updatedBy sql.NullString
	err := q.QueryRowContext(ctx, `SELECT mode, rules, updated_at, updated_by FROM agent_scopes WHERE agent_id = ?`, agentID).
		Scan(&mode, &rules, &updatedAt, &updatedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return sc, nil
	}
	if err != nil {
		return sc, err
	}
	var r storedRules
	if err := json.Unmarshal([]byte(rules), &r); err != nil {
		return sc, fmt.Errorf("agent_scopes %s: %w", agentID, err)
	}
	sc.Mode, sc.UpdatedAt, sc.UpdatedBy = mode, updatedAt, updatedBy.String
	if r.AllowServers != nil {
		sc.AllowServers = r.AllowServers
	}
	if r.AllowTools != nil {
		sc.AllowTools = r.AllowTools
	}
	if r.DenyServers != nil {
		sc.DenyServers = r.DenyServers
	}
	if r.DenyTools != nil {
		sc.DenyTools = r.DenyTools
	}
	return sc, nil
}

// SetAgentScope replaces agentID's limits with sc (normalized first) and
// drops the cached copy. An unlimited sc deletes the row, so the agent is
// back to exactly its owner's scope. by is recorded as updated_by.
func (s *Service) SetAgentScope(ctx context.Context, agentID string, sc AgentScope, by string) (AgentScope, error) {
	out, err := WriteAgentScope(ctx, s.db, agentID, sc, by)
	if err != nil {
		return AgentScope{}, err
	}
	s.InvalidateAgent(agentID)
	return out, nil
}

// WriteAgentScope is SetAgentScope's write on any querier, for callers
// that change an agent's limits inside their own transaction (mcpauth's
// consent). It does not touch the cache: call InvalidateAgent after the
// transaction commits.
func WriteAgentScope(ctx context.Context, q store.Querier, agentID string, sc AgentScope, by string) (AgentScope, error) {
	if !strings.HasPrefix(agentID, "ag_") {
		return AgentScope{}, fmt.Errorf("%w: unknown agent", ErrInvalidScope)
	}
	out, err := sc.Normalize()
	if err != nil {
		return AgentScope{}, err
	}
	if out.Unlimited() {
		if _, err := q.ExecContext(ctx, `DELETE FROM agent_scopes WHERE agent_id = ?`, agentID); err != nil {
			return AgentScope{}, err
		}
		return out, nil
	}
	rules, err := json.Marshal(storedRules{AllowServers: out.AllowServers, AllowTools: out.AllowTools, DenyServers: out.DenyServers, DenyTools: out.DenyTools})
	if err != nil {
		return AgentScope{}, err
	}
	now := time.Now().UnixMilli()
	if _, err := q.ExecContext(ctx,
		`INSERT INTO agent_scopes(agent_id, mode, rules, updated_at, updated_by) VALUES(?,?,?,?,?)
         ON CONFLICT(agent_id) DO UPDATE SET mode = excluded.mode, rules = excluded.rules,
             updated_at = excluded.updated_at, updated_by = excluded.updated_by`,
		agentID, out.Mode, string(rules), now, nullStr(by)); err != nil {
		return AgentScope{}, err
	}
	out.UpdatedAt, out.UpdatedBy = now, by
	return out, nil
}

// InvalidateAgent drops agentID's cached limits and owner, on every
// replica, so the next check reads them again. Call it after the agent's
// limits change, or the agent is disabled, deleted or revoked.
func (s *Service) InvalidateAgent(agentID string) {
	s.dropAgent(agentID)
	s.publish(msgAgent + agentID)
}

// dropAgent is InvalidateAgent on this replica only.
func (s *Service) dropAgent(agentID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.limits, agentID)
	delete(s.owners, agentID)
}

// agentLimits returns agentID's compiled limits (nil: none), with the same
// short cache as the owner lookup.
func (s *Service) agentLimits(ctx context.Context, agentID string) (*Limits, error) {
	s.mu.Lock()
	if c, ok := s.limits[agentID]; ok && s.now().Before(c.exp) {
		s.mu.Unlock()
		return c.v, nil
	}
	s.mu.Unlock()

	sc, err := ReadAgentScope(ctx, s.db, agentID)
	if err != nil {
		return nil, err
	}
	lim := sc.Compile()
	// An OAuth client (mcpauth's grant row) always has limits, so it never
	// inherits its owner's admin control by default.
	var manage int
	err = s.db.QueryRowContext(ctx, `SELECT may_manage FROM mcp_oauth_grants WHERE agent_id = ?`, agentID).Scan(&manage)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return nil, err
	default:
		if lim == nil {
			lim = &Limits{}
		}
		lim.OAuthClient, lim.Manage = true, manage != 0
	}
	s.mu.Lock()
	s.limits[agentID] = cached[*Limits]{v: lim, exp: s.now().Add(cacheTTL)}
	s.mu.Unlock()
	return lim, nil
}
