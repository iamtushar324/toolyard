// Package coderuns runs code-mode scripts for the REST code route
// (docs/design/code-rest-route.md, TEC-1627): the backup path an agent
// uses when its MCP connection fails.
//
// A run is a row in code_runs. Start takes a code-mode slot first (so a
// busy server answers 429 and creates nothing), stores the run, and runs
// the script on a goroutine that outlives the HTTP request; Get reads the
// run and can wait for it to reach a final or held status; Resume runs a
// held script again from the top after the owner decided. Every tool call
// of a run is recorded in code_run_calls (the tool, a hash of the script's
// arguments, the outcome and the value the script received), so a resume
// answers the calls before the held one from that record instead of
// sending them again, refuses a script that took another path, and never
// repeats a call whose outcome is unknown. Recover settles the runs a
// stopped process left behind, and Purge drops runs older than 7 days.
//
// One process owns its runs: the waiters are told in process, with the
// database as the fallback.
package coderuns

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/tusharbhardwaj/toolyard/internal/cluster"
	"github.com/tusharbhardwaj/toolyard/internal/codemode"
	"github.com/tusharbhardwaj/toolyard/internal/goroutines"
	"github.com/tusharbhardwaj/toolyard/internal/inbox"
	"github.com/tusharbhardwaj/toolyard/internal/logx"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// Run statuses of this package; the final and held ones are codemode's
// (codemode.StatusSucceeded and friends).
const (
	// StatusQueued: the run is stored and its script has not started.
	StatusQueued = "queued"
	// StatusRunning: the script is running.
	StatusRunning = "running"
)

// ErrorInterrupted is the error kind of a run the server stopped under:
// a queued run is failed with it (nothing ran), a running one is
// outcome_unknown with it (a tool call may have run).
const ErrorInterrupted = "interrupted"

// Limits of the REST route.
const (
	// MaxScriptBytes caps a script.
	MaxScriptBytes = 1 << 20
	// MinReasonLen and MaxReasonLen bound the trimmed reason in bytes, as
	// the gateway bounds a call's _reason.
	MinReasonLen = 20
	MaxReasonLen = 2000
	// MaxIdempotencyKeyLen caps an Idempotency-Key.
	MaxIdempotencyKeyLen = 128
	// MaxWait caps how long Get waits for a final or held status.
	MaxWait = 60 * time.Second
	// Retention is how long runs and their call records are kept.
	Retention = 7 * 24 * time.Hour
)

// goroutineName labels a panic recovered in a run goroutine.
const goroutineName = "code-run"

// Errors the service returns; the API maps each to its answer.
var (
	// ErrNotFound: no run with this id belongs to the agent.
	ErrNotFound = errors.New("run not found")
	// ErrIdempotencyKeyReused: the agent used the key with another script
	// or reason.
	ErrIdempotencyKeyReused = errors.New("idempotency_key_reused")
	// ErrNotHeld: a resume of a run that is not permission_required or
	// deferred.
	ErrNotHeld = errors.New("run_not_held")
	// ErrDraining: the server is shutting down; nothing was started.
	ErrDraining = errors.New("toolyard is restarting: no run was started; try again in a moment")
	// ErrUnavailable: this server has no code-mode runtime or Inbox.
	ErrUnavailable = errors.New("code mode is not available on this server")
	// ErrTooLarge: the script is over MaxScriptBytes.
	ErrTooLarge = errors.New("the script is over 1 MiB, the code-mode limit")
)

// InputError is bad input in a start or resume request (400).
type InputError struct {
	Field   string
	Message string
}

func (e *InputError) Error() string { return e.Field + ": " + e.Message }

// Runner runs scripts: *codemode.Runtime.
type Runner interface {
	AcquireSlot(ctx context.Context) (*codemode.Slot, error)
	Run(ctx context.Context, code string, opts codemode.RunOptions) (*codemode.RunResult, error)
}

