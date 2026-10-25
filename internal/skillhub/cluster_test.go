package skillhub

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/cluster"
	"github.com/tusharbhardwaj/toolyard/internal/sealbox"
	"github.com/tusharbhardwaj/toolyard/internal/store"
	"github.com/tusharbhardwaj/toolyard/internal/store/storetest"
)

// An activation on node 1 reaches node 2's index through the cluster, and
// so do a rollback and a delete. Both nodes share one Postgres database.
func TestTwoNodeActivation(t *testing.T) {
	db1, db2 := storetest.OpenPostgresPair(t)
	gh := newFakeGitHub(t, "acme", "skills")
	c1 := gh.commit("main", baseTree())
	cipher, err := sealbox.NewCipher(bytes.Repeat([]byte{9}, sealbox.MasterKeySize))
	if err != nil {
		t.Fatal(err)
	}
	node := func(db *store.DB, id string) *Service {
		ctx, cancel := context.WithCancel(context.Background())
		c, err := cluster.New(ctx, db, cluster.Options{InstanceID: id, ResyncInterval: -1})
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		svc, err := New(Options{DB: db, Cluster: c, Cipher: cipher, GitHubAPI: gh.srv.URL, HTTPClient: gh.srv.Client()})
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		t.Cleanup(func() {
			cancel()
			svc.Wait()
			_ = c.Close()
		})
		if err := svc.Start(ctx); err != nil {
			t.Fatal(err)
		}
		return svc
	}
	node1, node2 := node(db1, "node-1"), node(db2, "node-2")
	ctx := context.Background()

	commitOn := func(svc *Service) string {
		got, _ := svc.Sources(ctx, adminViewer)
		if len(got) != 1 {
			return ""
		}
		return got[0].Commit
	}
	waitFor := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !ok() {
			if time.Now().After(deadline) {
				t.Fatalf("node 2: %s", what)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	src, res := mustCreate(t, node1, SourceInput{URL: "acme/skills", Path: "skills", Mode: ModeAuto})
	if !res.Activated {
		t.Fatalf("sync %+v", res)
	}
	waitFor("the first activation did not arrive", func() bool { return commitOn(node2) == c1 })
	if sk, err := node2.Get(ctx, memberViewer, "skill://skills/pr-review/SKILL.md", true); err != nil || len(sk.Files) != 2 {
		t.Fatalf("node 2 get %+v %v", sk, err)
	}

	c2 := gh.commit("main", map[string]string{"skills/pr-review/checks.md": "v2\n"})
	if res, err := node1.SyncNow(ctx, src.ID); err != nil || !res.Activated {
		t.Fatalf("second sync %+v %v", res, err)
	}
	waitFor("the second activation did not arrive", func() bool { return commitOn(node2) == c2 })

	// A rollback on node 2 reaches node 1.
	if err := node2.Activate(ctx, "usr_admin", src.ID, res.VersionID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for commitOn(node1) != c1 {
		if time.Now().After(deadline) {
			t.Fatal("node 1: the rollback did not arrive")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Two syncs of one source never overlap: node 2 waits for node 1's lock.
	unlock, err := node1.cluster.Lock(ctx, "skillhub:sync:"+src.ID)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		_, _ = node2.SyncNow(ctx, src.ID)
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("node 2 synced while node 1 held the source's lock")
	case <-time.After(300 * time.Millisecond):
	}
	unlock()
	<-done

	if err := node1.DeleteSource(ctx, "usr_admin", src.ID); err != nil {
		t.Fatal(err)
	}
	waitFor("the delete did not arrive", func() bool { return commitOn(node2) == "" })
}
