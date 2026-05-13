// Package approval implements toolyard's approval bus.
//
// When a tool call needs human review the gateway calls Hold(), which
// persists the request, fans out to push + realtime listeners, and waits.
// The bus exposes Decide() so the REST API or hook can resolve a pending call,
// and Watch() so the gateway side can wait for the result.
//
// State survives restart: at startup, Recover() loads pending rows back into
// memory so a tap from the phone can still resolve the call (the agent host
// may have timed out, but the audit trail remains correct).
package approval

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/tusharbhardwaj/toolyard/internal/store"
)

const (
	StatusPending   = "pending"
	StatusAllowed   = "allowed"
	StatusDenied    = "denied"
	StatusExpired   = "expired"
	StatusCancelled = "cancelled"

	signingKeyPurpose = "approval_sign"
	// DefaultTTL is how long an approval row stays decidable. Phones and
	// laptops sleep, you may be in a meeting; three hours is a comfortable
	// upper bound that's still bounded enough to not pile up rows
	// indefinitely. Override per-process via Bus.SetTTL.
	DefaultTTL = 3 * time.Hour
)

var (
	ErrNotPending = errors.New("approval not pending")
	ErrNotFound   = errors.New("approval not found")
)

type Request struct {
	ID             string         `json:"id"`
	AgentID        string         `json:"agent_id"`
	UpstreamName   string         `json:"upstream_name"`
	ToolName       string         `json:"tool_name"`
	Arguments      map[string]any `json:"arguments"`
	Reason         string         `json:"reason"`
	IntentCategory string         `json:"intent_category,omitempty"`
	Status         string         `json:"status"`
	DecisionToken  string         `json:"decision_token,omitempty"`
	DecidedBy      string         `json:"decided_by,omitempty"`
	DecidedAt      int64          `json:"decided_at,omitempty"`
	CreatedAt      int64          `json:"created_at"`
	ExpiresAt      int64          `json:"expires_at"`
	Fingerprint    string         `json:"fingerprint,omitempty"`
	Coalesced      bool           `json:"coalesced,omitempty"` // true if Hold returned an existing pending row instead of creating a new one
	// AutoDecidedBy is the auto-approval rule id that decided this request,
	// or empty if a human (or the rule engine wasn't consulted). Set on
	// Status=allowed responses produced via the AutoApprover hook.
	AutoDecidedBy string `json:"auto_decided_by,omitempty"`

	// ResultEnvelope is the JSON-encoded executed tool result; populated
	// after the executor runs the approved tool and persists the answer.
	// Empty means "approved but executor hasn't completed yet" (or the
	// row pre-dates the auto-execute feature).
	ResultEnvelope string `json:"result_envelope,omitempty"`
	// ResultIsError mirrors mcp.CallToolResult.IsError on the executed
	// result. Meaningful only when ResultExecutedAt > 0.
	ResultIsError bool `json:"result_is_error,omitempty"`
	// ResultExecutedAt is the ms-UTC timestamp at which the executor
	// finished writing the result. Zero means "not yet executed."
	ResultExecutedAt int64 `json:"result_executed_at,omitempty"`
	// ResultError, when non-empty, is a toolyard-side execution failure
	// that prevented the tool from being invoked at all (e.g., the
	// upstream disappeared between approval and execution).
	ResultError string `json:"result_error,omitempty"`
}

// Notifier is implemented by the push and realtime services so the bus can
// fan out events without depending on them directly.
type Notifier interface {
	OnApproval(ctx context.Context, req *Request, eventType string)
}

// notifierFunc lets us register simple closures.
type notifierFunc func(ctx context.Context, req *Request, eventType string)

func (f notifierFunc) OnApproval(ctx context.Context, req *Request, eventType string) {
	f(ctx, req, eventType)
}

type pending struct {
	req  *Request
	done chan struct{}
}

