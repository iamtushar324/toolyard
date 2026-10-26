package skillhub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// Skill URIs percent-encode each path segment, so every listed file has a
// URI a client can send back, and splitURI decodes them again.
func TestSkillURIsArePercentEncoded(t *testing.T) {
	gh := newFakeGitHub(t, "acme", "skills")
	tree := baseTree()
	tree["skills/odd/SKILL.md"] = skillMD("odd", "Files with odd names.", "", "body\n")
	tree["skills/odd/references/my notes.md"] = "spaces\n"
	tree["skills/odd/références/été.md"] = "accents\n"
	tree["skills/odd/hash/#1.md"] = "hash\n"
	tree["skills/odd/pct/100%.md"] = "percent\n"
	tree["skills/odd/q/what?.md"] = "question\n"
	tree["skills/odd/sym/{a}|^.md"] = "symbols\n"
	gh.commit("main", tree)
	svc := newTestService(t, gh)
	mustCreate(t, svc, SourceInput{URL: "acme/skills", Path: "skills", Mode: ModeAuto})
	ctx := context.Background()
	v := memberViewer

	sk, err := svc.Get(ctx, v, "skill://skills/odd/SKILL.md", true)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"skill://skills/odd/SKILL.md":                              tree["skills/odd/SKILL.md"],
		"skill://skills/odd/references/my%20notes.md":              "spaces\n",
		"skill://skills/odd/r%C3%A9f%C3%A9rences/%C3%A9t%C3%A9.md": "accents\n",
		"skill://skills/odd/hash/%231.md":                          "hash\n",
		"skill://skills/odd/pct/100%25.md":                         "percent\n",
		"skill://skills/odd/q/what%3F.md":                          "question\n",
		"skill://skills/odd/sym/%7Ba%7D%7C%5E.md":                  "symbols\n",
	}
	if len(sk.Files) != len(want) {
		t.Fatalf("files %+v", sk.Files)
	}
	for _, f := range sk.Files {
		content, ok := want[f.URI]
		if !ok {
			t.Errorf("unexpected or unencoded URI %q (path %q)", f.URI, f.Path)
			continue
		}
		got, err := svc.Read(ctx, v, f.URI, 0, 0)
		if err != nil || got.Content != content || got.URI != f.URI {
			t.Errorf("read %s = %+v %v", f.URI, got, err)
		}
	}
	// An unencoded URI from a lenient client still reads.
	if got, err := svc.Read(ctx, v, "skill://skills/odd/references/my notes.md", 0, 0); err != nil || got.Content != "spaces\n" {
		t.Fatalf("unencoded URI: %+v %v", got, err)
	}
	// Invalid escapes, a decoded "/" and dot segments are refused.
	for _, bad := range []string{
		"skill://skills/odd/pct/100%.md", "skill://skills/odd/pct/100%zz.md", "skill://skills/odd/a%2Fb.md",
		"skill://skills/odd/../odd/SKILL.md", "skill://skills/odd/%2E%2E/odd/SKILL.md", "skill://skills/odd//SKILL.md",
	} {
		if _, err := svc.Read(ctx, v, bad, 0, 0); !errors.Is(err, ErrInvalid) {
			t.Errorf("read %q: %v, want ErrInvalid", bad, err)
		}
	}
	// Folders: encoded, no trailing slash (SEP-2640), and readable back.
	entries, err := svc.ListDir(ctx, v, "skill://skills/odd")
	if err != nil {
		t.Fatal(err)
	}
	dirs := map[string]bool{}
	for _, e := range entries {
		if strings.HasSuffix(e.URI, "/") {
			t.Errorf("folder URI with a trailing slash: %q", e.URI)
		}
		if e.IsDir {
			dirs[e.URI] = true
		}
	}
	for _, d := range []string{"skill://skills/odd/references", "skill://skills/odd/r%C3%A9f%C3%A9rences", "skill://skills/odd/hash", "skill://skills/odd/pct"} {
		if !dirs[d] {
			t.Errorf("folder %q missing from %+v", d, entries)
		}
	}
	inner, err := svc.ListDir(ctx, v, "skill://skills/odd/r%C3%A9f%C3%A9rences")
	if err != nil || len(inner) != 1 || inner[0].URI != "skill://skills/odd/r%C3%A9f%C3%A9rences/%C3%A9t%C3%A9.md" || inner[0].Name != "été.md" {
		t.Fatalf("encoded folder %+v %v", inner, err)
	}
	// Grep and search name the same encoded URIs.
	hits, err := svc.Grep(ctx, v, GrepQuery{Pattern: "accents"})
	if err != nil || len(hits) != 1 || hits[0].URI != "skill://skills/odd/r%C3%A9f%C3%A9rences/%C3%A9t%C3%A9.md" {
		t.Fatalf("grep %+v %v", hits, err)
	}
	if FileURI("s", "a b/c#d") != "skill://s/a%20b/c%23d" || FileURI("s", "") != "skill://s" || SkillURI("s", "x y") != "skill://s/x%20y/SKILL.md" {
		t.Fatalf("FileURI/SkillURI: %q %q %q", FileURI("s", "a b/c#d"), FileURI("s", ""), SkillURI("s", "x y"))
	}
}

