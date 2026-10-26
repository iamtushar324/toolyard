package skillhub

// A contributor's source that waits for approval makes no commit live,
// whatever its mode, and its approval is bound to the commit it was given
// for; a late Inbox answer never undoes a decision made on the Skills
// page.

import (
	"context"
	"errors"
	"strings"
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
	if _, err := svc.DecideApprovalFrom(ctx, "usr_admin", rej.ID, ApprovalPending, true, "", ""); !errors.Is(err, ErrConflict) {
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
