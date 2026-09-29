package gateway_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/tusharbhardwaj/toolyard/internal/approval"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/gateway"
	"github.com/tusharbhardwaj/toolyard/internal/memory"
	"github.com/tusharbhardwaj/toolyard/internal/metrics"
	"github.com/tusharbhardwaj/toolyard/internal/policy"
	"github.com/tusharbhardwaj/toolyard/internal/realtime"
	"github.com/tusharbhardwaj/toolyard/internal/store"
	"github.com/tusharbhardwaj/toolyard/internal/upstreams"
)

// This file is an external test package on purpose: the HTTP-level tests
// go through upstreams.Service (which imports gateway) so the header
// function under test is the real toCfg composition, not a copy.

const (
	identityHeader = "x-bk-bifrost-vk"
	// fwdReason satisfies the gateway's minimum _reason length.
	fwdReason = "identity forwarding test: prove which caller the call ran as"
)

// seenRequest is one HTTP request the recording upstream received: its
// JSON-RPC method and whether/what the identity header carried.
type seenRequest struct {
	method  string
	present bool
	value   string
}

// recordingUpstream is a real mcp-go streamable-HTTP server behind a
// handler that notes every inbound request. Its get_whoami tool echoes the
// identity header the server saw for that call, so a result proves which
// caller the call ran as.
type recordingUpstream struct {
	srv  *httptest.Server
	mu   sync.Mutex
	seen []seenRequest
}

type seenIdentityKey struct{}

func newRecordingUpstream(t *testing.T) *recordingUpstream {
	t.Helper()
	m := server.NewMCPServer("bk", "1.0.0")
	m.AddTool(mcp.NewTool("get_whoami", mcp.WithDescription("echo the identity header")),
		func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			v, _ := ctx.Value(seenIdentityKey{}).(string)
			return mcp.NewToolResultText(v), nil
		})
	inner := server.NewStreamableHTTPServer(m, server.WithHTTPContextFunc(
		func(ctx context.Context, r *http.Request) context.Context {
			return context.WithValue(ctx, seenIdentityKey{}, r.Header.Get(identityHeader))
		}))
	r := &recordingUpstream{}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var method string
		if req.Body != nil {
			body, _ := io.ReadAll(req.Body)
			var msg struct {
				Method string `json:"method"`
			}
			_ = json.Unmarshal(body, &msg)
			method = msg.Method
			req.Body = io.NopCloser(bytes.NewReader(body))
		}
		vals := req.Header.Values(identityHeader)
		r.mu.Lock()
		r.seen = append(r.seen, seenRequest{method: method, present: len(vals) > 0, value: req.Header.Get(identityHeader)})
		r.mu.Unlock()
		inner.ServeHTTP(w, req)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *recordingUpstream) requests() []seenRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]seenRequest(nil), r.seen...)
}

func (r *recordingUpstream) count(method string) int {
	n := 0
	for _, s := range r.requests() {
		if s.method == method {
			n++
		}
	}
	return n
}

// assertOnlyToolCallsCarryKey fails if any request other than a tools/call
// carried the identity header, and returns the tools/call values seen.
func (r *recordingUpstream) assertOnlyToolCallsCarryKey(t *testing.T) []string {
	t.Helper()
	var calls []string
	for _, s := range r.requests() {
		if s.method == "tools/call" {
			calls = append(calls, s.value)
			continue
		}
		if s.present {
			t.Fatalf("%s request carried the identity header (%q)", s.method, s.value)
		}
	}
	return calls
}

// fakeIdentity maps caller ids to keys. Unknown callers get
// ErrNoIdentityKey; err, when set, fails every lookup.
type fakeIdentity struct {
	mu   sync.Mutex
	keys map[string]string
	err  error
}

func (f *fakeIdentity) ForwardKey(_ context.Context, callerID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return "", f.err
	}
	k, ok := f.keys[callerID]
	if !ok {
		return "", gateway.ErrNoIdentityKey
	}
	return k, nil
}

type recMetrics struct {
	mu     sync.Mutex
	events []metrics.Event
}

func (m *recMetrics) Record(e metrics.Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, e)
}

func (m *recMetrics) last(tool string) (metrics.Event, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := len(m.events) - 1; i >= 0; i-- {
		if m.events[i].ToolName == tool {
			return m.events[i], true
		}
	}
	return metrics.Event{}, false
}

