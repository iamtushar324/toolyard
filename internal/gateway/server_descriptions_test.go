package gateway

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

func TestInitDescription(t *testing.T) {
	long := strings.Repeat("word ", 100)
	for _, tc := range []struct {
		name string
		res  *mcp.InitializeResult
		want string
	}{
		{"nil", nil, ""},
		{"nothing", &mcp.InitializeResult{ServerInfo: mcp.Implementation{Name: "bkt3"}}, ""},
		{"instructions", &mcp.InitializeResult{Instructions: " bk t3\non DS1 ", ServerInfo: mcp.Implementation{Title: "BK T3"}}, "bk t3 on DS1"},
		{"description", &mcp.InitializeResult{ServerInfo: mcp.Implementation{Description: "the usm host", Title: "BK T3"}}, "the usm host"},
		{"title", &mcp.InitializeResult{ServerInfo: mcp.Implementation{Title: "BK T3"}}, "BK T3"},
	} {
		if got := initDescription(tc.res); got != tc.want {
			t.Errorf("%s: initDescription = %q, want %q", tc.name, got, tc.want)
		}
	}
	got := initDescription(&mcp.InitializeResult{Instructions: long})
	if n := len([]rune(got)); n != selfDescriptionLen || !strings.HasSuffix(got, "…") {
		t.Fatalf("long instructions: %d runes, %q", n, got)
	}
}

// TestServersListShowsDescription: servers.list carries each server's
// description: the provider's (the admin's stored text) first, else the
// gateway's.
func TestServersListShowsDescription(t *testing.T) {
	f := newAccessFixture(t, nil)
	f.gw.SetServersProvider(&fakeServers{rows: []ServerInfo{
		{Name: "alpha", Description: "bk t3 on the production server DS1", Transport: "http", Enabled: true, Status: "ok", AuthMode: "shared"},
		{Name: "beta", Transport: "http", Enabled: true, Status: "ok", AuthMode: "shared"},
		{Name: "gamma", Transport: "http", Enabled: true, Status: "ok", AuthMode: "shared"},
	}})
	f.gw.SetServerDescription("beta", " bk t3 on the USA Mac mini ")
	f.gw.SetServerDescription("gamma", "gone")
	f.gw.SetServerDescription("gamma", "")

	res := f.call(t, f.admin, "servers.list", nil)
	if res.IsError {
		t.Fatalf("servers.list: %s", textOf(res))
	}
	var out struct {
		Servers []ServerInfo `json:"servers"`
	}
	if err := json.Unmarshal([]byte(textOf(res)), &out); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, s := range out.Servers {
		got[s.Name] = s.Description
	}
	want := map[string]string{"alpha": "bk t3 on the production server DS1", "beta": "bk t3 on the USA Mac mini", "gamma": ""}
	for name, d := range want {
		if got[name] != d {
			t.Errorf("%s description = %q, want %q", name, got[name], d)
		}
	}
}
