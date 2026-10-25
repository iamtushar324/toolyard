package callbacks

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/store"
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
		next       int64
	}{{"evt_old_done", "delivered", old, 0}, {"evt_old_failed", "failed", old, 0}, {"evt_old_pending", "pending", old, 0}, {"evt_new_done", "delivered", recent, 0},
		// Rows an older node queued during a rolling upgrade have no
		// created_at: their next attempt time stands in for it.
		{"evt_old_undated", "delivered", 0, old}, {"evt_new_undated", "delivered", 0, recent}} {
		if _, err := db.Exec(`INSERT INTO callback_outbox(id,request_id,decision_revision,receiver_id,receiver_revision,payload,status,next_attempt_at,created_at) VALUES(?,'rq',1,?,1,'{}',?,?,?)`, e.id, r.ID, e.status, e.next, e.created); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO callback_attempts(event_id,attempt,attempted_at,outcome) VALUES(?,1,0,'x')`, e.id); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.Sweep(ctx)
	if err != nil || n != 3 {
		t.Fatalf("swept %d (%v), want 3", n, err)
	}
	var left, attempts int
	db.QueryRow(`SELECT count(*) FROM callback_outbox`).Scan(&left)
	db.QueryRow(`SELECT count(*) FROM callback_attempts`).Scan(&attempts)
	if left != 3 || attempts != 3 {
		t.Fatalf("left %d events and %d attempts, want 3 and 3", left, attempts)
	}
	var undated int
	db.QueryRow(`SELECT count(*) FROM callback_outbox WHERE id='evt_new_undated'`).Scan(&undated)
	if undated != 1 {
		t.Fatal("a recent event without created_at was swept")
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

// outboxRow stores one event of receiver: status, next attempt time and
// claim end.
func outboxRow(t *testing.T, db *store.DB, id, receiver, status string, at, claim int64) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO callback_outbox(id,request_id,decision_revision,receiver_id,receiver_revision,payload,status,next_attempt_at,claim_until) VALUES(?,'rq',1,?,1,'{}',?,?,?)`, id, receiver, status, at, claim); err != nil {
		t.Fatal(err)
	}
}

