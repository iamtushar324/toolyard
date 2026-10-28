package api

import (
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

var (
	waitWaiterRE  = regexp.MustCompile(`name="waiter" value="([^"]+)"`)
	waitRequestRE = regexp.MustCompile(`name="request" value="([^"]+)"`)
	waitDenyRE    = regexp.MustCompile(`<div data-wait-state="denied" hidden>.*?<a class="btn primary" href="([^"]+)">Continue</a>`)
	htmlAttrRE    = regexp.MustCompile(`(?:href|action|src)="([^"]*)"`)
)

// waitingPage is the authorize stop page as the browser where the client
// started sees it (no toolyard session).
type waitingPage struct {
	rec                *httptest.ResponseRecorder
	body               string
	id, waiter, nonce  string
	cookie             *http.Cookie
	consentLinkInField string
}

func (f *oauthFlowEnv) waitingPage() *waitingPage {
	f.t.Helper()
	rec := f.do(http.MethodGet, f.authorizeURL(), nil, nil)
	p := &waitingPage{rec: rec, body: rec.Body.String()}
	id, waiter, nonce := stopConsentRE.FindStringSubmatch(p.body), waitWaiterRE.FindStringSubmatch(p.body), formNonceRE.FindStringSubmatch(p.body)
	for _, c := range rec.Result().Cookies() {
		if strings.HasPrefix(c.Name, waitNoncePrefix) {
			p.cookie = c
		}
	}
	if rec.Code != http.StatusOK || id == nil || waiter == nil || nonce == nil || p.cookie == nil {
		f.t.Fatalf("waiting stop page: %d %s", rec.Code, p.body)
	}
	p.id, p.waiter, p.nonce = id[1], waiter[1], nonce[1]
	if m := regexp.MustCompile(`id="oauth-consent-link"[^>]*value="([^"]+)"`).FindStringSubmatch(p.body); m != nil {
		p.consentLinkInField = m[1]
	}
	return p
}

// poll asks /oauth/wait as the page's script does and returns the status
// code and the raw body.
func (f *oauthFlowEnv) poll(id, waiter string, hdr map[string]string) (int, string) {
	f.t.Helper()
	raw, _ := json.Marshal(map[string]string{"request": id, "waiter": waiter})
	r := httptest.NewRequest(http.MethodPost, mcpauth.WaitPath, strings.NewReader(string(raw)))
	r.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, r)
	return rec.Code, strings.TrimSpace(rec.Body.String())
}

func (f *oauthFlowEnv) pollStatus(p *waitingPage) map[string]string {
	f.t.Helper()
	code, body := f.poll(p.id, p.waiter, map[string]string{"Sec-Fetch-Site": "same-origin"})
	var out map[string]string
	if code != http.StatusOK || json.Unmarshal([]byte(body), &out) != nil {
		f.t.Fatalf("poll: %d %s", code, body)
	}
	return out
}

// cont presses the waiting page's Continue.
func (f *oauthFlowEnv) cont(p *waitingPage, waiter, nonce, site string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	f.t.Helper()
	hdr := map[string]string{}
	if site != "" {
		hdr["Sec-Fetch-Site"] = site
	}
	return f.do(http.MethodPost, mcpauth.WaitContinuePath, url.Values{"request": {p.id}, "waiter": {waiter}, "nonce": {nonce}}, hdr, cookies...)
}

