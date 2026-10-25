package skillhub

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/settings"
)

// baseTree is a repository with two skills under skills/ and a README.
func baseTree() map[string]string {
	return map[string]string{
		"README.md":                    "# repo\n",
		"skills/pr-review/SKILL.md":    skillMD("pr-review", "Review a pull request.", "", "# Review\nRead the diff.\n"),
		"skills/pr-review/checks.md":   "1. tests\n2. docs\n",
		"skills/deploy/SKILL.md":       skillMD("deploy", "Deploy the service.", "metadata:\n  toolyard-runs: host", "Run tools/deploy.sh\n"),
		"skills/deploy/scripts/run.sh": "#!/bin/sh\necho deploy\n",
	}
}

func TestSyncAutoFullThenIncremental(t *testing.T) {
	gh := newFakeGitHub(t, "acme", "skills")
	c1 := gh.commit("main", baseTree())
	svc := newTestService(t, gh)
	ctx := context.Background()

	src, res := mustCreate(t, svc, SourceInput{URL: "https://github.com/acme/skills/tree/main/skills", Mode: ModeAuto})
	if src.Branch != "main" || src.Path != "skills" || src.Slug != "skills" || src.Mode != ModeAuto {
		t.Fatalf("source %+v", src)
	}
	if !res.Activated || !res.Full || res.SHA != c1 || res.Diff == nil || len(res.Diff.Added) != 2 {
		t.Fatalf("first sync %+v diff %+v", res, res.Diff)
	}
	if gh.count("tarball") != 1 || gh.count("codeload") != 1 {
		t.Fatalf("downloads: tarball %d codeload %d", gh.count("tarball"), gh.count("codeload"))
	}

	// An unchanged branch: the ETag check answers 304 and nothing is fetched.
	res, err := svc.SyncNow(ctx, src.ID)
	if err != nil || !res.Unchanged {
		t.Fatalf("second sync %+v %v", res, err)
	}
	if gh.count("head304") != 1 {
		t.Fatalf("304 answers: %d", gh.count("head304"))
	}

	// A supporting file changes: an incremental update fetches one file.
	c2 := gh.commit("main", map[string]string{"skills/pr-review/checks.md": "1. tests\n2. docs\n3. changelog\n"})
	res, err = svc.SyncNow(ctx, src.ID)
	if err != nil || !res.Activated || res.Full || res.SHA != c2 {
		t.Fatalf("incremental sync %+v %v", res, err)
	}
	if gh.count("tarball") != 1 || gh.count("contents") != 1 || gh.count("compare") != 1 {
		t.Fatalf("calls: tarball %d contents %d compare %d", gh.count("tarball"), gh.count("contents"), gh.count("compare"))
	}
	if strings.Join(res.Diff.Changed, ",") != "pr-review" || len(res.Diff.Added)+len(res.Diff.Removed) != 0 {
		t.Fatalf("diff %+v", res.Diff)
	}
	f, err := svc.Read(ctx, adminViewer, "skill://skills/pr-review/checks.md", 0, 0)
	if err != nil || !strings.Contains(f.Content, "changelog") || f.Commit != c2 {
		t.Fatalf("read after incremental: %+v %v", f, err)
	}
	// The unchanged skill still reads (its blobs were not copied again).
	if sk, err := svc.Get(ctx, adminViewer, "skills/deploy", true); err != nil || !strings.Contains(sk.Body, "deploy.sh") || len(sk.Files) != 2 {
		t.Fatalf("get after incremental: %+v %v", sk, err)
	}

	// A new skill needs a full download.
	c3 := gh.commit("main", map[string]string{"skills/new-skill/SKILL.md": skillMD("new-skill", "Fresh.", "", "")})
	res, err = svc.SyncNow(ctx, src.ID)
	if err != nil || !res.Activated || !res.Full || res.SHA != c3 || strings.Join(res.Diff.Added, ",") != "new-skill" {
		t.Fatalf("full sync %+v %v", res, err)
	}
	if gh.count("tarball") != 2 {
		t.Fatalf("tarball calls %d", gh.count("tarball"))
	}

	// A skill removed: full download, and the diff says so.
	gh.commit("main", nil, "skills/deploy/SKILL.md", "skills/deploy/scripts/run.sh")
	res, err = svc.SyncNow(ctx, src.ID)
	if err != nil || !res.Full || strings.Join(res.Diff.Removed, ",") != "deploy" {
		t.Fatalf("removal sync %+v %v", res, err)
	}
	if _, err := svc.Get(ctx, adminViewer, "skills/deploy", false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("removed skill still served: %v", err)
	}
}

