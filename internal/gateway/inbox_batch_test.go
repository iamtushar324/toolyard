package gateway

import (
	"context"
	"errors"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/tusharbhardwaj/toolyard/internal/inbox"
	"strings"
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

// TEC-1811: a dry run that finds problems is an error with valid false, so
// try_call reports it as failed; a passed one is valid true and no error.
func TestInboxDryRunReportsValid(t *testing.T) {
	f := newInboxFixture(t)
	dry := requestArgs(deployArgs())
	dry["dry_run"] = true
	res := f.call(t, f.agent, "inbox.request", dry)
	if s := structured(t, res); res.IsError || s["ok"] != true || s["valid"] != true || s["request_id"] != nil {
		t.Fatalf("passed dry run: isError=%v %v", res.IsError, s)
	}
	bad := requestArgs(deployArgs())
	bad["dry_run"] = true
	bad["tools"].([]any)[0].(map[string]any)["operation"] = "read"
	res = f.call(t, f.agent, "inbox.request", bad)
	s := structured(t, res)
	if !res.IsError || s["ok"] != false || s["valid"] != false || len(s["problems"].([]any)) == 0 {
		t.Fatalf("failed dry run: isError=%v %v", res.IsError, s)
	}
	if !strings.Contains(textOf(res), "Dry run found") {
		t.Fatalf("failed dry run text: %s", textOf(res))
	}
	// A real request has no valid field.
	if s := structured(t, f.call(t, f.agent, "inbox.request", requestArgs(deployArgs()))); s["ok"] != true || s["valid"] != nil {
		t.Fatalf("request: %v", s)
	}
}

// TEC-1812: fixture.echo is read-only, so a request may ask for it as a
// read; tools.search shows read_only for every result.
func TestInboxReadOperationUsesReadOnlyHint(t *testing.T) {
	f := newInboxFixture(t)
	echo := f.gw.tools["fixture.echo"]
	if h := echo.tool.Annotations.ReadOnlyHint; h == nil || !*h {
		t.Fatal("fixture.echo is not read-only")
	}
	read := inbox.ToolRequest{Tool: "fixture.echo", Operation: "read", Params: map[string]inbox.Constraint{"message": {Op: "eq", Eq: "hello"}}}
	if problems := f.gw.ValidateScope(f.agent, "ag_1", read); len(problems) != 0 {
		t.Fatalf("read of fixture.echo: %+v", problems)
	}
	read = inbox.ToolRequest{Tool: "deploy.run", Operation: "read", Params: map[string]inbox.Constraint{}}
	problems := f.gw.ValidateScope(f.agent, "ag_1", read)
	if len(problems) == 0 || problems[0].Path != "operation" || !strings.Contains(problems[0].Message, "read_only") {
		t.Fatalf("read of deploy.run: %+v", problems)
	}
	body := structured(t, f.call(t, f.agent, MetaSearchTool, map[string]any{"query": "", "detail": "summary", "limit": 200}))
	got := map[string]any{}
	for _, r := range body["results"].([]any) {
		m := r.(map[string]any)
		got[m["name"].(string)] = m["read_only"]
	}
	if got["fixture.echo"] != true || got["deploy.run"] != false {
		t.Fatalf("read_only: fixture.echo %v, deploy.run %v", got["fixture.echo"], got["deploy.run"])
	}
}
