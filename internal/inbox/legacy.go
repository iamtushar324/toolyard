package inbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"github.com/tusharbhardwaj/toolyard/internal/actor"
	"strings"
)

// SyncLegacy represents the original approvals, including history, in the only
// human decision interface. Original IDs, actors, deadlines and arguments stay
// intact. Missing historical explanations remain missing, never invented.
func (s *Service) SyncLegacy(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, legacyOwnerSnapshotSQL); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,COALESCE(agent_id,''),upstream_name,tool_name,arguments,reason,status,COALESCE(decided_by,''),COALESCE(decided_at,0),created_at,expires_at,COALESCE(result_executed_at,0),COALESCE(result_is_error,0),COALESCE(result_error,''),COALESCE(decided_via,''),COALESCE(decider_email,''),COALESCE(decider_name,''),COALESCE(decider_ref,'') FROM approval_requests`)
	if err != nil {
		return err
	}
	var imported []Request
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
		if err = decoder.Decode(&raw); err != nil {
			rows.Close()
			return fmt.Errorf("legacy %s arguments: %w", r.ID, err)
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
		e := tx.QueryRowContext(ctx, `SELECT doc FROM inbox_requests WHERE id=?`, r.ID).Scan(&existing)
		if e == nil {
			var prev Request
			if e = json.Unmarshal([]byte(existing), &prev); e != nil {
				return e
			}
			if prev.ExecutionMode != "legacy" {
				return fmt.Errorf("legacy ID collision: %s", r.ID)
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
		_, e = tx.ExecContext(ctx, `INSERT INTO inbox_requests(id,agent_id,kind,status,urgency,doc,created_at,updated_at,expires_at) VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET doc=excluded.doc,status=excluded.status,updated_at=excluded.updated_at`, r.ID, r.AgentID, r.Kind, r.Status, r.Urgency, string(doc), r.CreatedAt, r.UpdatedAt, r.ExpiresAt)
		if e != nil {
			return e
		}
	}
	return tx.Commit()
}

func (s *Service) legacyDecisionTx(ctx context.Context, tx *sql.Tx, r *Request) error {
	status := r.Status
	if status == StatusApproved {
		status = "allowed"
	}
	res, err := tx.ExecContext(ctx, `UPDATE approval_requests SET status=?,decided_by=?,decided_at=?,decided_via=?,decider_email=?,decider_name=?,decider_ref=? WHERE id=? AND status='pending'`, status, r.DecidedBy, r.DecidedAt, r.DeciderVia, r.DeciderEmail, r.DeciderName, r.DeciderRef, r.ID)
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
		return nil, err
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
WHERE NOT EXISTS (SELECT 1 FROM legacy_inbox_owners o WHERE o.request_id=r.id)`

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
