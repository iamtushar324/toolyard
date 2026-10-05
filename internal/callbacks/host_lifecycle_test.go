package callbacks

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/inbox"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

func callbackHost(t *testing.T) (*Service, *store.DB, string) {
	t.Helper()
	s, db := fixture(t)
	id := identity.New(db)
	u, e := id.CreateUser(t.Context(), "callback-owner", "a-test-password")
	if e != nil {
		t.Fatal(e)
	}
	_, a, e := id.CreateAgentWithToken(t.Context(), u.ID, "Local callback host")
	if e != nil {
		t.Fatal(e)
	}
	var hash string
	if e = db.QueryRow(`SELECT token_hash FROM agents WHERE id=?`, a.ID).Scan(&hash); e != nil {
		t.Fatal(e)
	}
	_, e = db.Exec(`INSERT INTO host_connections(environment_id,local_user_id,public_key,host_name,platform,user_id,agent_id,credential_enc,credential_hash,expires_at,created_at,updated_at) VALUES('mac','local-user','public-key','Mac','darwin',?,?, 'encrypted',?,?,0,0)`, u.ID, a.ID, hash, time.Now().Add(time.Hour).UnixMilli())
	if e != nil {
		t.Fatal(e)
	}
	return s, db, a.ID
}
func TestHostReceiverRevocationRegistrationBoundary(t *testing.T) {
	s, db, agent := callbackHost(t)
	ctx := t.Context()
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			s.RegisterPull(ctx, agent, "mac", fmt.Sprintf("hook-%d", i), testSecret())
		}()
	}
	close(start)
	tx, e := db.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = tx.Exec(`UPDATE host_connections SET status='revoked' WHERE agent_id=?`, agent); e != nil {
		t.Fatal(e)
	}
	if _, e = tx.Exec(`UPDATE callback_receivers SET status='removed' WHERE agent_id=?`, agent); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(); e != nil {
		t.Fatal(e)
	}
	wg.Wait()
	var n int
	db.QueryRow(`SELECT count(*) FROM callback_receivers WHERE agent_id=? AND status='active'`, agent).Scan(&n)
	if n != 0 {
		t.Fatal("registration recreated access after revoke", n)
	}
	if _, e = s.RegisterPull(ctx, agent, "mac", "after-revoke", testSecret()); e != ErrReceiver {
		t.Fatal("revoked registration", e)
	}
}
func TestHostReceiverSubscriptionAndDeliveryRequireActiveConnection(t *testing.T) {
	s, db, agent := callbackHost(t)
	ctx := t.Context()
	r, e := s.Register(ctx, agent, "", "public-hook", "https://receiver.example/events", testSecret(), false)
	if e != nil {
		t.Fatal(e)
	}
	db.Exec(`UPDATE host_connections SET status='revoked' WHERE agent_id=?`, agent)
	// Retain a synthetic stale receiver to exercise recovery from a pre-gate row.
	tx, e := db.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.RegisterTx(ctx, tx, &inbox.Request{ID: "request", AgentID: agent}, r.ID); e != ErrReceiver {
		tx.Rollback()
		t.Fatal("revoked subscription", e)
	}
	tx.Rollback()
	_, e = db.Exec(`INSERT INTO callback_outbox(id,request_id,decision_revision,receiver_id,receiver_revision,payload,next_attempt_at) VALUES('host-event','request',1,?,1,'{}',0)`, r.ID)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.DeliverOne(ctx); e != nil {
		t.Fatal(e)
	}
	var status, reason string
	db.QueryRow(`SELECT status,terminal_reason FROM callback_outbox WHERE id='host-event'`).Scan(&status, &reason)
	if status != "failed" || reason != "connection_revoked_or_expired" {
		t.Fatal("revoked callback delivery", status, reason)
	}
	tx, e = db.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = tx.Exec(`INSERT INTO inbox_requests(id,agent_id,kind,status,urgency,doc,created_at,updated_at,expires_at) VALUES('late-request',?,'access','pending','soon','{}',0,0,9999999999999)`, agent); e != nil {
		tx.Rollback()
		t.Fatal(e)
	}
	if _, e = tx.Exec(`INSERT INTO callback_subscriptions(request_id,receiver_id,receiver_revision) VALUES('late-request',?,1)`, r.ID); e != nil {
		tx.Rollback()
		t.Fatal(e)
	}
	if e = s.DecisionTx(ctx, tx, &inbox.Request{ID: "late-request", AgentID: agent, Revision: 2, Status: inbox.StatusApproved}); e != nil {
		tx.Rollback()
		t.Fatal(e)
	}
	if e = tx.Commit(); e != nil {
		t.Fatal(e)
	}
	db.QueryRow(`SELECT status,terminal_reason FROM callback_outbox WHERE request_id='late-request'`).Scan(&status, &reason)
	if status != "failed" || reason != "connection_revoked_or_expired" {
		t.Fatal("late decision created nonterminal delivery", status, reason)
	}
}
