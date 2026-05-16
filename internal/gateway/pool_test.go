package gateway

import (
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
)

// sentinel *client.Client used purely to mark upstreams as "live" for the
// pool-selection tests. We never call methods on it — selection works off
// (u.client != nil) and lastUsed; suspend()/Close() are not exercised in
// these tests.
var liveSentinel = &client.Client{}

func mkUpstream(name string, ageNanos int64, alive bool) *upstream {
	u := &upstream{cfg: UpstreamConfig{Name: name, Transport: "stdio"}}
	u.lastUsed.Store(ageNanos)
	if alive {
		u.client = liveSentinel
	}
	return u
}

// TestPickEvictionCandidate_LRU verifies the selection prefers the
// oldest live upstream when the cap is at saturation.
func TestPickEvictionCandidate_LRU(t *testing.T) {
	g := &Gateway{upstreams: map[string]*upstream{}, maxLiveUpstreams: 2}

	now := time.Now().UnixNano()
	oldest := mkUpstream("a", now-int64(5*time.Minute), true)
	mid := mkUpstream("b", now-int64(2*time.Minute), true)
	newest := mkUpstream("c", now-int64(30*time.Second), true)
	g.upstreams["a"] = oldest
	g.upstreams["b"] = mid
	g.upstreams["c"] = newest

	newcomer := &upstream{cfg: UpstreamConfig{Name: "d"}, pool: g}
	victim, live, limit := g.pickEvictionCandidate(newcomer)

	if victim != oldest {
		t.Fatalf("expected oldest upstream %q to be evicted, got %v", oldest.cfg.Name, victim)
	}
	if live != 3 {
		t.Errorf("live count = %d, want 3", live)
	}
	if limit != 2 {
		t.Errorf("limit = %d, want 2", limit)
	}
}

// TestPickEvictionCandidate_BelowCap returns nil when there's still room
// in the pool — nothing should be evicted.
func TestPickEvictionCandidate_BelowCap(t *testing.T) {
	g := &Gateway{upstreams: map[string]*upstream{}, maxLiveUpstreams: 4}
	g.upstreams["a"] = mkUpstream("a", time.Now().UnixNano(), true)
	g.upstreams["b"] = mkUpstream("b", time.Now().UnixNano(), true)

	newcomer := &upstream{cfg: UpstreamConfig{Name: "c"}, pool: g}
	victim, live, _ := g.pickEvictionCandidate(newcomer)
	if victim != nil {
		t.Fatalf("expected no eviction below cap, got victim %v", victim.cfg.Name)
	}
	if live != 2 {
		t.Errorf("live = %d, want 2", live)
	}
}

// TestPickEvictionCandidate_Unbounded confirms a 0 cap means never evict.
func TestPickEvictionCandidate_Unbounded(t *testing.T) {
	g := &Gateway{upstreams: map[string]*upstream{}, maxLiveUpstreams: 0}
	for i := 0; i < 50; i++ {
		name := string(rune('a' + i%26))
		g.upstreams[name] = mkUpstream(name, int64(i), true)
	}
	victim, _, limit := g.pickEvictionCandidate(&upstream{cfg: UpstreamConfig{Name: "x"}})
	if victim != nil {
		t.Fatalf("unbounded pool should never evict; got %v", victim.cfg.Name)
	}
	if limit != 0 {
		t.Errorf("limit = %d, want 0", limit)
	}
}

// TestPickEvictionCandidate_SkipsSuspended ensures suspended upstreams
// don't count toward the live cap and aren't candidates for eviction.
func TestPickEvictionCandidate_SkipsSuspended(t *testing.T) {
	g := &Gateway{upstreams: map[string]*upstream{}, maxLiveUpstreams: 2}
	g.upstreams["suspended-old"] = mkUpstream("suspended-old", 1, false) // dead
	g.upstreams["live-recent"] = mkUpstream("live-recent", time.Now().UnixNano(), true)

	newcomer := &upstream{cfg: UpstreamConfig{Name: "n"}, pool: g}
	victim, live, _ := g.pickEvictionCandidate(newcomer)
	if victim != nil {
		t.Fatalf("only 1 live upstream, cap=2: should not evict; got %v", victim.cfg.Name)
	}
	if live != 1 {
		t.Errorf("live = %d, want 1 (suspended must not count)", live)
	}
}

// TestLiveSuspendedCounts ensures the /v1/health-shaped accessors return
// matching numbers as upstreams flip between live/suspended.
func TestLiveSuspendedCounts(t *testing.T) {
	g := &Gateway{upstreams: map[string]*upstream{}}
	g.upstreams["live"] = mkUpstream("live", time.Now().UnixNano(), true)
	g.upstreams["suspended"] = mkUpstream("suspended", 0, false)
	if got := g.LiveUpstreamCount(); got != 1 {
		t.Errorf("LiveUpstreamCount = %d, want 1", got)
	}
	if got := g.SuspendedUpstreamCount(); got != 1 {
		t.Errorf("SuspendedUpstreamCount = %d, want 1", got)
	}
	g.SetMaxLiveUpstreams(7)
	if got := g.MaxLiveUpstreams(); got != 7 {
		t.Errorf("MaxLiveUpstreams = %d, want 7", got)
	}
}
