package policy

import "testing"

func TestEvalCategories(t *testing.T) {
	e := New()
	cases := []struct {
		name     string
		req      Request
		want     Action
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
