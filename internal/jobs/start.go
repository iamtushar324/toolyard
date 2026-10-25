package jobs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/goroutines"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// Start validates req, stores a queued job (within the agent, owner and
// anonymous caps) and runs fn on a goroutine of its own. The goroutine's
// context carries ctx's values but not its cancellation: the job outlives
// the request. With an IdempotencyKey the agent's job for that key is
// returned instead (with a nil Handle) when its tool and params hash
// match, ErrIdempotencyKeyReused when they do not.
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
	if req.Admit != nil {
		if err := req.Admit(ctx); err != nil {
			return nil, nil, err
		}
	}
	job, err := s.insertCapped(ctx, req, StatusQueued, 0)
	if err != nil {
		if req.IdempotencyKey != "" && store.IsUniqueViolation(err) {
			j, rerr := s.replay(ctx, req)
			if rerr == nil || errors.Is(rerr, ErrIdempotencyKeyReused) {
				return j, nil, rerr
			}
		}
		return nil, nil, err
	}
	runCtx, cancelTimeout := context.WithTimeout(context.WithoutCancel(ctx), s.maxDuration)
	runCtx, cancel := context.WithCancel(runCtx)
	r := &run{handle: newHandle(), cancel: func() { cancel(); cancelTimeout() }, agentID: req.AgentID, upstream: req.Upstream, tool: req.Tool}
	s.track(job.ID, r)
	go s.execute(runCtx, job, r, fn)
	return job, r.handle, nil
}

// Adoption is a call already running in this process that becomes a job:
// a tool that is async only after a while, still running past its inline
// wait. Its context is the caller's to set up (time limit included).
type Adoption struct {
	// StartedAt is when the call started.
	StartedAt time.Time
	// Cancel stops the call: jobs.cancel, the Guard, shutdown.
	Cancel context.CancelFunc
	// Context is the call's context (optional): a call that ended on its
	// own time limit is not one shutdown interrupted.
	Context context.Context
	// Done is closed when the call returned; Outcome is read after it.
	Done    <-chan struct{}
	Outcome func() Outcome
	// Relay carries the call's progress; Adopt attaches it to the job.
	Relay *ProgressRelay
}

// Adopt stores a running job for a call that is already running here and
// records its outcome when it returns. The call counts against this
// process's run slots without waiting for one. Within the caps like Start;
// ErrTooManyActive or ErrDraining leave the call to its caller.
func (s *Service) Adopt(ctx context.Context, req StartRequest, a Adoption) (*Job, error) {
	if s.draining.Load() {
		return nil, ErrDraining
	}
	if err := checkStart(&req); err != nil {
		return nil, err
	}
	req.IdempotencyKey = ""
	job, err := s.insertCapped(ctx, req, StatusRunning, a.StartedAt.UnixMilli())
	if err != nil {
		return nil, err
	}
	cancel := a.Cancel
	if cancel == nil {
		cancel = func() {}
	}
	r := &run{handle: newHandle(), cancel: cancel, agentID: req.AgentID, upstream: req.Upstream, tool: req.Tool}
	r.started.Store(true)
	s.track(job.ID, r)
	s.slots.take(req.AgentID)
	if a.Relay != nil {
		a.Relay.attach(s.progressFunc(job.ID, r))
	}
	go func() {
		defer s.untrack(job.ID)
		defer s.slots.release(req.AgentID)
		<-a.Done
		interrupted := r.shutdown.Load() && (a.Context == nil || errors.Is(a.Context.Err(), context.Canceled))
		out := Outcome{Status: StatusOutcomeUnknown}
		if a.Outcome != nil {
			out = s.normalize(a.Outcome())
		}
		s.finishRun(job, r, out, interrupted)
	}()
	return job, nil
}

// Record stores a job that is already final: a task-augmented call that
// was answered at once (refused before it could run, held, or answered
// inline) still gets a task the client can read. The row is written final
// in one transaction, with the Notifier's event. Nothing runs, so the
// active-job caps do not apply; instead the row is kept only
// RecordedRetention (ExpiresAt), and one agent (or the anonymous callers
// together) may hold at most MaxRecordedPerAgent of them, counted and
// inserted as one step (ErrTooManyRecorded).
func (s *Service) Record(ctx context.Context, req StartRequest, out Outcome) (*Job, error) {
	if err := checkStart(&req); err != nil {
		return nil, err
	}
	req.IdempotencyKey = ""
	if out.Status == "" || !Final(out.Status) {
		out.Status = StatusFailed
	}
	now := s.now().UnixMilli()
	j := &Job{
		ID: newJobID(), AgentID: req.AgentID, OwnerUserID: req.OwnerUserID, SessionID: req.SessionID,
		Tool: req.Tool, Upstream: req.Upstream, ParamsHash: req.ParamsHash, Reason: req.Reason, Via: req.Via,
		Status: out.Status, CallbackRef: req.CallbackRef, OwnerInstance: s.instance,
		CreatedAt: now, FinishedAt: now, UpdatedAt: now, ExpiresAt: now + RecordedRetention.Milliseconds(),
	}
	st := s.prepareResult(ctx, j, out)
	err := s.recordTx(ctx, j, st, true, "")
	var ne *noticeError
	if errors.As(err, &ne) {
		s.log.Warn("job: the finish event could not be queued; recording the task without it", "job_id", j.ID, "err", ne.err.Error())
		err = s.recordTx(ctx, j, st, false, clip(cleanText(ne.err.Error()), 500))
	}
	if err != nil {
		s.dropUnusedBlob(context.WithoutCancel(ctx), j.ID, st)
		return nil, err
	}
	return s.Get(ctx, j.ID)
}

