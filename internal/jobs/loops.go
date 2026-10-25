package jobs

import (
	"context"
	"math"
	"sync"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/cluster"
	"github.com/tusharbhardwaj/toolyard/internal/goroutines"
)

// Run starts the background loops until ctx ends. The heartbeat (lease
// renewal, cancel requests, the Guard, progress) and recovery are safe on
// every node: each node renews only its own rows, and recovery takes only
// rows whose lease ended, with a compare-and-set. The retention purge runs
// under the Leader.
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
				// One pass is a leader unit: a lost lease or a drain does
				// not cut it half-way, and shutdown waits for it.
				unit, end := cluster.BeginUnit(ctx, "tool-jobs retention")
				defer end()
				if n, err := s.Purge(unit); err != nil && unit.Err() == nil {
					s.log.Warn("job retention", "err", err.Error())
				} else if n > 0 {
					s.log.Info("job retention: removed finished jobs", "count", n)
				}
			})
			return ctx.Err()
		})
	})
}

// heartbeatEvery renews leases well within the lease, and at least every
// 5 s, the delay of a cancel made on another replica.
func (s *Service) heartbeatEvery() time.Duration {
	return max(min(s.leaseTTL/6, 5*time.Second), 50*time.Millisecond)
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

// Heartbeat renews the leases of this process's jobs on the database
// clock, stops the local runs whose rows are no longer active and owned
// here (recovered by another replica), applies cancel requests other
// replicas wrote, applies the Guard, and writes pending progress.
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
	var cancelIDs, lostIDs []string
	for start := 0; start < len(ids); start += 200 {
		batch := ids[start:min(start+200, len(ids))]
		idArgs := make([]any, len(batch))
		for i, id := range batch {
			idArgs[i] = id
		}
		args := append([]any{s.leaseTTL.Milliseconds(), s.instance}, idArgs...)
		res, err := s.db.ExecContext(ctx, `UPDATE tool_jobs SET lease_until = `+s.dbNow()+` + ?
             WHERE owner_instance = ? AND status IN ('queued','running') AND id IN (`+placeholders(len(batch))+`)`, args...)
		if err != nil {
			if ctx.Err() == nil {
				s.log.Warn("job heartbeat: could not renew leases", "err", err.Error())
			}
			return
		}
		if n, err := res.RowsAffected(); err == nil && int(n) < len(batch) {
			active, err := s.queryIDs(ctx, `SELECT id FROM tool_jobs WHERE owner_instance = ? AND status IN ('queued','running') AND id IN (`+
				placeholders(len(batch))+`)`, append([]any{s.instance}, idArgs...)...)
			if err == nil {
				live := make(map[string]bool, len(active))
				for _, id := range active {
					live[id] = true
				}
				for _, id := range batch {
					if !live[id] {
						lostIDs = append(lostIDs, id)
					}
				}
			}
		}
		asked, err := s.queryIDs(ctx, `SELECT id FROM tool_jobs WHERE cancel_requested = 1 AND status = 'running' AND id IN (`+placeholders(len(batch))+`)`, idArgs...)
		if err == nil {
			cancelIDs = append(cancelIDs, asked...)
		}
	}
	for _, id := range lostIDs {
		// Final already (it finished a moment ago, or another replica
		// recovered it): whatever still runs here has lost the job.
		s.cancelLocal(id, false)
	}
	for _, id := range cancelIDs {
		s.cancelLocal(id, true)
	}
	if g := s.guard.Load(); g != nil && *g != nil {
		for id, r := range runs {
			allowed := (*g)(ctx, r.agentID, r.upstream, r.tool)
			if ctx.Err() != nil {
				break // a lookup on an ended context proves nothing
			}
			if allowed {
				r.guardRefusals.Store(0)
				continue
			}
			// The lookup fails closed: one refusal may be a short database
			// error. Access removed reads the same on the next beat too.
			if r.guardRefusals.Add(1) < guardRefusalsToStop {
				s.log.Warn("job: the access check refused a running job once; it is stopped if the next check refuses it too", "job_id", id)
				continue
			}
			r.revoked.Store(true)
			s.cancelLocal(id, false)
		}
	}
	for id, r := range runs {
		s.flushProgress(ctx, id, r)
	}
}

