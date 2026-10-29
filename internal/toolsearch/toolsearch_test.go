package toolsearch

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// fixture is a catalog of 60+ tools shaped like production's: several
// servers whose tool names share words (issue, file, campaign), so a
// ranking has to weigh name, description and parameters to put the right
// one first.
func fixture() []Tool {
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	obj := func(required []string, props map[string]any) map[string]any {
		return map[string]any{"type": "object", "properties": props, "required": required}
	}
	tool := func(server, name, desc string, schema map[string]any) Tool {
		if schema == nil {
			schema = obj(nil, map[string]any{})
		}
		return Tool{Name: server + "." + name, Server: server, Description: desc, InputSchema: schema, CodeName: server + "." + strings.ReplaceAll(name, "-", "_")}
	}
	return []Tool{
		tool("Linear", "create_issue", "Create a new issue in a Linear team.", obj([]string{"title", "teamId"}, map[string]any{
			"title": str("Issue title"), "teamId": str("Team that owns it"), "description": str("Markdown body"),
			"priority": map[string]any{"type": "integer", "enum": []any{0, 1, 2, 3, 4}}})),
		tool("Linear", "get_issue", "Fetch one Linear issue by its key or id.", obj([]string{"id"}, map[string]any{"id": str("TEC-123 or the uuid")})),
		tool("Linear", "list_issues", "List Linear issues, filtered by team, state or assignee.", obj(nil, map[string]any{
			"team": str(""), "state": str("Workflow state name"), "assignee": str("")})),
		tool("Linear", "update_issue", "Change the title, state, assignee or priority of a Linear issue.", obj([]string{"id"}, map[string]any{"id": str(""), "state": str("")})),
		tool("Linear", "create_comment", "Add a comment to a Linear issue.", obj([]string{"issueId", "body"}, map[string]any{
			"issueId": str("The issue to comment on"), "body": str("Markdown text")})),
		tool("Linear", "list_comments", "List the comments of a Linear issue.", obj([]string{"issueId"}, map[string]any{"issueId": str("")})),
		tool("Linear", "list_projects", "List Linear projects.", nil),
		tool("Linear", "get_project", "Fetch one Linear project.", obj([]string{"id"}, map[string]any{"id": str("")})),
		tool("Linear", "list_teams", "List the teams of the Linear workspace.", nil),
		tool("Linear", "list_users", "List the members of the Linear workspace.", nil),
		tool("Linear", "list_issue_labels", "List the labels Linear issues can carry.", nil),
		tool("Linear", "list_cycles", "List the cycles (sprints) of a Linear team.", obj([]string{"teamId"}, map[string]any{"teamId": str("")})),
		tool("Linear", "get_document", "Fetch a Linear document.", obj([]string{"id"}, map[string]any{"id": str("")})),
		tool("GitHub", "create_issue", "Create a new issue in a GitHub repository.", obj([]string{"owner", "repo", "title"}, map[string]any{
			"owner": str("Repository owner"), "repo": str("Repository name"), "title": str(""), "body": str("")})),
		tool("GitHub", "get_issue", "Get one issue of a GitHub repository.", obj([]string{"owner", "repo", "number"}, map[string]any{
			"owner": str(""), "repo": str(""), "number": map[string]any{"type": "integer"}})),
		tool("GitHub", "list_issues", "List the issues of a GitHub repository.", obj([]string{"owner", "repo"}, map[string]any{"owner": str(""), "repo": str("")})),
		tool("GitHub", "create_pull_request", "Open a pull request from a branch.", obj([]string{"owner", "repo", "head", "base"}, map[string]any{
			"owner": str(""), "repo": str(""), "head": str("Branch with the changes"), "base": str("Branch to merge into"), "title": str("")})),
		tool("GitHub", "get_pull_request", "Get one pull request.", obj([]string{"number"}, map[string]any{"number": map[string]any{"type": "integer"}})),
		tool("GitHub", "list_pull_requests", "List the pull requests of a repository.", nil),
		tool("GitHub", "merge_pull_request", "Merge a pull request.", obj([]string{"number"}, map[string]any{
			"number": map[string]any{"type": "integer"}, "merge_method": map[string]any{"type": "string", "enum": []any{"merge", "squash", "rebase"}}})),
		tool("GitHub", "create_review_comment", "Comment on a line of a pull request diff.", nil),
		tool("GitHub", "search_code", "Search code across GitHub repositories.", obj([]string{"q"}, map[string]any{"q": str("GitHub code search query")})),
		tool("GitHub", "search_repositories", "Find GitHub repositories by name or topic.", obj([]string{"q"}, map[string]any{"q": str("")})),
		tool("GitHub", "get_file_contents", "Read a file or directory from a repository.", obj([]string{"path"}, map[string]any{"path": str("")})),
		tool("GitHub", "create_or_update_file", "Write one file to a branch as a commit.", nil),
		tool("GitHub", "list_commits", "List the commits of a branch.", nil),
		tool("GitHub", "create_branch", "Create a branch from another one.", nil),
		tool("Slack", "send_message", "Post a message to a Slack channel or person.", obj([]string{"channel", "text"}, map[string]any{
			"channel": str("Channel name or id"), "text": str("Message text")})),
		tool("Slack", "list_channels", "List the Slack channels you can see.", nil),
		tool("Slack", "read_channel", "Read the latest messages of a Slack channel.", obj([]string{"channel"}, map[string]any{"channel": str("")})),
		tool("Slack", "search_messages", "Search Slack messages by words.", obj([]string{"query"}, map[string]any{"query": str("")})),
		tool("Slack", "add_reaction", "React to a Slack message with an emoji.", nil),
		tool("GoogleDrive", "search_files", "Search Google Drive files by name or content.", obj([]string{"query"}, map[string]any{"query": str("")})),
		tool("GoogleDrive", "read_file_content", "Read the text content of a Google Drive file.", obj([]string{"fileId"}, map[string]any{"fileId": str("Drive file id")})),
		tool("GoogleDrive", "create_file", "Create a file in Google Drive.", nil),
		tool("GoogleDrive", "copy_file", "Copy a Google Drive file.", nil),
		tool("GoogleDrive", "get_file_permissions", "List who can open a Google Drive file.", nil),
		tool("GoogleSheets", "get_values", "Read cell values from a Google Sheets range.", obj([]string{"spreadsheetId", "range"}, map[string]any{
			"spreadsheetId": str(""), "range": str("A1 notation")})),
		tool("GoogleSheets", "update_values", "Write cell values to a Google Sheets range.", nil),
		tool("BkCoreServices", "get_client", "Fetch one client by id.", obj([]string{"clientId"}, map[string]any{"clientId": str("e.g. vai-us")})),
		tool("BkCoreServices", "get-all-clients", "List every client with its marketplace and status.", nil),
		tool("BkCoreServices", "onboard-client", "Onboard a new client account and schedule its first data pulls.", nil),
		tool("BkCoreServices", "trigger-extraction", "Start a data extraction for a client and report type.", nil),
		tool("BkCoreServices", "check-extraction-status", "Report the status of a running data extraction.", obj([]string{"jobId"}, map[string]any{"jobId": str("Extraction job id")})),
		tool("BkCoreServices", "run-readonly-query", "Run a read-only SQL query against the analytics warehouse.", obj([]string{"sql"}, map[string]any{"sql": str("SELECT statement")})),
		tool("BkCoreServices", "list-scheduled-extractions", "List the extractions scheduled for a client.", nil),
		tool("HeyReach", "add_leads_to_campaign", "Add LinkedIn leads to an outreach campaign.", obj([]string{"campaignId", "leads"}, map[string]any{
			"campaignId": map[string]any{"type": "integer"}, "leads": map[string]any{"type": "array", "items": map[string]any{"type": "object",
				"properties": map[string]any{"profileUrl": str("LinkedIn profile URL")}}}})),
		tool("HeyReach", "get_all_campaigns", "List every outreach campaign.", nil),
		tool("HeyReach", "pause_campaign", "Pause a running outreach campaign.", nil),
		tool("HeyReach", "resume_campaign", "Resume a paused outreach campaign.", nil),
		tool("HeyReach", "send_message", "Send a LinkedIn message to a lead.", nil),
		tool("SoldScope", "rank_tracker_list_groups", "List rank tracker groups of Amazon products.", nil),
		tool("SoldScope", "keyword_research_search_by_keyword", "Research Amazon search volume for a keyword.", nil),
		tool("inbox", "request", "Ask your owner to approve restricted tool calls, with evidence.", obj([]string{"task", "tools"}, map[string]any{
			"task": map[string]any{"type": "object"}, "tools": map[string]any{"type": "array"}})),
		tool("inbox", "status", "Read the decision and grants of your Inbox requests.", nil),
		tool("memory", "get", "Read a value from shared memory by key.", obj([]string{"key"}, map[string]any{"key": str("")})),
		tool("memory", "set", "Store a value in shared memory under a key.", obj([]string{"key", "value"}, map[string]any{"key": str(""), "value": str("")})),
		tool("memory", "delete", "Delete a key from shared memory.", nil),
		tool("policies", "explain", "Explain why a tool is allowed, held or denied for you.", obj([]string{"tool"}, map[string]any{"tool": str("Catalog name")})),
		tool("audit", "mine", "List your own recent audit events.", nil),
		tool("session", "start", "Start an agent session to group your calls.", nil),
		tool("events", "publish", "Publish an event to the shared event stream.", nil),
	}
}

