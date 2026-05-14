package skills

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// stubGateway mirrors internal/notes' test stub.
type stubGateway struct {
	has        map[string]bool
	calls      []call
	returnErr  error
	returnText string
	isError    bool
}

type call struct {
	target string
	args   map[string]any
}

func (g *stubGateway) HasTool(name string) bool { return g.has[name] }

func (g *stubGateway) CallInternal(_ context.Context, _, target string, args map[string]any) (*mcp.CallToolResult, error) {
	g.calls = append(g.calls, call{target: target, args: args})
	if g.returnErr != nil {
		return nil, g.returnErr
	}
	res := mcp.NewToolResultText(g.returnText)
	res.IsError = g.isError
	return res, nil
}

func tempDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func newSvc(t *testing.T, dir string, gw Dispatcher) *Service {
	t.Helper()
	return New(Options{
		DB:        tempDB(t),
		Gateway:   gw,
		SkillsDir: dir,
		Interval:  -1,
		WriteTool: DefaultWriteTool,
		DiaryTool: DefaultDiaryTool,
	})
}

const goodSkillMD = `---
name: hello-world
description: greet the user
---
# Hello World

This skill says hello.
`

func TestKebabSlug(t *testing.T) {
	cases := map[string]string{
		"hello-world":        "hello-world",
		"Hello World":        "hello-world",
		"hello_world":        "hello-world",
		"Hello   World!!":    "hello-world",
		"PlannotatorReview":  "plannotatorreview", // no boundary signal between adjacent letters
		"plannotator review": "plannotator-review",
		"  spaced  out  ":    "spaced-out",
		"":                   "",
	}
	for in, want := range cases {
		if got := KebabSlug(in); got != want {
			t.Errorf("KebabSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseSkillMD(t *testing.T) {
	fm, body, err := ParseSkillMD(goodSkillMD)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if fm.Name != "hello-world" || fm.Description != "greet the user" {
		t.Errorf("frontmatter wrong: %+v", fm)
	}
	if !strings.HasPrefix(body, "# Hello World") {
		t.Errorf("body wrong: %q", body)
	}
}

func TestParseSkillMDRejectsMissingFrontmatter(t *testing.T) {
	if _, _, err := ParseSkillMD("# no frontmatter\n"); !errors.Is(err, ErrFrontmatterMissing) {
		t.Errorf("want ErrFrontmatterMissing, got %v", err)
	}
}

func TestParseSkillMDRejectsUnclosedFrontmatter(t *testing.T) {
	bad := "---\nname: x\n# nope, never closed\n"
	if _, _, err := ParseSkillMD(bad); !errors.Is(err, ErrFrontmatterUnclosed) {
		t.Errorf("want ErrFrontmatterUnclosed, got %v", err)
	}
}

func TestValidateRejectsSlugMismatch(t *testing.T) {
	s := Skill{
		Slug:        "wrong-slug",
		Frontmatter: Frontmatter{Name: "hello-world", Description: "x"},
		Body:        "body",
	}
	if err := s.Validate(); !errors.Is(err, ErrSlugMismatch) {
		t.Errorf("want ErrSlugMismatch, got %v", err)
	}
}

func TestValidateRejectsScriptEscape(t *testing.T) {
	cases := []string{"../escape.sh", "/abs/path.sh", "ok/../bad.sh"}
	for _, c := range cases {
		s := Skill{
			Slug:        "hello-world",
			Frontmatter: Frontmatter{Name: "hello-world", Description: "x"},
			Body:        "body",
			Scripts:     map[string]string{c: "payload"},
		}
		if err := s.Validate(); err == nil {
			t.Errorf("Validate(%q) should fail", c)
		}
	}
}

func TestNewReturnsNilWhenDirEmpty(t *testing.T) {
	if New(Options{SkillsDir: "  "}) != nil {
		t.Errorf("expected nil for empty dir")
	}
}

func TestPublishWritesTreeAndIngests(t *testing.T) {
	dir := t.TempDir()
	gw := &stubGateway{
		has:        map[string]bool{DefaultWriteTool: true, DefaultDiaryTool: true},
		returnText: `{"success":true,"entry_id":"diary_xyz"}`,
	}
	s := newSvc(t, dir, gw)
	res, err := s.Publish(context.Background(), "agent-1", "", goodSkillMD,
		"interface:\n  display_name: hi\n",
		map[string]string{"build.sh": "#!/bin/sh\necho hi\n"},
		"")
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if res.Slug != "hello-world" {
		t.Errorf("slug = %q", res.Slug)
	}
	if !res.Indexed || res.PalaceID != "diary_xyz" {
		t.Errorf("expected indexed with palace_id, got %+v", res)
	}
	// Stub doesn't actually write, so we just verify the call sequence:
	// SKILL.md, agents/openai.yaml, scripts/build.sh, then mempalace diary.
	if len(gw.calls) != 4 {
		t.Fatalf("expected 4 CallInternal calls, got %d: %+v", len(gw.calls), gw.calls)
	}
	targets := []string{}
	for _, c := range gw.calls {
		targets = append(targets, c.target)
	}
	wantTargets := []string{DefaultWriteTool, DefaultWriteTool, DefaultWriteTool, DefaultDiaryTool}
	for i, w := range wantTargets {
		if targets[i] != w {
			t.Errorf("call %d: target=%q want %q", i, targets[i], w)
		}
	}
	// Final diary call carries the expected wing + agent.
	last := gw.calls[3].args
	if last["wing"] != "skills" || last["agent_name"] != "agent-1" {
		t.Errorf("diary args = %v", last)
	}
	entry, _ := last["entry"].(string)
	if !strings.HasPrefix(entry, "[skill:hello-world]") {
		t.Errorf("entry prefix wrong: %q", entry)
	}
}

func TestPublishFallsBackToDirectWriteWhenUpstreamMissing(t *testing.T) {
	dir := t.TempDir()
	gw := &stubGateway{
		has:        map[string]bool{DefaultDiaryTool: true}, // no skills.write_file
		returnText: `{"entry_id":"x"}`,
	}
	s := newSvc(t, dir, gw)
	if _, err := s.Publish(context.Background(), "agent", "", goodSkillMD, "", nil, ""); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "hello-world", "SKILL.md"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(body), "name: hello-world") {
		t.Errorf("on-disk SKILL.md missing frontmatter: %q", string(body))
	}
}

func TestPublishRejectsBadFrontmatter(t *testing.T) {
	dir := t.TempDir()
	gw := &stubGateway{has: map[string]bool{}}
	s := newSvc(t, dir, gw)
	_, err := s.Publish(context.Background(), "", "", "# no frontmatter\n", "", nil, "")
	if err == nil || !strings.Contains(err.Error(), "parse SKILL.md") {
		t.Errorf("expected parse error, got %v", err)
	}
}

func TestPublishRejectsSlugMismatch(t *testing.T) {
	dir := t.TempDir()
	gw := &stubGateway{has: map[string]bool{}}
	s := newSvc(t, dir, gw)
	_, err := s.Publish(context.Background(), "", "wrong-slug", goodSkillMD, "", nil, "")
	if !errors.Is(err, ErrSlugMismatch) {
		t.Errorf("want ErrSlugMismatch, got %v", err)
	}
}

func TestSyncIngestsOnlyChangedSkills(t *testing.T) {
	dir := t.TempDir()
	// Three skills + one non-skill dir + one hidden dir.
	mustWrite := func(rel, body string) {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("alpha/SKILL.md", "---\nname: alpha\n---\nbody\n")
	mustWrite("bravo/SKILL.md", "---\nname: bravo\n---\nbody\n")
	mustWrite("charlie/SKILL.md", "---\nname: charlie\n---\nbody\n")
	mustWrite("not-a-skill/README.md", "no SKILL.md here") // missing SKILL.md → skipped
	mustWrite(".hidden/SKILL.md", "ignored")               // hidden dir → skipped

	gw := &stubGateway{
		has:        map[string]bool{DefaultDiaryTool: true},
		returnText: `{"entry_id":"e"}`,
	}
	s := newSvc(t, dir, gw)
	ctx := context.Background()
	n, err := s.Sync(ctx)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if n != 3 {
		t.Errorf("first Sync ingested %d, want 3", n)
	}
	// Second sync — nothing changed → 0 ingests.
	gw.calls = nil
	n, _ = s.Sync(ctx)
	if n != 0 {
		t.Errorf("second Sync ingested %d, want 0", n)
	}
	// Bump mtime of one SKILL.md; expect exactly 1 ingest.
	bump := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "alpha", "SKILL.md"), bump, bump); err != nil {
		t.Fatal(err)
	}
	n, _ = s.Sync(ctx)
	if n != 1 {
		t.Errorf("third Sync ingested %d, want 1", n)
	}
}

func TestSyncShortCircuitsWhenDiaryToolMissing(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "alpha"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "alpha", "SKILL.md"), []byte("---\nname: alpha\n---\nbody"), 0o644); err != nil {
		t.Fatal(err)
	}
	gw := &stubGateway{has: map[string]bool{}}
	s := newSvc(t, dir, gw)
	n, _ := s.Sync(context.Background())
	if n != 0 {
		t.Errorf("ingested %d when diary tool missing; want 0", n)
	}
}

