//go:build e2e

// scripts/e2e_inbox_test.go drives the inbox flow against a running
// gateway with a real MCP client:
//
//	coaching → guide → check → session → dry run → request → owner
//	approves → agent waits → grant redeemed once → update closes the loop
//
// Run the gateway, then:
//
//	go test -tags=e2e ./scripts -run TestE2EInbox -toolyard=http://localhost:18787 -v
//
// With -media=http://host:port/ pointing at a directory holding shot.jpg
// and rec.mp4, the test also checks that linked media are copied (start
// the gateway with -inbox-fetch-private-networks if that host is local).
package scripts

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

var mediaBase = flag.String("media", "", "base URL serving shot.jpg and rec.mp4 for attachment tests (optional)")

func callTool(t *testing.T, c mcpClientLike, ctx context.Context, name string, args map[string]any) (*mcp.CallToolResult, map[string]any) {
	t.Helper()
	if _, ok := args["_reason"]; !ok {
		args["_reason"] = "end-to-end test of the inbox permission flow"
	}
	req := mcp.CallToolRequest{}
	req.Params.Name = name
	req.Params.Arguments = args
	res, err := c.CallTool(ctx, req)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	sc, _ := res.StructuredContent.(map[string]any)
	return res, sc
}

func TestE2EInbox(t *testing.T) {
	flag.Parse()
	h := &httpClient{base: *toolyardURL}
	mustLogin(t, h)
	var dummy map[string]any
	h.raw(t, "PATCH", "/v1/settings", map[string]any{"approval_mode": "inbox"}, &dummy)
	t.Cleanup(func() { h.raw(t, "PATCH", "/v1/settings", map[string]any{"approval_mode": "execute"}, &dummy) })

	token := enrollAgent(t, h, "inbox-e2e")
	c := mcpClient(t, *toolyardURL, token)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// The tools and the guide resource are visible.
	tools, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, tl := range tools.Tools {
		have[tl.Name] = true
	}
	for _, n := range []string{"inbox.guide", "inbox.check", "inbox.request", "inbox.wait", "session.start"} {
		if !have[n] {
			t.Fatalf("%s missing from tools/list", n)
		}
	}
	rr := mcp.ReadResourceRequest{}
	rr.Params.URI = "toolyard://guide"
	if res, err := c.ReadResource(ctx, rr); err != nil || len(res.Contents) == 0 {
		t.Fatalf("guide resource: %v", err)
	}

	key := "inbox-e2e-" + time.Now().Format("150405")

	// 1. A restricted call is coached, not queued.
	res, sc := callTool(t, c, ctx, "memory.set", map[string]any{"key": key, "value": "shipped"})
	if !res.IsError || sc["status"] != "permission_required" {
		t.Fatalf("want permission_required, got %v", sc)
	}
	draft := sc["draft"].(map[string]any)

	// 2. Guide and check.
	if res, _ := callTool(t, c, ctx, "inbox.guide", map[string]any{"topic": "requests"}); res.IsError {
		t.Fatal("inbox.guide failed")
	}
	_, chk := callTool(t, c, ctx, "inbox.check", map[string]any{"calls": []any{
		map[string]any{"tool": "memory.set"}, map[string]any{"tool": "memory.get"},
	}})
	calls := chk["calls"].([]any)
	if calls[0].(map[string]any)["status"] != "restricted" || calls[1].(map[string]any)["status"] != "open" {
		t.Fatalf("check: %v", calls)
	}

	// 3. Session.
	_, ses := callTool(t, c, ctx, "session.start", map[string]any{"title": "Inbox end-to-end test", "repo": "acme/toolyard", "host": "ci"})
	sessionID, _ := ses["id"].(string)
	if sessionID == "" {
		t.Fatalf("session.start: %v", ses)
	}

	// 4. Fill the draft the coaching result gave us.
	draft["title"] = "Record the release note in memory"
	draft["summary"] = "One memory write so other agents see the release."
	draft["message"] = "The release is out, so I'd like to record it in shared memory where the other agents look."
	draft["facts"] = map[string]any{"why_now": "Other agents are asking.", "if_it_goes_wrong": "One memory key is wrong.", "undo": "Delete the key."}
	draft["audio"] = map[string]any{"script": "I'd like to write one note to shared memory saying the release is out. It's a single key and easy to undo."}
	draft["session_id"] = sessionID
	tl := draft["tools"].([]any)[0].(map[string]any)
	tl["summary"] = "Write the release note to one memory key."
	atts := []any{
		map[string]any{"type": "table", "title": "Checks", "columns": []any{"Check", "Result"}, "rows": []any{[]any{"Unit tests", "passed"}}},
		map[string]any{"type": "chart", "chart": "line", "title": "Errors", "unit": "%", "x": []any{"-10m", "now"},
			"series": []any{map[string]any{"name": "errors", "values": []any{0.2, 0.2}}}},
		map[string]any{"type": "diff", "file": "notes.md", "patch": "@@ -1 +1 @@\n-old\n+new\n", "caption": "The note I'll write."},
		map[string]any{"type": "link", "label": "Release", "url": "https://example.com/release"},
	}
	if *mediaBase != "" {
		b := strings.TrimRight(*mediaBase, "/")
		atts = append(atts,
			map[string]any{"type": "image", "url": b + "/shot.jpg", "alt": "dashboard", "caption": "It's live."},
			map[string]any{"type": "video", "url": b + "/rec.mp4", "caption": "Recorded in staging."})
	}
	draft["attachments"] = atts

	draft["dry_run"] = true
	_, dry := callTool(t, c, ctx, "inbox.request", draft)
	if dry["ok"] != true || dry["request_id"] != nil {
		t.Fatalf("dry run: %v", dry)
	}
	delete(draft, "dry_run")
	_, sent := callTool(t, c, ctx, "inbox.request", draft)
	reqID, _ := sent["request_id"].(string)
	if sent["ok"] != true || reqID == "" {
		t.Fatalf("request: %v", sent)
	}

	// 5. Owner sees it, checked, with its dry run counted.
	var one map[string]any
	deadline := time.Now().Add(30 * time.Second)
	for {
		h.raw(t, "GET", "/v1/inbox/"+reqID, nil, &one)
		if one["request"].(map[string]any)["checked"] == true || time.Now().After(deadline) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	req := one["request"].(map[string]any)
	if req["checked"] != true || req["dry_run_count"].(float64) != 1 || req["agent_name"] != "inbox-e2e" || req["session_title"] != "Inbox end-to-end test" {
		t.Fatalf("owner view: checked=%v dry_runs=%v agent=%v session=%v", req["checked"], req["dry_run_count"], req["agent_name"], req["session_title"])
	}
	if *mediaBase != "" {
		for _, a := range req["attachments"].([]any) {
			am := a.(map[string]any)
			if am["type"] != "image" && am["type"] != "video" {
				continue
			}
			blob, _ := am["blob"].(string)
			if blob == "" {
				t.Fatalf("%s wasn't copied: %v", am["type"], am["fetch_error"])
			}
			hr, _ := http.NewRequest("GET", *toolyardURL+"/v1/inbox/blobs/"+blob, nil)
			for _, ck := range h.cookies {
				hr.AddCookie(ck)
			}
			resp, err := http.DefaultClient.Do(hr)
			if err != nil || resp.StatusCode != 200 {
				t.Fatalf("blob %s: %v %v", blob, err, resp)
			}
			resp.Body.Close()
		}
	}

	// 6. Approve, then the agent collects its grant.
	h.raw(t, "POST", "/v1/inbox/"+reqID+"/decide", map[string]any{"action": "approve", "allow": []bool{true}, "note": "fine"}, &dummy)
	_, waited := callTool(t, c, ctx, "inbox.wait", map[string]any{"ids": []any{reqID}, "timeout_seconds": 5})
	view := waited["requests"].([]any)[0].(map[string]any)
	grant, _ := view["tools"].([]any)[0].(map[string]any)["grant"].(string)
	if view["status"] != "approved" || view["owner_note"] != "fine" || !strings.HasPrefix(grant, "tyg_") {
		t.Fatalf("wait: %v", view)
	}

	// 7. The grant works once, within its parameters.
	if res, sc := callTool(t, c, ctx, "memory.set", map[string]any{"key": key, "value": "tampered", "_grant": grant}); !res.IsError || sc["status"] != "grant_invalid" {
		t.Fatalf("out-of-scope call should be refused: %v", sc)
	}
	if res, sc := callTool(t, c, ctx, "memory.set", map[string]any{"key": key, "value": "shipped", "_grant": grant}); res.IsError {
		t.Fatalf("granted call failed: %v", sc)
	}
	if res, sc := callTool(t, c, ctx, "memory.set", map[string]any{"key": key, "value": "shipped", "_grant": grant}); !res.IsError || sc["status"] != "grant_invalid" {
		t.Fatalf("second use should be refused: %v", sc)
	}
	res, _ = callTool(t, c, ctx, "memory.get", map[string]any{"key": key})
	if res.IsError || !strings.Contains(textContent(res), "shipped") {
		t.Fatalf("memory.get after the granted write: %s", textContent(res))
	}

	// 8. Close the loop.
	_, post := callTool(t, c, ctx, "inbox.post", map[string]any{
		"request_id": reqID, "title": "Release note recorded", "summary": "Written to shared memory.",
		"message": "Done. The note is in shared memory.", "audio": map[string]any{"script": "All done. The release note is in shared memory."}, "urgency": "fyi",
	})
	if post["ok"] != true {
		t.Fatalf("inbox.post: %v", post)
	}
	var sessions map[string]any
	h.raw(t, "GET", "/v1/inbox/sessions", nil, &sessions)
	found := false
	for _, s := range sessions["sessions"].([]any) {
		if s.(map[string]any)["id"] == sessionID {
			found = true
		}
	}
	if !found {
		t.Fatal("session missing from /v1/inbox/sessions")
	}
}

func textContent(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := mcp.AsTextContent(c); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// TestE2EInboxNarrowAndAttention: the owner narrows a request before
// approving (the grant then only covers the narrowed values and the
// shorter TTL), `now` is rate-limited per agent, and updates can be
// batch-read.
func TestE2EInboxNarrowAndAttention(t *testing.T) {
	flag.Parse()
	h := &httpClient{base: *toolyardURL}
	mustLogin(t, h)
	var dummy map[string]any
	h.raw(t, "PATCH", "/v1/settings", map[string]any{"approval_mode": "inbox", "inbox_now_per_hour": 1}, &dummy)
	t.Cleanup(func() {
		h.raw(t, "PATCH", "/v1/settings", map[string]any{"approval_mode": "execute", "inbox_now_per_hour": 3}, &dummy)
	})
	token := enrollAgent(t, h, "inbox-narrow-e2e")
	c := mcpClient(t, *toolyardURL, token)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	stamp := time.Now().Format("150405.000")
	k1, k2 := "narrow-a-"+stamp, "narrow-b-"+stamp
	_, sent := callTool(t, c, ctx, "inbox.request", map[string]any{
		"title": "Write one of two cache keys", "summary": "One memory write.",
		"message": "I need to write a cache key; I don't know yet which of the two.",
		"facts":   map[string]any{"why_now": "The cache is cold.", "if_it_goes_wrong": "One key is wrong.", "undo": "Delete it."},
		"audio":   map[string]any{"script": "I'd like to write one cache key."}, "urgency": "soon", "ttl_seconds": 600,
		"tools": []any{map[string]any{"tool": "memory.set", "required": true, "summary": "Write the key.",
			"params": map[string]any{"key": map[string]any{"in": []any{k1, k2}}, "value": map[string]any{"any": true}}}},
	})
	reqID, _ := sent["request_id"].(string)
	if sent["ok"] != true {
		t.Fatalf("request: %v", sent)
	}

	// Widening is refused; narrowing works.
	var out map[string]any
	code := h.rawStatus(t, "POST", "/v1/inbox/"+reqID+"/decide", map[string]any{"action": "approve", "allow": []bool{true},
		"params": map[string]any{"0": map[string]any{"key": "other"}}}, &out)
	if code != http.StatusBadRequest {
		t.Fatalf("widening should be refused, got %d %v", code, out)
	}
	code = h.rawStatus(t, "POST", "/v1/inbox/"+reqID+"/decide", map[string]any{"action": "approve", "allow": []bool{true}, "ttl_seconds": 120,
		"params": map[string]any{"0": map[string]any{"key": k1, "value": "warm"}}}, &out)
	if code != http.StatusOK {
		t.Fatalf("narrowed approve: %d %v", code, out)
	}
	_, waited := callTool(t, c, ctx, "inbox.wait", map[string]any{"ids": []any{reqID}, "timeout_seconds": 5})
	view := waited["requests"].([]any)[0].(map[string]any)
	tv := view["tools"].([]any)[0].(map[string]any)
	grant, _ := tv["grant"].(string)
	if tv["narrowed"] != true || tv["params"] == nil || grant == "" {
		t.Fatalf("agent view of the narrowed tool: %v", tv)
	}
	if exp, cr := view["grants_expire_at"].(float64), time.Now().Add(125*time.Second).UnixMilli(); int64(exp) > cr {
		t.Fatalf("grants outlive the shortened TTL: %v", view)
	}
	if res, sc := callTool(t, c, ctx, "memory.set", map[string]any{"key": k2, "value": "warm", "_grant": grant}); !res.IsError || sc["status"] != "grant_invalid" {
		t.Fatalf("a value the owner narrowed away was accepted: %v", sc)
	}
	if res, sc := callTool(t, c, ctx, "memory.set", map[string]any{"key": k1, "value": "warm", "_grant": grant}); res.IsError {
		t.Fatalf("narrowed call failed: %v", sc)
	}

	// `now` budget of 1 per hour: the second is lowered and the agent told.
	ask := func(title string) map[string]any {
		_, r := callTool(t, c, ctx, "inbox.ask", map[string]any{"title": title, "summary": "s", "message": "m",
			"audio": map[string]any{"script": "Quick question."}, "urgency": "now",
			"options": []any{map[string]any{"label": "Yes"}, map[string]any{"label": "No"}}})
		return r
	}
	ask("First urgent question " + stamp)
	second := ask("Second urgent question " + stamp)
	warned := false
	for _, w := range asSlice(second["warnings"]) {
		if w.(map[string]any)["path"] == "urgency" {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("second now request wasn't lowered: %v", second)
	}
	var one map[string]any
	h.raw(t, "GET", "/v1/inbox/"+second["request_id"].(string), nil, &one)
	if r := one["request"].(map[string]any); r["urgency"] != "soon" || r["requested_urgency"] != "now" || r["downgraded"] == nil {
		t.Fatalf("owner view of the lowered request: %v", r)
	}

	// Updates can be batch-read; approving is never batched.
	_, up := callTool(t, c, ctx, "inbox.post", map[string]any{"title": "Cache warmed", "summary": "Done.", "message": "Done.",
		"audio": map[string]any{"script": "Done."}, "urgency": "fyi"})
	upID := up["request_id"].(string)
	if code := h.rawStatus(t, "POST", "/v1/inbox/batch", map[string]any{"ids": []any{upID}, "action": "approve"}, &out); code != http.StatusBadRequest {
		t.Fatalf("batch approve: %d", code)
	}
	if code := h.rawStatus(t, "POST", "/v1/inbox/batch", map[string]any{"ids": []any{upID}, "action": "read"}, &out); code != http.StatusOK || out["done"].(float64) != 1 {
		t.Fatalf("batch read: %d %v", code, out)
	}
	var info map[string]any
	h.raw(t, "GET", "/v1/inbox/info", nil, &info)
	if info["info"].(map[string]any)["next_digest"] == nil {
		t.Fatalf("info: %v", info)
	}
}

func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

// rawStatus is raw without failing on 4xx: it returns the status code and
// decodes the body (error or not) into dst.
func (h *httpClient) rawStatus(t *testing.T, method, path string, body any, dst any) int {
	t.Helper()
	b, _ := json.Marshal(body)
	req, err := http.NewRequest(method, h.base+path, bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Requested-With", "toolyard")
	for _, c := range h.cookies {
		req.AddCookie(c)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	all, _ := io.ReadAll(resp.Body)
	if dst != nil && len(all) > 0 {
		_ = json.Unmarshal(all, dst)
	}
	return resp.StatusCode
}
