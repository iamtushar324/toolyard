package upstreams

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/gateway"
)

// bearerFake is a PerUserAuth whose users have fixed bearers.
type bearerFake struct {
	bearers map[string]string // user -> bearer
}

func (b bearerFake) ConnectedUsers(context.Context, string) ([]string, error) {
	var out []string
	for u := range b.bearers {
		out = append(out, u)
	}
	return out, nil
}
func (b bearerFake) UserConnected(_ context.Context, _, uid string) (bool, error) {
	_, ok := b.bearers[uid]
	return ok, nil
}
func (b bearerFake) FreshenUserToken(context.Context, string, string) error       { return nil }
func (b bearerFake) RefreshUserToken(context.Context, string, string) error       { return nil }
func (b bearerFake) MarkUserUnauthorized(context.Context, string, string, string) {}
func (b bearerFake) UserHeaderFunc(string) func(ctx context.Context, userID string) map[string]string {
	return func(_ context.Context, uid string) map[string]string {
		tok, ok := b.bearers[uid]
		if !ok {
			return nil
		}
		return map[string]string{"Authorization": "Bearer " + tok}
	}
}

// TestPerUserRefusesStaticAuthorization: a server where each person signs
// in cannot carry a static Authorization header in any letter case, on
// add or when switching an existing server.
func TestPerUserRefusesStaticAuthorization(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newServiceFixture(t)
	svc.SetPerUserAuth(bearerFake{bearers: map[string]string{}})
	for _, hdr := range []string{"Authorization", "authorization", "AUTHORIZATION"} {
		_, err := svc.Add(ctx, Server{Name: "x", Transport: "http", URL: "http://127.0.0.1:1/mcp",
			AuthMode: AuthPerUser, Headers: map[string]string{hdr: "Bearer admin-pat"}})
		if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), hdr) {
			t.Fatalf("add with static %s: %v", hdr, err)
		}
	}
	// Other static headers are fine.
	ts := newMCPTestServer(t)
	if _, err := svc.Add(ctx, Server{Name: "ok", Transport: "http", URL: ts.URL, AuthMode: AuthPerUser,
		Headers: map[string]string{"X-Api-Key": "k"}}); err != nil {
		t.Fatalf("add with another header: %v", err)
	}
	// A shared server with a static Authorization cannot be switched.
	if _, err := svc.Add(ctx, Server{Name: "shared", Transport: "http", URL: ts.URL,
		Headers: map[string]string{"Authorization": "Bearer pat"}}); err != nil {
		t.Fatalf("add shared: %v", err)
	}
	mode := AuthPerUser
	if _, err := svc.Update(ctx, "shared", Patch{AuthMode: &mode}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("switch with static Authorization: %v", err)
	}
	// Dropping the header first allows the switch.
	if _, err := svc.Update(ctx, "shared", Patch{Headers: mapp(map[string]string{}), AuthMode: &mode}); err != nil {
		t.Fatalf("switch after dropping the header: %v", err)
	}
}

// TestToCfgPerUserNeverSendsStaticAuthorization: even on a row that
// carries a static Authorization (saved before the rule, or by hand), the
// per_user header function sends only the person's bearer or no
// Authorization at all, while other static headers still go out.
func TestToCfgPerUserNeverSendsStaticAuthorization(t *testing.T) {
	svc, _, _ := newServiceFixture(t)
	svc.SetPerUserAuth(bearerFake{bearers: map[string]string{"u_ada": "ada-tok"}})
	svc.SetAuth(&recAuth{}) // a shared bearer provider must not leak in either
	cfg := svc.toCfg(Server{Name: "linear", Transport: "http", URL: "http://127.0.0.1:1/mcp", AuthMode: AuthPerUser,
		Headers: map[string]string{"X-Api-Key": "k", "authorization": "Bearer admin-pat", "Authorization": "Bearer admin-pat-2"}})
	if !cfg.PerUser || cfg.PerUserAuth == nil || cfg.HeaderFunc == nil {
		t.Fatalf("cfg = %+v", cfg)
	}
	authz := func(h map[string]string) []string {
		var out []string
		for k, v := range h {
			if strings.EqualFold(k, "Authorization") {
				out = append(out, k+"="+v)
			}
		}
		return out
	}
	with := cfg.HeaderFunc(gateway.WithUpstreamUser(context.Background(), "u_ada"))
	if got := authz(with); len(got) != 1 || got[0] != "Authorization=Bearer ada-tok" {
		t.Fatalf("headers for ada = %v", with)
	}
	if with["X-Api-Key"] != "k" {
		t.Fatalf("static header lost: %v", with)
	}
	without := cfg.HeaderFunc(gateway.WithUpstreamUser(context.Background(), "u_nobody"))
	if got := authz(without); len(got) != 0 {
		t.Fatalf("headers for a user with no bearer carry Authorization: %v", without)
	}
	if without["X-Api-Key"] != "k" {
		t.Fatalf("static header lost: %v", without)
	}
	none := cfg.HeaderFunc(context.Background())
	if got := authz(none); len(got) != 0 {
		t.Fatalf("headers with no user carry Authorization: %v", none)
	}
	// A shared server keeps its static Authorization when there is no OAuth
	// bearer (unchanged behaviour).
	svc.SetAuth(nil)
	shared := svc.toCfg(Server{Name: "s", Transport: "http", URL: "http://127.0.0.1:1/mcp",
		Headers: map[string]string{"Authorization": "Bearer pat"}})
	if got := shared.HeaderFunc(context.Background()); got["Authorization"] != "Bearer pat" {
		t.Fatalf("shared static Authorization = %v", got)
	}
}
