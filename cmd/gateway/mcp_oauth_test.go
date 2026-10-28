package main

// End-to-end tests of OAuth for MCP clients through the real handler chain
// (security headers, Origin check, API hardening, role guard, mux): the
// metadata documents, registration, the browser's authorize and consent
// steps with a signed-in session, the token endpoint, and /mcp with the
// resulting access token, including the per-agent scope the consent chose.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/server"

	"github.com/tusharbhardwaj/toolyard/internal/access"
	"github.com/tusharbhardwaj/toolyard/internal/api"
	"github.com/tusharbhardwaj/toolyard/internal/approval"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/federation"
	"github.com/tusharbhardwaj/toolyard/internal/gateway"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/mcpauth"
	"github.com/tusharbhardwaj/toolyard/internal/memory"
	"github.com/tusharbhardwaj/toolyard/internal/policy"
	"github.com/tusharbhardwaj/toolyard/internal/realtime"
	"github.com/tusharbhardwaj/toolyard/internal/sealbox"
	"github.com/tusharbhardwaj/toolyard/internal/store"
	"github.com/tusharbhardwaj/toolyard/internal/store/storetest"
)

const oauthPublic = "https://toolyard.example"

type oauthEnv struct {
	t       *testing.T
	db      *store.DB
	ids     *identity.Service
	svc     *mcpauth.Service
	user    *identity.User
	handler http.Handler
	session []*http.Cookie
	cimd    *httptest.Server
	// offset moves the authorization server's clock (refresh grace).
	offset time.Duration
}

const cimdRedirect = "http://127.0.0.1:43999/cb"

func newOAuthEnv(t *testing.T) *oauthEnv {
	t.Helper()
	ctx := context.Background()
	db := storetest.Open(t)
	ids := identity.New(db)
	u, err := ids.CreateUser(ctx, "owner", "long-enough-password")
	if err != nil {
		t.Fatal(err)
	}
	accessSvc := access.New(db)
	auditLog := audit.New(db)
	bus, err := approval.New(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	gw := gateway.New(gateway.Options{Policy: policy.New(db), Approval: bus, Audit: auditLog, Memory: memory.New(db), Hub: realtime.NewHub(), Access: accessSvc})
	gw.RegisterBuiltins()
	t.Cleanup(func() { _ = gw.Close() })
	cipher, err := sealbox.NewCipher(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	fed, err := federation.New(ctx, db, ids, cipher)
	if err != nil {
		t.Fatal(err)
	}
	var cimd *httptest.Server
	cimd = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"client_id": cimd.URL + r.URL.Path, "client_name": "CIMD Test Client",
			"redirect_uris": []string{cimdRedirect}, "token_endpoint_auth_method": "none",
		})
	}))
	t.Cleanup(cimd.Close)
	pool := x509.NewCertPool()
	pool.AddCert(cimd.Certificate())
	e := &oauthEnv{t: t, db: db, ids: ids, user: u, cimd: cimd}
	svc := mcpauth.New(mcpauth.Options{DB: db, PublicURL: oauthPublic, Access: accessSvc, AllowPrivateCIMD: true, CIMDRootCAs: pool,
		Now: func() time.Time { return time.Now().Add(e.offset) }})
	e.svc = svc
	sec := api.SecurityOptions{PublicURL: oauthPublic}
	apiSrv := api.New(ctx, api.Options{Identity: ids, Audit: auditLog, Approval: bus, Gateway: gw, Access: accessSvc, Federation: fed,
		SessionKey: []byte("0123456789abcdef0123456789abcdef"), Security: sec})
	mux := http.NewServeMux()
	apiSrv.Routes(mux)
	apiSrv.MCPOAuthRoutes(mux, svc)
	auth := mcpAuth{verify: fed.VerifyAgent, verifyOAuth: svc.VerifyAccessToken, resourceMetadata: svc.ResourceMetadataURL, requireAuth: true, sec: sec}
	streamable := server.NewStreamableHTTPServer(gw.MCPServer(), server.WithEndpointPath("/mcp"), server.WithStateLess(true),
		server.WithDisableStreaming(true), server.WithDisableLocalhostProtection(true), server.WithHTTPContextFunc(auth.contextFunc))
	mux.Handle("/mcp", gateway.PreserveToolCallNumbers(auth.guard(streamable)))
	mux.Handle("/", staticHandler())
	e.handler = apiSrv.SecurityHeaders(apiSrv.EnforceOriginOnMutations(apiSrv.HardenAPI(apiSrv.RoleGuard(mux))))
	login := e.do(http.MethodPost, "/v1/auth/login", strings.NewReader(`{"Username":"owner","Password":"long-enough-password"}`),
		map[string]string{"Content-Type": "application/json", "Origin": oauthPublic})
	if login.Code != http.StatusOK || len(login.Result().Cookies()) == 0 {
		t.Fatalf("login: %d %s", login.Code, login.Body)
	}
	e.session = login.Result().Cookies()
	return e
}

func (e *oauthEnv) do(method, target string, body *strings.Reader, hdr map[string]string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	e.t.Helper()
	var r *http.Request
	if body == nil {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, body)
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	for _, c := range cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	e.handler.ServeHTTP(w, r)
	return w
}

