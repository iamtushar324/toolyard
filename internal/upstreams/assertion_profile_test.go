package upstreams

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/tusharbhardwaj/toolyard/internal/approval"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/gateway"
	"github.com/tusharbhardwaj/toolyard/internal/memory"
	"github.com/tusharbhardwaj/toolyard/internal/policy"
	"github.com/tusharbhardwaj/toolyard/internal/realtime"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

const profileEndpoint = "http://127.0.0.1:18791/mcp"

type assertionRemote struct {
	mu                 sync.Mutex
	discoveries, posts int
	caller, operation  string
}

func (r *assertionRemote) Catalog() []mcp.Tool { return []mcp.Tool{mcp.NewTool("get_status")} }
func (r *assertionRemote) Operation(alias string) string {
	if alias == "get_status" {
		return "read_status"
	}
	return ""
}
func (r *assertionRemote) Discover(_ context.Context, caller string) ([]mcp.Tool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.discoveries++
	return r.Catalog(), nil
}
func (r *assertionRemote) Preflight(context.Context, string, string, map[string]any) error {
	return nil
}
func (r *assertionRemote) RequireApprovalBinding(context.Context, string, int64) error { return nil }
func (r *assertionRemote) Call(_ context.Context, caller, operation string, _ map[string]any) (json.RawMessage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.posts++
	r.caller, r.operation = caller, operation
	return json.RawMessage(`{"content":[{"type":"text","text":"ok"}]}`), nil
}

func newAssertionService(t *testing.T) (*Service, *gateway.Gateway, *store.DB) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "upstream.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	bus, err := approval.New(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	gw := gateway.New(gateway.Options{Policy: policy.New(db), Approval: bus, Audit: audit.New(db), Memory: memory.New(db), Hub: realtime.NewHub()})
	gw.RegisterBuiltins()
	t.Cleanup(func() { _ = gw.Close() })
	return New(db, gw), gw, db
}

func assertionServer(name string) Server {
	return Server{Name: name, Transport: "http", URL: profileEndpoint, AuthMode: AuthPerUser, AssertionProfile: "isolated_preview", Enabled: true}
}

func TestAssertionProfileRejectsCredentialAndProcessOverrides(t *testing.T) {
	cases := map[string]func(*Server){
		"shared OAuth":        func(s *Server) { s.AuthMode = AuthShared },
		"empty auth mode":     func(s *Server) { s.AuthMode = "" },
		"static credential":   func(s *Server) { s.Headers = map[string]string{"Authorization": "not-a-real-token"} },
		"custom header":       func(s *Server) { s.Headers = map[string]string{"X-Test": "static"} },
		"environment":         func(s *Server) { s.Env = map[string]string{"PATH": "/not-real"} },
		"command":             func(s *Server) { s.Command = "false" },
		"args":                func(s *Server) { s.Args = []string{"unused"} },
		"identity key":        func(s *Server) { s.Identity = &IdentityForwarding{Header: "x-identity"} },
		"stdio":               func(s *Server) { s.Transport, s.Command = "stdio", "false" },
		"alternate transport": func(s *Server) { s.Transport = "streamable-http" },
		"path traversal":      func(s *Server) { s.AssertionProfile = "../issuer" },
		"absolute path":       func(s *Server) { s.AssertionProfile = "/private/issuer" },
		"URL credential":      func(s *Server) { s.URL = "http://credential@127.0.0.1:18791/mcp" },
		"URL query":           func(s *Server) { s.URL += "?sid=untrusted" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			srv := assertionServer("private_control")
			mutate(&srv)
			if err := validate(srv); !errors.Is(err, ErrInvalid) {
				t.Fatalf("override accepted: %v", err)
			}
		})
	}
}

