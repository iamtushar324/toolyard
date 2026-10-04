package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/actor"
	"github.com/tusharbhardwaj/toolyard/internal/approval"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
)

func TestReviewLegacyHistoryKeepsOwnerAfterAgentDeletion(t *testing.T) {
	f := newInboxAPIFixture(t)
	ctx := context.Background()
	owner, err := f.srv.identity.PrimaryUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	member, err := f.srv.identity.UpsertClerkUser(ctx, identity.ClerkProfile{ClerkUserID: "legacy-review-member", Email: "legacy-member@example.test"}, "")
	if err != nil {
		t.Fatal(err)
	}
	_, ag, err := f.srv.identity.CreateAgentWithToken(ctx, member.ID, "Legacy member agent")
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	if err = f.srv.issueSession(rec, httptest.NewRequest("GET", "/", nil), member.ID); err != nil {
		t.Fatal(err)
	}
	var memberCookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName {
			memberCookie = c
		}
	}
	if memberCookie == nil {
		t.Fatal("member cookie missing")
	}
	hold := func(agent, name string) *approval.Request {
		r, e := f.srv.approval.Hold(ctx, approval.NewRequest{AgentID: agent, UpstreamName: "fixture", ToolName: "fixture.write", Arguments: map[string]any{"path": name + ".txt"}, Reason: "Original legacy explanation", RequireHuman: true}, 0)
		if e != nil {
			t.Fatal(e)
		}
		return r
	}
	primary := hold(f.agent, "original-owner")
	other := hold(ag.ID, "original-member")
	recovered := hold("ag_deleted_before_import", "audit-owner")
	conflict := hold("ag_deleted_before_import", "audit-conflict")
	unknown := hold("ag_deleted_without_evidence", "missing-owner")
	anonymous := hold("", "anonymous-owner")
	for _, entry := range []struct{ id, owner string }{{recovered.ID, owner.ID}, {conflict.ID, owner.ID}, {conflict.ID, member.ID}} {
		if err = f.srv.audit.Write(ctx, audit.Event{EventType: "call.start", ApprovalID: entry.id, Raiser: actor.Raiser{OwnerUserID: entry.owner}}); err != nil {
			t.Fatal(err)
		}
	}
	if err = f.svc.SyncLegacy(ctx); err != nil {
		t.Fatal(err)
	}
	if err = f.srv.identity.DeleteAgent(ctx, owner.ID, f.agent); err != nil {
		t.Fatal(err)
	}
	if err = f.srv.identity.DeleteAgent(ctx, member.ID, ag.ID); err != nil {
		t.Fatal(err)
	}
	if err = f.svc.SyncLegacy(ctx); err != nil {
		t.Fatal(err)
	}
	handler := f.srv.RoleGuard(f.mux)
	get := func(cookie *http.Cookie, id string) int {
		r := httptest.NewRequest("GET", "/v1/inbox/"+id, nil)
		r.AddCookie(cookie)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Code
	}
	for _, id := range []string{primary.ID, recovered.ID, anonymous.ID} {
		if code := get(f.cookie, id); code != 200 {
			t.Fatalf("original owner lost historical row %s: %d", id, code)
		}
		if code := get(memberCookie, id); code != 404 {
			t.Fatalf("other member acquired historical row %s: %d", id, code)
		}
	}
	if code := get(memberCookie, other.ID); code != 200 {
		t.Fatalf("member lost own removed agent history: %d", code)
	}
	if code := get(f.cookie, other.ID); code != 404 {
		t.Fatalf("primary acquired member history: %d", code)
	}
	for _, id := range []string{conflict.ID, unknown.ID} {
		for _, cookie := range []*http.Cookie{f.cookie, memberCookie} {
			if code := get(cookie, id); code != 404 {
				t.Fatalf("unknown owner was guessed for %s: %d", id, code)
			}
		}
	}
	if _, err = f.svc.DecideLegacy(ctx, primary.ID, "allowed", actor.Decider{UserID: member.ID}); err == nil {
		t.Fatal("other member decided original owner's legacy request")
	}
	if _, err = f.svc.DecideLegacy(ctx, primary.ID, "allowed", actor.Decider{UserID: owner.ID}); err != nil {
		t.Fatalf("original snapshot owner cannot decide after removal: %v", err)
	}
	if err = f.svc.SyncLegacy(ctx); err != nil {
		t.Fatal(err)
	}
	if code := get(f.cookie, primary.ID); code != 200 {
		t.Fatalf("decision removed preserved history: %d", code)
	}
	grants, err := f.svc.ListGrants(ctx, "", f.agent, 10)
	if err != nil || len(grants) != 0 {
		t.Fatalf("deleted legacy agent acquired grants: %+v %v", grants, err)
	}
}
