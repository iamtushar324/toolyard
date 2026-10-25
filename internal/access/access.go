// Package access decides which tool groups a dashboard user, and every agent
// that user owns, may use. A tool group is an upstream server name or a
// built-in data group (memory, events, notes, skills).
//
// Admins reach everything. Members reach only the groups an admin granted
// them in user_server_access, plus the always-on groups (the meta-tools and
// the inbox/session tools an agent uses to ask for access). Blocked users and
// unknown callers reach nothing.
package access

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// alwaysOn groups stay available to every active caller: the meta-tools,
// the inbox/session tools an agent uses to ask for access, the
// self-scoped access tools (policies, servers, audit, access) that tell an
// agent what it may do and why (the ones that change anything check the
// owner's role themselves), the connections tools it finds its owner's
// sign-in links with, and the jobs tools for its own async jobs.
var alwaysOn = map[string]bool{
	"tools": true, "inbox": true, "session": true,
	"policies": true, "servers": true, "audit": true, "access": true,
	"connections": true,
	// jobs.*: an agent reads and cancels only its own async jobs.
	"jobs": true,
}

// BuiltinGroups are the built-in data tools an admin grants like servers.
// They are shared across all agents, so members don't get them by default.
var BuiltinGroups = []string{"memory", "events", "notes", "skills"}

// retiredGroups are groups toolyard no longer has. A stored grant for one
// grants nothing and is not listed; the next SetGroups for that user drops
// the row. lake was the built-in ClickHouse data group.
var retiredGroups = map[string]bool{"lake": true}

var groupRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ErrInvalidGroup is returned by SetGroups for a malformed group name.
var ErrInvalidGroup = errors.New("invalid server name")

// AlwaysOn reports whether group is available to every active caller.
func AlwaysOn(group string) bool { return alwaysOn[group] }

// AlwaysOnGroups lists the always-on groups, sorted.
func AlwaysOnGroups() []string {
	out := make([]string, 0, len(alwaysOn))
	for g := range alwaysOn {
		out = append(out, g)
	}
	sort.Strings(out)
	return out
}

// IsBuiltinGroup reports whether group is one of BuiltinGroups.
func IsBuiltinGroup(group string) bool {
	for _, g := range BuiltinGroups {
		if g == group {
			return true
		}
	}
	return false
}

// GroupOf maps a registered tool to its access group. Built-in tools share
// the "builtin" upstream label, so their group is the tool-name prefix
// (memory.get -> memory); every other tool's group is its upstream name.
func GroupOf(upstream, toolName string) string {
	if upstream == "builtin" {
		if i := strings.IndexByte(toolName, '.'); i > 0 {
			return toolName[:i]
		}
		return toolName
	}
	return upstream
}

// Scope is what one caller may use.
type Scope struct {
	// Denied: blocked or unknown caller. Nothing is allowed.
	Denied bool
	// All: admin, or a local unauthenticated caller. Everything is allowed.
	// An agent with Limits never has All, so code that short-cuts on All
	// still filters it; OwnerAll keeps its owner's reach.
	All bool
	// Groups granted to a member.
	Groups map[string]bool
	// OwnerAll: the agent's owner is an admin (everything), and Limits
	// narrow that for this one agent.
	OwnerAll bool
	// Limits are the agent's own allow and deny lists (agent_scopes), on
	// top of its owner's scope. nil: no limits.
	Limits *Limits
}

// ownerAllows reports whether the scope reaches group before any agent
// limits.
func (s Scope) ownerAllows(group string) bool {
	if s.Denied {
		return false
	}
	return s.All || s.OwnerAll || alwaysOn[group] || s.Groups[group]
}

// Allows reports whether the scope reaches group: the owner may use it and
// the agent's limits allow at least part of it.
func (s Scope) Allows(group string) bool {
	return s.ownerAllows(group) && s.Limits.allowsGroup(group)
}

// AllowsTool reports whether the scope reaches a tool registered under
// upstream with the given full name.
func (s Scope) AllowsTool(upstream, toolName string) bool {
	group := GroupOf(upstream, toolName)
	return s.ownerAllows(group) && s.Limits.allowsTool(group, toolName)
}

// LimitedOut reports whether the owner may use the tool but the agent's
// own limits refuse it. The gateway then says so plainly instead of
// answering "not found": the agent's owner chose the limit and can see
// the tool anyway.
func (s Scope) LimitedOut(upstream, toolName string) bool {
	group := GroupOf(upstream, toolName)
	return s.ownerAllows(group) && !s.Limits.allowsTool(group, toolName)
}

