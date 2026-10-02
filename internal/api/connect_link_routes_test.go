package api

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/oauth"
)

// The connect link: the page an agent sends the person to when a server
// needs their sign-in. These tests open it the way a browser does (a GET
// for the confirm page, then the page's own form POST with its nonce, no
// CSRF header, no toolyard session unless stated) and finish the flow the
// way the provider's redirect does.

// connectLinkIdP is a token endpoint whose code decides the account: code
// "as:<email>" yields an id_token naming that email, "unverified:<email>"
// one that names it with email_verified false, "noemail" yields no
// id_token, anything else names <code>@example.test.
func connectLinkIdP(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		code := r.Form.Get("code")
		out := map[string]any{"access_token": "at-" + code, "refresh_token": "rt-" + code, "token_type": "Bearer", "expires_in": 3600}
		claims := map[string]any{"email": code + "@example.test"}
		switch {
		case code == "noemail":
			claims = nil
		case strings.HasPrefix(code, "as:"):
			claims["email"] = strings.TrimPrefix(code, "as:")
		case strings.HasPrefix(code, "unverified:"):
			claims["email"] = strings.TrimPrefix(code, "unverified:")
			claims["email_verified"] = false
		}
		if claims != nil {
			hdr := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
			pl, _ := json.Marshal(claims)
			out["id_token"] = hdr + "." + base64.RawURLEncoding.EncodeToString(pl) + ".s"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("/revoke", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// withConnectOAuth is withOAuth with the account-choosing IdP.
func withConnectOAuth(t *testing.T, e *accessTestEnv) *oauthEnv {
	t.Helper()
	key := make([]byte, oauth.MasterKeySize)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	cipher, err := oauth.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	idp := connectLinkIdP(t)
	oa := oauth.New(e.db, cipher, nil, nil, nil)
	oa.SetHTTPClient(idp.Client())
	e.srv.oauth = oa
	e.srv.upstreams.SetAuth(oa)
	e.srv.upstreams.SetPerUserAuth(oa)
	oa.SetUserReauthHook(e.srv.upstreams.DropUserConnection)
	return &oauthEnv{oa: oa, idp: idp}
}

var nonceFieldRE = regexp.MustCompile(`name="nonce" value="([^"]+)"`)

// confirmLink GETs the confirm page the way a browser does: no CSRF
// header, only the given cookies.
func (e *accessTestEnv) confirmLink(t *testing.T, ticket string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, connectLinkPath+ticket, nil)
	for _, c := range cookies {
		if c != nil {
			req.AddCookie(c)
		}
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

// nonceOf pulls the nonce cookie and the form nonce out of a confirm page.
func nonceOf(t *testing.T, page *httptest.ResponseRecorder) (*http.Cookie, string) {
	t.Helper()
	var cookie *http.Cookie
	for _, c := range page.Result().Cookies() {
		if strings.HasPrefix(c.Name, connectNoncePrefix) {
			cookie = c
		}
	}
	m := nonceFieldRE.FindStringSubmatch(page.Body.String())
	if cookie == nil || m == nil {
		t.Fatalf("confirm page without nonce: cookies=%v body=%s", page.Header().Values("Set-Cookie"), page.Body.String())
	}
	return cookie, m[1]
}

// continueLink POSTs the confirm page's form the way the browser does:
// form-encoded, the page's nonce cookie and field, Sec-Fetch-Site
// same-origin, no CSRF header, plus the given cookies (a session).
func (e *accessTestEnv) continueLink(t *testing.T, ticket string, page *httptest.ResponseRecorder, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	nonceCookie, nonce := nonceOf(t, page)
	return e.postLink(t, ticket, url.Values{"nonce": {nonce}}, "same-origin", append(cookies, nonceCookie)...)
}

// postLink is the raw form POST.
func (e *accessTestEnv) postLink(t *testing.T, ticket string, form url.Values, fetchSite string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, connectLinkPath+ticket, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if fetchSite != "" {
		req.Header.Set("Sec-Fetch-Site", fetchSite)
	}
	for _, c := range cookies {
		if c != nil {
			req.AddCookie(c)
		}
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

// followLink is the person's two clicks: open the link, press Continue.
// The given cookies ride on both requests.
func (e *accessTestEnv) followLink(t *testing.T, ticket string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	page := e.confirmLink(t, ticket, cookies...)
	if page.Code != http.StatusOK {
		t.Fatalf("confirm page: %d %s", page.Code, page.Body.String())
	}
	return e.continueLink(t, ticket, page, cookies...)
}

// redirectState checks the 303 points at the IdP's authorize endpoint with
// toolyard's callback as redirect_uri, and returns the state.
func redirectState(t *testing.T, rec *httptest.ResponseRecorder, idp *httptest.Server) string {
	t.Helper()
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d, want 303: %s", rec.Code, rec.Body.String())
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil || loc.String() == "" {
		t.Fatalf("Location %q: %v", rec.Header().Get("Location"), err)
	}
	if loc.Scheme+"://"+loc.Host != idp.URL || loc.Path != "/authorize" {
		t.Fatalf("redirected to %s, want the IdP's authorize endpoint", loc)
	}
	q := loc.Query()
	if q.Get("redirect_uri") != "https://toolyard.example/v1/mcp-oauth/callback" || q.Get("code_challenge") == "" || q.Get("state") == "" {
		t.Fatalf("authorize query = %v", q)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q", cc)
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "form-action 'self' "+idp.URL) {
		t.Fatalf("POST response CSP form-action does not allow the provider: %q", csp)
	}
	return q.Get("state")
}

// auditMentions counts audit rows whose text columns contain s.
func (e *accessTestEnv) auditMentions(t *testing.T, s string) int {
	t.Helper()
	var n int
	if err := e.db.QueryRow(`SELECT count(*) FROM audit_events WHERE result_summary LIKE '%'||?||'%' OR reason LIKE '%'||?||'%' OR COALESCE(arguments,'') LIKE '%'||?||'%'`, s, s, s).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// auditReasons lists the reasons on one event type, sorted (rows written
// in the same millisecond have no stable order).
func (e *accessTestEnv) auditReasons(t *testing.T, eventType string) []string {
	t.Helper()
	rows, err := e.db.Query(`SELECT COALESCE(reason,'') FROM audit_events WHERE event_type = ?`, eventType)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var r string
		_ = rows.Scan(&r)
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

func (e *accessTestEnv) pendingFlows(t *testing.T) int {
	t.Helper()
	var n int
	if err := e.db.QueryRow(`SELECT count(*) FROM oauth_pending`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

type connectLinkEnv struct {
	e   *accessTestEnv
	oe  *oauthEnv
	ada *identity.User // member with an email, granted linear
}

func newConnectLinkEnv(t *testing.T) *connectLinkEnv {
	t.Helper()
	e := newAccessTestServer(t)
	oe := withConnectOAuth(t, e)
	ts := newPatchMCPServer(t)
	addPerUserServer(t, e, "linear", ts)
	oe.client(t, "linear")
	ada := e.member(t, "user_ada", "ada@beknown.work")
	if err := e.access.SetGroups(t.Context(), ada.ID, []string{"linear"}, e.admin.ID); err != nil {
		t.Fatal(err)
	}
	return &connectLinkEnv{e: e, oe: oe, ada: ada}
}

func (c *connectLinkEnv) ticket(t *testing.T, uid, server, purpose string) string {
	t.Helper()
	tk, err := c.oe.oa.IssueConnectTicket(context.Background(), uid, server, purpose, "ag_test")
	if err != nil {
		t.Fatalf("issue ticket: %v", err)
	}
	return tk
}

// live reports whether a ticket can still be redeemed.
func (c *connectLinkEnv) live(t *testing.T, ticket string) bool {
	t.Helper()
	_, err := c.oe.oa.PeekConnectTicket(context.Background(), ticket)
	return err == nil
}

// admin makes a Clerk-linked admin with an email.
func (c *connectLinkEnv) admin(t *testing.T, clerkID, email string) *identity.User {
	t.Helper()
	u := c.e.member(t, clerkID, email)
	if err := c.e.id.SetRole(context.Background(), u.ID, identity.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	u.Role = identity.RoleAdmin
	return u
}

// TestConnectLinkConfirmPageHasNoSideEffects: opening a link shows whose
// it is and what Continue does, sets the nonce cookie and a CSP that lets
// the form's redirect reach the provider, and changes nothing: the ticket
// stays live, no flow starts, nothing is audited. HEAD does nothing.
func TestConnectLinkConfirmPageHasNoSideEffects(t *testing.T) {
	c := newConnectLinkEnv(t)
	e, ada := c.e, c.ada
	ticket := c.ticket(t, ada.ID, "linear", oauth.ConnectPurposePerUser)

	for i := 0; i < 3; i++ {
		page := e.confirmLink(t, ticket)
		if page.Code != http.StatusOK {
			t.Fatalf("confirm %d: %d %s", i, page.Code, page.Body.String())
		}
		body := page.Body.String()
		for _, want := range []string{"Connect linear for ada@beknown.work", `<form method="post" action="` + connectLinkPath + ticket + `"`, "Continue", `<link rel="stylesheet" href="/style.css">`} {
			if !strings.Contains(body, want) {
				t.Errorf("confirm page lacks %q:\n%s", want, body)
			}
		}
		if strings.Contains(body, "<style") || strings.Contains(body, " style=") {
			t.Fatalf("confirm page styles inline (blocked by the CSP):\n%s", body)
		}
		cookie, nonce := nonceOf(t, page)
		if cookie.Value != nonce || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != connectLinkPath || cookie.MaxAge != int(oauth.ConnectTicketTTL.Seconds()) {
			t.Fatalf("nonce cookie = %+v (form nonce %q)", cookie, nonce)
		}
		if csp := page.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "form-action 'self' "+c.oe.idp.URL) || !strings.Contains(csp, "script-src 'self'") {
			t.Fatalf("confirm page CSP = %q", csp)
		}
	}
	head := httptest.NewRequest(http.MethodHead, connectLinkPath+ticket, nil)
	hrec := httptest.NewRecorder()
	e.handler.ServeHTTP(hrec, head)
	if hrec.Code != http.StatusOK || hrec.Body.Len() != 0 {
		t.Fatalf("HEAD: %d %q", hrec.Code, hrec.Body.String())
	}
	if !c.live(t, ticket) || e.pendingFlows(t) != 0 || e.auditRows(t, connectLinkUsed, "") != 0 || e.auditRows(t, connectLinkRefused, "") != 0 {
		t.Fatal("showing the confirm page had side effects")
	}
}

// TestConnectLinkContinueStartsPerUserSignIn: Continue redeems the ticket,
// sets the flow cookie for that state and person, and redirects to the
// provider with toolyard's callback; the provider's return in that browser
// stores the token on the person's row (case-insensitive email match) and
// shows the "back to your agent" page. The link works once, the person's
// other live links for the server close with it, and the ticket never
// reaches the audit log.
func TestConnectLinkContinueStartsPerUserSignIn(t *testing.T) {
	c := newConnectLinkEnv(t)
	e, ada := c.e, c.ada
	ticket := c.ticket(t, ada.ID, "linear", oauth.ConnectPurposePerUser)
	spare := c.ticket(t, ada.ID, "linear", oauth.ConnectPurposePerUser)

	rec := e.followLink(t, ticket)
	state := redirectState(t, rec, c.oe.idp)
	cookie := flowCookieFrom(t, rec)
	if cookie.Name != oauthFlowCookieName(state) || cookie.Path != oauthFlowCookiePath || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("flow cookie = %+v", cookie)
	}
	if !e.srv.oauthFlowCookieValid(&http.Request{Header: http.Header{"Cookie": {cookie.String()}}}, state, ada.ID) {
		t.Fatal("flow cookie is not bound to (state, ada)")
	}
	if c.live(t, ticket) {
		t.Fatal("Continue did not redeem the ticket")
	}
	if e.auditRows(t, connectLinkUsed, "") != 1 || e.auditMentions(t, ticket) != 0 {
		t.Fatalf("link_used rows = %d, ticket mentions = %d", e.auditRows(t, connectLinkUsed, ""), e.auditMentions(t, ticket))
	}
	var agent, owner string
	_ = e.db.QueryRow(`SELECT COALESCE(agent_id,''), COALESCE(owner_user_id,'') FROM audit_events WHERE event_type = ?`, connectLinkUsed).Scan(&agent, &owner)
	if agent != "ag_test" || owner != ada.ID {
		t.Fatalf("link_used names agent %q owner %q", agent, owner)
	}

	// The provider sends the browser back; the email differs only in case.
	cb := e.callbackCookies(t, state, "as:Ada@Beknown.work", cookie)
	if cb.Code != http.StatusOK || !strings.Contains(cb.Body.String(), "Go back to T3 Code") || !strings.Contains(cb.Body.String(), "tell your agent to retry") || strings.Contains(cb.Body.String(), "<style") {
		t.Fatalf("callback: %d %s", cb.Code, cb.Body.String())
	}
	if rows := userTokenRows(t, e, "linear"); rows[ada.ID] != "active" || len(rows) != 1 {
		t.Fatalf("token rows = %v", rows)
	}
	var label string
	_ = e.db.QueryRow(`SELECT account_label FROM oauth_user_tokens WHERE upstream_name = 'linear' AND user_id = ?`, ada.ID).Scan(&label)
	if label != "Ada@Beknown.work" {
		t.Fatalf("account_label = %q", label)
	}
	if e.auditRows(t, connectUnverifiedAccount, "") != 0 || e.auditRows(t, connectAccountMismatch, "") != 0 {
		t.Fatal("a verified match was audited as unverified or mismatched")
	}
	// The spare link closed with the sign-in.
	if c.live(t, spare) {
		t.Fatal("another live link for the same person and server survived the sign-in")
	}

	// Once only: the page says so, Continue is refused.
	if page := e.confirmLink(t, ticket); page.Code != http.StatusGone || !strings.Contains(page.Body.String(), "expired or was already used") {
		t.Fatalf("confirm after use: %d %s", page.Code, page.Body.String())
	}
	if again := e.postLink(t, ticket, url.Values{"nonce": {"x"}}, "same-origin"); again.Code != http.StatusForbidden {
		t.Fatalf("continue after use without a nonce: %d", again.Code)
	}
	if e.auditMentions(t, ticket) != 0 {
		t.Fatal("ticket reached the audit log")
	}
}

// TestConnectLinkContinueNeedsNonceAndSameOrigin: a POST that did not come
// from the confirm page (no nonce cookie, a nonce that does not match, or
// a cross-site Sec-Fetch-Site) is refused and audited, and the ticket
// stays live for the real click.
func TestConnectLinkContinueNeedsNonceAndSameOrigin(t *testing.T) {
	c := newConnectLinkEnv(t)
	e, ada := c.e, c.ada
	ticket := c.ticket(t, ada.ID, "linear", oauth.ConnectPurposePerUser)
	page := e.confirmLink(t, ticket)
	nonceCookie, nonce := nonceOf(t, page)

	// No cookie at all (a form forged elsewhere).
	if rec := e.postLink(t, ticket, url.Values{"nonce": {nonce}}, "same-origin"); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "Open the link again") {
		t.Fatalf("no nonce cookie: %d %s", rec.Code, rec.Body.String())
	}
	// Cookie present, field wrong.
	if rec := e.postLink(t, ticket, url.Values{"nonce": {"not-it"}}, "same-origin", nonceCookie); rec.Code != http.StatusForbidden {
		t.Fatalf("wrong nonce: %d", rec.Code)
	}
	// Right nonce, but the browser says the request came from elsewhere.
	if rec := e.postLink(t, ticket, url.Values{"nonce": {nonce}}, "cross-site", nonceCookie); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site: %d", rec.Code)
	}
	if !c.live(t, ticket) || e.pendingFlows(t) != 0 {
		t.Fatal("a refused Continue redeemed the ticket or started a flow")
	}
	if got := e.auditReasons(t, connectLinkRefused); strings.Join(got, " ") != "cross_site nonce nonce" {
		t.Fatalf("link_refused reasons = %v", got)
	}
	// A browser that sends no Sec-Fetch-Site (older) still passes on the nonce alone.
	if rec := e.postLink(t, ticket, url.Values{"nonce": {nonce}}, "", nonceCookie); rec.Code != http.StatusSeeOther {
		t.Fatalf("real click: %d %s", rec.Code, rec.Body.String())
	}
}

// TestConnectLinkRefusesStaleTickets: unknown and expired tickets get the
// plain page on GET (no audit: showing a page changes nothing) and an
// audited refusal on Continue; the route takes GET and POST only and is
// never reachable with an operator token.
func TestConnectLinkRefusesStaleTickets(t *testing.T) {
	c := newConnectLinkEnv(t)
	e := c.e
	if connectLinkPath != oauth.ConnectLinkPath {
		t.Fatalf("route %q and oauth.ConnectLinkPath %q differ", connectLinkPath, oauth.ConnectLinkPath)
	}
	if page := e.confirmLink(t, "not-a-ticket"); page.Code != http.StatusGone || !strings.Contains(page.Body.String(), "expired or was already used") {
		t.Fatalf("unknown: %d %s", page.Code, page.Body.String())
	}
	stale := c.ticket(t, c.ada.ID, "linear", oauth.ConnectPurposePerUser)
	page := e.confirmLink(t, stale)
	if _, err := e.db.Exec(`UPDATE connect_tickets SET expires_at = ?`, time.Now().Add(-time.Minute).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if rec := e.confirmLink(t, stale); rec.Code != http.StatusGone {
		t.Fatalf("expired page: %d", rec.Code)
	}
	if rec := e.continueLink(t, stale, page); rec.Code != http.StatusGone {
		t.Fatalf("expired continue: %d %s", rec.Code, rec.Body.String())
	}
	if got := e.auditReasons(t, connectLinkRefused); strings.Join(got, " ") != "expired" {
		t.Fatalf("link_refused reasons = %v (GETs must not audit)", got)
	}
	if e.auditRows(t, connectLinkUsed, "") != 0 {
		t.Fatal("a refused link was audited as used")
	}

	req := httptest.NewRequest(http.MethodPut, connectLinkPath+"x", nil)
	req.Header.Set("X-Requested-With", "toolyard")
	w := httptest.NewRecorder()
	e.handler.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT: %d", w.Code)
	}
	for _, m := range []string{http.MethodGet, http.MethodPost} {
		if _, ok := operatorScopeFor(m, connectLinkPath+"abc"); ok {
			t.Fatalf("an operator token may %s a connect link", m)
		}
		if !memberAllowed(m, connectLinkPath+"abc") {
			t.Fatalf("member allowlist refuses %s of one ticket", m)
		}
	}
	if memberAllowed(http.MethodPut, connectLinkPath+"abc") || memberAllowed(http.MethodGet, connectLinkPath) || memberAllowed(http.MethodGet, connectLinkPath+"a/b") {
		t.Fatal("member allowlist: PUT, an empty ticket and extra segments must not pass")
	}
}

// TestConnectLinkRechecksAccessAndSignInKind: a grant revoked after the
// link was minted refuses it; a server whose sign-in is a pasted token has
// no browser flow to start.
func TestConnectLinkRechecksAccessAndSignInKind(t *testing.T) {
	c := newConnectLinkEnv(t)
	e, ada := c.e, c.ada
	ctx := context.Background()

	ticket := c.ticket(t, ada.ID, "linear", oauth.ConnectPurposePerUser)
	page := e.confirmLink(t, ticket)
	if err := e.access.SetGroups(ctx, ada.ID, nil, e.admin.ID); err != nil {
		t.Fatal(err)
	}
	if rec := e.confirmLink(t, ticket); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "no longer has access") {
		t.Fatalf("revoked grant page: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.continueLink(t, ticket, page); rec.Code != http.StatusForbidden {
		t.Fatalf("revoked grant continue: %d", rec.Code)
	}

	addPatchServer(t, e, "patsrv", newPatchMCPServer(t))
	if err := c.oe.oa.PutClient(ctx, oauth.ClientRecord{
		UpstreamName: "patsrv", Issuer: "(personal-access-token)", AuthorizationEndpoint: "(pat)", TokenEndpoint: "(pat)",
		ClientID: "(pat)", RedirectURI: "(pat)", TokenEndpointAuthMethod: "none",
	}); err != nil {
		t.Fatal(err)
	}
	root := c.admin(t, "user_root", "root@beknown.work")
	if rec := e.confirmLink(t, c.ticket(t, root.ID, "patsrv", oauth.ConnectPurposeShared)); rec.Code != http.StatusGone || !strings.Contains(rec.Body.String(), "pasted token") {
		t.Fatalf("pat server: %d %s", rec.Code, rec.Body.String())
	}
	if got := e.auditReasons(t, connectLinkRefused); strings.Join(got, " ") != "not_granted" {
		t.Fatalf("link_refused reasons = %v", got)
	}
	if e.pendingFlows(t) != 0 {
		t.Fatal("pending flows left by refused links")
	}
}

// TestConnectLinksNotHandedToOperatorTokens: a tool run made with an
// operator token gets no connect link (the token is not the person's
// browser), while the same call from a dashboard session is not blocked
// by that rule.
func TestConnectLinksNotHandedToOperatorTokens(t *testing.T) {
	c := newConnectLinkEnv(t)
	e := c.e
	e.gw.SetConnect(c.oe.oa, e.id)
	tok, _, err := e.id.CreateOperatorToken(context.Background(), e.admin.ID, "cli", []string{identity.ScopeRead, identity.ScopeWrite}, 0, "test")
	if err != nil {
		t.Fatal(err)
	}
	body := `{"tool":"connections.link","arguments":{"server":"linear","_reason":"operator token asks for a connect link to prove none is handed out"}}`
	rec := e.bearer(t, tok, http.MethodPost, "/v1/tools/run", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("tools/run with operator token: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		IsError bool `json:"is_error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.IsError || !strings.Contains(rec.Body.String(), "operator token") || strings.Contains(rec.Body.String(), connectLinkPath) {
		t.Fatalf("operator token got: %s", rec.Body.String())
	}
	var tickets int
	_ = e.db.QueryRow(`SELECT count(*) FROM connect_tickets`).Scan(&tickets)
	if tickets != 0 {
		t.Fatalf("%d tickets minted for an operator token", tickets)
	}
	// From the dashboard the rule does not apply; this test gateway has
	// no public URL, which is the only thing stopping a link here.
	rec = e.do(t, e.cookieFor(t, e.admin.ID), http.MethodPost, "/v1/tools/run", body)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "operator token") || !strings.Contains(rec.Body.String(), "no public URL") {
		t.Fatalf("dashboard tools/run: %d %s", rec.Code, rec.Body.String())
	}
}

// TestConnectLinkMismatchedAccountStoresNothing: the opener signed in to
// the provider as somebody else. No token is stored, the flow is burnt,
// and the page names neither account (the opener may not be the person
// the link was made for).
func TestConnectLinkMismatchedAccountStoresNothing(t *testing.T) {
	c := newConnectLinkEnv(t)
	e, ada := c.e, c.ada
	rec := e.followLink(t, c.ticket(t, ada.ID, "linear", oauth.ConnectPurposePerUser))
	state := redirectState(t, rec, c.oe.idp)
	cookie := flowCookieFrom(t, rec)

	cb := e.callbackCookies(t, state, "as:bob@beknown.work", cookie)
	if cb.Code != http.StatusBadRequest {
		t.Fatalf("callback: %d %s", cb.Code, cb.Body.String())
	}
	body := cb.Body.String()
	if !strings.Contains(body, "You signed in with a different account than this link is for") || strings.Contains(body, "ada@") || strings.Contains(body, "bob@") {
		t.Fatalf("mismatch page must name neither account: %s", body)
	}
	if rows := userTokenRows(t, e, "linear"); len(rows) != 0 {
		t.Fatalf("token stored after a mismatch: %v", rows)
	}
	if e.auditRows(t, connectAccountMismatch, "") != 1 || e.auditRows(t, "oauth.user_success", "") != 0 || e.auditMentions(t, "bob@beknown.work") != 0 {
		t.Fatal("mismatch audit: wrong rows, or the other account's email was written")
	}
	if _, err := c.oe.oa.LoadPending(context.Background(), state); !errors.Is(err, oauth.ErrPendingNotFound) {
		t.Fatalf("pending flow survives a mismatch: %v", err)
	}
	if cb := e.callbackCookies(t, state, "as:ada@beknown.work", cookie); cb.Code != http.StatusBadRequest {
		t.Fatalf("replay after mismatch: %d", cb.Code)
	}
}

// TestConnectLinkUnverifiedAccountNeedsOpenerSession: when the provider's
// account cannot be checked (no email, email_verified false, a non-ASCII
// label), nothing is stored unless the browser that pressed Continue held
// the person's own toolyard session; then it is stored and audited as
// unverified.
func TestConnectLinkUnverifiedAccountNeedsOpenerSession(t *testing.T) {
	c := newConnectLinkEnv(t)
	e, ada := c.e, c.ada

	for _, code := range []string{"noemail", "unverified:ada@beknown.work", "as:\u00e4da@beknown.work"} {
		rec := e.followLink(t, c.ticket(t, ada.ID, "linear", oauth.ConnectPurposePerUser))
		state := redirectState(t, rec, c.oe.idp)
		cb := e.callbackCookies(t, state, code, flowCookieFrom(t, rec))
		if cb.Code != http.StatusForbidden || !strings.Contains(cb.Body.String(), "can't be verified automatically") || !strings.Contains(cb.Body.String(), "Sign in to toolyard") {
			t.Fatalf("%s without a session: %d %s", code, cb.Code, cb.Body.String())
		}
		if rows := userTokenRows(t, e, "linear"); len(rows) != 0 {
			t.Fatalf("%s: token stored without a session: %v", code, rows)
		}
		if _, err := c.oe.oa.LoadPending(context.Background(), state); !errors.Is(err, oauth.ErrPendingNotFound) {
			t.Fatalf("%s: pending flow survives: %v", code, err)
		}
	}
	var decision string
	_ = e.db.QueryRow(`SELECT COALESCE(decision,'') FROM audit_events WHERE event_type = ? LIMIT 1`, connectAccountMismatch).Scan(&decision)
	if e.auditRows(t, connectAccountMismatch, "") != 3 || decision != "unverifiable" {
		t.Fatalf("unverifiable refusals audited %d times, decision %q", e.auditRows(t, connectAccountMismatch, ""), decision)
	}

	// Continue pressed in a browser signed in to toolyard as ada: the
	// callback (no session, as cross-site) stores it and says so.
	rec := e.followLink(t, c.ticket(t, ada.ID, "linear", oauth.ConnectPurposePerUser), e.cookieFor(t, ada.ID))
	state := redirectState(t, rec, c.oe.idp)
	if p, err := c.oe.oa.LoadPending(context.Background(), state); err != nil || !p.OpenerVerified {
		t.Fatalf("pending after a session Continue = %+v %v", p, err)
	}
	cb := e.callbackCookies(t, state, "noemail", flowCookieFrom(t, rec))
	if cb.Code != http.StatusOK || !strings.Contains(cb.Body.String(), "tell your agent to retry") {
		t.Fatalf("callback with verified opener: %d %s", cb.Code, cb.Body.String())
	}
	if rows := userTokenRows(t, e, "linear"); rows[ada.ID] != "active" {
		t.Fatalf("token rows = %v", rows)
	}
	_ = e.db.QueryRow(`SELECT COALESCE(decision,'') FROM audit_events WHERE event_type = ?`, connectUnverifiedAccount).Scan(&decision)
	if decision != "unverified_account" {
		t.Fatalf("unverified_account audit decision = %q", decision)
	}
}

// TestConnectLinkDashboardFlowKeepsItsPage: a per-user flow the dashboard
// started is not a ticket flow: no account rule, the My connections page.
func TestConnectLinkDashboardFlowKeepsItsPage(t *testing.T) {
	c := newConnectLinkEnv(t)
	e, ada := c.e, c.ada
	state, cookie := beginFor(t, e, e.cookieFor(t, ada.ID), "linear")
	cb := e.callbackCookies(t, state, "noemail", cookie)
	if cb.Code != http.StatusOK || !strings.Contains(cb.Body.String(), "My connections") || strings.Contains(cb.Body.String(), "T3 Code") {
		t.Fatalf("dashboard callback: %d %s", cb.Code, cb.Body.String())
	}
	if rows := userTokenRows(t, e, "linear"); rows[ada.ID] != "active" {
		t.Fatalf("token rows = %v", rows)
	}
	if e.auditRows(t, connectUnverifiedAccount, "") != 0 || e.auditRows(t, connectAccountMismatch, "") != 0 {
		t.Fatal("a dashboard flow was put through the ticket account rule")
	}
}

// TestConnectLinkSharedNeedsAdminSession: a shared-account link signs the
// server's shared account in only from a browser signed in to toolyard as
// an admin (the flow is then that admin's); it is never honoured once the
// shared account works, and a sign-in from a link cannot move the shared
// account to a different one than the one on file.
func TestConnectLinkSharedNeedsAdminSession(t *testing.T) {
	c := newConnectLinkEnv(t)
	e, ada := c.e, c.ada
	ctx := context.Background()
	addPatchServer(t, e, "gh", newPatchMCPServer(t))
	c.oe.client(t, "gh")
	root := c.admin(t, "user_root", "root@beknown.work")
	rootCookie := e.cookieFor(t, root.ID)
	if err := e.access.SetGroups(ctx, ada.ID, []string{"linear", "gh"}, e.admin.ID); err != nil {
		t.Fatal(err)
	}

	// Nobody signed in to toolyard in this browser: the page still shows
	// (a cross-site click never carries the Strict session cookie), but
	// Continue is refused with the sign-in hint and the ticket stays live.
	ticket := c.ticket(t, root.ID, "gh", oauth.ConnectPurposeShared)
	page := e.confirmLink(t, ticket)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "signed in to toolyard as an admin") || !strings.Contains(page.Body.String(), `href="/`) {
		t.Fatalf("shared confirm without session: %d %s", page.Code, page.Body.String())
	}
	rec := e.continueLink(t, ticket, page)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "Sign in to toolyard as an admin to continue") || !strings.Contains(rec.Body.String(), "Sign in to toolyard</a>") {
		t.Fatalf("shared continue without session: %d %s", rec.Code, rec.Body.String())
	}
	if !c.live(t, ticket) || e.pendingFlows(t) != 0 {
		t.Fatal("a refused shared Continue redeemed the ticket or started a flow")
	}
	// A member's session is not enough, on the page or on Continue.
	adaCookie := e.cookieFor(t, ada.ID)
	if page := e.confirmLink(t, ticket, adaCookie); page.Code != http.StatusForbidden || !strings.Contains(page.Body.String(), "Sign in to toolyard as an admin") {
		t.Fatalf("shared confirm with member session: %d %s", page.Code, page.Body.String())
	}
	if rec := e.continueLink(t, ticket, e.confirmLink(t, ticket), adaCookie); rec.Code != http.StatusForbidden {
		t.Fatalf("shared continue with member session: %d", rec.Code)
	}
	if got := e.auditReasons(t, connectLinkRefused); strings.Join(got, " ") != "admin_session admin_session" {
		t.Fatalf("link_refused reasons = %v", got)
	}

	// An admin's session: the flow is the admin's and the shared account
	// is signed in.
	rec = e.followLink(t, ticket, rootCookie)
	state := redirectState(t, rec, c.oe.idp)
	if !e.srv.oauthFlowCookieValid(&http.Request{Header: http.Header{"Cookie": {flowCookieFrom(t, rec).String()}}}, state, root.ID) {
		t.Fatal("shared flow not bound to the admin who pressed Continue")
	}
	cb := e.callbackCookies(t, state, "ops", flowCookieFrom(t, rec))
	if cb.Code != http.StatusOK || !strings.Contains(cb.Body.String(), "tell your agent to retry") {
		t.Fatalf("shared callback: %d %s", cb.Code, cb.Body.String())
	}
	var st, label string
	if err := e.db.QueryRow(`SELECT state, COALESCE(account_label,'') FROM oauth_tokens WHERE upstream_name = 'gh'`).Scan(&st, &label); err != nil || st != "active" || label != "ops@example.test" {
		t.Fatalf("shared token = %q %q %v", st, label, err)
	}

	// The shared account works: a link does nothing, even for an admin.
	working := c.ticket(t, root.ID, "gh", oauth.ConnectPurposeShared)
	if rec := e.followLink(t, working, rootCookie); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "already signed in as ops@example.test") {
		t.Fatalf("link for a working shared account: %d %s", rec.Code, rec.Body.String())
	}
	if !c.live(t, working) {
		t.Fatal("refused link was redeemed")
	}

	// The sign-in died: a link may renew it, but only as the same account.
	c.oe.oa.MarkReauthExternal(ctx, "gh", "revoked")
	rec = e.followLink(t, c.ticket(t, root.ID, "gh", oauth.ConnectPurposeShared), rootCookie)
	state = redirectState(t, rec, c.oe.idp)
	cb = e.callbackCookies(t, state, "intruder", flowCookieFrom(t, rec))
	if cb.Code != http.StatusForbidden || !strings.Contains(cb.Body.String(), "signed in as <strong>ops@example.test</strong>") || !strings.Contains(cb.Body.String(), "nothing changed") {
		t.Fatalf("different shared account: %d %s", cb.Code, cb.Body.String())
	}
	if err := e.db.QueryRow(`SELECT state, COALESCE(account_label,'') FROM oauth_tokens WHERE upstream_name = 'gh'`).Scan(&st, &label); err != nil || st != "needs_reauth" || label != "ops@example.test" {
		t.Fatalf("shared token after refused switch = %q %q %v, want the old row untouched", st, label, err)
	}
	rec = e.followLink(t, c.ticket(t, root.ID, "gh", oauth.ConnectPurposeShared), rootCookie)
	state = redirectState(t, rec, c.oe.idp)
	if cb := e.callbackCookies(t, state, "ops", flowCookieFrom(t, rec)); cb.Code != http.StatusOK {
		t.Fatalf("same shared account: %d %s", cb.Code, cb.Body.String())
	}
	if err := e.db.QueryRow(`SELECT state FROM oauth_tokens WHERE upstream_name = 'gh'`).Scan(&st); err != nil || st != "active" {
		t.Fatalf("shared token after renewal = %q %v", st, err)
	}

	// A shared ticket minted for a member, and links whose purpose no
	// longer fits the server, are refused whoever opens them.
	c.oe.oa.MarkReauthExternal(ctx, "gh", "revoked again")
	memberTicket := c.ticket(t, ada.ID, "gh", oauth.ConnectPurposeShared)
	if page := e.confirmLink(t, memberTicket, rootCookie); page.Code != http.StatusForbidden || !strings.Contains(page.Body.String(), "Only an admin") {
		t.Fatalf("member's shared ticket page: %d %s", page.Code, page.Body.String())
	}
	if rec := e.postLink(t, memberTicket, url.Values{"nonce": {"x"}}, "same-origin"); rec.Code != http.StatusForbidden || !c.live(t, memberTicket) {
		t.Fatalf("member's shared ticket continue: %d live=%v", rec.Code, c.live(t, memberTicket))
	}
	if page := e.confirmLink(t, c.ticket(t, ada.ID, "gh", oauth.ConnectPurposePerUser)); page.Code != http.StatusGone || !strings.Contains(page.Body.String(), "changed since the link was made") {
		t.Fatalf("mode mismatch: %d %s", page.Code, page.Body.String())
	}
	if page := e.confirmLink(t, c.ticket(t, root.ID, "linear", oauth.ConnectPurposeShared)); page.Code != http.StatusGone {
		t.Fatalf("shared link for a per_user server: %d", page.Code)
	}
}

// TestConnectLinkWithMemberSession: a member whose browser also holds a
// toolyard session passes RoleGuard on both requests and gets the same
// redirect; another person's session is refused on the page and on
// Continue, and cannot finish the flow either.
func TestConnectLinkWithMemberSession(t *testing.T) {
	c := newConnectLinkEnv(t)
	e, ada := c.e, c.ada
	session := e.cookieFor(t, ada.ID)
	rec := e.followLink(t, c.ticket(t, ada.ID, "linear", oauth.ConnectPurposePerUser), session)
	state := redirectState(t, rec, c.oe.idp)
	cb := e.callbackCookies(t, state, "as:ada@beknown.work", session)
	if cb.Code != http.StatusOK || !strings.Contains(cb.Body.String(), "tell your agent to retry") {
		t.Fatalf("callback with session: %d %s", cb.Code, cb.Body.String())
	}
	if rows := userTokenRows(t, e, "linear"); rows[ada.ID] != "active" {
		t.Fatalf("token rows = %v", rows)
	}

	bob := e.member(t, "user_bob", "bob@beknown.work")
	bobCookie := e.cookieFor(t, bob.ID)
	ticket := c.ticket(t, ada.ID, "linear", oauth.ConnectPurposePerUser)
	if page := e.confirmLink(t, ticket, bobCookie); page.Code != http.StatusForbidden || !strings.Contains(page.Body.String(), "for someone else") {
		t.Fatalf("confirm with another user's session: %d %s", page.Code, page.Body.String())
	}
	if rec := e.continueLink(t, ticket, e.confirmLink(t, ticket), bobCookie); rec.Code != http.StatusForbidden {
		t.Fatalf("continue with another user's session: %d", rec.Code)
	}
	if !c.live(t, ticket) {
		t.Fatal("another user's Continue redeemed the ticket")
	}
	rec = e.followLink(t, ticket)
	state = redirectState(t, rec, c.oe.idp)
	if cb := e.callbackCookies(t, state, "as:ada@beknown.work", bobCookie); cb.Code != http.StatusBadRequest {
		t.Fatalf("another user's session finished the flow: %d", cb.Code)
	}
	if rows := userTokenRows(t, e, "linear"); len(rows) != 1 {
		t.Fatalf("token rows = %v", rows)
	}
}

// TestConnectAccountCheck: the account rule compares ASCII-lowercased
// emails, and cannot run on a missing or unvouched-for email or anything
// non-ASCII.
func TestConnectAccountCheck(t *testing.T) {
	ada := &identity.User{Email: "Ada@Beknown.work"}
	cases := []struct {
		label    string
		verified bool
		user     *identity.User
		want     string
	}{
		{"ada@beknown.work", true, ada, accountMatch},
		{"ADA@BEKNOWN.WORK", true, ada, accountMatch},
		{" ada@beknown.work ", true, ada, accountMatch},
		{"bob@beknown.work", true, ada, accountMismatch},
		{"ada@beknown.work", false, ada, accountUnverifiable},
		{"", true, ada, accountUnverifiable},
		{"ada@beknown.work", true, &identity.User{}, accountUnverifiable},
		{"\u00e4da@beknown.work", true, ada, accountUnverifiable},
		{"ada@beknown.work", true, &identity.User{Email: "\u00e4da@beknown.work"}, accountUnverifiable},
	}
	for _, c := range cases {
		got := connectAccountCheck(c.user, &oauth.UserTokenRecord{AccountLabel: c.label, AccountVerified: c.verified})
		if got != c.want {
			t.Errorf("check(%q, verified=%v, user %q) = %s, want %s", c.label, c.verified, c.user.Email, got, c.want)
		}
	}
	if same, ok := sameEmail("Ops@Example.test", "ops@example.test"); !same || !ok {
		t.Fatal("sameEmail should match case-insensitively")
	}
	if _, ok := sameEmail("", "ops@example.test"); ok {
		t.Fatal("sameEmail should not compare an empty label")
	}
}