// recordingHook records staged versions.
type recordingHook struct {
	mu       sync.Mutex
	versions []Version
}

func (h *recordingHook) PendingVersion(_ context.Context, src Source, v Version) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if src.PendingVersionID != v.ID {
		return errors.New("source does not name the pending version")
	}
	h.versions = append(h.versions, v)
	return nil
}

func TestSyncReviewModeAndRollback(t *testing.T) {
	gh := newFakeGitHub(t, "outside", "kit")
	c1 := gh.commit("main", baseTree())
	svc := newTestService(t, gh)
	hook := &recordingHook{}
	svc.SetReviewHook(hook)
	ctx := context.Background()

	// An untrusted owner defaults to review.
	src, res := mustCreate(t, svc, SourceInput{URL: "outside/kit", Path: "skills"})
	if src.Mode != ModeReview || !res.Staged || res.Activated {
		t.Fatalf("source %+v result %+v", src, res)
	}
	if len(hook.versions) != 1 || hook.versions[0].SHA != c1 {
		t.Fatalf("hook calls %+v", hook.versions)
	}
	if got, _ := svc.Sources(ctx, adminViewer); len(got) != 0 {
		t.Fatalf("a staged version is served: %+v", got)
	}
	got, _ := svc.GetSource(ctx, src.ID)
	if got.PendingVersionID != res.VersionID || got.ActiveVersionID != "" {
		t.Fatalf("source after staging %+v", got)
	}
	// The same commit again does not stage twice.
	if res2, err := svc.SyncNow(ctx, src.ID); err != nil || !res2.Unchanged || len(hook.versions) != 1 {
		t.Fatalf("resync %+v %v hook %d", res2, err, len(hook.versions))
	}
	if err := svc.Activate(ctx, "usr_admin", src.ID, res.VersionID); err != nil {
		t.Fatal(err)
	}
	if got, _ := svc.Sources(ctx, adminViewer); len(got) != 1 || got[0].Commit != c1 {
		t.Fatalf("after approval %+v", got)
	}
	v1 := res.VersionID

	// A new commit is staged; a rejection keeps the old version live.
	gh.commit("main", map[string]string{"skills/pr-review/SKILL.md": skillMD("pr-review", "Review it better.", "", "")})
	res, err := svc.SyncNow(ctx, src.ID)
	if err != nil || !res.Staged {
		t.Fatalf("second staged %+v %v", res, err)
	}
	if err := svc.Reject(ctx, "usr_admin", src.ID, res.VersionID, "not now"); err != nil {
		t.Fatal(err)
	}
	if err := svc.Activate(ctx, "usr_admin", src.ID, res.VersionID); !errors.Is(err, ErrConflict) {
		t.Fatalf("activating a rejected version: %v", err)
	}
	v, _ := svc.GetVersion(ctx, src.ID, res.VersionID)
	if v.Status != VersionRejected || v.Note != "not now" || v.DecidedBy != "usr_admin" {
		t.Fatalf("rejected version %+v", v)
	}
	// The rejected commit is not staged again.
	if res2, err := svc.SyncNow(ctx, src.ID); err != nil || !res2.Unchanged {
		t.Fatalf("after rejection %+v %v", res2, err)
	}

	// Switch to auto: the next commit goes live; then roll back to v1.
	mode := ModeAuto
	if _, err := svc.UpdateSource(ctx, "usr_admin", src.ID, SourceUpdate{Mode: &mode}); err != nil {
		t.Fatal(err)
	}
	c3 := gh.commit("main", map[string]string{"skills/pr-review/checks.md": "changed\n"})
	if res, err = svc.SyncNow(ctx, src.ID); err != nil || !res.Activated || res.SHA != c3 {
		t.Fatalf("auto sync %+v %v", res, err)
	}
	if err := svc.Activate(ctx, "usr_admin", src.ID, v1); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if s, _ := svc.Sources(ctx, adminViewer); s[0].Commit != c1 {
		t.Fatalf("after rollback %+v", s)
	}
	// The rollback holds until a new commit arrives.
	if res, err = svc.SyncNow(ctx, src.ID); err != nil || !res.Unchanged {
		t.Fatalf("after rollback sync %+v %v", res, err)
	}
	if s, _ := svc.Sources(ctx, adminViewer); s[0].Commit != c1 {
		t.Fatalf("rollback undone %+v", s)
	}
	versions, err := svc.Versions(ctx, src.ID, 0)
	if err != nil || len(versions) != 3 {
		t.Fatalf("versions %d %v", len(versions), err)
	}
	statuses := map[VersionStatus]int{}
	for _, v := range versions {
		statuses[v.Status]++
	}
	if statuses[VersionActive] != 1 || statuses[VersionRetired] != 1 || statuses[VersionRejected] != 1 {
		t.Fatalf("statuses %v", statuses)
	}
}

