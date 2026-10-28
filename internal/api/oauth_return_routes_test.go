package api

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/mcpauth"
)

// oauthFlowEnv drives toolyard's OAuth server for MCP clients through its
// routes: the authorize stop page, the consent screen and the return link.
type oauthFlowEnv struct {
	*accessTestEnv
	t                   *testing.T
	svc                 *mcpauth.Service
	mux                 *http.ServeMux
	clock               time.Time
	clientID            string
	verifier, challenge string
	// codes are the confirmation codes the stop pages showed, by request.
	codes map[string]string
}

const (
	flowPublic   = "https://toolyard.example.com"
	flowRedirect = "https://client.example/cb"
)

func newOAuthFlowEnv(t *testing.T) *oauthFlowEnv {
	t.Helper()
	f := &oauthFlowEnv{accessTestEnv: newAccessTestServer(t), t: t, clock: time.Now(), codes: map[string]string{}}
	f.svc = mcpauth.New(mcpauth.Options{DB: f.db, PublicURL: flowPublic, Access: f.access, Now: func() time.Time { return f.clock }})
	f.mux = http.NewServeMux()
	f.srv.MCPOAuthRoutes(f.mux, f.svc)
	reg, oerr := f.svc.Register(t.Context(), []byte(`{"redirect_uris":["`+flowRedirect+`"],"token_endpoint_auth_method":"none","client_name":"Example Client"}`), "198.51.100.7")
	if oerr != nil {
		t.Fatal(oerr)
	}
	f.clientID = reg.ClientID
	b := make([]byte, 40)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	f.verifier = base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(f.verifier))
	f.challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return f
}

// do serves one request on the OAuth routes.
func (f *oauthFlowEnv) do(method, target string, form url.Values, hdr map[string]string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	f.t.Helper()
	var r *http.Request
	if form != nil {
		r = httptest.NewRequest(method, target, strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		r = httptest.NewRequest(method, target, nil)
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	for _, c := range cookies {
		r.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, r)
	return rec
}

func (f *oauthFlowEnv) authorizeURL() string {
	q := url.Values{"response_type": {"code"}, "client_id": {f.clientID}, "redirect_uri": {flowRedirect}, "state": {"st-9"},
		"code_challenge": {f.challenge}, "code_challenge_method": {"S256"}, "scope": {"mcp offline_access"}}
	return mcpauth.AuthorizePath + "?" + q.Encode()
}

var (
	stopConsentRE = regexp.MustCompile(`href="/oauth/consent\?request=([A-Za-z0-9_-]{43})"`)
	formNonceRE   = regexp.MustCompile(`name="nonce" value="([^"]+)"`)
	stopCodeRE    = regexp.MustCompile(`class="oauth-code">([2-9A-HJKMNP-Z]{3}-[2-9A-HJKMNP-Z]{3})</strong>`)
)

// pending runs authorize and returns the request id from the stop page,
// keeping its confirmation code in f.codes.
func (f *oauthFlowEnv) pending() string {
	f.t.Helper()
	page := f.do(http.MethodGet, f.authorizeURL(), nil, nil)
	m := stopConsentRE.FindStringSubmatch(page.Body.String())
	code := stopCodeRE.FindStringSubmatch(page.Body.String())
	if page.Code != http.StatusOK || m == nil || code == nil {
		f.t.Fatalf("authorize: %d %s", page.Code, page.Body)
	}
	f.codes[m[1]] = code[1]
	return m[1]
}

// answer posts the consent form for request id as the admin, from a
// browser that did not render the stop page (it types the stop page's
// code on an allow), and returns the response.
func (f *oauthFlowEnv) answer(id string, form url.Values) *httptest.ResponseRecorder {
	f.t.Helper()
	if form.Get("verdict") == "allow" && !form.Has("code") && f.codes[id] != "" {
		form.Set("code", f.codes[id])
	}
	consentURL := mcpauth.ConsentPath + "?request=" + url.QueryEscape(id)
	session := f.cookieFor(f.t, f.admin.ID)
	page := f.do(http.MethodGet, consentURL, nil, nil, session)
	m := formNonceRE.FindStringSubmatch(page.Body.String())
	if page.Code != http.StatusOK || m == nil {
		f.t.Fatalf("consent page: %d %s", page.Code, page.Body)
	}
	if csp := page.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "form-action 'self'; ") && !strings.HasSuffix(csp, "form-action 'self'") {
		f.t.Fatalf("the consent screen still allows posting to another origin: %q", csp)
	}
	form.Set("nonce", m[1])
	return f.do(http.MethodPost, consentURL, form, nil, append(page.Result().Cookies(), session)...)
}

// returnPage opens the return link with no toolyard session (the browser
// where the client waits) and returns the page and its nonce cookie.
func (f *oauthFlowEnv) returnPage(ticket string) (*httptest.ResponseRecorder, string, *http.Cookie) {
	f.t.Helper()
	page := f.do(http.MethodGet, mcpauth.ReturnPath+ticket, nil, nil)
	m := formNonceRE.FindStringSubmatch(page.Body.String())
	var nonce *http.Cookie
	for _, c := range page.Result().Cookies() {
		if strings.HasPrefix(c.Name, returnNoncePrefix) {
			nonce = c
		}
	}
	if page.Code != http.StatusOK || m == nil || nonce == nil {
		f.t.Fatalf("return page: %d %s", page.Code, page.Body)
	}
	return page, m[1], nonce
}

func (f *oauthFlowEnv) count(q string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.db.QueryRow(q, args...).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func hashOf(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])
}