// AutoApprover is the optional hook that decides whether a fresh approval
// request should be auto-decided rather than queued for human review.
// Returning a non-empty ruleID short-circuits the bus into an immediate
// allow with decided_via="auto".
type AutoApprover interface {
	Match(agentID, upstream, toolName, fingerprint string, isDestructive bool) *AutoMatch
	MarkHit(ctx context.Context, ruleID, agentID string)
	MarkDenial(ctx context.Context, agentID, toolName, fingerprint string)
	IsDestructive(ctx context.Context, toolName string) bool
}

// AutoMatch is the return value from AutoApprover.Match. ID is the rule's
// stable identifier — it appears as decided_by="rule:<id>" on the resulting
// approved approval.
type AutoMatch struct {
	ID   string
	Kind string
}

type Bus struct {
	db        *store.DB
	signKey   ed25519.PrivateKey
	verifyKey ed25519.PublicKey
	ttl       time.Duration

	mu      sync.Mutex
	waiters map[string]*pending // approval_id -> waiter

	notifiers []Notifier

	auto AutoApprover
	exec Executor

	// bgCtx is the long-lived context used for background work the bus
	// initiates itself (auto-execute on approve, recovery sweep on
	// startup). The dashboard's Decide() returns the moment the human's
	// tap is persisted; the actual tool dispatch runs on bgCtx so the
	// HTTP request finishing doesn't cancel the executor.
	bgCtx context.Context
}

// Executor runs an approved tool's actual call after the human (or an
// auto-approval rule) flips the request to allowed. The bus invokes
// Execute on its own background goroutine and expects the executor to
// call Bus.SetResult exactly once per request — either with the
// CallToolResult envelope from the dispatched tool, or with a non-empty
// execErr describing why dispatch couldn't even be attempted.
type Executor interface {
	Execute(ctx context.Context, req *Request)
}

func New(ctx context.Context, db *store.DB) (*Bus, error) {
	priv, pub, err := loadOrCreateSigningKey(ctx, db)
	if err != nil {
		return nil, err
	}
	return &Bus{
		db:        db,
		signKey:   priv,
		verifyKey: pub,
		ttl:       DefaultTTL,
		waiters:   map[string]*pending{},
		bgCtx:     ctx,
	}, nil
}

// SetExecutor installs the auto-execute hook. With one set, bus.Decide
// flipping a request to "allowed" spawns a background goroutine that
// invokes Executor.Execute (with bgCtx). The executor must call
// SetResult to persist the outcome. Pass nil to fall back to the legacy
// behaviour where the agent re-calls the original tool with
// _approval_id to drive execution.
func (b *Bus) SetExecutor(e Executor) { b.exec = e }

// SetTTL overrides how long pending approvals stay decidable. Pre-existing
// rows are not retroactively changed — only future Hold() calls use the
// new value. Pass 0 or negative to keep the current TTL.
func (b *Bus) SetTTL(d time.Duration) {
	if d <= 0 {
		return
	}
	b.ttl = d
}

// TTL returns the current approval-decision TTL.
func (b *Bus) TTL() time.Duration { return b.ttl }

// SetAutoApprover wires the auto-approval engine in. Pass nil to disable.
func (b *Bus) SetAutoApprover(a AutoApprover) { b.auto = a }

// AddNotifier registers a fan-out target. Notifiers are called with no lock
// held so they may use blocking IO.
func (b *Bus) AddNotifier(n Notifier) { b.notifiers = append(b.notifiers, n) }

func (b *Bus) AddNotifierFunc(f func(ctx context.Context, req *Request, eventType string)) {
	b.AddNotifier(notifierFunc(f))
}

func (b *Bus) PublicKey() ed25519.PublicKey { return b.verifyKey }

