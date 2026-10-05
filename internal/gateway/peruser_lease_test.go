package gateway_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/tusharbhardwaj/toolyard/internal/gateway"
)

type leasedCallResult struct {
	result *mcp.CallToolResult
	err    error
}

// The write stays in flight while another person needs a transport.
// Pool pressure must never turn that dispatched write into an unknown
// result or issue it a second time.
func newLeasedBearerUpstream(t *testing.T) (*bearerUpstream, <-chan struct{}, chan struct{}, *atomic.Int32) {
	t.Helper()
	entered, release := make(chan struct{}, 1), make(chan struct{})
	writes := &atomic.Int32{}
	m := server.NewMCPServer("leased-per-user", "1.0.0")
	m.AddTool(mcp.NewTool("get_whoami"), func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		bearer, _ := ctx.Value(seenBearerKey{}).(string)
		if bearer == "Bearer tok-bob" {
			writes.Add(1)
			entered <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return mcp.NewToolResultText(bearer), nil
	})
	inner := server.NewStreamableHTTPServer(m, server.WithHTTPContextFunc(func(ctx context.Context, r *http.Request) context.Context {
		return context.WithValue(ctx, seenBearerKey{}, r.Header.Get("Authorization"))
	}))
	up := &bearerUpstream{srv: httptest.NewServer(inner), deny: map[string]bool{}}
	t.Cleanup(up.srv.Close)
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	return up, entered, release, writes
}

func waitLeasedCall(t *testing.T, done <-chan leasedCallResult) leasedCallResult {
	t.Helper()
	select {
	case result := <-done:
		return result
	case <-time.After(5 * time.Second):
		t.Fatal("leased call did not return")
		return leasedCallResult{}
	}
}

func TestPerUserActiveCallSurvivesPoolPressure(t *testing.T) {
	up, entered, release, writes := newLeasedBearerUpstream(t)
	f := newPerUserFixture(t)
	f.tokens.connect("linear", uAlice, "tok-alice")
	f.tokens.connect("linear", uBob, "tok-bob")
	f.add(t, "linear", up)
	f.gw.SetMaxLivePerUser(1)
	done := make(chan leasedCallResult, 1)
	go func() {
		res, err := f.gw.RouteCall(gateway.WithAgentID(context.Background(), agBob), "lease", "linear.get_whoami", map[string]any{gateway.ReasonField: puReason})
		done <- leasedCallResult{res, err}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("write did not start")
	}
	if res := f.call(t, agAlice, "linear.get_whoami"); res.IsError || text(res) != "Bearer tok-alice" {
		t.Fatalf("other user: %q", text(res))
	}
	// Alice's completion restores cap1 without closing Bob's write.
	if n := f.gw.PerUserLiveCount(); n != 1 {
		t.Fatalf("live after idle overflow reclamation=%d", n)
	}
	if n := f.gw.SweepIdleStdioUpstreams(0); n != 0 {
		t.Fatalf("sweeper suspended %d active transports", n)
	}
	close(release)
	got := waitLeasedCall(t, done)
	if got.err != nil || got.result.IsError || text(got.result) != "Bearer tok-bob" {
		t.Fatalf("write outcome=%v %q", got.err, text(got.result))
	}
	if writes.Load() != 1 {
		t.Fatalf("write executions=%d", writes.Load())
	}
	if n := f.gw.SweepIdleStdioUpstreams(0); n != 1 {
		t.Fatalf("completed transport did not become idle: %d", n)
	}
}

func TestPerUserCanceledCallReleasesLease(t *testing.T) {
	up, entered, release, writes := newLeasedBearerUpstream(t)
	f := newPerUserFixture(t)
	f.tokens.connect("linear", uBob, "tok-bob")
	f.add(t, "linear", up)
	ctx, cancel := context.WithCancel(gateway.WithAgentID(context.Background(), agBob))
	defer cancel()
	done := make(chan leasedCallResult, 1)
	go func() {
		res, err := f.gw.RouteCall(ctx, "lease", "linear.get_whoami", map[string]any{gateway.ReasonField: puReason})
		done <- leasedCallResult{res, err}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("write did not start")
	}
	cancel()
	got := waitLeasedCall(t, done)
	if got.err == nil && !got.result.IsError {
		t.Fatal("canceled write succeeded")
	}
	close(release)
	if writes.Load() != 1 {
		t.Fatalf("canceled write was repeated: %d", writes.Load())
	}
	if n := f.gw.SweepIdleStdioUpstreams(0); n != 1 {
		t.Fatalf("canceled call retained its transport lease: %d", n)
	}
}

func TestPerUserShutdownCancelsActiveCall(t *testing.T) {
	up, entered, release, writes := newLeasedBearerUpstream(t)
	f := newPerUserFixture(t)
	f.tokens.connect("linear", uBob, "tok-bob")
	f.add(t, "linear", up)
	done := make(chan leasedCallResult, 1)
	go func() {
		res, err := f.gw.RouteCall(gateway.WithAgentID(context.Background(), agBob), "lease", "linear.get_whoami", map[string]any{gateway.ReasonField: puReason})
		done <- leasedCallResult{res, err}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("write did not start")
	}
	if err := f.gw.Close(); err != nil {
		t.Fatal(err)
	}
	got := waitLeasedCall(t, done)
	if got.err == nil && !got.result.IsError {
		t.Fatal("shutdown did not cancel the active transport")
	}
	close(release)
	if writes.Load() != 1 || f.gw.PerUserLiveCount() != 0 {
		t.Fatalf("shutdown executions=%d live=%d", writes.Load(), f.gw.PerUserLiveCount())
	}
}

func TestConcurrentSharedRegistrationsTrimIdleOverflow(t *testing.T) {
	m := server.NewMCPServer("shared-lease", "1.0.0")
	m.AddTool(mcp.NewTool("get_whoami"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("shared"), nil
	})
	inner := server.NewStreamableHTTPServer(m)
	entered, release := make(chan struct{}, 2), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			body, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(body))
			var message struct{ Method string }
			_ = json.Unmarshal(body, &message)
			if message.Method == "tools/list" {
				entered <- struct{}{}
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
			}
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	f := newPerUserFixture(t)
	f.gw.SetMaxLiveUpstreams(1)
	done := make(chan error, 2)
	for _, name := range []string{"first", "second"} {
		go func() {
			done <- f.gw.AddUpstream(context.Background(), gateway.UpstreamConfig{Name: name, Transport: "http", URL: srv.URL})
		}()
	}
	for range 2 {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent registrations did not reach the catalog barrier")
		}
	}
	close(release)
	for range 2 {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent registration did not complete")
		}
	}
	if live := f.gw.LiveUpstreamCount(); live > 1 {
		t.Fatalf("shared idle overflow survived registration: %d", live)
	}
	for _, name := range []string{"first", "second"} {
		if count := f.gw.UpstreamToolCount(name); count != 1 {
			t.Fatalf("%s cached catalog count=%d", name, count)
		}
	}
}