func search(t *testing.T, e *Engine, tools []Tool, q Query) *Result {
	t.Helper()
	res, err := e.Search(context.Background(), tools, q)
	if err != nil {
		t.Fatalf("search %+v: %v", q, err)
	}
	return res
}

func names(hits []Hit) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.Tool.Name
	}
	return out
}

func TestKeywordRanksTheExpectedToolFirst(t *testing.T) {
	tools := fixture()
	if len(tools) < 50 {
		t.Fatalf("the fixture has %d tools, want 50 or more", len(tools))
	}
	e := New(Options{})
	cases := map[string]string{
		"create a linear issue":            "Linear.create_issue",
		"create a github issue":            "GitHub.create_issue",
		"open a pull request":              "GitHub.create_pull_request",
		"merge pull request squash":        "GitHub.merge_pull_request",
		"send a slack message to channel":  "Slack.send_message",
		"read google drive file content":   "GoogleDrive.read_file_content",
		"get_client":                       "BkCoreServices.get_client",
		"BkCoreServices.get_client":        "BkCoreServices.get_client",
		"getClient":                        "BkCoreServices.get_client",
		"extraction status":                "BkCoreServices.check-extraction-status",
		"add leads to linkedin campaign":   "HeyReach.add_leads_to_campaign",
		"sql query warehouse":              "BkCoreServices.run-readonly-query",
		"list comments of a linear issue":  "Linear.list_comments",
		"write cell values to a sheet":     "GoogleSheets.update_values",
		"why is this tool held":            "policies.explain",
		"profile url":                      "HeyReach.add_leads_to_campaign",
		"approve restricted calls":         "inbox.request",
		"listing issues of a GitHub repos": "GitHub.list_issues",
	}
	for query, want := range cases {
		res := search(t, e, tools, Query{Text: query, Limit: 5})
		if len(res.Hits) == 0 || res.Hits[0].Tool.Name != want {
			t.Errorf("%q: got %v, want %s first", query, names(res.Hits), want)
		}
		if res.Mode != ModeKeyword {
			t.Errorf("%q: mode %s", query, res.Mode)
		}
	}
	// Scores fall from first to last.
	res := search(t, e, tools, Query{Text: "issue"})
	for i := 1; i < len(res.Hits); i++ {
		if res.Hits[i].Score > res.Hits[i-1].Score {
			t.Fatalf("hits out of order: %v", res.Hits)
		}
	}
	if res.Matched < 8 {
		t.Fatalf("\"issue\" matched %d tools", res.Matched)
	}
	// Nothing matches a word no tool has.
	if res := search(t, e, tools, Query{Text: "zebra"}); res.Matched != 0 || len(res.Hits) != 0 {
		t.Fatalf("zebra: %v", names(res.Hits))
	}
}

