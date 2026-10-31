package gateway

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/skillhub"
)

// fakeRegistry records the native registry calls and answers fixed
// results: a write is live as revision 7, unless pending or err says
// otherwise.
type fakeRegistry struct {
	mu      sync.Mutex
	writes  []skillhub.SkillWrite
	viewers []skillhub.Viewer
	gets    []int
	reads   []string
	// collections are the CreateCollection inputs.
	collections []skillhub.SourceInput
	pending     bool
	err         error
}

func (f *fakeRegistry) WriteSkill(_ context.Context, v skillhub.Viewer, w skillhub.SkillWrite) (*skillhub.WriteResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes = append(f.writes, w)
	f.viewers = append(f.viewers, v)
	if f.err != nil {
		return nil, f.err
	}
	if f.pending {
		return &skillhub.WriteResult{Status: skillhub.WritePending, URI: "skill://team/deploy/SKILL.md",
			Proposal: &skillhub.Proposal{ID: "skp_1", Kind: "change", Status: "pending"}, ApprovalReasons: []string{"it writes code"}}, nil
	}
	return &skillhub.WriteResult{Status: skillhub.WriteLive, URI: "skill://team/deploy/SKILL.md", Revision: 7, Commit: "abc"}, nil
}

func (f *fakeRegistry) SkillRevisions(_ context.Context, _ skillhub.Viewer, ref string, limit int) ([]skillhub.Revision, error) {
	return []skillhub.Revision{{Revision: 2, Op: "update", URI: "skill://team/deploy/SKILL.md", Current: true}, {Revision: 1, Op: "create"}}[:min(limit, 2)], nil
}

func (f *fakeRegistry) GetRevision(_ context.Context, _ skillhub.Viewer, ref string, revision int, _ bool) (*skillhub.Skill, error) {
	f.mu.Lock()
	f.gets = append(f.gets, revision)
	f.mu.Unlock()
	return &skillhub.Skill{SkillSummary: skillhub.SkillSummary{URI: "skill://team/deploy/SKILL.md", Name: "deploy", Revision: revision}, Body: "old\n"}, nil
}

func (f *fakeRegistry) CreateCollection(_ context.Context, v skillhub.Viewer, in skillhub.SourceInput) (*skillhub.Source, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.collections = append(f.collections, in)
	f.viewers = append(f.viewers, v)
	if f.err != nil {
		return nil, f.err
	}
	return &skillhub.Source{ID: "src_1", Slug: in.Slug, Kind: skillhub.KindNative, Mode: skillhub.ModeAuto, Visibility: in.Visibility,
		Members: in.Members, Editors: in.Editors, CreatedBy: v.UserID}, nil
}

func (f *fakeRegistry) ReadRevision(_ context.Context, _ skillhub.Viewer, uri string, revision, _, _ int) (*skillhub.File, error) {
	f.mu.Lock()
	f.reads = append(f.reads, uri)
	f.mu.Unlock()
	return &skillhub.File{FileRef: skillhub.FileRef{URI: uri, Path: "a.md"}, Content: "old line\n", TotalLines: 1}, nil
}

func TestSkillsCreateComposesTheSkill(t *testing.T) {
	f := newSkillsFixture(t)
	out := f.ok(t, f.member, SkillsCreateTool, map[string]any{
		"collection": "team", "name": "deploy", "description": "Deploy the service safely.",
		"instructions": "# Deploy\n\n1. Ship.\n", "triggers": []any{"the user asks to ship"}, "tags": []any{"ops"},
		"files": map[string]any{"references/a.md": "a\n"},
	})
	res, _ := out["result"].(map[string]any)
	if res["status"] != skillhub.WriteLive || res["revision"] != float64(7) || !strings.Contains(out["next"].(string), "skills.get") {
		t.Fatalf("answer %v", out)
	}
	w := f.registry.writes[0]
	md := w.Files["SKILL.md"]
	if w.Collection != "team" || w.Path != "deploy" || w.Files["references/a.md"] != "a\n" || w.Note != testReason ||
		!strings.Contains(md, "name: deploy") || !strings.Contains(md, "triggers: the user asks to ship") || !strings.Contains(md, "1. Ship.") {
		t.Fatalf("write %+v\n%s", w, md)
	}
	if v := f.registry.viewers[0]; v.UserID != "u_member" || v.AgentID != "ag_member" || !v.Delegated {
		t.Fatalf("viewer %+v", v)
	}
	// SKILL.md in files and the parts together: refused before any write.
	if code := f.refused(t, f.member, SkillsCreateTool, map[string]any{"collection": "team", "name": "x",
		"files": map[string]any{"SKILL.md": "---\nname: x\ndescription: d\n---\n"}}); code != SkillsErrInvalid {
		t.Fatalf("both: %q", code)
	}
	if code := f.refused(t, f.member, SkillsCreateTool, map[string]any{"collection": "team", "name": "Bad Name", "description": "d"}); code != SkillsErrInvalid {
		t.Fatalf("bad name: %q", code)
	}
	if len(f.registry.writes) != 1 {
		t.Fatalf("refused calls wrote: %d", len(f.registry.writes))
	}
}

