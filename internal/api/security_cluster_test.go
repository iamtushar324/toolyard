package api

import (
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/cluster"
	"github.com/tusharbhardwaj/toolyard/internal/store/storetest"
)

// Two replicas' throttles that share the database counter share one
// budget: a client cannot multiply it by the number of replicas.
func TestSharedThrottleSpansReplicas(t *testing.T) {
	db := storetest.Open(t)
	k := cluster.NewCounter(db)
	node1 := newLoginThrottle(5, 15*time.Minute)
	node2 := newLoginThrottle(5, 15*time.Minute)
	node1.share(k, "login")
	node2.share(k, "login")
	for i := 0; i < 5; i++ {
		th := node1
		if i%2 == 1 {
			th = node2
		}
		if !th.Allow("1.2.3.4") {
			t.Fatalf("attempt %d refused", i+1)
		}
	}
	if node1.Allow("1.2.3.4") || node2.Allow("1.2.3.4") {
		t.Fatal("a sixth attempt was allowed on another replica")
	}
	if !node2.Allow("5.6.7.8") {
		t.Fatal("another address was refused")
	}
	// A successful login resets the budget on every replica.
	node2.Reset("1.2.3.4")
	if !node1.Allow("1.2.3.4") {
		t.Fatal("the reset did not reach the shared budget")
	}
	// Different throttles never share buckets.
	other := newLoginThrottle(0, time.Hour)
	other.share(k, "unauth")
	if !other.AllowN("1.2.3.4", 1, time.Hour) {
		t.Fatal("the unauth throttle counted the login throttle's attempts")
	}
	node1.Sweep()
}
