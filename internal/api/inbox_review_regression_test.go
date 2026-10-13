package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/actor"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/inbox"
)

func TestReviewMemberInboxRealGuardAndRevokeAllOwnership(t *testing.T) {
	f := newInboxAPIFixture(t)
	ctx := context.Background()
	owner, err := f.srv.identity.PrimaryUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	member, err := f.srv.identity.UpsertClerkUser(ctx, identity.ClerkProfile{ClerkUserID: "review-member", Email: "member@example.test"}, "")
	if err != nil {
		t.Fatal(err)
	}
	_, ag, err := f.srv.identity.CreateAgentWithToken(ctx, member.ID, "review-member-agent")
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	if err = f.srv.issueSession(rec, httptest.NewRequest("GET", "/", nil), member.ID); err != nil {
		t.Fatal(err)
	}
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("no member cookie")
	}
	handler := f.srv.RoleGuard(f.mux)
	call := func(method, path string, body any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.AddCookie(cookie)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", "https://example.com")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	create := func(aid, uid string) *inbox.Request {
		sub, err := inbox.DecodeSubmission(accessRequest("memory.delete"))
		if err != nil {
			t.Fatal(err)
		}
		sub.Kind = inbox.KindAccess
		out, err := f.svc.Submit(ctx, aid, sub)
		if err != nil || !out.OK {
			t.Fatalf("submit %+v %v", out, err)
		}
		f.svc.Flush()
		r, err := f.svc.Get(ctx, out.RequestID)
		if err != nil {
			t.Fatal(err)
		}
		r, err = f.svc.Decide(ctx, r.ID, inbox.Decision{Action: "submit", RequestRevision: r.Revision, SubmissionID: "review:" + r.ID, Decider: actor.Decider{UserID: uid}, Verdicts: map[string]inbox.CallVerdict{r.Tools[0].CallID: {Verdict: inbox.VerdictAccepted}}})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	own := create(ag.ID, member.ID)
	other := create(f.agent, owner.ID)
	if w := call("GET", "/v1/inbox/"+own.ID, nil); w.Code != 200 {
		t.Fatalf("member own item: %d %s", w.Code, w.Body.String())
	}
	if w := call("GET", "/v1/inbox/"+other.ID, nil); w.Code != 404 {
		t.Fatalf("member other item: %d", w.Code)
	}
	if w := call("POST", "/v1/inbox/grants/revoke-all", map[string]string{"agent_id": f.agent}); w.Code != 404 {
		t.Fatalf("cross-owner revoke: %d %s", w.Code, w.Body.String())
	}
	if w := call("POST", "/v1/inbox/grants/revoke-all", map[string]string{}); w.Code != 200 {
		t.Fatalf("own revoke-all: %d %s", w.Code, w.Body.String())
	}
	ownGrant, _ := f.svc.Grant(ctx, own.Tools[0].GrantID)
	otherGrant, _ := f.svc.Grant(ctx, other.Tools[0].GrantID)
	if ownGrant.Status != inbox.GrantRevoked || otherGrant.Status != inbox.GrantActive {
		t.Fatalf("grant ownership: own=%s other=%s", ownGrant.Status, otherGrant.Status)
	}
	for _, path := range []string{"/v1/events/stream", "/v1/settings", "/v1/inbox/voice-key"} {
		if w := call("GET", path, nil); w.Code != 403 {
			t.Fatalf("member administrative route %s: %d", path, w.Code)
		}
	}
	if w := call("POST", "/v1/passkeys/register/begin", map[string]string{}); w.Code != 200 {
		t.Fatalf("own registration: %d %s", w.Code, w.Body.String())
	}
	if !memberAllowed("POST", "/v1/passkeys/credential/remove/begin") {
		t.Fatal("member cannot begin own passkey removal")
	}
}
