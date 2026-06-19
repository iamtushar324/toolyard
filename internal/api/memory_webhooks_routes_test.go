package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/mempalace"
	"github.com/tusharbhardwaj/toolyard/internal/memwebhook"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// stubMemIngester satisfies memwebhook.MemIngester without a live MemPalace. It
// is read by the background job worker (a goroutine) and by the test, so its
// mutable state is guarded by a mutex.
type stubMemIngester struct {
	mu        sync.Mutex
	available bool // set once at construction; not mutated after the worker starts
	failErr   error
	reject    bool
	calls     int
	lastWing  string
	lastEntry string
	lastAgent string
}

func (s *stubMemIngester) Available() bool { return s.available }

func (s *stubMemIngester) IngestTagged(_ context.Context, entry, topic, wing, agent string) (*mempalace.IngestResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.lastWing = wing
	s.lastEntry = entry
	s.lastAgent = agent
	if s.failErr != nil {
		return nil, s.failErr
	}
	if s.reject {
		return &mempalace.IngestResult{OK: false, Detail: "upstream rejected"}, nil
	}
	return &mempalace.IngestResult{OK: true, Detail: "stored"}, nil
}

func (s *stubMemIngester) setFailErr(err error) { s.mu.Lock(); s.failErr = err; s.mu.Unlock() }
func (s *stubMemIngester) Calls() int           { s.mu.Lock(); defer s.mu.Unlock(); return s.calls }
func (s *stubMemIngester) LastWing() string     { s.mu.Lock(); defer s.mu.Unlock(); return s.lastWing }
func (s *stubMemIngester) LastEntry() string    { s.mu.Lock(); defer s.mu.Unlock(); return s.lastEntry }
func (s *stubMemIngester) LastAgent() string    { s.mu.Lock(); defer s.mu.Unlock(); return s.lastAgent }

// newMemWebhookAPITestServer composes the api.Server with the production-like
// body-cap middleware chain (so the 413 path is exercised), starts the async
// ingest worker, logs a user in, and returns the handler + session cookie +
// ingester stub + audit logger.
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

	// Run the async ingest worker for the lifetime of the test.
	workerCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = mwSvc.RunWorker(workerCtx) }()

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

// mwIngestAsync POSTs and asserts the 202 envelope, returning the job id.
func mwIngestAsync(t *testing.T, h http.Handler, token, body string) string {
	t.Helper()
	rec := mwIngest(t, h, token, body)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("ingest: status %d, want 202; body %s", rec.Code, truncateForLog(rec.Body.String()))
	}
	var res map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode 202: %v", err)
	}
	if res["status"] != "queued" {
		t.Fatalf("202 status = %v, want queued", res["status"])
	}
	id, _ := res["job_id"].(string)
	if !strings.HasPrefix(id, "req_") {
		t.Fatalf("missing job_id in 202: %v", res)
	}
	if su, _ := res["status_url"].(string); su != MemoryWebhookJobsPrefix+id {
		t.Fatalf("status_url = %q, want %q", su, MemoryWebhookJobsPrefix+id)
	}
	return id
}

