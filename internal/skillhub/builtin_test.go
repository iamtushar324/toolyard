package skillhub

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/tusharbhardwaj/toolyard/internal/cluster"
	"github.com/tusharbhardwaj/toolyard/internal/skillhub/builtin"
	"github.com/tusharbhardwaj/toolyard/internal/store"
	"github.com/tusharbhardwaj/toolyard/internal/store/storetest"
)

// noGitHub is a GitHub API that fails the test on any call: the built-in
// source never reads GitHub.
func noGitHub(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the built-in source called GitHub: %s %s", r.Method, r.URL.Path)
		http.Error(w, "no", http.StatusTeapot)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newBuiltinService is a hub with the built-in skills on, over db, serving
// files (nil: the embedded tree), not started.
func newBuiltinService(t *testing.T, db *store.DB, c *cluster.Cluster, files map[string][]byte) *Service {
	t.Helper()
	gh := noGitHub(t)
	svc, err := New(Options{DB: db, Cluster: c, Settings: staticSettings{}, GitHubAPI: gh.URL, HTTPClient: gh.Client(), BuiltinSkills: true})
	if err != nil {
		t.Fatal(err)
	}
	if files != nil {
		setBuiltinFiles(svc, files)
	}
	return svc
}

// setBuiltinFiles makes svc serve files as its built-in tree.
func setBuiltinFiles(svc *Service, files map[string][]byte) {
	svc.bi.mu.Lock()
	defer svc.bi.mu.Unlock()
	svc.bi.load = func() (map[string][]byte, error) { return files, nil }
	svc.bi.tree = nil
}

// startService starts svc until the test ends.
func startService(t *testing.T, svc *Service) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(svc.Wait)
	t.Cleanup(cancel)
	if err := svc.Start(ctx); err != nil {
		t.Fatal(err)
	}
}