// TestStopPageWaitsWithItsSecretOnlyInTheBody: the stop page waits (texts,
// script, hidden Continue form), sets a SameSite=Strict nonce cookie, and
// carries the waiter secret once, in the form's body: never in a link, the
// copied consent link, a cookie or a header. Only its hash is stored.
func TestStopPageWaitsWithItsSecretOnlyInTheBody(t *testing.T) {
	f := newOAuthFlowEnv(t)
	p := f.waitingPage()
	if !mcpauth.ValidWaiter(p.waiter) {
		t.Fatalf("waiter %q has the wrong form", p.waiter)
	}
	for _, want := range []string{
		"Copy link for another browser",
		"Keep this window open: after you allow the app in the other browser, this window continues by itself.",
		`<script src="/oauth/wait.js" defer></script>`,
		`<script src="/oauth/copy.js" defer></script>`,
		`<div data-wait-state="approved" hidden><h2>Approved in another browser</h2>`,
		`action="` + mcpauth.WaitContinuePath + `"`,
		`<button class="primary" type="submit">Continue</button>`,
		`<div data-wait-state="denied" hidden><h2>You did not allow Example Client</h2>`,
		"This sign-in expired",
		"This sign-in was already finished",
		"<noscript><p>After you allow the app, toolyard gives you a link back to this browser, where Example Client waits.</p></noscript>",
	} {
		if !strings.Contains(p.body, want) {
			t.Errorf("the stop page lacks %q", want)
		}
	}
	// "Keep this window open" sits under the copy heading.
	if strings.Index(p.body, "Keep this window open") < strings.Index(p.body, "Copy link for another browser") {
		t.Error("the waiting note is not under the copy link")
	}
	if m := waitRequestRE.FindStringSubmatch(p.body); m == nil || m[1] != p.id {
		t.Fatalf("the Continue form carries request %v, want %s", m, p.id)
	}
	if n := strings.Count(p.body, p.waiter); n != 1 {
		t.Fatalf("the waiter appears %d times in the page, want once (the form field)", n)
	}
	for _, m := range htmlAttrRE.FindAllStringSubmatch(p.body, -1) {
		if strings.Contains(m[1], p.waiter) {
			t.Fatalf("a link or form action carries the waiter: %q", m[1])
		}
	}
	if p.consentLinkInField != flowPublic+mcpauth.ConsentPath+"?request="+p.id {
		t.Fatalf("the copied consent link is %q", p.consentLinkInField)
	}
	for k, vs := range p.rec.Header() {
		for _, v := range vs {
			if strings.Contains(v, p.waiter) {
				t.Fatalf("header %s carries the waiter", k)
			}
		}
	}
	c := p.cookie
	if c.Value != p.nonce || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != mcpauth.WaitPath || c.MaxAge <= 0 ||
		c.Name != waitNonceName(p.id) {
		t.Fatalf("nonce cookie %+v", c)
	}
	if csp := p.rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "form-action 'self' https://client.example") || !strings.Contains(csp, "script-src 'self'") {
		t.Errorf("stop page CSP: %q", csp)
	}
	if rp := p.rec.Header().Get("Referrer-Policy"); rp != "same-origin" {
		t.Errorf("Referrer-Policy %q, want same-origin", rp)
	}
	if f.count(`SELECT COUNT(*) FROM mcp_oauth_requests WHERE id = ? AND waiter_hash = ?`, p.id, hashOf(p.waiter)) != 1 ||
		f.count(`SELECT COUNT(*) FROM mcp_oauth_requests WHERE waiter_hash = ?`, p.waiter) != 0 {
		t.Fatal("the waiter is not stored by its hash alone")
	}
	js := f.do(http.MethodGet, "/oauth/wait.js", nil, nil)
	if js.Code != http.StatusOK || !strings.HasPrefix(js.Header().Get("Content-Type"), "text/javascript") ||
		!strings.Contains(js.Body.String(), "'/oauth/wait'") || strings.Contains(js.Body.String(), ".submit(") {
		t.Fatalf("wait script: %d %q", js.Code, js.Header().Get("Content-Type"))
	}
}

