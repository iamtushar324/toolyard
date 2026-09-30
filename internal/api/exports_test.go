package api

import (
	"context"
	"encoding/csv"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/actor"
	"github.com/tusharbhardwaj/toolyard/internal/approval"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// newExportTestServer builds the smallest Server that can serve the
// export handlers, plus a user to stand in the request context.
func newExportTestServer(t *testing.T) (*Server, *identity.User, *audit.Logger, *approval.Bus) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "api-exports.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	id := identity.New(db)
	user, err := id.CreateUser(ctx, "tester", "long-enough-password")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	bus, err := approval.New(ctx, db)
	if err != nil {
		t.Fatalf("new bus: %v", err)
	}
	lg := audit.New(db)
	srv := New(ctx, Options{Identity: id, Audit: lg, Approval: bus, SessionKey: []byte("0123456789abcdef0123456789abcdef")})
	return srv, user, lg, bus
}

func asUser(r *http.Request, u *identity.User) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), ctxUserKey, u))
}

func TestAuditFilterFromQueryActorParams(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet,
		"/v1/audit/export?owner=u_1&decided_by=u_2&session=ses_3&approval_id=ap_4&client_kind=t3&agent_id=ag_5&since=10", nil)
	got := auditFilterFromQuery(r)
	want := audit.Filter{Since: 10, AgentID: "ag_5", OwnerUserID: "u_1", DecidedByUserID: "u_2",
		AgentSessionID: "ses_3", ApprovalID: "ap_4", ClientKind: "t3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("filter = %+v, want %+v", got, want)
	}
}

func TestAuditExportCSVActorColumns(t *testing.T) {
	srv, user, lg, _ := newExportTestServer(t)
	ctx := context.Background()
	ev := audit.Event{
		EventType: audit.EventApprovalDecide, AgentID: "ag_1", UpstreamName: "github", ToolName: "github.create_issue",
		Decision: "allowed", ApprovalID: "ap_1",
		Raiser: actor.Raiser{AgentName: "claude-cloud-3", OwnerEmail: "owner@example.test", ClientKind: "t3",
			ClientSessionID: "t3-1", AgentSessionID: "ses_1", Via: "direct"},
	}
	ev.SetDecider(actor.Decider{UserID: "u_dec", Email: "dec@example.test", Via: actor.ViaPushToken, Ref: "sub_1"})
	if err := lg.Write(ctx, ev); err != nil {
		t.Fatalf("write: %v", err)
	}

	rec := httptest.NewRecorder()
	srv.auditExport(rec, asUser(httptest.NewRequest(http.MethodGet, "/v1/audit/export", nil), user))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	recs, err := csv.NewReader(strings.NewReader(rec.Body.String())).ReadAll()
	if err != nil {
		t.Fatalf("csv: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("got %d csv records, want header + 1", len(recs))
	}
	col := map[string]string{}
	for i, h := range recs[0] {
		col[h] = recs[1][i]
	}
	for _, h := range []string{"agent_name", "owner_email", "client_kind", "client_session_id", "agent_session_id", "via",
		"decided_by_email", "decided_via", "decider_ref"} {
		if _, ok := col[h]; !ok {
			t.Errorf("csv header lacks %q: %v", h, recs[0])
		}
	}
	want := map[string]string{
		"agent_name": "claude-cloud-3", "owner_email": "owner@example.test", "client_kind": "t3",
		"client_session_id": "t3-1", "agent_session_id": "ses_1", "via": "direct",
		"decided_by_email": "dec@example.test", "decided_via": actor.ViaPushToken, "decider_ref": "sub_1",
		"agent_id": "ag_1", "approval_id": "ap_1", "decision": "allowed",
	}
	for h, v := range want {
		if col[h] != v {
			t.Errorf("csv %s = %q, want %q", h, col[h], v)
		}
	}

	// The unauthenticated request is still refused.
	rec = httptest.NewRecorder()
	srv.auditExport(rec, httptest.NewRequest(http.MethodGet, "/v1/audit/export", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous export status = %d, want 401", rec.Code)
	}
}

func TestApprovalsExportCarriesDecider(t *testing.T) {
	srv, user, _, bus := newExportTestServer(t)
	ctx := context.Background()
	req, err := bus.Hold(ctx, approval.NewRequest{AgentID: "ag_1", UpstreamName: "github", ToolName: "t",
		Arguments: map[string]any{"a": 1}, RaisedBy: actor.Raiser{AgentName: "claude-cloud-3", OwnerEmail: "owner@example.test"}}, 0)
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	if _, err := bus.DecideAs(ctx, req.ID, approval.StatusAllowed,
		actor.Decider{UserID: "u_dec", Email: "dec@example.test", Via: actor.ViaDashboard}); err != nil {
		t.Fatalf("decide: %v", err)
	}

	rec := httptest.NewRecorder()
	srv.approvalsExport(rec, asUser(httptest.NewRequest(http.MethodGet, "/v1/approvals/export", nil), user))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	recs, err := csv.NewReader(strings.NewReader(rec.Body.String())).ReadAll()
	if err != nil || len(recs) != 2 {
		t.Fatalf("csv: err=%v records=%d", err, len(recs))
	}
	col := map[string]string{}
	for i, h := range recs[0] {
		col[h] = recs[1][i]
	}
	for h, v := range map[string]string{"decided_by": "u_dec", "decided_via": actor.ViaDashboard,
		"decider_email": "dec@example.test", "agent_name": "claude-cloud-3", "owner_email": "owner@example.test"} {
		if col[h] != v {
			t.Errorf("approvals csv %s = %q, want %q", h, col[h], v)
		}
	}

	rec = httptest.NewRecorder()
	srv.approvalsExport(rec, asUser(httptest.NewRequest(http.MethodGet, "/v1/approvals/export?format=json", nil), user))
	body := rec.Body.String()
	for _, needle := range []string{`"decided_via":"dashboard"`, `"decider_email":"dec@example.test"`, `"raised_by":{`, `"agent_name":"claude-cloud-3"`} {
		if !strings.Contains(body, needle) {
			t.Errorf("approvals json lacks %s: %s", needle, body)
		}
	}
	if strings.Contains(body, "decision_token") {
		t.Error("approvals json leaks decision_token")
	}
}
