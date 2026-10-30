package skillhub

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/tusharbhardwaj/toolyard/internal/toolsearch"
)

func TestRecommendTriggersAndTags(t *testing.T) {
	cases := []struct {
		name     string
		meta     map[string]any
		desc     string
		want     []string
		fromMeta bool
	}{
		{"metadata text", map[string]any{"triggers": "a deploy broke; errors jumped\nusers complain"}, "Roll back.",
			[]string{"a deploy broke", "errors jumped", "users complain"}, true},
		{"metadata list", map[string]any{"triggers": []any{" production is down ", 3, "", "an alert fired"}}, "Run it.",
			[]string{"production is down", "an alert fired"}, true},
		{"when-to-use", map[string]any{"when-to-use": "a test fails only sometimes"}, "Fix flaky tests.",
			[]string{"a test fails only sometimes"}, true},
		{"when_to_use", map[string]any{"when_to_use": "slow pages"}, "Tune queries.", []string{"slow pages"}, true},
		{"description cues", nil, "Deploy a service. Use when shipping a release! Not for docs.\nTriggers on a merge to main",
			[]string{"Use when shipping a release!", "Triggers on a merge to main"}, false},
		{"description, when the user", nil, "Take notes. Use this when the user shares a transcript.",
			[]string{"Use this when the user shares a transcript."}, false},
		{"nothing", map[string]any{"tags": "x"}, "Plain description without a cue.", nil, false},
	}
	for _, c := range cases {
		got, fromMeta := triggersFrom(c.meta, c.desc)
		if !reflect.DeepEqual(got, c.want) || fromMeta != c.fromMeta {
			t.Errorf("%s: triggers %q fromMeta %v, want %q %v", c.name, got, fromMeta, c.want, c.fromMeta)
		}
	}
	// At most recommendRankTriggers, each clipped, each once.
	many := strings.Repeat("same; ", 3) + strings.Repeat("a long trigger that keeps going and going; ", 1)
	for i := 0; i < 20; i++ {
		many += "t" + strings.Repeat("x", i) + "; "
	}
	many += strings.Repeat("y", 400)
	got, _ := triggersFrom(map[string]any{"triggers": many}, "")
	if len(got) != recommendRankTriggers || got[0] != "same" {
		t.Fatalf("triggers %q", got)
	}
	long, _ := triggersFrom(map[string]any{"triggers": strings.Repeat("word ", 100)}, "")
	if len(long) != 1 || len(long[0]) > recommendTriggerLen || !strings.HasSuffix(long[0], "…") {
		t.Fatalf("long trigger %q", long)
	}

	if got := tagsFrom(map[string]any{"tags": "Ops, deploy;ops\nweb,,"}); !reflect.DeepEqual(got, []string{"Ops", "deploy", "web"}) {
		t.Errorf("tags from text %q", got)
	}
	if got := tagsFrom(map[string]any{"tags": []any{"pdf", 1, " Documents "}}); !reflect.DeepEqual(got, []string{"pdf", "Documents"}) {
		t.Errorf("tags from a list %q", got)
	}
	if got := tagsFrom(nil); got != nil {
		t.Errorf("no metadata: %q", got)
	}

	// Stored frontmatter (JSON) is read the same way.
	e := &skillEntry{name: "x", description: "Do x. Use when y happens.",
		frontmatter: []byte(`{"name":"x","metadata":{"tags":"A, b","toolyard-runs":"host"}}`)}
	h := hintsOf(e)
	if !reflect.DeepEqual(h.triggers, []string{"Use when y happens."}) || h.fromMeta || !reflect.DeepEqual(h.tags, []string{"A", "b"}) {
		t.Fatalf("hints %+v", h)
	}
	if !h.hasTag(map[string]bool{"a": true}) || h.hasTag(map[string]bool{"c": true}) {
		t.Fatal("hasTag is not case-insensitive")
	}
	// Description triggers are not ranked twice; metadata ones and tags are.
	if txt := h.rankText(e.description); txt != "Do x. Use when y happens. Tags: A, b" {
		t.Fatalf("rank text %q", txt)
	}
	meta := skillHints{triggers: []string{"p", "q"}, fromMeta: true}
	if txt := meta.rankText("D."); txt != "D. When to use: p; q" {
		t.Fatalf("rank text %q", txt)
	}
	if h := hintsOf(&skillEntry{}); h.triggers != nil || h.tags != nil {
		t.Fatalf("empty frontmatter %+v", h)
	}
}

