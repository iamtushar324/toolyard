package memwebhook

// TEC-482: asynchronous ingest. The HTTP handler calls Enqueue, which validates
// and maps the payload synchronously (so malformed/spec-violating bodies still
// fail fast with 422) and then persists a 'queued' job and returns. A single
// supervised background worker (RunWorker) claims queued jobs and performs the
// slow MemPalace embed/index/store off the request path, chunking large entries
// and recording the terminal outcome in the existing memory_webhook_ingestions
// ledger exactly as the old synchronous path did.
//
// Security is unchanged: the wing written to MemPalace comes only from the
// (immutable) webhook row, snapshotted onto the job; the request body/headers
// are never consulted for it. The Job view returned by the status endpoint
// carries no token material and no payload text.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/mempalace"
)

// Worker tuning. Package constants (documented defaults); not yet CLI flags.
const (
	// jobChunkBytes is the max size of a single MemPalace write. Entries above
	// it are split (see splitEntry). 32 KiB keeps each embed bounded while
	// leaving typical small payloads as a single write; the ~41 KiB transcript
	// from the ticket splits into a couple of chunks.
	jobChunkBytes = 32 << 10
	// jobMaxAttempts bounds retries of transient failures (e.g. a throttled
	// MemPalace) before a job is marked failed.
	jobMaxAttempts = 3
	// jobRetention is how long terminal job rows are kept before purge. The
	// ledger keeps the long-term audit history, so this is just for the
	// pollable status window.
	jobRetention = 14 * 24 * time.Hour
	// workerTick is the fallback poll cadence; the wake channel handles the
	// common low-latency pickup, and the tick also releases jobs whose backoff
	// gate has elapsed.
	workerTick     = 15 * time.Second
	jobBackoffBase = 2 * time.Second
	jobBackoffMax  = 60 * time.Second
	purgeEvery     = time.Hour
)

// Job is the pollable view of an ingest job returned by the status endpoint. It
// deliberately omits the mapped entry text and never carries token material.
type Job struct {
	ID          string `json:"job_id"`
	WebhookID   string `json:"webhook_id"`
	WebhookName string `json:"webhook_name"`
	Wing        string `json:"wing"`
	Source      string `json:"source,omitempty"`
	Status      string `json:"status"` // queued | running | succeeded | failed
	PayloadSize int64  `json:"payload_size"`
	Attempts    int    `json:"attempts"`
	ChunkCount  int    `json:"chunk_count"`
	ChunksDone  int    `json:"chunks_done"`
	Detail      string `json:"detail,omitempty"`
	Error       string `json:"error,omitempty"`
	MempalaceOK bool   `json:"mempalace_ok"`
	QueuedAt    int64  `json:"queued_at"`
	StartedAt   int64  `json:"started_at,omitempty"`
	FinishedAt  int64  `json:"finished_at,omitempty"`
}

