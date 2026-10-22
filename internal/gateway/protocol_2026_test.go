package gateway

// TEC-1628: the gateway serves MCP 2026-07-28 (stateless, no initialize)
// and 2025-11-25 on one endpoint, and its upstream connections stay on
// 2025-11-25 unless -upstream-protocol=auto.

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
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// wireLog counts what crossed an HTTP MCP endpoint: JSON-RPC methods, and
// requests that carried an Mcp-Session-Id.
type wireLog struct {
	mu          sync.Mutex
	methods     map[string]int
	withSession int
}

func (w *wireLog) count(method string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.methods[method]
}

func (w *wireLog) sessions() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.withSession
}

// recordWire serves h and logs every request. intercept, when set, may
// answer a request itself (it returns true) before h sees it.
func recordWire(t *testing.T, h http.Handler, intercept func(method string, id json.RawMessage, w http.ResponseWriter) bool) (*httptest.Server, *wireLog) {
	t.Helper()
	log := &wireLog{methods: map[string]int{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if r.Body != nil {
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &msg)
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		log.mu.Lock()
		if msg.Method != "" {
			log.methods[msg.Method]++
		}
		if r.Header.Get("Mcp-Session-Id") != "" {
			log.withSession++
		}
		log.mu.Unlock()
		if intercept != nil && intercept(msg.Method, msg.ID, w) {
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, log
}

// serveGateway puts the gateway's MCP server behind a stateless streamable
// HTTP endpoint, as toolyard runs it, with every request from agentID.
func serveGateway(t *testing.T, g *Gateway, agentID string) (*httptest.Server, *wireLog) {
	t.Helper()
	h := server.NewStreamableHTTPServer(g.MCPServer(),
		server.WithStateLess(true),
		server.WithHTTPContextFunc(func(ctx context.Context, _ *http.Request) context.Context {
			return WithAgentID(ctx, agentID)
		}),
	)
	return recordWire(t, h, nil)
}

// dialMCP connects an mcp-go client. version "" lets the client negotiate
// (2026-07-28 when the server offers it).
func dialMCP(t *testing.T, url, version, name, clientVersion string) *client.Client {
	t.Helper()
	tr, err := transport.NewStreamableHTTP(url)
	if err != nil {
		t.Fatal(err)
	}
	c := client.NewClient(tr)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	req := mcp.InitializeRequest{}
	req.Params.ProtocolVersion = version
	req.Params.ClientInfo = mcp.Implementation{Name: name, Version: clientVersion}
	if _, err := c.Initialize(ctx, req); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	return c
}

func callText(t *testing.T, c *client.Client, tool string) string {
	t.Helper()
	req := mcp.CallToolRequest{}
	req.Params.Name = tool
	req.Params.Arguments = map[string]any{ReasonField: testReason}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := c.CallTool(ctx, req)
	if err != nil {
		t.Fatalf("call %s: %v", tool, err)
	}
	var sb strings.Builder
	for _, content := range res.Content {
		if tc, ok := content.(mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}

// A 2026-07-28 client lists and calls tools without initialize or a
// session, gets private cache hints on tools/list, and its clientInfo
// (sent in each request's _meta) names it on the call's raiser.
func TestModernClientWithoutInitialize(t *testing.T) {
	f := newActorFixture(t)
	srv, wire := serveGateway(t, f.gw, "ag_modern")
	c := dialMCP(t, srv.URL, "", "t3-code", "2.0")
	if v := c.ProtocolVersion(); v != mcp.ProtocolVersion20260728 {
		t.Fatalf("negotiated %q, want %q", v, mcp.ProtocolVersion20260728)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	page, err := c.ListToolsByPage(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	found := false
	for _, tl := range page.Tools {
		found = found || tl.Name == "t.get_status"
	}
	if !found {
		t.Fatalf("t.get_status missing from tools/list (%d tools)", len(page.Tools))
	}
	if page.TTLMs == nil || *page.TTLMs != toolsListCacheTTL.Milliseconds() || page.CacheScope != mcp.CacheScopePrivate {
		t.Fatalf("tools/list cache hints: ttlMs=%v scope=%q, want %d private", page.TTLMs, page.CacheScope, toolsListCacheTTL.Milliseconds())
	}

	if got := callText(t, c, "t.get_status"); got != "ran" {
		t.Fatalf("tools/call answered %q", got)
	}
	r, _ := f.lastRaiser(t)
	if r.ClientName != "t3-code/2.0" || r.ClientKind != "t3" {
		t.Fatalf("raiser client = %q/%q, want t3-code/2.0 and t3", r.ClientName, r.ClientKind)
	}
	if n := wire.count("initialize"); n != 0 {
		t.Fatalf("a 2026-07-28 client sent initialize %d times", n)
	}
	if wire.count("server/discover") == 0 {
		t.Fatal("the client never sent server/discover")
	}
	if n := wire.sessions(); n != 0 {
		t.Fatalf("%d requests carried an Mcp-Session-Id", n)
	}
}

// postRaw sends one JSON-RPC message to an MCP endpoint and decodes the
// answer (plain JSON or a one-event SSE stream).
func postRaw(t *testing.T, url string, headers map[string]string, body string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(line, "data:") {
				raw = []byte(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
				break
			}
		}
	}
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

const modernListBody = `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{` +
	`"io.modelcontextprotocol/protocolVersion":"2026-07-28",` +
	`"io.modelcontextprotocol/clientCapabilities":{},` +
	`"io.modelcontextprotocol/clientInfo":{"name":"raw","version":"1"}}}}`

// The 2026-07-28 header rules on the gateway's endpoint: a well-formed
// request gets resultType complete; a request whose Mcp-Method header
// differs from its body, or whose version is only in _meta, is refused
// with 400 before anything runs.
func TestModernRequestHeaderChecks(t *testing.T) {
	f := newActorFixture(t)
	srv, _ := serveGateway(t, f.gw, "ag_raw")

	status, out := postRaw(t, srv.URL, map[string]string{
		"Mcp-Protocol-Version": "2026-07-28", "Mcp-Method": "tools/list",
	}, modernListBody)
	result, _ := out["result"].(map[string]any)
	if status != http.StatusOK || result == nil {
		t.Fatalf("well-formed tools/list: %d %v", status, out)
	}
	if result["resultType"] != "complete" || result["cacheScope"] != "private" {
		t.Fatalf("tools/list result: resultType=%v cacheScope=%v", result["resultType"], result["cacheScope"])
	}

	if status, out := postRaw(t, srv.URL, map[string]string{
		"Mcp-Protocol-Version": "2026-07-28", "Mcp-Method": "tools/call",
	}, modernListBody); status != http.StatusBadRequest || out["error"] == nil {
		t.Fatalf("Mcp-Method mismatch: %d %v, want 400 with an error", status, out)
	}
	if status, out := postRaw(t, srv.URL, map[string]string{
		"Mcp-Method": "tools/list",
	}, modernListBody); status != http.StatusBadRequest || out["error"] == nil {
		t.Fatalf("version only in _meta: %d %v, want 400 with an error", status, out)
	}
}

// A 2025-11-25 client on the same endpoint still initializes, lists and
// calls, and the initialize clientInfo still names it.
func TestLegacyClientOnSameEndpoint(t *testing.T) {
	f := newActorFixture(t)
	srv, wire := serveGateway(t, f.gw, "ag_legacy")
	c := dialMCP(t, srv.URL, mcp.LATEST_LEGACY_PROTOCOL_VERSION, "claude-code", "1.0")
	if v := c.ProtocolVersion(); v != mcp.ProtocolVersion20251125 {
		t.Fatalf("negotiated %q, want %q", v, mcp.ProtocolVersion20251125)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := c.ListTools(ctx, mcp.ListToolsRequest{}); err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	if got := callText(t, c, "t.get_status"); got != "ran" {
		t.Fatalf("tools/call answered %q", got)
	}
	r, _ := f.lastRaiser(t)
	if r.ClientName != "claude-code/1.0" || r.ClientKind != "claude_code" {
		t.Fatalf("raiser client = %q/%q, want claude-code/1.0 and claude_code", r.ClientName, r.ClientKind)
	}
	if wire.count("initialize") != 1 || wire.count("server/discover") != 0 {
		t.Fatalf("initialize=%d server/discover=%d, want 1 and 0", wire.count("initialize"), wire.count("server/discover"))
	}
}

func okTool(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return mcp.NewToolResultText("ok"), nil
}

func hasTool(g *Gateway, name string) bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	_, ok := g.tools[name]
	return ok
}

// An upstream that pages its catalog two tools at a time loads all five,
// over a 2025-11-25 connection (the default: no server/discover).
func TestUpstreamCatalogLoadsEveryPage(t *testing.T) {
	m := server.NewMCPServer("paged", "1.0.0", server.WithPaginationLimit(2))
	for i := 1; i <= 5; i++ {
		m.AddTool(mcp.NewTool(fmt.Sprintf("tool_%d", i)), okTool)
	}
	ts, wire := recordWire(t, server.NewStreamableHTTPServer(m), nil)
	f := newActorFixture(t)
	if err := f.gw.AddUpstream(context.Background(), UpstreamConfig{Name: "paged", Transport: "http", URL: ts.URL}); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 5; i++ {
		if name := fmt.Sprintf("paged.tool_%d", i); !hasTool(f.gw, name) {
			t.Fatalf("%s missing from the catalog", name)
		}
	}
	if n := wire.count("tools/list"); n < 3 {
		t.Fatalf("tools/list sent %d times, want one per page (3)", n)
	}
	if wire.count("server/discover") != 0 || wire.count("initialize") != 1 {
		t.Fatalf("default upstream: server/discover=%d initialize=%d, want 0 and 1", wire.count("server/discover"), wire.count("initialize"))
	}
	f.gw.mu.RLock()
	u := f.gw.upstreams["paged"]
	f.gw.mu.RUnlock()
	if u.modern.Load() || !u.pooled() {
		t.Fatal("a default upstream must stay on 2025-11-25, in the pool")
	}
}

func newOKUpstream(t *testing.T, intercept func(string, json.RawMessage, http.ResponseWriter) bool) (*httptest.Server, *wireLog) {
	t.Helper()
	m := server.NewMCPServer("up", "1.0.0")
	m.AddTool(mcp.NewTool("get_ok"), okTool)
	return recordWire(t, server.NewStreamableHTTPServer(m), intercept)
}

func addedUpstream(t *testing.T, g *Gateway, name, url string) *upstream {
	t.Helper()
	if err := g.AddUpstream(context.Background(), UpstreamConfig{Name: name, Transport: "http", URL: url}); err != nil {
		t.Fatal(err)
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.upstreams[name]
}

// -upstream-protocol=auto: an upstream that offers 2026-07-28 is called
// statelessly (no initialize, no session) and stays out of the pool.
func TestAutoUpstreamUsesModernProtocol(t *testing.T) {
	ts, wire := newOKUpstream(t, nil)
	f := newActorFixture(t)
	f.gw.upstreamProtocol = ProtocolAuto
	u := addedUpstream(t, f.gw, "up", ts.URL)
	if !u.modern.Load() || u.pooled() || u.settled != mcp.ProtocolVersion20260728 {
		t.Fatalf("modern=%v pooled=%v settled=%q, want true false %q", u.modern.Load(), u.pooled(), u.settled, mcp.ProtocolVersion20260728)
	}
	res, err := u.callTool(context.Background(), "get_ok", nil)
	if err != nil || res.IsError {
		t.Fatalf("call: %v %+v", err, res)
	}
	if wire.count("initialize") != 0 || wire.sessions() != 0 {
		t.Fatalf("initialize=%d session requests=%d, want 0 and 0", wire.count("initialize"), wire.sessions())
	}
	if u.suspend() {
		t.Fatal("a 2026-07-28 connection was suspended")
	}
}

// -upstream-protocol=auto against a server that predates server/discover:
// one probe, then initialize at 2025-11-25; a re-dial after an idle
// suspend asks for 2025-11-25 straight away (the era is cached).
func TestAutoUpstreamFallsBackAndCachesEra(t *testing.T) {
	ts, wire := newOKUpstream(t, func(method string, id json.RawMessage, w http.ResponseWriter) bool {
		if method != "server/discover" {
			return false
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"Method not found"}}`, id)
		return true
	})
	f := newActorFixture(t)
	f.gw.upstreamProtocol = ProtocolAuto
	u := addedUpstream(t, f.gw, "old", ts.URL)
	if u.modern.Load() || !u.pooled() || u.settled != mcp.ProtocolVersion20251125 {
		t.Fatalf("modern=%v pooled=%v settled=%q, want false true %q", u.modern.Load(), u.pooled(), u.settled, mcp.ProtocolVersion20251125)
	}
	if wire.count("server/discover") != 1 || wire.count("initialize") != 1 {
		t.Fatalf("server/discover=%d initialize=%d, want 1 and 1", wire.count("server/discover"), wire.count("initialize"))
	}
	if !u.suspend() {
		t.Fatal("an idle 2025-11-25 connection should suspend")
	}
	if res, err := u.callTool(context.Background(), "get_ok", nil); err != nil || res.IsError {
		t.Fatalf("call after suspend: %v %+v", err, res)
	}
	if wire.count("server/discover") != 1 || wire.count("initialize") != 2 {
		t.Fatalf("after re-dial: server/discover=%d initialize=%d, want 1 and 2", wire.count("server/discover"), wire.count("initialize"))
	}
}

// hidingVisibility hides one tool from tools/list and from direct calls,
// as router_only and top_n modes do.
type hidingVisibility struct{ hidden string }

func (v hidingVisibility) List(_ context.Context, tools []mcp.Tool) []mcp.Tool {
	out := make([]mcp.Tool, 0, len(tools))
	for _, t := range tools {
		if t.Name != v.hidden {
			out = append(out, t)
		}
	}
	return out
}

func (v hidingVisibility) IsVisible(_ context.Context, name string) bool { return name != v.hidden }

// mcp-go v1 runs server tool filters on tools/call too. The visibility
// filter is a tools/list hook instead, so a direct call to a tool the
// list hides still reaches the gateway, which points at tools.execute.
func TestListHiddenToolStillAnswersDirectCall(t *testing.T) {
	f := newAccessFixture(t, hidingVisibility{hidden: "beta.get_status"})
	names := mcpToolsList(t, f.gw, f.admin)
	if has(names, "beta.get_status") || !has(names, "alpha.get_status") {
		t.Fatalf("admin tools/list: %v", names)
	}
	r := mcpToolsCall(t, f.gw, f.admin, "beta.get_status")
	if r.rpcErr || !r.isError || !strings.Contains(r.text, "hidden by the current agent-surface mode") {
		t.Fatalf("direct call to a list-hidden tool: %s", r.raw)
	}
	if r := mcpToolsCall(t, f.gw, f.member, "beta.get_status"); !r.rpcErr || r.message != rpcNotFoundMessage("beta.get_status") {
		t.Fatalf("ungranted direct call: %s", r.raw)
	}
}

// mcp-go's AddTool panics on invalid x-mcp-header annotations; an upstream
// tool that carries them is left out and its siblings still register.
func TestUpstreamToolWithInvalidHeaderAnnotationIsSkipped(t *testing.T) {
	f := newActorFixture(t)
	good := mcp.NewTool("get_good")
	bad := mcp.Tool{Name: "get_bad", InputSchema: mcp.ToolInputSchema{
		Type:       "object",
		Properties: map[string]any{"q": map[string]any{"type": "object", "x-mcp-header": "Q"}},
	}}
	f.gw.registerUpstreamTools("hdr", []mcp.Tool{good, bad}, nil)
	if !hasTool(f.gw, "hdr.get_good") {
		t.Fatal("the valid tool was not registered")
	}
	if hasTool(f.gw, "hdr.get_bad") {
		t.Fatal("the tool with an invalid x-mcp-header was registered")
	}
}

// A 2026-07-28 client on a stateful endpoint (toolyard's default,
// -stateless-mcp=false) also works without initialize or a session.
func TestModernClientOnStatefulEndpoint(t *testing.T) {
	f := newActorFixture(t)
	h := server.NewStreamableHTTPServer(f.gw.MCPServer(),
		server.WithHTTPContextFunc(func(ctx context.Context, _ *http.Request) context.Context {
			return WithAgentID(ctx, "ag_stateful")
		}),
	)
	srv, wire := recordWire(t, h, nil)
	c := dialMCP(t, srv.URL, "", "t3-code", "2.0")
	if v := c.ProtocolVersion(); v != mcp.ProtocolVersion20260728 {
		t.Fatalf("negotiated %q, want %q", v, mcp.ProtocolVersion20260728)
	}
	if got := callText(t, c, "t.get_status"); got != "ran" {
		t.Fatalf("tools/call answered %q", got)
	}
	if wire.count("initialize") != 0 || wire.sessions() != 0 {
		t.Fatalf("initialize=%d session requests=%d, want 0 and 0", wire.count("initialize"), wire.sessions())
	}
}

// -upstream-protocol=auto against an mcp-go v1 server restricted to the
// session-based versions: it answers the probe with
// UNSUPPORTED_PROTOCOL_VERSION, and the dial goes on with initialize at
// 2025-11-25.
func TestAutoUpstreamFallsBackOnUnsupportedVersion(t *testing.T) {
	m := server.NewMCPServer("up", "1.0.0")
	m.AddTool(mcp.NewTool("get_ok"), okTool)
	ts, wire := recordWire(t, server.NewStreamableHTTPServer(m,
		server.WithStreamableHTTPProtocolVersions(mcp.LegacyProtocolVersions()...)), nil)
	f := newActorFixture(t)
	f.gw.upstreamProtocol = ProtocolAuto
	u := addedUpstream(t, f.gw, "pinned", ts.URL)
	if u.modern.Load() || !u.pooled() || u.settled != mcp.ProtocolVersion20251125 {
		t.Fatalf("modern=%v pooled=%v settled=%q, want false true %q", u.modern.Load(), u.pooled(), u.settled, mcp.ProtocolVersion20251125)
	}
	if wire.count("server/discover") != 1 || wire.count("initialize") != 1 {
		t.Fatalf("server/discover=%d initialize=%d, want 1 and 1", wire.count("server/discover"), wire.count("initialize"))
	}
	if res, err := u.callTool(context.Background(), "get_ok", nil); err != nil || res.IsError {
		t.Fatalf("call: %v %+v", err, res)
	}
}

// sessionOnly turns away every POST without an Mcp-Session-Id, initialize
// aside, as a 2025-11-25 server does: HTTP 400 with JSON-RPC -32000, byte
// for byte what beknown-services mcp-server sends.
func sessionOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		var msg struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &msg)
		if r.Method == http.MethodPost && msg.Method != string(mcp.MethodInitialize) && r.Header.Get("Mcp-Session-Id") == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","error":{"code":-32000,"message":"Invalid session or missing initialization"},"id":null}`)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// An auto upstream that spoke 2026-07-28 and was then rolled back to a
// session-based server: the call that the server turns away for want of a
// session probes again, settles on 2025-11-25 and is retried once.
func TestAutoUpstreamRedialsAfterRollback(t *testing.T) {
	m := server.NewMCPServer("up", "1.0.0")
	m.AddTool(mcp.NewTool("get_ok"), okTool)
	var mu sync.Mutex
	current := http.Handler(server.NewStreamableHTTPServer(m))
	ts, wire := recordWire(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		h := current
		mu.Unlock()
		h.ServeHTTP(w, r)
	}), nil)
	f := newActorFixture(t)
	f.gw.upstreamProtocol = ProtocolAuto
	u := addedUpstream(t, f.gw, "up", ts.URL)
	if !u.modern.Load() {
		t.Fatal("the upstream should start on 2026-07-28")
	}

	mu.Lock()
	current = sessionOnly(server.NewStreamableHTTPServer(m,
		server.WithStreamableHTTPProtocolVersions(mcp.LegacyProtocolVersions()...)))
	mu.Unlock()
	res, err := u.callTool(context.Background(), "get_ok", nil)
	if err != nil || res.IsError {
		t.Fatalf("call after the rollback: %v %+v", err, res)
	}
	u.dialMu.Lock()
	settled := u.settled
	u.dialMu.Unlock()
	if u.modern.Load() || !u.pooled() || settled != mcp.ProtocolVersion20251125 {
		t.Fatalf("modern=%v pooled=%v settled=%q, want false true %q", u.modern.Load(), u.pooled(), settled, mcp.ProtocolVersion20251125)
	}
	if wire.count("server/discover") != 2 || wire.count("initialize") != 1 {
		t.Fatalf("server/discover=%d initialize=%d, want 2 (a fresh probe) and 1", wire.count("server/discover"), wire.count("initialize"))
	}
	if got := u.recoveries.Load(); got != 1 {
		t.Fatalf("recoveries = %d, want 1", got)
	}
}

// noUsersAuth is a PerUserAuth with nobody signed in.
type noUsersAuth struct{}

func (noUsersAuth) ConnectedUsers(context.Context, string) ([]string, error)     { return nil, nil }
func (noUsersAuth) UserConnected(context.Context, string, string) (bool, error)  { return false, nil }
func (noUsersAuth) FreshenUserToken(context.Context, string, string) error       { return nil }
func (noUsersAuth) RefreshUserToken(context.Context, string, string) error       { return nil }
func (noUsersAuth) MarkUserUnauthorized(context.Context, string, string, string) {}

// per_user connections stay on 2025-11-25 in auto mode.
func TestPerUserUpstreamStaysLegacyInAutoMode(t *testing.T) {
	f := newActorFixture(t)
	f.gw.upstreamProtocol = ProtocolAuto
	err := f.gw.AddUpstream(context.Background(), UpstreamConfig{
		Name: "pu", Transport: "http", URL: "http://127.0.0.1:1", PerUser: true, PerUserAuth: noUsersAuth{},
	})
	if !errors.Is(err, ErrWaitingForSignIn) {
		t.Fatalf("add: %v, want ErrWaitingForSignIn", err)
	}
	f.gw.mu.RLock()
	pu := f.gw.perUser["pu"]
	f.gw.mu.RUnlock()
	if pu == nil {
		t.Fatal("the per_user upstream was not added")
	}
	if pu.cfg.ProtocolVersion != "" {
		t.Fatalf("per_user config protocol = %q, want \"\" (2025-11-25)", pu.cfg.ProtocolVersion)
	}
}

// A 2026-07-28 call that the access hook denies still names its client:
// the hook runs before mcp-go parses the request's _meta, so it reads the
// clientInfo from the raw message.
func TestDeniedModernCallKeepsClientName(t *testing.T) {
	f := newAccessFixture(t, nil)
	msg, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": "tools/call",
		"params": map[string]any{
			"name":      "beta.get_status",
			"arguments": map[string]any{ReasonField: testReason},
			"_meta": map[string]any{
				mcp.MetaKeyProtocolVersion: mcp.ProtocolVersion20260728,
				mcp.MetaKeyClientInfo:      map[string]any{"name": "t3-code", "version": "2.0"},
			},
		},
	})
	f.gw.MCPServer().HandleMessage(f.member, json.RawMessage(msg))
	f.met.mu.Lock()
	defer f.met.mu.Unlock()
	for _, e := range f.met.events {
		if e.ToolName == "beta.get_status" {
			if e.ClientKind != "t3" {
				t.Fatalf("denied call client kind = %q, want t3", e.ClientKind)
			}
			return
		}
	}
	t.Fatal("no metrics event for the denied call")
}
