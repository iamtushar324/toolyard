package callbacks

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/inbox"
	"github.com/tusharbhardwaj/toolyard/internal/sealbox"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

func fixture(t *testing.T) (*Service, *store.DB) {
	t.Helper()
	db, e := store.Open(filepath.Join(t.TempDir(), "db.sqlite"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { db.Close() })
	cipher, e := sealbox.NewCipher(make([]byte, 32))
	if e != nil {
		t.Fatal(e)
	}
	s := New(db, cipher)
	s.Resolver = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}, nil
	}
	return s, db
}
func testSecret() string { return "whsec_" + base64.StdEncoding.EncodeToString(make([]byte, 32)) }
func TestCallbackAtomicDecisionAndRecovery(t *testing.T) {
	s, db := fixture(t)
	ctx := context.Background()
	receiver, e := s.Register(ctx, "ag_owner", "", "hook-1", "https://receiver.example/decision", testSecret(), false)
	if e != nil {
		t.Fatal(e)
	}
	again, e := s.Register(ctx, "ag_owner", "", "hook-1", "https://receiver.example/decision", testSecret(), false)
	if e != nil || again.ID != receiver.ID {
		t.Fatalf("idempotent register %v", e)
	}
	if _, e = s.Get(ctx, "ag_other", receiver.ID); e != ErrReceiver {
		t.Fatal("cross-agent receiver exposed", e)
	}
	r := &inbox.Request{ID: "rq_batch", AgentID: "ag_owner", Revision: 1, Status: inbox.StatusPending, ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}
	tx, e := db.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	_, e = tx.Exec(`INSERT INTO inbox_requests(id,agent_id,kind,status,urgency,doc,created_at,updated_at,expires_at) VALUES(?,?,'access','pending','soon','{}',0,0,?)`, r.ID, r.AgentID, r.ExpiresAt)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.RegisterTx(ctx, tx, r, receiver.ID); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(); e != nil {
		t.Fatal(e)
	}
	var count int
	db.QueryRow(`SELECT count(*) FROM callback_outbox`).Scan(&count)
	if count != 0 {
		t.Fatal("callback before decision")
	}
	r.Revision = 2
	r.Status = inbox.StatusApproved
	r.OwnerNote = "Accept only the two test files"
	r.GrantsExpire = time.Now().Add(30 * time.Minute).UnixMilli()
	r.Tools = []inbox.ToolRequest{{CallID: "one", Decision: inbox.ToolAllowed, Reason: "Useful"}, {CallID: "two", Decision: inbox.ToolRefused, Reason: "Keep this file"}, {CallID: "three", Decision: inbox.ToolAllowed}}
	tx, _ = db.BeginTx(ctx, nil)
	if e = s.DecisionTx(ctx, tx, r); e != nil {
		t.Fatal(e)
	}
	tx.Rollback()
	db.QueryRow(`SELECT count(*) FROM callback_outbox`).Scan(&count)
	if count != 0 {
		t.Fatal("outbox escaped transaction")
	}
	tx, _ = db.BeginTx(ctx, nil)
	if e = s.DecisionTx(ctx, tx, r); e != nil {
		t.Fatal(e)
	}
	tx.Commit()
	var payload string
	db.QueryRow(`SELECT payload FROM callback_outbox`).Scan(&payload)
	var event Event
	if e = json.Unmarshal([]byte(payload), &event); e != nil {
		t.Fatal(e)
	}
	if len(event.Data.Calls) != 3 || event.Data.Calls[1].Verdict != "rejected" || event.Data.Calls[1].Reason != "Keep this file" || event.Data.OverallNote != r.OwnerNote {
		t.Fatalf("incomplete decision: %s", payload)
	}
	if strings.Contains(payload, "tyg_") || strings.Contains(payload, "secret") {
		t.Fatal("credential in callback")
	}
	tx, _ = db.BeginTx(ctx, nil)
	if e = s.DecisionTx(ctx, tx, r); e == nil {
		t.Fatal("duplicate logical event")
	}
	tx.Rollback()
	restored := New(db, s.cipher)
	restored.Resolver = s.Resolver
	history, e := restored.History(ctx, "ag_owner", r.ID)
	if e != nil || len(history) != 1 || history[0].Status != "pending" {
		t.Fatalf("restart lost delivery %v", e)
	}
	changed, e := s.Change(ctx, "ag_owner", receiver.ID, "rotate", testSecret(), 1)
	if e != nil || changed.Revision != 2 {
		t.Fatal(e)
	}
	var bound int
	db.QueryRow(`SELECT receiver_revision FROM callback_outbox`).Scan(&bound)
	if bound != 2 {
		t.Fatal("rotation lost queued delivery")
	}
	if _, e = s.Change(ctx, "ag_owner", receiver.ID, "remove", "", 1); e != ErrConflict {
		t.Fatal("stale mutation accepted", e)
	}
	if _, e = s.Change(ctx, "ag_owner", receiver.ID, "disable", "", 2); e != nil {
		t.Fatal(e)
	}
	history, _ = s.History(ctx, "ag_owner", r.ID)
	if history[0].Status != "failed" || history[0].TerminalReason != "receiver_disabled" {
		t.Fatal("disabled target kept delivery")
	}
	if e = s.Retry(ctx, "ag_other", event.EventID); e != ErrReceiver {
		t.Fatal("unauthorized retry")
	}
	if e = s.Retry(ctx, "ag_owner", event.EventID); e != ErrReceiver {
		t.Fatal("retry revived disabled receiver")
	}
}
func TestCallbackDestinationValidation(t *testing.T) {
	s, _ := fixture(t)
	ctx := context.Background()
	for _, u := range []string{"http://receiver.example/", "https://user:pw@receiver.example/", "https://receiver.example/#fragment", "https://receiver.example:8443/"} {
		if _, e := s.resolve(ctx, u, false); e == nil {
			t.Fatal("accepted", u)
		}
	}
	s.Resolver = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
	}
	if _, e := s.resolve(ctx, "https://receiver.example/", false); e == nil {
		t.Fatal("DNS rebinding allowed private IP")
	}
	if _, e := s.resolve(ctx, "https://receiver.example/", true); e != nil {
		t.Fatal("admin registered private receiver refused", e)
	}
}
func TestCallbackRetryHistoryIsMonotonic(t *testing.T) {
	s, db := fixture(t)
	ctx := context.Background()
	r, _ := s.Register(ctx, "ag_owner", "", "hook", "https://receiver.example/", testSecret(), false)
	db.Exec(`INSERT INTO callback_outbox(id,request_id,decision_revision,receiver_id,receiver_revision,payload,status,attempts,next_attempt_at) VALUES('evt_test','rq',2,?,1,'{}','delivering',1,0)`, r.ID)
	if e := s.finish(ctx, "evt_test", 1, 500, "http_500", true); e != nil {
		t.Fatal(e)
	}
	if e := s.Retry(ctx, "ag_owner", "evt_test"); e != nil {
		t.Fatal(e)
	}
	db.Exec(`UPDATE callback_outbox SET status='delivering',attempts=2 WHERE id='evt_test'`)
	if e := s.finish(ctx, "evt_test", 2, 200, "delivered", false); e != nil {
		t.Fatal(e)
	}
	var n int
	db.QueryRow(`SELECT count(*) FROM callback_attempts WHERE event_id='evt_test'`).Scan(&n)
	if n != 2 {
		t.Fatal("manual retry lost attempt history")
	}
}

