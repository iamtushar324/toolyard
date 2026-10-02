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
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/tusharbhardwaj/toolyard/internal/approval"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/gateway"
	"github.com/tusharbhardwaj/toolyard/internal/memory"
	"github.com/tusharbhardwaj/toolyard/internal/policy"
	"github.com/tusharbhardwaj/toolyard/internal/realtime"
	"github.com/tusharbhardwaj/toolyard/internal/store"
	"github.com/tusharbhardwaj/toolyard/internal/upstreams"
)

// Per-user upstreams, through the real upstreams.Service so the header
// composition under test is toCfg's. The token store is a fake keyed by
// (upstream, user); the gateway contract is what is checked here. The
// full flow with a real oauth.Service and a fake IdP is in
// internal/upstreams.

const puReason = "per-user test: prove which account the call ran as"

// bearerRequest is one HTTP request the recording upstream saw.
type bearerRequest struct {
	method  string
	bearer  string // Authorization value, "" when absent
	session string // Mcp-Session-Id
}

// bearerUpstream is an mcp-go streamable-HTTP server whose get_whoami tool
// echoes the Authorization header the server saw, behind a handler that
// records every request and can answer 401 to a chosen bearer.
type bearerUpstream struct {
	srv  *httptest.Server
	mu   sync.Mutex
	seen []bearerRequest
	deny map[string]bool // bearers answered with 401
}

type seenBearerKey struct{}

func newBearerUpstream(t *testing.T) *bearerUpstream {
	t.Helper()
	m := server.NewMCPServer("per-user", "1.0.0")
	m.AddTool(mcp.NewTool("get_whoami", mcp.WithDescription("echo the bearer")),
		func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			v, _ := ctx.Value(seenBearerKey{}).(string)
			return mcp.NewToolResultText(v), nil
		})
	inner := server.NewStreamableHTTPServer(m, server.WithHTTPContextFunc(
		func(ctx context.Context, r *http.Request) context.Context {
			return context.WithValue(ctx, seenBearerKey{}, r.Header.Get("Authorization"))
		}))
	b := &bearerUpstream{deny: map[string]bool{}}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var method string
		if r.Body != nil {
			body, _ := io.ReadAll(r.Body)
			var msg struct {
				Method string `json:"method"`
			}
			_ = json.Unmarshal(body, &msg)
			method = msg.Method
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		auth := r.Header.Get("Authorization")
		b.mu.Lock()
		b.seen = append(b.seen, bearerRequest{method: method, bearer: auth, session: r.Header.Get("Mcp-Session-Id")})
		denied := b.deny[auth]
		b.mu.Unlock()
		if denied {
			w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(b.srv.Close)
	return b
}

func (b *bearerUpstream) requests() []bearerRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]bearerRequest(nil), b.seen...)
}

func (b *bearerUpstream) count(method string) int {
	n := 0
	for _, r := range b.requests() {
		if r.method == method {
			n++
		}
	}
	return n
}

