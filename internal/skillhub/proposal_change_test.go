package skillhub

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/actor"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/inbox"
)

// changeEnv is a synced source (acme/skills, folder skills, created by
// usr_admin), a real Inbox on the same database, change proposals that
// write to a fake GitHub, and a clock the test moves.
type changeEnv struct {
	gh      *fakeGitHub
	git     *fakeGitData
	svc     *Service
	inbox   *inbox.Service
	changes *ChangeProposals
	src     *Source
	base    string

	mu     sync.Mutex
	now    time.Time
	tokens map[string]string
}

const changeToken = "conn-token-admin"

// changeAgent is a member's agent that sees the source.
var changeAgent = Viewer{UserID: "usr_member", AgentID: "agt_member"}

func changeTestTree() map[string]string {
	return map[string]string{
		"README.md":                             "# skills\n",
		"skills/deploy/SKILL.md":                "---\nname: deploy\ndescription: Deploy the service safely\n---\n# Deploy\n\n1. Build.\n2. Ship.\n",
		"skills/deploy/references/checklist.md": "one\ntwo\nthree\nfour\nfive\nsix\nseven\n",
		"skills/deploy/scripts/run.sh":          "#!/bin/sh\necho run\n",
		"skills/deploy/old.md":                  "obsolete\n",
		"skills/other/SKILL.md":                 "---\nname: other\ndescription: Another skill\n---\nBody\n",
	}
}

func newChangeEnv(t *testing.T) *changeEnv {
	t.Helper()
	e := &changeEnv{now: time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC), tokens: map[string]string{"usr_admin": changeToken}}
	e.gh = newFakeGitHub(t, "acme", "skills")
	e.base = e.gh.commit("main", changeTestTree())
	e.svc = newTestService(t, e.gh)
	src, res := mustCreate(t, e.svc, SourceInput{URL: "acme/skills", Path: "skills", Mode: ModeAuto})
	if !res.Activated || res.SHA != e.base {
		t.Fatalf("first sync %+v", res)
	}
	e.src = src
	ib, err := inbox.New(context.Background(), inbox.Options{DB: e.svc.db, Now: e.clock})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ib.Flush)
	e.inbox = ib
	e.git = newFakeGitData(t, e.gh, changeToken)
	e.git.modes["skills/deploy/scripts/run.sh"] = "100755"
	e.changes, err = NewChangeProposals(ChangeOptions{DB: e.svc.db, Inbox: ib, Reader: e.svc, Admin: e.svc,
		Tokens: func(_ context.Context, uid string) (string, error) {
			e.mu.Lock()
			defer e.mu.Unlock()
			if tok := e.tokens[uid]; tok != "" {
				return tok, nil
			}
			return "", ErrNoGitHubConnection
		},
		HTTPClient: e.git.client(),
		AgentName:  func(_ context.Context, id string) string { return map[string]string{"agt_member": "docs-bot"}[id] },
		UserName:   func(_ context.Context, id string) string { return map[string]string{"usr_member": "Mia"}[id] },
		Audit:      audit.New(e.svc.db), Now: e.clock})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *changeEnv) clock() time.Time { e.mu.Lock(); defer e.mu.Unlock(); return e.now }

func (e *changeEnv) advance(d time.Duration) { e.mu.Lock(); e.now = e.now.Add(d); e.mu.Unlock() }

// changeInput4 changes one file, adds one, deletes one and edits the
// executable script.
func changeInput4() ChangeInput {
	return ChangeInput{
		Skill:   "skills/deploy",
		Title:   "Fix the checklist\x07order",
		Summary: "Step three comes after four.\nAlso drop old.md.",
		Files: map[string]string{
			"references/checklist.md": "one\ntwo\nfour\nthree\nfive\nsix\nseven\n",
			"references/new.md":       "fresh\n",
			"old.md":                  "",
			"scripts/run.sh":          "#!/bin/sh\necho run fast\n",
		},
	}
}

func (e *changeEnv) propose(t *testing.T, in ChangeInput) *Proposal {
	t.Helper()
	p, err := e.changes.ProposeChange(context.Background(), changeAgent, in)
	if err != nil {
		t.Fatalf("ProposeChange: %v", err)
	}
	return p
}

