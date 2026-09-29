package api

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/actor"
	"github.com/tusharbhardwaj/toolyard/internal/approval"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/auditlink"
	"github.com/tusharbhardwaj/toolyard/internal/gateway"
	"github.com/tusharbhardwaj/toolyard/internal/hooks"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/inbox"
	"github.com/tusharbhardwaj/toolyard/internal/memory"
	"github.com/tusharbhardwaj/toolyard/internal/policy"
	"github.com/tusharbhardwaj/toolyard/internal/realtime"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

const actorReason = "checking who raised and who decided, end to end"

// actorEnv is a Server on a store whose admin has an email and a display
// name, with the approval bus mirrored into the audit log the way main.go
// wires it (auditlink), a gateway with the built-in tools as the bus
// executor, the inbox and the hooks service, behind the HardenAPI ->
// RoleGuard -> mux chain.
type actorEnv struct {
	db       *store.DB
	srv      *Server
	handler  http.Handler
	id       *identity.Service
	audit    *audit.Logger
	bus      *approval.Bus
	gw       *gateway.Gateway
	inbox    *inbox.Service
	mem      *memory.Service
	admin    *identity.User
	agent    *identity.Agent
	agentTok string
	cookie   *http.Cookie
	sid      string // the cookie's dashboard session id
}

