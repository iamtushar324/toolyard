package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/access"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/mcpauth"
	"github.com/tusharbhardwaj/toolyard/internal/mempalace"
)

// TestBearerRoutesHonourAgentToolDenies: the bearer REST twins of the
// notes and mempalace tools follow an agent's own per-tool denies, not
// only its owner's groups.
func TestBearerRoutesHonourAgentToolDenies(t *testing.T) {
	e := newAccessTestServer(t)
	ctx := t.Context()
	tok, agent := e.agentFor(t, e.admin.ID)
	note := `{"path":"admin/note.md","content":"# hi"}`
	ingest := `{"entry":"remember this","topic":"t","wing":"w"}`
	expectOK(t, "notes publish before the deny", e.bearer(t, tok, http.MethodPost, "/v1/notes/publish", note))
	if _, err := e.access.SetAgentScope(ctx, agent, access.AgentScope{Mode: access.ScopeModeAll,
		DenyTools: []access.ToolRef{{Server: notesGroup, Tool: "notes.publish"}}}, e.admin.ID); err != nil {
		t.Fatal(err)
	}
	expectNotGranted(t, "notes publish with notes.publish denied", e.bearer(t, tok, http.MethodPost, "/v1/notes/publish", note))
	expectNotGranted(t, "notes sync with notes.publish denied", e.bearer(t, tok, http.MethodPost, "/v1/notes/sync", ""))
	expectOK(t, "mempalace ingest is another tool", e.bearer(t, tok, http.MethodPost, "/v1/mempalace/ingest", ingest))
	if _, err := e.access.SetAgentScope(ctx, agent, access.AgentScope{Mode: access.ScopeModeAll,
		DenyTools: []access.ToolRef{{Server: mempalaceGroup, Tool: mempalace.DiaryWriteTool}}}, e.admin.ID); err != nil {
		t.Fatal(err)
	}
	expectNotGranted(t, "mempalace ingest with the diary tool denied", e.bearer(t, tok, http.MethodPost, "/v1/mempalace/ingest", ingest))
	got := expectOK(t, "hook with the diary tool denied", e.bearer(t, tok, http.MethodPost, "/v1/hooks/ingest", `{"source":"codex","event_name":"UserPromptSubmit","text":"x"}`))
	if got["memory_ingested"] != false {
		t.Errorf("a hook forwarded to a denied diary tool: %v", got)
	}
}

// TestOperatorTokensNeedOwnerScopeForAppAccess: changing a connected
// app's or agent's access, revoking an app, and the OAuth settings need an
// operator token's owner scope; reading stays read.
func TestOperatorTokensNeedOwnerScopeForAppAccess(t *testing.T) {
	for _, c := range []struct{ method, path, want string }{
		{http.MethodPost, "/v1/me/agent-scopes", identity.ScopeOwner},
		{http.MethodPost, "/v1/me/apps/scope", identity.ScopeOwner},
		{http.MethodPost, "/v1/me/apps/revoke", identity.ScopeOwner},
		{http.MethodGet, "/v1/me/apps", identity.ScopeRead},
		{http.MethodGet, "/v1/me/agent-scopes", identity.ScopeRead},
	} {
		if got, ok := operatorScopeFor(c.method, c.path); !ok || got != c.want {
			t.Errorf("%s %s = %q %v, want %q", c.method, c.path, got, ok, c.want)
		}
	}
	for _, k := range []string{mcpauth.SettingEnabled, mcpauth.SettingRedirectHosts, mcpauth.SettingAccessTTLMinutes,
		mcpauth.SettingRefreshTTLDays, mcpauth.SettingMaxSessionDays, mcpauth.SettingCustomSchemes} {
		if !ownerSettingKeys[k] {
			t.Errorf("setting %s does not need the owner scope", k)
		}
	}
}

func TestConsentCSPAllowsOnlyTheRedirectTarget(t *testing.T) {
	csp := consentCSP("cursor://anysphere.cursor-mcp/oauth/callback", "https://logo.example/x.png")
	for _, want := range []string{"form-action 'self' cursor:", "img-src 'self' data: https://img.clerk.com https://logo.example"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP %q lacks %q", csp, want)
		}
	}
	if strings.Contains(consentCSP("https://a.example/cb;script-src *", ""), "script-src *") {
		t.Error("a redirect URI injected a CSP directive")
	}
}

