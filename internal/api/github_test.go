package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/actor"
	"github.com/tusharbhardwaj/toolyard/internal/approval"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/realtime"
	"github.com/tusharbhardwaj/toolyard/internal/upstreams"
)

func TestGitHubApprovalAPIsAreOwnerOnly(t *testing.T) {
	e := newAccessTestServer(t)
	bus, err := approval.New(context.Background(), e.db)
	if err != nil {
		t.Fatal(err)
	}
	e.srv.approval = bus
	alice := e.member(t, "alice", "alice@beknown.work")
	bob := e.member(t, "bob", "bob@beknown.work")
	r, err := bus.Hold(context.Background(), approval.NewRequest{AgentID: "agent-alice", UpstreamName: "github", ToolName: "github.create_pull_request_comment", Reason: "private proposed comment", RequireHuman: true, Arguments: map[string]any{approval.PersonalGitHubField: map[string]any{"owner_user_id": alice.ID}, "body": "private feedback"}, RaisedBy: actor.Raiser{OwnerUserID: alice.ID}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range []*identity.User{bob, e.admin} {
		cookie := e.cookieFor(t, u.ID)
		out := e.do(t, cookie, "GET", "/v1/approvals?status=pending", "")
		if out.Code != 200 || strings.Contains(out.Body.String(), r.ID) {
			t.Fatalf("list leaked to %s: %d %s", u.ID, out.Code, out.Body.String())
		}
		for _, path := range []string{"/v1/approvals/" + r.ID, "/v1/approvals/" + r.ID + "/decide"} {
			method, body := "GET", ""
			if strings.HasSuffix(path, "/decide") {
				method = "POST"
				body = `{"action":"allowed"}`
			}
			out := e.do(t, cookie, method, path, body)
			if out.Code != 404 {
				t.Fatalf("foreign request exposed: %d %s", out.Code, out.Body.String())
			}
		}
		out = e.do(t, cookie, "POST", "/v1/approvals/decide-batch", `{"ids":["`+r.ID+`"],"action":"allowed"}`)
		if strings.Contains(out.Body.String(), `"status":"allowed"`) {
			t.Fatal("foreign batch approved")
		}
	}
	if out := e.do(t, e.cookieFor(t, alice.ID), "GET", "/v1/approvals?status=pending", ""); !strings.Contains(out.Body.String(), r.ID) {
		t.Fatalf("owner cannot inspect: %s", out.Body.String())
	}
	if out := e.do(t, e.cookieFor(t, alice.ID), "POST", "/v1/approvals/"+r.ID+"/decide", `{"action":"denied"}`); out.Code != 200 {
		t.Fatalf("owner decision: %d %s", out.Code, out.Body.String())
	}
	for _, u := range []*identity.User{alice, bob, e.admin} {
		if got := personalEventVisible(realtime.Event{Type: "approval", Data: r}, u); got != (u.ID == alice.ID) {
			t.Fatalf("SSE leak to %s", u.ID)
		}
	}
	row := audit.Event{ToolName: r.ToolName, Raiser: actor.Raiser{OwnerUserID: alice.ID}, ResultSummary: "private feedback"}
	if len(visibleAudit([]audit.Event{row}, bob.ID)) != 0 {
		t.Fatal("audit leaked")
	}
}

type githubSetupTransport func(*http.Request) (*http.Response, error)

func (f githubSetupTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestGitHubAppSetupBindsBrowserAndStoresClient(t *testing.T) {
	e := newAccessTestServer(t)
	o := withOAuth(t, e)
	if _, err := e.srv.upstreams.Add(context.Background(), upstreams.Server{Name: "GitHubForUsers", Transport: "github", URL: "https://api.github.com", AuthMode: upstreams.AuthPerUser}); err != nil {
		t.Fatal(err)
	}
	posts := 0
	e.srv.githubHTTP = &http.Client{Transport: githubSetupTransport(func(r *http.Request) (*http.Response, error) {
		posts++
		if r.URL.String() != "https://api.github.com/app-manifests/code/conversions" || r.Method != "POST" {
			t.Fatalf("wrong conversion: %s", r.URL)
		}
		return &http.Response{StatusCode: 201, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"client_id":"Iv1.test","client_secret":"secret-value","slug":"toolyard-test","pem":"unused-private-key"}`))}, nil
	})}
	out := e.do(t, e.cookieFor(t, e.admin.ID), "POST", "/v1/github/setup/begin", `{"server":"GitHubForUsers"}`)
	if out.Code != 200 {
		t.Fatalf("begin: %d %s", out.Code, out.Body.String())
	}
	var begin struct {
		ActionURL string         `json:"action_url"`
		Manifest  map[string]any `json:"manifest"`
	}
	if json.Unmarshal(out.Body.Bytes(), &begin) != nil {
		t.Fatal("invalid setup response")
	}
	permissions := begin.Manifest["default_permissions"].(map[string]any)
	if len(permissions) != 3 || permissions["pull_requests"] != "write" || permissions["contents"] != "read" {
		t.Fatalf("excessive permissions: %v", permissions)
	}
	action, _ := url.Parse(begin.ActionURL)
	state := action.Query().Get("state")
	callback := "/v1/mcp-oauth/github-app?code=code&state=" + url.QueryEscape(state)
	// A state without the browser cookie cannot exchange credentials.
	bad := httptest.NewRecorder()
	e.handler.ServeHTTP(bad, httptest.NewRequest("GET", callback, nil))
	if posts != 0 {
		t.Fatal("unbound browser converted App")
	}
	request := httptest.NewRequest("GET", callback, nil)
	for _, cookie := range out.Result().Cookies() {
		request.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, request)
	if posts != 1 || !strings.Contains(rec.Body.String(), "Install GitHub App") || strings.Contains(rec.Body.String(), "secret-value") {
		t.Fatalf("callback: %d %s", rec.Code, rec.Body.String())
	}
	client, err := o.oa.GetClient(context.Background(), "GitHubForUsers")
	if err != nil || client.ClientID != "Iv1.test" || client.ClientSecret != "secret-value" || client.ExtraAuthorizeParams == nil {
		t.Fatalf("client not saved: %v", err)
	}
	connections := e.do(t, e.cookieFor(t, e.admin.ID), "GET", "/v1/me/connections", "")
	if !strings.Contains(connections.Body.String(), `"transport":"github"`) || !strings.Contains(connections.Body.String(), `"ready":true`) {
		t.Fatalf("connection not ready: %s", connections.Body.String())
	}
}

func TestGitHubOperatorCannotActAsHumanDecider(t *testing.T) {
	e := newAccessTestServer(t)
	bus, err := approval.New(context.Background(), e.db)
	if err != nil {
		t.Fatal(err)
	}
	e.srv.approval = bus
	r, err := bus.Hold(context.Background(), approval.NewRequest{AgentID: "a", ToolName: "github.create_pull_request_comment", RequireHuman: true, Arguments: map[string]any{approval.PersonalGitHubField: map[string]any{"owner_user_id": e.admin.ID}}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := e.id.CreateOperatorToken(context.Background(), e.admin.ID, "automation", []string{"read", "write", "owner"}, 0, "test")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/v1/approvals/" + r.ID + "/decide", "/v1/approvals/decide-by-token"} {
		body := `{"action":"allowed"}`
		if strings.HasSuffix(path, "decide-by-token") {
			body = `{"action":"allowed","token":"` + bus.DecisionTokenFor(r.ID, e.admin.ID) + `"}`
		}
		request := httptest.NewRequest("POST", path, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		e.handler.ServeHTTP(rec, request)
		if rec.Code != 403 {
			t.Fatalf("operator decided request: %d %s", rec.Code, rec.Body.String())
		}
	}
}
