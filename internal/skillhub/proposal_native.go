package skillhub

// Change proposals for native skills (native.go). A sensitive write by an
// agent, and any skills.propose_change on a native skill, becomes one
// change proposal: the same row, Inbox item kind, decision rules and sweep
// as a GitHub change proposal (proposal_change.go), with one difference in
// what the approval does. Instead of a pull request, the approval applies
// the files the question showed to the collection, on the revision they
// were based on: when the skill moved on since, the proposal fails and the
// agent writes again. Applying is idempotent (the revision records the
// proposal ID), so a resumed approval never writes twice.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/inbox"
)

// nativeProposal is a native write that waits for the collection owner.
type nativeProposal struct {
	src          Source
	uri          string
	skillPath    string
	name         string
	create       bool
	baseRevision int
	files        []changeFile
	title        string // "" makes one from the operation
	note         string
	reasons      []string
}

// proposeNative stores the proposal and asks the collection's creator in
// the Inbox. Nothing changes before they approve.
func (c *ChangeProposals) proposeNative(ctx context.Context, v Viewer, p nativeProposal) (*Proposal, error) {
	switch {
	case v.AgentID == "" || v.UserID == "":
		return nil, fmt.Errorf("%w: only an agent's write waits for approval", ErrInvalid)
	case p.src.CreatedBy == "":
		return nil, fmt.Errorf("%w: the collection %s has no owner who could approve the change", ErrInvalid, p.src.Slug)
	}
	title := changeLine(p.title)
	if title == "" {
		verb := "Update"
		if p.create {
			verb = "Create"
		}
		title = verb + " skill " + p.skillPath
	}
	if utf8.RuneCountInString(title) > MaxChangeTitle {
		title = reviewClip(title, MaxChangeTitle)
	}
	n, err := c.store.countPending(ctx, v.AgentID, proposalKindChange)
	if err != nil {
		return nil, err
	}
	if n >= MaxPendingChangeProposals {
		return nil, fmt.Errorf("%w: you already have %d change proposals waiting; wait for a decision first", ErrConflict, n)
	}
	// The owner approves exactly what the item shows: a diff too large to
	// show in full is refused, never shown in part.
	for _, f := range p.files {
		if f.Op == changeOpDelete {
			continue
		}
		if _, _, _, ok := changeUnifiedDiff(f.Old, f.Content, changeDiffContext); !ok {
			return nil, fmt.Errorf("%w: files: the change of %s is too large to show for approval; write a smaller change", ErrLimit, reviewCode(f.Path))
		}
	}
	id := proposalNewID()
	ci := changeInput{Skill: p.uri, SkillPath: p.skillPath, Name: p.name, Slug: p.src.Slug, Owner: p.src.Owner, Repo: p.src.Repo,
		BaseSHA: p.src.ActiveSHA, AgentOwner: v.UserID, Title: title, Summary: p.note, Files: p.files,
		Native: true, Create: p.create, BaseRevision: p.baseRevision, Reasons: p.reasons}
	agent := c.agentName(ctx, v.AgentID)
	message := nativeChangeMessage(agent, c.userName(ctx, v.UserID), &ci)
	if n := utf8.RuneCountInString(message); n > inbox.MaxMessage {
		return nil, fmt.Errorf("%w: the change is too large to show in full in the owner's Inbox (%d of at most %d characters with its diffs); write fewer or smaller files", ErrLimit, n, inbox.MaxMessage)
	}
	stored, err := json.Marshal(ci)
	if err != nil {
		return nil, err
	}
	allowed, err := c.limit.Allow(ctx, "skillhub-change:"+v.AgentID, MaxChangeProposalsPerHour, time.Hour, c.o.Now())
	if err != nil {
		return nil, fmt.Errorf("skillhub: change proposal rate limit: %w", err)
	}
	if !allowed {
		return nil, fmt.Errorf("%w: at most %d change proposals an hour; try again later", ErrLimit, MaxChangeProposalsPerHour)
	}
	row := &proposalRow{ID: id, Kind: proposalKindChange, Status: proposalPending, AgentID: v.AgentID,
		UserID: p.src.CreatedBy, SourceID: p.src.ID, VersionID: p.src.ActiveVersionID, Input: string(stored), Reason: p.note}
	if err := c.store.insert(ctx, row); err != nil {
		return nil, err
	}
	item, err := c.o.Inbox.CreateSystemItem(ctx, inbox.SystemItem{
		Kind: ChangeProposalItemKind, Ref: row.ID, UserID: p.src.CreatedBy,
		Title:     reviewClip("Skill change: "+title, inbox.MaxTitle),
		Summary:   reviewClip(changeItemSummary(agent, &ci), inbox.MaxSummary),
		Message:   message,
		Questions: reviewQuestion("Apply this change to "+p.uri+"?", "Approve: apply it now", "Reject: change nothing"),
		Activity:  "Written by " + agent + "; it needs your approval",
	})
	if err != nil {
		_, _ = c.store.move(ctx, row.ID, []string{proposalPending}, proposalFailed, proposalUpdate{Error: reviewClip(err.Error(), 500)})
		if errors.Is(err, inbox.ErrTooManyPending) {
			return nil, fmt.Errorf("%w: %v", ErrConflict, err)
		}
		return nil, err
	}
	if err := c.store.setInbox(ctx, row.ID, item.ID); err != nil {
		return nil, err
	}
	row.InboxID = item.ID
	c.audit(ctx, audit.Event{EventType: "skills.native_propose", AgentID: v.AgentID, Reason: title,
		ResultSummary: reviewClip(fmt.Sprintf("%s: %d files; needs approval: %s", p.uri, len(p.files), strings.Join(p.reasons, "; ")), 1000),
		ApprovalID:    row.ID})
	return proposalView(row), nil
}