// Hold creates an approval request and waits up to maxWait for a decision.
// The returned request is the final state (status will be allowed/denied/pending/expired).
// If the wait elapses but the approval is still pending, it remains pending in
// the DB and Watch() can still pick it up later.
func (b *Bus) Hold(ctx context.Context, in NewRequest, maxWait time.Duration) (*Request, error) {
	req, err := b.create(ctx, in)
	if err != nil {
		return nil, err
	}
	// Auto-approval short-circuit: if a rule matches the new request, decide
	// it in-line (status -> allowed) before any notifier sees it as pending.
	// Coalesced rows that landed on an existing pending request are skipped
	// — their fate is tied to the original.
	if b.auto != nil && !req.Coalesced && req.Status == StatusPending {
		destructive := b.auto.IsDestructive(ctx, req.ToolName)
		if m := b.auto.Match(req.AgentID, req.UpstreamName, req.ToolName, req.Fingerprint, destructive); m != nil {
			decided, derr := b.decideInline(ctx, req.ID, StatusAllowed, "rule:"+m.ID)
			if derr == nil && decided != nil {
				decided.AutoDecidedBy = m.ID
				b.auto.MarkHit(ctx, m.ID, req.AgentID)
				return decided, nil
			}
		}
	}
	b.fanOut(ctx, req, "approval.create")

	w := &pending{req: req, done: make(chan struct{})}
	b.mu.Lock()
	b.waiters[req.ID] = w
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.waiters, req.ID)
		b.mu.Unlock()
	}()

	if maxWait <= 0 {
		return req, nil
	}
	timer := time.NewTimer(maxWait)
	defer timer.Stop()

	select {
	case <-w.done:
		return b.Get(ctx, req.ID)
	case <-timer.C:
		return b.Get(ctx, req.ID)
	case <-ctx.Done():
		return b.Get(ctx, req.ID)
	}
}

// Watch returns a channel that closes when the given approval's status leaves
// "pending". Useful for the deferred-response polling path.
func (b *Bus) Watch(id string) (<-chan struct{}, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	w, ok := b.waiters[id]
	if !ok {
		return nil, false
	}
	return w.done, true
}

type NewRequest struct {
	AgentID        string
	UpstreamName   string
	ToolName       string
	Arguments      map[string]any
	Reason         string
	IntentCategory string
}

