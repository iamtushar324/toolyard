package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/inbox"
	"github.com/tusharbhardwaj/toolyard/internal/policy"
	"github.com/tusharbhardwaj/toolyard/internal/toolsearch"
)

// openAIToolName is OpenAI's tool-name rule: every name tools/list
// publishes with safe tool names on must match it.
var openAIToolName = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

func withSafeToolNames(o *Options) { o.SafeToolNames = true }

// registerTestTool adds a read tool under upstream alpha that answers "ran".
func registerTestTool(g *Gateway, name string) {
	g.registerEntry(toolEntry{
		tool:     mcp.Tool{Name: name, InputSchema: mcp.ToolInputSchema{Type: "object", Properties: addMetaProps(map[string]any{})}},
		upstream: "alpha", originalName: strings.TrimPrefix(name, "alpha."), reasonField: ReasonField,
		handle: func(context.Context, map[string]any) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText("ran"), nil
		},
	})
}

// mcpCallArgs runs tools/call with arguments through the MCP server.
func mcpCallArgs(t *testing.T, g *Gateway, ctx context.Context, tool string, args map[string]any) rpcReply {
	t.Helper()
	msg, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args},
	})
	b, _ := json.Marshal(g.MCPServer().HandleMessage(ctx, json.RawMessage(msg)))
	var out struct {
		Result *struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(b, &out); err != nil || (out.Result == nil) == (out.Error == nil) {
		t.Fatalf("tools/call %s: %v %s", tool, err, b)
	}
	r := rpcReply{raw: b}
	if out.Error != nil {
		r.rpcErr, r.code, r.message = true, out.Error.Code, out.Error.Message
		return r
	}
	r.isError = out.Result.IsError
	for _, c := range out.Result.Content {
		r.text += c.Text
	}
	return r
}