func TestInstallLinkMode(t *testing.T) {
	storeDir := t.TempDir()
	targetDir := t.TempDir()
	// Set up a skill on disk so Install has something to link.
	if err := os.MkdirAll(filepath.Join(storeDir, "alpha"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(storeDir, "alpha", "SKILL.md"), []byte("---\nname: alpha\n---\nbody"), 0o644); err != nil {
		t.Fatal(err)
	}
	gw := &stubGateway{has: map[string]bool{}}
	s := newSvc(t, storeDir, gw)
	res, err := s.Install(context.Background(), "agent", "alpha", "link", targetDir)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if res.Mode != "link" {
		t.Errorf("mode = %q", res.Mode)
	}
	info, err := os.Lstat(filepath.Join(targetDir, "alpha"))
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("target should be a symlink")
	}
	// Idempotent: second call returns OK.
	if _, err := s.Install(context.Background(), "agent", "alpha", "link", targetDir); err != nil {
		t.Errorf("second install (idempotent): %v", err)
	}
}

func TestInstallCopyMode(t *testing.T) {
	storeDir := t.TempDir()
	targetDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(storeDir, "alpha", "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(storeDir, "alpha", "SKILL.md"), []byte("---\nname: alpha\n---\nbody"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(storeDir, "alpha", "scripts", "run.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	gw := &stubGateway{has: map[string]bool{}}
	s := newSvc(t, storeDir, gw)
	res, err := s.Install(context.Background(), "agent", "alpha", "copy", targetDir)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if res.Mode != "copy" {
		t.Errorf("mode = %q", res.Mode)
	}
	// Both files copied over, not symlinked.
	for _, rel := range []string{"SKILL.md", "scripts/run.sh"} {
		p := filepath.Join(targetDir, "alpha", rel)
		info, err := os.Lstat(p)
		if err != nil {
			t.Fatalf("lstat %s: %v", p, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			t.Errorf("%s should not be a symlink", p)
		}
	}
}

func TestInstallRejectsUnknownSkill(t *testing.T) {
	storeDir := t.TempDir()
	gw := &stubGateway{has: map[string]bool{}}
	s := newSvc(t, storeDir, gw)
	_, err := s.Install(context.Background(), "agent", "ghost", "link", t.TempDir())
	if !errors.Is(err, ErrSkillNotFound) {
		t.Errorf("want ErrSkillNotFound, got %v", err)
	}
}

func TestSnapshotReportsTrackedCount(t *testing.T) {
	dir := t.TempDir()
	mustWrite := func(rel, body string) {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("alpha/SKILL.md", "---\nname: alpha\n---\nbody\n")
	gw := &stubGateway{
		has:        map[string]bool{DefaultDiaryTool: true},
		returnText: `{"entry_id":"e"}`,
	}
	s := newSvc(t, dir, gw)
	if _, err := s.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	st := s.Snapshot(context.Background())
	if !st.Enabled || st.Tracked != 1 {
		t.Errorf("snapshot = %+v", st)
	}
}

func TestParseTagListAcceptsCommaAndFlow(t *testing.T) {
	cases := map[string][]string{
		"coding, ai-tooling":          {"coding", "ai-tooling"},
		"[coding, ai]":                {"coding", "ai"},
		"  [ Personal Life, coding ]": {"personal-life", "coding"},
		"coding, coding, coding":      {"coding"}, // de-dupe after kebab
		"":                            nil,
		"   ":                         nil,
	}
	for in, want := range cases {
		got := ParseTagList(in)
		if len(got) != len(want) {
			t.Errorf("ParseTagList(%q) = %v, want %v", in, got, want)
			continue
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("ParseTagList(%q)[%d] = %q, want %q", in, i, got[i], want[i])
			}
		}
	}
}

func TestSerializeSkillMDEmitsTags(t *testing.T) {
	fm := Frontmatter{Name: "hello-world", Description: "say hi", Tags: []string{"coding", "ai"}}
	out := SerializeSkillMD(fm, "# body\n")
	if !strings.Contains(out, "tags: [coding, ai]") {
		t.Errorf("serialised SKILL.md missing tags line: %q", out)
	}
	// Round-trip: parse what we just wrote and confirm tags survive.
	parsed, _, err := ParseSkillMD(out)
	if err != nil {
		t.Fatalf("round-trip parse: %v", err)
	}
	if len(parsed.Tags) != 2 || parsed.Tags[0] != "coding" || parsed.Tags[1] != "ai" {
		t.Errorf("tags lost on round-trip: %v", parsed.Tags)
	}
}

func seedSkill(t *testing.T, dir, slug, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, slug), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, slug, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestListReturnsSummariesSortedBySlug(t *testing.T) {
	dir := t.TempDir()
	seedSkill(t, dir, "zeta", "---\nname: zeta\ndescription: zd\ntags: [coding]\n---\nbody\n")
	seedSkill(t, dir, "alpha", "---\nname: alpha\ndescription: ad\ntags: [coding, ai]\n---\nbody\n")
	seedSkill(t, dir, "mu", "---\nname: mu\ndescription: md\ntags: [personal-life]\n---\nbody\n")
	// A broken SKILL.md must not fail the whole listing.
	seedSkill(t, dir, "broken", "no frontmatter here")

	s := newSvc(t, dir, &stubGateway{has: map[string]bool{}})
	got, err := s.List(context.Background(), nil)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("List len = %d, want 3 (broken skill should be skipped)", len(got))
	}
	if got[0].Slug != "alpha" || got[1].Slug != "mu" || got[2].Slug != "zeta" {
		t.Errorf("List order wrong: %v", []string{got[0].Slug, got[1].Slug, got[2].Slug})
	}
}

func TestListFiltersByTag(t *testing.T) {
	dir := t.TempDir()
	seedSkill(t, dir, "alpha", "---\nname: alpha\ndescription: a\ntags: [coding, ai]\n---\n")
	seedSkill(t, dir, "bravo", "---\nname: bravo\ndescription: b\ntags: [personal-life]\n---\n")
	seedSkill(t, dir, "charlie", "---\nname: charlie\ndescription: c\ntags: [coding]\n---\n")

	s := newSvc(t, dir, &stubGateway{has: map[string]bool{}})
	got, err := s.List(context.Background(), []string{"Coding"}) // ensure normalisation happens
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 coding skills, got %d: %+v", len(got), got)
	}
	slugs := []string{got[0].Slug, got[1].Slug}
	if slugs[0] != "alpha" || slugs[1] != "charlie" {
		t.Errorf("filtered slugs = %v", slugs)
	}
}

