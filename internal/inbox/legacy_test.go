package inbox

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/actor"
)

func TestLegacyInboxMigrationAndDecision(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	deadline := time.Now().Add(time.Hour).UnixMilli()
	now := time.Now().UnixMilli()
	for _, row := range []struct {
		id, status string
		done       any
	}{{"ap_pending", "pending", nil}, {"ap_done", "allowed", now}} {
		_, err := e.svc.db.Exec(`INSERT INTO approval_requests(id,agent_id,upstream_name,tool_name,arguments,reason,status,created_at,expires_at,result_executed_at) VALUES(?,'ag_legacy','fixture','fixture.write','{"path":"test.txt"}','Original explanation',?,?,?,?)`, row.id, row.status, now, deadline, row.done)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := e.svc.SyncLegacy(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.SyncLegacy(ctx); err != nil {
		t.Fatal(err)
	}
	r, err := e.svc.Get(ctx, "ap_pending")
	if err != nil || r.ExecutionMode != "legacy" || r.ExpiresAt != deadline || r.Tools[0].Summary != "Original explanation" {
		t.Fatal("migration changed original", err)
	}
	done, err := e.svc.Get(ctx, "ap_done")
	if err != nil || done.LegacyExecutionState != "succeeded" {
		t.Fatal("history missing", err)
	}
	var calls atomic.Int32
	e.svc.opts.LegacyDecided = func(context.Context, string) { calls.Add(1) }
	d := Decision{Action: "submit", RequestRevision: r.Revision, SubmissionID: "legacy-once", Verdicts: map[string]CallVerdict{"call_1": {Verdict: VerdictAccepted, Reason: "Accept test file"}}, Note: "Original scope"}
	decided, err := e.svc.Decide(ctx, r.ID, d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.svc.Decide(ctx, r.ID, d); err != nil {
		t.Fatal("retry", err)
	}
	if calls.Load() != 1 || decided.Status != StatusApproved {
		t.Fatal("legacy dispatch repeated")
	}
	var status string
	e.svc.db.QueryRow(`SELECT status FROM approval_requests WHERE id=?`, r.ID).Scan(&status)
	if status != "allowed" {
		t.Fatal("legacy executor cannot observe decision")
	}
	var grants int
	e.svc.db.QueryRow(`SELECT count(*) FROM inbox_grants WHERE request_id=?`, r.ID).Scan(&grants)
	if grants != 0 {
		t.Fatal("legacy executable grant duplicated")
	}
	if err = e.svc.SyncLegacy(ctx); err != nil {
		t.Fatal(err)
	}
	r, _ = e.svc.Get(ctx, r.ID)
	if r.OwnerNote != d.Note || r.Tools[0].Reason != "Accept test file" || r.DecisionSubmissionID != d.SubmissionID {
		raw, _ := json.Marshal(r)
		t.Fatalf("sync lost Inbox verdicts %s", raw)
	}
}

// A personal GitHub write runs only when decided_by is the owner's user id
// (Gateway.Execute), so an Inbox decision must record the id, not a label.
func TestLegacyDecisionRecordsDeciderUserID(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()
	_, err := e.svc.db.Exec(`INSERT INTO approval_requests(id,agent_id,upstream_name,tool_name,arguments,reason,status,created_at,expires_at) VALUES('ap_owner','ag_legacy','fixture','fixture.write','{"path":"test.txt"}','Owner decides','pending',?,?)`, now, time.Now().Add(time.Hour).UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	if err = e.svc.SyncLegacy(ctx); err != nil {
		t.Fatal(err)
	}
	r, err := e.svc.Get(ctx, "ap_owner")
	if err != nil {
		t.Fatal(err)
	}
	d := Decision{Action: "submit", RequestRevision: r.Revision, SubmissionID: "legacy-owner", By: "Owner Label", Decider: actor.Decider{UserID: "u_owner", Name: "Owner Label", Via: actor.ViaDashboard}, Verdicts: map[string]CallVerdict{"call_1": {Verdict: VerdictAccepted}}}
	if _, err = e.svc.Decide(ctx, r.ID, d); err != nil {
		t.Fatal(err)
	}
	var decidedBy, deciderName string
	if err = e.svc.db.QueryRow(`SELECT decided_by, COALESCE(decider_name,'') FROM approval_requests WHERE id=?`, r.ID).Scan(&decidedBy, &deciderName); err != nil {
		t.Fatal(err)
	}
	if decidedBy != "u_owner" || deciderName != "Owner Label" {
		t.Fatalf("decided_by=%q decider_name=%q", decidedBy, deciderName)
	}
}
