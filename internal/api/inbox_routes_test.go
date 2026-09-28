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

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tusharbhardwaj/toolyard/internal/approval"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/gateway"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/inbox"
	"github.com/tusharbhardwaj/toolyard/internal/memory"
	"github.com/tusharbhardwaj/toolyard/internal/passkey"
	"github.com/tusharbhardwaj/toolyard/internal/passkey/passkeytest"
	"github.com/tusharbhardwaj/toolyard/internal/policy"
	"github.com/tusharbhardwaj/toolyard/internal/realtime"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

type inboxAPIFixture struct {
	srv    *Server
	pushes *[]inbox.Push
	mux    *http.ServeMux
	cookie *http.Cookie
	svc    *inbox.Service
	token  string
	agent  string
}

func newInboxAPIFixture(t *testing.T) *inboxAPIFixture {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "api-inbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	id := identity.New(db)
	u, err := id.CreateUser(ctx, "tester", "long-enough-password")
	if err != nil {
		t.Fatal(err)
	}
	tok, ag, err := id.CreateAgentWithToken(ctx, u.ID, "claude-cloud-3")
	if err != nil {
		t.Fatal(err)
	}
	bus, _ := approval.New(ctx, db)
	auditLog := audit.New(db)
	gw := gateway.New(gateway.Options{Policy: policy.New(db), Approval: bus, Audit: auditLog, Memory: memory.New(db), Hub: realtime.NewHub()})
	gw.RegisterBuiltins()
	t.Cleanup(func() { _ = gw.Close() })
	pushes := &[]inbox.Push{}
	pks := passkey.New(db)
	svc, err := inbox.New(ctx, inbox.Options{DB: db, Catalog: gw, Passkeys: pks,
		Notify: func(_ context.Context, p inbox.Push) { *pushes = append(*pushes, p) }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Flush)
	guide := inbox.NewGuide(nil)
	gw.SetInbox(svc, guide, func() string { return gateway.ApprovalModeInbox })
	gw.RegisterInboxTools()
	snaps, _ := inbox.NewSnapshotter(db, filepath.Join(dir, "blobs"))

	srv := New(ctx, Options{Identity: id, Audit: auditLog, Approval: bus, Gateway: gw, Inbox: svc, Snapshots: snaps, Guide: guide, Passkeys: pks,
		SessionKey: []byte("0123456789abcdef0123456789abcdef")})
	mux := http.NewServeMux()
	srv.Routes(mux)

	loginBody, _ := json.Marshal(map[string]string{"Username": "tester", "Password": "long-enough-password"})
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/login", bytes.NewReader(loginBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatalf("login failed: %d %s", rec.Code, rec.Body.String())
	}
	return &inboxAPIFixture{srv: srv, pushes: pushes, mux: mux, cookie: cookie, svc: svc, token: tok, agent: ag.ID}
}

