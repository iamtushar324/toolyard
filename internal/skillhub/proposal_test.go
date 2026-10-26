package skillhub

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/inbox"
)

func newProposalTest(t *testing.T) (*proposalEnv, *SourceProposals) {
	t.Helper()
	e := newProposalEnv(t)
	p, err := NewSourceProposals(ProposalOptions{DB: e.db, Inbox: e.inbox, Admin: e.admin, IsAdmin: e.isAdmin,
		AgentName: func(_ context.Context, id string) string { return map[string]string{"ag_1": "docs-bot"}[id] },
		Audit:     audit.New(e.db), Now: e.clock})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.bg.Wait) // background first syncs end before the database closes
	return e, p
}

var proposalAgent = Viewer{UserID: "u_owner", AgentID: "ag_1", Admin: true}

func proposeTestSource(t *testing.T, p *SourceProposals, url string) *Proposal {
	t.Helper()
	got, err := p.ProposeSource(context.Background(), proposalAgent, SourceInput{URL: url}, "The team keeps its release checklists there.\nWe need them in every session.")
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestProposeSourceAsksTheOwner(t *testing.T) {
	e, p := newProposalTest(t)
	ctx := context.Background()
	got := proposeTestSource(t, p, "https://github.com/acme/skills/tree/main/skills")
	if got.Kind != "source" || got.Status != "pending" || got.InboxID == "" || !strings.HasPrefix(got.ID, "skp_") {
		t.Fatalf("proposal %+v", got)
	}
	if len(e.admin.previewBy) != 1 || e.admin.previewBy[0] != "u_owner" {
		t.Fatalf("preview by %v", e.admin.previewBy)
	}
	item, err := e.inbox.Get(ctx, got.InboxID)
	if err != nil {
		t.Fatal(err)
	}
	if item.AgentID != "ag_1" || item.System == nil || item.System.Kind != SourceProposalItemKind || item.System.Ref != got.ID {
		t.Fatalf("item %q %+v", item.AgentID, item.System)
	}
	if item.Title != "Add skill source acme/skills" {
		t.Fatalf("title %q", item.Title)
	}
	for _, want := range []string{
		"Your agent **docs-bot** proposes a skill source",
		"[acme/skills](https://github.com/acme/skills)",
		"Branch: `main`, folder `skills`",
		"https://github.com/acme/skills/commit/0123456789abcdef0123456789abcdef01234567",
		"Found: 3 skills, 7 files, 4 KB",
		"each new commit waits for your review",
		"> The team keeps its release checklists there.\n> We need them in every session.",
		"`name_mismatch` (2): `skills/a/SKILL.md`, `skills/b/SKILL.md`",
		"`binary` (1): `skills/c/logo.png`",
		"`pdf`: Read PDF files",
	} {
		if !strings.Contains(item.Message, want) {
			t.Errorf("message lacks %q:\n%s", want, item.Message)
		}
	}
	if !strings.Contains(proposalActivity(item), "Proposed by docs-bot with skills.propose_source") {
		t.Errorf("activity %q", proposalActivity(item))
	}
	// The agent polls its own proposal; another agent cannot see it; the
	// owner can from the dashboard.
	if again, err := p.GetProposal(ctx, proposalAgent, got.ID); err != nil || again.Status != "pending" {
		t.Fatalf("poll: %+v %v", again, err)
	}
	if _, err := p.GetProposal(ctx, Viewer{UserID: "u_owner", AgentID: "ag_2", Admin: true}, got.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another agent: %v", err)
	}
	if _, err := p.GetProposal(ctx, Viewer{UserID: "u_owner", Admin: true}, got.ID); err != nil {
		t.Fatalf("owner: %v", err)
	}
	if _, err := p.GetProposal(ctx, Viewer{UserID: "u_other"}, got.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another person: %v", err)
	}
	// The proposal is audited.
	var n int
	_ = e.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_events WHERE event_type = ?`, "skills.source_propose").Scan(&n)
	if n != 1 {
		t.Errorf("audit rows %d", n)
	}
}

func TestProposeSourceRefusals(t *testing.T) {
	_, p := newProposalTest(t)
	ctx := context.Background()
	in := SourceInput{URL: "acme/skills"}
	reason := "We need these."
	cases := map[string]struct {
		v      Viewer
		in     SourceInput
		reason string
	}{
		"dashboard user": {Viewer{UserID: "u_owner", Admin: true}, in, reason},
		"member's agent": {Viewer{UserID: "u_member", AgentID: "ag_9"}, in, reason},
		"no url":         {proposalAgent, SourceInput{}, reason},
		"no reason":      {proposalAgent, in, "  "},
		"long reason":    {proposalAgent, in, strings.Repeat("x", MaxProposalReason+1)},
		"token":          {proposalAgent, SourceInput{URL: "acme/skills", Token: "not-a-real-token"}, reason},
		"token auth":     {proposalAgent, SourceInput{URL: "acme/skills", Auth: AuthToken}, reason},
	}
	for name, c := range cases {
		if _, err := p.ProposeSource(ctx, c.v, c.in, c.reason); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
	for i := 0; i < MaxPendingSourceProposals; i++ {
		if _, err := p.ProposeSource(ctx, proposalAgent, in, reason); err != nil {
			t.Fatalf("proposal %d: %v", i, err)
		}
	}
	if _, err := p.ProposeSource(ctx, proposalAgent, in, reason); !errors.Is(err, ErrConflict) {
		t.Fatalf("over the limit: %v", err)
	}
	if _, err := p.ProposeChange(ctx, proposalAgent, ChangeInput{}); !errors.Is(err, ErrChangeProposalsUnavailable) {
		t.Fatalf("ProposeChange: %v", err)
	}
}

func TestProposeSourcePreviewErrorsReachTheAgent(t *testing.T) {
	e, p := newProposalTest(t)
	e.admin.previewErr = ErrNeedsToken
	if _, err := p.ProposeSource(context.Background(), proposalAgent, SourceInput{URL: "acme/private"}, "We need these."); !errors.Is(err, ErrNeedsToken) {
		t.Fatalf("preview error: %v", err)
	}
	var n int
	_ = e.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM skill_proposals`).Scan(&n)
	if n != 0 {
		t.Fatalf("a refused proposal stored %d rows", n)
	}
}

