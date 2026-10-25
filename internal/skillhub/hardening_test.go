package skillhub

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// waitCount waits until the fake GitHub counted at least n calls of name.
func waitCount(t *testing.T, gh *fakeGitHub, name string, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for gh.count(name) < n {
		if time.Now().After(deadline) {
			t.Fatalf("%s calls: %d, want %d", name, gh.count(name), n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func warningCodes(ws []Warning) map[string]bool {
	out := map[string]bool{}
	for _, w := range ws {
		out[w.Code] = true
	}
	return out
}

// Content Postgres text refuses (a NUL byte from a YAML escape, a path
// that is not UTF-8) is skipped or left out with a warning, and the
// version is stored on both engines; a backslash outside the source folder
// does not fail the source.
func TestUnstorableContentIsLeftOut(t *testing.T) {
	gh := newFakeGitHub(t, "acme", "skills")
	tree := baseTree()
	tree["skills/nul-desc/SKILL.md"] = "---\nname: nul-desc\ndescription: \"bad\\0value\"\n---\nbody\n"
	tree["skills/pr-review/caf\xe9.md"] = "not a UTF-8 name\n"
	tree["skills/pr-review/back\\slash.md"] = "backslash in the name\n"
	tree["docs\\windows.txt"] = "outside the source folder\n"
	gh.commit("main", tree)
	svc := newTestService(t, gh)
	ctx := context.Background()
	src, res := mustCreate(t, svc, SourceInput{URL: "acme/skills", Path: "skills", Mode: ModeAuto})
	if !res.Activated {
		t.Fatalf("sync %+v", res)
	}
	v, err := svc.GetVersion(ctx, src.ID, res.VersionID)
	if err != nil {
		t.Fatal(err)
	}
	codes := warningCodes(v.Warnings)
	if !codes["invalid_text"] || !codes["invalid_path"] {
		t.Fatalf("warnings %+v", v.Warnings)
	}
	if _, err := svc.Get(ctx, adminViewer, "skills/nul-desc", false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a skill with a NUL description is served: %v", err)
	}
	sk, err := svc.Get(ctx, adminViewer, "skills/pr-review", true)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range sk.Files {
		if strings.Contains(f.Path, "slash") || strings.Contains(f.Path, "caf") {
			t.Fatalf("an unstorable path is served: %q", f.Path)
		}
	}
	if len(sk.Omitted) < 2 {
		t.Fatalf("omitted %+v", sk.Omitted)
	}
}

// A changed file that became a symbolic link is not read through the
// contents API (GitHub answers with the link target's content): the update
// downloads the archive, where the link is left out.
func TestIncrementalUpdateDoesNotFollowLinks(t *testing.T) {
	gh := newFakeGitHub(t, "acme", "skills")
	gh.commit("main", baseTree())
	svc := newTestService(t, gh)
	ctx := context.Background()
	src, _ := mustCreate(t, svc, SourceInput{URL: "acme/skills", Path: "skills", Mode: ModeAuto})
	gh.commit("main", map[string]string{"skills/pr-review/checks.md": symlinkPrefix + "../../README.md"})
	res, err := svc.SyncNow(ctx, src.ID)
	if err != nil || !res.Activated || !res.Full {
		t.Fatalf("sync %+v %v", res, err)
	}
	if gh.count("contents") != 1 || gh.count("tarball") != 2 {
		t.Fatalf("contents %d tarball %d", gh.count("contents"), gh.count("tarball"))
	}
	if _, err := svc.Read(ctx, adminViewer, "skill://skills/pr-review/checks.md", 0, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the link is served: %v", err)
	}
	// A .gitattributes change always downloads the archive.
	gh.commit("main", map[string]string{".gitattributes": "*.md export-ignore\n", "skills/deploy/scripts/run.sh": "echo 2\n"})
	if res, err := svc.SyncNow(ctx, src.ID); err != nil || !res.Full {
		t.Fatalf("after .gitattributes %+v %v", res, err)
	}
}

func TestIndexKeepsNewestSnapshotAndSlugOwner(t *testing.T) {
	ix := newIndex()
	t0 := time.Unix(1000, 0)
	snap := func(id string, created, updated time.Time, version string) *srcIndex {
		return &srcIndex{src: Source{ID: id, Slug: "kit", CreatedAt: created, UpdatedAt: updated}, versionID: version}
	}
	// Source a held slug "kit", was deleted, and b took the slug; this
	// node hears b first, then a stale reload of a, then a's delete.
	ix.put("b", snap("b", t0.Add(time.Hour), t0.Add(time.Hour), "vb"))
	ix.put("a", snap("a", t0, t0, "va"))
	if got := ix.slug("kit"); got == nil || got.src.ID != "b" {
		t.Fatalf("slug owner %+v", got)
	}
	ix.put("a", nil)
	if got := ix.slug("kit"); got == nil || got.src.ID != "b" {
		t.Fatalf("after a's delete the slug owner is %+v", got)
	}
	// A reload of a that read it before the delete cannot bring it back.
	if changed := ix.put("a", snap("a", t0, t0.Add(time.Minute), "va")); changed != nil || ix.get("a") != nil {
		t.Fatal("a deleted source came back")
	}
	// An older snapshot of b never replaces a newer one.
	ix.put("b", snap("b", t0.Add(time.Hour), t0.Add(2*time.Hour), "vb2"))
	if changed := ix.put("b", snap("b", t0.Add(time.Hour), t0.Add(90*time.Minute), "vb1")); changed != nil {
		t.Fatalf("older snapshot reported %v", changed)
	}
	if got := ix.get("b"); got.versionID != "vb2" {
		t.Fatalf("index regressed to %q", got.versionID)
	}
}

// A force-push back to an older commit makes its retired version live
// again; a cached head read right after an activation does not flap.
func TestForcePushBackToRetiredCommit(t *testing.T) {
	gh := newFakeGitHub(t, "acme", "skills")
	c1 := gh.commit("main", baseTree())
	svc := newTestService(t, gh)
	ctx := context.Background()
	src, _ := mustCreate(t, svc, SourceInput{URL: "acme/skills", Path: "skills", Mode: ModeAuto})
	c2 := gh.commit("main", map[string]string{"skills/pr-review/checks.md": "bad change\n"})
	if res, err := svc.SyncNow(ctx, src.ID); err != nil || !res.Activated || res.SHA != c2 {
		t.Fatalf("c2 sync %+v %v", res, err)
	}
	commit := func() string {
		got, _ := svc.Sources(ctx, adminViewer)
		return got[0].Commit
	}
	// A poll that reads a stale cached head (c1) right after c2 went live
	// keeps c2.
	gh.mu.Lock()
	gh.cachedHead["main"] = c1
	gh.mu.Unlock()
	if _, err := svc.db.Exec(`UPDATE skill_sources SET next_check_at = 0 WHERE id = ?`, src.ID); err != nil {
		t.Fatal(err)
	}
	if res, err := svc.syncSource(ctx, src.ID, syncRequest{tr: triggerPoll}); err != nil || !res.Unchanged || commit() != c2 {
		t.Fatalf("stale poll %+v %v, serving %s", res, err, commit())
	}
	// The branch really moves back to c1: "Sync now" serves it again.
	gh.mu.Lock()
	delete(gh.cachedHead, "main")
	gh.mu.Unlock()
	gh.setBranch("main", c1)
	res, err := svc.SyncNow(ctx, src.ID)
	if err != nil || !res.Activated || res.SHA != c1 || commit() != c1 {
		t.Fatalf("force-push sync %+v %v, serving %s", res, err, commit())
	}
	if gh.count("tarball") != 1 {
		t.Fatalf("the retired version was rebuilt: %d downloads", gh.count("tarball"))
	}
}

// An admin's edit during a sync wins: the sync's version (built with the
// old settings) is not made live, the check stays due, and the next sync
// builds with the new settings.
func TestUpdateSourceDuringSync(t *testing.T) {
	gh := newFakeGitHub(t, "acme", "skills")
	c1 := gh.commit("main", baseTree())
	svc := newTestService(t, gh)
	ctx := context.Background()
	src, _ := mustCreate(t, svc, SourceInput{URL: "acme/skills", Path: "skills", Mode: ModeAuto})
	c2 := gh.commit("main", map[string]string{
		"skills/new-skill/SKILL.md":    skillMD("new-skill", "New.", "", ""),
		"skills/new-skill/drafts/a.md": "draft\n",
	})
	gate := make(chan struct{})
	gh.mu.Lock()
	gh.codeloadGate = gate
	gh.mu.Unlock()
	defer func() {
		select {
		case <-gate:
		default:
			close(gate)
		}
	}()
	type result struct {
		res *SyncResult
		err error
	}
	done := make(chan result, 1)
	go func() {
		res, err := svc.SyncNow(ctx, src.ID)
		done <- result{res, err}
	}()
	waitCount(t, gh, "codeload", 2)
	exclude := []string{"drafts/"}
	if _, err := svc.UpdateSource(ctx, "usr_admin", src.ID, SourceUpdate{Exclude: &exclude}); err != nil {
		t.Fatal(err)
	}
	close(gate)
	r := <-done
	if r.err != nil || r.res.Activated {
		t.Fatalf("sync during the edit %+v %v", r.res, r.err)
	}
	got, _ := svc.Sources(ctx, adminViewer)
	if got[0].Commit != c1 {
		t.Fatalf("serving %s, want %s", got[0].Commit, c1)
	}
	s, _ := svc.GetSource(ctx, src.ID)
	if s.NextCheckAt == nil || s.NextCheckAt.After(time.Now()) || s.Exclude[0] != "drafts/" {
		t.Fatalf("source after the edit %+v", s)
	}
	versions, _ := svc.Versions(ctx, src.ID, 10)
	if len(versions) != 1 {
		t.Fatalf("the stale version was kept: %+v", versions)
	}
	res, err := svc.SyncNow(ctx, src.ID)
	if err != nil || !res.Activated || res.SHA != c2 {
		t.Fatalf("next sync %+v %v", res, err)
	}
	if _, err := svc.Read(ctx, adminViewer, "skill://skills/new-skill/drafts/a.md", 0, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the new exclude was not applied: %v", err)
	}
}

func TestOmittedFilesAreCapped(t *testing.T) {
	tree := map[string]string{"s/SKILL.md": skillMD("s", "S.", "", "")}
	for i := 0; i < maxOmittedKept+5; i++ {
		tree["s/bin/"+strings.Repeat("x", 3)+itoa(i)] = "\x00"
	}
	data := buildTarball(t, "top", tree)
	fs, err := readArchive(bytes.NewReader(data), "", nil, Limits{}.effective())
	if err != nil {
		t.Fatal(err)
	}
	if len(fs.omitted) != maxOmittedKept || fs.dropped != 5 || fs.entries != maxOmittedKept+6 {
		t.Fatalf("omitted %d dropped %d entries %d", len(fs.omitted), fs.dropped, fs.entries)
	}
	b, err := discover(fs, "", "top", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !warningCodes(b.warnings)["too_many_warnings"] {
		t.Fatal("the cut list is not reported")
	}
	// Every file entry counts against the file limit, kept or not.
	if _, err := readArchive(bytes.NewReader(data), "", nil, Limits{MaxFiles: 100}.effective()); !errors.Is(err, ErrLimit) {
		t.Fatalf("file limit over omitted files: %v", err)
	}
	// Bytes the secret check reads count against the byte limit.
	secret := "sk" + "-ant-" + strings.Repeat("k", 40) + "\n"
	big := map[string]string{"s/SKILL.md": skillMD("s", "S.", "", "")}
	for i := 0; i < 50; i++ {
		big["s/key"+itoa(i)+".txt"] = strings.Repeat(secret, 20)
	}
	if _, err := readArchive(bytes.NewReader(buildTarball(t, "top", big)), "", nil, Limits{MaxTotalBytes: 20000}.effective()); !errors.Is(err, ErrLimit) {
		t.Fatalf("byte limit over secret files: %v", err)
	}
}

func itoa(i int) string {
	var b []byte
	for {
		b = append([]byte{byte('0' + i%10)}, b...)
		if i /= 10; i == 0 {
			return string(b)
		}
	}
}

// Only the staged version waiting for review can be approved; a review
// hook call that failed is retried at the next check.
func TestActivateOnlyThePendingStagedVersion(t *testing.T) {
	gh := newFakeGitHub(t, "outside", "kit")
	gh.commit("main", baseTree())
	svc := newTestService(t, gh)
	hook := &recordingHook{failures: 1}
	svc.SetReviewHook(hook)
	ctx := context.Background()
	src, res := mustCreate(t, svc, SourceInput{URL: "outside/kit", Path: "skills"})
	if !res.Staged || len(hook.versions) != 0 {
		t.Fatalf("first stage %+v, hook %+v", res, hook.versions)
	}
	v1 := res.VersionID
	// The next check calls the hook again; it succeeds this time.
	if res, err := svc.SyncNow(ctx, src.ID); err != nil || !res.Unchanged || len(hook.versions) != 1 || hook.versions[0].ID != v1 {
		t.Fatalf("hook retry %+v %v, hook %+v", res, err, hook.versions)
	}
	gh.commit("main", map[string]string{"skills/pr-review/checks.md": "v2\n"})
	res, err := svc.SyncNow(ctx, src.ID)
	if err != nil || !res.Staged {
		t.Fatalf("second stage %+v %v", res, err)
	}
	if err := svc.Activate(ctx, "usr_admin", src.ID, v1); !errors.Is(err, ErrConflict) {
		t.Fatalf("activating a staged version that is not pending: %v", err)
	}
	if err := svc.Activate(ctx, "usr_admin", src.ID, res.VersionID); err != nil {
		t.Fatal(err)
	}
}

// A sync stopped because the leader lease (or the gateway) went away does
// not count as a failure of the source.
func TestCancelledSyncIsNotAFailure(t *testing.T) {
	gh := newFakeGitHub(t, "acme", "skills")
	gh.commit("main", baseTree())
	svc := newTestService(t, gh)
	src, err := svc.CreateSource(context.Background(), "usr_admin", SourceInput{URL: "acme/skills", Path: "skills", Mode: ModeAuto})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := svc.GetSource(context.Background(), src.ID)
	gate := make(chan struct{})
	gh.mu.Lock()
	gh.codeloadGate = gate
	gh.mu.Unlock()
	defer close(gate)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := svc.syncSource(ctx, src.ID, syncRequest{tr: triggerPoll})
		done <- err
	}()
	waitCount(t, gh, "codeload", 1)
	cancel()
	if err := <-done; err == nil {
		t.Fatal("the cancelled sync reported success")
	}
	after, _ := svc.GetSource(context.Background(), src.ID)
	var failures int
	if err := svc.db.QueryRow(`SELECT failures FROM skill_sources WHERE id = ?`, src.ID).Scan(&failures); err != nil {
		t.Fatal(err)
	}
	if after.LastError != "" || failures != 0 || after.NextCheckAt == nil || !after.NextCheckAt.Equal(*before.NextCheckAt) {
		t.Fatalf("source after the cancel %+v failures %d", after, failures)
	}
}

// A signed push names the commit to serve: the sync serves it without
// asking for the branch head, which GitHub may answer from a stale cache.
// "Sync now" asks for the head past that cache.
func TestPushServesThePushedCommit(t *testing.T) {
	gh := newFakeGitHub(t, "acme", "skills")
	c1 := gh.commit("main", baseTree())
	svc := newTestService(t, gh)
	ctx := context.Background()
	src, _ := mustCreate(t, svc, SourceInput{URL: "acme/skills", Path: "skills", Mode: ModeAuto})
	secret, err := svc.RotateWebhookSecret(ctx, "usr_admin", src.ID)
	if err != nil {
		t.Fatal(err)
	}
	gh.mu.Lock()
	gh.cachedHead["main"] = c1 // GitHub's cache still says c1
	gh.mu.Unlock()
	c2 := gh.commit("main", map[string]string{"skills/pr-review/checks.md": "pushed\n"})
	heads := gh.count("head") + gh.count("head304")
	push := []byte(`{"ref":"refs/heads/main","after":"` + c2 + `"}`)
	if err := svc.HandleWebhook(ctx, src.ID, "push", push, sign(secret, push)); err != nil {
		t.Fatal(err)
	}
	svc.Wait()
	if got, _ := svc.Sources(ctx, adminViewer); got[0].Commit != c2 {
		t.Fatalf("after the push serving %s, want %s", got[0].Commit, c2)
	}
	if gh.count("head")+gh.count("head304") != heads {
		t.Fatal("the push sync asked for the branch head")
	}
	// A branch delete (after all zeros) is ignored.
	del := []byte(`{"ref":"refs/heads/main","after":"` + strings.Repeat("0", 40) + `","deleted":true}`)
	if err := svc.HandleWebhook(ctx, src.ID, "push", del, sign(secret, del)); err != nil {
		t.Fatal(err)
	}
	svc.Wait()
	// "Sync now" sees a new commit at once, whatever the cache says.
	c3 := gh.commit("main", map[string]string{"skills/pr-review/checks.md": "third\n"})
	if res, err := svc.SyncNow(ctx, src.ID); err != nil || !res.Activated || res.SHA != c3 {
		t.Fatalf("sync now %+v %v", res, err)
	}
	// A body over the limit is refused before any signature work.
	if err := svc.HandleWebhook(ctx, src.ID, "push", make([]byte, maxWebhookBody+1), ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("oversized body: %v", err)
	}
}

// API calls follow a renamed repository's redirect within the API host.
func TestRenamedRepositoryRedirect(t *testing.T) {
	gh := newFakeGitHub(t, "acme", "skills")
	gh.renamedFrom = "old-skills"
	gh.commit("main", baseTree())
	svc := newTestService(t, gh)
	info, err := svc.Inspect(context.Background(), "acme/old-skills", "")
	if err != nil || info.Repo != "skills" {
		t.Fatalf("inspect %+v %v", info, err)
	}
	src, res := mustCreate(t, svc, SourceInput{URL: "acme/old-skills", Path: "skills", Mode: ModeAuto})
	if src.Repo != "skills" || !res.Activated {
		t.Fatalf("source %+v result %+v", src, res)
	}
}

func TestBranchNames(t *testing.T) {
	for b, want := range map[string]bool{
		"main": true, "feature/x": true, "release-1.2": true,
		"": false, "a..b": false, "../main": false, "a b": false, "a\x01": false, "/main": false,
		"main/": false, "a//b": false, "x.lock": false, "a@{1}": false, "-x": false, ".hidden": false, "a/.b": false,
	} {
		if got := validBranch(b); got != want {
			t.Errorf("validBranch(%q) = %v, want %v", b, got, want)
		}
	}
	gh := newFakeGitHub(t, "acme", "skills")
	gh.commit("main", baseTree())
	svc := newTestService(t, gh)
	if _, err := svc.CreateSource(context.Background(), "usr_admin", SourceInput{URL: "acme/skills", Branch: "main/../x"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a branch with .. was accepted: %v", err)
	}
}

func TestSecretAfterManyExamples(t *testing.T) {
	example := "AK" + "IA" + "IOSFODNN7EXAMPLE "
	real := "AK" + "IA" + "QWERTYUIOPASDFGH"
	if got := findSecret([]byte(strings.Repeat(example, 9) + real)); got != "an AWS access key" {
		t.Fatalf("got %q", got)
	}
	if got := findSecret([]byte(strings.Repeat(example, 9))); got != "" {
		t.Fatalf("examples flagged as %q", got)
	}
}

// An old or out-of-order push delivery (GitHub's "Redeliver") does not
// roll agents back: its commit does not descend from the active one, so
// the sync reads the branch head instead. A force-push to a new commit
// still goes live through that head read.
func TestOldPushDeliveryDoesNotRollBack(t *testing.T) {
	gh := newFakeGitHub(t, "acme", "skills")
	c1 := gh.commit("main", baseTree())
	svc := newTestService(t, gh)
	ctx := context.Background()
	src, _ := mustCreate(t, svc, SourceInput{URL: "acme/skills", Path: "skills", Mode: ModeAuto})
	secret, err := svc.RotateWebhookSecret(ctx, "usr_admin", src.ID)
	if err != nil {
		t.Fatal(err)
	}
	push := func(after string) {
		t.Helper()
		body := []byte(`{"ref":"refs/heads/main","after":"` + after + `"}`)
		if err := svc.HandleWebhook(ctx, src.ID, "push", body, sign(secret, body)); err != nil {
			t.Fatal(err)
		}
		svc.Wait()
	}
	serving := func() string {
		got, _ := svc.Sources(ctx, adminViewer)
		return got[0].Commit
	}
	c2 := gh.commit("main", map[string]string{"skills/pr-review/checks.md": "v2\n"})
	push(c2)
	if serving() != c2 {
		t.Fatalf("serving %s after the push of %s", serving(), c2)
	}
	// The push of c1 delivered again: c1 is retired, and an older commit.
	heads := gh.count("head")
	push(c1)
	if serving() != c2 {
		t.Fatalf("an old delivery rolled back to %s", serving())
	}
	if gh.count("head") != heads+1 {
		t.Fatalf("the old delivery did not read the branch head (%d reads)", gh.count("head")-heads)
	}
	// A force-push to a new commit that does not descend from c2.
	gh.setBranch("main", c1)
	c3 := gh.commit("main", map[string]string{"skills/pr-review/checks.md": "rewritten\n"})
	push(c3)
	if serving() != c3 {
		t.Fatalf("a force-push to %s serves %s", c3, serving())
	}
}