func TestAssertionProfileCreatedDisabledWithoutDiscovery(t *testing.T) {
	svc, gw, _ := newAssertionService(t)
	providerCalls := 0
	svc.SetAssertionProvider(func(context.Context, Server) (gateway.TrustedUpstream, error) {
		providerCalls++
		return &assertionRemote{}, nil
	})
	got, err := svc.Add(context.Background(), assertionServer("private_control"))
	if err != nil || got.Enabled || got.LastStatus != "disabled" || got.AssertionProfile != "isolated_preview" {
		t.Fatalf("new connector: %v %+v", err, got)
	}
	if providerCalls != 0 || gw.HasTool("private_control.get_status") {
		t.Fatal("creation activated a profile")
	}
	listed, err := svc.List(context.Background())
	if err != nil || len(listed) != 1 || Masked(listed[0]).AssertionProfile != "isolated_preview" {
		t.Fatalf("profile not persisted: %v %+v", err, listed)
	}
	if err := svc.AssertEnabled(context.Background(), got.Name, got.AssertionProfile, got.URL); err == nil {
		t.Fatal("disabled connector accepted a POST")
	}
	if _, err := svc.Reconnect(context.Background(), got.Name); err != nil || providerCalls != 0 {
		t.Fatal("reconnect activated disabled connector")
	}
}

func TestAssertionProfileActivationRequiresTrustedLocalProvider(t *testing.T) {
	svc, gw, _ := newAssertionService(t)
	got, err := svc.Add(context.Background(), assertionServer("private_control"))
	if err != nil {
		t.Fatal(err)
	}
	enabled := true
	if _, err := svc.Update(context.Background(), got.Name, Patch{Enabled: &enabled}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("missing provider enabled: %v", err)
	}
	svc.SetAssertionProvider(func(context.Context, Server) (gateway.TrustedUpstream, error) {
		return nil, errors.New("private path and diagnostics")
	})
	if _, err := svc.Update(context.Background(), got.Name, Patch{Enabled: &enabled}); !errors.Is(err, ErrInvalid) || strings.Contains(err.Error(), "diagnostics") {
		t.Fatalf("unknown profile result unsafe: %v", err)
	}
	stored, err := svc.Get(context.Background(), got.Name)
	if err != nil || stored.Enabled || gw.HasTool("private_control.get_status") {
		t.Fatalf("failed activation changed configuration: %v %+v", err, stored)
	}
}

func TestAssertionProfileLifecycleAndRevocationCheck(t *testing.T) {
	svc, gw, _ := newAssertionService(t)
	remote := &assertionRemote{}
	svc.SetAssertionProvider(func(_ context.Context, srv Server) (gateway.TrustedUpstream, error) {
		if srv.URL != profileEndpoint || srv.AssertionProfile != "isolated_preview" {
			return nil, errors.New("unrecognized profile")
		}
		return remote, nil
	})
	if _, err := svc.Add(context.Background(), assertionServer("renamed_control")); err != nil {
		t.Fatal(err)
	}
	enabled := true
	got, err := svc.Update(context.Background(), "renamed_control", Patch{Enabled: &enabled})
	if err != nil || !got.Enabled || got.LastStatus != "ok" || got.ToolCount != 1 || !gw.HasTool("renamed_control.get_status") {
		t.Fatalf("activation failed: %v %+v", err, got)
	}
	if remote.discoveries != 0 || remote.posts != 0 {
		t.Fatal("catalog registration made an anonymous network request")
	}
	if err := svc.AssertEnabled(context.Background(), got.Name, got.AssertionProfile, got.URL); err != nil {
		t.Fatal(err)
	}
	for _, wrong := range []struct{ name, profile, endpoint string }{{"missing", got.AssertionProfile, got.URL}, {got.Name, "different", got.URL}, {got.Name, got.AssertionProfile, "http://127.0.0.1:1/mcp"}} {
		if err := svc.AssertEnabled(context.Background(), wrong.name, wrong.profile, wrong.endpoint); err == nil {
			t.Fatal("configuration mismatch accepted")
		}
	}
	ctx := gateway.WithAgentID(context.Background(), "ag_synthetic")
	res, err := gw.RouteCall(ctx, "test", "renamed_control.get_status", map[string]any{gateway.ReasonField: "Inspect the isolated synthetic status for this test."})
	if err != nil || res.IsError {
		t.Fatalf("trusted call failed: %v %+v", err, res)
	}
	if remote.caller != "ag_synthetic" || remote.operation != "read_status" {
		t.Fatalf("actual agent or fixed operation lost: %s %s", remote.caller, remote.operation)
	}
	enabled = false
	got, err = svc.Update(context.Background(), got.Name, Patch{Enabled: &enabled})
	if err != nil || got.Enabled || gw.HasTool("renamed_control.get_status") {
		t.Fatalf("disable failed: %v %+v", err, got)
	}
	if err := svc.AssertEnabled(context.Background(), got.Name, got.AssertionProfile, got.URL); err == nil {
		t.Fatal("retained client can POST after disable")
	}
	if err := svc.Remove(context.Background(), got.Name); err != nil {
		t.Fatal(err)
	}
	if err := svc.AssertEnabled(context.Background(), got.Name, got.AssertionProfile, got.URL); err == nil {
		t.Fatal("retained client can POST after removal")
	}
}

