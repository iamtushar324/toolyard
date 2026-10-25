// Package jobs runs tool calls in the background as durable jobs: the
// async tools of v0.1 (docs/guidelines/async-tools.md).
//
// A job is a row in tool_jobs. Start stores it queued and runs the call on
// a goroutine that outlives the request; the caller hands the agent the job
// id at once (or, for a tool that is async only after a while, waits that
// long first). Get, List and WaitFor read jobs, Result reads a finished
// job's encoded result (inline, or in the blob store when large), Cancel
// stops one, and every final status is written in one transaction with the
// Notifier's event (job.completed / job.failed callbacks).
//
// High availability: every row carries owner_instance and lease_until.
// The process that runs a job renews its lease while the job is queued or
// running (the heartbeat), and claims it queued -> running with a
// compare-and-set. Recover takes only rows whose lease ended: a running
// job becomes outcome_unknown (its call may have run, so it is never run
// again) and a queued one failed (nothing ran). Waiters are woken in
// process and fall back to a database poll, so a job that runs on another
// replica is still seen.
package jobs

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/tusharbhardwaj/toolyard/internal/blobstore"
	"github.com/tusharbhardwaj/toolyard/internal/goroutines"
	"github.com/tusharbhardwaj/toolyard/internal/logx"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// Job statuses.
const (
	// StatusQueued: stored; the call has not started.
	StatusQueued = "queued"
	// StatusRunning: the call is running.
	StatusRunning = "running"
	// StatusSucceeded: the call returned a result.
	StatusSucceeded = "succeeded"
	// StatusFailed: the call returned an error result, or never started.
	StatusFailed = "failed"
	// StatusCancelled: jobs.cancel stopped it (a running write may already
	// have taken effect).
	StatusCancelled = "cancelled"
	// StatusOutcomeUnknown: the call may or may not have run (its server
	// stopped, or the upstream failed after dispatch). Never repeated.
	StatusOutcomeUnknown = "outcome_unknown"
)

// Final reports a status that never changes again.
func Final(status string) bool {
	switch status {
	case StatusSucceeded, StatusFailed, StatusCancelled, StatusOutcomeUnknown:
		return true
	}
	return false
}

// Defaults and limits.
const (
	DefaultLeaseTTL       = 30 * time.Second
	DefaultRetention      = 7 * 24 * time.Hour
	DefaultInlineLimit    = 256 << 10
	DefaultMaxResultBytes = 32 << 20
	DefaultMaxRunning     = 64
	DefaultMaxActive      = 32
	DefaultMaxDuration    = time.Hour
	// MaxWait caps one jobs.wait or Prefer: wait.
	MaxWait = 60 * time.Second
	// MaxCallbackRefLen and MaxIdempotencyKeyLen cap those inputs.
	MaxCallbackRefLen    = 512
	MaxIdempotencyKeyLen = 128
	// maxReasonLen and maxProgressMessage cap stored text.
	maxReasonLen       = 2000
	maxProgressMessage = 500
	// progressEvery is the most often one job's progress is written.
	progressEvery = time.Second
)

// Error texts of the statuses toolyard sets itself.
const (
	msgCancelledBeforeStart = "cancelled before it started; nothing ran"
	msgCancelledRunning     = "cancelled while running: the upstream call was interrupted, and a write may already have taken effect; check the target before you call it again"
	msgLostBeforeStart      = "the toolyard server that accepted this job stopped before the job started; nothing ran; call the tool again"
	msgLostRunning          = "the toolyard server that ran this job stopped while the call was running; the call may or may not have run; check the target before any new call"
	msgStoppedBeforeStart   = "toolyard stopped before this job started; nothing ran; call the tool again"
)

// Errors the service returns.
var (
	ErrNotFound             = errors.New("job not found")
	ErrIdempotencyKeyReused = errors.New("idempotency_key_reused")
	ErrTooManyActive        = errors.New("too many queued or running jobs for this agent; wait for some to finish or cancel them")
	ErrDraining             = errors.New("toolyard is restarting: no job was started; try again in a moment")
)

// InputError is bad input in a start request.
type InputError struct {
	Field   string
	Message string
}

func (e *InputError) Error() string { return e.Field + ": " + e.Message }

// Notifier is told, inside the transaction that records a job's final
// status, that the job finished. The orchestrator wires it to the
// callbacks service: callbacks.EmitForAgentTx(ctx, tx, job.AgentID,
// job.CallbackRef, CallbackEvent(job)), which uses the agent's default
// receiver when the job has no callback_ref and does nothing when there is
// none. It is called for every finished job; only an error it returns is
// recorded: the event is rolled back, the status written without it, and
// the error kept in callback_error.
type Notifier interface {
	JobFinished(ctx context.Context, tx *sql.Tx, job Job) error
}

// Callback event types of a finished job.
const (
	EventJobCompleted = "job.completed"
	EventJobFailed    = "job.failed"
)

// CallbackData is the data of a job.completed / job.failed event. It never
// carries the result itself: the receiver reads it with jobs.result or GET
// result_ref under the agent's own token.
type CallbackData struct {
	JobID       string `json:"job_id"`
	Tool        string `json:"tool"`
	Upstream    string `json:"upstream"`
	Status      string `json:"status"`
	IsError     bool   `json:"is_error"`
	ResultRef   string `json:"result_ref,omitempty"`
	ResultBytes int    `json:"result_bytes"`
	Error       string `json:"error,omitempty"`
	CallbackRef string `json:"callback_ref,omitempty"`
	FinishedAt  int64  `json:"finished_at"`
}

