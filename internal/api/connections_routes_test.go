package api

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/gateway"
	"github.com/tusharbhardwaj/toolyard/internal/oauth"
	"github.com/tusharbhardwaj/toolyard/internal/upstreams"
)

// connTokenIdP is a token endpoint for the connection tests: code "x"
// becomes access token "at-x" with an id_token naming x@example.test.
func connTokenIdP(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		code := r.Form.Get("code")
		hdr := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
		pl, _ := json.Marshal(map[string]string{"email": code + "@example.test"})
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "at-" + code, "refresh_token": "rt-" + code, "token_type": "Bearer", "expires_in": 3600,
			"id_token": hdr + "." + base64.RawURLEncoding.EncodeToString(pl) + ".s",
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// oauthEnv is the real oauth.Service wired into the test server, with the
// fake IdP its clients point at.
type oauthEnv struct {
	oa  *oauth.Service
	idp *httptest.Server
}

// withOAuth wires a real oauth.Service (fake IdP, random key) into the test
// server and the upstreams service, the way main.go does.
func withOAuth(t *testing.T, e *accessTestEnv) *oauthEnv {
	t.Helper()
	key := make([]byte, oauth.MasterKeySize)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	cipher, err := oauth.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	idp := connTokenIdP(t)
	oa := oauth.New(e.db, cipher, nil, nil, nil)
	oa.SetHTTPClient(idp.Client())
	e.srv.oauth = oa
	e.srv.upstreams.SetAuth(oa)
	e.srv.upstreams.SetPerUserAuth(oa)
	oa.SetUserReauthHook(e.srv.upstreams.DropUserConnection)
	return &oauthEnv{oa: oa, idp: idp}
}

// client registers the server's OAuth client (what discovery would store);
// the server row must exist first, the client row references it.
func (o *oauthEnv) client(t *testing.T, name string) {
	t.Helper()
	if err := o.oa.PutClient(context.Background(), oauth.ClientRecord{
		UpstreamName: name, Issuer: o.idp.URL, AuthorizationEndpoint: o.idp.URL + "/authorize", TokenEndpoint: o.idp.URL + "/token",
		ClientID: "cid", RedirectURI: "https://toolyard.example/v1/mcp-oauth/callback", TokenEndpointAuthMethod: "none",
	}); err != nil {
		t.Fatal(err)
	}
}

// addPerUserServer connects a real per_user http upstream.
func addPerUserServer(t *testing.T, e *accessTestEnv, name string, ts *httptest.Server) {
	t.Helper()
	if _, err := e.srv.upstreams.Add(context.Background(), upstreams.Server{
		Name: name, Transport: "http", URL: ts.URL, AuthMode: upstreams.AuthPerUser,
	}); err != nil {
		t.Fatalf("add %s: %v", name, err)
	}
}

func decodeConnections(t *testing.T, rec *httptest.ResponseRecorder) []connectionView {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var out []connectionView
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v %s", err, rec.Body.String())
	}
	return out
}

// stateOf pulls the state parameter out of an authorize URL.
func stateOf(t *testing.T, authorizeURL string) string {
	t.Helper()
	u, err := url.Parse(authorizeURL)
	if err != nil || u.Query().Get("state") == "" {
		t.Fatalf("authorize url %q: %v", authorizeURL, err)
	}
	return u.Query().Get("state")
}

// callback hits the OAuth return route the way the IdP redirect does: a
// plain GET with the browser's cookie and no CSRF header.
func (e *accessTestEnv) callback(t *testing.T, cookie *http.Cookie, state, code string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/mcp-oauth/callback?state="+url.QueryEscape(state)+"&code="+url.QueryEscape(code), nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

func userTokenRows(t *testing.T, e *accessTestEnv, name string) map[string]string {
	t.Helper()
	rows, err := e.db.Query(`SELECT user_id, state FROM oauth_user_tokens WHERE upstream_name = ?`, name)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var uid, state string
		_ = rows.Scan(&uid, &state)
		out[uid] = state
	}
	return out
}