func TestApprovedProposalCreatesTheSource(t *testing.T) {
	e, p := newProposalTest(t)
	ctx := context.Background()
	got := proposeTestSource(t, p, "https://github.com/acme/skills/tree/main/skills")
	item := e.answer(t, got.InboxID, "u_owner", reviewApprove)
	if len(e.admin.creates) != 1 || e.admin.createBy[0] != "u_owner" {
		t.Fatalf("creates %v by %v", e.admin.creates, e.admin.createBy)
	}
	created := e.admin.creates[0]
	if created.Mode != ModeReview || created.Branch != "main" || created.Path != "skills" || created.Token != "" {
		t.Fatalf("created input %+v", created)
	}
	// The approval does not wait for GitHub: the first sync runs in the
	// background.
	if !strings.Contains(proposalActivity(item), "Added skill source skills (src_00000001, mode review). Its first sync runs now") {
		t.Fatalf("activity %q", proposalActivity(item))
	}
	p.bg.Wait()
	if len(e.admin.syncs) != 1 || e.admin.syncs[0] != "src_00000001" {
		t.Fatalf("syncs %v", e.admin.syncs)
	}
	done, err := p.GetProposal(ctx, proposalAgent, got.ID)
	if err != nil || done.Status != "done" || done.Result != "src_00000001" {
		t.Fatalf("proposal %+v %v", done, err)
	}
	var n int
	_ = e.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_events WHERE event_type = ?`, "skills.source_create").Scan(&n)
	if n != 1 {
		t.Errorf("create audit rows %d", n)
	}
}

func TestApprovedProposalOfATrustedOwnerUpdatesAutomatically(t *testing.T) {
	e, p := newProposalTest(t)
	got := proposeTestSource(t, p, "https://github.com/anthropics/skills")
	e.answer(t, got.InboxID, "u_owner", reviewApprove)
	if len(e.admin.creates) != 1 || e.admin.creates[0].Mode != ModeAuto {
		t.Fatalf("creates %+v", e.admin.creates)
	}
}

func TestRejectedProposalAddsNothing(t *testing.T) {
	e, p := newProposalTest(t)
	ctx := context.Background()
	got := proposeTestSource(t, p, "acme/skills")
	item, err := e.inbox.Get(ctx, got.InboxID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.inbox.Decide(ctx, got.InboxID, inbox.Decision{Action: "answer", RequestRevision: item.Revision, SubmissionID: "s-r",
		Answers: map[string]inbox.Answer{reviewQuestionID: {Value: reviewReject}}, Note: "use the docs repo instead",
		Decider: proposalDecider("u_owner")}); err != nil {
		t.Fatal(err)
	}
	if len(e.admin.creates) != 0 {
		t.Fatal("a rejected proposal created a source")
	}
	res, err := p.GetProposal(ctx, proposalAgent, got.ID)
	if err != nil || res.Status != "rejected" || res.Error != "rejected by your owner: use the docs repo instead" {
		t.Fatalf("proposal %+v %v", res, err)
	}
}

func TestProposalDecisionFailures(t *testing.T) {
	e, p := newProposalTest(t)
	ctx := context.Background()
	// The owner is no longer an admin when they approve.
	got := proposeTestSource(t, p, "acme/skills")
	e.mu.Lock()
	e.admins["u_owner"] = false
	e.mu.Unlock()
	item := e.answer(t, got.InboxID, "u_owner", reviewApprove)
	if len(e.admin.creates) != 0 || !strings.Contains(proposalActivity(item), "only an admin") {
		t.Fatalf("non-admin approval: creates %d activity %q", len(e.admin.creates), proposalActivity(item))
	}
	if res, _ := p.GetProposal(ctx, proposalAgent, got.ID); res.Status != "failed" {
		t.Fatalf("status %s", res.Status)
	}
	e.mu.Lock()
	e.admins["u_owner"] = true
	e.mu.Unlock()

	// CreateSource fails: the proposal fails with the reason.
	e.admin.createErr = errors.New("skillhub: conflict: slug skills is taken")
	got = proposeTestSource(t, p, "acme/skills")
	item = e.answer(t, got.InboxID, "u_owner", reviewApprove)
	if !strings.Contains(proposalActivity(item), "slug skills is taken") {
		t.Fatalf("activity %q", proposalActivity(item))
	}
	res, _ := p.GetProposal(ctx, proposalAgent, got.ID)
	if res.Status != "failed" || !strings.Contains(res.Error, "slug skills is taken") {
		t.Fatalf("proposal %+v", res)
	}
	e.admin.createErr = nil

	// The first sync failing still leaves the source added.
	e.admin.syncErr = errors.New("GitHub is down")
	got = proposeTestSource(t, p, "acme/skills")
	item = e.answer(t, got.InboxID, "u_owner", reviewApprove)
	if !strings.Contains(proposalActivity(item), "Its first sync runs now") {
		t.Fatalf("activity %q", proposalActivity(item))
	}
	p.bg.Wait()
	if res, _ := p.GetProposal(ctx, proposalAgent, got.ID); res.Status != "done" {
		t.Fatalf("status %s", res.Status)
	}
}

func TestGetProposalSettlesWithdrawnExpiredAndMissedDecisions(t *testing.T) {
	e, p := newProposalTest(t)
	ctx := context.Background()

	// The agent withdraws its item.
	withdrawn := proposeTestSource(t, p, "acme/skills")
	if _, err := e.inbox.Cancel(ctx, "ag_1", withdrawn.InboxID); err != nil {
		t.Fatal(err)
	}
	if res, _ := p.GetProposal(ctx, proposalAgent, withdrawn.ID); res.Status != "rejected" || !strings.Contains(res.Error, "withdrawn") {
		t.Fatalf("withdrawn: %+v", res)
	}

	// A decision whose handler never ran (the node stopped) is applied
	// when the agent polls.
	missed := proposeTestSource(t, p, "acme/skills")
	e.inbox.HandleSystem(SourceProposalItemKind, func(context.Context, *inbox.Request) (string, error) { return "", nil })
	e.answer(t, missed.InboxID, "u_owner", reviewApprove)
	if len(e.admin.creates) != 0 {
		t.Fatal("the stand-in handler must not create")
	}
	if res, _ := p.GetProposal(ctx, proposalAgent, missed.ID); res.Status != "done" || len(e.admin.creates) != 1 {
		t.Fatalf("missed decision: %+v creates %d", res, len(e.admin.creates))
	}
	// Polling again applies nothing twice.
	if _, err := p.GetProposal(ctx, proposalAgent, missed.ID); err != nil || len(e.admin.creates) != 1 {
		t.Fatalf("second poll: creates %d %v", len(e.admin.creates), err)
	}

	// An item nobody decided expires.
	expired := proposeTestSource(t, p, "acme/skills")
	e.advance(inbox.SystemItemTTL + time.Minute)
	e.inbox.Sweep(ctx)
	if res, _ := p.GetProposal(ctx, proposalAgent, expired.ID); res.Status != "rejected" || !strings.Contains(res.Error, "expired") {
		t.Fatalf("expired: %+v", res)
	}
}

func TestProposalHelpers(t *testing.T) {
	groups := proposalWarningGroups([]Warning{{Code: "binary", Path: "a"}, {Code: "secret", Path: "b"}, {Code: "secret", Path: "c"}})
	if len(groups) != 2 || groups[0].code != "secret" || len(groups[0].paths) != 2 {
		t.Fatalf("groups %+v", groups)
	}
	if proposalBytes(512) != "512 bytes" || proposalBytes(2048) != "2 KB" || proposalBytes(3<<20) != "3.0 MB" {
		t.Fatal("proposalBytes")
	}
	if _, err := NewSourceProposals(ProposalOptions{}); err == nil {
		t.Fatal("NewSourceProposals without dependencies must fail")
	}
}

// TestProposalLimitFreesUpAfterExpiry: proposals whose items expired no
// longer count as waiting, so the agent can propose again without anyone
// polling them.
func TestProposalLimitFreesUpAfterExpiry(t *testing.T) {
	e, p := newProposalTest(t)
	ctx := context.Background()
	var ids []string
	for i := 0; i < MaxPendingSourceProposals; i++ {
		ids = append(ids, proposeTestSource(t, p, "acme/skills").ID)
	}
	if _, err := p.ProposeSource(ctx, proposalAgent, SourceInput{URL: "acme/skills"}, "We need these playbooks."); !errors.Is(err, ErrConflict) {
		t.Fatalf("over the limit: %v", err)
	}
	// A week later every item is past its deadline; the Inbox's own sweep
	// has not run yet.
	e.advance(inbox.SystemItemTTL + time.Minute)
	if _, err := p.ProposeSource(ctx, proposalAgent, SourceInput{URL: "acme/skills"}, "We need these playbooks."); err != nil {
		t.Fatalf("after the old items expired: %v", err)
	}
	for _, id := range ids {
		if res, _ := p.GetProposal(ctx, proposalAgent, id); res.Status != "rejected" || !strings.Contains(res.Error, "expired") {
			t.Fatalf("proposal %s: %+v", id, res)
		}
	}
}

// TestProposalSweep settles proposals nobody polls: a decision no handler
// ran, a withdrawn item, an expired one.
func TestProposalSweep(t *testing.T) {
	e, p := newProposalTest(t)
	ctx := context.Background()
	missed := proposeTestSource(t, p, "acme/skills")
	withdrawn := proposeTestSource(t, p, "acme/skills")
	expired := proposeTestSource(t, p, "acme/skills")
	e.inbox.HandleSystem(SourceProposalItemKind, func(context.Context, *inbox.Request) (string, error) { return "", nil })
	e.answer(t, missed.InboxID, "u_owner", reviewApprove)
	if _, err := e.inbox.Cancel(ctx, "ag_1", withdrawn.InboxID); err != nil {
		t.Fatal(err)
	}
	if len(e.admin.creates) != 0 {
		t.Fatal("the stand-in handler must not create")
	}
	if err := p.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	p.bg.Wait()
	if len(e.admin.creates) != 1 || len(e.admin.syncs) != 1 {
		t.Fatalf("missed decision: creates %d syncs %v", len(e.admin.creates), e.admin.syncs)
	}
	check := func(id, status, errPart string) {
		t.Helper()
		var got proposalRow
		row, err := p.store.get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		got = *row
		if got.Status != status || !strings.Contains(got.Error, errPart) {
			t.Fatalf("%s: %+v, want %s (%s)", id, got, status, errPart)
		}
	}
	check(missed.ID, "done", "")
	check(withdrawn.ID, "rejected", "withdrawn")
	check(expired.ID, "pending", "")
	e.advance(inbox.SystemItemTTL + time.Minute)
	if err := p.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	check(expired.ID, "rejected", "expired")
	// Nothing is applied twice; a nil sweeper is a no-op.
	if err := p.Sweep(ctx); err != nil || len(e.admin.creates) != 1 {
		t.Fatalf("second sweep: %v creates %d", err, len(e.admin.creates))
	}
	var nilP *SourceProposals
	if err := nilP.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
}

// TestProposalKeepsOnlyURLBranchAndFolder: the agent cannot choose the
// mode, visibility or anything else of the source it proposes.
func TestProposalKeepsOnlyURLBranchAndFolder(t *testing.T) {
	e, p := newProposalTest(t)
	ctx := context.Background()
	in := SourceInput{URL: "acme/skills", Branch: "dev", Path: "skills", Slug: "mine", Mode: ModeAuto, PinnedSHA: "abc",
		Visibility: VisibleMembers, Members: []string{"u_agent_friend"}, DefaultRuns: RunsHost, Exclude: []string{"**"},
		PollSeconds: 60, Limits: &Limits{MaxSkills: 1}}
	got, err := p.ProposeSource(ctx, proposalAgent, in, "The team keeps its release checklists there.")
	if err != nil {
		t.Fatal(err)
	}
	want := SourceInput{URL: "acme/skills", Branch: "dev", Path: "skills"}
	if prev := e.admin.previews[0]; prev.URL != want.URL || prev.Branch != want.Branch || prev.Path != want.Path || prev.Mode != "" ||
		prev.Visibility != "" || prev.Members != nil || prev.DefaultRuns != "" || prev.Exclude != nil || prev.PollSeconds != 0 ||
		prev.Limits != nil || prev.Slug != "" || prev.PinnedSHA != "" {
		t.Fatalf("previewed %+v", prev)
	}
	e.answer(t, got.InboxID, "u_owner", reviewApprove)
	created := e.admin.creates[0]
	if created.Mode != ModeReview || created.Visibility != "" || created.Members != nil || created.DefaultRuns != "" || created.Slug != "" {
		t.Fatalf("created %+v: the owner's defaults must decide", created)
	}
}

// TestProposalRateLimit: at most MaxSourceProposalsPerHour dry runs an
// hour per agent, whatever happened to them.
func TestProposalRateLimit(t *testing.T) {
	e, p := newProposalTest(t)
	ctx := context.Background()
	for i := 0; i < MaxSourceProposalsPerHour; i++ {
		got := proposeTestSource(t, p, "acme/skills")
		e.answer(t, got.InboxID, "u_owner", reviewReject) // none stays waiting
	}
	if _, err := p.ProposeSource(ctx, proposalAgent, SourceInput{URL: "acme/skills"}, "We need these playbooks."); !errors.Is(err, ErrLimit) {
		t.Fatalf("over the hourly limit: %v", err)
	}
	if n := len(e.admin.previews); n != MaxSourceProposalsPerHour {
		t.Fatalf("a refused proposal ran a dry run: %d previews", n)
	}
	// Another agent has its own budget; an hour later this one does too.
	if _, err := p.ProposeSource(ctx, Viewer{UserID: "u_owner", AgentID: "ag_2", Admin: true}, SourceInput{URL: "acme/skills"}, "We need these playbooks."); err != nil {
		t.Fatalf("another agent: %v", err)
	}
	e.advance(time.Hour + time.Minute)
	if _, err := p.ProposeSource(ctx, proposalAgent, SourceInput{URL: "acme/skills"}, "We need these playbooks."); err != nil {
		t.Fatalf("an hour later: %v", err)
	}
}

// TestProposalTextHasNoLineBreaks: the agent's name and the repository's
// text stay on their lines; the reason is quoted line by line, escaped.
func TestProposalTextHasNoLineBreaks(t *testing.T) {
	e := newProposalEnv(t)
	p, err := NewSourceProposals(ProposalOptions{DB: e.db, Inbox: e.inbox, Admin: e.admin, IsAdmin: e.isAdmin, Now: e.clock,
		AgentName: func(context.Context, string) string { return "bot\n# Approve" }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.bg.Wait)
	e.admin.preview.Warnings = []Warning{{Path: "x\n- [ ] approve", Code: "secret\nhi"}}
	got, err := p.ProposeSource(context.Background(), proposalAgent, SourceInput{URL: "acme/skills"}, "line one\n![img](https://evil.example/x.png)")
	if err != nil {
		t.Fatal(err)
	}
	item, err := e.inbox.Get(context.Background(), got.InboxID)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"\n# Approve", "\n- [ ] approve", "secret\nhi", "![img]"} {
		if strings.Contains(item.Message, bad) || strings.Contains(item.Summary, bad) {
			t.Errorf("item keeps %q:\n%s", bad, item.Message)
		}
	}
	if !strings.Contains(item.Message, "> line one\n> !\\[img\\](https://evil.example/x.png)") {
		t.Errorf("reason not quoted and escaped:\n%s", item.Message)
	}
}