// TestWaitPollRevealsNothingWithoutTheWaiter: a missing or wrong waiter,
// and an unknown request, get byte-for-byte the answer of an unknown
// request, before and after the approval. Only POST with JSON from the
// page's own origin is answered.
func TestWaitPollRevealsNothingWithoutTheWaiter(t *testing.T) {
	f := newOAuthFlowEnv(t)
	p := f.waitingPage()
	other := f.waitingPage()
	same := map[string]string{"Sec-Fetch-Site": "same-origin"}
	unknownCode, unknownBody := f.poll(strings.Repeat("A", 43), p.waiter, same)
	if unknownCode != http.StatusOK || unknownBody != `{"status":"expired"}` {
		t.Fatalf("unknown request: %d %s", unknownCode, unknownBody)
	}
	check := func(stage string) {
		t.Helper()
		for _, w := range [][2]string{{p.id, ""}, {p.id, "tywt_short"}, {p.id, "tywt_" + strings.Repeat("A", 43)}, {p.id, other.waiter}, {p.id, p.waiter + "x"}, {"", p.waiter}} {
			if code, body := f.poll(w[0], w[1], same); code != unknownCode || body != unknownBody {
				t.Errorf("%s: poll(%q, %q) = %d %s, want the unknown request's answer", stage, w[0], w[1], code, body)
			}
		}
	}
	check("pending")
	if st := f.pollStatus(p); st["status"] != mcpauth.WaitPending || st["client"] != "" {
		t.Fatalf("the right waiter: %v, want pending", st)
	}
	if done := f.answer(p.id, url.Values{"verdict": {"allow"}, "nickname": {"Profile B"}, "scope_mode": {"all"}}); done.Code != http.StatusSeeOther {
		t.Fatalf("consent: %d %s", done.Code, done.Body)
	}
	check("approved")

	if r := f.do(http.MethodGet, mcpauth.WaitPath, nil, nil); r.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /oauth/wait: %d", r.Code)
	}
	if code, _ := f.poll(p.id, p.waiter, map[string]string{"Sec-Fetch-Site": "cross-site"}); code != http.StatusForbidden {
		t.Errorf("cross-site poll: %d", code)
	}
	if r := f.do(http.MethodPost, mcpauth.WaitPath, url.Values{"request": {p.id}, "waiter": {p.waiter}}, nil); r.Code != http.StatusUnsupportedMediaType {
		t.Errorf("form-encoded poll: %d", r.Code)
	}
}

