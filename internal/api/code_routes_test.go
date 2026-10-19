package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/codemode"
	"github.com/tusharbhardwaj/toolyard/internal/coderuns"
	"github.com/tusharbhardwaj/toolyard/internal/gateway"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/inbox"
	"github.com/tusharbhardwaj/toolyard/internal/memory"
	"github.com/tusharbhardwaj/toolyard/internal/policy"
	"github.com/tusharbhardwaj/toolyard/internal/realtime"
	"github.com/tusharbhardwaj/toolyard/internal/store"
	"github.com/tusharbhardwaj/toolyard/internal/store/storetest"
)

// scriptRunner stands in for the code-mode runtime: each test script names
// the outcome it gets. The run service's hooks, the worker and the gateway
// pipeline are covered in internal/coderuns; this tests the HTTP surface.
type scriptRunner struct {
	busy        atomic.Bool
	runs        atomic.Int32
	release     chan struct{}
	releaseOnce sync.Once
}

// releaseSlow lets every slow() script finish.
func (r *scriptRunner) releaseSlow() { r.releaseOnce.Do(func() { close(r.release) }) }

func (r *scriptRunner) AcquireSlot(context.Context) (*codemode.Slot, error) {
	if r.busy.Load() {
		return nil, codemode.ErrBusy
	}
	return nil, nil
}

func (r *scriptRunner) Run(ctx context.Context, code string, opts codemode.RunOptions) (*codemode.RunResult, error) {
	r.runs.Add(1)
	call := func(status string) []codemode.CallRecord {
		return []codemode.CallRecord{{Seq: 1, Tool: "demo.write", Target: "demo.write", Status: status, StartedAt: 1, FinishedAt: 2}}
	}
	switch strings.TrimSpace(code) {
	case "result = 1":
		return &codemode.RunResult{Status: codemode.StatusSucceeded, Result: json.RawMessage(`{"v":1}`), Logs: []string{"hello"},
			Calls: []codemode.CallRecord{{Seq: 1, Tool: "demo.read", Target: "demo.read", Status: codemode.StatusSucceeded, StartedAt: 1, FinishedAt: 2}}}, nil
	case "fail()":
		return &codemode.RunResult{Status: codemode.StatusFailed, ErrorKind: codemode.ErrorToolError, ErrorMessage: "tool call failed for demo.write: boom"}, nil
	case "hold()":
		// One call to demo.write that runs only with the grant tok-w, as
		// the gateway would: the hooks add the grant on a resume.
		pc := &codemode.PendingCall{Seq: 1, Tool: "demo.write", Target: "demo.write",
			Args: map[string]any{"x": 1.0}, ScriptArgs: map[string]any{"x": 1.0}}
		if opts.Hooks != nil {
			if _, _, err := opts.Hooks.BeforeCall(ctx, pc); err != nil {
				return &codemode.RunResult{Status: codemode.StatusFailed, ErrorKind: codemode.ErrorScriptDiverged, ErrorMessage: err.Error(), Calls: call(codemode.StatusFailed)}, nil
			}
		}
		if pc.Args["_grant"] == "tok-w" {
			return &codemode.RunResult{Status: codemode.StatusSucceeded, Result: json.RawMessage(`{"ok":true}`), Calls: call(codemode.StatusSucceeded)}, nil
		}
		return &codemode.RunResult{Status: codemode.StatusPermissionRequired, Calls: call(codemode.StatusPermissionRequired),
			Held: &codemode.HeldCall{Seq: 1, Tool: "demo.write", Target: "demo.write", Status: codemode.StatusPermissionRequired,
				Draft: json.RawMessage(`{"title":"Write x"}`), Message: "demo.write is restricted"}}, nil
	case "defer()":
		return &codemode.RunResult{Status: codemode.StatusDeferred, Calls: call(codemode.StatusDeferred),
			Held: &codemode.HeldCall{Seq: 1, Tool: "demo.write", Target: "demo.write", Status: codemode.StatusDeferred, ApprovalID: "ap_1"}}, nil
	case "unknown()":
		return &codemode.RunResult{Status: codemode.StatusOutcomeUnknown, ErrorKind: codemode.ErrorOutcomeUnknown,
			ErrorMessage: "the outcome is unknown", Calls: call(codemode.StatusOutcomeUnknown)}, nil
	case "slow()":
		select {
		case <-r.release:
		case <-ctx.Done():
		}
		return &codemode.RunResult{Status: codemode.StatusSucceeded}, nil
	}
	return &codemode.RunResult{Status: codemode.StatusFailed, ErrorKind: codemode.ErrorScriptError, ErrorMessage: "unknown test script"}, nil
}

