package access

import (
	"context"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/cluster"
	"github.com/tusharbhardwaj/toolyard/internal/store"
	"github.com/tusharbhardwaj/toolyard/internal/store/storetest"
)

func newAccessNode(t *testing.T, db *store.DB, id string) *Service {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	c, err := cluster.New(ctx, db, cluster.Options{InstanceID: id, ResyncInterval: -1})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		_ = c.Close()
	})
	svc := New(db)
	// A clock that never moves: a cached entry never expires by time, so
	// only the cluster message can drop it.
	frozen := time.Now()
	svc.now = func() time.Time { return frozen }
	svc.SetCluster(c)
	return svc
}

// A change on one replica drops the cached scope on the others at once,
// not only after cacheTTL: a disabled agent and a blocked user are in
// force on every replica.
func TestTwoNodeAccessInvalidation(t *testing.T) {
	db1, db2 := storetest.OpenPostgresPair(t)
	ctx := context.Background()
	addUser(t, db1, "u_admin", "admin", "active")
	addAgent(t, db1, "ag_1", "u_admin")
	node1, node2 := newAccessNode(t, db1, "node-1"), newAccessNode(t, db2, "node-2")
	within := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("node 2 still serves its cached copy: %s", what)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	// The agent: node 2 caches it, node 1 disables it.
	if node2.ScopeFor(ctx, "ag_1").Denied {
		t.Fatal("an enabled agent is denied")
	}
	if _, err := db1.Exec(`UPDATE agents SET disabled = 1 WHERE id = 'ag_1'`); err != nil {
		t.Fatal(err)
	}
	if node2.ScopeFor(ctx, "ag_1").Denied {
		t.Fatal("test setup: node 2 did not serve its cached copy")
	}
	node1.InvalidateAgent("ag_1")
	within("a disabled agent", func() bool { return node2.ScopeFor(ctx, "ag_1").Denied })

	// The user: node 2 caches an admin, node 1 blocks the account.
	if !node2.UserScope(ctx, "u_admin").All {
		t.Fatal("the admin is not All")
	}
	if _, err := db1.Exec(`UPDATE users SET status = 'blocked' WHERE id = 'u_admin'`); err != nil {
		t.Fatal(err)
	}
	if node2.UserScope(ctx, "u_admin").Denied {
		t.Fatal("test setup: node 2 did not serve its cached copy of the user")
	}
	node1.Invalidate("u_admin")
	within("a blocked user", func() bool { return node2.UserScope(ctx, "u_admin").Denied })

	// A drop of everything.
	if _, err := db1.Exec(`UPDATE users SET status = 'active' WHERE id = 'u_admin'`); err != nil {
		t.Fatal(err)
	}
	node1.Invalidate("")
	within("an active user again", func() bool { return node2.UserScope(ctx, "u_admin").All })
}
