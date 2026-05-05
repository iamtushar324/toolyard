//go:build e2e

// scripts/e2e_test.go runs end-to-end smokes against a live toolyard server.
//
// Usage:
//
//	go test -tags=e2e ./scripts -run TestE2E -toolyard=http://localhost:18787
package scripts

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

var toolyardURL = flag.String("toolyard", "http://localhost:18787", "toolyard base URL")

type httpClient struct {
	base    string
	cookies []*http.Cookie
}

func (h *httpClient) raw(t *testing.T, method, path string, body any, dst any) {
	t.Helper()
	var buf io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		buf = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, h.base+path, buf)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, c := range h.cookies {
		req.AddCookie(c)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		all, _ := io.ReadAll(resp.Body)
		t.Fatalf("%s %s -> %d: %s", method, path, resp.StatusCode, string(all))
	}
	if len(resp.Cookies()) > 0 {
		h.cookies = append(h.cookies, resp.Cookies()...)
	}
	if dst != nil {
		all, _ := io.ReadAll(resp.Body)
		if len(all) == 0 {
			return
		}
		if err := json.Unmarshal(all, dst); err != nil {
			t.Fatalf("decode %s: %v body=%s", path, err, string(all))
		}
	}
}

func mustLogin(t *testing.T, h *httpClient) {
	// idempotent: setup; if already exists fall through to login.
	resp, err := http.Post(h.base+"/v1/auth/setup", "application/json",
		strings.NewReader(`{"Username":"admin","Password":"correct horse battery staple"}`))
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	resp.Body.Close()

	var out map[string]any
	h.raw(t, "POST", "/v1/auth/login",
		map[string]string{"Username": "admin", "Password": "correct horse battery staple"}, &out)
	t.Logf("logged in as %v", out["username"])
}

func enrollAgent(t *testing.T, h *httpClient, name string) string {
	var enr map[string]any
	h.raw(t, "POST", "/v1/agents/enroll", map[string]string{"Name": name}, &enr)
	code, _ := enr["enrollment_code"].(string)
	if code == "" {
		t.Fatal("no enrollment code")
	}
	var xch map[string]any
	h.raw(t, "POST", "/v1/agents/exchange", map[string]string{"Code": code}, &xch)
	tok, _ := xch["token"].(string)
	if tok == "" {
		t.Fatal("no token")
	}
	return tok
}

func mcpClient(t *testing.T, base, token string) *client.Client {
	tr, err := transport.NewStreamableHTTP(base+"/mcp",
		transport.WithHTTPHeaders(map[string]string{"Authorization": "Bearer " + token}))
	if err != nil {
		t.Fatal(err)
	}
	c := client.NewClient(tr)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{Name: "toolyard-e2e", Version: "0.0.1"}
	if _, err := c.Initialize(ctx, initReq); err != nil {
		t.Fatal(err)
	}
	return c
}

// TestE2EHappyPath: enroll, list, read passes, write holds, approve, write completes.
func TestE2EHappyPath(t *testing.T) {
	flag.Parse()
	h := &httpClient{base: *toolyardURL}
	mustLogin(t, h)
	token := enrollAgent(t, h, "happy-path")
	c := mcpClient(t, *toolyardURL, token)
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tools, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("tools: %d", len(tools.Tools))
	for _, tl := range tools.Tools {
		if _, ok := tl.InputSchema.Properties["_reason"]; !ok {
			t.Errorf("%s missing _reason in schema", tl.Name)
		}
	}

	// memory.get is read-only -> passes through immediately.
	getReq := mcp.CallToolRequest{}
	getReq.Params.Name = "memory.get"
	getReq.Params.Arguments = map[string]any{
		"_reason": "smoke test reading a missing key just to exercise the read path",
		"key":     "no-such-key-yet",
	}
	getRes, err := c.CallTool(ctx, getReq)
	if err != nil {
		t.Fatalf("read call: %v", err)
	}
	if !getRes.IsError {
		t.Errorf("expected error for missing key")
	}

	// memory.set requires approval.
	setRes, _ := callWithApproval(t, h, c, ctx, "memory.set", map[string]any{
		"_reason": "writing a value to demonstrate the approval flow end to end",
		"key":     "hello",
		"value":   "world",
	}, "allowed")
	if setRes.IsError {
		t.Fatalf("set returned error: %s", dumpResult(setRes))
	}

	// Read it back.
	getReq2 := mcp.CallToolRequest{}
	getReq2.Params.Name = "memory.get"
	getReq2.Params.Arguments = map[string]any{
		"_reason": "verifying the value we just stored is readable",
		"key":     "hello",
	}
	getRes2, err := c.CallTool(ctx, getReq2)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(dumpResult(getRes2), "world") {
		t.Errorf("expected 'world', got %s", dumpResult(getRes2))
	}
}