// recordTx checks the recorded-answer cap and inserts j, final, in one
// transaction (under an advisory lock on the agent on Postgres; SQLite has
// one writer), with the Notifier's event when withNotice is set.
func (s *Service) recordTx(ctx context.Context, j *Job, st settlement, withNotice bool, noticeErr string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if s.db.Dialect.IsPostgres() {
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(?)`, recordLockKey(j.AgentID)); err != nil {
			return err
		}
	}
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM tool_jobs WHERE agent_id = ? AND expires_at > ?`, j.AgentID, j.CreatedAt).Scan(&n); err != nil {
		return err
	}
	if n >= s.maxRecorded {
		return ErrTooManyRecorded
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO tool_jobs(id, agent_id, owner_user_id, session_id, tool, upstream,
            params_hash, reason, via, status, result, result_blob, result_bytes, result_is_error, error, callback_ref, callback_error,
            nothing_ran, owner_instance, lease_until, created_at, finished_at, updated_at, expires_at)
         VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,`+s.dbNow()+`,?,?,?,?)`,
		j.ID, j.AgentID, j.OwnerUserID, nullStr(j.SessionID), j.Tool, j.Upstream,
		j.ParamsHash, j.Reason, j.Via, st.status, nullStr(st.inline), nullStr(st.blob), st.bytes, boolInt(st.isError), nullStr(st.errText),
		nullStr(j.CallbackRef), nullStr(noticeErr), boolInt(st.nothingRan), j.OwnerInstance, j.CreatedAt, j.FinishedAt, j.UpdatedAt, j.ExpiresAt); err != nil {
		return err
	}
	if withNotice {
		row, err := getJob(ctx, tx, j.ID)
		if err == nil {
			err = s.notifier.Load().n.JobFinished(ctx, tx, *row)
		}
		if err != nil {
			return &noticeError{err: err}
		}
	}
	return tx.Commit()
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

// insertCapped checks the caps and inserts the row in one transaction.
// On Postgres a transaction-scoped advisory lock on the owner (or, without
// one, the agent) makes the count and the insert one step across replicas;
// SQLite has one writer, so its transaction already is.
func (s *Service) insertCapped(ctx context.Context, req StartRequest, status string, startedAt int64) (*Job, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if s.db.Dialect.IsPostgres() {
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(?)`, capLockKey(req)); err != nil {
			return nil, err
		}
	}
	if err := s.checkCaps(ctx, tx, req); err != nil {
		return nil, err
	}
	job, err := s.insert(ctx, tx, req, status, startedAt)
	if err != nil {
		return nil, err
	}
	return job, tx.Commit()
}

