package inbox

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/actor"
)

type notePasskeyGate struct{ calls int }

func (g *notePasskeyGate) CheckEnabled(context.Context) (bool, error) {
	g.calls++
	return true, nil
}
func (g *notePasskeyGate) VerifyCredential(context.Context, string, *PasskeyAssertion) (string, error) {
	g.calls++
	return "test-credential", nil
}

type decisionNoteSnapshot struct {
	doc      string
	counts   map[string]int
	realtime int
}

func snapshotDecisionNote(t *testing.T, e *testEnv, id string) decisionNoteSnapshot {
	t.Helper()
	snapshot := decisionNoteSnapshot{counts: map[string]int{}}
	if err := e.svc.db.QueryRow(`SELECT doc FROM inbox_requests WHERE id=?`, id).Scan(&snapshot.doc); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"inbox_decision_audit", "inbox_grants", "batch_test_events", "inbox_pushes"} {
		var count int
		if err := e.svc.db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		snapshot.counts[table] = count
	}
	e.mu.Lock()
	snapshot.realtime = len(e.events)
	e.mu.Unlock()
	return snapshot
}

func newDecisionNoteFixture(t *testing.T, kind, action string) (*testEnv, *Request, Decision) {
	t.Helper()
	e := newEnv(t)
	installTestCallbacks(t, e)
	submission := batchSubmission()
	switch kind {
	case KindQuestion, KindBlocker:
		submission = &Submission{SchemaVersion: 2, Kind: kind, Prompt: "What must the agent do next?", Question: &Question{Type: "free_text"}}
	case KindUpdate:
		submission = &Submission{Kind: kind, Title: "The checks finished", Summary: "The agent finished the checks.", Message: "The deployment checks succeeded.", Audio: Audio{Script: "The checks finished."}, Urgency: UrgencyFYI}
	}
	submission.CallbackRef = "cb_authorized"
	result, err := e.svc.Submit(context.Background(), "ag_note", submission)
	if err != nil || !result.OK {
		t.Fatalf("submit: %+v %v", result, err)
	}
	e.svc.Flush()
	r, err := e.svc.Get(context.Background(), result.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	d := Decision{Action: action, RequestRevision: r.Revision, SubmissionID: "note_submission", Decider: actor.Decider{UserID: "u_owner", Via: actor.ViaDashboard}}
	if action == "approve" {
		d = batchDecision(r, "note_submission", VerdictAccepted, VerdictRejected)
	} else if action == "answer" {
		d.Response = &Response{Text: "Continue with the requested checks."}
	} else if action == "snooze" {
		d.SnoozeMinutes = 5
	}
	return e, r, d
}

func TestDecisionNoteLimitAllActions(t *testing.T) {
	for _, tc := range []struct{ kind, action string }{
		{KindAccess, "approve"}, {KindAccess, "deny"}, {KindAccess, "return"}, {KindAccess, "snooze"},
		{KindQuestion, "answer"}, {KindBlocker, "answer"}, {KindUpdate, "read"},
	} {
		t.Run(tc.kind+"_"+tc.action, func(t *testing.T) {
			e, r, d := newDecisionNoteFixture(t, tc.kind, tc.action)
			gate := &notePasskeyGate{}
			e.svc.opts.Passkeys = gate
			before := snapshotDecisionNote(t, e, r.ID)
			d.Note = strings.Repeat("🙂", 10001)
			if _, err := e.svc.Decide(context.Background(), r.ID, d); err == nil || !strings.HasPrefix(err.Error(), "note:") || !strings.Contains(err.Error(), "10000") {
				t.Fatalf("oversized note lacks its field correction: %v", err)
			}
			if got := snapshotDecisionNote(t, e, r.ID); !reflect.DeepEqual(got, before) {
				t.Fatalf("refused note changed the request, grants, audit, callback, or notification: before=%+v after=%+v", before, got)
			}
			if gate.calls != 0 {
				t.Fatalf("refused note contacted the passkey gate %d times", gate.calls)
			}
			// The limit counts Unicode code points, rather than UTF-8 bytes
			// or UTF-16 units. No truncation changes the accepted note.
			e.svc.opts.Passkeys = nil
			d.Note = strings.Repeat("🙂", 10000)
			got, err := e.svc.Decide(context.Background(), r.ID, d)
			if err != nil || got.OwnerNote != d.Note {
				t.Fatalf("exact-limit Unicode note was not preserved: %v", err)
			}
			if tc.action != "snooze" {
				var eventNote string
				if err := e.svc.db.QueryRow(`SELECT json_extract(doc,'$.owner_note') FROM batch_test_events WHERE request_id=?`, r.ID).Scan(&eventNote); err != nil || eventNote != d.Note {
					t.Fatalf("callback lost the accepted note: %v", err)
				}
			}
		})
	}
}

func TestDecisionNoteHistoricalOversizedReplayIsReadOnly(t *testing.T) {
	for _, tc := range []struct{ kind, action, status string }{{KindQuestion, "answer", StatusAnswered}, {KindAccess, "deny", StatusDenied}} {
		t.Run(tc.kind+"_"+tc.action, func(t *testing.T) {
			e, r, d := newDecisionNoteFixture(t, tc.kind, tc.action)
			d.Note = "Historical note"
			if _, err := e.svc.Decide(context.Background(), r.ID, d); err != nil {
				t.Fatal(err)
			}
			// Rehearse a pre-limit record only in this consistent temporary DB.
			// Existing oversized history must remain readable and idempotent.
			d.Note = strings.Repeat("🙂", 10001)
			if err := e.svc.mutate(context.Background(), r.ID, func(existing *Request) error {
				existing.OwnerNote = d.Note
				if tc.kind == KindAccess {
					existing.DecisionFingerprint = decisionFingerprint(d)
				} else {
					existing.AnswerFingerprint = answerFingerprint(d)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			before := snapshotDecisionNote(t, e, r.ID)
			e.advance(time.Minute)
			gate := &notePasskeyGate{}
			e.svc.opts.Passkeys = gate
			got, err := e.svc.Decide(context.Background(), r.ID, d)
			if err != nil || !got.Replayed || got.OwnerNote != d.Note || got.ID != r.ID || got.Status != tc.status {
				t.Fatalf("historical decision retry failed: %+v %v", got, err)
			}
			if got := snapshotDecisionNote(t, e, r.ID); !reflect.DeepEqual(got, before) || gate.calls != 0 {
				t.Fatal("matching historical replay changed persistent state or called passkeys")
			}
			d.Note += "changed"
			if _, err := e.svc.Decide(context.Background(), r.ID, d); !errors.Is(err, ErrConflict) {
				t.Fatalf("changed-content historical retry lacked conflict: %v", err)
			}
			d.SubmissionID = "new_oversized_submission"
			if _, err := e.svc.Decide(context.Background(), r.ID, d); err == nil || !strings.HasPrefix(err.Error(), "note:") {
				t.Fatalf("new oversized submission reused historical decision: %v", err)
			}
			if got := snapshotDecisionNote(t, e, r.ID); !reflect.DeepEqual(got, before) || gate.calls != 0 {
				t.Fatalf("historical replay or refusal changed persistent state or called passkeys: %d", gate.calls)
			}
		})
	}
}
