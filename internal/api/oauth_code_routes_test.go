package api

import (
	"bytes"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/mcpauth"
)

const codeFieldText = "Type the code shown in the window where you started this connection. If you did not start a connection yourself just now, do not type a code: close this page."

// consentAs opens request id's consent page as the admin, with extra
// cookies (the stop page's, when this browser rendered it), and posts form
// on it. It returns the page and the answer.
func (f *oauthFlowEnv) consentAs(id string, form url.Values, extra ...*http.Cookie) (*httptest.ResponseRecorder, *httptest.ResponseRecorder) {
	f.t.Helper()
	consentURL := mcpauth.ConsentPath + "?request=" + url.QueryEscape(id)
	session := f.cookieFor(f.t, f.admin.ID)
	page := f.do(http.MethodGet, consentURL, nil, nil, append([]*http.Cookie{session}, extra...)...)
	m := formNonceRE.FindStringSubmatch(page.Body.String())
	if page.Code != http.StatusOK || m == nil {
		f.t.Fatalf("consent page: %d %s", page.Code, page.Body)
	}
	form.Set("nonce", m[1])
	cookies := append(append(page.Result().Cookies(), session), extra...)
	return page, f.do(http.MethodPost, consentURL, form, nil, cookies...)
}

func (f *oauthFlowEnv) attempts(id string) int {
	f.t.Helper()
	return f.count(`SELECT code_attempts FROM mcp_oauth_requests WHERE id = ?`, id)
}

func allowForm(code string) url.Values {
	v := url.Values{"verdict": {"allow"}, "nickname": {"Profile B"}, "scope_mode": {"all"}}
	if code != "" {
		v.Set("code", code)
	}
	return v
}

// wrongCode is a well-formed code that differs from code.
func wrongCode(code string) string {
	if strings.HasPrefix(code, "222") {
		return "333-333"
	}
	return "222-222"
}

// TestStopPageShowsACodeStoredAsAHash: the stop page shows a six-symbol
// code from the unambiguous alphabet, big, with the text; only an HMAC
// keyed with the request id is stored; the code is in no link, cookie or
// header.
func TestStopPageShowsACodeStoredAsAHash(t *testing.T) {
	f := newOAuthFlowEnv(t)
	p := f.waitingPage()
	code := f.codes[p.id]
	if !regexp.MustCompile(`^[2-9A-HJKMNP-Z]{3}-[2-9A-HJKMNP-Z]{3}$`).MatchString(code) {
		t.Fatalf("code %q is not XXX-XXX from the unambiguous alphabet", code)
	}
	if !strings.Contains(p.body, `Your code: <strong class="oauth-code">`+code+`</strong>. Type it on the consent page in the other browser.`) {
		t.Fatal("the stop page does not show the code with its text")
	}
	raw := strings.ReplaceAll(code, "-", "")
	var stored string
	if err := f.db.QueryRow(`SELECT code_hash FROM mcp_oauth_requests WHERE id = ?`, p.id).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if len(stored) != 64 || strings.Contains(stored, raw) || stored == hashOf(raw) {
		t.Fatalf("stored code %q is not a keyed hash", stored)
	}
	if f.count(`SELECT COUNT(*) FROM mcp_oauth_requests WHERE code_hash = ? OR code_hash = ?`, raw, code) != 0 {
		t.Fatal("the raw code is stored")
	}
	for _, m := range htmlAttrRE.FindAllStringSubmatch(p.body, -1) {
		if strings.Contains(m[1], raw) || strings.Contains(m[1], code) {
			t.Fatalf("a link carries the code: %q", m[1])
		}
	}
	for k, vs := range p.rec.Header() {
		for _, v := range vs {
			if strings.Contains(v, raw) || strings.Contains(v, code) {
				t.Fatalf("header %s carries the code", k)
			}
		}
	}
	// Each request gets its own code (two equal by chance: 1 in 887 million).
	if other := f.waitingPage(); f.codes[other.id] == code {
		t.Fatal("two requests share one code")
	}
}

// TestConsentInTheSameBrowserNeedsNoCode: the browser that rendered the
// stop page (its Strict cookie, bound to the request) sees no code field
// and allows without one.
func TestConsentInTheSameBrowserNeedsNoCode(t *testing.T) {
	f := newOAuthFlowEnv(t)
	p := f.waitingPage()
	page, done := f.consentAs(p.id, allowForm(""), p.cookie)
	if strings.Contains(page.Body.String(), `name="code"`) || strings.Contains(page.Body.String(), codeFieldText) {
		t.Fatal("the browser that rendered the stop page is asked for the code")
	}
	if !strings.Contains(page.Body.String(), "Allow it only if you started this connection yourself") {
		t.Error("the consent page lost its warning")
	}
	if done.Code != http.StatusSeeOther || !strings.HasPrefix(done.Header().Get("Location"), mcpauth.ReturnPath) {
		t.Fatalf("same-browser allow: %d %q %s", done.Code, done.Header().Get("Location"), done.Body)
	}
	if f.attempts(p.id) != 0 {
		t.Fatal("a same-browser allow counted an attempt")
	}
}