// Recover settles the jobs whose lease ended (on the database clock): a
// running one becomes outcome_unknown, a queued one failed. Rows with a
// live lease are never touched, whichever replica owns them.
func (s *Service) Recover(ctx context.Context) (failed, unknown int, err error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, status FROM tool_jobs WHERE status IN ('queued','running') AND lease_until < `+s.dbNow()+`
         ORDER BY lease_until LIMIT 200`)
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
		ok, err := s.settle(ctx, st.id, []string{st.status}, " AND lease_until < "+s.dbNow(), nil, set, true)
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
		s.auditJob(ctx, EventJobRecovered, st.id, "was "+st.status+": "+set.errText)
	}
	return failed, unknown, nil
}

// Purge deletes jobs that finished more than Retention ago, recorded task
// answers past their ExpiresAt, and their blob-store results that nothing
// else names.
func (s *Service) Purge(ctx context.Context) (int, error) {
	now := s.now()
	cutoff := now.Add(-s.retention).UnixMilli()
	total := 0
	for {
		rows, err := s.db.QueryContext(ctx, `SELECT id, COALESCE(result_blob,'') FROM tool_jobs
             WHERE finished_at IS NOT NULL AND (finished_at < ? OR (expires_at IS NOT NULL AND expires_at < ?)) LIMIT 500`, cutoff, now.UnixMilli())
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
		for key := range blobs {
			if s.blobs != nil && !s.blobReferenced(ctx, key) {
				if err := s.blobs.Delete(ctx, key); err != nil {
					s.log.Warn("job retention: could not delete a result blob", "key", key, "err", err.Error())
				}
			}
		}
		if len(ids) < 500 {
			return total, nil
		}
	}
}

// blobReferenced reports whether a stored job still names blob key. Job
// result keys are domain-separated and per job (blobKey), so no Inbox
// attachment and no other job can name one; both are checked anyway, to be
// safe.
func (s *Service) blobReferenced(ctx context.Context, key string) bool {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tool_jobs WHERE result_blob = ?`, key).Scan(&n); err != nil || n > 0 {
		return true
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM inbox_blobs WHERE sha256 = ?`, key).Scan(&n); err != nil || n > 0 {
		return true
	}
	return false
}

// ---- run slots --------------------------------------------------------------

// slotPool caps the jobs this process runs at once, and each agent's share
// of them, so one agent cannot take every slot.
type slotPool struct {
	mu       sync.Mutex
	max      int
	perAgent int
	used     int
	byAgent  map[string]int
	freed    chan struct{}
}

func newSlotPool(max, perAgent int) *slotPool {
	return &slotPool{max: max, perAgent: perAgent, byAgent: map[string]int{}, freed: make(chan struct{})}
}

// acquire waits for a slot for agent; false when ctx ended first.
func (p *slotPool) acquire(ctx context.Context, agent string) bool {
	for {
		if ctx.Err() != nil {
			return false
		}
		p.mu.Lock()
		if p.used < p.max && p.byAgent[agent] < p.perAgent {
			p.used++
			p.byAgent[agent]++
			p.mu.Unlock()
			return true
		}
		ch := p.freed
		p.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return false
		}
	}
}

// take counts a call that already runs (an adopted one) without waiting.
func (p *slotPool) take(agent string) {
	p.mu.Lock()
	p.used++
	p.byAgent[agent]++
	p.mu.Unlock()
}

func (p *slotPool) release(agent string) {
	p.mu.Lock()
	p.used--
	if p.byAgent[agent]--; p.byAgent[agent] <= 0 {
		delete(p.byAgent, agent)
	}
	close(p.freed)
	p.freed = make(chan struct{})
	p.mu.Unlock()
}

// ---- progress -------------------------------------------------------------

// progressFunc records progress notifications for job id without touching
// the database on the caller's goroutine (an upstream's read loop): the
// latest one is kept, and written by a background write at most once a
// second, or by the heartbeat.
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
		r.pendingProgress = p
		start := !r.writing && time.Since(r.progressWritten) >= progressEvery
		if start {
			r.writing = true
		}
		r.mu.Unlock()
		if start {
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				s.flushProgress(ctx, id, r)
				r.mu.Lock()
				r.writing = false
				r.mu.Unlock()
			}()
		}
	}
}

// flushProgress writes the run's pending progress, if any.
func (s *Service) flushProgress(ctx context.Context, id string, r *run) {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	r.mu.Lock()
	p := r.pendingProgress
	r.pendingProgress = nil
	if p != nil {
		r.progressWritten = time.Now()
	}
	r.mu.Unlock()
	if p == nil {
		return
	}
	var total any
	if p.Total != nil {
		total = *p.Total
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE tool_jobs SET progress = ?, progress_total = ?, progress_message = ?, progress_at = ?, updated_at = ?
         WHERE id = ? AND status = 'running'`, p.Progress, total, nullStr(p.Message), p.At, p.At, id); err != nil && ctx.Err() == nil {
		s.log.Warn("job: could not record progress", "job_id", id, "err", err.Error())
	}
}