func (b *Bus) create(ctx context.Context, in NewRequest) (*Request, error) {
	// Coalesce: if there's already a pending row with the same
	// (agent, upstream, tool, args) fingerprint, return it. This stops a
	// retrying hook or a model that re-issues the same call from piling up
	// identical approval cards on the dashboard.
	fp := ComputeFingerprint(in.AgentID, in.UpstreamName, in.ToolName, in.Arguments)
	if existing, err := b.findPendingByFingerprint(ctx, fp); err == nil && existing != nil {
		existing.Coalesced = true
		return existing, nil
	}

	id := "ap_" + uuid.NewString()
	now := time.Now()
	req := &Request{
		ID:             id,
		AgentID:        in.AgentID,
		UpstreamName:   in.UpstreamName,
		ToolName:       in.ToolName,
		Arguments:      in.Arguments,
		Reason:         in.Reason,
		IntentCategory: in.IntentCategory,
		Status:         StatusPending,
		CreatedAt:      now.UnixMilli(),
		ExpiresAt:      now.Add(b.ttl).UnixMilli(),
		Fingerprint:    fp,
	}
	req.DecisionToken = b.signToken(req.ID)
	args, _ := json.Marshal(in.Arguments)
	_, err := b.db.ExecContext(ctx,
		`INSERT INTO approval_requests(id, agent_id, upstream_name, tool_name, arguments,
            reason, intent_category, status, decision_token, created_at, expires_at, fingerprint)
         VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		req.ID, req.AgentID, req.UpstreamName, req.ToolName, string(args),
		req.Reason, nullStr(req.IntentCategory), req.Status, req.DecisionToken,
		req.CreatedAt, req.ExpiresAt, fp)
	if err != nil {
		// SQLite's UNIQUE constraint on the partial fingerprint index can fire
		// if a parallel request landed first; fall back to coalescing.
		if isUniqueViolation(err) {
			if existing, lookupErr := b.findPendingByFingerprint(ctx, fp); lookupErr == nil && existing != nil {
				existing.Coalesced = true
				return existing, nil
			}
		}
		return nil, err
	}
	return req, nil
}

// ComputeFingerprint is sha256(agent_id||0x1f||upstream||0x1f||tool||0x1f||canonical-args).
// Canonical args are produced by sorting object keys recursively so
// {a:1, b:2} and {b:2, a:1} hash identically. Exported so other packages
// (e.g. autoapproval, gateway) can derive the same identity without
// reimplementing the hash.
func ComputeFingerprint(agentID, upstream, tool string, args map[string]any) string {
	h := sha256.New()
	_, _ = h.Write([]byte(agentID))
	h.Write([]byte{0x1f})
	_, _ = h.Write([]byte(upstream))
	h.Write([]byte{0x1f})
	_, _ = h.Write([]byte(tool))
	h.Write([]byte{0x1f})
	if blob, err := canonicalJSON(args); err == nil {
		_, _ = h.Write(blob)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// canonicalJSON marshals v with sorted object keys at every level so two
// equivalent argument maps produce identical bytes.
func canonicalJSON(v any) ([]byte, error) {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var buf []byte
		buf = append(buf, '{')
		for i, k := range keys {
			if i > 0 {
				buf = append(buf, ',')
			}
			kb, _ := json.Marshal(k)
			buf = append(buf, kb...)
			buf = append(buf, ':')
			vb, err := canonicalJSON(x[k])
			if err != nil {
				return nil, err
			}
			buf = append(buf, vb...)
		}
		buf = append(buf, '}')
		return buf, nil
	case []any:
		var buf []byte
		buf = append(buf, '[')
		for i, item := range x {
			if i > 0 {
				buf = append(buf, ',')
			}
			vb, err := canonicalJSON(item)
			if err != nil {
				return nil, err
			}
			buf = append(buf, vb...)
		}
		buf = append(buf, ']')
		return buf, nil
	default:
		return json.Marshal(v)
	}
}

// requestSelectColumns is the canonical column list for SELECTs that
// hydrate a Request. Centralised so adding a column is a one-line edit
// instead of a five-site grep.
const requestSelectColumns = `id, agent_id, upstream_name, tool_name, arguments, reason,
            COALESCE(intent_category,''), status, COALESCE(decision_token,''),
            COALESCE(decided_by,''), COALESCE(decided_at,0), created_at, expires_at,
            COALESCE(fingerprint,''),
            COALESCE(result_envelope,''), COALESCE(result_is_error,0),
            COALESCE(result_executed_at,0), COALESCE(result_error,'')`

// rowScanner is the subset shared by *sql.Row and *sql.Rows; lets the
// helper hydrate either with the same code.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanRequest reads one approval row from any source that SELECTed
// requestSelectColumns. The resulting Request is fully hydrated,
// including the Arguments map decoded from its JSON column.
func scanRequest(s rowScanner) (*Request, error) {
	var req Request
	var args string
	var isErr int
	if err := s.Scan(&req.ID, &req.AgentID, &req.UpstreamName, &req.ToolName, &args,
		&req.Reason, &req.IntentCategory, &req.Status, &req.DecisionToken,
		&req.DecidedBy, &req.DecidedAt, &req.CreatedAt, &req.ExpiresAt,
		&req.Fingerprint,
		&req.ResultEnvelope, &isErr, &req.ResultExecutedAt, &req.ResultError); err != nil {
		return nil, err
	}
	req.ResultIsError = isErr != 0
	if args != "" {
		_ = json.Unmarshal([]byte(args), &req.Arguments)
	}
	return &req, nil
}

func (b *Bus) findPendingByFingerprint(ctx context.Context, fp string) (*Request, error) {
	if fp == "" {
		return nil, nil
	}
	row := b.db.QueryRowContext(ctx,
		`SELECT `+requestSelectColumns+`
         FROM approval_requests
         WHERE status = ? AND fingerprint = ? LIMIT 1`,
		StatusPending, fp)
	req, err := scanRequest(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return req, nil
}

func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	// mattn/go-sqlite3 returns "UNIQUE constraint failed" in the message.
	return contains(msg, "UNIQUE constraint failed") || contains(msg, "constraint failed: UNIQUE")
}

func contains(s, needle string) bool {
	return len(needle) > 0 && len(s) >= len(needle) && stringIndex(s, needle) >= 0
}

// stringIndex avoids importing strings into this file just for one call.
func stringIndex(s, needle string) int {
	n := len(needle)
	for i := 0; i+n <= len(s); i++ {
		if s[i:i+n] == needle {
			return i
		}
	}
	return -1
}

// Decide is the public decision entrypoint used by the dashboard and
// the signed-token tap path. On allow it kicks the registered Executor
// on a background goroutine; pollers wake when SetResult lands.
func (b *Bus) Decide(ctx context.Context, id, action, userID string) (*Request, error) {
	return b.decide(ctx, id, action, userID, true)
}

// decideInline is the in-process variant used by Hold's auto-approval
// short-circuit. Hold's caller (gateway.holdAndWait) dispatches the
// approved tool synchronously in its own goroutine, so we MUST NOT
// also fire the background executor or the tool would run twice.
func (b *Bus) decideInline(ctx context.Context, id, action, userID string) (*Request, error) {
	return b.decide(ctx, id, action, userID, false)
}

// decide is the shared implementation. runExec controls whether an
// allow with a registered executor schedules background dispatch.
//
// On allow + runExec + executor present: signal() is deferred until
// the executor calls SetResult. Pollers therefore wake when the result
// is ready, not when the human merely tapped Allow — a single
// wait_for_approval gets back the executed result instead of forcing
// the agent to poll again.
//
// On deny / cancel / expire / allow-without-executor: signal() fires
// immediately because there is nothing to wait for.
func (b *Bus) decide(ctx context.Context, id, action, userID string, runExec bool) (*Request, error) {
	if action != StatusAllowed && action != StatusDenied {
		return nil, fmt.Errorf("invalid action %q", action)
	}
	res, err := b.db.ExecContext(ctx,
		`UPDATE approval_requests SET status = ?, decided_by = ?, decided_at = ?
         WHERE id = ? AND status = ?`,
		action, userID, time.Now().UnixMilli(), id, StatusPending)
	if err != nil {
		return nil, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		existing, getErr := b.Get(ctx, id)
		if getErr != nil {
			return nil, getErr
		}
		return existing, ErrNotPending
	}
	req, err := b.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if action == StatusDenied && b.auto != nil {
		b.auto.MarkDenial(ctx, req.AgentID, req.ToolName, req.Fingerprint)
	}
	b.fanOut(ctx, req, "approval.decide")
	if action == StatusAllowed && runExec && b.exec != nil {
		go b.runExecutor(req)
		return req, nil
	}
	b.signal(id)
	return req, nil
}

// runExecutor invokes the registered executor on the bus's bgCtx,
// recovers from a panicking executor, and records a generic execution
// failure if the executor disappears mid-flight without persisting a
// result.
func (b *Bus) runExecutor(req *Request) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("approval executor: panic on %s: %v", req.ID, r)
			_ = b.SetResult(b.bgCtx, req.ID, "", false, fmt.Sprintf("executor panic: %v", r))
		}
	}()
	b.exec.Execute(b.bgCtx, req)
	// Defensive: if the executor returned without persisting a result,
	// mark the row as failed so pollers don't spin forever.
	cur, err := b.Get(b.bgCtx, req.ID)
	if err == nil && cur.Status == StatusAllowed && cur.ResultExecutedAt == 0 {
		_ = b.SetResult(b.bgCtx, req.ID, "", false, "executor returned without persisting result")
	}
}

// SetResult persists the executed tool's outcome and wakes any pollers
// blocked on this approval. envelope is a JSON blob shaped like an MCP
// CallToolResult; execErr is non-empty only when toolyard itself
// couldn't run the tool (vs. the tool returning a logical error).
//
// The UPDATE is gated on result_executed_at IS NULL so a Decide-spawned
// executor and the recovery sweep can't race — whichever lands first
// wins; the loser is a no-op.
func (b *Bus) SetResult(ctx context.Context, id, envelope string, isError bool, execErr string) error {
	res, err := b.db.ExecContext(ctx,
		`UPDATE approval_requests
            SET result_envelope = ?, result_is_error = ?, result_error = ?, result_executed_at = ?
          WHERE id = ? AND result_executed_at IS NULL`,
		nullStr(envelope), boolInt(isError), nullStr(execErr), time.Now().UnixMilli(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		b.signal(id)
		if req, gerr := b.Get(ctx, id); gerr == nil {
			b.fanOut(ctx, req, "approval.executed")
		}
	}
	return nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// SweepUnexecuted re-fires the executor for any approval that was
// allowed but whose result was never persisted. Called once at startup
// to recover from a crash between "human tapped Allow" and "tool
// finished writing the result." Safe to call multiple times — the
// SetResult UPDATE is gated on result_executed_at IS NULL.
func (b *Bus) SweepUnexecuted(ctx context.Context) (int, error) {
	if b.exec == nil {
		return 0, nil
	}
	rows, err := b.db.QueryContext(ctx,
		`SELECT `+requestSelectColumns+`
         FROM approval_requests
         WHERE status = ? AND result_executed_at IS NULL
         ORDER BY decided_at ASC`, StatusAllowed)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var pending []*Request
	for rows.Next() {
		req, err := scanRequest(rows)
		if err != nil {
			return 0, err
		}
		pending = append(pending, req)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, req := range pending {
		go b.runExecutor(req)
	}
	return len(pending), nil
}

// DecideByToken verifies a signed decision token and resolves the approval.
// Used for one-tap approval from a phone tap that may not carry the user
// session cookie. Returns the verified approval ID on success.
func (b *Bus) DecideByToken(ctx context.Context, token, action string) (*Request, error) {
	id, err := b.verifyToken(token)
	if err != nil {
		return nil, err
	}
	return b.Decide(ctx, id, action, "token")
}

// Get fetches an approval by ID, regardless of status.
func (b *Bus) Get(ctx context.Context, id string) (*Request, error) {
	row := b.db.QueryRowContext(ctx,
		`SELECT `+requestSelectColumns+` FROM approval_requests WHERE id = ?`, id)
	req, err := scanRequest(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return req, nil
}

// CancelByAgent flips a pending approval to cancelled, but only if the
// agent owns it. Used by tools.cancel_my_approval so an AI can withdraw
// its own queue entry without involving the human reviewer. Errors with
// ErrNotPending if the approval is already decided/expired/cancelled,
// or ErrNotFound on missing/wrong-agent.
func (b *Bus) CancelByAgent(ctx context.Context, id, agentID string) (*Request, error) {
	res, err := b.db.ExecContext(ctx,
		`UPDATE approval_requests SET status = ?, decided_by = ?, decided_at = ?
         WHERE id = ? AND status = ? AND agent_id = ?`,
		StatusCancelled, "agent:"+agentID, time.Now().UnixMilli(), id, StatusPending, agentID)
	if err != nil {
		return nil, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		existing, getErr := b.Get(ctx, id)
		if getErr != nil {
			return nil, ErrNotFound
		}
		// Either decided already, expired, or owned by a different agent —
		// uniformly reported as ErrNotPending so callers don't probe other
		// agents' queues.
		if existing.AgentID != agentID {
			return nil, ErrNotFound
		}
		return existing, ErrNotPending
	}
	req, err := b.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	b.signal(id)
	b.fanOut(ctx, req, "approval.cancel")
	return req, nil
}

// ListPendingByAgent is the agent-scoped view used by
// tools.list_my_pending_approvals — only returns rows whose agent_id
// matches the caller, never leaks other agents' queues.
func (b *Bus) ListPendingByAgent(ctx context.Context, agentID string) ([]Request, error) {
	if agentID == "" {
		return nil, nil
	}
	rows, err := b.db.QueryContext(ctx,
		`SELECT `+requestSelectColumns+`
         FROM approval_requests WHERE status = ? AND agent_id = ?
         ORDER BY created_at ASC`, StatusPending, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Request
	for rows.Next() {
		req, err := scanRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *req)
	}
	return out, rows.Err()
}

// CountPendingForAgent is the per-agent budget enforcer: how many
// pendings does this agent currently have outstanding?
func (b *Bus) CountPendingForAgent(ctx context.Context, agentID string) (int, error) {
	if agentID == "" {
		return 0, nil
	}
	row := b.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM approval_requests WHERE status = ? AND agent_id = ?`,
		StatusPending, agentID)
	var n int
	if err := row.Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

func (b *Bus) ListPending(ctx context.Context) ([]Request, error) {
	rows, err := b.db.QueryContext(ctx,
		`SELECT `+requestSelectColumns+`
         FROM approval_requests WHERE status = ? ORDER BY created_at DESC`, StatusPending)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Request
	for rows.Next() {
		req, err := scanRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *req)
	}
	return out, rows.Err()
}