func TestEmptyQueryListsEveryToolByName(t *testing.T) {
	tools := fixture()
	e := New(Options{})
	for _, mode := range []Mode{ModeKeyword, ModeRegex, ModeAuto} {
		res := search(t, e, tools, Query{Mode: mode, Limit: MaxLimit})
		if res.Matched != len(tools) || len(res.Hits) != len(tools) {
			t.Fatalf("%s: %d of %d", mode, len(res.Hits), len(tools))
		}
		got := names(res.Hits)
		for i := 1; i < len(got); i++ {
			if got[i] < got[i-1] {
				t.Fatalf("%s: not sorted by name: %v", mode, got)
			}
		}
	}
}

func TestLimitAndServerFilter(t *testing.T) {
	tools := fixture()
	e := New(Options{})
	res := search(t, e, tools, Query{Text: "issue", Limit: 3})
	if len(res.Hits) != 3 || res.Matched <= 3 {
		t.Fatalf("limit 3: %d hits of %d", len(res.Hits), res.Matched)
	}
	res = search(t, e, tools, Query{Text: "issue", Server: "GitHub"})
	for _, h := range res.Hits {
		if h.Tool.Server != "GitHub" {
			t.Fatalf("server filter let %s through", h.Tool.Name)
		}
	}
	if len(res.Hits) == 0 || res.Hits[0].Tool.Name != "GitHub.get_issue" && res.Hits[0].Tool.Name != "GitHub.list_issues" && res.Hits[0].Tool.Name != "GitHub.create_issue" {
		t.Fatalf("GitHub issue tools: %v", names(res.Hits))
	}
	if res := search(t, e, tools, Query{Limit: 1000}); len(res.Hits) != len(tools) {
		t.Fatalf("a limit over the cap is the cap: %d", len(res.Hits))
	}
	if _, err := e.Search(context.Background(), tools, Query{Text: "x", Limit: -1}); err == nil {
		t.Fatal("a negative limit should be refused")
	}
}

