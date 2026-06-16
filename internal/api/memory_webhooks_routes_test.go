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
	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/mempalace"
	"github.com/tusharbhardwaj/toolyard/internal/memwebhook"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// stubMemIngester satisfies memwebhook.MemIngester without a live MemPalace.
type stubMemIngester struct {
	available bool
	lastWing  string
	lastEntry string
	lastAgent string
	calls     int
}

func (s *stubMemIngester) Available() bool { return s.available }

func (s *stubMemIngester) IngestTagged(_ context.Context, entry, topic, wing, agent string) (*mempalace.IngestResult, error) {
	s.calls++
	s.lastWing = wing
	s.lastEntry = entry
	s.lastAgent = agent
	return &mempalace.IngestResult{OK: true, Detail: "stored"}, nil
}

// newMemWebhookAPITestServer composes the api.Server with the production-like
// body-cap middleware chain (so the 413 path is exercised), logs a user in, and
// returns the handler + session cookie + ingester stub + webhook service.
func newMemWebhookAPITestServer(t *testing.T, webhookMax int64) (http.Handler, *http.Cookie, *stubMemIngester, *audit.Logger) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "mw-api.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	id := identity.New(db)
	if _, err := id.CreateUser(context.Background(), "tester", "long-enough-password"); err != nil {
		t.Fatalf("create user: %v", err)
	}
	auditLog := audit.New(db)
	ing := &stubMemIngester{available: true}
	mwSvc := memwebhook.New(db, ing, auditLog)

	srv := New(context.Background(), Options{
		Identity:        id,
		Audit:           auditLog,
		MemWebhooks:     mwSvc,
		WebhookMaxBytes: webhookMax,
		SessionKey:      []byte("0123456789abcdef0123456789abcdef"),
	})
	mux := http.NewServeMux()
	srv.Routes(mux)

	// Mirror cmd/gateway's body-cap composition so the ingest route gets the
	// large cap and the 413 path is testable.
	var handler http.Handler = mux
	handler = LimitBodyByPath(handler, 4<<20, map[string]int64{MemoryWebhookIngestPath: webhookMax})
	handler = srv.HardenAPI(handler)

	loginBody, _ := json.Marshal(map[string]string{"Username": "tester", "Password": "long-enough-password"})
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/login", bytes.NewReader(loginBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Requested-With", "toolyard")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login: status %d body %s", rec.Code, rec.Body.String())
	}
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("no session cookie issued")
	}
	return handler, cookie, ing, auditLog
}

