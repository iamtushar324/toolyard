package skillhub

// Approval of a skill contributor's source (docs/skills-hub.md
// "Contributors"). A source a contributor adds starts pending approval: it
// syncs, and agents see nothing of it. RequestApproval puts one Inbox
// question for the owner (the review hook's fallback reviewer) with the
// repository, branch, folder, audience, the preview's counts and warnings;
// approve makes the source live (SharingAdmin.DecideApproval, which also
// makes its waiting version active), reject disables it with the reason.
// An admin may decide on the source page instead; the API then closes the
// open item (SettleApproval). The Inbox sweep settles what the handler
// could not (SweepApprovals, run by ReviewInbox.Sweep).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/inbox"
)

// ApprovalItemKind is the Inbox system item kind of a contributor source's
// approval.
const ApprovalItemKind = "skillhub.source_approval"

// proposalKindApproval is the skill_proposals kind of an approval row.
const proposalKindApproval = "approval"

// approvalInput is what an approval row keeps of the source it asks about.
type approvalInput struct {
	SHA         string `json:"sha,omitempty"`
	Skills      int    `json:"skills"`
	Warnings    int    `json:"warnings"`
	Contributor string `json:"contributor,omitempty"`
}

// RequestApproval asks the owner, in the Inbox, to approve a contributor's
// source that waits for approval. prev is the dry run the contributor's
// add was checked with (nil: counts unknown); contributor names who added
// it. A source that already has an open question gets no second one.
func (ri *ReviewInbox) RequestApproval(ctx context.Context, src Source, prev *Preview, contributor string) error {
	if src.Approval != ApprovalPending {
		return fmt.Errorf("%w: source %s does not wait for approval", ErrInvalid, src.Slug)
	}
	reviewer := ri.reviewer(ctx, "")
	if reviewer == "" {
		return fmt.Errorf("%w: there is no owner to approve source %s; an admin can approve it on the Skills page", ErrInvalid, src.Slug)
	}
	open, err := ri.store.pendingForSource(ctx, proposalKindApproval, src.ID)
	if err != nil || len(open) > 0 {
		return err
	}
	in := approvalInput{Contributor: contributor}
	if prev != nil {
		in.SHA, in.Skills, in.Warnings = prev.SHA, prev.SkillCount, len(prev.Warnings)
	}
	stored, _ := json.Marshal(in)
	row := &proposalRow{ID: proposalNewID(), Kind: proposalKindApproval, Status: proposalPending, UserID: reviewer,
		SourceID: src.ID, Input: string(stored)}
	if err := ri.store.insert(ctx, row); err != nil {
		return err
	}
	who := reviewPlain(strings.TrimSpace(contributor))
	if who == "" {
		who = "a skill contributor"
	}
	summary := fmt.Sprintf("%s added %s", who, RepoLabel(src.Owner, src.Repo, in.SHA))
	if prev != nil {
		summary += fmt.Sprintf(": %d skills, %d warning%s", prev.SkillCount, len(prev.Warnings), reviewPlural(len(prev.Warnings)))
	}
	item, err := ri.o.Inbox.CreateSystemItem(ctx, inbox.SystemItem{
		Kind: ApprovalItemKind, Ref: row.ID, UserID: reviewer,
		Title:     reviewClip(reviewPlain("Approve skill source "+src.Owner+"/"+src.Repo), inbox.MaxTitle),
		Summary:   reviewClip(reviewPlain(summary), inbox.MaxSummary),
		Message:   approvalMessage(src, prev, who, ri.o.DashboardURL),
		Questions: reviewQuestion("Let agents see this skill source?", "Approve: make it live", "Reject: disable it"),
		Activity:  "Added by " + who + " on the Skills page",
	})
	if err != nil {
		_ = ri.store.remove(ctx, row.ID)
		return fmt.Errorf("skillhub: approval item for %s: %w", src.Slug, err)
	}
	return ri.store.setInbox(ctx, row.ID, item.ID)
}

