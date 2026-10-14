package inbox

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/store"
	"github.com/tusharbhardwaj/toolyard/internal/store/storetest"
)

// lockWaitOrDone waits until another session of this test database waits
// on a lock (it returns false), or done delivers first (it returns true
// and the value). It fails the test after 10 seconds.
func lockWaitOrDone(t *testing.T, db *store.DB, done <-chan error) (bool, error) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		select {
		case err := <-done:
			return true, err
		default:
		}
		var n int
		if err := db.QueryRowContext(context.Background(), `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			return false, nil
		}
		if time.Now().After(deadline) {
			t.Fatal("nothing waited on a lock and nothing finished within 10s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A mutation that read the pending document (a snooze, reminder, summary
// or voice note) must never write it back over a decision that committed
// in between. On Postgres mutateTx reads the row FOR UPDATE, so the
// decision waits for the mutation and then decides the mutated document:
// both changes survive. (SQLite's one connection serializes them.)
func TestMutateTxDoesNotRevertConcurrentDecision(t *testing.T) {
	storetest.SkipUnlessPostgres(t)
	e := newEnv(t)
	ctx := context.Background()
	r := submitDeploy(t, e, "ag_1")

	read := make(chan struct{})
	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	defer releaseOnce()
	snoozeUntil := e.svc.now().Add(time.Hour).UnixMilli()
	mutated := make(chan error, 1)
	go func() {
		mutated <- e.svc.mutate(ctx, r.ID, func(cur *Request) error {
			close(read)
			<-release
			cur.SnoozedUntil = snoozeUntil
			return nil
		})
	}()
	<-read // the mutation holds the pending document

	decided := make(chan error, 1)
	go func() {
		_, err := e.svc.Decide(ctx, r.ID, Decision{Action: "deny", Note: "Not this week"})
		decided <- err
	}()
	// Fixed: the decision waits on the row lock. Broken: it commits now,
	// and the mutation below writes its stale pending copy over it.
	finished, decideErr := lockWaitOrDone(t, e.svc.db, decided)
	releaseOnce()
	if err := <-mutated; err != nil {
		t.Fatalf("mutation: %v", err)
	}
	if !finished {
		decideErr = <-decided
	}
	if decideErr != nil {
		t.Fatalf("decision: %v", decideErr)
	}
	got, err := e.svc.Get(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusDenied {
		t.Fatalf("status = %q after a concurrent mutation, want %q: the decision was reverted", got.Status, StatusDenied)
	}
	if got.SnoozedUntil != snoozeUntil {
		t.Fatalf("snoozed_until = %d, want %d: the decision lost the mutation", got.SnoozedUntil, snoozeUntil)
	}
	if got.Revision != r.Revision+1 {
		t.Fatalf("revision = %d, want %d", got.Revision, r.Revision+1)
	}
}

// Two submissions with the same client_request_id that miss each other's
// lookup (another process, where submitMu does not apply) both get one
// request: the unique index refuses the second insert, and the second
// caller gets the answer a sequential retry gets. Different content under
// the same key is ErrConflict, as for a sequential retry.
func TestConcurrentClientRequestIDSubmissionsReturnOneRequest(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	other, err := New(ctx, Options{DB: e.svc.db, Catalog: testCatalog, Now: e.svc.now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(other.Flush)
	keyed := func(key string) *Submission {
		s := deploySubmission()
		s.ClientRequestID = key
		return s
	}

	// The other process stores the same submission between this caller's
	// lookup and its insert.
	var first *SubmitResult
	var firstErr error
	e.svc.submitInsertHook = func() {
		e.svc.submitInsertHook = nil
		first, firstErr = other.Submit(ctx, "ag_1", keyed("deploy-once"))
	}
	second, err := e.svc.Submit(ctx, "ag_1", keyed("deploy-once"))
	if firstErr != nil || first == nil || !first.OK || first.RequestID == "" {
		t.Fatalf("first submission: %+v %v", first, firstErr)
	}
	if err != nil || second == nil || !second.OK {
		t.Fatalf("racing identical submission: %+v %v", second, err)
	}
	if second.RequestID != first.RequestID || second.Status != first.Status || second.ExpiresAt != first.ExpiresAt {
		t.Fatalf("racing submission got %+v, want the stored request %+v", second, first)
	}
	retry, err := e.svc.Submit(ctx, "ag_1", keyed("deploy-once"))
	if err != nil || retry.RequestID != first.RequestID {
		t.Fatalf("sequential retry: %+v %v", retry, err)
	}
	stored, err := e.svc.List(ctx, ListFilter{AgentID: "ag_1"})
	if err != nil || len(stored) != 1 {
		t.Fatalf("stored %d requests (%v), want 1", len(stored), err)
	}

	// Different content under one key: the racing caller gets ErrConflict.
	e.svc.submitInsertHook = func() {
		e.svc.submitInsertHook = nil
		changed := keyed("deploy-twice")
		changed.Title = "Ship billing v3 to production"
		first, firstErr = other.Submit(ctx, "ag_1", changed)
	}
	if _, err = e.svc.Submit(ctx, "ag_1", keyed("deploy-twice")); !errors.Is(err, ErrConflict) {
		t.Fatalf("racing submission with other content: %v, want ErrConflict", err)
	}
	if firstErr != nil || first == nil || !first.OK {
		t.Fatalf("other content first: %+v %v", first, firstErr)
	}
	e.svc.Flush()
	other.Flush()
}

// A NUL character anywhere in a request must not break its writes on
// Postgres, where jsonb refuses the \u0000 escape that json.Marshal writes
// and the client_request_id index casts the doc on every write.
func TestRequestWithNULCharacterSubmitsAndDecides(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	s := deploySubmission()
	s.ClientRequestID = "nul-check"
	s.Message += " before\x00after"
	res, err := e.svc.Submit(ctx, "ag_1", s)
	if err != nil || !res.OK {
		t.Fatalf("submit: %v %+v", err, res)
	}
	e.svc.Flush()
	again, err := e.svc.Submit(ctx, "ag_1", s)
	if err != nil || again.RequestID != res.RequestID {
		t.Fatalf("retry of a request with a NUL character: %+v %v", again, err)
	}
	got, err := e.decide(ctx, res.RequestID, Decision{Action: "deny", Note: "not\x00now", By: "owner"})
	if err != nil {
		t.Fatalf("decide a request with a NUL character: %v", err)
	}
	if got.Status == StatusPending {
		t.Fatalf("status still pending after the denial")
	}
}
