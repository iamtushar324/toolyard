// Package policy decides whether a tool call is allowed, must be approved, or
// denied. The evaluation order is:
//
//	forcedAction (builtins, handled by the gateway) →
//	explicit tool-scope policy → meta-tool shortcut (tools.*) →
//	explicit upstream-scope policy → escalating intent category →
//	read-name heuristic → default (writes need approval)
//
// If the stored policies could not be loaded, the engine fails closed:
// every call is denied (RuleUnavailable) until a load succeeds, because the
// heuristic alone would allow read-shaped tools that an explicit rule denies.
//
// The intent category is the caller's own `_intent_category` argument, so it
// may only make a decision stricter, never looser: write, destructive,
// external_communication, financial and privileged_admin force approval,
// while read (or any other value) is ignored and the call is judged by its
// name exactly as if no intent was given.
//
// Explicit policies live in the tool_policies table and are set from the UI.
package policy

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/tusharbhardwaj/toolyard/internal/cluster"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

type Action string

const (
	ActionAllow   Action = "allow"
	ActionApprove Action = "approve"
	ActionDeny    Action = "deny"
)

type Decision struct {
	Action Action
	Reason string // human-readable rationale for the decision
	RuleID string
	// RequireHuman is set when an explicit `ask` policy decided this call.
	// The bus uses it to skip the auto-approver so a learned auto rule can't
	// override the operator's explicit "always ask me".
	RequireHuman bool
}

type Request struct {
	AgentID      string
	UpstreamName string
	ToolName     string // WRAPPED catalog name (e.g. "github.create_issue")
	// IntentCategory is the caller-declared `_intent_category`. It can
	// force approval but never allows a call on its own (see Eval).
	IntentCategory string
	Arguments      map[string]any
	UserReason     string
}

