package settings

import (
	"context"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/cluster"
	"github.com/tusharbhardwaj/toolyard/internal/store/storetest"
)

// A setting saved through node 1 is read on node 2 within moments.
func TestSettingReachesOtherNode(t *testing.T) {
	storetest.SkipUnlessPostgres(t)
	db := storetest.Open(t)
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
		s, err := New(context.Background(), db)
		if err != nil {
			t.Fatal(err)
		}
		s.SetCluster(c)
		return s
	}
	node1, node2 := node("node-1"), node("node-2")
	ctx := context.Background()
	if got := node2.GetString(ApprovalMode, "unset"); got != "unset" {
		t.Fatalf("before: %q", got)
	}
	if err := node1.Set(ctx, ApprovalMode, ApprovalModeInbox); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for node2.GetString(ApprovalMode, "unset") != ApprovalModeInbox {
		if time.Now().After(deadline) {
			t.Fatalf("node 2 still reads %q", node2.GetString(ApprovalMode, "unset"))
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := node1.Patch(ctx, map[string]any{ApprovalMode: ApprovalModeExecute}); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for node2.GetString(ApprovalMode, "unset") != ApprovalModeExecute {
		if time.Now().After(deadline) {
			t.Fatalf("after Patch node 2 still reads %q", node2.GetString(ApprovalMode, "unset"))
		}
		time.Sleep(20 * time.Millisecond)
	}
}