func (f *inboxAPIFixture) owner(t *testing.T, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rd)
	req.AddCookie(f.cookie)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://example.com") // httptest's Host is example.com
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// agentRun calls a tool as the agent through /v1/agents/tools/run, the
// path the CLI uses.
func (f *inboxAPIFixture) agentRun(t *testing.T, tool string, args map[string]any) map[string]any {
	t.Helper()
	args["_reason"] = "API test acting as an enrolled agent"
	b, _ := json.Marshal(map[string]any{"tool": tool, "arguments": args})
	req := httptest.NewRequest(http.MethodPost, "/v1/agents/tools/run", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+f.token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: %d %s", tool, rec.Code, rec.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	sc, _ := out["structured_content"].(map[string]any)
	return sc
}

func accessRequest(tool string) map[string]any {
	return map[string]any{
		"title":   "Delete the stale preview env",
		"summary": "One cleanup call.",
		"message": "The preview environment for PR 12 has been idle for a month, so I'd like to delete it.",
		"facts":   map[string]any{"why_now": "It costs money every day.", "if_it_goes_wrong": "Only the preview env.", "undo": "Recreate it from the PR."},
		"audio":   map[string]any{"script": "I'd like to delete the old preview environment. It has been idle for a month."},
		"urgency": "soon",
		"tools":   []any{map[string]any{"tool": tool, "required": true, "summary": "Delete the preview env.", "params": map[string]any{"key": "preview-12"}}},
	}
}

func TestInboxAPIFlow(t *testing.T) {
	f := newInboxAPIFixture(t)
	// memory.delete is a built-in write, so it's restricted.
	sc := f.agentRun(t, "inbox.request", accessRequest("memory.delete"))
	id, _ := sc["request_id"].(string)
	if sc["ok"] != true || id == "" {
		t.Fatalf("request via REST: %v", sc)
	}
	f.svc.Flush()

	// Unauthenticated owner routes are refused.
	req := httptest.NewRequest(http.MethodGet, "/v1/inbox", nil)
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous list: %d", rec.Code)
	}

	code, list := f.owner(t, "GET", "/v1/inbox", nil)
	reqs, _ := list["requests"].([]any)
	if code != 200 || len(reqs) != 1 || reqs[0].(map[string]any)["agent_name"] != "claude-cloud-3" {
		t.Fatalf("list: %d %v", code, list)
	}
	code, one := f.owner(t, "GET", "/v1/inbox/"+id, nil)
	if code != 200 || one["request"].(map[string]any)["title"] != "Delete the stale preview env" {
		t.Fatalf("get: %d %v", code, one)
	}
	flags := one["request"].(map[string]any)["tools"].([]any)[0].(map[string]any)["flags"].([]any)
	if len(flags) == 0 || flags[0].(map[string]any)["label"] != inbox.FlagDeletes {
		t.Fatalf("delete tool should carry the Deletes data flag: %v", flags)
	}

	if code, out := f.owner(t, "POST", "/v1/inbox/"+id+"/summarize", map[string]any{}); code != 200 || out["source"] != "rules" {
		t.Fatalf("summarize: %d %v", code, out)
	}
	if code, out := f.owner(t, "POST", "/v1/inbox/"+id+"/explain", map[string]any{"tool_index": 0}); code != 200 || !strings.Contains(out["text"].(string), "memory.delete") {
		t.Fatalf("explain: %d %v", code, out)
	}
	if code, _ := f.owner(t, "POST", "/v1/inbox/"+id+"/decide", map[string]any{"action": "approve", "allow": []bool{false}}); code != 400 {
		t.Fatalf("refusing a required tool on approve should be 400, got %d", code)
	}
	if code, out := f.owner(t, "POST", "/v1/inbox/"+id+"/decide", map[string]any{"action": "approve", "allow": []bool{true}, "note": "go ahead"}); code != 200 ||
		out["request"].(map[string]any)["status"] != inbox.StatusApproved {
		t.Fatalf("approve: %d %v", code, out)
	}
	if code, _ := f.owner(t, "POST", "/v1/inbox/"+id+"/decide", map[string]any{"action": "deny"}); code != 409 {
		t.Fatalf("second decision should be 409, got %d", code)
	}

	// The agent collects its grant and uses it through the same REST path.
	st := f.agentRun(t, "inbox.status", map[string]any{"ids": []any{id}})
	tool := st["requests"].([]any)[0].(map[string]any)["tools"].([]any)[0].(map[string]any)
	tok, _ := tool["grant"].(string)
	if tok == "" {
		t.Fatalf("no grant in status: %v", st)
	}
	if res := f.agentRun(t, "memory.delete", map[string]any{"key": "preview-12", "_grant": tok}); res != nil && res["status"] == "grant_invalid" {
		t.Fatalf("grant rejected: %v", res)
	}

	code, sessions := f.owner(t, "GET", "/v1/inbox/sessions", nil)
	if code != 200 || sessions["grants"] == nil {
		t.Fatalf("sessions: %d %v", code, sessions)
	}
	if code, out := f.owner(t, "POST", "/v1/inbox/grants/revoke-all", nil); code != 200 {
		t.Fatalf("revoke-all: %d %v", code, out)
	}
	if code, _ := f.owner(t, "GET", "/v1/inbox/rq_nope", nil); code != 404 {
		t.Fatalf("missing request should 404, got %d", code)
	}
	// The router cleans ".." with a redirect; an encoded form reaches our
	// handler, which only accepts a sha256 name. Neither may serve a file.
	if code, _ := f.owner(t, "GET", "/v1/inbox/blobs/../../etc/passwd", nil); code == 200 {
		t.Fatal("blob traversal served a file")
	}
	if code, _ := f.owner(t, "GET", "/v1/inbox/blobs/..%2F..%2Fetc%2Fpasswd", nil); code != 404 {
		t.Fatalf("encoded blob traversal should 404, got %d", code)
	}
}

func TestCoachingViaREST(t *testing.T) {
	f := newInboxAPIFixture(t)
	sc := f.agentRun(t, "memory.delete", map[string]any{"key": "x"})
	if sc["status"] != "permission_required" {
		t.Fatalf("want permission_required, got %v", sc)
	}
}

