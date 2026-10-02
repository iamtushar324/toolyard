// Package access decides which tool groups a dashboard user, and every agent
// that user owns, may use. A tool group is an upstream server name or a
// built-in data group (memory, lake, events, notes, skills).
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
// the inbox/session tools an agent uses to ask for access, and the
// self-scoped access tools (policies, servers, audit, access) that tell an
// agent what it may do and why; the ones that change anything check the
// owner's role themselves.
var alwaysOn = map[string]bool{
	"tools": true, "inbox": true, "session": true,
	"policies": true, "servers": true, "audit": true, "access": true,
}

// BuiltinGroups are the built-in data tools an admin grants like servers.
// They are shared across all agents, so members don't get them by default.
var BuiltinGroups = []string{"memory", "lake", "events", "notes", "skills"}

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
	All bool
	// Groups granted to a member.
	Groups map[string]bool
}

// Allows reports whether the scope reaches group.
func (s Scope) Allows(group string) bool {
	if s.Denied {
		return false
	}
	return s.All || alwaysOn[group] || s.Groups[group]
}

// AllowsTool reports whether the scope reaches a tool registered under
// upstream with the given full name.
func (s Scope) AllowsTool(upstream, toolName string) bool {
	return s.Allows(GroupOf(upstream, toolName))
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
	users  map[string]cached[Scope]  // user id -> scope
	owners map[string]cached[string] // agent id -> owner user id ("" = unknown)
	now    func() time.Time
}

func New(db *store.DB) *Service {
	return &Service{
		db:     db,
		users:  map[string]cached[Scope]{},
		owners: map[string]cached[string]{},
		now:    time.Now,
	}
}

// ScopeFor resolves a gateway caller id:
//
//	""                   local unauthenticated MCP caller (only possible
//	                     without -require-auth-on-mcp): All
//	"ag_<uuid>"          an agent: its owner's scope
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
		return s.UserScope(ctx, owner)
	}
	return Scope{Denied: true}
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

	var owner string
	err := s.db.QueryRowContext(ctx, `SELECT owner_user FROM agents WHERE id = ?`, agentID).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		owner, err = "", nil
	}
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	s.owners[agentID] = cached[string]{v: owner, exp: s.now().Add(cacheTTL)}
	s.mu.Unlock()
	return owner, nil
}

// Groups lists the groups granted to userID, sorted.
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
		out = append(out, g)
	}
	return out, rows.Err()
}

// SetGroups replaces userID's grants with groups. Always-on groups are
// dropped (they need no grant); duplicates collapse. by is recorded as
// created_by on new rows.
func (s *Service) SetGroups(ctx context.Context, userID string, groups []string, by string) error {
	want := map[string]bool{}
	for _, g := range groups {
		g = strings.TrimSpace(g)
		if !groupRE.MatchString(g) {
			return fmt.Errorf("%w: %q", ErrInvalidGroup, g)
		}
		if !alwaysOn[g] {
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

// Invalidate drops the cached scope for userID, or every cached scope and
// agent owner when userID is "". Call it after a role, status, grant or
// agent-ownership change.
func (s *Service) Invalidate(userID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if userID == "" {
		s.users = map[string]cached[Scope]{}
		s.owners = map[string]cached[string]{}
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