// mcpToolDescriptions is tools/list as name -> description.
func mcpToolDescriptions(t *testing.T, g *Gateway, ctx context.Context) map[string]string {
	t.Helper()
	b, _ := json.Marshal(g.MCPServer().HandleMessage(ctx, json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)))
	var out struct {
		Result struct {
			Tools []struct {
				Name        string `json:"name"`
				Description string `json:"description"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("tools/list: %v %s", err, b)
	}
	m := make(map[string]string, len(out.Result.Tools))
	for _, tl := range out.Result.Tools {
		m[tl.Name] = tl.Description
	}
	return m
}

func TestPublishedToolName(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"inbox.request", "inbox_request"},
		{"tools.search", "tools_search"},
		{"getToolDocs", "getToolDocs"},
		{"BkCoreServices.get-client", "BkCoreServices_get-client"},
		{"bkt3-ds1.t3_answer", "bkt3-ds1_t3_answer"},
		{"github.repos/list", "github_repos_list"},
		{"a.é", "a__"},
	} {
		if got := PublishedToolName(c.in); got != c.want {
			t.Errorf("PublishedToolName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	long := "Server." + strings.Repeat("a_really_long_tool_name_", 4)
	got := PublishedToolName(long)
	if len(got) != maxPublishedNameLen || !strings.HasPrefix(got, sanitizeToolName(long)[:publishedHashPrefix]+"_") || !openAIToolName.MatchString(got) {
		t.Errorf("long name -> %q (%d characters)", got, len(got))
	}
	if again := PublishedToolName(long); again != got {
		t.Errorf("not deterministic: %q, then %q", got, again)
	}
	if other := PublishedToolName(long + "x"); other == got {
		t.Errorf("two long names that share their first 64 characters both got %q", got)
	}
	if e := PublishedToolName(""); !openAIToolName.MatchString(e) {
		t.Errorf("empty name -> %q", e)
	}
	for _, n := range []string{"tools_search", "a-b_C9", strings.Repeat("x", 64)} {
		if !ValidPublishedToolName(n) {
			t.Errorf("%q should be valid", n)
		}
	}
	for _, n := range []string{"", "tools.search", "a b", strings.Repeat("x", 65), "é"} {
		if ValidPublishedToolName(n) {
			t.Errorf("%q should not be valid", n)
		}
	}
}

// TestPublishedToolNamesCollisions: two tools that map to the same name
// both get the hashed form, except one whose canonical name is already
// that name: it keeps it. The answer does not depend on the order.
func TestPublishedToolNamesCollisions(t *testing.T) {
	names := []string{"a.b", "a/b", "a_b", "x.y", "solo"}
	got := publishedToolNames(names)
	if got["a_b"] != "a_b" || got["x.y"] != "x_y" || got["solo"] != "solo" {
		t.Fatalf("names without a collision changed: %v", got)
	}
	dot, slash := got["a.b"], got["a/b"]
	if dot == slash || dot == "a_b" || slash == "a_b" || !strings.HasPrefix(dot, "a_b_") || !strings.HasPrefix(slash, "a_b_") ||
		len(dot) != len("a_b_")+publishedHashLen {
		t.Fatalf("colliding names: a.b -> %q, a/b -> %q", dot, slash)
	}
	seen := map[string]string{}
	for c, p := range got {
		if !openAIToolName.MatchString(p) {
			t.Errorf("%s -> %q breaks the rule", c, p)
		}
		if prev, dup := seen[p]; dup {
			t.Errorf("%s and %s both publish as %q", prev, c, p)
		}
		seen[p] = c
	}
	rev := append([]string(nil), names...)
	sort.Sort(sort.Reverse(sort.StringSlice(rev)))
	if again := publishedToolNames(rev); !reflect.DeepEqual(again, got) {
		t.Fatalf("order changed the names: %v, then %v", got, again)
	}
	pair := publishedToolNames([]string{"m.n", "m/n"})
	if pair["m.n"] == "m_n" || pair["m/n"] == "m_n" || pair["m.n"] == pair["m/n"] {
		t.Fatalf("two renamed names that collide: %v", pair)
	}
}

// TestSafeToolNamesInToolsList: with safe names on, the small list is the
// five published names and the full list publishes every tool the agent
// may use under a valid, unique name: a long upstream name and two names
// that collide included. The access filter still applies first.
func TestSafeToolNamesInToolsList(t *testing.T) {
	f := newToolListFixture(t, withSafeToolNames)
	long := "alpha." + strings.Repeat("list_every_open_item_", 4)
	for _, n := range []string{long, "alpha.get.state", "alpha.get/state"} {
		registerTestTool(f.gw, n)
	}
	small := mcpToolsList(t, f.gw, WithAgentID(context.Background(), "ag_small"))
	want := make([]string, 0, len(SmallToolList))
	for _, n := range SmallToolList {
		want = append(want, PublishedToolName(n))
	}
	if got := sorted(small); !reflect.DeepEqual(got, sorted(want)) {
		t.Fatalf("small list = %v, want %v", got, sorted(want))
	}
	for _, n := range []string{"tools_search", "getToolDocs", "executeToolCode", "inbox_request", "inbox_status"} {
		if !has(small, n) {
			t.Errorf("small list lacks %s: %v", n, small)
		}
	}
	fullCtx := WithAgentID(context.Background(), "ag_full")
	full := mcpToolsList(t, f.gw, fullCtx)
	if n := len(f.gw.CatalogFor(fullCtx)); len(full) != n {
		t.Fatalf("full list has %d tools, the catalog %d", len(full), n)
	}
	seen := map[string]bool{}
	for _, n := range full {
		if !openAIToolName.MatchString(n) {
			t.Errorf("listed name %q breaks the rule", n)
		}
		if seen[n] {
			t.Errorf("%q is listed twice", n)
		}
		seen[n] = true
	}
	names := f.gw.toolNames()
	if lp := names.publish(long); len(lp) != maxPublishedNameLen || !seen[lp] {
		t.Errorf("long tool published as %q (%d characters), listed %v", lp, len(lp), seen[lp])
	}
	a, b := names.publish("alpha.get.state"), names.publish("alpha.get/state")
	if a == b || a == "alpha_get_state" || b == "alpha_get_state" || !seen[a] || !seen[b] {
		t.Errorf("colliding tools published as %q and %q", a, b)
	}
	if !seen["alpha_get_status"] || seen["alpha.get_status"] || !seen["tools_execute"] {
		t.Errorf("full list: %v", full)
	}
	deny := mcpToolsList(t, f.gw, WithAgentID(context.Background(), "ag_deny"))
	if has(deny, "inbox_status") || has(deny, "inbox.status") || len(deny) != len(SmallToolList)-1 {
		t.Errorf("an agent with inbox.status denied lists %v", deny)
	}
}

// TestSafeToolNamesOff: without the setting tools/list keeps the dotted
// names and the texts keep them too; a call by a published name still
// works, so a client that listed them earlier is not broken.
func TestSafeToolNamesOff(t *testing.T) {
	f := newToolListFixture(t)
	ctx := WithAgentID(context.Background(), "ag_small")
	small := mcpToolsList(t, f.gw, ctx)
	if !reflect.DeepEqual(sorted(small), sorted(SmallToolList)) || !has(small, "tools.search") || !has(small, "inbox.request") {
		t.Fatalf("small list with safe names off = %v", small)
	}
	if r := mcpToolsCall(t, f.gw, ctx, "alpha_get_status"); r.rpcErr || r.isError || !strings.Contains(r.text, "ran") {
		t.Fatalf("published name with safe names off: %s", r.raw)
	}
	descs := mcpToolDescriptions(t, f.gw, ctx)
	if d := descs["inbox.request"]; !strings.Contains(d, "inbox.request is the one way") || strings.Contains(d, "inbox_request") {
		t.Errorf("inbox.request description: %q", d)
	}
	text := buildInstructions(nil, time.Second, false)
	if strings.Contains(text, "NAMES:") || strings.Contains(text, "tools_search") || strings.Contains(text, "inbox_request") {
		t.Errorf("instructions with safe names off name published names: %q", text)
	}
	res, err := f.gw.SearchTools(WithAgentID(context.Background(), "ag_full"), SearchRequest{Query: "alpha", Mode: "keyword"})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range res["results"].([]toolsearch.Entry) {
		if e.MCPName != "" {
			t.Errorf("search result %s has mcp_name %q with safe names off", e.Name, e.MCPName)
		}
	}
}

// TestCallsByPublishedOrCanonicalName: a call by the published name runs
// the same tool as a call by the canonical name, over MCP and through
// tools.execute, and every audit row names the canonical tool. An unknown
// name stays unknown, and an agent's own deny holds under either name.
func TestCallsByPublishedOrCanonicalName(t *testing.T) {
	f := newToolListFixture(t, withSafeToolNames)
	ctx := WithAgentID(context.Background(), "ag_small")
	for _, name := range []string{"alpha_get_status", "alpha.get_status"} {
		if r := mcpToolsCall(t, f.gw, ctx, name); r.rpcErr || r.isError || !strings.Contains(r.text, "ran") {
			t.Fatalf("tools/call %s: %s", name, r.raw)
		}
	}
	res, err := f.gw.RouteCall(ctx, "test", "tools_execute", map[string]any{
		ReasonField: testReason, "tool": "alpha_get_status", "arguments": map[string]any{ReasonField: testReason},
	})
	if err != nil || res.IsError || !strings.Contains(textOf(res), "ran") {
		t.Fatalf("tools_execute of alpha_get_status: %v %s", err, textOf(res))
	}
	if n := f.calls.Load(); n != 3 {
		t.Fatalf("the tool ran %d times, want 3", n)
	}
	evs, err := f.gw.audit.Recent(context.Background(), 200)
	if err != nil {
		t.Fatal(err)
	}
	starts := 0
	for _, e := range evs {
		if e.ToolName == "alpha_get_status" || e.ToolName == "tools_execute" {
			t.Errorf("audit row %s names the published name %s", e.EventType, e.ToolName)
		}
		if e.ToolName == "alpha.get_status" && e.EventType == audit.EventCallStart {
			starts++
		}
	}
	if starts != 3 {
		t.Fatalf("%d call.start rows for alpha.get_status, want 3", starts)
	}
	if r := mcpToolsCall(t, f.gw, ctx, "alpha_no_such_tool"); !r.rpcErr || r.message != rpcNotFoundMessage("alpha_no_such_tool") {
		t.Fatalf("unknown name: %s", r.raw)
	}
	deny := WithAgentID(context.Background(), "ag_deny")
	for _, name := range []string{"inbox_status", "inbox.status"} {
		if r := mcpToolsCall(t, f.gw, deny, name); !r.rpcErr || !strings.Contains(r.message, "not allowed for this agent") {
			t.Errorf("%s by an agent that may not use it: %s", name, r.raw)
		}
	}
}

// grantFor sends one inbox.request for deploy.run that names it as tool,
// has the owner accept it and returns the grant token and the tool name
// the request kept.
func grantFor(t *testing.T, f *inboxFixture, tool string) (token, kept string) {
	t.Helper()
	args := requestArgs(map[string]any{"service": "api", "env": "prod"})
	args["tools"].([]any)[0].(map[string]any)["tool"] = tool
	s := structured(t, f.call(t, f.agent, "inbox.request", args))
	id, _ := s["request_id"].(string)
	if s["ok"] != true || id == "" {
		t.Fatalf("request naming %s: %v", tool, s)
	}
	f.svc.Flush()
	if _, err := f.svc.Decide(context.Background(), id, inbox.Decision{Action: "submit", RequestRevision: 1, SubmissionID: "approve_" + id,
		Verdicts: map[string]inbox.CallVerdict{"deploy_api": {Verdict: inbox.VerdictAccepted}}}); err != nil {
		t.Fatal(err)
	}
	w := structured(t, f.call(t, f.agent, "inbox.wait", map[string]any{"ids": []any{id}, "timeout_seconds": 1}))
	call := w["requests"].([]any)[0].(map[string]any)["tools"].([]any)[0].(map[string]any)
	token, _ = call["grant"].(string)
	kept, _ = call["tool"].(string)
	if !strings.HasPrefix(token, "tyg_") {
		t.Fatalf("no grant token for a request naming %s: %v", tool, w)
	}
	return token, kept
}

// TestGrantWorksWithEitherName: a request may name a tool by its published
// name and keeps the canonical one; a grant runs a call by either name,
// over MCP too, and the audit rows name the canonical tool.
func TestGrantWorksWithEitherName(t *testing.T) {
	f := newInboxFixture(t)
	f.mode = ApprovalModeInbox
	prod := func(tok string) map[string]any {
		return map[string]any{"service": "api", "env": "prod", GrantField: tok}
	}
	tok, kept := grantFor(t, f, "deploy_run")
	if kept != "deploy.run" {
		t.Fatalf("a request naming deploy_run kept %q", kept)
	}
	if res := f.call(t, f.agent, "deploy.run", prod(tok)); res.IsError || f.calls.Load() != 1 {
		t.Fatalf("canonical call with the grant of a published-name request: %s", textOf(res))
	}
	tok, _ = grantFor(t, f, "deploy.run")
	if res := f.call(t, f.agent, "deploy_run", prod(tok)); res.IsError || f.calls.Load() != 2 {
		t.Fatalf("published-name call with a canonical grant: %s", textOf(res))
	}
	tok, _ = grantFor(t, f, "deploy.run")
	args := prod(tok)
	args[ReasonField] = testReason
	if r := mcpCallArgs(t, f.gw, f.agent, "deploy_run", args); r.rpcErr || r.isError || f.calls.Load() != 3 {
		t.Fatalf("tools/call deploy_run with a grant: %s", r.raw)
	}
	// A used grant stays used under the other name.
	if res := f.call(t, f.agent, "deploy.run", prod(tok)); structured(t, res)["status"] != "grant_invalid" || f.calls.Load() != 3 {
		t.Fatalf("the used grant ran again: %s", textOf(res))
	}
	evs, err := f.aud.Recent(context.Background(), 500)
	if err != nil {
		t.Fatal(err)
	}
	granted := 0
	for _, e := range evs {
		if e.ToolName == "deploy_run" {
			t.Errorf("audit row %s names deploy_run", e.EventType)
		}
		if e.ToolName == "deploy.run" && e.Decision == "grant" {
			granted++
		}
	}
	if granted != 3 {
		t.Fatalf("%d granted rows for deploy.run, want 3", granted)
	}
	c := structured(t, f.call(t, f.agent, "inbox_check", map[string]any{"calls": []any{map[string]any{"tool": "deploy_run"}}}))
	first := c["calls"].([]any)[0].(map[string]any)
	if first["status"] != "restricted" || first["tool"] != "deploy.run" {
		t.Fatalf("inbox_check of deploy_run = %v", first)
	}
	ex := structured(t, f.call(t, f.agent, "policies_explain", map[string]any{"tool": "deploy_run"}))
	if ex["tool"] != "deploy.run" {
		t.Fatalf("policies_explain of deploy_run = %v", ex)
	}
}

// TestJobsStartGuardsHoldForPublishedNames: jobs.start refuses a jobs.*
// tool by its published name as it does by its canonical name.
func TestJobsStartGuardsHoldForPublishedNames(t *testing.T) {
	f := newJobsFixture(t)
	for _, target := range []string{MetaJobsStatus, PublishedToolName(MetaJobsStatus)} {
		res := f.call(t, "jobs_start", map[string]any{"tool": target, "arguments": map[string]any{}})
		if !res.IsError || !strings.Contains(textOf(res), "cannot run as a job") {
			t.Errorf("jobs_start of %s: %s", target, textOf(res))
		}
	}
}

// TestSafeToolNamesOnMCPTasks: through MCPTasks the list is published
// (and still marked for tasks, the jobs.* tools still not), a
// task-augmented call by the published name runs the canonical tool as a
// job, and a plain call by the published name keeps its exact JSON
// numbers.
func TestSafeToolNamesOnMCPTasks(t *testing.T) {
	f := newJobsFixture(t, func(c *jobsFixtureConfig) { c.safeNames = true })
	c := newTasksServer(t, f, "ag_1", true)
	c.initialize()
	tools := c.result("tools/list", map[string]any{})
	marked := map[string]string{}
	list, _ := tools["tools"].([]any)
	for _, raw := range list {
		tool, _ := raw.(map[string]any)
		name, _ := tool["name"].(string)
		if !openAIToolName.MatchString(name) {
			t.Errorf("listed name %q breaks the rule", name)
		}
		marked[name] = ""
		if ex, ok := tool["execution"].(map[string]any); ok {
			marked[name], _ = ex["taskSupport"].(string)
		}
	}
	jobsStatus := PublishedToolName(MetaJobsStatus)
	if _, listed := marked[jobsStatus]; !listed || marked[jobsStatus] != "" || marked["alpha_get_fast"] != "optional" {
		t.Fatalf("taskSupport marks = %v", marked)
	}

	created := c.result("tools/call", map[string]any{
		"name": "alpha_get_fast", "arguments": map[string]any{ReasonField: jobsTestReason}, "task": map[string]any{"ttl": 60000},
	})
	task, _ := created["task"].(map[string]any)
	taskID, _ := task["taskId"].(string)
	if !strings.HasPrefix(taskID, "job_") {
		t.Fatalf("tools/call alpha_get_fast with a task = %v", created)
	}
	res := c.result("tasks/result", map[string]any{"taskId": taskID})
	if !strings.Contains(asJSON(res["content"]), "fast") || res["isError"] == true {
		t.Fatalf("tasks/result = %v", res)
	}
	if st := structured(t, f.call(t, MetaJobsStatus, map[string]any{"job_id": taskID})); st["tool"] != "alpha.get_fast" || st["via"] != jobViaTask {
		t.Fatalf("the task's job = %v", st)
	}
	if out := c.send("tools/call", map[string]any{"name": "alpha_no_such_tool", "arguments": map[string]any{}, "task": map[string]any{}}, false); !strings.Contains(asJSON(out["error"]), "alpha_no_such_tool") {
		t.Fatalf("task call of an unknown name = %v", out)
	}

	plain := c.result("tools/call", map[string]any{
		"name": "alpha_get_echo", "arguments": map[string]any{ReasonField: jobsTestReason, "n": json.Number("9007199254740993")},
	})
	if !strings.Contains(asJSON(plain["content"]), "echo") {
		t.Fatalf("plain tools/call alpha_get_echo = %v", plain)
	}
	select {
	case args := <-f.echoArgs:
		// A float64 would print 9.007199254740992e+15.
		if got := fmt.Sprint(args["n"]); got != "9007199254740993" {
			t.Fatalf("alpha.get_echo got n = %#v, want the exact number", args["n"])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("alpha.get_echo did not run")
	}
}

// TestSafeToolNamesTexts: with safe names on, the instructions, the
// skills paragraph and the descriptions of the five listed tools name the
// MCP tools by their published names, say once that the dotted names work,
// and keep the code-mode names; search results carry mcp_name.
func TestSafeToolNamesTexts(t *testing.T) {
	text := buildInstructions(nil, time.Second, true)
	for _, n := range SmallToolList {
		if !strings.Contains(text, PublishedToolName(n)) {
			t.Errorf("the instructions do not name %s", PublishedToolName(n))
		}
	}
	for _, want := range []string{
		"NAMES:", "the dotted names work as well", "tools_search is tools.search",
		"inbox_request is the one way to get permission", "ONE inbox_request", "inbox_wait remains optional",
		"(mcp_name)", "toolyard.inbox_guide", "toolyard.inbox_ask", "tools.tryCall", "server.tool",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("instructions missing %q", want)
		}
	}
	if strings.Count(text, "the dotted names work as well") != 1 {
		t.Errorf("the instructions should say once that the dotted names work")
	}
	for _, stale := range []string{"inbox.request is the one way", "call tools.search", "Read inbox.status", "(inbox.guide,"} {
		if strings.Contains(text, stale) {
			t.Errorf("instructions still name a dotted MCP tool: %q", stale)
		}
	}
	if s := skillsInstructions(true); !strings.Contains(s, "call tools_search") || !strings.Contains(s, "skills_search searches") || !strings.Contains(s, "toolyard.skills_get") {
		t.Errorf("skills paragraph: %q", s)
	}
	if g := skillsGuideInstructions("toolyard/toolyard-basics", true); !strings.Contains(g, "(skills_get {") || !strings.Contains(g, "toolyard.skills_get(") {
		t.Errorf("guide sentence: %q", g)
	}

	f := newToolListFixture(t, withSafeToolNames)
	ctx := WithAgentID(context.Background(), "ag_small")
	descs := mcpToolDescriptions(t, f.gw, ctx)
	checks := map[string][]string{
		"tools_search":    {"ask with inbox_request", "Use it before tools_execute", "mcp_name", "await tools.search(", "toolyard.tools_search("},
		"getToolDocs":     {"find the tool with tools_search", "A tools_search result's code"},
		"executeToolCode": {"find the tool with tools_search", "ONCE with inbox_request", "decision with inbox_status", "await tools.search(", "tools.tryCall("},
		"inbox_request":   {"inbox_request is the one way", "read inbox_status", "inbox_wait is optional", "inbox_guide for the format"},
		"inbox_status":    {"your inbox_request items", "one new inbox_request"},
	}
	for tool, wants := range checks {
		d, ok := descs[tool]
		if !ok {
			t.Errorf("%s is not listed: %v", tool, descs)
			continue
		}
		for _, w := range wants {
			if !strings.Contains(d, w) {
				t.Errorf("%s description lacks %q: %q", tool, w, d)
			}
		}
	}

	raw := f.gw.MCPServer().HandleMessage(ctx, json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"names-test","version":"1"}}}`))
	b, _ := json.Marshal(raw)
	var init struct {
		Result struct {
			Instructions string `json:"instructions"`
		} `json:"result"`
	}
	if err := json.Unmarshal(b, &init); err != nil || !strings.Contains(init.Result.Instructions, "inbox_request is the one way") {
		t.Errorf("initialize instructions: %v %s", err, b)
	}

	res, err := f.gw.SearchTools(WithAgentID(context.Background(), "ag_full"), SearchRequest{Query: "alpha status", Mode: "keyword"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range res["results"].([]toolsearch.Entry) {
		if !openAIToolName.MatchString(e.MCPName) || e.MCPName != f.gw.toolNames().publish(e.Name) {
			t.Errorf("search result %s has mcp_name %q", e.Name, e.MCPName)
		}
		if e.Name == "alpha.get_status" && e.MCPName == "alpha_get_status" {
			found = true
		}
	}
	if !found {
		t.Errorf("search for alpha status: %v", res["results"])
	}
}

// registerNamedTool adds a read tool under upstream alpha that answers
// "ran <its canonical name>".
func registerNamedTool(g *Gateway, name string) {
	g.registerEntry(toolEntry{
		tool:     mcp.Tool{Name: name, InputSchema: mcp.ToolInputSchema{Type: "object", Properties: addMetaProps(map[string]any{})}},
		upstream: "alpha", originalName: strings.TrimPrefix(name, "alpha."), reasonField: ReasonField,
		handle: func(context.Context, map[string]any) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText("ran " + name), nil
		},
	})
}

// TestPublishedNamesStayUniqueAfterHashing (review r1-1): alpha.get.state
// and alpha.get_state collide, and the hashed form of alpha.get.state is
// alpha_get_state_e1109bd8, which alpha.get_state_e1109bd8 holds as its
// short form. Uniqueness is checked after hashing: alpha.get.state takes
// 16 hex digits, whatever the order, and each published name runs its own
// tool.
func TestPublishedNamesStayUniqueAfterHashing(t *testing.T) {
	names := []string{"alpha.get.state", "alpha.get_state", "alpha.get_state_e1109bd8"}
	want := map[string]string{
		"alpha.get_state_e1109bd8": "alpha_get_state_e1109bd8",
		"alpha.get.state":          "alpha_get_state_e1109bd894bffc5d",
		"alpha.get_state":          "alpha_get_state_5ba89d2d",
	}
	for _, order := range [][]string{names, {names[2], names[1], names[0]}, {names[1], names[2], names[0]}} {
		if got := publishedToolNames(order); !reflect.DeepEqual(got, want) {
			t.Fatalf("publishedToolNames(%v) = %v, want %v", order, got, want)
		}
	}
	f := newToolListFixture(t, withSafeToolNames)
	for _, n := range names {
		registerNamedTool(f.gw, n)
	}
	// "get.state" is no read verb, so it would need approval: allow it, so
	// every call answers with the tool that ran.
	if _, err := f.gw.policy.Set(context.Background(), policy.ScopeTool, "alpha.get.state", "allow", "", false); err != nil {
		t.Fatal(err)
	}
	ctx := WithAgentID(context.Background(), "ag_full")
	full := mcpToolsList(t, f.gw, ctx)
	for canonical, published := range want {
		if !has(full, published) {
			t.Errorf("%s is not listed as %s: %v", canonical, published, full)
		}
		if got := f.gw.resolveToolName(published); got != canonical {
			t.Errorf("%s resolves to %s, want %s", published, got, canonical)
		}
		if r := mcpToolsCall(t, f.gw, ctx, published); r.rpcErr || r.isError || r.text != "ran "+canonical {
			t.Errorf("tools/call %s: %s", published, r.raw)
		}
	}
}

// TestPublishedToolNamesProperties: over random catalogs, with names made
// to collide and names that are other names' hashed forms, every name
// gets a valid published name, no two names share one, a name that
// already follows the rule keeps it, and the answer does not depend on the
// order of the input.
func TestPublishedToolNamesProperties(t *testing.T) {
	rng := rand.New(rand.NewSource(1807))
	alphabet := []byte("ab_.-/9")
	for round := 0; round < 500; round++ {
		var names []string
		for i, n := 0, 1+rng.Intn(12); i < n; i++ {
			l := 1 + rng.Intn(6)
			if rng.Intn(8) == 0 {
				l = 56 + rng.Intn(24)
			}
			b := make([]byte, l)
			for j := range b {
				b[j] = alphabet[rng.Intn(len(alphabet))]
			}
			names = append(names, string(b))
		}
		for _, n := range append([]string(nil), names...) {
			if rng.Intn(3) != 0 {
				continue
			}
			s := sanitizeToolName(n)
			names = append(names, hashedCandidate(n, s, 0), strings.Replace(hashedCandidate(n, s, 0), "_", ".", 1))
			if rng.Intn(2) == 0 {
				names = append(names, strings.Replace(hashedCandidate(n, s, 1), "_", "/", 1))
			}
		}
		got := publishedToolNames(names)
		owner := map[string]string{}
		for _, n := range names {
			p, ok := got[n]
			if !ok {
				t.Fatalf("round %d: %q has no published name", round, n)
			}
			if !openAIToolName.MatchString(p) {
				t.Fatalf("round %d: %q -> %q breaks the rule", round, n, p)
			}
			if ValidPublishedToolName(n) && p != n {
				t.Fatalf("round %d: the valid name %q was renamed %q", round, n, p)
			}
			if prev, dup := owner[p]; dup && prev != n {
				t.Fatalf("round %d: %q and %q both publish as %q (%v)", round, prev, n, p, names)
			}
			owner[p] = n
		}
		shuffled := append([]string(nil), names...)
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		if again := publishedToolNames(shuffled); !reflect.DeepEqual(again, got) {
			t.Fatalf("round %d: the order changed the names: %v, then %v", round, got, again)
		}
	}
}

// TestAliasesNeverRevealHiddenTools (review r1-2): beta.run exists but the
// member was never granted beta; beta.nope exists nowhere. Under their
// published names both read as unknown everywhere, answered with the name
// the member sent: inbox.check, inbox.request validation, policies.explain,
// a direct call, tools.execute and a raw tools/call. A tool the member may
// use resolves as usual, and the refused calls are audited under the
// canonical name.
func TestAliasesNeverRevealHiddenTools(t *testing.T) {
	f := newAccessFixture(t, nil)
	hidden := []string{"beta_run", "beta_nope"}
	noLeak := func(what, text string) {
		t.Helper()
		if strings.Contains(text, "beta.run") || strings.Contains(text, "beta.nope") {
			t.Errorf("%s names the canonical tool: %s", what, text)
		}
	}

	c := structured(t, f.call(t, f.member, "inbox_check", map[string]any{"calls": []any{
		map[string]any{"tool": "beta_run"}, map[string]any{"tool": "beta_nope"}, map[string]any{"tool": "alpha_run"},
	}}))
	calls := c["calls"].([]any)
	for i, w := range []struct{ tool, status string }{{"beta_run", "unknown"}, {"beta_nope", "unknown"}, {"alpha.run", "restricted"}} {
		got := calls[i].(map[string]any)
		if got["tool"] != w.tool || got["status"] != w.status {
			t.Errorf("inbox_check %d = %v, want tool %s status %s", i, got, w.tool, w.status)
		}
	}
	noLeak("inbox_check", asJSON(c))

	for _, name := range hidden {
		args := requestArgs(map[string]any{"service": "api", "env": "prod"})
		args["tools"].([]any)[0].(map[string]any)["tool"] = name
		args["dry_run"] = true
		res := f.call(t, f.member, "inbox_request", args)
		body := asJSON(structured(t, res)) + textOf(res)
		if !strings.Contains(body, name) || !strings.Contains(body, "isn't a toolyard tool") {
			t.Errorf("inbox_request naming %s: %s", name, body)
		}
		noLeak("inbox_request naming "+name, body)

		res = f.call(t, f.member, "policies_explain", map[string]any{"tool": name})
		if !res.IsError || textOf(res) != notFoundText(name) {
			t.Errorf("policies_explain %s: %s", name, textOf(res))
		}
		noLeak("policies_explain "+name, textOf(res)+asJSON(res.StructuredContent))

		res = f.call(t, f.member, name, nil)
		if !res.IsError || textOf(res) != notFoundText(name) {
			t.Errorf("direct call %s: %s", name, textOf(res))
		}
		noLeak("direct call "+name, textOf(res))

		res = f.call(t, f.member, "tools_execute", map[string]any{"tool": name, "arguments": map[string]any{}})
		if !res.IsError || !strings.Contains(textOf(res), notFoundText(name)) {
			t.Errorf("tools_execute %s: %s", name, textOf(res))
		}
		noLeak("tools_execute "+name, textOf(res))

		if r := mcpToolsCall(t, f.gw, f.member, name); !r.rpcErr || r.message != rpcNotFoundMessage(name) {
			t.Errorf("raw tools/call %s: %s", name, r.raw)
		}
	}
	if n := f.calls.Load(); n != 0 {
		t.Fatalf("a hidden tool ran %d times", n)
	}
	if ex := structured(t, f.call(t, f.member, "policies_explain", map[string]any{"tool": "alpha_run"})); ex["tool"] != "alpha.run" {
		t.Errorf("policies_explain of a granted tool's published name = %v", ex)
	}
	evs, err := f.aud.Recent(context.Background(), 500)
	if err != nil {
		t.Fatal(err)
	}
	denied := 0
	for _, e := range evs {
		if e.ToolName == "beta_run" {
			t.Errorf("audit row %s names the published name", e.EventType)
		}
		if e.ToolName == "beta.run" && e.EventType == audit.EventCallDenied {
			denied++
		}
	}
	if denied < 3 {
		t.Errorf("%d call.denied rows for beta.run, want one per refused call (3)", denied)
	}
}

// headerTool registers alpha.get_region, whose region parameter travels
// in the Mcp-Param-Region header (x-mcp-header), and counts its runs.
func headerTool(g *Gateway) *atomic.Int32 {
	var ran atomic.Int32
	g.registerEntry(toolEntry{
		tool: mcp.Tool{Name: "alpha.get_region", InputSchema: mcp.ToolInputSchema{Type: "object", Properties: addMetaProps(map[string]any{
			"region": map[string]any{"type": "string", "x-mcp-header": "Region"},
		})}},
		upstream: "alpha", originalName: "get_region", reasonField: ReasonField,
		handle: func(context.Context, map[string]any) (*mcp.CallToolResult, error) {
			ran.Add(1)
			return mcp.NewToolResultText("ran"), nil
		},
	})
	return &ran
}

// modernMeta is a 2026-07-28 request's _meta.
func modernMeta() map[string]any {
	return map[string]any{
		mcp.MetaKeyProtocolVersion:    mcp.ProtocolVersion20260728,
		mcp.MetaKeyClientCapabilities: map[string]any{},
		mcp.MetaKeyClientInfo:         map[string]any{"name": "header-test", "version": "1"},
	}
}

// TestParamHeadersCheckedUnderEitherName (review r1-3): a 2026-07-28
// tools/call is held to its Mcp-Param-* headers whether it names the tool
// by its published or its canonical name, with and without
// PreserveToolCallNumbers in front: a matching header runs, a missing or
// conflicting one is refused with HEADER_MISMATCH (HTTP 400) and nothing
// runs. Mcp-Name is still checked against the name sent.
func TestParamHeadersCheckedUnderEitherName(t *testing.T) {
	f := newToolListFixture(t, withSafeToolNames)
	ran := headerTool(f.gw)
	streamable := server.NewStreamableHTTPServer(f.gw.MCPServer(), server.WithStateLess(true),
		server.WithHTTPContextFunc(func(ctx context.Context, _ *http.Request) context.Context { return WithAgentID(ctx, "ag_full") }))
	plain := httptest.NewServer(streamable)
	t.Cleanup(plain.Close)
	preserved := httptest.NewServer(PreserveToolCallNumbers(streamable))
	t.Cleanup(preserved.Close)

	call := func(url, name, nameHeader string, extra map[string]string) (int, map[string]any) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 7, "method": "tools/call", "params": map[string]any{
			"name": name, "arguments": map[string]any{ReasonField: testReason, "region": "eu-west"}, "_meta": modernMeta(),
		}})
		enc, _ := mcp.EncodeHeaderValue(nameHeader)
		h := map[string]string{mcp.HeaderProtocolVersion: mcp.ProtocolVersion20260728, mcp.HeaderMethod: "tools/call", mcp.HeaderName: enc}
		for k, v := range extra {
			h[k] = v
		}
		return postRaw(t, url, h, string(body))
	}
	mismatch := func(status int, out map[string]any) bool {
		e, _ := out["error"].(map[string]any)
		return status == http.StatusBadRequest && e != nil && e["code"] == float64(mcp.HEADER_MISMATCH)
	}
	want := int32(0)
	for _, url := range []string{plain.URL, preserved.URL} {
		for _, name := range []string{"alpha_get_region", "alpha.get_region"} {
			status, out := call(url, name, name, map[string]string{"Mcp-Param-Region": "eu-west"})
			res, _ := out["result"].(map[string]any)
			if status != http.StatusOK || res == nil || res["isError"] == true {
				t.Fatalf("%s with a matching header: %d %v", name, status, out)
			}
			want++
			if status, out := call(url, name, name, nil); !mismatch(status, out) {
				t.Errorf("%s without its Mcp-Param-Region header: %d %v", name, status, out)
			}
			if status, out := call(url, name, name, map[string]string{"Mcp-Param-Region": "us-east"}); !mismatch(status, out) {
				t.Errorf("%s with a conflicting Mcp-Param-Region header: %d %v", name, status, out)
			}
		}
		if status, out := call(url, "alpha_get_region", "alpha.get_region", map[string]string{"Mcp-Param-Region": "eu-west"}); !mismatch(status, out) {
			t.Errorf("Mcp-Name naming the canonical tool for a published-name body: %d %v", status, out)
		}
	}
	if got := ran.Load(); got != want {
		t.Fatalf("the tool ran %d times, want %d (only with a matching header)", got, want)
	}
}

