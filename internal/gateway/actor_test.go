package gateway

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tusharbhardwaj/toolyard/internal/actor"
	"github.com/tusharbhardwaj/toolyard/internal/approval"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/inbox"
	"github.com/tusharbhardwaj/toolyard/internal/memory"
	"github.com/tusharbhardwaj/toolyard/internal/metrics"
	"github.com/tusharbhardwaj/toolyard/internal/policy"
	"github.com/tusharbhardwaj/toolyard/internal/realtime"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// actorFixture is a gateway with no access resolver (so anonymous callers
// work, as on a local stdio gateway), a metrics sink, an audit tap and one
// upstream "t" with a read, a write and a policy-denied tool. The handler
// records the raiser and the arguments it was given.
type actorFixture struct {
	gw     *Gateway
	svc    *inbox.Service
	bus    *approval.Bus
	met    *fakeMetrics
	events <-chan audit.Event

	mu       sync.Mutex
	raisers  []actor.Raiser
	argsSeen []map[string]any
}

func newActorFixture(t *testing.T) *actorFixture {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "g.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	bus, err := approval.New(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	aud := audit.New(db)
	f := &actorFixture{bus: bus, met: &fakeMetrics{}}
	f.gw = New(Options{
		Policy: policy.New(db), Approval: bus, Audit: aud, Hub: realtime.NewHub(), Memory: memory.New(db),
		Metrics: f.met,
	})
	f.gw.RegisterBuiltins()
	t.Cleanup(func() { _ = f.gw.Close() })
	f.svc, err = inbox.New(context.Background(), inbox.Options{DB: db, Catalog: f.gw})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.svc.Flush)
	f.gw.SetInbox(f.svc, inbox.NewGuide(nil), func() string { return ApprovalModeExecute })
	f.gw.RegisterInboxTools()
	events, stop := aud.Subscribe()
	t.Cleanup(stop)
	f.events = events

	handler := func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		r, _ := actor.RaiserFrom(ctx)
		f.mu.Lock()
		f.raisers = append(f.raisers, r)
		f.argsSeen = append(f.argsSeen, args)
		f.mu.Unlock()
		return mcp.NewToolResultText("ran"), nil
	}
	deny := policy.ActionDeny
	for _, e := range []toolEntry{
		{tool: mcp.Tool{Name: "t.get_status"}, upstream: "t", originalName: "get_status"},
		{tool: mcp.Tool{Name: "t.run"}, upstream: "t", originalName: "run"},
		{tool: mcp.Tool{Name: "t.get_secret"}, upstream: "t", originalName: "get_secret", forcedAction: &deny},
	} {
		e.tool.InputSchema = mcp.ToolInputSchema{Type: "object", Properties: addMetaProps(map[string]any{})}
		e.reasonField, e.handle = ReasonField, handler
		f.gw.registerEntry(e)
	}
	return f
}

func (f *actorFixture) call(t *testing.T, ctx context.Context, via, tool string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	full := map[string]any{ReasonField: testReason}
	for k, v := range args {
		full[k] = v
	}
	res, err := f.gw.RouteCall(ctx, via, tool, full)
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	return res
}

func (f *actorFixture) lastRaiser(t *testing.T) (actor.Raiser, map[string]any) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.raisers) == 0 {
		t.Fatal("the tool never ran")
	}
	return f.raisers[len(f.raisers)-1], f.argsSeen[len(f.argsSeen)-1]
}

func (f *actorFixture) lastMetric(t *testing.T, tool string) metrics.Event {
	t.Helper()
	f.met.mu.Lock()
	defer f.met.mu.Unlock()
	for i := len(f.met.events) - 1; i >= 0; i-- {
		if f.met.events[i].ToolName == tool {
			return f.met.events[i]
		}
	}
	t.Fatalf("no metric for %s", tool)
	return metrics.Event{}
}

// drainAudit returns every audit event written so far.
func (f *actorFixture) drainAudit() []audit.Event {
	var out []audit.Event
	for {
		select {
		case e := <-f.events:
			out = append(out, e)
		default:
			return out
		}
	}
}

func findAudit(evs []audit.Event, eventType, tool string) (audit.Event, bool) {
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].EventType == eventType && evs[i].ToolName == tool {
			return evs[i], true
		}
	}
	return audit.Event{}, false
}

