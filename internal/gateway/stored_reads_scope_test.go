package gateway

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tusharbhardwaj/toolyard/internal/access"
	"github.com/tusharbhardwaj/toolyard/internal/actor"
	"github.com/tusharbhardwaj/toolyard/internal/approval"
	"github.com/tusharbhardwaj/toolyard/internal/jobs"
)

// guestScope is the scope of a guest whose invite lists only tools.
func guestScope(group string, tools ...string) access.Scope {
	sc := access.Scope{Guest: true, Groups: map[string]bool{group: true}, GuestTools: map[string]bool{}}
	for _, t := range tools {
		sc.GuestTools[t] = true
	}
	return sc
}

// A member's approval keeps its arguments and result. Once the member is a
// guest whose invite lacks that tool, polling it answers as an unknown id;
// an approval for a tool the guest still has is read as before.
func TestPollApprovalChecksCurrentScope(t *testing.T) {
	f := newAccessFixture(t, nil)
	hold := func(tool string) string {
		t.Helper()
		req, err := f.bus.Hold(context.Background(), approval.NewRequest{
			AgentID: memberID, UpstreamName: "alpha", ToolName: tool,
			Arguments: map[string]any{"secret": "x"}, Reason: testReason, RequireHuman: true,
		}, 0)
		if err != nil {
			t.Fatal(err)
		}
		return req.ID
	}
	run := hold("alpha.run")
	status := hold("alpha.get_status")
	for _, id := range []string{run, status} {
		if res := f.call(t, f.member, MetaPollApproval, map[string]any{"approval_id": id}); res.IsError {
			t.Fatalf("member polls %s: %s", id, textOf(res))
		}
	}

	f.res.set(memberID, guestScope("alpha", "alpha.get_status"))
	if res := f.call(t, f.member, MetaPollApproval, map[string]any{"approval_id": run}); !res.IsError || textOf(res) != "unknown approval_id" {
		t.Errorf("guest polls a lost tool's approval: %q (error %v), want unknown approval_id", textOf(res), res.IsError)
	}
	if res := f.call(t, f.member, MetaPollApproval, map[string]any{"approval_id": status}); res.IsError {
		t.Errorf("guest polls a kept tool's approval: %s", textOf(res))
	}
	results, _ := decodeResult(t, f.call(t, f.member, MetaPollApprovals, map[string]any{"approval_ids": []any{run, status}}))["results"].([]any)
	if len(results) != 2 {
		t.Fatalf("poll_approvals as a guest = %v", results)
	}
	for _, r := range results {
		m, _ := r.(map[string]any)
		lost := m["approval_id"] == run
		if (m["status"] == "unknown") != lost {
			t.Errorf("poll_approvals entry %v: unknown should be %v", m, lost)
		}
	}
}

// A job keeps its arguments and result. Once its agent's owner is a guest
// without that tool, jobs.result and jobs.status answer as for an unknown
// id; with the tool on the guest's list they answer as before.
func TestJobResultChecksCurrentScope(t *testing.T) {
	res := scopeResolver()
	f := newJobsFixture(t, withResolver(res))
	id := jobIDOf(t, f.call(t, MetaJobsStart, map[string]any{"tool": "alpha.get_fast", "arguments": map[string]any{}}))
	f.waitJob(t, id)

	res.set("ag_1", guestScope("alpha", "alpha.get_slow"))
	for _, tool := range []string{MetaJobsResult, MetaJobsStatus} {
		if r := f.call(t, tool, map[string]any{"job_id": id}); !r.IsError || !strings.Contains(textOf(r), "unknown job_id") {
			t.Errorf("guest %s on a lost tool's job: %s", tool, textOf(r))
		}
	}

	res.set("ag_1", guestScope("alpha", "alpha.get_fast"))
	if r := f.call(t, MetaJobsResult, map[string]any{"job_id": id}); r.IsError {
		t.Errorf("guest jobs.result on a kept tool's job: %s", textOf(r))
	}
}

// routeAsync runs one call in the background and hands back its result,
// so a test can change the caller's scope while the call blocks.
func routeAsync(g *Gateway, agent, tool string, args map[string]any) <-chan *mcp.CallToolResult {
	out := make(chan *mcp.CallToolResult, 1)
	go func() {
		res, err := g.RouteCall(WithAgentID(context.Background(), agent), "test", tool, args)
		if err != nil {
			res = mcp.NewToolResultError("route: " + err.Error())
		}
		out <- res
	}()
	return out
}

