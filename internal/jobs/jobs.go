// Package jobs runs tool calls in the background as durable jobs: the
// async tools of v0.1 (docs/guidelines/async-tools.md).
//
// A job is a row in tool_jobs. Start stores it queued and runs the call on
// a goroutine that outlives the request; the caller hands the agent the job
// id at once. Adopt stores a job for a call that is already running (a
// tool that is async only after a while, past its inline wait). Get, List
// and WaitFor read jobs, Result reads a finished job's encoded result
// (inline, or in the blob store when large), Cancel stops one, and every
// final status is written in one transaction with the Notifier's event
// (job.completed / job.failed callbacks).
//
// High availability: every row carries owner_instance and lease_until, on
// the database's clock. The process that runs a job renews its lease while
// the job is queued or running (the heartbeat), and claims it queued ->
// running with a compare-and-set. Recover takes only rows whose lease
// ended: a running job becomes outcome_unknown (its call may have run, so
// it is never run again) and a queued one failed (nothing ran). Waiters are
// woken in process and fall back to a database poll, so a job that runs on
// another replica is still seen. The per-agent, per-owner and anonymous
// caps are checked and the row inserted under one lock (an advisory
// transaction lock on Postgres; SQLite has one writer).
package jobs

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/blobstore"
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
	// DefaultMaxActive caps one agent's queued and running jobs,
	// DefaultMaxActivePerOwner all the agents of one owner together, and
	// DefaultMaxActiveAnonymous the jobs of anonymous callers together.
	DefaultMaxActive          = 32
	DefaultMaxActivePerOwner  = 64
	DefaultMaxActiveAnonymous = 8
	DefaultMaxDuration        = time.Hour
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
	// waitDebounce gathers a burst of state changes into one reread.
	waitDebounce = 25 * time.Millisecond
)

// Error texts of the statuses toolyard sets itself.
const (
	msgCancelledBeforeStart = "cancelled before it started; nothing ran"
	msgCancelledRunning     = "cancelled while running: the upstream call was interrupted, and a write may already have taken effect; check the target before you call it again"
	msgLostBeforeStart      = "the toolyard server that accepted this job stopped before the job started; nothing ran; call the tool again"
	msgLostRunning          = "the toolyard server that ran this job stopped while the call was running; the call may or may not have run; check the target before any new call"
	msgStoppedBeforeStart   = "toolyard stopped before this job started; nothing ran; call the tool again"
	msgRevokedRunning       = "the agent's access to this tool was removed while the job ran: the call was interrupted, and a write may already have taken effect; check the target before any new call"
)

// Audit event types the service reports through its Auditor.
const (
	EventJobRecovered      = "job.recovered"
	EventJobNotStarted     = "job.not_started"
	EventJobOutcomeUnknown = "job.outcome_unknown"
	EventJobRevoked        = "job.revoked"
)

// Errors the service returns.
var (
	ErrNotFound             = errors.New("job not found")
	ErrIdempotencyKeyReused = errors.New("idempotency_key_reused")
	ErrTooManyActive        = errors.New("too many queued or running jobs for this agent or owner; wait for some to finish or cancel them")
	ErrDraining             = errors.New("toolyard is restarting: no job was started; try again in a moment")
)

// InputError is bad input in a start request.
type InputError struct {
	Field   string
	Message string
}

func (e *InputError) Error() string { return e.Field + ": " + e.Message }

// Notifier is told, inside the transaction that records a job's final
// status, that the job finished. The gateway binary wires it to the
// callbacks service through EmitterNotifier: callbacks.EmitForAgentTx(ctx,
// tx, job.AgentID, job.CallbackRef, CallbackEvent(job)), which uses the
// agent's default receiver when the job has no callback_ref and does
// nothing when there is none. It is called for every finished job; only an error it returns is
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

// Emitter queues one callback event for agentID inside tx, to ref or, when
// ref is "", to the agent's default receiver (nil when it has none):
// callbacks.Service.EmitForAgentTx.
type Emitter func(ctx context.Context, tx *sql.Tx, agentID, ref, eventType string, data any) error

// EmitterNotifier is the Notifier that sends job.completed / job.failed
// (CallbackEvent) through emit. A job of an anonymous caller with no
// callback_ref has no receiver and sends nothing.
func EmitterNotifier(emit Emitter) Notifier { return emitterNotifier{emit: emit} }

type emitterNotifier struct{ emit Emitter }

func (n emitterNotifier) JobFinished(ctx context.Context, tx *sql.Tx, j Job) error {
	if n.emit == nil || (j.AgentID == "" && j.CallbackRef == "") {
		return nil
	}
	ev, data := CallbackEvent(j)
	return n.emit(ctx, tx, j.AgentID, j.CallbackRef, ev, data)
}

// Auditor records the job events toolyard decides itself (recovery, a job
// that never started, an unknown outcome, access removed while running) in
// the audit log. The gateway wires it.
type Auditor interface {
	JobEvent(ctx context.Context, eventType string, job Job, detail string)
}

// Guard reports whether agentID may still call tool (on upstream). The
// heartbeat asks it for every job this process runs and stops the ones it
// refuses: an agent or owner disabled, or a server access withdrawn. The
// gateway wires it to its access resolver.
type Guard func(ctx context.Context, agentID, upstream, tool string) bool

// Leader runs fn while this process is the one node that should (the
// retention purge): *cluster.Cluster in the binary. It blocks until ctx
// ends.
type Leader interface {
	RunLeader(ctx context.Context, name string, fn func(ctx context.Context) error)
}

