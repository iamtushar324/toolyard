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
// MCPTasks, as agent ag_1.
type tasksClient struct {
	t   *testing.T
	url string
	sid string
	id  int
}

func newTasksServer(t *testing.T, f *jobsFixture) *tasksClient {
	t.Helper()
	ctxFunc := func(ctx context.Context, _ *http.Request) context.Context { return WithAgentID(ctx, "ag_1") }
	streamable := server.NewStreamableHTTPServer(f.gw.MCPServer(), server.WithHTTPContextFunc(ctxFunc))
	srv := httptest.NewServer(f.gw.MCPTasks(streamable, ctxFunc))
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
	if c.sid != "" {
		req.Header.Set("Mcp-Session-Id", c.sid)
		req.Header.Set("Mcp-Protocol-Version", "2025-11-25")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatalf("%s: %v", method, err)
	}
	defer resp.Body.Close()
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
		c.t.Fatalf("%s: %d %q: %v", method, resp.StatusCode, raw, err)
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
	c := newTasksServer(t, f)

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
	if marked["alpha.get_slow"] != "optional" || marked["alpha.get_fast"] != "" {
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
	c := newTasksServer(t, f)
	c.initialize()
	created := c.result("tools/call", map[string]any{
		"name": "alpha.get_slow", "arguments": map[string]any{ReasonField: jobsTestReason}, "task": map[string]any{},
	})
	task, _ := created["task"].(map[string]any)
	taskID, _ := task["taskId"].(string)
	deadline := time.Now().Add(10 * time.Second)
	for {
		j, err := f.svc.Get(context.Background(), taskID)
		if err == nil && j.Status == jobs.StatusRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("task never started: %v %v", j, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancelled := c.result("tasks/cancel", map[string]any{"taskId": taskID})
	if cancelled["status"] != string(mcp.TaskStatusCancelled) {
		t.Fatalf("tasks/cancel = %v", cancelled)
	}
}