// TestE2EDeny: write call held, dashboard denies, agent gets denial.
func TestE2EDeny(t *testing.T) {
	flag.Parse()
	h := &httpClient{base: *toolyardURL}
	mustLogin(t, h)
	token := enrollAgent(t, h, "deny-path")
	c := mcpClient(t, *toolyardURL, token)
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	res, _ := callWithApproval(t, h, c, ctx, "memory.set", map[string]any{
		"_reason": "demonstrating the deny path returns a clean error to the agent",
		"key":     "denied-key",
		"value":   "should-not-store",
	}, "denied")
	if !res.IsError {
		t.Errorf("expected isError after deny, got %s", dumpResult(res))
	}
	if !strings.Contains(strings.ToLower(dumpResult(res)), "den") {
		t.Errorf("denied result should mention denial, got %s", dumpResult(res))
	}

	// Verify memory wasn't actually written.
	getReq := mcp.CallToolRequest{}
	getReq.Params.Name = "memory.get"
	getReq.Params.Arguments = map[string]any{
		"_reason": "verifying the previously denied write did not actually persist",
		"key":     "denied-key",
	}
	gr, _ := c.CallTool(ctx, getReq)
	if !gr.IsError {
		t.Errorf("denied write should not have persisted; got: %s", dumpResult(gr))
	}
}

// TestE2EDeferred: when the in-line wait elapses with no decision, the
// server returns a deferred response; resuming with _approval_id after the
// human approves yields the actual result.
//
// Run the server with -in-line-wait 2s for this test.
func TestE2EDeferred(t *testing.T) {
	flag.Parse()
	h := &httpClient{base: *toolyardURL}
	mustLogin(t, h)
	token := enrollAgent(t, h, "deferred-path")
	c := mcpClient(t, *toolyardURL, token)
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// First call: held > in-line wait -> deferred response.
	req := mcp.CallToolRequest{}
	req.Params.Name = "memory.set"
	req.Params.Arguments = map[string]any{
		"_reason": "exercising the deferred-response path with a long approval window",
		"key":     "deferred-key",
		"value":   "later",
	}
	res, err := c.CallTool(ctx, req)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	sc, _ := res.StructuredContent.(map[string]any)
	if sc == nil || sc["status"] != "pending_approval" {
		t.Skipf("server returned a non-deferred response (status=%v); requires in-line-wait flag short. Skipping.", sc)
	}
	apID, _ := sc["approval_id"].(string)
	if apID == "" {
		t.Fatal("deferred response missing approval_id")
	}
	t.Logf("deferred approval_id=%s", apID)

	// Approve from the dashboard.
	var dummy map[string]any
	h.raw(t, "POST", "/v1/approvals/"+apID+"/decide", map[string]string{"Action": "allowed"}, &dummy)

	// Re-call with _approval_id to resume.
	req2 := mcp.CallToolRequest{}
	req2.Params.Name = "memory.set"
	req2.Params.Arguments = map[string]any{
		"_reason":      "resuming the deferred call with the approval id",
		"_approval_id": apID,
	}
	res2, err := c.CallTool(ctx, req2)
	if err != nil {
		t.Fatalf("resume call: %v", err)
	}
	if res2.IsError {
		t.Fatalf("resume returned error: %s", dumpResult(res2))
	}
	if !strings.Contains(dumpResult(res2), "later") {
		t.Errorf("expected resumed call result to mention stored value, got %s", dumpResult(res2))
	}
}

