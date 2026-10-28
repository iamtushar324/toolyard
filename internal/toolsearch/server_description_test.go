package toolsearch

import "testing"

// TestRenderGivesEachServerDescriptionOnce: the same tool on two servers
// comes back with each server's description on that server's first result,
// at summary and full, and never at detail name.
func TestRenderGivesEachServerDescriptionOnce(t *testing.T) {
	hits := []Hit{
		{Tool: Tool{Name: "bkt3_ds1.t3_whoami", Server: "bkt3_ds1", ServerDescription: "bk t3 on the production server DS1"}, Score: 0.9},
		{Tool: Tool{Name: "bkt3_usm.t3_whoami", Server: "bkt3_usm", ServerDescription: "bk t3 on the USA Mac mini"}, Score: 0.9},
		{Tool: Tool{Name: "bkt3_ds1.t3_inbox", Server: "bkt3_ds1", ServerDescription: "bk t3 on the production server DS1"}, Score: 0.5},
		{Tool: Tool{Name: "Linear.create_issue", Server: "Linear"}, Score: 0.1},
	}
	for _, d := range []Detail{DetailSummary, DetailFull} {
		out := Render(hits, d)
		want := []string{"bk t3 on the production server DS1", "bk t3 on the USA Mac mini", "", ""}
		for i, e := range out {
			if e.ServerDescription != want[i] {
				t.Errorf("detail %s result %d (%s): server_description = %q, want %q", d, i, e.Name, e.ServerDescription, want[i])
			}
		}
	}
	for _, e := range Render(hits, DetailName) {
		if e.ServerDescription != "" {
			t.Errorf("detail name gives server_description %q", e.ServerDescription)
		}
	}
}

// TestDecisionsServersCarryDescription: the ranker's server context gives
// each server's description.
func TestDecisionsServersCarryDescription(t *testing.T) {
	tools := fixture()
	for i := range tools {
		if tools[i].Server == "Slack" {
			tools[i].ServerDescription = "the team chat"
		}
	}
	api := newFakeAPI(t, judge(only("Slack.send_message", 0.95), "Slack", 0.9))
	search(t, New(Options{Semantic: newTestRanker(api, &observed{})}), tools, Query{Text: "tell the team in chat that the deploy finished"})
	reqs := api.requests()
	if len(reqs) == 0 {
		t.Fatal("no request")
	}
	found := false
	for _, s := range reqs[0].State.Servers {
		switch {
		case s.Name == "Slack":
			found = true
			if s.Description != "the team chat" {
				t.Errorf("Slack description = %q", s.Description)
			}
		case s.Description != "":
			t.Errorf("%s description = %q, want none", s.Name, s.Description)
		}
	}
	if !found {
		t.Fatalf("no Slack server in %+v", reqs[0].State.Servers)
	}
}