func mustRegister(t *testing.T, s *Service, agent, client, destination string) *Receiver {
	t.Helper()
	r, err := s.Register(t.Context(), agent, "", client, destination, testSecret(), false)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// A receiver at its in-flight cap with a long backlog does not fill the due
// window: a newer event of another receiver is still picked.
func TestPickDueHasNoHeadOfLineBlocking(t *testing.T) {
	s, db := fixture(t)
	busy := mustRegister(t, s, "agent", "busy", "https://busy.example/hook")
	calm := mustRegister(t, s, "other", "calm", "https://calm.example/hook")
	future := time.Now().Add(time.Minute).UnixMilli()
	outboxRow(t, db, "evt_busy_a", busy.ID, "delivering", 1, future)
	outboxRow(t, db, "evt_busy_b", busy.ID, "delivering", 1, future)
	for i := 0; i < 20; i++ {
		outboxRow(t, db, fmt.Sprintf("evt_backlog_%02d", i), busy.ID, "pending", int64(10+i), 0)
	}
	outboxRow(t, db, "evt_calm", calm.ID, "pending", 100, 0)
	ids, err := s.pickDue(t.Context(), 1)
	if err != nil || len(ids) != 1 || ids[0] != "evt_calm" {
		t.Fatalf("picked %v (%v), want evt_calm", ids, err)
	}
}

// One agent keeps at most deliveryPerAgent events in flight across its
// receivers, and one destination host at most deliveryPerHost across
// agents.
func TestPickDueCapsAgentsAndHosts(t *testing.T) {
	s, db := fixture(t)
	future := time.Now().Add(time.Minute).UnixMilli()
	// The agent "many" has two receivers, two events in flight on each.
	for i := 0; i < 2; i++ {
		r := mustRegister(t, s, "many", fmt.Sprintf("many-%d", i), fmt.Sprintf("https://many-%d.example/hook", i))
		outboxRow(t, db, fmt.Sprintf("evt_many_%d_a", i), r.ID, "delivering", 1, future)
		outboxRow(t, db, fmt.Sprintf("evt_many_%d_b", i), r.ID, "delivering", 1, future)
	}
	third := mustRegister(t, s, "many", "many-2", "https://many-2.example/hook")
	outboxRow(t, db, "evt_many_third", third.ID, "pending", 10, 0)
	// Two agents send to shared.example, two events in flight each.
	for i := 0; i < 2; i++ {
		r := mustRegister(t, s, fmt.Sprintf("tenant-%d", i), "shared", fmt.Sprintf("https://SHARED.example/hook/%d", i))
		outboxRow(t, db, fmt.Sprintf("evt_shared_%d_a", i), r.ID, "delivering", 1, future)
		outboxRow(t, db, fmt.Sprintf("evt_shared_%d_b", i), r.ID, "delivering", 1, future)
	}
	crowded := mustRegister(t, s, "tenant-2", "shared", "https://shared.example/hook/2")
	outboxRow(t, db, "evt_shared_third", crowded.ID, "pending", 20, 0)
	free := mustRegister(t, s, "tenant-3", "free", "https://free.example/hook")
	outboxRow(t, db, "evt_free", free.ID, "pending", 30, 0)
	ids, err := s.pickDue(t.Context(), 8)
	if err != nil || len(ids) != 1 || ids[0] != "evt_free" {
		t.Fatalf("picked %v (%v), want only evt_free", ids, err)
	}
}

// The claim itself refuses a third event of a receiver while two are in
// flight, whichever node picked it.
func TestClaimCapsEachReceiver(t *testing.T) {
	s, db := fixture(t)
	r := mustRegister(t, s, "agent", "hook", "https://receiver.example/hook")
	future := time.Now().Add(time.Minute).UnixMilli()
	outboxRow(t, db, "evt_a", r.ID, "delivering", 1, future)
	outboxRow(t, db, "evt_b", r.ID, "delivering", 1, future)
	outboxRow(t, db, "evt_c", r.ID, "pending", 1, 0)
	if _, _, _, _, err := s.claim(t.Context(), "evt_c"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("a third event was claimed: %v", err)
	}
	if _, err := db.Exec(`UPDATE callback_outbox SET status='delivered' WHERE id='evt_a'`); err != nil {
		t.Fatal(err)
	}
	attempt, _, ref, _, err := s.claim(t.Context(), "evt_c")
	if err != nil || attempt != 1 || ref != r.ID {
		t.Fatalf("claim after a slot freed: %d %q %v", attempt, ref, err)
	}
}

// Concurrent tests of one receiver queue one ping, and concurrent
// registrations never pass the receiver cap (on Postgres, several
// connections race; the agent lock makes each check and insert one step).
func TestConcurrentPingsAndRegistrationsKeepTheLimits(t *testing.T) {
	s, db := fixture(t)
	ctx := t.Context()
	r, err := s.RegisterPullWith(ctx, "agent", "mac", "pinged", testSecret(), everything)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	got := map[string]bool{}
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := s.Test(ctx, "agent", r.ID)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				t.Errorf("test: %v", err)
				return
			}
			got[id] = true
		}()
	}
	wg.Wait()
	var pings int
	if err := db.QueryRow(`SELECT count(*) FROM callback_outbox WHERE event_type='ping'`).Scan(&pings); err != nil || pings != 1 || len(got) != 1 {
		t.Fatalf("%d pings stored, %d ids returned (%v)", pings, len(got), err)
	}
	// The receiver above counts too: fill the cap but one, then race.
	for i := 1; i < MaxReceiversPerAgent-1; i++ {
		mustRegister(t, s, "agent", fmt.Sprintf("hook-%d", i), fmt.Sprintf("https://receiver.example/%d", i))
	}
	var errs []string
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.Register(ctx, "agent", "", fmt.Sprintf("race-%d", i), fmt.Sprintf("https://race.example/%d", i), testSecret(), false)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				errs = append(errs, "ok")
			} else if errors.Is(err, ErrTooManyReceivers) {
				errs = append(errs, "full")
			} else {
				errs = append(errs, err.Error())
			}
		}(i)
	}
	wg.Wait()
	sort.Strings(errs)
	var kept int
	if err := db.QueryRow(`SELECT count(*) FROM callback_receivers WHERE agent_id='agent' AND status<>'removed'`).Scan(&kept); err != nil {
		t.Fatal(err)
	}
	if kept != MaxReceiversPerAgent || fmt.Sprint(errs) != "[full full full full ok]" {
		t.Fatalf("kept %d receivers, registrations %v", kept, errs)
	}
}

