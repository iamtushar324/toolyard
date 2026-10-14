package inbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/actor"
	"github.com/tusharbhardwaj/toolyard/internal/logx"
	"github.com/tusharbhardwaj/toolyard/internal/opsmetrics"
)

// LegacySkipError reports legacy approvals SyncLegacy could not represent
// in Inbox (unreadable arguments, an unreadable Inbox copy, an ID used by a
// non-legacy request). SyncLegacy committed every other row; skipped rows
// are tried again on every later sync.
type LegacySkipError struct {
	IDs  []string
	Errs []error // one per ID, same order
}

func (e *LegacySkipError) Error() string {
	msg := fmt.Sprintf("legacy Inbox sync skipped %d malformed row(s): %s", len(e.IDs), strings.Join(e.IDs, ", "))
	if len(e.Errs) > 0 {
		msg += " (first: " + e.Errs[0].Error() + ")"
	}
	return msg
}

func (e *LegacySkipError) add(id string, err error) {
	e.IDs = append(e.IDs, id)
	e.Errs = append(e.Errs, err)
}

// SyncLegacy represents the original approvals, including history, in the only
// human decision interface. Original IDs, actors, deadlines and arguments stay
// intact. Missing historical explanations remain missing, never invented.
//
// A malformed row is skipped, not fatal: every other row is committed and
// the skipped IDs come back in a *LegacySkipError. Database errors still
// abort the whole sync.
func (s *Service) SyncLegacy(ctx context.Context) error {
	s.legacySyncMu.Lock()
	defer s.legacySyncMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, legacyOwnerSnapshotSQL); err != nil {
		return err
	}
	if s.db.Dialect.IsPostgres() {
		// Lock every Inbox copy of a legacy approval BEFORE approval_requests
		// is read. Decide (mutateTx) locks the Inbox row and updates
		// approval_requests in one transaction, so with these locks held the
		// reads below see both of its writes or neither, and the upsert never
		// writes a stale pending copy over a committed decision. Only
		// inbox_requests rows are locked (never approval_requests, which
		// mutateTx updates while it holds the Inbox row), in id order.
		// SQLite's single connection already serializes this.
		if _, err = tx.ExecContext(ctx, `SELECT id FROM inbox_requests WHERE id IN (SELECT id FROM approval_requests) ORDER BY id FOR UPDATE OF inbox_requests`); err != nil {
			return err
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,COALESCE(agent_id,''),upstream_name,tool_name,arguments,reason,status,COALESCE(decided_by,''),COALESCE(decided_at,0),created_at,expires_at,COALESCE(result_executed_at,0),COALESCE(result_is_error,0),COALESCE(result_error,''),COALESCE(decided_via,''),COALESCE(decider_email,''),COALESCE(decider_name,''),COALESCE(decider_ref,'') FROM approval_requests`)
	if err != nil {
		return err
	}
	var imported []Request
	var skipped LegacySkipError
	for rows.Next() {
		var r Request
		var upstream, tool, args, reason, status, resultError string
		var executed int64
		var failed int
		if err = rows.Scan(&r.ID, &r.AgentID, &upstream, &tool, &args, &reason, &status, &r.DecidedBy, &r.DecidedAt, &r.CreatedAt, &r.ExpiresAt, &executed, &failed, &resultError, &r.DeciderVia, &r.DeciderEmail, &r.DeciderName, &r.DeciderRef); err != nil {
			rows.Close()
			return err
		}
		r.ExecutionMode = "legacy"
		r.SchemaVersion = 2
		r.Revision = 1
		r.Kind = KindAccess
		r.Urgency = UrgencySoon
		r.Title = tool
		r.Summary = reason
		r.Message = reason
		r.Checked = true
		r.TTLSeconds = 1800
		r.UpdatedAt = r.CreatedAt
		r.Status = status
		if status == "allowed" {
			r.Status = StatusApproved
		}
		if r.Status != StatusPending {
			r.Revision = 2
			r.UpdatedAt = r.DecidedAt
		}
		var raw map[string]any
		decoder := json.NewDecoder(strings.NewReader(args))
		decoder.UseNumber()
		if derr := decoder.Decode(&raw); derr != nil {
			skipped.add(r.ID, fmt.Errorf("legacy %s arguments: %w", r.ID, derr))
			continue
		}
		params := map[string]Constraint{}
		for k, v := range raw {
			params[k] = Constraint{Op: "eq", Eq: v}
		}
		t := ToolRequest{CallID: "call_1", LegacyParamsJSON: args, Tool: tool, Upstream: upstream, Summary: reason, Params: params}
		if status == "allowed" {
			t.Decision = ToolAllowed
			t.Verdict = VerdictAccepted
		}
		if status == "denied" || status == "expired" || status == "cancelled" {
			t.Decision = ToolRefused
			t.Verdict = VerdictRejected
		}
		r.Tools = []ToolRequest{t}
		r.LegacyExecutionState = "not_started"
		if executed > 0 {
			r.LegacyExecutionState = "succeeded"
			if failed != 0 || resultError != "" {
				r.LegacyExecutionState = "failed"
			}
		}
		imported = append(imported, r)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for i := range imported {
		r := &imported[i]
		var state string
		if e := tx.QueryRowContext(ctx, `SELECT state FROM legacy_execution_claims WHERE request_id=?`, r.ID).Scan(&state); e == nil && (r.LegacyExecutionState == "not_started" || state == "outcome_unknown") {
			r.LegacyExecutionState = state
		}
		var existing string
		e := tx.QueryRowContext(ctx, `SELECT doc FROM inbox_requests WHERE id=?`+s.db.Dialect.ForUpdate(), r.ID).Scan(&existing)
		if e == nil {
			var prev Request
			if e = json.Unmarshal([]byte(existing), &prev); e != nil {
				skipped.add(r.ID, fmt.Errorf("legacy %s: unreadable Inbox copy: %w", r.ID, e))
				continue
			}
			if prev.ExecutionMode != "legacy" {
				skipped.add(r.ID, fmt.Errorf("legacy ID collision: %s", r.ID))
				continue
			}
			// Preserve the submitted reasons, notes and decision revision from Inbox.
			r.DeciderUserID = prev.DeciderUserID
			r.OwnerNote = prev.OwnerNote
			r.DecisionSubmissionID = prev.DecisionSubmissionID
			r.DecisionFingerprint = prev.DecisionFingerprint
			r.Activity = prev.Activity
			if prev.Revision > r.Revision {
				r.Revision = prev.Revision
			}
			if len(prev.Tools) == 1 {
				r.Tools[0].Reason = prev.Tools[0].Reason
			}
		} else if e != sql.ErrNoRows {
			return e
		}
		doc, e := json.Marshal(r)
		if e != nil {
			return e
		}
		if string(doc) == existing {
			continue // already up to date: no write
		}
		_, e = tx.ExecContext(ctx, `INSERT INTO inbox_requests(id,agent_id,kind,status,urgency,doc,created_at,updated_at,expires_at) VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET doc=excluded.doc,status=excluded.status,updated_at=excluded.updated_at`, r.ID, r.AgentID, r.Kind, r.Status, r.Urgency, string(doc), r.CreatedAt, r.UpdatedAt, r.ExpiresAt)
		if e != nil {
			return e
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if len(skipped.IDs) > 0 {
		return &skipped
	}
	return nil
}

// RequestLegacySync asks RunLegacySync for a pass soon. It never blocks:
// requests made before the next pass starts coalesce into that one pass.
func (s *Service) RequestLegacySync() {
	select {
	case s.legacySync <- struct{}{}:
	default:
	}
}

// RunLegacySync runs one pass at a time until ctx is done: a full
// LegacySyncPass on every RequestLegacySync signal, and every interval a
// pass that skips its transaction when the legacy tables are unchanged
// since the last committed pass (legacySyncIfChanged).
func (s *Service) RunLegacySync(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = LegacySyncInterval
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.legacySync:
			_ = s.LegacySyncPass(ctx)
		case <-t.C:
			s.legacySyncIfChanged(ctx)
		}
	}
}

// legacySignatureSQL summarises everything SyncLegacy reads, so the
// periodic pass can see that nothing changed without a write transaction.
// Every write to approval_requests adds a row, moves a row between
// statuses, or stamps decided_at or result_executed_at with the current
// time; a claim is added, changes state, or gets finished_at.
//
// The counts use SUM(CASE ... THEN 1 ELSE 0 END): Postgres has no SUM over
// a boolean comparison. SQLite gets the same integers as before.
const legacySignatureSQL = `SELECT
 (SELECT COUNT(*)||':'||COALESCE(SUM(CASE WHEN status='pending' THEN 1 ELSE 0 END),0)||':'||COALESCE(SUM(CASE WHEN status='allowed' THEN 1 ELSE 0 END),0)||':'||
   COALESCE(SUM(CASE WHEN status='denied' THEN 1 ELSE 0 END),0)||':'||COALESCE(SUM(CASE WHEN status='expired' THEN 1 ELSE 0 END),0)||':'||COALESCE(SUM(CASE WHEN status='cancelled' THEN 1 ELSE 0 END),0)||':'||
   COALESCE(MAX(created_at),0)||':'||COALESCE(MAX(decided_at),0)||':'||COALESCE(MAX(result_executed_at),0)
  FROM approval_requests)
 ||'|'||
 (SELECT COUNT(*)||':'||COALESCE(SUM(CASE WHEN state='running' THEN 1 ELSE 0 END),0)||':'||COALESCE(SUM(CASE WHEN state='outcome_unknown' THEN 1 ELSE 0 END),0)||':'||
   COALESCE(MAX(claimed_at),0)||':'||COALESCE(MAX(finished_at),0)
  FROM legacy_execution_claims)`

func (s *Service) legacySignature(ctx context.Context) (string, error) {
	var sig string
	err := s.db.QueryRowContext(ctx, legacySignatureSQL).Scan(&sig)
	return sig, err
}

// legacySyncIfChanged is the periodic pass. While the legacy tables are as
// the last committed pass found them, it only reports the loop alive (and
// in sync, when that pass skipped no row); otherwise it runs
// LegacySyncPass.
func (s *Service) legacySyncIfChanged(ctx context.Context) {
	if sig, err := s.legacySignature(ctx); err == nil {
		s.legacyMu.Lock()
		same, clean := s.legacySig != "" && sig == s.legacySig, s.legacyClean
		s.legacyMu.Unlock()
		if same {
			now := time.Now()
			opsmetrics.SetLoopLastSuccess(LegacySyncLoop, now)
			if clean {
				opsmetrics.SetLegacySyncSuccess(now)
			}
			return
		}
	}
	_ = s.LegacySyncPass(ctx)
}

// rememberLegacySync records the signature read before a committed pass.
// An empty sig (unreadable) makes the next periodic pass run.
func (s *Service) rememberLegacySync(sig string, clean bool) {
	s.legacyMu.Lock()
	s.legacySig, s.legacyClean = sig, clean
	s.legacyMu.Unlock()
}

// legacySkipLogMax caps how many skipped IDs one log line names.
const legacySkipLogMax = 20

// LegacySyncPass runs SyncLegacy once and reports the outcome: the success
// time when it was clean; an ERROR line and the failure counter when it
// failed or skipped rows. The caller continues either way.
func (s *Service) LegacySyncPass(ctx context.Context) error {
	s.legacyPasses.Add(1)
	// Read before the sync: a change after this read makes the next
	// signature differ, so no change is ever missed (at worst one extra
	// pass runs).
	sig, sigErr := s.legacySignature(ctx)
	if sigErr != nil {
		sig = ""
	}
	err := s.SyncLegacy(ctx)
	lg := logx.For("inbox")
	var skip *LegacySkipError
	switch {
	case err == nil:
		s.rememberLegacySync(sig, true)
		now := time.Now()
		opsmetrics.SetLegacySyncSuccess(now)
		opsmetrics.SetLoopLastSuccess(LegacySyncLoop, now)
	case errors.As(err, &skip):
		// Committed, but not in sync: the periodic pass stays quiet until
		// the tables change, and the success time stays stale.
		s.rememberLegacySync(sig, false)
		opsmetrics.IncLegacySyncFailure()
		opsmetrics.SetLoopLastSuccess(LegacySyncLoop, time.Now())
		ids := skip.IDs
		if len(ids) > legacySkipLogMax {
			ids = ids[:legacySkipLogMax]
		}
		lg.Error("legacy Inbox sync skipped malformed rows; every other row was committed",
			"skipped", len(skip.IDs), "approval_ids", ids, "first_error", skip.Errs[0].Error())
	case ctx.Err() != nil:
		// Shutdown cancelled the pass; the next process syncs at start.
	default:
		s.rememberLegacySync("", false)
		opsmetrics.IncLegacySyncFailure()
		lg.Error("legacy Inbox sync failed", "err", err.Error())
	}
	return err
}

func (s *Service) legacyDecisionTx(ctx context.Context, tx *sql.Tx, r *Request) error {
	status := r.Status
	if status == StatusApproved {
		status = "allowed"
	}
	// The legacy executor compares decided_by with the owner's user id
	// (Gateway.Execute for personal GitHub writes), so record it the way
	// the approval bus does rather than the display label.
	decidedBy := r.DecidedBy
	if r.DeciderUserID != "" {
		decidedBy = r.Decider().Legacy()
	}
	res, err := tx.ExecContext(ctx, `UPDATE approval_requests SET status=?,decided_by=?,decided_at=?,decided_via=?,decider_email=?,decider_name=?,decider_ref=? WHERE id=? AND status='pending'`, status, decidedBy, r.DecidedAt, r.DeciderVia, r.DeciderEmail, r.DeciderName, r.DeciderRef, r.ID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrConflict
	}
	return nil
}

// DecideLegacy is an Inbox-backed compatibility path for a pre-existing single
// call. It never creates missing context, a new request, or an executable grant.
func (s *Service) DecideLegacy(ctx context.Context, id, action string, d actor.Decider) (*Request, error) {
	if action != "allowed" && action != "denied" {
		return nil, ErrBadDecision
	}
	if err := s.SyncLegacy(ctx); err != nil {
		// Skipped rows do not block the others; a skipped id reads as
		// not found below.
		var skip *LegacySkipError
		if !errors.As(err, &skip) {
			return nil, err
		}
	}
	r, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if r.ExecutionMode != "legacy" || len(r.Tools) != 1 {
		return nil, fmt.Errorf("inbox_decision_required: submit the complete call-ID verdict mapping in Inbox")
	}
	var owner sql.NullString
	err = s.db.QueryRowContext(ctx, `SELECT owner_user_id FROM legacy_inbox_owners WHERE request_id=?`, id).Scan(&owner)
	if err != nil || !owner.Valid || d.UserID == "" || owner.String != d.UserID {
		return nil, fmt.Errorf("inbox_decision_required: sign in as the original owner and submit the complete decision in Inbox")
	}

	verdict := VerdictRejected
	if action == "allowed" {
		verdict = VerdictAccepted
	}
	sub := Decision{Action: "submit", RequestRevision: 1, SubmissionID: "legacy:" + id + ":" + action, Decider: d, Verdicts: map[string]CallVerdict{r.Tools[0].CallID: {Verdict: verdict}}}
	return s.Decide(ctx, id, sub)
}

// Original owners remain authoritative after removal of the original agent.
// Missing ownership evidence is recorded once as NULL and is never guessed.
// ON CONFLICT DO NOTHING keeps a concurrent snapshot of the same request
// (which NOT EXISTS cannot see) from failing on the primary key.
const legacyOwnerSnapshotSQL = `INSERT INTO legacy_inbox_owners(request_id,agent_id,owner_user_id,agent_name)
SELECT r.id,COALESCE(r.agent_id,''),
 CASE WHEN COALESCE(r.agent_id,'')=''
      THEN (SELECT id FROM users ORDER BY created_at,id LIMIT 1)
      WHEN a.id IS NOT NULL THEN a.owner_user
      WHEN (SELECT count(DISTINCT owner_user_id) FROM audit_events
            WHERE approval_id=r.id AND owner_user_id IS NOT NULL AND owner_user_id<>'')=1
      THEN (SELECT min(owner_user_id) FROM audit_events
            WHERE approval_id=r.id AND owner_user_id IS NOT NULL AND owner_user_id<>'')
 END,a.name
FROM approval_requests r LEFT JOIN agents a ON a.id=r.agent_id
WHERE NOT EXISTS (SELECT 1 FROM legacy_inbox_owners o WHERE o.request_id=r.id)
ON CONFLICT DO NOTHING`

// LegacyAgentNames restores access to imported history without restoring the
// deleted agent, its credential, or any permission to execute a new call.
func (s *Service) LegacyAgentNames(ctx context.Context, ownerID string) (map[string]string, error) {
	out := map[string]string{}
	if ownerID == "" {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT agent_id,COALESCE(agent_name,'') FROM legacy_inbox_owners WHERE owner_user_id=?`, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, name string
		if err = rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		if name == "" {
			name = "Legacy retired agent"
			if id == "" {
				name = "Legacy unpaired agent"
			}
		}
		out[id] = name
	}
	return out, rows.Err()
}

func (s *Service) OwnsLegacyRequest(ctx context.Context, requestID, ownerID string) bool {
	var found int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM legacy_inbox_owners WHERE request_id=? AND owner_user_id=?`, requestID, ownerID).Scan(&found)
	return ownerID != "" && err == nil && found == 1
}