// Recent returns up to limit approvals (any status).
func (b *Bus) Recent(ctx context.Context, limit int) ([]Request, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := b.db.QueryContext(ctx,
		`SELECT `+requestSelectColumns+`
         FROM approval_requests ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Request
	for rows.Next() {
		req, err := scanRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *req)
	}
	return out, rows.Err()
}

// SweepExpired marks pending requests past their expiry as expired.
func (b *Bus) SweepExpired(ctx context.Context) (int, error) {
	now := time.Now().UnixMilli()
	res, err := b.db.ExecContext(ctx,
		`UPDATE approval_requests SET status = ? WHERE status = ? AND expires_at <= ?`,
		StatusExpired, StatusPending, now)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		// Wake any in-memory waiters so they observe the new status.
		b.mu.Lock()
		for _, w := range b.waiters {
			select {
			case <-w.done:
			default:
				close(w.done)
			}
		}
		b.mu.Unlock()
	}
	return int(n), nil
}

// RunSweeper periodically expires pending approvals.
func (b *Bus) RunSweeper(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := b.SweepExpired(ctx); err != nil {
				log.Printf("approval sweep: %v", err)
			} else if n > 0 {
				log.Printf("approval sweep: expired %d", n)
			}
		}
	}
}

func (b *Bus) signal(id string) {
	b.mu.Lock()
	w, ok := b.waiters[id]
	b.mu.Unlock()
	if !ok {
		return
	}
	select {
	case <-w.done:
	default:
		close(w.done)
	}
}

func (b *Bus) fanOut(ctx context.Context, req *Request, eventType string) {
	for _, n := range b.notifiers {
		// Decouple from caller's context cancellation; we always want to publish.
		go func(n Notifier) {
			defer func() { _ = recover() }()
			n.OnApproval(ctx, req, eventType)
		}(n)
	}
}

// ---- Ed25519 token plumbing -------------------------------------------------

// signToken returns a base64url(sig || id) so the dashboard/phone can carry an
// approval ID that the server can verify without a session round-trip.
func (b *Bus) signToken(id string) string {
	sig := ed25519.Sign(b.signKey, []byte(id))
	out := make([]byte, 0, len(sig)+len(id))
	out = append(out, sig...)
	out = append(out, []byte(id)...)
	return base64.RawURLEncoding.EncodeToString(out)
}

func (b *Bus) verifyToken(token string) (string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) < ed25519.SignatureSize+1 {
		return "", errors.New("malformed approval token")
	}
	sig := raw[:ed25519.SignatureSize]
	id := raw[ed25519.SignatureSize:]
	if !ed25519.Verify(b.verifyKey, id, sig) {
		return "", errors.New("invalid approval signature")
	}
	return string(id), nil
}

func loadOrCreateSigningKey(ctx context.Context, db *store.DB) (ed25519.PrivateKey, ed25519.PublicKey, error) {
	row := db.QueryRowContext(ctx,
		`SELECT private_key, public_key FROM server_keys WHERE purpose = ?`, signingKeyPurpose)
	var priv, pub []byte
	err := row.Scan(&priv, &pub)
	if err == nil {
		return ed25519.PrivateKey(priv), ed25519.PublicKey(pub), nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, nil, err
	}
	pubKey, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO server_keys(id, purpose, private_key, public_key, created_at) VALUES(?,?,?,?,?)`,
		"key_"+uuid.NewString(), signingKeyPurpose, []byte(privKey), []byte(pubKey),
		time.Now().UnixMilli()); err != nil {
		return nil, nil, err
	}
	return privKey, pubKey, nil
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
