package skillhub

// SourceProposals implements Proposals for skill sources: an agent's
// skills.propose_source becomes one Inbox question for the agent's owner,
// with a dry run of the source (Admin.Preview), and the owner's approval
// creates the source (Admin.CreateSource) and runs its first sync.
// ProposeChange goes to ChangeProposals (slice D) once it is set.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/inbox"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// SourceProposalItemKind is the Inbox system item kind of a source
// proposal.
const SourceProposalItemKind = "skillhub.source_proposal"

// Limits on source proposals.
const (
	// MaxProposalReason is the longest reason an agent may give.
	MaxProposalReason = 2000
	// MaxPendingSourceProposals is how many source proposals one agent may
	// have waiting at once.
	MaxPendingSourceProposals = 5
	// proposalSampleCap and proposalWarningCap bound what an item lists.
	proposalSampleCap  = 10
	proposalWarningCap = 5
)

// ErrChangeProposalsUnavailable answers skills.propose_change until change
// proposals ship.
var ErrChangeProposalsUnavailable = errors.New("skillhub: change proposals are not available yet")

// ProposalOptions configure SourceProposals.
type ProposalOptions struct {
	DB    *store.DB
	Inbox *inbox.Service
	Admin Admin
	// IsAdmin reports whether a user holds the admin role: only an admin's
	// agent may propose a source, and only while its owner still is an
	// admin may the approval create it. nil skips the check at approval
	// (the Viewer's Admin flag still applies when the agent proposes).
	IsAdmin func(ctx context.Context, userID string) bool
	// AgentName names an agent in the item ("" falls back to its ID).
	AgentName func(ctx context.Context, agentID string) string
	// Audit, when set, records every proposal and decision.
	Audit *audit.Logger
	Now   func() time.Time
}

// SourceProposals takes agents' source proposals. Create it with
// NewSourceProposals.
type SourceProposals struct {
	o     ProposalOptions
	store proposalStore
	// changes takes skills.propose_change (slice D, proposal_change.go);
	// nil until SetChangeProposals.
	changes *ChangeProposals
}

var _ Proposals = (*SourceProposals)(nil)

