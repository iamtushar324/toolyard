package skillhub

// The rows behind the skills hub's Inbox questions never stay open
// forever: an item decided on a replica that predates system items fails
// its row (and toolyard never acts on it), an approval whose process
// stopped is resumed exactly once, change proposals are capped per hour,
// and a deleted source closes the change proposals against it.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/actor"
	"github.com/tusharbhardwaj/toolyard/internal/inbox"
)

func TestReviewLostSystemTagFailsAndAsksAgain(t *testing.T) {
	e, ri := newReviewTest(t)
	ctx := context.Background()
	sha := "abcdef0123456789abcdef0123456789abcdef01"
	if err := reviewStage(ctx, e, ri, "skv_1", sha); err != nil {
		t.Fatal(err)
	}
	row := reviewRow(t, e, "skv_1")
	dropSystemTag(t, e.db, row.InboxID)
	e.answer(t, row.InboxID, "u_owner", reviewApprove) // no handler runs: the tag is gone
	if err := ri.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := ri.store.get(ctx, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != proposalFailed || got.Error != lostTagError {
		t.Fatalf("row %+v", got)
	}
	if len(e.admin.activates) != 0 || len(e.admin.rejects) != 0 {
		t.Fatalf("acted on an untagged answer: activates %v rejects %v", e.admin.activates, e.admin.rejects)
	}
	// The failed row no longer blocks the version: the next check asks
	// again with a new item.
	if err := reviewStage(ctx, e, ri, "skv_1", sha); err != nil {
		t.Fatal(err)
	}
	var pending, total int
	_ = e.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM skill_proposals WHERE kind = ? AND version_id = ? AND status = ?`, proposalKindReview, "skv_1", proposalPending).Scan(&pending)
	_ = e.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM skill_proposals WHERE kind = ? AND version_id = ?`, proposalKindReview, "skv_1").Scan(&total)
	if pending != 1 || total != 2 {
		t.Fatalf("after asking again: %d pending of %d rows", pending, total)
	}
}

