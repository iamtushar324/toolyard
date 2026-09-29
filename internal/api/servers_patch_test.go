package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/tusharbhardwaj/toolyard/internal/oauth"
	"github.com/tusharbhardwaj/toolyard/internal/sealbox"
	"github.com/tusharbhardwaj/toolyard/internal/secrets"
	"github.com/tusharbhardwaj/toolyard/internal/upstreams"
)

// newPatchMCPServer serves one read-only tool over streamable HTTP so the
// upstreams service can really connect and reconnect.
func newPatchMCPServer(t *testing.T) *httptest.Server {
	t.Helper()
	m := server.NewMCPServer("up", "1.0.0")
	m.AddTool(mcp.NewTool("get_status"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("ok"), nil
	})
	ts := server.NewTestStreamableHTTPServer(m)
	t.Cleanup(ts.Close)
	return ts
}

// addPatchServer connects a real http upstream named name.
func addPatchServer(t *testing.T, e *accessTestEnv, name string, ts *httptest.Server) {
	t.Helper()
	if _, err := e.srv.upstreams.Add(context.Background(), upstreams.Server{Name: name, Transport: "http", URL: ts.URL}); err != nil {
		t.Fatalf("add %s: %v", name, err)
	}
}

// seedOAuth inserts the client and token rows the OAuth flow would leave
// behind for name. Both cascade on delete of the server row.
func seedOAuth(t *testing.T, e *accessTestEnv, name string) {
	t.Helper()
	now := time.Now().UnixMilli()
	if _, err := e.db.Exec(`INSERT INTO oauth_clients(upstream_name, issuer, authorization_endpoint, token_endpoint,
			client_id, redirect_uri, scopes, token_endpoint_auth_method, registered_at, updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)`,
		name, "https://issuer.example", "https://issuer.example/auth", "https://issuer.example/token",
		"client-1", "https://toolyard.example/cb", "mcp", "none", now, now); err != nil {
		t.Fatalf("seed oauth client: %v", err)
	}
	if _, err := e.db.Exec(`INSERT INTO oauth_tokens(upstream_name, state) VALUES(?, 'authorized')`, name); err != nil {
		t.Fatalf("seed oauth token: %v", err)
	}
}

func oauthRows(t *testing.T, e *accessTestEnv, name string) (clients, tokens int) {
	t.Helper()
	if err := e.db.QueryRow(`SELECT count(*) FROM oauth_clients WHERE upstream_name = ?`, name).Scan(&clients); err != nil {
		t.Fatal(err)
	}
	if err := e.db.QueryRow(`SELECT count(*) FROM oauth_tokens WHERE upstream_name = ?`, name).Scan(&tokens); err != nil {
		t.Fatal(err)
	}
	return clients, tokens
}

func decodeServer(t *testing.T, rec *httptest.ResponseRecorder) upstreams.Server {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var out upstreams.Server
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v %s", err, rec.Body.String())
	}
	return out
}

