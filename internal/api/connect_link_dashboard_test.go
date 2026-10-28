package api

import (
	"context"
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/gateway"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/oauth"
	"github.com/tusharbhardwaj/toolyard/internal/upstreams"
)

// "Copy link for another browser": every sign-in the dashboard starts
// stops first, and the begin routes take {"mode":"link"} to mint the
// one-time connect link a person opens in another browser profile. These
// tests ask for the link the way the dashboard does (session cookie, CSRF
// header, JSON), then open it the way the other browser does (no CSRF
// header, its own cookies only) and finish the flow there.

// dashboardLink reads a begin answer in link mode: 200, never cached, no
// flow started or bound in this browser, a ten-minute link under base. It
// returns the ticket.
func dashboardLink(t *testing.T, rec *httptest.ResponseRecorder, base string) string {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("link mode: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Mode         string `json:"mode"`
		Link         string `json:"connect_link"`
		ExpiresIn    int    `json:"expires_in"`
		AuthorizeURL string `json:"authorize_url"`
		State        string `json:"state"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("link mode body: %v %s", err, rec.Body.String())
	}
	if out.Mode != connectModeLink || out.ExpiresIn != int(oauth.ConnectTicketTTL.Seconds()) || out.ExpiresIn != 600 ||
		out.AuthorizeURL != "" || out.State != "" || !strings.HasPrefix(out.Link, base+connectLinkPath) {
		t.Fatalf("link mode body = %s (want a link under %s)", rec.Body.String(), base)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("link answer Cache-Control = %q", cc)
	}
	for _, c := range rec.Result().Cookies() {
		if strings.Contains(c.Name, oauthFlowCookiePrefix) {
			t.Fatalf("link mode bound a flow to the dashboard's browser: %+v", c)
		}
	}
	ticket := strings.TrimPrefix(out.Link, base+connectLinkPath)
	if connectTicketFromPath(connectLinkPath+ticket) == "" {
		t.Fatalf("no ticket in %q", out.Link)
	}
	return ticket
}

func (e *accessTestEnv) connectTickets(t *testing.T) int {
	t.Helper()
	var n int
	if err := e.db.QueryRow(`SELECT count(*) FROM connect_tickets`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// issuedBy returns the agent and owner on the newest link_issued row.
func (e *accessTestEnv) issuedBy(t *testing.T) (agent, owner string) {
	t.Helper()
	_ = e.db.QueryRow(`SELECT COALESCE(agent_id,''), COALESCE(owner_user_id,'') FROM audit_events WHERE event_type = ? ORDER BY ts DESC LIMIT 1`,
		gateway.EventConnectLinkIssued).Scan(&agent, &owner)
	return agent, owner
}

// TestDashboardLinkOwnAccount: the person's own link is minted for them
// (recorded as theirs, not an agent's), starts nothing in the dashboard's
// browser, and finishes in another browser with no toolyard session when
// the provider account is the person's email; another account is refused,
// a browser signed in as somebody else cannot use it, and a link works once
// within ten minutes.
func TestDashboardLinkOwnAccount(t *testing.T) {
	c := newConnectLinkEnv(t)
	e, ada := c.e, c.ada
	ctx := context.Background()
	e.srv.security.PublicURL = "https://toolyard.example/"
	adaCookie := e.cookieFor(t, ada.ID)
	const base = "https://toolyard.example"

	ticket := dashboardLink(t, e.do(t, adaCookie, http.MethodPost, "/v1/me/connections/linear/begin", `{"mode":"link"}`), base)
	if e.pendingFlows(t) != 0 {
		t.Fatal("link mode started a flow in the dashboard's browser")
	}
	tk, err := c.oe.oa.PeekConnectTicket(ctx, ticket)
	if err != nil || tk.UserID != ada.ID || tk.Upstream != "linear" || tk.Purpose != oauth.ConnectPurposePerUser ||
		tk.AgentID != oauth.ConnectMintedBy(ada.ID) || tk.Kind != oauth.ConnectKindSignIn || !tk.MintedInDashboard() {
		t.Fatalf("ticket = %+v %v", tk, err)
	}
	if ttl := time.Until(tk.ExpiresAt); ttl > oauth.ConnectTicketTTL || ttl < oauth.ConnectTicketTTL-time.Minute {
		t.Fatalf("ticket lives %s, want %s", ttl, oauth.ConnectTicketTTL)
	}
	if agent, owner := e.issuedBy(t); agent != "user:"+ada.ID || owner != ada.ID || e.auditRows(t, gateway.EventConnectLinkIssued, "") != 1 {
		t.Fatalf("link_issued names agent %q owner %q (%d rows)", agent, owner, e.auditRows(t, gateway.EventConnectLinkIssued, ""))
	}
	if e.auditMentions(t, ticket) != 0 {
		t.Fatal("the ticket reached the audit log")
	}

	// The other browser: no toolyard session there. The page says which
	// account to use and where the result shows.
	page := e.confirmLink(t, ticket)
	body := page.Body.String()
	if page.Code != http.StatusOK || !strings.Contains(body, "the account for <strong>"+ada.Email+"</strong>") ||
		!strings.Contains(body, "another email is refused") || !strings.Contains(body, "dashboard then shows the connection") || strings.Contains(body, "tell your agent") {
		t.Fatalf("confirm page of a dashboard link: %d %s", page.Code, body)
	}
	rec := e.continueLink(t, ticket, page)
	state := redirectState(t, rec, c.oe.idp)
	cookie := flowCookieFrom(t, rec)
	if !e.srv.oauthFlowCookieValid(&http.Request{Header: http.Header{"Cookie": {cookie.String()}}}, state, ada.ID) {
		t.Fatal("the flow is not bound to the browser that opened the link, for ada")
	}
	cb := e.callbackCookies(t, state, "as:"+strings.ToUpper(ada.Email), cookie)
	if cb.Code != http.StatusOK || !strings.Contains(cb.Body.String(), "Copied this link from the toolyard dashboard") {
		t.Fatalf("callback in the other browser: %d %s", cb.Code, cb.Body.String())
	}
	if rows := userTokenRows(t, e, "linear"); rows[ada.ID] != "active" || len(rows) != 1 {
		t.Fatalf("token rows = %v", rows)
	}
	if page := e.confirmLink(t, ticket); page.Code != http.StatusGone || !strings.Contains(page.Body.String(), "copy a new link in the toolyard dashboard") {
		t.Fatalf("second use: %d %s", page.Code, page.Body.String())
	}

	// Already connected: a new link is the person's own request to sign in
	// again (as Reconnect is), and the email rule still holds there.
	again := dashboardLink(t, e.do(t, adaCookie, http.MethodPost, "/v1/me/connections/linear/begin", `{"mode":"link"}`), base)
	rec = e.followLink(t, again)
	state = redirectState(t, rec, c.oe.idp)
	cb = e.callbackCookies(t, state, "as:someone@example.com", flowCookieFrom(t, rec))
	if cb.Code != http.StatusBadRequest || !strings.Contains(cb.Body.String(), "different account than this link is for") {
		t.Fatalf("another account: %d %s", cb.Code, cb.Body.String())
	}
	var label string
	_ = e.db.QueryRow(`SELECT account_label FROM oauth_user_tokens WHERE upstream_name = 'linear' AND user_id = ?`, ada.ID).Scan(&label)
	if !strings.EqualFold(label, ada.Email) {
		t.Fatalf("account_label after a refused account = %q", label)
	}

	// A browser signed in to toolyard as somebody else cannot use it.
	bob := e.member(t, "user_bob", "bob@example.com")
	other := dashboardLink(t, e.do(t, adaCookie, http.MethodPost, "/v1/me/connections/linear/begin", `{"mode":"link"}`), base)
	bobCookie := e.cookieFor(t, bob.ID)
	if page := e.confirmLink(t, other, bobCookie); page.Code != http.StatusForbidden || !strings.Contains(page.Body.String(), "for someone else") {
		t.Fatalf("another person's browser: %d %s", page.Code, page.Body.String())
	}
	if rec := e.continueLink(t, other, e.confirmLink(t, other), bobCookie); rec.Code != http.StatusForbidden || !c.live(t, other) {
		t.Fatalf("another person's Continue: %d live=%v", rec.Code, c.live(t, other))
	}

	// Ten minutes: an expired link is dead on the page and on Continue.
	stale := e.confirmLink(t, other)
	if _, err := e.db.Exec(`UPDATE connect_tickets SET expires_at = ?`, time.Now().Add(-time.Second).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if page := e.confirmLink(t, other); page.Code != http.StatusGone {
		t.Fatalf("expired page: %d", page.Code)
	}
	if rec := e.continueLink(t, other, stale); rec.Code != http.StatusGone {
		t.Fatalf("expired continue: %d %s", rec.Code, rec.Body.String())
	}
}

// TestDashboardLinkOwnAccountRefusals: link mode runs begin's checks (a
// session, a granted per_user server with a browser sign-in, no operator
// token), mints nothing when they fail, and stops at the live-link cap.
func TestDashboardLinkOwnAccountRefusals(t *testing.T) {
	c := newConnectLinkEnv(t)
	e, ada := c.e, c.ada
	addPatchServer(t, e, "shared", newPatchMCPServer(t))
	c.oe.client(t, "shared")
	addPerUserServer(t, e, "other", newPatchMCPServer(t))
	addPerUserServer(t, e, "patsrv", newPatchMCPServer(t))
	if err := c.oe.oa.PutClient(context.Background(), oauth.ClientRecord{
		UpstreamName: "patsrv", Issuer: "(personal-access-token)", AuthorizationEndpoint: "(pat)", TokenEndpoint: "(pat)",
		ClientID: "(pat)", RedirectURI: "(pat)", TokenEndpointAuthMethod: "none",
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.access.SetGroups(context.Background(), ada.ID, []string{"linear", "shared", "other", "patsrv"}, e.admin.ID); err != nil {
		t.Fatal(err)
	}
	adaCookie := e.cookieFor(t, ada.ID)
	bob := e.member(t, "user_bob", "bob@example.com")
	link := `{"mode":"link"}`

	cases := []struct {
		name   string
		cookie *http.Cookie
		path   string
		body   string
		want   int
	}{
		{"no session", nil, "/v1/me/connections/linear/begin", link, http.StatusUnauthorized},
		{"not granted", e.cookieFor(t, bob.ID), "/v1/me/connections/linear/begin", link, http.StatusNotFound},
		{"unknown server", adaCookie, "/v1/me/connections/nope/begin", link, http.StatusNotFound},
		{"shared server", adaCookie, "/v1/me/connections/shared/begin", link, http.StatusBadRequest},
		{"no client yet", adaCookie, "/v1/me/connections/other/begin", link, http.StatusConflict},
		{"pasted token", adaCookie, "/v1/me/connections/patsrv/begin", link, http.StatusConflict},
		{"bad mode", adaCookie, "/v1/me/connections/linear/begin", `{"mode":"paste"}`, http.StatusBadRequest},
		{"unknown field", adaCookie, "/v1/me/connections/linear/begin", `{"mode":"link","user":"someone"}`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		if rec := e.do(t, tc.cookie, http.MethodPost, tc.path, tc.body); rec.Code != tc.want {
			t.Errorf("%s: %d %s, want %d", tc.name, rec.Code, rec.Body.String(), tc.want)
		}
	}
	tok, _, err := e.id.CreateOperatorToken(context.Background(), e.admin.ID, "cli", []string{identity.ScopeRead, identity.ScopeWrite}, 0, "test")
	if err != nil {
		t.Fatal(err)
	}
	if rec := e.bearer(t, tok, http.MethodPost, "/v1/me/connections/linear/begin", link); rec.Code != http.StatusConflict && rec.Code != http.StatusForbidden {
		t.Fatalf("operator token: %d %s", rec.Code, rec.Body.String())
	}
	if n := e.connectTickets(t); n != 0 || e.auditRows(t, gateway.EventConnectLinkIssued, "") != 0 {
		t.Fatalf("refused link requests minted %d tickets", n)
	}

	// The live-link cap is the agent links' cap.
	for i := 0; i < oauth.MaxLiveConnectTickets; i++ {
		if rec := e.do(t, adaCookie, http.MethodPost, "/v1/me/connections/linear/begin", link); rec.Code != http.StatusOK {
			t.Fatalf("link %d: %d %s", i, rec.Code, rec.Body.String())
		}
	}
	if rec := e.do(t, adaCookie, http.MethodPost, "/v1/me/connections/linear/begin", link); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("link over the cap: %d %s", rec.Code, rec.Body.String())
	}
	// The dashboard's own flow is unchanged and still binds this browser.
	if state, cookie := beginFor(t, e, adaCookie, "linear"); state == "" || cookie == nil {
		t.Fatal("begin without a mode no longer starts the flow here")
	}
}

// TestDashboardLinkSharedAccount: an admin's shared-account link is minted
// for them, points at the request's own host without a public URL, and
// needs an admin's toolyard session in the browser that continues it. It
// is refused for a member, a per_user server, a pasted token, an unknown
// server, and a shared account that works (a link never replaces it); after
// Reauthorize a link may renew it.
func TestDashboardLinkSharedAccount(t *testing.T) {
	c := newConnectLinkEnv(t)
	e, ada := c.e, c.ada
	ctx := context.Background()
	addPatchServer(t, e, "gh", newPatchMCPServer(t))
	c.oe.client(t, "gh")
	addPatchServer(t, e, "patsrv", newPatchMCPServer(t))
	if err := c.oe.oa.PutClient(ctx, oauth.ClientRecord{
		UpstreamName: "patsrv", Issuer: "(personal-access-token)", AuthorizationEndpoint: "(pat)", TokenEndpoint: "(pat)",
		ClientID: "(pat)", RedirectURI: "(pat)", TokenEndpointAuthMethod: "none",
	}); err != nil {
		t.Fatal(err)
	}
	root := c.admin(t, "user_root", "root@example.com")
	rootCookie := e.cookieFor(t, root.ID)
	link := `{"mode":"link"}`

	if rec := e.do(t, e.cookieFor(t, ada.ID), http.MethodPost, "/v1/servers/gh/oauth/begin", link); rec.Code != http.StatusForbidden {
		t.Fatalf("member: %d %s", rec.Code, rec.Body.String())
	}
	for path, want := range map[string]int{
		"/v1/servers/linear/oauth/begin": http.StatusBadRequest,
		"/v1/servers/patsrv/oauth/begin": http.StatusConflict,
		"/v1/servers/nope/oauth/begin":   http.StatusNotFound,
	} {
		if rec := e.do(t, rootCookie, http.MethodPost, path, link); rec.Code != want {
			t.Errorf("%s: %d %s, want %d", path, rec.Code, rec.Body.String(), want)
		}
	}
	if n := e.connectTickets(t); n != 0 {
		t.Fatalf("refused shared links minted %d tickets", n)
	}

	// No public URL: the link uses the host the dashboard was reached on.
	ticket := dashboardLink(t, e.do(t, rootCookie, http.MethodPost, "/v1/servers/gh/oauth/begin", link), "http://example.com")
	tk, err := c.oe.oa.PeekConnectTicket(ctx, ticket)
	if err != nil || tk.UserID != root.ID || tk.Purpose != oauth.ConnectPurposeShared || tk.AgentID != oauth.ConnectMintedBy(root.ID) {
		t.Fatalf("shared ticket = %+v %v", tk, err)
	}
	if agent, owner := e.issuedBy(t); agent != "user:"+root.ID || owner != root.ID {
		t.Fatalf("link_issued names agent %q owner %q", agent, owner)
	}
	if e.pendingFlows(t) != 0 {
		t.Fatal("link mode started a flow in the dashboard's browser")
	}

	// The other browser without a toolyard session: refused, still live.
	page := e.confirmLink(t, ticket)
	if rec := e.continueLink(t, ticket, page); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "as an admin") || !c.live(t, ticket) {
		t.Fatalf("shared continue without a session: %d live=%v", rec.Code, c.live(t, ticket))
	}
	// With an admin's session there: bound to that browser, signed in.
	rec := e.continueLink(t, ticket, page, e.cookieFor(t, root.ID))
	state := redirectState(t, rec, c.oe.idp)
	if cb := e.callbackCookies(t, state, "ops", flowCookieFrom(t, rec)); cb.Code != http.StatusOK {
		t.Fatalf("shared callback: %d %s", cb.Code, cb.Body.String())
	}
	var st string
	if err := e.db.QueryRow(`SELECT state FROM oauth_tokens WHERE upstream_name = 'gh'`).Scan(&st); err != nil || st != "active" {
		t.Fatalf("shared token = %q %v", st, err)
	}

	// It works now: no link replaces it.
	before := e.connectTickets(t)
	if rec := e.do(t, rootCookie, http.MethodPost, "/v1/servers/gh/oauth/begin", link); rec.Code != http.StatusConflict ||
		!strings.Contains(rec.Body.String(), "already signed in as ops@example.test") || !strings.Contains(rec.Body.String(), "Continue in this browser") {
		t.Fatalf("link for a working shared account: %d %s", rec.Code, rec.Body.String())
	}
	if e.connectTickets(t) != before {
		t.Fatal("a refused shared link minted a ticket")
	}
	// After Reauthorize the sign-in needs renewing: a link may do it.
	c.oe.oa.MarkReauthExternal(ctx, "gh", "operator-initiated reauth")
	dashboardLink(t, e.do(t, rootCookie, http.MethodPost, "/v1/servers/gh/oauth/begin", link), "http://example.com")
	// The other modes are untouched.
	if rec := e.do(t, rootCookie, http.MethodPost, "/v1/servers/gh/oauth/begin", `{"mode":"callback"}`); rec.Code != http.StatusOK {
		t.Fatalf("callback mode: %d %s", rec.Code, rec.Body.String())
	}
}

var (
	githubFormActionRE = regexp.MustCompile(`<form method="post" action="(https://github\.com/settings/apps/new\?state=[^"]+)">`)
	githubManifestRE   = regexp.MustCompile(`name="manifest" value="([^"]*)"`)
)

