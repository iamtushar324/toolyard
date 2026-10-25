package gateway

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tusharbhardwaj/toolyard/internal/actor"
	"github.com/tusharbhardwaj/toolyard/internal/approval"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/cluster"
	"github.com/tusharbhardwaj/toolyard/internal/inbox"
	"github.com/tusharbhardwaj/toolyard/internal/memory"
	"github.com/tusharbhardwaj/toolyard/internal/policy"
	"github.com/tusharbhardwaj/toolyard/internal/realtime"
	"github.com/tusharbhardwaj/toolyard/internal/store"
	"github.com/tusharbhardwaj/toolyard/internal/store/storetest"
)

// replica is one gateway stack: its own pool, cluster node, policy engine,
// approval bus, Inbox and gateway, with an upstream "t" whose tools count
// their runs.
type replica struct {
	db     *store.DB
	c      *cluster.Cluster
	policy *policy.Engine
	gw     *Gateway
	svc    *inbox.Service
	runs   atomic.Int32
}

func newReplica(t *testing.T, db *store.DB, name string) *replica {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	r := &replica{db: db}
	var err error
	if r.c, err = cluster.New(ctx, db, cluster.Options{InstanceID: name, ResyncInterval: -1}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.c.Close() })
	if r.policy, err = policy.Load(ctx, db); err != nil {
		t.Fatal(err)
	}
	r.policy.SetCluster(r.c)
	bus, err := approval.New(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	bus.SetCluster(r.c)
	r.gw = New(Options{
		Policy: r.policy, Approval: bus, Audit: audit.New(db), Hub: realtime.NewHub(), Memory: memory.New(db),
		Metrics: &fakeMetrics{},
	})
	r.gw.SetCluster(r.c)
	r.gw.RegisterBuiltins()
	t.Cleanup(func() { _ = r.gw.Close() })
	if r.svc, err = inbox.New(ctx, inbox.Options{DB: db, Catalog: r.gw, Cluster: r.c}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.svc.Flush)
	r.gw.SetInbox(r.svc, inbox.NewGuide(nil), func() string { return ApprovalModeExecute })
	r.gw.RegisterInboxTools()
	handler := func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		r.runs.Add(1)
		return mcp.NewToolResultText("ran on " + name), nil
	}
	for _, e := range []toolEntry{
		{tool: mcp.Tool{Name: "t.get_status"}, upstream: "t", originalName: "get_status"},
		{tool: mcp.Tool{Name: "t.run"}, upstream: "t", originalName: "run"},
	} {
		e.tool.InputSchema = mcp.ToolInputSchema{Type: "object", Properties: addMetaProps(map[string]any{})}
		e.reasonField, e.handle = ReasonField, handler
		r.gw.registerEntry(e)
	}
	return r
}

func (r *replica) call(t *testing.T, ctx context.Context, tool string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	full := map[string]any{ReasonField: testReason}
	for k, v := range args {
		full[k] = v
	}
	res, err := r.gw.RouteCall(ctx, "test", tool, full)
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	return res
}