// DirectLeader runs fn here, as on a single node: again after it returns,
// until ctx ends. Running the purge on several nodes at once is harmless.
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
	// LeaseUntil is on the database's clock; CreatedAt, StartedAt,
	// FinishedAt and UpdatedAt on the server's. All are Unix milliseconds;
	// StartedAt and FinishedAt are 0 until set.
	LeaseUntil int64
	CreatedAt  int64
	StartedAt  int64
	FinishedAt int64
	UpdatedAt  int64

	// resultBlob is the blob store key of a large result ("" when inline).
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

// ProgressFunc records a progress notification for the job's call. It
// never blocks on the database: it is called from an upstream's read loop.
type ProgressFunc func(progress float64, total *float64, message string)

// RunFunc runs the job's call on ctx (cancelled by Cancel, by the job's
// time limit, by the Guard and at shutdown) and reports how it ended.
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
}

// Options configure a Service. Zero values pick the defaults.
type Options struct {
	// InstanceID names this process in owner_instance (the binary passes
	// cluster.InstanceID). Default: host name, process id and random hex.
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
	// MaxRunning caps the jobs this process runs at once, and
	// MaxRunningPerAgent one agent's share of them (default a quarter);
	// the rest wait queued, their leases still renewed.
	MaxRunning         int
	MaxRunningPerAgent int
	// MaxActive, MaxActivePerOwner and MaxActiveAnonymous cap queued and
	// running jobs per agent, per owner and for anonymous callers.
	MaxActive          int
	MaxActivePerOwner  int
	MaxActiveAnonymous int
	// MaxDuration is a job's time limit.
	MaxDuration time.Duration
	// Now is the clock for the times shown and the retention (tests).
	// Leases always use the database's clock.
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
	db           *store.DB
	instance     string
	notifier     atomic.Pointer[notifierBox]
	leader       Leader
	blobs        blobstore.Store
	leaseTTL     time.Duration
	pollEvery    time.Duration
	retention    time.Duration
	inlineLimit  int
	maxResult    int
	maxActive    int
	maxPerOwner  int
	maxAnonymous int
	maxDuration  time.Duration
	now          func() time.Time
	log          *slog.Logger
	slots        *slotPool
	auditor      atomic.Pointer[auditorBox]
	guard        atomic.Pointer[Guard]

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

type auditorBox struct{ a Auditor }

type notifierBox struct{ n Notifier }

// run is one job goroutine of this process.
type run struct {
	cancel     context.CancelFunc
	handle     *Handle
	agentID    string
	upstream   string
	tool       string
	userCancel atomic.Bool
	revoked    atomic.Bool
	// started is set once the call runs (claimed, or adopted).
	started atomic.Bool

	mu              sync.Mutex
	progressWritten time.Time
	pendingProgress *Progress
	writing         bool
	// writeMu orders this run's progress writes: the one that takes the
	// latest progress writes last.
	writeMu sync.Mutex
}

// New returns a Service over db.
func New(db *store.DB, opts Options) *Service {
	s := &Service{
		db: db, instance: opts.InstanceID, leader: opts.Leader, blobs: opts.Blobs,
		leaseTTL: opts.LeaseTTL, pollEvery: opts.PollEvery, retention: opts.Retention,
		inlineLimit: opts.InlineLimit, maxResult: opts.MaxResultBytes, maxActive: opts.MaxActive,
		maxPerOwner: opts.MaxActivePerOwner, maxAnonymous: opts.MaxActiveAnonymous,
		maxDuration: opts.MaxDuration, now: opts.Now, log: logx.For("jobs"),
		inflight: map[string]*run{}, idle: make(chan struct{}), changed: make(chan struct{}), released: make(chan struct{}),
	}
	if s.instance == "" {
		s.instance = defaultInstanceID()
	}
	s.SetNotifier(opts.Notifier)
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
	if s.maxPerOwner <= 0 {
		s.maxPerOwner = DefaultMaxActivePerOwner
	}
	if s.maxAnonymous <= 0 {
		s.maxAnonymous = DefaultMaxActiveAnonymous
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
	perAgent := opts.MaxRunningPerAgent
	if perAgent <= 0 {
		perAgent = max(1, maxRunning/4)
	}
	s.slots = newSlotPool(maxRunning, perAgent)
	return s
}

// SetNotifier replaces the Notifier (nil: NopNotifier), for a callbacks
// service built after the job service. Safe to call while it runs.
func (s *Service) SetNotifier(n Notifier) {
	if n == nil {
		n = NopNotifier{}
	}
	s.notifier.Store(&notifierBox{n: n})
}

// SetAuditor wires the audit log. Safe to call while the service runs.
func (s *Service) SetAuditor(a Auditor) { s.auditor.Store(&auditorBox{a: a}) }

// SetGuard wires the access check the heartbeat applies to running jobs.
// Safe to call while the service runs.
func (s *Service) SetGuard(g Guard) { s.guard.Store(&g) }

func (s *Service) audit(ctx context.Context, eventType string, j *Job, detail string) {
	if b := s.auditor.Load(); b != nil && b.a != nil && j != nil {
		b.a.JobEvent(ctx, eventType, *j, detail)
	}
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
	outcome Outcome
}

func newHandle() *Handle { return &Handle{done: make(chan struct{})} }

// Done is closed once the job ended and its final status is stored.
func (h *Handle) Done() <-chan struct{} { return h.done }

// Outcome is how the job ended; valid after Done.
func (h *Handle) Outcome() Outcome { return h.outcome }

func (h *Handle) complete(out Outcome) {
	h.outcome = out
	close(h.done)
}