type forwardFixture struct {
	gw  *gateway.Gateway
	svc *upstreams.Service
	met *recMetrics
}

const (
	alice = "ag_alice"
	bob   = "ag_bob"
	nokey = "ag_nokey"
)

// newForwardFixture wires a gateway (with resolver ids, which may be nil)
// and the real upstreams service in front of it.
func newForwardFixture(t *testing.T, ids gateway.IdentityResolver) *forwardFixture {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "fwd.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	bus, err := approval.New(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	met := &recMetrics{}
	opts := gateway.Options{
		Policy: policy.New(db), Approval: bus, Audit: audit.New(db), Memory: memory.New(db),
		Hub: realtime.NewHub(), Metrics: met,
	}
	if ids != nil {
		opts.Identity = ids
	}
	gw := gateway.New(opts)
	t.Cleanup(func() { _ = gw.Close() })
	return &forwardFixture{gw: gw, svc: upstreams.New(db, gw), met: met}
}

func keys() *fakeIdentity {
	return &fakeIdentity{keys: map[string]string{alice: "vk-alice-secret", bob: "vk-bob-secret"}}
}

func (f *forwardFixture) add(t *testing.T, name string, up *recordingUpstream, id *upstreams.IdentityForwarding, headers map[string]string) {
	t.Helper()
	if _, err := f.svc.Add(context.Background(), upstreams.Server{
		Name: name, Transport: "http", URL: up.srv.URL, Identity: id, Headers: headers,
	}); err != nil {
		t.Fatalf("Add %s: %v", name, err)
	}
}

func callerCtx(id string) context.Context { return gateway.WithAgentID(context.Background(), id) }

func (f *forwardFixture) call(t *testing.T, ctx context.Context, tool string) *mcp.CallToolResult {
	t.Helper()
	res, err := f.gw.RouteCall(ctx, "test", tool, map[string]any{gateway.ReasonField: fwdReason})
	if err != nil {
		t.Fatalf("RouteCall %s: %v", tool, err)
	}
	return res
}

func text(res *mcp.CallToolResult) string {
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := mcp.AsTextContent(c); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}

// TestIdentityForwardOnlyOnToolCalls: the key rides on tools/call and
// nothing else. initialize, notifications/initialized and tools/list at
// connect time go out bare; so does the re-dial after an idle suspend.
func TestIdentityForwardOnlyOnToolCalls(t *testing.T) {
	up := newRecordingUpstream(t)
	f := newForwardFixture(t, keys())
	f.add(t, "bk", up, &upstreams.IdentityForwarding{Header: identityHeader, Register: true}, nil)

	if up.count("initialize") != 1 || up.count("tools/list") != 1 {
		t.Fatalf("connect traffic: %+v", up.requests())
	}
	if calls := up.assertOnlyToolCallsCarryKey(t); len(calls) != 0 {
		t.Fatalf("tools/call before any call: %v", calls)
	}

	res := f.call(t, callerCtx(alice), "bk.get_whoami")
	if res.IsError || text(res) != "vk-alice-secret" {
		t.Fatalf("alice's call ran as %q (isError=%v)", text(res), res.IsError)
	}
	if calls := up.assertOnlyToolCallsCarryKey(t); len(calls) != 1 || calls[0] != "vk-alice-secret" {
		t.Fatalf("tools/call headers = %v", calls)
	}

	// Suspend and let the next call re-dial: the new initialize must not
	// carry bob's key even though bob's call triggers it.
	if n := f.gw.SweepIdleStdioUpstreams(0); n != 1 {
		t.Fatalf("suspended %d upstreams, want 1", n)
	}
	res = f.call(t, callerCtx(bob), "bk.get_whoami")
	if res.IsError || text(res) != "vk-bob-secret" {
		t.Fatalf("bob's call after resume ran as %q (isError=%v)", text(res), res.IsError)
	}
	if up.count("initialize") != 2 {
		t.Fatalf("expected a second initialize after resume: %+v", up.requests())
	}
	calls := up.assertOnlyToolCallsCarryKey(t)
	if len(calls) != 2 || calls[1] != "vk-bob-secret" {
		t.Fatalf("tools/call headers = %v", calls)
	}

	// The raw MCP path (what an agent's client sends) is the same route.
	msg, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 7, "method": "tools/call",
		"params": map[string]any{"name": "bk.get_whoami", "arguments": map[string]any{gateway.ReasonField: fwdReason}},
	})
	raw := f.gw.MCPServer().HandleMessage(callerCtx(alice), json.RawMessage(msg))
	b, _ := json.Marshal(raw)
	if !strings.Contains(string(b), "vk-alice-secret") || strings.Contains(string(b), `"isError":true`) {
		t.Fatalf("raw MCP tools/call = %s", b)
	}
	if m, ok := f.met.last("bk.get_whoami"); !ok || m.Outcome != metrics.OutcomeOK {
		t.Fatalf("metrics for a forwarded call = %+v", m)
	}
}

