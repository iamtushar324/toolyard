package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/access"
	"github.com/tusharbhardwaj/toolyard/internal/approval"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/inbox"
	"github.com/tusharbhardwaj/toolyard/internal/metrics"
)

// previewControlFake replaces only the external control boundary. All gateway
// policy, access, approval, grants, audit and result persistence are real.
type previewControlFake struct {
	mu                           sync.Mutex
	preflights, posts            int
	agent, owner, committedOwner string
	committedAtMillis            int64
	revoked                      bool
	operation                    string
	args                         map[string]any
}

func newPreviewControlFake(agent string) *previewControlFake {
	return &previewControlFake{agent: agent, owner: "u_owner"}
}

func (p *previewControlFake) authorized(agent string) bool {
	return agent == p.agent && !p.revoked && p.owner != "" && (p.committedOwner == "" || p.committedOwner == p.owner)
}

func (p *previewControlFake) Preflight(_ context.Context, agent, operation string, args map[string]any) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.preflights++
	if !p.authorized(agent) {
		return errors.New("private diagnostics must never reach a caller")
	}
	if p.committedOwner == "" {
		p.committedOwner = p.owner
		p.committedAtMillis = time.Now().UnixMilli()
	}
	return nil
}

func (p *previewControlFake) Call(_ context.Context, agent, operation string, args map[string]any) (json.RawMessage, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.authorized(agent) || p.committedOwner == "" {
		return nil, errors.New("private diagnostics must never reach a caller")
	}
	p.posts++
	p.operation = operation
	p.args = args
	return json.RawMessage(`{"content":[{"type":"text","text":"queued"}]}`), nil
}