func TestSourceProposalLostSystemTagFails(t *testing.T) {
	e, p := newProposalTest(t)
	ctx := context.Background()
	got := proposeTestSource(t, p, "acme/skills")
	dropSystemTag(t, e.db, got.InboxID)
	e.answer(t, got.InboxID, "u_owner", reviewApprove)
	if err := p.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	res, err := p.GetProposal(ctx, proposalAgent, got.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != proposalFailed || res.Error != lostTagError {
		t.Fatalf("proposal %+v", res)
	}
	if len(e.admin.creates) != 0 {
		t.Fatalf("created a source from an untagged answer: %v", e.admin.creates)
	}
	if n, err := p.store.countPending(ctx, proposalAgent.AgentID, proposalKindSource); err != nil || n != 0 {
		t.Fatalf("the failed proposal still counts as waiting: %d %v", n, err)
	}
}

func TestChangeLostSystemTagFails(t *testing.T) {
	ctx := context.Background()
	e := newChangeEnv(t)
	p := e.propose(t, changeInput4())
	dropSystemTag(t, e.svc.db, p.InboxID)
	e.decide(t, p, "usr_admin", reviewApprove, actor.ViaDashboard)
	calls := e.git.totalCalls()
	if err := e.changes.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if row := e.row(t, p.ID); row.Status != proposalFailed || row.Error != lostTagError {
		t.Fatalf("row %+v", row)
	}
	if n := e.git.totalCalls(); n != calls || len(e.git.pulls) != 0 {
		t.Fatalf("wrote to GitHub on an untagged answer: %d calls (was %d), %d pulls", n, calls, len(e.git.pulls))
	}
}

// staleReviews lists the review rows the sweep would resume now.
func staleReviews(t *testing.T, e *proposalEnv, ri *ReviewInbox) []proposalRow {
	t.Helper()
	rows, err := ri.store.staleApproved(context.Background(), proposalKindReview, e.clock().Add(-approvedStaleAfter).UnixMilli(), 10)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestReviewResumesAStoppedApproval(t *testing.T) {
	ctx := context.Background()
	t.Run("activates once", func(t *testing.T) {
		e, ri := newReviewTest(t)
		if err := reviewStage(ctx, e, ri, "skv_1", "abcdef0123456789abcdef0123456789abcdef01"); err != nil {
			t.Fatal(err)
		}
		row := reviewRow(t, e, "skv_1")
		ageRow(t, e.db, row.ID, "u_owner", e.clock())
		// A fresh approval may still be running: not resumed yet.
		if err := ri.Sweep(ctx); err != nil {
			t.Fatal(err)
		}
		if len(e.admin.activates) != 0 {
			t.Fatalf("a fresh approval was resumed: %v", e.admin.activates)
		}
		e.advance(approvedStaleAfter + time.Minute)
		stale := staleReviews(t, e, ri)
		if len(stale) != 1 {
			t.Fatalf("stale rows %d", len(stale))
		}
		// Two replicas holding the same snapshot: only one wins the claim.
		first, second := stale[0], stale[0]
		if err := ri.resume(ctx, &first); err != nil {
			t.Fatal(err)
		}
		if err := ri.resume(ctx, &second); err != nil {
			t.Fatal(err)
		}
		if err := ri.Sweep(ctx); err != nil {
			t.Fatal(err)
		}
		if len(e.admin.activates) != 1 || e.admin.activates[0] != [3]string{"u_owner", "src_aaaa", "skv_1"} {
			t.Fatalf("activates %v", e.admin.activates)
		}
		if got := reviewRow(t, e, "skv_1"); got.Status != proposalDone {
			t.Fatalf("row %+v", got)
		}
	})
	t.Run("already live", func(t *testing.T) {
		e, ri := newReviewTest(t)
		if err := reviewStage(ctx, e, ri, "skv_1", "abcdef0123456789abcdef0123456789abcdef01"); err != nil {
			t.Fatal(err)
		}
		row := reviewRow(t, e, "skv_1")
		ageRow(t, e.db, row.ID, "u_owner", e.clock())
		e.admin.mu.Lock()
		e.admin.pending["src_aaaa"], e.admin.active["src_aaaa"] = "", "skv_1"
		e.admin.mu.Unlock()
		e.advance(approvedStaleAfter + time.Minute)
		if err := ri.Sweep(ctx); err != nil {
			t.Fatal(err)
		}
		if got := reviewRow(t, e, "skv_1"); got.Status != proposalDone || len(e.admin.activates) != 0 {
			t.Fatalf("row %+v, activates %v", got, e.admin.activates)
		}
	})
	t.Run("superseded", func(t *testing.T) {
		e, ri := newReviewTest(t)
		if err := reviewStage(ctx, e, ri, "skv_1", "abcdef0123456789abcdef0123456789abcdef01"); err != nil {
			t.Fatal(err)
		}
		row := reviewRow(t, e, "skv_1")
		ageRow(t, e.db, row.ID, "u_owner", e.clock())
		e.admin.setPending("src_aaaa", "skv_2")
		e.advance(approvedStaleAfter + time.Minute)
		if err := ri.Sweep(ctx); err != nil {
			t.Fatal(err)
		}
		got := reviewRow(t, e, "skv_1")
		if got.Status != proposalCancelled || !strings.Contains(got.Error, "superseded") || len(e.admin.activates) != 0 {
			t.Fatalf("row %+v, activates %v", got, e.admin.activates)
		}
	})
	t.Run("source deleted", func(t *testing.T) {
		e, ri := newReviewTest(t)
		if err := reviewStage(ctx, e, ri, "skv_1", "abcdef0123456789abcdef0123456789abcdef01"); err != nil {
			t.Fatal(err)
		}
		row := reviewRow(t, e, "skv_1")
		ageRow(t, e.db, row.ID, "u_owner", e.clock())
		e.admin.mu.Lock()
		delete(e.admin.pending, "src_aaaa")
		e.admin.mu.Unlock()
		e.advance(approvedStaleAfter + time.Minute)
		if err := ri.Sweep(ctx); err != nil {
			t.Fatal(err)
		}
		got := reviewRow(t, e, "skv_1")
		if got.Status != proposalCancelled || !strings.Contains(got.Error, "deleted") || len(e.admin.activates) != 0 {
			t.Fatalf("row %+v, activates %v", got, e.admin.activates)
		}
	})
}

func TestSourceProposalResumesAStoppedApproval(t *testing.T) {
	e, p := newProposalTest(t)
	ctx := context.Background()
	// Stopped after the source was created: the sweep finds it.
	after := proposeTestSource(t, p, "acme/skills")
	ageRow(t, e.db, after.ID, "u_owner", e.clock())
	if _, err := e.admin.CreateSource(ctx, "u_owner", SourceInput{URL: "acme/skills", Branch: "main", Path: "skills", Mode: ModeReview}); err != nil {
		t.Fatal(err)
	}
	// Stopped before: the sweep creates it.
	before := proposeTestSource(t, p, "acme/other")
	ageRow(t, e.db, before.ID, "u_owner", e.clock())
	if err := p.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if len(e.admin.creates) != 1 {
		t.Fatalf("a fresh approval was resumed: %d creates", len(e.admin.creates))
	}
	e.advance(approvedStaleAfter + time.Minute)
	if err := p.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	p.bg.Wait()
	if len(e.admin.creates) != 2 || e.admin.creates[1].URL != "acme/other" || e.admin.createBy[1] != "u_owner" {
		t.Fatalf("creates %+v by %v", e.admin.creates, e.admin.createBy)
	}
	for _, id := range []string{after.ID, before.ID} {
		row, err := p.store.get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if row.Status != proposalDone || row.Result != "src_00000001" {
			t.Fatalf("%s: %+v", id, row)
		}
	}
	if len(e.admin.syncs) != 2 {
		t.Fatalf("first syncs %v", e.admin.syncs)
	}
	// Nothing is applied twice.
	if err := p.Sweep(ctx); err != nil || len(e.admin.creates) != 2 {
		t.Fatalf("second sweep: %v, %d creates", err, len(e.admin.creates))
	}
	// A create that fails now fails the row with the reason.
	broken := proposeTestSource(t, p, "acme/broken")
	ageRow(t, e.db, broken.ID, "u_owner", e.clock())
	e.admin.mu.Lock()
	e.admin.createErr = errors.New("the repository is gone")
	e.admin.mu.Unlock()
	e.advance(approvedStaleAfter + time.Minute)
	if err := p.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	row, err := p.store.get(ctx, broken.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != proposalFailed || !strings.Contains(row.Error, "the repository is gone") {
		t.Fatalf("broken: %+v", row)
	}
}

func TestChangeProposalsAreCappedPerHour(t *testing.T) {
	ctx := context.Background()
	e := newChangeEnv(t)
	change := func(i int) ChangeInput {
		return ChangeInput{Skill: "skills/deploy", Title: fmt.Sprintf("Change %d", i), Files: map[string]string{"notes.md": "x\n"}}
	}
	for i := 0; i < MaxChangeProposalsPerHour; i++ {
		e.decide(t, e.propose(t, change(i)), "usr_admin", reviewReject, actor.ViaDashboard) // none stays waiting
	}
	items := func() int {
		t.Helper()
		var n int
		if err := e.svc.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM inbox_requests`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	n := items()
	if _, err := e.changes.ProposeChange(ctx, changeAgent, change(99)); !errors.Is(err, ErrLimit) || !strings.Contains(err.Error(), "an hour") {
		t.Fatalf("over the hourly cap: %v", err)
	}
	if items() != n {
		t.Fatal("a refused proposal reached the owner's Inbox")
	}
	e.advance(time.Hour + time.Minute)
	e.propose(t, change(100))
}

func TestDeletedSourceClosesItsChangeProposals(t *testing.T) {
	ctx := context.Background()
	t.Run("sweep", func(t *testing.T) {
		e := newChangeEnv(t)
		p := e.propose(t, changeInput4())
		if err := e.svc.DeleteSource(ctx, "usr_admin", e.src.ID); err != nil {
			t.Fatal(err)
		}
		if err := e.changes.Sweep(ctx); err != nil {
			t.Fatal(err)
		}
		if row := e.row(t, p.ID); row.Status != proposalRejected || !strings.Contains(row.Error, "deleted") {
			t.Fatalf("row %+v", row)
		}
		item, err := e.inbox.Get(ctx, p.InboxID)
		if err != nil {
			t.Fatal(err)
		}
		if item.Status != inbox.StatusCancelled {
			t.Fatalf("item %s", item.Status)
		}
		if len(e.git.pulls) != 0 {
			t.Fatalf("pulls %d", len(e.git.pulls))
		}
	})
	t.Run("on delete", func(t *testing.T) {
		e := newChangeEnv(t)
		p := e.propose(t, changeInput4())
		ri, err := NewReviewInbox(ReviewOptions{DB: e.svc.db, Inbox: e.inbox, Admin: e.svc, Now: e.clock})
		if err != nil {
			t.Fatal(err)
		}
		if err := e.svc.DeleteSource(ctx, "usr_admin", e.src.ID); err != nil {
			t.Fatal(err)
		}
		if err := ri.SourceDeleted(ctx, e.src.ID, "the source was deleted"); err != nil {
			t.Fatal(err)
		}
		if row := e.row(t, p.ID); row.Status != proposalRejected || row.Error != "the source was deleted" {
			t.Fatalf("row %+v", row)
		}
		item, err := e.inbox.Get(ctx, p.InboxID)
		if err != nil {
			t.Fatal(err)
		}
		if item.Status != inbox.StatusCancelled {
			t.Fatalf("item %s", item.Status)
		}
	})
}
