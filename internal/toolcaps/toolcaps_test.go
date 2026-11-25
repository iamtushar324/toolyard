package toolcaps_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/gateway"
	"github.com/tusharbhardwaj/toolyard/internal/store/storetest"
	"github.com/tusharbhardwaj/toolyard/internal/toolcaps"
)

// clock is a settable test clock.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

func newService(t *testing.T, at time.Time) (*toolcaps.Service, *clock) {
	t.Helper()
	db := storetest.Open(t)
	t.Cleanup(func() { _ = db.Close() })
	s := toolcaps.New(db)
	c := &clock{t: at}
	s.SetClock(c.now)
	return s, c
}

func setCap(t *testing.T, s *toolcaps.Service, per, total int64) {
	t.Helper()
	if _, err := s.Set(context.Background(), gateway.ToolCap{Upstream: "bk", Tool: "bk.get_rank", PerCallerDaily: per, TotalDaily: total}, "u_admin"); err != nil {
		t.Fatalf("Set: %v", err)
	}
}

func reserve(t *testing.T, s *toolcaps.Service, caller string) (func(), *gateway.CapRefusal) {
	t.Helper()
	release, refused, err := s.Reserve(context.Background(), "bk", "bk.get_rank", func() string { return caller })
	if err != nil {
		t.Fatalf("Reserve(%s): %v", caller, err)
	}
	return release, refused
}

func usage(t *testing.T, s *toolcaps.Service, day string) map[string]int64 {
	t.Helper()
	rows, err := s.Usage(context.Background(), day)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int64{}
	for _, r := range rows {
		out[r.Caller] = r.Calls
	}
	return out
}

var noon = time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

// TestReservePerCallerAndTotal: each caller gets per_caller_daily slots,
// everyone together total_daily; a refused total gives the caller's slot
// back, so a refusal never counts.
func TestReservePerCallerAndTotal(t *testing.T) {
	s, _ := newService(t, noon)
	setCap(t, s, 2, 3)

	for i := 0; i < 2; i++ {
		if _, r := reserve(t, s, "user:alice"); r != nil {
			t.Fatalf("alice call %d refused: %+v", i+1, r)
		}
	}
	_, r := reserve(t, s, "user:alice")
	if r == nil || r.Scope != gateway.CapScopePerCaller || r.Limit != 2 {
		t.Fatalf("alice's third call: refusal %+v, want per_caller limit 2", r)
	}
	if _, r := reserve(t, s, "user:bob"); r != nil {
		t.Fatalf("bob's first call refused: %+v", r)
	}
	_, r = reserve(t, s, "user:bob")
	if r == nil || r.Scope != gateway.CapScopeTotal || r.Limit != 3 {
		t.Fatalf("bob's second call: refusal %+v, want total limit 3", r)
	}
	if want := time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC); !r.ResetAt.Equal(want) {
		t.Fatalf("ResetAt = %s, want %s", r.ResetAt, want)
	}
	u := usage(t, s, "2026-11-25")
	if u["user:alice"] != 2 || u["user:bob"] != 1 || u[toolcaps.TotalCaller] != 3 {
		t.Fatalf("usage = %v, want alice 2, bob 1 (the refused call gave its slot back), total 3", u)
	}
}

// TestReleaseGivesSlotBack: a failed call's release returns both slots,
// once, however often it is called.
func TestReleaseGivesSlotBack(t *testing.T) {
	s, _ := newService(t, noon)
	setCap(t, s, 1, 0)
	release, r := reserve(t, s, "user:alice")
	if r != nil || release == nil {
		t.Fatalf("first call: release=%v refusal=%+v", release != nil, r)
	}
	release()
	release()
	u := usage(t, s, "2026-11-25")
	if u["user:alice"] != 0 || u[toolcaps.TotalCaller] != 0 {
		t.Fatalf("usage after release = %v, want 0", u)
	}
	if _, r := reserve(t, s, "user:alice"); r != nil {
		t.Fatalf("call after a released slot refused: %+v", r)
	}
	if _, r := reserve(t, s, "user:alice"); r == nil {
		t.Fatal("second kept call allowed over a per-caller limit of 1")
	}
}

// TestCapResetsAtUTCMidnight: a full cap opens again at 00:00 UTC.
func TestCapResetsAtUTCMidnight(t *testing.T) {
	late := time.Date(2026, 11, 25, 23, 59, 59, 0, time.UTC)
	s, c := newService(t, late)
	setCap(t, s, 1, 0)
	if _, r := reserve(t, s, "user:alice"); r != nil {
		t.Fatalf("first call refused: %+v", r)
	}
	_, r := reserve(t, s, "user:alice")
	if r == nil {
		t.Fatal("second call allowed over the cap")
	}
	if want := time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC); !r.ResetAt.Equal(want) {
		t.Fatalf("ResetAt = %s, want %s", r.ResetAt, want)
	}
	c.set(time.Date(2026, 11, 26, 0, 0, 1, 0, time.UTC))
	if _, r := reserve(t, s, "user:alice"); r != nil {
		t.Fatalf("call after 00:00 UTC refused: %+v", r)
	}
	if day, end := s.Today(); day != "2026-11-26" || !end.Equal(time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("Today = %s %s", day, end)
	}
	// A clock in another zone counts the UTC day.
	ist := time.FixedZone("IST", 5*3600+1800)
	c.set(time.Date(2026, 11, 27, 4, 0, 0, 0, ist)) // 2026-11-26 22:30 UTC
	if _, r := reserve(t, s, "user:alice"); r == nil {
		t.Fatal("a second call on the same UTC day was allowed")
	}
}