// TestGitHubAppSetupLinkAcrossBrowsers: "Copy link" for Register GitHub
// App mints a one-time link of its own kind. Opening it changes nothing;
// Continue needs the page's nonce and an admin's toolyard session in that
// browser, then binds the setup to that browser and shows GitHub's
// manifest form behind a button (nothing submits on its own). GitHub's
// return succeeds in that browser only, and the link works once.
func TestGitHubAppSetupLinkAcrossBrowsers(t *testing.T) {
	e := newAccessTestServer(t)
	o := withOAuth(t, e)
	ctx := context.Background()
	if _, err := e.srv.upstreams.Add(ctx, upstreams.Server{Name: "GitHubForUsers", Transport: "github", URL: "https://api.github.com", AuthMode: upstreams.AuthPerUser}); err != nil {
		t.Fatal(err)
	}
	posts := 0
	e.srv.githubHTTP = &http.Client{Transport: githubSetupTransport(func(r *http.Request) (*http.Response, error) {
		posts++
		if r.URL.String() != "https://api.github.com/app-manifests/code/conversions" || r.Method != "POST" {
			t.Fatalf("wrong conversion: %s", r.URL)
		}
		return &http.Response{StatusCode: 201, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"client_id":"Iv1.test","client_secret":"secret-value","slug":"toolyard-test"}`))}, nil
	})}
	e.srv.security.PublicURL = "https://toolyard.example"
	adminCookie := e.cookieFor(t, e.admin.ID)
	member := e.member(t, "user_ada", "ada@example.com")

	if rec := e.do(t, e.cookieFor(t, member.ID), http.MethodPost, "/v1/github/setup/begin", `{"server":"GitHubForUsers","mode":"link"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("member: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do(t, adminCookie, http.MethodPost, "/v1/github/setup/begin", `{"server":"GitHubForUsers","mode":"paste"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad mode: %d %s", rec.Code, rec.Body.String())
	}
	ticket := dashboardLink(t, e.do(t, adminCookie, http.MethodPost, "/v1/github/setup/begin", `{"server":"GitHubForUsers","mode":"link"}`), "https://toolyard.example")
	tk, err := o.oa.PeekConnectTicket(ctx, ticket)
	if err != nil || tk.Kind != oauth.ConnectKindGitHubAppSetup || tk.UserID != e.admin.ID || tk.AgentID != oauth.ConnectMintedBy(e.admin.ID) {
		t.Fatalf("setup ticket = %+v %v", tk, err)
	}

	// Opening it: a confirm page, the nonce cookie, nothing else.
	page := e.confirmLink(t, ticket)
	body := page.Body.String()
	if page.Code != http.StatusOK || !strings.Contains(body, "Register the GitHub App for GitHubForUsers") ||
		!strings.Contains(body, `<form method="post" action="`+connectLinkPath+ticket+`"`) || strings.Contains(body, "github.com/settings") {
		t.Fatalf("setup confirm page: %d %s", page.Code, body)
	}
	nonceCookie, nonce := nonceOf(t, page)
	if nonceCookie.SameSite != http.SameSiteStrictMode || !nonceCookie.HttpOnly {
		t.Fatalf("nonce cookie = %+v", nonceCookie)
	}
	for _, ck := range page.Result().Cookies() {
		if strings.Contains(ck.Name, oauthFlowCookiePrefix) {
			t.Fatal("the confirm page bound a setup to the browser")
		}
	}
	if ready, _ := o.oa.HasClient(ctx, "GitHubForUsers"); ready || e.auditRows(t, connectLinkUsed, "") != 0 || e.auditRows(t, connectLinkRefused, "") != 0 {
		t.Fatal("the confirm page had side effects")
	}
	if _, err := o.oa.PeekConnectTicket(ctx, ticket); err != nil {
		t.Fatalf("the confirm page spent the ticket: %v", err)
	}
	// A member's session there: the page refuses.
	if page := e.confirmLink(t, ticket, e.cookieFor(t, member.ID)); page.Code != http.StatusForbidden || !strings.Contains(page.Body.String(), "as an admin") {
		t.Fatalf("setup confirm with a member session: %d %s", page.Code, page.Body.String())
	}

	// Continue without the nonce, cross-site, or without an admin session:
	// refused, the ticket stays live.
	profileB := e.cookieFor(t, e.admin.ID)
	if rec := e.postLink(t, ticket, url.Values{"nonce": {nonce}}, "same-origin", profileB); rec.Code != http.StatusForbidden {
		t.Fatalf("no nonce cookie: %d", rec.Code)
	}
	if rec := e.postLink(t, ticket, url.Values{"nonce": {nonce}}, "cross-site", profileB, nonceCookie); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site: %d", rec.Code)
	}
	if rec := e.continueLink(t, ticket, page); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "as an admin") {
		t.Fatalf("no session: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := o.oa.PeekConnectTicket(ctx, ticket); err != nil {
		t.Fatalf("a refused Continue spent the ticket: %v", err)
	}

	// An admin's session in profile B: the form, behind a button.
	rec := e.continueLink(t, ticket, page, profileB)
	body = rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "Continue to GitHub") || strings.Contains(body, "<script") {
		t.Fatalf("setup continue: %d %s", rec.Code, body)
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "form-action 'self' https://github.com") || !strings.Contains(csp, "script-src 'self'") {
		t.Fatalf("setup continue CSP = %q", csp)
	}
	m := githubFormActionRE.FindStringSubmatch(body)
	mf := githubManifestRE.FindStringSubmatch(body)
	if m == nil || mf == nil {
		t.Fatalf("no manifest form: %s", body)
	}
	action, err := url.Parse(html.UnescapeString(m[1]))
	if err != nil {
		t.Fatal(err)
	}
	state := action.Query().Get("state")
	var manifest map[string]any
	if err := json.Unmarshal([]byte(html.UnescapeString(mf[1])), &manifest); err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if manifest["redirect_url"] != "https://toolyard.example/v1/mcp-oauth/github-app" {
		t.Fatalf("manifest redirect_url = %v", manifest["redirect_url"])
	}
	if perms := manifest["default_permissions"].(map[string]any); len(perms) != 3 || perms["pull_requests"] != "write" {
		t.Fatalf("manifest permissions = %v", perms)
	}
	flow := flowCookieFrom(t, rec)
	if !e.srv.oauthFlowCookieValid(&http.Request{Header: http.Header{"Cookie": {flow.String()}}}, state, e.admin.ID) {
		t.Fatal("the setup is not bound to profile B")
	}
	if _, err := o.oa.PeekConnectTicket(ctx, ticket); err == nil || e.auditRows(t, connectLinkUsed, "") != 1 || e.auditMentions(t, ticket) != 0 {
		t.Fatal("Continue did not spend the ticket once, or the audit is wrong")
	}

	// GitHub's return: refused without profile B's cookie, done with it.
	callback := "/v1/mcp-oauth/github-app?code=code&state=" + url.QueryEscape(state)
	e.handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, callback, nil))
	if posts != 0 {
		t.Fatal("a browser without the setup cookie converted the App")
	}
	req := httptest.NewRequest(http.MethodGet, callback, nil)
	req.AddCookie(flow)
	done := httptest.NewRecorder()
	e.handler.ServeHTTP(done, req)
	if posts != 1 || !strings.Contains(done.Body.String(), "Install GitHub App") || strings.Contains(done.Body.String(), "secret-value") {
		t.Fatalf("callback in profile B: %d %s", done.Code, done.Body.String())
	}
	if cli, err := o.oa.GetClient(ctx, "GitHubForUsers"); err != nil || cli.ClientID != "Iv1.test" {
		t.Fatalf("client not saved: %v", err)
	}

	// Once only, and no new link once the App exists.
	if page := e.confirmLink(t, ticket); page.Code != http.StatusGone {
		t.Fatalf("setup link after use: %d", page.Code)
	}
	if rec := e.do(t, adminCookie, http.MethodPost, "/v1/github/setup/begin", `{"server":"GitHubForUsers","mode":"link"}`); rec.Code != http.StatusConflict {
		t.Fatalf("link after setup: %d %s", rec.Code, rec.Body.String())
	}
}

