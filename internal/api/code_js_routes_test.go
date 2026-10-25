package api

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tusharbhardwaj/toolyard/internal/codemode"
	"github.com/tusharbhardwaj/toolyard/internal/coderuns"
)

// jsCaller is the gateway for the JavaScript route test: demo.read
// answers JSON text; demo.write needs the grant tok-w (approvedGrants'
// rq_1) and answers permission_required without it.
type jsCaller struct{}

func (jsCaller) Tools(context.Context) []codemode.Tool {
	return []codemode.Tool{
		{Server: "demo", Name: "read", Target: "demo.read", Description: "Read a value.", ReasonField: "_reason",
			Properties: map[string]any{"q": map[string]any{"type": "string"}}, Required: []string{"q"}},
		{Server: "demo", Name: "write", Target: "demo.write", Description: "Write a value.", ReasonField: "_reason",
			Properties: map[string]any{"x": map[string]any{"type": "integer"}}, Required: []string{"x"}},
	}
}

func (jsCaller) RouteCall(_ context.Context, _, target string, args map[string]any) (*mcp.CallToolResult, error) {
	switch target {
	case "demo.read":
		b, _ := json.Marshal(map[string]any{"q": args["q"]})
		return mcp.NewToolResultText(string(b)), nil
	case "demo.write":
		if args["_grant"] == "tok-w" {
			return mcp.NewToolResultText(`{"ok": true}`), nil
		}
		res := mcp.NewToolResultError("demo.write is restricted. Nothing ran.")
		res.StructuredContent = map[string]any{"status": "permission_required", "draft": map[string]any{"title": "Write x"}}
		res.Meta = &mcp.Meta{AdditionalFields: map[string]any{"toolyard.permission_required": true}}
		return res, nil
	}
	return mcp.NewToolResultErrorf("tool %q not found", target), nil
}

// realEngine runs scripts on a real runtime whose worker is this binary.
type realEngine struct{ rt *codemode.Runtime }

func (e realEngine) Runner() coderuns.Runner { return e.rt }

func (e realEngine) RunContext(ctx context.Context, _, _ string) (context.Context, bool) {
	return ctx, true
}

func newJSCodeEnv(t *testing.T) *codeEnv {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	rt := codemode.New(jsCaller{}, codemode.Limits{ScriptTimeout: 60 * time.Second})
	rt.SetWorker(codemode.Worker{Path: exe, Env: []string{apiTestWorkerEnv + "=1"}})
	return newCodeEnvWith(t, realEngine{rt: rt})
}

func decodeView(t *testing.T, body string) coderuns.View {
	t.Helper()
	var v coderuns.View
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatalf("not a run view: %v\n%s", err, body)
	}
	return v
}

