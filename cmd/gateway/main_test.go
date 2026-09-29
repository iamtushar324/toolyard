package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/upstreams"
)

func TestMCPCredential(t *testing.T) {
	cases := []struct {
		name      string
		auth, vk  string
		wantTok   string
		wantFound bool
	}{
		{"bearer only", "Bearer ag_1.tok", "", "ag_1.tok", true},
		{"x-bf-vk only", "", "ag_1.vk", "ag_1.vk", true},
		{"x-bf-vk padded", "", "  ag_1.vk ", "ag_1.vk", true},
		{"both: Authorization wins", "Bearer ag_1.tok", "ag_2.vk", "ag_1.tok", true},
		{"non-bearer Authorization blocks x-bf-vk", "Basic abc", "ag_1.vk", "", false},
		{"neither", "", "", "", false},
		{"empty bearer", "Bearer ", "", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
			if c.auth != "" {
				r.Header.Set("Authorization", c.auth)
			}
			if c.vk != "" {
				r.Header.Set("X-Bf-Vk", c.vk)
			}
			tok, ok := mcpCredential(r)
			if tok != c.wantTok || ok != c.wantFound {
				t.Errorf("mcpCredential = (%q, %v), want (%q, %v)", tok, ok, c.wantTok, c.wantFound)
			}
		})
	}
	// Header names are case-insensitive: a lowercase x-bf-vk is the same.
	r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	r.Header["x-bf-vk"] = []string{"raw"}
	r.Header.Set("x-bf-vk", "ag_1.vk")
	if tok, ok := mcpCredential(r); !ok || tok != "ag_1.vk" {
		t.Errorf("canonical x-bf-vk: (%q, %v)", tok, ok)
	}
}

func TestRegistryUpstreams(t *testing.T) {
	servers := []upstreams.Server{
		{Name: "BkCoreServicesProd", Identity: &upstreams.IdentityForwarding{Header: "x-bk-bifrost-vk", Register: true}},
		{Name: "BkDocsServices", Identity: &upstreams.IdentityForwarding{Header: "x-bk-bifrost-vk"}},
		{Name: "github"},
		{Name: "BkCoreServices", Identity: &upstreams.IdentityForwarding{Header: "x-bk-bifrost-vk", Register: true}},
	}
	got := registryUpstreams(servers)
	if len(got) != 2 || got[0] != "BkCoreServicesProd" || got[1] != "BkCoreServices" {
		t.Errorf("registryUpstreams = %v", got)
	}
	if got := registryUpstreams(nil); len(got) != 0 {
		t.Errorf("registryUpstreams(nil) = %v", got)
	}
}
