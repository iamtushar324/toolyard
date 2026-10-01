package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/gateway"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/memory"
	"github.com/tusharbhardwaj/toolyard/internal/policy"
	"github.com/tusharbhardwaj/toolyard/internal/realtime"
	"github.com/tusharbhardwaj/toolyard/internal/sealbox"
	"github.com/tusharbhardwaj/toolyard/internal/secrets"
	"github.com/tusharbhardwaj/toolyard/internal/settings"
	"github.com/tusharbhardwaj/toolyard/internal/store"
	"github.com/tusharbhardwaj/toolyard/internal/upstreams"
)

type opEnv struct {
	h       http.Handler
	id      *identity.Service
	user    *identity.User
	secrets *secrets.Service
	audit   *audit.Logger
	db      *store.DB
}

// newOperatorEnv wires the production middleware chain (RoleGuard, HardenAPI,
// Origin check with a public URL) around a real API server.
func newOperatorEnv(t *testing.T) *opEnv {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "op.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	id := identity.New(db)
	u, err := id.CreateUser(ctx, "owner", "long-enough-password")
	if err != nil {
		t.Fatal(err)
	}
	auditLog := audit.New(db)
	key, _ := sealbox.LoadOrCreateKey(dir, "secrets.key")
	cipher, _ := sealbox.NewCipher(key)
	sec := secrets.New(db, cipher, auditLog)
	gw := gateway.New(gateway.Options{
		Policy: policy.New(db), Audit: auditLog, Memory: memory.New(db), Hub: realtime.NewHub(),
	})
	up := upstreams.New(db, gw)
	up.SetSecrets(sec)
	set, err := settings.New(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	srv := New(ctx, Options{
		Identity: id, Audit: auditLog, Secrets: sec, Upstreams: up, Gateway: gw, Settings: set,
		SessionKey: []byte("0123456789abcdef0123456789abcdef"),
		Security:   SecurityOptions{PublicURL: "https://toolyard.example"},
	})
	mux := http.NewServeMux()
	srv.Routes(mux)
	var h http.Handler = srv.RoleGuard(mux)
	h = srv.HardenAPI(h)
	h = srv.EnforceOriginOnMutations(h)
	return &opEnv{h: h, id: id, user: u, secrets: sec, audit: auditLog, db: db}
}

func (e *opEnv) token(t *testing.T, scopes ...string) string {
	t.Helper()
	tok, _, err := e.id.CreateOperatorToken(context.Background(), e.user.ID, "test", scopes, 0, "test")
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// do sends what the CLI sends: bearer, JSON, no cookie, no Origin, and no
// X-Requested-With unless withCSRF.
func (e *opEnv) do(t *testing.T, tok, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func TestOperatorTokenDrivesDashboardAPI(t *testing.T) {
	e := newOperatorEnv(t)
	tok := e.token(t) // read write

	if rec := e.do(t, tok, "GET", "/v1/auth/me", ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"owner"`) {
		t.Fatalf("me: %d %s", rec.Code, rec.Body)
	}
	// A mutation with no Origin and no X-Requested-With goes through.
	rec := e.do(t, tok, "POST", "/v1/agents", `{"name":"hermes"}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"token"`) {
		t.Fatalf("create agent: %d %s", rec.Code, rec.Body)
	}
	// Request a secret, wire a server to it with an embedded ref.
	rec = e.do(t, tok, "POST", "/v1/secrets", `{"name":"API_KEY","description":"for x","request":true}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"ref":"secret://API_KEY"`) ||
		!strings.Contains(rec.Body.String(), "https://toolyard.example/#settings") {
		t.Fatalf("request secret: %d %s", rec.Code, rec.Body)
	}
	rec = e.do(t, tok, "POST", "/v1/servers",
		`{"name":"x","transport":"http","url":"http://127.0.0.1:1/mcp","headers":{"Authorization":"Bearer ${secret://API_KEY}"},"enabled":true}`)
	if rec.Code != 200 && rec.Code != 202 {
		t.Fatalf("add server: %d %s", rec.Code, rec.Body)
	}
	rec = e.do(t, tok, "GET", "/v1/secrets", "")
	var list []map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list) != 1 || list[0]["pending"] != true || !strings.Contains(rec.Body.String(), `"used_by":["x"]`) {
		t.Fatalf("secrets list: %s", rec.Body)
	}
	// Unknown secret refs fail at save with a hint to request them.
	rec = e.do(t, tok, "POST", "/v1/servers",
		`{"name":"y","transport":"http","url":"http://127.0.0.1:1/mcp","headers":{"Authorization":"Bearer ${secret://NOPE}"}}`)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "request") {
		t.Fatalf("unknown ref: %d %s", rec.Code, rec.Body)
	}
	// Ordinary settings are writable; approval settings are not.
	if rec := e.do(t, tok, "PATCH", "/v1/settings", `{"top_n_count":7}`); rec.Code != 200 {
		t.Fatalf("patch setting: %d %s", rec.Code, rec.Body)
	}
	if rec := e.do(t, tok, "PATCH", "/v1/settings", `{"approval_mode":"execute"}`); rec.Code != 403 ||
		!strings.Contains(rec.Body.String(), "approval_mode") {
		t.Fatalf("owner setting: %d %s", rec.Code, rec.Body)
	}
	// Mutations are audited under the token.
	var n int
	_ = e.db.QueryRow(`SELECT count(*) FROM audit_events WHERE event_type='operator.request' AND agent_id LIKE 'operator:op_%'`).Scan(&n)
	if n < 3 {
		t.Fatalf("operator audit rows = %d", n)
	}
}

func TestOperatorTokenCannotTouchSecretValuesOrApprovals(t *testing.T) {
	e := newOperatorEnv(t)
	tok := e.token(t)
	if _, err := e.secrets.Create(context.Background(), "REAL", "sentinel-value-xyz", ""); err != nil {
		t.Fatal(err)
	}
	cases := []struct{ method, path, body string }{
		{"POST", "/v1/secrets", `{"name":"NEW","value":"v"}`},
		{"PUT", "/v1/secrets/REAL", `{"value":"overwrite"}`},
		{"DELETE", "/v1/secrets/REAL?force=1", ""},
		{"POST", "/v1/approvals/decide-batch", `{"ids":[],"decision":"allow"}`},
		{"POST", "/v1/approvals/ap_1", `{"decision":"allow"}`},
		{"POST", "/v1/policies", `{}`},
		{"POST", "/v1/insights/auto/rules", `{}`},
		{"POST", "/v1/inbox/rq_1/decide", `{}`},
		{"POST", "/v1/users", `{"username":"x","password":"long-enough-password"}`},
		{"POST", "/v1/push/subscribe", `{}`},
		{"POST", "/v1/operator-tokens", `{"name":"esc","scopes":["owner"]}`},
	}
	for _, c := range cases {
		rec := e.do(t, tok, c.method, c.path, c.body)
		if rec.Code != 403 || !strings.Contains(rec.Body.String(), "owner") {
			t.Errorf("%s %s: %d %s", c.method, c.path, rec.Code, rec.Body)
		}
	}
	if v, err := e.secrets.Resolve(context.Background(), "REAL"); err != nil || v != "sentinel-value-xyz" {
		t.Fatalf("secret changed: %q %v", v, err)
	}
	// Never available, whatever the scope.
	owner := e.token(t, "owner")
	for _, c := range []struct{ method, path string }{
		{"POST", "/v1/settings/reveal"}, {"POST", "/v1/me/identity-key/reveal"},
		{"POST", "/v1/auth/logout"}, {"POST", "/v1/auth/login"},
	} {
		rec := e.do(t, owner, c.method, c.path, `{}`)
		if rec.Code != 403 {
			t.Errorf("%s %s with owner scope: %d", c.method, c.path, rec.Code)
		}
	}
	// The owner scope can set values.
	if rec := e.do(t, owner, "PUT", "/v1/secrets/REAL", `{"value":"new"}`); rec.Code != 200 {
		t.Fatalf("owner set: %d %s", rec.Code, rec.Body)
	}
	// No response anywhere carries a secret value.
	for _, p := range []string{"/v1/secrets", "/v1/servers", "/v1/audit?limit=500", "/v1/settings"} {
		if body := e.do(t, owner, "GET", p, "").Body.String(); strings.Contains(body, "sentinel-value-xyz") || strings.Contains(body, `"new"`) {
			t.Errorf("%s leaks a secret value", p)
		}
	}
}

func TestOperatorTokenAuthFailures(t *testing.T) {
	e := newOperatorEnv(t)
	read := e.token(t, "read")
	if rec := e.do(t, read, "GET", "/v1/servers", ""); rec.Code != 200 {
		t.Fatalf("read: %d", rec.Code)
	}
	if rec := e.do(t, read, "POST", "/v1/agents", `{"name":"a"}`); rec.Code != 403 ||
		!strings.Contains(rec.Body.String(), `"required_scope":"write"`) {
		t.Fatalf("read-only write: %d %s", rec.Code, rec.Body)
	}
	if rec := e.do(t, "tyop_deadbeef.nope", "GET", "/v1/servers", ""); rec.Code != 401 {
		t.Fatalf("bad token: %d", rec.Code)
	}
	tok := e.token(t)
	ot, _, _ := e.id.VerifyOperatorToken(context.Background(), tok)
	_ = e.id.RevokeOperatorToken(context.Background(), "", ot.ID)
	if rec := e.do(t, tok, "GET", "/v1/servers", ""); rec.Code != 401 {
		t.Fatalf("revoked token: %d", rec.Code)
	}
	// Without a token the Origin check still guards cookie-less mutations.
	if rec := e.do(t, "", "POST", "/v1/agents", `{"name":"a"}`); rec.Code != 403 {
		t.Fatalf("anonymous mutation: %d", rec.Code)
	}
	// An agent token is not an operator token.
	agentTok, _, _ := e.id.CreateAgentWithToken(context.Background(), e.user.ID, "ag")
	if rec := e.do(t, agentTok, "GET", "/v1/servers", ""); rec.Code != 401 {
		t.Fatalf("agent token on dashboard API: %d", rec.Code)
	}
}

func TestOperatorTokenMintingCannotEscalate(t *testing.T) {
	e := newOperatorEnv(t)
	tok := e.token(t)
	rec := e.do(t, tok, "POST", "/v1/operator-tokens", `{"name":"child","scopes":["read"]}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "tyop_") {
		t.Fatalf("mint child: %d %s", rec.Code, rec.Body)
	}
	rec = e.do(t, tok, "GET", "/v1/operator-tokens", "")
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "tyop_") || strings.Contains(rec.Body.String(), "token_hash") {
		t.Fatalf("list leaks tokens: %s", rec.Body)
	}
}

func TestOperatorCatalogCoversEveryRoute(t *testing.T) {
	have := map[string]bool{}
	for _, c := range operatorCatalog {
		have[c.Path] = true
		if strings.HasSuffix(c.Path, "{id}") {
			have[strings.TrimSuffix(c.Path, "{id}")] = true
		}
	}
	for _, p := range registeredRoutePaths(t) {
		if !strings.HasPrefix(p, "/v1/") {
			continue
		}
		if !have[p] {
			t.Errorf("route %s is missing from operatorCatalog", p)
		}
	}
}

func TestOperatorScopeFor(t *testing.T) {
	cases := []struct {
		method, path, want string
		ok                 bool
	}{
		{"GET", "/v1/servers", "read", true},
		{"POST", "/v1/servers", "write", true},
		{"PATCH", "/v1/servers/x", "write", true},
		{"POST", "/v1/secrets", "write", true},
		{"PUT", "/v1/secrets/X", "owner", true},
		{"POST", "/v1/approvals/decide-batch", "owner", true},
		{"POST", "/v1/inbox/abc/decide", "owner", true},
		{"POST", "/v1/inbox/abc/summarize", "write", true},
		{"POST", "/v1/insights/tools/github.x", "owner", true},
		{"POST", "/v1/settings/reveal", "", false},
		{"GET", "/v1/me/identity-key/reveal", "", false},
		{"POST", "/v1/auth/login", "", false},
	}
	for _, c := range cases {
		got, ok := operatorScopeFor(c.method, c.path)
		if got != c.want || ok != c.ok {
			t.Errorf("%s %s = %q %v, want %q %v", c.method, c.path, got, ok, c.want, c.ok)
		}
	}
}