func TestSkillsUpdateVersionsAndRevisions(t *testing.T) {
	f := newSkillsFixture(t)
	f.ok(t, f.member, SkillsUpdateTool, map[string]any{"skill": "team/deploy", "files": map[string]any{"old.md": ""},
		"base_revision": 3, "note": "drop the old notes"})
	if w := f.registry.writes[0]; w.Skill != "team/deploy" || w.BaseRevision != 3 || w.Note != "drop the old notes" || w.Files["old.md"] != "" {
		t.Fatalf("update %+v", w)
	}
	if code := f.refused(t, f.member, SkillsUpdateTool, map[string]any{"skill": "team/deploy"}); code != SkillsErrInvalid {
		t.Fatalf("nothing to change: %q", code)
	}
	if code := f.refused(t, f.member, SkillsUpdateTool, map[string]any{"skill": "team/deploy", "files": map[string]any{"a.md": 1}}); code != SkillsErrInvalid {
		t.Fatalf("bad files: %q", code)
	}

	// Pending: the answer says to poll the proposal.
	f.registry.pending = true
	out := f.ok(t, f.member, SkillsUpdateTool, map[string]any{"skill": "team/deploy", "files": map[string]any{"run.sh": "#!/bin/sh\n"}})
	if !strings.Contains(out["next"].(string), "skills.proposal (id skp_1)") {
		t.Fatalf("pending answer %v", out)
	}
	// A refusal keeps its code.
	f.registry.pending, f.registry.err = false, skillhub.ErrForbidden
	if code := f.refused(t, f.member, SkillsUpdateTool, map[string]any{"skill": "team/deploy", "files": map[string]any{"a.md": "a"}}); code != SkillsErrForbidden {
		t.Fatalf("forbidden: %q", code)
	}

	out = f.ok(t, f.member, SkillsVersionsTool, map[string]any{"skill": "team/deploy", "limit": 1})
	if revs, _ := out["revisions"].([]any); len(revs) != 1 || revs[0].(map[string]any)["revision"] != float64(2) {
		t.Fatalf("versions %v", out)
	}
	f.ok(t, f.member, SkillsGetTool, map[string]any{"skill": "team/deploy", "revision": 1})
	if len(f.registry.gets) != 1 || f.registry.gets[0] != 1 {
		t.Fatalf("get revision %v", f.registry.gets)
	}
	f.ok(t, f.member, SkillsReadTool, map[string]any{"uri": "skill://team/deploy/a.md?revision=1"})
	if len(f.registry.reads) != 1 {
		t.Fatalf("read revision %v", f.registry.reads)
	}
	if code := f.refused(t, f.member, SkillsGetTool, map[string]any{"skill": "team/deploy", "revision": 0}); code != SkillsErrInvalid {
		t.Fatalf("revision 0: %q", code)
	}
}

func TestSkillsBaseRevisionMustBeAWholeNumber(t *testing.T) {
	f := newSkillsFixture(t)
	for _, bad := range []any{"1", 1.5, 0, -2} {
		if code := f.refused(t, f.member, SkillsUpdateTool, map[string]any{"skill": "team/deploy", "base_revision": bad,
			"files": map[string]any{"a.md": "a"}}); code != SkillsErrInvalid {
			t.Errorf("base_revision %v: %q", bad, code)
		}
	}
	if len(f.registry.writes) != 0 {
		t.Fatalf("a refused base_revision wrote: %+v", f.registry.writes)
	}
	if n, ok := exactInt(2.0); !ok || n != 2 {
		t.Fatalf("2.0 = %d %v", n, ok)
	}
}

func TestSkillsCreateCollectionTool(t *testing.T) {
	f := newSkillsFixture(t)
	out := f.ok(t, f.admin, SkillsCreateCollectionTool, map[string]any{"slug": "team", "visibility": "members",
		"members": []any{"u_admin"}, "editors": []any{"u_member"}, "mode": "review"})
	c, _ := out["collection"].(map[string]any)
	if c["slug"] != "team" || c["kind"] != "native" || !strings.Contains(out["next"].(string), "skills.create") {
		t.Fatalf("answer %v", out)
	}
	in := f.registry.collections[0]
	if in.Slug != "team" || in.Visibility != "members" || in.Mode != "review" || len(in.Members) != 1 || in.Editors[0] != "u_member" {
		t.Fatalf("input %+v", in)
	}
	if code := f.refused(t, f.admin, SkillsCreateCollectionTool, map[string]any{}); code != SkillsErrInvalid {
		t.Fatalf("no slug: %q", code)
	}
	f.registry.err = skillhub.ErrForbidden
	if code := f.refused(t, f.member, SkillsCreateCollectionTool, map[string]any{"slug": "mine"}); code != SkillsErrForbidden {
		t.Fatalf("member: %q", code)
	}
}
