package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/tusharbhardwaj/toolyard/internal/jobs"
	"github.com/tusharbhardwaj/toolyard/internal/policy"
)

// tasksClient speaks raw MCP 2025-11-25 JSON-RPC to the gateway through
// MCPTasks, as one agent.
type tasksClient struct {
	t   *testing.T
	url string
	sid string
	id  int
	// host, when set, is the Host header sent; header adds headers.
	host   string
	header http.Header
	status int
}

func newTasksServer(t *testing.T, f *jobsFixture, agent string, localhostProtection bool) *tasksClient {
	t.Helper()
	ctxFunc := func(ctx context.Context, _ *http.Request) context.Context { return WithAgentID(ctx, agent) }
	streamable := server.NewStreamableHTTPServer(f.gw.MCPServer(), server.WithHTTPContextFunc(ctxFunc),
		server.WithDisableLocalhostProtection(!localhostProtection))
	srv := httptest.NewServer(PreserveToolCallNumbers(f.gw.MCPTasks(streamable, ctxFunc, localhostProtection)))
	t.Cleanup(srv.Close)
	return &tasksClient{t: t, url: srv.URL + "/mcp"}
}

// send posts one message and returns the JSON-RPC answer (nil for a
// notification).
func (c *tasksClient) send(method string, params any, notification bool) map[string]any {
	c.t.Helper()
	msg := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		msg["params"] = params
	}
	if !notification {
		c.id++
		msg["id"] = c.id
	}
	body, _ := json.Marshal(msg)
	req, _ := http.NewRequest(http.MethodPost, c.url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if c.host != "" {
		req.Host = c.host
	}
	for k, vs := range c.header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if c.sid != "" {
		req.Header.Set("Mcp-Session-Id", c.sid)
		req.Header.Set("Mcp-Protocol-Version", "2025-11-25")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatalf("%s: %v", method, err)
	}
	defer resp.Body.Close()
	c.status = resp.StatusCode
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		c.sid = sid
	}
	raw, _ := io.ReadAll(resp.Body)
	if notification {
		return nil
	}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		sc := bufio.NewScanner(bytes.NewReader(raw))
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		var last []byte
		for sc.Scan() {
			if line := sc.Text(); strings.HasPrefix(line, "data:") {
				data := []byte(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
				if bytes.Contains(data, []byte(`"id"`)) {
					last = data
				}
			}
		}
		raw = last
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return map[string]any{"http_status": float64(resp.StatusCode), "raw": string(raw)}
	}
	return out
}

func (c *tasksClient) result(method string, params any) map[string]any {
	c.t.Helper()
	out := c.send(method, params, false)
	res, ok := out["result"].(map[string]any)
	if !ok {
		c.t.Fatalf("%s: no result: %v", method, out)
	}
	return res
}

func (c *tasksClient) initialize() map[string]any {
	c.t.Helper()
	res := c.result("initialize", map[string]any{
		"protocolVersion": "2025-11-25",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "tasks-test", "version": "1.0.0"},
	})
	c.send("notifications/initialized", nil, true)
	return res
}