// mcpInitialize runs an initialize request through the MCP server as a
// client would, naming itself.
func mcpInitialize(t *testing.T, g *Gateway, ctx context.Context, name, version string) {
	t.Helper()
	msg, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": "2024-11-05", "capabilities": map[string]any{},
			"clientInfo": map[string]any{"name": name, "version": version},
		},
	})
	raw := g.MCPServer().HandleMessage(ctx, json.RawMessage(msg))
	b, _ := json.Marshal(raw)
	if strings.Contains(string(b), `"error"`) {
		t.Fatalf("initialize: %s", b)
	}
}

func TestClientKindOf(t *testing.T) {
	cases := map[string]string{
		"claude-code": "claude_code", "Claude Code": "claude_code", "claude_code": "claude_code",
		"claude-code/2.0": "claude_code", "codex": "codex", "codex-mcp-client": "codex",
		"cursor": "cursor", "Cursor": "cursor", "opencode": "opencode", "t3": "t3", "t3-code": "t3", "T3 Code": "t3",
		"": "unknown", "hermes": "unknown", "mcp-inspector": "unknown",
	}
	for in, want := range cases {
		if got := ClientKindOf(in); got != want {
			t.Errorf("ClientKindOf(%q) = %q, want %q", in, got, want)
		}
	}
}

// Via reaches the metric and the raiser for every path in: the meta-tool
// name from tools.execute, "dashboard", "cli", "voice", and "direct" for
// a call the MCP handler routes itself.
func TestViaReachesMetricsAndRaiser(t *testing.T) {
	f := newActorFixture(t)
	ctx := WithAgentID(context.Background(), "ag_1")
	for _, via := range []string{MetaExecuteTool, "dashboard", "cli", "voice"} {
		f.call(t, ctx, via, "t.get_status", nil)
		if ev := f.lastMetric(t, "t.get_status"); ev.Via != via {
			t.Fatalf("metric via = %q, want %q", ev.Via, via)
		}
		if r, _ := f.lastRaiser(t); r.Via != via || r.CallerID != "ag_1" || r.AgentKind != "agent" {
			t.Fatalf("raiser for %s: %+v", via, r)
		}
	}
	if reply := mcpToolsCall(t, f.gw, ctx, "t.get_status"); reply.rpcErr || reply.isError {
		t.Fatalf("direct call: %+v", reply)
	}
	if ev := f.lastMetric(t, "t.get_status"); ev.Via != "direct" {
		t.Fatalf("direct metric via = %q", ev.Via)
	}
	if r, _ := f.lastRaiser(t); r.Via != "direct" {
		t.Fatalf("direct raiser via = %q", r.Via)
	}
	// tools.execute end to end: the inner call is recorded as reached
	// through the meta-tool, the outer as direct.
	f.met.mu.Lock()
	f.met.events = nil
	f.met.mu.Unlock()
	res, err := f.gw.RouteCall(ctx, "direct", MetaExecuteTool, map[string]any{
		ReasonField: testReason, "tool": "t.get_status", "arguments": map[string]any{ReasonField: testReason},
	})
	if err != nil || res.IsError {
		t.Fatalf("tools.execute: %v %+v", err, res)
	}
	if ev := f.lastMetric(t, "t.get_status"); ev.Via != MetaExecuteTool {
		t.Fatalf("inner via = %q", ev.Via)
	}
	if ev := f.lastMetric(t, MetaExecuteTool); ev.Via != "direct" {
		t.Fatalf("outer via = %q", ev.Via)
	}
}