type scriptEngine struct {
	r        *scriptRunner
	draining atomic.Bool
}

func (e *scriptEngine) Runner() coderuns.Runner { return e.r }

func (e *scriptEngine) RunContext(ctx context.Context, _, _ string) (context.Context, bool) {
	return ctx, !e.draining.Load()
}

// approvedGrants knows one approved request, rq_1, granting demo.write.
type approvedGrants struct{}

func (approvedGrants) Status(_ context.Context, _ string, ids []string) ([]inbox.AgentView, error) {
	out := make([]inbox.AgentView, 0, len(ids))
	for _, id := range ids {
		if id == "rq_1" {
			out = append(out, inbox.AgentView{RequestID: id, Status: "approved",
				Tools: []inbox.AgentToolView{{CallID: "call_1", Tool: "demo.write", Decision: "allowed", Grant: "tok-w"}}})
			continue
		}
		out = append(out, inbox.AgentView{RequestID: id, Status: "not_found"})
	}
	return out, nil
}

// codeEnv is a Server with every route behind the production-like Origin
// check -> HardenAPI -> RoleGuard -> mux chain, two agents of one owner, an
// operator token, and a run service on scriptRunner.
type codeEnv struct {
	h      http.Handler
	db     *store.DB
	id     *identity.Service
	tokA   string
	agA    *identity.Agent
	tokB   string
	opTok  string
	runner *scriptRunner
	engine *scriptEngine
}

func newCodeEnv(t *testing.T) *codeEnv {
	t.Helper()
	ctx := context.Background()
	db := storetest.Open(t)
	id := identity.New(db)
	owner, err := id.CreateUser(ctx, "owner", "long-enough-password")
	if err != nil {
		t.Fatal(err)
	}
	tokA, agA, err := id.CreateAgentWithToken(ctx, owner.ID, "agent-a")
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
	auditLog := audit.New(db)
	gw := gateway.New(gateway.Options{Policy: policy.New(db), Audit: auditLog, Memory: memory.New(db), Hub: realtime.NewHub()})
	gw.RegisterBuiltins()
	t.Cleanup(func() { _ = gw.Close() })

	runner := &scriptRunner{release: make(chan struct{})}
	engine := &scriptEngine{r: runner}
	runs := coderuns.New(db, coderuns.Options{Engine: engine, Grants: approvedGrants{}, PollEvery: 20 * time.Millisecond})
	t.Cleanup(func() {
		runner.releaseSlow()
		wctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		runs.Wait(wctx)
	})
	srvCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	srv := New(srvCtx, Options{
		Identity: id, Audit: auditLog, Gateway: gw, CodeRuns: runs,
		SessionKey: []byte("0123456789abcdef0123456789abcdef"),
		Security:   SecurityOptions{PublicURL: "https://toolyard.example"},
	})
	mux := http.NewServeMux()
	srv.Routes(mux)
	var h http.Handler = srv.RoleGuard(mux)
	h = srv.HardenAPI(h)
	h = srv.EnforceOriginOnMutations(h)
	return &codeEnv{h: h, db: db, id: id, tokA: tokA, agA: agA, tokB: tokB, opTok: opTok, runner: runner, engine: engine}
}

