package policy

import (
	"context"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/cluster"
	"github.com/tusharbhardwaj/toolyard/internal/store"
	"github.com/tusharbhardwaj/toolyard/internal/store/storetest"
)

// openNode opens one replica's pool on url with its own engine and
// cluster node.
func openNode(t *testing.T, url, id string) *Engine {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	db, err := store.OpenPostgres(ctx, url, 4)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	c, err := cluster.New(ctx, db, cluster.Options{InstanceID: id, ResyncInterval: -1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	e, err := Load(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	e.SetCluster(c)
	return e
}

func waitDecision(t *testing.T, e *Engine, req Request, want Action) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if got := e.Eval(req).Action; got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("node 2 still decides %q for %s 5s later, want %q", e.Eval(req).Action, req.ToolName, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A deny written through node 1 is enforced by node 2 within seconds, and
// so is its removal.
func TestDenyPolicySpansNodes(t *testing.T) {
	storetest.SkipUnlessPostgres(t)
	url := storetest.NewPostgresDatabase(t)
	node1 := openNode(t, url, "node-1")
	node2 := openNode(t, url, "node-2")
	ctx := context.Background()

	req := Request{AgentID: "ag_1", UpstreamName: "github", ToolName: "github.get_issue"}
	if got := node2.Eval(req).Action; got != ActionAllow {
		t.Fatalf("before the deny node 2 decides %q", got)
	}
	start := time.Now()
	if _, err := node1.Set(ctx, ScopeTool, "github.get_issue", "deny", "stop reads", false); err != nil {
		t.Fatalf("set on node 1: %v", err)
	}
	waitDecision(t, node2, req, ActionDeny)
	t.Logf("node 2 enforced node 1's deny after %s", time.Since(start))
	if d, ok := node2.ExplicitDeny("github.get_issue", "github"); !ok || d.Action != ActionDeny {
		t.Fatalf("node 2 ExplicitDeny = %+v %v", d, ok)
	}

	if err := node1.DeleteTarget(ctx, ScopeTool, "github.get_issue"); err != nil {
		t.Fatal(err)
	}
	waitDecision(t, node2, req, ActionAllow)

	// An upstream-wide deny through node 2 reaches node 1.
	if _, err := node2.Set(ctx, ScopeUpstream, "github", "deny", "", false); err != nil {
		t.Fatal(err)
	}
	waitDecision(t, node1, req, ActionDeny)
}

// Writes from two nodes take turns: a guarded Change judges the rules the
// other node wrote a moment before.
func TestChangeSeesOtherNodesWrite(t *testing.T) {
	storetest.SkipUnlessPostgres(t)
	url := storetest.NewPostgresDatabase(t)
	node1 := openNode(t, url, "node-1")
	node2 := openNode(t, url, "node-2")
	ctx := context.Background()
	if _, err := node1.Set(ctx, ScopeTool, "deploy.run", "ask", "", false); err != nil {
		t.Fatal(err)
	}
	var seen string
	if _, _, err := node2.Change(ctx, ScopeTool, "deploy.run", "deny", "", false, func(before, _ *Engine) error {
		p, _ := before.Get(ScopeTool, "deploy.run")
		seen = p.Action
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if seen != "ask" {
		t.Fatalf("node 2's guard saw %q, want node 1's ask", seen)
	}
}
