package api

import (
	"bytes"
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
	"github.com/tusharbhardwaj/toolyard/internal/store"
	"github.com/tusharbhardwaj/toolyard/internal/upstreams"
)

// newSecretsAPITestServer wires a full-enough api.Server (identity, audit,
// secrets, upstreams) and logs a user in, returning the mux + the session
// cookie to reuse on authenticated requests.
func newSecretsAPITestServer(t *testing.T) (*http.ServeMux, *http.Cookie) {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "api-secrets.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	id := identity.New(db)
	if _, err := id.CreateUser(context.Background(), "tester", "long-enough-password"); err != nil {
		t.Fatalf("create user: %v", err)
	}
	auditLog := audit.New(db)

	key, _ := sealbox.LoadOrCreateKey(dir, "secrets.key")
	cipher, _ := sealbox.NewCipher(key)
	secretsSvc := secrets.New(db, cipher, auditLog)

	gw := gateway.New(gateway.Options{
		Policy: policy.New(db), Approval: nil,
		Audit: auditLog, Memory: memory.New(db), Hub: realtime.NewHub(),
	})
	upSvc := upstreams.New(db, gw)
	upSvc.SetSecrets(secretsSvc)

	srv := New(context.Background(), Options{
		Identity:   id,
		Audit:      auditLog,
		Secrets:    secretsSvc,
		Upstreams:  upSvc,
		Gateway:    gw,
		SessionKey: []byte("0123456789abcdef0123456789abcdef"),
	})
	mux := http.NewServeMux()
	srv.Routes(mux)

	// Log in to obtain a session cookie.
	loginBody, _ := json.Marshal(map[string]string{"Username": "tester", "Password": "long-enough-password"})
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/login", bytes.NewReader(loginBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login: status %d, body %s", rec.Code, rec.Body.String())
	}
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("no session cookie issued on login")
	}
	return mux, cookie
}

func doAuthed(t *testing.T, mux *http.ServeMux, cookie *http.Cookie, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.AddCookie(cookie)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Requested-With", "toolyard")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// TestSecretValueNeverLeaks is the load-bearing test for the broker: a sentinel
// secret value must not appear in any API response across /v1/secrets,
// /v1/servers, or /v1/audit.
func TestSecretValueNeverLeaks(t *testing.T) {
	const sentinel = "SUPERSECRETSENTINEL_dead_beef_123"
	mux, cookie := newSecretsAPITestServer(t)

	// Create the secret.
	rec := doAuthed(t, mux, cookie, http.MethodPost, "/v1/secrets",
		`{"name":"SENTINEL_KEY","value":"`+sentinel+`","description":"leak test"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("create secret: status %d, body %s", rec.Code, rec.Body.String())
	}

	// Reference it from an upstream (connect fails since the command is bogus,
	// but the row persists with the ref).
	rec = doAuthed(t, mux, cookie, http.MethodPost, "/v1/servers",
		`{"name":"refsrv","transport":"stdio","command":"/nonexistent-toolyard-xyz","env":{"KEY":"secret://SENTINEL_KEY"}}`)
	if rec.Code != http.StatusOK && rec.Code != http.StatusAccepted {
		t.Fatalf("add server: status %d, body %s", rec.Code, rec.Body.String())
	}

	// Now sweep every read surface for the sentinel.
	for _, path := range []string{"/v1/secrets", "/v1/servers", "/v1/audit?limit=1000"} {
		rec := doAuthed(t, mux, cookie, http.MethodGet, path, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: status %d body %s", path, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), sentinel) {
			t.Fatalf("SENTINEL LEAKED in %s response: %s", path, rec.Body.String())
		}
	}

	// The /v1/servers response should still show the ref (refs aren't secret).
	rec = doAuthed(t, mux, cookie, http.MethodGet, "/v1/servers", "")
	if !strings.Contains(rec.Body.String(), "secret://SENTINEL_KEY") {
		t.Fatalf("expected the secret ref to be visible: %s", rec.Body.String())
	}
}

func TestSecretDeleteBlockedWhenReferenced(t *testing.T) {
	mux, cookie := newSecretsAPITestServer(t)
	doAuthed(t, mux, cookie, http.MethodPost, "/v1/secrets", `{"name":"USED_KEY","value":"v"}`)
	doAuthed(t, mux, cookie, http.MethodPost, "/v1/servers",
		`{"name":"srv2","transport":"stdio","command":"/nonexistent-xyz","env":{"K":"secret://USED_KEY"}}`)

	rec := doAuthed(t, mux, cookie, http.MethodDelete, "/v1/secrets/USED_KEY", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("delete referenced secret: status %d, want 409, body %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if _, ok := body["used_by"]; !ok {
		t.Fatalf("409 should list used_by: %s", rec.Body.String())
	}
	// Force delete succeeds.
	rec = doAuthed(t, mux, cookie, http.MethodDelete, "/v1/secrets/USED_KEY?force=1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("force delete: status %d, body %s", rec.Code, rec.Body.String())
	}
}