// TestUncappedToolTouchesNothing: no cap, no counter and no caller lookup.
func TestUncappedToolTouchesNothing(t *testing.T) {
	s, _ := newService(t, noon)
	var asked atomic.Int32
	release, r, err := s.Reserve(context.Background(), "bk", "bk.other", func() string { asked.Add(1); return "x" })
	if err != nil || r != nil || release != nil || asked.Load() != 0 {
		t.Fatalf("uncapped: release=%v refusal=%+v err=%v caller asked %d times", release != nil, r, err, asked.Load())
	}
	if u := usage(t, s, "2026-11-25"); len(u) != 0 {
		t.Fatalf("counters for an uncapped tool: %v", u)
	}
}

// TestConcurrentReservesNeverPassTheCap: with many callers at once, exactly
// total_daily calls get a slot.
func TestConcurrentReservesNeverPassTheCap(t *testing.T) {
	s, _ := newService(t, noon)
	setCap(t, s, 0, 5)
	var wg sync.WaitGroup
	var granted atomic.Int32
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, r, err := s.Reserve(context.Background(), "bk", "bk.get_rank", func() string { return "caller" + string(rune('a'+i)) })
			if err != nil {
				t.Error(err)
				return
			}
			if r == nil {
				granted.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if granted.Load() != 5 {
		t.Fatalf("granted %d slots, want exactly 5", granted.Load())
	}
	if u := usage(t, s, "2026-11-25"); u[toolcaps.TotalCaller] != 5 {
		t.Fatalf("total counter = %d, want 5", u[toolcaps.TotalCaller])
	}
}

// TestSetValidatesAndApplies: limits must be >= 0 and not both 0; a change
// applies to the next call at once; Clear removes the cap.
func TestSetValidatesAndApplies(t *testing.T) {
	s, _ := newService(t, noon)
	ctx := context.Background()
	for _, c := range []gateway.ToolCap{
		{Upstream: "bk", Tool: "bk.get_rank"},
		{Upstream: "bk", Tool: "bk.get_rank", PerCallerDaily: -1, TotalDaily: 3},
		{Upstream: "", Tool: "bk.get_rank", TotalDaily: 3},
	} {
		if _, err := s.Set(ctx, c, "u_admin"); !errors.Is(err, gateway.ErrInvalidCap) {
			t.Errorf("Set(%+v) err = %v, want ErrInvalidCap", c, err)
		}
	}
	setCap(t, s, 1, 0)
	reserve(t, s, "user:alice")
	if _, r := reserve(t, s, "user:alice"); r == nil {
		t.Fatal("cap of 1 not enforced")
	}
	setCap(t, s, 2, 0) // raised: applies at once on this replica
	if _, r := reserve(t, s, "user:alice"); r != nil {
		t.Fatalf("raised cap not applied: %+v", r)
	}
	list, err := s.List(ctx)
	if err != nil || len(list) != 1 || list[0].PerCallerDaily != 2 || list[0].UpdatedBy != "u_admin" {
		t.Fatalf("List = %+v, %v", list, err)
	}
	if ok, err := s.Clear(ctx, "bk", "bk.get_rank"); !ok || err != nil {
		t.Fatalf("Clear = %v, %v", ok, err)
	}
	if _, r := reserve(t, s, "user:alice"); r != nil {
		t.Fatalf("cleared cap still enforced: %+v", r)
	}
	if ok, _ := s.Clear(ctx, "bk", "bk.get_rank"); ok {
		t.Fatal("second Clear reported a cap")
	}
}

// TestPruneKeepsRecentDays: Prune deletes the counters of older days only.
func TestPruneKeepsRecentDays(t *testing.T) {
	s, c := newService(t, noon.AddDate(0, 0, -40))
	setCap(t, s, 5, 0)
	reserve(t, s, "user:alice")
	c.set(noon)
	reserve(t, s, "user:alice")
	n, err := s.Prune(context.Background(), 35)
	if err != nil || n != 2 { // the caller's and the total row of the old day
		t.Fatalf("Prune = %d, %v; want 2 rows", n, err)
	}
	if u := usage(t, s, "2026-11-25"); u["user:alice"] != 1 {
		t.Fatalf("today's counters were pruned: %v", u)
	}
}