// Two gateway replicas on one Postgres database: an agent's wait on
// replica 2 wakes when its owner decides on replica 1, the grant runs once
// on replica 2 under replica 2's lease, and a deny saved through replica 1
// is enforced by replica 2 within seconds.
func TestTwoNodeGateways(t *testing.T) {
	db1, db2 := storetest.OpenPostgresPair(t)
	one := newReplica(t, db1, "gw-1")
	two := newReplica(t, db2, "gw-2")
	ctx := context.Background()
	agent := WithAgentID(ctx, "ag_1")

	// A read call works on either replica.
	if res := two.call(t, agent, "t.get_status", nil); res.IsError || two.runs.Load() != 1 {
		t.Fatalf("read on replica 2: %+v", res)
	}

	// The agent asks on replica 1 and waits on replica 2.
	sub, err := one.svc.Submit(ctx, "ag_1", &inbox.Submission{
		Task: &inbox.TaskContext{Objective: "Run the controlled production operation."}, Kind: inbox.KindAccess, Title: "Run the thing", Summary: "One run.", Message: "I'd like to run t.run once in production to finish the task.",
		Facts: &inbox.Facts{WhyNow: "The controlled operation is ready.", IfItGoesWrong: "The production operation can fail.", Undo: "Restore the previous operation state."}, Audio: inbox.Audio{Script: "Run it?"}, Urgency: inbox.UrgencySoon,
		Tools: []inbox.SubmissionTool{{CallID: "run_once", Target: "Production controlled resource", Operation: "write", ExpectedEffects: "One controlled operation completes.", AffectedScope: "The controlled production resource", MaterialRisks: "A wrong argument can change the wrong resource.", Undo: "Restore the previous resource state.", Tool: "t.run", Required: true, Summary: "Run it.", Params: map[string]any{"env": "prod"}}},
	})
	if err != nil || !sub.OK {
		t.Fatalf("submit on replica 1: %v %+v", err, sub)
	}
	one.svc.Flush()
	waited := make(chan *mcp.CallToolResult, 1)
	go func() {
		res, err := two.gw.RouteCall(agent, "test", "inbox.wait", map[string]any{ReasonField: testReason, "ids": []any{sub.RequestID}, "timeout_seconds": float64(30)})
		if err != nil {
			res = mcp.NewToolResultError(err.Error())
		}
		waited <- res
	}()
	time.Sleep(300 * time.Millisecond) // replica 2's wait is under way
	owner := actor.Decider{UserID: "u_alice", Email: "alice@example.com", Name: "Alice", Via: actor.ViaDashboard}
	if _, err := one.svc.Decide(ctx, sub.RequestID, inbox.Decision{Action: "submit", RequestRevision: 1, SubmissionID: "two_node_test",
		Verdicts: map[string]inbox.CallVerdict{"run_once": {Verdict: inbox.VerdictAccepted}}, Decider: owner}); err != nil {
		t.Fatalf("decide on replica 1: %v", err)
	}
	select {
	case res := <-waited:
		if res.IsError || strings.Contains(resultText(res), `"status":"pending"`) || !strings.Contains(resultText(res), "approved") {
			t.Fatalf("replica 2's wait: %s", resultText(res))
		}
	case <-time.After(15 * time.Second):
		t.Fatal("replica 2's inbox.wait did not wake after replica 1's decision")
	}

	// The grant runs once, on replica 2, which owns the execution.
	status, err := two.svc.Status(ctx, "ag_1", []string{sub.RequestID})
	if err != nil || len(status) != 1 || status[0].Tools[0].Grant == "" {
		t.Fatalf("status on replica 2: %v %+v", err, status)
	}
	res := two.call(t, agent, "t.run", map[string]any{"env": "prod", GrantField: status[0].Tools[0].Grant})
	if res.IsError || two.runs.Load() != 2 || one.runs.Load() != 0 {
		t.Fatalf("grant call on replica 2: %s (runs: replica 1 %d, replica 2 %d)", resultText(res), one.runs.Load(), two.runs.Load())
	}
	var owned string
	if err := db1.QueryRowContext(ctx, `SELECT COALESCE(owner_instance, '') FROM inbox_executions WHERE grant_id = ?`, status[0].Tools[0].GrantID).Scan(&owned); err != nil {
		t.Fatal(err)
	}
	if owned != two.c.InstanceID() {
		t.Fatalf("execution owned by %q, want replica 2 (%s)", owned, two.c.InstanceID())
	}
	// Replica 1's recovery leaves replica 2's execution (finished or live)
	// alone; the grant cannot run again on either replica.
	if _, err := one.svc.RecoverExecutions(ctx); err != nil {
		t.Fatal(err)
	}
	if res := one.call(t, agent, "t.run", map[string]any{"env": "prod", GrantField: status[0].Tools[0].Grant}); !res.IsError || one.runs.Load() != 0 {
		t.Fatalf("a used grant ran again on replica 1: %s", resultText(res))
	}

	// A deny saved through replica 1 is enforced by replica 2.
	start := time.Now()
	if _, err := one.policy.Set(ctx, policy.ScopeTool, "t.get_status", "deny", "stop", false); err != nil {
		t.Fatal(err)
	}
	for {
		before := two.runs.Load()
		res := two.call(t, agent, "t.get_status", nil)
		if res.IsError && two.runs.Load() == before {
			break
		}
		if time.Since(start) > 5*time.Second {
			t.Fatalf("replica 2 still runs a tool replica 1 denied, 5s later: %s", resultText(res))
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Logf("replica 2 enforced replica 1's deny after %s", time.Since(start))
}