// bearersOn returns the distinct Authorization values seen on method.
func (b *bearerUpstream) bearersOn(method string) []string {
	set := map[string]bool{}
	for _, r := range b.requests() {
		if r.method == method {
			set[r.bearer] = true
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// sessionsByBearer maps each MCP session id to the set of bearers that
// used it: a per-user upstream must show exactly one bearer per session.
// The bodiless DELETE mcp-go sends on Close carries no headers from the
// header function (in shared mode too) and names nobody, so it is skipped.
func (b *bearerUpstream) sessionsByBearer() map[string]map[string]bool {
	out := map[string]map[string]bool{}
	for _, r := range b.requests() {
		if r.session == "" || r.method == "" {
			continue
		}
		if out[r.session] == nil {
			out[r.session] = map[string]bool{}
		}
		out[r.session][r.bearer] = true
	}
	return out
}

func (b *bearerUpstream) denyBearer(v string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.deny[v] = true
}

// fakeUserTokens is the per-user token store: connected users and their
// bearers per upstream, the 401 marks the gateway reported, and what a
// refresh does: switch to the next bearer when one is queued (a refresh
// token that works), else fail the way the store does when there is
// nothing to refresh with (the row is marked, errNoRefresh returned).
type fakeUserTokens struct {
	mu      sync.Mutex
	tokens  map[string]map[string]string // upstream -> user -> bearer
	next    map[string]map[string]string // upstream -> user -> bearer a refresh switches to
	expired map[string]bool              // "upstream/user" whose access token has expired (FreshenUserToken refreshes)
	marked  []string                     // "upstream/user"
	refresh []string                     // "upstream/user" per RefreshUserToken call
	freshen []string                     // "upstream/user" per FreshenUserToken call
	listErr error
	// empty, when set, makes UserHeaderFunc return nothing for that
	// "upstream/user" although UserConnected still says yes (a read or
	// decrypt failure, or a flip between the check and the request).
	empty map[string]bool
}

var errNoRefresh = errors.New("fake: nothing to refresh with")

func newFakeUserTokens() *fakeUserTokens {
	return &fakeUserTokens{
		tokens: map[string]map[string]string{}, next: map[string]map[string]string{},
		expired: map[string]bool{}, empty: map[string]bool{},
	}
}

func (f *fakeUserTokens) connect(upstream, uid, bearer string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tokens[upstream] == nil {
		f.tokens[upstream] = map[string]string{}
	}
	f.tokens[upstream][uid] = bearer
}

// canRefreshTo queues the bearer a refresh for (upstream, uid) yields.
func (f *fakeUserTokens) canRefreshTo(upstream, uid, bearer string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.next[upstream] == nil {
		f.next[upstream] = map[string]string{}
	}
	f.next[upstream][uid] = bearer
}

// refreshLocked applies a queued refresh or marks the row. Caller holds f.mu.
func (f *fakeUserTokens) refreshLocked(upstream, uid string) error {
	if nb, ok := f.next[upstream][uid]; ok {
		delete(f.next[upstream], uid)
		f.tokens[upstream][uid] = nb
		return nil
	}
	f.marked = append(f.marked, upstream+"/"+uid)
	delete(f.tokens[upstream], uid)
	return errNoRefresh
}

func (f *fakeUserTokens) FreshenUserToken(_ context.Context, upstream, uid string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.freshen = append(f.freshen, upstream+"/"+uid)
	if !f.expired[upstream+"/"+uid] {
		return nil
	}
	delete(f.expired, upstream+"/"+uid)
	return f.refreshLocked(upstream, uid)
}

func (f *fakeUserTokens) RefreshUserToken(_ context.Context, upstream, uid string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refresh = append(f.refresh, upstream+"/"+uid)
	return f.refreshLocked(upstream, uid)
}

func (f *fakeUserTokens) refreshes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.refresh...)
}

func (f *fakeUserTokens) freshens() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.freshen...)
}

func (f *fakeUserTokens) ConnectedUsers(_ context.Context, upstream string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []string
	for uid := range f.tokens[upstream] {
		out = append(out, uid)
	}
	sort.Strings(out)
	return out, nil
}

func (f *fakeUserTokens) UserConnected(_ context.Context, upstream, uid string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.tokens[upstream][uid]
	return ok, nil
}

func (f *fakeUserTokens) MarkUserUnauthorized(_ context.Context, upstream, uid, _ string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.marked = append(f.marked, upstream+"/"+uid)
	delete(f.tokens[upstream], uid)
}

func (f *fakeUserTokens) UserHeaderFunc(upstream string) func(ctx context.Context, userID string) map[string]string {
	return func(_ context.Context, uid string) map[string]string {
		f.mu.Lock()
		defer f.mu.Unlock()
		tok, ok := f.tokens[upstream][uid]
		if !ok || f.empty[upstream+"/"+uid] {
			return nil
		}
		return map[string]string{"Authorization": "Bearer " + tok}
	}
}

func (f *fakeUserTokens) marks() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.marked...)
}

// fakeOwners maps agent ids to the users who own them.
type fakeOwners map[string]string

func (o fakeOwners) OwnerUser(_ context.Context, callerID string) (string, error) {
	return o[callerID], nil
}

// sharedAuth is a HeaderProvider that always has a shared bearer: a
// per_user server must never send it.
type sharedAuth struct{}

