package coderuns

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/cluster"
	"github.com/tusharbhardwaj/toolyard/internal/codemode"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// Messages a recovered run carries.
const (
	msgInterruptedQueued  = "toolyard stopped before the script started; nothing ran. Start the run again."
	msgInterruptedRunning = "toolyard stopped while the script was running, so a tool call may have run. Check what the script changed before you run it again."
)

// runColumns are the code_runs columns a view needs (not the script).
const runColumns = `id, agent_id, COALESCE(session_id, ''), COALESCE(idempotency_key, ''), fingerprint, reason, COALESCE(language, 'starlark'), status,
	COALESCE(result, ''), COALESCE(error_kind, ''), COALESCE(error_message, ''), COALESCE(logs, ''), COALESCE(held, ''),
	created_at, updated_at, COALESCE(finished_at, 0)`

// runWriteTimeout bounds the writes that must not follow the client's
// connection: a disconnect while one commits would leave a run row with
// nothing to execute it.
const runWriteTimeout = 10 * time.Second

// insertRun stores a new run.
func (s *Service) insertRun(ctx context.Context, r *Run) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), runWriteTimeout)
	defer cancel()
	lang := r.Language
	if lang == "" {
		lang = codemode.LanguageStarlark
	}
	// The run is this node's from the start: it runs on this process, under
	// a lease it renews (RunLeaseRenewal).
	_, err := s.db.ExecContext(ctx, `INSERT INTO code_runs
		(id, agent_id, session_id, idempotency_key, fingerprint, code, reason, language, status, created_at, updated_at, owner_instance, lease_until)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, `+cluster.LeaseUntilSQL(s.db.Dialect)+`)`,
		r.ID, r.AgentID, nullStr(r.SessionID), nullStr(r.IdempotencyKey), r.Fingerprint, r.code, r.Reason, lang, r.Status, r.CreatedAt, r.UpdatedAt,
		s.cluster.InstanceID())
	return err
}

// replay answers a start that reuses an Idempotency-Key: the stored run
// for the same script and reason, ErrIdempotencyKeyReused for others,
// ErrNotFound when the agent never used the key.
func (s *Service) replay(ctx context.Context, agentID, key, fp string) (*Run, error) {
	var id, have string
	err := s.db.QueryRowContext(ctx, `SELECT id, fingerprint FROM code_runs WHERE agent_id = ? AND idempotency_key = ?`, agentID, key).Scan(&id, &have)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if have != fp {
		return nil, ErrIdempotencyKeyReused
	}
	return s.loadRun(ctx, agentID, id, false)
}

// loadRun reads the agent's run with its calls; withCode reads the script
// too. Another agent's run is ErrNotFound.
func (s *Service) loadRun(ctx context.Context, agentID, id string, withCode bool) (*Run, error) {
	q := `SELECT ` + runColumns
	if withCode {
		q += `, code`
	}
	q += ` FROM code_runs WHERE id = ? AND agent_id = ?`
	var r Run
	var result, logs, held string
	dest := []any{&r.ID, &r.AgentID, &r.SessionID, &r.IdempotencyKey, &r.Fingerprint, &r.Reason, &r.Language, &r.Status,
		&result, &r.ErrorKind, &r.ErrorMessage, &logs, &held, &r.CreatedAt, &r.UpdatedAt, &r.FinishedAt}
	if withCode {
		dest = append(dest, &r.code)
	}
	err := s.db.QueryRowContext(ctx, q, id, agentID).Scan(dest...)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if result != "" {
		r.Result = json.RawMessage(result)
	}
	if logs != "" {
		if err := json.Unmarshal([]byte(logs), &r.Logs); err != nil {
			return nil, fmt.Errorf("run %s: logs: %w", id, err)
		}
	}
	if held != "" {
		r.Held = &Held{}
		if err := json.Unmarshal([]byte(held), r.Held); err != nil {
			return nil, fmt.Errorf("run %s: held: %w", id, err)
		}
	}
	if r.Calls, err = s.loadCalls(ctx, id); err != nil {
		return nil, err
	}
	return &r, nil
}

