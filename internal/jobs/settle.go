package jobs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/store"
)

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

// settleOutcome stores out's result for job (inline or in the blob store)
// and writes the final status. A result over the per-job limit, or over
// what the job's agent or owner may keep, is not kept. A blob written for
// a row that did not move (it was already final) is deleted again unless
// something else names it.
func (s *Service) settleOutcome(ctx context.Context, job *Job, from []string, out Outcome, notify bool) (bool, error) {
	id := job.ID
	st := settlement{status: out.Status, isError: out.IsError, errText: out.Error, bytes: len(out.Envelope)}
	env := out.Envelope
	over := ""
	switch {
	case env == "":
	case len(env) > s.maxResult:
		over = fmt.Sprintf("the result (%d bytes) is over the %d byte job limit and was not kept; the call itself finished", len(env), s.maxResult)
	default:
		over = s.storedOver(ctx, job, len(env))
	}
	switch {
	case env == "":
	case over != "":
		if st.errText == "" {
			st.errText = over
		}
		st.bytes = 0
	case len(env) > s.inlineLimit && s.blobs != nil:
		key := blobKey(id, env)
		if _, err := s.blobs.Put(ctx, key, strings.NewReader(env), int64(len(env))); err != nil {
			s.log.Warn("job: could not store a large result in the blob store; keeping it in the row", "job_id", id, "err", err.Error())
			st.inline = env
		} else {
			st.blob = key
		}
	default:
		st.inline = env
	}
	ok, err := s.settle(ctx, id, from, "", nil, st, notify)
	if !ok && st.blob != "" && !s.blobReferenced(ctx, st.blob) {
		if derr := s.blobs.Delete(ctx, st.blob); derr != nil {
			s.log.Warn("job: could not delete an orphaned result blob", "job_id", id, "err", derr.Error())
		}
	}
	if err == nil && !ok {
		s.log.Info("job: already final when its outcome came (recovered meanwhile); the late outcome is not kept", "job_id", id, "status", out.Status)
	}
	return ok, err
}

// storedOver says why a result of n bytes is not kept for job: its
// agent's, or its owner's, stored results would pass their cap. "" when it
// fits. A failed count keeps the result (the per-job limit still holds).
func (s *Service) storedOver(ctx context.Context, job *Job, n int) string {
	sum := func(col, v string) (int64, bool) {
		var total int64
		err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(result_bytes),0) FROM tool_jobs WHERE `+col+` = ? AND id <> ?`, v, job.ID).Scan(&total)
		if err != nil {
			s.log.Warn("job: could not count the stored result bytes; keeping the result", "job_id", job.ID, "err", err.Error())
			return 0, false
		}
		return total, true
	}
	if total, ok := sum("agent_id", job.AgentID); ok && total+int64(n) > s.maxStoredAg {
		return fmt.Sprintf("the result (%d bytes) was not kept: this agent's stored job results would pass their %d byte limit; the call itself finished. Results are removed %s after their job ends",
			n, s.maxStoredAg, s.retention)
	}
	if job.OwnerUserID != "" {
		if total, ok := sum("owner_user_id", job.OwnerUserID); ok && total+int64(n) > s.maxStoredOwn {
			return fmt.Sprintf("the result (%d bytes) was not kept: the stored job results of this owner's agents would pass their %d byte limit; the call itself finished. Results are removed %s after their job ends",
				n, s.maxStoredOwn, s.retention)
		}
	}
	return ""
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
				err = s.notifier.Load().n.JobFinished(ctx, tx, *j)
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

// getMany reads the jobs with ids in one query, by id.
func (s *Service) getMany(ctx context.Context, ids []string) (map[string]*Job, error) {
	out := make(map[string]*Job, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+jobColumns+` FROM tool_jobs WHERE id IN (`+placeholders(len(ids))+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out[j.ID] = j
	}
	return out, rows.Err()
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
// or with anyOne set the first. It returns their latest state in the order
// of ids (nil for an id that is not stored) and whether the time ran out
// (or ctx ended, or ReleaseWaiters was called) before the condition held.
// The first read runs even on an ended ctx (bounded on its own), and when
// ctx ends later it returns the last rows it read: it never answers empty
// rows for jobs that exist (a nil row always means the job is not
// stored); a failed read is an error. Each reread is one query; a burst
// of changes is one reread.
func (s *Service) WaitFor(ctx context.Context, ids []string, anyOne bool, timeout time.Duration) ([]*Job, bool, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	poll := time.NewTicker(s.pollEvery)
	defer poll.Stop()
	var last []*Job
	for {
		ch := s.changedCh()
		rctx, cancel := ctx, context.CancelFunc(func() {})
		if last == nil {
			rctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		}
		byID, err := s.getMany(rctx, ids)
		cancel()
		if err != nil {
			if ctx.Err() != nil && last != nil {
				return last, true, nil
			}
			return nil, false, err
		}
		jobs := make([]*Job, len(ids))
		final, all := false, true
		for i, id := range ids {
			j := byID[id]
			jobs[i] = j
			if j == nil || j.Final() {
				final = true
			} else {
				all = false
			}
		}
		if (anyOne && final) || (!anyOne && all) {
			return jobs, false, nil
		}
		last = jobs
		select {
		case <-ch:
			select {
			case <-time.After(waitDebounce):
			case <-ctx.Done():
				return jobs, true, nil
			}
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

// BeginWait admits one wait of agentID's (jobs.wait, tasks/result,
// Prefer: wait) within the per-agent cap, MaxWaitersPerAgent, since each
// wait rereads the database. Call done when the wait ends.
// ErrTooManyWaiters when the agent is at the cap.
func (s *Service) BeginWait(agentID string) (done func(), err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.waiters[agentID] >= s.maxWaiters {
		return nil, ErrTooManyWaiters
	}
	s.waiters[agentID]++
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.waiters[agentID]--; s.waiters[agentID] <= 0 {
				delete(s.waiters, agentID)
			}
		})
	}, nil
}

// Cancel stops job id: a queued job is cancelled at once (nothing ran); a
// running one is asked to stop (on whichever replica runs it) and ends
// cancelled unless its call answered first. cancelled is false when the
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