// A skill at the source folder itself whose name differs from the slug is
// served under its name, so the URI's last skill-path segment is the name
// (SEP-2640); the old URI still resolves.
func TestRootSkillServedUnderItsName(t *testing.T) {
	svc, _, _ := readerFixture(t)
	ctx := context.Background()
	team := Viewer{UserID: "usr_team", AgentID: "agt_team"}

	// (The bare name "deploy" is ambiguous here: the public source has a
	// deploy skill too.)
	for _, ref := range []string{"skill://team/SKILL.md", "skill://team/deploy/SKILL.md", "team/deploy"} {
		sk, err := svc.Get(ctx, team, ref, true)
		if err != nil || sk.URI != "skill://team/deploy/SKILL.md" || sk.Path != "" || sk.Name != "deploy" {
			t.Fatalf("get %q = %+v %v", ref, sk, err)
		}
		var uris []string
		for _, f := range sk.Files {
			uris = append(uris, f.URI)
		}
		if got := strings.Join(uris, ","); got != "skill://team/deploy/SKILL.md,skill://team/deploy/scripts/run.sh" {
			t.Fatalf("root skill files %s", got)
		}
	}
	for _, uri := range []string{"skill://team/deploy/scripts/run.sh", "skill://team/scripts/run.sh"} {
		f, err := svc.Read(ctx, team, uri, 0, 0)
		if err != nil || f.URI != "skill://team/deploy/scripts/run.sh" || f.Skill != "skill://team/deploy/SKILL.md" || f.Path != "scripts/run.sh" {
			t.Fatalf("read %q = %+v %v", uri, f, err)
		}
	}
	list, _, err := svc.List(ctx, team, ListQuery{Source: "team", IncludeHost: true})
	if err != nil || len(list) != 1 || list[0].URI != "skill://team/deploy/SKILL.md" {
		t.Fatalf("list %+v %v", list, err)
	}
	entries, err := svc.ListDir(ctx, team, "skill://team/deploy")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.URI)
	}
	if strings.Join(got, ",") != "skill://team/deploy/SKILL.md,skill://team/deploy/scripts" {
		t.Fatalf("root skill folder %v", got)
	}
	if inner, err := svc.ListDir(ctx, team, "skill://team/deploy/scripts"); err != nil || len(inner) != 1 || inner[0].URI != "skill://team/deploy/scripts/run.sh" {
		t.Fatalf("aliased subfolder %+v %v", inner, err)
	}
	// A source whose folder skill is named after the slug keeps
	// skill://<slug>/SKILL.md.
	gh := newFakeGitHub(t, "acme", "deploy")
	gh.commit("main", map[string]string{"SKILL.md": skillMD("deploy", "Deploy it.", "", "Run it.\n")})
	svc2 := newTestService(t, gh)
	mustCreate(t, svc2, SourceInput{URL: "acme/deploy", Mode: ModeAuto})
	if sk, err := svc2.Get(ctx, memberViewer, "deploy", false); err != nil || sk.URI != "skill://deploy/SKILL.md" {
		t.Fatalf("slug equals name: %+v %v", sk, err)
	}
}

