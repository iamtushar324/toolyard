package inbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/actor"
)

type testCallbacks struct{ fail bool }

func (c *testCallbacks) RegisterTx(ctx context.Context, tx *sql.Tx, r *Request, ref string) error {
	if ref == "" {
		return nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO batch_test_subscriptions(request_id,reference) VALUES(?,?)`, r.ID, ref)
	return err
}
func (c *testCallbacks) DecisionTx(ctx context.Context, tx *sql.Tx, r *Request) error {
	if c.fail {
		return errors.New("outbox unavailable")
	}
	doc, _ := json.Marshal(r)
	_, err := tx.ExecContext(ctx, `INSERT INTO batch_test_events(request_id,revision,doc) SELECT ?,?,? WHERE EXISTS(SELECT 1 FROM batch_test_subscriptions WHERE request_id=?)`, r.ID, r.Revision, string(doc), r.ID)
	return err
}
func installTestCallbacks(t *testing.T, e *testEnv) *testCallbacks {
	t.Helper()
	_, err := e.svc.db.Exec(`CREATE TABLE batch_test_subscriptions(request_id TEXT PRIMARY KEY,reference TEXT);CREATE TABLE batch_test_events(request_id TEXT,revision INTEGER,doc TEXT,UNIQUE(request_id,revision))`)
	if err != nil {
		t.Fatal(err)
	}
	cb := &testCallbacks{}
	e.svc.SetCallbacks(cb)
	return cb
}

func batchSubmission() *Submission {
	sub := deploySubmission()
	sub.Tools = sub.Tools[:2]
	sub.Tools[1] = sub.Tools[0]
	sub.Tools[0].CallID = "file_alpha"
	sub.Tools[1].CallID = "file_beta"
	sub.Tools[0].Params = map[string]any{"env": "stage", "migration": "0042"}
	sub.Tools[1].Params = map[string]any{"env": "stage", "migration": "0043"}
	return sub
}
func submitBatch(t *testing.T, e *testEnv) *Request {
	t.Helper()
	result, err := e.svc.Submit(context.Background(), "ag_1", batchSubmission())
	if err != nil || !result.OK {
		t.Fatalf("submit: %+v %v", result, err)
	}
	e.svc.Flush()
	r, err := e.svc.Get(context.Background(), result.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func batchDecision(r *Request, id string, a, b string) Decision {
	return Decision{Action: "submit", RequestRevision: r.Revision, SubmissionID: id, Verdicts: map[string]CallVerdict{"file_alpha": {Verdict: a, Reason: "Alpha is needed for the test."}, "file_beta": {Verdict: b, Reason: "Beta can wait until the test finishes."}}, Note: "Keep the second call out of this test.", Decider: actor.Decider{UserID: "u_owner", Via: actor.ViaDashboard}}
}

func TestBatchMixedRepeatedToolAndCompleteCallback(t *testing.T) {
	e := newEnv(t)
	installTestCallbacks(t, e)
	sub := batchSubmission()
	sub.CallbackRef = "cb_authorized"
	result, err := e.svc.Submit(context.Background(), "ag_1", sub)
	if err != nil || !result.OK {
		t.Fatalf("submit: %+v %v", result, err)
	}
	e.svc.Flush()
	r, _ := e.svc.Get(context.Background(), result.RequestID)
	var n int
	if err := e.svc.db.QueryRow(`SELECT count(*) FROM batch_test_events`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("callback before decision: %d %v", n, err)
	}
	d := batchDecision(r, "submission_1", VerdictRejected, VerdictAccepted)
	got, err := e.svc.Decide(context.Background(), r.ID, d)
	if err != nil {
		t.Fatal(err)
	}
	if got.Tools[0].GrantID != "" || got.Tools[0].Verdict != VerdictRejected || got.Tools[0].Reason != d.Verdicts["file_alpha"].Reason || got.Tools[1].GrantID == "" || got.Revision != 2 {
		t.Fatalf("mixed outcome: %+v", got)
	}
	// The rejected first call was marked required, but the decision is complete.
	replay, err := e.svc.Decide(context.Background(), r.ID, d)
	if err != nil || !replay.Replayed || replay.Tools[1].GrantID != got.Tools[1].GrantID {
		t.Fatalf("retry: %+v %v", replay, err)
	}
	changed := d
	changed.Note = "Different content"
	if _, err := e.svc.Decide(context.Background(), r.ID, changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed retry: %v", err)
	}
	if err := e.svc.db.QueryRow(`SELECT count(*) FROM batch_test_events`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("logical callbacks: %d %v", n, err)
	}
	var doc string
	if err := e.svc.db.QueryRow(`SELECT doc FROM batch_test_events`).Scan(&doc); err != nil {
		t.Fatal(err)
	}
	var event Request
	json.Unmarshal([]byte(doc), &event)
	if len(event.Tools) != 2 || event.Tools[0].Reason == "" || event.Tools[1].Reason == "" || event.OwnerNote != d.Note {
		t.Fatalf("incomplete callback decision: %s", doc)
	}
	grants, _ := e.svc.ListGrants(context.Background(), "", "ag_1", 10)
	if len(grants) != 1 {
		t.Fatalf("grants: %+v", grants)
	}
}

func TestBatchAllRejectedAndDecisionRace(t *testing.T) {
	e := newEnv(t)
	r := submitBatch(t, e)
	d := batchDecision(r, "all_rejected", VerdictRejected, VerdictRejected)
	var wg sync.WaitGroup
	var mu sync.Mutex
	success := 0
	conflict := 0
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			decision := d
			if i == 1 {
				decision.SubmissionID = "competing_decision"
			}
			_, err := e.svc.Decide(context.Background(), r.ID, decision)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				success++
			} else if errors.Is(err, ErrNotPending) {
				conflict++
			} else {
				t.Errorf("race error: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if success != 1 || conflict != 1 {
		t.Fatalf("race results %d/%d", success, conflict)
	}
	got, _ := e.svc.Get(context.Background(), r.ID)
	grants, _ := e.svc.ListGrants(context.Background(), "", "ag_1", 10)
	if got.Status != StatusDenied || len(grants) != 0 || got.GrantsExpire != 0 {
		t.Fatalf("all rejected: %+v %+v", got, grants)
	}
}

func TestBatchAtomicOutboxFailureAndRegistration(t *testing.T) {
	e := newEnv(t)
	cb := installTestCallbacks(t, e)
	sub := batchSubmission()
	sub.CallbackRef = "cb_authorized"
	res, err := e.svc.Submit(context.Background(), "ag_1", sub)
	if err != nil || !res.OK {
		t.Fatal(err)
	}
	e.svc.Flush()
	r, _ := e.svc.Get(context.Background(), res.RequestID)
	cb.fail = true
	if _, err = e.svc.Decide(context.Background(), r.ID, batchDecision(r, "first", VerdictAccepted, VerdictRejected)); err == nil {
		t.Fatal("failed outbox must roll back decision")
	}
	got, _ := e.svc.Get(context.Background(), r.ID)
	grants, _ := e.svc.ListGrants(context.Background(), "", "ag_1", 10)
	var n int
	e.svc.db.QueryRow(`SELECT count(*) FROM inbox_decision_audit`).Scan(&n)
	if got.Status != StatusPending || len(grants) != 0 || n != 0 {
		t.Fatalf("partial transaction: %+v %+v %d", got, grants, n)
	}
	cb.fail = false
	if _, err = e.svc.Decide(context.Background(), r.ID, batchDecision(r, "retry", VerdictAccepted, VerdictRejected)); err != nil {
		t.Fatal(err)
	}
}

func TestBatchContextIDsScopeAndDependencies(t *testing.T) {
	cases := []struct {
		name, path string
		edit       func(*Submission)
	}{{"objective", "task.objective", func(s *Submission) { s.Task.Objective = "TBD" }}, {"placeholder", "tools[0].summary", func(s *Submission) { s.Tools[0].Summary = "<explain why>" }}, {"write_context", "tools[0].undo", func(s *Submission) { s.Tools[0].Undo = "" }}, {"duplicate", "tools[1].call_id", func(s *Submission) { s.Tools[1].CallID = s.Tools[0].CallID }}, {"unbounded", "tools[0].params.env", func(s *Submission) { s.Tools[0].Params["env"] = map[string]any{"any": true} }}, {"cycle", "tools.after", func(s *Submission) {
		s.Tools[0].After = []string{"file_beta"}
		s.Tools[1].After = []string{"file_alpha"}
	}}, {"reference", "tools[0].after[0]", func(s *Submission) { s.Tools[0].After = []string{"db.migrate"} }}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := batchSubmission()
			tc.edit(s)
			_, probs, _ := Validate(context.Background(), testCatalog, "ag_1", s)
			found := false
			for _, p := range probs {
				if p.Path == tc.path {
					found = true
				}
			}
			if !found {
				t.Fatalf("missing %s: %+v", tc.path, probs)
			}
		})
	}
}

func TestBatchGrantRecoveryClaimAndUnknownOutcome(t *testing.T) {
	e := newEnv(t)
	r := submitBatch(t, e)
	_, err := e.svc.Decide(context.Background(), r.ID, batchDecision(r, "approved", VerdictAccepted, VerdictRejected))
	if err != nil {
		t.Fatal(err)
	}
	first, _ := e.svc.Status(context.Background(), "ag_1", []string{r.ID})
	second, _ := e.svc.Status(context.Background(), "ag_1", []string{r.ID})
	tok := first[0].Tools[0].Grant
	if tok == "" || tok != second[0].Tools[0].Grant {
		t.Fatal("lost response cannot recover same token")
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var claimed *Grant
	success := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			g, err := e.svc.Redeem(context.Background(), tok, "ag_1", "db.migrate", map[string]any{"env": "stage", "migration": "0042"})
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				success++
				claimed = g
			} else if !errors.Is(err, ErrGrantUsed) {
				t.Errorf("redeem: %v", err)
			}
		}()
	}
	wg.Wait()
	if success != 1 {
		t.Fatalf("claims %d", success)
	}
	running, err := e.svc.Execution(context.Background(), claimed.ID, "ag_1")
	if err != nil || running.State != ExecutionRunning || running.CallID != "file_alpha" {
		t.Fatalf("execution claim: %+v %v", running, err)
	}
	if n, err := e.svc.RecoverExecutions(context.Background()); err != nil || n != 1 {
		t.Fatalf("recover: %d %v", n, err)
	}
	current, _ := e.svc.Status(context.Background(), "ag_1", []string{r.ID})
	if current[0].Tools[0].Grant != "" || current[0].Tools[0].Execution.State != ExecutionUnknown {
		t.Fatalf("unknown: %+v", current)
	}
	if _, err := e.svc.Redeem(context.Background(), tok, "ag_1", "db.migrate", map[string]any{"env": "stage", "migration": "0042"}); !errors.Is(err, ErrGrantUsed) {
		t.Fatalf("unknown execution repeated: %v", err)
	}
	if _, err := e.svc.Execution(context.Background(), claimed.ID, "ag_2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong agent: %v", err)
	}
}

func TestBatchSeparateDeadlineAndGrantLifetime(t *testing.T) {
	e := newEnv(t)
	r := submitBatch(t, e)
	if r.ExpiresAt-r.CreatedAt != int64((24*time.Hour)/time.Millisecond) || r.TTLSeconds != 1800 {
		t.Fatalf("deadlines %+v", r)
	}
	e.advance(23 * time.Hour)
	got, err := e.svc.Decide(context.Background(), r.ID, batchDecision(r, "late", VerdictAccepted, VerdictRejected))
	if err != nil {
		t.Fatal(err)
	}
	if got.GrantsExpire-got.DecidedAt != 1800000 {
		t.Fatal("grant lifetime must start at decision")
	}
	before, _ := e.svc.Status(context.Background(), "ag_1", []string{r.ID})
	token := before[0].Tools[0].Grant
	e.advance(31 * time.Minute)
	after, _ := e.svc.Status(context.Background(), "ag_1", []string{r.ID})
	if after[0].Tools[0].Grant != "" {
		t.Fatal("expired grant recovered")
	}
	if _, err := e.svc.Redeem(context.Background(), token, "ag_1", "db.migrate", map[string]any{"env": "stage", "migration": "0042"}); !errors.Is(err, ErrGrantExpired) {
		t.Fatalf("expired: %v", err)
	}
}