func newActorEnv(t *testing.T) *actorEnv {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "actors-api.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	id := identity.New(db)
	admin, err := id.CreateUser(ctx, "ada", "long-enough-password")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE users SET email = ?, display_name = ? WHERE id = ?`,
		"ada@beknown.work", "Ada Lovelace", admin.ID); err != nil {
		t.Fatal(err)
	}
	if admin, err = id.GetUserByID(ctx, admin.ID); err != nil {
		t.Fatal(err)
	}
	auditLog := audit.New(db)
	bus, err := approval.New(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	bus.AddNotifier(auditlink.Notifier(auditLog))
	mem := memory.New(db)
	gw := gateway.New(gateway.Options{Policy: policy.New(db), Approval: bus, Audit: auditLog, Memory: mem, Hub: realtime.NewHub()})
	gw.RegisterBuiltins()
	t.Cleanup(func() { _ = gw.Close() })
	bus.SetExecutor(gw)
	svc, err := inbox.New(ctx, inbox.Options{DB: db, Catalog: gw})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Flush)
	agentTok, ag, err := id.CreateAgentWithToken(ctx, admin.ID, "claude-cloud-3")
	if err != nil {
		t.Fatal(err)
	}
	if ag, err = id.VerifyAgentToken(ctx, agentTok); err != nil {
		t.Fatal(err)
	}
	_, proxy, _ := net.ParseCIDR("10.0.0.0/8")
	srv := New(ctx, Options{
		Identity: id, Audit: auditLog, Approval: bus, Gateway: gw, Inbox: svc, Hooks: hooks.New(db, nil),
		SessionKey: []byte("0123456789abcdef0123456789abcdef"),
		Security:   SecurityOptions{TrustedProxies: []*net.IPNet{proxy}},
	})
	mux := http.NewServeMux()
	srv.Routes(mux)
	e := &actorEnv{db: db, srv: srv, handler: srv.HardenAPI(srv.RoleGuard(mux)), id: id, audit: auditLog,
		bus: bus, gw: gw, inbox: svc, mem: mem, admin: admin, agent: ag, agentTok: agentTok}

	rec := httptest.NewRecorder()
	if err := srv.issueSession(rec, httptest.NewRequest(http.MethodGet, "/", nil), admin.ID); err != nil {
		t.Fatal(err)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName {
			e.cookie = c
		}
	}
	if e.cookie == nil {
		t.Fatal("no session cookie issued")
	}
	withCookie := httptest.NewRequest(http.MethodGet, "/", nil)
	withCookie.AddCookie(e.cookie)
	if e.sid = srv.currentSessionID(withCookie); e.sid == "" {
		t.Fatal("cookie has no session id")
	}
	return e
}

// do sends a dashboard-style request (CSRF header, JSON body) through
// the full chain; cookie may be nil.
func (e *actorEnv) do(t *testing.T, cookie *http.Cookie, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Requested-With", "toolyard")
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

// bearer sends an agent-authenticated request (the CLI / hook shape).
func (e *actorEnv) bearer(t *testing.T, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = "198.51.100.4:6000"
	req.Header.Set("Authorization", "Bearer "+e.agentTok)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

// agentRaiser is what the MCP ingress would have put on the context for
// this agent.
func (e *actorEnv) agentRaiser() actor.Raiser {
	return actor.Raiser{
		CallerID: e.agent.ID, AgentName: e.agent.Name, AgentKind: e.agent.Kind,
		OwnerUserID: e.agent.Owner, OwnerEmail: e.agent.OwnerEmail, OwnerName: e.agent.OwnerName(),
		ClientKind: "t3", ClientSessionID: "thr_42", ClientIP: "203.0.113.9",
	}
}

// hold parks a request for a human, raised by the agent.
func (e *actorEnv) hold(t *testing.T, key string) *approval.Request {
	t.Helper()
	req, err := e.bus.Hold(context.Background(), approval.NewRequest{
		AgentID: e.agent.ID, UpstreamName: "builtin", ToolName: "memory.set",
		Arguments: map[string]any{"key": key, "value": "v"}, Reason: actorReason,
		RequireHuman: true, RaisedBy: e.agentRaiser(),
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func (e *actorEnv) get(t *testing.T, id string) *approval.Request {
	t.Helper()
	req, err := e.bus.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

// waitEvent returns the audit row of eventType for approvalID (or, when
// approvalID is empty, the first of that type with tool), waiting for
// the bus's asynchronous fan-out.
func (e *actorEnv) waitEvent(t *testing.T, eventType, approvalID, tool string) audit.Event {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		evs, err := e.audit.Recent(context.Background(), 500)
		if err != nil {
			t.Fatal(err)
		}
		for _, ev := range evs {
			if ev.EventType == eventType && (approvalID == "" || ev.ApprovalID == approvalID) && (tool == "" || ev.ToolName == tool) {
				return ev
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no %s audit row for approval %q tool %q", eventType, approvalID, tool)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (e *actorEnv) countEvents(t *testing.T, eventType, summary string) int {
	t.Helper()
	evs, err := e.audit.Recent(context.Background(), 500)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, ev := range evs {
		if ev.EventType == eventType && (summary == "" || ev.ResultSummary == summary) {
			n++
		}
	}
	return n
}

func (e *actorEnv) wantDashboardDecider(via string) actor.Decider {
	return actor.Decider{UserID: e.admin.ID, Email: "ada@beknown.work", Name: "Ada Lovelace", Via: via, Ref: e.sid}
}

func checkAuditDecider(t *testing.T, ev audit.Event, want actor.Decider) {
	t.Helper()
	got := actor.Decider{UserID: ev.DecidedByUserID, Email: ev.DecidedByEmail, Name: ev.DecidedByName, Via: ev.DecidedVia, Ref: ev.DeciderRef}
	if got != want {
		t.Fatalf("%s audit decider = %+v, want %+v", ev.EventType, got, want)
	}
}

func checkAuditRaiser(t *testing.T, ev audit.Event, e *actorEnv) {
	t.Helper()
	if ev.AgentID != e.agent.ID || ev.AgentName != "claude-cloud-3" || ev.AgentKind != identity.AgentKindAgent ||
		ev.OwnerUserID != e.admin.ID || ev.OwnerEmail != "ada@beknown.work" || ev.OwnerName != "Ada Lovelace" ||
		ev.ClientKind != "t3" || ev.ClientSessionID != "thr_42" || ev.ClientIP != "203.0.113.9" {
		t.Fatalf("%s audit raiser: %+v", ev.EventType, ev.Raiser)
	}
}

// Every API decision path records who decided and how, on the approval
// row and on the approval.decide audit row (which also carries who
// raised the call).
func TestApprovalDecisionPathsRecordDecider(t *testing.T) {
	e := newActorEnv(t)

	t.Run("dashboard", func(t *testing.T) {
		req := e.hold(t, "dashboard")
		rec := e.do(t, e.cookie, http.MethodPost, "/v1/approvals/"+req.ID+"/decide", `{"action":"denied"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("decide: %d %s", rec.Code, rec.Body.String())
		}
		row := e.get(t, req.ID)
		want := e.wantDashboardDecider(actor.ViaDashboard)
		if row.Status != approval.StatusDenied || row.Decider() != want || row.DecidedBy != e.admin.ID {
			t.Fatalf("row: status %s decider %+v decided_by %q", row.Status, row.Decider(), row.DecidedBy)
		}
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if body["decided_via"] != actor.ViaDashboard || body["decider_email"] != "ada@beknown.work" || body["decider_name"] != "Ada Lovelace" {
			t.Fatalf("response decider fields: %v", body)
		}
		ev := e.waitEvent(t, audit.EventApprovalDecide, req.ID, "")
		checkAuditDecider(t, ev, want)
		checkAuditRaiser(t, ev, e)
		if ev.Decision != approval.StatusDenied {
			t.Fatalf("decision = %q", ev.Decision)
		}
		create := e.waitEvent(t, audit.EventApprovalCreate, req.ID, "")
		checkAuditRaiser(t, create, e)
		if create.DecidedVia != "" || create.Reason != actorReason {
			t.Fatalf("create row: %+v", create)
		}
	})

	t.Run("batch", func(t *testing.T) {
		a, b := e.hold(t, "batch-a"), e.hold(t, "batch-b")
		rec := e.do(t, e.cookie, http.MethodPost, "/v1/approvals/decide-batch",
			`{"ids":["`+a.ID+`","`+b.ID+`"],"action":"denied"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("batch: %d %s", rec.Code, rec.Body.String())
		}
		da, db := e.get(t, a.ID).Decider(), e.get(t, b.ID).Decider()
		if da.Via != actor.ViaDashboardBatch || !strings.HasPrefix(da.Ref, "batch_") || da.Ref != db.Ref ||
			da.UserID != e.admin.ID || da.Email != "ada@beknown.work" || da.Name != "Ada Lovelace" || db.UserID != e.admin.ID {
			t.Fatalf("batch deciders: %+v / %+v", da, db)
		}
		checkAuditDecider(t, e.waitEvent(t, audit.EventApprovalDecide, a.ID, ""), da)
		checkAuditDecider(t, e.waitEvent(t, audit.EventApprovalDecide, b.ID, ""), db)
	})

	t.Run("token with a signed-in session", func(t *testing.T) {
		req := e.hold(t, "token-session")
		rec := e.do(t, e.cookie, http.MethodPost, "/v1/approvals/decide-by-token",
			`{"token":"`+req.DecisionToken+`","action":"denied"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("token+cookie: %d %s", rec.Code, rec.Body.String())
		}
		want := e.wantDashboardDecider(actor.ViaPushToken)
		if got := e.get(t, req.ID).Decider(); got != want {
			t.Fatalf("decider = %+v, want %+v", got, want)
		}
		checkAuditDecider(t, e.waitEvent(t, audit.EventApprovalDecide, req.ID, ""), want)
	})

	t.Run("bound token without a session", func(t *testing.T) {
		req := e.hold(t, "token-bound")
		tok := e.bus.DecisionTokenFor(req.ID, e.admin.ID)
		rec := e.do(t, nil, http.MethodPost, "/v1/approvals/decide-by-token", `{"token":"`+tok+`","action":"denied"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("bound token: %d %s", rec.Code, rec.Body.String())
		}
		want := actor.Decider{UserID: e.admin.ID, Email: "ada@beknown.work", Name: "Ada Lovelace", Via: actor.ViaPushToken}
		if got := e.get(t, req.ID).Decider(); got != want {
			t.Fatalf("decider = %+v, want %+v", got, want)
		}
		checkAuditDecider(t, e.waitEvent(t, audit.EventApprovalDecide, req.ID, ""), want)
	})

	t.Run("legacy token without a session", func(t *testing.T) {
		req := e.hold(t, "token-legacy")
		rec := e.do(t, nil, http.MethodPost, "/v1/approvals/decide-by-token",
			`{"token":"`+req.DecisionToken+`","action":"denied"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("legacy token: %d %s", rec.Code, rec.Body.String())
		}
		want := actor.Decider{Via: actor.ViaPushToken}
		if got := e.get(t, req.ID).Decider(); got != want {
			t.Fatalf("decider = %+v, want %+v", got, want)
		}
		checkAuditDecider(t, e.waitEvent(t, audit.EventApprovalDecide, req.ID, ""), want)
	})

	t.Run("forged token", func(t *testing.T) {
		req := e.hold(t, "token-forged")
		rec := e.do(t, e.cookie, http.MethodPost, "/v1/approvals/decide-by-token", `{"token":"bm9wZQ","action":"denied"}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("forged token: %d %s", rec.Code, rec.Body.String())
		}
		if e.get(t, req.ID).Status != approval.StatusPending {
			t.Fatal("forged token decided the request")
		}
	})
}

// The workbench, the CLI and hook ingest each put a raiser with the
// person's email and name on the audit rows they write.
func TestRESTEntryPointsRecordRaiser(t *testing.T) {
	e := newActorEnv(t)
	ctx := context.Background()

	t.Run("dashboard workbench", func(t *testing.T) {
		rec := e.do(t, e.cookie, http.MethodPost, "/v1/tools/run",
			`{"tool":"memory.get","arguments":{"key":"k","_reason":"`+actorReason+`"}}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("tools/run: %d %s", rec.Code, rec.Body.String())
		}
		evs, _ := e.audit.Recent(ctx, 100)
		var found bool
		for _, ev := range evs {
			if ev.AgentID != "dashboard:"+e.admin.ID || ev.ToolName != "memory.get" {
				continue
			}
			found = true
			if ev.AgentKind != "dashboard" || ev.OwnerUserID != e.admin.ID || ev.OwnerEmail != "ada@beknown.work" ||
				ev.OwnerName != "Ada Lovelace" || ev.ClientKind != "browser" || ev.Via != "dashboard" {
				t.Fatalf("%s raiser: %+v", ev.EventType, ev.Raiser)
			}
		}
		if !found {
			t.Fatal("no audit row for the workbench call")
		}
	})

	t.Run("cli", func(t *testing.T) {
		rec := e.bearer(t, http.MethodPost, "/v1/agents/tools/run",
			`{"tool":"memory.get","arguments":{"key":"k","_reason":"`+actorReason+`"}}`,
			map[string]string{"User-Agent": "toolyard-cli/dev", "x-toolyard-client": "cli"})
		if rec.Code != http.StatusOK {
			t.Fatalf("cli run: %d %s", rec.Code, rec.Body.String())
		}
		ev := e.waitEvent(t, "call.cli", "", "memory.get")
		if ev.AgentID != e.agent.ID || ev.AgentName != "claude-cloud-3" || ev.AgentKind != identity.AgentKindAgent ||
			ev.OwnerUserID != e.admin.ID || ev.OwnerEmail != "ada@beknown.work" || ev.OwnerName != "Ada Lovelace" ||
			ev.ClientKind != "cli" || ev.ClientIP != "198.51.100.4" || ev.Via != "cli" || ev.ResultSummary != "cli:memory.get" {
			t.Fatalf("call.cli row: %+v", ev)
		}
		// The row is no longer misfiled as an enrolment.
		if n := e.countEvents(t, audit.EventAgentEnroll, "cli:memory.get"); n != 0 {
			t.Fatalf("%d agent.enroll rows for a CLI call", n)
		}
		// The gateway's own rows for the call carry the same raiser.
		evs, _ := e.audit.Recent(ctx, 100)
		var gwRows int
		for _, row := range evs {
			if row.AgentID == e.agent.ID && row.ToolName == "memory.get" && row.EventType != "call.cli" {
				gwRows++
				if row.OwnerEmail != "ada@beknown.work" || row.ClientKind != "cli" || row.Via != "cli" {
					t.Fatalf("%s raiser: %+v", row.EventType, row.Raiser)
				}
			}
		}
		if gwRows == 0 {
			t.Fatal("no gateway audit rows for the CLI call")
		}
	})

	t.Run("hooks ingest", func(t *testing.T) {
		rec := e.bearer(t, http.MethodPost, "/v1/hooks/ingest?source=codex", `{"event_name":"Stop","text":"done"}`,
			map[string]string{"x-toolyard-client": "claude_code"})
		if rec.Code != http.StatusOK {
			t.Fatalf("hooks ingest: %d %s", rec.Code, rec.Body.String())
		}
		ev := e.waitEvent(t, "hook.ingest", "", "Stop")
		if ev.AgentID != e.agent.ID || ev.AgentName != "claude-cloud-3" || ev.OwnerEmail != "ada@beknown.work" ||
			ev.OwnerName != "Ada Lovelace" || ev.ClientKind != "claude_code" || ev.Via != "hook" {
			t.Fatalf("hook.ingest row: %+v", ev)
		}
	})
}

// Inbox decisions, batches and grant revocations record the signed-in
// person, and the inbox.decide audit rows carry the decider.
func TestInboxRoutesRecordDecider(t *testing.T) {
	e := newActorEnv(t)
	ctx := context.Background()
	question := func(title string) string {
		t.Helper()
		res, err := e.inbox.Submit(ctx, e.agent.ID, &inbox.Submission{Kind: inbox.KindQuestion, Title: title, Summary: "s", Message: "m",
			Audio: inbox.Audio{Script: title}, Urgency: inbox.UrgencySoon, Options: []inbox.Option{{Label: "Yes"}, {Label: "No"}}})
		if err != nil || !res.OK {
			t.Fatalf("submit %s: %v %+v", title, err, res)
		}
		e.inbox.Flush()
		return res.RequestID
	}
	access := func(title, key string) string {
		t.Helper()
		res, err := e.inbox.Submit(ctx, e.agent.ID, &inbox.Submission{Kind: inbox.KindAccess, Title: title, Summary: "s",
			Message: "May I write the marker?", Audio: inbox.Audio{Script: title}, Urgency: inbox.UrgencySoon,
			Facts: &inbox.Facts{WhyNow: "w", IfItGoesWrong: "g", Undo: "u"},
			Tools: []inbox.SubmissionTool{{Tool: "memory.set", Required: true, Summary: "Write the marker.", Params: map[string]any{"key": key, "value": "v"}}}})
		if err != nil || !res.OK {
			t.Fatalf("submit %s: %v %+v", title, err, res)
		}
		e.inbox.Flush()
		return res.RequestID
	}
	grantsFor := func(status string) []inbox.Grant {
		t.Helper()
		gs, err := e.inbox.ListGrants(ctx, status, e.agent.ID, 50)
		if err != nil {
			t.Fatal(err)
		}
		return gs
	}

	t.Run("decide", func(t *testing.T) {
		id := question("Retire v1?")
		rec := e.do(t, e.cookie, http.MethodPost, "/v1/inbox/"+id+"/decide", `{"action":"deny","note":"not yet"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("decide: %d %s", rec.Code, rec.Body.String())
		}
		r, err := e.inbox.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		want := e.wantDashboardDecider(actor.ViaDashboard)
		if r.Decider() != want || r.DecidedBy != "Ada Lovelace" {
			t.Fatalf("inbox decider = %+v, decided_by %q", r.Decider(), r.DecidedBy)
		}
		checkAuditDecider(t, e.waitEvent(t, "inbox.decide", id, ""), want)
	})

	t.Run("batch", func(t *testing.T) {
		a, b := question("Batch a?"), question("Batch b?")
		rec := e.do(t, e.cookie, http.MethodPost, "/v1/inbox/batch", `{"ids":["`+a+`","`+b+`"],"action":"deny"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("batch: %d %s", rec.Code, rec.Body.String())
		}
		ra, _ := e.inbox.Get(ctx, a)
		rb, _ := e.inbox.Get(ctx, b)
		da, db := ra.Decider(), rb.Decider()
		if da.Via != actor.ViaDashboardBatch || !strings.HasPrefix(da.Ref, "batch_") || da.Ref != db.Ref ||
			da.UserID != e.admin.ID || da.Email != "ada@beknown.work" || db.Name != "Ada Lovelace" {
			t.Fatalf("batch deciders: %+v / %+v", da, db)
		}
		checkAuditDecider(t, e.waitEvent(t, "inbox.decide", a, ""), da)
	})

	t.Run("grant revoke and revoke-all", func(t *testing.T) {
		id := access("Write marker one", "one")
		rec := e.do(t, e.cookie, http.MethodPost, "/v1/inbox/"+id+"/decide", `{"action":"approve","allow":[true]}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("approve: %d %s", rec.Code, rec.Body.String())
		}
		active := grantsFor(inbox.GrantActive)
		if len(active) != 1 || active[0].IssuedBy != e.admin.ID {
			t.Fatalf("active grants after approve: %+v", active)
		}
		rec = e.do(t, e.cookie, http.MethodPost, "/v1/inbox/grants/"+active[0].ID+"/revoke", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("revoke: %d %s", rec.Code, rec.Body.String())
		}
		revoked := grantsFor(inbox.GrantRevoked)
		if len(revoked) != 1 || revoked[0].RevokedBy != e.admin.ID {
			t.Fatalf("revoked grants: %+v", revoked)
		}
		r, _ := e.inbox.Get(ctx, id)
		var named bool
		for _, a := range r.Activity {
			if strings.Contains(a.Text, "Ada Lovelace revoked the permission for memory.set") {
				named = true
			}
		}
		if !named {
			t.Fatalf("activity does not name the revoker: %+v", r.Activity)
		}

		id2 := access("Write marker two", "two")
		if rec := e.do(t, e.cookie, http.MethodPost, "/v1/inbox/"+id2+"/decide", `{"action":"approve","allow":[true]}`); rec.Code != http.StatusOK {
			t.Fatalf("approve 2: %d %s", rec.Code, rec.Body.String())
		}
		rec = e.do(t, e.cookie, http.MethodPost, "/v1/inbox/grants/revoke-all", `{"agent_id":"`+e.agent.ID+`"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("revoke-all: %d %s", rec.Code, rec.Body.String())
		}
		for _, g := range grantsFor(inbox.GrantRevoked) {
			if g.RevokedBy != e.admin.ID {
				t.Fatalf("grant %s revoked_by = %q", g.ID, g.RevokedBy)
			}
		}
		if n := len(grantsFor(inbox.GrantActive)); n != 0 {
			t.Fatalf("%d grants still active after revoke-all", n)
		}
	})
}

// An agent's call is held for approval, the admin approves it in the
// dashboard, the tool runs, and the audit rows name both the raiser
// (agent name, owner email) and the decider (admin email, via dashboard).
func TestAgentCallApprovedInDashboardEndToEnd(t *testing.T) {
	e := newActorEnv(t)
	ctx := actor.WithRaiser(gateway.WithAgentID(context.Background(), e.agent.ID), e.agentRaiser())
	res, err := e.gw.RouteCall(ctx, "direct", "memory.set", map[string]any{"_reason": actorReason, "key": "deploy", "value": "go"})
	if err != nil {
		t.Fatal(err)
	}
	pending, err := e.bus.ListPendingByAgent(context.Background(), e.agent.ID)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending = %d, %v (call result %+v)", len(pending), err, res)
	}
	id := pending[0].ID
	if rb := pending[0].RaisedBy; rb == nil || rb.AgentName != "claude-cloud-3" || rb.OwnerEmail != "ada@beknown.work" || rb.Via != "direct" {
		t.Fatalf("raised_by on the approval: %+v", pending[0].RaisedBy)
	}

	rec := e.do(t, e.cookie, http.MethodPost, "/v1/approvals/"+id+"/decide", `{"action":"allowed"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", rec.Code, rec.Body.String())
	}
	row := e.get(t, id)
	want := e.wantDashboardDecider(actor.ViaDashboard)
	if row.Status != approval.StatusAllowed || row.Decider() != want {
		t.Fatalf("approval row: status %s decider %+v", row.Status, row.Decider())
	}

	create := e.waitEvent(t, audit.EventApprovalCreate, id, "")
	checkAuditRaiser(t, create, e)
	if create.Via != "direct" || create.ToolName != "memory.set" {
		t.Fatalf("create row: %+v", create)
	}
	decide := e.waitEvent(t, audit.EventApprovalDecide, id, "")
	checkAuditRaiser(t, decide, e)
	checkAuditDecider(t, decide, want)
	if decide.Decision != approval.StatusAllowed {
		t.Fatalf("decide row decision = %q", decide.Decision)
	}
	executed := e.waitEvent(t, audit.EventApprovalExecuted, id, "")
	checkAuditRaiser(t, executed, e)
	checkAuditDecider(t, executed, want)
	if executed.ResultSummary != "executed" {
		t.Fatalf("executed row: %+v", executed)
	}
	if final := e.get(t, id); final.ResultExecutedAt == 0 || final.ResultError != "" {
		t.Fatalf("final row: executed_at %d error %q", final.ResultExecutedAt, final.ResultError)
	}
	if en, err := e.mem.Get(context.Background(), "", "deploy"); err != nil || en.Value != "go" {
		t.Fatalf("memory after approval: %+v, %v", en, err)
	}
}

// A REST client may name itself in x-toolyard-client; a value we have
// never seen is recorded as "unknown", not as given.
func TestDeclaredClientKind(t *testing.T) {
	for header, want := range map[string]string{
		"": "", "cli": "cli", "T3": "t3", "Claude_Code": "claude_code", "claude-code/1.2": "claude_code",
		"browser": "browser", "something-else": "unknown", " codex ": "codex",
	} {
		r := httptest.NewRequest(http.MethodPost, "/v1/hooks/ingest", nil)
		if header != "" {
			r.Header.Set("x-toolyard-client", header)
		}
		if got := declaredClientKind(r); got != want {
			t.Errorf("%q: declaredClientKind = %q, want %q", header, got, want)
		}
	}
}
