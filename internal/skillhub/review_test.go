package skillhub

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

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

// reviewStage stages a version of src_aaaa as the sync engine does: the
// source's pending version first, then the hook.
func reviewStage(ctx context.Context, e *proposalEnv, ri *ReviewInbox, versionID, sha string) error {
	e.admin.setPending("src_aaaa", versionID)
	return ri.PendingVersion(ctx, reviewTestSource(), reviewTestVersion(versionID, sha))
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
	if err := reviewStage(ctx, e, ri, "skv_1", sha); err != nil {
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
	if err := reviewStage(ctx, e, ri, "skv_1", sha); err != nil {
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
	if err := reviewStage(ctx, e, ri, "skv_1", sha); err != nil {
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
	if err := reviewStage(ctx, e, ri, "skv_1", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); err != nil {
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
	if err := reviewStage(ctx, e, ri, "skv_2", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"); err != nil {
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
	if err := reviewStage(ctx, e, ri, "skv_1", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); err != nil {
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
	if err := reviewStage(ctx, e, ri, "skv_2", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"); err != nil {
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
	if err := reviewStage(ctx, e, ri, "skv_1", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); err != nil {
		t.Fatal(err)
	}
	first := reviewRow(t, e, "skv_1")
	if err := reviewStage(ctx, e, ri, "skv_2", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"); err != nil {
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

// TestReviewOutdatedApprovalIsRefused: approving a question whose version
// is no longer the source's pending version (a settings change or an
// automatic commit replaced it) activates nothing and says why.
func TestReviewOutdatedApprovalIsRefused(t *testing.T) {
	e, ri := newReviewTest(t)
	ctx := context.Background()
	if err := reviewStage(ctx, e, ri, "skv_1", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); err != nil {
		t.Fatal(err)
	}
	row := reviewRow(t, e, "skv_1")
	// The source moved on without raising a new question (mode changed to
	// auto and a newer commit went live).
	e.admin.setPending("src_aaaa", "")
	got := e.answer(t, row.InboxID, "u_owner", reviewApprove)
	if len(e.admin.activates) != 0 || len(e.admin.rejects) != 0 {
		t.Fatalf("an outdated approval decided something: activates %v rejects %v", e.admin.activates, e.admin.rejects)
	}
	if !strings.Contains(proposalActivity(got), "no longer waits for review") {
		t.Fatalf("activity %q", proposalActivity(got))
	}
	if row = reviewRow(t, e, "skv_1"); row.Status != proposalCancelled {
		t.Fatalf("row %+v", row)
	}
	// Another version pending: still refused.
	if err := reviewStage(ctx, e, ri, "skv_2", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"); err != nil {
		t.Fatal(err)
	}
	row = reviewRow(t, e, "skv_2")
	e.admin.setPending("src_aaaa", "skv_3")
	e.answer(t, row.InboxID, "u_owner", reviewApprove)
	if len(e.admin.activates) != 0 {
		t.Fatalf("activates %v", e.admin.activates)
	}
	// A deleted source: refused too.
	if err := reviewStage(ctx, e, ri, "skv_4", "cccccccccccccccccccccccccccccccccccccccc"); err != nil {
		t.Fatal(err)
	}
	row = reviewRow(t, e, "skv_4")
	e.admin.mu.Lock()
	delete(e.admin.pending, "src_aaaa")
	e.admin.mu.Unlock()
	got = e.answer(t, row.InboxID, "u_owner", reviewReject)
	if len(e.admin.rejects) != 0 || !strings.Contains(proposalActivity(got), "the source was deleted") {
		t.Fatalf("deleted source: rejects %v activity %q", e.admin.rejects, proposalActivity(got))
	}
}

// TestReviewGoesToTheOwnerWhenTheCreatorCannotDecide: a creator who lost
// the admin role (or is blocked) no longer gets the review.
func TestReviewGoesToTheOwnerWhenTheCreatorCannotDecide(t *testing.T) {
	e := newProposalEnv(t)
	e.admins["u_root"] = true
	ri, err := NewReviewInbox(ReviewOptions{DB: e.db, Inbox: e.inbox, Admin: e.admin, IsAdmin: e.isAdmin, Now: e.clock,
		FallbackReviewer: func(context.Context) (string, error) { return "u_root", nil }})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	e.mu.Lock()
	e.admins["u_owner"] = false
	e.mu.Unlock()
	if err := reviewStage(ctx, e, ri, "skv_1", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); err != nil {
		t.Fatal(err)
	}
	row := reviewRow(t, e, "skv_1")
	item, err := e.inbox.Get(ctx, row.InboxID)
	if err != nil {
		t.Fatal(err)
	}
	if row.UserID != "u_root" || item.AgentID != inbox.SystemAgentID("u_root") || !strings.Contains(item.Message, "can no longer approve it") {
		t.Fatalf("review went to %q (%q): %s", row.UserID, item.AgentID, item.Message)
	}
	e.answer(t, row.InboxID, "u_root", reviewApprove)
	if len(e.admin.activates) != 1 || e.admin.activates[0][0] != "u_root" {
		t.Fatalf("activates %v", e.admin.activates)
	}
	// Without a fallback the review stays with the creator.
	ri2, _ := NewReviewInbox(ReviewOptions{DB: e.db, Inbox: e.inbox, Admin: e.admin, IsAdmin: e.isAdmin, Now: e.clock})
	if got := ri2.reviewer(ctx, "u_owner"); got != "u_owner" {
		t.Fatalf("no fallback: %q", got)
	}
}

// TestReviewSweep applies a decision no handler ran, and closes items that
// were withdrawn, expired, or whose version no longer waits.
func TestReviewSweep(t *testing.T) {
	e, ri := newReviewTest(t)
	ctx := context.Background()
	if err := reviewStage(ctx, e, ri, "skv_1", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); err != nil {
		t.Fatal(err)
	}
	row := reviewRow(t, e, "skv_1")
	// The decision lands while no handler runs (a node stopped).
	e.inbox.HandleSystem(ReviewItemKind, func(context.Context, *inbox.Request) (string, error) { return "", nil })
	e.answer(t, row.InboxID, "u_owner", reviewApprove)
	if len(e.admin.activates) != 0 || reviewRow(t, e, "skv_1").Status != proposalPending {
		t.Fatal("the stand-in handler must not decide")
	}
	if err := ri.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if len(e.admin.activates) != 1 || reviewRow(t, e, "skv_1").Status != proposalDone {
		t.Fatalf("sweep: activates %v row %+v", e.admin.activates, reviewRow(t, e, "skv_1"))
	}
	// A second sweep applies nothing twice.
	if err := ri.Sweep(ctx); err != nil || len(e.admin.activates) != 1 {
		t.Fatalf("second sweep: %v activates %v", err, e.admin.activates)
	}

	// An open question whose version no longer waits is cancelled.
	if err := reviewStage(ctx, e, ri, "skv_2", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"); err != nil {
		t.Fatal(err)
	}
	e.admin.setPending("src_aaaa", "")
	if err := ri.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	row = reviewRow(t, e, "skv_2")
	item, _ := e.inbox.Get(ctx, row.InboxID)
	if row.Status != proposalCancelled || item.Status != inbox.StatusCancelled {
		t.Fatalf("outdated question: row %s item %s", row.Status, item.Status)
	}

	// A question past its deadline is closed even before the Inbox marks
	// it expired.
	if err := reviewStage(ctx, e, ri, "skv_3", "cccccccccccccccccccccccccccccccccccccccc"); err != nil {
		t.Fatal(err)
	}
	e.advance(inbox.SystemItemTTL + time.Minute)
	if err := ri.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if row = reviewRow(t, e, "skv_3"); row.Status != proposalCancelled || !strings.Contains(row.Error, "expired") {
		t.Fatalf("expired question: %+v", row)
	}
	if len(e.admin.activates)+len(e.admin.rejects) != 1 {
		t.Fatalf("the sweep decided more than the one missed decision: %v %v", e.admin.activates, e.admin.rejects)
	}
	// A nil sweeper and RunInboxSweeps with a cancelled context return.
	var nilRI *ReviewInbox
	if err := nilRI.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	RunInboxSweeps(cctx, time.Millisecond, ri, nil)
}

// TestReviewTextHasNoLineBreaks: repository text cannot start a new
// Markdown line (a heading, a link) in an item toolyard sends.
func TestReviewTextHasNoLineBreaks(t *testing.T) {
	if got := reviewCode("a\nb\r\tc\u2028d"); got != "`a b  c d`" {
		t.Errorf("reviewCode = %q", got)
	}
	if got := reviewMD("x\n# y\u202e"); got != "x # y " {
		t.Errorf("reviewMD = %q", got)
	}
	e, ri := newReviewTest(t)
	ctx := context.Background()
	v := reviewTestVersion("skv_1", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	v.Diff.Added = []string{"ok\n\n# Approve now\n[click](https://evil.example)"}
	e.admin.setPending("src_aaaa", "skv_1")
	src := reviewTestSource()
	src.Branch = "main\n## injected"
	if err := ri.PendingVersion(ctx, src, v); err != nil {
		t.Fatal(err)
	}
	item, err := e.inbox.Get(ctx, reviewRow(t, e, "skv_1").InboxID)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"\n# Approve now", "\n## injected", "\n[click]"} {
		if strings.Contains(item.Message, bad) {
			t.Errorf("message keeps %q:\n%s", bad, item.Message)
		}
	}
}

// TestReviewHookIsIdempotent: the sync engine calls PendingVersion again at
// every check while a version waits. An open question, or an answer being
// applied, adds nothing; an expired question is asked again; a failed
// Inbox create leaves no row behind.
func TestReviewHookIsIdempotent(t *testing.T) {
	e, ri := newReviewTest(t)
	ctx := context.Background()
	count := func(versionID string) int {
		t.Helper()
		var n int
		if err := e.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM skill_proposals WHERE kind = ? AND version_id = ?`, proposalKindReview, versionID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	for i := 0; i < 3; i++ {
		if err := reviewStage(ctx, e, ri, "skv_1", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); err != nil {
			t.Fatal(err)
		}
	}
	if n := count("skv_1"); n != 1 {
		t.Fatalf("three calls made %d questions", n)
	}
	// An answer being applied (approved, not done yet) holds too.
	row := &proposalRow{ID: proposalNewID(), Kind: proposalKindReview, Status: proposalApproved, UserID: "u_owner", SourceID: "src_aaaa", VersionID: "skv_2"}
	if err := ri.store.insert(ctx, row); err != nil {
		t.Fatal(err)
	}
	if err := reviewStage(ctx, e, ri, "skv_2", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"); err != nil {
		t.Fatal(err)
	}
	if n := count("skv_2"); n != 1 {
		t.Fatalf("a question was raised while an answer is applied: %d rows", n)
	}
	// An expired question is asked again at the next check.
	if err := reviewStage(ctx, e, ri, "skv_3", "cccccccccccccccccccccccccccccccccccccccc"); err != nil {
		t.Fatal(err)
	}
	e.advance(inbox.SystemItemTTL + time.Minute)
	if err := ri.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if err := reviewStage(ctx, e, ri, "skv_3", "cccccccccccccccccccccccccccccccccccccccc"); err != nil {
		t.Fatal(err)
	}
	var pending int
	_ = e.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM skill_proposals WHERE version_id = ? AND status = ?`, "skv_3", proposalPending).Scan(&pending)
	if n := count("skv_3"); n != 2 || pending != 1 {
		t.Fatalf("after expiry: %d rows, %d pending", n, pending)
	}
	// An Inbox that refuses the item: no row is left, the call fails, and
	// the next call tries again.
	off, err := inbox.New(ctx, inbox.Options{DB: e.db, Now: e.clock, RichEnabled: func() bool { return false }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(off.Flush)
	riOff, err := NewReviewInbox(ReviewOptions{DB: e.db, Inbox: off, Admin: e.admin, IsAdmin: e.isAdmin, Now: e.clock})
	if err != nil {
		t.Fatal(err)
	}
	e.admin.setPending("src_aaaa", "skv_4")
	if err := riOff.PendingVersion(ctx, reviewTestSource(), reviewTestVersion("skv_4", "dddddddddddddddddddddddddddddddddddddddd")); err == nil {
		t.Fatal("a refused Inbox item must fail the call")
	}
	if n := count("skv_4"); n != 0 {
		t.Fatalf("a failed create left %d rows", n)
	}
}
