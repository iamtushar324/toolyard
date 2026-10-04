package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/oauth"
	"github.com/tusharbhardwaj/toolyard/internal/upstreams"
)

const assertionServerBody = `{"name":"private_control","transport":"http","url":"http://127.0.0.1:18791/mcp","auth_mode":"per_user","assertion_profile":"isolated_preview","enabled":true}`

func TestAssertionProfileServerAPIAdminCreatesDisabled(t *testing.T) {
	e := newAccessTestServer(t)
	admin := e.cookieFor(t, e.admin.ID)
	got := decodeServer(t, e.do(t, admin, http.MethodPost, "/v1/servers", assertionServerBody))
	if got.Enabled || got.AuthMode != upstreams.AuthPerUser || got.AssertionProfile != "isolated_preview" || got.LastStatus != "disabled" {
		t.Fatalf("connector did not remain disabled: %+v", got)
	}
	if e.gw.UpstreamToolCount(got.Name) != 0 {
		t.Fatal("server creation activated a runtime")
	}
	rec := e.do(t, admin, http.MethodGet, "/v1/servers", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"assertion_profile":"isolated_preview"`) {
		t.Fatalf("profile metadata absent: %d %s", rec.Code, rec.Body.String())
	}
	for _, body := range []string{`{"assertion_profile":"different"}`, `{"url":"http://127.0.0.1:1/mcp"}`, `{"auth_mode":"shared"}`, `{"headers":{"X-Test":"static"}}`, `{"enabled":true}`} {
		rec := e.do(t, admin, http.MethodPatch, "/v1/servers/private_control", body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("unsafe edit %s: %d %s", body, rec.Code, rec.Body.String())
		}
	}
}

func TestAssertionProfileServerAPIMemberAndAgentCannotMutate(t *testing.T) {
	e := newAccessTestServer(t)
	member := e.member(t, "user_assertion_test", "assertion-test@example.test")
	cookie := e.cookieFor(t, member.ID)
	if rec := e.do(t, cookie, http.MethodPost, "/v1/servers", assertionServerBody); rec.Code != http.StatusForbidden {
		t.Fatalf("member created connector: %d %s", rec.Code, rec.Body.String())
	}
	token, _ := e.agentFor(t, member.ID)
	if rec := e.bearer(t, token, http.MethodPost, "/v1/servers", assertionServerBody); rec.Code != http.StatusUnauthorized {
		t.Fatalf("agent created connector: %d %s", rec.Code, rec.Body.String())
	}
	admin := e.cookieFor(t, e.admin.ID)
	decodeServer(t, e.do(t, admin, http.MethodPost, "/v1/servers", assertionServerBody))
	for _, change := range []struct{ method, path, body string }{
		{http.MethodPatch, "/v1/servers/private_control", `{"enabled":true}`},
		{http.MethodPost, "/v1/servers/private_control/reconnect", `{}`},
		{http.MethodDelete, "/v1/servers/private_control", ""},
	} {
		if rec := e.do(t, cookie, change.method, change.path, change.body); rec.Code != http.StatusForbidden {
			t.Fatalf("member changed connector: %d %s", rec.Code, rec.Body.String())
		}
		if rec := e.bearer(t, token, change.method, change.path, change.body); rec.Code != http.StatusUnauthorized {
			t.Fatalf("agent changed connector: %d %s", rec.Code, rec.Body.String())
		}
	}
}

func TestAssertionProfileAPIDeniesOAuthAndPATWithoutNetwork(t *testing.T) {
	e := newAccessTestServer(t)
	admin := e.cookieFor(t, e.admin.ID)
	decodeServer(t, e.do(t, admin, http.MethodPost, "/v1/servers", assertionServerBody))
	// An absent OAuth service proves these requests stop at the assertion
	// profile guard, before discovery, token state or a remote connection.
	for _, route := range []string{"discover", "manual-client", "begin", "device-begin", "device-poll", "reauth", "pat"} {
		rec := e.do(t, admin, http.MethodPost, "/v1/servers/private_control/oauth/"+route, `{}`)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "do not accept OAuth or static tokens") {
			t.Fatalf("credential override %s: %d %s", route, rec.Code, rec.Body.String())
		}
	}
	got, err := e.srv.upstreams.Get(context.Background(), "private_control")
	if err != nil || got.Enabled || got.LastStatus != "disabled" {
		t.Fatalf("credential request changed profile: %v %+v", err, got)
	}
}

func TestAssertionProfileAbsentFromPersonalOAuthConnections(t *testing.T) {
	e := newAccessTestServer(t)
	withOAuth(t, e)
	admin := e.cookieFor(t, e.admin.ID)
	decodeServer(t, e.do(t, admin, http.MethodPost, "/v1/servers", assertionServerBody))
	connections := decodeConnections(t, e.do(t, admin, http.MethodGet, "/v1/me/connections", ""))
	if len(connections) != 0 {
		t.Fatalf("assertion profile offered OAuth sign-in: %+v", connections)
	}
	for _, path := range []string{"/v1/me/connections/private_control/begin", "/v1/servers/private_control/connections"} {
		method := http.MethodPost
		if strings.HasSuffix(path, "/connections") {
			method = http.MethodGet
		}
		rec := e.do(t, admin, method, path, `{}`)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "do not accept OAuth") {
			t.Fatalf("profile sign-in admitted: %d %s", rec.Code, rec.Body.String())
		}
	}
}

func TestAssertionProfileRejectsStaleOAuthCallbackAndPaste(t *testing.T) {
	for _, mode := range []string{oauth.ModeCallback, oauth.ModePaste} {
		t.Run(mode, func(t *testing.T) {
			e := newAccessTestServer(t)
			o := withOAuth(t, e)
			admin := e.cookieFor(t, e.admin.ID)
			decodeServer(t, e.do(t, admin, http.MethodPost, "/v1/servers", assertionServerBody))
			// Synthetic stale OAuth state represents a flow from an older
			// connector with the same name. It must not exchange a token.
			o.client(t, "private_control")
			_, state, err := o.oa.BeginCallback(context.Background(), "private_control", e.admin.ID, mode, nil, false)
			if err != nil {
				t.Fatal(err)
			}
			if mode == oauth.ModeCallback {
				rec := e.callback(t, admin, state, "synthetic")
				if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "do not accept OAuth") {
					t.Fatalf("callback admitted: %d %s", rec.Code, rec.Body.String())
				}
			} else {
				body, err := json.Marshal(map[string]string{"url": "https://callback.invalid/?code=synthetic&state=" + state})
				if err != nil {
					t.Fatal(err)
				}
				rec := e.do(t, admin, http.MethodPost, "/v1/mcp-oauth/paste", string(body))
				if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "do not accept OAuth") {
					t.Fatalf("paste admitted: %d %s", rec.Code, rec.Body.String())
				}
			}
			if token, err := o.oa.GetToken(context.Background(), "private_control"); err != nil || token != nil {
				t.Fatalf("stale flow stored a token: %v %+v", err, token)
			}
		})
	}
}
