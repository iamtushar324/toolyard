package oauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"
)

// TestDiscoverGoogleStyleResource reproduces drivemcp.googleapis.com
// (TEC-2072): the protected-resource metadata lives only at the RFC 9728
// path-inserted URL, it names an authorization server on another host with
// a trailing slash, that server lists no resource scopes and no
// registration endpoint, and nothing answers at the resource origin.
func TestDiscoverGoogleStyleResource(t *testing.T) {
	var as *httptest.Server
	as = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/oauth-authorization-server" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                 as.URL,
			"authorization_endpoint": as.URL + "/o/oauth2/v2/auth",
			"token_endpoint":         as.URL + "/token",
		})
	}))
	defer as.Close()

	var hits []string
	rs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.URL.Path)
		if r.URL.Path != "/.well-known/oauth-protected-resource/mcp/v1" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"resource":              "x",
			"authorization_servers": []string{as.URL + "/"},
			"scopes_supported":      []string{"https://www.googleapis.com/auth/drive"},
		})
	}))
	defer rs.Close()

	s := &Service{httpc: rs.Client()}
	md, err := s.Discover(context.Background(), rs.URL+"/mcp/v1")
	if err != nil {
		t.Fatalf("Discover: %v (paths tried: %v)", err, hits)
	}
	if md.AuthorizationEndpoint != as.URL+"/o/oauth2/v2/auth" || md.TokenEndpoint != as.URL+"/token" {
		t.Errorf("endpoints: %+v", md)
	}
	if !reflect.DeepEqual(md.ScopesSupported, []string{"https://www.googleapis.com/auth/drive"}) {
		t.Errorf("resource scopes not carried over: %v", md.ScopesSupported)
	}
	if len(hits) == 0 || hits[0] != "/.well-known/oauth-protected-resource/mcp/v1" {
		t.Errorf("the RFC 9728 path-inserted URL must be tried first: %v", hits)
	}
}

func TestPRCandidates(t *testing.T) {
	u, _ := url.Parse("https://drivemcp.googleapis.com/mcp/v1/")
	want := []string{
		"https://drivemcp.googleapis.com/.well-known/oauth-protected-resource/mcp/v1",
		"https://drivemcp.googleapis.com/.well-known/oauth-protected-resource",
		"https://drivemcp.googleapis.com/mcp/v1/.well-known/oauth-protected-resource",
	}
	if got := prCandidates(u); !reflect.DeepEqual(got, want) {
		t.Errorf("prCandidates = %v", got)
	}
	root, _ := url.Parse("https://mcp.linear.app")
	if got := prCandidates(root); !reflect.DeepEqual(got, []string{"https://mcp.linear.app/.well-known/oauth-protected-resource"}) {
		t.Errorf("root prCandidates = %v", got)
	}
}

func TestASCandidates(t *testing.T) {
	plain := []string{
		"https://accounts.google.com/.well-known/oauth-authorization-server",
		"https://accounts.google.com/.well-known/openid-configuration",
	}
	for _, in := range []string{"https://accounts.google.com/", "https://accounts.google.com"} {
		if got := asCandidates(in); !reflect.DeepEqual(got, plain) {
			t.Errorf("asCandidates(%q) = %v", in, got)
		}
	}
	got := asCandidates("https://login.example.com/tenant1/")
	want := []string{
		"https://login.example.com/.well-known/oauth-authorization-server/tenant1",
		"https://login.example.com/.well-known/openid-configuration/tenant1",
		"https://login.example.com/tenant1/.well-known/oauth-authorization-server",
		"https://login.example.com/tenant1/.well-known/openid-configuration",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("asCandidates(tenant) = %v", got)
	}
}