func TestRecommendTextHelpers(t *testing.T) {
	if got := recClip("short  text\n here", 240); got != "short text here" {
		t.Errorf("recClip %q", got)
	}
	for _, s := range []string{strings.Repeat("é", 200), strings.Repeat("word ", 100), strings.Repeat("x", 500)} {
		got := recClip(s, recommendDescLen)
		if len(got) > recommendDescLen || !utf8.ValidString(got) || !strings.HasSuffix(got, "…") {
			t.Errorf("recClip(%.20q…) = %d bytes %q", s, len(got), got)
		}
	}
	if got := sentences("One. Two!\nThree? e.g.x Four"); !reflect.DeepEqual(got, []string{"One.", "Two!", "Three?", "e.g.x Four"}) {
		t.Errorf("sentences %q", got)
	}
	if got := recTokens("Roll back the deploy, the DEPLOY that broke prod-2 a b"); !reflect.DeepEqual(got, []string{"roll", "back", "deploy", "broke", "prod"}) {
		t.Errorf("recTokens %q", got)
	}
	for _, c := range []struct {
		q, f string
		want bool
	}{
		{"deploy", "deploy", true}, {"deploy", "deployment", true}, {"deployment", "deploy", true},
		{"test", "testing", true}, {"api", "apis", false}, {"roll", "rollback", true}, {"back", "rollback", false},
	} {
		if got := recWordMatch(c.q, c.f); got != c.want {
			t.Errorf("recWordMatch(%q, %q) = %v", c.q, c.f, got)
		}
	}
	if got := joinLabels([]string{"name", "triggers", "description"}); got != "name, triggers and description" {
		t.Errorf("joinLabels %q", got)
	}
}

func TestRecommendRationale(t *testing.T) {
	e := &skillEntry{name: "rollback-release", path: "ops/rollback-release", description: "Roll back a bad release."}
	h := skillHints{triggers: []string{"a deploy broke production"}, tags: []string{"ops"}}
	task := recTokens("Roll back the deploy that broke prod")
	want := `matches "roll", "back", "deploy" in its name, triggers and description`
	if got := recRationale(task, e, h, toolsearch.RankedKeyword, 7.2, false); got != want+" (keyword rank)" {
		t.Errorf("keyword rationale %q", got)
	}
	if got := recRationale(task, e, h, toolsearch.RankedJev, 0.912, true); got != want+"; Jev rates it relevant (0.91)" {
		t.Errorf("Jev rationale %q", got)
	}
	if got := recRationale(task, e, h, toolsearch.RankedJev, 0, false); got != want+" (keyword match; Jev did not rate it)" {
		t.Errorf("unjudged rationale %q", got)
	}
	if got := recRationale(recTokens("ops"), e, h, toolsearch.RankedKeyword, 1, false); got != `matches "ops" in its name and tags (keyword rank)` {
		t.Errorf("tags rationale %q", got)
	}
	none := recTokens("convert the guide to spanish")
	if got := recRationale(none, e, h, toolsearch.RankedJev, 0.83, true); got != "Jev rates it relevant to the task (0.83); no shared words" {
		t.Errorf("no shared words %q", got)
	}
	if got := recRationale(none, e, h, toolsearch.RankedJev, 0.42, true); got != "Jev rates it a weak match for the task (0.42); no shared words" {
		t.Errorf("weak %q", got)
	}
	long := &skillEntry{name: strings.Repeat("abcdefgh-", 7) + "x", description: strings.Repeat("abcdefgh ", 40)}
	if got := recRationale(recTokens("abcdefgh abcdefghij abcdefghijk"), long, skillHints{triggers: []string{"abcdefgh"}, tags: []string{"abcdefgh"}}, toolsearch.RankedJev, 0.5, true); len(got) > recommendRationaleLen {
		t.Errorf("rationale is %d bytes", len(got))
	}
}

