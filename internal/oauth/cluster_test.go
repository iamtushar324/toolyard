package oauth

import (
	"context"
	"crypto/rand"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/cluster"
	"github.com/tusharbhardwaj/toolyard/internal/store/storetest"
)

// A token written through node 1 replaces the bearer node 2 had cached.
func TestBearerChangeReachesOtherNode(t *testing.T) {
	storetest.SkipUnlessPostgres(t)
	db := storetest.Open(t)
	if _, err := db.Exec(`INSERT INTO upstream_servers(name, transport, url, enabled, auth_mode, created_at, updated_at) VALUES('shared','http','http://127.0.0.1:1/mcp',1,'shared',1,1)`); err != nil {
		t.Fatal(err)
	}
	key := make([]byte, MasterKeySize)
	_, _ = rand.Read(key)
	cipher, err := NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	node := func(id string) *Service {
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
		s := New(db, cipher, &recBus{}, &recNotifier{}, nil)
		s.SetCluster(c)
		return s
	}
	node1, node2 := node("node-1"), node("node-2")
	ctx := context.Background()
	bearer := func() string { return node2.HeaderFunc("shared")(ctx)["Authorization"] }

	if err := node1.PutPAT(ctx, "shared", "pat-one"); err != nil {
		t.Fatal(err)
	}
	if got := bearer(); got != "Bearer pat-one" {
		t.Fatalf("node 2 bearer %q", got)
	}
	if err := node1.PutPAT(ctx, "shared", "pat-two"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for bearer() != "Bearer pat-two" {
		if time.Now().After(deadline) {
			t.Fatalf("node 2 still sends %q 5s after node 1 stored a new token", bearer())
		}
		time.Sleep(20 * time.Millisecond)
	}
	// A rejected token (needs_reauth) stops node 2 sending it too.
	node1.MarkReauthExternal(ctx, "shared", "401 from upstream")
	deadline = time.Now().Add(5 * time.Second)
	for bearer() != "" {
		if time.Now().After(deadline) {
			t.Fatalf("node 2 still sends %q after node 1 marked the token rejected", bearer())
		}
		time.Sleep(20 * time.Millisecond)
	}
}