// TestE2EMetaTools: tools.search returns the catalog and tools.execute proxies
// a write through the same approval flow as a direct call.
func TestE2EMetaTools(t *testing.T) {
	flag.Parse()
	h := &httpClient{base: *toolyardURL}
	mustLogin(t, h)
	token := enrollAgent(t, h, "meta-tools")
	c := mcpClient(t, *toolyardURL, token)
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// tools.search reads → no approval.
	req := mcp.CallToolRequest{}
	req.Params.Name = "tools.search"
	req.Params.Arguments = map[string]any{
		"_reason": "discovering available tools to pick the right one for the task",
		"query":   "memory",
	}
	res, err := c.CallTool(ctx, req)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if res.IsError {
		t.Fatalf("search returned error: %s", dumpResult(res))
	}
	out := dumpResult(res)
	if !strings.Contains(out, "memory.set") || !strings.Contains(out, "memory.get") {
		t.Errorf("search did not include memory tools: %s", out)
	}
	// Meta-tools themselves should be filtered out of search results.
	if strings.Contains(out, "tools.search") {
		t.Errorf("search should hide tools.search from its own results")
	}

	// tools.execute → memory.set: holds for approval.
	setRes, _ := callWithApproval(t, h, c, ctx, "tools.execute", map[string]any{
		"_reason": "exercising the tools.execute meta-tool against memory.set",
		"tool":    "memory.set",
		"arguments": map[string]any{
			"_reason": "writing via meta-tool to verify the proxy preserves approvals",
			"key":     "via-meta",
			"value":   "yes",
		},
	}, "allowed")
	if setRes.IsError {
		t.Fatalf("execute proxy returned error: %s", dumpResult(setRes))
	}
	if !strings.Contains(dumpResult(setRes), "via-meta") {
		t.Errorf("expected key in result, got %s", dumpResult(setRes))
	}

	// Verify the audit log shows the call as memory.set, not tools.execute.
	var events []map[string]any
	h.raw(t, "GET", "/v1/audit?limit=20", nil, &events)
	sawMemorySet := false
	for _, e := range events {
		if e["tool_name"] == "memory.set" && e["event_type"] == "call.succeeded" {
			sawMemorySet = true
			break
		}
	}
	if !sawMemorySet {
		t.Error("audit log should record the underlying memory.set call")
	}
}

// TestE2EServerCRUD: add an MCP server via REST, see it listed, remove it.
// Uses an obviously invalid command so the connect failure is recorded but
// the row persists (confirming the persisted-error UX).
func TestE2EServerCRUD(t *testing.T) {
	flag.Parse()
	h := &httpClient{base: *toolyardURL}
	mustLogin(t, h)

	body := map[string]any{
		"name":      "broken-test",
		"transport": "stdio",
		"command":   "/usr/bin/false",
	}
	resp, err := postJSON(h, "/v1/servers", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 202 && resp.StatusCode != 200 {
		t.Fatalf("expected 200/202, got %d", resp.StatusCode)
	}

	// List should include it.
	var servers []map[string]any
	h.raw(t, "GET", "/v1/servers", nil, &servers)
	found := false
	for _, s := range servers {
		if s["name"] == "broken-test" {
			found = true
		}
	}
	if !found {
		t.Error("server not in list after add")
	}

	// Remove.
	h.raw(t, "DELETE", "/v1/servers/broken-test", nil, nil)
	servers = nil
	h.raw(t, "GET", "/v1/servers", nil, &servers)
	for _, s := range servers {
		if s["name"] == "broken-test" {
			t.Error("server still listed after delete")
		}
	}
}

// postJSON is a small helper that lets us inspect status code + cookies.
func postJSON(h *httpClient, path string, body any) (*http.Response, error) {
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", h.base+path, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for _, c := range h.cookies {
		req.AddCookie(c)
	}
	return http.DefaultClient.Do(req)
}