func TestListTagsCountsAcrossSkills(t *testing.T) {
	dir := t.TempDir()
	seedSkill(t, dir, "alpha", "---\nname: alpha\ntags: [coding, ai]\n---\n")
	seedSkill(t, dir, "bravo", "---\nname: bravo\ntags: [coding]\n---\n")
	seedSkill(t, dir, "charlie", "---\nname: charlie\ntags: [personal-life]\n---\n")

	s := newSvc(t, dir, &stubGateway{has: map[string]bool{}})
	tags, err := s.ListTags(context.Background())
	if err != nil {
		t.Fatalf("ListTags: %v", err)
	}
	if tags["coding"] != 2 || tags["ai"] != 1 || tags["personal-life"] != 1 {
		t.Errorf("tag counts wrong: %+v", tags)
	}
}

func TestGetReturnsSkillMDOnlyByDefault(t *testing.T) {
	dir := t.TempDir()
	seedSkill(t, dir, "alpha", "---\nname: alpha\n---\n# body\n")
	if err := os.MkdirAll(filepath.Join(dir, "alpha", "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "alpha", "scripts", "run.sh"), []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := newSvc(t, dir, &stubGateway{has: map[string]bool{}})
	got, err := s.Get(context.Background(), "alpha", false)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !strings.Contains(got.SkillMD, "name: alpha") {
		t.Errorf("SKILL.md not returned: %q", got.SkillMD)
	}
	if got.Scripts != nil && len(got.Scripts) > 0 {
		t.Errorf("scripts leaked into default Get response: %v", got.Scripts)
	}
}

func TestGetIncludesFilesWhenAsked(t *testing.T) {
	dir := t.TempDir()
	seedSkill(t, dir, "alpha", "---\nname: alpha\n---\n# body\n")
	if err := os.MkdirAll(filepath.Join(dir, "alpha", "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "alpha", "scripts", "run.sh"), []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "alpha", "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "alpha", "agents", "openai.yaml"), []byte("interface: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := newSvc(t, dir, &stubGateway{has: map[string]bool{}})
	got, err := s.Get(context.Background(), "alpha", true)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.AgentsOpenAIYAML == "" {
		t.Errorf("agents YAML missing from full Get")
	}
	if got.Scripts["run.sh"] != "#!/bin/sh\n" {
		t.Errorf("scripts content wrong: %+v", got.Scripts)
	}
}

func TestGetRejectsUnknownSkill(t *testing.T) {
	dir := t.TempDir()
	s := newSvc(t, dir, &stubGateway{has: map[string]bool{}})
	_, err := s.Get(context.Background(), "ghost", false)
	if !errors.Is(err, ErrSkillNotFound) {
		t.Errorf("want ErrSkillNotFound, got %v", err)
	}
}

func TestBundleRoundTrip(t *testing.T) {
	dir := t.TempDir()
	seedSkill(t, dir, "alpha", "---\nname: alpha\n---\n# alpha body\n")
	seedSkill(t, dir, "bravo", "---\nname: bravo\n---\n# bravo body\n")

	s := newSvc(t, dir, &stubGateway{has: map[string]bool{}})
	res, err := s.Bundle(context.Background(), []string{"alpha", "bravo"})
	if err != nil {
		t.Fatalf("Bundle: %v", err)
	}
	if len(res.Slugs) != 2 {
		t.Errorf("Slugs = %v", res.Slugs)
	}
	if res.RawBytes == 0 || res.Bytes == 0 {
		t.Errorf("byte counts zero: %+v", res)
	}

	// Decode + untar and confirm we can recover the SKILL.md files.
	raw, err := base64.StdEncoding.DecodeString(res.TarGzB64)
	if err != nil {
		t.Fatalf("base64 decode: %v", err)
	}
	gzr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	defer gzr.Close()
	tr := tar.NewReader(gzr)
	found := map[string]string{}
	for {
		hdr, err := tr.Next()
		if err != nil {
			break
		}
		if hdr.FileInfo().IsDir() {
			continue
		}
		body, _ := io.ReadAll(tr)
		found[hdr.Name] = string(body)
	}
	if !strings.Contains(found["alpha/SKILL.md"], "name: alpha") {
		t.Errorf("alpha SKILL.md missing from bundle: %v", found)
	}
	if !strings.Contains(found["bravo/SKILL.md"], "name: bravo") {
		t.Errorf("bravo SKILL.md missing from bundle: %v", found)
	}
}

func TestBundleEmptySlugsBundlesEverything(t *testing.T) {
	dir := t.TempDir()
	seedSkill(t, dir, "alpha", "---\nname: alpha\n---\n")
	seedSkill(t, dir, "bravo", "---\nname: bravo\n---\n")
	s := newSvc(t, dir, &stubGateway{has: map[string]bool{}})
	res, err := s.Bundle(context.Background(), nil)
	if err != nil {
		t.Fatalf("Bundle: %v", err)
	}
	if len(res.Slugs) != 2 {
		t.Errorf("expected 2 slugs auto-discovered, got %v", res.Slugs)
	}
}

func TestBundleRejectsUnknownSlug(t *testing.T) {
	dir := t.TempDir()
	s := newSvc(t, dir, &stubGateway{has: map[string]bool{}})
	_, err := s.Bundle(context.Background(), []string{"ghost"})
	if !errors.Is(err, ErrSkillNotFound) {
		t.Errorf("want ErrSkillNotFound, got %v", err)
	}
}

func TestNilServiceIsSafe(t *testing.T) {
	var s *Service
	if s.Enabled() {
		t.Errorf("nil should not be enabled")
	}
	if dir := s.SkillsDir(); dir != "" {
		t.Errorf("nil SkillsDir = %q", dir)
	}
	st := s.Snapshot(context.Background())
	if st.Enabled {
		t.Errorf("nil Snapshot.Enabled = true")
	}
}