// TestParamHeadersCheckedOnMCPTasks: a 2026-07-28 tools/call with a task
// (answered by MCPTasks, which mcp-go never sees) is held to the same
// headers under either name.
func TestParamHeadersCheckedOnMCPTasks(t *testing.T) {
	f := newJobsFixture(t, func(c *jobsFixtureConfig) { c.safeNames = true })
	headerTool(f.gw)
	c := newTasksServer(t, f, "ag_1", true)
	for _, name := range []string{"alpha_get_region", "alpha.get_region"} {
		params := map[string]any{"name": name, "arguments": map[string]any{ReasonField: jobsTestReason, "region": "eu-west"},
			"task": map[string]any{}, "_meta": modernMeta()}
		enc, _ := mcp.EncodeHeaderValue(name)
		base := http.Header{mcp.HeaderProtocolVersion: {mcp.ProtocolVersion20260728}, mcp.HeaderMethod: {"tools/call"}, mcp.HeaderName: {enc}}
		with := func(region string) http.Header {
			h := base.Clone()
			if region != "" {
				h.Set("Mcp-Param-Region", region)
			}
			return h
		}
		c.header = with("eu-west")
		created := c.send("tools/call", params, false)
		res, _ := created["result"].(map[string]any)
		if task, _ := res["task"].(map[string]any); c.status != http.StatusOK || task == nil {
			t.Fatalf("%s task with a matching header: %d %v", name, c.status, created)
		}
		for _, region := range []string{"", "us-east"} {
			c.header = with(region)
			out := c.send("tools/call", params, false)
			e, _ := out["error"].(map[string]any)
			if c.status != http.StatusBadRequest || e == nil || e["code"] != float64(mcp.HEADER_MISMATCH) {
				t.Errorf("%s task with Mcp-Param-Region %q: %d %v", name, region, c.status, out)
			}
		}
	}
}