// routeNative sends skills.propose_change on a native skill to
// proposeNative; ok is false for every other skill.
func (c *ChangeProposals) routeNative(ctx context.Context, v Viewer, in ChangeInput, title, summary string) (*Proposal, bool, error) {
	if c.native == nil || v.AgentID == "" || v.UserID == "" {
		return nil, false, nil
	}
	si, e, err := c.native.resolve(v, strings.TrimSpace(in.Skill))
	if err != nil || !si.src.IsNative() {
		return nil, false, nil
	}
	files, err := c.native.nativeFiles(ctx, si, e, e.path, in.Files)
	if err != nil {
		return nil, true, err
	}
	src := si.src
	reasons := sensitiveReasons(&src, e, files)
	if !canEdit(v, &src) {
		reasons = append([]string{"the agent's owner is not an editor of the collection"}, reasons...)
	}
	if len(reasons) == 0 {
		reasons = []string{"the agent asked for a review (skills.propose_change)"}
	}
	p, err := c.proposeNative(ctx, v, nativeProposal{src: src, uri: si.skillURI(e.path), skillPath: e.path, name: e.name,
		baseRevision: e.revision, files: files, title: title, note: summary, reasons: reasons})
	return p, true, err
}

// finishNative applies an approved native proposal and records the
// outcome: done with the skill's URI and revision, failed with the reason,
// or still approved when the write could not run (the sweep retries; the
// write is applied once).
func (c *ChangeProposals) finishNative(ctx context.Context, row *proposalRow, ci *changeInput) (string, error) {
	if c.native == nil {
		msg := "this server cannot apply native skill changes"
		_, _ = c.store.changeSetOutcome(ctx, row.ID, proposalApproved, proposalFailed, "", msg)
		return "", errors.New(msg)
	}
	// The approval is the collection owner's: it counts only while that
	// person still owns the collection.
	src, err := c.o.Admin.GetSource(ctx, row.SourceID)
	switch {
	case errors.Is(err, ErrNotFound):
		msg := "the collection was deleted; nothing changed"
		_, _ = c.store.changeSetOutcome(ctx, row.ID, proposalApproved, proposalFailed, "", msg)
		return "", errors.New(msg)
	case err != nil:
		msg := reviewClip(err.Error()+"; toolyard tries again within a few minutes", 500)
		_, _ = c.store.changeSetOutcome(ctx, row.ID, proposalApproved, proposalApproved, "", msg)
		return "", errors.New(msg)
	case src.CreatedBy != row.UserID || row.DecidedBy == "" || row.DecidedBy != row.UserID:
		msg := "the collection has another owner now, or the approval is not its owner's; nothing changed, ask the agent to write again"
		_, _ = c.store.changeSetOutcome(ctx, row.ID, proposalApproved, proposalFailed, "", msg)
		return "", errors.New(msg)
	}
	base := ci.BaseRevision
	if !ci.Create && base == 0 {
		base = -1 // the skill had no recorded revision: it must still have none
	}
	done, err := c.native.applyNative(ctx, nativeApply{sourceID: row.SourceID, skillPath: ci.SkillPath, create: ci.Create,
		files: ci.Files, baseRevision: base, author: ci.AgentOwner, agent: row.AgentID, note: ci.Summary, proposalID: row.ID})
	if err != nil {
		msg := reviewClip(err.Error(), 500)
		if errors.Is(err, ErrConflict) || errors.Is(err, ErrInvalid) || errors.Is(err, ErrNotFound) || errors.Is(err, ErrLimit) {
			msg = reviewClip(strings.TrimPrefix(strings.TrimPrefix(err.Error(), ErrConflict.Error()+": "), ErrInvalid.Error()+": ")+
				"; nothing changed, ask the agent to write again", 500)
			_, _ = c.store.changeSetOutcome(ctx, row.ID, proposalApproved, proposalFailed, "", msg)
			c.audit(ctx, audit.Event{EventType: "skills.change_proposal_fail", AgentID: row.AgentID, Reason: msg, ApprovalID: row.ID,
				DecidedByUserID: row.DecidedBy})
			return "", errors.New(msg)
		}
		msg = reviewClip(err.Error()+"; toolyard tries again within a few minutes", 500)
		_, _ = c.store.changeSetOutcome(ctx, row.ID, proposalApproved, proposalApproved, "", msg)
		return "", errors.New(msg)
	}
	result := fmt.Sprintf("%s revision %d", ci.Skill, done.revision)
	if _, err := c.store.changeSetOutcome(ctx, row.ID, proposalApproved, proposalDone, result, ""); err != nil {
		return "", err
	}
	c.audit(ctx, audit.Event{EventType: "skills.native_apply", AgentID: row.AgentID, Reason: "approved proposal " + row.ID,
		ResultSummary: result, ApprovalID: row.ID, DecidedByUserID: row.DecidedBy})
	return "Applied: " + result + ".", nil
}