// TestAuthorizeStopsWithBothActions: /oauth/authorize never redirects by
// itself, signed in or not: it shows "Continue in this browser" (the
// consent step) and the same step's absolute link to copy.
func TestAuthorizeStopsWithBothActions(t *testing.T) {
	f := newOAuthFlowEnv(t)
	for name, cookies := range map[string][]*http.Cookie{"no session": nil, "signed in": {f.cookieFor(t, f.admin.ID)}} {
		page := f.do(http.MethodGet, f.authorizeURL(), nil, nil, cookies...)
		body := page.Body.String()
		m := stopConsentRE.FindStringSubmatch(body)
		if page.Code != http.StatusOK || page.Header().Get("Location") != "" || m == nil {
			t.Fatalf("%s: %d Location=%q %s", name, page.Code, page.Header().Get("Location"), body)
		}
		for _, want := range []string{
			"Example Client wants to connect to your toolyard",
			">Continue in this browser</a>",
			"Copy link for another browser",
			`value="` + flowPublic + `/oauth/consent?request=` + m[1] + `"`,
			`data-copy="oauth-consent-link"`,
			`<script src="/oauth/copy.js" defer></script>`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("%s: the stop page lacks %q", name, want)
			}
		}
		if strings.Contains(body, `http-equiv="refresh"`) {
			t.Errorf("%s: the stop page still moves on by itself", name)
		}
	}
	// The link leads to the consent step, in any browser signed in there.
	id := f.pending()
	if page := f.do(http.MethodGet, mcpauth.ConsentPath+"?request="+id, nil, nil, f.cookieFor(t, f.admin.ID)); page.Code != http.StatusOK ||
		!strings.Contains(page.Body.String(), "Allow this app to use your toolyard?") {
		t.Fatalf("consent from the copied link: %d %s", page.Code, page.Body)
	}
	if page := f.do(http.MethodGet, mcpauth.ConsentPath+"?request="+id, nil, nil); page.Code != http.StatusUnauthorized ||
		!strings.Contains(page.Body.String(), "/login?oauth_request="+id) {
		t.Fatalf("consent without a session: %d %s", page.Code, page.Body)
	}
	// The copy button's script is ours, so the strict CSP allows it.
	js := f.do(http.MethodGet, "/oauth/copy.js", nil, nil)
	if js.Code != http.StatusOK || !strings.HasPrefix(js.Header().Get("Content-Type"), "text/javascript") || !strings.Contains(js.Body.String(), "data-copy") {
		t.Fatalf("copy script: %d %q", js.Code, js.Header().Get("Content-Type"))
	}
	// An invalid request is still an error page, not a stop page.
	bad := f.do(http.MethodGet, mcpauth.AuthorizePath+"?client_id="+url.QueryEscape(f.clientID)+"&redirect_uri="+url.QueryEscape("https://evil.example/cb"), nil, nil)
	if bad.Code != http.StatusBadRequest || bad.Header().Get("Location") != "" || strings.Contains(bad.Body.String(), "Copy link") {
		t.Fatalf("invalid request: %d %s", bad.Code, bad.Body)
	}
}

