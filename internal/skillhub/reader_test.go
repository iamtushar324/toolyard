package skillhub

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"sort"
	"strings"
	"testing"
)

// readerFixture is a hub with one public and one members-only source.
func readerFixture(t *testing.T) (*Service, *Source, *Source) {
	t.Helper()
	gh := newFakeGitHub(t, "acme", "skills")
	tree := baseTree()
	tree["skills/pr-review/references/style.md"] = "Prefer small diffs.\nNo force pushes.\n"
	tree["skills/pr-review/nested-helper/SKILL.md"] = skillMD("nested-helper", "A helper inside pr-review.", "", "helper body\n")
	gh.commit("main", tree)
	svc := newTestService(t, gh)
	pub, _ := mustCreate(t, svc, SourceInput{URL: "acme/skills", Path: "skills", Mode: ModeAuto})
	priv, _ := mustCreate(t, svc, SourceInput{URL: "acme/skills", Slug: "team", Path: "skills/deploy", Mode: ModeAuto,
		Visibility: VisibleMembers, Members: []string{"usr_team"}})
	return svc, pub, priv
}

func TestVisibilityAndRuns(t *testing.T) {
	svc, pub, priv := readerFixture(t)
	ctx := context.Background()
	if pub.Slug != "skills" || priv.Slug != "team" {
		t.Fatalf("slugs %q %q", pub.Slug, priv.Slug)
	}
	slugs := func(v Viewer) string {
		got, err := svc.Sources(ctx, v)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, s := range got {
			out = append(out, s.Slug)
		}
		return strings.Join(out, ",")
	}
	if got := slugs(adminViewer); got != "skills,team" {
		t.Fatalf("admin sees %q", got)
	}
	if got := slugs(memberViewer); got != "skills" {
		t.Fatalf("member sees %q", got)
	}
	teamAgent := Viewer{UserID: "usr_team", AgentID: "agt_team"}
	if got := slugs(teamAgent); got != "skills,team" {
		t.Fatalf("team member's agent sees %q", got)
	}
	// A skill at the source folder itself (skills/deploy is the "team"
	// source's folder) is named after the folder.
	if sk, err := svc.Get(ctx, teamAgent, "skill://team/SKILL.md", false); err != nil || sk.Name != "deploy" || sk.Path != "" {
		t.Fatalf("root skill %+v %v", sk, err)
	}
	if _, err := svc.Get(ctx, memberViewer, "skill://team/SKILL.md", false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("invisible source: %v", err)
	}

	// deploy runs on the host: hidden from listings off the host unless
	// asked for, but a direct reference still reads it.
	names := func(v Viewer, includeHost bool) string {
		got, _, err := svc.List(ctx, v, ListQuery{Source: "skills", IncludeHost: includeHost})
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, s := range got {
			out = append(out, s.Name+"="+string(s.Runs))
		}
		return strings.Join(out, ",")
	}
	if got := names(memberViewer, false); got != "pr-review=anywhere,nested-helper=anywhere" {
		t.Fatalf("member list %q", got)
	}
	if got := names(memberViewer, true); got != "deploy=host,pr-review=anywhere,nested-helper=anywhere" {
		t.Fatalf("member list with host %q", got)
	}
	if got := names(Viewer{UserID: "usr_member", OnHost: true}, false); !strings.Contains(got, "deploy=host") {
		t.Fatalf("host caller list %q", got)
	}
	if _, err := svc.Get(ctx, memberViewer, "skill://skills/deploy/SKILL.md", false); err != nil {
		t.Fatalf("direct get of a host skill: %v", err)
	}

	// Admin override beats the frontmatter; clearing it restores it.
	if err := svc.SetRuns(ctx, "usr_admin", pub.ID, "deploy", RunsAnywhere); err != nil {
		t.Fatal(err)
	}
	if got := names(memberViewer, false); !strings.Contains(got, "deploy=anywhere") {
		t.Fatalf("after override %q", got)
	}
	usage, err := svc.Skills(ctx, pub.ID)
	if err != nil {
		t.Fatal(err)
	}
	var deploy SkillUsage
	for _, u := range usage {
		if u.Path == "deploy" {
			deploy = u
		}
	}
	if deploy.Runs != RunsAnywhere || deploy.RunsOverride != RunsAnywhere {
		t.Fatalf("usage row %+v", deploy)
	}
	if err := svc.SetRuns(ctx, "usr_admin", pub.ID, "deploy", ""); err != nil {
		t.Fatal(err)
	}
	if got := names(memberViewer, false); strings.Contains(got, "deploy") {
		t.Fatalf("after clearing %q", got)
	}
	if err := svc.SetRuns(ctx, "usr_admin", pub.ID, "nope", RunsHost); !errors.Is(err, ErrNotFound) {
		t.Fatalf("override of a missing skill: %v", err)
	}
	// A source default of host applies where the frontmatter is silent.
	host := RunsHost
	if _, err := svc.UpdateSource(ctx, "usr_admin", pub.ID, SourceUpdate{DefaultRuns: &host}); err != nil {
		t.Fatal(err)
	}
	if got := names(memberViewer, false); got != "" {
		t.Fatalf("with default host %q", got)
	}
	// Visibility changes reach the index.
	members := VisibleMembers
	if _, err := svc.UpdateSource(ctx, "usr_admin", pub.ID, SourceUpdate{Visibility: &members}); err != nil {
		t.Fatal(err)
	}
	if got := slugs(memberViewer); got != "" {
		t.Fatalf("after making it members-only, member sees %q", got)
	}
}