// CallbackEvent is the event a finished job sends: job.completed when it
// succeeded, job.failed for failed, cancelled and outcome_unknown. A
// Notifier wired to the callbacks service passes it on as it is:
//
//	ev, data := jobs.CallbackEvent(job)
//	return callbacks.EmitForAgentTx(ctx, tx, job.AgentID, job.CallbackRef, ev, data)
func CallbackEvent(j Job) (string, CallbackData) {
	ev := EventJobFailed
	if j.Status == StatusSucceeded {
		ev = EventJobCompleted
	}
	d := CallbackData{
		JobID: j.ID, Tool: j.Tool, Upstream: j.Upstream, Status: j.Status, IsError: j.ResultIsError,
		ResultBytes: j.ResultBytes, Error: j.Error, CallbackRef: j.CallbackRef, FinishedAt: j.FinishedAt,
	}
	if j.HasResult() {
		d.ResultRef = "/v1/jobs/" + j.ID + "/result"
	}
	return ev, d
}

// NopNotifier sends nothing.
type NopNotifier struct{}

// JobFinished does nothing.
func (NopNotifier) JobFinished(context.Context, *sql.Tx, Job) error { return nil }

// Leader runs fn while this process is the one node that should (the
// retention purge). The orchestrator wires it to cluster.RunLeader. It
// blocks until ctx ends.
type Leader interface {
	RunLeader(ctx context.Context, name string, fn func(ctx context.Context) error)
}

// DirectLeader runs fn here, as on a single node: again after it returns,
// until ctx ends.
type DirectLeader struct{}

// RunLeader runs fn until ctx ends.
func (DirectLeader) RunLeader(ctx context.Context, _ string, fn func(ctx context.Context) error) {
	for ctx.Err() == nil {
		_ = fn(ctx)
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
		}
	}
}

// Progress is the last progress notification of a job's call.
type Progress struct {
	Progress float64  `json:"progress"`
	Total    *float64 `json:"total,omitempty"`
	Message  string   `json:"message,omitempty"`
	// At is when it arrived, Unix milliseconds.
	At int64 `json:"at"`
}

// Job is one stored job.
type Job struct {
	ID             string
	AgentID        string
	OwnerUserID    string
	SessionID      string
	Tool           string
	Upstream       string
	ParamsHash     string
	Reason         string
	Via            string
	Status         string
	Progress       *Progress
	ResultBytes    int
	ResultIsError  bool
	Error          string
	CallbackRef    string
	CallbackError  string
	IdempotencyKey string
	// CancelRequested is set by Cancel on a running job.
	CancelRequested bool
	OwnerInstance   string
	// LeaseUntil, CreatedAt, StartedAt, FinishedAt and UpdatedAt are Unix
	// milliseconds; StartedAt and FinishedAt are 0 until set.
	LeaseUntil int64
	CreatedAt  int64
	StartedAt  int64
	FinishedAt int64
	UpdatedAt  int64

	// resultBlob is the blob store sha of a large result ("" when inline).
	resultBlob string
}

// Final reports whether the job's status never changes again.
func (j *Job) Final() bool { return Final(j.Status) }

// HasResult reports whether a finished job kept an encoded result.
func (j *Job) HasResult() bool { return j.ResultBytes > 0 }

// ResultInBlobStore reports whether the result is in the blob store.
func (j *Job) ResultInBlobStore() bool { return j.resultBlob != "" }

// Outcome is how a job's call ended, as the RunFunc reports it.
type Outcome struct {
	// Status is succeeded, failed, cancelled or outcome_unknown.
	Status string
	// Envelope is the encoded tool result ("" when there is none). The
	// gateway encodes and decodes it; this package only stores it.
	Envelope string
	IsError  bool
	// Error explains a status other than succeeded.
	Error string
}

// ProgressFunc records a progress notification for the job's call.
type ProgressFunc func(progress float64, total *float64, message string)

// RunFunc runs the job's call on ctx (cancelled by Cancel, by the job's
// time limit and at shutdown) and reports how it ended.
type RunFunc func(ctx context.Context, progress ProgressFunc) Outcome

// StartRequest describes a job to start.
type StartRequest struct {
	AgentID     string
	OwnerUserID string
	SessionID   string
	Tool        string
	Upstream    string
	ParamsHash  string
	Reason      string
	Via         string
	CallbackRef string
	// IdempotencyKey is optional, scoped to the agent.
	IdempotencyKey string
	// InlineWait says the caller may still deliver the result inline
	// (Handle.WaitInline): the callback is then not sent. Without it the
	// caller hands the job id back at once and the callback always goes.
	InlineWait bool
}

// Options configure a Service. Zero values pick the defaults.
type Options struct {
	// InstanceID names this process in owner_instance (cluster.InstanceID
	// once slice A lands). Default: host name, process id and random hex.
	InstanceID string
	Notifier   Notifier
	Leader     Leader
	// Blobs keeps results over InlineLimit. nil keeps every result inline.
	Blobs blobstore.Store
	// LeaseTTL is how long a job's lease lasts without a renewal.
	LeaseTTL time.Duration
	// PollEvery is how often a waiter rereads the database besides the
	// in-process notice.
	PollEvery time.Duration
	// Retention is how long a finished job is kept.
	Retention time.Duration
	// InlineLimit is the largest result stored in the row itself.
	InlineLimit int
	// MaxResultBytes is the largest result kept at all.
	MaxResultBytes int
	// MaxRunning caps the jobs this process runs at once; the rest wait
	// queued (their leases still renewed).
	MaxRunning int
	// MaxActive caps one agent's queued and running jobs.
	MaxActive int
	// MaxDuration is a job's time limit.
	MaxDuration time.Duration
	// Now is the clock (tests).
	Now func() time.Time
}

// goroutine and loop names (metric labels).
const (
	goroutineName = "tool-job"
	loopHeartbeat = "tool-jobs-heartbeat"
	loopRecover   = "tool-jobs-recover"
	leaderPurge   = "tool-jobs-retention"
)