// TestIdentityForwardConcurrentCallersNoCrossTalk: over the one shared
// upstream session, every call still lands with its own caller's key.
func TestIdentityForwardConcurrentCallersNoCrossTalk(t *testing.T) {
	up := newRecordingUpstream(t)
	f := newForwardFixture(t, keys())
	f.add(t, "bk", up, &upstreams.IdentityForwarding{Header: identityHeader}, nil)

	const perCaller = 25
	var wg sync.WaitGroup
	errs := make(chan error, 2*perCaller)
	for _, c := range []struct{ id, key string }{{alice, "vk-alice-secret"}, {bob, "vk-bob-secret"}} {
		for i := 0; i < perCaller; i++ {
			wg.Add(1)
			go func(id, key string) {
				defer wg.Done()
				res, err := f.gw.RouteCall(callerCtx(id), "test", "bk.get_whoami",
					map[string]any{gateway.ReasonField: fwdReason})
				if err != nil {
					errs <- err
					return
				}
				if res.IsError || text(res) != key {
					errs <- fmt.Errorf("%s ran as %q (isError=%v)", id, text(res), res.IsError)
				}
			}(c.id, c.key)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if got := up.count("tools/call"); got != 2*perCaller {
		t.Fatalf("upstream saw %d tools/call, want %d", got, 2*perCaller)
	}
	up.assertOnlyToolCallsCarryKey(t)
}

// TestIdentityStaticHeaderRefused: a static header spelled like the
// identity header is rejected at save time, in any letter case.
func TestIdentityStaticHeaderRefused(t *testing.T) {
	up := newRecordingUpstream(t)
	f := newForwardFixture(t, keys())
	_, err := f.svc.Add(context.Background(), upstreams.Server{
		Name: "bk", Transport: "http", URL: up.srv.URL,
		Identity: &upstreams.IdentityForwarding{Header: identityHeader},
		Headers:  map[string]string{"X-BK-Bifrost-VK": "pinned"},
	})
	if !errors.Is(err, upstreams.ErrInvalid) {
		t.Fatalf("Add: err = %v, want ErrInvalid", err)
	}
	if _, err := f.svc.Get(context.Background(), "bk"); !errors.Is(err, upstreams.ErrNotFound) {
		t.Fatalf("rejected server was saved: %v", err)
	}
	if len(up.requests()) != 0 {
		t.Fatalf("rejected server was dialled: %+v", up.requests())
	}
}

// TestIdentityCallerWithoutKeyRefused: no key, no call. The upstream never
// sees the request, the agent gets the provisioning hint, the metric says
// why. No key material appears in the result.
func TestIdentityCallerWithoutKeyRefused(t *testing.T) {
	up := newRecordingUpstream(t)
	f := newForwardFixture(t, keys())
	f.add(t, "bk", up, &upstreams.IdentityForwarding{Header: identityHeader}, nil)

	res := f.call(t, callerCtx(nokey), "bk.get_whoami")
	if !res.IsError {
		t.Fatalf("call without a key succeeded: %q", text(res))
	}
	want := "bk records who made each change, and your toolyard user has no Beknown key yet. Ask a toolyard admin to provision one (Users page)."
	if text(res) != want {
		t.Fatalf("message = %q\nwant      %q", text(res), want)
	}
	if up.count("tools/call") != 0 {
		t.Fatalf("upstream saw the refused call: %+v", up.requests())
	}
	m, ok := f.met.last("bk.get_whoami")
	if !ok || m.Outcome != metrics.OutcomeError || m.ErrorClass != "identity" {
		t.Fatalf("metrics = %+v, want error/identity", m)
	}

	// Any other resolver failure is refused too, with a generic message.
	ids := keys()
	ids.err = errors.New("sealbox: open failed for vk-would-be-secret")
	f2 := newForwardFixture(t, ids)
	up2 := newRecordingUpstream(t)
	f2.add(t, "bk", up2, &upstreams.IdentityForwarding{Header: identityHeader}, nil)
	res = f2.call(t, callerCtx(alice), "bk.get_whoami")
	if !res.IsError || strings.Contains(text(res), "vk-would-be-secret") || text(res) == want {
		t.Fatalf("generic refusal = %q", text(res))
	}
	if up2.count("tools/call") != 0 {
		t.Fatalf("upstream saw the refused call: %+v", up2.requests())
	}
	if m, ok := f2.met.last("bk.get_whoami"); !ok || m.ErrorClass != "identity" {
		t.Fatalf("metrics = %+v", m)
	}
}

// TestIdentityNilResolverRefusesCalls: a gateway without a resolver never
// calls an identity-forwarding upstream under its own credentials.
func TestIdentityNilResolverRefusesCalls(t *testing.T) {
	up := newRecordingUpstream(t)
	f := newForwardFixture(t, nil)
	f.add(t, "bk", up, &upstreams.IdentityForwarding{Header: identityHeader}, nil)

	res := f.call(t, callerCtx(alice), "bk.get_whoami")
	if !res.IsError {
		t.Fatalf("call without a resolver succeeded: %q", text(res))
	}
	if up.count("tools/call") != 0 {
		t.Fatalf("upstream saw the refused call: %+v", up.requests())
	}
	if m, ok := f.met.last("bk.get_whoami"); !ok || m.ErrorClass != "identity" {
		t.Fatalf("metrics = %+v", m)
	}
}

// TestIdentityPlainUpstreamUnchanged: a server without an identity setting
// behaves as before. A static header of the same name is sent verbatim,
// no caller key is ever attached, and callers without keys are served.
func TestIdentityPlainUpstreamUnchanged(t *testing.T) {
	up := newRecordingUpstream(t)
	f := newForwardFixture(t, keys())
	f.add(t, "plain", up, nil, map[string]string{identityHeader: "pinned-shared"})

	for _, id := range []string{alice, nokey, ""} {
		res := f.call(t, callerCtx(id), "plain.get_whoami")
		if res.IsError || text(res) != "pinned-shared" {
			t.Fatalf("caller %q on the plain server got %q (isError=%v)", id, text(res), res.IsError)
		}
	}
	for _, s := range up.requests() {
		if s.value != "pinned-shared" {
			t.Fatalf("plain server request %s carried %q", s.method, s.value)
		}
	}

	// And with no static header at all: nothing is sent.
	up2 := newRecordingUpstream(t)
	f.add(t, "bare", up2, nil, nil)
	if res := f.call(t, callerCtx(alice), "bare.get_whoami"); res.IsError || text(res) != "" {
		t.Fatalf("bare server saw %q", text(res))
	}
	for _, s := range up2.requests() {
		if s.present {
			t.Fatalf("bare server request %s carried an identity header", s.method)
		}
	}
}

// TestIdentityCallInternalForwards: the trusted in-process path (used to
// register keys) forwards the caller's key from the context, and refuses
// a caller without one.
func TestIdentityCallInternalForwards(t *testing.T) {
	up := newRecordingUpstream(t)
	f := newForwardFixture(t, keys())
	f.add(t, "bk", up, &upstreams.IdentityForwarding{Header: identityHeader, Register: true}, nil)

	res, err := f.gw.CallInternal(callerCtx(bob), "identitykeys", "bk.get_whoami", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || text(res) != "vk-bob-secret" {
		t.Fatalf("CallInternal ran as %q (isError=%v)", text(res), res.IsError)
	}
	if calls := up.assertOnlyToolCallsCarryKey(t); len(calls) != 1 || calls[0] != "vk-bob-secret" {
		t.Fatalf("tools/call headers = %v", calls)
	}

	res, err = f.gw.CallInternal(callerCtx(nokey), "identitykeys", "bk.get_whoami", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || up.count("tools/call") != 1 {
		t.Fatalf("CallInternal without a key: isError=%v, upstream calls=%d", res.IsError, up.count("tools/call"))
	}
	if m, ok := f.met.last("bk.get_whoami"); !ok || m.ErrorClass != "identity" || m.Via != "identitykeys" {
		t.Fatalf("metrics = %+v", m)
	}
}