// SettleApproval closes the open approval question of a source after an
// admin decided on the Skills page or deleted the source.
func (ri *ReviewInbox) SettleApproval(ctx context.Context, sourceID, reason string) error {
	rows, err := ri.store.pendingForSource(ctx, proposalKindApproval, sourceID)
	if err != nil {
		return err
	}
	return ri.settleRows(ctx, rows, reason)
}

// decideApproval is the Inbox handler of approval items.
func (ri *ReviewInbox) decideApproval(ctx context.Context, r *inbox.Request) (string, error) {
	row, err := ri.store.get(ctx, r.System.Ref)
	if err != nil {
		return "", fmt.Errorf("approval %s: %w", r.System.Ref, err)
	}
	if row.Kind != proposalKindApproval {
		return "", fmt.Errorf("approval %s: not an approval", row.ID)
	}
	if row.Status != proposalPending {
		return "This approval was already settled (" + row.Status + ").", nil
	}
	decider := r.DeciderUserID
	if err := checkDecider(ctx, ri.o.IsAdmin, decider, row.UserID); err != nil {
		_, _ = ri.store.move(ctx, row.ID, []string{proposalPending}, proposalFailed, proposalUpdate{Error: err.Error()})
		return "", err
	}
	sa, ok := ri.o.Admin.(SharingAdmin)
	if !ok {
		_, _ = ri.store.move(ctx, row.ID, []string{proposalPending}, proposalFailed, proposalUpdate{Error: "approvals are not available on this server"})
		return "", errors.New("approvals are not available on this server")
	}
	approve := r.SystemChoice(reviewQuestionID) == reviewApprove
	note := strings.TrimSpace(r.OwnerNote)
	if approve {
		if ok, err := ri.store.move(ctx, row.ID, []string{proposalPending}, proposalApproved, proposalUpdate{DecidedBy: decider}); err != nil || !ok {
			return "This approval was already settled.", err
		}
		return ri.applyApproval(ctx, sa, row, decider, "inbox")
	}
	if note == "" {
		note = "rejected in the Inbox"
	}
	if ok, err := ri.store.move(ctx, row.ID, []string{proposalPending}, proposalRejected, proposalUpdate{DecidedBy: decider}); err != nil || !ok {
		return "This approval was already settled.", err
	}
	src, err := sa.DecideApproval(ctx, decider, row.SourceID, false, note)
	if err != nil {
		_, _ = ri.store.move(ctx, row.ID, []string{proposalRejected}, proposalFailed, proposalUpdate{Error: reviewClip(err.Error(), 500)})
		return "", fmt.Errorf("reject the source: %w", err)
	}
	ri.auditApproval(ctx, "skills.source_reject", decider, row, note)
	return "Skill source " + src.Slug + " was rejected and disabled; agents see nothing of it.", nil
}

// applyApproval makes an approved row's source live and records the
// outcome; the row is approved (the caller won that move or a resume's
// claim).
func (ri *ReviewInbox) applyApproval(ctx context.Context, sa SharingAdmin, row *proposalRow, decider, via string) (string, error) {
	src, err := sa.DecideApproval(ctx, decider, row.SourceID, true, "")
	if err != nil {
		to := proposalFailed
		if errors.Is(err, ErrNotFound) || errors.Is(err, ErrConflict) {
			to = proposalCancelled // deleted, or decided on the Skills page meanwhile
		}
		_, _ = ri.store.move(ctx, row.ID, []string{proposalApproved}, to, proposalUpdate{Error: reviewClip(err.Error(), 500)})
		return "", fmt.Errorf("approve the source: %w", err)
	}
	_, _ = ri.store.move(ctx, row.ID, []string{proposalApproved}, proposalDone, proposalUpdate{Result: src.ID})
	ri.auditApproval(ctx, "skills.source_approve", decider, row, via)
	return "Skill source " + src.Slug + " is live: agents see it by its sharing settings.", nil
}

func (ri *ReviewInbox) auditApproval(ctx context.Context, event, by string, row *proposalRow, reason string) {
	if ri.o.Audit == nil {
		return
	}
	_ = ri.o.Audit.Write(ctx, audit.Event{EventType: event, AgentID: "user:" + by, Reason: reason,
		ResultSummary: row.SourceID, ApprovalID: row.ID, DecidedByUserID: by})
}

