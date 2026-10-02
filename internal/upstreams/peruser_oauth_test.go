package upstreams

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

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
)

// The whole per-user path with the real pieces: oauth.Service sealing each
// person's tokens, a fake IdP that issues them, upstreams.Service composing
// the headers, the gateway opening one session per person, and a fake MCP
// server that reports which bearer each request carried.

// tokenIdP is a token endpoint: code "x" becomes access token "at-x" with
// an id_token naming x@example.test.
func tokenIdP(t *testing.T) *httptest.Server {
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

// bearerMCP is an MCP server whose get_whoami echoes the Authorization
// header, recording (method, bearer, session) for every request.
type bearerMCP struct {
	srv  *httptest.Server
	mu   sync.Mutex
	seen []struct{ method, bearer, session string }
}

type bearerKey struct{}

func newBearerMCP(t *testing.T) *bearerMCP {
	t.Helper()
	m := server.NewMCPServer("up", "1.0.0")
	m.AddTool(mcp.NewTool("get_whoami"), func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		v, _ := ctx.Value(bearerKey{}).(string)
		return mcp.NewToolResultText(v), nil
	})
	inner := server.NewStreamableHTTPServer(m, server.WithHTTPContextFunc(func(ctx context.Context, r *http.Request) context.Context {
		return context.WithValue(ctx, bearerKey{}, r.Header.Get("Authorization"))
	}))
	b := &bearerMCP{}
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
		b.mu.Lock()
		b.seen = append(b.seen, struct{ method, bearer, session string }{method, r.Header.Get("Authorization"), r.Header.Get("Mcp-Session-Id")})
		b.mu.Unlock()
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(b.srv.Close)
	return b
}

// bearersPerSession maps session id -> bearers seen on JSON-RPC requests.
func (b *bearerMCP) bearersPerSession() map[string]map[string]bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := map[string]map[string]bool{}
	for _, s := range b.seen {
		if s.session == "" || s.method == "" {
			continue
		}
		if out[s.session] == nil {
			out[s.session] = map[string]bool{}
		}
		out[s.session][s.bearer] = true
	}
	return out
}

