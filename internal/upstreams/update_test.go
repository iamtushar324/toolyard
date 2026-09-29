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