// ProgressRelay holds a call's progress until the call becomes a job
// (Adopt), then passes it on. Its Report never blocks.
type ProgressRelay struct {
	mu      sync.Mutex
	target  ProgressFunc
	pending *relayed
}

type relayed struct {
	progress float64
	total    *float64
	message  string
}

// NewProgressRelay returns an empty relay.
func NewProgressRelay() *ProgressRelay { return &ProgressRelay{} }

// Report is the call's ProgressFunc.
func (p *ProgressRelay) Report(progress float64, total *float64, message string) {
	p.mu.Lock()
	target := p.target
	if target == nil {
		p.pending = &relayed{progress, total, message}
	}
	p.mu.Unlock()
	if target != nil {
		target(progress, total, message)
	}
}

func (p *ProgressRelay) attach(fn ProgressFunc) {
	p.mu.Lock()
	p.target = fn
	pending := p.pending
	p.pending = nil
	p.mu.Unlock()
	if pending != nil {
		fn(pending.progress, pending.total, pending.message)
	}
}

// ---- lifecycle --------------------------------------------------------------

// BeginDrain makes Start and Adopt refuse new jobs (shutdown).
func (s *Service) BeginDrain() { s.draining.Store(true) }

// ReleaseWaiters makes every WaitFor answer now with the current state.
func (s *Service) ReleaseWaiters() {
	s.releaseOnce.Do(func() { close(s.released) })
}

// Released is closed by ReleaseWaiters.
func (s *Service) Released() <-chan struct{} { return s.released }

// Wait returns once no job goroutine of this process is left. When ctx
// ends first it cancels the jobs still going (a running call is recorded
// outcome_unknown unless it succeeded or proves it never ran, a queued job
// failed) and gives them up to two seconds to record that.
func (s *Service) Wait(ctx context.Context) {
	if !s.waitIdle(ctx.Done()) {
		// Jobs that never started go first, so a slot a running job frees
		// on its way out is not taken by one of them.
		s.mu.Lock()
		for _, r := range s.inflight {
			if !r.started.Load() {
				r.shutdown.Store(true)
				r.cancel()
			}
		}
		for _, r := range s.inflight {
			r.shutdown.Store(true)
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

func (s *Service) track(id string, r *run) {
	s.mu.Lock()
	s.inflight[id] = r
	s.mu.Unlock()
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
