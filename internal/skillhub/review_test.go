package skillhub

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/inbox"
)

func reviewTestSource() Source {
	return Source{ID: "src_aaaa", Slug: "team-skills", Owner: "acme", Repo: "skills", Branch: "main", Path: "skills",
		Mode: ModeReview, CreatedBy: "u_owner"}
}

func reviewTestVersion(id, sha string) Version {
	return Version{ID: id, SourceID: "src_aaaa", SHA: sha, Status: VersionStaged, SkillCount: 4, FileCount: 9,
		Warnings: []Warning{{Path: "skills/x/SKILL.md", Code: "description_too_long", Severity: "warn"}},
		Diff: &Diff{FromSHA: "1111111111111111111111111111111111111111", ToSHA: sha,
			Added: []string{"pdf", "docx"}, Changed: []string{"pr-merge-ready"}, Removed: nil}}
}

func newReviewTest(t *testing.T) (*proposalEnv, *ReviewInbox) {
	t.Helper()
	e := newProposalEnv(t)
	ri, err := NewReviewInbox(ReviewOptions{DB: e.db, Inbox: e.inbox, Admin: e.admin, IsAdmin: e.isAdmin,
		DashboardURL: "https://toolyard.example", Now: e.clock})
	if err != nil {
		t.Fatal(err)
	}
	return e, ri
}