func TestGuideEndpoints(t *testing.T) {
	f := newInboxAPIFixture(t)
	get := func(path, bearer string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		rec := httptest.NewRecorder()
		f.mux.ServeHTTP(rec, req)
		return rec
	}
	if rec := get("/v1/guide", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous guide: %d", rec.Code)
	}
	if rec := get("/v1/guide", "bogus"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad token guide: %d", rec.Code)
	}
	if rec := get("/v1/guide?topic=grants", f.token); rec.Code != 200 || !strings.Contains(rec.Body.String(), "_grant") {
		t.Fatalf("guide grants: %d %s", rec.Code, rec.Body.String())
	}
	if rec := get("/v1/guide?topic=nope", f.token); rec.Code != 404 {
		t.Fatalf("unknown topic: %d", rec.Code)
	}
	if rec := get("/v1/guide/skill", f.token); rec.Code != 200 || !strings.HasPrefix(rec.Body.String(), "---\nname: toolyard-inbox") {
		t.Fatalf("skill: %d %.40s", rec.Code, rec.Body.String())
	}
}

var _ = mcp.NewToolResultText // keep the mcp import for readers of this file's helpers

// A notification tap works without a cookie or the CSRF header, only for
// the actions the notification offered, and never approves.
func TestInboxDecideByToken(t *testing.T) {
	f := newInboxAPIFixture(t)
	ctx := context.Background()
	res, err := f.svc.Submit(ctx, f.agent, &inbox.Submission{Kind: inbox.KindQuestion, Title: "Retire v1?", Summary: "s", Message: "m",
		Audio: inbox.Audio{Script: "Retire it?"}, Urgency: inbox.UrgencyNow, Options: []inbox.Option{{Label: "Yes"}, {Label: "No"}}})
	if err != nil || !res.OK {
		t.Fatalf("submit: %v %+v", err, res)
	}
	if len(*f.pushes) != 1 || (*f.pushes)[0].TapToken == "" {
		t.Fatalf("push: %+v", *f.pushes)
	}
	tok := (*f.pushes)[0].TapToken
	h := f.srv.HardenAPI(f.mux)
	tap := func(action string) int {
		b, _ := json.Marshal(map[string]string{"token": tok, "action": action})
		req := httptest.NewRequest(http.MethodPost, "/v1/inbox/decide-by-token", bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if c := tap("deny"); c != http.StatusGone {
		t.Fatalf("an action the notification didn't offer: %d", c)
	}
	if c := tap("snooze"); c != http.StatusOK {
		t.Fatalf("snooze: %d", c)
	}
	if c := tap("snooze"); c != http.StatusOK {
		t.Fatalf("snooze again: %d", c)
	}
	r, _ := f.svc.Get(ctx, res.RequestID)
	if r.Status != inbox.StatusPending || r.SnoozedUntil == 0 {
		t.Fatalf("after snooze: %+v", r)
	}
}

// The passkey flow over HTTP: register, high-risk approve refused without
// it, confirm, approve; removal needs a passkey too.
func TestPasskeyAPIFlow(t *testing.T) {
	f := newInboxAPIFixture(t)
	ctx := context.Background()
	auth := passkeytest.New("https://example.com")

	code, out := f.owner(t, http.MethodPost, "/v1/passkeys/register/begin", map[string]any{})
	if code != http.StatusOK {
		t.Fatalf("register begin: %d %v", code, out)
	}
	var creation protocol.CredentialCreation
	remarshal(t, out["options"], &creation)
	cred, err := auth.Create(&creation)
	if err != nil {
		t.Fatal(err)
	}
	code, out = f.owner(t, http.MethodPost, "/v1/passkeys/register/finish",
		map[string]any{"session_id": out["session_id"], "name": "iPhone", "credential": json.RawMessage(cred)})
	if code != http.StatusOK {
		t.Fatalf("register finish: %d %v", code, out)
	}
	pkID := out["passkey"].(map[string]any)["id"].(string)
	if code, out = f.owner(t, http.MethodGet, "/v1/passkeys", nil); code != http.StatusOK || len(out["passkeys"].([]any)) != 1 {
		t.Fatalf("list: %d %v", code, out)
	}

	res, err := f.svc.Submit(ctx, f.agent, &inbox.Submission{Kind: inbox.KindAccess, Title: "Deploy api", Summary: "s",
		Message: "I'd like to deploy the api to production.", Audio: inbox.Audio{Script: "Deploy?"}, Urgency: inbox.UrgencySoon,
		Facts: &inbox.Facts{WhyNow: "w", IfItGoesWrong: "g", Undo: "u"},
		Tools: []inbox.SubmissionTool{{Tool: "memory.delete", Required: true, Summary: "Delete the prod key.", Params: map[string]any{"key": "prod-cache"}}}})
	if err != nil || !res.OK {
		t.Fatalf("submit: %v %+v", err, res)
	}
	f.svc.Flush()
	r, _ := f.svc.Get(ctx, res.RequestID)
	if !inbox.HighRisk(r) {
		t.Fatalf("expected a high-risk request, flags: %+v", r.AllFlags())
	}
	decision := map[string]any{"action": "approve", "allow": []bool{true}}
	code, out = f.owner(t, http.MethodPost, "/v1/inbox/"+r.ID+"/decide", decision)
	if code != http.StatusPreconditionRequired || out["code"] != "passkey_required" {
		t.Fatalf("approve without passkey: %d %v", code, out)
	}
	code, out = f.owner(t, http.MethodPost, "/v1/inbox/"+r.ID+"/passkey", map[string]any{"decision": decision})
	if code != http.StatusOK {
		t.Fatalf("passkey begin: %d %v", code, out)
	}
	var assertion protocol.CredentialAssertion
	remarshal(t, out["options"], &assertion)
	resp, err := auth.Get(&assertion, f.ownerID(t))
	if err != nil {
		t.Fatal(err)
	}
	decision["passkey"] = map[string]any{"session_id": out["session_id"], "response": json.RawMessage(resp)}
	code, out = f.owner(t, http.MethodPost, "/v1/inbox/"+r.ID+"/decide", decision)
	if code != http.StatusOK {
		t.Fatalf("approve with passkey: %d %v", code, out)
	}

	// Removing needs a passkey confirmation for that removal.
	if code, _ = f.owner(t, http.MethodPost, "/v1/passkeys/"+pkID+"/remove", map[string]any{"session_id": "nope", "response": json.RawMessage(`{}`)}); code != http.StatusForbidden {
		t.Fatalf("remove without confirmation: %d", code)
	}
	code, out = f.owner(t, http.MethodPost, "/v1/passkeys/"+pkID+"/remove/begin", map[string]any{})
	if code != http.StatusOK {
		t.Fatalf("remove begin: %d %v", code, out)
	}
	remarshal(t, out["options"], &assertion)
	resp, _ = auth.Get(&assertion, f.ownerID(t))
	if code, out = f.owner(t, http.MethodPost, "/v1/passkeys/"+pkID+"/remove", map[string]any{"session_id": out["session_id"], "response": json.RawMessage(resp)}); code != http.StatusOK {
		t.Fatalf("remove: %d %v", code, out)
	}
}

func remarshal(t *testing.T, in, out any) {
	t.Helper()
	b, _ := json.Marshal(in)
	if err := json.Unmarshal(b, out); err != nil {
		t.Fatal(err)
	}
}

func (f *inboxAPIFixture) ownerID(t *testing.T) string {
	t.Helper()
	uid, err := f.srv.requireUser(func() *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.AddCookie(f.cookie)
		return r
	}())
	if err != nil {
		t.Fatal(err)
	}
	return uid
}

func TestInboxBatchAndInfo(t *testing.T) {
	f := newInboxAPIFixture(t)
	ctx := context.Background()
	var ids []string
	for i := 0; i < 3; i++ {
		res, err := f.svc.Submit(ctx, f.agent, &inbox.Submission{Kind: inbox.KindUpdate, Title: "Done", Summary: "s", Message: "m",
			Audio: inbox.Audio{Script: "Done."}, Urgency: inbox.UrgencyFYI})
		if err != nil || !res.OK {
			t.Fatal(err, res)
		}
		ids = append(ids, res.RequestID)
	}
	if code, out := f.owner(t, http.MethodPost, "/v1/inbox/batch", map[string]any{"ids": ids, "action": "approve"}); code != http.StatusBadRequest {
		t.Fatalf("batch approve must be refused: %d %v", code, out)
	}
	code, out := f.owner(t, http.MethodPost, "/v1/inbox/batch", map[string]any{"ids": append(ids, "rq_missing"), "action": "read"})
	if code != http.StatusOK || out["done"].(float64) != 3 || len(out["failed"].(map[string]any)) != 1 {
		t.Fatalf("batch read: %d %v", code, out)
	}
	code, out = f.owner(t, http.MethodGet, "/v1/inbox/info", nil)
	if code != http.StatusOK || out["info"].(map[string]any)["next_digest"] == nil {
		t.Fatalf("info: %d %v", code, out)
	}
}