func TestCallbackConcurrentRegistrationAndRotationRecovery(t *testing.T) {
	s, db := fixture(t)
	ctx := t.Context()
	const n = 8
	refs := make(chan string, n)
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			r, e := s.Register(ctx, "ag_owner", "", "same-client", "https://receiver.example/decision", testSecret(), false)
			if e != nil {
				errs <- e
				return
			}
			refs <- r.ID
		}()
	}
	var ref string
	for i := 0; i < n; i++ {
		select {
		case e := <-errs:
			t.Fatal(e)
		case got := <-refs:
			if ref != "" && ref != got {
				t.Fatal("registration duplicated")
			}
			ref = got
		}
	}
	var count int
	db.QueryRow(`SELECT count(*) FROM callback_receivers`).Scan(&count)
	if count != 1 {
		t.Fatal(count)
	}
	next := "whsec_" + base64.StdEncoding.EncodeToString([]byte("12345678901234567890123456789012"))
	rotated, e := s.Change(ctx, "ag_owner", ref, "rotate", next, 1)
	if e != nil {
		t.Fatal(e)
	}
	recovered, e := s.Change(ctx, "ag_owner", ref, "rotate", next, 1)
	if e != nil || recovered.Revision != rotated.Revision {
		t.Fatal("rotation reply recovery", e)
	}
	if _, e = s.Change(ctx, "ag_owner", ref, "rotate", testSecret(), 1); e != ErrConflict {
		t.Fatal("changed content accepted", e)
	}
}