// Service starts, reads, cancels, recovers and purges jobs.
type Service struct {
	db          *store.DB
	instance    string
	notifier    Notifier
	leader      Leader
	blobs       blobstore.Store
	leaseTTL    time.Duration
	pollEvery   time.Duration
	retention   time.Duration
	inlineLimit int
	maxResult   int
	maxActive   int
	maxDuration time.Duration
	now         func() time.Time
	log         *slog.Logger
	slots       chan struct{}

	mu sync.Mutex
	// inflight is every job this process runs, by id.
	inflight map[string]*run
	// idle is closed and replaced whenever a job goroutine ends.
	idle chan struct{}
	// changed is closed and replaced whenever a job of this process
	// changes state, so waiters reread.
	changed chan struct{}

	released    chan struct{}
	releaseOnce sync.Once
	draining    atomic.Bool
}

// run is one job goroutine of this process.
type run struct {
	cancel     context.CancelFunc
	handle     *Handle
	userCancel atomic.Bool

	mu              sync.Mutex
	progressWritten time.Time
	pendingProgress *Progress
}

// New returns a Service over db.
func New(db *store.DB, opts Options) *Service {
	s := &Service{
		db: db, instance: opts.InstanceID, notifier: opts.Notifier, leader: opts.Leader, blobs: opts.Blobs,
		leaseTTL: opts.LeaseTTL, pollEvery: opts.PollEvery, retention: opts.Retention,
		inlineLimit: opts.InlineLimit, maxResult: opts.MaxResultBytes, maxActive: opts.MaxActive,
		maxDuration: opts.MaxDuration, now: opts.Now, log: logx.For("jobs"),
		inflight: map[string]*run{}, idle: make(chan struct{}), changed: make(chan struct{}), released: make(chan struct{}),
	}
	if s.instance == "" {
		s.instance = defaultInstanceID()
	}
	if s.notifier == nil {
		s.notifier = NopNotifier{}
	}
	if s.leader == nil {
		s.leader = DirectLeader{}
	}
	if s.leaseTTL <= 0 {
		s.leaseTTL = DefaultLeaseTTL
	}
	if s.pollEvery <= 0 {
		s.pollEvery = time.Second
	}
	if s.retention <= 0 {
		s.retention = DefaultRetention
	}
	if s.inlineLimit <= 0 {
		s.inlineLimit = DefaultInlineLimit
	}
	if s.maxResult <= 0 {
		s.maxResult = DefaultMaxResultBytes
	}
	if s.maxActive <= 0 {
		s.maxActive = DefaultMaxActive
	}
	if s.maxDuration <= 0 {
		s.maxDuration = DefaultMaxDuration
	}
	if s.now == nil {
		s.now = time.Now
	}
	maxRunning := opts.MaxRunning
	if maxRunning <= 0 {
		maxRunning = DefaultMaxRunning
	}
	s.slots = make(chan struct{}, maxRunning)
	return s
}

// InstanceID is this process's owner_instance.
func (s *Service) InstanceID() string { return s.instance }

// Retention is how long finished jobs are kept.
func (s *Service) Retention() time.Duration { return s.retention }

// MaxDuration is a job's time limit.
func (s *Service) MaxDuration() time.Duration { return s.maxDuration }

// PollEvery is how often waiters reread the database.
func (s *Service) PollEvery() time.Duration { return s.pollEvery }

// Handle follows one job this process started.
type Handle struct {
	done    chan struct{}
	state   atomic.Int32
	outcome Outcome
}

const (
	handleWaiting   = 0
	handleHandedOff = 1
	handleInline    = 2
)

func newHandle(inlineWait bool) *Handle {
	h := &Handle{done: make(chan struct{})}
	if !inlineWait {
		h.state.Store(handleHandedOff)
	}
	return h
}

// Done is closed once the job ended and its final status is stored.
func (h *Handle) Done() <-chan struct{} { return h.done }

// Outcome is how the job ended; valid after Done.
func (h *Handle) Outcome() Outcome { return h.outcome }

// WaitInline waits up to d for a job started with InlineWait. When the job
// ends in time it returns the outcome and true: the caller delivers the
// result inline and no callback is sent. Otherwise it hands the job off
// (the callback will be sent) and returns false.
func (h *Handle) WaitInline(ctx context.Context, d time.Duration) (Outcome, bool) {
	if h == nil {
		return Outcome{}, false
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-h.done:
		return h.outcome, h.state.Load() == handleInline
	case <-t.C:
	case <-ctx.Done():
	}
	if h.state.CompareAndSwap(handleWaiting, handleHandedOff) {
		return Outcome{}, false
	}
	// The call ended a moment ago and claimed inline delivery.
	<-h.done
	return h.outcome, h.state.Load() == handleInline
}

func (h *Handle) complete(out Outcome) {
	h.outcome = out
	close(h.done)
}