// do sends what curl sends: a bearer (when token is set), the headers, no
// cookie, no Origin and no X-Requested-With.
func (e *codeEnv) do(t *testing.T, method, path, token, body string, hdr ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Add(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

const codeTestReason = "check the code route answers for TEC-1627"

// run posts a raw script.
func (e *codeEnv) run(t *testing.T, token, code string, hdr ...string) *httptest.ResponseRecorder {
	t.Helper()
	h := append([]string{"Content-Type", "text/x-starlark", "X-Toolyard-Reason", codeTestReason}, hdr...)
	return e.do(t, http.MethodPost, "/v1/code/run", token, code, h...)
}

func runView(t *testing.T, rec *httptest.ResponseRecorder) coderuns.View {
	t.Helper()
	var v coderuns.View
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil || v.RunID == "" {
		t.Fatalf("not a run view (%v): %d %s", err, rec.Code, rec.Body.String())
	}
	return v
}

func (e *codeEnv) runCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := e.db.QueryRow(`SELECT count(*) FROM code_runs`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestCodeRunHTTPStatusPerRunStatus(t *testing.T) {
	e := newCodeEnv(t)

	rec := e.run(t, e.tokA, "result = 1")
	v := runView(t, rec)
	if rec.Code != http.StatusOK || v.Status != codemode.StatusSucceeded || string(v.Result) != `{"v":1}` || v.Error != nil || v.Held != nil ||
		len(v.Logs) != 1 || v.Logs[0] != "hello" || len(v.Calls) != 1 || v.Calls[0].Tool != "demo.read" || v.Links.Self != "/v1/code/runs/"+v.RunID {
		t.Fatalf("succeeded: %d %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Cache-Control") != "no-store" || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		t.Errorf("headers = %v", rec.Header())
	}

	rec = e.do(t, http.MethodPost, "/v1/code/run", e.tokA, `{"code":"fail()","reason":"`+codeTestReason+`","wait_seconds":10}`, "Content-Type", "application/json")
	if v := runView(t, rec); rec.Code != http.StatusOK || v.Status != codemode.StatusFailed || v.Error == nil || v.Error.Kind != codemode.ErrorToolError {
		t.Fatalf("failed: %d %s", rec.Code, rec.Body.String())
	}

	rec = e.run(t, e.tokA, "hold()")
	v = runView(t, rec)
	if rec.Code != http.StatusForbidden || v.Status != codemode.StatusPermissionRequired || v.Held == nil || v.Held.Tool != "demo.write" ||
		string(v.Held.Draft) != `{"title":"Write x"}` || v.Held.ApprovalID != nil || v.Links.Resume != "/v1/code/runs/"+v.RunID+"/resume" {
		t.Fatalf("permission_required: %d %s", rec.Code, rec.Body.String())
	}
	heldID := v.RunID

	rec = e.run(t, e.tokA, "defer()")
	v = runView(t, rec)
	if rec.Code != http.StatusAccepted || v.Status != codemode.StatusDeferred || rec.Header().Get("Location") != "/v1/code/runs/"+v.RunID ||
		v.Held == nil || v.Held.ApprovalID == nil || *v.Held.ApprovalID != "ap_1" {
		t.Fatalf("deferred: %d %s %v", rec.Code, rec.Body.String(), rec.Header())
	}
	deferredID := v.RunID

	rec = e.run(t, e.tokA, "unknown()")
	if v := runView(t, rec); rec.Code != http.StatusConflict || v.Status != codemode.StatusOutcomeUnknown || v.Error == nil || v.Error.Kind != codemode.ErrorOutcomeUnknown {
		t.Fatalf("outcome_unknown: %d %s", rec.Code, rec.Body.String())
	}

	// The wait ends first: 202 with Location; the run carries on.
	rec = e.run(t, e.tokA, "slow()", "Prefer", "wait=0")
	v = runView(t, rec)
	if rec.Code != http.StatusAccepted || !coderuns.InProgress(v.Status) || rec.Header().Get("Location") != "/v1/code/runs/"+v.RunID {
		t.Fatalf("still running: %d %s", rec.Code, rec.Body.String())
	}
	slowID := v.RunID
	if rec := e.do(t, http.MethodGet, "/v1/code/runs/"+slowID, e.tokA, ""); rec.Code != http.StatusAccepted {
		t.Fatalf("GET while running: %d %s", rec.Code, rec.Body.String())
	}
	e.runner.releaseSlow()
	rec = e.do(t, http.MethodGet, "/v1/code/runs/"+slowID, e.tokA, "", "Prefer", "respond-async, wait=10")
	if v := runView(t, rec); rec.Code != http.StatusOK || v.Status != codemode.StatusSucceeded {
		t.Fatalf("GET with wait: %d %s", rec.Code, rec.Body.String())
	}

	// GET answers with the same mapping; another agent's run is 404.
	if rec := e.do(t, http.MethodGet, "/v1/code/runs/"+heldID, e.tokA, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("GET held: %d", rec.Code)
	}
	if rec := e.do(t, http.MethodGet, "/v1/code/runs/"+heldID, e.tokB, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("another agent's run: %d", rec.Code)
	}
	if rec := e.do(t, http.MethodGet, "/v1/code/runs/cr_nope", e.tokA, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown run: %d", rec.Code)
	}

	// Resume: a request_id is needed for permission_required; an approved
	// request's grant reaches the held call.
	resume := func(id, body string) *httptest.ResponseRecorder {
		if body == "" {
			return e.do(t, http.MethodPost, "/v1/code/runs/"+id+"/resume", e.tokA, "")
		}
		return e.do(t, http.MethodPost, "/v1/code/runs/"+id+"/resume", e.tokA, body, "Content-Type", "application/json")
	}
	if rec := resume(heldID, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("resume without request_id: %d %s", rec.Code, rec.Body.String())
	}
	if rec := resume(heldID, `{"request_id":"rq_other"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("resume with an unknown request: %d %s", rec.Code, rec.Body.String())
	}
	if rec := resume(heldID, `{"request_id":"rq_1","extra":1}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("resume with an unknown field: %d %s", rec.Code, rec.Body.String())
	}
	rec = resume(heldID, `{"request_id":"rq_1"}`)
	if v := runView(t, rec); rec.Code != http.StatusOK || v.Status != codemode.StatusSucceeded || string(v.Result) != `{"ok":true}` {
		t.Fatalf("resume: %d %s", rec.Code, rec.Body.String())
	}
	if rec := resume(heldID, `{"request_id":"rq_1"}`); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "run_not_held") {
		t.Fatalf("resume of a finished run: %d %s", rec.Code, rec.Body.String())
	}
	// A deferred run resumes with no body (its approval id); this runner
	// defers it again.
	if rec := resume(deferredID, "{}"); rec.Code != http.StatusAccepted {
		t.Fatalf("resume deferred: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do(t, http.MethodPost, "/v1/code/runs/"+deferredID+"/resume", e.tokA, "x", "Content-Type", "text/x-starlark"); rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("resume with a script body: %d", rec.Code)
	}
	if rec := resume("cr_nope", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("resume unknown: %d", rec.Code)
	}

	// Busy: 429 with Retry-After and no run. Draining: 503.
	before := e.runCount(t)
	e.runner.busy.Store(true)
	rec = e.run(t, e.tokA, "result = 1")
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("busy: %d %s", rec.Code, rec.Body.String())
	}
	e.runner.busy.Store(false)
	e.engine.draining.Store(true)
	if rec := e.run(t, e.tokA, "result = 1"); rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("draining: %d %s", rec.Code, rec.Body.String())
	}
	e.engine.draining.Store(false)
	if n := e.runCount(t); n != before {
		t.Fatalf("a refused start stored a run: %d, want %d", n, before)
	}
}

func TestCodeRunIdempotencyKey(t *testing.T) {
	e := newCodeEnv(t)
	first := e.run(t, e.tokA, "result = 1", "Idempotency-Key", "key-1")
	if first.Code != http.StatusOK {
		t.Fatalf("first: %d %s", first.Code, first.Body.String())
	}
	again := e.run(t, e.tokA, "result = 1", "Idempotency-Key", "key-1")
	if again.Code != http.StatusOK || runView(t, again).RunID != runView(t, first).RunID {
		t.Fatalf("replay: %d %s", again.Code, again.Body.String())
	}
	if n := e.runner.runs.Load(); n != 1 {
		t.Fatalf("the script ran %d times, want 1", n)
	}
	rec := e.run(t, e.tokA, "fail()", "Idempotency-Key", "key-1")
	if rec.Code != http.StatusUnprocessableEntity || decodeJSON(t, rec)["error"] != "idempotency_key_reused" {
		t.Fatalf("reused key: %d %s", rec.Code, rec.Body.String())
	}
	rec = e.run(t, e.tokA, "result = 1", "Idempotency-Key", "a", "Idempotency-Key", "b")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("two keys: %d", rec.Code)
	}
	// The start is audited with the reason and the run id, never the script.
	var summary, reason, args string
	if err := e.db.QueryRow(`SELECT COALESCE(result_summary,''), COALESCE(reason,''), COALESCE(arguments,'') FROM audit_events WHERE event_type = ? AND agent_id = ?`,
		eventCodeRun, e.agA.ID).Scan(&summary, &reason, &args); err != nil {
		t.Fatal(err)
	}
	if summary != runView(t, first).RunID || reason != codeTestReason || strings.Contains(args, "result = 1") {
		t.Fatalf("audit: summary %q reason %q args %q", summary, reason, args)
	}
}