// waitResult reads routeAsync's answer, failing after a while.
func waitResult(t *testing.T, ch <-chan *mcp.CallToolResult) *mcp.CallToolResult {
	t.Helper()
	select {
	case res := <-ch:
		return res
	case <-time.After(30 * time.Second):
		t.Fatal("the wait never returned")
		return nil
	}
}

// Access ends while tools.wait_for_approval blocks: when the approval
// settles, the wait answers as for an unknown id instead of the snapshot.
func TestWaitForApprovalRechecksScopeAfterWait(t *testing.T) {
	f := newAccessFixture(t, nil)
	req, err := f.bus.Hold(context.Background(), approval.NewRequest{
		AgentID: memberID, UpstreamName: "alpha", ToolName: "alpha.run",
		Arguments: map[string]any{"secret": "x"}, Reason: testReason, RequireHuman: true,
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	ch := routeAsync(f.gw, memberID, MetaWaitForApproval, map[string]any{
		"approval_id": req.ID, "timeout_seconds": float64(20), ReasonField: testReason,
	})
	time.Sleep(300 * time.Millisecond) // let the wait pass its first check and block
	f.res.set(memberID, guestScope("alpha", "alpha.get_status"))
	if _, err := f.bus.DecideAs(context.Background(), req.ID, approval.StatusDenied, actor.Decider{UserID: "u_admin", Via: actor.ViaDashboard}); err != nil {
		t.Fatal(err)
	}
	if res := waitResult(t, ch); !res.IsError || textOf(res) != "unknown approval_id" {
		t.Errorf("wait after access ended: %q (error %v), want unknown approval_id", textOf(res), res.IsError)
	}
}

// Access ends while jobs.wait blocks: when the job ends, the wait answers
// as for an unknown id instead of the job's snapshot and result.
func TestJobsWaitRechecksScopeAfterWait(t *testing.T) {
	res := scopeResolver()
	f := newJobsFixture(t, withResolver(res))
	defer f.releaseAll()
	f.rules.set("alpha", jobs.AsyncSetting{Tools: map[string]jobs.AsyncRule{"get_slow": {Mode: jobs.AsyncAlways}}})
	id := jobIDOf(t, f.call(t, "alpha.get_slow", nil))
	ch := routeAsync(f.gw, "ag_1", MetaJobsWait, map[string]any{"job_id": id, "timeout_seconds": 20, ReasonField: jobsTestReason})
	time.Sleep(300 * time.Millisecond) // let the wait pass its first check and block
	res.set("ag_1", guestScope("alpha", "alpha.get_fast"))
	f.releaseAll()
	if r := waitResult(t, ch); !r.IsError || !strings.Contains(textOf(r), "unknown job_id") {
		t.Errorf("jobs.wait after access ended: %s", textOf(r))
	}
}

// jobs.list leaves out the jobs whose tool the caller lost and reads on
// past them, so the page is still full of jobs it may see.
func TestJobsListHidesLostToolsAndFillsThePage(t *testing.T) {
	res := scopeResolver()
	f := newJobsFixture(t, withResolver(res))
	defer f.releaseAll()
	f.rules.set("alpha", jobs.AsyncSetting{Tools: map[string]jobs.AsyncRule{"get_slow": {Mode: jobs.AsyncAlways}}})
	start := func() string {
		return jobIDOf(t, f.call(t, MetaJobsStart, map[string]any{"tool": "alpha.get_fast", "arguments": map[string]any{}}))
	}
	oldest := start()
	time.Sleep(5 * time.Millisecond)
	hidden := jobIDOf(t, f.call(t, "alpha.get_slow", nil))
	time.Sleep(5 * time.Millisecond)
	newest := start()

	res.set("ag_1", guestScope("alpha", "alpha.get_fast"))
	list := structured(t, f.call(t, MetaJobsList, map[string]any{"limit": 2}))
	text := asJSON(list["jobs"])
	if list["count"] != float64(2) || !strings.Contains(text, oldest) || !strings.Contains(text, newest) || strings.Contains(text, hidden) {
		t.Errorf("guest's jobs.list = %v", list)
	}
}