// Start validates req, stores a queued job and runs fn on a goroutine of
// its own. The goroutine's context carries ctx's values but not its
// cancellation: the job outlives the request. With an IdempotencyKey the
// agent's job for that key is returned instead (with a nil Handle) when its
// tool and params hash match, ErrIdempotencyKeyReused when they do not.
func (s *Service) Start(ctx context.Context, req StartRequest, fn RunFunc) (*Job, *Handle, error) {
	if s.draining.Load() {
		return nil, nil, ErrDraining
	}
	if err := checkStart(&req); err != nil {
		return nil, nil, err
	}
	if req.IdempotencyKey != "" {
		j, err := s.replay(ctx, req)
		if err == nil || !errors.Is(err, ErrNotFound) {
			return j, nil, err
		}
	}
	if req.AgentID != "" {
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tool_jobs WHERE agent_id = ? AND status IN ('queued','running')`, req.AgentID).Scan(&n); err != nil {
			return nil, nil, err
		}
		if n >= s.maxActive {
			return nil, nil, ErrTooManyActive
		}
	}
	job, err := s.insert(ctx, req)
	if err != nil {
		if req.IdempotencyKey != "" && store.IsUniqueViolation(err) {
			j, rerr := s.replay(ctx, req)
			if rerr == nil || errors.Is(rerr, ErrIdempotencyKeyReused) {
				return j, nil, rerr
			}
		}
		return nil, nil, err
	}
	h := newHandle(req.InlineWait)
	runCtx, cancelTimeout := context.WithTimeout(context.WithoutCancel(ctx), s.maxDuration)
	runCtx, cancel := context.WithCancel(runCtx)
	r := &run{handle: h, cancel: func() { cancel(); cancelTimeout() }}
	s.mu.Lock()
	s.inflight[job.ID] = r
	s.mu.Unlock()
	go s.execute(runCtx, job, r, fn)
	return job, h, nil
}

// Record stores a job that is already final: a task-augmented call that
// was refused before it could run still gets a task the client can read.
// The Notifier is told, as for any job.
func (s *Service) Record(ctx context.Context, req StartRequest, out Outcome) (*Job, error) {
	if err := checkStart(&req); err != nil {
		return nil, err
	}
	req.IdempotencyKey = ""
	job, err := s.insert(ctx, req)
	if err != nil {
		return nil, err
	}
	if out.Status == "" || !Final(out.Status) {
		out.Status = StatusFailed
	}
	if _, err := s.settleOutcome(ctx, job.ID, []string{StatusQueued}, out, true); err != nil {
		return nil, err
	}
	s.broadcast()
	return s.Get(ctx, job.ID)
}

// checkStart checks and trims req.
func checkStart(req *StartRequest) error {
	req.Tool = strings.TrimSpace(req.Tool)
	if req.Tool == "" {
		return &InputError{Field: "tool", Message: "a tool is required"}
	}
	req.Reason = clip(cleanText(strings.TrimSpace(req.Reason)), maxReasonLen)
	if req.Via == "" {
		req.Via = "direct"
	}
	if err := checkToken("callback_ref", req.CallbackRef, MaxCallbackRefLen); err != nil {
		return err
	}
	return checkToken("Idempotency-Key", req.IdempotencyKey, MaxIdempotencyKeyLen)
}

// CheckIdempotencyKey checks an Idempotency-Key: "" (none) or 1-128
// printable ASCII characters.
func CheckIdempotencyKey(key string) error {
	return checkToken("Idempotency-Key", key, MaxIdempotencyKeyLen)
}

// CheckCallbackRef checks a callback_ref: "" (none) or 1-512 printable
// ASCII characters.
func CheckCallbackRef(ref string) error { return checkToken("callback_ref", ref, MaxCallbackRefLen) }

func checkToken(field, v string, max int) error {
	if len(v) > max {
		return &InputError{Field: field, Message: fmt.Sprintf("must be at most %d printable ASCII characters", max)}
	}
	for i := 0; i < len(v); i++ {
		if c := v[i]; c < 0x20 || c > 0x7e {
			return &InputError{Field: field, Message: fmt.Sprintf("must be at most %d printable ASCII characters", max)}
		}
	}
	return nil
}

// replay returns the agent's job for req's Idempotency-Key.
func (s *Service) replay(ctx context.Context, req StartRequest) (*Job, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM tool_jobs WHERE agent_id = ? AND idempotency_key = ?`, req.AgentID, req.IdempotencyKey)
	j, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if j.Tool != req.Tool || j.ParamsHash != req.ParamsHash {
		return nil, ErrIdempotencyKeyReused
	}
	return j, nil
}

func (s *Service) insert(ctx context.Context, req StartRequest) (*Job, error) {
	now := s.now().UnixMilli()
	j := &Job{
		ID: newJobID(), AgentID: req.AgentID, OwnerUserID: req.OwnerUserID, SessionID: req.SessionID,
		Tool: req.Tool, Upstream: req.Upstream, ParamsHash: req.ParamsHash, Reason: req.Reason, Via: req.Via,
		Status: StatusQueued, CallbackRef: req.CallbackRef, IdempotencyKey: req.IdempotencyKey,
		OwnerInstance: s.instance, LeaseUntil: now + s.leaseTTL.Milliseconds(), CreatedAt: now, UpdatedAt: now,
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO tool_jobs(id, agent_id, owner_user_id, session_id, tool, upstream,
            params_hash, reason, via, status, callback_ref, idempotency_key, owner_instance, lease_until, created_at, updated_at)
         VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		j.ID, j.AgentID, j.OwnerUserID, nullStr(j.SessionID), j.Tool, j.Upstream,
		j.ParamsHash, j.Reason, j.Via, j.Status, nullStr(j.CallbackRef), nullStr(j.IdempotencyKey),
		j.OwnerInstance, j.LeaseUntil, j.CreatedAt, j.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return j, nil
}

// execute runs one job: it waits for a slot, claims the row, runs the call
// and records the outcome.
func (s *Service) execute(ctx context.Context, job *Job, r *run, fn RunFunc) {
	defer s.untrack(job.ID)
	select {
	case s.slots <- struct{}{}:
	case <-ctx.Done():
		s.settleUnstarted(job.ID, r)
		return
	}
	defer func() { <-s.slots }()
	claimed, err := s.claim(ctx, job.ID)
	if err != nil {
		s.log.Warn("job: could not mark running", "job_id", job.ID, "err", err.Error())
	}
	if !claimed {
		s.settleUnstarted(job.ID, r)
		return
	}
	s.broadcast()
	out := s.call(ctx, r, job.ID, fn)
	if r.userCancel.Load() && out.Status != StatusSucceeded {
		out.Status, out.Error = StatusCancelled, msgCancelledRunning
	}
	inline := r.handle.state.CompareAndSwap(handleWaiting, handleInline)
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if _, err := s.settleOutcome(wctx, job.ID, []string{StatusRunning}, out, !inline); err != nil {
		s.log.Error("job: could not record the outcome; the job reads running until its lease ends and recovery marks it outcome_unknown",
			"job_id", job.ID, "status", out.Status, "err", err.Error())
	}
	r.handle.complete(out)
}

// call runs fn, turning a panic into outcome_unknown.
func (s *Service) call(ctx context.Context, r *run, id string, fn RunFunc) (out Outcome) {
	defer func() {
		if p := recover(); p != nil {
			goroutines.Report(goroutineName, p)
			out = Outcome{Status: StatusOutcomeUnknown, Error: "the job stopped on an internal error in toolyard; the call may have run"}
		}
	}()
	out = fn(ctx, s.progressFunc(id, r))
	switch out.Status {
	case StatusSucceeded, StatusFailed, StatusCancelled, StatusOutcomeUnknown:
	default:
		out.Status = StatusOutcomeUnknown
	}
	if out.Status == StatusOutcomeUnknown && out.Error == "" {
		out.Error = "the call may or may not have run; check the target before any new call"
	}
	return out
}

// settleUnstarted records a job that never started: cancelled while queued
// (Cancel already wrote it), or stopped by shutdown or its time limit.
func (s *Service) settleUnstarted(id string, r *run) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := s.settle(ctx, id, []string{StatusQueued}, "", nil, settlement{status: StatusFailed, errText: msgStoppedBeforeStart}, true); err != nil {
		s.log.Warn("job: could not record a job that never started", "job_id", id, "err", err.Error())
	}
	out := Outcome{Status: StatusFailed, Error: msgStoppedBeforeStart}
	if j, err := s.Get(ctx, id); err == nil && j.Final() {
		out = Outcome{Status: j.Status, Error: j.Error}
	}
	r.handle.state.CompareAndSwap(handleWaiting, handleInline)
	r.handle.complete(out)
}