// loadCalls reads a run's call records in order.
func (s *Service) loadCalls(ctx context.Context, runID string) ([]Call, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT seq, tool, target, args_hash, status, replayed, COALESCE(replay_text, ''), started_at, COALESCE(finished_at, 0),
		sent, COALESCE(error_kind, ''), COALESCE(error_text, ''), COALESCE(approval_id, ''), COALESCE(hold_content, '')
		FROM code_run_calls WHERE run_id = ? ORDER BY seq`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Call
	for rows.Next() {
		var c Call
		var replayed, sent int64
		var text string
		if err := rows.Scan(&c.Seq, &c.Tool, &c.Target, &c.ArgsHash, &c.Status, &replayed, &text, &c.StartedAt, &c.FinishedAt,
			&sent, &c.errorKind, &c.errorText, &c.approvalID, &c.hold); err != nil {
			return nil, err
		}
		c.Replayed = replayed != 0
		c.sent = sent != 0
		if text != "" {
			if err := json.Unmarshal([]byte(text), &c.replayText); err != nil {
				return nil, fmt.Errorf("run %s call %d: replay text: %w", runID, c.Seq, err)
			}
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// markRunning moves a queued run to running.
func (s *Service) markRunning(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE code_runs SET status = ?, updated_at = ?, owner_instance = ?, lease_until = `+cluster.LeaseUntilSQL(s.db.Dialect)+`
		WHERE id = ? AND status = ?`,
		StatusRunning, s.now().UnixMilli(), s.cluster.InstanceID(), id, StatusQueued)
	return err
}

// claimResume moves a held run to running, unless another resume (or
// anything else) changed its status since r was read.
func (s *Service) claimResume(ctx context.Context, r *Run) (bool, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), runWriteTimeout)
	defer cancel()
	now := s.now().UnixMilli()
	res, err := s.db.ExecContext(ctx, `UPDATE code_runs SET status = ?, error_kind = NULL, error_message = NULL, finished_at = NULL, updated_at = ?,
		owner_instance = ?, lease_until = `+cluster.LeaseUntilSQL(s.db.Dialect)+`
		WHERE id = ? AND agent_id = ? AND status = ?`, StatusRunning, now, s.cluster.InstanceID(), r.ID, r.AgentID, r.Status)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 1 {
		r.Status, r.UpdatedAt = StatusRunning, now
	}
	return n == 1, nil
}

// upsertCall writes one call record: with the replay value of a
// succeeded call, the error kind and text of a failed one, and the text,
// approval id and structured content of a held one.
func upsertCall(ctx context.Context, q store.Querier, runID string, c Call) error {
	var text any
	if c.Status == codemode.StatusSucceeded {
		raw, err := json.Marshal(c.replayText)
		if err != nil {
			return err
		}
		text = string(raw)
	}
	replayed, sent := 0, 0
	if c.Replayed {
		replayed = 1
	}
	if c.sent {
		sent = 1
	}
	_, err := q.ExecContext(ctx, `INSERT INTO code_run_calls
		(run_id, seq, tool, target, args_hash, status, replayed, replay_text, started_at, finished_at, sent, error_kind, error_text, approval_id, hold_content)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (run_id, seq) DO UPDATE SET tool = excluded.tool, target = excluded.target, args_hash = excluded.args_hash,
			status = excluded.status, replayed = excluded.replayed, replay_text = excluded.replay_text,
			started_at = excluded.started_at, finished_at = excluded.finished_at, sent = excluded.sent,
			error_kind = excluded.error_kind, error_text = excluded.error_text, approval_id = excluded.approval_id, hold_content = excluded.hold_content
		WHERE excluded.sent = 1 OR code_run_calls.status NOT IN (?, ?)`,
		runID, c.Seq, c.Tool, c.Target, c.ArgsHash, c.Status, replayed, text, c.StartedAt, nullInt(c.FinishedAt),
		sent, nullStr(c.errorKind), nullStr(c.errorText), nullStr(c.approvalID), nullStr(c.hold),
		// A call that never reached the gateway (refused by its check, or
		// stopped first) never replaces the record of a call that ran.
		codemode.StatusSucceeded, codemode.StatusOutcomeUnknown)
	return err
}