func TestCodeRunBadInput(t *testing.T) {
	e := newCodeEnv(t)
	big := strings.Repeat("x", coderuns.MaxScriptBytes+1)
	bigJSON, _ := json.Marshal(map[string]any{"code": big, "reason": codeTestReason})
	reason := func(r string) []string { return []string{"Content-Type", "text/x-starlark", "X-Toolyard-Reason", r} }
	jsonCT := []string{"Content-Type", "application/json"}
	for name, c := range map[string]struct {
		body string
		hdr  []string
		want int
	}{
		"no content type":  {"result = 1", nil, http.StatusUnsupportedMediaType},
		"text/plain":       {"result = 1", []string{"Content-Type", "text/plain"}, http.StatusUnsupportedMediaType},
		"no reason":        {"result = 1", []string{"Content-Type", "text/x-starlark"}, http.StatusBadRequest},
		"short reason":     {"result = 1", reason("too short"), http.StatusBadRequest},
		"empty script":     {"  ", reason(codeTestReason), http.StatusBadRequest},
		"script too large": {big, reason(codeTestReason), http.StatusRequestEntityTooLarge},
		"json too large":   {string(bigJSON), jsonCT, http.StatusRequestEntityTooLarge},
		"json no code":     {`{"reason":"` + codeTestReason + `"}`, jsonCT, http.StatusBadRequest},
		"json bad wait":    {`{"code":"result = 1","reason":"` + codeTestReason + `","wait_seconds":61}`, jsonCT, http.StatusBadRequest},
		"json extra field": {`{"code":"result = 1","reason":"` + codeTestReason + `","x":1}`, jsonCT, http.StatusBadRequest},
		"json not json":    {`{"code":`, jsonCT, http.StatusBadRequest},
		"bad key":          {"result = 1", append(reason(codeTestReason), "Idempotency-Key", strings.Repeat("k", 129)), http.StatusBadRequest},
	} {
		rec := e.do(t, http.MethodPost, "/v1/code/run", e.tokA, c.body, c.hdr...)
		if rec.Code != c.want {
			t.Errorf("%s: %d %s, want %d", name, rec.Code, rec.Body.String(), c.want)
		}
	}
	if rec := e.do(t, http.MethodGet, "/v1/code/run", e.tokA, ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /v1/code/run: %d", rec.Code)
	}
	if n := e.runCount(t); n != 0 {
		t.Fatalf("bad input stored %d runs", n)
	}
}

