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

// A sharing change on node 1 (a skill set to nobody, a block list, a
// contributor source's approval) reaches node 2's index through the
// cluster.
func TestTwoNodeSharingReload(t *testing.T) {
	db1, db2 := storetest.OpenPostgresPair(t)
	gh := newFakeGitHub(t, "acme", "skills")
	gh.commit("main", baseTree())
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
	sees := func(v Viewer, ref string) bool {
		_, err := node2.Get(ctx, v, ref, false)
		return err == nil
	}
	src, _ := mustCreate(t, node1, SourceInput{URL: "acme/skills", Path: "skills", Mode: ModeAuto})
	waitFor("the source did not arrive", func() bool { return sees(memberViewer, "pr-review") })

	if _, err := node1.SetSkillSharing(ctx, "usr_admin", src.ID, "pr-review", SkillSharing{Mode: VisibleNobody}); err != nil {
		t.Fatal(err)
	}
	waitFor("the skill set to nobody is still shown", func() bool { return !sees(memberViewer, "pr-review") })
	if !sees(adminViewer, "pr-review") {
		t.Fatal("node 2 hides the skill from an admin in the dashboard")
	}
	blocked := []string{"usr_member"}
	if _, err := node1.UpdateSource(ctx, "usr_admin", src.ID, SourceUpdate{Blocked: &blocked}); err != nil {
		t.Fatal(err)
	}
	waitFor("the block list did not arrive", func() bool { return !sees(memberViewer, "skill://skills/deploy/SKILL.md") })

	two, err := node1.CreateSource(ctx, "usr_contrib", SourceInput{URL: "acme/skills", Slug: "two", Path: "skills", Mode: ModeAuto, PendingApproval: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := node1.SyncNow(ctx, two.ID); err != nil {
		t.Fatal(err)
	}
	other := Viewer{UserID: "usr_other", AgentID: "agt_other"}
	waitFor("the pending source arrived visible or not at all", func() bool { return sees(adminViewer, "skill://two/pr-review/SKILL.md") })
	if sees(other, "skill://two/pr-review/SKILL.md") {
		t.Fatal("node 2 shows a source pending approval")
	}
	if _, err := node1.DecideApproval(ctx, "usr_admin", two.ID, true, "", ""); err != nil {
		t.Fatal(err)
	}
	waitFor("the approval did not arrive", func() bool { return sees(other, "skill://two/pr-review/SKILL.md") })
}
