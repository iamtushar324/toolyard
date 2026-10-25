package gateway

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/tusharbhardwaj/toolyard/internal/jobs"
)

// An upstream's notifications/progress for a job's call are recorded on
// the job: the call carries a progress token, and the gateway routes the
// upstream's notifications for that token to the job.
func TestUpstreamProgressIsRecordedOnTheJob(t *testing.T) {
	up := server.NewMCPServer("progress-upstream", "1.0.0")
	tokens := make(chan mcp.ProgressToken, 1)
	release := make(chan struct{})
	up.AddTool(mcp.NewTool("get_progress"), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var tok mcp.ProgressToken
		if req.Params.Meta != nil {
			tok = req.Params.Meta.ProgressToken
		}
		tokens <- tok
		if tok != nil {
			_ = server.ServerFromContext(ctx).SendNotificationToClient(ctx, string(mcp.MethodNotificationProgress), map[string]any{
				"progressToken": tok, "progress": 1, "total": 4, "message": "a quarter",
			})
		}
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return mcp.NewToolResultText("progress done"), nil
	})
	srv := httptest.NewServer(server.NewStreamableHTTPServer(up))
	t.Cleanup(srv.Close)

	f := newJobsFixture(t)
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	if err := f.gw.AddUpstream(context.Background(), UpstreamConfig{Name: "beta", Transport: "http", URL: srv.URL}); err != nil {
		t.Fatal(err)
	}
	f.rules.set("beta", jobs.AsyncSetting{Mode: jobs.AsyncAlways})
	id := jobIDOf(t, f.call(t, "beta.get_progress", nil))

	select {
	case tok := <-tokens:
		if s, _ := tok.(string); !strings.HasPrefix(s, "tyjob-") {
			t.Fatalf("the upstream call carried progress token %v", tok)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the upstream tool was never called")
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		st := structured(t, f.call(t, MetaJobsStatus, map[string]any{"job_id": id}))
		if p, _ := st["progress"].(map[string]any); p != nil && p["message"] == "a quarter" && p["progress"] == float64(1) && p["total"] == float64(4) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("progress never recorded: %v", st)
		}
		time.Sleep(20 * time.Millisecond)
	}
	close(release)
	if w := f.waitJob(t, id); w["status"] != jobs.StatusSucceeded || !strings.Contains(asJSON(w["result"]), "progress done") {
		t.Fatalf("job = %v", w)
	}
	// A call outside a job carries no token.
	f.rules.set("beta", jobs.AsyncSetting{})
	go func() {
		_, _ = f.gw.RouteCall(WithAgentID(context.Background(), "ag_1"), "test", "beta.get_progress", map[string]any{ReasonField: jobsTestReason})
	}()
	select {
	case tok := <-tokens:
		if tok != nil {
			t.Fatalf("an inline call carried progress token %v", tok)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the inline call never reached the upstream")
	}
}