// finish records a run's outcome: its calls (the latest execution's, with
// the arguments hash and replay value the hooks saw), and its status,
// result, error, print lines and held call.
func (s *Service) finish(ctx context.Context, run *Run, rr *codemode.RunResult, h *hooks) error {
	status := rr.Status
	switch status {
	case codemode.StatusSucceeded, codemode.StatusFailed, codemode.StatusPermissionRequired,
		codemode.StatusDeferred, codemode.StatusOutcomeUnknown:
	default:
		status = codemode.StatusFailed
		rr.ErrorKind, rr.ErrorMessage = codemode.ErrorInternal, fmt.Sprintf("the script runner reported an unknown status %q", rr.Status)
	}
	var result, errKind, errMsg, held any
	if status == codemode.StatusSucceeded && len(rr.Result) > 0 {
		result = redactGrants(string(rr.Result))
	}
	if status == codemode.StatusFailed || status == codemode.StatusOutcomeUnknown {
		kind := rr.ErrorKind
		if kind == "" {
			kind = codemode.ErrorInternal
		}
		errKind, errMsg = kind, redactGrants(cleanText(rr.ErrorMessage))
	}
	if IsHeld(status) && rr.Held != nil {
		raw, err := json.Marshal(Held{
			Seq: rr.Held.Seq, Tool: rr.Held.Tool, Target: rr.Held.Target, Status: rr.Held.Status,
			Draft: rr.Held.Draft, ApprovalID: rr.Held.ApprovalID, Message: rr.Held.Message,
		})
		if err != nil {
			return err
		}
		held = redactGrants(string(raw))
	}
	logs := make([]string, 0, len(rr.Logs))
	for _, l := range rr.Logs {
		logs = append(logs, redactGrants(l))
	}
	logsJSON, err := json.Marshal(logs)
	if err != nil {
		return err
	}
	now := s.now().UnixMilli()
	var finished any
	if !IsHeld(status) {
		finished = now
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, rec := range rr.Calls {
		if rec.Replayed {
			if _, err := tx.ExecContext(ctx, `UPDATE code_run_calls SET replayed = 1 WHERE run_id = ? AND seq = ?`, run.ID, rec.Seq); err != nil {
				return err
			}
			continue
		}
		seen := h.callSeen(rec.Seq)
		c := Call{Seq: rec.Seq, Tool: rec.Tool, Target: rec.Target, ArgsHash: seen.argsHash, Status: rec.Status,
			StartedAt: rec.StartedAt, FinishedAt: rec.FinishedAt}
		c.setSeen(seen, rec.Sent)
		if err := upsertCall(ctx, tx, run.ID, c); err != nil {
			return err
		}
	}
	// Calls this execution did not reach keep their records from an
	// earlier one: a call that ran stays known (a later resume answers it
	// from its record instead of sending it as new), and the highest call
	// number the run ever reached stays the mark past which calls are new.
	if _, err := tx.ExecContext(ctx, `UPDATE code_runs SET status = ?, result = ?, error_kind = ?, error_message = ?, logs = ?, held = ?,
		updated_at = ?, finished_at = ? WHERE id = ?`,
		status, result, errKind, errMsg, string(logsJSON), held, now, finished, run.ID); err != nil {
		return err
	}
	if status == codemode.StatusFailed && rr.ErrorKind == codemode.ErrorBusy && len(rr.Calls) == 0 {
		// No slot came free, so nothing ran: the Idempotency-Key is free
		// again, and a retry with it starts a new run instead of answering
		// with this one.
		if _, err := tx.ExecContext(ctx, `UPDATE code_runs SET idempotency_key = NULL WHERE id = ?`, run.ID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Recover settles the runs a stopped process left behind: queued runs
// failed with kind interrupted (their script never started), running runs
// outcome_unknown with kind interrupted (a tool call may have run). Call
// it at startup, before the routes serve, on a single node: every queued
// or running run then belongs to a stopped process. On a multi-node
// database (SetCluster) only the leader calls it, one lease after it took
// over, and it takes only runs whose lease ended by the database's clock
// (or that have none), so it never touches the live runs of another node,
// nor of this one.
func (s *Service) Recover(ctx context.Context) (failed, unknown int64, err error) {
	now := s.now().UnixMilli()
	expired := ""
	if s.cluster.MultiNode() {
		expired = ` AND ` + cluster.LeaseEndedSQL(s.db.Dialect, "lease_until")
	}
	// Literal statuses let Postgres use the partial lease index.
	res, err := s.db.ExecContext(ctx, `UPDATE code_runs SET status = ?, error_kind = ?, error_message = ?, updated_at = ?, finished_at = ? WHERE status = 'queued'`+expired,
		codemode.StatusFailed, ErrorInterrupted, msgInterruptedQueued, now, now)
	if err != nil {
		return 0, 0, err
	}
	failed, _ = res.RowsAffected()
	res, err = s.db.ExecContext(ctx, `UPDATE code_runs SET status = ?, error_kind = ?, error_message = ?, updated_at = ?, finished_at = ? WHERE status = 'running'`+expired,
		codemode.StatusOutcomeUnknown, ErrorInterrupted, msgInterruptedRunning, now, now)
	if err != nil {
		return failed, 0, err
	}
	unknown, _ = res.RowsAffected()
	return failed, unknown, nil
}

// Purge deletes the runs created more than Retention ago, with their call
// records, and returns how many runs it deleted. A run still queued or
// running is left for Recover.
func (s *Service) Purge(ctx context.Context) (int64, error) {
	cutoff := s.now().Add(-Retention).UnixMilli()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM code_run_calls WHERE run_id IN
		(SELECT id FROM code_runs WHERE created_at < ? AND status NOT IN (?, ?))`, cutoff, StatusQueued, StatusRunning); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM code_runs WHERE created_at < ? AND status NOT IN (?, ?)`, cutoff, StatusQueued, StatusRunning)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return n, tx.Commit()
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullInt(n int64) any {
	if n == 0 {
		return nil
	}
	return n
}