// TestWaitingPageContinuesAfterApprovalInAnotherBrowser walks the waiting
// path: pending, the person allows the app in another session (the copied
// consent link), approved with the app's name; Continue is refused without
// the nonce cookie, from another site, with a wrong nonce or a wrong
// waiter, and none of that uses the approval; then it mints the code and
// 303s to the client with state and iss; the code works with PKCE; the
// return link of the same approval is then refused as used.
func TestWaitingPageContinuesAfterApprovalInAnotherBrowser(t *testing.T) {
	f := newOAuthFlowEnv(t)
	p := f.waitingPage()
	if st := f.pollStatus(p); st["status"] != mcpauth.WaitPending {
		t.Fatalf("before consent: %v", st)
	}
	done := f.answer(p.id, url.Values{"verdict": {"allow"}, "nickname": {"Profile B"}, "scope_mode": {"all"}})
	ticket := strings.TrimPrefix(done.Header().Get("Location"), mcpauth.ReturnPath)
	if done.Code != http.StatusSeeOther || !mcpauth.ValidReturnTicket(ticket) {
		t.Fatalf("consent: %d %q", done.Code, done.Header().Get("Location"))
	}
	if st := f.pollStatus(p); st["status"] != mcpauth.WaitApproved || st["client"] != "Example Client" {
		t.Fatalf("after consent: %v, want approved with the app's name", st)
	}

	for name, r := range map[string]*httptest.ResponseRecorder{
		"no nonce cookie": f.cont(p, p.waiter, p.nonce, "same-origin"),
		"cross-site":      f.cont(p, p.waiter, p.nonce, "cross-site", p.cookie),
		"wrong nonce":     f.cont(p, p.waiter, p.nonce+"x", "same-origin", p.cookie),
	} {
		if r.Code != http.StatusForbidden || r.Header().Get("Location") != "" {
			t.Errorf("%s: %d %q", name, r.Code, r.Header().Get("Location"))
		}
	}
	if r := f.cont(p, "tywt_"+strings.Repeat("A", 43), p.nonce, "same-origin", p.cookie); r.Code != http.StatusNotFound ||
		r.Header().Get("Location") != "" || !strings.Contains(r.Body.String(), "This page is not valid") {
		t.Errorf("wrong waiter: %d %s", r.Code, r.Body)
	}
	if r := f.do(http.MethodGet, mcpauth.WaitContinuePath, nil, nil); r.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET continue: %d", r.Code)
	}
	if f.count(`SELECT COUNT(*) FROM mcp_oauth_return_tickets WHERE used_at IS NOT NULL`)+f.count(`SELECT COUNT(*) FROM mcp_oauth_codes`) != 0 {
		t.Fatal("a refused Continue used the approval")
	}

	back := f.cont(p, p.waiter, p.nonce, "same-origin", p.cookie)
	to, err := url.Parse(back.Header().Get("Location"))
	if back.Code != http.StatusSeeOther || err != nil || !strings.HasPrefix(back.Header().Get("Location"), flowRedirect+"?") ||
		to.Query().Get("code") == "" || to.Query().Get("state") != "st-9" || to.Query().Get("iss") != flowPublic {
		t.Fatalf("Continue: %d %q %s", back.Code, back.Header().Get("Location"), back.Body)
	}
	cleared := false
	for _, c := range back.Result().Cookies() {
		if c.Name == p.cookie.Name && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Error("Continue did not clear the nonce cookie")
	}
	tok := f.do(http.MethodPost, mcpauth.TokenPath, url.Values{"grant_type": {"authorization_code"}, "code": {to.Query().Get("code")},
		"redirect_uri": {flowRedirect}, "code_verifier": {f.verifier}, "client_id": {f.clientID}}, nil)
	var out map[string]any
	if err := json.Unmarshal(tok.Body.Bytes(), &out); err != nil || tok.Code != http.StatusOK || out["access_token"] == nil {
		t.Fatalf("token endpoint: %d %s", tok.Code, tok.Body)
	}

	// The same approval is used: the waiting page reports it, a second
	// Continue and the return link are refused.
	if st := f.pollStatus(p); st["status"] != mcpauth.WaitUsed {
		t.Fatalf("after Continue: %v, want used", st)
	}
	if again := f.cont(p, p.waiter, p.nonce, "same-origin", p.cookie); again.Code != http.StatusGone ||
		!strings.Contains(again.Body.String(), "This sign-in was already finished") || !strings.Contains(again.Body.String(), "Start again from Example Client") {
		t.Fatalf("second Continue: %d %s", again.Code, again.Body)
	}
	if ret := f.do(http.MethodGet, mcpauth.ReturnPath+ticket, nil, nil); ret.Code != http.StatusGone || !strings.Contains(ret.Body.String(), "This return link was already used") {
		t.Fatalf("the return link after the waiting page: %d %s", ret.Code, ret.Body)
	}
	if f.count(`SELECT COUNT(*) FROM mcp_oauth_codes`) != 1 {
		t.Fatal("more than one code was minted")
	}
}

