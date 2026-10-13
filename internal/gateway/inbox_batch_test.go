package gateway

import (
	"context"
	"errors"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/tusharbhardwaj/toolyard/internal/inbox"
	"testing"
	"time"
)

func TestInboxBatchCatalogValidationAndReadClaim(t *testing.T) {
	f := newInboxFixture(t)
	f.gw.registerEntry(toolEntry{tool: mcp.Tool{Name: "files.write", InputSchema: mcp.ToolInputSchema{Type: "object", Required: []string{"path", "content"}, Properties: map[string]any{"path": map[string]any{"type": "string"}, "content": map[string]any{"type": "string"}}}}, upstream: "files", handle: func(context.Context, map[string]any) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("ok"), nil
	}})
	args := requestArgs(map[string]any{"path": 4, "content": "hello"})
	call := args["tools"].([]any)[0].(map[string]any)
	call["tool"] = "files.write"
	call["operation"] = "read"
	res := f.call(t, f.agent, "inbox.request", args)
	body := structured(t, res)
	if body["ok"] == true {
		t.Fatal("invalid schema or write claiming read accepted")
	}
	problems := body["problems"].([]any)
	hasOperation, hasParams := false, false
	for _, p := range problems {
		path := p.(map[string]any)["path"]
		hasOperation = hasOperation || path == "tools[0].operation"
		hasParams = hasParams || path == "tools[0].params"
	}
	if !hasOperation || !hasParams {
		t.Fatalf("field-specific errors: %+v", problems)
	}
}

func TestInboxBatchDispatchTimeoutPersistsUnknown(t *testing.T) {
	f := newInboxFixture(t)
	f.gw.mu.Lock()
	entry := f.gw.tools["deploy.run"]
	entry.handle = func(ctx context.Context, _ map[string]any) (*mcp.CallToolResult, error) {
		f.calls.Add(1)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	f.gw.tools["deploy.run"] = entry
	f.gw.mu.Unlock()
	token := f.approvedGrant(t)
	f.gw.upstreamCallTimeout = 10 * time.Millisecond
	result := f.call(t, f.agent, "deploy.run", map[string]any{"service": "api", "env": "prod", GrantField: token})
	if !result.IsError {
		t.Fatal("timeout expected")
	}
	requests, err := f.svc.List(context.Background(), inbox.ListFilter{AgentID: "ag_1"})
	if err != nil || len(requests) != 1 {
		t.Fatalf("requests: %v", err)
	}
	views, err := f.svc.Status(context.Background(), "ag_1", []string{requests[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	execution := views[0].Tools[0].Execution
	if execution == nil || execution.State != inbox.ExecutionUnknown {
		t.Fatalf("unknown outcome: %+v", views)
	}
	f.call(t, f.agent, "deploy.run", map[string]any{"service": "api", "env": "prod", GrantField: token})
	if f.calls.Load() != 1 {
		t.Fatalf("blind retry dispatched %d calls", f.calls.Load())
	}
	if _, err := f.svc.Redeem(context.Background(), token, "ag_1", "deploy.run", deployArgs()); !errors.Is(err, inbox.ErrGrantUsed) {
		t.Fatalf("used permission: %v", err)
	}
}