// Enqueue validates + maps the payload (fast, synchronous) and persists a
// queued job, returning immediately. The expensive MemPalace write happens
// later in RunWorker. A schema violation is terminal and returns
// ErrSchemaViolation without queuing anything (still recorded in the ledger as
// 'rejected'). When MemPalace is entirely disabled it fails fast with
// ErrMemUnavailable rather than queuing a doomed job — a merely slow/throttled
// MemPalace is still Available() and is handled by the worker.
func (s *Service) Enqueue(ctx context.Context, wh *Webhook, in IngestRequest) (*Job, error) {
	source := in.Source
	if source == "" {
		source = wh.Source
	}

	entry, topic, err := buildEntry(wh.Spec, in.Payload, entryMeta{
		WebhookName: wh.Name,
		RequestID:   in.RequestID,
		Source:      source,
		Bytes:       in.PayloadSize,
	})
	if err != nil {
		s.recordIngestionRow(ctx, in.RequestID, wh.ID, wh.Name, wh.Wing, source, in.PayloadSize, statusRejected, err.Error(), false)
		s.bumpCounters(ctx, wh.ID, false)
		s.audit2(ctx, "memory_webhook.ingest_failed", wh.Name, "rejected req="+in.RequestID)
		return nil, err
	}

	if s.mem == nil || !s.mem.Available() {
		s.recordIngestionRow(ctx, in.RequestID, wh.ID, wh.Name, wh.Wing, source, in.PayloadSize, statusFailed, "mempalace unavailable", false)
		s.bumpCounters(ctx, wh.ID, false)
		s.audit2(ctx, "memory_webhook.ingest_failed", wh.Name, "mempalace_unavailable req="+in.RequestID)
		return nil, ErrMemUnavailable
	}

	now := time.Now().UnixMilli()
	if s.db != nil {
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO memory_webhook_jobs(
				id, webhook_id, webhook_name, wing, source, entry, topic, payload_size,
				status, attempts, chunk_count, chunks_done, mempalace_ok,
				queued_at, next_attempt_at, created_at, updated_at)
			 VALUES(?,?,?,?,?,?,?,?,?,0,0,0,0,?,0,?,?)`,
			in.RequestID, wh.ID, wh.Name, wh.Wing, nullStr(source), entry, nullStr(topic), in.PayloadSize,
			jobQueued, now, now, now); err != nil {
			return nil, err
		}
	}
	s.audit2(ctx, "memory_webhook.ingest_queued", wh.Name,
		fmt.Sprintf("wing=%s req=%s bytes=%d", wh.Wing, in.RequestID, in.PayloadSize))
	s.signalWake()

	return &Job{
		ID:          in.RequestID,
		WebhookID:   wh.ID,
		WebhookName: wh.Name,
		Wing:        wh.Wing,
		Source:      source,
		Status:      jobQueued,
		PayloadSize: in.PayloadSize,
		QueuedAt:    now,
	}, nil
}

// GetJob returns one job by id, or ErrNotFound. Callers (the status endpoint)
// must additionally scope by webhook id; GetJob itself does not.
func (s *Service) GetJob(ctx context.Context, id string) (*Job, error) {
	if s.db == nil {
		return nil, ErrNotFound
	}
	row := s.db.QueryRowContext(ctx, jobSelectCols+` FROM memory_webhook_jobs WHERE id=?`, id)
	j, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return j, err
}

// RunWorker is the supervised background loop (goroutines.Supervise entrypoint).
// It only returns on context cancellation; per-job errors are recorded on the
// job/ledger and never crash the loop.
//
// Jobs are processed strictly one at a time. There is exactly one RunWorker
// goroutine (a single goroutines.Supervise call in cmd/gateway, which never runs
// two invocations concurrently), and its drain loop runs each job to completion
// before claiming the next. This is deliberate: MemPalace is the throttled
// bottleneck this ticket is about, so we never fan out parallel embeds that
// would worsen memory pressure, and it matches SQLite's single-writer model.
// The atomic claim (UPDATE ... WHERE status='queued') is belt-and-suspenders in
// case that single-worker invariant is ever violated.
func (s *Service) RunWorker(ctx context.Context) error {
	s.recoverRunningJobs(ctx)
	s.drain(ctx)

	tick := time.NewTicker(workerTick)
	defer tick.Stop()
	purge := time.NewTicker(purgeEvery)
	defer purge.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.wake:
			s.drain(ctx)
		case <-tick.C:
			s.drain(ctx)
		case <-purge.C:
			s.purgeOldJobs(ctx)
		}
	}
}

// drain processes claimable jobs until none remain or ctx is cancelled.
func (s *Service) drain(ctx context.Context) {
	for ctx.Err() == nil {
		processed, err := s.processOnce(ctx)
		if err != nil || !processed {
			return
		}
	}
}

// signalWake nudges the worker without blocking when the buffer is full.
func (s *Service) signalWake() {
	if s.wake == nil {
		return
	}
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// processOnce claims the oldest claimable job and runs it. It reports whether a
// job was claimed so drain knows when the queue is empty.
func (s *Service) processOnce(ctx context.Context) (bool, error) {
	if s.db == nil {
		return false, nil
	}
	now := time.Now().UnixMilli()

	var id string
	err := s.db.QueryRowContext(ctx,
		`SELECT id FROM memory_webhook_jobs
		 WHERE status=? AND next_attempt_at<=?
		 ORDER BY queued_at LIMIT 1`, jobQueued, now).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	// Atomic claim: the status='queued' guard makes it idempotent.
	res, err := s.db.ExecContext(ctx,
		`UPDATE memory_webhook_jobs
		 SET status=?, started_at=COALESCE(started_at,?), attempts=attempts+1, updated_at=?
		 WHERE id=? AND status=?`,
		jobRunning, now, now, id, jobQueued)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return true, nil // someone else claimed it; try the next one
	}

	w, err := s.loadJobWork(ctx, id)
	if err != nil {
		return true, err
	}
	s.runJob(ctx, w)
	return true, nil
}

// jobWork is the worker's internal view, including the mapped entry text that
// the public Job intentionally omits.
type jobWork struct {
	ID          string
	WebhookID   string
	WebhookName string
	Wing        string
	Source      string
	Entry       string
	Topic       string
	PayloadSize int64
	Attempts    int
	ChunksDone  int
}

func (s *Service) loadJobWork(ctx context.Context, id string) (*jobWork, error) {
	var w jobWork
	var source, entry, topic sql.NullString
	if err := s.db.QueryRowContext(ctx,
		`SELECT id, webhook_id, webhook_name, wing, source, entry, topic, payload_size, attempts, chunks_done
		 FROM memory_webhook_jobs WHERE id=?`, id).
		Scan(&w.ID, &w.WebhookID, &w.WebhookName, &w.Wing, &source, &entry, &topic,
			&w.PayloadSize, &w.Attempts, &w.ChunksDone); err != nil {
		return nil, err
	}
	w.Source = source.String
	w.Entry = entry.String
	w.Topic = topic.String
	return &w, nil
}

// runJob performs the MemPalace write for a claimed job: it splits the entry
// into chunks and writes each under the job's LOCKED wing (never the payload's),
// resuming from chunks_done so a retry never duplicates an already-stored chunk.
func (s *Service) runJob(ctx context.Context, w *jobWork) {
	chunks := splitEntry(w.Entry, jobChunkBytes)
	n := len(chunks)
	s.setChunkCount(ctx, w.ID, n)

	agent := safeAgentName("webhook", w.WebhookName)
	var lastDetail string
	for i := w.ChunksDone; i < n; i++ {
		text := chunks[i]
		if i > 0 {
			text = chunkMarker(w.WebhookName, w.ID, i+1, n) + "\n\n" + text
		}
		ir, err := s.mem.IngestTagged(ctx, text, w.Topic, w.Wing, agent)
		if err != nil {
			s.failOrRetry(ctx, w, i, fmt.Sprintf("chunk %d/%d failed: %v", i+1, n, err), err)
			return
		}
		if ir == nil || !ir.OK {
			detail := "upstream rejected entry"
			if ir != nil && ir.Detail != "" {
				detail = ir.Detail
			}
			s.failOrRetry(ctx, w, i, fmt.Sprintf("chunk %d/%d rejected: %s", i+1, n, detail), ErrUpstreamRejected)
			return
		}
		lastDetail = ir.Detail
		w.ChunksDone = i + 1
		s.markChunkDone(ctx, w.ID, w.ChunksDone)
	}

	detail := lastDetail
	if n > 1 {
		detail = fmt.Sprintf("stored %d chunks: %s", n, lastDetail)
	}
	// The terminal transition is the guard against double-counting on the rare
	// crash-then-recover-then-finalize path: only the worker that flips
	// running->succeeded writes the ledger row and bumps the counters.
	if !s.markTerminal(ctx, w.ID, jobSucceeded, detail, "", true) {
		return
	}
	s.recordIngestionRow(ctx, w.ID, w.WebhookID, w.WebhookName, w.Wing, w.Source, w.PayloadSize, statusOK, detail, true)
	s.bumpCounters(ctx, w.WebhookID, true)
	s.audit2(ctx, "memory_webhook.ingest_ok", w.WebhookName,
		fmt.Sprintf("wing=%s req=%s bytes=%d", w.Wing, w.ID, w.PayloadSize))
}

// failOrRetry either requeues a job (transient failure, nothing written yet,
// attempts remain) or marks it terminally failed and records the ledger row.
func (s *Service) failOrRetry(ctx context.Context, w *jobWork, chunkIdx int, detail string, cause error) {
	transient := errors.Is(cause, ErrMemUnavailable) ||
		errors.Is(cause, mempalace.ErrDisabled) ||
		errors.Is(cause, mempalace.ErrNotReady) ||
		errors.Is(cause, context.DeadlineExceeded)
	// Only retry when nothing has been written for this job yet, so retries can
	// never duplicate already-stored chunks. A content rejection
	// (ErrUpstreamRejected) is never transient.
	canRetry := transient && chunkIdx == 0 && w.ChunksDone == 0 && w.Attempts < jobMaxAttempts
	if canRetry {
		s.requeue(ctx, w.ID, w.Attempts)
		s.audit2(ctx, "memory_webhook.ingest_retry", w.WebhookName,
			fmt.Sprintf("req=%s attempt=%d %s", w.ID, w.Attempts, detail))
		return
	}
	if !s.markTerminal(ctx, w.ID, jobFailed, detail, detail, false) {
		return
	}
	s.recordIngestionRow(ctx, w.ID, w.WebhookID, w.WebhookName, w.Wing, w.Source, w.PayloadSize, statusFailed, detail, false)
	s.bumpCounters(ctx, w.WebhookID, false)
	s.audit2(ctx, "memory_webhook.ingest_failed", w.WebhookName,
		fmt.Sprintf("req=%s %s", w.ID, detail))
}

// markTerminal flips a running job to its terminal status (nulling the entry to
// reclaim space) and reports whether it actually transitioned. The
// status='running' guard ensures the terminal side effects fire at most once.
func (s *Service) markTerminal(ctx context.Context, id, status, detail, errMsg string, mempalaceOK bool) bool {
	if s.db == nil {
		return true
	}
	now := time.Now().UnixMilli()
	res, err := s.db.ExecContext(ctx,
		`UPDATE memory_webhook_jobs
		 SET status=?, detail=?, error=?, mempalace_ok=?, finished_at=?, entry=NULL, updated_at=?
		 WHERE id=? AND status=?`,
		status, nullStr(truncate(detail, maxDetail)), nullStr(truncate(errMsg, maxDetail)),
		boolToInt(mempalaceOK), now, now, id, jobRunning)
	if err != nil {
		return false
	}
	n, _ := res.RowsAffected()
	return n == 1
}

func (s *Service) requeue(ctx context.Context, id string, attempts int) {
	if s.db == nil {
		return
	}
	shift := attempts - 1
	if shift < 0 {
		shift = 0
	}
	backoff := jobBackoffBase << uint(shift)
	if backoff > jobBackoffMax {
		backoff = jobBackoffMax
	}
	now := time.Now().UnixMilli()
	_, _ = s.db.ExecContext(ctx,
		`UPDATE memory_webhook_jobs SET status=?, next_attempt_at=?, updated_at=? WHERE id=? AND status=?`,
		jobQueued, now+backoff.Milliseconds(), now, id, jobRunning)
}

func (s *Service) setChunkCount(ctx context.Context, id string, n int) {
	if s.db == nil {
		return
	}
	_, _ = s.db.ExecContext(ctx,
		`UPDATE memory_webhook_jobs SET chunk_count=?, updated_at=? WHERE id=?`,
		n, time.Now().UnixMilli(), id)
}

func (s *Service) markChunkDone(ctx context.Context, id string, done int) {
	if s.db == nil {
		return
	}
	_, _ = s.db.ExecContext(ctx,
		`UPDATE memory_webhook_jobs SET chunks_done=?, updated_at=? WHERE id=?`,
		done, time.Now().UnixMilli(), id)
}

// recoverRunningJobs requeues jobs left 'running' by a crash/restart so they
// resume (from chunks_done) on the next worker pass.
func (s *Service) recoverRunningJobs(ctx context.Context) {
	if s.db == nil {
		return
	}
	now := time.Now().UnixMilli()
	_, _ = s.db.ExecContext(ctx,
		`UPDATE memory_webhook_jobs SET status=?, next_attempt_at=0, updated_at=? WHERE status=?`,
		jobQueued, now, jobRunning)
}

// purgeOldJobs deletes terminal job rows past the retention window. The ledger
// remains the durable audit record.
func (s *Service) purgeOldJobs(ctx context.Context) {
	if s.db == nil {
		return
	}
	cutoff := time.Now().Add(-jobRetention).UnixMilli()
	_, _ = s.db.ExecContext(ctx,
		`DELETE FROM memory_webhook_jobs
		 WHERE status IN(?,?) AND COALESCE(finished_at, updated_at) < ?`,
		jobSucceeded, jobFailed, cutoff)
}

const jobSelectCols = `SELECT id, webhook_id, webhook_name, wing, COALESCE(source,''),
	status, payload_size, attempts, chunk_count, chunks_done,
	COALESCE(detail,''), COALESCE(error,''), mempalace_ok,
	queued_at, COALESCE(started_at,0), COALESCE(finished_at,0)`

func scanJob(sc scanner) (*Job, error) {
	var j Job
	var mok int
	if err := sc.Scan(&j.ID, &j.WebhookID, &j.WebhookName, &j.Wing, &j.Source,
		&j.Status, &j.PayloadSize, &j.Attempts, &j.ChunkCount, &j.ChunksDone,
		&j.Detail, &j.Error, &mok, &j.QueuedAt, &j.StartedAt, &j.FinishedAt); err != nil {
		return nil, err
	}
	j.MempalaceOK = mok != 0
	return &j, nil
}
