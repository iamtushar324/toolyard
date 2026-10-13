package api

import (
	"context"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/inbox"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestInboxBatchOwnerIsolation(t *testing.T) {
	f := newInboxAPIFixture(t)
	ctx := context.Background()
	second, err := f.srv.identity.UpsertClerkUser(ctx, identity.ClerkProfile{ClerkUserID: "second_member", Email: "second@beknown.work", DisplayName: "Second Member"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = f.srv.identity.SetRole(ctx, second.ID, identity.RoleMember); err != nil {
		t.Fatal(err)
	}
	_, agent, err := f.srv.identity.CreateAgentWithToken(ctx, second.ID, "member-agent")
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	record := httptest.NewRecorder()
	if err := f.srv.issueSession(record, request, second.ID); err != nil {
		t.Fatal(err)
	}
	other := *f
	for _, cookie := range record.Result().Cookies() {
		if cookie.Name == sessionCookieName {
			other.cookie = cookie
		}
	}
	create := func(agentID string) *inbox.Request {
		sub, err := inbox.DecodeSubmission(accessRequest("memory.delete"))
		if err != nil {
			t.Fatal(err)
		}
		sub.Kind = inbox.KindAccess
		res, err := f.svc.Submit(ctx, agentID, sub)
		if err != nil || !res.OK {
			t.Fatalf("submit: %+v %v", res, err)
		}
		f.svc.Flush()
		r, err := f.svc.Get(ctx, res.RequestID)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	ownerRequest, memberRequest := create(f.agent), create(agent.ID)
	if code, _ := other.owner(t, http.MethodGet, "/v1/inbox/"+ownerRequest.ID, nil); code != 404 {
		t.Fatalf("member saw owner item: %d", code)
	}
	if code, _ := f.owner(t, http.MethodGet, "/v1/inbox/"+memberRequest.ID, nil); code != 404 {
		t.Fatalf("owner saw member item: %d", code)
	}
	for _, client := range []*inboxAPIFixture{f, &other} {
		code, body := client.owner(t, http.MethodGet, "/v1/inbox", nil)
		if code != 200 || len(body["requests"].([]any)) != 1 {
			t.Fatalf("isolated list: %d %+v", code, body)
		}
	}
	decision := map[string]any{"action": "submit", "request_revision": ownerRequest.Revision, "submission_id": "member_spoof", "verdicts": map[string]any{"delete_preview": map[string]any{"verdict": "accepted"}}}
	if code, _ := other.owner(t, http.MethodPost, "/v1/inbox/"+ownerRequest.ID+"/decide", decision); code != 404 {
		t.Fatalf("wrong owner decision: %d", code)
	}
	decision["submission_id"] = "owner_decision"
	if code, body := f.owner(t, http.MethodPost, "/v1/inbox/"+ownerRequest.ID+"/decide", decision); code != 200 {
		t.Fatalf("owner decision: %d %+v", code, body)
	}
	grants, _ := f.svc.ListGrants(ctx, "", f.agent, 10)
	if len(grants) != 1 {
		t.Fatal("expected a grant")
	}
	if code, _ := other.owner(t, http.MethodPost, "/v1/inbox/grants/"+grants[0].ID+"/revoke", nil); code != 404 {
		t.Fatalf("wrong owner revocation: %d", code)
	}
	if code, body := other.owner(t, http.MethodGet, "/v1/inbox/grants", nil); code != 200 || len(body["grants"].([]any)) != 0 {
		t.Fatalf("member saw owner grants: %d %+v", code, body)
	}
}