func TestCodeRoutesAuth(t *testing.T) {
	e := newCodeEnv(t)
	ctx := context.Background()
	sessTok, _, err := e.id.MintSessionToken(ctx, e.agA.ID, e.tokA, "t3-thread-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	type route struct{ method, path, body string }
	codeRoutes := []route{
		{http.MethodPost, "/v1/code/run", "result = 1"},
		{http.MethodGet, "/v1/code/runs/cr_x", ""},
		{http.MethodPost, "/v1/code/runs/cr_x/resume", ""},
		{http.MethodGet, "/v1/code/files", ""},
		{http.MethodGet, "/v1/code/files/toolyard/memory_get.pyi", ""},
		{http.MethodGet, "/v1/code/docs/toolyard/memory_get", ""},
		{http.MethodGet, "/v1/code/openapi.json", ""},
		{http.MethodGet, "/v1/code/ty", ""},
	}
	send := func(r route, token string) *httptest.ResponseRecorder {
		if r.body != "" {
			return e.run(t, token, r.body)
		}
		return e.do(t, r.method, r.path, token, "")
	}

	// An operator token is refused everywhere under /v1/code/ and on the
	// session token routes, whatever its scopes.
	for _, r := range append(codeRoutes, route{http.MethodDelete, "/v1/agents/session-tokens/st_x", ""}) {
		if rec := send(r, e.opTok); rec.Code != http.StatusForbidden {
			t.Errorf("operator %s %s: %d %s", r.method, r.path, rec.Code, rec.Body.String())
		}
	}
	if rec := e.do(t, http.MethodPost, "/v1/agents/session-tokens", e.opTok, `{"session_id":"s"}`, "Content-Type", "application/json"); rec.Code != http.StatusForbidden {
		t.Errorf("operator mint: %d", rec.Code)
	}

	// No token: 401, except the two discovery documents.
	for _, r := range codeRoutes {
		rec := send(r, "")
		switch r.path {
		case "/v1/code/openapi.json", "/v1/code/ty":
			if rec.Code != http.StatusOK {
				t.Errorf("anonymous %s: %d", r.path, rec.Code)
			}
		default:
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("anonymous %s %s: %d %s", r.method, r.path, rec.Code, rec.Body.String())
			}
		}
	}
	if rec := e.run(t, "tys_st_nope.secret", "result = 1"); rec.Code != http.StatusUnauthorized {
		t.Errorf("bad session token: %d", rec.Code)
	}

	// A session token runs code as its agent, with its session in the audit.
	rec := e.run(t, sessTok, "result = 1")
	if rec.Code != http.StatusOK {
		t.Fatalf("session token run: %d %s", rec.Code, rec.Body.String())
	}
	runID := runView(t, rec).RunID
	var agentID, sessionID string
	if err := e.db.QueryRow(`SELECT agent_id, COALESCE(session_id,'') FROM code_runs WHERE id = ?`, runID).Scan(&agentID, &sessionID); err != nil {
		t.Fatal(err)
	}
	if agentID != e.agA.ID || sessionID != "t3-thread-1" {
		t.Fatalf("run row: agent %q session %q", agentID, sessionID)
	}
	var auditSession string
	var claimed int
	if err := e.db.QueryRow(`SELECT COALESCE(client_session_id,''), client_session_claimed FROM audit_events WHERE event_type = ? AND result_summary = ?`,
		eventCodeRun, runID).Scan(&auditSession, &claimed); err != nil {
		t.Fatal(err)
	}
	if auditSession != "t3-thread-1" || claimed != 0 {
		t.Fatalf("audit session %q claimed %d", auditSession, claimed)
	}
	// The agent token sees the run the session made, and the other way round.
	if rec := e.do(t, http.MethodGet, "/v1/code/runs/"+runID, e.tokA, ""); rec.Code != http.StatusOK {
		t.Fatalf("agent token reads the session's run: %d", rec.Code)
	}
	if rec := e.do(t, http.MethodGet, "/v1/code/files", sessTok, ""); rec.Code != http.StatusOK {
		t.Fatalf("session token stubs: %d", rec.Code)
	}
	rec = e.do(t, http.MethodGet, "/v1/agents/whoami", sessTok, "")
	if rec.Code != http.StatusOK || decodeJSON(t, rec)["session_id"] != "t3-thread-1" {
		t.Fatalf("whoami with a session token: %d %s", rec.Code, rec.Body.String())
	}
	// ... and nothing else.
	for _, r := range []route{
		{http.MethodGet, "/v1/agents/tools", ""},
		{http.MethodPost, "/v1/agents/tools/run", ""},
		{http.MethodPost, "/v1/hooks/ingest", ""},
		{http.MethodGet, "/v1/servers", ""},
		{http.MethodGet, "/v1/code/../servers", ""},
	} {
		if rec := e.do(t, r.method, r.path, sessTok, ""); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "session tokens") {
			t.Errorf("session token on %s %s: %d %s", r.method, r.path, rec.Code, rec.Body.String())
		}
	}
}

