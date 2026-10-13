package inbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"github.com/google/uuid"
	"strings"
	"unicode/utf8"
)

func (s *Service) SetCallbacks(callbacks DecisionCallbacks) { s.opts.Callbacks = callbacks }

func validateVerdicts(r *Request, d Decision) error {
	if len(d.Verdicts) != len(r.Tools) {
		return fmt.Errorf("verdicts: supply one accepted/rejected verdict for every call ID (%d)", len(r.Tools))
	}
	if utf8.RuneCountInString(d.Note) > 10000 {
		return fmt.Errorf("note: max 10000 characters")
	}
	known := map[string]bool{}
	for _, t := range r.Tools {
		known[t.CallID] = true
		v, ok := d.Verdicts[t.CallID]
		if !ok {
			return fmt.Errorf("verdicts.%s: missing verdict", t.CallID)
		}
		if v.Verdict != VerdictAccepted && v.Verdict != VerdictRejected {
			return fmt.Errorf("verdicts.%s.verdict: use accepted or rejected", t.CallID)
		}
		if utf8.RuneCountInString(v.Reason) > 4000 {
			return fmt.Errorf("verdicts.%s.reason: max 4000 characters", t.CallID)
		}
	}
	for id := range d.Verdicts {
		if !known[id] {
			return fmt.Errorf("verdicts.%s: unknown call ID", id)
		}
	}
	return nil
}

func decisionFingerprint(d Decision) string {
	// Identity and transient passkey signatures do not change decision content.
	return hashJSON(struct {
		Action   string
		Revision int
		Verdicts map[string]CallVerdict
		Allow    []bool
		Note     string
		Params   map[int]map[string]Constraint
		TTL      int
	}{d.Action, d.RequestRevision, d.Verdicts, d.Allow, strings.TrimSpace(d.Note), d.Params, d.TTLSeconds})
}

func (s *Service) recordDecisionAuditTx(ctx context.Context, tx *sql.Tx, r *Request) error {
	doc, err := json.Marshal(struct {
		Status  string        `json:"status"`
		Note    string        `json:"note,omitempty"`
		Tools   []ToolRequest `json:"calls,omitempty"`
		Decider any           `json:"decider"`
	}{r.Status, r.OwnerNote, r.Tools, r.Decider()})
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO inbox_decision_audit(id,request_id,revision,submission_id,actor_user_id,decision,recorded_at) VALUES(?,?,?,?,?,?,?)`, "ida_"+uuid.NewString(), r.ID, r.Revision, nullStr(r.DecisionSubmissionID), nullStr(r.DeciderUserID), string(doc), s.now().UnixMilli())
	return err
}

// BlobBelongsToAgent checks evidence ownership before an authenticated download.
func (s *Service) BlobBelongsToAgent(ctx context.Context, agentID, sha string) bool {
	var found int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM inbox_requests r,json_tree(r.doc) j WHERE r.agent_id=? AND j.key IN ('blob','poster_blob') AND j.type='text' AND j.value=? LIMIT 1`, agentID, sha).Scan(&found)
	return err == nil && found == 1
}
func (s *Service) Grant(ctx context.Context, id string) (*Grant, error) {
	g, _, err := s.getGrantWithHash(ctx, id)
	if err != nil {
		return nil, ErrNotFound
	}
	return g, nil
}
