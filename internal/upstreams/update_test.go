package upstreams

import (
	"context"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"strings"
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
)

// newMCPTestServer serves one read-only tool over streamable HTTP so a
// Service can really connect.
func newMCPTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	m := server.NewMCPServer("up", "1.0.0")
	m.AddTool(mcp.NewTool("get_status"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("ok"), nil
	})
	ts := server.NewTestStreamableHTTPServer(m)
	t.Cleanup(ts.Close)
	return ts
}

func newServiceFixture(t *testing.T) (*Service, *store.DB, *gateway.Gateway) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "up.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	bus, _ := approval.New(ctx, db)
	gw := gateway.New(gateway.Options{Policy: policy.New(db), Approval: bus, Audit: audit.New(db), Memory: memory.New(db), Hub: realtime.NewHub()})
	t.Cleanup(func() { _ = gw.Close() })
	return New(db, gw), db, gw
}

// recAuth is a HeaderProvider that reports OAuth state per server and
// records Disconnect calls; err makes Disconnect fail.
type recAuth struct {
	clients     map[string]bool
	disconnects []string
	err         error
}

func (r *recAuth) HeaderFunc(string) func(ctx context.Context) map[string]string {
	return func(context.Context) map[string]string { return map[string]string{"Authorization": "Bearer live"} }
}
func (r *recAuth) HasClient(_ context.Context, name string) (bool, error) {
	return r.clients[name], nil
}
func (r *recAuth) Disconnect(_ context.Context, name string) error {
	if r.err != nil {
		return r.err
	}
	r.disconnects = append(r.disconnects, name)
	return nil
}

func strp(s string) *string                       { return &s }
func boolp(b bool) *bool                          { return &b }
func mapp(m map[string]string) *map[string]string { return &m }

func setIdentity(id *IdentityForwarding) OptionalIdentity {
	return OptionalIdentity{Set: true, Value: id}
}

// TestIdentityPersists: Add stores identity_json, list and get read it back,
// and the live upstream's config names the header.
func TestIdentityPersists(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newServiceFixture(t)
	ts := newMCPTestServer(t)

	added, err := svc.Add(ctx, Server{Name: "bk", Transport: "http", URL: ts.URL,
		Identity: &IdentityForwarding{Header: "x-bk-bifrost-vk", Register: true}})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if added.Identity == nil || added.Identity.Header != "x-bk-bifrost-vk" || !added.Identity.Register {
		t.Fatalf("Add returned identity %+v", added.Identity)
	}
	got, err := svc.Get(ctx, "bk")
	if err != nil {
		t.Fatal(err)
	}
	if got.Identity == nil || got.Identity.Header != "x-bk-bifrost-vk" || !got.Identity.Register {
		t.Fatalf("Get identity = %+v", got.Identity)
	}
	all, err := svc.List(ctx)
	if err != nil || len(all) != 1 || all[0].Identity == nil || all[0].Identity.Header != "x-bk-bifrost-vk" {
		t.Fatalf("List = %+v, %v", all, err)
	}

	// A server without identity reads back nil, not an empty struct.
	if _, err := svc.Add(ctx, Server{Name: "plain", Transport: "http", URL: ts.URL}); err != nil {
		t.Fatalf("Add plain: %v", err)
	}
	if p, _ := svc.Get(ctx, "plain"); p.Identity != nil {
		t.Fatalf("plain identity = %+v, want nil", p.Identity)
	}

	// The clash rule holds at Add: nothing is saved.
	_, err = svc.Add(ctx, Server{Name: "pinned", Transport: "http", URL: ts.URL,
		Headers:  map[string]string{"X-BK-Bifrost-VK": "fixed"},
		Identity: &IdentityForwarding{Header: "x-bk-bifrost-vk"}})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("Add with pinned identity header: err = %v, want ErrInvalid", err)
	}
	if _, err := svc.Get(ctx, "pinned"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rejected server was saved: %v", err)
	}
}

// TestUpsertBuiltinPersistsIdentity: the built-in path writes identity_json
// on insert and on conflict.
func TestUpsertBuiltinPersistsIdentity(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newServiceFixture(t)
	ts := newMCPTestServer(t)

	if _, err := svc.UpsertBuiltin(ctx, Server{Name: "notes", Transport: "http", URL: ts.URL,
		Identity: &IdentityForwarding{Header: "x-id"}}); err != nil {
		t.Fatalf("UpsertBuiltin: %v", err)
	}
	if got, _ := svc.Get(ctx, "notes"); got.Identity == nil || got.Identity.Header != "x-id" {
		t.Fatalf("identity after insert = %+v", got.Identity)
	}
	if _, err := svc.UpsertBuiltin(ctx, Server{Name: "notes", Transport: "http", URL: ts.URL}); err != nil {
		t.Fatalf("UpsertBuiltin again: %v", err)
	}
	if got, _ := svc.Get(ctx, "notes"); got.Identity != nil {
		t.Fatalf("identity after conflict update = %+v, want nil", got.Identity)
	}
}