func TestAssertionProfileURLAndProfileImmutable(t *testing.T) {
	svc, _, _ := newAssertionService(t)
	if _, err := svc.Add(context.Background(), assertionServer("private_control")); err != nil {
		t.Fatal(err)
	}
	profile, endpoint, mode := "different", "http://127.0.0.1:1/mcp", AuthShared
	for _, patch := range []Patch{{AssertionProfile: &profile}, {URL: &endpoint}, {AuthMode: &mode}, {Headers: &map[string]string{"X-Test": "override"}}} {
		if _, err := svc.Update(context.Background(), "private_control", patch); !errors.Is(err, ErrInvalid) {
			t.Fatalf("immutable edit accepted: %v", err)
		}
	}
	got, err := svc.Get(context.Background(), "private_control")
	if err != nil || got.URL != profileEndpoint || got.AssertionProfile != "isolated_preview" || got.AuthMode != AuthPerUser {
		t.Fatalf("failed edit changed config: %v %+v", err, got)
	}
}

func TestAssertionProfileStartupRegistersOfflineBeforeOrdinaryLoad(t *testing.T) {
	svc, gw, db := newAssertionService(t)
	remote := &assertionRemote{}
	svc.SetAssertionProvider(func(context.Context, Server) (gateway.TrustedUpstream, error) { return remote, nil })
	if _, err := svc.Add(context.Background(), assertionServer("private_control")); err != nil {
		t.Fatal(err)
	}
	enabled := true
	if _, err := svc.Update(context.Background(), "private_control", Patch{Enabled: &enabled}); err != nil {
		t.Fatal(err)
	}
	if err := gw.RemoveUpstream("private_control"); err != nil {
		t.Fatal(err)
	}
	restarted := New(db, gw)
	restarted.SetAssertionProvider(func(context.Context, Server) (gateway.TrustedUpstream, error) { return remote, nil })
	if err := restarted.LoadAll(context.Background()); err != nil || gw.HasTool("private_control.get_status") {
		t.Fatal("ordinary startup used the assertion network path")
	}
	if err := restarted.LoadAssertions(context.Background()); err != nil || !gw.HasTool("private_control.get_status") {
		t.Fatalf("offline catalog absent: %v", err)
	}
	if remote.discoveries != 0 || remote.posts != 0 {
		t.Fatal("startup made an anonymous assertion request")
	}
}

func TestAssertionProfileDoesNotComposeOAuthHeaders(t *testing.T) {
	svc := &Service{auth: fakeAuth{headers: map[string]string{"Authorization": "not-a-real-token"}}}
	cfg := svc.toCfg(assertionServer("private_control"))
	if cfg.HeaderFunc != nil || cfg.IdentityHeader != "" || cfg.PerUserAuth != nil {
		t.Fatal("profile composed an ordinary credential source")
	}
}

var _ gateway.TrustedUpstream = (*assertionRemote)(nil)
