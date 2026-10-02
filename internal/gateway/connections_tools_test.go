package gateway_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tusharbhardwaj/toolyard/internal/access"
	"github.com/tusharbhardwaj/toolyard/internal/approval"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/gateway"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/memory"
	"github.com/tusharbhardwaj/toolyard/internal/oauth"
	"github.com/tusharbhardwaj/toolyard/internal/policy"
	"github.com/tusharbhardwaj/toolyard/internal/realtime"
	"github.com/tusharbhardwaj/toolyard/internal/store"
	"github.com/tusharbhardwaj/toolyard/internal/upstreams"
)

// Connect links through the real pieces: the oauth.Service minting and
// redeeming tickets, access deciding who is an admin and who may use
// which server, identity naming the admins, and the gateway wording the
// refusals and answering connections.status / connections.link.

const (
	cfRoot = "u_root" // admin, root@beknown.work
	cfBob  = "u_bob"  // member, bob@beknown.work; granted linear and gh
	cfPub  = "https://toolyard.example"
)

var connectLinkRE = regexp.MustCompile(regexp.QuoteMeta(cfPub+oauth.ConnectLinkPath) + `([A-Za-z0-9_-]+)`)

// connectTokenIdP: code "x" becomes access token "at-x" naming
// x@example.test.
func connectTokenIdP(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		code := r.Form.Get("code")
		hdr := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
		pl, _ := json.Marshal(map[string]string{"email": code + "@example.test"})
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "at-" + code, "refresh_token": "rt-" + code, "token_type": "Bearer", "expires_in": 3600,
			"id_token": hdr + "." + base64.RawURLEncoding.EncodeToString(pl) + ".s",
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

type connectFixture struct {
	db     *store.DB
	gw     *gateway.Gateway
	svc    *upstreams.Service
	oa     *oauth.Service
	ids    *identity.Service
	idp    *httptest.Server
	agRoot string
	agBob  string
}

func newConnectFixture(t *testing.T) *connectFixture {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "connect.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, q := range []string{
		`INSERT INTO users(id, username, password_hash, role, email, created_at, updated_at) VALUES('u_root','root','x','admin','root@beknown.work',1,1)`,
		`INSERT INTO users(id, username, password_hash, role, email, created_at, updated_at) VALUES('u_bob','bob','x','member','bob@beknown.work',2,2)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	ids := identity.New(db)
	_, agRoot, err := ids.CreateAgentWithToken(ctx, cfRoot, "root-bot")
	if err != nil {
		t.Fatal(err)
	}
	_, agBob, err := ids.CreateAgentWithToken(ctx, cfBob, "bob-bot")
	if err != nil {
		t.Fatal(err)
	}
	acc := access.New(db)
	if err := acc.SetGroups(ctx, cfBob, []string{"linear", "gh"}, cfRoot); err != nil {
		t.Fatal(err)
	}
	bus, err := approval.New(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	gw := gateway.New(gateway.Options{
		Policy: policy.New(db), Approval: bus, Audit: audit.New(db), Memory: memory.New(db), Hub: realtime.NewHub(),
		Access: acc, Owners: acc, PublicURL: cfPub + "/",
	})
	gw.RegisterBuiltins()
	t.Cleanup(func() { _ = gw.Close() })

	key := make([]byte, oauth.MasterKeySize)
	_, _ = rand.Read(key)
	cipher, err := oauth.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	idp := connectTokenIdP(t)
	oa := oauth.New(db, cipher, nil, nil, nil)
	oa.SetHTTPClient(idp.Client())
	gw.SetConnect(oa, ids)
	svc := upstreams.New(db, gw)
	svc.SetAuth(oa)
	svc.SetPerUserAuth(oa)
	oa.SetUserReauthHook(svc.DropUserConnection)
	return &connectFixture{db: db, gw: gw, svc: svc, oa: oa, ids: ids, idp: idp, agRoot: agRoot.ID, agBob: agBob.ID}
}

// add registers an http server in the given mode.
func (f *connectFixture) add(t *testing.T, name, mode string, up *bearerUpstream) {
	t.Helper()
	if _, err := f.svc.Add(context.Background(), upstreams.Server{Name: name, Transport: "http", URL: up.srv.URL, AuthMode: mode}); err != nil {
		t.Fatalf("Add %s: %v", name, err)
	}
}

// client registers the server's OAuth client (what discovery would store).
func (f *connectFixture) client(t *testing.T, name string) {
	t.Helper()
	if err := f.oa.PutClient(context.Background(), oauth.ClientRecord{
		UpstreamName: name, Issuer: f.idp.URL, AuthorizationEndpoint: f.idp.URL + "/authorize", TokenEndpoint: f.idp.URL + "/token",
		ClientID: "cid", RedirectURI: cfPub + "/v1/mcp-oauth/callback", TokenEndpointAuthMethod: "none",
	}); err != nil {
		t.Fatal(err)
	}
}

// connectUser signs uid in to a per_user server the way the callback does.
func (f *connectFixture) connectUser(t *testing.T, name, uid, code string) {
	t.Helper()
	ctx := context.Background()
	_, state, err := f.oa.BeginForUser(ctx, name, uid)
	if err != nil {
		t.Fatal(err)
	}
	p, err := f.oa.LoadPending(ctx, state)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.oa.ExchangeCodeForUser(ctx, name, uid, code, p.CodeVerifier); err != nil {
		t.Fatal(err)
	}
	f.svc.ReconnectAfterUserAuth(ctx, name, uid)
}

// connectShared signs a shared server in as an admin would from the
// dashboard.
func (f *connectFixture) connectShared(t *testing.T, name, code string) {
	t.Helper()
	ctx := context.Background()
	_, state, err := f.oa.BeginCallback(ctx, name, cfRoot, oauth.ModeCallback, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	p, err := f.oa.LoadPending(ctx, state)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.oa.ExchangeCode(ctx, name, code, p.CodeVerifier); err != nil {
		t.Fatal(err)
	}
	f.svc.ReconnectAfterAuth(ctx, name)
}

func (f *connectFixture) call(t *testing.T, agent, tool string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	if args == nil {
		args = map[string]any{}
	}
	args[gateway.ReasonField] = "connect link test: see whether the call runs or how it is refused"
	res, err := f.gw.RouteCall(gateway.WithAgentID(context.Background(), agent), "test", tool, args)
	if err != nil {
		t.Fatalf("RouteCall %s as %s: %v", tool, agent, err)
	}
	return res
}

// linkIn pulls the connect link's ticket out of a message.
func linkIn(t *testing.T, msg string) string {
	t.Helper()
	m := connectLinkRE.FindStringSubmatch(msg)
	if m == nil {
		t.Fatalf("no connect link in %q", msg)
	}
	return m[1]
}

// redeem redeems a ticket and checks whose it was.
func (f *connectFixture) redeem(t *testing.T, ticket, uid, upstream, purpose, agent string) {
	t.Helper()
	tk, err := f.oa.RedeemConnectTicket(context.Background(), ticket)
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}
	if tk.UserID != uid || tk.Upstream != upstream || tk.Purpose != purpose || tk.AgentID != agent {
		t.Fatalf("ticket = %+v, want %s %s %s %s", tk, uid, upstream, purpose, agent)
	}
}

func (f *connectFixture) auditMentions(t *testing.T, s string) int {
	t.Helper()
	var n int
	if err := f.db.QueryRow(`SELECT count(*) FROM audit_events WHERE result_summary LIKE '%'||?||'%' OR reason LIKE '%'||?||'%' OR COALESCE(arguments,'') LIKE '%'||?||'%'`, s, s, s).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (f *connectFixture) auditCount(t *testing.T, where string, args ...any) int {
	t.Helper()
	var n int
	if err := f.db.QueryRow(`SELECT count(*) FROM audit_events WHERE `+where, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestPerUserRefusalCarriesRedeemableLink: a per_user call from an owner
// who has not connected is refused with a one-time link for that owner,
// server and agent; the ticket never reaches the audit log; after the
// person connects the call runs as them; a sign-in the provider rejected
// gets the "sign in again" wording with a fresh link.
func TestPerUserRefusalCarriesRedeemableLink(t *testing.T) {
	f := newConnectFixture(t)
	up := newBearerUpstream(t)
	f.add(t, "linear", upstreams.AuthPerUser, up)
	f.client(t, "linear")
	// The tools arrive with the first sign-in; root's.
	f.connectUser(t, "linear", cfRoot, "root")
	if f.gw.UpstreamToolCount("linear") != 1 {
		t.Fatalf("linear tools = %d", f.gw.UpstreamToolCount("linear"))
	}

	before := len(up.requests())
	res := f.call(t, f.agBob, "linear.get_whoami", nil)
	if !res.IsError {
		t.Fatalf("bob's call ran: %q", text(res))
	}
	msg := text(res)
	for _, want := range []string{"linear runs as you and isn't connected yet", "one-time, 10 minutes", "then ask me to retry", "Nothing was run"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal %q lacks %q", msg, want)
		}
	}
	if strings.Contains(msg, "#connections") {
		t.Errorf("refusal still points at the dashboard: %q", msg)
	}
	if len(up.requests()) != before {
		t.Fatalf("refused call reached the upstream")
	}
	ticket := linkIn(t, msg)
	if f.auditMentions(t, ticket) != 0 {
		t.Fatal("ticket reached the audit log")
	}
	if n := f.auditCount(t, `event_type = ? AND upstream_name = 'linear' AND agent_id = ? AND owner_user_id = ?`, gateway.EventConnectLinkIssued, f.agBob, cfBob); n != 1 {
		t.Fatalf("link_issued rows = %d", n)
	}
	f.redeem(t, ticket, cfBob, "linear", oauth.ConnectPurposePerUser, f.agBob)

	// Bob connects; the call runs as him.
	f.connectUser(t, "linear", cfBob, "bob")
	if res := f.call(t, f.agBob, "linear.get_whoami", nil); res.IsError || text(res) != "Bearer at-bob" {
		t.Fatalf("bob after connecting: isError=%v %q", res.IsError, text(res))
	}

	// His token is rejected for good: the next call says sign in again.
	f.oa.MarkUserUnauthorized(context.Background(), "linear", cfBob, "401")
	res = f.call(t, f.agBob, "linear.get_whoami", nil)
	if !res.IsError || !strings.Contains(text(res), "your sign-in to it has expired") || !strings.Contains(text(res), "sign in again") {
		t.Fatalf("after reauth mark: isError=%v %q", res.IsError, text(res))
	}
	f.redeem(t, linkIn(t, text(res)), cfBob, "linear", oauth.ConnectPurposePerUser, f.agBob)
}

// TestSharedSignInGuidance: a shared OAuth server with no usable token
// gives an admin owner a shared link (naming the account to sign in as
// once known) and a member the admins to ask: on the upstream's 401 when
// the connection is live, and before the dial when it is down and the
// token row says the sign-in died.
func TestSharedSignInGuidance(t *testing.T) {
	f := newConnectFixture(t)
	ctx := context.Background()
	up := newBearerUpstream(t)
	f.add(t, "gh", upstreams.AuthShared, up)
	f.client(t, "gh")

	// Never signed in. The dial needed no bearer, so the connection is
	// live and the upstream's 401 is what refuses the call.
	up.denyBearer("")
	res := f.call(t, f.agRoot, "gh.get_whoami", nil)
	msg := text(res)
	if !res.IsError || !strings.Contains(msg, "gh uses one shared account and it was never signed in") || !strings.Contains(msg, "You're an admin") || !strings.Contains(msg, "Nothing was run") {
		t.Fatalf("admin, no token: isError=%v %q", res.IsError, msg)
	}
	if strings.Contains(msg, "Sign in as") {
		t.Fatalf("no account is known yet, but the message names one: %q", msg)
	}
	f.redeem(t, linkIn(t, msg), cfRoot, "gh", oauth.ConnectPurposeShared, f.agRoot)
	res = f.call(t, f.agBob, "gh.get_whoami", nil)
	msg = text(res)
	if !res.IsError || !strings.Contains(msg, "gh needs an admin to sign it in; ask an admin (root@beknown.work)") || !strings.Contains(msg, "Nothing was run") {
		t.Fatalf("member, no token: isError=%v %q", res.IsError, msg)
	}
	if connectLinkRE.MatchString(msg) {
		t.Fatalf("a member was given a shared link: %q", msg)
	}
	if n := f.auditCount(t, `event_type = 'call.failed' AND upstream_name = 'gh' AND result_summary LIKE 'shared_signin: it was never signed in: %'`); n != 2 {
		t.Fatalf("shared_signin audit rows = %d", n)
	}
	if n := f.auditCount(t, `event_type = ? AND upstream_name = 'gh'`, gateway.EventConnectLinkIssued); n != 1 {
		t.Fatalf("shared links issued = %d, want the admin's only", n)
	}

	// The admin signs in: calls run with the shared bearer.
	f.connectShared(t, "gh", "ops")
	if res := f.call(t, f.agBob, "gh.get_whoami", nil); res.IsError || text(res) != "Bearer at-ops" {
		t.Fatalf("after shared sign-in: isError=%v %q", res.IsError, text(res))
	}

	// The upstream rejects that token: 401 becomes the guidance, with the
	// account to sign in as.
	up.denyBearer("Bearer at-ops")
	res = f.call(t, f.agRoot, "gh.get_whoami", nil)
	msg = text(res)
	if !res.IsError || !strings.Contains(msg, "rejected its token (401)") || !strings.Contains(msg, "Sign in as ops@example.test") {
		t.Fatalf("admin after 401: isError=%v %q", res.IsError, msg)
	}
	f.redeem(t, linkIn(t, msg), cfRoot, "gh", oauth.ConnectPurposeShared, f.agRoot)

	// The token is marked for re-auth and the connection is down (idle
	// sweep): refused before any dial, the upstream sees nothing.
	f.oa.MarkReauthExternal(ctx, "gh", "revoked")
	f.gw.SweepIdleStdioUpstreams(0)
	before := len(up.requests())
	res = f.call(t, f.agRoot, "gh.get_whoami", nil)
	msg = text(res)
	if !res.IsError || !strings.Contains(msg, "its sign-in needs to be renewed") || !strings.Contains(msg, "Sign in as ops@example.test") {
		t.Fatalf("admin after reauth mark: isError=%v %q", res.IsError, msg)
	}
	f.redeem(t, linkIn(t, msg), cfRoot, "gh", oauth.ConnectPurposeShared, f.agRoot)
	if res := f.call(t, f.agBob, "gh.get_whoami", nil); !res.IsError || !strings.Contains(text(res), "needs to be renewed") || !strings.Contains(text(res), "ask an admin (root@beknown.work)") {
		t.Fatalf("member after reauth mark: isError=%v %q", res.IsError, text(res))
	}
	if len(up.requests()) != before {
		t.Fatalf("refused calls reached the upstream: %+v", up.requests()[before:])
	}
	if n := f.auditCount(t, `event_type = 'call.failed' AND upstream_name = 'gh' AND result_summary = 'shared_signin: its sign-in needs to be renewed'`); n != 2 {
		t.Fatalf("pre-dial shared_signin audit rows = %d", n)
	}

	// A server with static headers and no OAuth client is left alone.
	plain := newBearerUpstream(t)
	if _, err := f.svc.Add(ctx, upstreams.Server{Name: "plain", Transport: "http", URL: plain.srv.URL, Headers: map[string]string{"X-Team": "ops"}}); err != nil {
		t.Fatal(err)
	}
	if res := f.call(t, f.agRoot, "plain.get_whoami", nil); res.IsError || text(res) != "" {
		t.Fatalf("plain server: isError=%v %q", res.IsError, text(res))
	}
}

// statusView is connections.status's structured content, as a test reads it.
type statusView struct {
	Server        string `json:"server"`
	Mode          string `json:"mode"`
	State         string `json:"state"`
	Account       string `json:"account"`
	ConnectLink   string `json:"connect_link"`
	LinkExpiresIn int    `json:"link_expires_in"`
	Note          string `json:"note"`
}

func statusOf(t *testing.T, res *mcp.CallToolResult) map[string]statusView {
	t.Helper()
	if res.IsError {
		t.Fatalf("connections.status failed: %s", text(res))
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Servers []statusView `json:"servers"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	out := map[string]statusView{}
	for _, v := range body.Servers {
		out[v.Server] = v
	}
	if len(out) != len(body.Servers) {
		t.Fatalf("duplicate servers in %s", raw)
	}
	return out
}

// TestConnectionsStatusAndLink: the status tool lists only the OAuth
// servers the owner may use, with the right state, account and link rule
// (shared links for admins only); the link tool follows the same rules.
func TestConnectionsStatusAndLink(t *testing.T) {
	f := newConnectFixture(t)
	up := newBearerUpstream(t)
	f.add(t, "linear", upstreams.AuthPerUser, up)
	f.client(t, "linear")
	f.add(t, "gh", upstreams.AuthShared, newBearerUpstream(t))
	f.client(t, "gh")
	f.add(t, "secret", upstreams.AuthShared, newBearerUpstream(t)) // not granted to bob
	f.client(t, "secret")
	f.add(t, "plain", upstreams.AuthShared, newBearerUpstream(t))  // no OAuth client
	f.add(t, "fresh", upstreams.AuthPerUser, newBearerUpstream(t)) // per_user, OAuth setup not done

	// Bob, a member: his two granted servers, nothing about the others.
	bob := statusOf(t, f.call(t, f.agBob, gateway.ConnectionsStatusTool, nil))
	if len(bob) != 2 {
		t.Fatalf("bob sees %v", bob)
	}
	if v := bob["linear"]; v.Mode != "per_user" || v.State != "needs_signin" || v.ConnectLink == "" || v.LinkExpiresIn != 600 {
		t.Fatalf("bob/linear = %+v", v)
	}
	f.redeem(t, linkIn(t, bob["linear"].ConnectLink), cfBob, "linear", oauth.ConnectPurposePerUser, f.agBob)
	if v := bob["gh"]; v.Mode != "shared" || v.State != "needs_signin" || v.ConnectLink != "" || !strings.Contains(v.Note, "ask an admin (root@beknown.work)") {
		t.Fatalf("bob/gh = %+v", v)
	}

	// Root, an admin: everything with an OAuth client, shared links
	// included, plus the per_user server whose setup is not done.
	root := statusOf(t, f.call(t, f.agRoot, gateway.ConnectionsStatusTool, nil))
	if len(root) != 4 || root["plain"].Server != "" {
		t.Fatalf("root sees %v", root)
	}
	if v := root["fresh"]; v.Mode != "per_user" || v.State != "needs_setup" || v.ConnectLink != "" || !strings.Contains(v.Note, "OAuth setup") {
		t.Fatalf("root/fresh = %+v", v)
	}
	if res := f.call(t, f.agRoot, gateway.ConnectionsLinkTool, map[string]any{"server": "fresh"}); !res.IsError || !strings.Contains(text(res), "no sign-in set up yet") {
		t.Fatalf("root link fresh: isError=%v %q", res.IsError, text(res))
	}
	for _, name := range []string{"gh", "secret"} {
		v := root[name]
		if v.Mode != "shared" || v.State != "needs_signin" || v.ConnectLink == "" {
			t.Fatalf("root/%s = %+v", name, v)
		}
		f.redeem(t, linkIn(t, v.ConnectLink), cfRoot, name, oauth.ConnectPurposeShared, f.agRoot)
	}

	// Sign-ins change the states; a connected server gets no link, and a
	// signed-in server whose token died needs a renewal.
	f.connectUser(t, "linear", cfRoot, "root")
	f.connectShared(t, "gh", "ops")
	f.connectShared(t, "secret", "sec")
	f.oa.MarkReauthExternal(context.Background(), "secret", "revoked")
	root = statusOf(t, f.call(t, f.agRoot, gateway.ConnectionsStatusTool, nil))
	if v := root["linear"]; v.State != "connected" || v.Account != "root@example.test" || v.ConnectLink != "" {
		t.Fatalf("root/linear after sign-in = %+v", v)
	}
	if v := root["gh"]; v.State != "connected" || v.Account != "ops@example.test" || v.ConnectLink != "" {
		t.Fatalf("root/gh after sign-in = %+v", v)
	}
	if v := root["secret"]; v.State != "needs_reauth" || v.Account != "sec@example.test" || v.ConnectLink == "" || !strings.Contains(v.Note, "sign in as sec@example.test") {
		t.Fatalf("root/secret after reauth mark = %+v", v)
	}
	bob = statusOf(t, f.call(t, f.agBob, gateway.ConnectionsStatusTool, nil))
	if v := bob["linear"]; v.State != "needs_signin" || v.ConnectLink == "" {
		t.Fatalf("bob/linear after root's sign-in = %+v (root's sign-in is not bob's)", v)
	}
	if v := bob["gh"]; v.State != "connected" || v.Account != "ops@example.test" {
		t.Fatalf("bob/gh after shared sign-in = %+v", v)
	}
	if !strings.Contains(text(f.call(t, f.agBob, gateway.ConnectionsStatusTool, nil)), "clickable Markdown link") {
		t.Fatal("status text lacks the guidance")
	}

	// connections.link.
	res := f.call(t, f.agBob, gateway.ConnectionsLinkTool, map[string]any{"server": "linear"})
	if res.IsError || !strings.Contains(text(res), "clickable Markdown link") {
		t.Fatalf("bob link linear: isError=%v %q", res.IsError, text(res))
	}
	f.redeem(t, linkIn(t, text(res)), cfBob, "linear", oauth.ConnectPurposePerUser, f.agBob)
	res = f.call(t, f.agBob, gateway.ConnectionsLinkTool, map[string]any{"server": "gh"})
	if !res.IsError || !strings.Contains(text(res), "only an admin can sign it in") || !strings.Contains(text(res), "root@beknown.work") || connectLinkRE.MatchString(text(res)) {
		t.Fatalf("bob link gh: isError=%v %q", res.IsError, text(res))
	}
	for _, name := range []string{"secret", "plain", "nope"} {
		res = f.call(t, f.agBob, gateway.ConnectionsLinkTool, map[string]any{"server": name})
		if !res.IsError || !strings.Contains(text(res), "unknown server") {
			t.Fatalf("bob link %s: isError=%v %q", name, res.IsError, text(res))
		}
	}
	res = f.call(t, f.agRoot, gateway.ConnectionsLinkTool, map[string]any{"server": "secret"})
	if res.IsError || !strings.Contains(text(res), "needs_reauth") {
		t.Fatalf("root link secret: isError=%v %q", res.IsError, text(res))
	}
	f.redeem(t, linkIn(t, text(res)), cfRoot, "secret", oauth.ConnectPurposeShared, f.agRoot)
	res = f.call(t, f.agRoot, gateway.ConnectionsLinkTool, map[string]any{"server": "plain"})
	if !res.IsError || !strings.Contains(text(res), "does not use an OAuth sign-in") {
		t.Fatalf("root link plain: isError=%v %q", res.IsError, text(res))
	}
	if res := f.call(t, f.agRoot, gateway.ConnectionsLinkTool, nil); !res.IsError || !strings.Contains(text(res), "server is required") {
		t.Fatalf("link without server: %q", text(res))
	}
	// Every link handed out was audited without its ticket.
	if f.auditCount(t, `event_type = ?`, gateway.EventConnectLinkIssued) == 0 || f.auditMentions(t, oauth.ConnectLinkPath) != 0 {
		t.Fatal("link audit rows missing, or a link was written to the audit log")
	}
}

// TestConnectionsToolsInCodeMode: code mode binds the connections tools
// under the one toolyard server, as toolyard.connections_status and
// toolyard.connections_link, for a member too; a script's call hands back
// the same redeemable link.
func TestConnectionsToolsInCodeMode(t *testing.T) {
	f := newConnectFixture(t)
	up := newBearerUpstream(t)
	f.add(t, "linear", upstreams.AuthPerUser, up)
	f.client(t, "linear")

	listing := text(f.call(t, f.agBob, gateway.CodeModeListToolFiles, nil))
	for _, want := range []string{"  toolyard/\n", "    connections_link.pyi\n", "    connections_status.pyi\n"} {
		if !strings.Contains(listing, want) {
			t.Errorf("member listing missing %q:\n%s", want, listing)
		}
	}
	if strings.Contains(listing, "  connections/") {
		t.Fatalf("connections bound as its own server instead of under toolyard:\n%s", listing)
	}
	stub := text(f.call(t, f.agBob, gateway.CodeModeReadToolFile, map[string]any{"fileName": "servers/toolyard/connections_link.pyi"}))
	if !strings.Contains(stub, "def connections_link(") || !strings.Contains(stub, "server: str") {
		t.Fatalf("connections_link stub:\n%s", stub)
	}

	res := f.call(t, f.agBob, gateway.CodeModeExecuteToolCode, map[string]any{"code": "result = toolyard.connections_status()"})
	if res.IsError {
		t.Fatalf("status script: %s", text(res))
	}
	// A script receives the tool's text; the link is in it.
	_, ret, ok := strings.Cut(text(res), "Return value: ")
	if !ok || !strings.Contains(ret, "linear (per_user): needs_signin") || !strings.Contains(ret, "clickable Markdown link") {
		t.Fatalf("status script return:\n%s", text(res))
	}
	f.redeem(t, linkIn(t, ret), cfBob, "linear", oauth.ConnectPurposePerUser, f.agBob)

	res = f.call(t, f.agBob, gateway.CodeModeExecuteToolCode, map[string]any{"code": `result = toolyard.connections_link(server="linear")`})
	if res.IsError {
		t.Fatalf("link script: %s", text(res))
	}
	_, ret, _ = strings.Cut(text(res), "Return value: ")
	f.redeem(t, linkIn(t, ret), cfBob, "linear", oauth.ConnectPurposePerUser, f.agBob)
}

// stubConnect is a ConnectProvider with one per_user server nobody has
// connected, for the wiring test.
type stubConnect struct{}

func (stubConnect) IssueConnectTicket(context.Context, string, string, string, string) (string, error) {
	return "stub-ticket", nil
}
func (stubConnect) OAuthServers(context.Context) ([]oauth.OAuthServer, error) {
	return []oauth.OAuthServer{{Name: "x", AuthMode: oauth.ConnectPurposePerUser, Enabled: true}}, nil
}
func (stubConnect) SharedConnection(context.Context, string) (*oauth.SharedConnection, error) {
	return nil, nil
}
func (stubConnect) UserConnectionOf(context.Context, string, string) (*oauth.UserConnection, error) {
	return nil, nil
}

// TestConnectionsToolsNeedOwnerAndWiring: without SetConnect the tools say
// links are not available; an agent nobody owns is told so; with access
// wired an unknown agent sees no such tool at all; the tools are pinned
// and in the always-on group.
func TestConnectionsToolsNeedOwnerAndWiring(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "bare.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	gw := gateway.New(gateway.Options{
		Policy: policy.New(db), Audit: audit.New(db), Memory: memory.New(db), Hub: realtime.NewHub(),
		Owners: fakeOwners{"ag_owned": "u_1"}, PublicURL: cfPub,
	})
	gw.RegisterBuiltins()
	t.Cleanup(func() { _ = gw.Close() })
	for _, n := range []string{gateway.ConnectionsStatusTool, gateway.ConnectionsLinkTool} {
		if !gateway.IsPinned(n) || !gw.HasTool(n) || !access.AlwaysOn("connections") {
			t.Fatalf("%s: pinned=%v registered=%v alwaysOn=%v", n, gateway.IsPinned(n), gw.HasTool(n), access.AlwaysOn("connections"))
		}
	}
	call := func(agent, tool string) *mcp.CallToolResult {
		res, err := gw.RouteCall(gateway.WithAgentID(context.Background(), agent), "test", tool,
			map[string]any{gateway.ReasonField: "bare gateway: check the connections tools answer", "server": "x"})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if res := call("ag_owned", gateway.ConnectionsStatusTool); !res.IsError || !strings.Contains(text(res), "not available") {
		t.Fatalf("unwired status: %q", text(res))
	}
	gw.SetConnect(stubConnect{}, nil)
	if res := call("ag_nobody", gateway.ConnectionsLinkTool); !res.IsError || !strings.Contains(text(res), "need an enrolled agent with an owner") {
		t.Fatalf("orphan agent: %q", text(res))
	}
	// A call marked as made with an operator token is never handed a link.
	noLinks, err := gw.RouteCall(gateway.WithoutConnectLinks(gateway.WithAgentID(context.Background(), "ag_owned")), "test", gateway.ConnectionsLinkTool,
		map[string]any{gateway.ReasonField: "operator-marked call: check no link is minted", "server": "x"})
	if err != nil {
		t.Fatal(err)
	}
	if !noLinks.IsError || !strings.Contains(text(noLinks), "operator token") || strings.Contains(text(noLinks), "stub-ticket") {
		t.Fatalf("operator-marked call: %q", text(noLinks))
	}
	if res := call("ag_owned", gateway.ConnectionsStatusTool); res.IsError || !strings.Contains(text(res), "x (per_user): needs_signin") ||
		!strings.Contains(text(res), cfPub+oauth.ConnectLinkPath+"stub-ticket") {
		t.Fatalf("owned agent: isError=%v %q", res.IsError, text(res))
	}
	// With access wired, an agent nobody owns is outside every scope and
	// the tool reads as unknown, like every other tool.
	f := newConnectFixture(t)
	res, err := f.gw.RouteCall(gateway.WithAgentID(context.Background(), "ag_nobody"), "test", gateway.ConnectionsLinkTool,
		map[string]any{gateway.ReasonField: "unknown agent: check the connections tools are hidden", "server": "x"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(text(res), "not found") {
		t.Fatalf("unknown agent: %q", text(res))
	}
}