func (s *Service) claim(ctx context.Context, id string) (bool, error) {
	now := s.now().UnixMilli()
	res, err := s.db.ExecContext(ctx, `UPDATE tool_jobs SET status = 'running', started_at = ?, updated_at = ?, owner_instance = ?, lease_until = ?
         WHERE id = ? AND status = 'queued'`, now, now, s.instance, now+s.leaseTTL.Milliseconds(), id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// settlement is one final status write.
type settlement struct {
	status  string
	inline  string
	blob    string
	bytes   int
	isError bool
	errText string
}

// noticeError is a Notifier failure inside settle.
type noticeError struct{ err error }

func (e *noticeError) Error() string { return e.err.Error() }

// settleOutcome stores out's result (inline or in the blob store) and
// writes the final status.
func (s *Service) settleOutcome(ctx context.Context, id string, from []string, out Outcome, notify bool) (bool, error) {
	st := settlement{status: out.Status, isError: out.IsError, errText: out.Error, bytes: len(out.Envelope)}
	env := out.Envelope
	switch {
	case env == "":
	case len(env) > s.maxResult:
		if st.errText == "" {
			st.errText = fmt.Sprintf("the result (%d bytes) is over the %d byte job limit and was not kept; the call itself finished", len(env), s.maxResult)
		}
		st.bytes = 0
	case len(env) > s.inlineLimit && s.blobs != nil:
		sum := sha256.Sum256([]byte(env))
		sha := hex.EncodeToString(sum[:])
		if _, err := s.blobs.Put(ctx, sha, strings.NewReader(env), int64(len(env))); err != nil {
			s.log.Warn("job: could not store a large result in the blob store; keeping it in the row", "job_id", id, "err", err.Error())
			st.inline = env
		} else {
			st.blob = sha
		}
	default:
		st.inline = env
	}
	ok, err := s.settle(ctx, id, from, "", nil, st, notify)
	if err == nil && !ok {
		s.log.Info("job: already final when its outcome came (recovered meanwhile); the late outcome is not kept", "job_id", id, "status", out.Status)
	}
	return ok, err
}

// settle moves job id from one of the from statuses to st.status, in one
// transaction with the Notifier's event. guard is SQL ANDed to the WHERE
// clause, with guardArgs. It reports whether the row moved.
func (s *Service) settle(ctx context.Context, id string, from []string, guard string, guardArgs []any, st settlement, notify bool) (bool, error) {
	try := func(withNotice bool, noticeErr string) (bool, error) {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return false, err
		}
		defer func() { _ = tx.Rollback() }()
		now := s.now().UnixMilli()
		args := []any{st.status, nullStr(st.inline), nullStr(st.blob), st.bytes, boolInt(st.isError), nullStr(st.errText), nullStr(noticeErr), now, now, id}
		for _, f := range from {
			args = append(args, f)
		}
		args = append(args, guardArgs...)
		res, err := tx.ExecContext(ctx, `UPDATE tool_jobs SET status = ?, result = ?, result_blob = ?, result_bytes = ?, result_is_error = ?,
            error = ?, callback_error = ?, finished_at = ?, updated_at = ?
         WHERE id = ? AND status IN (`+placeholders(len(from))+`)`+guard, args...)
		if err != nil {
			return false, err
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			return false, err
		}
		if withNotice {
			j, err := getJob(ctx, tx, id)
			if err == nil {
				err = s.notifier.JobFinished(ctx, tx, *j)
			}
			if err != nil {
				return false, &noticeError{err: err}
			}
		}
		return true, tx.Commit()
	}
	ok, err := try(notify, "")
	var ne *noticeError
	if errors.As(err, &ne) {
		s.log.Warn("job: the finish event could not be queued; recording the status without it", "job_id", id, "err", ne.err.Error())
		ok, err = try(false, clip(cleanText(ne.err.Error()), 500))
	}
	if ok {
		s.broadcast()
	}
	return ok, err
}

// Get returns the job with id (any agent's: callers scope it).
func (s *Service) Get(ctx context.Context, id string) (*Job, error) {
	return getJob(ctx, s.db, id)
}

func getJob(ctx context.Context, q store.Querier, id string) (*Job, error) {
	j, err := scanJob(q.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM tool_jobs WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return j, err
}

// Result returns the encoded result of a finished job ("" when it kept
// none).
func (s *Service) Result(ctx context.Context, j *Job) (string, error) {
	if j == nil || !j.HasResult() {
		return "", nil
	}
	if j.resultBlob != "" {
		if s.blobs == nil {
			return "", errors.New("the result is in the blob store, which this server has not configured")
		}
		b, err := s.blobs.Open(ctx, j.resultBlob)
		if err != nil {
			return "", err
		}
		defer b.Close()
		data, err := io.ReadAll(io.LimitReader(b, int64(s.maxResult)+1))
		return string(data), err
	}
	var res sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT result FROM tool_jobs WHERE id = ?`, j.ID).Scan(&res); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", err
	}
	return res.String, nil
}

// Filter selects jobs for List. Every non-empty field narrows; with none
// of AgentID, OwnerUserID and All set, List returns nothing.
type Filter struct {
	AgentID     string
	OwnerUserID string
	All         bool
	// Status is one status, "active" (queued or running) or "" for all.
	Status string
	Limit  int
	// BeforeCreated and BeforeID page backwards: jobs created before that
	// job (newest first).
	BeforeCreated int64
	BeforeID      string
}

// List returns jobs newest first.
func (s *Service) List(ctx context.Context, f Filter) ([]*Job, error) {
	var where []string
	var args []any
	if f.AgentID != "" {
		where, args = append(where, "agent_id = ?"), append(args, f.AgentID)
	}
	if f.OwnerUserID != "" {
		where, args = append(where, "owner_user_id = ?"), append(args, f.OwnerUserID)
	}
	if len(where) == 0 && !f.All {
		return nil, nil
	}
	switch f.Status {
	case "":
	case "active":
		where = append(where, "status IN ('queued','running')")
	default:
		where, args = append(where, "status = ?"), append(args, f.Status)
	}
	if f.BeforeCreated > 0 {
		where = append(where, "(created_at < ? OR (created_at = ? AND id < ?))")
		args = append(args, f.BeforeCreated, f.BeforeCreated, f.BeforeID)
	}
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	q := `SELECT ` + jobColumns + ` FROM tool_jobs`
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, " AND ")
	}
	q += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// WaitFor waits up to timeout for the jobs to be final: every one of them,
// or with anyOne set the first. It returns their latest state in the order of
// ids (nil for an id that is not stored) and whether the time ran out (or
// ctx ended, or ReleaseWaiters was called) before the condition held.
func (s *Service) WaitFor(ctx context.Context, ids []string, anyOne bool, timeout time.Duration) ([]*Job, bool, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	poll := time.NewTicker(s.pollEvery)
	defer poll.Stop()
	for {
		ch := s.changedCh()
		jobs := make([]*Job, len(ids))
		final, all := false, true
		for i, id := range ids {
			j, err := s.Get(ctx, id)
			switch {
			case errors.Is(err, ErrNotFound):
				final = true
				continue
			case err != nil:
				if ctx.Err() != nil {
					return jobs, true, nil
				}
				return nil, false, err
			}
			jobs[i] = j
			if j.Final() {
				final = true
			} else {
				all = false
			}
		}
		if (anyOne && final) || (!anyOne && all) {
			return jobs, false, nil
		}
		select {
		case <-ch:
		case <-poll.C:
		case <-deadline.C:
			return jobs, true, nil
		case <-ctx.Done():
			return jobs, true, nil
		case <-s.released:
			return jobs, true, nil
		}
	}
}

// Cancel stops job id: a queued job is cancelled at once (nothing ran); a
// running one is asked to stop (on whichever replica runs it) and ends
// cancelled unless its call finished first. cancelled is false when the
// job was already final.
func (s *Service) Cancel(ctx context.Context, id string) (job *Job, cancelled bool, err error) {
	for attempt := 0; attempt < 3; attempt++ {
		j, err := s.Get(ctx, id)
		if err != nil {
			return nil, false, err
		}
		switch j.Status {
		case StatusQueued:
			ok, err := s.settle(ctx, id, []string{StatusQueued}, "", nil, settlement{status: StatusCancelled, errText: msgCancelledBeforeStart}, true)
			if err != nil {
				return nil, false, err
			}
			if !ok {
				continue // it started meanwhile
			}
			s.cancelLocal(id, true)
		case StatusRunning:
			res, err := s.db.ExecContext(ctx, `UPDATE tool_jobs SET cancel_requested = 1, updated_at = ? WHERE id = ? AND status = 'running'`,
				s.now().UnixMilli(), id)
			if err != nil {
				return nil, false, err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				continue // it finished meanwhile
			}
			s.cancelLocal(id, true)
		default:
			return j, false, nil
		}
		j, err = s.Get(ctx, id)
		return j, err == nil, err
	}
	j, err := s.Get(ctx, id)
	return j, false, err
}

// cancelLocal cancels this process's goroutine of job id, if it runs here.
func (s *Service) cancelLocal(id string, byUser bool) {
	s.mu.Lock()
	r := s.inflight[id]
	s.mu.Unlock()
	if r == nil {
		return
	}
	if byUser {
		r.userCancel.Store(true)
	}
	r.cancel()
}

// Run starts the background loops until ctx ends. The heartbeat (lease
// renewal, cancel requests, progress) and recovery are safe on every node:
// each node renews only its own rows, and recovery takes only rows whose
// lease ended, with a compare-and-set. The retention purge runs under the
// Leader.
func (s *Service) Run(ctx context.Context) {
	goroutines.Loop(ctx, loopHeartbeat, time.Minute, func(ctx context.Context) {
		every(ctx, s.heartbeatEvery(), s.Heartbeat)
	})
	goroutines.Loop(ctx, loopRecover, time.Minute, func(ctx context.Context) {
		every(ctx, max(s.leaseTTL/2, 20*time.Millisecond), func(ctx context.Context) {
			if _, _, err := s.Recover(ctx); err != nil && ctx.Err() == nil {
				s.log.Warn("job recovery", "err", err.Error())
			}
		})
	})
	goroutines.Go(leaderPurge, func() {
		s.leader.RunLeader(ctx, leaderPurge, func(ctx context.Context) error {
			every(ctx, time.Hour, func(ctx context.Context) {
				if n, err := s.Purge(ctx); err != nil && ctx.Err() == nil {
					s.log.Warn("job retention", "err", err.Error())
				} else if n > 0 {
					s.log.Info("job retention: removed finished jobs", "count", n)
				}
			})
			return ctx.Err()
		})
	})
}

func (s *Service) heartbeatEvery() time.Duration {
	return max(s.leaseTTL/6, 50*time.Millisecond)
}

// every runs fn now and then every d until ctx ends.
func every(ctx context.Context, d time.Duration, fn func(context.Context)) {
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		fn(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Heartbeat renews the leases of this process's jobs, applies cancel
// requests other replicas wrote, and writes pending progress.
func (s *Service) Heartbeat(ctx context.Context) {
	s.mu.Lock()
	ids := make([]string, 0, len(s.inflight))
	runs := make(map[string]*run, len(s.inflight))
	for id, r := range s.inflight {
		ids = append(ids, id)
		runs[id] = r
	}
	s.mu.Unlock()
	if len(ids) == 0 {
		return
	}
	lease := s.now().Add(s.leaseTTL).UnixMilli()
	var cancelIDs []string
	for start := 0; start < len(ids); start += 200 {
		batch := ids[start:min(start+200, len(ids))]
		args := []any{lease, s.instance}
		for _, id := range batch {
			args = append(args, id)
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE tool_jobs SET lease_until = ?
             WHERE owner_instance = ? AND status IN ('queued','running') AND id IN (`+placeholders(len(batch))+`)`, args...); err != nil {
			if ctx.Err() == nil {
				s.log.Warn("job heartbeat: could not renew leases", "err", err.Error())
			}
			return
		}
		asked, err := s.queryIDs(ctx, `SELECT id FROM tool_jobs WHERE cancel_requested = 1 AND status = 'running' AND id IN (`+placeholders(len(batch))+`)`, args[2:]...)
		if err == nil {
			cancelIDs = append(cancelIDs, asked...)
		}
	}
	for _, id := range cancelIDs {
		s.cancelLocal(id, true)
	}
	for id, r := range runs {
		r.mu.Lock()
		p := r.pendingProgress
		r.pendingProgress = nil
		if p != nil {
			r.progressWritten = time.Now()
		}
		r.mu.Unlock()
		if p != nil {
			s.writeProgress(ctx, id, p)
		}
	}
}

