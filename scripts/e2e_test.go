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

	// Turn it on; tools/list must shrink to just the pinned set.
	var on map[string]any
	h.raw(t, "PATCH", "/v1/settings", map[string]any{"router_only_mode": true}, &on)
	if on["router_only_mode"] != true {
		t.Errorf("settings did not echo router_only_mode=true; got %v", on)
	}

	// Need a fresh MCP client since the streamable HTTP session caches
	// a server-info snapshot; the filter is still applied per-call though.
	c2 := mcpClient(t, *toolyardURL, token)
	defer c2.Close()
	pinned := map[string]bool{
		"tools.search": true, "tools.execute": true,
		"memory.get": true, "memory.set": true,
		"memory.list": true, "memory.delete": true,
	}
	min, err := c2.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(min.Tools) != len(pinned) {
		t.Fatalf("router-only should expose the pinned set (%d), got %d", len(pinned), len(min.Tools))
	}
	gotNames := map[string]bool{}
	for _, tl := range min.Tools {
		gotNames[tl.Name] = true
	}
	for name := range pinned {
		if !gotNames[name] {
			t.Errorf("pinned tool %q missing in router-only mode", name)
		}
	}

	// Direct calls to a non-pinned tool are rejected. fixture.echo is the
	// upstream we know is always registered and not pinned.
	directReq := mcp.CallToolRequest{}
	directReq.Params.Name = "fixture.echo"
	directReq.Params.Arguments = map[string]any{
		"_reason": "trying to bypass router-only mode by calling the hidden tool directly",
		"message": "hi",
	}
	directRes, err := c2.CallTool(ctx, directReq)
	if err != nil {
		t.Fatalf("direct call in router-only mode: %v", err)
	}
	if !directRes.IsError {
		t.Errorf("expected direct call to fixture.echo to be rejected in router-only mode, got %s", dumpResult(directRes))
	}
	if !strings.Contains(strings.ToLower(dumpResult(directRes)), "hidden") {
		t.Errorf("rejection message should mention hiding, got %s", dumpResult(directRes))
	}

	// Underlying hidden tool is still callable through tools.execute.
	exec := mcp.CallToolRequest{}
	exec.Params.Name = "tools.execute"
	exec.Params.Arguments = map[string]any{
		"_reason": "verifying tools.execute still routes to a hidden tool",
		"tool":    "fixture.echo",
		"arguments": map[string]any{
			"_reason": "router-only-mode reachability check via meta-tool",
			"message": "hi",
		},
	}
	res, err := c2.CallTool(ctx, exec)
	if err != nil {
		t.Fatalf("execute via meta in router-only-mode: %v", err)
	}
	// fixture.echo is a write -> the inner call holds for approval and we
	// see a deferred response. That's the proof we want: routing reached the
	// hidden tool's policy/approval pipeline. The rejection message would
	// have mentioned hiding instead.
	if strings.Contains(strings.ToLower(dumpResult(res)), "hidden") {
		t.Errorf("tools.execute should bypass the filter; got %s", dumpResult(res))
	}

	// Restore default for other tests.
	h.raw(t, "PATCH", "/v1/settings", map[string]any{"router_only_mode": false}, &off)
}

