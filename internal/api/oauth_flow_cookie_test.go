package api

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/oauth"
)

// The OAuth flow cookie: in production the session cookie is
// SameSite=Strict, so the provider's cross-site redirect back to the
// callback carries no session. The begin response sets a Lax cookie bound
// to (state, user) and the callback accepts the flow from a browser that
// presents it. These tests send the callback the way that browser would:
// no session cookie, only what the redirect carries.

// flowCookieFrom returns the flow cookie a begin response set.
func flowCookieFrom(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if strings.HasPrefix(c.Name, oauthFlowCookiePrefix) {
			return c
		}
	}
	t.Fatalf("begin response set no flow cookie: %v", rec.Header().Values("Set-Cookie"))
	return nil
}

// callbackCookies hits the callback with exactly the given cookies.
func (e *accessTestEnv) callbackCookies(t *testing.T, state, code string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/mcp-oauth/callback?state="+url.QueryEscape(state)+"&code="+url.QueryEscape(code), nil)
	for _, c := range cookies {
		if c != nil {
			req.AddCookie(c)
		}
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

// beginFor starts ada's per-user flow and returns the state and the flow
// cookie the response set.
func beginFor(t *testing.T, e *accessTestEnv, cookie *http.Cookie, server string) (string, *http.Cookie) {
	t.Helper()
	rec := e.do(t, cookie, http.MethodPost, "/v1/me/connections/"+server+"/begin", "{}")
	if rec.Code != http.StatusOK {
		t.Fatalf("begin: %d %s", rec.Code, rec.Body.String())
	}
	var begun struct {
		State string `json:"state"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &begun)
	return begun.State, flowCookieFrom(t, rec)
}

// TestOAuthFlowCookieAttributes: the cookie the callback relies on is
// scoped to the OAuth return routes, HttpOnly, SameSite=Lax (so the
// cross-site top-level redirect carries it), lives as long as the pending
// flow, and is Secure exactly when the request came over HTTPS.
func TestOAuthFlowCookieAttributes(t *testing.T) {
	e := newAccessTestServer(t)
	oe := withOAuth(t, e)
	ts := newPatchMCPServer(t)
	addPerUserServer(t, e, "linear", ts)
	oe.client(t, "linear")
	ada := e.member(t, "user_ada", "ada@beknown.work")
	if err := e.access.SetGroups(t.Context(), ada.ID, []string{"linear"}, e.admin.ID); err != nil {
		t.Fatal(err)
	}
	state, c := beginFor(t, e, e.cookieFor(t, ada.ID), "linear")
	if c.Name != oauthFlowCookieName(state) || c.Path != oauthFlowCookiePath || !c.HttpOnly ||
		c.SameSite != http.SameSiteLaxMode || c.MaxAge != int(oauth.PendingTTL.Seconds()) || c.Secure {
		t.Fatalf("flow cookie over http = %+v", c)
	}
	if c.Value != e.srv.oauthFlowMAC(state, ada.ID) || strings.Contains(c.Value, ada.ID) || strings.Contains(c.Value, state) {
		t.Fatalf("flow cookie value is not the bare MAC: %q", c.Value)
	}
	// Over TLS the cookie is Secure; the clear is too.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/me/connections/linear/begin", nil)
	req.TLS = &tls.ConnectionState{}
	e.srv.setOAuthFlowCookie(rec, req, state, ada.ID)
	if got := rec.Result().Cookies(); len(got) != 1 || !got[0].Secure || got[0].SameSite != http.SameSiteLaxMode {
		t.Fatalf("flow cookie over https = %+v", got)
	}
	rec = httptest.NewRecorder()
	e.srv.clearOAuthFlowCookie(rec, req, state)
	if got := rec.Result().Cookies(); len(got) != 1 || got[0].MaxAge != -1 || got[0].Path != oauthFlowCookiePath || !got[0].Secure {
		t.Fatalf("flow cookie clear = %+v", got)
	}
	// Two flows in one browser get two cookies.
	state2, c2 := beginFor(t, e, e.cookieFor(t, ada.ID), "linear")
	if c2.Name == c.Name || state2 == state {
		t.Fatalf("second flow reused the first cookie: %s", c2.Name)
	}
}

// TestPerUserCallbackWithoutSessionCookie: the production path. The
// callback arrives with the flow cookie only (no session) and succeeds;
// with somebody else's flow cookie, the cookie of another flow, or no
// cookie at all it is refused, nothing is stored, and the flow is burnt.
func TestPerUserCallbackWithoutSessionCookie(t *testing.T) {
	e := newAccessTestServer(t)
	oe := withOAuth(t, e)
	ts := newPatchMCPServer(t)
	addPerUserServer(t, e, "linear", ts)
	oe.client(t, "linear")
	ada := e.member(t, "user_ada", "ada@beknown.work")
	bob := e.member(t, "user_bob", "bob@beknown.work")
	for _, u := range []string{ada.ID, bob.ID} {
		if err := e.access.SetGroups(t.Context(), u, []string{"linear"}, e.admin.ID); err != nil {
			t.Fatal(err)
		}
	}
	adaCookie, bobCookie := e.cookieFor(t, ada.ID), e.cookieFor(t, bob.ID)

	// Bob's own flow cookie on ada's state: refused, burnt.
	adaState, adaFlow := beginFor(t, e, adaCookie, "linear")
	_, bobFlow := beginFor(t, e, bobCookie, "linear")
	// Bob's cookie is for his state, so its name differs; present it under
	// ada's cookie name as a forged cookie would be.
	forged := &http.Cookie{Name: oauthFlowCookieName(adaState), Value: bobFlow.Value}
	rec := e.callbackCookies(t, adaState, "ada", forged)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "did not come back to the browser") {
		t.Fatalf("another user's flow cookie: %d %s", rec.Code, rec.Body.String())
	}
	if rows := userTokenRows(t, e, "linear"); len(rows) != 0 {
		t.Fatalf("rows after forged cookie = %v", rows)
	}
	if rec := e.callbackCookies(t, adaState, "ada", adaFlow); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "expired or was already used") {
		t.Fatalf("ada on the burnt flow: %d %s", rec.Code, rec.Body.String())
	}

	// The cookie of one of ada's own flows does not finish another of hers.
	s1, f1 := beginFor(t, e, adaCookie, "linear")
	s2, _ := beginFor(t, e, adaCookie, "linear")
	rec = e.callbackCookies(t, s2, "ada", &http.Cookie{Name: oauthFlowCookieName(s2), Value: f1.Value})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("cookie of another flow: %d", rec.Code)
	}
	// No cookie at all: refused and burnt.
	if rec := e.callbackCookies(t, s1, "ada"); rec.Code != http.StatusBadRequest {
		t.Fatalf("no cookie: %d", rec.Code)
	}
	if rec := e.callbackCookies(t, s1, "ada", f1); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "expired or was already used") {
		t.Fatalf("flow not burnt after a cookieless attempt: %d %s", rec.Code, rec.Body.String())
	}
	if rows := userTokenRows(t, e, "linear"); len(rows) != 0 {
		t.Fatalf("rows after refused attempts = %v", rows)
	}
	if e.auditRows(t, "oauth.user_mismatch", "") != 3 {
		t.Fatalf("mismatch audit rows = %d, want 3", e.auditRows(t, "oauth.user_mismatch", ""))
	}

	// The real thing: flow cookie, no session.
	state, flow := beginFor(t, e, adaCookie, "linear")
	rec = e.callbackCookies(t, state, "ada", flow)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Connected") {
		t.Fatalf("flow cookie only: %d %s", rec.Code, rec.Body.String())
	}
	if rows := userTokenRows(t, e, "linear"); len(rows) != 1 || rows[ada.ID] != oauth.StateActive {
		t.Fatalf("rows after the flow-cookie callback = %v", rows)
	}
	// The response clears the flow cookie.
	cleared := false
	for _, c := range rec.Result().Cookies() {
		if c.Name == flow.Name && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatalf("flow cookie not cleared: %v", rec.Header().Values("Set-Cookie"))
	}
	// Used once: the same cookie and state again is gone.
	if rec := e.callbackCookies(t, state, "ada", flow); rec.Code != http.StatusBadRequest {
		t.Fatalf("replay: %d", rec.Code)
	}
	// A session for another user plus ada's valid flow cookie: the
	// session wins the identity check and it is not ada's, so refused.
	state, flow = beginFor(t, e, adaCookie, "linear")
	if rec := e.callbackCookies(t, state, "ada2", flow, bobCookie); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "different toolyard user") {
		t.Fatalf("bob's session with ada's flow cookie: %d %s", rec.Code, rec.Body.String())
	}
	if rows := userTokenRows(t, e, "linear"); len(rows) != 1 {
		t.Fatalf("rows = %v", rows)
	}
	waitStatus(t, e, "linear", "ok")
}

// TestSharedFlowBoundToAdminBrowser: an admin's shared-account flow
// started from a browser is bound the same way, so in production (no
// session on the return) a member's browser cannot finish it, the admin's
// can with the flow cookie alone, and a flow started without a browser
// (operator token) stays finishable by whoever opens the authorize URL,
// as before.
func TestSharedFlowBoundToAdminBrowser(t *testing.T) {
	e := newAccessTestServer(t)
	oe := withOAuth(t, e)
	e.seedServer(t, "shared", "ok", true)
	oe.client(t, "shared")
	adminCookie := e.cookieFor(t, e.admin.ID)
	sharedTokens := func() int {
		var n int
		_ = e.db.QueryRow(`SELECT count(*) FROM oauth_tokens WHERE upstream_name = 'shared'`).Scan(&n)
		return n
	}
	begin := func(cookie *http.Cookie) (string, *http.Cookie) {
		t.Helper()
		rec := e.do(t, cookie, http.MethodPost, "/v1/servers/shared/oauth/begin", `{"mode":"callback"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("begin: %d %s", rec.Code, rec.Body.String())
		}
		var begun struct {
			State string `json:"state"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &begun)
		return begun.State, flowCookieFrom(t, rec)
	}

	// No cookie at all on a browser-started flow: refused and burnt.
	state, flow := begin(adminCookie)
	if rec := e.callbackCookies(t, state, "bot"); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "did not come back to the browser") {
		t.Fatalf("cookieless shared callback: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.callbackCookies(t, state, "bot", flow); rec.Code != http.StatusBadRequest {
		t.Fatalf("burnt shared flow: %d", rec.Code)
	}
	if sharedTokens() != 0 {
		t.Fatal("shared token stored by a cookieless callback")
	}
	// The admin's browser with the flow cookie and no session: finishes.
	state, flow = begin(adminCookie)
	rec := e.callbackCookies(t, state, "bot", flow)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Authorized") {
		t.Fatalf("admin flow cookie only: %d %s", rec.Code, rec.Body.String())
	}
	if sharedTokens() != 1 {
		t.Fatal("shared token not stored")
	}
	// A flow whose admin was demoted meanwhile cannot be finished with the cookie.
	state, flow = begin(adminCookie)
	other := e.member(t, "user_x", "x@beknown.work")
	if err := e.id.SetRole(context.Background(), other.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	if err := e.id.SetRole(context.Background(), e.admin.ID, "member"); err != nil {
		t.Fatal(err)
	}
	if rec := e.callbackCookies(t, state, "bot2", flow); rec.Code != http.StatusBadRequest {
		t.Fatalf("demoted admin's flow cookie: %d %s", rec.Code, rec.Body.String())
	}
	if err := e.id.SetRole(context.Background(), e.admin.ID, "admin"); err != nil {
		t.Fatal(err)
	}

	// Started with an operator token (no browser): unbound, finishable
	// anonymously, as before.
	tok, _, err := e.id.CreateOperatorToken(context.Background(), e.admin.ID, "cli", []string{"read", "write"}, 0, "test")
	if err != nil {
		t.Fatal(err)
	}
	rec = e.bearer(t, tok, http.MethodPost, "/v1/servers/shared/oauth/begin", `{"mode":"callback"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("operator begin: %d %s", rec.Code, rec.Body.String())
	}
	for _, c := range rec.Result().Cookies() {
		if strings.HasPrefix(c.Name, oauthFlowCookiePrefix) {
			t.Fatalf("operator begin set a flow cookie: %v", c)
		}
	}
	var begun struct {
		State string `json:"state"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &begun)
	if rec := e.callbackCookies(t, begun.State, "bot3"); rec.Code != http.StatusOK {
		t.Fatalf("anonymous callback on an operator-started flow: %d %s", rec.Code, rec.Body.String())
	}

	// An operator token cannot start a personal sign-in: nothing to bind.
	addPerUserServer(t, e, "linear", newPatchMCPServer(t))
	oe.client(t, "linear")
	if rec := e.bearer(t, tok, http.MethodPost, "/v1/me/connections/linear/begin", `{}`); rec.Code != http.StatusConflict ||
		!strings.Contains(rec.Body.String(), "My connections") {
		t.Fatalf("operator per-user begin: %d %s", rec.Code, rec.Body.String())
	}
}

// TestConnectionsListShowsHostOnly: members see the server's host, never
// its path or query (a URL can embed a key).
func TestConnectionsListShowsHostOnly(t *testing.T) {
	e := newAccessTestServer(t)
	withOAuth(t, e)
	now := int64(1)
	if _, err := e.db.Exec(`INSERT INTO upstream_servers(name, transport, url, enabled, auth_mode, last_status, created_at, updated_at)
		VALUES('keyed','http','https://mcp.example.com/t/supersecretpath/mcp?key=supersecretkey',1,'per_user','waiting_signin',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	ada := e.member(t, "user_ada", "ada@beknown.work")
	if err := e.access.SetGroups(t.Context(), ada.ID, []string{"keyed"}, e.admin.ID); err != nil {
		t.Fatal(err)
	}
	rec := e.do(t, e.cookieFor(t, ada.ID), http.MethodGet, "/v1/me/connections", "")
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, `"host":"mcp.example.com"`) {
		t.Fatalf("list: %d %s", rec.Code, body)
	}
	if strings.Contains(body, "supersecret") || strings.Contains(body, `"url"`) {
		t.Fatalf("list leaks the server URL: %s", body)
	}
}