func TestRegexSearch(t *testing.T) {
	tools := fixture()
	e := New(Options{})
	res := search(t, e, tools, Query{Mode: ModeRegex, Text: `^linear\..*issue`})
	got := names(res.Hits)
	want := []string{"Linear.create_issue", "Linear.get_issue", "Linear.list_issue_labels", "Linear.list_issues", "Linear.update_issue"}
	// Each matched the name field only (plus whatever else), so all score
	// at least the name weight; compare the set.
	if len(got) < len(want) {
		t.Fatalf("regex names: %v", got)
	}
	for _, w := range want {
		found := false
		for _, g := range got[:len(want)] {
			found = found || g == w
		}
		if !found {
			t.Errorf("regex: %s missing from the top %d: %v", w, len(want), got)
		}
	}
	// Case-insensitive by default; (?-i) turns that off.
	if res := search(t, e, tools, Query{Mode: ModeRegex, Text: `LINKEDIN`}); len(res.Hits) == 0 {
		t.Fatal("regex should be case-insensitive by default")
	}
	if res := search(t, e, tools, Query{Mode: ModeRegex, Text: `(?-i)LINKEDIN`}); len(res.Hits) != 0 {
		t.Fatalf("(?-i) should match case: %v", names(res.Hits))
	}
	// Parameter names and descriptions count.
	if res := search(t, e, tools, Query{Mode: ModeRegex, Text: `profileUrl`}); len(res.Hits) != 1 || res.Hits[0].Tool.Name != "HeyReach.add_leads_to_campaign" {
		t.Fatalf("regex over nested parameter names: %v", names(res.Hits))
	}
	// A name match outranks a description match.
	res = search(t, e, tools, Query{Mode: ModeRegex, Text: `campaign`})
	if len(res.Hits) < 2 || !strings.Contains(strings.ToLower(res.Hits[0].Tool.Name), "campaign") {
		t.Fatalf("campaign: %v", names(res.Hits))
	}
	// Too long, and invalid, patterns are refused with a clear error.
	_, err := e.Search(context.Background(), tools, Query{Mode: ModeRegex, Text: strings.Repeat("a", MaxPatternLen+1)})
	var qe *QueryError
	if !errors.As(err, &qe) || !strings.Contains(err.Error(), "at most 200") {
		t.Fatalf("a 201-character pattern: %v", err)
	}
	if _, err := e.Search(context.Background(), tools, Query{Mode: ModeRegex, Text: strings.Repeat("a", MaxPatternLen)}); err != nil {
		t.Fatalf("a 200-character pattern: %v", err)
	}
	if _, err := e.Search(context.Background(), tools, Query{Mode: ModeRegex, Text: `(unclosed`}); !errors.As(err, &qe) || !strings.Contains(err.Error(), "RE2") {
		t.Fatalf("an invalid pattern: %v", err)
	}
}

