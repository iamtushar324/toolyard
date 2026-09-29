package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

const sessionTestHeader = "x-bk-bifrost-vk"

// forgetfulUpstream is a real mcp-go streamable-HTTP server behind a
// handler that can "forget" the live session the way BkCoreServices does
// after an idle timeout, max age or restart: every later request carrying
// that session id is rejected before the tool runs, with HTTP 400 and
// JSON-RPC -32000 (mode "400") or a plain 404 (mode "404"). A fresh
// initialize without a session id is served normally. It also counts
// requests per method and records the identity header each one carried.
type forgetfulUpstream struct {
	srv  *httptest.Server
	mode string

	mu        sync.Mutex
	current   string          // the session id the client is using
	forgotten map[string]bool // ids the server no longer recognises
	fail500   int             // reject this many tools/call with a 500 first
	// injectRPC, when set, answers the next tools/call itself with HTTP 200
	// and this JSON-RPC error: an upstream whose tool reports a custom code.
	injectRPC *rpcInject
	counts    map[string]int
	identity  map[string][]string // method -> identity header per request ("" = absent)
}

type rpcInject struct {
	code    int
	message string
}

func newForgetfulUpstream(t *testing.T, mode string) *forgetfulUpstream {
	t.Helper()
	m := server.NewMCPServer("bk", "1.0.0")
	m.AddTool(mcp.NewTool("get_ok"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("ok"), nil
	})
	m.AddTool(mcp.NewTool("get_fail"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultError("nope"), nil
	})
	// A handler error: mcp-go reports it as JSON-RPC -32603 with this text.
	m.AddTool(mcp.NewTool("get_handler_error"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return nil, errors.New("session not found")
	})
	inner := server.NewStreamableHTTPServer(m)
	f := &forgetfulUpstream{mode: mode, forgotten: map[string]bool{}, counts: map[string]int{}, identity: map[string][]string{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if r.Body != nil {
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &msg)
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		method := msg.Method
		if method == "" {
			method = r.Method // GET stream / DELETE close
		}
		sid := r.Header.Get("Mcp-Session-Id")

		f.mu.Lock()
		f.counts[method]++
		f.identity[method] = append(f.identity[method], r.Header.Get(sessionTestHeader))
		if sid != "" && !f.forgotten[sid] {
			f.current = sid
		}
		dead := sid != "" && f.forgotten[sid]
		fail := false
		if method == "tools/call" && f.fail500 > 0 {
			f.fail500--
			fail = true
		}
		var inject *rpcInject
		if method == "tools/call" && f.injectRPC != nil {
			inject, f.injectRPC = f.injectRPC, nil
		}
		f.mu.Unlock()

		switch {
		case dead && f.mode == "404":
			http.Error(w, "Session not found", http.StatusNotFound)
			return
		case dead:
			// Byte-for-byte what beknown-services mcp-server sends.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","error":{"code":-32000,"message":"Invalid session or missing initialization"},"id":null}`)
			return
		case fail:
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		case inject != nil:
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":{"code":%d,"message":%q}}`, msg.ID, inject.code, inject.message)
			return
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// forget makes the server drop the session the client is currently using.
func (f *forgetfulUpstream) forget(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.current == "" {
		t.Fatal("no live session to forget")
	}
	f.forgotten[f.current] = true
	f.current = ""
}

func (f *forgetfulUpstream) count(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.counts[method]
}

func (f *forgetfulUpstream) identityOn(method string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.identity[method]...)
}

// dialForgetful connects an upstream whose header func mirrors the real
// identity composition: the forwarded key or nothing.
func dialForgetful(t *testing.T, f *forgetfulUpstream) *upstream {
	t.Helper()
	u, err := newUpstream(context.Background(), UpstreamConfig{
		Name: "bk", Transport: "http", URL: f.srv.URL, IdentityHeader: sessionTestHeader,
		HeaderFunc: func(ctx context.Context) map[string]string {
			if k, ok := ForwardedKey(ctx); ok {
				return map[string]string{sessionTestHeader: k}
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = u.close() })
	if _, err := u.listTools(context.Background()); err != nil {
		t.Fatalf("listTools: %v", err)
	}
	if f.count("initialize") != 1 {
		t.Fatalf("initialize count after dial = %d", f.count("initialize"))
	}
	return u
}

func resultText(res *mcp.CallToolResult) string {
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := mcp.AsTextContent(c); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}

// TestSessionRecoveryRetriesOnce: after the server forgets the session, a
// call re-initialises exactly once and the retried call succeeds, in both
// rejection styles.
func TestSessionRecoveryRetriesOnce(t *testing.T) {
	for _, mode := range []string{"400", "404"} {
		t.Run(mode, func(t *testing.T) {
			f := newForgetfulUpstream(t, mode)
			u := dialForgetful(t, f)
			res, err := u.callTool(context.Background(), "get_ok", nil)
			if err != nil || resultText(res) != "ok" {
				t.Fatalf("warm call: %v %v", err, res)
			}

			f.forget(t)
			res, err = u.callTool(context.Background(), "get_ok", nil)
			if err != nil {
				t.Fatalf("call after forget: %v", err)
			}
			if res.IsError || resultText(res) != "ok" {
				t.Fatalf("call after forget = %q (isError=%v)", resultText(res), res.IsError)
			}
			if got := f.count("initialize"); got != 2 {
				t.Fatalf("initialize count = %d, want 2 (one re-initialize)", got)
			}
			// warm call + rejected attempt + retry
			if got := f.count("tools/call"); got != 3 {
				t.Fatalf("tools/call count = %d, want 3", got)
			}
			if u.recoveries.Load() != 1 {
				t.Fatalf("recoveries = %d, want 1", u.recoveries.Load())
			}
			// The new session serves the next call without another re-dial.
			if _, err := u.callTool(context.Background(), "get_ok", nil); err != nil || f.count("initialize") != 2 {
				t.Fatalf("follow-up call: err=%v initialize=%d", err, f.count("initialize"))
			}
		})
	}
}

// TestSessionRecoveryConcurrentCallersOneRedial: many callers hitting the
// dead session together cause one re-initialize, and all of them succeed.
func TestSessionRecoveryConcurrentCallersOneRedial(t *testing.T) {
	f := newForgetfulUpstream(t, "400")
	u := dialForgetful(t, f)
	if _, err := u.callTool(context.Background(), "get_ok", nil); err != nil {
		t.Fatal(err)
	}
	f.forget(t)

	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := u.callTool(context.Background(), "get_ok", nil)
			if err != nil {
				errs <- err
				return
			}
			if res.IsError || resultText(res) != "ok" {
				errs <- fmt.Errorf("result %q isError=%v", resultText(res), res.IsError)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if got := f.count("initialize"); got != 2 {
		t.Fatalf("initialize count = %d, want 2", got)
	}
	if u.recoveries.Load() != 1 {
		t.Fatalf("recoveries = %d, want 1", u.recoveries.Load())
	}
}

// TestSessionRecoveryNotForOtherErrors: a transport error that isn't a
// session rejection is returned as is, with no re-dial and no retry.
func TestSessionRecoveryNotForOtherErrors(t *testing.T) {
	f := newForgetfulUpstream(t, "400")
	u := dialForgetful(t, f)
	f.mu.Lock()
	f.fail500 = 1
	f.mu.Unlock()

	_, err := u.callTool(context.Background(), "get_ok", nil)
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("expected the 500 to surface, got %v", err)
	}
	if f.count("initialize") != 1 || f.count("tools/call") != 1 || u.recoveries.Load() != 0 {
		t.Fatalf("500 was retried: initialize=%d tools/call=%d recoveries=%d",
			f.count("initialize"), f.count("tools/call"), u.recoveries.Load())
	}
	if u.suspended() {
		t.Fatal("client was dropped on a non-session error")
	}
}

// TestSessionRecoveryNotForToolErrors: a tool that answers isError ran, so
// it is never retried.
func TestSessionRecoveryNotForToolErrors(t *testing.T) {
	f := newForgetfulUpstream(t, "400")
	u := dialForgetful(t, f)
	res, err := u.callTool(context.Background(), "get_fail", nil)
	if err != nil || !res.IsError || resultText(res) != "nope" {
		t.Fatalf("tool error call: err=%v res=%v", err, res)
	}
	if f.count("initialize") != 1 || f.count("tools/call") != 1 || u.recoveries.Load() != 0 {
		t.Fatalf("tool error was retried: initialize=%d tools/call=%d recoveries=%d",
			f.count("initialize"), f.count("tools/call"), u.recoveries.Load())
	}
}

// TestSessionRecoveryNotForToolJSONRPCErrors: a JSON-RPC error produced by
// the tool itself is not a session rejection, whatever its message says.
// The tool may have run, so there is no re-dial and no retry: neither for
// the standard -32603 mcp-go reports a handler error as, nor for an
// upstream that answers a custom -32000 from a tool.
func TestSessionRecoveryNotForToolJSONRPCErrors(t *testing.T) {
	f := newForgetfulUpstream(t, "400")
	u := dialForgetful(t, f)

	_, err := u.callTool(context.Background(), "get_handler_error", nil)
	if err == nil || !strings.Contains(err.Error(), "session not found") {
		t.Fatalf("handler error should surface, got %v", err)
	}
	if f.count("initialize") != 1 || f.count("tools/call") != 1 || u.recoveries.Load() != 0 {
		t.Fatalf("-32603 handler error was retried: initialize=%d tools/call=%d recoveries=%d",
			f.count("initialize"), f.count("tools/call"), u.recoveries.Load())
	}

	f.mu.Lock()
	f.injectRPC = &rpcInject{code: -32000, message: "session not found"}
	f.mu.Unlock()
	_, err = u.callTool(context.Background(), "get_ok", nil)
	if err == nil || err.Error() != "session not found" {
		t.Fatalf("custom-code tool error should surface as is, got %v", err)
	}
	if f.count("initialize") != 1 || f.count("tools/call") != 2 || u.recoveries.Load() != 0 {
		t.Fatalf("custom -32000 tool error was retried: initialize=%d tools/call=%d recoveries=%d",
			f.count("initialize"), f.count("tools/call"), u.recoveries.Load())
	}
	if u.suspended() {
		t.Fatal("client was dropped on a tool-level JSON-RPC error")
	}
	// The session is still fine: a real rejection afterwards still recovers.
	f.forget(t)
	if res, err := u.callTool(context.Background(), "get_ok", nil); err != nil || resultText(res) != "ok" || f.count("initialize") != 2 {
		t.Fatalf("recovery after tool errors: err=%v initialize=%d", err, f.count("initialize"))
	}
}

// TestSessionRecoveryKeepsIdentityOnRetryOnly: the caller's key is on the
// retried tools/call, and the re-initialize goes out without it.
func TestSessionRecoveryKeepsIdentityOnRetryOnly(t *testing.T) {
	f := newForgetfulUpstream(t, "400")
	u := dialForgetful(t, f)
	ctx := WithForwardedKey(context.Background(), "vk-alice-secret")
	if _, err := u.callTool(ctx, "get_ok", nil); err != nil {
		t.Fatal(err)
	}
	f.forget(t)
	res, err := u.callTool(ctx, "get_ok", nil)
	if err != nil || res.IsError {
		t.Fatalf("call after forget: %v %v", err, res)
	}
	inits := f.identityOn("initialize")
	if len(inits) != 2 || inits[0] != "" || inits[1] != "" {
		t.Fatalf("identity header on initialize requests = %q, want none", inits)
	}
	calls := f.identityOn("tools/call")
	if len(calls) != 3 || calls[2] != "vk-alice-secret" {
		t.Fatalf("identity header on tools/call requests = %q, want the key on the retry", calls)
	}
	for method, vals := range map[string][]string{"notifications/initialized": f.identityOn("notifications/initialized"), "tools/list": f.identityOn("tools/list")} {
		for _, v := range vals {
			if v != "" {
				t.Fatalf("%s carried the identity header", method)
			}
		}
	}
}

// TestIsSessionError pins the classification. Yes: mcp-go's
// terminated-session sentinel (the 404 path) and, whole and in any case,
// the exact -32000 rejections a transport sends with HTTP 400 (which
// mcp-go hands over as a bare error, since it maps no code for -32000).
// No: any error carrying a standard JSON-RPC code (mcp-go wraps those in
// its sentinels; a tool handler's error is -32603), a rejection phrase
// with extra text around it, or a wrong HTTP status.
func TestIsSessionError(t *testing.T) {
	yes := []error{
		transport.ErrSessionTerminated,
		fmt.Errorf("failed to send request: %w", transport.ErrSessionTerminated),
		transport.NewError(fmt.Errorf("failed to send request: %w", transport.ErrSessionTerminated)),
		errors.New("Invalid session or missing initialization"), // beknown, parsed -32000 body
		errors.New("invalid session or missing initialization"),
		transport.NewError(errors.New("request failed with status 400: Invalid session or missing initialization\n")),
		errors.New("Bad Request: Server not initialized"),            // @modelcontextprotocol/sdk
		errors.New("Bad Request: Mcp-Session-Id header is required"), // @modelcontextprotocol/sdk
	}
	for _, err := range yes {
		if _, ok := sessionErrorReason(err); !ok {
			t.Errorf("%v: want session error", err)
		}
	}
	no := []error{
		nil,
		fmt.Errorf("%w: %s", mcp.ErrInternalError, "session not found"),                         // tool handler error (-32603)
		fmt.Errorf("%w: %s", mcp.ErrInternalError, "Invalid session or missing initialization"), // handler echoing the phrase
		fmt.Errorf("%w: %s", mcp.ErrInvalidParams, "invalid session"),
		errors.New("session not found"),
		errors.New("Invalid session ID"),
		errors.New("session terminated"),
		errors.New("Invalid session or missing initialization: details"),
		errors.New("tool: Invalid session or missing initialization"),
		transport.NewError(errors.New("request failed with status 500: Invalid session or missing initialization")),
		transport.NewError(errors.New("request failed with status 400: boom")),
		errors.New("request failed with status 500: boom"),
		context.DeadlineExceeded,
		errors.New("tool failed: session_id must be a string"),
		errors.New("client not initialized"),
	}
	for _, err := range no {
		if reason, ok := sessionErrorReason(err); ok {
			t.Errorf("%v: classified as session error (%q)", err, reason)
		}
	}
}