// sweepApprovals settles the open approval rows (as Sweep does for
// reviews) and resumes approvals whose process stopped. The first error is
// returned.
func (ri *ReviewInbox) sweepApprovals(ctx context.Context) error {
	rows, err := ri.store.pendingRows(ctx, proposalKindApproval, "", inboxSweepBatch)
	if err != nil {
		return err
	}
	var first error
	for i := range rows {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := ri.sweepApprovalRow(ctx, &rows[i]); err != nil && first == nil {
			first = err
		}
	}
	stale, err := ri.store.staleApproved(ctx, proposalKindApproval, ri.o.Now().Add(-approvedStaleAfter).UnixMilli(), inboxSweepBatch)
	if err != nil {
		if first == nil {
			first = err
		}
		return first
	}
	for i := range stale {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := ri.resumeApproval(ctx, &stale[i]); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// resumeApproval finishes an approved approval row whose process stopped.
func (ri *ReviewInbox) resumeApproval(ctx context.Context, row *proposalRow) error {
	ok, err := ri.store.claim(ctx, row.ID, proposalApproved, row.UpdatedAt)
	if err != nil || !ok {
		return err
	}
	decider := row.DecidedBy
	if decider == "" {
		decider = row.UserID
	}
	src, err := ri.o.Admin.GetSource(ctx, row.SourceID)
	switch {
	case errors.Is(err, ErrNotFound):
		_, err = ri.store.move(ctx, row.ID, []string{proposalApproved}, proposalCancelled, proposalUpdate{Error: "the source was deleted"})
		return err
	case err != nil:
		return err
	case src.Approval == ApprovalApproved:
		_, err = ri.store.move(ctx, row.ID, []string{proposalApproved}, proposalDone, proposalUpdate{Result: src.ID})
		return err
	case src.Approval != ApprovalPending:
		_, err = ri.store.move(ctx, row.ID, []string{proposalApproved}, proposalCancelled, proposalUpdate{Error: "the source was decided on the Skills page"})
		return err
	}
	sa, ok := ri.o.Admin.(SharingAdmin)
	if !ok {
		_, err = ri.store.move(ctx, row.ID, []string{proposalApproved}, proposalFailed, proposalUpdate{Error: "approvals are not available on this server"})
		return err
	}
	_, _ = ri.applyApproval(ctx, sa, row, decider, "inbox, resumed") // records its outcome on the row
	return nil
}

func (ri *ReviewInbox) sweepApprovalRow(ctx context.Context, row *proposalRow) error {
	if row.InboxID == "" {
		if ri.o.Now().UnixMilli()-row.CreatedAt > inboxOrphanAfter.Milliseconds() {
			_, err := ri.store.move(ctx, row.ID, []string{proposalPending}, proposalFailed, proposalUpdate{Error: "the Inbox item was never created"})
			return err
		}
		return nil
	}
	item, err := ri.o.Inbox.Get(ctx, row.InboxID)
	if errors.Is(err, inbox.ErrNotFound) {
		_, err = ri.store.move(ctx, row.ID, []string{proposalPending}, proposalFailed, proposalUpdate{Error: "the Inbox item is gone"})
		return err
	}
	if err != nil {
		return err
	}
	const waits = "; the source still waits for approval on the Skills page"
	switch item.Status {
	case inbox.StatusAnswered, inbox.StatusDenied:
		if lostSystemTag(item, ApprovalItemKind) {
			_, err = ri.store.move(ctx, row.ID, []string{proposalPending}, proposalFailed, proposalUpdate{Error: lostTagError})
			return err
		}
		_, _ = ri.decideApproval(ctx, item) // records its outcome on the row
		return nil
	case inbox.StatusCancelled:
		_, err = ri.store.move(ctx, row.ID, []string{proposalPending}, proposalCancelled, proposalUpdate{Error: "the Inbox item was withdrawn" + waits})
		return err
	case inbox.StatusExpired:
		_, err = ri.store.move(ctx, row.ID, []string{proposalPending}, proposalCancelled, proposalUpdate{Error: "the Inbox item expired without a decision" + waits})
		return err
	case inbox.StatusPending:
		if item.ExpiresAt > 0 && item.ExpiresAt <= ri.o.Now().UnixMilli() {
			_, err = ri.store.move(ctx, row.ID, []string{proposalPending}, proposalCancelled, proposalUpdate{Error: "the Inbox item expired without a decision" + waits})
			return err
		}
		src, err := ri.o.Admin.GetSource(ctx, row.SourceID)
		switch {
		case errors.Is(err, ErrNotFound):
			return ri.settleRows(ctx, []proposalRow{*row}, "the source was deleted")
		case err != nil:
			return err
		case src.Approval != ApprovalPending:
			return ri.settleRows(ctx, []proposalRow{*row}, "the source was decided on the Skills page")
		}
	}
	return nil
}

// approvalMessage is the Markdown body of an approval item.
func approvalMessage(src Source, prev *Preview, who, dashboard string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**%s**, a skill contributor, added a skill source. It syncs, but agents see nothing of it until an admin approves it.\n\n", reviewMD(who))
	fmt.Fprintf(&b, "- Repository: [%s/%s](%s)\n", reviewMD(src.Owner), reviewMD(src.Repo), reviewRepoURL(src.Owner, src.Repo))
	fmt.Fprintf(&b, "- Branch: %s, %s\n", reviewCode(src.Branch), reviewFolder(src.Path))
	fmt.Fprintf(&b, "- Slug: %s\n", reviewCode(src.Slug))
	switch src.Mode {
	case ModeAuto:
		b.WriteString("- Updates: automatic (the repository owner is trusted)\n")
	case ModePinned:
		b.WriteString("- Updates: pinned to one commit\n")
	default:
		b.WriteString("- Updates: each new commit waits for an admin's review\n")
	}
	fmt.Fprintf(&b, "- Who would see it: %s\n", audienceText(src.Visibility, len(src.Members), len(src.Blocked)))
	if prev != nil {
		if prev.SHA != "" {
			fmt.Fprintf(&b, "- Commit checked: [%s](%s)\n", reviewCode(reviewShort(prev.SHA)), reviewCommitURL(src.Owner, src.Repo, prev.SHA))
		}
		fmt.Fprintf(&b, "- Found: %d skills, %d files, %s\n", prev.SkillCount, prev.FileCount, proposalBytes(prev.TotalBytes))
		if len(prev.Warnings) > 0 {
			fmt.Fprintf(&b, "\n**Warnings (%d):**\n\n", len(prev.Warnings))
			for _, g := range proposalWarningGroups(prev.Warnings) {
				fmt.Fprintf(&b, "- %s (%d): %s\n", reviewCode(g.code), len(g.paths), reviewListN(g.paths, proposalWarningCap))
			}
		}
		if len(prev.Sample) > 0 {
			b.WriteString("\n**Some of the skills:**\n\n")
			for _, s := range proposalSample(prev.Sample, proposalSampleCap) {
				fmt.Fprintf(&b, "- %s: %s\n", reviewCode(s.Path), reviewMD(reviewClip(s.Description, 160)))
			}
		}
	}
	if link := reviewSourceLink(dashboard, src.ID); link != "" {
		fmt.Fprintf(&b, "\n[Open the source in toolyard](%s) to read its skills and change who sees them.\n", link)
	}
	b.WriteString("\nApprove makes the source live for the people it is shared with. Reject disables it. You can hide or remove it at any time on the Skills page.")
	return b.String()
}

// audienceText says in words who sees a source.
func audienceText(vis Visibility, members, blocked int) string {
	var out string
	switch vis {
	case VisibleMembers:
		out = fmt.Sprintf("%d chosen member%s and their agents", members, reviewPlural(members))
	case VisibleNobody:
		out = "nobody (admins and the contributor only, in the dashboard)"
	default:
		out = "every member and their agents"
	}
	if blocked > 0 {
		out += fmt.Sprintf(", %d blocked", blocked)
	}
	return out
}