// Many receivers of one shared host (one T3 server) with a backlog do not
// fill the due window while that host is at its in-flight cap: an event
// for another host is still picked.
func TestPickDueHostBacklogDoesNotFillTheWindow(t *testing.T) {
	s, db := fixture(t)
	future := time.Now().Add(time.Minute).UnixMilli()
	for i := 0; i < 12; i++ {
		r := mustRegister(t, s, fmt.Sprintf("session-%02d", i), "t3", fmt.Sprintf("https://T3.example/api/session-webhooks/%d", i))
		if i < 2 {
			outboxRow(t, db, fmt.Sprintf("evt_t3_%02d_busy_a", i), r.ID, "delivering", 1, future)
			outboxRow(t, db, fmt.Sprintf("evt_t3_%02d_busy_b", i), r.ID, "delivering", 1, future)
		}
		for j := 0; j < 3; j++ {
			outboxRow(t, db, fmt.Sprintf("evt_t3_%02d_%d", i, j), r.ID, "pending", int64(10+i*3+j), 0)
		}
	}
	other := mustRegister(t, s, "elsewhere", "hook", "https://other.example/hook")
	outboxRow(t, db, "evt_other", other.ID, "pending", 1000, 0)
	ids, err := s.pickDue(t.Context(), 1)
	if err != nil || len(ids) != 1 || ids[0] != "evt_other" {
		t.Fatalf("picked %v (%v), want evt_other", ids, err)
	}
	var host string
	if err := db.QueryRow(`SELECT host FROM callback_receivers WHERE id=?`, other.ID).Scan(&host); err != nil || host != "other.example" {
		t.Fatalf("stored host %q %v", host, err)
	}
}

// destinationHost gives the value migration 0062 derives in SQL.
func TestDestinationHost(t *testing.T) {
	for in, want := range map[string]string{
		"https://Hooks.Example.com/a?x=1":   "hooks.example.com",
		"https://receiver.example:443/hook": "receiver.example",
		"https://[2001:DB8::1]/hook":        "2001:db8::1",
		"https://q.example?only":            "q.example",
		"https://plain.example":             "plain.example",
		"":                                  "",
	} {
		if got := destinationHost(in); got != want {
			t.Errorf("destinationHost(%q) = %q, want %q", in, got, want)
		}
	}
}

// A failed ping is not retried by hand (409 conflict): callbacks.test
// sends a new one, and a receiver keeps at most one open ping.
func TestRetryRefusesPings(t *testing.T) {
	s, db := fixture(t)
	ctx := t.Context()
	r, err := s.RegisterPullWith(ctx, "agent", "mac", "pinged", testSecret(), everything)
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.Test(ctx, "agent", r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`UPDATE callback_outbox SET status='failed',terminal_reason='http_500' WHERE id=?`, first); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Test(ctx, "agent", r.ID); err != nil {
		t.Fatal(err)
	}
	err = s.Retry(ctx, "agent", first)
	if !errors.Is(err, ErrPingNotRetried) || !errors.Is(err, ErrConflict) {
		t.Fatalf("retry of a ping: %v", err)
	}
	if err = s.Retry(ctx, "other", first); !errors.Is(err, ErrReceiver) {
		t.Fatalf("another agent's ping: %v", err)
	}
}