// TestConsentApproveGivesAOneTimeReturnLink walks the whole path: the
// consent POST stores a ticket (its hash only) and sends the browser to the
// return page, never to the client; the page changes nothing; its POST
// needs the nonce and a same-origin form, mints the code then and sends
// the browser to the client; the code works at the token endpoint; a
// second use is refused.
func TestConsentApproveGivesAOneTimeReturnLink(t *testing.T) {
	f := newOAuthFlowEnv(t)
	done := f.answer(f.pending(), url.Values{"verdict": {"allow"}, "nickname": {"Profile B"}, "scope_mode": {"all"}})
	loc := done.Header().Get("Location")
	if done.Code != http.StatusSeeOther || !strings.HasPrefix(loc, mcpauth.ReturnPath) || strings.Contains(loc, "client.example") {
		t.Fatalf("consent allow: %d %q, want 303 to the return link", done.Code, loc)
	}
	ticket := strings.TrimPrefix(loc, mcpauth.ReturnPath)
	if !mcpauth.ValidReturnTicket(ticket) {
		t.Fatalf("return link %q", loc)
	}
	if f.count(`SELECT COUNT(*) FROM mcp_oauth_codes`) != 0 {
		t.Fatal("a code was minted before the return link was used")
	}
	if f.count(`SELECT COUNT(*) FROM mcp_oauth_return_tickets WHERE ticket_hash = ? AND used_at IS NULL`, hashOf(ticket)) != 1 ||
		f.count(`SELECT COUNT(*) FROM mcp_oauth_return_tickets WHERE ticket_hash = ?`, ticket) != 0 {
		t.Fatal("the ticket is not stored by its hash alone")
	}

	// The return page, in a browser with no toolyard session.
	page, nonce, nonceCookie := f.returnPage(ticket)
	body := page.Body.String()
	for _, want := range []string{
		"Approved. Return to Example Client",
		"<strong>client.example</strong>",
		"<strong>Profile B</strong>",
		">Continue in this browser</button>",
		"Copy return link",
		`value="` + flowPublic + mcpauth.ReturnPath + ticket + `"`,
		`action="` + mcpauth.ReturnPath + ticket + `"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the return page lacks %q", want)
		}
	}
	if csp := page.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "form-action 'self' https://client.example") {
		t.Errorf("return page CSP does not allow the redirect target: %q", csp)
	}
	if rp := page.Header().Get("Referrer-Policy"); rp != "same-origin" {
		t.Errorf("Referrer-Policy %q, want same-origin", rp)
	}
	if head := f.do(http.MethodHead, mcpauth.ReturnPath+ticket, nil, nil); head.Code != http.StatusOK {
		t.Errorf("HEAD: %d", head.Code)
	}
	if f.count(`SELECT COUNT(*) FROM mcp_oauth_return_tickets WHERE used_at IS NOT NULL`)+f.count(`SELECT COUNT(*) FROM mcp_oauth_codes`) != 0 {
		t.Fatal("opening the return page used the ticket")
	}

	// Refused: no nonce cookie, a cross-site form, a wrong nonce.
	post := func(nonce string, site string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
		hdr := map[string]string{}
		if site != "" {
			hdr["Sec-Fetch-Site"] = site
		}
		return f.do(http.MethodPost, mcpauth.ReturnPath+ticket, url.Values{"nonce": {nonce}}, hdr, cookies...)
	}
	if r := post(nonce, "same-origin"); r.Code != http.StatusForbidden {
		t.Errorf("POST without the nonce cookie: %d", r.Code)
	}
	if r := post(nonce, "cross-site", nonceCookie); r.Code != http.StatusForbidden {
		t.Errorf("cross-site POST: %d", r.Code)
	}
	if r := post(nonce+"x", "same-origin", nonceCookie); r.Code != http.StatusForbidden {
		t.Errorf("POST with a wrong nonce: %d", r.Code)
	}
	if f.count(`SELECT COUNT(*) FROM mcp_oauth_return_tickets WHERE used_at IS NOT NULL`) != 0 {
		t.Fatal("a refused POST used the ticket")
	}

	// Continue: the code is minted now and goes to the client.
	back := post(nonce, "same-origin", nonceCookie)
	to, err := url.Parse(back.Header().Get("Location"))
	if back.Code != http.StatusSeeOther || err != nil || !strings.HasPrefix(back.Header().Get("Location"), flowRedirect+"?") ||
		to.Query().Get("code") == "" || to.Query().Get("state") != "st-9" || to.Query().Get("iss") != flowPublic {
		t.Fatalf("return POST: %d %q", back.Code, back.Header().Get("Location"))
	}
	tok := f.do(http.MethodPost, mcpauth.TokenPath, url.Values{"grant_type": {"authorization_code"}, "code": {to.Query().Get("code")},
		"redirect_uri": {flowRedirect}, "code_verifier": {f.verifier}, "client_id": {f.clientID}}, nil)
	var out map[string]any
	if err := json.Unmarshal(tok.Body.Bytes(), &out); err != nil || tok.Code != http.StatusOK || out["access_token"] == nil {
		t.Fatalf("token endpoint: %d %s", tok.Code, tok.Body)
	}

	// A second use is refused, on the page and on the button.
	if again := f.do(http.MethodGet, mcpauth.ReturnPath+ticket, nil, nil); again.Code != http.StatusGone ||
		!strings.Contains(again.Body.String(), "This return link was already used") || !strings.Contains(again.Body.String(), "Start again from Example Client") {
		t.Fatalf("return page after use: %d %s", again.Code, again.Body)
	}
	if again := post(nonce, "same-origin", nonceCookie); again.Code != http.StatusGone || !strings.Contains(again.Body.String(), "already used") {
		t.Fatalf("second POST: %d %s", again.Code, again.Body)
	}
	if f.count(`SELECT COUNT(*) FROM mcp_oauth_codes`) != 1 {
		t.Fatal("a second use minted another code")
	}
	if unknown := f.do(http.MethodGet, mcpauth.ReturnPath+"tyrl_bogus", nil, nil); unknown.Code != http.StatusNotFound ||
		!strings.Contains(unknown.Body.String(), "This return link is not valid") {
		t.Fatalf("unknown link: %d %s", unknown.Code, unknown.Body)
	}
}

// TestExpiredReturnLinkIsRefused: ten minutes after the consent the link
// is dead, on the page and on the button, and says to start again.
func TestExpiredReturnLinkIsRefused(t *testing.T) {
	f := newOAuthFlowEnv(t)
	done := f.answer(f.pending(), url.Values{"verdict": {"allow"}, "nickname": {"Slow"}, "scope_mode": {"all"}})
	ticket := strings.TrimPrefix(done.Header().Get("Location"), mcpauth.ReturnPath)
	_, nonce, nonceCookie := f.returnPage(ticket)
	f.clock = f.clock.Add(mcpauth.ReturnTTL + time.Second)
	r := f.do(http.MethodPost, mcpauth.ReturnPath+ticket, url.Values{"nonce": {nonce}}, map[string]string{"Sec-Fetch-Site": "same-origin"}, nonceCookie)
	if r.Code != http.StatusGone || !strings.Contains(r.Body.String(), "This return link expired") || r.Header().Get("Location") != "" {
		t.Fatalf("expired POST: %d %s", r.Code, r.Body)
	}
	if page := f.do(http.MethodGet, mcpauth.ReturnPath+ticket, nil, nil); page.Code != http.StatusGone ||
		!strings.Contains(page.Body.String(), "Start again from Example Client") {
		t.Fatalf("expired page: %d %s", page.Code, page.Body)
	}
	if f.count(`SELECT COUNT(*) FROM mcp_oauth_codes`) != 0 {
		t.Fatal("an expired link minted a code")
	}
}

// TestConsentDenyStopsWithALinkForTheClient: a deny also stops on a page
// of ours, whose links carry access_denied to the client in this browser
// or another.
func TestConsentDenyStopsWithALinkForTheClient(t *testing.T) {
	f := newOAuthFlowEnv(t)
	done := f.answer(f.pending(), url.Values{"verdict": {"deny"}})
	body := done.Body.String()
	if done.Code != http.StatusOK || done.Header().Get("Location") != "" {
		t.Fatalf("deny: %d Location=%q", done.Code, done.Header().Get("Location"))
	}
	m := regexp.MustCompile(`<a class="btn primary" href="([^"]+)">Continue in this browser</a>`).FindStringSubmatch(body)
	if m == nil || !strings.Contains(body, "You did not allow Example Client") || !strings.Contains(body, `data-copy="oauth-deny-link"`) {
		t.Fatalf("deny page: %s", body)
	}
	to, err := url.Parse(strings.ReplaceAll(m[1], "&amp;", "&"))
	if err != nil || !strings.HasPrefix(to.String(), flowRedirect+"?") || to.Query().Get("error") != "access_denied" ||
		to.Query().Get("state") != "st-9" || to.Query().Get("code") != "" {
		t.Fatalf("deny link %q", m[1])
	}
	if f.count(`SELECT COUNT(*) FROM mcp_oauth_return_tickets`)+f.count(`SELECT COUNT(*) FROM mcp_oauth_grants`) != 0 {
		t.Fatal("a deny stored a return link or a grant")
	}
}
