package callbacks

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/inbox"
)

func TestPullCallbackDurableRecoveryAndAck(t *testing.T) {
	s, db := fixture(t)
	ctx := t.Context()
	r, err := s.RegisterPull(ctx, "agent", "mac", "session", testSecret())
	if err != nil {
		t.Fatal(err)
	}
	same, err := s.RegisterPull(ctx, "agent", "mac", "session", testSecret())
	if err != nil || same.ID != r.ID {
		t.Fatal("register retry", err)
	}
	if _, err = s.Register(ctx, "agent", "mac", "session", "https://receiver.example/hook", testSecret(), false); err != ErrConflict {
		t.Fatal("transport changed", err)
	}
	req := &inbox.Request{ID: "pull-request", AgentID: "agent", Revision: 1, Status: inbox.StatusPending, ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}
	tx, _ := db.BeginTx(ctx, nil)
	_, err = tx.Exec(`INSERT INTO inbox_requests(id,agent_id,kind,status,urgency,doc,created_at,updated_at,expires_at) VALUES(?,?,'access','pending','soon','{}',0,0,?)`, req.ID, req.AgentID, req.ExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RegisterTx(ctx, tx, req, r.ID); err != nil {
		t.Fatal(err)
	}
	tx.Commit()
	before, err := s.Pull(ctx, "agent", r.ID)
	if err != nil || len(before) != 0 {
		t.Fatal("before decision", err)
	}
	req.Revision = 2
	req.Status = inbox.StatusDenied
	req.Tools = []inbox.ToolRequest{{CallID: "required-rejected", Decision: inbox.ToolRefused, Required: true, Reason: "Keep the existing file"}, {CallID: "same-tool-rejected", Decision: inbox.ToolRefused}}
	tx, _ = db.BeginTx(ctx, nil)
	if err = s.DecisionTx(ctx, tx, req); err != nil {
		t.Fatal(err)
	}
	tx.Commit()
	// The push delivery worker must never claim a pull item.
	if err = s.DeliverOne(ctx); err != nil {
		t.Fatal(err)
	}
	var status string
	var attempts int
	db.QueryRow(`SELECT status,attempts FROM callback_outbox`).Scan(&status, &attempts)
	if status != "pending" || attempts != 0 {
		t.Fatal("push claimed pull")
	}
	events, err := s.Pull(ctx, "agent", r.ID)
	if err != nil || len(events) != 1 {
		t.Fatal("missing event", err)
	}
	e := events[0]
	key, _ := secretBytes(testSecret())
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(e.EventID + "." + e.Headers["webhook-timestamp"] + "." + e.Body))
	if e.Headers["webhook-signature"] != "v1,"+base64.StdEncoding.EncodeToString(mac.Sum(nil)) {
		t.Fatal("signature wrong")
	}
	if !strings.Contains(e.Body, "required-rejected") || !strings.Contains(e.Body, "Keep the existing file") {
		t.Fatal("outcomes absent")
	}
	restart := New(db, s.cipher)
	again, err := restart.Pull(ctx, "agent", r.ID)
	if err != nil || len(again) != 1 || again[0].Body != e.Body || again[0].EventID != e.EventID {
		t.Fatal("restart recovery", err)
	}
	if _, err = s.Pull(ctx, "other", r.ID); err != ErrReceiver {
		t.Fatal("cross-owner pull", err)
	}
	if err = s.Ack(ctx, "other", r.ID, e.EventID); err != ErrReceiver {
		t.Fatal("cross-owner ack", err)
	}
	if err = s.Ack(ctx, "agent", r.ID, "unrelated"); err != ErrReceiver {
		t.Fatal("unknown ack", err)
	}
	if err = s.Ack(ctx, "agent", r.ID, e.EventID); err != nil {
		t.Fatal(err)
	}
	if err = restart.Ack(ctx, "agent", r.ID, e.EventID); err != nil {
		t.Fatal("lost ack retry", err)
	}
	after, _ := s.Pull(ctx, "agent", r.ID)
	if len(after) != 0 {
		t.Fatal("ack still pending")
	}
	db.QueryRow(`SELECT status,attempts FROM callback_outbox`).Scan(&status, &attempts)
	if status != "delivered" || attempts != 1 {
		t.Fatal("duplicate ack attempt")
	}
}
func TestPullRotationAndDisable(t *testing.T) {
	s, db := fixture(t)
	ctx := t.Context()
	r, err := s.RegisterPull(ctx, "agent", "mac", "session", testSecret())
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO callback_outbox(id,request_id,decision_revision,receiver_id,receiver_revision,payload,next_attempt_at) VALUES('evt-one','req',1,?,1,'{}',0)`, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	rotated := "whsec_" + base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 32)))
	r, err = s.Change(ctx, "agent", r.ID, "rotate", rotated, r.Revision)
	if err != nil {
		t.Fatal(err)
	}
	events, err := s.Pull(ctx, "agent", r.ID)
	if err != nil || len(events) != 1 {
		t.Fatal(err)
	}
	key, _ := secretBytes(rotated)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(events[0].EventID + "." + events[0].Headers["webhook-timestamp"] + "." + events[0].Body))
	if events[0].Headers["webhook-signature"] != "v1,"+base64.StdEncoding.EncodeToString(mac.Sum(nil)) {
		t.Fatal("old key used")
	}
	if _, err = s.Change(ctx, "agent", r.ID, "disable", "", r.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Pull(ctx, "agent", r.ID); err != ErrReceiver {
		t.Fatal("disabled pull", err)
	}
	if err = s.Ack(ctx, "agent", r.ID, "evt-one"); err != ErrReceiver {
		t.Fatal("disabled ack", err)
	}
}