func (p *previewControlFake) RequireApprovalBinding(ctx context.Context, agent string, createdAtMillis int64) error {
	if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 10*time.Second {
		return errors.New("approved dispatch has no bounded context")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if createdAtMillis <= 0 || p.committedOwner == "" || p.committedAtMillis > createdAtMillis || !p.authorized(agent) {
		return errors.New("original immutable enrollment absent or invalid")
	}
	return nil
}

func (p *previewControlFake) counts() (int, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.preflights, p.posts
}

func previewPolicy(t *testing.T, g *Gateway, tool, action string) {
	t.Helper()
	if _, err := g.policy.Set(context.Background(), "tool", tool, action, "synthetic test policy", true); err != nil {
		t.Fatal(err)
	}
}

func TestBKSPreviewCatalogDisabledAndReserved(t *testing.T) {
	f := newActorFixture(t)
	f.gw.RegisterBKSPreviewTools(nil)
	if f.gw.HasTool("bks_preview.create") {
		t.Fatal("disabled preview tools are exposed")
	}
	p := newPreviewControlFake("ag_1")
	f.gw.RegisterBKSPreviewTools(p)
	if preflights, posts := p.counts(); preflights != 0 || posts != 0 {
		t.Fatal("registration contacted the private control")
	}
	want := map[string]string{
		"bks_preview.create": "preview_create", "bks_preview.inspect": "preview_inspect",
		"bks_preview.heartbeat": "preview_heartbeat", "bks_preview.release": "preview_release",
		"bks_preview.snapshots": "preview_snapshots", "bks_preview.results": "preview_results",
	}
	for name, operation := range want {
		if !f.gw.HasTool(name) {
			t.Fatalf("missing fixed tool %s", name)
		}
		previewPolicy(t, f.gw, name, "allow")
		args := map[string]any{"environment_id": "00000000-0000-4000-8000-000000000001"}
		if name == "bks_preview.create" {
			args = map[string]any{"source_ref": "draft:synthetic", "request_id": "00000000-0000-4000-8000-000000000001", "snapshot_id": "synthetic"}
		}
		if name == "bks_preview.snapshots" {
			args = nil
		}
		if res := f.call(t, WithAgentID(context.Background(), "ag_1"), "test", name, args); res.IsError {
			t.Fatalf("%s failed: %s", name, textOf(res))
		}
		if p.operation != operation {
			t.Fatalf("%s targets %s", name, p.operation)
		}
	}
	if before, posts := p.counts(); before != 6 || posts != 6 {
		t.Fatalf("catalog calls = %d/%d", before, posts)
	}
	if err := f.gw.AddUpstream(context.Background(), UpstreamConfig{Name: "bks_preview"}); err == nil {
		t.Fatal("upstream can replace the private builtin prefix")
	}
}

func TestBKSPreviewDirectAndMetaKeepReasonAuditAndMetrics(t *testing.T) {
	f := newActorFixture(t)
	p := newPreviewControlFake("ag_1")
	f.gw.RegisterBKSPreviewTools(p)
	previewPolicy(t, f.gw, "bks_preview.snapshots", "allow")
	ctx := WithAgentID(context.Background(), "ag_1")
	if reply := mcpToolsCall(t, f.gw, ctx, "bks_preview.snapshots"); reply.rpcErr || reply.isError || reply.text != "queued" {
		t.Fatalf("direct call: %+v", reply)
	}
	res := f.call(t, ctx, "direct", MetaExecuteTool, map[string]any{
		"tool": "bks_preview.snapshots", "arguments": map[string]any{ReasonField: testReason},
	})
	if res.IsError {
		t.Fatalf("meta call: %s", textOf(res))
	}
	if ev := f.lastMetric(t, "bks_preview.snapshots"); ev.Via != MetaExecuteTool || ev.ReasonText != testReason || ev.Outcome != metrics.OutcomeOK {
		t.Fatalf("metric: %+v", ev)
	}
	if row, ok := findAudit(f.drainAudit(), audit.EventCallAllowed, "bks_preview.snapshots"); !ok || row.Reason != testReason || row.Decision != "allow" {
		t.Fatalf("audit: %+v", row)
	}
	if len(p.args) != 0 {
		t.Fatalf("reason metadata reached the control: %v", p.args)
	}
	if preflights, posts := p.counts(); preflights != 2 || posts != 2 {
		t.Fatalf("calls %d/%d", preflights, posts)
	}
}

func TestBKSPreviewAccessReasonAndPolicyDenyBeforeIdentity(t *testing.T) {
	for _, scenario := range []string{"access", "reason", "deny"} {
		t.Run(scenario, func(t *testing.T) {
			f := newActorFixture(t)
			p := newPreviewControlFake("ag_1")
			f.gw.RegisterBKSPreviewTools(p)
			previewPolicy(t, f.gw, "bks_preview.snapshots", "allow")
			args := map[string]any{ReasonField: testReason}
			switch scenario {
			case "access":
				f.gw.access = &fakeResolver{scopes: map[string]access.Scope{}}
			case "reason":
				delete(args, ReasonField)
			case "deny":
				previewPolicy(t, f.gw, "bks_preview.snapshots", "deny")
			}
			res, err := f.gw.RouteCall(WithAgentID(context.Background(), "ag_1"), "test", "bks_preview.snapshots", args)
			if err != nil || !res.IsError {
				t.Fatalf("expected refusal: %v %+v", err, res)
			}
			if preflights, posts := p.counts(); preflights != 0 || posts != 0 {
				t.Fatalf("refused call reached control: %d/%d", preflights, posts)
			}
		})
	}
}

func TestBKSPreviewInternalDispatchRefusedWithoutChangingOrdinaryTools(t *testing.T) {
	f := newActorFixture(t)
	p := newPreviewControlFake("ag_1")
	f.gw.RegisterBKSPreviewTools(p)
	ctx := WithAgentID(context.Background(), "ag_1")
	res, err := f.gw.CallInternal(ctx, "synthetic", "bks_preview.snapshots", nil)
	if err != nil || !res.IsError || textOf(res) != "agent policy route required" {
		t.Fatalf("internal refusal: %v %+v", err, res)
	}
	if preflights, posts := p.counts(); preflights != 0 || posts != 0 {
		t.Fatal("internal call reached control")
	}
	if row, ok := findAudit(f.drainAudit(), audit.EventCallDenied, "bks_preview.snapshots"); !ok || row.Decision != "internal-denied" {
		t.Fatalf("audit: %+v", row)
	}
	if ev := f.lastMetric(t, "bks_preview.snapshots"); ev.Outcome != metrics.OutcomeDenied {
		t.Fatalf("metric: %+v", ev)
	}
	res, err = f.gw.CallInternal(ctx, "synthetic", "t.run", nil)
	if err != nil || res.IsError || textOf(res) != "ran" {
		t.Fatalf("ordinary internal call changed: %v %+v", err, res)
	}
}

func TestBKSPreviewMissingBindingRefusesBeforeApproval(t *testing.T) {
	f := newActorFixture(t)
	p := newPreviewControlFake("ag_1")
	p.revoked = true
	f.gw.RegisterBKSPreviewTools(p)
	res := f.call(t, WithAgentID(context.Background(), "ag_1"), "test", "bks_preview.snapshots", nil)
	if !res.IsError || textOf(res) != "trusted preview caller/session binding unavailable" {
		t.Fatalf("unsafe refusal: %s", textOf(res))
	}
	if f.lastMetric(t, "bks_preview.snapshots").ApprovalID != "" {
		t.Fatal("invalid identity queued approval")
	}
	if row, ok := findAudit(f.drainAudit(), audit.EventCallFailed, "bks_preview.snapshots"); !ok || strings.Contains(row.ResultSummary, "diagnostics") {
		t.Fatalf("audit: %+v", row)
	}
}

func TestBKSPreviewCallerSessionClaimCannotEstablishAuthority(t *testing.T) {
	f := newActorFixture(t)
	p := newPreviewControlFake("ag_1")
	f.gw.RegisterBKSPreviewTools(p)
	for _, caller := range []string{"", "ag_other", "u_owner"} {
		res := f.call(t, WithAgentID(context.Background(), caller), "dashboard", "bks_preview.snapshots", map[string]any{
			SessionField: "e851eba3-123b-495c-887c-0947cc997d92",
		})
		if !res.IsError || f.lastMetric(t, "bks_preview.snapshots").ApprovalID != "" {
			t.Fatalf("caller %q established authority with a session claim", caller)
		}
	}
	if _, posts := p.counts(); posts != 0 {
		t.Fatal("untrusted caller reached control")
	}
}

func TestBKSPreviewInboxModeKeepsPermissionCoaching(t *testing.T) {
	f := newActorFixture(t)
	p := newPreviewControlFake("ag_1")
	f.gw.RegisterBKSPreviewTools(p)
	f.gw.SetInbox(f.svc, inbox.NewGuide(nil), func() string { return ApprovalModeInbox })
	res := f.call(t, WithAgentID(context.Background(), "ag_1"), "test", "bks_preview.snapshots", nil)
	if s := structured(t, res); !res.IsError || s["status"] != "permission_required" || s["executed"] != false {
		t.Fatalf("permission coaching changed: %v", s)
	}
	requests, err := f.svc.List(context.Background(), inbox.ListFilter{})
	if err != nil || len(requests) != 0 {
		t.Fatalf("coaching created a request: %v %+v", err, requests)
	}
	if _, posts := p.counts(); posts != 0 {
		t.Fatal("coaching executed control operation")
	}
}

func TestBKSPreviewDeferredRevocationAndOwnerChange(t *testing.T) {
	for _, change := range []string{"revocation", "owner"} {
		t.Run(change, func(t *testing.T) {
			f := newActorFixture(t)
			p := newPreviewControlFake("ag_1")
			f.gw.RegisterBKSPreviewTools(p)
			ctx := WithAgentID(context.Background(), "ag_1")
			f.call(t, ctx, "test", "bks_preview.snapshots", nil)
			id := f.lastMetric(t, "bks_preview.snapshots").ApprovalID
			if id == "" || p.committedOwner != "u_owner" {
				t.Fatal("approval did not pin the original owner")
			}
			p.mu.Lock()
			if change == "revocation" {
				p.revoked = true
			} else {
				p.owner = "u_other"
			}
			p.mu.Unlock()
			req, err := f.bus.Decide(context.Background(), id, approval.StatusAllowed, "u_owner")
			if err != nil {
				t.Fatal(err)
			}
			f.gw.Execute(context.Background(), req)
			res := f.call(t, ctx, "test", "bks_preview.snapshots", map[string]any{ApprovalIDField: id})
			if !res.IsError {
				t.Fatal("revoked or reassigned binding executed after approval")
			}
			if _, posts := p.counts(); posts != 0 {
				t.Fatal("revoked or reassigned binding reached the control")
			}
		})
	}
}

func TestBKSPreviewApprovedResultCachedAndOwnerScoped(t *testing.T) {
	f := newActorFixture(t)
	p := newPreviewControlFake("ag_1")
	f.gw.RegisterBKSPreviewTools(p)
	ctx := WithAgentID(context.Background(), "ag_1")
	f.call(t, ctx, "test", "bks_preview.snapshots", nil)
	id := f.lastMetric(t, "bks_preview.snapshots").ApprovalID
	req, err := f.bus.Decide(context.Background(), id, approval.StatusAllowed, "u_owner")
	if err != nil {
		t.Fatal(err)
	}
	f.gw.Execute(context.Background(), req)
	for i := 0; i < 2; i++ {
		res := f.call(t, ctx, "test", "bks_preview.snapshots", map[string]any{ApprovalIDField: id})
		if res.IsError || textOf(res) != "queued" {
			t.Fatalf("cached result: %s", textOf(res))
		}
	}
	res := f.call(t, WithAgentID(context.Background(), "ag_other"), "test", "bks_preview.snapshots", map[string]any{ApprovalIDField: id})
	if !res.IsError || strings.Contains(textOf(res), "queued") {
		t.Fatal("another agent read the result")
	}
	if preflights, posts := p.counts(); preflights != 1 || posts != 1 {
		t.Fatalf("resume executed twice: %d/%d", preflights, posts)
	}
}

func TestBKSPreviewRestartSweepUsesRegisteredToolsAndRechecksBinding(t *testing.T) {
	f := newActorFixture(t)
	p := newPreviewControlFake("ag_1")
	f.gw.RegisterBKSPreviewTools(p)
	f.call(t, WithAgentID(context.Background(), "ag_1"), "test", "bks_preview.snapshots", nil)
	id := f.lastMetric(t, "bks_preview.snapshots").ApprovalID
	if _, err := f.bus.Decide(context.Background(), id, approval.StatusAllowed, "u_owner"); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	p.revoked = true
	p.mu.Unlock()
	f.bus.SetExecutor(f.gw)
	n, err := f.bus.SweepUnexecuted(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("sweep: %d %v", n, err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		req, err := f.bus.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if req.ResultExecutedAt != 0 {
			if !req.ResultIsError {
				t.Fatal("sweep used a revoked binding")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("sweep result was not persisted")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, posts := p.counts(); posts != 0 {
		t.Fatal("sweep reached control after revocation")
	}
}

func TestBKSPreviewExecuteRejectsHistoricalApprovalWithoutOriginalEnrollment(t *testing.T) {
	for _, lateBinding := range []bool{false, true} {
		t.Run(map[bool]string{false: "no original binding", true: "binding added after approval"}[lateBinding], func(t *testing.T) {
			f := newActorFixture(t)
			p := newPreviewControlFake("ag_1")
			f.gw.RegisterBKSPreviewTools(p)
			req, err := f.bus.Hold(context.Background(), approval.NewRequest{
				AgentID: "ag_1", UpstreamName: "bks_preview", ToolName: "bks_preview.snapshots", Arguments: map[string]any{}, Reason: testReason,
			}, 0)
			if err != nil {
				t.Fatal(err)
			}
			if lateBinding {
				time.Sleep(5 * time.Millisecond)
				if err := p.Preflight(context.Background(), "ag_1", "preview_snapshots", nil); err != nil {
					t.Fatal(err)
				}
			}
			req, err = f.bus.Decide(context.Background(), req.ID, approval.StatusAllowed, "u_owner")
			if err != nil {
				t.Fatal(err)
			}
			f.gw.Execute(context.Background(), req)
			got, err := f.bus.Get(context.Background(), req.ID)
			if err != nil || !got.ResultIsError || got.ResultExecutedAt == 0 {
				t.Fatalf("historical approval not refused: %v %+v", err, got)
			}
			if _, posts := p.counts(); posts != 0 {
				t.Fatal("historical approval reached private control")
			}
		})
	}
}

func TestBKSPreviewExecuteUsesPersistedStatusAndExpiry(t *testing.T) {
	for _, state := range []string{"pending", "denied", "expired"} {
		t.Run(state, func(t *testing.T) {
			f := newActorFixture(t)
			p := newPreviewControlFake("ag_1")
			f.gw.RegisterBKSPreviewTools(p)
			if state == "expired" {
				f.bus.SetTTL(time.Millisecond)
			}
			f.call(t, WithAgentID(context.Background(), "ag_1"), "test", "bks_preview.snapshots", nil)
			id := f.lastMetric(t, "bks_preview.snapshots").ApprovalID
			req, err := f.bus.Get(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			if state != "pending" {
				decision := approval.StatusDenied
				if state == "expired" {
					decision = approval.StatusAllowed
				}
				req, err = f.bus.Decide(context.Background(), id, decision, "u_owner")
				if err != nil {
					t.Fatal(err)
				}
			}
			if state == "expired" {
				time.Sleep(5 * time.Millisecond)
			}
			// Forged in-memory fields must not override the persisted decision.
			req.Status, req.ExpiresAt = approval.StatusAllowed, time.Now().Add(time.Hour).UnixMilli()
			f.gw.Execute(context.Background(), req)
			got, err := f.bus.Get(context.Background(), id)
			if err != nil || !got.ResultIsError {
				t.Fatalf("persisted %s accepted: %v %+v", state, err, got)
			}
			if _, posts := p.counts(); posts != 0 {
				t.Fatalf("persisted %s reached control", state)
			}
		})
	}
}

func TestBKSPreviewExecuteUsesPersistedApprovedArguments(t *testing.T) {
	f := newActorFixture(t)
	p := newPreviewControlFake("ag_1")
	f.gw.RegisterBKSPreviewTools(p)
	f.call(t, WithAgentID(context.Background(), "ag_1"), "test", "bks_preview.inspect", map[string]any{"environment_id": "00000000-0000-4000-8000-000000000001"})
	id := f.lastMetric(t, "bks_preview.inspect").ApprovalID
	req, err := f.bus.Decide(context.Background(), id, approval.StatusAllowed, "u_owner")
	if err != nil {
		t.Fatal(err)
	}
	req.Arguments["environment_id"] = "00000000-0000-4000-8000-000000000002"
	req.Reason = "unapproved replacement reason"
	f.gw.Execute(context.Background(), req)
	if p.args["environment_id"] != "00000000-0000-4000-8000-000000000001" {
		t.Fatal("in-memory arguments replaced approved arguments")
	}
	if ev := f.lastMetric(t, "bks_preview.inspect"); ev.ReasonText != testReason {
		t.Fatalf("unapproved reason reached metric: %+v", ev)
	}
}

func TestBKSPreviewGrantPreservedOnIdentityFailureThenRedeemed(t *testing.T) {
	f := newActorFixture(t)
	p := newPreviewControlFake("ag_1")
	f.gw.RegisterBKSPreviewTools(p)
	ctx := context.Background()
	sub, err := f.svc.Submit(ctx, "ag_1", &inbox.Submission{
		Kind: inbox.KindAccess, Title: "List synthetic snapshots", Summary: "Read the approved catalog.",
		Message: "I need the reviewed synthetic snapshot catalog to select an isolated preview input.",
		Facts:   &inbox.Facts{WhyNow: "Select the fixture.", IfItGoesWrong: "No changes.", Undo: "No changes."},
		Audio:   inbox.Audio{Script: "Permit one snapshot catalog read."}, Urgency: inbox.UrgencySoon,
		Tools: []inbox.SubmissionTool{{Tool: "bks_preview.snapshots", Required: true, Summary: "Read the catalog.", Params: map[string]any{}}},
	})
	if err != nil || !sub.OK {
		t.Fatalf("submit: %v %+v", err, sub)
	}
	f.svc.Flush()
	if _, err := f.svc.Decide(ctx, sub.RequestID, inbox.Decision{Action: "approve", Allow: []bool{true}}); err != nil {
		t.Fatal(err)
	}
	status, err := f.svc.Status(ctx, "ag_1", []string{sub.RequestID})
	if err != nil || len(status) != 1 || status[0].Tools[0].Grant == "" {
		t.Fatalf("status: %v %+v", err, status)
	}
	token := status[0].Tools[0].Grant
	p.revoked = true
	res := f.call(t, WithAgentID(ctx, "ag_1"), "test", "bks_preview.snapshots", map[string]any{GrantField: token})
	if !res.IsError {
		t.Fatal("missing binding redeemed a grant")
	}
	p.revoked = false
	res = f.call(t, WithAgentID(ctx, "ag_1"), "test", "bks_preview.snapshots", map[string]any{GrantField: token})
	if res.IsError || textOf(res) != "queued" {
		t.Fatalf("grant was consumed on identity failure: %s", textOf(res))
	}
	if ev := f.lastMetric(t, "bks_preview.snapshots"); ev.ApprovalVia != "grant" {
		t.Fatalf("grant metric: %+v", ev)
	}
}

var _ PreviewCaller = (*previewControlFake)(nil)
