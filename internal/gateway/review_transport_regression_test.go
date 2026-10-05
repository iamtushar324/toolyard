package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/tusharbhardwaj/toolyard/internal/approval"
)

func TestReviewHTTPMCPRetainsOriginalNumbersAndAgentContext(t *testing.T) {
	f := newInboxFixture(t)
	var seen map[string]any
	var seenAgent string
	f.gw.registerEntry(toolEntry{tool: mcp.Tool{Name: "precision.get_status", InputSchema: mcp.ToolInputSchema{Type: "object", Properties: addMetaProps(map[string]any{"resource": map[string]any{"type": "integer"}, "nested": map[string]any{"type": "object"}})}}, upstream: "precision", originalName: "get_status", reasonField: ReasonField, handle: func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		seen = args
		seenAgent = agentIDFromContext(ctx)
		return mcp.NewToolResultText("checked"), nil
	}})
	transport := server.NewStreamableHTTPServer(f.gw.MCPServer(), server.WithStateLess(true), server.WithDisableStreaming(true), server.WithHTTPContextFunc(func(ctx context.Context, r *http.Request) context.Context { return WithAgentID(ctx, "ag_http") }))
	handler := PreserveToolCallNumbers(transport)
	r := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"precision.get_status","arguments":{"_reason":"Review the exact resource identifier through the HTTP transport","resource":9007199254740993,"nested":{"other":9007199254740995}}}}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK || seen == nil {
		t.Fatalf("HTTP call did not reach handler: %d %s", w.Code, w.Body.String())
	}
	if got, ok := seen["resource"].(json.Number); !ok || got.String() != "9007199254740993" {
		t.Fatalf("resource precision lost: %#v", seen["resource"])
	}
	if got, ok := seen["nested"].(map[string]any)["other"].(json.Number); !ok || got.String() != "9007199254740995" {
		t.Fatalf("nested precision lost: %#v", seen["nested"])
	}
	if seenAgent != "ag_http" {
		t.Fatalf("authentication context lost: %q", seenAgent)
	}
}

type reviewUnknownExecutor struct {
	bus  *approval.Bus
	done chan error
}

func (e reviewUnknownExecutor) Execute(ctx context.Context, r *approval.Request) {
	e.done <- e.bus.SetResultState(ctx, r.ID, "", true, "Transport acknowledgement lost", "outcome_unknown")
}

func TestReviewUnknownLegacyResumeAndPollAreTerminal(t *testing.T) {
	f := newActorFixture(t)
	ctx := WithAgentID(context.Background(), "ag_review")
	done := make(chan error, 1)
	f.bus.SetExecutor(reviewUnknownExecutor{bus: f.bus, done: done})
	r, err := f.bus.Hold(ctx, approval.NewRequest{AgentID: "ag_review", UpstreamName: "t", ToolName: "t.run", Arguments: map[string]any{"env": "prod"}, Reason: testReason, RequireHuman: true}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.bus.Decide(ctx, r.ID, approval.StatusAllowed, "review-owner"); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("unknown outcome did not persist")
	}
	for _, call := range []struct {
		tool string
		args map[string]any
	}{{"t.run", map[string]any{ApprovalIDField: r.ID}}, {"tools.poll_approval", map[string]any{"approval_id": r.ID}}} {
		res := f.call(t, ctx, "review", call.tool, call.args)
		snap := decodeResult(t, res)
		if snap["status"] != "outcome_unknown" {
			t.Fatalf("%s returned nonterminal snapshot: %+v", call.tool, snap)
		}
	}
	if f.runCount() != 0 {
		t.Fatal("unknown legacy call dispatched again")
	}
}

func TestReviewLegacyTransportTimeoutPersistsUnknown(t *testing.T) {
	f := newActorFixture(t)
	ctx := WithAgentID(context.Background(), "ag_review")
	var calls atomic.Int32
	f.gw.mu.Lock()
	entry := f.gw.tools["t.run"]
	entry.handle = func(ctx context.Context, _ map[string]any) (*mcp.CallToolResult, error) {
		calls.Add(1)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	f.gw.tools["t.run"] = entry
	f.gw.mu.Unlock()
	f.gw.upstreamCallTimeout = 10 * time.Millisecond
	f.bus.SetExecutor(f.gw)
	r, err := f.bus.Hold(ctx, approval.NewRequest{AgentID: "ag_review", UpstreamName: "t", ToolName: "t.run", Arguments: map[string]any{"env": "prod"}, Reason: testReason, RequireHuman: true}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.bus.Decide(ctx, r.ID, approval.StatusAllowed, "review-owner"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		r, err = f.bus.Get(ctx, r.ID)
		if err != nil {
			t.Fatal(err)
		}
		if r.ResultExecutedAt > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timeout outcome not persisted")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if r.ExecutionState != "outcome_unknown" {
		t.Fatalf("possibly committed timeout classified as %q", r.ExecutionState)
	}
	res := f.call(t, ctx, "review", "t.run", map[string]any{ApprovalIDField: r.ID})
	if snap := decodeResult(t, res); snap["status"] != "outcome_unknown" {
		t.Fatalf("resume: %+v", snap)
	}
	if calls.Load() != 1 {
		t.Fatalf("uncertain write dispatched %d times", calls.Load())
	}
}
