package callbacks

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"
)

// Every special-use range is refused, including IPv4 embedded in IPv6.
func TestPrivateIPDeniesSpecialUseRanges(t *testing.T) {
	for _, a := range []string{
		"0.0.0.1", "10.1.2.3", "100.64.0.1", "100.127.255.254", "127.0.0.1", "169.254.169.254", "172.16.5.4", "192.0.0.8",
		"192.0.2.1", "192.88.99.1", "192.168.1.1", "198.18.0.1", "198.19.255.1", "198.51.100.7", "203.0.113.9", "224.0.0.1",
		"240.0.0.1", "255.255.255.255", "::", "::1", "::ffff:10.0.0.1", "::ffff:127.0.0.1", "64:ff9b::a00:1", "64:ff9b:1::1",
		"2001::1", "2001:db8::1", "2002:a00:1::1", "fc00::1", "fd12::1", "fe80::1", "fec0::1", "ff02::1", "100::1",
	} {
		if !privateIP(net.ParseIP(a)) {
			t.Errorf("%s was treated as public", a)
		}
	}
	for _, a := range []string{"93.184.216.34", "8.8.8.8", "1.1.1.1", "2606:4700:4700::1111", "::ffff:93.184.216.34", "100.128.0.1", "198.20.0.1"} {
		if privateIP(net.ParseIP(a)) {
			t.Errorf("%s was treated as private", a)
		}
	}
	s, _ := fixture(t)
	s.Resolver = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("100.64.0.1")}}, nil
	}
	if _, err := s.Register(t.Context(), "agent", "", "cgn", "https://receiver.example/hook", testSecret(), false); err == nil {
		t.Fatal("a receiver resolving to shared address space was registered")
	}
}

// pickDue takes the oldest due events, never more than two in flight for
// one receiver, so one slow destination cannot hold the queue.
func TestPickDueCapsEachReceiver(t *testing.T) {
	s, db := fixture(t)
	ctx := t.Context()
	slow, err := s.Register(ctx, "agent", "", "slow", "https://slow.example/hook", testSecret(), false)
	if err != nil {
		t.Fatal(err)
	}
	fast, err := s.Register(ctx, "agent", "", "fast", "https://fast.example/hook", testSecret(), false)
	if err != nil {
		t.Fatal(err)
	}
	insert := func(id, receiver, status string, at, claim int64) {
		t.Helper()
		if _, err := db.Exec(`INSERT INTO callback_outbox(id,request_id,decision_revision,receiver_id,receiver_revision,payload,status,next_attempt_at,claim_until) VALUES(?,'rq',1,?,1,'{}',?,?,?)`, id, receiver, status, at, claim); err != nil {
			t.Fatal(err)
		}
	}
	future := time.Now().Add(time.Minute).UnixMilli()
	insert("evt_busy", slow.ID, "delivering", 1, future)
	for i := 0; i < 5; i++ {
		insert(fmt.Sprintf("evt_slow_%d", i), slow.ID, "pending", int64(10+i), 0)
	}
	insert("evt_fast", fast.ID, "pending", 20, 0)
	ids, err := s.pickDue(ctx, 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] != "evt_slow_0" || ids[1] != "evt_fast" {
		t.Fatalf("picked %v, want one more for the slow receiver and the fast one", ids)
	}
	if ids, _ = s.pickDue(ctx, 1); len(ids) != 1 || ids[0] != "evt_slow_0" {
		t.Fatalf("limit 1: %v", ids)
	}
}

// Delivered and failed events older than the retention go, with their
// attempts; pending ones and recent ones stay.
func TestSweepDeletesOldFinishedEvents(t *testing.T) {
	s, db := fixture(t)
	ctx := t.Context()
	r, err := s.Register(ctx, "agent", "", "hook", "https://receiver.example/hook", testSecret(), false)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-OutboxRetention - time.Hour).UnixMilli()
	recent := time.Now().UnixMilli()
	for _, e := range []struct {
		id, status string
		created    int64
	}{{"evt_old_done", "delivered", old}, {"evt_old_failed", "failed", old}, {"evt_old_pending", "pending", old}, {"evt_new_done", "delivered", recent}} {
		if _, err := db.Exec(`INSERT INTO callback_outbox(id,request_id,decision_revision,receiver_id,receiver_revision,payload,status,next_attempt_at,created_at) VALUES(?,'rq',1,?,1,'{}',?,0,?)`, e.id, r.ID, e.status, e.created); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO callback_attempts(event_id,attempt,attempted_at,outcome) VALUES(?,1,0,'x')`, e.id); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.Sweep(ctx)
	if err != nil || n != 2 {
		t.Fatalf("swept %d (%v), want 2", n, err)
	}
	var left, attempts int
	db.QueryRow(`SELECT count(*) FROM callback_outbox`).Scan(&left)
	db.QueryRow(`SELECT count(*) FROM callback_attempts`).Scan(&attempts)
	if left != 2 || attempts != 2 {
		t.Fatalf("left %d events and %d attempts, want 2 and 2", left, attempts)
	}
}

func TestReceiversPerAgentAreCapped(t *testing.T) {
	s, _ := fixture(t)
	ctx := t.Context()
	var first *Receiver
	for i := 0; i < MaxReceiversPerAgent; i++ {
		r, err := s.Register(ctx, "agent", "", fmt.Sprintf("hook-%d", i), fmt.Sprintf("https://receiver.example/%d", i), testSecret(), false)
		if err != nil {
			t.Fatal(err)
		}
		if first == nil {
			first = r
		}
	}
	if _, err := s.Register(ctx, "agent", "", "one-too-many", "https://receiver.example/x", testSecret(), false); !errors.Is(err, ErrTooManyReceivers) {
		t.Fatalf("receiver cap: %v", err)
	}
	if _, err := s.Change(ctx, "agent", first.ID, "remove", "", first.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Register(ctx, "agent", "", "one-too-many", "https://receiver.example/x", testSecret(), false); err != nil {
		t.Fatalf("a removed receiver still counted: %v", err)
	}
}