func (sharedAuth) HeaderFunc(string) func(ctx context.Context) map[string]string {
	return func(context.Context) map[string]string {
		return map[string]string{"Authorization": "Bearer shared-token"}
	}
}
func (sharedAuth) HasClient(context.Context, string) (bool, error) { return true, nil }
func (sharedAuth) Disconnect(context.Context, string) error        { return nil }

type perUserFixture struct {
	db     *store.DB
	gw     *gateway.Gateway
	svc    *upstreams.Service
	tokens *fakeUserTokens
}

const (
	agAlice = "ag_alice"
	agBob   = "ag_bob"
	agOrph  = "ag_orphan" // no owner
	uAlice  = "u_alice"
	uBob    = "u_bob"
)

func newPerUserFixture(t *testing.T) *perUserFixture {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "pu.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	bus, err := approval.New(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	gw := gateway.New(gateway.Options{
		Policy: policy.New(db), Approval: bus, Audit: audit.New(db), Memory: memory.New(db),
		Hub:       realtime.NewHub(),
		Owners:    fakeOwners{agAlice: uAlice, agBob: uBob},
		PublicURL: "https://toolyard.example/",
	})
	t.Cleanup(func() { _ = gw.Close() })
	svc := upstreams.New(db, gw)
	tokens := newFakeUserTokens()
	svc.SetPerUserAuth(tokens)
	svc.SetAuth(sharedAuth{})
	return &perUserFixture{db: db, gw: gw, svc: svc, tokens: tokens}
}

func (f *perUserFixture) add(t *testing.T, name string, up *bearerUpstream) *upstreams.Server {
	t.Helper()
	srv, err := f.svc.Add(context.Background(), upstreams.Server{
		Name: name, Transport: "http", URL: up.srv.URL, AuthMode: upstreams.AuthPerUser,
	})
	if err != nil {
		t.Fatalf("Add %s: %v", name, err)
	}
	return srv
}

func (f *perUserFixture) call(t *testing.T, caller, tool string) *mcp.CallToolResult {
	t.Helper()
	res, err := f.gw.RouteCall(gateway.WithAgentID(context.Background(), caller), "test", tool,
		map[string]any{gateway.ReasonField: puReason})
	if err != nil {
		t.Fatalf("RouteCall %s as %s: %v", tool, caller, err)
	}
	return res
}

// ownersOnAudit lists owner_user_id on the call.succeeded rows for a tool.
func (f *perUserFixture) ownersOnAudit(t *testing.T, tool string) []string {
	t.Helper()
	rows, err := f.db.Query(`SELECT COALESCE(owner_user_id,'') FROM audit_events WHERE event_type = 'call.succeeded' AND tool_name = ? ORDER BY ts`, tool)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var o string
		_ = rows.Scan(&o)
		out = append(out, o)
	}
	return out
}