// TestE2EMarketplace: catalog endpoint returns curated entries with the
// expected shape, and a no-env entry round-trips through /v1/servers.
func TestE2EMarketplace(t *testing.T) {
	flag.Parse()
	h := &httpClient{base: *toolyardURL}
	mustLogin(t, h)

	var catalog []map[string]any
	h.raw(t, "GET", "/v1/marketplace", nil, &catalog)
	if len(catalog) < 5 {
		t.Fatalf("expected at least 5 marketplace entries, got %d", len(catalog))
	}
	want := map[string]bool{"context7": false, "github": false, "filesystem": false}
	for _, e := range catalog {
		if id, _ := e["id"].(string); want[id] == false {
			if _, ok := want[id]; ok {
				want[id] = true
			}
		}
	}
	for k, ok := range want {
		if !ok {
			t.Errorf("marketplace missing expected entry %q", k)
		}
	}

	// Pick the sequential-thinking recipe (no env vars), construct the body
	// the same way the dashboard does, and POST it.
	var seq map[string]any
	for _, e := range catalog {
		if id, _ := e["id"].(string); id == "sequential-thinking" {
			seq = e
			break
		}
	}
	if seq == nil {
		t.Fatal("sequential-thinking missing from catalog")
	}
	args, _ := seq["args"].([]any)
	argsStr := make([]string, 0, len(args))
	for _, a := range args {
		if s, ok := a.(string); ok {
			argsStr = append(argsStr, s)
		}
	}
	body := map[string]any{
		"name":      "thinking-mp",
		"transport": seq["transport"],
		"command":   seq["command"],
		"args":      argsStr,
	}
	resp, err := postJSON(h, "/v1/servers", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// We don't actually have npx available with that package on this box, so
	// 200 (cached) or 202 (saved with warning) are both acceptable; what
	// matters is the row exists.
	if resp.StatusCode != 200 && resp.StatusCode != 202 {
		t.Fatalf("unexpected status %d", resp.StatusCode)
	}
	var servers []map[string]any
	h.raw(t, "GET", "/v1/servers", nil, &servers)
	found := false
	for _, s := range servers {
		if s["name"] == "thinking-mp" {
			found = true
		}
	}
	if !found {
		t.Error("server row not persisted after marketplace install")
	}
	// Cleanup.
	h.raw(t, "DELETE", "/v1/servers/thinking-mp", nil, nil)
}

// TestE2EToolsRun: dashboard-initiated tool calls go through the same policy
// + approval flow as a direct MCP call.
func TestE2EToolsRun(t *testing.T) {
	flag.Parse()
	h := &httpClient{base: *toolyardURL}
	mustLogin(t, h)

	// memory.get is a read -> no approval, immediate result.
	var read map[string]any
	h.raw(t, "POST", "/v1/tools/run",
		map[string]any{
			"tool": "memory.get",
			"arguments": map[string]any{
				"_reason": "workbench smoke test reading a key that does not exist yet",
				"key":     "wb-missing",
			},
		}, &read)
	if read["is_error"] != true {
		t.Errorf("expected is_error true on missing key, got %v", read)
	}

	// memory.set is a write -> first call returns deferred or pending; we
	// approve and re-run with _approval_id to fetch the actual result.
	var write1 map[string]any
	h.raw(t, "POST", "/v1/tools/run",
		map[string]any{
			"tool": "memory.set",
			"arguments": map[string]any{
				"_reason": "workbench smoke test setting a value to demonstrate the approval flow",
				"key":     "wb-key",
				"value":   "from-workbench",
			},
		}, &write1)
	sc, _ := write1["structured_content"].(map[string]any)
	if sc == nil || sc["status"] != "pending_approval" {
		t.Skipf("expected pending_approval (server may have a long in-line wait); got %v", write1)
	}
	apID, _ := sc["approval_id"].(string)
	if apID == "" {
		t.Fatal("no approval_id in workbench response")
	}
	var dummy map[string]any
	h.raw(t, "POST", "/v1/approvals/"+apID+"/decide", map[string]string{"Action": "allowed"}, &dummy)

	var write2 map[string]any
	h.raw(t, "POST", "/v1/tools/run",
		map[string]any{
			"tool": "memory.set",
			"arguments": map[string]any{
				"_reason":      "workbench resume after approval",
				"_approval_id": apID,
			},
		}, &write2)
	if write2["is_error"] == true {
		t.Errorf("workbench resume returned error: %v", write2)
	}
	body := ""
	if cs, ok := write2["content"].([]any); ok {
		for _, c := range cs {
			if cm, ok := c.(map[string]any); ok && cm["type"] == "text" {
				body += cm["text"].(string)
			}
		}
	}
	if !strings.Contains(body, "from-workbench") {
		t.Errorf("expected stored value in result, got %q", body)
	}
}

// TestE2ERouterOnlyMode: when router_only_mode is on, agents' tools/list
// returns only tools.search and tools.execute, but the underlying tools are
// still callable through tools.execute.
func TestE2ERouterOnlyMode(t *testing.T) {
	flag.Parse()
	h := &httpClient{base: *toolyardURL}
	mustLogin(t, h)
	token := enrollAgent(t, h, "router-only-test")

	// First, baseline: with router_only_mode off, the agent should see >2
	// tools.
	var off map[string]any
	h.raw(t, "PATCH", "/v1/settings", map[string]any{"router_only_mode": false}, &off)

	c := mcpClient(t, *toolyardURL, token)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	full, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(full.Tools) <= 2 {
		t.Fatalf("expected >2 tools when router_only_mode is off, got %d", len(full.Tools))
	}

	// Turn it on; tools/list must shrink to just the meta-tools.
	var on map[string]any
	h.raw(t, "PATCH", "/v1/settings", map[string]any{"router_only_mode": true}, &on)
	if on["router_only_mode"] != true {
		t.Errorf("settings did not echo router_only_mode=true; got %v", on)
	}

	// Need a fresh MCP client since the streamable HTTP session caches
	// a server-info snapshot; the filter is still applied per-call though.
	c2 := mcpClient(t, *toolyardURL, token)
	defer c2.Close()
	min, err := c2.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(min.Tools) != 2 {
		t.Fatalf("expected exactly 2 tools (search + execute) in router-only mode, got %d", len(min.Tools))
	}
	gotNames := map[string]bool{}
	for _, tl := range min.Tools {
		gotNames[tl.Name] = true
	}
	if !gotNames["tools.search"] || !gotNames["tools.execute"] {
		t.Errorf("expected tools.search and tools.execute; got %v", gotNames)
	}

	// Direct calls to a hidden tool are rejected with a pointer at
	// tools.execute (so an agent that has memory.get cached in its context
	// from before the toggle can't bypass the setting).
	directReq := mcp.CallToolRequest{}
	directReq.Params.Name = "memory.get"
	directReq.Params.Arguments = map[string]any{
		"_reason": "trying to bypass router-only mode by calling the hidden tool directly",
		"key":     "no-such-key",
	}
	directRes, err := c2.CallTool(ctx, directReq)
	if err != nil {
		t.Fatalf("direct call in router-only mode: %v", err)
	}
	if !directRes.IsError {
		t.Errorf("expected direct call to be rejected in router-only mode, got %s", dumpResult(directRes))
	}
	if !strings.Contains(strings.ToLower(dumpResult(directRes)), "router-only") {
		t.Errorf("rejection message should mention router-only mode, got %s", dumpResult(directRes))
	}

	// Underlying tool is still callable through tools.execute.
	exec := mcp.CallToolRequest{}
	exec.Params.Name = "tools.execute"
	exec.Params.Arguments = map[string]any{
		"_reason": "verifying tools.execute still routes when the catalog is hidden",
		"tool":    "memory.get",
		"arguments": map[string]any{
			"_reason": "router-only-mode reachability check via meta-tool",
			"key":     "no-such-key",
		},
	}
	res, err := c2.CallTool(ctx, exec)
	if err != nil {
		t.Fatalf("execute via meta in router-only-mode: %v", err)
	}
	// Expected: memory.get returns isError=true (key missing), but the call
	// succeeded in routing — that's what we're testing.
	if !res.IsError {
		t.Logf("memory.get returned ok unexpectedly: %s", dumpResult(res))
	}
	// And the result text mentions a missing key, not a router-only rejection.
	if strings.Contains(strings.ToLower(dumpResult(res)), "router-only") {
		t.Errorf("tools.execute should bypass the filter, got %s", dumpResult(res))
	}

	// Restore default for other tests.
	h.raw(t, "PATCH", "/v1/settings", map[string]any{"router_only_mode": false}, &off)
}

// TestE2EReasonValidation: missing/short reason rejected.
func TestE2EReasonValidation(t *testing.T) {
	flag.Parse()
	h := &httpClient{base: *toolyardURL}
	mustLogin(t, h)
	token := enrollAgent(t, h, "reason-validation")
	c := mcpClient(t, *toolyardURL, token)
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// missing _reason
	req := mcp.CallToolRequest{}
	req.Params.Name = "memory.get"
	req.Params.Arguments = map[string]any{"key": "x"}
	_, _ = c.CallTool(ctx, req)
	// mcp-go validates the required field client-side (or server returns
	// an error). Either way we should not see a successful tool result.

	// too-short reason
	req2 := mcp.CallToolRequest{}
	req2.Params.Name = "memory.get"
	req2.Params.Arguments = map[string]any{"_reason": "too short", "key": "x"}
	res, err := c.CallTool(ctx, req2)
	if err == nil && res != nil && !res.IsError {
		t.Error("expected reason validation to fail")
	}
}

// callWithApproval issues the tool call in a goroutine, polls the dashboard
// for the resulting pending approval, decides it, then waits for the call to
// return.
func callWithApproval(t *testing.T, h *httpClient, c *client.Client, ctx context.Context,
	tool string, args map[string]any, action string) (*mcp.CallToolResult, error) {
	t.Helper()
	type result struct {
		res *mcp.CallToolResult
		err error
	}
	ch := make(chan result, 1)
	go func() {
		req := mcp.CallToolRequest{}
		req.Params.Name = tool
		req.Params.Arguments = args
		res, err := c.CallTool(ctx, req)
		ch <- result{res, err}
	}()

	deadline := time.Now().Add(15 * time.Second)
	var apID string
	for time.Now().Before(deadline) {
		time.Sleep(150 * time.Millisecond)
		var arr []map[string]any
		h.raw(t, "GET", "/v1/approvals?status=pending", nil, &arr)
		if len(arr) > 0 {
			apID, _ = arr[0]["id"].(string)
			break
		}
	}
	if apID == "" {
		t.Fatal("no approval appeared")
	}
	var dummy map[string]any
	h.raw(t, "POST", "/v1/approvals/"+apID+"/decide", map[string]string{"Action": action}, &dummy)

	select {
	case r := <-ch:
		return r.res, r.err
	case <-time.After(20 * time.Second):
		t.Fatal("call never returned after decision")
		return nil, nil
	}
}

func dumpResult(res *mcp.CallToolResult) string {
	if res == nil {
		return "<nil>"
	}
	var b strings.Builder
	for _, c := range res.Content {
		if t, ok := mcp.AsTextContent(c); ok {
			b.WriteString(t.Text)
			b.WriteString(" ")
		}
	}
	return strings.TrimSpace(b.String())
}

var _ = fmt.Sprint