// TestOnlyTheOwnerAdminMayLetAnAppManageToolyard: over the Connected apps
// API, an admin cannot give another person's app admin control; the
// owner, while an admin, can; anyone who manages the app can take it away.
func TestOnlyTheOwnerAdminMayLetAnAppManageToolyard(t *testing.T) {
	e := newAccessTestServer(t)
	ctx := t.Context()
	svc := mcpauth.New(mcpauth.Options{DB: e.db, PublicURL: "https://toolyard.example.com", Access: e.access})
	mux := http.NewServeMux()
	e.srv.MCPOAuthRoutes(mux, svc)
	handler := e.srv.HardenAPI(e.srv.RoleGuard(mux))
	m := e.member(t, "user_m", "m@beknown.work")
	appOf := func(u *identity.User, redirect string) string {
		body := `{"redirect_uris":["` + redirect + `"],"token_endpoint_auth_method":"none"}`
		reg, oerr := svc.Register(ctx, []byte(body), "198.51.100.7")
		if oerr != nil {
			t.Fatal(oerr)
		}
		q := url.Values{"response_type": {"code"}, "client_id": {reg.ClientID}, "redirect_uri": {redirect},
			"code_challenge": {strings.Repeat("a", 43)}, "code_challenge_method": {"S256"}}
		req, aerr := svc.BeginAuthorize(ctx, q, "198.51.100.7")
		if aerr != nil {
			t.Fatalf("authorize: %+v", aerr)
		}
		if _, err := svc.Approve(ctx, req.ID, u, mcpauth.Consent{SameBrowser: true, Nickname: "App"}); err != nil {
			t.Fatal(err)
		}
		apps, err := svc.ListApps(ctx, u.ID)
		if err != nil || len(apps) == 0 {
			t.Fatalf("apps: %v %v", apps, err)
		}
		return apps[0].ID
	}
	adminApp, memberApp := appOf(e.admin, "https://a.example/cb"), appOf(m, "https://b.example/cb")
	post := func(cookie *http.Cookie, id string, manage bool) int {
		body := `{"id":"` + id + `","may_manage":` + map[bool]string{true: "true", false: "false"}[manage] + `}`
		req := httptest.NewRequest(http.MethodPost, "/v1/me/apps/scope", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Requested-With", "toolyard")
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code
	}
	adminCookie, memberCookie := e.cookieFor(t, e.admin.ID), e.cookieFor(t, m.ID)
	if code := post(adminCookie, memberApp, true); code != http.StatusForbidden {
		t.Fatalf("an admin turned may_manage on for a member's app: %d", code)
	}
	if code := post(memberCookie, memberApp, true); code != http.StatusForbidden {
		t.Fatalf("a member turned may_manage on: %d", code)
	}
	if code := post(adminCookie, adminApp, true); code != http.StatusOK {
		t.Fatalf("the admin owner could not turn may_manage on: %d", code)
	}
	if code := post(adminCookie, adminApp, false); code != http.StatusOK {
		t.Fatalf("the admin owner could not turn may_manage off: %d", code)
	}
}

// TestAgentToolListRoundTripsThroughTheScopeRoutes: tool_list is part of
// the scope the REST routes read and write, under the same checks as the
// rest of it: the owner switches an agent to the full list and back, an
// unknown value is refused, another member cannot change it, and the
// switch changes no access. A connected app's list is set on the Connected
// apps route and survives connecting again (the consent screen does not
// show it).
func TestAgentToolListRoundTripsThroughTheScopeRoutes(t *testing.T) {
	e := newAccessTestServer(t)
	ctx := t.Context()
	svc := mcpauth.New(mcpauth.Options{DB: e.db, PublicURL: "https://toolyard.example.com", Access: e.access})
	mux := http.NewServeMux()
	e.srv.MCPOAuthRoutes(mux, svc)
	handler := e.srv.HardenAPI(e.srv.RoleGuard(mux))
	send := func(cookie *http.Cookie, method, path, body string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("X-Requested-With", "toolyard")
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	toolList := func(out map[string]any) string {
		sc, _ := out["scope"].(map[string]any)
		v, _ := sc["tool_list"].(string)
		return v
	}
	adminCookie := e.cookieFor(t, e.admin.ID)
	_, agent := e.agentFor(t, e.admin.ID)
	scopeBody := func(list string) string {
		return `{"agent_id":"` + agent + `","scope":{"mode":"all","tool_list":"` + list + `"}}`
	}

	if code, out := send(adminCookie, http.MethodGet, "/v1/me/agent-scopes?agent_id="+agent, ""); code != http.StatusOK || toolList(out) != access.ToolListSmall {
		t.Fatalf("default scope: %d %v", code, out)
	}
	code, out := send(adminCookie, http.MethodPost, "/v1/me/agent-scopes", scopeBody("full"))
	if summary, _ := out["scope_summary"].(string); code != http.StatusOK || toolList(out) != access.ToolListFull || !strings.Contains(summary, "full tool list") {
		t.Fatalf("switch to full: %d %v", code, out)
	}
	if code, out := send(adminCookie, http.MethodGet, "/v1/me/agent-scopes?agent_id="+agent, ""); code != http.StatusOK || toolList(out) != access.ToolListFull {
		t.Fatalf("read back: %d %v", code, out)
	}
	if e.access.ToolListFor(ctx, agent) != access.ToolListFull || !e.access.ScopeFor(ctx, agent).All {
		t.Fatal("the full list must be stored, and must not limit the agent")
	}
	if code, _ := send(adminCookie, http.MethodPost, "/v1/me/agent-scopes", scopeBody("everything")); code != http.StatusBadRequest {
		t.Fatalf("an unknown tool list: %d, want 400", code)
	}
	m := e.member(t, "user_m", "m@example.com")
	if code, _ := send(e.cookieFor(t, m.ID), http.MethodPost, "/v1/me/agent-scopes", scopeBody("small")); code != http.StatusNotFound {
		t.Fatalf("a member changing another person's agent: %d, want 404", code)
	}
	if e.access.ToolListFor(ctx, agent) != access.ToolListFull {
		t.Fatal("a refused change switched the tool list")
	}
	if code, out := send(adminCookie, http.MethodPost, "/v1/me/agent-scopes", `{"agent_id":"`+agent+`","scope":{"mode":"all"}}`); code != http.StatusOK || toolList(out) != access.ToolListSmall {
		t.Fatalf("a scope without tool_list is the small list: %d %v", code, out)
	}

	// A connected app.
	reg, oerr := svc.Register(ctx, []byte(`{"redirect_uris":["https://a.example/cb"],"token_endpoint_auth_method":"none"}`), "198.51.100.7")
	if oerr != nil {
		t.Fatal(oerr)
	}
	authorize := func() *mcpauth.AuthorizeRequest {
		t.Helper()
		q := url.Values{"response_type": {"code"}, "client_id": {reg.ClientID}, "redirect_uri": {"https://a.example/cb"},
			"code_challenge": {strings.Repeat("a", 43)}, "code_challenge_method": {"S256"}}
		req, aerr := svc.BeginAuthorize(ctx, q, "198.51.100.7")
		if aerr != nil {
			t.Fatalf("authorize: %+v", aerr)
		}
		return req
	}
	if _, err := svc.Approve(ctx, authorize().ID, e.admin, mcpauth.Consent{SameBrowser: true, Nickname: "App"}); err != nil {
		t.Fatal(err)
	}
	apps, err := svc.ListApps(ctx, e.admin.ID)
	if err != nil || len(apps) != 1 || apps[0].Scope.ToolList != access.ToolListSmall {
		t.Fatalf("a new app: %+v %v", apps, err)
	}
	code, out = send(adminCookie, http.MethodPost, "/v1/me/apps/scope", `{"id":"`+apps[0].ID+`","scope":{"mode":"all","tool_list":"full"}}`)
	if app, _ := out["app"].(map[string]any); code != http.StatusOK || toolList(app) != access.ToolListFull {
		t.Fatalf("app switch to full: %d %v", code, out)
	}
	// Connecting again through the consent screen keeps it.
	pending := authorize()
	consentURL := mcpauth.ConsentPath + "?request=" + url.QueryEscape(pending.ID)
	r := httptest.NewRequest(http.MethodGet, consentURL, nil)
	r.AddCookie(adminCookie)
	page := httptest.NewRecorder()
	mux.ServeHTTP(page, r)
	nonce := regexp.MustCompile(`name="nonce" value="([^"]+)"`).FindStringSubmatch(page.Body.String())
	if page.Code != http.StatusOK || nonce == nil {
		t.Fatalf("consent page: %d %s", page.Code, page.Body)
	}
	// This browser did not render the stop page: it types the code.
	form := url.Values{"verdict": {"allow"}, "nonce": {nonce[1]}, "nickname": {"App"}, "scope_mode": {"all"}, "code": {pending.Code}}
	r = httptest.NewRequest(http.MethodPost, consentURL, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(adminCookie)
	for _, c := range page.Result().Cookies() {
		r.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), mcpauth.ReturnPath) {
		t.Fatalf("consent again: %d %s %s", rec.Code, rec.Header().Get("Location"), rec.Body)
	}
	if got := e.access.ToolListFor(ctx, apps[0].AgentID); got != access.ToolListFull {
		t.Fatalf("connecting again reset the tool list to %q", got)
	}
}

// TestDeletingAnAppAgentRunsTheCancelHook: DELETE /v1/agents/{id} on an
// OAuth app's agent runs the revocation follow-up (OnRevoke).
func TestDeletingAnAppAgentRunsTheCancelHook(t *testing.T) {
	e := newAccessTestServer(t)
	ctx := t.Context()
	var revoked []string
	svc := mcpauth.New(mcpauth.Options{DB: e.db, PublicURL: "https://toolyard.example.com", Access: e.access,
		OnRevoke: func(_ context.Context, agentID string) { revoked = append(revoked, agentID) }})
	e.srv.MCPOAuthRoutes(http.NewServeMux(), svc)
	reg, oerr := svc.Register(ctx, []byte(`{"redirect_uris":["https://a.example/cb"],"token_endpoint_auth_method":"none"}`), "198.51.100.7")
	if oerr != nil {
		t.Fatal(oerr)
	}
	q := url.Values{"response_type": {"code"}, "client_id": {reg.ClientID}, "redirect_uri": {"https://a.example/cb"},
		"code_challenge": {strings.Repeat("a", 43)}, "code_challenge_method": {"S256"}}
	req, aerr := svc.BeginAuthorize(ctx, q, "198.51.100.7")
	if aerr != nil {
		t.Fatalf("authorize: %+v", aerr)
	}
	if _, err := svc.Approve(ctx, req.ID, e.admin, mcpauth.Consent{SameBrowser: true, Nickname: "App"}); err != nil {
		t.Fatal(err)
	}
	apps, _ := svc.ListApps(ctx, e.admin.ID)
	r := httptest.NewRequest(http.MethodDelete, "/v1/agents/"+apps[0].AgentID, nil)
	r.Header.Set("X-Requested-With", "toolyard")
	r.AddCookie(e.cookieFor(t, e.admin.ID))
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	if len(revoked) != 1 || revoked[0] != apps[0].AgentID {
		t.Fatalf("OnRevoke after the delete: %v", revoked)
	}
}

// TestAppAgentsReportTheirKindAndClient: GET /v1/agents reports a
// connected app's agent as kind oauth_client (from its grant), like its
// audit rows and access.whoami, and GET /v1/audit returns and filters by
// the app's OAuth client id.
func TestAppAgentsReportTheirKindAndClient(t *testing.T) {
	e := newAccessTestServer(t)
	ctx := t.Context()
	svc := mcpauth.New(mcpauth.Options{DB: e.db, PublicURL: "https://toolyard.example.com", Access: e.access})
	e.srv.MCPOAuthRoutes(http.NewServeMux(), svc)
	reg, oerr := svc.Register(ctx, []byte(`{"redirect_uris":["https://a.example/cb"],"token_endpoint_auth_method":"none"}`), "198.51.100.7")
	if oerr != nil {
		t.Fatal(oerr)
	}
	q := url.Values{"response_type": {"code"}, "client_id": {reg.ClientID}, "redirect_uri": {"https://a.example/cb"},
		"code_challenge": {strings.Repeat("a", 43)}, "code_challenge_method": {"S256"}}
	req, aerr := svc.BeginAuthorize(ctx, q, "198.51.100.7")
	if aerr != nil {
		t.Fatalf("authorize: %+v", aerr)
	}
	if _, err := svc.Approve(ctx, req.ID, e.admin, mcpauth.Consent{SameBrowser: true, Nickname: "App"}); err != nil {
		t.Fatal(err)
	}
	_, plain := e.agentFor(t, e.admin.ID)
	apps, _ := svc.ListApps(ctx, e.admin.ID)
	appAgent := apps[0].AgentID
	// A call row written without the client id gets it from the grant.
	if err := e.audit.Write(ctx, audit.Event{EventType: audit.EventCallSucceeded, AgentID: appAgent}); err != nil {
		t.Fatal(err)
	}
	if err := e.audit.Write(ctx, audit.Event{EventType: audit.EventCallSucceeded, AgentID: plain}); err != nil {
		t.Fatal(err)
	}
	get := func(path string, out any) {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.AddCookie(e.cookieFor(t, e.admin.ID))
		rec := httptest.NewRecorder()
		e.handler.ServeHTTP(rec, r)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: %d %s", path, rec.Code, rec.Body)
		}
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
	}
	var agents []struct{ ID, Kind string }
	get("/v1/agents", &agents)
	kinds := map[string]string{}
	for _, a := range agents {
		kinds[a.ID] = a.Kind
	}
	if kinds[appAgent] != identity.AgentKindOAuthClient || kinds[plain] != identity.AgentKindAgent {
		t.Fatalf("agent kinds: %v (app %s, plain %s)", kinds, appAgent, plain)
	}
	var rows []audit.Event
	get("/v1/audit?oauth_client_id="+url.QueryEscape(reg.ClientID), &rows)
	var calls int
	for _, row := range rows {
		if row.OAuthClientID != reg.ClientID || row.AgentID != appAgent {
			t.Fatalf("audit row %s of the client: %+v", row.EventType, row)
		}
		if row.EventType == audit.EventCallSucceeded {
			calls++
		}
	}
	if calls != 1 || len(rows) < 2 {
		t.Fatalf("audit rows of the client: %d (%d calls), want the authorized row and one call", len(rows), calls)
	}
}
