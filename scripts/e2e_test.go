//go:build e2e

// scripts/e2e_test.go runs an end-to-end smoke against a live toolyard server.
//
// Usage:
//
//	go test -tags=e2e ./scripts -run TestEndToEnd -toolyard=http://localhost:18787
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

func (h *httpClient) do(t *testing.T, method, path string, body any) map[string]any {
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
	var out map[string]any
	all, _ := io.ReadAll(resp.Body)
	if len(all) == 0 {
		return nil
	}
	if err := json.Unmarshal(all, &out); err != nil {
		// Maybe an array; ignore for this helper.
		return map[string]any{"_raw": string(all)}
	}
	return out
}

func TestEndToEnd(t *testing.T) {
	flag.Parse()
	h := &httpClient{base: *toolyardURL}

	// Idempotent: try setup; if user already exists, just login.
	_, err := http.Post(h.base+"/v1/auth/setup", "application/json",
		strings.NewReader(`{"Username":"admin","Password":"correct horse battery staple"}`))
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	loginRes := h.do(t, "POST", "/v1/auth/login",
		map[string]string{"Username": "admin", "Password": "correct horse battery staple"})
	t.Logf("logged in as %v", loginRes["username"])

	// Enroll an agent.
	enr := h.do(t, "POST", "/v1/agents/enroll", map[string]string{"Name": "e2e"})
	code, _ := enr["enrollment_code"].(string)
	if code == "" {
		t.Fatal("no enrollment code")
	}
	xch := h.do(t, "POST", "/v1/agents/exchange", map[string]string{"Code": code})
	token, _ := xch["token"].(string)
	if token == "" {
		t.Fatal("no token")
	}
	t.Logf("enrolled, token = %s…", token[:min(16, len(token))])

	// Connect MCP streamable client.
	tr, err := transport.NewStreamableHTTP(*toolyardURL+"/mcp",
		transport.WithHTTPHeaders(map[string]string{"Authorization": "Bearer " + token}))
	if err != nil {
		t.Fatal(err)
	}
	c := client.NewClient(tr)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{Name: "toolyard-e2e", Version: "0.0.1"}
	if _, err := c.Initialize(ctx, initReq); err != nil {
		t.Fatal(err)
	}

	tools, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("tools: %d", len(tools.Tools))
	if len(tools.Tools) == 0 {
		t.Fatal("expected at least the built-in memory tools")
	}
	var foundReason bool
	for _, tl := range tools.Tools {
		_, hasReason := tl.InputSchema.Properties["_reason"]
		if hasReason {
			foundReason = true
		}
		if tl.Name == "memory.set" && !hasReason {
			t.Errorf("memory.set is missing _reason field")
		}
	}
	if !foundReason {
		t.Fatal("schema-wrap did not inject _reason on any tool")
	}

	// memory.get should pass without approval (read-only).
	getReq := mcp.CallToolRequest{}
	getReq.Params.Name = "memory.get"
	getReq.Params.Arguments = map[string]any{
		"_reason": "smoke test reading a missing key just to exercise the read path",
		"key":     "no-such-key",
	}
	getRes, err := c.CallTool(ctx, getReq)
	if err != nil {
		t.Fatalf("memory.get call: %v", err)
	}
	t.Logf("memory.get result isError=%v content=%s", getRes.IsError, dumpResult(getRes))

	// memory.set requires approval. Run in a goroutine and approve mid-flight.
	resultCh := make(chan *mcp.CallToolResult, 1)
	errCh := make(chan error, 1)
	go func() {
		setReq := mcp.CallToolRequest{}
		setReq.Params.Name = "memory.set"
		setReq.Params.Arguments = map[string]any{
			"_reason": "writing a value to demonstrate the approval flow end to end",
			"key":     "hello",
			"value":   "world",
		}
		res, err := c.CallTool(ctx, setReq)
		if err != nil {
			errCh <- err
			return
		}
		resultCh <- res
	}()

	// Poll for the pending approval and approve it.
	deadline := time.Now().Add(15 * time.Second)
	var apID string
	for time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
		list := h.do(t, "GET", "/v1/approvals?status=pending", nil)
		raw, _ := json.Marshal(list)
		t.Logf("pending list: %s", string(raw))
		// `list` is a JSON array but we decoded as map; switch to direct decode.
		req, err := http.NewRequest("GET", h.base+"/v1/approvals?status=pending", nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range h.cookies {
			req.AddCookie(c)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var arr []map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&arr)
		resp.Body.Close()
		if len(arr) > 0 {
			apID, _ = arr[0]["id"].(string)
			break
		}
	}
	if apID == "" {
		t.Fatal("no approval appeared")
	}
	t.Logf("approving %s", apID)
	h.do(t, "POST", "/v1/approvals/"+apID+"/decide", map[string]string{"Action": "allowed"})

	select {
	case res := <-resultCh:
		if res.IsError {
			t.Fatalf("memory.set returned error: %s", dumpResult(res))
		}
		t.Logf("memory.set OK: %s", dumpResult(res))
	case err := <-errCh:
		t.Fatalf("memory.set call: %v", err)
	case <-time.After(20 * time.Second):
		t.Fatal("memory.set never returned")
	}

	// Verify the value landed.
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
		t.Errorf("expected 'world' in result, got %s", dumpResult(getRes2))
	}

	// Audit log should have records.
	req, err := http.NewRequest("GET", h.base+"/v1/audit?limit=20", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range h.cookies {
		req.AddCookie(c)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var arr []map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&arr)
	resp.Body.Close()
	if len(arr) < 2 {
		t.Fatalf("expected multiple audit events, got %d", len(arr))
	}
	t.Logf("audit events: %d", len(arr))
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

func min(a, b int) int { if a < b { return a }; return b }

var _ = fmt.Sprint
