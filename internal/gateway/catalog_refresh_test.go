package gateway

import (
	"context"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// TEC-1628: a live upstream's catalog is read again on the refresh timer.
// Added tools appear, removed ones go, an unchanged catalog changes
// nothing, a suspended upstream is not dialled, and the read never moves
// the idle clock.
func TestRefreshUpstreamCatalogs(t *testing.T) {
	m := server.NewMCPServer("up", "1.0.0")
	m.AddTool(mcp.NewTool("tool_a"), okTool)
	m.AddTool(mcp.NewTool("tool_b"), okTool)
	ts, wire := recordWire(t, server.NewStreamableHTTPServer(m), nil)
	f := newActorFixture(t)
	u := addedUpstream(t, f.gw, "up", ts.URL)
	ctx := context.Background()

	if n := f.gw.RefreshUpstreamCatalogs(ctx); n != 0 {
		t.Fatalf("unchanged catalog: %d changed, want 0", n)
	}

	m.DeleteTools("tool_b")
	m.AddTool(mcp.NewTool("tool_c"), okTool)
	idleBefore := u.lastUsed.Load()
	if n := f.gw.RefreshUpstreamCatalogs(ctx); n != 1 {
		t.Fatalf("changed catalog: %d changed, want 1", n)
	}
	if !hasTool(f.gw, "up.tool_a") || !hasTool(f.gw, "up.tool_c") || hasTool(f.gw, "up.tool_b") {
		t.Fatal("catalog after refresh: want up.tool_a and up.tool_c, without up.tool_b")
	}
	if f.gw.mcp.GetTool("up.tool_c") == nil || f.gw.mcp.GetTool("up.tool_b") != nil {
		t.Fatal("mcp-go's registry after refresh: want up.tool_c, without up.tool_b")
	}
	if u.lastUsed.Load() != idleBefore {
		t.Fatal("a refresh moved the idle clock")
	}

	if !u.suspend() {
		t.Fatal("the idle upstream should suspend")
	}
	lists, inits := wire.count("tools/list"), wire.count("initialize")
	m.AddTool(mcp.NewTool("tool_d"), okTool)
	if n := f.gw.RefreshUpstreamCatalogs(ctx); n != 0 {
		t.Fatalf("suspended upstream: %d changed, want 0", n)
	}
	if wire.count("tools/list") != lists || wire.count("initialize") != inits {
		t.Fatal("a refresh dialled a suspended upstream")
	}
	if hasTool(f.gw, "up.tool_d") {
		t.Fatal("a suspended upstream's catalog changed without a read")
	}
}

// An upstream that restarted with new tools has forgotten our session.
// The refresh re-establishes it once, as a call would, and reads the new
// catalog; the idle clock stays where it was.
func TestRefreshRecoversLostSession(t *testing.T) {
	for _, mode := range []string{"400", "404"} {
		t.Run(mode, func(t *testing.T) {
			up := newForgetfulUpstream(t, mode)
			f := newActorFixture(t)
			u := addedUpstream(t, f.gw, "bk", up.srv.URL)
			up.forget(t)
			up.m.AddTool(mcp.NewTool("get_new"), okTool)
			idleBefore := u.lastUsed.Load()

			if n := f.gw.RefreshUpstreamCatalogs(context.Background()); n != 1 {
				t.Fatalf("refresh after a lost session: %d changed, want 1", n)
			}
			if !hasTool(f.gw, "bk.get_new") {
				t.Fatal("the new tool is missing after the refresh")
			}
			if got := u.recoveries.Load(); got != 1 {
				t.Fatalf("session recoveries = %d, want 1", got)
			}
			if got := up.count("initialize"); got != 2 {
				t.Fatalf("initialize count = %d, want 2", got)
			}
			if u.lastUsed.Load() != idleBefore {
				t.Fatal("the refresh's re-dial moved the idle clock")
			}
		})
	}
}

// A catalog read from a connection that was removed, or replaced by
// another one under the same name, while the read was in flight is not
// applied.
func TestRefreshDropsCatalogOfReplacedUpstream(t *testing.T) {
	ts, _ := newOKUpstream(t, nil)
	f := newActorFixture(t)
	addedUpstream(t, f.gw, "up", ts.URL)
	stale := []mcp.Tool{mcp.NewTool("get_stale")}

	other := &upstream{cfg: UpstreamConfig{Name: "up"}}
	if f.gw.replaceUpstreamTools("up", stale, other) || hasTool(f.gw, "up.get_stale") {
		t.Fatal("a catalog read from a replaced connection was applied")
	}
	if !hasTool(f.gw, "up.get_ok") {
		t.Fatal("the current catalog was lost")
	}

	removed := addedUpstream(t, f.gw, "gone", ts.URL)
	if err := f.gw.RemoveUpstream("gone"); err != nil {
		t.Fatal(err)
	}
	if f.gw.replaceUpstreamTools("gone", stale, removed) || hasTool(f.gw, "gone.get_stale") {
		t.Fatal("a catalog read from a removed upstream was applied")
	}
}

// The catalog compare ignores order, so an upstream that lists its tools
// in a different order each time is not announced on every refresh.
func TestSameToolsIgnoresOrder(t *testing.T) {
	a, b, c := mcp.NewTool("a"), mcp.NewTool("b"), mcp.NewTool("c")
	if !sameTools([]mcp.Tool{a, b}, []mcp.Tool{b, a}) {
		t.Fatal("the same tools in another order compared different")
	}
	if sameTools([]mcp.Tool{a, b}, []mcp.Tool{a, c}) {
		t.Fatal("different tools compared the same")
	}
}
