package callbacks

import (
	"errors"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/inbox"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// age moves a receiver's registration back by d.
func age(t *testing.T, db *store.DB, id string, d time.Duration) {
	t.Helper()
	if _, err := db.Exec(`UPDATE callback_receivers SET created_at=? WHERE id=?`, time.Now().Add(-d).UnixMilli(), id); err != nil {
		t.Fatal(err)
	}
}

func statusOf(t *testing.T, db *store.DB, id string) (string, string) {
	t.Helper()
	var status, reason string
	if err := db.QueryRow(`SELECT status,disabled_reason FROM callback_receivers WHERE id=?`, id).Scan(&status, &reason); err != nil {
		t.Fatal(err)
	}
	return status, reason
}

// The sweep disables push receivers with no successful delivery and
// nothing new to follow for 30 days; recent use, pull receivers and
// receivers already disabled by their agent are left alone.
func TestDisableIdleReceivers(t *testing.T) {
	s, db := fixture(t)
	ctx := t.Context()
	old := IdleReceiverAfter + time.Hour
	idle := mustRegister(t, s, "agent", "idle", "https://idle.example/hook")
	delivered := mustRegister(t, s, "agent", "delivered", "https://delivered.example/hook")
	followed := mustRegister(t, s, "agent", "followed", "https://followed.example/hook")
	fresh := mustRegister(t, s, "agent", "fresh", "https://fresh.example/hook")
	off := mustRegister(t, s, "agent", "off", "https://off.example/hook")
	pull, err := s.RegisterPull(ctx, "agent", "mac", "pull", testSecret())
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{idle.ID, delivered.ID, followed.ID, off.ID, pull.ID} {
		age(t, db, id, old)
	}
	if _, err := s.Change(ctx, "agent", off.ID, "disable", "", off.Revision); err != nil {
		t.Fatal(err)
	}
	// An event delivered last week, and an Inbox item that names a
	// receiver today.
	week := time.Now().Add(-7 * 24 * time.Hour).UnixMilli()
	if _, err := db.Exec(`INSERT INTO callback_outbox(id,request_id,decision_revision,receiver_id,receiver_revision,payload,status,next_attempt_at,delivered_at,created_at) VALUES('evt_old','rq',1,?,1,'{}','delivered',0,?,?)`,
		delivered.ID, week, time.Now().Add(-old).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	newItem(t, s, db, "rq_followed", "agent", followed.ID)
	if _, err := db.Exec(`UPDATE inbox_requests SET created_at=? WHERE id='rq_followed'`, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	n, err := s.DisableIdle(ctx)
	if err != nil || n != 1 {
		t.Fatalf("disabled %d (%v), want 1", n, err)
	}
	if st, why := statusOf(t, db, idle.ID); st != "disabled" || why != DisabledIdle {
		t.Fatalf("idle receiver: %s %q", st, why)
	}
	for _, id := range []string{delivered.ID, followed.ID, fresh.ID, pull.ID} {
		if st, _ := statusOf(t, db, id); st != "active" {
			t.Fatalf("%s was disabled", id)
		}
	}
	if st, why := statusOf(t, db, off.ID); st != "disabled" || why != "" {
		t.Fatalf("agent-disabled receiver: %s %q", st, why)
	}
	// The revision tracks the agent's own changes only.
	if r, _ := s.Get(ctx, "agent", idle.ID); r.Revision != idle.Revision || r.DisabledReason != DisabledIdle {
		t.Fatalf("idle receiver view: %+v", r)
	}
	// An idle receiver does not count toward the cap: four are active.
	s.MaxReceivers = func() int { return 5 }
	mustRegister(t, s, "agent", "in-its-place", "https://new.example/hook")
}

// Using an idle receiver again wakes it: naming it on an item, setting it
// as the default, or the same registration again. A receiver its agent
// disabled is never woken, and a woken one is not disabled again at once.
func TestIdleReceiversWakeWhenUsed(t *testing.T) {
	s, db := fixture(t)
	ctx := t.Context()
	idleNow := func(id string) {
		t.Helper()
		age(t, db, id, IdleReceiverAfter+time.Hour)
		if _, err := db.Exec(`UPDATE callback_receivers SET woken_at=0 WHERE id=?`, id); err != nil {
			t.Fatal(err)
		}
		if _, err := s.DisableIdle(ctx); err != nil {
			t.Fatal(err)
		}
		if st, why := statusOf(t, db, id); st != "disabled" || why != DisabledIdle {
			t.Fatalf("%s did not go idle: %s %q", id, st, why)
		}
	}
	named := mustRegister(t, s, "agent", "named", "https://named.example/hook")
	idleNow(named.ID)
	if err := s.CheckSubscription(ctx, "agent", named.ID, &inbox.Request{ID: "rq_named", AgentID: "agent"}); err != nil {
		t.Fatalf("an idle receiver named on an item: %v", err)
	}
	if st, _ := statusOf(t, db, named.ID); st != "active" {
		t.Fatal("naming it did not wake it")
	}
	if n, _ := s.DisableIdle(ctx); n != 0 {
		t.Fatal("a woken receiver was disabled again at once")
	}

	again := mustRegister(t, s, "agent", "again", "https://again.example/hook")
	idleNow(again.ID)
	s.MaxReceivers = func() int { return 1 }
	if r, err := s.Register(ctx, "agent", "", "again", "https://again.example/hook", testSecret(), false); err != nil || r.ID != again.ID || r.Status != "active" {
		t.Fatalf("the same registration of an idle receiver, at the cap: %+v %v", r, err)
	}
	s.MaxReceivers = nil

	def := mustRegister(t, s, "agent", "default", "https://default.example/hook")
	if err := s.SetDefault(ctx, "agent", def.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE callback_defaults SET updated_at=0`); err != nil {
		t.Fatal(err)
	}
	idleNow(def.ID)
	newItem(t, s, db, "rq_default", "agent", "")
	if st, _ := statusOf(t, db, def.ID); st != "active" {
		t.Fatal("a new item did not wake the idle default receiver")
	}
	var subs int
	db.QueryRow(`SELECT count(*) FROM callback_subscriptions WHERE request_id='rq_default' AND receiver_id=?`, def.ID).Scan(&subs)
	if subs != 1 {
		t.Fatal("the woken default receiver does not follow the item")
	}

	off := mustRegister(t, s, "agent", "off", "https://off.example/hook")
	if _, err := s.Change(ctx, "agent", off.ID, "disable", "", off.Revision); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckRef(ctx, "agent", off.ID); !errors.Is(err, ErrReceiver) {
		t.Fatalf("an agent-disabled receiver was woken: %v", err)
	}
	// Another agent cannot wake it either.
	idleNow(named.ID)
	if err := s.CheckRef(ctx, "other", named.ID); !errors.Is(err, ErrReceiver) {
		t.Fatalf("another agent's ref: %v", err)
	}
	if st, _ := statusOf(t, db, named.ID); st != "disabled" {
		t.Fatal("another agent woke the receiver")
	}
}