func (b *bearerMCP) bearers(method string) map[string]int {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := map[string]int{}
	for _, s := range b.seen {
		if s.method == method {
			out[s.bearer]++
		}
	}
	return out
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

func TestPerUserOAuthEndToEnd(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "pu.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ids := identity.New(db)
	for _, q := range []string{
		`INSERT INTO users(id, username, password_hash, created_at, updated_at) VALUES('u_ada','ada','x',1,1)`,
		`INSERT INTO users(id, username, password_hash, created_at, updated_at) VALUES('u_bob','bob','x',1,1)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	_, adaAgent, err := ids.CreateAgentWithToken(ctx, "u_ada", "ada-bot")
	if err != nil {
		t.Fatal(err)
	}
	_, bobAgent, err := ids.CreateAgentWithToken(ctx, "u_bob", "bob-bot")
	if err != nil {
		t.Fatal(err)
	}

	// Both are members: the server must be granted to them, as an admin
	// would on the Users page.
	acc := access.New(db)
	for _, uid := range []string{"u_ada", "u_bob"} {
		if err := acc.SetGroups(ctx, uid, []string{"linear"}, "admin"); err != nil {
			t.Fatal(err)
		}
	}
	bus, _ := approval.New(ctx, db)
	gw := gateway.New(gateway.Options{
		Policy: policy.New(db), Approval: bus, Audit: audit.New(db), Memory: memory.New(db), Hub: realtime.NewHub(),
		Access: acc, Owners: acc, PublicURL: "https://toolyard.example",
	})
	t.Cleanup(func() { _ = gw.Close() })

	key := make([]byte, oauth.MasterKeySize)
	_, _ = rand.Read(key)
	cipher, _ := oauth.NewCipher(key)
	idp := tokenIdP(t)
	oa := oauth.New(db, cipher, nil, nil, nil)
	oa.SetHTTPClient(idp.Client())
	svc := New(db, gw)
	svc.SetAuth(oa)
	svc.SetPerUserAuth(oa)
	oa.SetUserReauthHook(svc.DropUserConnection)

	up := newBearerMCP(t)
	if _, err := svc.Add(ctx, Server{Name: "linear", Transport: "http", URL: up.srv.URL, AuthMode: AuthPerUser}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := oa.PutClient(ctx, oauth.ClientRecord{
		UpstreamName: "linear", Issuer: idp.URL, AuthorizationEndpoint: idp.URL + "/authorize", TokenEndpoint: idp.URL + "/token",
		ClientID: "cid", RedirectURI: "https://toolyard.example/v1/mcp-oauth/callback", TokenEndpointAuthMethod: "none",
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := svc.Get(ctx, "linear")
	if got.LastStatus != gateway.StatusWaitingSignIn || got.ToolCount != 0 {
		t.Fatalf("before any sign-in: %q/%d", got.LastStatus, got.ToolCount)
	}

	// Each person signs in the way the callback does, then the service
	// is told; the first one brings the tools.
	connect := func(uid, code string) {
		t.Helper()
		_, state, err := oa.BeginForUser(ctx, "linear", uid)
		if err != nil {
			t.Fatal(err)
		}
		p, err := oa.LoadPending(ctx, state)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := oa.ExchangeCodeForUser(ctx, "linear", p.UserID, code, p.CodeVerifier); err != nil {
			t.Fatal(err)
		}
		svc.ReconnectAfterUserAuth(ctx, "linear", uid)
	}
	connect("u_ada", "ada")
	got, _ = svc.Get(ctx, "linear")
	if got.LastStatus != "ok" || got.ToolCount != 1 || gw.UpstreamToolCount("linear") != 1 {
		t.Fatalf("after ada: %q/%d (gw %d)", got.LastStatus, got.ToolCount, gw.UpstreamToolCount("linear"))
	}
	if b := up.bearers("initialize"); len(b) != 1 || b["Bearer at-ada"] != 1 {
		t.Fatalf("initialize bearers after ada = %v", b)
	}
	connect("u_bob", "bob")

	call := func(agentID string) *mcp.CallToolResult {
		t.Helper()
		res, err := gw.RouteCall(gateway.WithAgentID(ctx, agentID), "test", "linear.get_whoami",
			map[string]any{gateway.ReasonField: "end-to-end per-user oauth test call"})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if res := call(adaAgent.ID); res.IsError || resultText(res) != "Bearer at-ada" {
		t.Fatalf("ada's agent ran as %q (isError=%v)", resultText(res), res.IsError)
	}
	if res := call(bobAgent.ID); res.IsError || resultText(res) != "Bearer at-bob" {
		t.Fatalf("bob's agent ran as %q (isError=%v)", resultText(res), res.IsError)
	}
	sessions := up.bearersPerSession()
	if len(sessions) != 2 {
		t.Fatalf("sessions = %v", sessions)
	}
	for sid, bs := range sessions {
		if len(bs) != 1 {
			t.Fatalf("session %s carried %v", sid, bs)
		}
	}
	if gw.LiveUpstreamCount() != 2 {
		t.Fatalf("live = %d", gw.LiveUpstreamCount())
	}

	// A 401 reported for bob closes his connection through the hook.
	oa.MarkUserUnauthorized(ctx, "linear", "u_bob", "401")
	deadline := 50
	for gw.LiveUpstreamCount() != 1 && deadline > 0 {
		deadline--
		time.Sleep(10 * time.Millisecond)
	}
	if gw.LiveUpstreamCount() != 1 {
		t.Fatalf("bob's connection not dropped after 401: live=%d", gw.LiveUpstreamCount())
	}
	if res := call(bobAgent.ID); !res.IsError || !strings.Contains(resultText(res), "#connections") {
		t.Fatalf("bob after 401: isError=%v %q", res.IsError, resultText(res))
	}

	// Switching the server to shared replaces the per-person sessions with
	// one shared connection and keeps everyone's rows; switching back
	// finds them again. No token is dropped either way.
	shared := AuthShared
	got, err = svc.Update(ctx, "linear", Patch{AuthMode: &shared})
	if err != nil {
		t.Fatalf("to shared: %v", err)
	}
	if got.AuthMode != AuthShared || gw.IsPerUser("linear") || got.ToolCount != 1 {
		t.Fatalf("after switch to shared: mode=%q perUser=%v tools=%d", got.AuthMode, gw.IsPerUser("linear"), got.ToolCount)
	}
	if res := call(adaAgent.ID); res.IsError || resultText(res) != "" {
		t.Fatalf("shared call carried a per-user bearer: %q (isError=%v)", resultText(res), res.IsError)
	}
	rows, err := oa.ListUserConnections(ctx, "linear")
	if err != nil || len(rows) != 2 {
		t.Fatalf("user rows after switch = %v %v", rows, err)
	}
	perUser := AuthPerUser
	got, err = svc.Update(ctx, "linear", Patch{AuthMode: &perUser})
	if err != nil {
		t.Fatalf("back to per_user: %v", err)
	}
	if got.AuthMode != AuthPerUser || !gw.IsPerUser("linear") || got.LastStatus != "ok" || got.ToolCount != 1 {
		t.Fatalf("after switch back: %+v", got)
	}
	if res := call(adaAgent.ID); res.IsError || resultText(res) != "Bearer at-ada" {
		t.Fatalf("ada after switch back ran as %q", resultText(res))
	}

	// Removing the server drops the client and every person's row.
	if err := svc.Remove(ctx, "linear"); err != nil {
		t.Fatal(err)
	}
	if rows, _ := oa.ListUserConnections(ctx, "linear"); len(rows) != 0 {
		t.Fatalf("rows after remove = %v", rows)
	}
}
