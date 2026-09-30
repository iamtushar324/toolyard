package policy

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// newTestEngine is a DB-backed engine, so explicit rules can be set.
func newTestEngine(t *testing.T) *Engine {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "policy.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return New(db)
}

func TestEvalCategories(t *testing.T) {
	e := New(nil) // heuristic-only: no stored policies (regression = pre-feature behaviour)
	cases := []struct {
		name string
		req  Request
		want Action
	}{
		{"explicit read", Request{IntentCategory: "read", ToolName: "github.delete_repo"}, ActionAllow},
		{"explicit write", Request{IntentCategory: "write", ToolName: "memory.get"}, ActionApprove},
		{"explicit destructive", Request{IntentCategory: "destructive", ToolName: "memory.get"}, ActionApprove},
		{"name get_", Request{ToolName: "github.get_repo"}, ActionAllow},
		{"name list_", Request{ToolName: "github.list_issues"}, ActionAllow},
		{"name dotted get", Request{ToolName: "memory.get"}, ActionAllow},
		{"name dotted list", Request{ToolName: "memory.list"}, ActionAllow},
		{"name dotted set", Request{ToolName: "memory.set"}, ActionApprove},
		{"name create_", Request{ToolName: "github.create_issue"}, ActionApprove},
		{"unknown verb", Request{ToolName: "github.transmogrify"}, ActionApprove},
		{"snake read", Request{ToolName: "read_file"}, ActionAllow},
		{"meta tools.execute", Request{UpstreamName: "tools", ToolName: "execute"}, ActionAllow},
		{"meta tools.search", Request{UpstreamName: "tools", ToolName: "search"}, ActionAllow},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := e.Eval(c.req)
			if got.Action != c.want {
				t.Errorf("got %v, want %v (reason: %s)", got.Action, c.want, got.Reason)
			}
		})
	}
}

// TestExplicitToolRuleBeatsMetaToolShortcut: the tools.* group is allowed
// by default, but an explicit tool-scope rule still decides one of its
// tools, so an operator can deny executeToolCode outright or make it ask.
func TestExplicitToolRuleBeatsMetaToolShortcut(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	req := Request{UpstreamName: "tools", ToolName: "executeToolCode"}
	if d := e.Eval(req); d.Action != ActionAllow || d.RuleID != "v0.1-meta-tool" {
		t.Fatalf("default = %+v, want the meta-tool allow", d)
	}
	if _, err := e.Set(ctx, ScopeTool, "executeToolCode", "deny", "no scripts here", false); err != nil {
		t.Fatal(err)
	}
	if d := e.Eval(req); d.Action != ActionDeny || !strings.Contains(d.Reason, "explicit tool-policy: deny") {
		t.Fatalf("after deny = %+v", d)
	}
	if d := e.Eval(Request{UpstreamName: "tools", ToolName: "listToolFiles"}); d.Action != ActionAllow {
		t.Fatalf("a sibling tool is unaffected: %+v", d)
	}
	if _, err := e.Set(ctx, ScopeTool, "executeToolCode", "ask", "", false); err != nil {
		t.Fatal(err)
	}
	if d := e.Eval(req); d.Action != ActionApprove || !d.RequireHuman {
		t.Fatalf("after ask = %+v", d)
	}
	// An upstream-scope rule on the group itself still yields to the
	// shortcut, as before.
	if _, err := e.Set(ctx, ScopeUpstream, "tools", "deny", "", false); err != nil {
		t.Fatal(err)
	}
	if d := e.Eval(Request{UpstreamName: "tools", ToolName: "search"}); d.Action != ActionAllow {
		t.Fatalf("group-scope rule on tools should not apply: %+v", d)
	}
}
