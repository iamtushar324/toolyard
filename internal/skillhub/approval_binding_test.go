package skillhub

// A contributor's source that waits for approval makes no commit live,
// whatever its mode, and its approval is bound to the commit it was given
// for; a late Inbox answer never undoes a decision made on the Skills
// page.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/actor"
	"github.com/tusharbhardwaj/toolyard/internal/inbox"
)

// A trusted owner's contributor source in auto mode: while it waits, Sync
// now, a webhook's sync and the poller stage every new commit and never
// make one live. Approving a commit that is no longer the one that would
// go live is refused; approving the waiting commit makes exactly it live.
func TestPendingAutoSourceNeverServesAnUnseenCommit(t *testing.T) {
	gh := newFakeGitHub(t, "acme", "skills")
	gh.commit("main", baseTree())
	svc := newTestService(t, gh, trustAcme)
	ctx := context.Background()
	src, err := svc.CreateSource(ctx, "usr_contrib", SourceInput{URL: "acme/skills", Path: "skills", Mode: ModeAuto, PendingApproval: true})
	if err != nil || src.Mode != ModeAuto || src.Approval != ApprovalPending {
		t.Fatalf("create: %+v %v", src, err)
	}
	// The commit the approval question shows.
	shown, err := svc.SyncNow(ctx, src.ID)
	if err != nil || !shown.Staged || shown.Activated {
		t.Fatalf("first sync: %+v %v", shown, err)
	}
	// The contributor pushes again: Sync now stages it.
	pushed := gh.commit("main", map[string]string{"skills/pr-review/checks.md": "changed after the question\n"})
	res, err := svc.SyncNow(ctx, src.ID)
	if err != nil || !res.Staged || res.Activated || res.SHA != pushed {
		t.Fatalf("sync of a new commit: %+v %v", res, err)
	}
	// And again, through a push webhook's sync.
	hooked := gh.commit("main", map[string]string{"skills/deploy/scripts/run.sh": "#!/bin/sh\necho changed\n"})
	res, err = svc.syncSource(ctx, src.ID, syncRequest{tr: triggerWebhook, sha: hooked})
	if err != nil || !res.Staged || res.Activated || res.SHA != hooked {
		t.Fatalf("webhook sync: %+v %v", res, err)
	}
	// The poller finds the waiting commit and changes nothing.
	if _, err := svc.db.ExecContext(ctx, `UPDATE skill_sources SET next_check_at = NULL WHERE id = ?`, src.ID); err != nil {
		t.Fatal(err)
	}
	svc.pollDue(ctx)
	got, _ := svc.GetSource(ctx, src.ID)
	if got.ActiveVersionID != "" || got.PendingVersionID != res.VersionID {
		t.Fatalf("a pending source went live: active %q (%s) pending %q, want pending %q", got.ActiveVersionID, got.ActiveSHA, got.PendingVersionID, res.VersionID)
	}
	if _, err := svc.Get(ctx, adminViewer, "skill://"+src.Slug+"/pr-review/SKILL.md", false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a pending source serves a skill: %v", err)
	}
	// Approving an older commit (the question's, or the one after it) is
	// refused: the newest would go live. Nothing changes.
	for _, sha := range []string{shown.SHA, pushed, ""} {
		if _, err := svc.DecideApproval(ctx, "usr_admin", src.ID, true, "", sha); !errors.Is(err, ErrConflict) {
			t.Fatalf("approve commit %q while %s waits: %v", sha, hooked, err)
		}
	}
	if got, _ := svc.GetSource(ctx, src.ID); got.Approval != ApprovalPending || got.ActiveVersionID != "" || got.PendingVersionID != res.VersionID {
		t.Fatalf("after refused approvals: %+v", got)
	}
	// Approving the waiting commit makes exactly it live.
	ok, err := svc.DecideApproval(ctx, "usr_admin", src.ID, true, "", hooked)
	if err != nil || ok.Approval != ApprovalApproved || ok.ActiveSHA != hooked || ok.PendingVersionID != "" {
		t.Fatalf("approve the waiting commit: %+v %v", ok, err)
	}
	if s, err := svc.Get(ctx, memberViewer, "skill://"+src.Slug+"/pr-review/SKILL.md", false); err != nil || s.Commit != hooked {
		t.Fatalf("after the approval: %+v %v", s, err)
	}
	// Approved, the source follows its mode again: a new commit goes live.
	next := gh.commit("main", map[string]string{"skills/pr-review/checks.md": "after the approval\n"})
	if res, err := svc.SyncNow(ctx, src.ID); err != nil || !res.Activated || res.SHA != next {
		t.Fatalf("sync after the approval: %+v %v", res, err)
	}
}