// TestGitHubAppSetupTicketIsNotASignIn: a setup ticket never starts a
// sign-in, and a sign-in ticket never shows the setup form.
func TestGitHubAppSetupTicketIsNotASignIn(t *testing.T) {
	c := newConnectLinkEnv(t)
	e := c.e
	ctx := context.Background()
	setup, err := c.oe.oa.IssueGitHubAppSetupTicket(ctx, e.admin.ID, "linear")
	if err != nil {
		t.Fatal(err)
	}
	// linear is not a GitHub server: the setup page refuses it, and the
	// sign-in lookup never takes it.
	if page := e.confirmLink(t, setup); page.Code != http.StatusGone {
		t.Fatalf("setup ticket on a non-GitHub server: %d %s", page.Code, page.Body.String())
	}
	r := httptest.NewRequest(http.MethodGet, connectLinkPath+setup, nil)
	if _, refusal := e.srv.lookupConnectLink(r); refusal == nil || refusal.reason != "bad_purpose" {
		t.Fatalf("sign-in lookup of a setup ticket = %+v", refusal)
	}
	signIn := c.ticket(t, c.ada.ID, "linear", oauth.ConnectPurposePerUser)
	r = httptest.NewRequest(http.MethodGet, connectLinkPath+signIn, nil)
	if _, refusal := e.srv.lookupGitHubSetupLink(r); refusal == nil || refusal.reason != "bad_purpose" {
		t.Fatalf("setup lookup of a sign-in ticket = %+v", refusal)
	}
}
