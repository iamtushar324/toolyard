package callbacks

import (
	"database/sql"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/store/storetest"
)

// finish is fenced on its claim: when a newer claim owns the event (the
// stale claim's lease expired and DeliverOne claimed it again), the stale
// attempt is recorded in the history but changes nothing on the event.
func TestCallbackFinishWithStaleAttemptLeavesEvent(t *testing.T) {
	s, db := fixture(t)
	ctx := t.Context()
	r, err := s.Register(ctx, "ag_owner", "", "hook", "https://receiver.example/", testSecret(), false)
	if err != nil {
		t.Fatal(err)
	}
	claimUntil := time.Now().Add(time.Minute).UnixMilli()
	// Attempt 2 holds the claim now.
	if _, err = db.Exec(`INSERT INTO callback_outbox(id,request_id,decision_revision,receiver_id,receiver_revision,payload,status,attempts,next_attempt_at,claim_until) VALUES('evt_stale','rq',2,?,1,'{}','delivering',2,0,?)`, r.ID, claimUntil); err != nil {
		t.Fatal(err)
	}
	state := func() (string, int, int64, sql.NullInt64) {
		t.Helper()
		var status string
		var attempts int
		var claim int64
		var delivered sql.NullInt64
		if err := db.QueryRow(`SELECT status,attempts,claim_until,delivered_at FROM callback_outbox WHERE id='evt_stale'`).Scan(&status, &attempts, &claim, &delivered); err != nil {
			t.Fatal(err)
		}
		return status, attempts, claim, delivered
	}
	// The stale attempt 1 ends in a terminal failure: without the fence it
	// would fail the event that attempt 2 is delivering.
	if err = s.finish(ctx, "evt_stale", 1, 410, "http_410", true); err != nil {
		t.Fatal(err)
	}
	if status, attempts, claim, delivered := state(); status != "delivering" || attempts != 2 || claim != claimUntil || delivered.Valid {
		t.Fatalf("stale finish changed the event: status=%s attempts=%d claim_until=%d delivered=%v", status, attempts, claim, delivered)
	}
	// The current claim still finishes.
	if err = s.finish(ctx, "evt_stale", 2, 200, "delivered", false); err != nil {
		t.Fatal(err)
	}
	if status, _, claim, delivered := state(); status != "delivered" || claim != 0 || !delivered.Valid {
		t.Fatalf("current finish: status=%s claim_until=%d delivered=%v", status, claim, delivered)
	}
	var n int
	if err = db.QueryRow(`SELECT count(*) FROM callback_attempts WHERE event_id='evt_stale'`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("attempt history has %d rows (%v), want 2 (stale attempt 1 and attempt 2)", n, err)
	}
}

// A repeated Ack answers success and records nothing new, whether it runs
// after the first one (every engine) or races it (below).
func TestPullSecondAckIsANoOp(t *testing.T) {
	s, db := fixture(t)
	ctx := t.Context()
	r, err := s.RegisterPull(ctx, "agent", "mac", "session", testSecret())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO callback_outbox(id,request_id,decision_revision,receiver_id,receiver_revision,payload,next_attempt_at) VALUES('evt-ack','req',1,?,1,'{}',0)`, r.ID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err = s.Ack(ctx, "agent", r.ID, "evt-ack"); err != nil {
			t.Fatalf("ack %d: %v", i+1, err)
		}
	}
	var status string
	var attempts, history int
	if err = db.QueryRow(`SELECT status,attempts FROM callback_outbox WHERE id='evt-ack'`).Scan(&status, &attempts); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow(`SELECT count(*) FROM callback_attempts WHERE event_id='evt-ack'`).Scan(&history); err != nil {
		t.Fatal(err)
	}
	if status != "delivered" || attempts != 1 || history != 1 {
		t.Fatalf("after two acks: status=%s attempts=%d history=%d, want delivered 1 1", status, attempts, history)
	}
}

// Two Acks of one event race (Postgres: both read pending before either
// writes). The one that loses the update answers success, as a repeated
// Ack does, instead of failing on the duplicate attempt row.
func TestPullConcurrentAckIsANoOp(t *testing.T) {
	storetest.SkipUnlessPostgres(t) // the hook uses a second connection while Ack holds its transaction
	s, db := fixture(t)
	ctx := t.Context()
	r, err := s.RegisterPull(ctx, "agent", "mac", "session", testSecret())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO callback_outbox(id,request_id,decision_revision,receiver_id,receiver_revision,payload,next_attempt_at) VALUES('evt-race','req',1,?,1,'{}',0)`, r.ID); err != nil {
		t.Fatal(err)
	}
	other := New(db, s.cipher)
	var otherErr error
	ran := false
	s.ackHook = func() {
		s.ackHook = nil
		ran = true
		otherErr = other.Ack(ctx, "agent", r.ID, "evt-race") // commits between this Ack's read and update
	}
	if err = s.Ack(ctx, "agent", r.ID, "evt-race"); err != nil {
		t.Fatalf("ack that lost the race: %v", err)
	}
	if !ran || otherErr != nil {
		t.Fatalf("racing ack: ran=%v err=%v", ran, otherErr)
	}
	var status string
	var attempts, history int
	if err = db.QueryRow(`SELECT status,attempts FROM callback_outbox WHERE id='evt-race'`).Scan(&status, &attempts); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow(`SELECT count(*) FROM callback_attempts WHERE event_id='evt-race'`).Scan(&history); err != nil {
		t.Fatal(err)
	}
	if status != "delivered" || attempts != 1 || history != 1 {
		t.Fatalf("after racing acks: status=%s attempts=%d history=%d, want delivered 1 1", status, attempts, history)
	}
}
