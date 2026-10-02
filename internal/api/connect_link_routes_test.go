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
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/oauth"
)

// The connect link: the page an agent sends the person to when a server
// needs their sign-in. These tests open it the way a browser does (a plain
// GET, no CSRF header, no toolyard session) and finish the flow the way
// the provider's redirect does.

// connectLinkIdP is a token endpoint whose code decides the account: code
// "as:<email>" yields an id_token naming that email, "noemail" yields no
// id_token, anything else names <code>@example.test.
func connectLinkIdP(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		code := r.Form.Get("code")
		out := map[string]any{"access_token": "at-" + code, "refresh_token": "rt-" + code, "token_type": "Bearer", "expires_in": 3600}
		email := code + "@example.test"
		switch {
		case code == "noemail":
			email = ""
		case strings.HasPrefix(code, "as:"):
			email = strings.TrimPrefix(code, "as:")
		}
		if email != "" {
			hdr := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
			pl, _ := json.Marshal(map[string]string{"email": email})
			out["id_token"] = hdr + "." + base64.RawURLEncoding.EncodeToString(pl) + ".s"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
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

// openLink GETs a connect link the way a browser does: no CSRF header, only
// the given cookies.
func (e *accessTestEnv) openLink(t *testing.T, ticket string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
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

// redirectState checks the 302 points at the IdP's authorize endpoint with
// toolyard's callback as redirect_uri, and returns the state.
func redirectState(t *testing.T, rec *httptest.ResponseRecorder, idp *httptest.Server) string {
	t.Helper()
	if rec.Code != http.StatusFound {
		t.Fatalf("status %d, want 302: %s", rec.Code, rec.Body.String())
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

type connectLinkEnv struct {
	e   *accessTestEnv
	oe  *oauthEnv
	ada *identity.User // member with an email
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

// TestConnectLinkStartsPerUserSignIn: opening the link redeems the ticket,
// sets the flow cookie for that state and person, and redirects to the
// provider with toolyard's callback; the provider's return in that browser
// stores the token on the person's row (case-insensitive email match) and
// shows the "back to your agent" page. The link works once, and the ticket
// never reaches the audit log.
func TestConnectLinkStartsPerUserSignIn(t *testing.T) {
	c := newConnectLinkEnv(t)
	e, ada := c.e, c.ada
	ticket := c.ticket(t, ada.ID, "linear", oauth.ConnectPurposePerUser)

	rec := e.openLink(t, ticket)
	state := redirectState(t, rec, c.oe.idp)
	cookie := flowCookieFrom(t, rec)
	if cookie.Name != oauthFlowCookieName(state) || cookie.Path != oauthFlowCookiePath || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("flow cookie = %+v", cookie)
	}
	if !e.srv.oauthFlowCookieValid(&http.Request{Header: http.Header{"Cookie": {cookie.String()}}}, state, ada.ID) {
		t.Fatal("flow cookie is not bound to (state, ada)")
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
	if cb.Code != http.StatusOK || !strings.Contains(cb.Body.String(), "Go back to T3 Code") || !strings.Contains(cb.Body.String(), "tell your agent to retry") {
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

	// Once only.
	again := e.openLink(t, ticket)
	if again.Code != http.StatusGone || !strings.Contains(again.Body.String(), "expired or was already used") || !strings.Contains(again.Body.String(), "ask your agent for a new one") {
		t.Fatalf("second open: %d %s", again.Code, again.Body.String())
	}
	if got := e.auditReasons(t, connectLinkRefused); len(got) != 1 || got[0] != "used" {
		t.Fatalf("link_refused reasons = %v", got)
	}
	if e.auditMentions(t, ticket) != 0 {
		t.Fatal("ticket reached the audit log")
	}
}

// TestConnectLinkRefusesStaleTickets: unknown, expired and malformed
// tickets get the plain page and an audit row naming the reason; the
// route is GET-only and never reachable with an operator token.
func TestConnectLinkRefusesStaleTickets(t *testing.T) {
	c := newConnectLinkEnv(t)
	e := c.e
	if connectLinkPath != oauth.ConnectLinkPath {
		t.Fatalf("route %q and oauth.ConnectLinkPath %q differ", connectLinkPath, oauth.ConnectLinkPath)
	}

	rec := e.openLink(t, "not-a-ticket")
	if rec.Code != http.StatusGone || !strings.Contains(rec.Body.String(), "expired or was already used") {
		t.Fatalf("unknown: %d %s", rec.Code, rec.Body.String())
	}
	stale := c.ticket(t, c.ada.ID, "linear", oauth.ConnectPurposePerUser)
	if _, err := e.db.Exec(`UPDATE connect_tickets SET expires_at = ?`, time.Now().Add(-time.Minute).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if rec := e.openLink(t, stale); rec.Code != http.StatusGone {
		t.Fatalf("expired: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.openLink(t, ""); rec.Code != http.StatusGone {
		t.Fatalf("empty: %d", rec.Code)
	}
	if got := e.auditReasons(t, connectLinkRefused); strings.Join(got, " ") != "expired unknown unknown" {
		t.Fatalf("link_refused reasons = %v", got)
	}
	if e.auditRows(t, connectLinkUsed, "") != 0 {
		t.Fatal("a refused link was audited as used")
	}

	req := httptest.NewRequest(http.MethodPost, connectLinkPath+"x", nil)
	req.Header.Set("X-Requested-With", "toolyard")
	w := httptest.NewRecorder()
	e.handler.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST: %d", w.Code)
	}
	if _, ok := operatorScopeFor(http.MethodGet, connectLinkPath+"abc"); ok {
		t.Fatal("an operator token may redeem a connect link")
	}
	if !memberAllowed(http.MethodGet, connectLinkPath+"abc") || memberAllowed(http.MethodPost, connectLinkPath+"abc") ||
		memberAllowed(http.MethodGet, connectLinkPath) || memberAllowed(http.MethodGet, connectLinkPath+"a/b") {
		t.Fatal("member allowlist: GET of one ticket must pass; POST, an empty ticket and extra segments must not")
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
	if err := e.access.SetGroups(ctx, ada.ID, nil, e.admin.ID); err != nil {
		t.Fatal(err)
	}
	rec := e.openLink(t, ticket)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "no longer have access") {
		t.Fatalf("revoked grant: %d %s", rec.Code, rec.Body.String())
	}

	addPatchServer(t, e, "patsrv", newPatchMCPServer(t))
	if err := c.oe.oa.PutClient(ctx, oauth.ClientRecord{
		UpstreamName: "patsrv", Issuer: "(personal-access-token)", AuthorizationEndpoint: "(pat)", TokenEndpoint: "(pat)",
		ClientID: "(pat)", RedirectURI: "(pat)", TokenEndpointAuthMethod: "none",
	}); err != nil {
		t.Fatal(err)
	}
	root := e.member(t, "user_root", "root@beknown.work")
	if err := e.id.SetRole(ctx, root.ID, identity.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	rec = e.openLink(t, c.ticket(t, root.ID, "patsrv", oauth.ConnectPurposeShared))
	if rec.Code != http.StatusGone || !strings.Contains(rec.Body.String(), "pasted token") {
		t.Fatalf("pat server: %d %s", rec.Code, rec.Body.String())
	}
	if got := e.auditReasons(t, connectLinkRefused); strings.Join(got, " ") != "not_granted token_only" {
		t.Fatalf("link_refused reasons = %v", got)
	}
	var pending int
	_ = e.db.QueryRow(`SELECT count(*) FROM oauth_pending`).Scan(&pending)
	if pending != 0 {
		t.Fatalf("%d pending flows left by refused links", pending)
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
		IsError bool              `json:"is_error"`
		Content []json.RawMessage `json:"content"`
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

// TestConnectLinkMismatchedAccountStoresNothing: the person signed in to
// the provider as somebody else. No token is stored, the flow is burnt,
// and the page names both accounts.
func TestConnectLinkMismatchedAccountStoresNothing(t *testing.T) {
	c := newConnectLinkEnv(t)
	e, ada := c.e, c.ada
	rec := e.openLink(t, c.ticket(t, ada.ID, "linear", oauth.ConnectPurposePerUser))
	state := redirectState(t, rec, c.oe.idp)
	cookie := flowCookieFrom(t, rec)

	cb := e.callbackCookies(t, state, "as:bob@beknown.work", cookie)
	if cb.Code != http.StatusBadRequest {
		t.Fatalf("callback: %d %s", cb.Code, cb.Body.String())
	}
	body := cb.Body.String()
	if !strings.Contains(body, "This link was made for ada@beknown.work; you signed in as bob@beknown.work. Sign in with the right account.") {
		t.Fatalf("mismatch page: %s", body)
	}
	if rows := userTokenRows(t, e, "linear"); len(rows) != 0 {
		t.Fatalf("token stored after a mismatch: %v", rows)
	}
	if e.auditRows(t, connectAccountMismatch, "") != 1 || e.auditRows(t, "oauth.user_success", "") != 0 {
		t.Fatalf("audit: mismatch=%d success=%d", e.auditRows(t, connectAccountMismatch, ""), e.auditRows(t, "oauth.user_success", ""))
	}
	if e.auditMentions(t, "bob@beknown.work") != 0 {
		t.Fatal("the other account's email was written to the audit log")
	}
	if _, err := c.oe.oa.LoadPending(context.Background(), state); !errors.Is(err, oauth.ErrPendingNotFound) {
		t.Fatalf("pending flow survives a mismatch: %v", err)
	}
	// The code is spent and the flow gone: a retry with the right account
	// cannot resurrect it.
	if cb := e.callbackCookies(t, state, "as:ada@beknown.work", cookie); cb.Code != http.StatusBadRequest {
		t.Fatalf("replay after mismatch: %d", cb.Code)
	}
}

// TestConnectLinkUnverifiedAccountIsStoredAndAudited: a provider that
// names no account cannot be checked; the token is stored and the audit
// log says the match was not verified.
func TestConnectLinkUnverifiedAccountIsStoredAndAudited(t *testing.T) {
	c := newConnectLinkEnv(t)
	e, ada := c.e, c.ada
	rec := e.openLink(t, c.ticket(t, ada.ID, "linear", oauth.ConnectPurposePerUser))
	state := redirectState(t, rec, c.oe.idp)
	cb := e.callbackCookies(t, state, "noemail", flowCookieFrom(t, rec))
	if cb.Code != http.StatusOK || !strings.Contains(cb.Body.String(), "tell your agent to retry") {
		t.Fatalf("callback: %d %s", cb.Code, cb.Body.String())
	}
	if rows := userTokenRows(t, e, "linear"); rows[ada.ID] != "active" {
		t.Fatalf("token rows = %v", rows)
	}
	var decision string
	if err := e.db.QueryRow(`SELECT COALESCE(decision,'') FROM audit_events WHERE event_type = ?`, connectUnverifiedAccount).Scan(&decision); err != nil || decision != "unverified_account" {
		t.Fatalf("unverified_account audit: %q %v", decision, err)
	}
}

// TestConnectLinkDashboardFlowKeepsItsPage: a per-user flow the dashboard
// started is not a ticket flow: no account check, the My connections page.
func TestConnectLinkDashboardFlowKeepsItsPage(t *testing.T) {
	c := newConnectLinkEnv(t)
	e, ada := c.e, c.ada
	state, cookie := beginFor(t, e, e.cookieFor(t, ada.ID), "linear")
	cb := e.callbackCookies(t, state, "as:somebody@else.test", cookie)
	if cb.Code != http.StatusOK || !strings.Contains(cb.Body.String(), "My connections") || strings.Contains(cb.Body.String(), "T3 Code") {
		t.Fatalf("dashboard callback: %d %s", cb.Code, cb.Body.String())
	}
	if rows := userTokenRows(t, e, "linear"); rows[ada.ID] != "active" {
		t.Fatalf("token rows = %v", rows)
	}
	if e.auditRows(t, connectUnverifiedAccount, "") != 0 || e.auditRows(t, connectAccountMismatch, "") != 0 {
		t.Fatal("a dashboard flow was put through the ticket account check")
	}
}

// TestConnectLinkSharedNeedsAdmin: a shared-purpose link signs the
// server's shared account in, for an admin only; a member's shared link and
// a link whose purpose no longer matches the server are refused.
func TestConnectLinkSharedNeedsAdmin(t *testing.T) {
	c := newConnectLinkEnv(t)
	e := c.e
	ctx := context.Background()
	addPatchServer(t, e, "gh", newPatchMCPServer(t))
	c.oe.client(t, "gh")
	root := e.member(t, "user_root", "root@beknown.work")
	if err := e.id.SetRole(ctx, root.ID, identity.RoleAdmin); err != nil {
		t.Fatal(err)
	}

	rec := e.openLink(t, c.ticket(t, root.ID, "gh", oauth.ConnectPurposeShared))
	state := redirectState(t, rec, c.oe.idp)
	cb := e.callbackCookies(t, state, "ops", flowCookieFrom(t, rec))
	if cb.Code != http.StatusOK || !strings.Contains(cb.Body.String(), "tell your agent to retry") {
		t.Fatalf("shared callback: %d %s", cb.Code, cb.Body.String())
	}
	var st, label string
	if err := e.db.QueryRow(`SELECT state, COALESCE(account_label,'') FROM oauth_tokens WHERE upstream_name = 'gh'`).Scan(&st, &label); err != nil || st != "active" || label != "ops@example.test" {
		t.Fatalf("shared token = %q %q %v", st, label, err)
	}

	// A member's shared link, even for a server granted to them: refused,
	// nothing started.
	if err := e.access.SetGroups(ctx, c.ada.ID, []string{"linear", "gh"}, e.admin.ID); err != nil {
		t.Fatal(err)
	}
	rec = e.openLink(t, c.ticket(t, c.ada.ID, "gh", oauth.ConnectPurposeShared))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "Only an admin") {
		t.Fatalf("member shared link: %d %s", rec.Code, rec.Body.String())
	}
	// A per_user link for a shared server, and a shared link for a
	// per_user server: the server changed since the link was made.
	if rec := e.openLink(t, c.ticket(t, c.ada.ID, "gh", oauth.ConnectPurposePerUser)); rec.Code != http.StatusGone || !strings.Contains(rec.Body.String(), "changed since the link was made") {
		t.Fatalf("mode mismatch: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.openLink(t, c.ticket(t, root.ID, "linear", oauth.ConnectPurposeShared)); rec.Code != http.StatusGone {
		t.Fatalf("shared link for a per_user server: %d", rec.Code)
	}
	if got := e.auditReasons(t, connectLinkRefused); strings.Join(got, " ") != "mode_changed mode_changed not_admin" {
		t.Fatalf("link_refused reasons = %v", got)
	}
	var pending int
	_ = e.db.QueryRow(`SELECT count(*) FROM oauth_pending`).Scan(&pending)
	if pending != 0 {
		t.Fatalf("%d pending flows left by refused links", pending)
	}
}

// TestConnectLinkWithMemberSession: a member whose browser also holds a
// toolyard session passes RoleGuard and gets the same redirect; their own
// session finishes the flow too.
func TestConnectLinkWithMemberSession(t *testing.T) {
	c := newConnectLinkEnv(t)
	e, ada := c.e, c.ada
	session := e.cookieFor(t, ada.ID)
	rec := e.openLink(t, c.ticket(t, ada.ID, "linear", oauth.ConnectPurposePerUser), session)
	state := redirectState(t, rec, c.oe.idp)
	cb := e.callbackCookies(t, state, "as:ada@beknown.work", session)
	if cb.Code != http.StatusOK || !strings.Contains(cb.Body.String(), "tell your agent to retry") {
		t.Fatalf("callback with session: %d %s", cb.Code, cb.Body.String())
	}
	if rows := userTokenRows(t, e, "linear"); rows[ada.ID] != "active" {
		t.Fatalf("token rows = %v", rows)
	}
	// Another person's session cannot finish ada's link flow.
	bob := e.member(t, "user_bob", "bob@beknown.work")
	rec = e.openLink(t, c.ticket(t, ada.ID, "linear", oauth.ConnectPurposePerUser))
	state = redirectState(t, rec, c.oe.idp)
	if cb := e.callbackCookies(t, state, "as:ada@beknown.work", e.cookieFor(t, bob.ID)); cb.Code != http.StatusBadRequest {
		t.Fatalf("another user's session finished the flow: %d", cb.Code)
	}
	if rows := userTokenRows(t, e, "linear"); len(rows) != 1 {
		t.Fatalf("token rows = %v", rows)
	}
}