// TestPerUserWaitsForFirstSignIn: adding a per_user server nobody has
// connected succeeds with no tools and a waiting status, a call is refused
// before the upstream is contacted with a message naming the server and
// the My connections page, and the first sign-in loads the tools over
// that person's own session.
func TestPerUserWaitsForFirstSignIn(t *testing.T) {
	up := newBearerUpstream(t)
	f := newPerUserFixture(t)
	ctx := context.Background()

	srv := f.add(t, "linear", up)
	if srv.LastStatus != gateway.StatusWaitingSignIn || srv.LastError != "" || srv.ToolCount != 0 || srv.AuthMode != upstreams.AuthPerUser {
		t.Fatalf("server after add = status %q err %q tools %d mode %q", srv.LastStatus, srv.LastError, srv.ToolCount, srv.AuthMode)
	}
	if !f.gw.IsPerUser("linear") || f.gw.UpstreamToolCount("linear") != 0 || len(up.requests()) != 0 {
		t.Fatalf("registered=%v tools=%d requests=%d", f.gw.IsPerUser("linear"), f.gw.UpstreamToolCount("linear"), len(up.requests()))
	}

	// Alice connects; the tools arrive over her session and only hers.
	f.tokens.connect("linear", uAlice, "tok-alice")
	f.svc.ReconnectAfterUserAuth(ctx, "linear", uAlice)
	srv, _ = f.svc.Get(ctx, "linear")
	if srv.LastStatus != "ok" || srv.ToolCount != 1 {
		t.Fatalf("server after sign-in = status %q tools %d", srv.LastStatus, srv.ToolCount)
	}
	if up.count("initialize") != 1 || up.count("tools/list") != 1 {
		t.Fatalf("connect traffic: %+v", up.requests())
	}
	if got := up.bearersOn("initialize"); len(got) != 1 || got[0] != "Bearer tok-alice" {
		t.Fatalf("initialize bearers = %v", got)
	}

	res := f.call(t, agAlice, "linear.get_whoami")
	if res.IsError || text(res) != "Bearer tok-alice" {
		t.Fatalf("alice ran as %q (isError=%v)", text(res), res.IsError)
	}

	// Bob has not connected: refused before the upstream, with the pointer.
	before := len(up.requests())
	res = f.call(t, agBob, "linear.get_whoami")
	if !res.IsError {
		t.Fatalf("bob's call ran: %q", text(res))
	}
	msg := text(res)
	for _, want := range []string{"linear", "https://toolyard.example/#connections", "not connected", "not made"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal %q lacks %q", msg, want)
		}
	}
	if len(up.requests()) != before {
		t.Fatalf("refused call reached the upstream: %+v", up.requests()[before:])
	}
	// Refused, not held for approval: no approval row was created.
	var approvals int
	_ = f.db.QueryRow(`SELECT count(*) FROM approval_requests`).Scan(&approvals)
	if approvals != 0 {
		t.Fatalf("%d approval rows after a refusal", approvals)
	}
	var failed int
	_ = f.db.QueryRow(`SELECT count(*) FROM audit_events WHERE event_type='call.failed' AND result_summary LIKE 'per_user:%' AND owner_user_id = ?`, uBob).Scan(&failed)
	if failed != 1 {
		t.Fatalf("per_user refusal audit rows for bob = %d", failed)
	}

	// An agent with no owner cannot run as anyone.
	res = f.call(t, agOrph, "linear.get_whoami")
	if !res.IsError || !strings.Contains(text(res), "could not tell whose agent") {
		t.Fatalf("orphan agent: isError=%v %q", res.IsError, text(res))
	}
	// A shared-mode bearer is never sent on a per_user server.
	for _, r := range up.requests() {
		if strings.Contains(r.bearer, "shared-token") {
			t.Fatalf("shared bearer sent on a per_user server: %+v", r)
		}
	}
}

