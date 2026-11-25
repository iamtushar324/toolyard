package gateway_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/tusharbhardwaj/toolyard/internal/access"
	"github.com/tusharbhardwaj/toolyard/internal/approval"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/gateway"
	"github.com/tusharbhardwaj/toolyard/internal/memory"
	"github.com/tusharbhardwaj/toolyard/internal/metrics"
	"github.com/tusharbhardwaj/toolyard/internal/policy"
	"github.com/tusharbhardwaj/toolyard/internal/realtime"
	"github.com/tusharbhardwaj/toolyard/internal/store/storetest"
	"github.com/tusharbhardwaj/toolyard/internal/toolcaps"
	"github.com/tusharbhardwaj/toolyard/internal/upstreams"
)

// capUpstream is a real MCP server with three read tools: get_ok always
// succeeds, get_flaky fails (isError) when called with fail=true, and
// get_open is marked guest-safe in its _meta. Every tool echoes the
// identity header it saw; calls counts the tools/call requests.
type capUpstream struct {
	srv   *httptest.Server
	calls atomic.Int32
	mu    sync.Mutex
	// headers holds, per tools/call, whether the identity header was
	// present and its value.
	headers []seenRequest
}

func newCapUpstream(t *testing.T) *capUpstream {
	t.Helper()
	u := &capUpstream{}
	m := server.NewMCPServer("caps", "1.0.0")
	echo := func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		v, _ := ctx.Value(seenIdentityKey{}).(string)
		return mcp.NewToolResultText("ran:" + v), nil
	}
	m.AddTool(mcp.NewTool("get_ok", mcp.WithDescription("always works")), echo)
	m.AddTool(mcp.NewTool("get_flaky", mcp.WithDescription("fails on request"), mcp.WithBoolean("fail")),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			if req.GetBool("fail", false) {
				return mcp.NewToolResultError("flaky failed"), nil
			}
			return echo(ctx, req)
		})
	open := mcp.NewTool("get_open", mcp.WithDescription("open to guests"))
	open.Meta = mcp.NewMetaFromMap(map[string]any{gateway.GuestSafeMeta: true})
	m.AddTool(open, echo)
	inner := server.NewStreamableHTTPServer(m, server.WithHTTPContextFunc(
		func(ctx context.Context, r *http.Request) context.Context {
			return context.WithValue(ctx, seenIdentityKey{}, r.Header.Get(identityHeader))
		}))
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if readMethod(req) == "tools/call" {
			u.calls.Add(1)
			vals := req.Header.Values(identityHeader)
			u.mu.Lock()
			u.headers = append(u.headers, seenRequest{method: "tools/call", present: len(vals) > 0, value: req.Header.Get(identityHeader)})
			u.mu.Unlock()
		}
		inner.ServeHTTP(w, req)
	}))
	t.Cleanup(u.srv.Close)
	return u
}

// readMethod is the JSON-RPC method of req; the body stays readable.
func readMethod(req *http.Request) string {
	if req.Body == nil {
		return ""
	}
	body, _ := io.ReadAll(req.Body)
	req.Body = io.NopCloser(bytes.NewReader(body))
	var msg struct {
		Method string `json:"method"`
	}
	_ = json.Unmarshal(body, &msg)
	return msg.Method
}

func (u *capUpstream) lastCall() seenRequest {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.headers) == 0 {
		return seenRequest{}
	}
	return u.headers[len(u.headers)-1]
}

// scopes is an access.Resolver with fixed scopes; unknown callers are
// denied.
type scopes map[string]access.Scope

func (s scopes) ScopeFor(_ context.Context, callerID string) access.Scope {
	if sc, ok := s[callerID]; ok {
		return sc
	}
	return access.Scope{Denied: true}
}

type capFixture struct {
	gw   *gateway.Gateway
	caps *toolcaps.Service
	aud  *audit.Logger
	met  *recMetrics
	up   *capUpstream
	now  *atomic.Pointer[time.Time]
}

const (
	capAdmin = "ag_admin"
	capGuest = "ag_guest"
)