// Listing a source folder or a folder above skills leaves out what only
// host-only skills hold, unless the viewer is on the host; a folder inside
// a skill (a direct reference) lists everything.
func TestListDirHidesHostOnlyFolders(t *testing.T) {
	svc, _, _ := readerFixture(t)
	ctx := context.Background()
	names := func(v Viewer, uri string) string {
		t.Helper()
		entries, err := svc.ListDir(ctx, v, uri)
		if err != nil {
			t.Fatalf("listdir %s: %v", uri, err)
		}
		var out []string
		for _, e := range entries {
			out = append(out, e.Name)
		}
		return strings.Join(out, ",")
	}
	if got := names(memberViewer, "skill://skills"); got != "pr-review" {
		t.Fatalf("member source folder %q", got)
	}
	if got := names(Viewer{UserID: "usr_member", OnHost: true}, "skill://skills"); got != "deploy,pr-review" {
		t.Fatalf("host source folder %q", got)
	}
	if got := names(memberViewer, "skill://skills/deploy"); got != "SKILL.md,scripts" {
		t.Fatalf("member host skill folder %q", got)
	}
}

// Frontmatter numbers stay exact: an integer above 2^53 keeps every digit.
func TestFrontmatterNumbersStayExact(t *testing.T) {
	gh := newFakeGitHub(t, "acme", "skills")
	tree := baseTree()
	tree["skills/numbers/SKILL.md"] = skillMD("numbers", "Big numbers.", "metadata:\n  big: 9007199254740993\n  ratio: 0.1", "body\n")
	gh.commit("main", tree)
	svc := newTestService(t, gh)
	mustCreate(t, svc, SourceInput{URL: "acme/skills", Path: "skills", Mode: ModeAuto})
	sk, err := svc.Get(context.Background(), memberViewer, "numbers", false)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(sk.Frontmatter)
	if err != nil || !strings.Contains(string(raw), `"big":9007199254740993`) || !strings.Contains(string(raw), `"ratio":0.1`) {
		t.Fatalf("frontmatter JSON %s %v", raw, err)
	}
}

// A skill over SEP-2640's per-skill limits is served, with a warning.
func TestSEPSizeWarning(t *testing.T) {
	gh := newFakeGitHub(t, "acme", "skills")
	tree := baseTree()
	tree["skills/many/SKILL.md"] = skillMD("many", "Many files.", "", "body\n")
	for i := 0; i < sepMaxResources; i++ {
		tree[fmt.Sprintf("skills/many/files/f%04d.md", i)] = "x\n"
	}
	gh.commit("main", tree)
	svc := newTestService(t, gh)
	src, res := mustCreate(t, svc, SourceInput{URL: "acme/skills", Path: "skills", Mode: ModeAuto})
	if !res.Activated {
		t.Fatalf("sync %+v", res)
	}
	v, err := svc.GetVersion(context.Background(), src.ID, res.VersionID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range v.Warnings {
		if w.Code == "sep_too_many_files" && w.Severity == "warn" && strings.Contains(w.Path, "many/SKILL.md") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no SEP size warning in %+v", v.Warnings)
	}
	if sk, err := svc.Get(context.Background(), memberViewer, "many", true); err != nil || len(sk.Files) != sepMaxResources+1 {
		t.Fatalf("the skill must still be served: %v", err)
	}
}