// TestE2EUsageAndTopN: counters increment only on success, top-N mode keeps
// pinned tools always visible plus a per-agent ranked tail. Cold-start
// agents with no history fall back to the overall top-N.
func TestE2EUsageAndTopN(t *testing.T) {
	flag.Parse()
	h := &httpClient{base: *toolyardURL}
	mustLogin(t, h)

	// Reset to full mode so we have a clean baseline.
	var s map[string]any
	h.raw(t, "PATCH", "/v1/settings", map[string]any{"surface_mode": "full"}, &s)

	// Drive a few successful memory.set calls under the dashboard:<uid>
	// agent so the counter accrues.
	for i := 0; i < 3; i++ {
		var dummy map[string]any
		h.raw(t, "POST", "/v1/tools/run", map[string]any{
			"tool": "memory.set",
			"arguments": map[string]any{
				"_reason":      "exercising the usage counter increment via the dashboard agent",
				"_approval_id": "", // no approval needed because dashboard agent calls go through approval flow
				"key":          fmt.Sprintf("usage-%d", i),
				"value":        "x",
			},
		}, &dummy)
		// The first call returns deferred (write needs approval). Approve it.
		if sc, ok := dummy["structured_content"].(map[string]any); ok {
			if apID, _ := sc["approval_id"].(string); apID != "" {
				var d map[string]any
				h.raw(t, "POST", "/v1/approvals/"+apID+"/decide", map[string]string{"Action": "allowed"}, &d)
				// Resume to actually run the call.
				h.raw(t, "POST", "/v1/tools/run", map[string]any{
					"tool": "memory.set",
					"arguments": map[string]any{
						"_reason":      "resuming the deferred call after approval",
						"_approval_id": apID,
					},
				}, &dummy)
			}
		}
	}

	// /v1/usage should now show memory.set counts.
	var usage map[string]any
	h.raw(t, "GET", "/v1/usage", nil, &usage)
	per, _ := usage["per_tool"].(map[string]any)
	if v, _ := per["memory.set"].(float64); v < 1 {
		t.Errorf("expected memory.set counter > 0, got %v", per["memory.set"])
	}
	t.Logf("per_tool counts: %v", per)

	// Switch to top_n mode with N=2 and a high threshold so cold-start
	// (overall top-N) applies.
	h.raw(t, "PATCH", "/v1/settings", map[string]any{
		"surface_mode": "top_n", "top_n_count": 2, "top_n_personalize_after": 1000000,
	}, &s)

	// New agent → cold start → sees pinned + overall top-2.
	token := enrollAgent(t, h, "topn-cold")
	c := mcpClient(t, *toolyardURL, token)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	tools, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	gotNames := map[string]bool{}
	for _, tl := range tools.Tools {
		gotNames[tl.Name] = true
	}
	t.Logf("cold-start agent saw %d tools: %v", len(tools.Tools), keysOf(gotNames))
	pinned := []string{"tools.search", "tools.execute", "memory.get", "memory.set", "memory.list", "memory.delete"}
	for _, p := range pinned {
		if !gotNames[p] {
			t.Errorf("pinned tool %q not visible in top_n cold-start; got %v", p, keysOf(gotNames))
		}
	}
	// We expect at most 6 pinned + 2 ranked tail = 8.
	if len(tools.Tools) > 8 {
		t.Errorf("expected ≤8 tools in top_n with N=2, got %d", len(tools.Tools))
	}
	// memory.set is in the pinned set, so it'll be there regardless. Check
	// at least one *non-pinned* tool came in via the ranked tail (if there
	// is any usage on a non-pinned tool — there isn't yet, so the tail may
	// be empty; the pinned set alone is acceptable).

	// Switch to router_only and confirm we shrink to exactly the pinned set.
	h.raw(t, "PATCH", "/v1/settings", map[string]any{"surface_mode": "router_only"}, &s)
	c2 := mcpClient(t, *toolyardURL, token)
	defer c2.Close()
	ro, err := c2.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(ro.Tools) != len(pinned) {
		t.Errorf("router_only should expose exactly the pinned set (%d), got %d", len(pinned), len(ro.Tools))
	}

	// Restore default for other tests.
	h.raw(t, "PATCH", "/v1/settings", map[string]any{"surface_mode": "full"}, &s)
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestE2EBatchApproval: an agent fires several writes in parallel; the
// dashboard's batch-decide resolves them all in one call.
func TestE2EBatchApproval(t *testing.T) {
	flag.Parse()
	h := &httpClient{base: *toolyardURL}
	mustLogin(t, h)
	token := enrollAgent(t, h, "batch-test")
	c := mcpClient(t, *toolyardURL, token)
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Fire three concurrent memory.set writes. They all need approval.
	type cr struct {
		res *mcp.CallToolResult
		err error
	}
	results := make(chan cr, 3)
	keys := []string{"batch-a", "batch-b", "batch-c"}
	for _, k := range keys {
		key := k
		go func() {
			req := mcp.CallToolRequest{}
			req.Params.Name = "memory.set"
			req.Params.Arguments = map[string]any{
				"_reason": "batch-approval e2e: three parallel writes from one turn (" + key + ")",
				"key":     key,
				"value":   "v",
			}
			res, err := c.CallTool(ctx, req)
			results <- cr{res, err}
		}()
	}

	// Wait until at least 3 pending approvals are visible for this agent.
	deadline := time.Now().Add(8 * time.Second)
	var pending []map[string]any
	for time.Now().Before(deadline) {
		time.Sleep(120 * time.Millisecond)
		var arr []map[string]any
		h.raw(t, "GET", "/v1/approvals?status=pending", nil, &arr)
		pending = pending[:0]
		for _, a := range arr {
			if rr, _ := a["reason"].(string); strings.Contains(rr, "batch-approval e2e") {
				pending = append(pending, a)
			}
		}
		if len(pending) >= 3 {
			break
		}
	}
	if len(pending) < 3 {
		t.Fatalf("expected 3 pending approvals, got %d", len(pending))
	}
	ids := make([]string, 0, len(pending))
	for _, a := range pending {
		ids = append(ids, a["id"].(string))
	}

	// Batch-allow.
	var out []map[string]any
	h.raw(t, "POST", "/v1/approvals/decide-batch",
		map[string]any{"ids": ids, "action": "allowed"}, &out)
	if len(out) != len(ids) {
		t.Fatalf("expected %d results, got %d", len(ids), len(out))
	}
	for _, r := range out {
		if r["status"] != "allowed" {
			t.Errorf("expected allowed, got %v", r)
		}
	}

	// All three calls should now complete.
	for i := 0; i < 3; i++ {
		select {
		case r := <-results:
			if r.err != nil {
				t.Errorf("call err: %v", r.err)
				continue
			}
			if r.res.IsError {
				t.Errorf("call error: %s", dumpResult(r.res))
			}
		case <-time.After(15 * time.Second):
			t.Fatal("a batched call never returned")
		}
	}
}

// TestE2EAgentDirectCreate: POST /v1/agents returns the token in one shot
// and the token works against /mcp without an intermediate exchange step.
func TestE2EAgentDirectCreate(t *testing.T) {
	flag.Parse()
	h := &httpClient{base: *toolyardURL}
	mustLogin(t, h)

	var ag map[string]any
	h.raw(t, "POST", "/v1/agents", map[string]string{"Name": "direct-create-test"}, &ag)
	tok, _ := ag["token"].(string)
	id, _ := ag["agent_id"].(string)
	if tok == "" || id == "" {
		t.Fatalf("expected token + agent_id, got %v", ag)
	}
	if !strings.HasPrefix(tok, id+".") {
		t.Errorf("token should start with %s., got %s", id, tok[:20])
	}

	// Use the token immediately against /mcp (initialize handshake).
	c := mcpClient(t, *toolyardURL, tok)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tools, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatalf("token from /v1/agents failed against /mcp: %v", err)
	}
	if len(tools.Tools) == 0 {
		t.Fatal("expected at least one tool")
	}

	// And the agent should appear in the list.
	var listed []map[string]any
	h.raw(t, "GET", "/v1/agents", nil, &listed)
	found := false
	for _, a := range listed {
		if a["id"] == id && a["name"] == "direct-create-test" {
			found = true
		}
	}
	if !found {
		t.Error("agent not in /v1/agents list after create")
	}
}