// ToolPolicy is one stored explicit gate. Scope is "tool" (target is the
// wrapped tool name) or "upstream" (target is the upstream name). Action is
// "allow" | "ask" | "deny".
type ToolPolicy struct {
	ID        string `json:"id"`
	Scope     string `json:"scope"`
	Target    string `json:"target"`
	Action    string `json:"action"`
	Note      string `json:"note,omitempty"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

const (
	ScopeTool     = "tool"
	ScopeUpstream = "upstream"
)

// RuleUnavailable is the RuleID of the deny Eval returns for every call
// while the stored policies have not loaded (see Ready).
const RuleUnavailable = "policy-unavailable"

// Engine is the policy evaluator. It is nil-tolerant: built with a nil db
// (probe/tests) it falls back to the heuristic-only behaviour with no stored
// policies, which is exactly the pre-feature behaviour.
type Engine struct {
	db     *store.DB
	mu     sync.RWMutex
	byTool map[string]ToolPolicy
	byUp   map[string]ToolPolicy
	// loadErr is the error of a failed first load. While it is set the
	// engine knows none of the stored rules, so Eval denies every call
	// instead of falling back to the name heuristic. The next successful
	// reload clears it. Guarded by mu.
	loadErr error
	// writeMu serialises Set, Delete, DeleteTarget and Change, from the
	// dashboard and from agents alike, so a guarded Change sees no other
	// write land between its check and its own write. With a multi-node
	// cluster a cluster lock extends it to every replica (lockWrite).
	writeMu sync.Mutex
	// cluster, when it spans several nodes, carries every write to the
	// other replicas' caches (SetCluster, cluster.go).
	cluster *cluster.Cluster
}

// New constructs the engine and primes its policy cache from the db. Pass a
// nil db for the heuristic-only engine used by probes and tests. If the
// first load fails, the engine fails closed: Eval denies every call and
// Ready returns the error until a later reload (for example through Set)
// succeeds. A caller that can refuse to start should use Load instead.
func New(db *store.DB) *Engine {
	e := newEngine(db)
	if db != nil {
		if err := e.reload(context.Background()); err != nil {
			e.loadErr = fmt.Errorf("load tool policies: %w", err)
		}
	}
	return e
}

// Load constructs the engine and loads the stored policies, returning the
// error instead of an engine when they cannot be read, so the caller can
// refuse to start rather than run without its explicit rules. It needs a
// db: use New(nil) for the heuristic-only engine.
func Load(ctx context.Context, db *store.DB) (*Engine, error) {
	if db == nil {
		return nil, ErrNoDB
	}
	e := newEngine(db)
	if err := e.reload(ctx); err != nil {
		return nil, fmt.Errorf("load tool policies: %w", err)
	}
	return e, nil
}

func newEngine(db *store.DB) *Engine {
	return &Engine{db: db, byTool: map[string]ToolPolicy{}, byUp: map[string]ToolPolicy{}}
}

// Ready returns the error of a failed first load, or nil once the stored
// policies are loaded (always nil for a heuristic-only engine). While it is
// non-nil, Eval denies every call. A reload that fails after a successful
// one does not set it: the engine keeps its last good snapshot.
func (e *Engine) Ready() error {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.loadErr
}

// reload reads every stored policy and swaps the cache only once all rows
// have been read. On any error it returns before the swap, so the engine
// keeps its last good snapshot (or, after a failed first load, keeps
// failing closed). A successful reload clears loadErr.
func (e *Engine) reload(ctx context.Context) error {
	rows, err := e.db.QueryContext(ctx,
		`SELECT id, scope, target, action, COALESCE(note,''), created_at, updated_at FROM tool_policies`)
	if err != nil {
		return err
	}
	defer rows.Close()
	byTool := map[string]ToolPolicy{}
	byUp := map[string]ToolPolicy{}
	for rows.Next() {
		var p ToolPolicy
		if err := rows.Scan(&p.ID, &p.Scope, &p.Target, &p.Action, &p.Note, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return err
		}
		if p.Scope == ScopeUpstream {
			byUp[p.Target] = p
		} else {
			byTool[p.Target] = p
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	e.mu.Lock()
	e.byTool, e.byUp = byTool, byUp
	e.loadErr = nil
	e.mu.Unlock()
	return nil
}

// Eval returns the decision for req.
func (e *Engine) Eval(req Request) Decision {
	// Explicit policies: tool scope beats upstream scope, and it beats the
	// meta-tool shortcut below, so an operator can deny or gate one tools.*
	// tool (executeToolCode) while the group stays open by default.
	e.mu.RLock()
	tp, okT := e.byTool[req.ToolName]
	up, okU := e.byUp[req.UpstreamName]
	loadErr := e.loadErr
	e.mu.RUnlock()
	if loadErr != nil {
		return unavailableDecision()
	}
	if okT {
		if d, ok := decisionFromPolicy(tp); ok {
			return d
		}
	}

	if req.UpstreamName == "tools" {
		return Decision{Action: ActionAllow, Reason: "meta-tool routes inner call", RuleID: "v0.1-meta-tool"}
	}

	if okU {
		if d, ok := decisionFromPolicy(up); ok {
			return d
		}
	}

	// A declared intent only escalates. "read" is the caller's word, not a
	// fact, so it falls through to the name heuristic and the default like
	// any unknown value; trusting it would let an agent skip approval on a
	// write just by saying so.
	switch req.IntentCategory {
	case "write", "destructive", "external_communication", "financial", "privileged_admin":
		return Decision{
			Action: ActionApprove,
			Reason: "category=" + req.IntentCategory + " requires human approval",
			RuleID: "v0.1-category",
		}
	}
	if IsReadOnlyName(req.ToolName) {
		return Decision{Action: ActionAllow, Reason: "tool name looks read-only", RuleID: "v0.1-name-heuristic"}
	}
	return Decision{
		Action: ActionApprove,
		Reason: "default: writes require approval",
		RuleID: "v0.1-default-write",
	}
}

// ExplicitDeny returns the explicit deny that applies to a call of tool (the
// wrapped catalog name, as in Request.ToolName) on upstream, and true. It
// resolves the explicit rules exactly as Eval does: a tool-scope rule wins,
// then the meta-tool shortcut shadows any upstream-scope rule on the tools
// group, then an upstream-scope rule applies. It returns Decision{} and
// false when no explicit rule applies or the winning rule is not a deny
// (so an upstream deny with a tool allow is not a deny). While the stored
// policies have not loaded (see Ready), it returns the RuleUnavailable deny
// and true, as Eval does.
func (e *Engine) ExplicitDeny(tool, upstream string) (Decision, bool) {
	e.mu.RLock()
	tp, okT := e.byTool[tool]
	up, okU := e.byUp[upstream]
	loadErr := e.loadErr
	e.mu.RUnlock()
	if loadErr != nil {
		return unavailableDecision(), true
	}
	if okT {
		if d, ok := decisionFromPolicy(tp); ok {
			if d.Action == ActionDeny {
				return d, true
			}
			return Decision{}, false
		}
	}
	if upstream == "tools" {
		return Decision{}, false
	}
	if okU {
		if d, ok := decisionFromPolicy(up); ok && d.Action == ActionDeny {
			return d, true
		}
	}
	return Decision{}, false
}

func unavailableDecision() Decision {
	return Decision{
		Action: ActionDeny,
		Reason: "policy rules are unavailable (they failed to load); every call is denied until they load",
		RuleID: RuleUnavailable,
	}
}

func decisionFromPolicy(p ToolPolicy) (Decision, bool) {
	switch p.Action {
	case "allow":
		return Decision{Action: ActionAllow, Reason: "explicit " + p.Scope + "-policy: allow", RuleID: p.ID}, true
	case "deny":
		return Decision{Action: ActionDeny, Reason: "explicit " + p.Scope + "-policy: deny", RuleID: p.ID}, true
	case "ask":
		return Decision{Action: ActionApprove, Reason: "explicit " + p.Scope + "-policy: ask", RuleID: p.ID, RequireHuman: true}, true
	}
	return Decision{}, false
}

// ---- CRUD -------------------------------------------------------------------

// ErrForceRequired is returned when allowing a destructive-looking tool
// without force=true. Allowing such a tool bypasses the destructive veto, so
// it must be an explicit operator override.
var ErrForceRequired = errString("allowing a destructive tool requires force=true")

// ErrNoDB is returned by CRUD methods on a heuristic-only (nil-db) engine.
var ErrNoDB = errString("policy engine has no database")

type errString string

func (e errString) Error() string { return string(e) }

// List returns all stored policies (cache snapshot).
func (e *Engine) List() []ToolPolicy {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]ToolPolicy, 0, len(e.byTool)+len(e.byUp))
	for _, p := range e.byTool {
		out = append(out, p)
	}
	for _, p := range e.byUp {
		out = append(out, p)
	}
	return out
}

// WithChange returns a detached copy of the engine's rule set with one
// change applied: action allow|ask|deny upserts the (scope,target) rule,
// "" removes it. The copy has no database, so it can only Eval; nothing
// is persisted. It is how the agent-facing policies.set works out what a
// change would do to every tool's effective access before making it. A
// copy of an engine whose rules have not loaded fails closed as well.
func (e *Engine) WithChange(scope, target, action string) *Engine {
	e.mu.RLock()
	loadErr := e.loadErr
	byTool := make(map[string]ToolPolicy, len(e.byTool)+1)
	for k, v := range e.byTool {
		byTool[k] = v
	}
	byUp := make(map[string]ToolPolicy, len(e.byUp)+1)
	for k, v := range e.byUp {
		byUp[k] = v
	}
	e.mu.RUnlock()
	m := byTool
	if scope == ScopeUpstream {
		m = byUp
	}
	if action == "" {
		delete(m, target)
	} else {
		p, ok := m[target]
		if !ok {
			p = ToolPolicy{ID: "tp_simulated", Scope: scope, Target: target}
		}
		p.Action = action
		m[target] = p
	}
	return &Engine{byTool: byTool, byUp: byUp, loadErr: loadErr}
}

// Get returns the stored policy for a (scope,target), if any.
func (e *Engine) Get(scope, target string) (ToolPolicy, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if scope == ScopeUpstream {
		p, ok := e.byUp[target]
		return p, ok
	}
	p, ok := e.byTool[target]
	return p, ok
}

// Set upserts a policy. action must be allow|ask|deny. Allowing a
// destructive-looking tool requires force=true.
func (e *Engine) Set(ctx context.Context, scope, target, action, note string, force bool) (*ToolPolicy, error) {
	if e.db == nil {
		return nil, ErrNoDB
	}
	unlock, err := e.lockWrite(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	return e.setLocked(ctx, scope, target, action, note, force)
}

// Change applies one change, action allow|ask|deny upserting the
// (scope,target) rule and "" removing it, after guard has approved it:
// guard sees the engine as it is (before) and a detached copy with the
// change applied (after), and a non-nil error aborts without writing and
// is returned as is. Check and write happen under the write lock, so no
// Set or Delete from anywhere lands in between. guard must not call Set,
// Delete or Change itself. existed reports, for a removal, whether there
// was a rule to remove; p is the stored rule after an upsert.
func (e *Engine) Change(ctx context.Context, scope, target, action, note string, force bool,
	guard func(before, after *Engine) error) (p *ToolPolicy, existed bool, err error) {
	if e.db == nil {
		return nil, false, ErrNoDB
	}
	if scope != ScopeTool && scope != ScopeUpstream {
		return nil, false, errString("scope must be tool or upstream")
	}
	if action != "" && action != "allow" && action != "ask" && action != "deny" {
		return nil, false, errString("action must be allow, ask, or deny")
	}
	unlock, err := e.lockWrite(ctx)
	if err != nil {
		return nil, false, err
	}
	defer unlock()
	// The guard judges the change against the rules in force. While the
	// first load has not succeeded the engine does not know them, so read
	// them now and refuse the change if they still cannot be read.
	if e.Ready() != nil {
		if err := e.reload(ctx); err != nil {
			return nil, false, fmt.Errorf("load tool policies: %w", err)
		}
	}
	if guard != nil {
		if err := guard(e, e.WithChange(scope, target, action)); err != nil {
			return nil, false, err
		}
	}
	if action == "" {
		if _, ok := e.Get(scope, target); !ok {
			return nil, false, nil
		}
		return nil, true, e.deleteTargetLocked(ctx, scope, target)
	}
	p, err = e.setLocked(ctx, scope, target, action, note, force)
	return p, true, err
}

// setLocked is Set with writeMu held.
func (e *Engine) setLocked(ctx context.Context, scope, target, action, note string, force bool) (*ToolPolicy, error) {
	if scope != ScopeTool && scope != ScopeUpstream {
		return nil, errString("scope must be tool or upstream")
	}
	if action != "allow" && action != "ask" && action != "deny" {
		return nil, errString("action must be allow, ask, or deny")
	}
	if action == "allow" && scope == ScopeTool && IsDestructiveName(target) && !force {
		return nil, ErrForceRequired
	}
	now := time.Now().UnixMilli()
	id := "tp_" + uuid.NewString()
	// Upsert keyed on (scope,target). Keep the existing id/created_at on update.
	_, err := e.db.ExecContext(ctx,
		`INSERT INTO tool_policies(id, scope, target, action, note, created_at, updated_at)
         VALUES(?,?,?,?,?,?,?)
         ON CONFLICT(scope,target) DO UPDATE SET action=excluded.action, note=excluded.note, updated_at=excluded.updated_at`,
		id, scope, target, action, nullStr(note), now, now)
	if err != nil {
		return nil, err
	}
	e.publishChange(ctx)
	// A failed reload is returned to the caller; the cache keeps its last
	// good snapshot until a later reload succeeds.
	if err := e.reload(ctx); err != nil {
		return nil, err
	}
	p, _ := e.Get(scope, target)
	return &p, nil
}

// Delete removes a policy by id. A failed reload after the delete is
// returned; the cache keeps its last good snapshot.
func (e *Engine) Delete(ctx context.Context, id string) error {
	if e.db == nil {
		return ErrNoDB
	}
	unlock, err := e.lockWrite(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := e.db.ExecContext(ctx, `DELETE FROM tool_policies WHERE id = ?`, id); err != nil {
		return err
	}
	e.publishChange(ctx)
	return e.reload(ctx)
}

// DeleteTarget removes a policy by (scope,target). Used by the insights
// "default" mode to clear an explicit policy.
func (e *Engine) DeleteTarget(ctx context.Context, scope, target string) error {
	if e.db == nil {
		return ErrNoDB
	}
	unlock, err := e.lockWrite(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	return e.deleteTargetLocked(ctx, scope, target)
}

// deleteTargetLocked is DeleteTarget with writeMu held.
func (e *Engine) deleteTargetLocked(ctx context.Context, scope, target string) error {
	if _, err := e.db.ExecContext(ctx, `DELETE FROM tool_policies WHERE scope = ? AND target = ?`, scope, target); err != nil {
		return err
	}
	e.publishChange(ctx)
	return e.reload(ctx)
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// readVerbs are the leading verb-words that mark a tool as read-only when
// matched against the first segment of a tool name (split on . _ /).
var readVerbs = map[string]bool{
	"get": true, "list": true, "search": true, "find": true, "read": true,
	"show": true, "describe": true, "fetch": true, "view": true,
	"lookup": true, "query": true, "head": true, "ls": true,
}

// IsReadOnlyName is a name-only heuristic: returns true when the tool name's
// first verb-segment matches one of the known read verbs. Exported so the
// gateway can populate its is_write metric column without re-running policy
// eval.
func IsReadOnlyName(name string) bool {
	low := strings.ToLower(name)
	if i := strings.IndexAny(low, "./"); i >= 0 {
		low = low[i+1:]
	}
	first := low
	for i := 0; i < len(low); i++ {
		if low[i] == '_' || low[i] == '-' {
			first = low[:i]
			break
		}
	}
	return readVerbs[first]
}

// destructiveMarkers flag tool names whose explicit `allow` policy bypasses
// the destructive veto and therefore needs force=true.
var destructiveMarkers = []string{"delete", "destroy", "drop", "remove", "purge", "wipe", "truncate"}

// IsDestructiveName reports whether a tool name looks destructive.
func IsDestructiveName(name string) bool {
	low := strings.ToLower(name)
	for _, m := range destructiveMarkers {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}