// OwnerIsAdmin reports whether the caller's owner reaches everything
// (an admin), whatever the agent's own limits.
func (s Scope) OwnerIsAdmin() bool { return !s.Denied && (s.All || s.OwnerAll) }

// CanAdminister reports whether the caller may use toolyard's admin
// controls (policies.set/clear, servers.reconnect, shared connections): an
// admin's unlimited agent or the admin's own session, or an admin's agent
// whose limits say it may manage toolyard. A limited agent, and every
// OAuth client its owner did not explicitly let manage toolyard, may not.
func (s Scope) CanAdminister() bool {
	if s.Denied {
		return false
	}
	return s.All || (s.OwnerAll && s.Limits != nil && s.Limits.Manage)
}

// Resolver is what the gateway asks on every tool list and call.
type Resolver interface {
	ScopeFor(ctx context.Context, callerID string) Scope
}

// cacheTTL bounds how stale a scope can be when a write skips Invalidate.
const cacheTTL = 5 * time.Second

type cached[T any] struct {
	v   T
	exp time.Time
}

// Service reads roles and grants from the store, with a short cache because
// the gateway asks on every call.
type Service struct {
	db     *store.DB
	mu     sync.Mutex
	users  map[string]cached[Scope]   // user id -> scope
	owners map[string]cached[string]  // agent id -> owner user id ("" = unknown)
	limits map[string]cached[*Limits] // agent id -> its limits (nil = none)
	now    func() time.Time
}

func New(db *store.DB) *Service {
	return &Service{
		db:     db,
		users:  map[string]cached[Scope]{},
		owners: map[string]cached[string]{},
		limits: map[string]cached[*Limits]{},
		now:    time.Now,
	}
}

// ScopeFor resolves a gateway caller id:
//
//	""                   local unauthenticated MCP caller (only possible
//	                     without -require-auth-on-mcp): All
//	"ag_<uuid>"          an agent: its owner's scope, narrowed by the
//	                     agent's own limits (agent_scopes; every OAuth
//	                     client has limits); a disabled agent is Denied
//	"dashboard:<uid>"    dashboard Call tab: that user's scope
//	"voice:<uid>"        voice session: that user's scope
//
// Anything else is Denied.
func (s *Service) ScopeFor(ctx context.Context, callerID string) Scope {
	switch {
	case callerID == "":
		return Scope{All: true}
	case strings.HasPrefix(callerID, "dashboard:"):
		return s.UserScope(ctx, strings.TrimPrefix(callerID, "dashboard:"))
	case strings.HasPrefix(callerID, "voice:"):
		return s.UserScope(ctx, strings.TrimPrefix(callerID, "voice:"))
	case strings.HasPrefix(callerID, "ag_"):
		owner, err := s.agentOwner(ctx, callerID)
		if err != nil || owner == "" {
			return Scope{Denied: true}
		}
		sc := s.UserScope(ctx, owner)
		lim, err := s.agentLimits(ctx, callerID)
		if err != nil {
			return Scope{Denied: true} // fail closed, like every lookup here
		}
		return sc.withLimits(lim)
	}
	return Scope{Denied: true}
}

// withLimits narrows s by an agent's limits. All moves to OwnerAll so no
// caller mistakes a limited agent for one that may use everything.
func (s Scope) withLimits(lim *Limits) Scope {
	if lim == nil || s.Denied {
		return s
	}
	s.OwnerAll, s.All, s.Limits = s.All, false, lim
	return s
}

// OwnerUser implements gateway.OwnerResolver: the dashboard user behind a
// caller id, "" when there is none (an unknown agent, a local anonymous
// caller). Agents resolve through the same short owner cache as ScopeFor.
func (s *Service) OwnerUser(ctx context.Context, callerID string) (string, error) {
	switch {
	case strings.HasPrefix(callerID, "dashboard:"):
		return strings.TrimPrefix(callerID, "dashboard:"), nil
	case strings.HasPrefix(callerID, "voice:"):
		return strings.TrimPrefix(callerID, "voice:"), nil
	case strings.HasPrefix(callerID, "ag_"):
		return s.agentOwner(ctx, callerID)
	}
	return "", nil
}