// nativeChangeMessage is the Markdown body of a native proposal's Inbox
// item: why it needs approval and everything the approval writes.
func nativeChangeMessage(agent, agentOwner string, ci *changeInput) string {
	var b strings.Builder
	fmt.Fprintf(&b, "The agent **%s**", reviewMD(agent))
	if agentOwner != "" {
		fmt.Fprintf(&b, " (owner: %s)", reviewMD(agentOwner))
	}
	verb := "changes"
	if ci.Create {
		verb = "creates"
	}
	fmt.Fprintf(&b, " %s the skill %s in your native skill collection **%s**.\n\n", verb, changeCode(ci.SkillPath), reviewMD(ci.Slug))
	if len(ci.Reasons) > 0 {
		b.WriteString("**Why it needs your approval:**\n\n")
		for _, r := range ci.Reasons {
			fmt.Fprintf(&b, "- %s\n", reviewMD(r))
		}
		b.WriteString("\n")
	}
	if ci.Create {
		b.WriteString("- A new skill\n")
	} else if ci.BaseRevision > 0 {
		fmt.Fprintf(&b, "- Based on revision %d, the version agents see now\n", ci.BaseRevision)
	}
	fmt.Fprintf(&b, "- Changes: %s\n\n", changeCounts(ci.Files))
	fmt.Fprintf(&b, "**Title:** %s\n\n", reviewMD(ci.Title))
	if ci.Summary != "" {
		b.WriteString("**Note, in the agent's words:**\n\n")
		b.WriteString(changeQuote(ci.Summary))
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "**Every file it writes (%d):**\n\n", len(ci.Files))
	for _, f := range ci.Files {
		fmt.Fprintf(&b, "- %s: %s\n", changeCode(repoPath(ci.SkillPath, f.Path)), changeOpLabel(f.Op))
	}
	b.WriteString("\n**The complete diff:**\n\n")
	for _, f := range ci.Files {
		b.WriteString(changeFileSection(f))
	}
	b.WriteString("Approve applies exactly these files at once: every agent that can see the skill reads the new revision. ")
	b.WriteString("If the skill changed since this question was asked, nothing is applied and the agent must write again. Reject changes nothing.")
	return b.String()
}