// The raiser is the ingress raiser, the client that introduced itself at
// initialize and the agent session the call names; the tool sees it on
// ctx and never sees `_session_id`.
func TestRaiserMergesContextClientInfoAndSession(t *testing.T) {
	f := newActorFixture(t)
	ctx := context.Background()
	agent := WithAgentID(ctx, "ag_1")
	mcpInitialize(t, f.gw, agent, "claude-code", "1.2.3")
	mine, err := f.svc.StartSession(ctx, "ag_1", "Ship billing", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := f.svc.StartSession(ctx, "ag_2", "Someone else's work", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	ingress := actor.WithRaiser(agent, actor.Raiser{
		CallerID: "ag_1", AgentName: "builder", OwnerUserID: "u_owner", OwnerEmail: "o@example.com",
		MCPSessionID: "mcp-1", ClientSessionID: "t3-thread", ClientIP: "10.0.0.7",
	})

	f.call(t, ingress, "test", "t.get_status", map[string]any{"q": 1, SessionField: mine.ID})
	r, args := f.lastRaiser(t)
	want := actor.Raiser{
		CallerID: "ag_1", AgentName: "builder", AgentKind: "agent", OwnerUserID: "u_owner", OwnerEmail: "o@example.com",
		MCPSessionID: "mcp-1", AgentSessionID: mine.ID, ClientSessionID: "t3-thread",
		ClientKind: "claude_code", ClientName: "claude-code/1.2.3", ClientIP: "10.0.0.7", Via: "test",
	}
	if r != want {
		t.Fatalf("raiser:\n got %+v\nwant %+v", r, want)
	}
	if _, leaked := args[SessionField]; leaked || args["q"] != 1 {
		t.Fatalf("upstream args: %v", args)
	}
	ev := f.lastMetric(t, "t.get_status")
	if ev.AgentName != "builder" || ev.OwnerUserID != "u_owner" || ev.ClientKind != "claude_code" || ev.SessionID != mine.ID || ev.Via != "test" {
		t.Fatalf("metric: %+v", ev)
	}
	// The session was heartbeated.
	if ss, _ := f.svc.GetSession(ctx, mine.ID); ss.LastHeartbeatAt < mine.LastHeartbeatAt {
		t.Fatal("session not heartbeated")
	}

	// Another agent's session is ignored: the metric falls back to the
	// MCP session, the tool still runs, the argument is still stripped.
	f.call(t, ingress, "test", "t.get_status", map[string]any{SessionField: theirs.ID})
	r, args = f.lastRaiser(t)
	if r.AgentSessionID != "" || r.MCPSessionID != "mcp-1" {
		t.Fatalf("foreign session accepted: %+v", r)
	}
	if _, leaked := args[SessionField]; leaked {
		t.Fatal("_session_id forwarded upstream")
	}
	if ev := f.lastMetric(t, "t.get_status"); ev.SessionID != "mcp-1" {
		t.Fatalf("metric session = %q", ev.SessionID)
	}
	// So is a session that doesn't exist, and one on an anonymous call.
	f.call(t, ingress, "test", "t.get_status", map[string]any{SessionField: "ses_nope"})
	if r, _ := f.lastRaiser(t); r.AgentSessionID != "" {
		t.Fatalf("unknown session accepted: %+v", r)
	}
	f.call(t, ctx, "test", "t.get_status", map[string]any{SessionField: mine.ID})
	if r, _ := f.lastRaiser(t); r.AgentSessionID != "" || r.CallerID != "" {
		t.Fatalf("anonymous call took a session: %+v", r)
	}
}

// Without a raiser on ctx the call works exactly as before, attributed to
// the caller id alone; dashboard: and voice: callers get their kind and
// owner from the id.
func TestRaiserWithoutIngressRaiser(t *testing.T) {
	f := newActorFixture(t)
	f.call(t, context.Background(), "test", "t.get_status", nil)
	if r, _ := f.lastRaiser(t); r != (actor.Raiser{Via: "test"}) {
		t.Fatalf("anonymous raiser: %+v", r)
	}
	if ev := f.lastMetric(t, "t.get_status"); ev.AgentID != "" || ev.Outcome != metrics.OutcomeOK {
		t.Fatalf("anonymous metric: %+v", ev)
	}
	f.call(t, WithAgentID(context.Background(), "dashboard:u_9"), "dashboard", "t.get_status", nil)
	r, _ := f.lastRaiser(t)
	if r.AgentKind != "dashboard" || r.OwnerUserID != "u_9" || r.CallerID != "dashboard:u_9" || r.Via != "dashboard" {
		t.Fatalf("dashboard raiser: %+v", r)
	}
	if ev := f.lastMetric(t, "t.get_status"); ev.OwnerUserID != "u_9" {
		t.Fatalf("dashboard metric: %+v", ev)
	}
	f.call(t, WithAgentID(context.Background(), "voice:u_9"), "voice", "t.get_status", nil)
	if r, _ := f.lastRaiser(t); r.AgentKind != "voice" || r.OwnerUserID != "u_9" {
		t.Fatalf("voice raiser: %+v", r)
	}
	// A wrong kind from ingress is corrected for these callers.
	wrong := actor.WithRaiser(WithAgentID(context.Background(), "dashboard:u_9"), actor.Raiser{AgentKind: "agent"})
	f.call(t, wrong, "dashboard", "t.get_status", nil)
	if r, _ := f.lastRaiser(t); r.AgentKind != "dashboard" {
		t.Fatalf("kind not fixed: %+v", r)
	}
}

// Policy allow and deny rows name the rule that decided; the schema reject
// row names the agent.
func TestPolicyDeciderOnAuditRows(t *testing.T) {
	f := newActorFixture(t)
	ctx := WithAgentID(context.Background(), "ag_1")
	f.call(t, ctx, "test", "t.get_status", nil)
	evs := f.drainAudit()
	row, ok := findAudit(evs, audit.EventCallAllowed, "t.get_status")
	if !ok || row.DecidedVia != actor.ViaPolicy || row.DeciderRef == "" || row.DecidedByUserID != "" {
		t.Fatalf("allow row decider: %+v", row)
	}
	res := f.call(t, ctx, "test", "t.get_secret", nil)
	if !res.IsError {
		t.Fatal("forced deny ran")
	}
	row, ok = findAudit(f.drainAudit(), audit.EventCallDenied, "t.get_secret")
	if !ok || row.DecidedVia != actor.ViaPolicy || row.DeciderRef != "builtin-forced-deny" {
		t.Fatalf("deny row decider: %+v", row)
	}
	if _, err := f.gw.RouteCall(ctx, "test", "t.get_status", map[string]any{ReasonField: "short"}); err != nil {
		t.Fatal(err)
	}
	row, ok = findAudit(f.drainAudit(), audit.EventCallFailed, "t.get_status")
	if !ok || row.AgentID != "ag_1" || !strings.HasPrefix(row.ResultSummary, "rejected:") {
		t.Fatalf("schema reject row: %+v", row)
	}
}

// A call run under an inbox grant records the grant as the instrument and
// the owner who issued it as the decider, by user id, email and name, the
// same person the inbox.decide row names.
func TestGrantUseDecider(t *testing.T) {
	f := newActorFixture(t)
	ctx := context.Background()
	agent := WithAgentID(ctx, "ag_1")
	sub, err := f.svc.Submit(ctx, "ag_1", &inbox.Submission{
		Kind: inbox.KindAccess, Title: "Run the thing", Summary: "One run.", Message: "I'd like to run t.run once in production to finish the task.",
		Facts: &inbox.Facts{WhyNow: "w", IfItGoesWrong: "g", Undo: "u"}, Audio: inbox.Audio{Script: "Run it?"}, Urgency: inbox.UrgencySoon,
		Tools: []inbox.SubmissionTool{{Tool: "t.run", Required: true, Summary: "Run it.", Params: map[string]any{"env": "prod"}}},
	})
	if err != nil || !sub.OK {
		t.Fatalf("submit: %v %+v", err, sub)
	}
	f.svc.Flush()
	alice := actor.Decider{UserID: "u_alice", Email: "alice@example.com", Name: "Alice", Via: actor.ViaDashboard}
	if _, err := f.svc.Decide(ctx, sub.RequestID, inbox.Decision{Action: "approve", Allow: []bool{true}, Decider: alice}); err != nil {
		t.Fatal(err)
	}
	status, err := f.svc.Status(ctx, "ag_1", []string{sub.RequestID})
	if err != nil || len(status) != 1 || status[0].Tools[0].Grant == "" {
		t.Fatalf("status: %v %+v", err, status)
	}
	tok, grantID := status[0].Tools[0].Grant, status[0].Tools[0].GrantID
	f.drainAudit()
	res := f.call(t, agent, "test", "t.run", map[string]any{"env": "prod", GrantField: tok})
	if res.IsError {
		t.Fatalf("grant call: %+v", res)
	}
	row, ok := findAudit(f.drainAudit(), audit.EventCallAllowed, "t.run")
	if !ok || row.Decision != "grant" || row.DecidedVia != actor.ViaInboxGrant || row.DeciderRef != grantID || row.DecidedByUserID != "u_alice" {
		t.Fatalf("grant row: %+v", row)
	}
	if row.DecidedByEmail != "alice@example.com" || row.DecidedByName != "Alice" {
		t.Fatalf("grant row decider email/name = %q/%q, want alice@example.com/Alice", row.DecidedByEmail, row.DecidedByName)
	}
	ev := f.lastMetric(t, "t.run")
	if ev.ApprovalVia != "grant" || ev.ApprovalDecider != "u_alice" || ev.ApprovalID != grantID {
		t.Fatalf("grant metric: %+v", ev)
	}
}

// Execute runs an approved request with the raiser it was raised with and
// fills the approval columns from the human's decision.
func TestExecuteKeepsRaiserAndApprovalMetrics(t *testing.T) {
	f := newActorFixture(t)
	ctx := context.Background()
	raised := actor.Raiser{CallerID: "ag_1", AgentName: "builder", AgentKind: "agent", OwnerUserID: "u_owner",
		AgentSessionID: "ses_0123456789abcdef", ClientKind: "codex", Via: MetaExecuteTool}
	req, err := f.bus.Hold(ctx, approval.NewRequest{
		AgentID: "ag_1", UpstreamName: "t", ToolName: "t.run", Arguments: map[string]any{"env": "prod"},
		Reason: testReason, RaisedBy: raised,
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.bus.Decide(ctx, req.ID, approval.StatusAllowed, "u_owner"); err != nil {
		t.Fatal(err)
	}
	req, err = f.bus.Get(ctx, req.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Set on the value: the row's own persistence of these is another
	// builder's; Execute only reads them.
	req.RaisedBy = &raised
	req.DecidedVia, req.DeciderEmail, req.DeciderName = actor.ViaDashboard, "o@example.com", "Owner"
	req.DecidedAt = req.CreatedAt + 1500

	f.drainAudit()
	f.gw.Execute(ctx, req)
	r, args := f.lastRaiser(t)
	if r != raised {
		t.Fatalf("raiser on execute:\n got %+v\nwant %+v", r, raised)
	}
	if args["env"] != "prod" {
		t.Fatalf("args: %v", args)
	}
	ev := f.lastMetric(t, "t.run")
	if ev.Via != "auto-execute" || ev.ApprovalVia != actor.ViaDashboard || ev.ApprovalDecider != "u_owner" ||
		ev.ApprovalLatencyMs != 1500 || ev.AgentName != "builder" || ev.OwnerUserID != "u_owner" ||
		ev.SessionID != "ses_0123456789abcdef" || ev.ClientKind != "codex" || ev.ApprovalID != req.ID {
		t.Fatalf("execute metric: %+v", ev)
	}
	if row, ok := findAudit(f.drainAudit(), audit.EventCallSucceeded, "t.run"); !ok || row.ApprovalID != req.ID || row.AgentID != "ag_1" {
		t.Fatalf("succeeded row: %+v %v", row, ok)
	}
	got, _ := f.bus.Get(ctx, req.ID)
	if got.ResultExecutedAt == 0 {
		t.Fatal("result not persisted")
	}
}

// A request held in-line and decided by an auto-rule or a person records
// that decider on the allowed row and the metric.
func TestHoldAndWaitRecordsDecider(t *testing.T) {
	f := newActorFixture(t)
	f.gw.inLineWait = 2 * time.Second
	ctx := WithAgentID(context.Background(), "ag_1")
	done := make(chan *mcp.CallToolResult, 1)
	go func() {
		res, _ := f.gw.RouteCall(ctx, "test", "t.run", map[string]any{ReasonField: testReason, "env": "prod"})
		done <- res
	}()
	var pending []approval.Request
	deadline := time.Now().Add(2 * time.Second)
	for len(pending) == 0 && time.Now().Before(deadline) {
		pending, _ = f.bus.ListPendingByAgent(context.Background(), "ag_1")
		time.Sleep(10 * time.Millisecond)
	}
	if len(pending) != 1 {
		t.Fatal("no pending approval")
	}
	if pending[0].RaisedBy == nil || pending[0].RaisedBy.CallerID != "ag_1" || pending[0].RaisedBy.Via != "test" {
		t.Fatalf("raised_by on the approval: %+v", pending[0].RaisedBy)
	}
	if _, err := f.bus.Decide(context.Background(), pending[0].ID, approval.StatusDenied, "u_owner"); err != nil {
		t.Fatal(err)
	}
	res := <-done
	if res == nil || !res.IsError {
		t.Fatalf("denied call: %+v", res)
	}
	row, ok := findAudit(f.drainAudit(), audit.EventCallDenied, "t.run")
	if !ok || row.DecidedByUserID != "u_owner" || row.DecidedVia != actor.ViaDashboard || row.ApprovalID != pending[0].ID {
		t.Fatalf("denied row: %+v", row)
	}
	ev := f.lastMetric(t, "t.run")
	if ev.ApprovalVia != actor.ViaDashboard || ev.ApprovalDecider != "u_owner" || ev.ApprovalOutcome != metrics.ApprovalDenied {
		t.Fatalf("denied metric: %+v", ev)
	}
}

func TestApprovalDeciderReadsLegacyValues(t *testing.T) {
	cases := []struct {
		name string
		req  approval.Request
		want actor.Decider
	}{
		{"auto rule, legacy", approval.Request{AutoDecidedBy: "rule_1", DecidedBy: "rule:rule_1"}, actor.Decider{Via: actor.ViaAutoRule, Ref: "rule_1"}},
		{"auto rule, no row value", approval.Request{AutoDecidedBy: "rule_1"}, actor.Decider{Via: actor.ViaAutoRule, Ref: "rule_1"}},
		{"structured auto", approval.Request{AutoDecidedBy: "rule_1", DecidedVia: actor.ViaAutoRule, DeciderRef: "rule_1", DecidedBy: "u_creator"},
			actor.Decider{Via: actor.ViaAutoRule, Ref: "rule_1", UserID: "u_creator"}},
		{"legacy user", approval.Request{DecidedBy: "u_1"}, actor.Decider{UserID: "u_1", Via: actor.ViaDashboard}},
		{"structured user", approval.Request{DecidedBy: "u_1", DecidedVia: actor.ViaTelegram, DeciderRef: "12345", DeciderName: "Ann"},
			actor.Decider{UserID: "u_1", Via: actor.ViaTelegram, Ref: "12345", Name: "Ann"}},
		{"legacy via:ref", approval.Request{DecidedBy: "agent:ag_1"}, actor.Decider{Via: actor.ViaAgentCancel, Ref: "ag_1"}},
		{"legacy via", approval.Request{DecidedBy: "expiry"}, actor.Decider{Via: actor.ViaExpiry}},
		{"expired row", approval.Request{Status: approval.StatusExpired}, actor.Decider{Via: actor.ViaExpiry}},
		{"undecided", approval.Request{}, actor.Decider{}},
	}
	for _, c := range cases {
		if got := approvalDecider(&c.req); got != c.want {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestSchemaAdvertisesAndStripsSessionField(t *testing.T) {
	out, _ := wrapSchema(mcp.Tool{Name: "x", InputSchema: mcp.ToolInputSchema{Type: "object", Properties: map[string]any{"a": map[string]any{"type": "string"}}}})
	if _, ok := out.InputSchema.Properties[SessionField]; !ok {
		t.Fatal("wrapped schema lacks _session_id")
	}
	for _, r := range out.InputSchema.Required {
		if r == SessionField {
			t.Fatal("_session_id must be optional")
		}
	}
	_, _, clean, err := extractReason(map[string]any{ReasonField: testReason, SessionField: "ses_1", "a": "b"}, ReasonField)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := clean[SessionField]; ok || clean["a"] != "b" {
		t.Fatalf("clean args: %v", clean)
	}
}

// fakeAuto is an approval.AutoApprover that allows everything under one
// rule, created by one person.
type fakeAuto struct{ rule, creator string }

func (a fakeAuto) Match(string, string, string, string, bool) *approval.AutoMatch {
	return &approval.AutoMatch{ID: a.rule, Kind: "tool", CreatedBy: a.creator}
}
func (fakeAuto) MarkHit(context.Context, string, string)            {}
func (fakeAuto) MarkDenial(context.Context, string, string, string) {}
func (fakeAuto) IsDestructive(context.Context, string) bool         { return false }

// A request decided by an auto-rule records via "auto" and the rule id
// whether the gateway holds the fresh response from Hold or re-reads the
// row later (resumeDeferred, Execute); the audit row names the rule (the
// rule decided, not the person who created it).
func TestApprovalMetricsAgreeInlineAndReread(t *testing.T) {
	f := newActorFixture(t)
	f.bus.SetAutoApprover(fakeAuto{rule: "rule_1", creator: "u_creator"})
	ctx := WithAgentID(context.Background(), "ag_1")
	f.drainAudit()

	res := f.call(t, ctx, "test", "t.run", map[string]any{"env": "prod"})
	if res.IsError {
		t.Fatalf("inline auto-approve: %+v", res)
	}
	inline := f.lastMetric(t, "t.run")
	if inline.ApprovalVia != "auto" || inline.ApprovalDecider != "rule_1" || inline.ApprovalOutcome != metrics.ApprovalAuto || inline.ApprovalID == "" {
		t.Fatalf("inline metric: %+v", inline)
	}
	row, ok := findAudit(f.drainAudit(), audit.EventCallAllowed, "t.run")
	if !ok || row.DecidedVia != actor.ViaAutoRule || row.DeciderRef != "rule_1" || row.DecidedByUserID != "" {
		t.Fatalf("inline allow row: %+v", row)
	}

	// The legacy `_approval_id` re-call reads the row back.
	res = f.call(t, ctx, "test", "t.run", map[string]any{ApprovalIDField: inline.ApprovalID})
	if res.IsError {
		t.Fatalf("resume: %+v", res)
	}
	resumed := f.lastMetric(t, "t.run")
	if resumed.ApprovalVia != inline.ApprovalVia || resumed.ApprovalDecider != inline.ApprovalDecider || resumed.ApprovalOutcome != metrics.ApprovalAuto {
		t.Fatalf("resume metric %+v disagrees with inline %+v", resumed, inline)
	}

	// So does the executor, given the row from the database.
	req, err := f.bus.Get(context.Background(), inline.ApprovalID)
	if err != nil {
		t.Fatal(err)
	}
	f.gw.Execute(context.Background(), req)
	executed := f.lastMetric(t, "t.run")
	if executed.Via != "auto-execute" || executed.ApprovalVia != inline.ApprovalVia || executed.ApprovalDecider != inline.ApprovalDecider ||
		executed.ApprovalOutcome != inline.ApprovalOutcome {
		t.Fatalf("execute metric %+v disagrees with inline %+v", executed, inline)
	}
}

func TestApprovalMetricsFromDecider(t *testing.T) {
	cases := []struct {
		name         string
		req          approval.Request
		via, decider string
	}{
		{"auto, fresh from Hold", approval.Request{AutoDecidedBy: "rule_1", DecidedVia: actor.ViaAutoRule, DeciderRef: "rule_1", DecidedBy: "u_creator"}, "auto", "rule_1"},
		{"auto, re-read", approval.Request{DecidedVia: actor.ViaAutoRule, DeciderRef: "rule_1", DecidedBy: "u_creator"}, "auto", "rule_1"},
		{"auto, legacy row", approval.Request{DecidedBy: "rule:rule_1"}, "auto", "rule_1"},
		{"auto, response only", approval.Request{AutoDecidedBy: "rule_1"}, "auto", "rule_1"},
		{"person on the dashboard", approval.Request{DecidedVia: actor.ViaDashboard, DecidedBy: "u_1", DeciderName: "Ann"}, actor.ViaDashboard, "u_1"},
		{"push token", approval.Request{DecidedVia: actor.ViaPushToken, DecidedBy: "u_1"}, actor.ViaPushToken, "u_1"},
		{"telegram, no user", approval.Request{DecidedVia: actor.ViaTelegram, DeciderRef: "12345", DecidedBy: "telegram:12345"}, actor.ViaTelegram, "telegram:12345"},
		{"undecided", approval.Request{}, "", ""},
	}
	for _, c := range cases {
		var ev metrics.Event
		c.req.CreatedAt, c.req.DecidedAt = 1000, 3500
		approvalMetrics(&ev, &c.req)
		if ev.ApprovalVia != c.via || ev.ApprovalDecider != c.decider || ev.ApprovalLatencyMs != 2500 {
			t.Errorf("%s: via=%q decider=%q latency=%d, want %q %q 2500", c.name, ev.ApprovalVia, ev.ApprovalDecider, ev.ApprovalLatencyMs, c.via, c.decider)
		}
	}
}

// fakeSession is the mcp-go client session a stateful transport puts on
// ctx before handling initialize.
type fakeSession struct {
	id string
	ch chan mcp.JSONRPCNotification
}

func (s *fakeSession) Initialize()                                         {}
func (s *fakeSession) Initialized() bool                                   { return true }
func (s *fakeSession) NotificationChannel() chan<- mcp.JSONRPCNotification { return s.ch }
func (s *fakeSession) SessionID() string                                   { return s.id }

// The clientInfo cache tells two clients on one agent token apart by MCP
// session, then by the client's own session id under -stateless-mcp, and
// only then falls back to the agent; anonymous callers are never cached.
func TestClientInfoKeyedBySession(t *testing.T) {
	f := newActorFixture(t)
	agent := WithAgentID(context.Background(), "ag_1")
	withSession := func(id string) context.Context {
		return f.gw.MCPServer().WithContext(agent, &fakeSession{id: id, ch: make(chan mcp.JSONRPCNotification, 8)})
	}
	client := func(r actor.Raiser) actor.Raiser {
		r.CallerID = "ag_1"
		f.call(t, actor.WithRaiser(agent, r), "test", "t.get_status", nil)
		got, _ := f.lastRaiser(t)
		return got
	}

	// Stateful: one token, two connections, two clients.
	mcpInitialize(t, f.gw, withSession("mcp-A"), "claude-code", "1.0")
	mcpInitialize(t, f.gw, withSession("mcp-B"), "t3-code", "2.0")
	if r := client(actor.Raiser{MCPSessionID: "mcp-A"}); r.ClientName != "claude-code/1.0" || r.ClientKind != "claude_code" {
		t.Fatalf("session A: %+v", r)
	}
	if r := client(actor.Raiser{MCPSessionID: "mcp-B"}); r.ClientName != "t3-code/2.0" || r.ClientKind != "t3" {
		t.Fatalf("session B: %+v", r)
	}
	// A session the cache never saw, and nothing session-less to fall
	// back on: unnamed rather than guessed.
	if r := client(actor.Raiser{MCPSessionID: "mcp-C"}); r.ClientName != "" || r.ClientKind != "" {
		t.Fatalf("unknown session named a client: %+v", r)
	}

	// Stateless: no MCP session; the client's own session id (a T3
	// thread from the ingress raiser) keeps two clients apart.
	mcpInitialize(t, f.gw, actor.WithRaiser(agent, actor.Raiser{CallerID: "ag_1", ClientSessionID: "thread-1"}), "cursor", "0.5")
	mcpInitialize(t, f.gw, actor.WithRaiser(agent, actor.Raiser{CallerID: "ag_1", ClientSessionID: "thread-2"}), "opencode", "3")
	if r := client(actor.Raiser{ClientSessionID: "thread-1"}); r.ClientName != "cursor/0.5" || r.ClientKind != "cursor" {
		t.Fatalf("thread-1: %+v", r)
	}
	if r := client(actor.Raiser{ClientSessionID: "thread-2"}); r.ClientName != "opencode/3" || r.ClientKind != "opencode" {
		t.Fatalf("thread-2: %+v", r)
	}
	// A call that names both sessions prefers the MCP one.
	if r := client(actor.Raiser{MCPSessionID: "mcp-A", ClientSessionID: "thread-2"}); r.ClientName != "claude-code/1.0" {
		t.Fatalf("mcp session should win: %+v", r)
	}

	// Neither session id anywhere: the agent alone.
	if r := client(actor.Raiser{}); r.ClientName != "" {
		t.Fatalf("no session-less entry yet: %+v", r)
	}
	mcpInitialize(t, f.gw, agent, "codex", "9")
	if r := client(actor.Raiser{}); r.ClientName != "codex/9" || r.ClientKind != "codex" {
		t.Fatalf("agent-only: %+v", r)
	}
	// An ingress client kind (a trusted header) wins over the client's
	// self-description; the name is still filled in.
	if r := client(actor.Raiser{ClientKind: "cli"}); r.ClientKind != "cli" || r.ClientName != "codex/9" {
		t.Fatalf("ingress kind: %+v", r)
	}

	// Anonymous: initialize is not cached and never named.
	mcpInitialize(t, f.gw, context.Background(), "hermes", "1")
	f.call(t, context.Background(), "test", "t.get_status", nil)
	if r, _ := f.lastRaiser(t); r.ClientName != "" || r.ClientKind != "" {
		t.Fatalf("anonymous caller named: %+v", r)
	}
	f.gw.clientMu.Lock()
	_, cached := f.gw.clients[""]
	f.gw.clientMu.Unlock()
	if cached {
		t.Fatal("anonymous clientInfo cached")
	}
}
