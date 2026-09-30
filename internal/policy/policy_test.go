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
		// A declared intent only escalates: "read" never loosens.
		{"declared read on a destructive name", Request{IntentCategory: "read", ToolName: "github.delete_repo"}, ActionApprove},
		{"declared read on a write name", Request{IntentCategory: "read", ToolName: "github.create_issue"}, ActionApprove},
		{"declared read on a read name", Request{IntentCategory: "read", ToolName: "github.get_repo"}, ActionAllow},
		{"unknown intent on a write name", Request{IntentCategory: "harmless", ToolName: "github.create_issue"}, ActionApprove},
		{"unknown intent on a read name", Request{IntentCategory: "harmless", ToolName: "github.list_issues"}, ActionAllow},
		{"declared write on a read name", Request{IntentCategory: "write", ToolName: "github.get_repo"}, ActionApprove},
		{"explicit write", Request{IntentCategory: "write", ToolName: "memory.get"}, ActionApprove},
		{"explicit destructive", Request{IntentCategory: "destructive", ToolName: "memory.get"}, ActionApprove},
		{"explicit external_communication", Request{IntentCategory: "external_communication", ToolName: "memory.get"}, ActionApprove},
		{"explicit financial", Request{IntentCategory: "financial", ToolName: "memory.get"}, ActionApprove},
		{"explicit privileged_admin", Request{IntentCategory: "privileged_admin", ToolName: "memory.get"}, ActionApprove},
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

// TestDeclaredReadIntentIsIgnored: a caller that declares
// _intent_category "read" (or any value that isn't an escalation) gets
// exactly the decision it would get with no intent at all, so it can't
// talk its way past approval on a write.
func TestDeclaredReadIntentIsIgnored(t *testing.T) {
	e := New(nil)
	for _, name := range []string{"github.create_issue", "github.delete_repo", "memory.set", "github.get_repo", "read_file", "t.run"} {
		base := e.Eval(Request{UpstreamName: "github", ToolName: name})
		for _, intent := range []string{"read", "READ", "harmless", ""} {
			got := e.Eval(Request{UpstreamName: "github", ToolName: name, IntentCategory: intent})
			if got != base {
				t.Errorf("%s with intent %q = %+v, want the no-intent decision %+v", name, intent, got, base)
			}
		}
	}
	d := e.Eval(Request{UpstreamName: "github", ToolName: "github.create_issue", IntentCategory: "read"})
	if d.Action != ActionApprove || d.RuleID != "v0.1-default-write" {
		t.Fatalf("write name declared read = %+v, want the default approve", d)
	}
	d = e.Eval(Request{UpstreamName: "github", ToolName: "github.get_repo", IntentCategory: "write"})
	if d.Action != ActionApprove || d.RuleID != "v0.1-category" {
		t.Fatalf("read name declared write = %+v, want the category approve", d)
	}
}

// An explicit allow rule is the operator's word and still beats a declared
// escalation; a declared "read" still can't get past an explicit ask.
func TestExplicitRulesAndDeclaredIntent(t *testing.T) {
	e := newTestEngine(t)
	ctx := context.Background()
	if _, err := e.Set(ctx, ScopeTool, "github.create_issue", "ask", "", false); err != nil {
		t.Fatal(err)
	}
	if d := e.Eval(Request{UpstreamName: "github", ToolName: "github.create_issue", IntentCategory: "read"}); d.Action != ActionApprove || !d.RequireHuman {
		t.Fatalf("ask rule with declared read = %+v", d)
	}
	if _, err := e.Set(ctx, ScopeTool, "github.add_label", "allow", "", false); err != nil {
		t.Fatal(err)
	}
	if d := e.Eval(Request{UpstreamName: "github", ToolName: "github.add_label", IntentCategory: "write"}); d.Action != ActionAllow {
		t.Fatalf("allow rule with declared write = %+v", d)
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