func TestCodeStubsAndDiscovery(t *testing.T) {
	e := newCodeEnv(t)
	for _, c := range []struct {
		path string
		code int
		want string
	}{
		{"/v1/code/files", http.StatusOK, "servers/toolyard/memory_get.pyi"},
		{"/v1/code/files/toolyard/memory_get.pyi", http.StatusOK, "def memory_get("},
		{"/v1/code/docs/toolyard/memory_get", http.StatusOK, "memory_get"},
		{"/v1/code/files/toolyard/memory_get", http.StatusNotFound, ""},
		{"/v1/code/files/nope/tool.pyi", http.StatusNotFound, ""},
		{"/v1/code/docs/nope/tool", http.StatusNotFound, ""},
		{"/v1/code/files/a/b/c.pyi", http.StatusNotFound, ""},
	} {
		rec := e.do(t, http.MethodGet, c.path, e.tokA, "")
		if rec.Code != c.code || !strings.Contains(rec.Body.String(), c.want) {
			t.Errorf("%s: %d %.300s", c.path, rec.Code, rec.Body.String())
			continue
		}
		if c.code == http.StatusOK && !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") {
			t.Errorf("%s: Content-Type %q", c.path, rec.Header().Get("Content-Type"))
		}
	}
	rec := e.do(t, http.MethodGet, "/v1/code/openapi.json", "", "")
	var doc struct {
		OpenAPI string `json:"openapi"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &doc) != nil || !strings.HasPrefix(doc.OpenAPI, "3.1") {
		t.Fatalf("openapi.json: %d %.200s", rec.Code, rec.Body.String())
	}
	rec = e.do(t, http.MethodGet, "/v1/code/ty", "", "")
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Body.String(), "#!/usr/bin/env python3\n") ||
		!strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("ty: %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if rec := e.do(t, http.MethodPost, "/v1/code/ty", "", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST ty: %d", rec.Code)
	}
}

func TestPreferWait(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"wait=0": 0, "wait=25": 25 * time.Second, "respond-async, wait=5": 5 * time.Second,
		"wait=600": 60 * time.Second, `wait="7"`: 7 * time.Second, "WAIT=3": 3 * time.Second,
	} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("Prefer", in)
		if got, ok := preferWait(r); !ok || got != want {
			t.Errorf("Prefer %q = %v %v, want %v", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "respond-async", "wait=-1", "wait=soon"} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("Prefer", in)
		if _, ok := preferWait(r); ok {
			t.Errorf("Prefer %q should be ignored", in)
		}
	}
}

func TestCodeMiddlewareEntries(t *testing.T) {
	if !exemptFromCSRFHeader("/v1/code/run") || !exemptFromOriginCheck("/v1/code/runs/cr_1/resume") ||
		!exemptFromCSRFHeader(sessionTokensPath) || !exemptFromCSRFHeader(sessionTokensPrefix+"st_1") {
		t.Error("code and session token routes must skip the CSRF header and Origin checks")
	}
	if perRouteBodyCap("/v1/code/run") < coderuns.MaxScriptBytes || perRouteBodyCap("/v1/code/runs/cr_1/resume") > 16<<10 {
		t.Errorf("body caps: run %d, resume %d", perRouteBodyCap("/v1/code/run"), perRouteBodyCap("/v1/code/runs/cr_1/resume"))
	}
	if unauthRouteLimit("/v1/code/openapi.json") == nil || unauthRouteLimit("/v1/code/ty") == nil {
		t.Error("the public discovery routes need the unauthenticated rate limit")
	}
	for _, p := range []string{"/v1/code/run", "/v1/code/files", "/v1/agents/session-tokens", "/v1/agents/session-tokens/st_1"} {
		if _, ok := operatorScopeFor(http.MethodGet, p); ok {
			t.Errorf("operator tokens must not reach %s", p)
		}
	}
	for path, want := range map[string]bool{
		"/v1/code/run": true, "/v1/code/runs/cr_1": true, "/v1/agents/whoami": true, "/v1/agents/approvals/ap_1": true,
		"/v1/agents/session-tokens": true, "/v1/agents/session-tokens/st_1": true,
		"/v1/agents/tools": false, "/v1/servers": false, "/v1/code/../servers": false, "/v1/agents": false,
	} {
		if got := sessionTokenAllowed(path); got != want {
			t.Errorf("sessionTokenAllowed(%s) = %v, want %v", path, got, want)
		}
	}
}
