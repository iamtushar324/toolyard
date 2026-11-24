package gateway

import (
	"context"
	"strings"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/access"
	"github.com/tusharbhardwaj/toolyard/internal/approval"
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