// rpc posts one JSON-RPC request to /mcp with the bearer token.
func (e *oauthEnv) rpc(token, method string, params any) (*httptest.ResponseRecorder, map[string]any) {
	e.t.Helper()
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	w := e.do(http.MethodPost, "/mcp", strings.NewReader(string(raw)), map[string]string{
		"Authorization": "Bearer " + token, "Content-Type": "application/json", "Accept": "application/json, text/event-stream",
	})
	if w.Code != http.StatusOK {
		return w, nil
	}
	body := w.Body.String()
	if strings.Contains(w.Header().Get("Content-Type"), "text/event-stream") {
		for _, line := range strings.Split(body, "\n") {
			if strings.HasPrefix(line, "data:") {
				body = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			}
		}
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		e.t.Fatalf("%s: %v in %q", method, err, body)
	}
	return w, out
}

func (e *oauthEnv) toolNames(token string) map[string]bool {
	e.t.Helper()
	w, out := e.rpc(token, "tools/list", map[string]any{})
	if out == nil || out["result"] == nil {
		e.t.Fatalf("tools/list: %d %s", w.Code, w.Body)
	}
	names := map[string]bool{}
	for _, t := range out["result"].(map[string]any)["tools"].([]any) {
		names[t.(map[string]any)["name"].(string)] = true
	}
	// An agent's tools/list shows only the small tool list (TEC-1807); the
	// rest of what it may use is what tools.search finds.
	if msg, res := e.call(token, "tools.search", map[string]any{"query": "", "detail": "name", "limit": 200}); msg == "" && res != nil {
		sc, _ := res["structuredContent"].(map[string]any)
		rs, _ := sc["results"].([]any)
		for _, r := range rs {
			if m, ok := r.(map[string]any); ok {
				if n, ok := m["name"].(string); ok {
					names[n] = true
				}
			}
		}
	}
	return names
}

// call runs tools/call and returns the JSON-RPC error message, or "" with
// the result.
func (e *oauthEnv) call(token, tool string, args map[string]any) (string, map[string]any) {
	e.t.Helper()
	args["_reason"] = "Check that this connection's tool scope is enforced end to end"
	w, out := e.rpc(token, "tools/call", map[string]any{"name": tool, "arguments": args})
	if out == nil {
		e.t.Fatalf("tools/call %s: %d %s", tool, w.Code, w.Body)
	}
	if errObj, ok := out["error"].(map[string]any); ok {
		msg, _ := errObj["message"].(string)
		return msg, nil
	}
	res, _ := out["result"].(map[string]any)
	return "", res
}

func resultText(res map[string]any) string {
	var b strings.Builder
	for _, c := range res["content"].([]any) {
		if s, ok := c.(map[string]any)["text"].(string); ok {
			b.WriteString(s)
		}
	}
	return b.String()
}

func newPKCE(t *testing.T) (string, string) {
	t.Helper()
	b := make([]byte, 40)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	v := base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(v))
	return v, base64.RawURLEncoding.EncodeToString(sum[:])
}