func TestSyncTrustedOwnerDefaultsToAuto(t *testing.T) {
	gh := newFakeGitHub(t, "acme", "skills")
	gh.commit("main", baseTree())
	svc := newTestService(t, gh, func(o *Options) {
		o.Settings = staticSettings{settings.SkillsTrustedGitHubOwners: " other , ACME "}
	})
	if !svc.TrustedOwner(context.Background(), "acme") || svc.TrustedOwner(context.Background(), "acm") {
		t.Fatal("trusted owner list not read")
	}
	src, res := mustCreate(t, svc, SourceInput{URL: "acme/skills"})
	if src.Mode != ModeAuto || !res.Activated {
		t.Fatalf("source %+v result %+v", src, res)
	}
}

func TestSyncPinned(t *testing.T) {
	gh := newFakeGitHub(t, "acme", "skills")
	c1 := gh.commit("main", baseTree())
	c2 := gh.commit("main", map[string]string{"skills/pr-review/checks.md": "v2\n"})
	svc := newTestService(t, gh)
	ctx := context.Background()
	if _, err := svc.CreateSource(ctx, "usr_admin", SourceInput{URL: "acme/skills", Mode: ModePinned}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("pinned without a SHA: %v", err)
	}
	src, res := mustCreate(t, svc, SourceInput{URL: "acme/skills", Path: "skills", Mode: ModePinned, PinnedSHA: c1})
	if !res.Activated || res.SHA != c1 {
		t.Fatalf("pinned sync %+v", res)
	}
	if gh.count("head")+gh.count("head304") != 0 {
		t.Fatal("a pinned source asked for the branch head")
	}
	gh.commit("main", map[string]string{"skills/pr-review/checks.md": "v3\n"})
	if res, err := svc.SyncNow(ctx, src.ID); err != nil || !res.Unchanged || res.SHA != c1 {
		t.Fatalf("pinned resync %+v %v", res, err)
	}
	// Pin to another commit.
	if _, err := svc.UpdateSource(ctx, "usr_admin", src.ID, SourceUpdate{PinnedSHA: &c2}); err != nil {
		t.Fatal(err)
	}
	if res, err := svc.SyncNow(ctx, src.ID); err != nil || !res.Activated || res.SHA != c2 {
		t.Fatalf("re-pinned %+v %v", res, err)
	}
	// Back to the first pin: its retired version is served again.
	if _, err := svc.UpdateSource(ctx, "usr_admin", src.ID, SourceUpdate{PinnedSHA: &c1}); err != nil {
		t.Fatal(err)
	}
	if res, err := svc.SyncNow(ctx, src.ID); err != nil || !res.Activated || res.SHA != c1 {
		t.Fatalf("pin back %+v %v", res, err)
	}
	// The re-pin was an incremental update; the pin back reused a version.
	if gh.count("tarball") != 1 {
		t.Fatalf("tarball downloads %d, want 1", gh.count("tarball"))
	}
}