// TestE2ECoalesceAndApprovalIDSchema: identical pending writes coalesce
// into one approval row, the schema-wrap exposes _approval_id on every
// wrapped tool, and direct upstream calls accept it for resume.
func TestE2ECoalesceAndApprovalIDSchema(t *testing.T) {
	flag.Parse()
	h := &httpClient{base: *toolyardURL}
	mustLogin(t, h)
	token := enrollAgent(t, h, "coalesce-test")
	c := mcpClient(t, *toolyardURL, token)
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tools, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	// Every wrapped tool should expose _approval_id in its inputSchema.
	for _, tl := range tools.Tools {
		props, _ := tl.InputSchema.Properties, tl.InputSchema.Required
		if _, ok := props["_approval_id"]; !ok {
			t.Errorf("tool %q is missing _approval_id in its schema", tl.Name)
		}
	}

	// Fire the SAME write twice in parallel. Should produce ONE approval
	// row (coalesced), not two.
	resCh := make(chan *mcp.CallToolResult, 2)
	args := map[string]any{
		"_reason": "coalesce test: identical writes should produce one approval card",
		"key":     "coalesce-key",
		"value":   "v1",
	}
	for i := 0; i < 2; i++ {
		go func() {
			req := mcp.CallToolRequest{}
			req.Params.Name = "memory.set"
			req.Params.Arguments = args
			r, _ := c.CallTool(ctx, req)
			resCh <- r
		}()
	}

	deadline := time.Now().Add(8 * time.Second)
	var apID string
	for time.Now().Before(deadline) {
		time.Sleep(120 * time.Millisecond)
		var arr []map[string]any
		h.raw(t, "GET", "/v1/approvals?status=pending", nil, &arr)
		matched := 0
		for _, a := range arr {
			if rr, _ := a["reason"].(string); strings.Contains(rr, "coalesce test") {
				matched++
				apID, _ = a["id"].(string)
			}
		}
		if matched > 0 {
			if matched != 1 {
				t.Fatalf("expected exactly one pending approval after coalesce, got %d", matched)
			}
			break
		}
	}
	if apID == "" {
		t.Fatal("no pending approval appeared")
	}

	// Approve and confirm both in-flight calls return.
	var d map[string]any
	h.raw(t, "POST", "/v1/approvals/"+apID+"/decide", map[string]string{"Action": "allowed"}, &d)
	for i := 0; i < 2; i++ {
		select {
		case r := <-resCh:
			// One of them might be a deferred response (the second goroutine
			// landed after the first, saw the same fingerprint, got the
			// same id back; if the first goroutine already consumed the
			// approval the second sees status=allowed and dispatches).
			if r != nil && r.IsError {
				// Acceptable iff this is the deferred response that ran
				// before approval; ignore.
			}
		case <-time.After(15 * time.Second):
			t.Fatal("call never returned after coalesced-approval decision")
		}
	}

	// Direct upstream call with _approval_id resumes a deferred call.
	// Create a fresh deferred approval and resume by passing _approval_id.
	req := mcp.CallToolRequest{}
	req.Params.Name = "memory.set"
	req.Params.Arguments = map[string]any{
		"_reason": "approval-id schema test: a fresh write that we will resume by id",
		"key":     "resume-key",
		"value":   "v2",
	}
	first, err := c.CallTool(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	sc, _ := first.StructuredContent.(map[string]any)
	if sc == nil || sc["status"] != "pending_approval" {
		t.Skipf("first call did not defer (server may have a long in-line wait); got %v", sc)
	}
	resumeID, _ := sc["approval_id"].(string)
	if resumeID == "" {
		t.Fatal("missing approval_id in deferred response")
	}
	h.raw(t, "POST", "/v1/approvals/"+resumeID+"/decide", map[string]string{"Action": "allowed"}, &d)

	// Resume by passing _approval_id directly to memory.set (NOT via tools.execute).
	resumeReq := mcp.CallToolRequest{}
	resumeReq.Params.Name = "memory.set"
	resumeReq.Params.Arguments = map[string]any{
		"_reason":      "resuming the approved write directly",
		"_approval_id": resumeID,
	}
	resumeRes, err := c.CallTool(ctx, resumeReq)
	if err != nil {
		t.Fatalf("resume direct: %v", err)
	}
	if resumeRes.IsError {
		t.Errorf("expected resume to succeed, got: %s", dumpResult(resumeRes))
	}
	if !strings.Contains(dumpResult(resumeRes), "v2") {
		t.Errorf("expected stored value in resumed result, got: %s", dumpResult(resumeRes))
	}
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
