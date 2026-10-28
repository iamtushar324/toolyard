package gateway

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/policy"
	"github.com/tusharbhardwaj/toolyard/internal/toolsearch"
)

// searchBody runs tools.search as the member and returns its structured
// answer.
func searchBody(t *testing.T, f *accessFixture, args map[string]any) map[string]any {
	t.Helper()
	res := f.call(t, f.member, MetaSearchTool, args)
	if res.IsError {
		t.Fatalf("tools.search %v: %s", args, textOf(res))
	}
	return structured(t, res)
}

// TestToolsSearchReportsRunAndRanking (TEC-1807): every result at summary
// or full detail says what a call would get now (the decision a call with
// no arguments gets), and the answer says what ranked it and why it fell
// back.
func TestToolsSearchReportsRunAndRanking(t *testing.T) {
	f := newAccessFixture(t, nil)
	if _, err := f.gw.policy.Set(context.Background(), policy.ScopeTool, "alpha.get_status", "deny", "blocked for the test", false); err != nil {
		t.Fatal(err)
	}
	for _, detail := range []string{"summary", "full", ""} {
		body := searchBody(t, f, map[string]any{"query": "", "detail": detail, "limit": 200})
		if body["ranked_by"] != toolsearch.RankedName || body["fallback"] != "" {
			t.Fatalf("%q: ranked_by %v fallback %v", detail, body["ranked_by"], body["fallback"])
		}
		runs := map[string]any{}
		for _, r := range body["results"].([]any) {
			m := r.(map[string]any)
			runs[m["name"].(string)] = m["run"]
		}
		for tool, want := range map[string]string{"alpha.get_status": RunBlocked, "alpha.run": RunNeedsApproval, "inbox.guide": RunNow} {
			if runs[tool] != want {
				t.Errorf("%q: %s run %v, want %s", detail, tool, runs[tool], want)
			}
		}
		for tool, run := range runs {
			if run != RunNow && run != RunNeedsApproval && run != RunBlocked {
				t.Errorf("%q: %s has run %v", detail, tool, run)
			}
		}
	}
	// The name detail stays the name alone.
	for _, r := range searchBody(t, f, map[string]any{"query": "alpha", "detail": "name"})["results"].([]any) {
		if len(r.(map[string]any)) != 1 {
			t.Fatalf("name detail: %v", r)
		}
	}
	// auto without a ranker is keyword and says the ranker is off; an
	// explicit keyword search does not fall back; regex says regex.
	if body := searchBody(t, f, map[string]any{"query": "status"}); body["mode"] != "keyword" || body["ranked_by"] != "keyword" || body["fallback"] != toolsearch.FallbackOff {
		t.Fatalf("auto: %v %v %v", body["mode"], body["ranked_by"], body["fallback"])
	}
	if body := searchBody(t, f, map[string]any{"query": "status", "mode": "keyword"}); body["ranked_by"] != "keyword" || body["fallback"] != "" {
		t.Fatalf("keyword: %v %v", body["ranked_by"], body["fallback"])
	}
	if body := searchBody(t, f, map[string]any{"query": `^alpha\.`, "mode": "regex"}); body["ranked_by"] != "regex" || body["fallback"] != "" {
		t.Fatalf("regex: %v %v", body["ranked_by"], body["fallback"])
	}
	// The description tells the agent how to read run and how to ask.
	f.gw.mu.RLock()
	desc := f.gw.tools[MetaSearchTool].tool.Description
	f.gw.mu.RUnlock()
	for _, want := range []string{"Jev", "needs_approval", "inbox.request", "ONE request", "ranked_by", "fallback"} {
		if !strings.Contains(desc, want) {
			t.Errorf("the tools.search description does not say %q", want)
		}
	}
}

// alphaFirst is a semantic provider that ranks the alpha tools alone.
type alphaFirst struct{ err error }

func (a alphaFirst) Rank(_ context.Context, tools []toolsearch.Tool, _ string) ([]toolsearch.Hit, error) {
	if a.err != nil {
		return nil, a.err
	}
	var hits []toolsearch.Hit
	for _, t := range tools {
		if strings.HasPrefix(t.Name, "alpha.") {
			hits = append(hits, toolsearch.Hit{Tool: t, Score: 0.9})
		}
	}
	return hits, nil
}

// TestToolsSearchUsesTheRanker: with a ranker the default mode ranks by
// it; a failing ranker falls back to keyword and says so.
func TestToolsSearchUsesTheRanker(t *testing.T) {
	f := newAccessFixture(t, nil)
	f.gw.SetToolSearch(toolsearch.New(toolsearch.Options{Semantic: alphaFirst{}}))
	body := searchBody(t, f, map[string]any{"query": "check the state of alpha"})
	results := body["results"].([]any)
	if body["mode"] != "semantic" || body["ranked_by"] != toolsearch.RankedJev || body["fallback"] != "" || len(results) != 2 {
		t.Fatalf("ranker: %v %v %v %v", body["mode"], body["ranked_by"], body["fallback"], results)
	}
	for _, r := range results {
		if m := r.(map[string]any); m["run"] == nil || m["score"] != 0.9 {
			t.Fatalf("ranked result %v", m)
		}
	}
	f.gw.SetToolSearch(toolsearch.New(toolsearch.Options{Semantic: alphaFirst{err: errors.New("provider down")}}))
	body = searchBody(t, f, map[string]any{"query": "status", "mode": "semantic"})
	if body["mode"] != "keyword" || body["ranked_by"] != "keyword" || body["fallback"] != toolsearch.FallbackError || len(body["results"].([]any)) == 0 {
		t.Fatalf("fallback: %v %v %v %v", body["mode"], body["ranked_by"], body["fallback"], body["results"])
	}
}