// NewSourceProposals registers the proposal item handler with the Inbox.
func NewSourceProposals(o ProposalOptions) (*SourceProposals, error) {
	if o.DB == nil || o.Inbox == nil || o.Admin == nil {
		return nil, errors.New("skillhub: source proposals need a database, the Inbox and the admin service")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	p := &SourceProposals{o: o, store: proposalStore{db: o.DB, now: o.Now}}
	o.Inbox.HandleSystem(SourceProposalItemKind, p.decide)
	return p, nil
}

// ProposeSource dry-runs the proposed source and asks the agent's owner in
// the Inbox. Only an agent whose owner is an admin may propose; a proposal
// never carries a token (an admin adds a private repository with its
// token on the Skills page).
func (p *SourceProposals) ProposeSource(ctx context.Context, v Viewer, in SourceInput, reason string) (*Proposal, error) {
	reason = strings.TrimSpace(reason)
	in.URL = strings.TrimSpace(in.URL)
	switch {
	case v.AgentID == "" || v.UserID == "":
		return nil, fmt.Errorf("%w: only an agent can propose a skill source", ErrInvalid)
	case !v.Admin:
		return nil, fmt.Errorf("%w: only an agent whose owner is an admin can propose a skill source; ask an admin to add it on the Skills page", ErrInvalid)
	case in.URL == "":
		return nil, fmt.Errorf("%w: url is required", ErrInvalid)
	case reason == "":
		return nil, fmt.Errorf("%w: reason is required: say why your owner should add this source", ErrInvalid)
	case utf8.RuneCountInString(reason) > MaxProposalReason:
		return nil, fmt.Errorf("%w: reason is longer than %d characters", ErrInvalid, MaxProposalReason)
	case in.Token != "" || in.Auth == AuthToken:
		return nil, fmt.Errorf("%w: a proposal cannot carry a token; an admin adds a private repository and its token on the Skills page", ErrInvalid)
	}
	n, err := p.store.countPending(ctx, v.AgentID, proposalKindSource)
	if err != nil {
		return nil, err
	}
	if n >= MaxPendingSourceProposals {
		return nil, fmt.Errorf("%w: you already have %d source proposals waiting for your owner", ErrConflict, n)
	}
	prev, err := p.o.Admin.Preview(ctx, v.UserID, in)
	if err != nil {
		return nil, err
	}
	// The owner approves exactly what was previewed: the branch and the
	// folder are fixed now (the commit is not: the source follows its
	// branch).
	if in.Branch == "" {
		in.Branch = prev.Source.Branch
	}
	if in.Path == "" {
		in.Path = prev.Source.Path
	}
	stored, _ := json.Marshal(in)
	row := &proposalRow{ID: proposalNewID(), Kind: proposalKindSource, Status: proposalPending, AgentID: v.AgentID,
		UserID: v.UserID, Input: string(stored), Reason: reason}
	if err := p.store.insert(ctx, row); err != nil {
		return nil, err
	}
	name := p.agentName(ctx, v.AgentID)
	repo := prev.Source.Owner + "/" + prev.Source.Repo
	item, err := p.o.Inbox.CreateSystemItem(ctx, inbox.SystemItem{
		Kind: SourceProposalItemKind, Ref: row.ID, AgentID: v.AgentID,
		Title: reviewClip("Add skill source "+repo, inbox.MaxTitle),
		Summary: reviewClip(fmt.Sprintf("%s proposes %s: %d skills, %d warning%s", name, RepoLabel(prev.Source.Owner, prev.Source.Repo, prev.SHA),
			prev.SkillCount, len(prev.Warnings), reviewPlural(len(prev.Warnings))), inbox.MaxSummary),
		Message:   p.message(ctx, name, prev, reason),
		Questions: reviewQuestion("Add this skill source?", "Approve: add it and sync now", "Reject"),
		Activity:  "Proposed by " + name + " with skills.propose_source",
	})
	if err != nil {
		_, _ = p.store.move(ctx, row.ID, []string{proposalPending}, proposalFailed, proposalUpdate{Error: reviewClip(err.Error(), 500)})
		if errors.Is(err, inbox.ErrTooManyPending) {
			return nil, fmt.Errorf("%w: %v", ErrConflict, err)
		}
		return nil, err
	}
	if err := p.store.setInbox(ctx, row.ID, item.ID); err != nil {
		return nil, err
	}
	row.InboxID = item.ID
	p.audit(ctx, audit.Event{EventType: "skills.source_propose", AgentID: v.AgentID, Reason: reason,
		ResultSummary: RepoLabel(prev.Source.Owner, prev.Source.Repo, prev.SHA), ApprovalID: row.ID})
	return proposalView(row), nil
}

// ProposeChange hands the change to ChangeProposals (SetChangeProposals);
// without it every change is refused.
func (p *SourceProposals) ProposeChange(ctx context.Context, v Viewer, in ChangeInput) (*Proposal, error) {
	if p.changes == nil {
		return nil, ErrChangeProposalsUnavailable
	}
	return p.changes.ProposeChange(ctx, v, in)
}

// GetProposal returns one of the calling agent's proposals (or, for a
// dashboard viewer, one sent to that person). A proposal whose Inbox item
// was decided, withdrawn or expired while toolyard could not act on it is
// settled now.
func (p *SourceProposals) GetProposal(ctx context.Context, v Viewer, id string) (*Proposal, error) {
	row, err := p.store.get(ctx, strings.TrimSpace(id))
	if err != nil {
		return nil, err
	}
	if row.Kind == proposalKindChange && p.changes != nil {
		return p.changes.GetProposal(ctx, v, row.ID)
	}
	mine := (v.AgentID != "" && row.AgentID == v.AgentID) || (v.AgentID == "" && v.UserID != "" && row.UserID == v.UserID)
	if row.Kind != proposalKindSource || !mine {
		return nil, ErrNotFound
	}
	if row.Status == proposalPending && row.InboxID != "" {
		if err := p.reconcile(ctx, row); err != nil {
			return nil, err
		}
		if row, err = p.store.get(ctx, row.ID); err != nil {
			return nil, err
		}
	}
	return proposalView(row), nil
}

// reconcile settles a pending proposal from its Inbox item.
func (p *SourceProposals) reconcile(ctx context.Context, row *proposalRow) error {
	item, err := p.o.Inbox.Get(ctx, row.InboxID)
	if errors.Is(err, inbox.ErrNotFound) {
		_, err = p.store.move(ctx, row.ID, []string{proposalPending}, proposalFailed, proposalUpdate{Error: "the Inbox item is gone"})
		return err
	}
	if err != nil {
		return err
	}
	switch item.Status {
	case inbox.StatusAnswered, inbox.StatusDenied:
		if item.System == nil {
			return nil
		}
		_, _ = p.decide(ctx, item) // records its own outcome on the row
	case inbox.StatusCancelled:
		_, err = p.store.move(ctx, row.ID, []string{proposalPending}, proposalRejected, proposalUpdate{Error: "the Inbox item was withdrawn"})
	case inbox.StatusExpired:
		_, err = p.store.move(ctx, row.ID, []string{proposalPending}, proposalRejected, proposalUpdate{Error: "the Inbox item expired without a decision"})
	}
	return err
}

// decide is the Inbox handler of source proposals.
func (p *SourceProposals) decide(ctx context.Context, r *inbox.Request) (string, error) {
	row, err := p.store.get(ctx, r.System.Ref)
	if err != nil {
		return "", fmt.Errorf("proposal %s: %w", r.System.Ref, err)
	}
	if row.Kind != proposalKindSource {
		return "", fmt.Errorf("proposal %s: not a source proposal", row.ID)
	}
	if row.Status != proposalPending {
		return "This proposal was already settled (" + row.Status + ").", nil
	}
	decider := r.DeciderUserID
	if err := checkDecider(ctx, p.o.IsAdmin, decider, row.UserID); err != nil {
		_, _ = p.store.move(ctx, row.ID, []string{proposalPending}, proposalFailed, proposalUpdate{Error: err.Error()})
		return "", err
	}
	if r.SystemChoice(reviewQuestionID) != reviewApprove {
		why := "rejected by your owner"
		if note := strings.TrimSpace(r.OwnerNote); note != "" {
			why += ": " + reviewClip(note, 500)
		}
		if ok, err := p.store.move(ctx, row.ID, []string{proposalPending}, proposalRejected, proposalUpdate{DecidedBy: decider, Error: why}); err != nil || !ok {
			return "This proposal was already settled.", err
		}
		p.audit(ctx, audit.Event{EventType: "skills.source_proposal_reject", AgentID: row.AgentID, Reason: r.OwnerNote,
			ApprovalID: row.ID, DecidedByUserID: decider})
		return "Proposal rejected; nothing was added.", nil
	}
	if ok, err := p.store.move(ctx, row.ID, []string{proposalPending}, proposalApproved, proposalUpdate{DecidedBy: decider}); err != nil || !ok {
		return "This proposal was already settled.", err
	}
	var in SourceInput
	if err := json.Unmarshal([]byte(row.Input), &in); err != nil {
		_, _ = p.store.move(ctx, row.ID, []string{proposalApproved}, proposalFailed, proposalUpdate{Error: "the stored proposal is unreadable"})
		return "", fmt.Errorf("proposal %s: %w", row.ID, err)
	}
	if in.Mode == "" {
		in.Mode = ModeReview
		if ref, err := p.o.Admin.ParseURL(in.URL); err == nil && p.o.Admin.TrustedOwner(ctx, ref.Owner) {
			in.Mode = ModeAuto
		}
	}
	src, err := p.o.Admin.CreateSource(ctx, decider, in)
	if err != nil {
		_, _ = p.store.move(ctx, row.ID, []string{proposalApproved}, proposalFailed, proposalUpdate{Error: reviewClip(err.Error(), 500)})
		return "", fmt.Errorf("add the source: %w", err)
	}
	_, _ = p.store.move(ctx, row.ID, []string{proposalApproved}, proposalDone, proposalUpdate{Result: src.ID})
	p.audit(ctx, audit.Event{EventType: "skills.source_create", AgentID: row.AgentID, Reason: "approved proposal " + row.ID,
		ResultSummary: src.ID + " " + RepoLabel(src.Owner, src.Repo, ""), ApprovalID: row.ID, DecidedByUserID: decider})
	note := fmt.Sprintf("Added skill source %s (%s, mode %s).", src.Slug, src.ID, src.Mode)
	res, err := p.o.Admin.SyncNow(ctx, src.ID)
	switch {
	case err != nil:
		note += " The first sync failed: " + reviewClip(err.Error(), 300)
	case res.Error != "":
		note += " The first sync failed: " + reviewClip(res.Error, 300)
	case res.Staged:
		note += " The first version " + reviewShort(res.SHA) + " waits for review on the Skills page."
	default:
		note += " The first sync copied commit " + reviewShort(res.SHA) + "."
	}
	return note, nil
}

func (p *SourceProposals) agentName(ctx context.Context, agentID string) string {
	if p.o.AgentName != nil {
		if n := strings.TrimSpace(p.o.AgentName(ctx, agentID)); n != "" {
			return n
		}
	}
	return "agent " + agentID
}

func (p *SourceProposals) audit(ctx context.Context, ev audit.Event) {
	if p.o.Audit != nil {
		_ = p.o.Audit.Write(ctx, ev)
	}
}

// message is the Markdown body of a proposal item.
func (p *SourceProposals) message(ctx context.Context, agent string, prev *Preview, reason string) string {
	src := prev.Source
	mode := src.Mode
	if mode == "" {
		mode = ModeReview
		if p.o.Admin.TrustedOwner(ctx, src.Owner) {
			mode = ModeAuto
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Your agent **%s** proposes a skill source. toolyard would copy the skills in this repository into its database, keep them current, and serve them to agents.\n\n", reviewMD(agent))
	fmt.Fprintf(&b, "- Repository: [%s/%s](%s)\n", reviewMD(src.Owner), reviewMD(src.Repo), reviewRepoURL(src.Owner, src.Repo))
	fmt.Fprintf(&b, "- Branch: %s, %s\n", reviewCode(src.Branch), reviewFolder(src.Path))
	if prev.SHA != "" {
		fmt.Fprintf(&b, "- Commit now: [%s](%s)\n", reviewCode(reviewShort(prev.SHA)), reviewCommitURL(src.Owner, src.Repo, prev.SHA))
	}
	fmt.Fprintf(&b, "- Found: %d skills, %d files, %s\n", prev.SkillCount, prev.FileCount, proposalBytes(prev.TotalBytes))
	switch mode {
	case ModeAuto:
		b.WriteString("- Updates: automatic (the repository owner is trusted)\n")
	case ModePinned:
		b.WriteString("- Updates: pinned to one commit\n")
	default:
		b.WriteString("- Updates: each new commit waits for your review\n")
	}
	if src.Visibility == VisibleMembers {
		fmt.Fprintf(&b, "- Who sees it: %d chosen members\n", len(src.Members))
	} else {
		b.WriteString("- Who sees it: every member and their agents\n")
	}
	b.WriteString("\n**Why, in the agent's words:**\n\n")
	for _, line := range strings.Split(reason, "\n") {
		b.WriteString("> " + line + "\n")
	}
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
	b.WriteString("\nApprove adds the source and runs its first sync. You can change or remove it later on the Skills page.")
	return b.String()
}

// proposalView is the agent-facing shape of a row.
func proposalView(row *proposalRow) *Proposal {
	return &Proposal{ID: row.ID, Kind: row.Kind, Status: row.Status, InboxID: row.InboxID,
		CreatedAt: time.UnixMilli(row.CreatedAt).UTC(), Result: row.Result, Error: row.Error}
}

type proposalWarningGroup struct {
	code  string
	paths []string
}

// proposalWarningGroups groups warnings by code, the largest group first.
func proposalWarningGroups(ws []Warning) []proposalWarningGroup {
	idx := map[string]int{}
	var out []proposalWarningGroup
	for _, w := range ws {
		i, ok := idx[w.Code]
		if !ok {
			i = len(out)
			idx[w.Code] = i
			out = append(out, proposalWarningGroup{code: w.Code})
		}
		out[i].paths = append(out[i].paths, w.Path)
	}
	sort.SliceStable(out, func(a, b int) bool { return len(out[a].paths) > len(out[b].paths) })
	return out
}

func proposalSample(s []SkillSummary, n int) []SkillSummary {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func proposalBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", n>>10)
	}
	return fmt.Sprintf("%d bytes", n)
}