// newCapFixture wires a gateway with caps, an access resolver (admin,
// members alice/bob/nokey/carol, a guest) and the identity resolver of
// identity_forward_test.go, and adds the cap upstream as "up" (identity
// header set when withIdentity).
func newCapFixture(t *testing.T, withIdentity bool) *capFixture {
	t.Helper()
	ctx := context.Background()
	db := storetest.Open(t)
	t.Cleanup(func() { _ = db.Close() })
	bus, err := approval.New(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	member := access.Scope{Groups: map[string]bool{"up": true}}
	res := scopes{
		capAdmin: {All: true},
		alice:    member, bob: member, nokey: member, "ag_carol": member,
		capGuest: {Guest: true, Groups: map[string]bool{"up": true}},
	}
	caps := toolcaps.New(db)
	now := &atomic.Pointer[time.Time]{}
	start := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	now.Store(&start)
	caps.SetClock(func() time.Time { return *now.Load() })
	met := &recMetrics{}
	aud := audit.New(db)
	gw := gateway.New(gateway.Options{
		Policy: policy.New(db), Approval: bus, Audit: aud, Memory: memory.New(db), Hub: realtime.NewHub(),
		Metrics: met, Access: res, Identity: keys(), ToolCaps: caps,
	})
	gw.RegisterBuiltins()
	t.Cleanup(func() { _ = gw.Close() })
	up := newCapUpstream(t)
	srv := upstreams.Server{Name: "up", Transport: "http", URL: up.srv.URL}
	if withIdentity {
		srv.Identity = &upstreams.IdentityForwarding{Header: identityHeader}
	}
	if _, err := upstreams.New(db, gw).Add(ctx, srv); err != nil {
		t.Fatalf("add upstream: %v", err)
	}
	return &capFixture{gw: gw, caps: caps, aud: aud, met: met, up: up, now: now}
}

func (f *capFixture) call(t *testing.T, caller, tool string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	full := map[string]any{gateway.ReasonField: fwdReason}
	for k, v := range args {
		full[k] = v
	}
	res, err := f.gw.RouteCall(callerCtx(caller), "test", tool, full)
	if err != nil {
		t.Fatalf("RouteCall %s: %v", tool, err)
	}
	return res
}

func (f *capFixture) setCap(t *testing.T, tool string, per, total int64) {
	t.Helper()
	if _, err := f.caps.Set(context.Background(), gateway.ToolCap{Upstream: "up", Tool: tool, PerCallerDaily: per, TotalDaily: total}, "u_admin"); err != nil {
		t.Fatal(err)
	}
}

// capRefusals are the refused call.failed audit rows of one caller whose
// summary says a cap refused them.
func (f *capFixture) capRefusals(t *testing.T, caller string) []audit.Event {
	t.Helper()
	evs, err := f.aud.Query(context.Background(), audit.Filter{EventType: audit.EventCallFailed, AgentID: caller, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	var out []audit.Event
	for _, e := range evs {
		if strings.HasPrefix(e.ResultSummary, "cap: ") {
			out = append(out, e)
		}
	}
	return out
}

func mustCapRefusal(t *testing.T, res *mcp.CallToolResult, tool, scope string) {
	t.Helper()
	if !res.IsError {
		t.Fatalf("call over the cap ran: %s", text(res))
	}
	msg := text(res)
	for _, want := range []string{tool, "daily cap", "00:00 UTC", "was not made"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal %q does not mention %q", msg, want)
		}
	}
	sc, _ := res.StructuredContent.(map[string]any)
	if sc["status"] != "cap_reached" || sc["scope"] != scope {
		t.Errorf("structured = %v, want cap_reached/%s", sc, scope)
	}
}

// TestCapCountsOnlySuccessfulCalls: failed calls reach the upstream and do
// not use the cap; once the successful calls reach it, the next call is
// refused before the upstream is contacted, audited with the caller, and
// its metric says "cap".
func TestCapCountsOnlySuccessfulCalls(t *testing.T) {
	f := newCapFixture(t, false)
	f.setCap(t, "up.get_flaky", 1, 0)

	for i := 0; i < 3; i++ {
		if res := f.call(t, alice, "up.get_flaky", map[string]any{"fail": true}); !res.IsError {
			t.Fatalf("failing call %d succeeded: %s", i, text(res))
		}
	}
	if got := f.up.calls.Load(); got != 3 {
		t.Fatalf("upstream saw %d calls, want 3 (failures do not use the cap)", got)
	}
	if res := f.call(t, alice, "up.get_flaky", nil); res.IsError {
		t.Fatalf("first successful call refused: %s", text(res))
	}
	res := f.call(t, alice, "up.get_flaky", nil)
	mustCapRefusal(t, res, "up.get_flaky", gateway.CapScopePerCaller)
	if got := f.up.calls.Load(); got != 4 {
		t.Fatalf("upstream saw %d calls, want 4: the refused call must not reach it", got)
	}
	if m, ok := f.met.last("up.get_flaky"); !ok || m.Outcome != metrics.OutcomeError || m.ErrorClass != "cap" {
		t.Fatalf("metrics = %+v, want error/cap", m)
	}
	refusals := f.capRefusals(t, alice)
	if len(refusals) != 1 || refusals[0].ToolName != "up.get_flaky" || refusals[0].Decision != "refused" {
		t.Fatalf("cap refusal audit rows = %+v", refusals)
	}
	// The successful capped call is on record as such.
	ok, err := f.aud.Query(context.Background(), audit.Filter{EventType: audit.EventCallSucceeded, AgentID: alice, Limit: 10})
	if err != nil || len(ok) != 1 || ok[0].ToolName != "up.get_flaky" {
		t.Fatalf("call.succeeded rows for alice = %+v, %v", ok, err)
	}
	// Another caller has their own per-caller cap.
	if res := f.call(t, bob, "up.get_flaky", nil); res.IsError {
		t.Fatalf("bob refused by alice's cap: %s", text(res))
	}
	// An uncapped tool is untouched.
	if res := f.call(t, alice, "up.get_ok", nil); res.IsError {
		t.Fatalf("uncapped tool refused: %s", text(res))
	}
}

// TestCapTotalAcrossCallers: the total limit holds for everyone together,
// and the refusal says it is the total.
func TestCapTotalAcrossCallers(t *testing.T) {
	f := newCapFixture(t, false)
	f.setCap(t, "up.get_ok", 0, 2)
	for _, c := range []string{alice, bob} {
		if res := f.call(t, c, "up.get_ok", nil); res.IsError {
			t.Fatalf("%s refused under the total: %s", c, text(res))
		}
	}
	before := f.up.calls.Load()
	mustCapRefusal(t, f.call(t, "ag_carol", "up.get_ok", nil), "up.get_ok", gateway.CapScopeTotal)
	if f.up.calls.Load() != before {
		t.Fatal("the refused call reached the upstream")
	}
	if !strings.Contains(text(f.call(t, alice, "up.get_ok", nil)), "everyone together") {
		t.Fatal("total refusal does not say it is everyone's limit")
	}
}

// TestCapResetsAtUTCDayBoundary: a cap reached today opens at 00:00 UTC.
func TestCapResetsAtUTCDayBoundary(t *testing.T) {
	f := newCapFixture(t, false)
	late := time.Date(2026, 11, 25, 23, 59, 30, 0, time.UTC)
	f.now.Store(&late)
	f.setCap(t, "up.get_ok", 1, 0)
	if res := f.call(t, alice, "up.get_ok", nil); res.IsError {
		t.Fatalf("first call refused: %s", text(res))
	}
	res := f.call(t, alice, "up.get_ok", nil)
	mustCapRefusal(t, res, "up.get_ok", gateway.CapScopePerCaller)
	if sc, _ := res.StructuredContent.(map[string]any); sc["reset_at"] != "2026-11-26T00:00:00Z" {
		t.Fatalf("reset_at = %v", sc["reset_at"])
	}
	next := time.Date(2026, 11, 26, 0, 0, 5, 0, time.UTC)
	f.now.Store(&next)
	if res := f.call(t, alice, "up.get_ok", nil); res.IsError {
		t.Fatalf("call after 00:00 UTC refused: %s", text(res))
	}
}

// TestCapToolsAdminOnly: only an admin's agent changes caps; a member's
// attempt is refused and audited; everyone may list the caps they reach.
func TestCapToolsAdminOnly(t *testing.T) {
	f := newCapFixture(t, false)
	args := map[string]any{"tool": "up.get_ok", "per_caller_daily": 3}
	res := f.call(t, alice, "policies.set_cap", args)
	if !res.IsError || !strings.Contains(text(res), "only an admin") {
		t.Fatalf("member set a cap: %s", text(res))
	}
	if list, _ := f.caps.List(context.Background()); len(list) != 0 {
		t.Fatalf("member's change was saved: %+v", list)
	}
	evs, _ := f.aud.Query(context.Background(), audit.Filter{EventType: gateway.EventToolCapChange, AgentID: alice, Limit: 5})
	if len(evs) != 1 || evs[0].Decision != "refused" {
		t.Fatalf("member's attempt audit = %+v", evs)
	}

	res = f.call(t, capAdmin, "policies.set_cap", args)
	if res.IsError {
		t.Fatalf("admin set_cap: %s", text(res))
	}
	list, _ := f.caps.List(context.Background())
	if len(list) != 1 || list[0].Upstream != "up" || list[0].Tool != "up.get_ok" || list[0].PerCallerDaily != 3 {
		t.Fatalf("saved caps = %+v", list)
	}
	f.call(t, alice, "up.get_ok", nil)
	listed := f.call(t, alice, "policies.list_caps", nil)
	sc, _ := listed.StructuredContent.(map[string]any)
	rows, _ := sc["caps"].([]map[string]any)
	if listed.IsError || len(rows) != 1 || rows[0]["tool"] != "up.get_ok" || rows[0]["used_today_mine"] != int64(1) || rows[0]["used_today_total"] != int64(1) {
		t.Fatalf("list_caps for alice = %s", text(listed))
	}
	if res := f.call(t, alice, "policies.clear_cap", map[string]any{"tool": "up.get_ok"}); !res.IsError {
		t.Fatal("member cleared a cap")
	}
	if res := f.call(t, capAdmin, "policies.clear_cap", map[string]any{"tool": "up.get_ok"}); res.IsError {
		t.Fatalf("admin clear_cap: %s", text(res))
	}
	if list, _ := f.caps.List(context.Background()); len(list) != 0 {
		t.Fatalf("caps after clear = %+v", list)
	}
}

// TestGuestPassThroughOnlyForGuestSafeTools: on an identity-forwarding
// upstream a guest calls a guest-safe tool with no identity header, is
// refused on any other tool, and a member without a key is still refused
// even on the guest-safe tool. A member with a key runs as themselves.
func TestGuestPassThroughOnlyForGuestSafeTools(t *testing.T) {
	f := newCapFixture(t, true)

	res := f.call(t, capGuest, "up.get_open", nil)
	if res.IsError || text(res) != "ran:" {
		t.Fatalf("guest on a guest-safe tool: %q (isError=%v)", text(res), res.IsError)
	}
	if last := f.up.lastCall(); last.present {
		t.Fatalf("guest call carried the identity header %q", last.value)
	}

	before := f.up.calls.Load()
	res = f.call(t, capGuest, "up.get_ok", nil)
	if !res.IsError || !strings.Contains(text(res), "not open to guests") {
		t.Fatalf("guest on an unmarked tool: %q (isError=%v)", text(res), res.IsError)
	}
	if f.up.calls.Load() != before {
		t.Fatal("the guest's refused call reached the upstream")
	}
	if m, ok := f.met.last("up.get_ok"); !ok || m.ErrorClass != "identity" {
		t.Fatalf("metrics = %+v", m)
	}

	res = f.call(t, nokey, "up.get_open", nil)
	if !res.IsError || !strings.Contains(text(res), "no Beknown key yet") {
		t.Fatalf("member without a key on a guest-safe tool: %q (isError=%v)", text(res), res.IsError)
	}
	if f.up.calls.Load() != before {
		t.Fatal("the keyless member's refused call reached the upstream")
	}

	res = f.call(t, alice, "up.get_open", nil)
	if res.IsError || text(res) != "ran:vk-alice-secret" {
		t.Fatalf("alice on the guest-safe tool ran as %q (isError=%v)", text(res), res.IsError)
	}
}

// TestGuestSafeMarker: only the JSON value true marks a tool.
func TestGuestSafeMarker(t *testing.T) {
	for _, c := range []struct {
		meta *mcp.Meta
		want bool
	}{
		{nil, false},
		{mcp.NewMetaFromMap(map[string]any{}), false},
		{mcp.NewMetaFromMap(map[string]any{gateway.GuestSafeMeta: "true"}), false},
		{mcp.NewMetaFromMap(map[string]any{gateway.GuestSafeMeta: false}), false},
		{mcp.NewMetaFromMap(map[string]any{gateway.GuestSafeMeta: true}), true},
	} {
		if got := gateway.GuestSafe(mcp.Tool{Name: "x", Meta: c.meta}); got != c.want {
			t.Errorf("GuestSafe(%+v) = %v, want %v", c.meta, got, c.want)
		}
	}
}
