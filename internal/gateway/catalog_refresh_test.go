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
