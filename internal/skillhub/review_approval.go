package skillhub

// Approval of a skill contributor's source (docs/skills-hub.md
// "Contributors"). A source a contributor adds starts pending approval: it
// syncs, every new commit waits (whatever the update mode), and agents see
// nothing of it. RequestApproval puts one Inbox question for the owner
// (the review hook's fallback reviewer) with the repository, branch,
// folder, audience, the commit and its counts and warnings; when a newer
// commit comes to wait, the question is asked again for it
// (PendingApprovalVersion). Approve makes the source live, bound to the
// question's commit (SharingAdmin.DecideApprovalFrom, which makes that
// waiting version active and refuses when another commit would go live);
// reject disables it with the reason. Both apply only while the source
// still waits. An admin may decide on the source page instead; the API
// then closes the open item (SettleApproval). The Inbox sweep settles what
// the handler could not (SweepApprovals, run by ReviewInbox.Sweep).

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
// Version is the waiting version the question shows ("" for the dry run
// of a new or changed source) and Config the settings it shows
// (sourceRow.configHash): the approval applies only to a waiting version
// they match (ApprovalBinding).
type approvalInput struct {
	SHA         string `json:"sha,omitempty"`
	Skills      int    `json:"skills"`
	Warnings    int    `json:"warnings"`
	Contributor string `json:"contributor,omitempty"`
	Version     string `json:"version,omitempty"`
	Config      string `json:"config,omitempty"`
}

func parseApprovalInput(raw string) approvalInput {
	var in approvalInput
	_ = json.Unmarshal([]byte(raw), &in)
	return in
}

// binding is what approving the question approves.
func (in approvalInput) binding() ApprovalBinding {
	return ApprovalBinding{SHA: in.SHA, Version: in.Version, Config: in.Config}
}

// currentFor reports whether the question shows waiting version w of a
// source whose settings are cfg (w built with them): it names w, or it is
// the dry run of w's commit with the same settings.
func (in approvalInput) currentFor(w *versionRow, cfg string) bool {
	switch {
	case w == nil || w.config != cfg:
		return false
	case in.Version != "":
		return in.Version == w.ID
	}
	return in.Config == cfg && strings.EqualFold(in.SHA, w.SHA)
}

// approvalSubject is the version approving a contributor's source would
// make (or keep) live: the waiting one, else the live one an admin made
// live while the source waits.
func approvalSubject(waiting, live *versionRow) *versionRow {
	if waiting != nil {
		return waiting
	}
	return live
}

// subjectFor is the version an approval question about cur may show now:
// its waiting version when built with cur's settings, else (nothing
// waiting) its live version when approving it is allowed (liveApproval;
// in review mode, when built with cur's settings). nil when there is none.
func subjectFor(cur *sourceRow, waiting, live, next *versionRow) *versionRow {
	cfg := cur.configHash()
	switch {
	case waiting != nil && waiting.config == cfg:
		return waiting
	case waiting != nil || live == nil:
		return nil
	case cur.Mode == ModeReview && live.config == cfg:
		return live
	case cur.Mode != ModeReview && liveApproval(cur, live, next) == nil:
		return live
	}
	return nil
}

// versionPreview is the counts and warnings of version v, as an approval
// question shows them.
func versionPreview(src Source, v *versionRow) *Preview {
	src.PendingVersionID = v.ID
	return &Preview{Source: src, SHA: v.SHA, SkillCount: v.SkillCount, FileCount: v.FileCount, TotalBytes: v.TotalBytes,
		Warnings: nonNilWarnings(v.Warnings), Sample: []SkillSummary{}}
}

// RequestApproval asks the owner, in the Inbox, to approve a contributor's
// source that waits for approval. prev is the dry run the contributor's
// add (or change) was checked with: it must be of the settings the source
// has now, else nothing is asked (a newer change asks). nil asks about the
// waiting version, else the live one (when it was built with the source's
// current settings; one built with older settings asks nothing: the check
// that stages the current version asks), and with unknown counts when the
// source has neither. contributor names who added it. A source that
// already has an open question (waiting, or an answer being applied) gets
// no second one, on any replica. The question shows the source as it is
// when it is asked.
func (ri *ReviewInbox) RequestApproval(ctx context.Context, src Source, prev *Preview, contributor string) error {
	if src.Approval != ApprovalPending {
		return fmt.Errorf("%w: source %s does not wait for approval", ErrInvalid, src.Slug)
	}
	reviewer := ri.reviewer(ctx, "")
	if reviewer == "" {
		return fmt.Errorf("%w: there is no owner to approve source %s; an admin can approve it on the Skills page", ErrInvalid, src.Slug)
	}
	var shown Source
	var card *Preview
	row, err := ri.store.lockedApproval(ctx, src.ID, func(cur *sourceRow, w, live, next *versionRow, open []proposalRow) *proposalRow {
		if cur.Approval != ApprovalPending || len(open) > 0 {
			return nil
		}
		cfg := cur.configHash()
		in := approvalInput{Contributor: contributor}
		subject := subjectFor(cur, w, live, next)
		switch {
		case prev != nil:
			if configOf(prev.Source) != cfg || prev.Source.Branch != cur.Branch || prev.Source.PinnedSHA != cur.PinnedSHA {
				return nil // a dry run of a branch or settings the source no longer has
			}
			in.SHA, in.Skills, in.Warnings, in.Config = prev.SHA, prev.SkillCount, len(prev.Warnings), cfg
			card = prev
		case subject != nil:
			in.SHA, in.Skills, in.Warnings, in.Version, in.Config = subject.SHA, subject.SkillCount, len(subject.Warnings), subject.ID, cfg
			card = versionPreview(cur.Source, subject)
		case approvalSubject(w, live) != nil:
			return nil // not approvable now (older branch or settings): the check that stages the current version asks
		}
		shown = cur.Source
		return ri.approvalRow(reviewer, cur.ID, in)
	})
	if err != nil || row == nil {
		return err
	}
	return ri.approvalItem(ctx, row, shown, card, contributor)
}