// Engine is what a run needs from the gateway.
type Engine interface {
	// Runner returns the runtime for one run (nil when there is none).
	// Ask for it per run: the gateway may replace its runtime.
	Runner() Runner
	// RunContext is the context a run's nested calls run on: the agent
	// they are scoped and audited as, the verified client session, and
	// admission past a drain. ok is false while the gateway drains.
	RunContext(ctx context.Context, agentID, sessionID string) (context.Context, bool)
}

// CodeModeGateway is the part of *gateway.Gateway GatewayEngine uses.
type CodeModeGateway interface {
	CodeMode() *codemode.Runtime
	CodeModeContext(ctx context.Context, agentID, sessionID string) (context.Context, bool)
}

// GatewayEngine adapts the gateway to Engine.
func GatewayEngine(g CodeModeGateway) Engine { return gatewayEngine{g: g} }

type gatewayEngine struct{ g CodeModeGateway }

func (e gatewayEngine) Runner() Runner {
	rt := e.g.CodeMode()
	if rt == nil {
		return nil
	}
	return rt
}

func (e gatewayEngine) RunContext(ctx context.Context, agentID, sessionID string) (context.Context, bool) {
	return e.g.CodeModeContext(ctx, agentID, sessionID)
}

// Grants reads an agent's Inbox requests: *inbox.Service. Status returns
// the still-valid unused grant tokens of an approved request without
// spending them.
type Grants interface {
	Status(ctx context.Context, agentID string, ids []string) ([]inbox.AgentView, error)
}

// grantRequests is implemented by a Grants that can also read a request
// (*inbox.Service): a resume then matches each grant to a call by its
// parameters, not only by its tool.
type grantRequests interface {
	Get(ctx context.Context, id string) (*inbox.Request, error)
}

// Options configure a Service.
type Options struct {
	// Engine runs scripts; nil makes Start and Resume ErrUnavailable.
	Engine Engine
	// Grants reads Inbox grants for a resume with a request_id; nil makes
	// such a resume ErrUnavailable.
	Grants Grants
	// Now is the clock (tests); nil is time.Now.
	Now func() time.Time
	// PollEvery is how often a waiting Get rereads the database besides
	// the in-process notice; 0 is one second.
	PollEvery time.Duration
}

// Service starts, reads, resumes, recovers and purges runs.
type Service struct {
	db        *store.DB
	engine    Engine
	grants    Grants
	now       func() time.Time
	pollEvery time.Duration
	log       *slog.Logger
	notify    *notifier

	mu sync.Mutex
	// inflight cancels each run goroutine of this process, by run id.
	inflight map[string]context.CancelFunc
	// changed is closed and replaced whenever a run goroutine ends.
	changed chan struct{}

	// released is closed by ReleaseWaiters: waiting Gets answer at once.
	released    chan struct{}
	releaseOnce sync.Once

	// cluster names this node when replicas share the database
	// (SetCluster, lease.go); nil is a single node.
	cluster *cluster.Cluster
}