// waitStatus polls the server row until last_status matches, since the
// post-sign-in reconnect runs in the background.
func waitStatus(t *testing.T, e *accessTestEnv, name, want string) upstreams.Server {
	t.Helper()
	var sv *upstreams.Server
	for i := 0; i < 200; i++ {
		sv, _ = e.srv.upstreams.Get(context.Background(), name)
		if sv != nil && sv.LastStatus == want {
			return *sv
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("server %s status = %+v, want %q", name, sv, want)
	return upstreams.Server{}
}

// TestConnectionsMemberFlow: a member sees the per_user servers granted to
// them, starts their own sign-in, and the callback stores the token only
// when the same person's browser returns; another member's browser
// returning with that state stores nothing and burns the flow. The admin
// view names who connected; disconnecting removes the row.
func TestConnectionsMemberFlow(t *testing.T) {
	e := newAccessTestServer(t)
	oe := withOAuth(t, e)
	ts := newPatchMCPServer(t)
	addPerUserServer(t, e, "linear", ts)
	oe.client(t, "linear")
	e.seedServer(t, "shared", "ok", true)
	oe.client(t, "shared")
	e.seedServer(t, "other", "ok", true)
	if _, err := e.db.Exec(`UPDATE upstream_servers SET auth_mode = 'per_user' WHERE name = 'other'`); err != nil {
		t.Fatal(err)
	}
	ada := e.member(t, "user_ada", "ada@beknown.work")
	bob := e.member(t, "user_bob", "bob@beknown.work")
	if err := e.access.SetGroups(t.Context(), ada.ID, []string{"linear", "shared"}, e.admin.ID); err != nil {
		t.Fatal(err)
	}
	adaCookie, bobCookie, adminCookie := e.cookieFor(t, ada.ID), e.cookieFor(t, bob.ID), e.cookieFor(t, e.admin.ID)

	// Before anyone connects, the server is up without tools.
	if sv, _ := e.srv.upstreams.Get(t.Context(), "linear"); sv.LastStatus != gateway.StatusWaitingSignIn || sv.ToolCount != 0 {
		t.Fatalf("linear before sign-in: %+v", sv)
	}

	// Ada sees linear (granted, per_user) and not shared (shared) nor
	// other (per_user, not granted); bob sees nothing.
	list := decodeConnections(t, e.do(t, adaCookie, http.MethodGet, "/v1/me/connections", ""))
	if len(list) != 1 || list[0].Server != "linear" || list[0].State != oauth.ConnNeedsSignIn || !list[0].Ready ||
		list[0].ServerStatus != gateway.StatusWaitingSignIn || list[0].ToolCount != 0 {
		t.Fatalf("ada's connections = %+v", list)
	}
	if list := decodeConnections(t, e.do(t, bobCookie, http.MethodGet, "/v1/me/connections", "")); len(list) != 0 {
		t.Fatalf("bob's connections = %+v", list)
	}
	// The admin sees every per_user server, including one with no client
	// registered yet (not ready).
	list = decodeConnections(t, e.do(t, adminCookie, http.MethodGet, "/v1/me/connections", ""))
	if len(list) != 2 || list[0].Server != "linear" || list[1].Server != "other" || list[1].Ready {
		t.Fatalf("admin's connections = %+v", list)
	}

	// Begin: ada may, bob may not (same answer as an unknown server), a
	// shared server has nothing to connect.
	rec := e.do(t, adaCookie, http.MethodPost, "/v1/me/connections/linear/begin", "{}")
	if rec.Code != http.StatusOK {
		t.Fatalf("ada begin: %d %s", rec.Code, rec.Body.String())
	}
	var begun struct {
		AuthorizeURL string `json:"authorize_url"`
		State        string `json:"state"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &begun)
	if begun.AuthorizeURL == "" || stateOf(t, begun.AuthorizeURL) != begun.State {
		t.Fatalf("begin body = %s", rec.Body.String())
	}
	if rec := e.do(t, bobCookie, http.MethodPost, "/v1/me/connections/linear/begin", "{}"); rec.Code != http.StatusNotFound {
		t.Fatalf("bob begin on an ungranted server: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do(t, adaCookie, http.MethodPost, "/v1/me/connections/shared/begin", "{}"); rec.Code != http.StatusBadRequest {
		t.Fatalf("begin on a shared server: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do(t, adaCookie, http.MethodPost, "/v1/me/connections/nope/begin", "{}"); rec.Code != http.StatusNotFound {
		t.Fatalf("begin on an unknown server: %d", rec.Code)
	}
	if rec := e.do(t, adminCookie, http.MethodPost, "/v1/me/connections/other/begin", "{}"); rec.Code != http.StatusConflict {
		t.Fatalf("begin without a client: %d %s", rec.Code, rec.Body.String())
	}

	// Bob's browser comes back with ada's state: refused, nothing stored,
	// and the flow is burnt so ada cannot finish it either.
	rec = e.callback(t, bobCookie, begun.State, "ada")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "different toolyard user") {
		t.Fatalf("bob on ada's callback: %d %s", rec.Code, rec.Body.String())
	}
	if rows := userTokenRows(t, e, "linear"); len(rows) != 0 {
		t.Fatalf("token rows after mismatch = %v", rows)
	}
	if rec := e.callback(t, adaCookie, begun.State, "ada"); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "expired or was already used") {
		t.Fatalf("ada on a burnt flow: %d %s", rec.Code, rec.Body.String())
	}
	if e.auditRows(t, "oauth.user_mismatch", "") != 1 {
		t.Fatal("mismatch not audited")
	}

	// A fresh flow with no browser session is refused too.
	rec = e.do(t, adaCookie, http.MethodPost, "/v1/me/connections/linear/begin", "{}")
	_ = json.Unmarshal(rec.Body.Bytes(), &begun)
	if rec := e.callback(t, nil, begun.State, "ada"); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "not signed in") {
		t.Fatalf("anonymous on a per-user callback: %d %s", rec.Code, rec.Body.String())
	}

	// Ada's own browser finishes her flow: token stored on her row, the
	// server gets its tools over her session, the page shows connected.
	rec = e.do(t, adaCookie, http.MethodPost, "/v1/me/connections/linear/begin", "{}")
	_ = json.Unmarshal(rec.Body.Bytes(), &begun)
	rec = e.callback(t, adaCookie, begun.State, "ada")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Connected") || !strings.Contains(rec.Body.String(), "ada@example.test") {
		t.Fatalf("ada's callback: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "at-ada") {
		t.Fatal("callback page leaks the access token")
	}
	if rows := userTokenRows(t, e, "linear"); len(rows) != 1 || rows[ada.ID] != oauth.StateActive {
		t.Fatalf("token rows after ada = %v", rows)
	}
	sv := waitStatus(t, e, "linear", "ok")
	if sv.ToolCount != 1 {
		t.Fatalf("linear after first sign-in: %+v", sv)
	}
	list = decodeConnections(t, e.do(t, adaCookie, http.MethodGet, "/v1/me/connections", ""))
	if len(list) != 1 || list[0].State != oauth.ConnConnected || list[0].AccountLabel != "ada@example.test" || list[0].ConnectedAt == 0 || list[0].ToolCount != 1 {
		t.Fatalf("ada's connections after sign-in = %+v", list)
	}
	if strings.Contains(e.do(t, adaCookie, http.MethodGet, "/v1/me/connections", "").Body.String(), "at-ada") {
		t.Fatal("connections list leaks a token")
	}

	// The admin's view of the server names ada; a member gets the guard's
	// 403 on every /v1/servers route.
	rec = e.do(t, adminCookie, http.MethodGet, "/v1/servers/linear/connections", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("admin connections: %d %s", rec.Code, rec.Body.String())
	}
	var who struct {
		AuthMode    string                 `json:"auth_mode"`
		Status      string                 `json:"status"`
		Connections []serverConnectionView `json:"connections"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &who)
	if who.AuthMode != upstreams.AuthPerUser || who.Status != "ok" || len(who.Connections) != 1 ||
		who.Connections[0].UserID != ada.ID || who.Connections[0].Email != "ada@beknown.work" ||
		who.Connections[0].State != oauth.ConnConnected || who.Connections[0].AccountLabel != "ada@example.test" {
		t.Fatalf("who signs in = %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "at-ada") || strings.Contains(rec.Body.String(), "rt-ada") {
		t.Fatal("admin view leaks a token")
	}
	for _, p := range []string{"/v1/servers/linear/connections", "/v1/servers/linear/oauth", "/v1/servers"} {
		if rec := e.do(t, adaCookie, http.MethodGet, p, ""); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "admin_only") {
			t.Errorf("member GET %s: %d %s", p, rec.Code, rec.Body.String())
		}
	}
	if rec := e.do(t, adaCookie, http.MethodPost, "/v1/servers/linear/oauth/begin", "{}"); rec.Code != http.StatusForbidden {
		t.Errorf("member on the shared begin route: %d", rec.Code)
	}

	// Ada's agent runs as her account; bob's agent has nothing to run as.
	adaTok, _ := e.agentFor(t, ada.ID)
	rec = e.bearer(t, adaTok, http.MethodPost, "/v1/agents/tools/run",
		`{"tool":"linear.get_status","arguments":{"_reason":"connections test: run as ada's own account"}}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok"`) || strings.Contains(rec.Body.String(), `"isError":true`) {
		t.Fatalf("ada's agent call: %d %s", rec.Code, rec.Body.String())
	}
	var owner string
	_ = e.db.QueryRow(`SELECT owner_user_id FROM audit_events WHERE event_type='call.succeeded' AND tool_name='linear.get_status' ORDER BY ts DESC LIMIT 1`).Scan(&owner)
	if owner != ada.ID {
		t.Fatalf("audit owner on the per-user call = %q", owner)
	}

	// Disconnect: row gone, revoked at the IdP is best-effort, a second
	// delete is 404, and the page is back to needs_signin.
	if rec := e.do(t, adaCookie, http.MethodDelete, "/v1/me/connections/linear", ""); rec.Code != http.StatusOK {
		t.Fatalf("disconnect: %d %s", rec.Code, rec.Body.String())
	}
	if rows := userTokenRows(t, e, "linear"); len(rows) != 0 {
		t.Fatalf("rows after disconnect = %v", rows)
	}
	if rec := e.do(t, adaCookie, http.MethodDelete, "/v1/me/connections/linear", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("second disconnect: %d", rec.Code)
	}
	list = decodeConnections(t, e.do(t, adaCookie, http.MethodGet, "/v1/me/connections", ""))
	if list[0].State != oauth.ConnNeedsSignIn {
		t.Fatalf("after disconnect = %+v", list)
	}
	if ok, _ := oe.oa.UserConnected(t.Context(), "linear", ada.ID); ok {
		t.Fatal("service still reports ada connected")
	}
	if e.auditRows(t, "oauth.user_begin", "") != 3 || e.auditRows(t, "oauth.user_success", "") != 1 || e.auditRows(t, "oauth.user_disconnect", "") != 1 {
		t.Fatalf("audit rows: begin=%d success=%d disconnect=%d",
			e.auditRows(t, "oauth.user_begin", ""), e.auditRows(t, "oauth.user_success", ""), e.auditRows(t, "oauth.user_disconnect", ""))
	}
}

// TestConnectionsSharedFlowStaysAdminOnly: with the callback open to
// members, a member's browser cannot finish an admin's shared-account
// flow, through the callback or the paste route.
func TestConnectionsSharedFlowStaysAdminOnly(t *testing.T) {
	e := newAccessTestServer(t)
	oe := withOAuth(t, e)
	e.seedServer(t, "shared", "ok", true)
	oe.client(t, "shared")
	m := e.member(t, "user_m", "m@beknown.work")
	memberCookie, adminCookie := e.cookieFor(t, m.ID), e.cookieFor(t, e.admin.ID)

	rec := e.do(t, adminCookie, http.MethodPost, "/v1/servers/shared/oauth/begin", `{"mode":"callback"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin begin: %d %s", rec.Code, rec.Body.String())
	}
	var begun struct {
		State string `json:"state"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &begun)
	if rec := e.callback(t, memberCookie, begun.State, "bot"); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "only an admin") {
		t.Fatalf("member on a shared callback: %d %s", rec.Code, rec.Body.String())
	}
	var tokens int
	_ = e.db.QueryRow(`SELECT count(*) FROM oauth_tokens WHERE upstream_name = 'shared'`).Scan(&tokens)
	if tokens != 0 {
		t.Fatal("member's browser stored the shared token")
	}
	// The flow is still the admin's to finish.
	rec = e.callback(t, adminCookie, begun.State, "bot")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Authorized") {
		t.Fatalf("admin on the shared callback: %d %s", rec.Code, rec.Body.String())
	}
	_ = e.db.QueryRow(`SELECT count(*) FROM oauth_tokens WHERE upstream_name = 'shared'`).Scan(&tokens)
	if tokens != 1 {
		t.Fatal("admin's shared token not stored")
	}
	// The shared status now names the account the server acts as.
	rec = e.do(t, adminCookie, http.MethodGet, "/v1/servers/shared/oauth", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"account_label":"bot@example.test"`) || strings.Contains(rec.Body.String(), "at-bot") {
		t.Fatalf("shared status: %d %s", rec.Code, rec.Body.String())
	}

	// Paste: a member cannot finish a shared paste flow either.
	rec = e.do(t, adminCookie, http.MethodPost, "/v1/servers/shared/oauth/begin", `{"mode":"paste"}`)
	_ = json.Unmarshal(rec.Body.Bytes(), &begun)
	rec = e.do(t, memberCookie, http.MethodPost, "/v1/mcp-oauth/paste", `{"url":"https://blackhole.invalid/cb?code=bot2&state=`+begun.State+`"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("member paste on a shared flow: %d %s", rec.Code, rec.Body.String())
	}
}

// TestConnectionsPasteForUser: a per-user flow can also be finished by
// pasting the redirect URL, by the person who started it and nobody else.
func TestConnectionsPasteForUser(t *testing.T) {
	e := newAccessTestServer(t)
	oe := withOAuth(t, e)
	ts := newPatchMCPServer(t)
	addPerUserServer(t, e, "linear", ts)
	oe.client(t, "linear")
	ada := e.member(t, "user_ada", "ada@beknown.work")
	bob := e.member(t, "user_bob", "bob@beknown.work")
	if err := e.access.SetGroups(t.Context(), ada.ID, []string{"linear"}, e.admin.ID); err != nil {
		t.Fatal(err)
	}
	adaCookie, bobCookie := e.cookieFor(t, ada.ID), e.cookieFor(t, bob.ID)

	rec := e.do(t, adaCookie, http.MethodPost, "/v1/me/connections/linear/begin", "{}")
	var begun struct {
		State string `json:"state"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &begun)
	paste := func(cookie *http.Cookie, code string) *httptest.ResponseRecorder {
		return e.do(t, cookie, http.MethodPost, "/v1/mcp-oauth/paste", `{"url":"https://blackhole.invalid/cb?code=`+code+`&state=`+begun.State+`"}`)
	}
	if rec := paste(bobCookie, "ada"); rec.Code != http.StatusForbidden {
		t.Fatalf("bob pasting ada's flow: %d %s", rec.Code, rec.Body.String())
	}
	if rows := userTokenRows(t, e, "linear"); len(rows) != 0 {
		t.Fatalf("rows after bob's paste = %v", rows)
	}
	// Burnt: ada starts over and pastes her own.
	rec = e.do(t, adaCookie, http.MethodPost, "/v1/me/connections/linear/begin", "{}")
	_ = json.Unmarshal(rec.Body.Bytes(), &begun)
	rec = paste(adaCookie, "ada")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"account_label":"ada@example.test"`) || strings.Contains(rec.Body.String(), "at-ada") {
		t.Fatalf("ada's paste: %d %s", rec.Code, rec.Body.String())
	}
	if rows := userTokenRows(t, e, "linear"); len(rows) != 1 || rows[ada.ID] != oauth.StateActive {
		t.Fatalf("rows after ada's paste = %v", rows)
	}
	waitStatus(t, e, "linear", "ok")
}

// TestServersAuthModeRoundTrip: the Servers API accepts and returns
// auth_mode on create and patch, rejects a bad value, and a per_user
// server is created fine with nobody connected.
func TestServersAuthModeRoundTrip(t *testing.T) {
	e := newAccessTestServer(t)
	withOAuth(t, e)
	ts := newPatchMCPServer(t)
	adminCookie := e.cookieFor(t, e.admin.ID)

	rec := e.do(t, adminCookie, http.MethodPost, "/v1/servers",
		`{"name":"linear","transport":"http","url":"`+ts.URL+`","auth_mode":"per_user"}`)
	sv := decodeServer(t, rec)
	if sv.AuthMode != upstreams.AuthPerUser || sv.LastStatus != gateway.StatusWaitingSignIn || sv.LastError != "" || sv.ToolCount != 0 {
		t.Fatalf("created per_user server = %+v", sv)
	}
	rec = e.do(t, adminCookie, http.MethodPost, "/v1/servers",
		`{"name":"plain","transport":"http","url":"`+ts.URL+`"}`)
	if sv := decodeServer(t, rec); sv.AuthMode != upstreams.AuthShared || sv.LastStatus != "ok" {
		t.Fatalf("created default server = %+v", sv)
	}
	if rec := e.do(t, adminCookie, http.MethodPost, "/v1/servers",
		`{"name":"bad","transport":"http","url":"`+ts.URL+`","auth_mode":"everyone"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad auth_mode: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do(t, adminCookie, http.MethodPost, "/v1/servers",
		`{"name":"local","transport":"stdio","command":"true","auth_mode":"per_user"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("stdio per_user: %d %s", rec.Code, rec.Body.String())
	}

	// Patch to per_user and back; the list shows the mode.
	sv = decodeServer(t, e.do(t, adminCookie, http.MethodPatch, "/v1/servers/plain", `{"auth_mode":"per_user"}`))
	if sv.AuthMode != upstreams.AuthPerUser || sv.LastStatus != gateway.StatusWaitingSignIn {
		t.Fatalf("patched to per_user = %+v", sv)
	}
	sv = decodeServer(t, e.do(t, adminCookie, http.MethodPatch, "/v1/servers/plain", `{"auth_mode":"shared"}`))
	if sv.AuthMode != upstreams.AuthShared || sv.LastStatus != "ok" || sv.ToolCount != 1 {
		t.Fatalf("patched back to shared = %+v", sv)
	}
	rec = e.do(t, adminCookie, http.MethodGet, "/v1/servers", "")
	if !strings.Contains(rec.Body.String(), `"auth_mode":"per_user"`) || !strings.Contains(rec.Body.String(), `"auth_mode":"shared"`) {
		t.Fatalf("list lacks auth_mode: %s", rec.Body.String())
	}
}
