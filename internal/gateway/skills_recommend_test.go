package gateway

import (
	"errors"
	"strings"
	"testing"
)

// skills.recommend passes the task, filters and mode to the Reader as the
// caller, answers its ranking as JSON, and refuses bad arguments before
// the Reader is asked.
func TestSkillsRecommendTool(t *testing.T) {
	f := newSkillsFixture(t)
	e := f.gw.tools[SkillsRecommendTool]
	if e.tool.Annotations.ReadOnlyHint == nil || !*e.tool.Annotations.ReadOnlyHint || !IsPinned(SkillsRecommendTool) {
		t.Fatalf("skills.recommend must be a pinned read-only tool: %+v", e.tool.Annotations)
	}
	out := f.ok(t, f.member, SkillsRecommendTool, map[string]any{
		"task": "deploy the api service", "limit": 3, "tags": []any{"ops", " "}, "mode": "Keyword", "source": "team",
	})
	if out["ranked_by"] != "keyword" || out["task"] != "deploy the api service" {
		t.Fatalf("answer %v", out)
	}
	skills, _ := out["skills"].([]any)
	if len(skills) == 0 {
		t.Fatalf("no skills: %v", out)
	}
	first, _ := skills[0].(map[string]any)
	if first["uri"] != "skill://team/ops/deploy/SKILL.md" || !strings.Contains(first["rationale"].(string), "deploy") {
		t.Fatalf("first skill %v", first)
	}
	f.reader.mu.Lock()
	q := f.reader.recommends[len(f.reader.recommends)-1]
	f.reader.mu.Unlock()
	if q.Limit != 3 || q.Mode != "keyword" || q.Source != "team" || len(q.Tags) != 1 || q.Tags[0] != "ops" {
		t.Fatalf("query %+v", q)
	}
	if v := f.reader.lastViewer(t); v.AgentID != "ag_member" {
		t.Fatalf("viewer %+v", v)
	}
	// No match is an empty list, not null.
	if out := f.ok(t, f.member, SkillsRecommendTool, map[string]any{"task": "zzz"}); out["skills"] == nil {
		t.Fatalf("empty answer %v", out)
	}

	f.reader.mu.Lock()
	asked := len(f.reader.recommends)
	f.reader.mu.Unlock()
	tooMany := make([]any, skillsRecommendMaxTags+1)
	for i := range tooMany {
		tooMany[i] = "t"
	}
	for _, args := range []map[string]any{
		{},
		{"task": "  "},
		{"task": "deploy", "mode": "regex"},
		{"task": "deploy", "tags": "ops"},
		{"task": "deploy", "tags": []any{"ops", 3}},
		{"task": "deploy", "tags": tooMany},
		{"task": "deploy", "limit": 0},
	} {
		if code := f.refused(t, f.member, SkillsRecommendTool, args); code != SkillsErrInvalid {
			t.Errorf("%v: code %q", args, code)
		}
	}
	f.reader.mu.Lock()
	if n := len(f.reader.recommends); n != asked {
		t.Errorf("the reader was asked %d times for refused calls", n-asked)
	}
	f.reader.mu.Unlock()

	// The Reader's refusal is the tool's.
	f.reader.setFail("Recommend", errors.New("down"))
	if code := f.refused(t, f.member, SkillsRecommendTool, map[string]any{"task": "deploy"}); code != SkillsErrInternal {
		t.Fatalf("failing reader: code %q", code)
	}
}