// An automatic source that synced nothing yet cannot be approved: its
// first sync would make a commit live that nobody saw. A live version
// (made live by an admin while it waited) binds the approval too.
func TestApprovalNeedsTheCommitThatGoesLive(t *testing.T) {
	gh := newFakeGitHub(t, "acme", "skills")
	gh.commit("main", baseTree())
	svc := newTestService(t, gh, trustAcme)
	ctx := context.Background()
	src, err := svc.CreateSource(ctx, "usr_contrib", SourceInput{URL: "acme/skills", Path: "skills", Mode: ModeAuto, PendingApproval: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.DecideApproval(ctx, "usr_admin", src.ID, true, "", strings.Repeat("a", 40)); !errors.Is(err, ErrConflict) {
		t.Fatalf("approve before the first sync: %v", err)
	}
	res, err := svc.SyncNow(ctx, src.ID)
	if err != nil || !res.Staged {
		t.Fatalf("sync: %+v %v", res, err)
	}
	// An admin makes the waiting version live while the source waits.
	if err := svc.Activate(ctx, "usr_admin", src.ID, res.VersionID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.DecideApproval(ctx, "usr_admin", src.ID, true, "", strings.Repeat("b", 40)); !errors.Is(err, ErrConflict) {
		t.Fatalf("approve another commit than the live one: %v", err)
	}
	if got, err := svc.DecideApproval(ctx, "usr_admin", src.ID, true, "", res.SHA); err != nil || got.Approval != ApprovalApproved || got.ActiveSHA != res.SHA {
		t.Fatalf("approve the live commit: %+v %v", got, err)
	}
}

// newApprovalInbox wires a ReviewInbox (owner u_owner) to svc.
func newApprovalInbox(t *testing.T, svc *Service) (*ReviewInbox, *inbox.Service) {
	t.Helper()
	ctx := context.Background()
	ib, err := inbox.New(ctx, inbox.Options{DB: svc.db})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ib.Flush)
	ri, err := NewReviewInbox(ReviewOptions{DB: svc.db, Inbox: ib, Admin: svc,
		IsAdmin:          func(_ context.Context, uid string) bool { return uid == "u_owner" },
		FallbackReviewer: func(context.Context) (string, error) { return "u_owner", nil }})
	if err != nil {
		t.Fatal(err)
	}
	svc.SetReviewHook(ri)
	return ri, ib
}

// decideApprovalItem answers an approval item as uid.
func decideApprovalItem(t *testing.T, ib *inbox.Service, id, uid, choice string) {
	t.Helper()
	ctx := context.Background()
	r, err := ib.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ib.Decide(ctx, id, inbox.Decision{Action: "answer", RequestRevision: r.Revision,
		SubmissionID: "sub-" + id, Answers: map[string]inbox.Answer{reviewQuestionID: {Value: choice}},
		Decider: actor.Decider{UserID: uid, Name: uid, Via: actor.ViaDashboard}}); err != nil {
		t.Fatalf("decide: %v", err)
	}
}

// An Inbox answer applies only while the source still waits: approving
// after an admin rejected on the Skills page leaves it rejected (and
// rejecting after an approval leaves it approved); the question is
// settled as cancelled.
func TestInboxAnswerNeverUndoesAPageDecision(t *testing.T) {
	gh := newFakeGitHub(t, "acme", "skills")
	gh.commit("main", baseTree())
	svc := newTestService(t, gh, trustAcme)
	ri, ib := newApprovalInbox(t, svc)
	ctx := context.Background()
	ask := func(slug string) (*Source, *SyncResult, proposalRow) {
		t.Helper()
		src, err := svc.CreateSource(ctx, "usr_contrib", SourceInput{URL: "acme/skills", Slug: slug, Path: "skills", Mode: ModeReview, PendingApproval: true})
		if err != nil {
			t.Fatal(err)
		}
		res, err := svc.SyncNow(ctx, src.ID)
		if err != nil || !res.Staged {
			t.Fatalf("sync: %+v %v", res, err)
		}
		prev, err := svc.PreviewSource(ctx, src.ID)
		if err != nil || prev.SHA != res.SHA {
			t.Fatalf("preview: %+v %v", prev, err)
		}
		src, _ = svc.GetSource(ctx, src.ID)
		if err := ri.RequestApproval(ctx, *src, prev, "Casey"); err != nil {
			t.Fatal(err)
		}
		rows, err := ri.store.pendingForSource(ctx, proposalKindApproval, src.ID)
		if err != nil || len(rows) != 1 {
			t.Fatalf("approval rows %+v %v", rows, err)
		}
		return src, res, rows[0]
	}

	// Rejected on the page (the open question was not closed), then
	// approved in the Inbox: it stays rejected.
	rej, _, row := ask("rejected-on-page")
	if _, err := svc.DecideApproval(ctx, "usr_admin", rej.ID, false, "not ours", ""); err != nil {
		t.Fatal(err)
	}
	decideApprovalItem(t, ib, row.InboxID, "u_owner", reviewApprove)
	if got, _ := svc.GetSource(ctx, rej.ID); got.Approval != ApprovalRejected || got.Enabled || got.ActiveVersionID != "" {
		t.Fatalf("an Inbox approval undid the page's rejection: %+v", got)
	}
	if r, _ := ri.store.get(ctx, row.ID); r.Status != proposalCancelled {
		t.Fatalf("approval row after a page rejection: %+v", r)
	}
	// The page can still approve it.
	if _, err := svc.DecideApproval(ctx, "usr_admin", rej.ID, true, "", ""); err != nil {
		t.Fatalf("page approval of a rejected review-mode source: %v", err)
	}

	// Approved on the page, then rejected in the Inbox: it stays approved.
	app, res, row := ask("approved-on-page")
	if _, err := svc.DecideApproval(ctx, "usr_admin", app.ID, true, "", res.SHA); err != nil {
		t.Fatal(err)
	}
	decideApprovalItem(t, ib, row.InboxID, "u_owner", reviewReject)
	if got, _ := svc.GetSource(ctx, app.ID); got.Approval != ApprovalApproved || !got.Enabled || got.ActiveSHA != res.SHA {
		t.Fatalf("an Inbox rejection undid the page's approval: %+v", got)
	}
	if r, _ := ri.store.get(ctx, row.ID); r.Status != proposalCancelled {
		t.Fatalf("rejection row after a page approval: %+v", r)
	}

	// DecideApprovalFrom refuses any state but the one named.
	if _, err := svc.DecideApprovalFrom(ctx, "usr_admin", rej.ID, ApprovalPending, true, "", ApprovalBinding{}); !errors.Is(err, ErrConflict) {
		t.Fatalf("decide from pending an approved source: %v", err)
	}
}

// A waiting source whose folder goes A, B and back to A with no new commit
// finds A's staged version again: it becomes the waiting version again,
// so approving the commit serves the folder the source has now.
func TestPendingSourceFindsItsStagedVersionAgain(t *testing.T) {
	gh := newFakeGitHub(t, "acme", "skills")
	tree := baseTree()
	tree["other/pr-review/SKILL.md"] = skillMD("pr-review", "Review a pull request, the other way.", "", "# Other\n")
	gh.commit("main", tree)
	svc := newTestService(t, gh, trustAcme)
	ctx := context.Background()
	src, err := svc.CreateSource(ctx, "usr_contrib", SourceInput{URL: "acme/skills", Path: "skills", Mode: ModeAuto, PendingApproval: true})
	if err != nil {
		t.Fatal(err)
	}
	a, err := svc.SyncNow(ctx, src.ID)
	if err != nil || !a.Staged {
		t.Fatalf("sync of folder skills: %+v %v", a, err)
	}
	folder := func(p string) {
		t.Helper()
		if _, err := svc.UpdateSource(ctx, "usr_contrib", src.ID, SourceUpdate{Path: &p}); err != nil {
			t.Fatal(err)
		}
	}
	folder("other")
	b, err := svc.SyncNow(ctx, src.ID)
	if err != nil || !b.Staged || b.VersionID == a.VersionID || b.SHA != a.SHA {
		t.Fatalf("sync of folder other: %+v %v (first %+v)", b, err, a)
	}
	folder("skills")
	again, err := svc.SyncNow(ctx, src.ID)
	if err != nil || !again.Staged || again.VersionID != a.VersionID {
		t.Fatalf("sync back to folder skills: %+v %v, want version %s waiting", again, err, a.VersionID)
	}
	if got, _ := svc.GetSource(ctx, src.ID); got.PendingVersionID != a.VersionID {
		t.Fatalf("waiting version %q, want %q", got.PendingVersionID, a.VersionID)
	}
	ok, err := svc.DecideApproval(ctx, "usr_admin", src.ID, true, "", a.SHA)
	if err != nil || ok.ActiveVersionID != a.VersionID {
		t.Fatalf("approve the commit: %+v %v", ok, err)
	}
}

// The approval question is never lost: an answer that cannot apply asks
// again for the commit the source waits with (or, with nothing synced yet,
// the sync that stages one asks); a check with no open question asks; a
// newer commit is asked about while an answer is applied; parallel asks
// add one question.
func TestApprovalQuestionIsNeverLost(t *testing.T) {
	gh := newFakeGitHub(t, "acme", "skills")
	first := gh.commit("main", baseTree())
	svc := newTestService(t, gh, trustAcme)
	ri, ib := newApprovalInbox(t, svc)
	ctx := context.Background()
	src, err := svc.CreateSource(ctx, "usr_contrib", SourceInput{URL: "acme/skills", Path: "skills", Mode: ModeAuto, PendingApproval: true})
	if err != nil {
		t.Fatal(err)
	}
	open := func() []proposalRow {
		t.Helper()
		rows, err := ri.store.pendingForSource(ctx, proposalKindApproval, src.ID)
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	shows := func(rows []proposalRow, sha string) bool {
		return len(rows) == 1 && strings.Contains(rows[0].Input, sha) && strings.Contains(rows[0].Input, "Casey")
	}
	// The owner approves before the first sync: refused (nothing synced),
	// and there is nothing to ask about yet.
	firstPrev, err := svc.PreviewSource(ctx, src.ID)
	if err != nil || firstPrev.SHA != first {
		t.Fatalf("dry run: %+v %v", firstPrev, err)
	}
	if err := ri.RequestApproval(ctx, *src, firstPrev, "Casey"); err != nil {
		t.Fatal(err)
	}
	rows := open()
	if !shows(rows, first) {
		t.Fatalf("first question: %+v", rows)
	}
	decideApprovalItem(t, ib, rows[0].InboxID, "u_owner", reviewApprove)
	if r, _ := ri.store.get(ctx, rows[0].ID); r.Status != proposalCancelled {
		t.Fatalf("an approval before the first sync: %+v", r)
	}
	if got, _ := svc.GetSource(ctx, src.ID); got.Approval != ApprovalPending {
		t.Fatalf("approved with nothing synced: %+v", got)
	}
	// The first sync stages the commit and asks again; later checks add
	// nothing.
	res, err := svc.SyncNow(ctx, src.ID)
	if err != nil || !res.Staged || res.SHA != first {
		t.Fatalf("first sync: %+v %v", res, err)
	}
	if rows = open(); !shows(rows, first) {
		t.Fatalf("question after the first sync: %+v", rows)
	}
	if _, err := svc.SyncNow(ctx, src.ID); err != nil {
		t.Fatal(err)
	}
	if rows = open(); len(rows) != 1 {
		t.Fatalf("a check asked twice: %+v", rows)
	}
	// An answer is being applied while a newer commit is staged: the
	// answer is refused (another commit would go live) and the question
	// is asked for the newer commit.
	if ok, err := ri.store.move(ctx, rows[0].ID, []string{proposalPending}, proposalApproved, proposalUpdate{DecidedBy: "u_owner"}); err != nil || !ok {
		t.Fatalf("claim the answer: %v %v", ok, err)
	}
	pushed := gh.commit("main", map[string]string{"skills/pr-review/checks.md": "pushed while the answer was applied\n"})
	if res, err := svc.SyncNow(ctx, src.ID); err != nil || !res.Staged || res.SHA != pushed {
		t.Fatalf("sync of the newer commit: %+v %v", res, err)
	}
	if rows := open(); len(rows) != 0 {
		t.Fatalf("asked while an answer was applied: %+v", rows)
	}
	claimed, err := ri.store.get(ctx, rows[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ri.applyApproval(ctx, svc, claimed, "u_owner", "inbox"); !errors.Is(err, ErrConflict) {
		t.Fatalf("an answer for the older commit: %v", err)
	}
	if rows = open(); !shows(rows, pushed) {
		t.Fatalf("question after a refused answer: %+v", rows)
	}
	// The question closed out of band: the next check asks again.
	if err := ri.SettleApproval(ctx, src.ID, "withdrawn by the test"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SyncNow(ctx, src.ID); err != nil {
		t.Fatal(err)
	}
	if rows = open(); !shows(rows, pushed) {
		t.Fatalf("question after a check with none open: %+v", rows)
	}
	// Parallel asks add one question.
	if err := ri.SettleApproval(ctx, src.ID, "withdrawn by the test"); err != nil {
		t.Fatal(err)
	}
	cur, _ := svc.GetSource(ctx, src.ID)
	pushedPrev, err := svc.PreviewSource(ctx, src.ID)
	if err != nil || pushedPrev.SHA != pushed {
		t.Fatalf("dry run: %+v %v", pushedPrev, err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = ri.RequestApproval(ctx, *cur, pushedPrev, "Casey")
		}()
	}
	wg.Wait()
	if rows = open(); !shows(rows, pushed) {
		t.Fatalf("parallel asks: %+v", rows)
	}
	// Approving that question makes the waiting commit live.
	decideApprovalItem(t, ib, rows[0].InboxID, "u_owner", reviewApprove)
	if got, _ := svc.GetSource(ctx, src.ID); got.Approval != ApprovalApproved || got.ActiveSHA != pushed {
		t.Fatalf("after the approval: %+v", got)
	}
}

// An approval question shows the settings that go live. A sync that read
// the source before a folder change hands the hook its outdated source and
// version, and a dry run of the old folder comes late: nothing is asked
// from them. An approval bound to the old version, or to the old settings,
// is refused, also once the new folder's version (the same commit) waits.
// The question asked with a dry run of the new folder is the one whose
// approval makes the new folder live.
func TestApprovalQuestionShowsTheSettingsThatGoLive(t *testing.T) {
	gh := newFakeGitHub(t, "acme", "skills")
	tree := baseTree()
	tree["other/pr-review/SKILL.md"] = skillMD("pr-review", "Review a pull request, the other way.", "", "# Other\n")
	gh.commit("main", tree)
	svc := newTestService(t, gh, trustAcme)
	ri, ib := newApprovalInbox(t, svc)
	ctx := context.Background()
	src, err := svc.CreateSource(ctx, "usr_contrib", SourceInput{URL: "acme/skills", Path: "skills", Mode: ModeAuto, PendingApproval: true})
	if err != nil {
		t.Fatal(err)
	}
	open := func() []proposalRow {
		t.Helper()
		rows, err := ri.store.pendingForSource(ctx, proposalKindApproval, src.ID)
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	// The sync stages folder skills and asks about that version.
	a, err := svc.SyncNow(ctx, src.ID)
	if err != nil || !a.Staged {
		t.Fatalf("sync: %+v %v", a, err)
	}
	rows := open()
	if len(rows) != 1 {
		t.Fatalf("questions after the first sync: %+v", rows)
	}
	oldQ := parseApprovalInput(rows[0].Input)
	if oldQ.Version != a.VersionID || oldQ.Config == "" || oldQ.SHA != a.SHA {
		t.Fatalf("the question is not bound to the waiting version: %+v", oldQ)
	}
	staleSrc, _ := svc.GetSource(ctx, src.ID)
	staleV, err := svc.GetVersion(ctx, src.ID, a.VersionID)
	if err != nil {
		t.Fatal(err)
	}
	oldPrev, err := svc.PreviewSource(ctx, src.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The contributor changes the folder: the waiting version is dropped,
	// and the route closes the question.
	other := "other"
	if _, err := svc.UpdateSource(ctx, "usr_contrib", src.ID, SourceUpdate{Path: &other}); err != nil {
		t.Fatal(err)
	}
	if got, _ := svc.GetSource(ctx, src.ID); got.PendingVersionID != "" {
		t.Fatalf("the waiting version of the old folder stayed: %q", got.PendingVersionID)
	}
	if err := ri.SettleApproval(ctx, src.ID, "the folder changed"); err != nil {
		t.Fatal(err)
	}
	if err := ri.PendingApprovalVersion(ctx, *staleSrc, *staleV); err != nil {
		t.Fatal(err)
	}
	if err := ri.RequestApproval(ctx, *staleSrc, oldPrev, "Casey"); err != nil {
		t.Fatal(err)
	}
	if rows := open(); len(rows) != 0 {
		t.Fatalf("a question from an outdated read: %+v", rows)
	}
	oldSettings := ApprovalBinding{SHA: a.SHA, Config: oldQ.Config}
	for name, bind := range map[string]ApprovalBinding{"old version": oldQ.binding(), "old settings": oldSettings} {
		if _, err := svc.DecideApprovalFrom(ctx, "usr_admin", src.ID, ApprovalPending, true, "", bind); !errors.Is(err, ErrConflict) {
			t.Fatalf("an approval bound to the %s: %v", name, err)
		}
	}
	// The route asks with a dry run of the new folder; the sync builds the
	// new folder's version of the same commit and keeps that question.
	newPrev, err := svc.PreviewSource(ctx, src.ID)
	if err != nil || newPrev.SHA != a.SHA {
		t.Fatalf("dry run of the new folder: %+v %v", newPrev, err)
	}
	cur, _ := svc.GetSource(ctx, src.ID)
	if err := ri.RequestApproval(ctx, *cur, newPrev, "Casey"); err != nil {
		t.Fatal(err)
	}
	b, err := svc.SyncNow(ctx, src.ID)
	if err != nil || !b.Staged || b.VersionID == a.VersionID || b.SHA != a.SHA {
		t.Fatalf("sync of the new folder: %+v %v", b, err)
	}
	rows = open()
	if len(rows) != 1 || parseApprovalInput(rows[0].Input).Config == oldQ.Config {
		t.Fatalf("question for the new folder: %+v", rows)
	}
	for name, bind := range map[string]ApprovalBinding{"old version": oldQ.binding(), "old settings": oldSettings} {
		if _, err := svc.DecideApprovalFrom(ctx, "usr_admin", src.ID, ApprovalPending, true, "", bind); !errors.Is(err, ErrConflict) {
			t.Fatalf("an approval bound to the %s, the new folder waiting: %v", name, err)
		}
	}
	decideApprovalItem(t, ib, rows[0].InboxID, "u_owner", reviewApprove)
	if got, _ := svc.GetSource(ctx, src.ID); got.Approval != ApprovalApproved || got.ActiveVersionID != b.VersionID {
		t.Fatalf("after the approval: %+v", got)
	}
	if s, err := svc.Get(ctx, memberViewer, "skill://"+src.Slug+"/pr-review/SKILL.md", false); err != nil || !strings.Contains(s.Body, "# Other") {
		t.Fatalf("the approved folder is not live: %+v %v", s, err)
	}
}

// Two open questions, one about the waiting version and one left over (a
// crash between asking the new question and closing the old one): the
// next check closes the leftover.
func TestCheckClosesALeftoverQuestion(t *testing.T) {
	gh := newFakeGitHub(t, "acme", "skills")
	gh.commit("main", baseTree())
	svc := newTestService(t, gh, trustAcme)
	ri, _ := newApprovalInbox(t, svc)
	ctx := context.Background()
	src, err := svc.CreateSource(ctx, "usr_contrib", SourceInput{URL: "acme/skills", Path: "skills", Mode: ModeReview, PendingApproval: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SyncNow(ctx, src.ID); err != nil {
		t.Fatal(err)
	}
	rows, err := ri.store.pendingForSource(ctx, proposalKindApproval, src.ID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("questions after the first sync: %+v %v", rows, err)
	}
	current := rows[0]
	leftover := &proposalRow{ID: proposalNewID(), Kind: proposalKindApproval, Status: proposalPending, UserID: "u_owner",
		SourceID: src.ID, Input: `{"sha":"` + strings.Repeat("e", 40) + `","skills":1,"warnings":0}`}
	if err := ri.store.insert(ctx, leftover); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SyncNow(ctx, src.ID); err != nil {
		t.Fatal(err)
	}
	rows, err = ri.store.pendingForSource(ctx, proposalKindApproval, src.ID)
	if err != nil || len(rows) != 1 || rows[0].ID != current.ID {
		t.Fatalf("questions after a check: %+v %v", rows, err)
	}
	if r, _ := ri.store.get(ctx, leftover.ID); r.Status != proposalCancelled {
		t.Fatalf("the leftover question: %+v", r)
	}
}

// The Skills page approval is bound to the version the page showed (the
// route passes its version_id): after a folder change on the same commit,
// an approval naming the old version is refused, whether the new version
// waits or an admin made it live; naming the version that goes live
// approves.
func TestPageApprovalIsBoundToTheVersionShown(t *testing.T) {
	gh := newFakeGitHub(t, "acme", "skills")
	tree := baseTree()
	tree["other/pr-review/SKILL.md"] = skillMD("pr-review", "Review a pull request, the other way.", "", "# Other\n")
	gh.commit("main", tree)
	svc := newTestService(t, gh, trustAcme)
	ctx := context.Background()
	src, err := svc.CreateSource(ctx, "usr_contrib", SourceInput{URL: "acme/skills", Path: "skills", Mode: ModeAuto, PendingApproval: true})
	if err != nil {
		t.Fatal(err)
	}
	a, err := svc.SyncNow(ctx, src.ID)
	if err != nil || !a.Staged {
		t.Fatalf("sync of folder skills: %+v %v", a, err)
	}
	other := "other"
	if _, err := svc.UpdateSource(ctx, "usr_contrib", src.ID, SourceUpdate{Path: &other}); err != nil {
		t.Fatal(err)
	}
	b, err := svc.SyncNow(ctx, src.ID)
	if err != nil || !b.Staged || b.SHA != a.SHA || b.VersionID == a.VersionID {
		t.Fatalf("sync of folder other: %+v %v", b, err)
	}
	page := func(versionID string) error {
		_, err := svc.DecideApprovalFrom(ctx, "usr_admin", src.ID, ApprovalPending, true, "", ApprovalBinding{SHA: a.SHA, Version: versionID})
		return err
	}
	// The page showed folder skills' version; folder other's waits now.
	if err := page(a.VersionID); !errors.Is(err, ErrConflict) {
		t.Fatalf("a page approval of the old version, the new one waiting: %v", err)
	}
	// An admin makes the new version live while the source waits: a page
	// that showed the old version is still refused.
	if err := svc.Activate(ctx, "usr_admin", src.ID, b.VersionID); err != nil {
		t.Fatal(err)
	}
	if err := page(a.VersionID); !errors.Is(err, ErrConflict) {
		t.Fatalf("a page approval of the old version, the new one live: %v", err)
	}
	if got, _ := svc.GetSource(ctx, src.ID); got.Approval != ApprovalPending {
		t.Fatalf("approved by a refused page approval: %+v", got)
	}
	if err := page(b.VersionID); err != nil {
		t.Fatalf("a page approval of the live version: %v", err)
	}
	if got, _ := svc.GetSource(ctx, src.ID); got.Approval != ApprovalApproved || got.ActiveVersionID != b.VersionID {
		t.Fatalf("after the approval: %+v", got)
	}
}
