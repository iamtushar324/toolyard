package gateway

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tusharbhardwaj/toolyard/internal/access"
	"github.com/tusharbhardwaj/toolyard/internal/approval"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/inbox"
	"github.com/tusharbhardwaj/toolyard/internal/memory"
	"github.com/tusharbhardwaj/toolyard/internal/policy"
	"github.com/tusharbhardwaj/toolyard/internal/realtime"
	"github.com/tusharbhardwaj/toolyard/internal/store/storetest"
)

// toolListFixture is a gateway on the real access service, so each
// agent's tool list comes from its stored scope. Three agents of one admin:
// ag_small (no scope row: the default), ag_full (switched to the full
// list) and ag_deny (small, with inbox.status denied). Besides the
// built-ins and the Inbox tools there is one upstream tool,
// alpha.get_status, a read that runs without approval.
type toolListFixture struct {
	gw    *Gateway
	acc   *access.Service
	calls atomic.Int32
}

func newToolListFixture(t *testing.T) *toolListFixture {
	t.Helper()
	ctx := context.Background()
	db := storetest.Open(t)
	t.Cleanup(func() { _ = db.Close() })
	for _, q := range []string{
		`INSERT INTO users(id, username, password_hash, role, status, created_at, updated_at) VALUES('u_admin','admin','x','admin','active',1,1)`,
		`INSERT INTO agents(id, name, owner_user, token_hash, created_at) VALUES('ag_small','small','u_admin','',1)`,
		`INSERT INTO agents(id, name, owner_user, token_hash, created_at) VALUES('ag_full','full','u_admin','',1)`,
		`INSERT INTO agents(id, name, owner_user, token_hash, created_at) VALUES('ag_deny','deny','u_admin','',1)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	bus, err := approval.New(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	f := &toolListFixture{acc: access.New(db)}
	f.gw = New(Options{Policy: policy.New(db), Approval: bus, Audit: audit.New(db), Hub: realtime.NewHub(), Memory: memory.New(db), Access: f.acc})
	f.gw.RegisterBuiltins()
	t.Cleanup(func() { _ = f.gw.Close() })
	svc, err := inbox.New(ctx, inbox.Options{DB: db, Catalog: f.gw})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Flush)
	f.gw.SetInbox(svc, inbox.NewGuide(nil), func() string { return ApprovalModeExecute })
	f.gw.RegisterInboxTools()
	f.gw.registerEntry(toolEntry{
		tool:     mcp.Tool{Name: "alpha.get_status", InputSchema: mcp.ToolInputSchema{Type: "object", Properties: addMetaProps(map[string]any{})}},
		upstream: "alpha", originalName: "get_status", reasonField: ReasonField,
		handle: func(context.Context, map[string]any) (*mcp.CallToolResult, error) {
			f.calls.Add(1)
			return mcp.NewToolResultText("ran"), nil
		},
	})
	if _, err := f.acc.SetAgentScope(ctx, "ag_full", access.AgentScope{ToolList: access.ToolListFull}, "u_admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.acc.SetAgentScope(ctx, "ag_deny", access.AgentScope{Mode: access.ScopeModeAll,
		DenyTools: []access.ToolRef{{Server: inboxUpstream, Tool: "inbox.status"}}}, "u_admin"); err != nil {
		t.Fatal(err)
	}
	return f
}

func sorted(names []string) []string {
	out := append([]string(nil), names...)
	sort.Strings(out)
	return out
}

// TestSmallToolListIsEveryAgentsDefault: an agent with no scope row lists
// exactly the five tools; switched to the full list it lists more, and
// switched back the five again.
func TestSmallToolListIsEveryAgentsDefault(t *testing.T) {
	f := newToolListFixture(t)
	ctx := WithAgentID(context.Background(), "ag_small")
	if got, want := sorted(mcpToolsList(t, f.gw, ctx)), sorted(SmallToolList); !reflect.DeepEqual(got, want) {
		t.Fatalf("default tools/list = %v, want %v", got, want)
	}
	if _, err := f.acc.SetAgentScope(context.Background(), "ag_small", access.AgentScope{ToolList: access.ToolListFull}, "u_admin"); err != nil {
		t.Fatal(err)
	}
	if got := mcpToolsList(t, f.gw, ctx); len(got) <= len(SmallToolList) || !has(got, "alpha.get_status") {
		t.Fatalf("tools/list after the switch to full: %v", got)
	}
	if _, err := f.acc.SetAgentScope(context.Background(), "ag_small", access.AgentScope{ToolList: access.ToolListSmall}, "u_admin"); err != nil {
		t.Fatal(err)
	}
	if got := mcpToolsList(t, f.gw, ctx); len(got) != len(SmallToolList) {
		t.Fatalf("tools/list after the switch back: %v", got)
	}
}

// TestFullToolListShowsEverythingTheAgentMayUse: the full list is the
// whole catalog the caller may use, as before the small list; callers
// without an agent scope (a local unauthenticated caller, a dashboard
// session) always get it.
func TestFullToolListShowsEverythingTheAgentMayUse(t *testing.T) {
	f := newToolListFixture(t)
	for _, caller := range []string{"ag_full", "", "dashboard:u_admin"} {
		ctx := WithAgentID(context.Background(), caller)
		got, want := sorted(mcpToolsList(t, f.gw, ctx)), catalogNames(f.gw.CatalogFor(ctx))
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%q: tools/list = %v, want the whole catalog %v", caller, got, want)
		}
		for _, n := range append([]string{"alpha.get_status", MetaExecuteTool, "memory.get"}, SmallToolList...) {
			if !has(got, n) {
				t.Errorf("%q: tools/list lacks %s", caller, n)
			}
		}
	}
}

// TestSmallToolListKeepsAccessDenies: the small list only drops tools, so
// one of the five that the agent's own limits deny stays hidden.
func TestSmallToolListKeepsAccessDenies(t *testing.T) {
	f := newToolListFixture(t)
	got := mcpToolsList(t, f.gw, WithAgentID(context.Background(), "ag_deny"))
	if has(got, "inbox.status") || len(got) != len(SmallToolList)-1 {
		t.Fatalf("tools/list of an agent with inbox.status denied: %v", got)
	}
	for _, n := range SmallToolList {
		if n != "inbox.status" && !has(got, n) {
			t.Errorf("tools/list lacks %s: %v", n, got)
		}
	}
}

// TestHiddenToolsStayCallable: a tool the small list hides still runs
// when called directly over MCP, through tools.execute and from code mode.
func TestHiddenToolsStayCallable(t *testing.T) {
	f := newToolListFixture(t)
	ctx := WithAgentID(context.Background(), "ag_small")
	if has(mcpToolsList(t, f.gw, ctx), "alpha.get_status") {
		t.Fatal("alpha.get_status should be hidden from the small list")
	}
	if r := mcpToolsCall(t, f.gw, ctx, "alpha.get_status"); r.rpcErr || r.isError || !strings.Contains(r.text, "ran") {
		t.Fatalf("direct tools/call of a hidden tool: %s", r.raw)
	}
	res, err := f.gw.RouteCall(ctx, "test", MetaExecuteTool, map[string]any{
		ReasonField: testReason, "tool": "alpha.get_status", "arguments": map[string]any{ReasonField: testReason},
	})
	if err != nil || res.IsError || !strings.Contains(textOf(res), "ran") {
		t.Fatalf("tools.execute of a hidden tool: %v %s", err, textOf(res))
	}
	res = route(t, f.gw, ctx, CodeModeExecuteToolCode, map[string]any{"code": "result = alpha.get_status()"})
	if res.IsError || !strings.Contains(textOf(res), "Execution completed successfully") {
		t.Fatalf("code mode call of a hidden tool:\n%s", textOf(res))
	}
	if n := f.calls.Load(); n != 3 {
		t.Fatalf("the hidden tool ran %d times, want 3", n)
	}
}