// TestPerUserTwoUsersTwoConnections: two people on one per_user server
// get two upstream sessions, every request on a session carries that one
// person's bearer, concurrent callers never cross, and the audit rows
// name whose account did the work.
func TestPerUserTwoUsersTwoConnections(t *testing.T) {
	up := newBearerUpstream(t)
	f := newPerUserFixture(t)
	f.tokens.connect("linear", uAlice, "tok-alice")
	f.tokens.connect("linear", uBob, "tok-bob")
	srv := f.add(t, "linear", up)
	if srv.LastStatus != "ok" || srv.ToolCount != 1 {
		t.Fatalf("server after add = %q/%d", srv.LastStatus, srv.ToolCount)
	}
	// Tools came over the first connected user's session only.
	if up.count("initialize") != 1 || up.count("tools/list") != 1 {
		t.Fatalf("connect traffic: %+v", up.requests())
	}

	if res := f.call(t, agAlice, "linear.get_whoami"); res.IsError || text(res) != "Bearer tok-alice" {
		t.Fatalf("alice ran as %q", text(res))
	}
	if res := f.call(t, agBob, "linear.get_whoami"); res.IsError || text(res) != "Bearer tok-bob" {
		t.Fatalf("bob ran as %q", text(res))
	}
	if up.count("initialize") != 2 {
		t.Fatalf("expected a second initialize for bob: %+v", up.requests())
	}
	if n := f.gw.LiveUpstreamCount(); n != 2 {
		t.Fatalf("live connections = %d, want 2", n)
	}

	const perCaller = 20
	var wg sync.WaitGroup
	errs := make(chan error, 2*perCaller)
	for _, c := range []struct{ ag, want string }{{agAlice, "Bearer tok-alice"}, {agBob, "Bearer tok-bob"}} {
		for i := 0; i < perCaller; i++ {
			wg.Add(1)
			go func(ag, want string) {
				defer wg.Done()
				res, err := f.gw.RouteCall(gateway.WithAgentID(context.Background(), ag), "test", "linear.get_whoami",
					map[string]any{gateway.ReasonField: puReason})
				if err != nil {
					errs <- err
					return
				}
				if res.IsError || text(res) != want {
					errs <- fmt.Errorf("%s ran as %q", ag, text(res))
				}
			}(c.ag, c.want)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	// One bearer per session, always.
	sessions := up.sessionsByBearer()
	if len(sessions) != 2 {
		t.Fatalf("sessions = %d, want 2: %v", len(sessions), sessions)
	}
	for sid, bearers := range sessions {
		if len(bearers) != 1 {
			t.Fatalf("session %s carried %d bearers: %v", sid, len(bearers), bearers)
		}
	}
	if got := up.bearersOn("tools/call"); len(got) != 2 || got[0] != "Bearer tok-alice" || got[1] != "Bearer tok-bob" {
		t.Fatalf("tools/call bearers = %v", got)
	}

	owners := f.ownersOnAudit(t, "linear.get_whoami")
	if len(owners) != 2+2*perCaller {
		t.Fatalf("call.succeeded rows = %d", len(owners))
	}
	seen := map[string]int{}
	for _, o := range owners {
		seen[o]++
	}
	if seen[uAlice] != 1+perCaller || seen[uBob] != 1+perCaller {
		t.Fatalf("owners on audit rows = %v", seen)
	}

	// The raw MCP path (what an agent's client sends) is the same route.
	msg, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "linear.get_whoami", "arguments": map[string]any{gateway.ReasonField: puReason}},
	})
	raw := f.gw.MCPServer().HandleMessage(gateway.WithAgentID(context.Background(), agBob), json.RawMessage(msg))
	b, _ := json.Marshal(raw)
	if !strings.Contains(string(b), "Bearer tok-bob") || strings.Contains(string(b), `"isError":true`) {
		t.Fatalf("raw MCP tools/call = %s", b)
	}

	// Removing the server closes every person's connection.
	if err := f.svc.Remove(context.Background(), "linear"); err != nil {
		t.Fatal(err)
	}
	if f.gw.IsPerUser("linear") || f.gw.UpstreamToolCount("linear") != 0 || f.gw.LiveUpstreamCount() != 0 {
		t.Fatalf("after remove: perUser=%v tools=%d live=%d", f.gw.IsPerUser("linear"), f.gw.UpstreamToolCount("linear"), f.gw.LiveUpstreamCount())
	}
}

// TestPerUserIdleSweepAndCap: per-user connections are suspended by the
// idle sweep and re-dialed with the right bearer on the next call, and
// they count against the live cap like any other connection.
func TestPerUserIdleSweepAndCap(t *testing.T) {
	up := newBearerUpstream(t)
	f := newPerUserFixture(t)
	f.tokens.connect("linear", uAlice, "tok-alice")
	f.tokens.connect("linear", uBob, "tok-bob")
	f.add(t, "linear", up)
	f.call(t, agAlice, "linear.get_whoami")
	f.call(t, agBob, "linear.get_whoami")
	if n := f.gw.LiveUpstreamCount(); n != 2 {
		t.Fatalf("live = %d", n)
	}
	if n := f.gw.SweepIdleStdioUpstreams(0); n != 2 {
		t.Fatalf("suspended %d, want 2", n)
	}
	if live, idle := f.gw.LiveUpstreamCount(), f.gw.SuspendedUpstreamCount(); live != 0 || idle != 2 {
		t.Fatalf("after sweep live=%d idle=%d", live, idle)
	}
	// Tools survive the sweep; the next call re-dials as its own user.
	if f.gw.UpstreamToolCount("linear") != 1 {
		t.Fatal("tools lost on sweep")
	}
	inits := up.count("initialize")
	if res := f.call(t, agBob, "linear.get_whoami"); res.IsError || text(res) != "Bearer tok-bob" {
		t.Fatalf("bob after resume ran as %q", text(res))
	}
	if up.count("initialize") != inits+1 {
		t.Fatalf("expected one re-dial: %+v", up.requests())
	}
	for sid, bearers := range up.sessionsByBearer() {
		if len(bearers) != 1 {
			t.Fatalf("session %s carried %v", sid, bearers)
		}
	}

	// Per-user cap of one: alice's call evicts bob's connection. The
	// shared cap does not touch per-user connections (their own pool).
	f.gw.SetMaxLiveUpstreams(1)
	f.call(t, agAlice, "linear.get_whoami")
	if n := f.gw.PerUserLiveCount(); n != 2 {
		t.Fatalf("per-user live under a shared cap of 1 = %d, want 2 (separate pool)", n)
	}
	// Admission runs on a dial: drop alice's live connection so her next
	// call dials, and that dial evicts bob (the pool is at its cap).
	f.gw.SetMaxLivePerUser(1)
	f.svc.DropUserConnection("linear", uAlice)
	if res := f.call(t, agAlice, "linear.get_whoami"); res.IsError || text(res) != "Bearer tok-alice" {
		t.Fatalf("alice under per-user cap 1 ran as %q", text(res))
	}
	if n := f.gw.PerUserLiveCount(); n != 1 {
		t.Fatalf("per-user live under per-user cap 1 = %d", n)
	}
}