// New returns a Service over db.
func New(db *store.DB, opts Options) *Service {
	s := &Service{
		db: db, engine: opts.Engine, grants: opts.Grants, now: opts.Now, pollEvery: opts.PollEvery,
		log: logx.For("coderuns"), notify: newNotifier(),
		inflight: map[string]context.CancelFunc{}, changed: make(chan struct{}), released: make(chan struct{}),
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.pollEvery <= 0 {
		s.pollEvery = time.Second
	}
	return s
}

// Run is one stored run.
type Run struct {
	ID             string
	AgentID        string
	SessionID      string
	IdempotencyKey string
	Fingerprint    string
	Reason         string
	Status         string
	// Result is the script's result as JSON (succeeded runs only).
	Result       json.RawMessage
	ErrorKind    string
	ErrorMessage string
	Logs         []string
	// Held is the call that held the run; meaningful while the status is
	// permission_required or deferred.
	Held  *Held
	Calls []Call
	// CreatedAt, UpdatedAt and FinishedAt are Unix milliseconds;
	// FinishedAt is 0 until the run is final.
	CreatedAt  int64
	UpdatedAt  int64
	FinishedAt int64

	// code is the script; loaded for a resume only.
	code string
}

// Held is the call that held a run.
type Held struct {
	Seq        int             `json:"seq"`
	Tool       string          `json:"tool"`
	Target     string          `json:"target,omitempty"`
	Status     string          `json:"status"`
	Draft      json.RawMessage `json:"draft,omitempty"`
	ApprovalID string          `json:"approval_id,omitempty"`
	Message    string          `json:"message,omitempty"`
}

// Call is one recorded tool call.
type Call struct {
	Seq    int
	Tool   string
	Target string
	// ArgsHash is the sha256 of the canonical JSON of the script's own
	// arguments.
	ArgsHash string
	Status   string
	Replayed bool
	// StartedAt and FinishedAt are Unix milliseconds.
	StartedAt  int64
	FinishedAt int64

	// replayText is the value the script received ("" unless succeeded).
	replayText string
}

// InProgress reports a status that will still change on its own: queued
// or running.
func InProgress(status string) bool { return status == StatusQueued || status == StatusRunning }

// IsHeld reports a status a resume may continue: permission_required or
// deferred.
func IsHeld(status string) bool {
	return status == codemode.StatusPermissionRequired || status == codemode.StatusDeferred
}

// HTTPStatus is the HTTP status of a run view for a run status
// (docs/design/code-rest-route.md, "HTTP status per run status").
func HTTPStatus(status string) int {
	switch status {
	case codemode.StatusSucceeded, codemode.StatusFailed:
		return http.StatusOK
	case codemode.StatusPermissionRequired:
		return http.StatusForbidden
	case codemode.StatusOutcomeUnknown:
		return http.StatusConflict
	}
	return http.StatusAccepted // deferred, queued, running
}

// RunPath is a run's URL path.
func RunPath(id string) string { return "/v1/code/runs/" + id }

// View is the run view every run route answers.
type View struct {
	RunID     string          `json:"run_id"`
	Status    string          `json:"status"`
	Result    json.RawMessage `json:"result"`
	Error     *ViewError      `json:"error"`
	Logs      []string        `json:"logs"`
	Calls     []ViewCall      `json:"calls"`
	Held      *ViewHeld       `json:"held"`
	CreatedAt int64           `json:"created_at"`
	UpdatedAt int64           `json:"updated_at"`
	Links     Links           `json:"links"`
}

// ViewError is a failed or outcome_unknown run's error.
type ViewError struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

// ViewCall is one call in the run view.
type ViewCall struct {
	Seq        int    `json:"seq"`
	Tool       string `json:"tool"`
	Status     string `json:"status"`
	Replayed   bool   `json:"replayed"`
	StartedAt  int64  `json:"started_at"`
	FinishedAt *int64 `json:"finished_at"`
}

// ViewHeld is the held call in the run view.
type ViewHeld struct {
	Seq        int             `json:"seq"`
	Tool       string          `json:"tool"`
	Status     string          `json:"status"`
	Draft      json.RawMessage `json:"draft"`
	ApprovalID *string         `json:"approval_id"`
	// Message is the gateway's text for the call: what to do next.
	Message string `json:"message,omitempty"`
}

// Links are the run's URL paths.
type Links struct {
	Self   string `json:"self"`
	Resume string `json:"resume,omitempty"`
}

// View renders the run view.
func (r *Run) View() View {
	v := View{
		RunID: r.ID, Status: r.Status, Logs: r.Logs, Calls: make([]ViewCall, 0, len(r.Calls)),
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, Links: Links{Self: RunPath(r.ID)},
	}
	if v.Logs == nil {
		v.Logs = []string{}
	}
	switch r.Status {
	case codemode.StatusSucceeded:
		if len(r.Result) > 0 {
			v.Result = r.Result
		}
	case codemode.StatusFailed, codemode.StatusOutcomeUnknown:
		v.Error = &ViewError{Kind: r.ErrorKind, Message: r.ErrorMessage}
	}
	for _, c := range r.Calls {
		vc := ViewCall{Seq: c.Seq, Tool: c.Tool, Status: c.Status, Replayed: c.Replayed, StartedAt: c.StartedAt}
		if c.FinishedAt > 0 {
			f := c.FinishedAt
			vc.FinishedAt = &f
		}
		v.Calls = append(v.Calls, vc)
	}
	if IsHeld(r.Status) && r.Held != nil {
		h := &ViewHeld{Seq: r.Held.Seq, Tool: r.Held.Tool, Status: r.Held.Status, Draft: r.Held.Draft, Message: r.Held.Message}
		if r.Held.ApprovalID != "" {
			id := r.Held.ApprovalID
			h.ApprovalID = &id
		}
		v.Held = h
		v.Links.Resume = RunPath(r.ID) + "/resume"
	}
	return v
}

// StartRequest starts a run.
type StartRequest struct {
	// AgentID is the caller every nested call runs as.
	AgentID string
	// SessionID is the verified client session (a session token's), or "".
	SessionID string
	Code      string
	Reason    string
	// IdempotencyKey is optional, scoped to the agent.
	IdempotencyKey string
}

// Start validates req and starts a run, or returns the agent's existing
// run for the same Idempotency-Key, script and reason (created false).
// ctx carries the request's values (the actor.Raiser for the audit); the
// run itself continues after ctx ends. codemode.ErrBusy means no slot
// came free and nothing was stored.
func (s *Service) Start(ctx context.Context, req StartRequest) (run *Run, created bool, err error) {
	if req.AgentID == "" {
		return nil, false, &InputError{Field: "agent", Message: "an agent is required"}
	}
	reason, err := checkReason(req.Reason)
	if err != nil {
		return nil, false, err
	}
	if err := checkCode(req.Code); err != nil {
		return nil, false, err
	}
	if err := CheckIdempotencyKey(req.IdempotencyKey); err != nil {
		return nil, false, err
	}
	fp := fingerprint(req.Code, reason)
	if req.IdempotencyKey != "" {
		r, err := s.replay(ctx, req.AgentID, req.IdempotencyKey, fp)
		if err == nil || !errors.Is(err, ErrNotFound) {
			return r, false, err
		}
	}
	if s.engine == nil {
		return nil, false, ErrUnavailable
	}
	runCtx, ok := s.engine.RunContext(context.WithoutCancel(ctx), req.AgentID, req.SessionID)
	if !ok {
		return nil, false, ErrDraining
	}
	rt := s.engine.Runner()
	if rt == nil {
		return nil, false, ErrUnavailable
	}
	slot, err := rt.AcquireSlot(ctx)
	if err != nil {
		return nil, false, err
	}
	now := s.now().UnixMilli()
	run = &Run{
		ID: newRunID(), AgentID: req.AgentID, SessionID: req.SessionID, IdempotencyKey: req.IdempotencyKey,
		Fingerprint: fp, Reason: reason, Status: StatusQueued, CreatedAt: now, UpdatedAt: now, code: req.Code,
	}
	if err := s.insertRun(ctx, run); err != nil {
		slot.Release()
		// Another request with the same key committed first: answer as a
		// sequential retry would.
		if req.IdempotencyKey != "" && store.IsUniqueViolation(err) {
			r, rerr := s.replay(ctx, req.AgentID, req.IdempotencyKey, fp)
			if rerr == nil || errors.Is(rerr, ErrIdempotencyKeyReused) {
				return r, false, rerr
			}
		}
		return nil, false, err
	}
	s.launch(runCtx, rt, slot, run, newHooks(s, run.ID, nil), true)
	return run, true, nil
}

// ResumeRequest resumes a held run.
type ResumeRequest struct {
	AgentID   string
	SessionID string
	RunID     string
	// RequestID is the approved Inbox request covering the held call; ""
	// resumes a deferred legacy approval with its approval id.
	RequestID string
}

// Resume runs a permission_required or deferred run again from the top:
// the calls before the held one are answered from the call record, the
// held call and later ones carry the request's grants (or the legacy
// approval id). ErrNotHeld (with the run) when the run is not held.
func (s *Service) Resume(ctx context.Context, req ResumeRequest) (*Run, error) {
	run, err := s.loadRun(ctx, req.AgentID, req.RunID, true)
	if err != nil {
		return nil, err
	}
	if !IsHeld(run.Status) || run.Held == nil {
		return run, ErrNotHeld
	}
	rs := &resumeState{heldSeq: run.Held.Seq}
	switch {
	case req.RequestID != "":
		if s.grants == nil {
			return nil, ErrUnavailable
		}
		views, err := s.grants.Status(ctx, req.AgentID, []string{req.RequestID})
		if err != nil {
			return nil, err
		}
		if len(views) != 1 || views[0].Status == "not_found" {
			return nil, &InputError{Field: "request_id", Message: "no Inbox request with this id belongs to this agent"}
		}
		var params map[string]map[string]inbox.Constraint // by call id
		if gr, ok := s.grants.(grantRequests); ok {
			if r, err := gr.Get(ctx, req.RequestID); err == nil && r != nil && r.AgentID == req.AgentID {
				params = make(map[string]map[string]inbox.Constraint, len(r.Tools))
				for _, t := range r.Tools {
					params[t.CallID] = t.Params
				}
			}
		}
		for _, t := range views[0].Tools {
			if t.Grant == "" {
				continue
			}
			g := grantToken{tool: t.Tool, token: t.Grant}
			if t.Narrowed {
				g.params, g.matchParams = t.Params, true
			} else if p, ok := params[t.CallID]; ok {
				g.params, g.matchParams = p, true
			}
			rs.grants = append(rs.grants, g)
		}
	case run.Held.Status == codemode.StatusDeferred && run.Held.ApprovalID != "":
		rs.approvalID = run.Held.ApprovalID
	default:
		return nil, &InputError{Field: "request_id", Message: "request_id is required: submit held.draft as an Inbox request (toolyard.inbox_request), then resume with its request_id once the owner decided"}
	}
	calls, err := s.loadCalls(ctx, run.ID)
	if err != nil {
		return nil, err
	}
	rs.recorded = make(map[int]Call, len(calls))
	for _, c := range calls {
		rs.recorded[c.Seq] = c
	}
	if s.engine == nil {
		return nil, ErrUnavailable
	}
	runCtx, ok := s.engine.RunContext(context.WithoutCancel(ctx), req.AgentID, req.SessionID)
	if !ok {
		return nil, ErrDraining
	}
	rt := s.engine.Runner()
	if rt == nil {
		return nil, ErrUnavailable
	}
	slot, err := rt.AcquireSlot(ctx)
	if err != nil {
		return nil, err
	}
	claimed, err := s.claimResume(ctx, run)
	if err != nil || !claimed {
		slot.Release()
		if err != nil {
			return nil, err
		}
		// Another resume took it first.
		cur, lerr := s.loadRun(ctx, req.AgentID, req.RunID, false)
		if lerr != nil {
			return nil, lerr
		}
		return cur, ErrNotHeld
	}
	s.launch(runCtx, rt, slot, run, newHooks(s, run.ID, rs), false)
	return run, nil
}

// Get returns the agent's run. With wait > 0 it waits up to wait (at most
// MaxWait) for the run to leave queued and running, and answers early
// when ctx ends or ReleaseWaiters is called.
func (s *Service) Get(ctx context.Context, agentID, runID string, wait time.Duration) (*Run, error) {
	wait = min(wait, MaxWait)
	ch := s.notify.subscribe(runID)
	run, err := s.loadRun(ctx, agentID, runID, false)
	if err != nil || !InProgress(run.Status) || wait <= 0 {
		// A run that is final already gets no more notices: drop the
		// channel this call made (a running one keeps it for its notice).
		if err != nil || !InProgress(run.Status) {
			s.notify.forget(runID, ch)
		}
		return run, err
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	poll := time.NewTicker(s.pollEvery)
	defer poll.Stop()
	for {
		select {
		case <-ctx.Done():
			return run, nil
		case <-s.released:
			return s.reload(ctx, run)
		case <-timer.C:
			return s.reload(ctx, run)
		case <-ch:
			ch = s.notify.subscribe(runID)
		case <-poll.C:
		}
		cur, err := s.loadRun(ctx, agentID, runID, false)
		if err != nil {
			if ctx.Err() != nil {
				return run, nil
			}
			return nil, err
		}
		run = cur
		if !InProgress(run.Status) {
			s.notify.forget(runID, ch)
			return run, nil
		}
	}
}

// reload rereads run, keeping the last state when ctx has ended.
func (s *Service) reload(ctx context.Context, run *Run) (*Run, error) {
	cur, err := s.loadRun(ctx, run.AgentID, run.ID, false)
	if err != nil {
		if ctx.Err() != nil {
			return run, nil
		}
		return nil, err
	}
	return cur, nil
}

// launch runs run's script on a goroutine of its own.
func (s *Service) launch(base context.Context, rt Runner, slot *codemode.Slot, run *Run, h *hooks, fromQueued bool) {
	ctx, cancel := context.WithCancel(base)
	s.mu.Lock()
	s.inflight[run.ID] = cancel
	s.mu.Unlock()
	go s.execute(ctx, rt, slot, run, h, fromQueued)
}

// execute runs the script and records its outcome.
func (s *Service) execute(ctx context.Context, rt Runner, slot *codemode.Slot, run *Run, h *hooks, fromQueued bool) {
	defer s.untrack(run.ID)
	var rr *codemode.RunResult
	func() {
		defer func() {
			if r := recover(); r != nil {
				s.reportPanic(r)
				rr = &codemode.RunResult{Status: codemode.StatusFailed, ErrorKind: codemode.ErrorInternal,
					ErrorMessage: "the run stopped on an internal error in toolyard"}
			}
		}()
		if fromQueued {
			if err := s.markRunning(ctx, run.ID); err != nil {
				s.log.Warn("code run: could not mark running", "run_id", run.ID, "err", err.Error())
			}
		}
		res, err := rt.Run(ctx, run.code, codemode.RunOptions{Reason: run.Reason, Hooks: h, Slot: slot})
		switch {
		case err != nil:
			kind := codemode.ErrorInternal
			if errors.Is(err, codemode.ErrBusy) {
				kind = codemode.ErrorBusy
			}
			rr = &codemode.RunResult{Status: codemode.StatusFailed, ErrorKind: kind, ErrorMessage: err.Error()}
		case res == nil:
			rr = &codemode.RunResult{Status: codemode.StatusFailed, ErrorKind: codemode.ErrorInternal,
				ErrorMessage: "the script runner returned no result"}
		default:
			rr = res
		}
	}()
	slot.Release()
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := s.finish(wctx, run, rr, h); err != nil {
		s.log.Error("code run: could not record the outcome; the run reads running until the next restart marks it outcome_unknown",
			"run_id", run.ID, "status", rr.Status, "err", err.Error())
	}
	s.notify.notify(run.ID)
}

func (s *Service) untrack(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cancel, ok := s.inflight[id]; ok {
		cancel()
		delete(s.inflight, id)
	}
	close(s.changed)
	s.changed = make(chan struct{})
}

// ReleaseWaiters makes every waiting Get answer now, with the run's
// current state (shutdown). Later Gets do not wait.
func (s *Service) ReleaseWaiters() {
	s.releaseOnce.Do(func() { close(s.released) })
}

// Wait returns once no run goroutine of this process is left. When ctx
// ends first it cancels the runs still going (their in-flight calls are
// recorded outcome_unknown) and gives them up to two seconds to record
// their outcome.
func (s *Service) Wait(ctx context.Context) {
	if s.waitIdle(ctx.Done()) {
		return
	}
	s.mu.Lock()
	for _, cancel := range s.inflight {
		cancel()
	}
	n := len(s.inflight)
	s.mu.Unlock()
	if n > 0 {
		s.log.Warn("code run: runs still going at the drain deadline were stopped", "count", n)
	}
	grace, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s.waitIdle(grace.Done())
}

// waitIdle reports whether every run goroutine ended before stop closed.
func (s *Service) waitIdle(stop <-chan struct{}) bool {
	for {
		s.mu.Lock()
		n, ch := len(s.inflight), s.changed
		s.mu.Unlock()
		if n == 0 {
			return true
		}
		select {
		case <-ch:
		case <-stop:
			return false
		}
	}
}

// reportPanic counts, dumps and logs a panic a run goroutine recovered.
func (s *Service) reportPanic(r any) { goroutines.Report(goroutineName, r) }

// checkReason trims a reason and checks its length.
func checkReason(reason string) (string, error) {
	r := strings.TrimSpace(reason)
	switch {
	case len(r) < MinReasonLen || len(r) > MaxReasonLen:
		return "", &InputError{Field: "reason", Message: fmt.Sprintf("reason must be %d-%d characters (got %d)", MinReasonLen, MaxReasonLen, len(r))}
	case strings.ContainsRune(r, 0):
		return "", &InputError{Field: "reason", Message: "reason must not contain a NUL character"}
	case !utf8.ValidString(r):
		return "", &InputError{Field: "reason", Message: "reason must be valid UTF-8"}
	}
	return r, nil
}

// checkCode checks a script's size and content.
func checkCode(code string) error {
	switch {
	case len(code) > MaxScriptBytes:
		return ErrTooLarge
	case strings.TrimSpace(code) == "":
		return &InputError{Field: "code", Message: "the script is empty"}
	case strings.ContainsRune(code, 0):
		return &InputError{Field: "code", Message: "the script must not contain a NUL character"}
	case grantTokenRE.MatchString(code):
		return &InputError{Field: "code", Message: "the script contains an Inbox grant token (tyg_…); scripts are stored, so never put a token in one: resume the held run with its request_id instead"}
	}
	return nil
}

// CheckIdempotencyKey checks an Idempotency-Key: "" (none) or 1-128
// printable ASCII characters.
func CheckIdempotencyKey(key string) error {
	if len(key) > MaxIdempotencyKeyLen {
		return &InputError{Field: "Idempotency-Key", Message: "must be 1-128 printable ASCII characters"}
	}
	for i := 0; i < len(key); i++ {
		if c := key[i]; c < 0x20 || c > 0x7e {
			return &InputError{Field: "Idempotency-Key", Message: "must be 1-128 printable ASCII characters"}
		}
	}
	return nil
}

// fingerprint identifies a script and reason for an Idempotency-Key.
func fingerprint(code, reason string) string {
	h := sha256.New()
	fmt.Fprintf(h, "%d:", len(code))
	h.Write([]byte(code))
	h.Write([]byte(reason))
	return hex.EncodeToString(h.Sum(nil))
}

// argsHash is the sha256 of the canonical JSON of a call's script
// arguments: keys sorted, and every number as float64 (a worker hands
// numbers over as float64, so the same arguments hash the same whichever
// way they arrived).
func argsHash(args map[string]any) string {
	raw, err := json.Marshal(args)
	if err == nil {
		var v any
		if err = json.Unmarshal(raw, &v); err == nil {
			raw, err = json.Marshal(v)
		}
	}
	if err != nil {
		raw = []byte(fmt.Sprintf("unencodable:%v", args))
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// newRunID returns "cr_" and 32 hex characters.
func newRunID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("coderuns: no randomness: %v", err))
	}
	return "cr_" + hex.EncodeToString(b[:])
}

// cleanText makes s storable in a text column on both engines: valid
// UTF-8 without NUL.
func cleanText(s string) string {
	s = strings.ToValidUTF8(s, "�")
	return strings.ReplaceAll(s, "\x00", "�")
}