func TestReaderSurface(t *testing.T) {
	svc, _, _ := readerFixture(t)
	ctx := context.Background()
	v := memberViewer

	// Search: keyword and regex over name, description and path.
	hits, err := svc.Search(ctx, v, SearchQuery{Text: "review pull request"})
	if err != nil || len(hits) == 0 || hits[0].Name != "pr-review" || hits[0].URI != "skill://skills/pr-review/SKILL.md" {
		t.Fatalf("keyword search %+v %v", hits, err)
	}
	if hits[0].Repo != RepoLabel("acme", "skills", hits[0].Commit) || len(hits[0].Commit) != 40 {
		t.Fatalf("search hit labels %+v", hits[0])
	}
	hits, err = svc.Search(ctx, v, SearchQuery{Text: "^nested", Mode: "regex"})
	if err != nil || len(hits) != 1 || hits[0].Path != "pr-review/nested-helper" {
		t.Fatalf("regex search %+v %v", hits, err)
	}
	if hits, _ := svc.Search(ctx, v, SearchQuery{Text: "deploy"}); len(hits) != 0 {
		t.Fatalf("host skill found off the host: %+v", hits)
	}
	if hits, _ := svc.Search(ctx, v, SearchQuery{Text: "deploy", IncludeHost: true}); len(hits) == 0 {
		t.Fatal("host skill not found with include_host")
	}
	if _, err := svc.Search(ctx, v, SearchQuery{Text: "x", Mode: "semantic"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("semantic mode: %v", err)
	}

	// Get by URI, slug/path and bare name; the file list has digests.
	sk, err := svc.Get(ctx, v, "pr-review", true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(sk.Body, "# Review") || sk.Frontmatter["name"] != "pr-review" || !strings.HasPrefix(sk.Raw, "---\n") {
		t.Fatalf("get %+v", sk)
	}
	var files []string
	for _, f := range sk.Files {
		if !strings.HasPrefix(f.Digest, "sha256:") || len(f.Digest) != 71 {
			t.Fatalf("digest %q", f.Digest)
		}
		files = append(files, f.Path)
	}
	if got := strings.Join(files, ","); got != "SKILL.md,checks.md,nested-helper/SKILL.md,references/style.md" {
		t.Fatalf("files %s", got)
	}
	if _, err := svc.Get(ctx, v, "skills/pr-review/nested-helper", false); err != nil {
		t.Fatalf("nested skill by path: %v", err)
	}
	if _, err := svc.Get(ctx, adminViewer, "deploy", false); !errors.Is(err, ErrConflict) {
		t.Fatalf("ambiguous name: %v", err)
	}
	if _, err := svc.Get(ctx, v, "skills/missing", false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing skill: %v", err)
	}

	// Read with line offset and limit.
	f, err := svc.Read(ctx, v, "skill://skills/pr-review/references/style.md", 1, 1)
	if err != nil || f.Content != "No force pushes.\n" || f.TotalLines != 2 || f.Truncated || f.Skill != "skill://skills/pr-review/SKILL.md" || f.Path != "references/style.md" {
		t.Fatalf("read %+v %v", f, err)
	}
	f, err = svc.Read(ctx, v, "skill://skills/pr-review/nested-helper/SKILL.md", 0, 1)
	if err != nil || f.Skill != "skill://skills/pr-review/nested-helper/SKILL.md" || !f.Truncated {
		t.Fatalf("read nested %+v %v", f, err)
	}
	if _, err := svc.Read(ctx, v, "skill://skills/README.md", 0, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("file outside skills: %v", err)
	}

	// ListDir.
	entries, err := svc.ListDir(ctx, v, "skill://skills/pr-review/")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		n := e.Name
		if e.IsDir {
			n += "/"
		}
		names = append(names, n)
	}
	sort.Strings(names)
	if got := strings.Join(names, ","); got != "SKILL.md,checks.md,nested-helper/,references/" {
		t.Fatalf("listdir %s", got)
	}

	// List pages with a cursor.
	page1, next, err := svc.List(ctx, v, ListQuery{Limit: 1})
	if err != nil || len(page1) != 1 || next == "" {
		t.Fatalf("list page 1 %+v %q %v", page1, next, err)
	}
	page2, next2, err := svc.List(ctx, v, ListQuery{Limit: 5, Cursor: next})
	if err != nil || len(page2) != 1 || next2 != "" || page2[0].Path == page1[0].Path {
		t.Fatalf("list page 2 %+v %q %v", page2, next2, err)
	}
	if len(page1[0].Files) == 0 || page1[0].Frontmatter["description"] == nil {
		t.Fatalf("list entry %+v", page1[0])
	}

	// Grep with path glob and case folding.
	ghits, err := svc.Grep(ctx, v, GrepQuery{Pattern: "force PUSHES", IgnoreCase: true})
	if err != nil || len(ghits) != 1 || ghits[0].Line != 2 || ghits[0].URI != "skill://skills/pr-review/references/style.md" {
		t.Fatalf("grep %+v %v", ghits, err)
	}
	if ghits, _ := svc.Grep(ctx, v, GrepQuery{Pattern: "deploy"}); len(ghits) != 0 {
		t.Fatalf("grep found host skill files off the host: %+v", ghits)
	}
	if ghits, _ := svc.Grep(ctx, v, GrepQuery{Pattern: ".", PathGlob: "*.md", Limit: 3}); len(ghits) != 3 {
		t.Fatalf("grep limit %d", len(ghits))
	}
	if ghits, _ := svc.Grep(ctx, v, GrepQuery{Pattern: "small", PathGlob: "pr-review/references/*"}); len(ghits) != 1 {
		t.Fatalf("grep anchored glob %+v", ghits)
	}
	if _, err := svc.Grep(ctx, v, GrepQuery{Pattern: "(unclosed"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad pattern: %v", err)
	}

	// Bundle: one folder per skill.
	var buf bytes.Buffer
	if err := svc.Bundle(ctx, v, []string{"skill://skills/pr-review/SKILL.md", "nested-helper"}, &buf); err != nil {
		t.Fatal(err)
	}
	got := tarNames(t, buf.Bytes())
	want := "nested-helper/SKILL.md,pr-review/SKILL.md,pr-review/checks.md,pr-review/nested-helper/SKILL.md,pr-review/references/style.md"
	if got != want {
		t.Fatalf("bundle %s", got)
	}

	// Reads were counted: two Gets, two Reads and two bundled skills.
	srcs, _ := svc.ListSources(ctx)
	usage, err := svc.Skills(ctx, srcs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	reads := map[string]int{}
	for _, u := range usage {
		reads[u.Path] = u.Reads30d
	}
	if reads["pr-review"] != 3 || reads["pr-review/nested-helper"] != 3 {
		t.Fatalf("reads %v", reads)
	}
}

func tarNames(t *testing.T, data []byte) string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var names []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}

func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestWebhook(t *testing.T) {
	gh := newFakeGitHub(t, "acme", "skills")
	gh.commit("main", baseTree())
	svc := newTestService(t, gh)
	ctx := context.Background()
	src, _ := mustCreate(t, svc, SourceInput{URL: "acme/skills", Path: "skills", Mode: ModeAuto})

	body := []byte(`{"zen":"Keep it logically awesome."}`)
	if err := svc.HandleWebhook(ctx, src.ID, "ping", body, sign("x", body)); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("no secret yet: %v", err)
	}
	secret, err := svc.RotateWebhookSecret(ctx, "usr_admin", src.ID)
	if err != nil || len(secret) != 64 {
		t.Fatalf("rotate %q %v", secret, err)
	}
	s, _ := svc.GetSource(ctx, src.ID)
	if !s.HasWebhookSecret || s.WebhookURLPath != "/v1/skills/sources/"+src.ID+"/webhook" {
		t.Fatalf("source %+v", s)
	}
	if err := svc.HandleWebhook(ctx, src.ID, "ping", body, sign(secret, body)); err != nil {
		t.Fatalf("ping: %v", err)
	}
	for _, sig := range []string{"", "sha256=", "sha256=zz", sign("wrong", body), strings.Replace(sign(secret, body), "sha256=", "sha1=", 1)} {
		if err := svc.HandleWebhook(ctx, src.ID, "ping", body, sig); !errors.Is(err, ErrBadSignature) {
			t.Fatalf("signature %q: %v", sig, err)
		}
	}

	// A push to another branch is ignored; a push to main syncs.
	other := []byte(`{"ref":"refs/heads/dev","after":"x"}`)
	if err := svc.HandleWebhook(ctx, src.ID, "push", other, sign(secret, other)); err != nil {
		t.Fatal(err)
	}
	svc.Wait()
	c2 := gh.commit("main", map[string]string{"skills/pr-review/checks.md": "pushed\n"})
	push := []byte(`{"ref":"refs/heads/main","after":"` + c2 + `"}`)
	if err := svc.HandleWebhook(ctx, src.ID, "push", push, sign(secret, push)); err != nil {
		t.Fatal(err)
	}
	svc.Wait()
	if got, _ := svc.Sources(ctx, adminViewer); len(got) != 1 || got[0].Commit != c2 {
		t.Fatalf("after push %+v", got)
	}
	// A rotated secret replaces the old one.
	secret2, _ := svc.RotateWebhookSecret(ctx, "usr_admin", src.ID)
	if err := svc.HandleWebhook(ctx, src.ID, "ping", body, sign(secret, body)); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("old secret still accepted: %v", err)
	}
	if err := svc.HandleWebhook(ctx, src.ID, "ping", body, sign(secret2, body)); err != nil {
		t.Fatal(err)
	}
	if err := svc.HandleWebhook(ctx, "src_missing", "ping", body, sign(secret2, body)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown source: %v", err)
	}
}