type fakeSemantic struct{ seen string }

func (f *fakeSemantic) Rank(_ context.Context, tools []Tool, query string) ([]Hit, error) {
	f.seen = query
	var hits []Hit
	for _, t := range tools {
		if strings.HasPrefix(t.Name, "Slack.") {
			hits = append(hits, Hit{Tool: t, Score: 0.9})
		}
	}
	return hits, nil
}

func TestSemanticNeedsAProvider(t *testing.T) {
	tools := fixture()
	off := New(Options{})
	if off.SemanticEnabled() {
		t.Fatal("no provider: semantic must be off")
	}
	if _, err := off.Search(context.Background(), tools, Query{Mode: ModeSemantic, Text: "post to chat"}); !errors.Is(err, ErrSemanticOff) {
		t.Fatalf("semantic without a provider: %v", err)
	}
	// auto without a provider is keyword, and says the provider is off.
	if res := search(t, off, tools, Query{Mode: ModeAuto, Text: "create a linear issue"}); res.Mode != ModeKeyword || res.Hits[0].Tool.Name != "Linear.create_issue" ||
		res.RankedBy != RankedKeyword || res.Fallback != FallbackOff {
		t.Fatalf("auto: %s %s %s %v", res.Mode, res.RankedBy, res.Fallback, names(res.Hits))
	}
	f := &fakeSemantic{}
	on := New(Options{Semantic: f})
	res := search(t, on, tools, Query{Mode: ModeSemantic, Text: "post to chat", Limit: 2})
	if !on.SemanticEnabled() || f.seen != "post to chat" || res.Mode != ModeSemantic || len(res.Hits) != 2 || res.Matched != 5 ||
		res.RankedBy != RankedJev || res.Fallback != "" {
		t.Fatalf("semantic with a provider: %+v", res)
	}
	// auto (and the default, "") with a provider is semantic.
	for _, mode := range []Mode{ModeAuto, ""} {
		if res := search(t, on, tools, Query{Mode: mode, Text: "post to chat"}); res.Mode != ModeSemantic || res.RankedBy != RankedJev {
			t.Fatalf("auto %q with a provider: %s %s", mode, res.Mode, res.RankedBy)
		}
	}
}

type failingSemantic struct{ err error }

func (f failingSemantic) Rank(context.Context, []Tool, string) ([]Hit, error) { return nil, f.err }

// TestModesRankAndFallBack (TEC-1807): auto is the default; each search
// says what ranked it and, for auto and semantic, why it fell back to
// keyword; keyword and regex are unchanged.
func TestModesRankAndFallBack(t *testing.T) {
	tools := fixture()
	off := New(Options{})
	cases := []struct {
		engine   *Engine
		q        Query
		mode     Mode
		rankedBy string
		fallback string
	}{
		{off, Query{Text: "create a linear issue"}, ModeKeyword, RankedKeyword, FallbackOff},
		{off, Query{Text: "create a linear issue", Mode: ModeKeyword}, ModeKeyword, RankedKeyword, ""},
		{off, Query{Text: `^linear\.`, Mode: ModeRegex}, ModeRegex, RankedRegex, ""},
		{off, Query{}, ModeKeyword, RankedName, ""},
		{New(Options{Semantic: failingSemantic{errors.New("boom")}}), Query{Text: "create a linear issue"}, ModeKeyword, RankedKeyword, FallbackError},
		{New(Options{Semantic: failingSemantic{ErrNoKey}}), Query{Text: "create a linear issue", Mode: ModeSemantic}, ModeKeyword, RankedKeyword, FallbackNoKey},
		{New(Options{Semantic: failingSemantic{context.DeadlineExceeded}}), Query{Text: "create a linear issue"}, ModeKeyword, RankedKeyword, FallbackTimeout},
		{New(Options{Semantic: failingSemantic{errors.New("boom")}}), Query{Text: "create a linear issue", Mode: ModeKeyword}, ModeKeyword, RankedKeyword, ""},
	}
	for i, c := range cases {
		res := search(t, c.engine, tools, c.q)
		if res.Mode != c.mode || res.RankedBy != c.rankedBy || res.Fallback != c.fallback {
			t.Errorf("case %d %+v: mode %s ranked_by %s fallback %q, want %s %s %q", i, c.q, res.Mode, res.RankedBy, res.Fallback, c.mode, c.rankedBy, c.fallback)
		}
		if c.rankedBy == RankedKeyword && (len(res.Hits) == 0 || res.Hits[0].Tool.Name != "Linear.create_issue") {
			t.Errorf("case %d: a keyword ranking or fallback put %v first", i, names(res.Hits))
		}
	}
	// No tools to rank: the provider is not asked.
	f := &fakeSemantic{}
	res := search(t, New(Options{Semantic: f}), tools, Query{Text: "post to chat", Server: "nowhere"})
	if res.Fallback != FallbackEmpty || f.seen != "" || len(res.Hits) != 0 {
		t.Fatalf("empty catalog: %+v, provider saw %q", res, f.seen)
	}
}

