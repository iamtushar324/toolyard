package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/gateway"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/jobs"
	"github.com/tusharbhardwaj/toolyard/internal/memory"
	"github.com/tusharbhardwaj/toolyard/internal/policy"
	"github.com/tusharbhardwaj/toolyard/internal/realtime"
	"github.com/tusharbhardwaj/toolyard/internal/store/storetest"
)

// jobsEnv is a Server behind the production-like middleware chain with
// async jobs on, two agents of one owner and an operator token. The
// fixture.echo tool is allowed by policy.
type jobsEnv struct {
	h     http.Handler
	tokA  string
	tokB  string
	opTok string
}

const jobsRouteReason = "check the async job routes answer correctly"

func newJobsEnv(t *testing.T) *jobsEnv {
	t.Helper()
	ctx := context.Background()
	db := storetest.Open(t)
	id := identity.New(db)
	owner, err := id.CreateUser(ctx, "owner", "long-enough-password")
	if err != nil {
		t.Fatal(err)
	}
	tokA, _, err := id.CreateAgentWithToken(ctx, owner.ID, "agent-a")
	if err != nil {
		t.Fatal(err)
	}
	tokB, _, err := id.CreateAgentWithToken(ctx, owner.ID, "agent-b")
	if err != nil {
		t.Fatal(err)
	}
	opTok, _, err := id.CreateOperatorToken(ctx, owner.ID, "cli", []string{"owner"}, 0, "cli")
	if err != nil {
		t.Fatal(err)
	}
	pol := policy.New(db)
	if _, err := pol.Set(ctx, policy.ScopeTool, "fixture.echo", "allow", "", false); err != nil {
		t.Fatal(err)
	}
	svc := jobs.New(db, jobs.Options{PollEvery: 20 * time.Millisecond})
	auditLog := audit.New(db)
	gw := gateway.New(gateway.Options{Policy: pol, Audit: auditLog, Memory: memory.New(db), Hub: realtime.NewHub(), Jobs: svc})
	gw.RegisterBuiltins()
	t.Cleanup(func() {
		wctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		svc.Wait(wctx)
		_ = gw.Close()
	})
	srvCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	srv := New(srvCtx, Options{
		Identity: id, Audit: auditLog, Gateway: gw, Jobs: svc,
		SessionKey: []byte("0123456789abcdef0123456789abcdef"),
		Security:   SecurityOptions{PublicURL: "https://toolyard.example"},
	})
	mux := http.NewServeMux()
	srv.Routes(mux)
	var h http.Handler = srv.RoleGuard(mux)
	h = srv.HardenAPI(h)
	h = srv.EnforceOriginOnMutations(h)
	return &jobsEnv{h: h, tokA: tokA, tokB: tokB, opTok: opTok}
}