// Recover settles the jobs whose lease ended: a running one becomes
// outcome_unknown, a queued one failed. Rows with a live lease are never
// touched, whichever replica owns them.
func (s *Service) Recover(ctx context.Context) (failed, unknown int, err error) {
	now := s.now().UnixMilli()
	rows, err := s.db.QueryContext(ctx, `SELECT id, status FROM tool_jobs WHERE status IN ('queued','running') AND lease_until < ?
         ORDER BY lease_until LIMIT 200`, now)
	if err != nil {
		return 0, 0, err
	}
	type stale struct{ id, status string }
	var found []stale
	for rows.Next() {
		var st stale
		if err := rows.Scan(&st.id, &st.status); err != nil {
			rows.Close()
			return 0, 0, err
		}
		found = append(found, st)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	for _, st := range found {
		set := settlement{status: StatusOutcomeUnknown, errText: msgLostRunning}
		if st.status == StatusQueued {
			set = settlement{status: StatusFailed, errText: msgLostBeforeStart}
		}
		ok, err := s.settle(ctx, st.id, []string{st.status}, " AND lease_until < ?", []any{now}, set, true)
		if err != nil {
			return failed, unknown, err
		}
		if !ok {
			continue
		}
		// A goroutine of this process whose lease lapsed (its heartbeat
		// failed) has lost the job: stop it.
		s.cancelLocal(st.id, false)
		if set.status == StatusFailed {
			failed++
		} else {
			unknown++
		}
		s.log.Warn("job recovered after its lease ended", "job_id", st.id, "was", st.status, "now", set.status)
	}
	return failed, unknown, nil
}

// Purge deletes jobs that finished more than Retention ago, and their
// blob-store results that nothing else names.
func (s *Service) Purge(ctx context.Context) (int, error) {
	cutoff := s.now().Add(-s.retention).UnixMilli()
	total := 0
	for {
		rows, err := s.db.QueryContext(ctx, `SELECT id, COALESCE(result_blob,'') FROM tool_jobs
             WHERE finished_at IS NOT NULL AND finished_at < ? ORDER BY finished_at LIMIT 500`, cutoff)
		if err != nil {
			return total, err
		}
		var ids []string
		blobs := map[string]bool{}
		for rows.Next() {
			var id, blob string
			if err := rows.Scan(&id, &blob); err != nil {
				rows.Close()
				return total, err
			}
			ids = append(ids, id)
			if blob != "" {
				blobs[blob] = true
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return total, err
		}
		if len(ids) == 0 {
			return total, nil
		}
		args := make([]any, len(ids))
		for i, id := range ids {
			args[i] = id
		}
		if _, err := s.db.ExecContext(ctx, `DELETE FROM tool_jobs WHERE id IN (`+placeholders(len(ids))+`)`, args...); err != nil {
			return total, err
		}
		total += len(ids)
		for sha := range blobs {
			if s.blobs != nil && !s.blobReferenced(ctx, sha) {
				if err := s.blobs.Delete(ctx, sha); err != nil {
					s.log.Warn("job retention: could not delete a result blob", "sha", sha, "err", err.Error())
				}
			}
		}
		if len(ids) < 500 {
			return total, nil
		}
	}
}

// blobReferenced reports whether a stored job or an Inbox attachment
// still names blob sha (the store is content-addressed and shared).
func (s *Service) blobReferenced(ctx context.Context, sha string) bool {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tool_jobs WHERE result_blob = ?`, sha).Scan(&n); err != nil || n > 0 {
		return true
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM inbox_blobs WHERE sha256 = ?`, sha).Scan(&n); err != nil || n > 0 {
		return true
	}
	return false
}

// progressFunc records progress notifications for job id: at most one
// write a second, the latest kept for the heartbeat to write.
func (s *Service) progressFunc(id string, r *run) ProgressFunc {
	return func(progress float64, total *float64, message string) {
		if math.IsNaN(progress) || math.IsInf(progress, 0) {
			return
		}
		if total != nil && (math.IsNaN(*total) || math.IsInf(*total, 0)) {
			total = nil
		}
		p := &Progress{Progress: progress, Total: total, Message: clip(cleanText(message), maxProgressMessage), At: s.now().UnixMilli()}
		r.mu.Lock()
		due := time.Since(r.progressWritten) >= progressEvery
		if due {
			r.progressWritten = time.Now()
			r.pendingProgress = nil
		} else {
			r.pendingProgress = p
		}
		r.mu.Unlock()
		if due {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			s.writeProgress(ctx, id, p)
		}
	}
}

func (s *Service) writeProgress(ctx context.Context, id string, p *Progress) {
	var total any
	if p.Total != nil {
		total = *p.Total
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE tool_jobs SET progress = ?, progress_total = ?, progress_message = ?, progress_at = ?, updated_at = ?
         WHERE id = ? AND status = 'running'`, p.Progress, total, nullStr(p.Message), p.At, p.At, id); err != nil && ctx.Err() == nil {
		s.log.Warn("job: could not record progress", "job_id", id, "err", err.Error())
	}
}

// BeginDrain makes Start refuse new jobs (shutdown).
func (s *Service) BeginDrain() { s.draining.Store(true) }

// ReleaseWaiters makes every WaitFor answer now with the current state.
func (s *Service) ReleaseWaiters() {
	s.releaseOnce.Do(func() { close(s.released) })
}

// Released is closed by ReleaseWaiters.
func (s *Service) Released() <-chan struct{} { return s.released }

// Wait returns once no job goroutine of this process is left. When ctx
// ends first it cancels the jobs still going (a running call is recorded
// outcome_unknown, a queued job failed) and gives them up to two seconds
// to record that.
func (s *Service) Wait(ctx context.Context) {
	if s.waitIdle(ctx.Done()) {
		return
	}
	s.mu.Lock()
	for _, r := range s.inflight {
		r.cancel()
	}
	n := len(s.inflight)
	s.mu.Unlock()
	if n > 0 {
		s.log.Warn("jobs still going at the drain deadline were stopped", "count", n)
	}
	grace, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s.waitIdle(grace.Done())
}

// Running is how many job goroutines this process has.
func (s *Service) Running() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.inflight)
}

func (s *Service) waitIdle(stop <-chan struct{}) bool {
	for {
		s.mu.Lock()
		n, ch := len(s.inflight), s.idle
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

func (s *Service) untrack(id string) {
	s.mu.Lock()
	if r, ok := s.inflight[id]; ok {
		r.cancel()
		delete(s.inflight, id)
	}
	close(s.idle)
	s.idle = make(chan struct{})
	s.mu.Unlock()
	s.broadcast()
}

// changedCh is closed at the next state change of a job of this process.
func (s *Service) changedCh() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.changed
}

func (s *Service) broadcast() {
	s.mu.Lock()
	close(s.changed)
	s.changed = make(chan struct{})
	s.mu.Unlock()
}

func (s *Service) queryIDs(ctx context.Context, q string, args ...any) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// jobColumns is the SELECT list scanJob reads (the inline result is read
// on its own by Result).
const jobColumns = `id, agent_id, owner_user_id, COALESCE(session_id,''), tool, upstream, params_hash, reason, via, status,
    progress, progress_total, COALESCE(progress_message,''), COALESCE(progress_at,0),
    COALESCE(result_blob,''), result_bytes, result_is_error, COALESCE(error,''), COALESCE(callback_ref,''),
    COALESCE(callback_error,''), COALESCE(idempotency_key,''), cancel_requested, owner_instance, lease_until,
    created_at, COALESCE(started_at,0), COALESCE(finished_at,0), updated_at`

type rowScanner interface{ Scan(dest ...any) error }

func scanJob(r rowScanner) (*Job, error) {
	var j Job
	var prog, total sql.NullFloat64
	var progMsg string
	var progAt, resultBytes, isErr, cancelReq int64
	if err := r.Scan(&j.ID, &j.AgentID, &j.OwnerUserID, &j.SessionID, &j.Tool, &j.Upstream, &j.ParamsHash, &j.Reason, &j.Via, &j.Status,
		&prog, &total, &progMsg, &progAt,
		&j.resultBlob, &resultBytes, &isErr, &j.Error, &j.CallbackRef,
		&j.CallbackError, &j.IdempotencyKey, &cancelReq, &j.OwnerInstance, &j.LeaseUntil,
		&j.CreatedAt, &j.StartedAt, &j.FinishedAt, &j.UpdatedAt); err != nil {
		return nil, err
	}
	j.ResultBytes = int(resultBytes)
	j.ResultIsError = isErr != 0
	j.CancelRequested = cancelReq != 0
	if prog.Valid {
		p := &Progress{Progress: prog.Float64, Message: progMsg, At: progAt}
		if total.Valid {
			t := total.Float64
			p.Total = &t
		}
		j.Progress = p
	}
	return &j, nil
}

func placeholders(n int) string {
	if n <= 0 {
		return "NULL"
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// cleanText makes s storable in a text column on both engines: valid
// UTF-8 without NUL.
func cleanText(s string) string {
	s = strings.ToValidUTF8(s, "�")
	return strings.ReplaceAll(s, "\x00", "�")
}

// clip cuts s to at most n bytes on a rune boundary.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// newJobID returns "job_" and 32 hex characters.
func newJobID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("jobs: no randomness: %v", err))
	}
	return "job_" + hex.EncodeToString(b[:])
}

// defaultInstanceID names this process: host, process id and random hex.
func defaultInstanceID() string {
	host, _ := os.Hostname()
	if host == "" {
		host = "toolyard"
	}
	var b [4]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%s-%d-%s", host, os.Getpid(), hex.EncodeToString(b[:]))
}