// TestUpdate edits url, headers, env, identity and enabled in place: the row
// keeps its created_at (it is never re-created), the live upstream is
// reconnected with the new config, and an explicit nil identity clears it.
func TestUpdate(t *testing.T) {
	ctx := context.Background()
	svc, db, gw := newServiceFixture(t)
	ts1 := newMCPTestServer(t)
	ts2 := newMCPTestServer(t)

	orig, err := svc.Add(ctx, Server{Name: "bk", Transport: "http", URL: ts1.URL,
		Headers: map[string]string{"X-Api-Key": "shared"}})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if gw.UpstreamToolCount("bk") != 1 {
		t.Fatalf("tool count after Add = %d", gw.UpstreamToolCount("bk"))
	}

	// url + identity
	got, err := svc.Update(ctx, "bk", Patch{URL: strp(ts2.URL),
		Identity: setIdentity(&IdentityForwarding{Header: "x-bk-bifrost-vk", Register: true})})
	if err != nil {
		t.Fatalf("Update url+identity: %v", err)
	}
	if got.URL != ts2.URL || got.Identity == nil || got.Identity.Header != "x-bk-bifrost-vk" || !got.Identity.Register {
		t.Fatalf("after update: url=%q identity=%+v", got.URL, got.Identity)
	}
	if got.Headers["X-Api-Key"] != "shared" {
		t.Fatalf("headers changed by an unrelated patch: %+v", got.Headers)
	}
	if got.CreatedAt != orig.CreatedAt {
		t.Fatalf("created_at changed %d -> %d: row was re-created", orig.CreatedAt, got.CreatedAt)
	}
	if got.LastStatus != "ok" || gw.UpstreamToolCount("bk") != 1 {
		t.Fatalf("not reconnected: status=%q err=%q tools=%d", got.LastStatus, got.LastError, gw.UpstreamToolCount("bk"))
	}

	// headers + env replace the whole map
	got, err = svc.Update(ctx, "bk", Patch{Headers: mapp(map[string]string{"X-Other": "v"}), Env: mapp(map[string]string{"A": "1"})})
	if err != nil {
		t.Fatalf("Update headers: %v", err)
	}
	if _, still := got.Headers["X-Api-Key"]; still || got.Headers["X-Other"] != "v" || got.Env["A"] != "1" {
		t.Fatalf("headers/env = %+v / %+v", got.Headers, got.Env)
	}
	if got.Identity == nil {
		t.Fatal("identity dropped by a headers patch")
	}

	// identity: null clears
	got, err = svc.Update(ctx, "bk", Patch{Identity: setIdentity(nil)})
	if err != nil {
		t.Fatalf("Update clear identity: %v", err)
	}
	if got.Identity != nil {
		t.Fatalf("identity not cleared: %+v", got.Identity)
	}
	var raw *string
	if err := db.QueryRowContext(ctx, `SELECT identity_json FROM upstream_servers WHERE name='bk'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw != nil {
		t.Fatalf("identity_json = %q, want NULL", *raw)
	}

	// empty patch is a no-op that still succeeds
	if _, err := svc.Update(ctx, "bk", Patch{}); err != nil {
		t.Fatalf("empty patch: %v", err)
	}

	// enabled: false disconnects; true reconnects
	got, err = svc.Update(ctx, "bk", Patch{Enabled: boolp(false)})
	if err != nil {
		t.Fatalf("disable: %v", err)
	}
	if got.Enabled || gw.UpstreamToolCount("bk") != 0 || got.LastStatus != "disabled" {
		t.Fatalf("after disable: enabled=%v tools=%d status=%q", got.Enabled, gw.UpstreamToolCount("bk"), got.LastStatus)
	}
	got, err = svc.Update(ctx, "bk", Patch{Enabled: boolp(true)})
	if err != nil {
		t.Fatalf("enable: %v", err)
	}
	if !got.Enabled || gw.UpstreamToolCount("bk") != 1 || got.LastStatus != "ok" {
		t.Fatalf("after enable: enabled=%v tools=%d status=%q", got.Enabled, gw.UpstreamToolCount("bk"), got.LastStatus)
	}
}

// TestUpdateRejects: invalid patches change nothing, unknown names and
// built-ins are refused, and a failed reconnect keeps the saved config.
func TestUpdateRejects(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newServiceFixture(t)
	svc.SetSecrets(fakeResolver{store: map[string]string{"KNOWN": "v"}})
	ts := newMCPTestServer(t)
	if _, err := svc.Add(ctx, Server{Name: "bk", Transport: "http", URL: ts.URL}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	bad := map[string]Patch{
		"reserved header": {Identity: setIdentity(&IdentityForwarding{Header: "Authorization"})},
		"bad token":       {Identity: setIdentity(&IdentityForwarding{Header: "x id"})},
		"static clash":    {Headers: mapp(map[string]string{"X-BK-BIFROST-VK": "x"}), Identity: setIdentity(&IdentityForwarding{Header: "x-bk-bifrost-vk"})},
		"empty url":       {URL: strp("")},
		"unknown secret":  {Headers: mapp(map[string]string{"X-Key": "secret://MISSING"})},
		"malformed ref":   {Headers: mapp(map[string]string{"X-Key": "secret://lower"})},
	}
	for name, p := range bad {
		if _, err := svc.Update(ctx, "bk", p); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
	got, _ := svc.Get(ctx, "bk")
	if got.URL != ts.URL || got.Identity != nil || len(got.Headers) != 0 {
		t.Fatalf("rejected patch changed the row: %+v", got)
	}

	// A clash with the existing static headers is caught even when only
	// the identity is patched.
	if _, err := svc.Update(ctx, "bk", Patch{Headers: mapp(map[string]string{"x-bk-bifrost-vk": "pinned"})}); err != nil {
		t.Fatalf("set static header: %v", err)
	}
	if _, err := svc.Update(ctx, "bk", Patch{Identity: setIdentity(&IdentityForwarding{Header: "X-BK-Bifrost-VK"})}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("identity over an existing static header: err = %v, want ErrInvalid", err)
	}

	if _, err := svc.Update(ctx, "nope", Patch{URL: strp(ts.URL)}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown: err = %v, want ErrNotFound", err)
	}
	if _, err := svc.Update(ctx, "mempalace", Patch{URL: strp(ts.URL)}); !errors.Is(err, ErrReserved) {
		t.Fatalf("built-in: err = %v, want ErrReserved", err)
	}

	// Identity on a stdio row is refused.
	if _, err := svc.Add(ctx, Server{Name: "fs", Transport: "stdio", Command: "/nonexistent/toolyard-xyz"}); err == nil {
		t.Fatal("expected the bogus stdio command to fail to connect")
	}
	if _, err := svc.Update(ctx, "fs", Patch{Identity: setIdentity(&IdentityForwarding{Header: "x-id"})}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("identity on stdio: err = %v, want ErrInvalid", err)
	}

	// A url that doesn't answer: the config is saved, the error recorded.
	dead := httptest.NewServer(nil)
	dead.Close()
	got, err := svc.Update(ctx, "bk", Patch{URL: strp(dead.URL)})
	if err == nil {
		t.Fatal("expected a connect error for a dead url")
	}
	if got == nil || got.URL != dead.URL || got.LastStatus == "ok" || !strings.Contains(got.LastError, "bk") {
		t.Fatalf("after failed reconnect: %+v", got)
	}
}

// TestUpdateResetsOAuthOnOriginChange: moving a server to another
// scheme+host drops its OAuth client and tokens first, so the stored
// bearer is never sent to the new host. A path change on the same origin,
// or a move on a server with no OAuth state, keeps everything.
func TestUpdateResetsOAuthOnOriginChange(t *testing.T) {
	ctx := context.Background()
	svc, _, gw := newServiceFixture(t)
	auth := &recAuth{clients: map[string]bool{"bk": true}}
	svc.SetAuth(auth)
	ts1 := newMCPTestServer(t)
	ts2 := newMCPTestServer(t) // another port: another origin
	if _, err := svc.Add(ctx, Server{Name: "bk", Transport: "http", URL: ts1.URL}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Same origin, new path: OAuth untouched.
	got, err := svc.Update(ctx, "bk", Patch{URL: strp(ts1.URL + "/v2")})
	if err != nil {
		t.Fatalf("same-origin update: %v", err)
	}
	if got.OAuthReset || len(auth.disconnects) != 0 || got.URL != ts1.URL+"/v2" || got.LastStatus != "ok" {
		t.Fatalf("same-origin update: reset=%v disconnects=%v url=%q status=%q", got.OAuthReset, auth.disconnects, got.URL, got.LastStatus)
	}
	// Only the scheme/host matter, not letter case or an unchanged path.
	upper := "HTTP://" + strings.TrimPrefix(ts1.URL, "http://") + "/v2"
	if got, err = svc.Update(ctx, "bk", Patch{URL: strp(upper)}); err != nil || got.OAuthReset || len(auth.disconnects) != 0 {
		t.Fatalf("case-only origin change: err=%v reset=%v disconnects=%v", err, got.OAuthReset, auth.disconnects)
	}
	// Non-url patches never reset, whatever the origin.
	if got, err = svc.Update(ctx, "bk", Patch{Headers: mapp(map[string]string{"X-Other": "v"})}); err != nil || got.OAuthReset || len(auth.disconnects) != 0 {
		t.Fatalf("headers patch: err=%v reset=%v disconnects=%v", err, got.OAuthReset, auth.disconnects)
	}

	// New origin with OAuth state: reset, reported, reconnected.
	got, err = svc.Update(ctx, "bk", Patch{URL: strp(ts2.URL)})
	if err != nil {
		t.Fatalf("cross-origin update: %v", err)
	}
	if !got.OAuthReset || len(auth.disconnects) != 1 || auth.disconnects[0] != "bk" {
		t.Fatalf("cross-origin update: reset=%v disconnects=%v", got.OAuthReset, auth.disconnects)
	}
	if got.URL != ts2.URL || got.LastStatus != "ok" || gw.UpstreamToolCount("bk") != 1 {
		t.Fatalf("after cross-origin update: url=%q status=%q tools=%d", got.URL, got.LastStatus, gw.UpstreamToolCount("bk"))
	}
	// The flag is a response detail, not a stored one.
	if again, _ := svc.Get(ctx, "bk"); again.OAuthReset {
		t.Fatal("oauth_reset persisted on the row")
	}

	// No OAuth state: a move is just a move.
	auth.clients["bk"] = false
	got, err = svc.Update(ctx, "bk", Patch{URL: strp(ts1.URL)})
	if err != nil || got.OAuthReset || len(auth.disconnects) != 1 {
		t.Fatalf("move without oauth state: err=%v reset=%v disconnects=%v", err, got.OAuthReset, auth.disconnects)
	}

	// A failed reset blocks the change: url stays, upstream stays up.
	auth.clients["bk"] = true
	auth.err = errors.New("db locked")
	if _, err := svc.Update(ctx, "bk", Patch{URL: strp(ts2.URL)}); err == nil || !strings.Contains(err.Error(), "db locked") {
		t.Fatalf("failed reset: err = %v", err)
	}
	if cur, _ := svc.Get(ctx, "bk"); cur.URL != ts1.URL || gw.UpstreamToolCount("bk") != 1 {
		t.Fatalf("failed reset changed state: url=%q tools=%d", cur.URL, gw.UpstreamToolCount("bk"))
	}
}

// TestMaskedValuesRejectedOnSave: neither Add nor Update accepts the mask
// placeholder as a header or env value, and a rejected Update leaves the
// real value in place.
func TestMaskedValuesRejectedOnSave(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newServiceFixture(t)
	ts := newMCPTestServer(t)

	_, err := svc.Add(ctx, Server{Name: "bk", Transport: "http", URL: ts.URL, Headers: map[string]string{"X-Api-Key": MaskedValue}})
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "masked value") {
		t.Fatalf("Add with masked header: err = %v", err)
	}
	if _, err := svc.Get(ctx, "bk"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rejected Add saved a row: %v", err)
	}

	if _, err := svc.Add(ctx, Server{Name: "bk", Transport: "http", URL: ts.URL, Headers: map[string]string{"X-Api-Key": "real"}}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	// The round-trip an API client would do: PATCH the masked GET view back.
	masked := Masked(Server{Headers: map[string]string{"X-Api-Key": "real"}, Env: map[string]string{"TOKEN": "t"}})
	if _, err := svc.Update(ctx, "bk", Patch{Headers: mapp(masked.Headers)}); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "X-Api-Key") {
		t.Fatalf("Update with masked header: err = %v", err)
	}
	if _, err := svc.Update(ctx, "bk", Patch{Env: mapp(masked.Env)}); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "TOKEN") {
		t.Fatalf("Update with masked env: err = %v", err)
	}
	if cur, _ := svc.Get(ctx, "bk"); cur.Headers["X-Api-Key"] != "real" || len(cur.Env) != 0 {
		t.Fatalf("rejected Update changed the row: %+v", cur)
	}
}