// TestBusyCallerFallsBackToKeyword (TEC-1807): a caller whose search slots
// are all taken gets a keyword ranking at once (fallback busy), and the
// provider is not asked, so one script cannot start many paid searches.
func TestBusyCallerFallsBackToKeyword(t *testing.T) {
	f := &fakeSemantic{}
	e := New(Options{Semantic: f})
	var releases []func()
	for i := 0; i < MaxSearchesPerCaller; i++ {
		release, ok := e.tryEnter("ag_busy")
		if !ok {
			t.Fatalf("slot %d refused", i)
		}
		releases = append(releases, release)
	}
	res := search(t, e, fixture(), Query{Text: "create a linear issue", Caller: "ag_busy"})
	if res.Fallback != FallbackBusy || res.RankedBy != RankedKeyword || f.seen != "" {
		t.Fatalf("busy caller: fallback %q ranked_by %s, provider saw %q", res.Fallback, res.RankedBy, f.seen)
	}
	if res := search(t, e, fixture(), Query{Text: "create a linear issue", Caller: "ag_other"}); res.RankedBy != RankedJev {
		t.Fatalf("another caller: ranked_by %s fallback %q", res.RankedBy, res.Fallback)
	}
	for _, release := range releases {
		release()
	}
	if res := search(t, e, fixture(), Query{Text: "create a linear issue", Caller: "ag_busy"}); res.RankedBy != RankedJev {
		t.Fatalf("after the slots free: ranked_by %s fallback %q", res.RankedBy, res.Fallback)
	}
	e.mu.Lock()
	left := len(e.running)
	e.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d callers still hold slots", left)
	}
}