func (e *oauthEnv) form(path string, v url.Values) (*httptest.ResponseRecorder, map[string]any) {
	e.t.Helper()
	w := e.do(http.MethodPost, path, strings.NewReader(v.Encode()), map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w, out
}

var (
	nonceRE    = regexp.MustCompile(`name="nonce" value="([^"]+)"`)
	stopLinkRE = regexp.MustCompile(`href="(/oauth/consent\?request=[A-Za-z0-9_-]{43})"`)
	denyLinkRE = regexp.MustCompile(`<a class="btn primary" href="([^"]+)">Continue in this browser</a>`)
)

// consent walks authorize (the stop page) → consent page → consent POST →
// return page → its Continue, and returns the redirect the browser is
// finally sent to. A deny stops on a page of ours; it returns that page's
// link to the client.
func (e *oauthEnv) consent(clientID, redirect, challenge string, form url.Values) *url.URL {
	e.t.Helper()
	q := url.Values{"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {redirect}, "state": {"xyz"},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"}, "resource": {oauthPublic + "/mcp"}, "scope": {"mcp"}}
	// Without the session cookie (a cross-site navigation under SameSite=
	// Strict) and with it, authorize stops on a page of ours: continue to
	// the consent step here, or copy its link into another browser.
	var consentURL string
	for _, cookies := range [][]*http.Cookie{nil, e.session} {
		stop := e.do(http.MethodGet, "/oauth/authorize?"+q.Encode(), nil, nil, cookies...)
		body := stop.Body.String()
		m := stopLinkRE.FindStringSubmatch(body)
		if stop.Code != http.StatusOK || stop.Header().Get("Location") != "" || m == nil || strings.Contains(body, `http-equiv="refresh"`) ||
			!strings.Contains(body, "Continue in this browser") || !strings.Contains(body, `value="`+oauthPublic+m[1]+`"`) {
			e.t.Fatalf("authorize stop page (session cookies: %d): %d %s", len(cookies), stop.Code, body)
		}
		consentURL = m[1]
	}
	if anon := e.do(http.MethodGet, consentURL, nil, nil); anon.Code != http.StatusUnauthorized || !strings.Contains(anon.Body.String(), "/login?oauth_request=") {
		e.t.Fatalf("consent without a session: %d %s", anon.Code, anon.Body)
	}
	page := e.do(http.MethodGet, consentURL, nil, nil, e.session...)
	if page.Code != http.StatusOK {
		e.t.Fatalf("consent page: %d %s", page.Code, page.Body)
	}
	// The consent POST answers with a page of ours, never the client.
	if csp := page.Header().Get("Content-Security-Policy"); !strings.HasSuffix(csp, "form-action 'self'") {
		e.t.Fatalf("consent CSP allows posting elsewhere: %q", csp)
	}
	if !strings.Contains(page.Body.String(), mcpauth.RedirectHost(redirect)) {
		e.t.Fatalf("consent page does not show the redirect host %q", mcpauth.RedirectHost(redirect))
	}
	// Who the app is: its document's host, or self-registered; the name
	// is labelled as its own claim; the admin sees the manage switch, off.
	body := page.Body.String()
	label := "Self-registered app"
	if strings.HasPrefix(clientID, "https://") {
		u, _ := url.Parse(clientID)
		label = "Identified by <strong>" + u.Hostname() + "</strong>"
	}
	if !strings.Contains(body, label) || !strings.Contains(body, "(not checked)") || strings.Contains(body, "Verified") {
		e.t.Fatalf("consent page trust labels: want %q and a not-checked name", label)
	}
	if !strings.Contains(body, `name="may_manage" value="1">`) {
		e.t.Fatal("the admin's consent page lacks the manage switch, or it is on by default")
	}
	m := nonceRE.FindStringSubmatch(page.Body.String())
	var nonceCookie *http.Cookie
	for _, c := range page.Result().Cookies() {
		if strings.HasPrefix(c.Name, "ty_oauth_") {
			nonceCookie = c
		}
	}
	if len(m) != 2 || nonceCookie == nil {
		e.t.Fatal("consent nonce missing")
	}
	post := func(target string, v url.Values, origin, site string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
		return e.do(http.MethodPost, target, strings.NewReader(v.Encode()), map[string]string{
			"Content-Type": "application/x-www-form-urlencoded", "Origin": origin, "Sec-Fetch-Site": site}, cookies...)
	}
	form.Set("nonce", m[1])
	if bad := post(consentURL, form, "https://evil.example", "same-origin", append(e.session, nonceCookie)...); bad.Code != http.StatusForbidden {
		e.t.Fatalf("cross-origin consent: %d", bad.Code)
	}
	if bad := post(consentURL, form, oauthPublic, "same-origin", e.session...); bad.Code != http.StatusForbidden {
		e.t.Fatalf("consent without the nonce cookie: %d", bad.Code)
	}
	done := post(consentURL, form, oauthPublic, "same-origin", append(e.session, nonceCookie)...)
	checkClientRedirect := func(raw string) *url.URL {
		e.t.Helper()
		loc, err := url.Parse(raw)
		if err != nil || !strings.HasPrefix(raw, redirect) || loc.Query().Get("state") != "xyz" || loc.Query().Get("iss") != oauthPublic {
			e.t.Fatalf("redirect to the client %q", raw)
		}
		return loc
	}
	if form.Get("verdict") == "deny" {
		dm := denyLinkRE.FindStringSubmatch(done.Body.String())
		if done.Code != http.StatusOK || done.Header().Get("Location") != "" || dm == nil {
			e.t.Fatalf("deny page: %d %q %s", done.Code, done.Header().Get("Location"), done.Body)
		}
		return checkClientRedirect(strings.ReplaceAll(dm[1], "&amp;", "&"))
	}
	returnURL := done.Header().Get("Location")
	if done.Code != http.StatusSeeOther || !strings.HasPrefix(returnURL, mcpauth.ReturnPath) {
		e.t.Fatalf("consent submit: %d %q %s", done.Code, returnURL, done.Body)
	}
	// The return page needs no toolyard session: the browser where the
	// client waits may not be signed in.
	ret := e.do(http.MethodGet, returnURL, nil, nil)
	if ret.Code != http.StatusOK || !strings.Contains(ret.Body.String(), "Approved. Return to") ||
		!strings.Contains(ret.Body.String(), `value="`+oauthPublic+returnURL+`"`) || !strings.Contains(ret.Body.String(), mcpauth.RedirectHost(redirect)) {
		e.t.Fatalf("return page: %d %s", ret.Code, ret.Body)
	}
	if csp := ret.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "form-action 'self' "+mcpauth.CSPSource(redirect)) {
		e.t.Fatalf("return page CSP does not allow the redirect target: %q", csp)
	}
	rm := nonceRE.FindStringSubmatch(ret.Body.String())
	var retCookie *http.Cookie
	for _, c := range ret.Result().Cookies() {
		if strings.HasPrefix(c.Name, "toolyard_oauth_return_") {
			retCookie = c
		}
	}
	if len(rm) != 2 || retCookie == nil {
		e.t.Fatal("return page nonce missing")
	}
	nonce := url.Values{"nonce": {rm[1]}}
	if bad := post(returnURL, nonce, "https://evil.example", "same-origin", retCookie); bad.Code != http.StatusForbidden {
		e.t.Fatalf("cross-origin return: %d", bad.Code)
	}
	if bad := post(returnURL, nonce, oauthPublic, "cross-site", retCookie); bad.Code != http.StatusForbidden {
		e.t.Fatalf("cross-site return: %d", bad.Code)
	}
	if bad := post(returnURL, nonce, oauthPublic, "same-origin"); bad.Code != http.StatusForbidden {
		e.t.Fatalf("return without the nonce cookie: %d", bad.Code)
	}
	back := post(returnURL, nonce, oauthPublic, "same-origin", retCookie)
	if back.Code != http.StatusSeeOther {
		e.t.Fatalf("return continue: %d %s", back.Code, back.Body)
	}
	return checkClientRedirect(back.Header().Get("Location"))
}