func TestMCPTasksRunAsyncToolsAsDurableTasks(t *testing.T) {
	f := newJobsFixture(t)
	f.rules.set("alpha", jobs.AsyncSetting{Tools: map[string]jobs.AsyncRule{"get_slow": {Mode: jobs.AsyncAlways}}})
	c := newTasksServer(t, f, "ag_1", true)

	init := c.initialize()
	if caps, _ := init["capabilities"].(map[string]any); caps == nil || caps["tasks"] == nil {
		t.Fatalf("initialize does not advertise tasks: %v", init)
	}
	tools := c.result("tools/list", map[string]any{})
	marked := map[string]string{}
	list, _ := tools["tools"].([]any)
	for _, raw := range list {
		tool, _ := raw.(map[string]any)
		name, _ := tool["name"].(string)
		if ex, ok := tool["execution"].(map[string]any); ok {
			marked[name], _ = ex["taskSupport"].(string)
		}
	}
	// Any tool can run as a job: every tool is marked, the jobs tools not.
	if marked["alpha.get_slow"] != "optional" || marked["alpha.get_fast"] != "optional" || marked[MetaJobsStatus] != "" {
		t.Fatalf("taskSupport marks = %v", marked)
	}

	created := c.result("tools/call", map[string]any{
		"name": "alpha.get_slow", "arguments": map[string]any{ReasonField: jobsTestReason}, "task": map[string]any{"ttl": 60000},
	})
	task, _ := created["task"].(map[string]any)
	taskID, _ := task["taskId"].(string)
	if !strings.HasPrefix(taskID, "job_") || task["status"] != string(mcp.TaskStatusWorking) {
		t.Fatalf("tools/call with a task = %v", created)
	}
	if got := c.result("tasks/get", map[string]any{"taskId": taskID}); got["status"] != string(mcp.TaskStatusWorking) || got["taskId"] != taskID {
		t.Fatalf("tasks/get while running = %v", got)
	}
	// The same work is a job the jobs.* tools read.
	if st := structured(t, f.call(t, MetaJobsStatus, map[string]any{"job_id": taskID})); st["via"] != jobViaTask {
		t.Fatalf("jobs.status of the task = %v", st)
	}

	f.releaseAll()
	res := c.result("tasks/result", map[string]any{"taskId": taskID})
	if !strings.Contains(asJSON(res["content"]), "slow done") || res["isError"] == true {
		t.Fatalf("tasks/result = %v", res)
	}
	if meta, _ := res["_meta"].(map[string]any); meta == nil || meta[mcp.RelatedTaskMetaKey] == nil {
		t.Fatalf("tasks/result has no related-task meta: %v", res)
	}
	if got := c.result("tasks/get", map[string]any{"taskId": taskID}); got["status"] != string(mcp.TaskStatusCompleted) {
		t.Fatalf("tasks/get after = %v", got)
	}
	listed := c.result("tasks/list", map[string]any{})
	if !strings.Contains(asJSON(listed["tasks"]), taskID) {
		t.Fatalf("tasks/list = %v", listed)
	}
	if out := c.send("tasks/cancel", map[string]any{"taskId": taskID}, false); out["error"] == nil {
		t.Fatalf("cancelling a completed task = %v", out)
	}
	if out := c.send("tasks/get", map[string]any{"taskId": "job_unknown"}, false); out["error"] == nil {
		t.Fatalf("tasks/get of an unknown task = %v", out)
	}

	// A call refused before it could run still gets a task, final, whose
	// result is the refusal.
	if _, err := f.gw.policy.Set(context.Background(), policy.ScopeTool, "alpha.get_fast", "deny", "", false); err != nil {
		t.Fatal(err)
	}
	refused := c.result("tools/call", map[string]any{
		"name": "alpha.get_fast", "arguments": map[string]any{ReasonField: jobsTestReason}, "task": map[string]any{},
	})
	rt, _ := refused["task"].(map[string]any)
	if rt["status"] != string(mcp.TaskStatusFailed) {
		t.Fatalf("refused task = %v", refused)
	}
	// A recorded answer is kept an hour, not the jobs' retention.
	if rt["ttl"] != float64(jobs.RecordedRetention.Milliseconds()) {
		t.Fatalf("refused task ttl = %v", rt["ttl"])
	}
	rres := c.result("tasks/result", map[string]any{"taskId": rt["taskId"]})
	if rres["isError"] != true || !strings.Contains(asJSON(rres["content"]), "denied by policy") {
		t.Fatalf("refused task result = %v", rres)
	}
	// A plain tools/call (no task) is still answered by the transport.
	plain := c.result("tools/call", map[string]any{"name": "alpha.get_echo", "arguments": map[string]any{ReasonField: jobsTestReason}})
	if !strings.Contains(asJSON(plain["content"]), "echo") {
		t.Fatalf("plain tools/call = %v", plain)
	}
}

func TestMCPTasksCancelARunningTask(t *testing.T) {
	f := newJobsFixture(t)
	c := newTasksServer(t, f, "ag_1", true)
	c.initialize()
	created := c.result("tools/call", map[string]any{
		"name": "alpha.get_slow", "arguments": map[string]any{ReasonField: jobsTestReason}, "task": map[string]any{},
	})
	task, _ := created["task"].(map[string]any)
	taskID, _ := task["taskId"].(string)
	// The upstream call is in flight before the cancel.
	select {
	case <-f.slowStarted:
	case <-time.After(10 * time.Second):
		t.Fatalf("the task's call never started (%s)", taskID)
	}
	cancelled := c.result("tasks/cancel", map[string]any{"taskId": taskID})
	if cancelled["status"] != string(mcp.TaskStatusCancelled) {
		t.Fatalf("tasks/cancel = %v", cancelled)
	}
}

// A DNS rebinding page cannot reach the tasks methods of a local gateway:
// a loopback listener with a Host that is not loopback is refused, as the
// transport refuses it, unless that protection is off (-public-url).
func TestMCPTasksRefuseDNSRebinding(t *testing.T) {
	f := newJobsFixture(t)
	c := newTasksServer(t, f, "ag_1", true)
	c.host = "evil.example"
	out := c.send("tools/call", map[string]any{"name": "alpha.get_fast", "arguments": map[string]any{ReasonField: jobsTestReason}, "task": map[string]any{}}, false)
	if c.status != http.StatusForbidden {
		t.Fatalf("rebinding tools/call with a task = %d %v", c.status, out)
	}
	c.send("tasks/list", map[string]any{}, false)
	if c.status != http.StatusForbidden {
		t.Fatalf("rebinding tasks/list = %d", c.status)
	}
	if n := f.jobCount(t); n != 0 {
		t.Fatalf("%d jobs stored by a rebinding request", n)
	}
	off := newTasksServer(t, f, "ag_1", false)
	off.host = "evil.example"
	if out := off.send("tasks/list", map[string]any{}, false); off.status != http.StatusOK || out["result"] == nil {
		t.Fatalf("tasks/list with the protection off = %d %v", off.status, out)
	}
}