// reviewRows returns the source's review rows by version.
func reviewRow(t *testing.T, e *proposalEnv, versionID string) *proposalRow {
	t.Helper()
	var id string
	if err := e.db.QueryRowContext(context.Background(), `SELECT id FROM skill_proposals WHERE kind = ? AND version_id = ?`, proposalKindReview, versionID).Scan(&id); err != nil {
		t.Fatalf("review row for %s: %v", versionID, err)
	}
	row, err := proposalStore{db: e.db, now: e.clock}.get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func TestReviewItemGoesToTheCreator(t *testing.T) {
	e, ri := newReviewTest(t)
	ctx := context.Background()
	sha := "abcdef0123456789abcdef0123456789abcdef01"
	if err := ri.PendingVersion(ctx, reviewTestSource(), reviewTestVersion("skv_1", sha)); err != nil {
		t.Fatal(err)
	}
	row := reviewRow(t, e, "skv_1")
	if row.Status != proposalPending || row.UserID != "u_owner" || row.SourceID != "src_aaaa" || row.InboxID == "" {
		t.Fatalf("row %+v", row)
	}
	item, err := e.inbox.Get(ctx, row.InboxID)
	if err != nil {
		t.Fatal(err)
	}
	if item.AgentID != inbox.SystemAgentID("u_owner") || item.System == nil || item.System.Kind != ReviewItemKind || item.System.Ref != row.ID {
		t.Fatalf("item owner/tag: %q %+v", item.AgentID, item.System)
	}
	if !strings.HasPrefix(item.Title, "Review skills update: team-skills") || len([]rune(item.Title)) > inbox.MaxTitle {
		t.Fatalf("title %q", item.Title)
	}
	for _, want := range []string{
		"https://github.com/acme/skills/commit/" + sha,
		"https://github.com/acme/skills/commit/1111111111111111111111111111111111111111",
		"**Added (2):** `pdf`, `docx`",
		"**Changed (1):** `pr-merge-ready`",
		"**Removed (0):** none",
		"**Warnings:** 1",
		"https://toolyard.example/#skills/src_aaaa",
	} {
		if !strings.Contains(item.Message, want) {
			t.Errorf("message lacks %q:\n%s", want, item.Message)
		}
	}
	if !strings.Contains(item.Summary, "2 added, 1 changed, 0 removed, 1 warning") {
		t.Errorf("summary %q", item.Summary)
	}
	if len(item.Questions) != 1 || item.Questions[0].Prompt != "Make this version live?" || len(item.Questions[0].Options) != 2 {
		t.Fatalf("questions %+v", item.Questions)
	}
	// The same version again adds no second item.
	if err := ri.PendingVersion(ctx, reviewTestSource(), reviewTestVersion("skv_1", sha)); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = e.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM skill_proposals WHERE kind = ?`, proposalKindReview).Scan(&n)
	if n != 1 {
		t.Fatalf("review rows %d, want 1", n)
	}
}

func TestReviewApproveActivatesTheVersion(t *testing.T) {
	e, ri := newReviewTest(t)
	ctx := context.Background()
	sha := "abcdef0123456789abcdef0123456789abcdef01"
	if err := ri.PendingVersion(ctx, reviewTestSource(), reviewTestVersion("skv_1", sha)); err != nil {
		t.Fatal(err)
	}
	row := reviewRow(t, e, "skv_1")
	got := e.answer(t, row.InboxID, "u_owner", reviewApprove)
	if len(e.admin.activates) != 1 || e.admin.activates[0] != [3]string{"u_owner", "src_aaaa", "skv_1"} {
		t.Fatalf("activates %v", e.admin.activates)
	}
	if len(e.admin.rejects) != 0 {
		t.Fatalf("rejects %v", e.admin.rejects)
	}
	if !strings.Contains(proposalActivity(got), "Version abcdef012345 is live.") {
		t.Fatalf("activity %q", proposalActivity(got))
	}
	if row = reviewRow(t, e, "skv_1"); row.Status != proposalDone || row.DecidedBy != "u_owner" {
		t.Fatalf("row %+v", row)
	}
}

func TestReviewRejectAndDenyRejectTheVersion(t *testing.T) {
	e, ri := newReviewTest(t)
	ctx := context.Background()
	if err := ri.PendingVersion(ctx, reviewTestSource(), reviewTestVersion("skv_1", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")); err != nil {
		t.Fatal(err)
	}
	row := reviewRow(t, e, "skv_1")
	got := e.answer(t, row.InboxID, "u_owner", reviewReject)
	if len(e.admin.rejects) != 1 || e.admin.rejects[0][2] != "skv_1" || e.admin.rejects[0][3] != "rejected in the Inbox" {
		t.Fatalf("rejects %v", e.admin.rejects)
	}
	if !strings.Contains(proposalActivity(got), "was rejected") || reviewRow(t, e, "skv_1").Status != proposalRejected {
		t.Fatalf("activity %q", proposalActivity(got))
	}
	// A deny with a note rejects the next version with that note.
	if err := ri.PendingVersion(ctx, reviewTestSource(), reviewTestVersion("skv_2", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")); err != nil {
		t.Fatal(err)
	}
	row = reviewRow(t, e, "skv_2")
	if _, err := e.inbox.Decide(ctx, row.InboxID, inbox.Decision{Action: "deny", Note: "wait for the release", Decider: proposalDecider("u_owner")}); err != nil {
		t.Fatal(err)
	}
	if len(e.admin.rejects) != 2 || e.admin.rejects[1][3] != "wait for the release" {
		t.Fatalf("rejects %v", e.admin.rejects)
	}
	if len(e.admin.activates) != 0 {
		t.Fatalf("activates %v", e.admin.activates)
	}
}

func TestReviewRefusesANonAdminAndRecordsFailures(t *testing.T) {
	e, ri := newReviewTest(t)
	ctx := context.Background()
	if err := ri.PendingVersion(ctx, reviewTestSource(), reviewTestVersion("skv_1", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")); err != nil {
		t.Fatal(err)
	}
	// The creator lost the admin role since the source was added.
	e.mu.Lock()
	e.admins["u_owner"] = false
	e.mu.Unlock()
	row := reviewRow(t, e, "skv_1")
	got := e.answer(t, row.InboxID, "u_owner", reviewApprove)
	if len(e.admin.activates) != 0 {
		t.Fatal("a non-admin's approval activated the version")
	}
	if !strings.Contains(proposalActivity(got), "toolyard could not apply this decision: only an admin") {
		t.Fatalf("activity %q", proposalActivity(got))
	}
	if row = reviewRow(t, e, "skv_1"); row.Status != proposalFailed {
		t.Fatalf("row %+v", row)
	}

	// Activate failing (the version is gone) is recorded on the item.
	e.mu.Lock()
	e.admins["u_owner"] = true
	e.mu.Unlock()
	e.admin.activateErr = errors.New("skillhub: conflict: the version is not staged")
	if err := ri.PendingVersion(ctx, reviewTestSource(), reviewTestVersion("skv_2", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")); err != nil {
		t.Fatal(err)
	}
	row = reviewRow(t, e, "skv_2")
	got = e.answer(t, row.InboxID, "u_owner", reviewApprove)
	if !strings.Contains(proposalActivity(got), "the version is not staged") || reviewRow(t, e, "skv_2").Status != proposalFailed {
		t.Fatalf("activity %q", proposalActivity(got))
	}
}

func TestReviewNewerCommitAndDashboardDecisionsCloseOpenItems(t *testing.T) {
	e, ri := newReviewTest(t)
	ctx := context.Background()
	if err := ri.PendingVersion(ctx, reviewTestSource(), reviewTestVersion("skv_1", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")); err != nil {
		t.Fatal(err)
	}
	first := reviewRow(t, e, "skv_1")
	if err := ri.PendingVersion(ctx, reviewTestSource(), reviewTestVersion("skv_2", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")); err != nil {
		t.Fatal(err)
	}
	if row := reviewRow(t, e, "skv_1"); row.Status != proposalCancelled || !strings.Contains(row.Error, "newer commit") {
		t.Fatalf("older row %+v", row)
	}
	item, _ := e.inbox.Get(ctx, first.InboxID)
	if item.Status != inbox.StatusCancelled || !strings.Contains(proposalActivity(item), "a newer commit is waiting for review") {
		t.Fatalf("older item %s %q", item.Status, proposalActivity(item))
	}
	// An admin approves skv_2 on the source page: its item closes.
	second := reviewRow(t, e, "skv_2")
	if err := ri.Settle(ctx, "src_aaaa", "skv_2", "approved on the Skills page"); err != nil {
		t.Fatal(err)
	}
	item, _ = e.inbox.Get(ctx, second.InboxID)
	if item.Status != inbox.StatusCancelled || reviewRow(t, e, "skv_2").Status != proposalCancelled {
		t.Fatalf("settled item %s", item.Status)
	}
	// Settling twice, or with nothing open, is fine.
	if err := ri.Settle(ctx, "src_aaaa", "", "source deleted"); err != nil {
		t.Fatal(err)
	}
	if len(e.admin.activates)+len(e.admin.rejects) != 0 {
		t.Fatal("settling must not decide anything")
	}
}

func TestReviewNeedsACreator(t *testing.T) {
	_, ri := newReviewTest(t)
	src := reviewTestSource()
	src.CreatedBy = ""
	if err := ri.PendingVersion(context.Background(), src, reviewTestVersion("skv_1", "aaaa")); !errors.Is(err, ErrInvalid) {
		t.Fatalf("no creator: %v", err)
	}
	if _, err := NewReviewInbox(ReviewOptions{}); err == nil {
		t.Fatal("NewReviewInbox without dependencies must fail")
	}
}

func TestReviewTextHelpers(t *testing.T) {
	if got := reviewCode("a`b"); got != "`a'b`" {
		t.Errorf("reviewCode = %q", got)
	}
	if got := reviewMD("*x_[y]*"); got != `\*x\_\[y\]\*` {
		t.Errorf("reviewMD = %q", got)
	}
	if got := reviewClip("abcdef", 4); got != "abc…" {
		t.Errorf("reviewClip = %q", got)
	}
	many := make([]string, 25)
	for i := range many {
		many[i] = "s"
	}
	if got := reviewList(many); !strings.HasSuffix(got, "and 5 more") {
		t.Errorf("reviewList = %q", got)
	}
	if reviewSourceLink("", "src_1") != "" || reviewSourceLink("https://t.example/", "src_1") != "https://t.example/#skills/src_1" {
		t.Error("reviewSourceLink")
	}
	if reviewFolder("") != "the repository root" || reviewFolder("skills") != "folder `skills`" {
		t.Error("reviewFolder")
	}
}