func TestMCPOAuthDiscovery(t *testing.T) {
	e := newOAuthEnv(t)
	pr := e.do(http.MethodGet, "/.well-known/oauth-protected-resource", nil, nil)
	var doc map[string]any
	if err := json.Unmarshal(pr.Body.Bytes(), &doc); err != nil || pr.Code != 200 || doc["resource"] != oauthPublic+"/mcp" ||
		doc["authorization_servers"].([]any)[0] != oauthPublic {
		t.Fatalf("protected resource: %d %s", pr.Code, pr.Body)
	}
	if w := e.do(http.MethodGet, "/.well-known/oauth-protected-resource/mcp", nil, nil); w.Code != 200 || !strings.Contains(w.Body.String(), oauthPublic+"/mcp") {
		t.Fatalf("protected resource (path form): %d", w.Code)
	}
	as := e.do(http.MethodGet, "/.well-known/oauth-authorization-server", nil, nil)
	doc = nil
	if err := json.Unmarshal(as.Body.Bytes(), &doc); err != nil || as.Code != 200 || doc["issuer"] != oauthPublic ||
		doc["token_endpoint"] != oauthPublic+"/oauth/token" || doc["registration_endpoint"] != oauthPublic+"/oauth/register" ||
		doc["client_id_metadata_document_supported"] != true {
		t.Fatalf("authorization server: %d %s", as.Code, as.Body)
	}
	for _, p := range []string{"/.well-known/x", "/.well-known/openid-configuration", "/oauth/unknown", "/register", "/token"} {
		if w := e.do(http.MethodGet, p, nil, nil); w.Code != http.StatusNotFound || strings.Contains(w.Body.String(), "<html") {
			t.Errorf("%s: %d, want a plain 404", p, w.Code)
		}
	}
	if w := e.do(http.MethodGet, "/.well-known/toolyard-instance", nil, nil); w.Code != 200 || !strings.Contains(w.Body.String(), "instance_id") {
		t.Fatalf("federation discovery broke: %d %s", w.Code, w.Body)
	}
	// Every 401 from /mcp points at the protected-resource metadata.
	prm := `resource_metadata="` + oauthPublic + `/.well-known/oauth-protected-resource"`
	for name, tok := range map[string]string{"no token": "", "bad OAuth token": "tyat_bogus", "bad agent token": "ag_x.y"} {
		hdr := map[string]string{"Content-Type": "application/json"}
		if tok != "" {
			hdr["Authorization"] = "Bearer " + tok
		}
		w := e.do(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`), hdr)
		h := w.Header().Get("WWW-Authenticate")
		if w.Code != http.StatusUnauthorized || !strings.HasPrefix(h, "Bearer ") || !strings.Contains(h, prm) || !strings.Contains(w.Body.String(), `"jsonrpc":"2.0"`) {
			t.Errorf("%s: %d %q %s", name, w.Code, h, w.Body)
		}
		if (tok != "") != strings.Contains(h, `error="invalid_token"`) {
			t.Errorf("%s: error=invalid_token presence wrong in %q", name, h)
		}
	}
}

func TestMCPOAuthFullFlowWithDynamicRegistration(t *testing.T) {
	e := newOAuthEnv(t)
	ctx := context.Background()
	redirect := "http://127.0.0.1:43123/callback"
	reg := e.do(http.MethodPost, "/oauth/register", strings.NewReader(`{"redirect_uris":["`+redirect+`"],"token_endpoint_auth_method":"none","client_name":"Claude Code (test)","grant_types":["authorization_code","refresh_token"],"response_types":["code"]}`),
		map[string]string{"Content-Type": "application/json"})
	var client map[string]any
	if err := json.Unmarshal(reg.Body.Bytes(), &client); err != nil || reg.Code != http.StatusCreated || client["client_id"] == "" {
		t.Fatalf("register: %d %s", reg.Code, reg.Body)
	}
	clientID := client["client_id"].(string)

	verifier, challenge := newPKCE(t)
	loc := e.consent(clientID, redirect, challenge, url.Values{
		"verdict": {"allow"}, "nickname": {"My Claude Code"}, "scope_mode": {"listed"},
		"allow_server": {"memory"}, "deny_tool": {"memory.delete"},
	})
	w, tok := e.form("/oauth/token", url.Values{"grant_type": {"authorization_code"}, "code": {loc.Query().Get("code")},
		"redirect_uri": {redirect}, "code_verifier": {verifier}, "client_id": {clientID}, "resource": {oauthPublic + "/mcp"}})
	if w.Code != 200 || tok["access_token"] == nil || tok["refresh_token"] == nil || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("token: %d %s", w.Code, w.Body)
	}
	access1, refresh1 := tok["access_token"].(string), tok["refresh_token"].(string)

	// tools/list shows what the consent allowed, plus the always-on tools.
	names := e.toolNames(access1)
	for _, want := range []string{"memory.get", "memory.set", "access.whoami", "tools.search"} {
		if !names[want] {
			t.Errorf("tools/list lacks %s", want)
		}
	}
	for _, hidden := range []string{"memory.delete", "fixture.echo"} {
		if names[hidden] {
			t.Errorf("tools/list shows %s, which the consent did not allow", hidden)
		}
	}
	// A denied or not-allowed tool is refused with a clear error.
	for _, tool := range []string{"memory.delete", "fixture.echo"} {
		msg, _ := e.call(access1, tool, map[string]any{"key": "k", "message": "hi"})
		if !strings.Contains(msg, "not allowed for this agent") {
			t.Errorf("%s: got %q, want a clear refusal", tool, msg)
		}
	}
	if msg, res := e.call(access1, "memory.get", map[string]any{"key": "nothing-here"}); msg != "" || res == nil || strings.Contains(resultText(res), "not allowed") {
		t.Fatalf("an allowed tool was refused: %q %v", msg, res)
	}
	// Audit rows name the agent (the nickname), its kind and its owner.
	apps, err := e.svc.ListApps(ctx, e.user.ID)
	if err != nil || len(apps) != 1 || apps[0].Nickname != "My Claude Code" || apps[0].ClientID != clientID || apps[0].ClientKind != mcpauth.ClientKindDCR {
		t.Fatalf("apps: %+v %v", apps, err)
	}
	agentID := apps[0].AgentID
	rows, err := e.db.Query(`SELECT event_type, COALESCE(agent_name,''), COALESCE(agent_kind,''), COALESCE(owner_user_id,''), COALESCE(reason,'') FROM audit_events WHERE agent_id = ?`, agentID)
	if err != nil {
		t.Fatal(err)
	}
	var calls, limited int
	for rows.Next() {
		var ev, name, kind, owner, reason string
		if err := rows.Scan(&ev, &name, &kind, &owner, &reason); err != nil {
			t.Fatal(err)
		}
		if name != "My Claude Code" || kind != mcpauth.AgentKindOAuthClient || owner != e.user.ID {
			t.Errorf("audit row %s names %q (%q) owned by %q", ev, name, kind, owner)
		}
		if strings.HasPrefix(ev, "call.") {
			calls++
		}
		if reason == "access: not allowed for this agent" {
			limited++
		}
	}
	rows.Close()
	if calls < 3 || limited != 2 {
		t.Fatalf("audit: %d call rows, %d limited refusals", calls, limited)
	}
	var missing int
	if err := e.db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE agent_id = ? AND event_type LIKE 'call.%' AND COALESCE(oauth_client_id, '') <> ?`, agentID, clientID).Scan(&missing); err != nil || missing != 0 {
		t.Fatalf("%d call rows lack the OAuth client id (%v)", missing, err)
	}
	// Without may_manage, an admin's app uses tools but has no admin control.
	_, who := e.call(access1, "access.whoami", map[string]any{})
	if sc, _ := who["structuredContent"].(map[string]any); sc["can_change_policies"] != false {
		t.Fatalf("an admin's app may change policies by default: %v", who)
	}
	if msg, res := e.call(access1, "policies.set", map[string]any{"scope": "tool", "target": "memory.get", "access": "deny"}); msg != "" ||
		res == nil || !strings.Contains(resultText(res), "has not let this agent manage toolyard") {
		t.Fatalf("policies.set from an app without may_manage: %q %v", msg, res)
	}

	// The Connected apps API renames the app; the next call carries it.
	rest := func(method, path, body string) *httptest.ResponseRecorder {
		var rd *strings.Reader
		if body != "" {
			rd = strings.NewReader(body)
		}
		return e.do(method, path, rd, map[string]string{"Content-Type": "application/json", "X-Requested-With": "toolyard", "Origin": oauthPublic}, e.session...)
	}
	if w := rest(http.MethodGet, "/v1/me/apps", ""); w.Code != 200 || !strings.Contains(w.Body.String(), "My Claude Code") {
		t.Fatalf("apps list: %d %s", w.Code, w.Body)
	}
	if w := rest(http.MethodPost, "/v1/me/apps/rename", `{"id":"`+apps[0].ID+`","nickname":"Laptop Claude"}`); w.Code != 200 || !strings.Contains(w.Body.String(), "Laptop Claude") {
		t.Fatalf("rename: %d %s", w.Code, w.Body)
	}
	if msg, _ := e.call(access1, "memory.get", map[string]any{"key": "again"}); msg != "" {
		t.Fatal(msg)
	}
	var renamed int
	if err := e.db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE agent_id = ? AND event_type LIKE 'call.%' AND agent_name = 'Laptop Claude'`, agentID).Scan(&renamed); err != nil || renamed == 0 {
		t.Fatalf("no call row carries the new nickname: %d %v", renamed, err)
	}
	if w := rest(http.MethodGet, "/v1/me/apps/catalog?id="+apps[0].ID, ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"name":"memory"`) {
		t.Fatalf("catalog: %d %s", w.Code, w.Body)
	}
	if w := rest(http.MethodGet, "/v1/me/apps/audit?id="+apps[0].ID, ""); w.Code != 200 || !strings.Contains(w.Body.String(), "not allowed for this agent") {
		t.Fatalf("app audit: %d %s", w.Code, w.Body)
	}

	// Refresh rotates; the old refresh token used again revokes the grant.
	w, tok = e.form("/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh1}, "client_id": {clientID}})
	if w.Code != 200 || tok["refresh_token"] == refresh1 || tok["access_token"] == access1 {
		t.Fatalf("refresh: %d %s", w.Code, w.Body)
	}
	access2, refresh2 := tok["access_token"].(string), tok["refresh_token"].(string)
	if names := e.toolNames(access2); !names["memory.get"] {
		t.Fatal("the rotated access token does not work")
	}
	// A retry within the grace is refused without revoking anything.
	w, tok = e.form("/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh1}, "client_id": {clientID}})
	if w.Code != 400 || tok["error"] != "invalid_grant" {
		t.Fatalf("refresh token retry: %d %s", w.Code, w.Body)
	}
	if names := e.toolNames(access2); !names["memory.get"] {
		t.Fatal("a retry within the grace revoked the grant")
	}
	e.offset += 61 * time.Second // past the refresh reuse grace
	w, tok = e.form("/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh1}, "client_id": {clientID}})
	if w.Code != 400 || tok["error"] != "invalid_grant" {
		t.Fatalf("refresh token reuse: %d %s", w.Code, w.Body)
	}
	if w, _ := e.rpc(access2, "tools/list", map[string]any{}); w.Code != http.StatusUnauthorized || !strings.Contains(w.Header().Get("WWW-Authenticate"), `error="invalid_token"`) {
		t.Fatalf("access token after reuse detection: %d", w.Code)
	}
	if w, _ := e.form("/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh2}, "client_id": {clientID}}); w.Code != 400 {
		t.Fatalf("newest refresh token after reuse detection: %d", w.Code)
	}

	// Connecting again asks for consent again, then revocation ends it.
	verifier, challenge = newPKCE(t)
	loc = e.consent(clientID, redirect, challenge, url.Values{"verdict": {"allow"}, "nickname": {"Claude again"}, "scope_mode": {"all"}})
	w, tok = e.form("/oauth/token", url.Values{"grant_type": {"authorization_code"}, "code": {loc.Query().Get("code")},
		"redirect_uri": {redirect}, "code_verifier": {verifier}, "client_id": {clientID}})
	if w.Code != 200 {
		t.Fatalf("token after reconnect: %d %s", w.Code, w.Body)
	}
	access3, refresh3 := tok["access_token"].(string), tok["refresh_token"].(string)
	if names := e.toolNames(access3); !names["fixture.echo"] || !names["memory.delete"] {
		t.Fatal("scope 'everything' did not restore every tool")
	}
	if w, _ := e.form("/oauth/revoke", url.Values{"token": {access3}, "client_id": {clientID}}); w.Code != 200 {
		t.Fatalf("revoke access token: %d", w.Code)
	}
	if w, _ := e.rpc(access3, "tools/list", map[string]any{}); w.Code != http.StatusUnauthorized {
		t.Fatalf("revoked access token still works: %d", w.Code)
	}
	if w, _ := e.form("/oauth/revoke", url.Values{"token": {refresh3}, "token_type_hint": {"refresh_token"}, "client_id": {clientID}}); w.Code != 200 {
		t.Fatalf("revoke refresh token: %d", w.Code)
	}
	if w, _ := e.form("/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh3}, "client_id": {clientID}}); w.Code != 400 {
		t.Fatalf("revoked refresh token still works: %d", w.Code)
	}
	if apps, _ := e.svc.ListApps(ctx, e.user.ID); len(apps) != 1 || apps[0].AgentID != agentID || apps[0].Status != "revoked" {
		t.Fatalf("apps after revoke: %+v", apps)
	}
}

func TestMCPOAuthFullFlowWithMetadataDocument(t *testing.T) {
	e := newOAuthEnv(t)
	clientID := e.cimd.URL + "/oauth/client.json"
	verifier, challenge := newPKCE(t)
	loc := e.consent(clientID, cimdRedirect, challenge, url.Values{"verdict": {"allow"}, "nickname": {"Doc client"}, "scope_mode": {"all"}, "deny_server": {"fixture"},
		"may_manage": {"1"}})
	w, tok := e.form("/oauth/token", url.Values{"grant_type": {"authorization_code"}, "code": {loc.Query().Get("code")},
		"redirect_uri": {cimdRedirect}, "code_verifier": {verifier}, "client_id": {clientID}})
	if w.Code != 200 {
		t.Fatalf("token: %d %s", w.Code, w.Body)
	}
	names := e.toolNames(tok["access_token"].(string))
	if !names["memory.delete"] || names["fixture.echo"] {
		t.Fatalf("scope 'everything except fixture' not applied: memory.delete=%v fixture.echo=%v", names["memory.delete"], names["fixture.echo"])
	}
	apps, _ := e.svc.ListApps(context.Background(), e.user.ID)
	if len(apps) != 1 || apps[0].ClientKind != mcpauth.ClientKindCIMD || apps[0].ClientName != "CIMD Test Client" || !apps[0].MayManage {
		t.Fatalf("apps: %+v", apps)
	}
	// The admin let this app manage toolyard on the consent screen.
	_, who := e.call(tok["access_token"].(string), "access.whoami", map[string]any{})
	if sc, _ := who["structuredContent"].(map[string]any); sc["can_change_policies"] != true {
		t.Fatalf("an app with may_manage cannot change policies: %v", who)
	}
}

// TestMCPOAuthNoOpenRedirect: a client that registered someone else's site
// cannot use an invalid authorization request to bounce a browser there.
func TestMCPOAuthNoOpenRedirect(t *testing.T) {
	e := newOAuthEnv(t)
	reg := e.do(http.MethodPost, "/oauth/register", strings.NewReader(`{"redirect_uris":["https://evil.example/landing"],"token_endpoint_auth_method":"none"}`),
		map[string]string{"Content-Type": "application/json"})
	var client map[string]any
	if err := json.Unmarshal(reg.Body.Bytes(), &client); err != nil || reg.Code != http.StatusCreated {
		t.Fatalf("register: %d %s", reg.Code, reg.Body)
	}
	_, challenge := newPKCE(t)
	q := url.Values{"response_type": {"token"}, "client_id": {client["client_id"].(string)}, "redirect_uri": {"https://evil.example/landing"},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"}}
	w := e.do(http.MethodGet, "/oauth/authorize?"+q.Encode(), nil, nil)
	if w.Code == http.StatusFound || strings.Contains(w.Header().Get("Location"), "evil.example") || w.Code != http.StatusBadRequest {
		t.Fatalf("an invalid request redirected: %d %s", w.Code, w.Header().Get("Location"))
	}
	// A valid response type with the PKCE challenge missing: still an
	// error page, since a self-registered redirect URI is never trusted.
	q.Set("response_type", "code")
	q.Del("code_challenge")
	w = e.do(http.MethodGet, "/oauth/authorize?"+q.Encode(), nil, nil)
	if w.Code != http.StatusBadRequest || w.Header().Get("Location") != "" || !strings.Contains(w.Body.String(), "Toolyard cannot connect this app") {
		t.Fatalf("a request without PKCE redirected: %d %q", w.Code, w.Header().Get("Location"))
	}
}

func TestMCPOAuthDenyAndStaticTokensWithAgentScope(t *testing.T) {
	e := newOAuthEnv(t)
	redirect := "https://client.example/cb"
	reg := e.do(http.MethodPost, "/oauth/register", strings.NewReader(`{"redirect_uris":["`+redirect+`"],"token_endpoint_auth_method":"none"}`), map[string]string{"Content-Type": "application/json"})
	var client map[string]any
	if err := json.Unmarshal(reg.Body.Bytes(), &client); err != nil || reg.Code != http.StatusCreated {
		t.Fatalf("register: %d %s", reg.Code, reg.Body)
	}
	_, challenge := newPKCE(t)
	loc := e.consent(client["client_id"].(string), redirect, challenge, url.Values{"verdict": {"deny"}})
	if loc.Query().Get("error") != "access_denied" || loc.Query().Get("code") != "" {
		t.Fatalf("deny redirect: %s", loc)
	}

	// A static agent token keeps working, and the same per-agent scope
	// applies to an ordinary agent.
	token, ag, err := e.ids.CreateAgentWithToken(context.Background(), e.user.ID, "plain-agent")
	if err != nil {
		t.Fatal(err)
	}
	if names := e.toolNames(token); !names["memory.delete"] || !names["fixture.echo"] {
		t.Fatal("an unlimited static agent lost tools")
	}
	body := `{"agent_id":"` + ag.ID + `","scope":{"mode":"listed","allow_servers":["fixture"],"allow_tools":[],"deny_servers":[],"deny_tools":[]}}`
	w := e.do(http.MethodPost, "/v1/me/agent-scopes", strings.NewReader(body),
		map[string]string{"Content-Type": "application/json", "X-Requested-With": "toolyard", "Origin": oauthPublic}, e.session...)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"mode":"listed"`) {
		t.Fatalf("set agent scope: %d %s", w.Code, w.Body)
	}
	names := e.toolNames(token)
	if !names["fixture.echo"] || names["memory.get"] || !names["access.whoami"] {
		t.Fatalf("limited static agent sees fixture.echo=%v memory.get=%v access.whoami=%v", names["fixture.echo"], names["memory.get"], names["access.whoami"])
	}
	if msg, _ := e.call(token, "memory.get", map[string]any{"key": "k"}); !strings.Contains(msg, "not allowed for this agent") {
		t.Fatalf("limited static agent: %q", msg)
	}
	_, res := e.call(token, "access.whoami", map[string]any{})
	who, _ := res["structuredContent"].(map[string]any)
	owner, _ := who["owner"].(map[string]any)
	limits, _ := who["agent_limits"].(map[string]any)
	if owner["role"] != "admin" || limits["mode"] != "listed" {
		t.Fatalf("whoami does not report the limits and the owner's role: %v", res)
	}
}

// TestMCPOAuthWaitingStopPageFinishesWhereTheClientStarted walks the
// waiting path through the real handler chain: the stop page in browser A
// (no toolyard session) carries its waiter secret only in its body and
// sets a Strict nonce cookie; A asks /oauth/wait (Origin checked) while B,
// signed in, allows the app through the copied consent link; A then sees
// approved and its Continue sends the code to the client from A; the code
// works with PKCE, and the return link B got is refused as used.
func TestMCPOAuthWaitingStopPageFinishesWhereTheClientStarted(t *testing.T) {
	e := newOAuthEnv(t)
	redirect := "http://127.0.0.1:43124/callback"
	reg := e.do(http.MethodPost, "/oauth/register", strings.NewReader(`{"redirect_uris":["`+redirect+`"],"token_endpoint_auth_method":"none","client_name":"Popup client"}`),
		map[string]string{"Content-Type": "application/json"})
	var client map[string]any
	if err := json.Unmarshal(reg.Body.Bytes(), &client); err != nil || reg.Code != http.StatusCreated {
		t.Fatalf("register: %d %s", reg.Code, reg.Body)
	}
	clientID := client["client_id"].(string)
	verifier, challenge := newPKCE(t)
	q := url.Values{"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {redirect}, "state": {"popup-state"},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"}, "resource": {oauthPublic + "/mcp"}, "scope": {"mcp"}}

	// A: the stop page.
	stop := e.do(http.MethodGet, "/oauth/authorize?"+q.Encode(), nil, nil)
	body := stop.Body.String()
	link := stopLinkRE.FindStringSubmatch(body)
	waiter := regexp.MustCompile(`name="waiter" value="([^"]+)"`).FindStringSubmatch(body)
	nonce := nonceRE.FindStringSubmatch(body)
	var waitCookie *http.Cookie
	for _, c := range stop.Result().Cookies() {
		if strings.HasPrefix(c.Name, "toolyard_oauth_wait_") {
			waitCookie = c
		}
	}
	if stop.Code != http.StatusOK || link == nil || waiter == nil || nonce == nil || waitCookie == nil || waitCookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("waiting stop page: %d %s", stop.Code, body)
	}
	consentURL := link[1]
	id := strings.TrimPrefix(consentURL, "/oauth/consent?request=")
	if strings.Contains(consentURL, waiter[1]) || strings.Count(body, waiter[1]) != 1 || strings.Contains(stop.Header().Get("Set-Cookie"), waiter[1]) {
		t.Fatal("the waiter leaves the page body")
	}
	if csp := stop.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "form-action 'self' "+mcpauth.CSPSource(redirect)) {
		t.Fatalf("stop page CSP does not allow the redirect target: %q", csp)
	}
	poll := func(secret, origin string) (int, string) {
		t.Helper()
		raw, _ := json.Marshal(map[string]string{"request": id, "waiter": secret})
		w := e.do(http.MethodPost, "/oauth/wait", strings.NewReader(string(raw)),
			map[string]string{"Content-Type": "application/json", "Origin": origin, "Sec-Fetch-Site": "same-origin"})
		var out map[string]string
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out["status"]
	}
	if code, st := poll(waiter[1], oauthPublic); code != http.StatusOK || st != mcpauth.WaitPending {
		t.Fatalf("poll before consent: %d %q", code, st)
	}
	if code, _ := poll(waiter[1], "https://evil.example"); code != http.StatusForbidden {
		t.Fatalf("cross-origin poll: %d", code)
	}
	if code, st := poll("tywt_"+strings.Repeat("A", 43), oauthPublic); code != http.StatusOK || st != mcpauth.WaitExpired {
		t.Fatalf("poll with a wrong waiter: %d %q, want the unknown request's answer", code, st)
	}

	// B: the copied consent link, signed in.
	page := e.do(http.MethodGet, consentURL, nil, nil, e.session...)
	m := nonceRE.FindStringSubmatch(page.Body.String())
	var consentCookie *http.Cookie
	for _, c := range page.Result().Cookies() {
		if strings.HasPrefix(c.Name, "ty_oauth_") {
			consentCookie = c
		}
	}
	if page.Code != http.StatusOK || m == nil || consentCookie == nil {
		t.Fatalf("consent page: %d %s", page.Code, page.Body)
	}
	post := func(target string, v url.Values, origin, site string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
		return e.do(http.MethodPost, target, strings.NewReader(v.Encode()), map[string]string{
			"Content-Type": "application/x-www-form-urlencoded", "Origin": origin, "Sec-Fetch-Site": site}, cookies...)
	}
	done := post(consentURL, url.Values{"nonce": {m[1]}, "verdict": {"allow"}, "nickname": {"Popup client"}, "scope_mode": {"all"}},
		oauthPublic, "same-origin", append(e.session, consentCookie)...)
	returnURL := done.Header().Get("Location")
	if done.Code != http.StatusSeeOther || !strings.HasPrefix(returnURL, mcpauth.ReturnPath) {
		t.Fatalf("consent submit: %d %q", done.Code, returnURL)
	}
	if code, st := poll(waiter[1], oauthPublic); code != http.StatusOK || st != mcpauth.WaitApproved {
		t.Fatalf("poll after consent: %d %q", code, st)
	}

	// A: Continue.
	cont := url.Values{"request": {id}, "waiter": {waiter[1]}, "nonce": {nonce[1]}}
	if bad := post("/oauth/wait/continue", cont, "https://evil.example", "same-origin", waitCookie); bad.Code != http.StatusForbidden {
		t.Fatalf("cross-origin Continue: %d", bad.Code)
	}
	if bad := post("/oauth/wait/continue", cont, oauthPublic, "cross-site", waitCookie); bad.Code != http.StatusForbidden {
		t.Fatalf("cross-site Continue: %d", bad.Code)
	}
	if bad := post("/oauth/wait/continue", cont, oauthPublic, "same-origin"); bad.Code != http.StatusForbidden {
		t.Fatalf("Continue without the nonce cookie: %d", bad.Code)
	}
	back := post("/oauth/wait/continue", cont, oauthPublic, "same-origin", waitCookie)
	loc, err := url.Parse(back.Header().Get("Location"))
	if back.Code != http.StatusSeeOther || err != nil || !strings.HasPrefix(back.Header().Get("Location"), redirect+"?") ||
		loc.Query().Get("state") != "popup-state" || loc.Query().Get("iss") != oauthPublic || loc.Query().Get("code") == "" {
		t.Fatalf("Continue: %d %q %s", back.Code, back.Header().Get("Location"), back.Body)
	}
	w, tok := e.form("/oauth/token", url.Values{"grant_type": {"authorization_code"}, "code": {loc.Query().Get("code")},
		"redirect_uri": {redirect}, "code_verifier": {verifier}, "client_id": {clientID}, "resource": {oauthPublic + "/mcp"}})
	if w.Code != http.StatusOK || tok["access_token"] == nil {
		t.Fatalf("token: %d %s", w.Code, w.Body)
	}

	// The return link B got shares the approval: it is used now.
	if ret := e.do(http.MethodGet, returnURL, nil, nil); ret.Code != http.StatusGone || !strings.Contains(ret.Body.String(), "already used") {
		t.Fatalf("return link after the waiting page: %d %s", ret.Code, ret.Body)
	}
	if code, st := poll(waiter[1], oauthPublic); code != http.StatusOK || st != mcpauth.WaitUsed {
		t.Fatalf("poll after Continue: %d %q", code, st)
	}
}