// TestConsentInAnotherBrowserNeedsTheCode: another browser (no stop page
// cookie, or one whose MAC does not bind it to this request) sees the
// required field; no code and a wrong code are refused and counted; the
// right code, typed loosely, allows.
func TestConsentInAnotherBrowserNeedsTheCode(t *testing.T) {
	f := newOAuthFlowEnv(t)
	p := f.waitingPage()
	code := f.codes[p.id]
	forged := &http.Cookie{Name: p.cookie.Name, Value: "forged.value"}
	for name, extra := range map[string][]*http.Cookie{"no stop page cookie": nil, "forged stop page cookie": {forged}} {
		page, done := f.consentAs(p.id, allowForm(""), extra...)
		body := page.Body.String()
		if !strings.Contains(body, `name="code" required`) || !strings.Contains(body, codeFieldText) ||
			!strings.Contains(body, "Code from the window where you started") {
			t.Fatalf("%s: the consent page lacks the code field", name)
		}
		if done.Code != http.StatusBadRequest || !strings.Contains(done.Body.String(), "That code is not right") || done.Header().Get("Location") != "" {
			t.Fatalf("%s: allow without a code: %d %s", name, done.Code, done.Body)
		}
	}
	if n := f.attempts(p.id); n != 2 {
		t.Fatalf("%d attempts counted, want 2", n)
	}
	_, done := f.consentAs(p.id, allowForm(wrongCode(code)))
	if done.Code != http.StatusBadRequest || !strings.Contains(done.Body.String(), "That code is not right") {
		t.Fatalf("allow with a wrong code: %d %s", done.Code, done.Body)
	}
	if strings.Contains(done.Body.String(), wrongCode(code)) {
		t.Error("the page shows the typed code again")
	}
	if n := f.attempts(p.id); n != 3 {
		t.Fatalf("%d attempts counted, want 3", n)
	}
	if f.count(`SELECT COUNT(*) FROM mcp_oauth_return_tickets`)+f.count(`SELECT COUNT(*) FROM mcp_oauth_grants`) != 0 {
		t.Fatal("a refused code allowed the app")
	}
	loose := " " + strings.ToLower(strings.ReplaceAll(code, "-", " ")) + " "
	_, done = f.consentAs(p.id, allowForm(loose))
	if done.Code != http.StatusSeeOther || !strings.HasPrefix(done.Header().Get("Location"), mcpauth.ReturnPath) {
		t.Fatalf("allow with the right code: %d %s", done.Code, done.Body)
	}
	if st := f.pollStatus(p); st["status"] != mcpauth.WaitApproved {
		t.Fatalf("the waiting page after the right code: %v", st)
	}
}

// TestFifthWrongCodeEndsTheRequest: four wrong codes leave the request
// open; the fifth ends it, so even the right code is refused afterwards,
// the waiting page reports expired, and an audit row records it.
func TestFifthWrongCodeEndsTheRequest(t *testing.T) {
	f := newOAuthFlowEnv(t)
	p := f.waitingPage()
	code := f.codes[p.id]
	for i := 1; i < mcpauth.MaxCodeAttempts; i++ {
		if _, done := f.consentAs(p.id, allowForm(wrongCode(code))); done.Code != http.StatusBadRequest {
			t.Fatalf("wrong code %d: %d %s", i, done.Code, done.Body)
		}
	}
	if st := f.pollStatus(p); st["status"] != mcpauth.WaitPending {
		t.Fatalf("after %d wrong codes: %v, want pending", mcpauth.MaxCodeAttempts-1, st)
	}
	_, done := f.consentAs(p.id, allowForm(wrongCode(code)))
	if done.Code != http.StatusForbidden || !strings.Contains(done.Body.String(), "Too many wrong codes") ||
		!strings.Contains(done.Body.String(), "Start again from Example Client") {
		t.Fatalf("fifth wrong code: %d %s", done.Code, done.Body)
	}
	if page := f.do(http.MethodGet, mcpauth.ConsentPath+"?request="+p.id, nil, nil, f.cookieFor(t, f.admin.ID)); page.Code != http.StatusBadRequest ||
		!strings.Contains(page.Body.String(), "expired or was already answered") {
		t.Fatalf("the consent page after the request ended: %d %s", page.Code, page.Body)
	}
	if st := f.pollStatus(p); st["status"] != mcpauth.WaitExpired {
		t.Fatalf("the waiting page after the request ended: %v, want expired", st)
	}
	if r := f.cont(p, p.waiter, p.nonce, "same-origin", p.cookie); r.Code != http.StatusGone {
		t.Fatalf("Continue after the request ended: %d %s", r.Code, r.Body)
	}
	if f.count(`SELECT COUNT(*) FROM audit_events WHERE event_type = 'mcp_oauth.code_locked'`) != 1 {
		t.Fatal("no audit row for the ended request")
	}
	if f.count(`SELECT COUNT(*) FROM mcp_oauth_return_tickets`)+f.count(`SELECT COUNT(*) FROM mcp_oauth_codes`) != 0 {
		t.Fatal("an ended request left an approval or a code")
	}
}