func TestSubscribeAndDelete(t *testing.T) {
	svc, pub, _ := readerFixture(t)
	ctx := context.Background()
	got := make(chan string, 8)
	cancel := svc.Subscribe(func(slug string) { got <- slug })
	defer cancel()
	if err := svc.DeleteSource(ctx, "usr_admin", pub.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case slug := <-got:
		if slug != "skills" {
			t.Fatalf("notified %q", slug)
		}
	default:
		t.Fatal("no notification after the delete")
	}
	if _, err := svc.Get(ctx, adminViewer, "skill://skills/pr-review/SKILL.md", false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted source still served: %v", err)
	}
	if err := svc.DeleteSource(ctx, "usr_admin", pub.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
	var n int
	if err := svc.db.QueryRow(`SELECT COUNT(*) FROM skill_versions WHERE source_id = ?`, pub.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("versions left %d %v", n, err)
	}
	if _, err := svc.CreateSource(ctx, "usr_admin", SourceInput{URL: "acme/skills", Slug: "team"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("taken slug: %v", err)
	}
	if _, err := svc.CreateSource(ctx, "usr_admin", SourceInput{URL: "acme/skills", Slug: "Bad Slug"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad slug: %v", err)
	}
	// Default slugs are made unique.
	a, err := svc.CreateSource(ctx, "usr_admin", SourceInput{URL: "acme/skills"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := svc.CreateSource(ctx, "usr_admin", SourceInput{URL: "acme/skills"})
	if err != nil || a.Slug != "skills" || b.Slug != "skills-2" {
		t.Fatalf("default slugs %q %q %v", a.Slug, b.Slug, err)
	}
}

func TestPreview(t *testing.T) {
	gh := newFakeGitHub(t, "acme", "skills")
	sha := gh.commit("main", baseTree())
	svc := newTestService(t, gh)
	p, err := svc.Preview(context.Background(), "usr_admin", SourceInput{URL: "https://github.com/acme/skills/tree/main/skills"})
	if err != nil {
		t.Fatal(err)
	}
	if p.SHA != sha || p.SkillCount != 2 || p.FileCount != 4 || len(p.Sample) != 2 || p.Source.Mode != ModeReview || p.Source.Path != "skills" {
		t.Fatalf("preview %+v", p)
	}
	if p.Sample[0].URI != "skill://skills/deploy/SKILL.md" || p.Sample[0].Runs != RunsHost {
		t.Fatalf("sample %+v", p.Sample[0])
	}
	if srcs, _ := svc.ListSources(context.Background()); len(srcs) != 0 {
		t.Fatal("preview stored a source")
	}
}