// decide answers a proposal's item as uid.
func (e *changeEnv) decide(t *testing.T, p *Proposal, uid, choice, via string) *inbox.Request {
	t.Helper()
	r, err := e.inbox.Get(context.Background(), p.InboxID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := e.inbox.Decide(context.Background(), p.InboxID, inbox.Decision{Action: "answer", RequestRevision: r.Revision,
		SubmissionID: "sub-" + p.ID + "-" + choice, Answers: map[string]inbox.Answer{reviewQuestionID: {Value: choice}},
		Decider: actor.Decider{UserID: uid, Name: uid, Via: via}})
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	return got
}

func (e *changeEnv) row(t *testing.T, id string) *proposalRow {
	t.Helper()
	row, err := e.changes.store.get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func (e *changeEnv) auditCount(t *testing.T, event string) int {
	t.Helper()
	var n int
	if err := e.svc.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM audit_events WHERE event_type = ?`, event).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestProposeChangeValidation(t *testing.T) {
	e := newChangeEnv(t)
	ctx := context.Background()
	one := func(path, content string) ChangeInput {
		return ChangeInput{Skill: "skills/deploy", Title: "A change", Files: map[string]string{path: content}}
	}
	many := ChangeInput{Skill: "skills/deploy", Title: "Too many", Files: map[string]string{}}
	for i := 0; i <= MaxChangeFiles; i++ {
		many.Files["notes/n"+strings.Repeat("x", i%7)+string(rune('a'+i%26))+string(rune('a'+i/26))+".md"] = "x\n"
	}
	// Built at run time, so the source file holds no key-shaped text.
	awsKey := "AK" + "IA" + strings.Repeat("Z7", 8)
	cases := []struct {
		name string
		v    Viewer
		in   ChangeInput
		want error
	}{
		{"dashboard user", Viewer{UserID: "usr_admin", Admin: true}, one("a.md", "x"), ErrInvalid},
		{"no title", changeAgent, ChangeInput{Skill: "skills/deploy", Files: map[string]string{"a.md": "x"}}, ErrInvalid},
		{"long title", changeAgent, ChangeInput{Skill: "skills/deploy", Title: strings.Repeat("t", MaxChangeTitle+1), Files: map[string]string{"a.md": "x"}}, ErrInvalid},
		{"no files", changeAgent, ChangeInput{Skill: "skills/deploy", Title: "A change"}, ErrInvalid},
		{"too many files", changeAgent, many, ErrLimit},
		{"unknown skill", changeAgent, ChangeInput{Skill: "skills/nope", Title: "A change", Files: map[string]string{"a.md": "x"}}, ErrNotFound},
		{"dot dot", changeAgent, one("../other/SKILL.md", "x"), ErrInvalid},
		{"inner dot dot", changeAgent, one("references/../../x.md", "x"), ErrInvalid},
		{"absolute", changeAgent, one("/etc/passwd", "x"), ErrInvalid},
		{"dot segment", changeAgent, one("./a.md", "x"), ErrInvalid},
		{"empty segment", changeAgent, one("a//b.md", "x"), ErrInvalid},
		{"trailing slash", changeAgent, one("a/", "x"), ErrInvalid},
		{"backslash", changeAgent, one(`a\b.md`, "x"), ErrInvalid},
		{"git folder", changeAgent, one(".git/config", "x"), ErrInvalid},
		{"control character", changeAgent, one("a\nb.md", "x"), ErrInvalid},
		{"bidi override", changeAgent, one("a\u202eb.md", "x"), ErrInvalid},
		{"not UTF-8", changeAgent, one("a.md", "\xff\xfe"), ErrInvalid},
		{"NUL byte", changeAgent, one("a.md", "a\x00b"), ErrInvalid},
		{"too large", changeAgent, one("a.md", strings.Repeat("x", int(DefaultMaxFileBytes)+1)), ErrLimit},
		{"secret", changeAgent, one("a.md", "key = "+awsKey+"\n"), ErrInvalid},
		{"delete missing", changeAgent, one("missing.md", ""), ErrInvalid},
		{"delete SKILL.md", changeAgent, one("SKILL.md", ""), ErrInvalid},
		{"broken SKILL.md", changeAgent, one("SKILL.md", "---\nname: renamed\ndescription: x\n---\n"), ErrInvalid},
		{"file inside a file", changeAgent, one("old.md/inner.md", "x"), ErrInvalid},
		{"file over a folder", changeAgent, one("references", "x"), ErrInvalid},
		{"no change", changeAgent, one("old.md", "obsolete\n"), ErrInvalid},
	}
	for _, c := range cases {
		if _, err := e.changes.ProposeChange(ctx, c.v, c.in); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
	if e.git.totalCalls() != 0 {
		t.Fatalf("validation called GitHub %d times", e.git.totalCalls())
	}
	var n int
	_ = e.svc.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM skill_proposals`).Scan(&n)
	if n != 0 {
		t.Fatalf("refused proposals stored %d rows", n)
	}

	// A file inside a file that the same proposal deletes is fine.
	if _, err := e.changes.ProposeChange(ctx, changeAgent, ChangeInput{Skill: "skills/deploy", Title: "Make old a folder",
		Files: map[string]string{"old.md": "", "old.md/inner.md": "x\n"}}); err != nil {
		t.Fatalf("delete and replace with a folder: %v", err)
	}

	// The creator needs a GitHub connection before anyone is asked.
	e.mu.Lock()
	delete(e.tokens, "usr_admin")
	e.mu.Unlock()
	if _, err := e.changes.ProposeChange(ctx, changeAgent, one("a.md", "x\n")); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "GitHub connection") {
		t.Fatalf("no connection: %v", err)
	}
	e.mu.Lock()
	e.tokens["usr_admin"] = changeToken
	e.mu.Unlock()

	// One agent may have MaxPendingChangeProposals waiting.
	for i := 1; i < MaxPendingChangeProposals; i++ {
		if _, err := e.changes.ProposeChange(ctx, changeAgent, one("a.md", strings.Repeat("y", i)+"\n")); err != nil {
			t.Fatalf("proposal %d: %v", i, err)
		}
	}
	if _, err := e.changes.ProposeChange(ctx, changeAgent, one("a.md", "z\n")); !errors.Is(err, ErrConflict) {
		t.Fatalf("over the pending cap: %v", err)
	}
}