func TestParseModeAndDetail(t *testing.T) {
	for in, want := range map[string]Mode{"": ModeAuto, "keyword": ModeKeyword, "REGEX": ModeRegex, " semantic ": ModeSemantic, "auto": ModeAuto} {
		if got, err := ParseMode(in); err != nil || got != want {
			t.Errorf("ParseMode(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := ParseMode("fuzzy"); err == nil {
		t.Error("ParseMode should refuse fuzzy")
	}
	for in, want := range map[string]Detail{"": DetailSummary, "name": DetailName, "Summary": DetailSummary, "full": DetailFull} {
		if got, err := ParseDetail(in); err != nil || got != want {
			t.Errorf("ParseDetail(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := ParseDetail("everything"); err == nil {
		t.Error("ParseDetail should refuse everything")
	}
}

func TestRenderDetail(t *testing.T) {
	long := strings.Repeat("word ", 100) + "\n\nSecond paragraph."
	hit := Hit{Score: 1.234567, Tool: Tool{Name: "A.b", Server: "A", Description: long, CodeName: "A.b",
		InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"}}}
	if got := Render([]Hit{hit}, DetailName); !reflect.DeepEqual(got, []Entry{{Name: "A.b"}}) {
		t.Fatalf("name: %+v", got)
	}
	sum := Render([]Hit{hit}, DetailSummary)[0]
	if sum.Upstream != "A" || sum.Code != "A.b" || sum.Score != 1.2346 || sum.InputSchema != nil || sum.OutputSchema != nil ||
		len(sum.Description) > summaryLen+4 || !strings.HasSuffix(sum.Description, "…") || strings.Contains(sum.Description, "Second") {
		t.Fatalf("summary: %+v", sum)
	}
	full := Render([]Hit{hit}, DetailFull)[0]
	if full.Description != strings.TrimSpace(long) || full.InputSchema == nil || full.OutputSchema == nil {
		t.Fatalf("full: %+v", full)
	}
}

func TestTokens(t *testing.T) {
	cases := map[string][]string{
		"getClientID":               {"get", "client", "id", "getclientid"},
		"HTTPServer":                {"http", "server", "httpserver"},
		"GitHub":                    {"git", "hub", "github"},
		"github":                    {"github"},
		"get-all-clients":           {"get", "all", "client"},
		"BkCoreServices.get_client": {"bk", "core", "servic", "bkcoreservic", "get", "client"},
		"Create the issues":         {"creat", "issu"},
		"creating issue":            {"creat", "issu"},
		"queries listing files":     {"query", "list", "file"},
		"status analysis":           {"status", "analysis"},
	}
	for in, want := range cases {
		if got := tokens(in, false); !reflect.DeepEqual(got, want) {
			t.Errorf("tokens(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestIndexIsCachedPerCatalog(t *testing.T) {
	tools := fixture()
	e := New(Options{})
	a := e.index(tools)
	if b := e.index(fixture()); a != b {
		t.Fatal("the same catalog should reuse its index")
	}
	changed := fixture()
	changed[0].Description = "Something else entirely."
	if c := e.index(changed); c == a {
		t.Fatal("a changed catalog needs a new index")
	}
}

// TestSearchCostIsBounded (review P1-3): a short pattern cannot compile to
// a huge program, counted repetition is capped, and keyword queries have a
// length cap and count only their first terms.
func TestSearchCostIsBounded(t *testing.T) {
	tools := fixture()
	e := New(Options{})
	ctx := context.Background()
	var qe *QueryError
	for pattern, want := range map[string]string{
		`a{101}`:                  "at most {100}",
		`((abcdefghij){100}){10}`: "instructions",
	} {
		// RE2's own parser may call a pattern too large first; either way
		// it is refused before it runs.
		if _, err := e.Search(ctx, tools, Query{Mode: ModeRegex, Text: pattern}); !errors.As(err, &qe) ||
			!(strings.Contains(err.Error(), want) || strings.Contains(err.Error(), "too large")) {
			t.Errorf("pattern %s: %v, want an error naming %q", pattern, err, want)
		}
	}
	// Within the limits a counted repetition is fine.
	if _, err := e.Search(ctx, tools, Query{Mode: ModeRegex, Text: `(abcdefghij){100}`}); err != nil {
		t.Errorf("a repetition of 100: %v", err)
	}
	if _, err := e.Search(ctx, tools, Query{Text: strings.Repeat("issue ", 200)}); !errors.As(err, &qe) || !strings.Contains(err.Error(), "at most 1000") {
		t.Errorf("a 1200-character query: %v", err)
	}
	// Many distinct words: only the first ones count, and the search runs.
	var words []string
	for i := 0; i < 60; i++ {
		words = append(words, fmt.Sprintf("w%d", i))
	}
	if res := search(t, e, tools, Query{Text: "linear issue " + strings.Join(words, " ")}); len(res.Hits) == 0 {
		t.Error("a query with many terms found nothing")
	}
}

// TestIndexCacheKeyCoversOutputSchema (review P2-7): a changed output
// schema makes a new index, so full results never show a stale one.
func TestIndexCacheKeyCoversOutputSchema(t *testing.T) {
	e := New(Options{})
	a := fixture()
	b := fixture()
	b[0].OutputSchema = map[string]any{"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string"}}}
	if e.index(a) == e.index(b) {
		t.Fatal("a changed output schema reused the old index")
	}
	res := search(t, e, b, Query{Text: "create a linear issue", Limit: 1})
	if res.Hits[0].Tool.OutputSchema == nil {
		t.Fatal("the hit carries the old output schema")
	}
}

// TestRegexWorkIsBoundedBeforeItRuns (B2 P2-1): a pattern whose program
// times the catalog's text passes the work budget is refused before any
// matching; each text is matched on its first 4 KiB; a search stops when
// its context ends; one caller runs at most two searches at once.
func TestRegexWorkIsBoundedBeforeItRuns(t *testing.T) {
	e := New(Options{})
	ctx := context.Background()
	long := strings.Repeat("lorem ipsum ", 400) // 4800 bytes, matched on 4 KiB
	big := make([]Tool, 2000)
	for i := range big {
		big[i] = Tool{Name: fmt.Sprintf("srv.tool_%d", i), Server: "srv", Description: long}
	}
	var qe *QueryError
	if _, err := e.Search(ctx, big, Query{Mode: ModeRegex, Text: `(abcdefghij){50}`}); !errors.As(err, &qe) || !strings.Contains(err.Error(), "too complex") {
		t.Fatalf("a complex pattern over 8 MB of text: %v", err)
	}
	small := big[:200]
	if res := search(t, e, small, Query{Mode: ModeRegex, Text: `tool_19\b`}); len(res.Hits) != 1 {
		t.Fatalf("a simple pattern: %v", names(res.Hits))
	}
	capped := []Tool{
		{Name: "srv.late", Server: "srv", Description: strings.Repeat("a", 5000) + " needle"},
		{Name: "srv.early", Server: "srv", Description: "a needle " + strings.Repeat("a", 5000)},
	}
	if res := search(t, e, capped, Query{Mode: ModeRegex, Text: "needle"}); !reflect.DeepEqual(names(res.Hits), []string{"srv.early"}) {
		t.Fatalf("matched past 4 KiB: %v", names(res.Hits))
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := e.Search(cctx, small, Query{Mode: ModeRegex, Text: "tool"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled search: %v", err)
	}

	e.wait = 50 * time.Millisecond
	rel1, err := e.enter(ctx, "ag_a")
	if err != nil {
		t.Fatal(err)
	}
	rel2, err := e.enter(ctx, "ag_a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Search(ctx, small, Query{Mode: ModeRegex, Text: "tool", Caller: "ag_a"}); !errors.As(err, &qe) || !strings.Contains(err.Error(), "already runs 2 searches") {
		t.Fatalf("a third regex search of one caller: %v", err)
	}
	if _, err := e.Search(ctx, small, Query{Mode: ModeRegex, Text: "tool", Caller: "ag_b"}); err != nil {
		t.Fatalf("another caller: %v", err)
	}
	// Keyword searches are not capped.
	if _, err := e.Search(ctx, small, Query{Text: "tool", Caller: "ag_a"}); err != nil {
		t.Fatalf("a keyword search while two regex searches run: %v", err)
	}
	// A third search waits for one to finish.
	e.wait = 5 * time.Second
	done := make(chan error, 1)
	go func() {
		_, err := e.Search(ctx, small, Query{Mode: ModeRegex, Text: "tool", Caller: "ag_a"})
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	rel1()
	if err := <-done; err != nil {
		t.Fatalf("a search that waited for a slot: %v", err)
	}
	rel2()
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.running) != 0 {
		t.Fatalf("running left behind: %v", e.running)
	}
}

// TestDefaultLimitIsTen (TEC-1815): a search without a limit answers at
// most 10 results; an explicit limit still reaches MaxLimit.
func TestDefaultLimitIsTen(t *testing.T) {
	var tools []Tool
	for i := 0; i < 30; i++ {
		tools = append(tools, Tool{Name: fmt.Sprintf("srv.read_item_%02d", i), Server: "srv", Description: "Read one item."})
	}
	e := New(Options{})
	if res := search(t, e, tools, Query{Text: "item", Mode: ModeKeyword}); len(res.Hits) != 10 || res.Matched != 30 {
		t.Fatalf("default limit: %d hits of %d matched, want 10 of 30", len(res.Hits), res.Matched)
	}
	if res := search(t, e, tools, Query{Text: "item", Mode: ModeKeyword, Limit: MaxLimit}); len(res.Hits) != 30 {
		t.Fatalf("limit %d: %d hits, want 30", MaxLimit, len(res.Hits))
	}
}