// TestReturnLinkFirstRefusesTheWaitingPageContinue: the return link used
// first, the waiting page reports used and its Continue is refused.
func TestReturnLinkFirstRefusesTheWaitingPageContinue(t *testing.T) {
	f := newOAuthFlowEnv(t)
	p := f.waitingPage()
	done := f.answer(p.id, url.Values{"verdict": {"allow"}, "nickname": {"Tab"}, "scope_mode": {"all"}})
	ticket := strings.TrimPrefix(done.Header().Get("Location"), mcpauth.ReturnPath)
	page, nonce, nonceCookie := f.returnPage(ticket)
	if body := page.Body.String(); !strings.Contains(body, "If Example Client is waiting in its window in the other browser, continue there: it is ready.") ||
		!strings.Contains(body, "Otherwise copy this link into that same window (not a new tab).") {
		t.Errorf("the return page lacks the waiting window's text: %s", body)
	}
	if r := f.do(http.MethodPost, mcpauth.ReturnPath+ticket, url.Values{"nonce": {nonce}}, map[string]string{"Sec-Fetch-Site": "same-origin"}, nonceCookie); r.Code != http.StatusSeeOther {
		t.Fatalf("return link: %d %s", r.Code, r.Body)
	}
	if st := f.pollStatus(p); st["status"] != mcpauth.WaitUsed {
		t.Fatalf("after the return link: %v, want used", st)
	}
	r := f.cont(p, p.waiter, p.nonce, "same-origin", p.cookie)
	if r.Code != http.StatusGone || r.Header().Get("Location") != "" || !strings.Contains(r.Body.String(), "already finished") {
		t.Fatalf("Continue after the return link: %d %s", r.Code, r.Body)
	}
	if f.count(`SELECT COUNT(*) FROM mcp_oauth_codes`) != 1 {
		t.Fatal("the waiting page minted a second code")
	}
}

// TestWaitingPageAfterDenyOrExpiry: a deny is reported as denied, and the
// page's denied block links where the deny page does; Continue is refused.
// An unanswered request past its ten minutes is expired.
func TestWaitingPageAfterDenyOrExpiry(t *testing.T) {
	f := newOAuthFlowEnv(t)
	p := f.waitingPage()
	done := f.answer(p.id, url.Values{"verdict": {"deny"}})
	denyPage := regexp.MustCompile(`<a class="btn primary" href="([^"]+)">Continue in this browser</a>`).FindStringSubmatch(done.Body.String())
	if done.Code != http.StatusOK || denyPage == nil {
		t.Fatalf("deny: %d %s", done.Code, done.Body)
	}
	if st := f.pollStatus(p); st["status"] != mcpauth.WaitDenied || st["client"] != "" {
		t.Fatalf("after deny: %v, want denied", st)
	}
	m := waitDenyRE.FindStringSubmatch(p.body)
	if m == nil || m[1] != denyPage[1] {
		t.Fatalf("the stop page's denied block links %v, want the deny page's %q", m, denyPage[1])
	}
	to, err := url.Parse(strings.ReplaceAll(m[1], "&amp;", "&"))
	if err != nil || to.Query().Get("error") != "access_denied" || to.Query().Get("state") != "st-9" || to.Query().Get("code") != "" {
		t.Fatalf("denied link %q", m[1])
	}
	if r := f.cont(p, p.waiter, p.nonce, "same-origin", p.cookie); r.Code != http.StatusForbidden || !strings.Contains(r.Body.String(), "You did not allow Example Client") {
		t.Fatalf("Continue after deny: %d %s", r.Code, r.Body)
	}

	late := f.waitingPage()
	f.clock = f.clock.Add(mcpauth.RequestTTL + time.Second)
	if st := f.pollStatus(late); st["status"] != mcpauth.WaitExpired {
		t.Fatalf("unanswered past its lifetime: %v, want expired", st)
	}
	if r := f.cont(late, late.waiter, late.nonce, "same-origin", late.cookie); r.Code != http.StatusGone || !strings.Contains(r.Body.String(), "This sign-in expired") {
		t.Fatalf("Continue after expiry: %d %s", r.Code, r.Body)
	}
	if f.count(`SELECT COUNT(*) FROM mcp_oauth_codes`) != 0 {
		t.Fatal("a denied or expired request minted a code")
	}
}

// TestConsentWarnsThatTheStartingWindowCanFinish: the consent screen says
// that the window where the connection started can finish it.
func TestConsentWarnsThatTheStartingWindowCanFinish(t *testing.T) {
	f := newOAuthFlowEnv(t)
	id := f.pending()
	page := f.do(http.MethodGet, mcpauth.ConsentPath+"?request="+id, nil, nil, f.cookieFor(t, f.admin.ID))
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "Allow it only if you started this connection yourself") {
		t.Fatalf("consent page: %d %s", page.Code, page.Body)
	}
}