// TestServersPatchEditsInPlace: url, headers and identity change through
// PATCH; the live upstream is reconnected; OAuth rows survive because the
// server row is never re-created; secret values never come back.
func TestServersPatchEditsInPlace(t *testing.T) {
	e := newAccessTestServer(t)
	admin := e.cookieFor(t, e.admin.ID)
	ts1 := newPatchMCPServer(t)
	addPatchServer(t, e, "bk", ts1)
	seedOAuth(t, e, "bk")
	if e.gw.UpstreamToolCount("bk") != 1 {
		t.Fatalf("tool count after add = %d", e.gw.UpstreamToolCount("bk"))
	}

	// url: a new path on the same origin keeps OAuth (the cross-origin
	// case is TestServersPatchURLOriginChangeResetsOAuth).
	newURL := ts1.URL + "/v2"
	got := decodeServer(t, e.do(t, admin, http.MethodPatch, "/v1/servers/bk", `{"url":"`+newURL+`"}`))
	if got.URL != newURL || got.LastStatus != "ok" || !got.Enabled || got.OAuthReset {
		t.Fatalf("after url patch: %+v", got)
	}
	if e.gw.UpstreamToolCount("bk") != 1 {
		t.Fatalf("upstream not reconnected: tools=%d", e.gw.UpstreamToolCount("bk"))
	}
	if c, k := oauthRows(t, e, "bk"); c != 1 || k != 1 {
		t.Fatalf("oauth rows after url patch: clients=%d tokens=%d, want 1/1", c, k)
	}

	// headers: masked in the response, raw in the store
	rec := e.do(t, admin, http.MethodPatch, "/v1/servers/bk", `{"headers":{"X-Api-Key":"raw-shared-key"}}`)
	got = decodeServer(t, rec)
	if got.Headers["X-Api-Key"] != upstreams.MaskedValue || strings.Contains(rec.Body.String(), "raw-shared-key") {
		t.Fatalf("header value leaked or missing: %s", rec.Body.String())
	}
	if stored, _ := e.srv.upstreams.Get(context.Background(), "bk"); stored.Headers["X-Api-Key"] != "raw-shared-key" || stored.URL != newURL {
		t.Fatalf("stored = %+v", stored)
	}

	// identity on, then off with an explicit null
	got = decodeServer(t, e.do(t, admin, http.MethodPatch, "/v1/servers/bk",
		`{"identity":{"header":"x-bk-bifrost-vk","register":true}}`))
	if got.Identity == nil || got.Identity.Header != "x-bk-bifrost-vk" || !got.Identity.Register {
		t.Fatalf("identity after patch = %+v", got.Identity)
	}
	if got.Headers["X-Api-Key"] != upstreams.MaskedValue || got.URL != newURL {
		t.Fatalf("identity patch disturbed other fields: %+v", got)
	}
	rec = e.do(t, admin, http.MethodPatch, "/v1/servers/bk", `{"identity":null}`)
	got = decodeServer(t, rec)
	if got.Identity != nil || strings.Contains(rec.Body.String(), `"identity"`) {
		t.Fatalf("identity not cleared: %s", rec.Body.String())
	}

	// GET /v1/servers reflects the same shape.
	rec = e.do(t, admin, http.MethodPatch, "/v1/servers/bk", `{"identity":{"header":"x-bk-bifrost-vk"}}`)
	decodeServer(t, rec)
	rec = e.do(t, admin, http.MethodGet, "/v1/servers", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"identity":{"header":"x-bk-bifrost-vk"}`) {
		t.Fatalf("GET /v1/servers: %d %s", rec.Code, rec.Body.String())
	}

	// enabled: false takes the upstream down without deleting anything.
	got = decodeServer(t, e.do(t, admin, http.MethodPatch, "/v1/servers/bk", `{"enabled":false}`))
	if got.Enabled || got.LastStatus != "disabled" || e.gw.UpstreamToolCount("bk") != 0 {
		t.Fatalf("after disable: %+v tools=%d", got, e.gw.UpstreamToolCount("bk"))
	}
	if c, k := oauthRows(t, e, "bk"); c != 1 || k != 1 {
		t.Fatalf("oauth rows after disable: clients=%d tokens=%d", c, k)
	}
	got = decodeServer(t, e.do(t, admin, http.MethodPatch, "/v1/servers/bk", `{"enabled":true}`))
	if !got.Enabled || got.LastStatus != "ok" || e.gw.UpstreamToolCount("bk") != 1 {
		t.Fatalf("after enable: %+v tools=%d", got, e.gw.UpstreamToolCount("bk"))
	}

	// A url that doesn't answer is saved with its error and reported 202.
	dead := httptest.NewServer(nil)
	dead.Close()
	rec = e.do(t, admin, http.MethodPatch, "/v1/servers/bk", `{"url":"`+dead.URL+`"}`)
	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), `"warning"`) {
		t.Fatalf("dead url: %d %s", rec.Code, rec.Body.String())
	}
	if c, k := oauthRows(t, e, "bk"); c != 1 || k != 1 {
		t.Fatalf("oauth rows after failed reconnect: clients=%d tokens=%d", c, k)
	}
}

// TestServersPatchRejects covers the error statuses: 400 for invalid
// input, 404 for an unknown server, 403 for a built-in and for a member.
func TestServersPatchRejects(t *testing.T) {
	e := newAccessTestServer(t)
	admin := e.cookieFor(t, e.admin.ID)
	ts := newPatchMCPServer(t)
	addPatchServer(t, e, "bk", ts)

	// Wire the real secrets broker so refs are checked at save time.
	key, _ := sealbox.LoadOrCreateKey(t.TempDir(), "secrets.key")
	cipher, _ := sealbox.NewCipher(key)
	e.srv.upstreams.SetSecrets(secrets.New(e.db, cipher, e.audit))

	bad := map[string]string{
		"reserved header": `{"identity":{"header":"Authorization"}}`,
		"bad token":       `{"identity":{"header":"x id"}}`,
		"static clash":    `{"headers":{"X-BK-Bifrost-VK":"x"},"identity":{"header":"x-bk-bifrost-vk"}}`,
		"empty url":       `{"url":""}`,
		"unknown secret":  `{"headers":{"X-Key":"secret://MISSING"}}`,
		"unknown field":   `{"transport":"stdio"}`,
		"identity extra":  `{"identity":{"header":"x-id","bogus":1}}`,
		"not json":        `{"url":`,
		"empty body":      ``,
		"masked header":   `{"headers":{"X-Api-Key":"` + upstreams.MaskedValue + `"}}`,
		"masked env":      `{"env":{"TOKEN":"` + upstreams.MaskedValue + `"}}`,
	}
	for name, body := range bad {
		rec := e.do(t, admin, http.MethodPatch, "/v1/servers/bk", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400: %s", name, rec.Code, rec.Body.String())
		}
		if strings.HasPrefix(name, "masked") && !strings.Contains(rec.Body.String(), "masked value") {
			t.Errorf("%s: body should explain the mask: %s", name, rec.Body.String())
		}
	}
	if stored, _ := e.srv.upstreams.Get(context.Background(), "bk"); stored.URL != ts.URL || stored.Identity != nil || len(stored.Headers) != 0 {
		t.Fatalf("a rejected patch changed the row: %+v", stored)
	}

	// Identity on a stdio server.
	now := time.Now().UnixMilli()
	if _, err := e.db.Exec(`INSERT INTO upstream_servers(name, transport, command, enabled, created_at, updated_at)
		VALUES('fs', 'stdio', '/nonexistent/toolyard-xyz', 1, ?, ?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if rec := e.do(t, admin, http.MethodPatch, "/v1/servers/fs", `{"identity":{"header":"x-id"}}`); rec.Code != http.StatusBadRequest {
		t.Errorf("identity on stdio: status %d: %s", rec.Code, rec.Body.String())
	}

	if rec := e.do(t, admin, http.MethodPatch, "/v1/servers/nope", `{"url":"`+ts.URL+`"}`); rec.Code != http.StatusNotFound {
		t.Errorf("unknown server: status %d: %s", rec.Code, rec.Body.String())
	}
	if rec := e.do(t, admin, http.MethodPatch, "/v1/servers/mempalace", `{"url":"`+ts.URL+`"}`); rec.Code != http.StatusForbidden {
		t.Errorf("built-in: status %d: %s", rec.Code, rec.Body.String())
	}

	// Members are kept out (RoleGuard in front, requireAdmin behind it).
	m := e.member(t, "user_m", "m@beknown.work")
	if rec := e.do(t, e.cookieFor(t, m.ID), http.MethodPatch, "/v1/servers/bk", `{"url":"`+ts.URL+`"}`); rec.Code != http.StatusForbidden {
		t.Errorf("member: status %d: %s", rec.Code, rec.Body.String())
	}
	if rec := e.do(t, nil, http.MethodPatch, "/v1/servers/bk", `{"url":"`+ts.URL+`"}`); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous: status %d: %s", rec.Code, rec.Body.String())
	}
}

// TestServersPatchRequireAdminWithoutGuard: the handler enforces the admin
// role itself, so a mux without RoleGuard in front still refuses members.
func TestServersPatchRequireAdminWithoutGuard(t *testing.T) {
	e := newAccessTestServer(t)
	mux := http.NewServeMux()
	e.srv.Routes(mux)
	m := e.member(t, "user_m", "m@beknown.work")
	req := httptest.NewRequest(http.MethodPatch, "/v1/servers/bk", strings.NewReader(`{"url":"http://127.0.0.1:1/mcp"}`))
	req.AddCookie(e.cookieFor(t, m.ID))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "admin_only") {
		t.Fatalf("member without RoleGuard: %d %s", rec.Code, rec.Body.String())
	}
}

// TestServersPatchURLOriginChangeResetsOAuth: with the real OAuth service
// wired, moving a server to another origin drops its client and token
// rows, says so in the response and the audit log, and still reconnects.
// A path change on the same origin keeps them.
func TestServersPatchURLOriginChangeResetsOAuth(t *testing.T) {
	e := newAccessTestServer(t)
	admin := e.cookieFor(t, e.admin.ID)
	key, _ := sealbox.LoadOrCreateKey(t.TempDir(), "oauth.key")
	cipher, _ := oauth.NewCipher(key)
	e.srv.upstreams.SetAuth(oauth.New(e.db, cipher, nil, nil, nil))
	ts1 := newPatchMCPServer(t)
	ts2 := newPatchMCPServer(t)
	addPatchServer(t, e, "bk", ts1)
	seedOAuth(t, e, "bk")

	rec := e.do(t, admin, http.MethodPatch, "/v1/servers/bk", `{"url":"`+ts1.URL+`/v2"}`)
	got := decodeServer(t, rec)
	if got.OAuthReset || strings.Contains(rec.Body.String(), "oauth_reset") {
		t.Fatalf("same-origin move reported a reset: %s", rec.Body.String())
	}
	if c, k := oauthRows(t, e, "bk"); c != 1 || k != 1 {
		t.Fatalf("oauth rows after same-origin move: clients=%d tokens=%d", c, k)
	}

	rec = e.do(t, admin, http.MethodPatch, "/v1/servers/bk", `{"url":"`+ts2.URL+`"}`)
	got = decodeServer(t, rec)
	if !got.OAuthReset || !strings.Contains(rec.Body.String(), `"oauth_reset":true`) {
		t.Fatalf("cross-origin move not reported: %s", rec.Body.String())
	}
	if got.URL != ts2.URL || got.LastStatus != "ok" || e.gw.UpstreamToolCount("bk") != 1 {
		t.Fatalf("after cross-origin move: %+v tools=%d", got, e.gw.UpstreamToolCount("bk"))
	}
	if c, k := oauthRows(t, e, "bk"); c != 0 || k != 0 {
		t.Fatalf("oauth rows after cross-origin move: clients=%d tokens=%d, want 0/0", c, k)
	}
	if n := e.auditRows(t, "oauth.disconnect", ""); n != 1 {
		t.Fatalf("oauth.disconnect audit rows = %d, want 1", n)
	}
	// A later GET doesn't carry the flag.
	rec = e.do(t, admin, http.MethodGet, "/v1/servers", "")
	if strings.Contains(rec.Body.String(), "oauth_reset") {
		t.Fatalf("oauth_reset leaked into GET: %s", rec.Body.String())
	}
}