func mwAuthed(t *testing.T, h http.Handler, cookie *http.Cookie, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.AddCookie(cookie)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Requested-With", "toolyard")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func mwIngest(t *testing.T, h http.Handler, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, MemoryWebhookIngestPath, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// createWebhook POSTs a webhook and returns its plaintext token.
func createWebhook(t *testing.T, h http.Handler, cookie *http.Cookie, body string) (id, token string) {
	t.Helper()
	rec := mwAuthed(t, h, cookie, http.MethodPost, "/v1/memory/webhooks", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("create webhook: status %d body %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Webhook map[string]any `json:"webhook"`
		Token   string         `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	if resp.Token == "" {
		t.Fatal("create did not return a token")
	}
	return resp.Webhook["id"].(string), resp.Token
}

func TestMemWebhookIngestHappyPath(t *testing.T) {
	h, cookie, ing, _ := newMemWebhookAPITestServer(t, 1<<20)
	_, token := createWebhook(t, h, cookie, `{"name":"meetings","wing":"meetings","payload_spec":{"mode":"field","entry_field":"transcript"}}`)

	rec := mwIngest(t, h, token, `{"transcript":"we shipped TEC-481"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("ingest: status %d body %s", rec.Code, rec.Body.String())
	}
	var res map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if res["ok"] != true || res["wing"] != "meetings" {
		t.Fatalf("unexpected result: %v", res)
	}
	if res["request_id"] == nil || !strings.HasPrefix(res["request_id"].(string), "req_") {
		t.Fatalf("missing request_id: %v", res)
	}
	if ing.calls != 1 || ing.lastWing != "meetings" {
		t.Fatalf("ingester calls=%d wing=%q", ing.calls, ing.lastWing)
	}
	if ing.lastEntry != strings.TrimSpace(ing.lastEntry) || !strings.Contains(ing.lastEntry, "we shipped TEC-481") {
		t.Fatalf("entry not mapped: %q", ing.lastEntry)
	}
}

func TestMemWebhookIngestBearerNoCookieNoCSRF(t *testing.T) {
	// The ingest path must work with only a bearer token: no session cookie,
	// no X-Requested-With (it is CSRF-exempt).
	h, cookie, _, _ := newMemWebhookAPITestServer(t, 1<<20)
	_, token := createWebhook(t, h, cookie, `{"name":"w","wing":"meetings"}`)

	req := httptest.NewRequest(http.MethodPost, MemoryWebhookIngestPath, strings.NewReader(`{"x":1}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("bearer-only ingest: status %d body %s", rec.Code, rec.Body.String())
	}
}

func TestMemWebhookBadAuthRejected(t *testing.T) {
	h, cookie, ing, auditLog := newMemWebhookAPITestServer(t, 1<<20)
	createWebhook(t, h, cookie, `{"name":"w","wing":"meetings"}`)

	// Missing token.
	if rec := mwIngest(t, h, "", `{"x":1}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing token: status %d", rec.Code)
	}
	// Bogus token.
	if rec := mwIngest(t, h, "mwh_bogus.nope", `{"x":1}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bogus token: status %d", rec.Code)
	}
	if ing.calls != 0 {
		t.Fatalf("auth failures must not reach mempalace, got %d calls", ing.calls)
	}

	// Both auth failures must be audited.
	evs, err := auditLog.Query(context.Background(), audit.Filter{EventType: "memory_webhook.auth_failed", Limit: 10})
	if err != nil {
		t.Fatalf("audit query: %v", err)
	}
	if len(evs) < 2 {
		t.Fatalf("expected >=2 audited auth failures, got %d", len(evs))
	}
}

func TestMemWebhookWingCannotBeOverridden(t *testing.T) {
	h, cookie, ing, _ := newMemWebhookAPITestServer(t, 1<<20)
	_, token := createWebhook(t, h, cookie, `{"name":"locked","wing":"meetings"}`)

	rec := mwIngest(t, h, token, `{"wing":"attacker-wing","closet":"x","transcript":"sneaky"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("ingest: status %d body %s", rec.Code, rec.Body.String())
	}
	if ing.lastWing != "meetings" {
		t.Fatalf("wing override succeeded: upstream wing = %q, want meetings", ing.lastWing)
	}
}

func TestMemWebhookUsesMemPalaceSafeAgentName(t *testing.T) {
	h, cookie, ing, _ := newMemWebhookAPITestServer(t, 1<<20)
	_, token := createWebhook(t, h, cookie, `{"name":"from n8n Google Meeting Transcript To Linear Ticket","wing":"meetings"}`)

	rec := mwIngest(t, h, token, `{"transcript":"safe agent name regression"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("ingest: status %d body %s", rec.Code, rec.Body.String())
	}
	if got, want := ing.lastAgent, "webhook_from_n8n_Google_Meeting_Transcript_To_Linear_Ticket"; got != want {
		t.Fatalf("agent name = %q, want %q", got, want)
	}
}

func TestMemWebhookOversizedPayload413(t *testing.T) {
	// Cap is 2 MiB — above the standard 1 MiB control-plane decode cap and the
	// /v1/memory per-route cap, proving the ingest route is NOT clamped to them.
	const cap = 2 << 20
	h, cookie, ing, _ := newMemWebhookAPITestServer(t, cap)
	_, token := createWebhook(t, h, cookie, `{"name":"big","wing":"meetings","payload_spec":{"mode":"field","entry_field":"transcript"}}`)

	// 1.5 MiB payload (> 1 MiB) must succeed under the 2 MiB cap.
	big := `{"transcript":"` + strings.Repeat("x", 1500*1024) + `"}`
	if rec := mwIngest(t, h, token, big); rec.Code != http.StatusOK {
		t.Fatalf("1.5MiB payload: status %d (want 200) body %s", rec.Code, truncateForLog(rec.Body.String()))
	}
	if ing.calls != 1 {
		t.Fatalf("expected the large payload to be ingested, calls=%d", ing.calls)
	}

	// 2.5 MiB payload (> 2 MiB cap) must be rejected with 413.
	tooBig := `{"transcript":"` + strings.Repeat("y", 2500*1024) + `"}`
	rec := mwIngest(t, h, token, tooBig)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized payload: status %d, want 413; body %s", rec.Code, truncateForLog(rec.Body.String()))
	}
	if ing.calls != 1 {
		t.Fatalf("oversized payload must not reach mempalace, calls=%d", ing.calls)
	}
}

func TestMemWebhookSchemaViolation422(t *testing.T) {
	h, cookie, _, _ := newMemWebhookAPITestServer(t, 1<<20)
	_, token := createWebhook(t, h, cookie,
		`{"name":"req","wing":"w","payload_spec":{"mode":"whole","required_fields":["transcript"]}}`)

	rec := mwIngest(t, h, token, `{"title":"no transcript"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("schema violation: status %d, want 422; body %s", rec.Code, rec.Body.String())
	}
}

func TestMemWebhookManagementRequiresSession(t *testing.T) {
	h, _, _, _ := newMemWebhookAPITestServer(t, 1<<20)
	// No cookie → 401.
	req := httptest.NewRequest(http.MethodGet, "/v1/memory/webhooks", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated list: status %d, want 401", rec.Code)
	}
}

func TestMemWebhookListAndDeleteNeverLeakToken(t *testing.T) {
	h, cookie, _, _ := newMemWebhookAPITestServer(t, 1<<20)
	id, token := createWebhook(t, h, cookie, `{"name":"w","wing":"meetings"}`)

	rec := mwAuthed(t, h, cookie, http.MethodGet, "/v1/memory/webhooks", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list: status %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), token) {
		t.Fatalf("list leaked the token: %s", rec.Body.String())
	}

	// Rotate returns a new token and invalidates the old one.
	rec = mwAuthed(t, h, cookie, http.MethodPost, "/v1/memory/webhooks/"+id+"/rotate-token", "{}")
	if rec.Code != http.StatusOK {
		t.Fatalf("rotate: status %d body %s", rec.Code, rec.Body.String())
	}
	if got := mwIngest(t, h, token, `{"x":1}`); got.Code != http.StatusUnauthorized {
		t.Fatalf("old token after rotate should 401, got %d", got.Code)
	}

	// Delete revokes the webhook.
	rec = mwAuthed(t, h, cookie, http.MethodDelete, "/v1/memory/webhooks/"+id, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: status %d body %s", rec.Code, rec.Body.String())
	}
}

func TestMemWebhookMetricsShape(t *testing.T) {
	h, cookie, _, _ := newMemWebhookAPITestServer(t, 1<<20)
	_, token := createWebhook(t, h, cookie, `{"name":"m","wing":"meetings"}`)
	mwIngest(t, h, token, `{"x":1}`)

	rec := mwAuthed(t, h, cookie, http.MethodGet, "/v1/memory/metrics", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics: status %d body %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Mempalace map[string]any `json:"mempalace"`
		Ledger    struct {
			Totals       map[string]any `json:"totals"`
			WebhookCount int            `json:"webhook_count"`
		} `json:"ledger"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode metrics: %v body %s", err, rec.Body.String())
	}
	if resp.Ledger.WebhookCount != 1 {
		t.Fatalf("expected webhook_count 1, got %d", resp.Ledger.WebhookCount)
	}
	if resp.Ledger.Totals["total"] == nil {
		t.Fatalf("metrics missing totals: %s", rec.Body.String())
	}
}

func truncateForLog(s string) string {
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}