// approvalRow is a new pending approval row about source sourceID.
func (ri *ReviewInbox) approvalRow(reviewer, sourceID string, in approvalInput) *proposalRow {
	stored, _ := json.Marshal(in)
	return &proposalRow{ID: proposalNewID(), Kind: proposalKindApproval, Status: proposalPending, UserID: reviewer,
		SourceID: sourceID, Input: string(stored)}
}

// approvalItem creates the Inbox item of a new approval row, which shows
// src and prev (nil: counts unknown). When the item cannot be created the
// row is removed: the next ask tries again.
func (ri *ReviewInbox) approvalItem(ctx context.Context, row *proposalRow, src Source, prev *Preview, contributor string) error {
	in := parseApprovalInput(row.Input)
	who := reviewPlain(strings.TrimSpace(contributor))
	if who == "" {
		who = "a skill contributor"
	}
	summary := fmt.Sprintf("%s added %s", who, RepoLabel(src.Owner, src.Repo, in.SHA))
	if prev != nil {
		summary += fmt.Sprintf(": %d skills, %d warning%s", prev.SkillCount, len(prev.Warnings), reviewPlural(len(prev.Warnings)))
	}
	item, err := ri.o.Inbox.CreateSystemItem(ctx, inbox.SystemItem{
		Kind: ApprovalItemKind, Ref: row.ID, UserID: row.UserID,
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
	ok, err := ri.store.setInboxIfPending(ctx, row.ID, item.ID)
	if ok && err == nil {
		return nil
	}
	if err != nil {
		// The item could not be recorded on the row: drop the row, so the
		// next check, which finds no open question, asks again.
		_ = ri.store.remove(ctx, row.ID)
	}
	// The row was closed while its item was being created, or the item is
	// recorded nowhere: withdraw the item, which nobody else knows of.
	if _, cerr := ri.o.Inbox.CancelBySystem(ctx, inbox.SystemAgentID(row.UserID), item.ID, "this question was closed while it was asked"); cerr != nil &&
		!errors.Is(cerr, inbox.ErrNotPending) && !errors.Is(cerr, inbox.ErrNotFound) && err == nil {
		err = cerr
	}
	return err
}

var _ ApprovalHook = (*ReviewInbox)(nil)

// PendingApprovalVersion follows a contributor's source that waits for
// approval to its waiting version v (the sync engine calls it whenever a
// sync stages a version, or finds one still waiting, so at every check).
// src and v are what the caller read and may be outdated: under the
// source's row lock (lockedApproval) the source is read again, and nothing
// is asked unless the caller read the source's current branch and pin, and
// v is still the version a question may show (subjectFor: its waiting
// version built with the settings it has now, or, with nothing waiting,
// its live version when approving it is allowed); else the check that
// reads the current version asks. An
// approval is bound to the question's version, so the source keeps one
// open question that shows its waiting version, as it is now:
//
//   - while an answer is being applied, nothing changes (its outcome asks
//     again when it cannot apply, applyApproval);
//   - when an open question shows the waiting version, every other open
//     question (another commit or older settings, left over by a crash) is
//     closed;
//   - otherwise a question about the waiting version is asked first, and
//     the open ones closed only once it exists, so a failure leaves them
//     open and the next check tries again; with no open question (the
//     first ask failed, it expired, an answer could not apply) one is
//     asked.
func (ri *ReviewInbox) PendingApprovalVersion(ctx context.Context, src Source, v Version) error {
	if src.Approval != ApprovalPending || v.ID == "" {
		return nil
	}
	reviewer := ri.reviewer(ctx, "")
	if reviewer == "" {
		return fmt.Errorf("%w: there is no owner to approve source %s; an admin can approve it on the Skills page", ErrInvalid, src.Slug)
	}
	// Who added it, as the last question named them, read before the
	// lock (decide must not use the database).
	fallback := ""
	if last, err := ri.store.latestForSource(ctx, proposalKindApproval, src.ID); err == nil && last != nil {
		fallback = parseApprovalInput(last.Input).Contributor
	}
	if fallback == "" && ri.o.UserLabel != nil {
		fallback = ri.o.UserLabel(ctx, src.CreatedBy)
	}
	var (
		replace []proposalRow
		shown   Source
		card    *Preview
		who     string
	)
	row, err := ri.store.lockedApproval(ctx, src.ID, func(cur *sourceRow, waiting, live, next *versionRow, open []proposalRow) *proposalRow {
		cfg := cur.configHash()
		w := subjectFor(cur, waiting, live, next)
		if cur.Approval != ApprovalPending || src.Branch != cur.Branch || src.PinnedSHA != cur.PinnedSHA || w == nil || w.ID != v.ID {
			return nil
		}
		for _, r := range open {
			if r.Status != proposalPending {
				return nil
			}
		}
		current := false
		for _, r := range open {
			in := parseApprovalInput(r.Input)
			if in.currentFor(w, cfg) && !current {
				current = true
				continue
			}
			if who == "" {
				who = in.Contributor
			}
			replace = append(replace, r)
		}
		if current {
			return nil
		}
		if who == "" {
			who = fallback
		}
		shown, card = cur.Source, versionPreview(cur.Source, w)
		return ri.approvalRow(reviewer, cur.ID, approvalInput{SHA: w.SHA, Skills: w.SkillCount, Warnings: len(w.Warnings),
			Contributor: who, Version: w.ID, Config: cfg})
	})
	if err != nil {
		return err
	}
	if row != nil {
		if err := ri.approvalItem(ctx, row, shown, card, who); err != nil {
			return err
		}
	}
	if len(replace) == 0 {
		return nil
	}
	return ri.settleRows(ctx, replace, "the source waits with another version now: commit "+reviewShort(v.SHA))
}

// askAgainForWaiting asks the approval question again, for its waiting
// version (else its live one, which an admin made live while it waits), of
// a source that still waits after an answer could not apply (another
// commit or other settings would go live). With no such version (nothing
// synced yet), or one built with settings the source no longer has,
// nothing is asked now: the sync that stages the current version asks
// (PendingApprovalVersion; a settings change makes that check due). A
// failure is left to the next check, which asks when no question is open.
func (ri *ReviewInbox) askAgainForWaiting(ctx context.Context, sourceID string) {
	src, err := ri.o.Admin.GetSource(ctx, sourceID)
	if err != nil || src.Approval != ApprovalPending {
		return
	}
	id := src.PendingVersionID
	if id == "" {
		id = src.ActiveVersionID
	}
	if id == "" {
		return
	}
	v, err := ri.o.Admin.GetVersion(ctx, sourceID, id)
	if err != nil {
		return
	}
	_ = ri.PendingApprovalVersion(ctx, *src, *v)
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
	// Only a source that still waits: one decided on the Skills page
	// meanwhile keeps that decision.
	src, err := sa.DecideApprovalFrom(ctx, decider, row.SourceID, ApprovalPending, false, note, ApprovalBinding{})
	if err != nil {
		to := proposalFailed
		if errors.Is(err, ErrNotFound) || errors.Is(err, ErrConflict) {
			to = proposalCancelled // deleted, or decided on the Skills page meanwhile
		}
		_, _ = ri.store.move(ctx, row.ID, []string{proposalRejected}, to, proposalUpdate{Error: reviewClip(err.Error(), 500)})
		return "", fmt.Errorf("reject the source: %w", err)
	}
	ri.auditApproval(ctx, "skills.source_reject", decider, row, note)
	return "Skill source " + src.Slug + " was rejected and disabled; agents see nothing of it.", nil
}

// applyApproval makes an approved row's source live and records the
// outcome; the row is approved (the caller won that move or a resume's
// claim). The approval is bound to the commit the question showed, and
// applies only while the source still waits: a source an admin rejected
// (or approved) on the Skills page meanwhile keeps that decision, and the
// row is cancelled.
func (ri *ReviewInbox) applyApproval(ctx context.Context, sa SharingAdmin, row *proposalRow, decider, via string) (string, error) {
	in := parseApprovalInput(row.Input)
	src, err := sa.DecideApprovalFrom(ctx, decider, row.SourceID, ApprovalPending, true, "", in.binding())
	if err != nil {
		to := proposalFailed
		if errors.Is(err, ErrNotFound) || errors.Is(err, ErrConflict) {
			to = proposalCancelled // deleted, decided on the Skills page, or another commit would go live
		}
		_, _ = ri.store.move(ctx, row.ID, []string{proposalApproved}, to, proposalUpdate{Error: reviewClip(err.Error(), 500)})
		if errors.Is(err, ErrConflict) {
			// A source that still waits (the answer named another version
			// than the one that would go live) is asked again, for the
			// version it waits with now: its question is never lost.
			ri.askAgainForWaiting(ctx, row.SourceID)
		}
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