// checkCaps refuses a job over the agent's (or the anonymous callers')
// cap or the owner's.
func (s *Service) checkCaps(ctx context.Context, q store.Querier, req StartRequest) error {
	count := func(col, v string) (int, error) {
		var n int
		err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM tool_jobs WHERE `+col+` = ? AND status IN ('queued','running')`, v).Scan(&n)
		return n, err
	}
	limit := s.maxActive
	if req.AgentID == "" {
		limit = s.maxAnonymous
	}
	n, err := count("agent_id", req.AgentID)
	if err != nil {
		return err
	}
	if n >= limit {
		return ErrTooManyActive
	}
	if req.OwnerUserID != "" {
		n, err := count("owner_user_id", req.OwnerUserID)
		if err != nil {
			return err
		}
		if n >= s.maxPerOwner {
			return ErrTooManyActive
		}
	}
	return nil
}

// insert writes a new queued or running row (Record writes its own).
func (s *Service) insert(ctx context.Context, q store.Querier, req StartRequest, status string, startedAt int64) (*Job, error) {
	now := s.now().UnixMilli()
	j := &Job{
		ID: newJobID(), AgentID: req.AgentID, OwnerUserID: req.OwnerUserID, SessionID: req.SessionID,
		Tool: req.Tool, Upstream: req.Upstream, ParamsHash: req.ParamsHash, Reason: req.Reason, Via: req.Via,
		Status: status, CallbackRef: req.CallbackRef, IdempotencyKey: req.IdempotencyKey,
		OwnerInstance: s.instance, LeaseUntil: now + s.leaseTTL.Milliseconds(), CreatedAt: now, StartedAt: startedAt, UpdatedAt: now,
	}
	var started any
	if startedAt > 0 {
		started = startedAt
	}
	_, err := q.ExecContext(ctx, `INSERT INTO tool_jobs(id, agent_id, owner_user_id, session_id, tool, upstream,
            params_hash, reason, via, status, callback_ref, idempotency_key, owner_instance, lease_until, created_at, started_at, updated_at)
         VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,`+s.dbNow()+` + ?,?,?,?)`,
		j.ID, j.AgentID, j.OwnerUserID, nullStr(j.SessionID), j.Tool, j.Upstream,
		j.ParamsHash, j.Reason, j.Via, j.Status, nullStr(j.CallbackRef), nullStr(j.IdempotencyKey),
		j.OwnerInstance, s.leaseTTL.Milliseconds(), j.CreatedAt, started, j.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return j, nil
}

// execute runs one queued job: it waits for a run slot (the agent's share
// of this process's slots), claims the row, runs the call and records the
// outcome.
func (s *Service) execute(ctx context.Context, job *Job, r *run, fn RunFunc) {
	defer s.untrack(job.ID)
	if !s.slots.acquire(ctx, r.agentID) {
		s.settleUnstarted(ctx, job.ID, r, "")
		return
	}
	defer s.slots.release(r.agentID)
	if ctx.Err() != nil {
		s.settleUnstarted(ctx, job.ID, r, "")
		return
	}
	claimed, err := s.claim(ctx, job.ID)
	if err != nil {
		s.log.Warn("job: could not mark running", "job_id", job.ID, "err", err.Error())
	}
	if !claimed {
		// Not queued any more (cancelled, or recovered by another
		// replica): settleUnstarted only reads the final row. Still
		// queued after a failed write: it never started.
		msg := ""
		if err != nil && ctx.Err() == nil {
			msg = msgClaimFailed
		}
		s.settleUnstarted(ctx, job.ID, r, msg)
		return
	}
	r.started.Store(true)
	s.broadcast()
	out := s.call(ctx, r, job.ID, fn)
	// Interrupted by shutdown only when Wait's cancel ended the call: a
	// call past its own time limit (a deadline) ended on that.
	s.finishRun(job, r, out, r.shutdown.Load() && errors.Is(ctx.Err(), context.Canceled))
}

// finishRun records how a running job ended. interrupted says Wait
// stopped the call at the drain deadline (shutdown): unless it succeeded,
// or proves it never ran, its outcome is unknown, whatever error the
// interrupted call answered.
func (s *Service) finishRun(job *Job, r *run, out Outcome, interrupted bool) {
	switch {
	case r.revoked.Load() && out.Status != StatusSucceeded && out.Status != StatusFailed && out.NothingRan:
		out.Status, out.Error = StatusCancelled, msgRevokedBeforeStart
	case r.revoked.Load() && out.Status != StatusSucceeded && out.Status != StatusFailed:
		out.Status, out.Error = StatusCancelled, msgRevokedRunning
	case r.userCancel.Load() && out.Status == StatusOutcomeUnknown:
		// A call that answered (succeeded or failed) keeps its answer; one
		// the cancel interrupted is cancelled.
		out.Status, out.Error = StatusCancelled, msgCancelledRunning
	case r.userCancel.Load():
		// A user's cancel came first: the call's own answer stands, even
		// when a drain began meanwhile.
	case interrupted && out.Status != StatusSucceeded && out.NothingRan:
		out.Status, out.Error = StatusFailed, msgStoppedBeforeStart
	case interrupted && out.Status != StatusSucceeded && out.Status != StatusOutcomeUnknown:
		out.Status, out.Error = StatusOutcomeUnknown, msgStoppedRunning
	}
	moved, err := s.settleFinal(job, out)
	if err != nil {
		s.log.Error("job: could not record the outcome; the job reads running until its lease ends and recovery marks it outcome_unknown",
			"job_id", job.ID, "status", out.Status, "err", err.Error())
	}
	wctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if moved {
		switch {
		case r.revoked.Load() && out.Status == StatusCancelled:
			s.auditJob(wctx, EventJobRevoked, job.ID, out.Error)
		case out.Status == StatusOutcomeUnknown:
			s.auditJob(wctx, EventJobOutcomeUnknown, job.ID, out.Error)
		}
	}
	r.handle.complete(out)
}

// settleAttemptHook, when set (tests only), sees each final write attempt.
var settleAttemptHook atomic.Pointer[func(id string, err error)]

// settleFinal records a running job's outcome. A failed write is tried
// again, with a growing pause, for as long as the job's lease would last:
// the heartbeat keeps renewing it while the run is tracked, so a short
// database error does not turn a known outcome into outcome_unknown. A
// write whose commit landed but whose answer was lost reads as "not
// moved" on the next try: the row is read back, and when it carries this
// very outcome, it moved.
func (s *Service) settleFinal(job *Job, out Outcome) (bool, error) {
	pctx, pcancel := context.WithTimeout(context.Background(), 30*time.Second)
	st := s.prepareResult(pctx, job, out)
	pcancel()
	giveUp := time.Now().Add(s.leaseTTL)
	pause := settleRetryFirst
	failedBefore := false
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		moved, err := s.settle(ctx, job.ID, []string{StatusRunning}, "", nil, st, true)
		if h := settleAttemptHook.Load(); h != nil {
			(*h)(job.ID, err)
		}
		if err == nil && !moved && failedBefore {
			if j, gerr := s.Get(ctx, job.ID); gerr == nil && j.Status == st.status && j.Error == st.errText && j.OwnerInstance == s.instance {
				moved = true
			}
		}
		if err == nil || time.Now().Add(pause).After(giveUp) {
			if !moved {
				s.dropUnusedBlob(ctx, job.ID, st)
			}
			if err == nil && !moved {
				s.log.Info("job: already final when its outcome came (cancelled first, or recovered by another replica after its lease ended); the late outcome is not kept",
					"job_id", job.ID, "status", out.Status)
			}
			cancel()
			return moved, err
		}
		cancel()
		failedBefore = true
		s.log.Warn("job: could not record the outcome; trying again", "job_id", job.ID, "in", pause.String(), "err", err.Error())
		time.Sleep(pause)
		pause = min(pause*2, settleRetryMax)
	}
}

// auditJob reads job id and reports eventType for it.
func (s *Service) auditJob(ctx context.Context, eventType, id, detail string) {
	if s.auditor.Load() == nil {
		return
	}
	if j, err := s.Get(ctx, id); err == nil {
		s.audit(ctx, eventType, j, detail)
	}
}

// call runs fn, turning a panic into outcome_unknown.
func (s *Service) call(ctx context.Context, r *run, id string, fn RunFunc) (out Outcome) {
	defer func() {
		if p := recover(); p != nil {
			goroutines.Report(goroutineName, p)
			out = Outcome{Status: StatusOutcomeUnknown, Error: "the job stopped on an internal error in toolyard; the call may have run"}
		}
	}()
	return s.normalize(fn(ctx, s.progressFunc(id, r)))
}

// normalize makes out's status one of the final ones.
func (s *Service) normalize(out Outcome) Outcome {
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

// settleUnstarted records a job that never started, with the reason:
// cancelled while queued (Cancel already wrote it), its access removed
// (the Guard), its whole time limit spent waiting for a run slot, a
// failed claim (msg), or shutdown. runCtx is the job's own context.
func (s *Service) settleUnstarted(runCtx context.Context, id string, r *run, msg string) {
	set, event := settlement{status: StatusFailed, errText: msgStoppedBeforeStart, nothingRan: true}, EventJobNotStarted
	switch {
	case r.revoked.Load():
		set, event = settlement{status: StatusCancelled, errText: msgRevokedBeforeStart, nothingRan: true}, EventJobRevoked
	case msg != "":
		set.errText = msg
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		set.errText = msgNoRunSlot
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	moved, err := s.settle(ctx, id, []string{StatusQueued}, "", nil, set, true)
	if err != nil {
		s.log.Warn("job: could not record a job that never started", "job_id", id, "err", err.Error())
	}
	out := Outcome{Status: set.status, Error: set.errText}
	if j, err := s.Get(ctx, id); err == nil && j.Final() {
		out = Outcome{Status: j.Status, Error: j.Error}
		if moved {
			s.audit(ctx, event, j, j.Error)
		}
	}
	r.handle.complete(out)
}

func (s *Service) claim(ctx context.Context, id string) (bool, error) {
	now := s.now().UnixMilli()
	res, err := s.db.ExecContext(ctx, `UPDATE tool_jobs SET status = 'running', started_at = ?, updated_at = ?, owner_instance = ?,
            lease_until = `+s.dbNow()+` + ?
         WHERE id = ? AND status = 'queued'`, now, now, s.instance, s.leaseTTL.Milliseconds(), id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}