func embeddedTree(t *testing.T) map[string][]byte {
	t.Helper()
	files, err := builtin.Files()
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// builtinSkillNames are the top folders of a tree that hold a SKILL.md.
func builtinSkillNames(files map[string][]byte) []string {
	var names []string
	for p := range files {
		if dir, file := path.Split(p); file == skillFile && strings.Count(p, "/") == 1 {
			names = append(names, strings.TrimSuffix(dir, "/"))
		}
	}
	sort.Strings(names)
	return names
}

func versionCount(t *testing.T, db *store.DB) int {
	t.Helper()
	var n int64
	if err := db.QueryRow(`SELECT COUNT(*) FROM skill_versions WHERE source_id = ?`, BuiltinSourceID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return int(n)
}

// Every built-in skill follows the Agent Skills specification with no
// warning, reads as plain public text, and links only to files and
// built-in skills that exist.
func TestBuiltinSkillsAreValid(t *testing.T) {
	files := embeddedTree(t)
	names := builtinSkillNames(files)
	if len(names) < 6 || len(names) > 10 {
		t.Fatalf("built-in skills: %v (want 6 to 10)", names)
	}
	isSkill := map[string]bool{}
	for _, n := range names {
		isSkill[n] = true
	}
	if !isSkill[BuiltinGuideSkill] {
		t.Fatalf("the guide %s is missing: %v", BuiltinGuideSkill, names)
	}
	for p := range files {
		if top, _, _ := strings.Cut(p, "/"); !isSkill[top] {
			t.Errorf("%s is outside a skill folder", p)
		}
	}
	for _, name := range names {
		md := files[name+"/"+skillFile]
		meta, warns, skip := parseSkillMD(name+"/"+skillFile, name, md)
		if skip || len(warns) > 0 {
			t.Errorf("%s: skipped %v, warnings %+v", name, skip, warns)
			continue
		}
		if meta.name != name || meta.runs != RunsAnywhere {
			t.Errorf("%s: name %q runs %q (want the folder name and metadata.toolyard-runs: anywhere)", name, meta.name, meta.runs)
		}
		if n := utf8.RuneCountInString(meta.description); n < 80 || n > maxDescription {
			t.Errorf("%s: description is %d characters (want what and when, at most %d)", name, n, maxDescription)
		}
		_, body, _ := splitFrontmatter(string(md))
		if lines := strings.Count(body, "\n"); lines >= 500 || lines < 20 {
			t.Errorf("%s: body is %d lines (want a useful body under 500)", name, lines)
		}
	}
	row := &sourceRow{Source: Source{Kind: KindBuiltin, Owner: builtinOwner, Repo: builtinRepo}}
	b, err := buildBuiltin(files, row)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.warnings) > 0 || len(b.skills) != len(names) || len(b.files) != len(files) {
		t.Fatalf("built %d skills, %d of %d files, warnings %+v", len(b.skills), len(b.files), len(files), b.warnings)
	}

	// Public text only: no organisation, people, internal hosts or
	// addresses, nothing that looks like a credential.
	forbidden := regexp.MustCompile(`(?i)beknown|tushar|\.live\b|\bts\.net\b|tailnet|dev-server|\b(?:\d{1,3}\.){3}\d{1,3}\b`)
	link := regexp.MustCompile(`\]\(([^)\s]+)\)`)
	ref := regexp.MustCompile(`toolyard/(toolyard-[a-z0-9-]+)`)
	for p, content := range files {
		text := string(content)
		if !isText(content) {
			t.Errorf("%s is not UTF-8 text", p)
		}
		if what := findSecret(content); what != "" {
			t.Errorf("%s looks like it holds %s", p, what)
		}
		if m := forbidden.FindString(text); m != "" {
			t.Errorf("%s mentions %q: built-in skills describe toolyard for any deployment", p, m)
		}
		for _, m := range link.FindAllStringSubmatch(text, -1) {
			target := m[1]
			if strings.Contains(target, "://") || strings.HasPrefix(target, "#") || strings.HasPrefix(target, "mailto:") {
				continue
			}
			target, _, _ = strings.Cut(target, "#")
			if _, ok := files[path.Join(path.Dir(p), target)]; !ok {
				t.Errorf("%s links to %s, which is not a built-in file", p, m[1])
			}
		}
		for _, m := range ref.FindAllStringSubmatch(text, -1) {
			if !isSkill[m[1]] {
				t.Errorf("%s refers to toolyard/%s, which is not a built-in skill", p, m[1])
			}
		}
	}
}

// The hub installs the built-in source at start, on SQLite and Postgres,
// with no GitHub call; agents find, read and list its skills like any
// other; a restart with the same skills makes no new version.
func TestBuiltinSourceInstalledAtStart(t *testing.T) {
	db := storetest.Open(t)
	files := embeddedTree(t)
	digest := builtin.Digest(files)
	svc := newBuiltinService(t, db, nil, nil)
	startService(t, svc)
	ctx := context.Background()

	src, err := svc.GetSource(ctx, BuiltinSourceID)
	if err != nil {
		t.Fatal(err)
	}
	if src.Kind != KindBuiltin || src.Slug != BuiltinSlug || src.ActiveSHA != digest || src.LastCheckedAt == nil || src.LastError != "" ||
		src.Visibility != VisibleEveryone || src.DefaultRuns != RunsAnywhere || src.SkillCount != len(builtinSkillNames(files)) || src.WarningCount != 0 {
		t.Fatalf("built-in source %+v (digest %s)", src, digest)
	}
	label := "toolyard/builtin@" + digest[:12]
	infos, err := svc.Sources(ctx, memberViewer)
	if err != nil || len(infos) != 1 || infos[0].Slug != BuiltinSlug || infos[0].Repo != label || infos[0].Commit != digest || infos[0].HTMLURL != "" {
		t.Fatalf("member sources %+v %v", infos, err)
	}

	hits, err := svc.Search(ctx, memberViewer, SearchQuery{Text: "toolyard basics"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, h := range hits {
		found = found || h.URI == "skill://toolyard/toolyard-basics/SKILL.md"
	}
	if !found {
		t.Fatalf("search for the guide: %+v", hits)
	}
	sk, err := svc.Get(ctx, memberViewer, svc.BuiltinGuide(), true)
	if err != nil || sk.Name != BuiltinGuideSkill || sk.Repo != label || sk.Commit != digest || strings.TrimSpace(sk.Body) == "" || len(sk.Files) == 0 {
		t.Fatalf("get the guide: %+v %v", sk, err)
	}
	var refFile string
	for _, p := range sortedPaths(files) {
		if strings.Contains(p, "/references/") {
			refFile = p
			break
		}
	}
	if refFile == "" {
		t.Fatal("no built-in skill has a references/ file")
	}
	f, err := svc.Read(ctx, memberViewer, "skill://toolyard/"+refFile, 0, 0)
	if err != nil || f.Content != string(files[refFile]) || f.Repo != label {
		t.Fatalf("read %s: %+v %v", refFile, f, err)
	}
	list, _, err := svc.List(ctx, memberViewer, ListQuery{Limit: 500})
	if err != nil || len(list) != len(builtinSkillNames(files)) {
		t.Fatalf("SEP-2640 listing: %d skills %v", len(list), err)
	}
	for _, e := range list {
		if e.Source != BuiltinSlug || len(e.Files) == 0 || !strings.HasPrefix(e.Files[0].Digest, "sha256:") {
			t.Fatalf("listing entry %+v", e)
		}
	}
	if grep, err := svc.Grep(ctx, memberViewer, GrepQuery{Pattern: "_reason", Source: BuiltinSlug}); err != nil || len(grep) == 0 {
		t.Fatalf("grep the built-in skills: %d hits %v", len(grep), err)
	}
	if n := versionCount(t, db); n != 1 {
		t.Fatalf("versions after start: %d", n)
	}
	// The poller leaves the built-in source alone (noGitHub fails on a call).
	svc.pollDue(ctx)

	// A restart with the same skills makes no new version.
	again := newBuiltinService(t, db, nil, nil)
	startService(t, again)
	if n := versionCount(t, db); n != 1 {
		t.Fatalf("versions after a restart: %d", n)
	}
	if res, err := again.SyncNow(ctx, BuiltinSourceID); err != nil || !res.Unchanged || res.SHA != digest {
		t.Fatalf("sync now of the built-in source: %+v %v", res, err)
	}
}

// A new toolyard whose skills differ makes exactly one new version, even
// when two replicas install it at once; an unchanged one makes none; a
// downgrade makes the older version live again without a new one.
func TestBuiltinUpgradeMakesOneVersion(t *testing.T) {
	db := storetest.Open(t)
	v1 := map[string][]byte{"alpha/SKILL.md": []byte(skillMD("alpha", "Alpha skill.", "metadata:\n  toolyard-runs: anywhere", "# Alpha\n"))}
	v2 := map[string][]byte{
		"alpha/SKILL.md":          []byte(skillMD("alpha", "Alpha skill, revised.", "metadata:\n  toolyard-runs: anywhere", "# Alpha\nMore.\n")),
		"beta/SKILL.md":           []byte(skillMD("beta", "Beta skill.", "", "# Beta\n")),
		"beta/references/more.md": []byte("details\n"),
	}
	a := newBuiltinService(t, db, nil, v1)
	b := newBuiltinService(t, db, nil, v1)
	ctx := context.Background()
	var wg sync.WaitGroup
	for _, svc := range []*Service{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			startService(t, svc)
		}()
	}
	wg.Wait()
	if n := versionCount(t, db); n != 1 {
		t.Fatalf("two replicas starting together made %d versions", n)
	}

	setBuiltinFiles(a, v2)
	setBuiltinFiles(b, v2)
	results := make([]*SyncResult, 2)
	for i, svc := range []*Service{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := svc.ensureBuiltin(ctx)
			if err != nil {
				t.Errorf("install: %v", err)
			}
			results[i] = res
		}()
	}
	wg.Wait()
	if n := versionCount(t, db); n != 2 {
		t.Fatalf("an upgrade on two replicas made %d versions in all (want 2)", n)
	}
	var made *SyncResult
	for _, r := range results {
		if r != nil && r.Diff != nil {
			made = r
		}
	}
	if made == nil || strings.Join(made.Diff.Added, ",") != "beta" || strings.Join(made.Diff.Changed, ",") != "alpha" {
		t.Fatalf("upgrade results %+v %+v", results[0], results[1])
	}
	src, _ := a.GetSource(ctx, BuiltinSourceID)
	if src.ActiveSHA != builtin.Digest(v2) || src.SkillCount != 2 {
		t.Fatalf("after the upgrade: %+v", src)
	}
	if f, err := b.Read(ctx, memberViewer, "skill://toolyard/beta/references/more.md", 0, 0); err != nil || f.Content != "details\n" {
		t.Fatalf("read the new skill on the other replica: %+v %v", f, err)
	}
	if res, err := a.ensureBuiltin(ctx); err != nil || !res.Unchanged || versionCount(t, db) != 2 {
		t.Fatalf("unchanged skills: %+v %v, %d versions", res, err, versionCount(t, db))
	}

	// A downgrade reuses the older version.
	setBuiltinFiles(a, v1)
	if res, err := a.ensureBuiltin(ctx); err != nil || !res.Activated || res.Diff != nil || versionCount(t, db) != 2 {
		t.Fatalf("downgrade: %+v %v, %d versions", res, err, versionCount(t, db))
	}
	if src, _ := a.GetSource(ctx, BuiltinSourceID); src.ActiveSHA != builtin.Digest(v1) {
		t.Fatalf("after the downgrade: %+v", src)
	}
}

// Two replicas with a real cluster (one Postgres database) make one
// version at start and one per upgrade, and each serves the other's.
func TestBuiltinTwoNodeUpgrade(t *testing.T) {
	db1, db2 := storetest.OpenPostgresPair(t)
	v1 := map[string][]byte{"alpha/SKILL.md": []byte(skillMD("alpha", "Alpha skill.", "", "# Alpha\n"))}
	v2 := map[string][]byte{"alpha/SKILL.md": []byte(skillMD("alpha", "Alpha skill.", "", "# Alpha\nv2\n"))}
	node := func(db *store.DB, id string) *Service {
		ctx, cancel := context.WithCancel(context.Background())
		c, err := cluster.New(ctx, db, cluster.Options{InstanceID: id, ResyncInterval: -1})
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		svc := newBuiltinService(t, db, c, v1)
		t.Cleanup(func() {
			cancel()
			svc.Wait()
			_ = c.Close()
		})
		return svc
	}
	n1, n2 := node(db1, "node-1"), node(db2, "node-2")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	var wg sync.WaitGroup
	for _, svc := range []*Service{n1, n2} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := svc.Start(ctx); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := versionCount(t, db1); n != 1 {
		t.Fatalf("two nodes starting together made %d versions", n)
	}
	commitOn := func(svc *Service) string {
		got, _ := svc.Sources(context.Background(), memberViewer)
		if len(got) != 1 {
			return ""
		}
		return got[0].Commit
	}
	waitFor := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !ok() {
			if time.Now().After(deadline) {
				t.Fatal(what)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	waitFor("node 2 does not serve the built-in source", func() bool { return commitOn(n2) == builtin.Digest(v1) })

	setBuiltinFiles(n1, v2)
	setBuiltinFiles(n2, v2)
	for _, svc := range []*Service{n1, n2} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := svc.checkBuiltin(ctx); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := versionCount(t, db1); n != 2 {
		t.Fatalf("an upgrade on two nodes made %d versions in all (want 2)", n)
	}
	waitFor("node 1 does not serve the upgrade", func() bool { return commitOn(n1) == builtin.Digest(v2) })
	waitFor("node 2 does not serve the upgrade", func() bool { return commitOn(n2) == builtin.Digest(v2) })
}

// Admins change only who sees the built-in source and where its skills
// run; everything else is refused, and no agent or admin source can take
// its slug.
func TestBuiltinSourceIsReadOnly(t *testing.T) {
	db := storetest.Open(t)
	svc := newBuiltinService(t, db, nil, nil)
	startService(t, svc)
	ctx := context.Background()
	str := func(s string) *string { return &s }
	mode := func(m UpdateMode) *UpdateMode { return &m }
	no := false
	poll := 60
	for name, up := range map[string]SourceUpdate{
		"branch":  {Branch: str("main")},
		"folder":  {Path: str("skills")},
		"mode":    {Mode: mode(ModeReview)},
		"pin":     {PinnedSHA: str(strings.Repeat("a", 40))},
		"token":   {Token: str("a-read-token")},
		"clear":   {ClearToken: true},
		"exclude": {Exclude: &[]string{"*.md"}},
		"poll":    {PollSeconds: &poll},
		"limits":  {Limits: &Limits{MaxSkills: 3}},
		"pause":   {Enabled: &no},
	} {
		if _, err := svc.UpdateSource(ctx, "usr_admin", BuiltinSourceID, up); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "built-in") {
			t.Errorf("update %s: %v (want a conflict)", name, err)
		}
	}
	// Sharing: hide it from members, then show it again. Unchanged
	// fields sent along are fine.
	members := VisibleMembers
	if _, err := svc.UpdateSource(ctx, "usr_admin", BuiltinSourceID, SourceUpdate{Visibility: &members, Members: &[]string{"usr_other"}, Mode: mode(ModeAuto)}); err != nil {
		t.Fatal(err)
	}
	if got, _ := svc.Sources(ctx, memberViewer); len(got) != 0 {
		t.Fatalf("a hidden built-in source is still listed for a member: %+v", got)
	}
	if got, _ := svc.Sources(ctx, adminViewer); len(got) != 1 {
		t.Fatalf("admins always see it: %+v", got)
	}
	if _, err := svc.Get(ctx, memberViewer, svc.BuiltinGuide(), false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a hidden built-in skill must read as not found: %v", err)
	}
	everyone := VisibleEveryone
	if _, err := svc.UpdateSource(ctx, "usr_admin", BuiltinSourceID, SourceUpdate{Visibility: &everyone}); err != nil {
		t.Fatal(err)
	}
	// Where a skill runs: an override.
	if err := svc.SetRuns(ctx, "usr_admin", BuiltinSourceID, BuiltinGuideSkill, RunsHost); err != nil {
		t.Fatal(err)
	}
	if hits, _ := svc.Search(ctx, memberViewer, SearchQuery{Text: "toolyard basics"}); len(hits) > 0 && hits[0].Name == BuiltinGuideSkill {
		t.Fatalf("a host-only override still lists the guide for a member off the host: %+v", hits)
	}
	if err := svc.SetRuns(ctx, "usr_admin", BuiltinSourceID, BuiltinGuideSkill, ""); err != nil {
		t.Fatal(err)
	}

	if err := svc.DeleteSource(ctx, "usr_admin", BuiltinSourceID); !errors.Is(err, ErrConflict) {
		t.Errorf("delete: %v", err)
	}
	if _, err := svc.RotateWebhookSecret(ctx, "usr_admin", BuiltinSourceID); !errors.Is(err, ErrConflict) {
		t.Errorf("webhook secret: %v", err)
	}
	src, _ := svc.GetSource(ctx, BuiltinSourceID)
	if err := svc.Activate(ctx, "usr_admin", BuiltinSourceID, src.ActiveVersionID); !errors.Is(err, ErrConflict) {
		t.Errorf("activate: %v", err)
	}
	if err := svc.Reject(ctx, "usr_admin", BuiltinSourceID, src.ActiveVersionID, ""); !errors.Is(err, ErrConflict) {
		t.Errorf("reject: %v", err)
	}
	if err := svc.HandleWebhook(ctx, BuiltinSourceID, "push", []byte(`{}`), "sha256=00"); !errors.Is(err, ErrBadSignature) {
		t.Errorf("webhook: %v", err)
	}
	if _, err := svc.GetSource(ctx, BuiltinSourceID); err != nil {
		t.Fatalf("the built-in source is gone: %v", err)
	}
}

// No source may take the reserved slug: a given slug is refused, and a
// repository named toolyard gets the next free default slug.
func TestBuiltinSlugIsReserved(t *testing.T) {
	gh := newFakeGitHub(t, "acme", "toolyard")
	gh.commit("main", baseTree())
	svc := newTestService(t, gh)
	ctx := context.Background()
	if _, err := svc.CreateSource(ctx, "usr_admin", SourceInput{URL: "acme/toolyard", Path: "skills", Slug: BuiltinSlug}); !errors.Is(err, ErrConflict) {
		t.Fatalf("slug toolyard: %v", err)
	}
	src, err := svc.CreateSource(ctx, "usr_admin", SourceInput{URL: "acme/toolyard", Path: "skills"})
	if err != nil || src.Slug != "toolyard-2" {
		t.Fatalf("default slug of a repository named toolyard: %+v %v", src, err)
	}
	if p, err := svc.Preview(ctx, "usr_admin", SourceInput{URL: "acme/toolyard", Path: "skills"}); err != nil || p.Source.Slug == BuiltinSlug {
		t.Fatalf("preview slug: %+v %v", p, err)
	}
}

// A source that already holds the slug (made before the slug was
// reserved) keeps it; the built-in source is not installed and says why.
func TestBuiltinSlugTakenBeforehand(t *testing.T) {
	db := storetest.Open(t)
	svc := newBuiltinService(t, db, nil, nil)
	old := &sourceRow{Source: Source{ID: "src_old", Slug: BuiltinSlug, Kind: KindGitHub, Owner: "acme", Repo: "toolyard", Branch: "main",
		Mode: ModeAuto, Auth: AuthNone, PollSeconds: DefaultPollSeconds, Enabled: false, Visibility: VisibleEveryone, DefaultRuns: RunsAnywhere, CreatedBy: "usr_admin"}}
	if err := svc.insertSource(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	startService(t, svc)
	if _, err := svc.GetSource(context.Background(), BuiltinSourceID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the built-in source was installed over a taken slug: %v", err)
	}
	if _, err := svc.ensureBuiltin(context.Background()); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("install over a taken slug: %v", err)
	}
	if src, err := svc.GetSource(context.Background(), "src_old"); err != nil || src.Slug != BuiltinSlug {
		t.Fatalf("the older source changed: %+v %v", src, err)
	}
}