// UserScope returns a dashboard user's scope. Lookup errors fail closed.
func (s *Service) UserScope(ctx context.Context, userID string) Scope {
	if userID == "" {
		return Scope{Denied: true}
	}
	s.mu.Lock()
	if c, ok := s.users[userID]; ok && s.now().Before(c.exp) {
		s.mu.Unlock()
		return c.v
	}
	s.mu.Unlock()

	sc, err := s.loadUserScope(ctx, userID)
	if err != nil {
		return Scope{Denied: true}
	}
	s.mu.Lock()
	s.users[userID] = cached[Scope]{v: sc, exp: s.now().Add(cacheTTL)}
	s.mu.Unlock()
	return sc
}

func (s *Service) loadUserScope(ctx context.Context, userID string) (Scope, error) {
	var role, status string
	err := s.db.QueryRowContext(ctx, `SELECT role, status FROM users WHERE id = ?`, userID).Scan(&role, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return Scope{Denied: true}, nil
	}
	if err != nil {
		return Scope{}, err
	}
	if status != "active" {
		return Scope{Denied: true}, nil
	}
	if role == "admin" {
		return Scope{All: true}, nil
	}
	groups, err := s.Groups(ctx, userID)
	if err != nil {
		return Scope{}, err
	}
	sc := Scope{Groups: make(map[string]bool, len(groups))}
	for _, g := range groups {
		sc.Groups[g] = true
	}
	return sc, nil
}

func (s *Service) agentOwner(ctx context.Context, agentID string) (string, error) {
	s.mu.Lock()
	if c, ok := s.owners[agentID]; ok && s.now().Before(c.exp) {
		s.mu.Unlock()
		return c.v, nil
	}
	s.mu.Unlock()

	// A disabled agent has no owner here, so it reaches nothing: a call it
	// queued (an approval, an Inbox grant) cannot run after a revoke.
	var owner string
	var disabled int
	err := s.db.QueryRowContext(ctx, `SELECT owner_user, COALESCE(disabled, 0) FROM agents WHERE id = ?`, agentID).Scan(&owner, &disabled)
	if errors.Is(err, sql.ErrNoRows) {
		owner, err = "", nil
	}
	if err != nil {
		return "", err
	}
	if disabled != 0 {
		owner = ""
	}
	s.mu.Lock()
	s.owners[agentID] = cached[string]{v: owner, exp: s.now().Add(cacheTTL)}
	s.mu.Unlock()
	return owner, nil
}

// Groups lists the groups granted to userID, sorted. Grants for retired
// groups are left out.
func (s *Service) Groups(ctx context.Context, userID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT server FROM user_server_access WHERE user_id = ? ORDER BY server`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var g string
		if err := rows.Scan(&g); err != nil {
			return nil, err
		}
		if retiredGroups[g] {
			continue
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// SetGroups replaces userID's grants with groups. Always-on and retired
// groups are dropped (they need no grant, or grant nothing); duplicates
// collapse. by is recorded as created_by on new rows. The user's row is
// locked first (on Postgres), so two concurrent replacements for one user
// run one after the other: the last one wins whole, never a union of both
// or a primary-key error.
func (s *Service) SetGroups(ctx context.Context, userID string, groups []string, by string) error {
	want := map[string]bool{}
	for _, g := range groups {
		g = strings.TrimSpace(g)
		if !groupRE.MatchString(g) {
			return fmt.Errorf("%w: %q", ErrInvalidGroup, g)
		}
		if !alwaysOn[g] && !retiredGroups[g] {
			want[g] = true
		}
	}
	names := make([]string, 0, len(want))
	for g := range want {
		names = append(names, g)
	}
	sort.Strings(names)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// A missing user is left to the inserts below, as before: the foreign
	// key refuses grants for them.
	var locked string
	if err := tx.QueryRowContext(ctx,
		`SELECT id FROM users WHERE id = ?`+s.db.Dialect.ForUpdate(), userID).Scan(&locked); err != nil &&
		!errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM user_server_access WHERE user_id = ?`, userID); err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	for _, g := range names {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO user_server_access(user_id, server, created_at, created_by) VALUES(?,?,?,?)`,
			userID, g, now, nullStr(by)); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.Invalidate(userID)
	return nil
}

// Invalidate drops the cached scope for userID, or every cached scope,
// agent owner and agent limit when userID is "". Call it after a role,
// status, grant or agent-ownership change.
func (s *Service) Invalidate(userID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if userID == "" {
		s.users = map[string]cached[Scope]{}
		s.owners = map[string]cached[string]{}
		s.limits = map[string]cached[*Limits]{}
		return
	}
	delete(s.users, userID)
}

func nullStr(v string) any {
	if v == "" {
		return nil
	}
	return v
}