// TestCodeRunJavaScriptOverREST is the JavaScript acceptance script over
// POST /v1/code/run with text/javascript: two calls, a held call through
// tryCall the script carries on past, parallel with a limit, a returned
// value; then a run held on an uncaught call, resumed with the owner's
// grant.
func TestCodeRunJavaScriptOverREST(t *testing.T) {
	e := newJSCodeEnv(t)
	script := `const a = await tools.demo.read({ q: "one" });
const b = await tools.call("demo.read", { q: "two" });
const held = await tools.tryCall("demo.write", { x: 1 });
const all = await tools.parallel([1, 2, 3].map((i) => () => tools.demo.read({ q: "p" + i })), { limit: 2 });
console.log("held", held.status);
return { a: a.q, b: b.q, held: held.status, all: all.map((r) => r.q) };`
	start := time.Now()
	rec := e.do(t, http.MethodPost, "/v1/code/run", e.tokA, script,
		"Content-Type", "text/javascript", "X-Toolyard-Reason", codeTestReason, "Prefer", "wait=60")
	t.Logf("JavaScript over REST took %s", time.Since(start))
	if rec.Code != http.StatusOK {
		t.Fatalf("run: %d %s", rec.Code, rec.Body.String())
	}
	v := decodeView(t, rec.Body.String())
	var result map[string]any
	_ = json.Unmarshal(v.Result, &result)
	if v.Status != codemode.StatusSucceeded || v.Language != codemode.LanguageJavaScript || result["a"] != "one" || result["b"] != "two" ||
		result["held"] != codemode.StatusPermissionRequired || len(v.Logs) != 1 || v.Logs[0] != "held permission_required" {
		t.Fatalf("view = %+v (result %s)", v, v.Result)
	}
	if all, _ := result["all"].([]any); len(all) != 3 || all[0] != "p1" || all[2] != "p3" {
		t.Fatalf("parallel results %v", result["all"])
	}
	if len(v.Calls) != 6 || v.Calls[2].Tool != "demo.write" || v.Calls[2].Status != codemode.StatusPermissionRequired || v.Calls[0].FinishedAt == nil {
		t.Fatalf("calls %+v", v.Calls)
	}

	// JSON with a language.
	body, _ := json.Marshal(map[string]any{"code": "return 1 + 1;", "reason": codeTestReason, "language": "javascript", "wait_seconds": 60})
	rec = e.do(t, http.MethodPost, "/v1/code/run", e.tokA, string(body), "Content-Type", "application/json")
	if v := decodeView(t, rec.Body.String()); rec.Code != http.StatusOK || string(v.Result) != "2" || v.Language != codemode.LanguageJavaScript {
		t.Fatalf("JSON JavaScript run: %d %s", rec.Code, rec.Body.String())
	}
	body, _ = json.Marshal(map[string]any{"code": "x", "reason": codeTestReason, "language": "cobol"})
	if rec := e.do(t, http.MethodPost, "/v1/code/run", e.tokA, string(body), "Content-Type", "application/json"); rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "unknown language") {
		t.Fatalf("cobol: %d %s", rec.Code, rec.Body.String())
	}

	// Held on an uncaught call: 403 with the held call; resumed with the
	// grant, the read before it is replayed and the write runs.
	held := `const before = await tools.demo.read({ q: "before" });
const w = await tools.demo.write({ x: 1 });
return { before: before.q, wrote: w.ok };`
	rec = e.do(t, http.MethodPost, "/v1/code/run", e.tokA, held,
		"Content-Type", "text/javascript", "X-Toolyard-Reason", codeTestReason, "Prefer", "wait=60")
	v = decodeView(t, rec.Body.String())
	if rec.Code != http.StatusForbidden || v.Status != codemode.StatusPermissionRequired || v.Held == nil || v.Held.Tool != "demo.write" {
		t.Fatalf("held run: %d %s", rec.Code, rec.Body.String())
	}
	rec = e.do(t, http.MethodPost, "/v1/code/runs/"+v.RunID+"/resume", e.tokA, `{"request_id": "rq_1"}`,
		"Content-Type", "application/json", "Prefer", "wait=60")
	v = decodeView(t, rec.Body.String())
	if rec.Code != http.StatusOK || v.Status != codemode.StatusSucceeded || string(v.Result) != `{"before":"before","wrote":true}` ||
		len(v.Calls) != 2 || !v.Calls[0].Replayed {
		t.Fatalf("resumed: %d %s", rec.Code, rec.Body.String())
	}
}

func TestCodeSearchAndTypeScriptFilesOverREST(t *testing.T) {
	e := newCodeEnv(t)
	rec := e.do(t, http.MethodGet, "/v1/code/search?q=%5Ememory%5C.&mode=regex&limit=5", e.tokA, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("search: %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Total   int    `json:"total"`
		Mode    string `json:"mode"`
		Results []struct {
			Name string `json:"name"`
			Code string `json:"code"`
		} `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Mode != "regex" || body.Total == 0 || len(body.Results) > 5 {
		t.Fatalf("search body: %v %s", err, rec.Body.String())
	}
	for _, r := range body.Results {
		if !strings.Contains(r.Name, "memory") || !strings.HasPrefix(r.Code, "toolyard.memory_") {
			t.Fatalf("result %+v", r)
		}
	}
	for path, want := range map[string]int{
		"/v1/code/search?q=x&mode=fuzzy":    http.StatusBadRequest,
		"/v1/code/search?q=x&mode=semantic": http.StatusBadRequest,
		"/v1/code/search?q=x&limit=0":       http.StatusBadRequest,
		"/v1/code/search?q=x&limit=many":    http.StatusBadRequest,
	} {
		if rec := e.do(t, http.MethodGet, path, e.tokA, ""); rec.Code != want {
			t.Errorf("%s: %d %s", path, rec.Code, rec.Body.String())
		}
	}
	if rec := e.do(t, http.MethodGet, "/v1/code/search?q=x", "", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("search without a token: %d", rec.Code)
	}

	rec = e.do(t, http.MethodGet, "/v1/code/files?language=javascript", e.tokA, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "index.d.ts") || !strings.Contains(rec.Body.String(), "memory_get.d.ts") {
		t.Fatalf("JavaScript listing: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do(t, http.MethodGet, "/v1/code/files?language=perl", e.tokA, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("perl listing: %d", rec.Code)
	}
	for path, want := range map[string]string{
		"/v1/code/files/index.d.ts":               "declare global",
		"/v1/code/files/toolyard/memory_get.d.ts": "export interface Input",
		"/v1/code/files/toolyard.d.ts":            "export interface Tools {",
		"/v1/code/files/toolyard/memory_get.pyi":  "def memory_get(",
	} {
		rec := e.do(t, http.MethodGet, path, e.tokA, "")
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("%s: %d %s", path, rec.Code, rec.Body.String())
		}
	}
	if rec := e.do(t, http.MethodGet, "/v1/code/files/toolyard/nope.d.ts", e.tokA, ""); rec.Code != http.StatusNotFound {
		t.Errorf("missing stub: %d", rec.Code)
	}
}
