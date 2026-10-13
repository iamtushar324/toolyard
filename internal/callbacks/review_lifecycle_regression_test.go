package callbacks

import (
	"context"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/identity"
)

func TestReviewExpiredFederationStopsUnboundReceiverDelivery(t *testing.T) {
	s, db := fixture(t)
	ctx := context.Background()
	id := identity.New(db)
	u, err := id.CreateUser(ctx, "review-owner", "long-enough-password")
	if err != nil {
		t.Fatal(err)
	}
	token, ag, err := id.CreateAgentWithToken(ctx, u.ID, "review-agent")
	if err != nil {
		t.Fatal(err)
	}
	credential, err := s.cipher.Seal([]byte(token), []byte("federation|review|subject"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.Register(ctx, ag.ID, "", "review-client", "https://receiver.example/decision", testSecret(), false)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO federation_issuers(issuer,public_key,kid,origin,registered_by,created_at) VALUES('review','key','key','https://review.example',?,0)`, []any{u.ID}},
		{`INSERT INTO federation_connections(issuer,subject,user_id,agent_id,credential_enc,version,expires_at) VALUES('review','subject',?,?,?,1,?)`, []any{u.ID, ag.ID, credential, time.Now().Add(-time.Minute).UnixMilli()}},
		{`INSERT INTO callback_outbox(id,request_id,decision_revision,receiver_id,receiver_revision,payload,next_attempt_at) VALUES('evt_review','rq_review',2,?,1,'{}',0)`, []any{r.ID}},
	} {
		if _, err = db.Exec(q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.DeliverOne(ctx); err != nil {
		t.Fatal(err)
	}
	var status, reason string
	if err = db.QueryRow(`SELECT status,terminal_reason FROM callback_outbox WHERE id='evt_review'`).Scan(&status, &reason); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || reason != "connection_revoked_or_expired" {
		t.Fatalf("expired connection delivery: %s %s", status, reason)
	}
}

func TestReviewReceiverRevisionMismatchDoesNotDiscardEvent(t *testing.T) {
	s, db := fixture(t)
	ctx := context.Background()
	r, err := s.Register(ctx, "ag_review", "", "client-review", "https://receiver.example/decision", testSecret(), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`UPDATE callback_receivers SET revision=2 WHERE id=?`, r.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO callback_outbox(id,request_id,decision_revision,receiver_id,receiver_revision,payload,next_attempt_at) VALUES('evt_review','rq_review',2,?,1,'{}',0)`, r.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.DeliverOne(ctx); err != nil {
		t.Fatal(err)
	}
	var status string
	if err = db.QueryRow(`SELECT status FROM callback_outbox WHERE id='evt_review'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "pending" {
		t.Fatalf("rotation interleaving discarded event: %s", status)
	}
}