// TestDenyNeedsNoCode: a deny from another browser needs no code and
// counts no attempt.
func TestDenyNeedsNoCode(t *testing.T) {
	f := newOAuthFlowEnv(t)
	p := f.waitingPage()
	_, done := f.consentAs(p.id, url.Values{"verdict": {"deny"}})
	if done.Code != http.StatusOK || !strings.Contains(done.Body.String(), "You did not allow Example Client") {
		t.Fatalf("deny without a code: %d %s", done.Code, done.Body)
	}
	if f.attempts(p.id) != 0 {
		t.Fatal("a deny counted a code attempt")
	}
	if st := f.pollStatus(p); st["status"] != mcpauth.WaitDenied {
		t.Fatalf("after deny: %v", st)
	}
}

// TestRequestWithoutACodeNeedsNone: a request stored before codes existed
// (no code hash) is allowed from another browser as before.
func TestRequestWithoutACodeNeedsNone(t *testing.T) {
	f := newOAuthFlowEnv(t)
	id := f.pending()
	if _, err := f.db.Exec(`UPDATE mcp_oauth_requests SET code_hash = NULL WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	page, done := f.consentAs(id, allowForm(""))
	if strings.Contains(page.Body.String(), `name="code"`) {
		t.Fatal("a request without a code asks for one")
	}
	if done.Code != http.StatusSeeOther || !strings.HasPrefix(done.Header().Get("Location"), mcpauth.ReturnPath) {
		t.Fatalf("allow of a request without a code: %d %s", done.Code, done.Body)
	}
}

// TestCodeStaysOutOfLinksCookiesAndLogs walks a wrong and a right code
// with the logs captured: the code is in no log line, audit row, cookie,
// header or later page.
func TestCodeStaysOutOfLinksCookiesAndLogs(t *testing.T) {
	var logs bytes.Buffer
	prevLog, prevFlags, prevSlog := log.Writer(), log.Flags(), slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	log.SetOutput(&logs)
	t.Cleanup(func() {
		slog.SetDefault(prevSlog)
		log.SetOutput(prevLog)
		log.SetFlags(prevFlags)
	})
	f := newOAuthFlowEnv(t)
	p := f.waitingPage()
	code := f.codes[p.id]
	raw := strings.ReplaceAll(code, "-", "")
	var seen []*httptest.ResponseRecorder
	page, wrong := f.consentAs(p.id, allowForm(wrongCode(code)))
	seen = append(seen, page, wrong)
	page, done := f.consentAs(p.id, allowForm(code))
	seen = append(seen, page, done)
	ret := f.do(http.MethodGet, done.Header().Get("Location"), nil, nil)
	seen = append(seen, ret)
	if done.Code != http.StatusSeeOther || ret.Code != http.StatusOK {
		t.Fatalf("flow: %d %d", done.Code, ret.Code)
	}
	for i, rec := range seen {
		for k, vs := range rec.Header() {
			for _, v := range vs {
				if strings.Contains(v, raw) || strings.Contains(v, code) {
					t.Errorf("response %d: header %s carries the code", i, k)
				}
			}
		}
		if strings.Contains(rec.Body.String(), raw) || strings.Contains(rec.Body.String(), code) {
			t.Errorf("response %d shows the code", i)
		}
	}
	rows, err := f.db.Query(`SELECT COALESCE(arguments, ''), COALESCE(reason, ''), COALESCE(result_summary, '') FROM audit_events`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var a, b, c string
		if err := rows.Scan(&a, &b, &c); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(a+b+c, raw) {
			t.Error("an audit row carries the code")
		}
	}
	if strings.Contains(logs.String(), raw) || strings.Contains(logs.String(), code) {
		t.Error("a log line carries the code")
	}
}