func (e *jobsEnv) do(t *testing.T, method, path, token, body string, hdr ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Add(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func startBody(message string) string {
	b, _ := json.Marshal(map[string]any{"tool": "fixture.echo", "arguments": map[string]any{"message": message}, "reason": jobsRouteReason})
	return string(b)
}

func decodeMap(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("not JSON (%v): %d %s", err, rec.Code, rec.Body.String())
	}
	return m
}

func TestJobRoutesStartReadAndScope(t *testing.T) {
	e := newJobsEnv(t)

	// Without a wait the answer is 202 with Location.
	rec := e.do(t, http.MethodPost, "/v1/jobs", e.tokA, startBody("first"))
	v := decodeMap(t, rec)
	id, _ := v["job_id"].(string)
	if rec.Code != http.StatusAccepted || !strings.HasPrefix(id, "job_") || rec.Header().Get("Location") != "/v1/jobs/"+id || v["via"] != "rest" {
		t.Fatalf("POST /v1/jobs = %d %s", rec.Code, rec.Body.String())
	}
	rec = e.do(t, http.MethodGet, "/v1/jobs/"+id, e.tokA, "", "Prefer", "wait=10")
	if v := decodeMap(t, rec); rec.Code != http.StatusOK || v["status"] != jobs.StatusSucceeded || v["result_available"] != true {
		t.Fatalf("GET job = %d %s", rec.Code, rec.Body.String())
	}
	rec = e.do(t, http.MethodGet, "/v1/jobs/"+id+"/result", e.tokA, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "echo: first") {
		t.Fatalf("GET result = %d %s", rec.Code, rec.Body.String())
	}

	// With a wait the final view comes back at once.
	rec = e.do(t, http.MethodPost, "/v1/jobs", e.tokA, startBody("second"), "Prefer", "wait=10")
	if v := decodeMap(t, rec); rec.Code != http.StatusOK || v["status"] != jobs.StatusSucceeded {
		t.Fatalf("POST with wait = %d %s", rec.Code, rec.Body.String())
	}

	// Another agent of the same owner cannot read it.
	if rec := e.do(t, http.MethodGet, "/v1/jobs/"+id, e.tokB, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("agent B read agent A's job: %d %s", rec.Code, rec.Body.String())
	}
	// Operator tokens never reach the job routes.
	if rec := e.do(t, http.MethodGet, "/v1/jobs", e.opTok, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("operator token = %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do(t, http.MethodGet, "/v1/jobs", "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token = %d", rec.Code)
	}

	rec = e.do(t, http.MethodGet, "/v1/jobs?status=succeeded", e.tokA, "")
	list := decodeMap(t, rec)
	if jl, _ := list["jobs"].([]any); rec.Code != http.StatusOK || len(jl) != 2 {
		t.Fatalf("GET /v1/jobs = %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do(t, http.MethodGet, "/v1/jobs", e.tokB, ""); !strings.Contains(rec.Body.String(), `"jobs":[]`) {
		t.Fatalf("agent B lists agent A's jobs: %s", rec.Body.String())
	}

	rec = e.do(t, http.MethodPost, "/v1/jobs/"+id+"/cancel", e.tokA, "")
	if c := decodeMap(t, rec); rec.Code != http.StatusOK || c["cancel_accepted"] != false {
		t.Fatalf("cancel a finished job = %d %s", rec.Code, rec.Body.String())
	}
}

func TestJobRoutesIdempotencyAndRefusals(t *testing.T) {
	e := newJobsEnv(t)
	first := decodeMap(t, e.do(t, http.MethodPost, "/v1/jobs", e.tokA, startBody("once"), "Idempotency-Key", "k-1"))
	again := decodeMap(t, e.do(t, http.MethodPost, "/v1/jobs", e.tokA, startBody("once"), "Idempotency-Key", "k-1"))
	if first["job_id"] == nil || first["job_id"] != again["job_id"] {
		t.Fatalf("replay = %v then %v", first["job_id"], again["job_id"])
	}
	if rec := e.do(t, http.MethodPost, "/v1/jobs", e.tokA, startBody("other"), "Idempotency-Key", "k-1"); rec.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(rec.Body.String(), "idempotency_key_reused") {
		t.Fatalf("key reused for other arguments = %d %s", rec.Code, rec.Body.String())
	}

	// A tool the gateway does not know starts nothing.
	unknown := `{"tool":"nope.missing","arguments":{},"reason":"` + jobsRouteReason + `"}`
	rec := e.do(t, http.MethodPost, "/v1/jobs", e.tokA, unknown)
	if v := decodeMap(t, rec); rec.Code != http.StatusUnprocessableEntity || v["status"] != "not_started" || v["job_id"] != nil {
		t.Fatalf("unknown tool = %d %s", rec.Code, rec.Body.String())
	}
	for name, body := range map[string]string{
		"short reason": `{"tool":"fixture.echo","arguments":{},"reason":"too short"}`,
		"no tool":      `{"arguments":{},"reason":"` + jobsRouteReason + `"}`,
		"extra field":  `{"tool":"fixture.echo","reason":"` + jobsRouteReason + `","other":1}`,
		"bad wait":     `{"tool":"fixture.echo","reason":"` + jobsRouteReason + `","wait_seconds":61}`,
	} {
		if rec := e.do(t, http.MethodPost, "/v1/jobs", e.tokA, body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	if rec := e.do(t, http.MethodGet, "/v1/jobs/job_missing", e.tokA, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("missing job = %d", rec.Code)
	}
	if rec := e.do(t, http.MethodGet, "/v1/jobs?status=weird", e.tokA, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad status filter = %d", rec.Code)
	}
}