// mwGetJob fetches a job's status with a bearer token.
func mwGetJob(t *testing.T, h http.Handler, token, jobID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, MemoryWebhookJobsPrefix+jobID, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// mwWaitJob polls the status endpoint until the job reaches want, then returns it.
func mwWaitJob(t *testing.T, h http.Handler, token, jobID, want string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var last map[string]any
	for time.Now().Before(deadline) {
		rec := mwGetJob(t, h, token, jobID)
		if rec.Code == http.StatusOK {
			var j map[string]any
			_ = json.Unmarshal(rec.Body.Bytes(), &j)
			last = j
			if j["status"] == want {
				return j
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("job %s did not reach status %q in time (last=%v)", jobID, want, last)
	return nil
}

// createWebhook POSTs a webhook and returns its id + plaintext token.
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

	// Async: 202 quickly, then poll to success.
	jobID := mwIngestAsync(t, h, token, `{"transcript":"we shipped TEC-482"}`)
	job := mwWaitJob(t, h, token, jobID, "succeeded")
	if job["wing"] != "meetings" || job["mempalace_ok"] != true {
		t.Fatalf("unexpected job: %v", job)
	}
	if ing.Calls() != 1 || ing.LastWing() != "meetings" {
		t.Fatalf("ingester calls=%d wing=%q", ing.Calls(), ing.LastWing())
	}
	if got := ing.LastEntry(); got != strings.TrimSpace(got) || !strings.Contains(got, "we shipped TEC-482") {
		t.Fatalf("entry not mapped: %q", got)
	}
}

func TestMemWebhookIngestBearerNoCookieNoCSRF(t *testing.T) {
	// The ingest path must work with only a bearer token: no session cookie,
	// no X-Requested-With (it is CSRF-exempt). It now returns 202.
	h, cookie, _, _ := newMemWebhookAPITestServer(t, 1<<20)
	_, token := createWebhook(t, h, cookie, `{"name":"w","wing":"meetings"}`)

	req := httptest.NewRequest(http.MethodPost, MemoryWebhookIngestPath, strings.NewReader(`{"x":1}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
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
	if ing.Calls() != 0 {
		t.Fatalf("auth failures must not reach mempalace, got %d calls", ing.Calls())
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

	jobID := mwIngestAsync(t, h, token, `{"wing":"attacker-wing","closet":"x","transcript":"sneaky"}`)
	mwWaitJob(t, h, token, jobID, "succeeded")
	if ing.LastWing() != "meetings" {
		t.Fatalf("wing override succeeded: upstream wing = %q, want meetings", ing.LastWing())
	}
}

func TestMemWebhookUsesMemPalaceSafeAgentName(t *testing.T) {
	h, cookie, ing, _ := newMemWebhookAPITestServer(t, 1<<20)
	_, token := createWebhook(t, h, cookie, `{"name":"from n8n Google Meeting Transcript To Linear Ticket","wing":"meetings"}`)

	jobID := mwIngestAsync(t, h, token, `{"transcript":"safe agent name regression"}`)
	mwWaitJob(t, h, token, jobID, "succeeded")
	if got, want := ing.LastAgent(), "webhook_from_n8n_Google_Meeting_Transcript_To_Linear_Ticket"; got != want {
		t.Fatalf("agent name = %q, want %q", got, want)
	}
}

func TestMemWebhookOversizedPayload413(t *testing.T) {
	// Cap is 2 MiB — above the standard 1 MiB control-plane decode cap and the
	// /v1/memory per-route cap, proving the ingest route is NOT clamped to them.
	const cap = 2 << 20
	h, cookie, ing, _ := newMemWebhookAPITestServer(t, cap)
	_, token := createWebhook(t, h, cookie, `{"name":"big","wing":"meetings","payload_spec":{"mode":"field","entry_field":"transcript"}}`)

	// 1.5 MiB payload (> 1 MiB) must be accepted (202) under the 2 MiB cap. The
	// worker chunks it, so it reaches MemPalace as one or more writes.
	big := `{"transcript":"` + strings.Repeat("x", 1500*1024) + `"}`
	jobID := mwIngestAsync(t, h, token, big)
	mwWaitJob(t, h, token, jobID, "succeeded")
	afterFirst := ing.Calls()
	if afterFirst < 1 {
		t.Fatalf("expected the large payload to be ingested, calls=%d", afterFirst)
	}

	// 2.5 MiB payload (> 2 MiB cap) must be rejected with 413, synchronously,
	// before any job is queued.
	tooBig := `{"transcript":"` + strings.Repeat("y", 2500*1024) + `"}`
	rec := mwIngest(t, h, token, tooBig)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized payload: status %d, want 413; body %s", rec.Code, truncateForLog(rec.Body.String()))
	}
	if ing.Calls() != afterFirst {
		t.Fatalf("oversized payload must not reach mempalace, calls=%d (was %d)", ing.Calls(), afterFirst)
	}
}

func TestMemWebhookLargePayloadChunkedAsync(t *testing.T) {
	const cap = 2 << 20
	h, cookie, ing, _ := newMemWebhookAPITestServer(t, cap)
	_, token := createWebhook(t, h, cookie, `{"name":"chunky","wing":"meetings","payload_spec":{"mode":"field","entry_field":"transcript"}}`)

	// A large, multi-line transcript so the worker splits it into chunks.
	transcript := strings.Repeat("meeting transcript line with several words here\n", 40000)
	body, _ := json.Marshal(map[string]string{"transcript": transcript})

	jobID := mwIngestAsync(t, h, token, string(body))
	job := mwWaitJob(t, h, token, jobID, "succeeded")

	cc, _ := job["chunk_count"].(float64)
	if cc < 2 {
		t.Fatalf("large payload should be chunked, chunk_count=%v", job["chunk_count"])
	}
	if ing.Calls() < 2 {
		t.Fatalf("expected multiple chunk writes, got %d", ing.Calls())
	}
	if ing.LastWing() != "meetings" {
		t.Fatalf("chunks must stay in the locked wing, got %q", ing.LastWing())
	}
}

func TestMemWebhookJobMemPalaceFailure(t *testing.T) {
	h, cookie, ing, _ := newMemWebhookAPITestServer(t, 1<<20)
	ing.setFailErr(errors.New("embed exploded"))
	_, token := createWebhook(t, h, cookie, `{"name":"f","wing":"meetings","payload_spec":{"mode":"field","entry_field":"transcript"}}`)

	jobID := mwIngestAsync(t, h, token, `{"transcript":"will fail downstream"}`)
	job := mwWaitJob(t, h, token, jobID, "failed")
	if job["mempalace_ok"] != false {
		t.Fatalf("mempalace_ok should be false: %v", job)
	}
	if e, _ := job["error"].(string); e == "" {
		t.Fatalf("failed job must carry an error reason: %v", job)
	}
}

func TestMemWebhookJobScopedToWebhook(t *testing.T) {
	h, cookie, _, _ := newMemWebhookAPITestServer(t, 1<<20)
	_, tokenA := createWebhook(t, h, cookie, `{"name":"a","wing":"wa"}`)
	_, tokenB := createWebhook(t, h, cookie, `{"name":"b","wing":"wb"}`)

	jobID := mwIngestAsync(t, h, tokenA, `{"x":1}`)
	mwWaitJob(t, h, tokenA, jobID, "succeeded")

	// Webhook B's token must not read webhook A's job — 404, no existence leak.
	if rec := mwGetJob(t, h, tokenB, jobID); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-webhook job read: status %d, want 404", rec.Code)
	}
	// Missing / bogus token → 401.
	if rec := mwGetJob(t, h, "", jobID); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: status %d, want 401", rec.Code)
	}
	if rec := mwGetJob(t, h, "mwh_bogus.nope", jobID); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad token: status %d, want 401", rec.Code)
	}
	// Unknown job id for a valid webhook → 404.
	if rec := mwGetJob(t, h, tokenA, "req_does-not-exist"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown job: status %d, want 404", rec.Code)
	}
}

func TestMemWebhookJobStatusNeverLeaksToken(t *testing.T) {
	h, cookie, _, _ := newMemWebhookAPITestServer(t, 1<<20)
	_, token := createWebhook(t, h, cookie, `{"name":"leak","wing":"meetings","payload_spec":{"mode":"field","entry_field":"transcript"}}`)

	// The 202 envelope must not contain the token.
	rec := mwIngest(t, h, token, `{"transcript":"secret words"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("ingest: status %d, want 202", rec.Code)
	}
	if strings.Contains(rec.Body.String(), token) {
		t.Fatalf("202 envelope leaked the token: %s", rec.Body.String())
	}
	var env map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	jobID := env["job_id"].(string)

	// Neither must the status body — nor the mapped entry text.
	job := mwWaitJob(t, h, token, jobID, "succeeded")
	b, _ := json.Marshal(job)
	if strings.Contains(string(b), token) {
		t.Fatalf("job status leaked the token: %s", b)
	}
	if strings.Contains(string(b), "secret words") || strings.Contains(string(b), "\"entry\"") {
		t.Fatalf("job status leaked the entry text: %s", b)
	}
}

func TestMemWebhookSchemaViolation422(t *testing.T) {
	h, cookie, ing, _ := newMemWebhookAPITestServer(t, 1<<20)
	_, token := createWebhook(t, h, cookie,
		`{"name":"req","wing":"w","payload_spec":{"mode":"whole","required_fields":["transcript"]}}`)

	// Schema violations are rejected synchronously (422), before any job queues.
	rec := mwIngest(t, h, token, `{"title":"no transcript"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("schema violation: status %d, want 422; body %s", rec.Code, rec.Body.String())
	}
	if ing.Calls() != 0 {
		t.Fatalf("rejected payload must not reach mempalace, calls=%d", ing.Calls())
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
	jobID := mwIngestAsync(t, h, token, `{"x":1}`)
	mwWaitJob(t, h, token, jobID, "succeeded")

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
	// The completed job wrote a terminal ledger row.
	if tot, _ := resp.Ledger.Totals["total"].(float64); tot != 1 {
		t.Fatalf("metrics total = %v, want 1: %s", resp.Ledger.Totals["total"], rec.Body.String())
	}
}

func truncateForLog(s string) string {
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}