func TestProposeChangeAsksTheSourceOwner(t *testing.T) {
	e := newChangeEnv(t)
	ctx := context.Background()
	in := changeInput4()
	in.Files["references/fences.md"] = "````\n```\ncode\n```\n"
	p := e.propose(t, in)
	if p.Kind != "change" || p.Status != "pending" || p.InboxID == "" || !strings.HasPrefix(p.ID, "skp_") {
		t.Fatalf("proposal %+v", p)
	}
	item, err := e.inbox.Get(ctx, p.InboxID)
	if err != nil {
		t.Fatal(err)
	}
	// The item goes to the source's creator, from toolyard itself.
	if item.AgentID != inbox.SystemAgentID("usr_admin") || item.System == nil || item.System.Kind != ChangeProposalItemKind || item.System.Ref != p.ID {
		t.Fatalf("item %q %+v", item.AgentID, item.System)
	}
	if strings.ContainsRune(item.Title, '\x07') || !strings.HasPrefix(item.Title, "Skill change: Fix the checklist") {
		t.Fatalf("title %q", item.Title)
	}
	if len(item.Questions) != 1 || item.Questions[0].Prompt != "Open a pull request on acme/skills?" {
		t.Fatalf("questions %+v", item.Questions)
	}
	for _, want := range []string{
		"The agent **docs-bot** (owner: Mia) proposes a change to the skill `deploy` in your skill source **skills**",
		"[acme/skills](https://github.com/acme/skills), branch `main`",
		"https://github.com/acme/skills/commit/" + e.base,
		"Changes: 5 files (2 added, 2 changed, 1 deleted)",
		"> Step three comes after four.\n> Also drop old.md.",
		"**`references/checklist.md`**: changed, +1 −1",
		"@@ -1,7 +1,7 @@\n one\n two\n-three\n four\n+three\n five\n six\n seven\n",
		"**`references/new.md`**: added, +1 −0",
		"**`old.md`**: deleted (1 line)",
		"**`scripts/run.sh`**: changed",
		"-echo run\n+echo run fast",
		// A fence longer than the four backticks inside the file.
		"`````diff\n@@ -0,0 +1,4 @@\n+````\n+```\n+code\n+```\n`````",
		"a new branch `toolyard/skills/" + p.ID + "` from commit `" + e.base[:12] + "`, into `main`",
	} {
		if !strings.Contains(item.Message, want) {
			t.Errorf("message lacks %q:\n%s", want, item.Message)
		}
	}
	if !strings.Contains(proposalActivity(item), "Proposed by docs-bot with skills.propose_change") {
		t.Errorf("activity %q", proposalActivity(item))
	}
	// Who may poll it: the agent, the person it went to, and the agent's
	// owner; nobody else.
	for name, v := range map[string]Viewer{"agent": changeAgent, "creator": {UserID: "usr_admin", Admin: true}, "agent owner": {UserID: "usr_member"}} {
		if got, err := e.changes.GetProposal(ctx, v, p.ID); err != nil || got.Status != "pending" {
			t.Errorf("%s: %+v %v", name, got, err)
		}
	}
	for name, v := range map[string]Viewer{"another agent": {UserID: "usr_member", AgentID: "agt_other"}, "another person": {UserID: "usr_other"}} {
		if _, err := e.changes.GetProposal(ctx, v, p.ID); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if n := e.auditCount(t, "skills.change_propose"); n != 1 {
		t.Errorf("audit rows %d", n)
	}
	if e.git.totalCalls() != 0 {
		t.Fatalf("a proposal called GitHub %d times before the approval", e.git.totalCalls())
	}
	row := e.row(t, p.ID)
	if row.UserID != "usr_admin" || row.AgentID != "agt_member" || row.SourceID != e.src.ID || row.VersionID == "" {
		t.Fatalf("row %+v", row)
	}
}

func TestChangeApprovalOpensPullRequest(t *testing.T) {
	e := newChangeEnv(t)
	ctx := context.Background()
	p := e.propose(t, changeInput4())
	got := e.decide(t, p, "usr_admin", reviewApprove, actor.ViaDashboard)
	url := "https://github.com/acme/skills/pull/1"
	if !strings.Contains(proposalActivity(got), "Opened the pull request "+url) {
		t.Fatalf("activity %q", proposalActivity(got))
	}
	done, err := e.changes.GetProposal(ctx, changeAgent, p.ID)
	if err != nil || done.Status != "done" || done.Result != url || done.Error != "" {
		t.Fatalf("proposal %+v %v", done, err)
	}
	if len(e.git.pulls) != 1 {
		t.Fatalf("pulls %+v", e.git.pulls)
	}
	pr := e.git.pulls[0]
	branch := ChangeBranchPrefix + p.ID
	if pr.head != branch || pr.base != "main" || pr.title != "Fix the checklist order" {
		t.Fatalf("pull %+v", pr)
	}
	for _, want := range []string{"Step three comes after four.", "**docs-bot**", "toolyard proposal: `" + p.ID + "`", e.base} {
		if !strings.Contains(pr.body, want) {
			t.Errorf("pull body lacks %q:\n%s", want, pr.body)
		}
	}
	if strings.Contains(pr.body, "Mia") {
		t.Errorf("the pull request body names a person:\n%s", pr.body)
	}
	files, modes, commit, ok := e.git.branchTree(branch)
	if !ok {
		t.Fatal("no branch")
	}
	if len(commit.parents) != 1 || commit.parents[0] != e.base || !strings.Contains(commit.message, "toolyard-proposal: "+p.ID) {
		t.Fatalf("commit %+v", commit)
	}
	want := changeTestTree()
	want["skills/deploy/references/checklist.md"] = "one\ntwo\nfour\nthree\nfive\nsix\nseven\n"
	want["skills/deploy/references/new.md"] = "fresh\n"
	want["skills/deploy/scripts/run.sh"] = "#!/bin/sh\necho run fast\n"
	delete(want, "skills/deploy/old.md")
	if len(files) != len(want) {
		t.Fatalf("tree %v", files)
	}
	for p, c := range want {
		if files[p] != c {
			t.Errorf("%s = %q, want %q", p, files[p], c)
		}
	}
	if fakeMode(modes, "skills/deploy/scripts/run.sh") != "100755" || fakeMode(modes, "skills/deploy/references/new.md") != "100644" {
		t.Errorf("modes %v", modes)
	}
	// Every call used the creator's connection.
	if len(e.git.auth) != 1 || !e.git.auth["Bearer "+changeToken] {
		t.Errorf("authorization %v", e.git.auth)
	}
	if e.git.count("POST refs") != 1 || e.git.count("POST pulls") != 1 || e.git.count("POST commits") != 1 {
		t.Errorf("calls %v", e.git.calls)
	}
	if n := e.auditCount(t, "skills.change_pull_request"); n != 1 {
		t.Errorf("audit rows %d", n)
	}
	// A second decision path (a poll) does nothing more.
	if again, err := e.changes.GetProposal(ctx, changeAgent, p.ID); err != nil || again.Status != "done" || len(e.git.pulls) != 1 {
		t.Fatalf("poll after done: %+v %v", again, err)
	}
}

func TestChangeApprovalReconcilesUncertainWrites(t *testing.T) {
	ctx := context.Background()
	// GitHub opened the pull request but the answer was lost: toolyard
	// finds it instead of opening another.
	e := newChangeEnv(t)
	e.git.fail["POST pulls"] = fakeGitFail{status: 502, apply: true}
	p := e.propose(t, changeInput4())
	e.decide(t, p, "usr_admin", reviewApprove, actor.ViaDashboard)
	if got, _ := e.changes.GetProposal(ctx, changeAgent, p.ID); got.Status != "done" || got.Result != "https://github.com/acme/skills/pull/1" {
		t.Fatalf("lost pull answer: %+v", got)
	}
	if len(e.git.pulls) != 1 || e.git.count("POST pulls") != 1 {
		t.Fatalf("pulls %d, POST pulls %d", len(e.git.pulls), e.git.count("POST pulls"))
	}

	// GitHub made the branch but the answer was lost: toolyard finds its
	// own branch and goes on.
	e = newChangeEnv(t)
	e.git.fail["POST refs"] = fakeGitFail{status: 500, apply: true}
	p = e.propose(t, changeInput4())
	e.decide(t, p, "usr_admin", reviewApprove, actor.ViaDashboard)
	if got, _ := e.changes.GetProposal(ctx, changeAgent, p.ID); got.Status != "done" {
		t.Fatalf("lost branch answer: %+v", got)
	}
	if e.git.count("POST refs") != 1 || len(e.git.pulls) != 1 {
		t.Fatalf("POST refs %d, pulls %d", e.git.count("POST refs"), len(e.git.pulls))
	}

	// Neither the pull request nor the check after it answered: the
	// proposal stays approved and nothing is written again until a later
	// poll, which finds the branch and opens the pull request once.
	e = newChangeEnv(t)
	e.git.fail["POST pulls"] = fakeGitFail{status: 502}
	e.git.fail["GET pulls"] = fakeGitFail{status: 502, after: 1}
	p = e.propose(t, changeInput4())
	got := e.decide(t, p, "usr_admin", reviewApprove, actor.ViaDashboard)
	if !strings.Contains(proposalActivity(got), "GitHub did not confirm the pull request") {
		t.Fatalf("activity %q", proposalActivity(got))
	}
	if row := e.row(t, p.ID); row.Status != proposalApproved || !strings.Contains(row.Error, "before it writes again") {
		t.Fatalf("row %+v", row)
	}
	// Too soon: a poll does not write.
	if got, _ := e.changes.GetProposal(ctx, changeAgent, p.ID); got.Status != "approved" || e.git.count("POST pulls") != 1 {
		t.Fatalf("early poll: %+v, POST pulls %d", got, e.git.count("POST pulls"))
	}
	e.advance(changeStaleAfter + time.Minute)
	done, err := e.changes.GetProposal(ctx, changeAgent, p.ID)
	if err != nil || done.Status != "done" || done.Error != "" || done.Result != "https://github.com/acme/skills/pull/1" {
		t.Fatalf("resumed: %+v %v", done, err)
	}
	if e.git.count("POST refs") != 1 || e.git.count("POST commits") != 1 || e.git.count("POST pulls") != 2 || len(e.git.pulls) != 1 {
		t.Fatalf("writes after the resume: %v, pulls %d", e.git.calls, len(e.git.pulls))
	}
}

func TestChangeSweepSettlesAndResumes(t *testing.T) {
	ctx := context.Background()
	e := newChangeEnv(t)
	// An approved proposal whose pull request got no answer is resumed by
	// the sweep once it is stale, without waiting for the agent.
	e.git.fail["POST pulls"] = fakeGitFail{status: 503}
	e.git.fail["GET pulls"] = fakeGitFail{status: 503, after: 1}
	resumed := e.propose(t, changeInput4())
	e.decide(t, resumed, "usr_admin", reviewApprove, actor.ViaDashboard)
	if row := e.row(t, resumed.ID); row.Status != proposalApproved {
		t.Fatalf("after the lost answer: %+v", row)
	}
	// An item nobody decides expires; the sweep closes its proposal.
	idle := e.propose(t, ChangeInput{Skill: "skills/deploy", Title: "Another change", Files: map[string]string{"notes.md": "x\n"}})
	if err := e.changes.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if row := e.row(t, resumed.ID); row.Status != proposalApproved || e.git.count("POST pulls") != 1 {
		t.Fatalf("a fresh approved row was resumed: %+v", row)
	}
	e.advance(inbox.SystemItemTTL + time.Hour)
	if err := e.changes.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if row := e.row(t, resumed.ID); row.Status != proposalDone || row.Result != "https://github.com/acme/skills/pull/1" {
		t.Fatalf("not resumed: %+v", row)
	}
	if row := e.row(t, idle.ID); row.Status != proposalRejected || !strings.Contains(row.Error, "expired") {
		t.Fatalf("expired item: %+v", row)
	}
	if len(e.git.pulls) != 1 || e.git.count("POST refs") != 1 {
		t.Fatalf("pulls %d, POST refs %d", len(e.git.pulls), e.git.count("POST refs"))
	}
}

func TestChangeRejectAndRefusedDecisions(t *testing.T) {
	ctx := context.Background()
	e := newChangeEnv(t)

	// Reject: nothing reaches GitHub.
	p := e.propose(t, changeInput4())
	got := e.decide(t, p, "usr_admin", reviewReject, actor.ViaDashboard)
	if !strings.Contains(proposalActivity(got), "nothing was sent to GitHub") {
		t.Fatalf("activity %q", proposalActivity(got))
	}
	if r, _ := e.changes.GetProposal(ctx, changeAgent, p.ID); r.Status != "rejected" {
		t.Fatalf("rejected: %+v", r)
	}

	// Another admin cannot approve a write with the creator's account.
	p = e.propose(t, changeInput4())
	got = e.decide(t, p, "usr_other_admin", reviewApprove, actor.ViaDashboard)
	if !strings.Contains(proposalActivity(got), "only the person this item was sent to") {
		t.Fatalf("activity %q", proposalActivity(got))
	}
	if row := e.row(t, p.ID); row.Status != proposalFailed {
		t.Fatalf("non-creator row %+v", row)
	}

	// An automatic rule never approves a GitHub write.
	p = e.propose(t, changeInput4())
	e.decide(t, p, "usr_admin", reviewApprove, actor.ViaAutoRule)
	if row := e.row(t, p.ID); row.Status != proposalFailed || !strings.Contains(row.Error, "own decision") {
		t.Fatalf("auto rule row %+v", row)
	}
	if e.git.totalCalls() != 0 {
		t.Fatalf("refused decisions called GitHub: %v", e.git.calls)
	}
	if n := e.auditCount(t, "skills.change_proposal_reject"); n != 1 {
		t.Errorf("reject audit rows %d", n)
	}

	// A branch of that name that toolyard did not write fails the
	// proposal instead of being reused.
	p = e.propose(t, changeInput4())
	e.git.mu.Lock()
	e.git.refs[ChangeBranchPrefix+p.ID] = e.base
	e.git.mu.Unlock()
	e.decide(t, p, "usr_admin", reviewApprove, actor.ViaDashboard)
	if row := e.row(t, p.ID); row.Status != proposalFailed || !strings.Contains(row.Error, "toolyard did not write it") {
		t.Fatalf("foreign branch row %+v", row)
	}
	if len(e.git.pulls) != 0 {
		t.Fatalf("pulls %+v", e.git.pulls)
	}
}

func TestChangeApprovalFailsCleanly(t *testing.T) {
	ctx := context.Background()
	e := newChangeEnv(t)
	// The creator disconnected GitHub after the proposal.
	p := e.propose(t, changeInput4())
	e.mu.Lock()
	delete(e.tokens, "usr_admin")
	e.mu.Unlock()
	e.decide(t, p, "usr_admin", reviewApprove, actor.ViaDashboard)
	if r, _ := e.changes.GetProposal(ctx, changeAgent, p.ID); r.Status != "failed" || !strings.Contains(r.Error, "GitHub connection") {
		t.Fatalf("no connection: %+v", r)
	}
	e.mu.Lock()
	e.tokens["usr_admin"] = changeToken
	e.mu.Unlock()

	// GitHub refuses the write (no permission): a clear failure, not a
	// retry.
	p = e.propose(t, changeInput4())
	e.git.token = "another-token"
	e.decide(t, p, "usr_admin", reviewApprove, actor.ViaDashboard)
	r, _ := e.changes.GetProposal(ctx, changeAgent, p.ID)
	if r.Status != "failed" || !strings.Contains(r.Error, "HTTP 401") || strings.Contains(r.Error, changeToken) {
		t.Fatalf("refused: %+v", r)
	}
	if n := e.auditCount(t, "skills.change_proposal_fail"); n != 2 {
		t.Errorf("fail audit rows %d", n)
	}
}

func TestSourceProposalsDelegateChanges(t *testing.T) {
	e := newChangeEnv(t)
	ctx := context.Background()
	sp, err := NewSourceProposals(ProposalOptions{DB: e.svc.db, Inbox: e.inbox, Admin: e.svc, Now: e.clock})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sp.ProposeChange(ctx, changeAgent, changeInput4()); !errors.Is(err, ErrChangeProposalsUnavailable) {
		t.Fatalf("without change proposals: %v", err)
	}
	sp.SetChangeProposals(e.changes)
	p, err := sp.ProposeChange(ctx, changeAgent, changeInput4())
	if err != nil || p.Kind != "change" {
		t.Fatalf("delegated: %+v %v", p, err)
	}
	if got, err := sp.GetProposal(ctx, changeAgent, p.ID); err != nil || got.ID != p.ID || got.Kind != "change" {
		t.Fatalf("poll through source proposals: %+v %v", got, err)
	}
}

func TestChangeUnifiedDiff(t *testing.T) {
	lines, add, del, ok := changeUnifiedDiff("a\nb\nc\n", "a\nB\nc\nd\n", 3)
	if !ok || add != 2 || del != 1 {
		t.Fatalf("counts +%d -%d ok %v", add, del, ok)
	}
	if got := strings.Join(lines, "\n"); got != "@@ -1,3 +1,4 @@\n a\n-b\n+B\n c\n+d" {
		t.Fatalf("diff:\n%s", got)
	}
	// Two changes far apart make two hunks.
	var a, b []string
	for i := 0; i < 30; i++ {
		a = append(a, "line")
		b = append(b, "line")
	}
	b[2], b[25] = "two", "twenty-five"
	lines, _, _, _ = changeUnifiedDiff(strings.Join(a, "\n")+"\n", strings.Join(b, "\n")+"\n", 3)
	var hunks []string
	for _, l := range lines {
		if strings.HasPrefix(l, "@@") {
			hunks = append(hunks, l)
		}
	}
	if len(hunks) != 2 || hunks[0] != "@@ -1,6 +1,6 @@" || hunks[1] != "@@ -23,7 +23,7 @@" {
		t.Fatalf("hunks %v", hunks)
	}
	if changeFence("no ticks") != "```" || changeFence("a ```` b") != "`````" {
		t.Fatal("fence")
	}
	if got := changeLine("a\tb\u202ec\r\nd"); got != "a b c  d" {
		t.Fatalf("changeLine %q", got)
	}
}
