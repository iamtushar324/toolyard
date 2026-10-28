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
		{Name: "alpha", Description: "bk t3 on the production\x1b server DS1\u202e", Transport: "http", Enabled: true, Status: "ok", AuthMode: "shared"},
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
	for _, s := range out.Servers {
		wantSource := map[string]string{"alpha": DescriptionAdmin, "beta": DescriptionAdmin, "gamma": ""}[s.Name]
		if s.DescriptionSource != wantSource {
			t.Errorf("%s description_source = %q, want %q", s.Name, s.DescriptionSource, wantSource)
		}
	}
}

// TestSanitizeDescription: control characters (NUL, ESC, C1) and bidi
// controls are dropped before anything is cut, for the admin's text and
// for the upstream's.
func TestSanitizeDescription(t *testing.T) {
	in := "bk t3\x00 on \x1b[31mDS1‮⁦ host‏\u0085\tnow"
	want := "bk t3 on [31mDS1 host now"
	if got := sanitizeDescription(in); got != want {
		t.Fatalf("sanitizeDescription = %q, want %q", got, want)
	}
	if got := initDescription(&mcp.InitializeResult{Instructions: in}); got != want {
		t.Fatalf("initDescription = %q, want %q", got, want)
	}
	// Only controls: nothing left, so the next field is used.
	if got := initDescription(&mcp.InitializeResult{Instructions: "\x00‮", ServerInfo: mcp.Implementation{Title: "BK T3"}}); got != "BK T3" {
		t.Fatalf("controls-only instructions: %q", got)
	}
	// The cut counts the clean text: 300 runes of controls then text keep
	// the text.
	if got := initDescription(&mcp.InitializeResult{Instructions: strings.Repeat("‮", 400) + "the usm host"}); got != "the usm host" {
		t.Fatalf("controls before the text: %q", got)
	}
	g := &Gateway{}
	g.SetServerDescription("bk", "\x1bDS1‮")
	if d, src := g.describeServer("bk", ""); d != "DS1" || src != DescriptionAdmin {
		t.Fatalf("admin description = %q (%s)", d, src)
	}
}

// TestDescribeServerSources: the admin's text is "admin"; a shared
// upstream's own text is "upstream"; a per_user upstream's own text comes
// from the asking user's connection only, never another person's.
func TestDescribeServerSources(t *testing.T) {
	g := &Gateway{upstreams: map[string]*upstream{}, perUser: map[string]*perUserGroup{}}
	shared := &upstream{cfg: UpstreamConfig{Name: "bkt3_usm"}}
	shared.selfDescription.Store("bk t3 on usm")
	g.upstreams["bkt3_usm"] = shared
	pu := &perUserGroup{cfg: UpstreamConfig{Name: "linear"}, pool: g, conns: map[string]*upstream{}}
	g.perUser["linear"] = pu
	alice := &upstream{cfg: UpstreamConfig{Name: "linear"}, userID: "u_alice"}
	alice.selfDescription.Store("Linear for alice's workspace")
	pu.conns["u_alice"] = alice
	// Bob's connection exists but was never dialled.
	pu.conns["u_bob"] = &upstream{cfg: UpstreamConfig{Name: "linear"}, userID: "u_bob"}

	for _, tc := range []struct {
		name, user, desc, source string
	}{
		{"bkt3_usm", "", "bk t3 on usm", DescriptionUpstream},
		{"linear", "u_alice", "Linear for alice's workspace", DescriptionUpstream},
		{"linear", "u_bob", "", ""},
		{"linear", "u_carol", "", ""},
		{"linear", "", "", ""},
		{"missing", "u_alice", "", ""},
	} {
		if d, src := g.describeServer(tc.name, tc.user); d != tc.desc || src != tc.source {
			t.Errorf("describeServer(%s, %s) = %q (%s), want %q (%s)", tc.name, tc.user, d, src, tc.desc, tc.source)
		}
	}
	if len(pu.conns) != 2 {
		t.Fatalf("describeServer created a connection: %d", len(pu.conns))
	}
	g.SetServerDescription("linear", "Linear, each person's own account")
	if d, src := g.describeServer("linear", "u_bob"); d != "Linear, each person's own account" || src != DescriptionAdmin {
		t.Fatalf("admin description = %q (%s)", d, src)
	}
}