// Recommend sees exactly what Search sees: the sharing rule and where a
// skill runs, then the source and tag filters.
func TestRecommendVisibilityAndFilters(t *testing.T) {
	svc, _, _ := readerFixture(t)
	ctx := context.Background()
	uris := func(v Viewer, q RecommendQuery) []string {
		t.Helper()
		if q.Mode == "" {
			q.Mode = "keyword"
		}
		res, err := svc.Recommend(ctx, v, q)
		if err != nil {
			t.Fatal(err)
		}
		if res.Skills == nil || res.RankedBy != toolsearch.RankedKeyword {
			t.Fatalf("result %+v", res)
		}
		var out []string
		for _, s := range res.Skills {
			out = append(out, s.URI)
		}
		return out
	}
	if got := uris(memberViewer, RecommendQuery{Task: "deploy the service"}); len(got) != 0 {
		t.Fatalf("a host skill off the host: %v", got)
	}
	if got := uris(memberViewer, RecommendQuery{Task: "deploy the service", IncludeHost: true}); strings.Join(got, ",") != "skill://skills/deploy/SKILL.md" {
		t.Fatalf("member with host skills: %v", got)
	}
	teamAgent := Viewer{UserID: "usr_team", AgentID: "agt_team"}
	got := uris(teamAgent, RecommendQuery{Task: "deploy the service", IncludeHost: true})
	if len(got) != 2 || !strings.Contains(strings.Join(got, ","), "skill://team/") {
		t.Fatalf("team member: %v", got)
	}
	if got := uris(teamAgent, RecommendQuery{Task: "deploy the service", IncludeHost: true, Source: "team"}); len(got) != 1 || !strings.HasPrefix(got[0], "skill://team/") {
		t.Fatalf("one source: %v", got)
	}
	if got := uris(memberViewer, RecommendQuery{Task: "review a pull request", Tags: []string{"nope"}}); len(got) != 0 {
		t.Fatalf("tag filter: %v", got)
	}
	res, err := svc.Recommend(ctx, memberViewer, RecommendQuery{Task: "review a pull request", Limit: 1})
	if err != nil || len(res.Skills) != 1 || res.Skills[0].Name != "pr-review" || res.Fallback != toolsearch.FallbackOff {
		t.Fatalf("auto without a ranker: %+v %v", res, err)
	}
	if r := res.Skills[0]; r.Source != "skills" || r.Path != "pr-review" || r.Runs != RunsAnywhere || r.Score <= 0 ||
		!strings.Contains(r.Rationale, `"review"`) || !strings.HasSuffix(r.Rationale, "(keyword rank)") {
		t.Fatalf("recommendation %+v", r)
	}

	for _, q := range []RecommendQuery{
		{Task: "   "},
		{Task: strings.Repeat("x", recommendMaxTask+1)},
		{Task: "deploy", Mode: "regex"},
		{Task: "deploy", Mode: "semantic"}, // no ranker on this server
		{Task: "deploy", Tags: make([]string, recommendMaxTags+1)},
	} {
		if _, err := svc.Recommend(ctx, memberViewer, q); !errors.Is(err, ErrInvalid) {
			t.Errorf("%.40q mode %q: %v", q.Task, q.Mode, err)
		}
	}
}

// A skill that becomes visible is recommended by the very next call: the
// index reloads when a version goes live, and the engine's caches are
// keyed by content.
func TestRecommendSeesNewSkillsAtOnce(t *testing.T) {
	gh := newFakeGitHub(t, "acme", "skills")
	gh.commit("main", baseTree())
	svc := newTestService(t, gh)
	src, _ := mustCreate(t, svc, SourceInput{URL: "acme/skills", Path: "skills", Mode: ModeAuto})
	ctx := context.Background()
	task := RecommendQuery{Task: "rotate the signing keys", Mode: "keyword"}
	res, err := svc.Recommend(ctx, memberViewer, task)
	if err != nil || len(res.Skills) != 0 {
		t.Fatalf("before: %+v %v", res, err)
	}
	gh.commit("main", map[string]string{"skills/key-rotation/SKILL.md": skillMD("key-rotation",
		"Rotate signing keys without downtime.", "metadata:\n  tags: security\n  triggers: a key leaked; a key expires soon", "Steps.\n")})
	sync, err := svc.SyncNow(ctx, src.ID)
	if err != nil || !sync.Activated {
		t.Fatalf("sync %+v %v", sync, err)
	}
	res, err = svc.Recommend(ctx, memberViewer, task)
	if err != nil || len(res.Skills) == 0 {
		t.Fatalf("after: %+v %v", res, err)
	}
	r := res.Skills[0]
	if r.URI != "skill://skills/key-rotation/SKILL.md" || !reflect.DeepEqual(r.Triggers, []string{"a key leaked", "a key expires soon"}) ||
		!reflect.DeepEqual(r.Tags, []string{"security"}) {
		t.Fatalf("new skill %+v", r)
	}
	// The tag filter sees it too.
	res, err = svc.Recommend(ctx, memberViewer, RecommendQuery{Task: "keys", Mode: "keyword", Tags: []string{"SECURITY"}})
	if err != nil || len(res.Skills) != 1 || res.Skills[0].Name != "key-rotation" {
		t.Fatalf("by tag: %+v %v", res, err)
	}
}