// TestPerUser401MarksOnlyThatUser: an upstream 401 on one person's call
// marks that person's token for re-auth and drops their connection; the
// next call from them is refused with the connect message, and the other
// person is untouched.
func TestPerUser401MarksOnlyThatUser(t *testing.T) {
	up := newBearerUpstream(t)
	f := newPerUserFixture(t)
	f.tokens.connect("linear", uAlice, "tok-alice")
	f.tokens.connect("linear", uBob, "tok-bob")
	f.add(t, "linear", up)
	f.call(t, agBob, "linear.get_whoami")

	up.denyBearer("Bearer tok-bob")
	res := f.call(t, agBob, "linear.get_whoami")
	if !res.IsError {
		t.Fatalf("bob's call with a dead token ran: %q", text(res))
	}
	if got := f.tokens.marks(); len(got) != 1 || got[0] != "linear/"+uBob {
		t.Fatalf("401 marks = %v", got)
	}
	res = f.call(t, agBob, "linear.get_whoami")
	if !res.IsError || !strings.Contains(text(res), "#connections") {
		t.Fatalf("bob after 401: isError=%v %q", res.IsError, text(res))
	}
	if res := f.call(t, agAlice, "linear.get_whoami"); res.IsError || text(res) != "Bearer tok-alice" {
		t.Fatalf("alice after bob's 401 ran as %q", text(res))
	}
	if got := f.tokens.marks(); len(got) != 1 {
		t.Fatalf("marks after alice's call = %v", got)
	}

	// A 401 on the dial (dead token found at re-connect) marks too.
	f.tokens.connect("linear", uBob, "tok-bob-2")
	up.denyBearer("Bearer tok-bob-2")
	res = f.call(t, agBob, "linear.get_whoami")
	if !res.IsError {
		t.Fatalf("bob's dial with a dead token ran: %q", text(res))
	}
	if got := f.tokens.marks(); len(got) != 2 || got[1] != "linear/"+uBob {
		t.Fatalf("marks after dial 401 = %v", got)
	}

	// Disconnecting a person (their row deleted) drops their connection.
	f.svc.DropUserConnection("linear", uAlice)
	if n := f.gw.LiveUpstreamCount(); n != 0 {
		t.Fatalf("live after drop = %d", n)
	}
}

// TestPerUserRejectsStdio: per-user sign-in is an http thing.
func TestPerUserRejectsStdio(t *testing.T) {
	f := newPerUserFixture(t)
	_, err := f.svc.Add(context.Background(), upstreams.Server{
		Name: "local", Transport: "stdio", Command: "true", AuthMode: upstreams.AuthPerUser,
	})
	if err == nil || !strings.Contains(err.Error(), "http") {
		t.Fatalf("stdio per_user: %v", err)
	}
	_, err = f.svc.Add(context.Background(), upstreams.Server{
		Name: "x", Transport: "http", URL: "http://127.0.0.1:1/mcp", AuthMode: "bogus",
	})
	if err == nil || !strings.Contains(err.Error(), "auth_mode") {
		t.Fatalf("bogus mode: %v", err)
	}
}