// A 2026-07-28 request gets the transport's checks: the version header
// must match _meta and Mcp-Method the body, errors carry HTTP statuses, and
// tasks/result and tasks/list do not exist in that version.
func TestMCPTasksCheckModernRequests(t *testing.T) {
	f := newJobsFixture(t)
	c := newTasksServer(t, f, "ag_1", true)
	modernMeta := map[string]any{
		mcp.MetaKeyProtocolVersion: mcp.ProtocolVersion20260728,
		mcp.MetaKeyClientInfo:      map[string]any{"name": "modern-test", "version": "1.0.0"},
	}
	params := func(extra map[string]any) map[string]any {
		p := map[string]any{"_meta": modernMeta}
		for k, v := range extra {
			p[k] = v
		}
		return p
	}
	c.header = http.Header{mcp.HeaderProtocolVersion: {"2025-11-25"}, mcp.HeaderMethod: {"tasks/get"}}
	if out := c.send("tasks/get", params(map[string]any{"taskId": "job_x"}), false); c.status != http.StatusBadRequest || out["error"] == nil {
		t.Fatalf("mismatched version header = %d %v", c.status, out)
	}
	c.header = http.Header{mcp.HeaderProtocolVersion: {mcp.ProtocolVersion20260728}, mcp.HeaderMethod: {"tasks/cancel"}}
	if out := c.send("tasks/get", params(map[string]any{"taskId": "job_x"}), false); c.status != http.StatusBadRequest || out["error"] == nil {
		t.Fatalf("mismatched Mcp-Method = %d %v", c.status, out)
	}
	c.header = http.Header{mcp.HeaderProtocolVersion: {mcp.ProtocolVersion20260728}, mcp.HeaderMethod: {"tasks/get"}}
	if out := c.send("tasks/get", params(map[string]any{"taskId": "job_x"}), false); c.status != http.StatusBadRequest || out["error"] == nil {
		t.Fatalf("unknown task on a modern request = %d %v", c.status, out)
	}
	c.header = http.Header{mcp.HeaderProtocolVersion: {mcp.ProtocolVersion20260728}, mcp.HeaderMethod: {"tasks/list"}}
	if out := c.send("tasks/list", params(nil), false); c.status != http.StatusNotFound || out["error"] == nil {
		t.Fatalf("modern tasks/list = %d %v", c.status, out)
	}

	// A modern tools/call with a task, then tasks/get carries the result.
	call := params(map[string]any{"name": "alpha.get_fast", "arguments": map[string]any{ReasonField: jobsTestReason}, "task": map[string]any{}})
	name, _ := mcp.EncodeHeaderValue("alpha.get_fast")
	c.header = http.Header{mcp.HeaderProtocolVersion: {mcp.ProtocolVersion20260728}, mcp.HeaderMethod: {"tools/call"}, mcp.HeaderName: {name}}
	created := c.send("tools/call", call, false)
	res, _ := created["result"].(map[string]any)
	task, _ := res["task"].(map[string]any)
	taskID, _ := task["taskId"].(string)
	if c.status != http.StatusOK || taskID == "" || res["resultType"] != string(mcp.ResultTypeComplete) {
		t.Fatalf("modern tools/call with a task = %d %v", c.status, created)
	}
	if _, _, err := f.svc.WaitFor(context.Background(), []string{taskID}, false, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	c.header = http.Header{mcp.HeaderProtocolVersion: {mcp.ProtocolVersion20260728}, mcp.HeaderMethod: {"tasks/get"}}
	got := c.result("tasks/get", params(map[string]any{"taskId": taskID}))
	if got["status"] != string(mcp.TaskStatusCompleted) || !strings.Contains(asJSON(got["result"]), "fast") {
		t.Fatalf("modern tasks/get = %v", got)
	}
}

func TestMCPTasksRefuseABadCallbackRef(t *testing.T) {
	f := newJobsFixture(t)
	c := newTasksServer(t, f, "ag_1", true)
	c.initialize()
	out := c.send("tools/call", map[string]any{"name": "alpha.get_fast", "task": map[string]any{},
		"arguments": map[string]any{ReasonField: jobsTestReason, callbackRefArg: "bad\nref"}}, false)
	errObj, _ := out["error"].(map[string]any)
	if errObj == nil || errObj["code"] != float64(mcp.INVALID_PARAMS) {
		t.Fatalf("bad _callback_ref = %v", out)
	}
	if n := f.jobCount(t); n != 0 {
		t.Fatalf("%d jobs stored for a bad _callback_ref", n)
	}
}