func TestSyncRetention(t *testing.T) {
	gh := newFakeGitHub(t, "acme", "skills")
	gh.commit("main", baseTree())
	svc := newTestService(t, gh)
	ctx := context.Background()
	src, _ := mustCreate(t, svc, SourceInput{URL: "acme/skills", Path: "skills", Mode: ModeAuto})
	for i := 0; i < KeepVersions+3; i++ {
		gh.commit("main", map[string]string{"skills/pr-review/checks.md": strings.Repeat("x", i+1) + "\n"})
		if _, err := svc.SyncNow(ctx, src.ID); err != nil {
			t.Fatal(err)
		}
	}
	versions, err := svc.Versions(ctx, src.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != KeepVersions+1 {
		t.Fatalf("%d versions kept, want %d", len(versions), KeepVersions+1)
	}
	// Blobs no version references are purged once the grace passed.
	var blobs int
	if err := svc.db.QueryRow(`SELECT COUNT(*) FROM skill_blobs`).Scan(&blobs); err != nil {
		t.Fatal(err)
	}
	svc.now = func() time.Time { return time.Now().Add(2 * blobGrace) }
	if err := svc.purgeBlobs(ctx); err != nil {
		t.Fatal(err)
	}
	var after, referenced int
	if err := svc.db.QueryRow(`SELECT COUNT(*) FROM skill_blobs`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if err := svc.db.QueryRow(`SELECT COUNT(DISTINCT sha256) FROM skill_files`).Scan(&referenced); err != nil {
		t.Fatal(err)
	}
	if after != referenced || after >= blobs {
		t.Fatalf("blobs before %d after %d referenced %d", blobs, after, referenced)
	}
}

func TestSyncLimitKeepsActiveVersion(t *testing.T) {
	gh := newFakeGitHub(t, "acme", "skills")
	gh.commit("main", baseTree())
	svc := newTestService(t, gh)
	ctx := context.Background()
	src, res := mustCreate(t, svc, SourceInput{URL: "acme/skills", Path: "skills", Mode: ModeAuto, Limits: &Limits{MaxFiles: 6}})
	if !res.Activated {
		t.Fatalf("first sync %+v", res)
	}
	tree := map[string]string{}
	for i := 0; i < 5; i++ {
		tree["skills/new/f"+string(rune('a'+i))+".md"] = "x\n"
	}
	tree["skills/new/SKILL.md"] = skillMD("new", "New.", "", "")
	gh.commit("main", tree)
	res, err := svc.SyncNow(ctx, src.ID)
	if !errors.Is(err, ErrLimit) || res == nil || res.Error == "" {
		t.Fatalf("over the limit: %+v %v", res, err)
	}
	s, _ := svc.GetSource(ctx, src.ID)
	if s.LastError == "" || s.ActiveSHA == "" || s.NextCheckAt == nil || !s.NextCheckAt.After(time.Now().Add(9*time.Minute)) {
		t.Fatalf("source after failure %+v", s)
	}
	if got, _ := svc.Sources(ctx, adminViewer); len(got) != 1 || got[0].SkillCount != 2 {
		t.Fatalf("active version lost: %+v", got)
	}
	versions, _ := svc.Versions(ctx, src.ID, 10)
	if versions[0].Status != VersionFailed || !strings.Contains(versions[0].Error, "limit") {
		t.Fatalf("failed version %+v", versions[0])
	}
	// A webhook (or poll) does not download the failed commit again.
	downloads := gh.count("tarball")
	if res, err := svc.syncSource(ctx, src.ID, triggerWebhook); err == nil || !res.Unchanged {
		t.Fatalf("webhook retry %+v %v", res, err)
	}
	if gh.count("tarball") != downloads {
		t.Fatal("the failed commit was downloaded again")
	}
	limits := Limits{MaxFiles: 100}
	if _, err := svc.UpdateSource(ctx, "usr_admin", src.ID, SourceUpdate{Limits: &limits}); err != nil {
		t.Fatal(err)
	}
	if res, err := svc.SyncNow(ctx, src.ID); err != nil || !res.Activated {
		t.Fatalf("after raising the limit %+v %v", res, err)
	}
}

func TestPrivateRepositoryToken(t *testing.T) {
	gh := newFakeGitHub(t, "acme", "private-skills")
	gh.private = true
	gh.token = "test-read-token-value"
	gh.commit("main", baseTree())
	svc := newTestService(t, gh)
	ctx := context.Background()

	info, err := svc.Inspect(ctx, "acme/private-skills", "")
	if err != nil || !info.NeedsToken {
		t.Fatalf("inspect without token: %+v %v", info, err)
	}
	info, err = svc.Inspect(ctx, "https://github.com/acme/private-skills/tree/main/skills", gh.token)
	if err != nil || !info.Private || info.Branch != "main" || info.Path != "skills" || len(info.Branches) != 1 {
		t.Fatalf("inspect with token: %+v %v", info, err)
	}
	if _, err := svc.CreateSource(ctx, "usr_admin", SourceInput{URL: "acme/private-skills"}); !errors.Is(err, ErrNeedsToken) {
		t.Fatalf("create without token: %v", err)
	}
	src, res := mustCreate(t, svc, SourceInput{URL: "acme/private-skills", Path: "skills", Token: gh.token, Mode: ModeAuto})
	if !res.Activated || !src.HasToken || src.Auth != AuthToken {
		t.Fatalf("source %+v result %+v", src, res)
	}
	// The token is sealed in the database and never sent to the archive host.
	var sealed string
	if err := svc.db.QueryRow(`SELECT token_sealed FROM skill_sources WHERE id = ?`, src.ID).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if sealed == "" || strings.Contains(sealed, gh.token) {
		t.Fatal("token not sealed")
	}
	for _, a := range gh.codeloadAuth {
		if a != "" {
			t.Fatalf("the archive download got an Authorization header")
		}
	}
	// The token never shows in errors: a bad token fails without it.
	bad := "wrong-token-value"
	if _, err := svc.UpdateSource(ctx, "usr_admin", src.ID, SourceUpdate{Token: &bad}); err != nil {
		t.Fatal(err)
	}
	gh.commit("main", map[string]string{"skills/pr-review/checks.md": "new\n"})
	_, err = svc.SyncNow(ctx, src.ID)
	if err == nil || strings.Contains(err.Error(), bad) {
		t.Fatalf("sync with a bad token: %v", err)
	}
	s, _ := svc.GetSource(ctx, src.ID)
	if strings.Contains(s.LastError, bad) {
		t.Fatal("token in last_error")
	}
}

func TestRateLimitBacksOff(t *testing.T) {
	gh := newFakeGitHub(t, "acme", "skills")
	gh.commit("main", baseTree())
	svc := newTestService(t, gh)
	ctx := context.Background()
	src, err := svc.CreateSource(ctx, "usr_admin", SourceInput{URL: "acme/skills", Path: "skills", Mode: ModeAuto})
	if err != nil {
		t.Fatal(err)
	}
	gh.mu.Lock()
	gh.retryAfter = 1200
	gh.mu.Unlock()
	if _, err := svc.SyncNow(ctx, src.ID); err == nil || !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("rate limited sync: %v", err)
	}
	s, _ := svc.GetSource(ctx, src.ID)
	if s.NextCheckAt == nil || s.NextCheckAt.Before(time.Now().Add(19*time.Minute)) {
		t.Fatalf("next check %v, want after Retry-After", s.NextCheckAt)
	}
	// Calls wait for the limit without asking GitHub.
	before := gh.count("head")
	if _, err := svc.SyncNow(ctx, src.ID); err == nil {
		t.Fatal("a call went through during the rate limit")
	}
	if gh.count("head") != before {
		t.Fatal("GitHub was called during the rate limit")
	}
}

func TestPollerSyncsDueSources(t *testing.T) {
	gh := newFakeGitHub(t, "acme", "skills")
	c1 := gh.commit("main", baseTree())
	svc := newTestService(t, gh)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	src, err := svc.CreateSource(ctx, "usr_admin", SourceInput{URL: "acme/skills", Path: "skills", Mode: ModeAuto})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { svc.RunPoller(ctx); close(done) }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if got, _ := svc.Sources(ctx, adminViewer); len(got) == 1 && got[0].Commit == c1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the poller did not sync the new source")
		}
		time.Sleep(20 * time.Millisecond)
	}
	s, _ := svc.GetSource(ctx, src.ID)
	if s.NextCheckAt == nil || s.NextCheckAt.Before(time.Now().Add(4*time.Minute)) {
		t.Fatalf("next check %v", s.NextCheckAt)
	}
	cancel()
	<-done
}

func TestBackoff(t *testing.T) {
	poll := 5 * time.Minute
	for n, want := range map[int]time.Duration{1: 10 * time.Minute, 2: 20 * time.Minute, 3: 40 * time.Minute, 4: time.Hour, 9: time.Hour} {
		if got := backoff(poll, n); got != want {
			t.Errorf("backoff(%d) = %s, want %s", n, got, want)
		}
	}
}
