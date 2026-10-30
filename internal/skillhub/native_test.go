package skillhub

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/actor"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/inbox"
)

// nativeEnv is a native collection ("team", created by usr_admin), a real
// Inbox on the same database, change proposals for the sensitive writes,
// and a clock the test moves.
type nativeEnv struct {
	svc     *Service
	inbox   *inbox.Service
	changes *ChangeProposals
	src     *Source

	mu  sync.Mutex
	now time.Time
}

// Viewers of the native tests: the editor's agent, the same person in the
// dashboard, and a member who may read but not write.
var (
	editorAgent  = Viewer{UserID: "usr_editor", AgentID: "agt_editor", Delegated: true}
	editorPerson = Viewer{UserID: "usr_editor"}
	readerAgent  = Viewer{UserID: "usr_reader", AgentID: "agt_reader", Delegated: true}
)

func newNativeEnv(t *testing.T, in SourceInput) *nativeEnv {
	t.Helper()
	ctx := context.Background()
	e := &nativeEnv{now: time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)}
	e.svc = newTestService(t, newFakeGitHub(t, "acme", "skills"), func(o *Options) { o.Now = e.clock })
	ib, err := inbox.New(ctx, inbox.Options{DB: e.svc.db, Now: e.clock})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ib.Flush)
	e.inbox = ib
	e.changes, err = NewChangeProposals(ChangeOptions{DB: e.svc.db, Inbox: ib, Reader: e.svc, Admin: e.svc,
		AgentName: func(_ context.Context, id string) string { return map[string]string{"agt_editor": "docs-bot"}[id] },
		Audit:     audit.New(e.svc.db), Now: e.clock})
	if err != nil {
		t.Fatal(err)
	}
	e.svc.SetNativeApprovals(e.changes)
	e.svc.SetAudit(audit.New(e.svc.db))
	in.Kind = KindNative
	if in.Slug == "" {
		in.Slug = "team"
	}
	if in.Editors == nil {
		in.Editors = []string{"usr_editor"}
	}
	e.src, err = e.svc.CreateSource(ctx, "usr_admin", in)
	if err != nil {
		t.Fatalf("CreateSource native: %v", err)
	}
	return e
}

func (e *nativeEnv) clock() time.Time { e.mu.Lock(); defer e.mu.Unlock(); return e.now }

func (e *nativeEnv) advance(d time.Duration) { e.mu.Lock(); e.now = e.now.Add(d); e.mu.Unlock() }

// deploySkill is a new skill's SKILL.md.
func deploySkill(t *testing.T, desc string) string {
	t.Helper()
	md, err := ComposeSkillMD(SkillDraft{Name: "deploy", Description: desc,
		Triggers: []string{"the user asks to ship a release"}, Tags: []string{"Ops", "release"}, Body: "# Deploy\n\n1. Build.\n2. Ship.\n"})
	if err != nil {
		t.Fatal(err)
	}
	return md
}

func (e *nativeEnv) write(t *testing.T, v Viewer, w SkillWrite) *WriteResult {
	t.Helper()
	res, err := e.svc.WriteSkill(context.Background(), v, w)
	if err != nil {
		t.Fatalf("WriteSkill %+v: %v", w, err)
	}
	return res
}

func (e *nativeEnv) auditCount(t *testing.T, event string) int {
	t.Helper()
	var n int
	if err := e.svc.db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE event_type = ?`, event).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestNativeCollectionSettings(t *testing.T) {
	e := newNativeEnv(t, SourceInput{Visibility: VisibleEveryone})
	ctx := context.Background()
	if e.src.Kind != KindNative || e.src.Mode != ModeAuto || e.src.Owner != nativeOwner || e.src.Repo != "team" ||
		len(e.src.Editors) != 1 || e.src.NextCheckAt == nil || e.src.NextCheckAt.UnixMilli() != builtinNever {
		t.Fatalf("collection %+v", e.src)
	}
	// No repository settings, no contributor collections.
	for name, in := range map[string]SourceInput{
		"url":     {Kind: KindNative, Slug: "a", URL: "acme/skills"},
		"pinned":  {Kind: KindNative, Slug: "b", Mode: ModePinned},
		"no slug": {Kind: KindNative},
	} {
		if _, err := e.svc.CreateSource(ctx, "usr_admin", in); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
	if _, err := e.svc.CreateSource(ctx, "usr_x", SourceInput{Kind: KindNative, Slug: "c", PendingApproval: true}); !errors.Is(err, ErrForbidden) {
		t.Errorf("contributor: %v, want ErrForbidden", err)
	}
	if _, err := e.svc.CreateSource(ctx, "usr_admin", SourceInput{Kind: KindNative, Slug: BuiltinSlug}); !errors.Is(err, ErrConflict) {
		t.Errorf("reserved slug: %v", err)
	}
	if _, err := e.svc.CreateSource(ctx, "usr_admin", SourceInput{URL: "acme/skills", Editors: []string{"u"}}); !errors.Is(err, ErrInvalid) {
		t.Errorf("editors on a GitHub source: %v", err)
	}
	// What a repository has cannot change; who sees and who writes can.
	branch := "main"
	if _, err := e.svc.UpdateSource(ctx, "usr_admin", e.src.ID, SourceUpdate{Branch: &branch}); !errors.Is(err, ErrConflict) {
		t.Errorf("branch change: %v", err)
	}
	review, editors := ModeReview, []string{"usr_editor", "usr_two"}
	src, err := e.svc.UpdateSource(ctx, "usr_admin", e.src.ID, SourceUpdate{Mode: &review, Editors: &editors})
	if err != nil || src.Mode != ModeReview || len(src.Editors) != 2 {
		t.Fatalf("update %+v %v", src, err)
	}
	// Never synced, no webhook, nothing to reject.
	if res, err := e.svc.SyncNow(ctx, e.src.ID); err != nil || !res.Unchanged {
		t.Errorf("SyncNow %+v %v", res, err)
	}
	if _, err := e.svc.RotateWebhookSecret(ctx, "usr_admin", e.src.ID); !errors.Is(err, ErrConflict) {
		t.Errorf("webhook: %v", err)
	}
}

func TestNativeCreateGetUpdateAndRevisions(t *testing.T) {
	e := newNativeEnv(t, SourceInput{Visibility: VisibleEveryone})
	ctx := context.Background()

	res := e.write(t, editorAgent, SkillWrite{Collection: "team", Files: map[string]string{
		"SKILL.md": deploySkill(t, "Deploy the service safely"), "references/checklist.md": "one\ntwo\n"}, Note: "first draft"})
	if res.Status != WriteLive || res.URI != "skill://team/deploy/SKILL.md" || res.Revision != 1 || res.Commit == "" || len(res.Files) != 2 {
		t.Fatalf("create %+v", res)
	}
	// Live at once, for every reader that sees the collection.
	sk, err := e.svc.Get(ctx, readerAgent, "team/deploy", true)
	if err != nil || sk.Description != "Deploy the service safely" || sk.Revision != 1 || len(sk.Files) != 2 ||
		!strings.HasPrefix(sk.Repo, "toolyard.native/team@") {
		t.Fatalf("get %+v %v", sk, err)
	}
	if hits, err := e.svc.Search(ctx, readerAgent, SearchQuery{Text: "deploy service"}); err != nil || len(hits) != 1 || hits[0].Revision != 1 {
		t.Fatalf("search %+v %v", hits, err)
	}
	if meta, _ := sk.Frontmatter["metadata"].(map[string]any); meta["tags"] != "ops, release" || meta["triggers"] != "the user asks to ship a release" {
		t.Fatalf("frontmatter %v", sk.Frontmatter)
	}
	// A second create of the same skill is refused.
	if _, err := e.svc.WriteSkill(ctx, editorAgent, SkillWrite{Collection: "team", Files: map[string]string{"SKILL.md": deploySkill(t, "x")}}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate create: %v", err)
	}

	// An update of the description keeps everything else.
	desc := "Deploy the service safely, with a rollback plan"
	md, err := PatchSkillMD(sk.Raw, SkillPatch{Description: &desc})
	if err != nil {
		t.Fatal(err)
	}
	res = e.write(t, editorAgent, SkillWrite{Skill: sk.URI, Files: map[string]string{"SKILL.md": md, "references/checklist.md": ""},
		BaseRevision: 1, Note: "add the rollback"})
	if res.Status != WriteLive || res.Revision != 2 {
		t.Fatalf("update %+v", res)
	}
	sk2, err := e.svc.Get(ctx, readerAgent, "team/deploy", true)
	if err != nil || sk2.Description != desc || sk2.Revision != 2 || len(sk2.Files) != 1 || !strings.Contains(sk2.Body, "1. Build.") {
		t.Fatalf("after update %+v %v", sk2, err)
	}
	// A write on an old revision is refused.
	if _, err := e.svc.WriteSkill(ctx, editorAgent, SkillWrite{Skill: sk.URI, Files: map[string]string{"notes.md": "x\n"}, BaseRevision: 1}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale base: %v", err)
	}
	// Nothing changes: refused.
	if _, err := e.svc.WriteSkill(ctx, editorAgent, SkillWrite{Skill: sk.URI, Files: map[string]string{"SKILL.md": md}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("no-op write: %v", err)
	}

	revs, err := e.svc.SkillRevisions(ctx, readerAgent, "team/deploy", 0)
	if err != nil || len(revs) != 2 || revs[0].Revision != 2 || !revs[0].Current || revs[1].Current ||
		revs[0].Author != "usr_editor" || revs[0].Agent != "agt_editor" || revs[0].Note != "add the rollback" || revs[1].Op != revisionCreate {
		t.Fatalf("revisions %+v %v", revs, err)
	}
	// Revision 1 stays readable: its SKILL.md and its deleted file.
	old, err := e.svc.GetRevision(ctx, readerAgent, "team/deploy", 1, true)
	if err != nil || old.Description != "Deploy the service safely" || old.Revision != 1 || len(old.Files) != 2 {
		t.Fatalf("revision 1 %+v %v", old, err)
	}
	var checklist string
	for _, f := range old.Files {
		if f.Path == "references/checklist.md" {
			checklist = f.URI
		}
	}
	if checklist != "skill://team/deploy/references/checklist.md?revision=1" {
		t.Fatalf("revision file URI %q", checklist)
	}
	f, err := e.svc.ReadRevision(ctx, readerAgent, checklist, 0, 0, 0)
	if err != nil || f.Content != "one\ntwo\n" || f.TotalLines != 2 {
		t.Fatalf("read revision %+v %v", f, err)
	}
	if _, err := e.svc.ReadRevision(ctx, readerAgent, checklist, 2, 0, 0); !errors.Is(err, ErrInvalid) {
		t.Fatalf("revision mismatch: %v", err)
	}
	if _, err := e.svc.GetRevision(ctx, readerAgent, "team/deploy", 9, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing revision: %v", err)
	}
	if n := e.auditCount(t, "skills.native_create"); n != 1 {
		t.Errorf("create audit rows %d", n)
	}
	if n := e.auditCount(t, "skills.native_update"); n != 1 {
		t.Errorf("update audit rows %d", n)
	}
}

func TestNativeWriteRule(t *testing.T) {
	e := newNativeEnv(t, SourceInput{Visibility: VisibleMembers, Members: []string{"usr_editor", "usr_reader"}})
	ctx := context.Background()
	e.write(t, editorAgent, SkillWrite{Collection: "team", Files: map[string]string{"SKILL.md": deploySkill(t, "Deploy")}})

	// A member who sees the collection but is no editor: refused.
	if _, err := e.svc.WriteSkill(ctx, readerAgent, SkillWrite{Skill: "team/deploy", Files: map[string]string{"notes.md": "x\n"}}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("reader writes: %v", err)
	}
	if _, err := e.svc.WriteSkill(ctx, readerAgent, SkillWrite{Collection: "team", Path: "other",
		Files: map[string]string{"SKILL.md": "---\nname: other\ndescription: d\n---\n"}}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("reader creates: %v", err)
	}
	// Outside the audience, the collection does not exist.
	outsider := Viewer{UserID: "usr_out", AgentID: "agt_out", Delegated: true}
	if _, err := e.svc.WriteSkill(ctx, outsider, SkillWrite{Collection: "team", Files: map[string]string{"SKILL.md": deploySkill(t, "x")}}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("outsider: %v", err)
	}
	if _, err := e.svc.SkillRevisions(ctx, outsider, "team/deploy", 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("outsider revisions: %v", err)
	}
	// Anonymous callers write nothing.
	if _, err := e.svc.WriteSkill(ctx, Viewer{}, SkillWrite{Skill: "team/deploy", Files: map[string]string{"a.md": "a"}}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("anonymous: %v", err)
	}
	// Admins and the creator write without being editors.
	admin := Viewer{UserID: "usr_admin", Admin: true}
	if res := e.write(t, admin, SkillWrite{Skill: "team/deploy", Files: map[string]string{"notes.md": "by admin\n"}}); res.Revision != 2 {
		t.Fatalf("admin write %+v", res)
	}
	// Bad paths and contents.
	for name, w := range map[string]SkillWrite{
		"escape":          {Skill: "team/deploy", Files: map[string]string{"../x.md": "x"}},
		"secret":          {Skill: "team/deploy", Files: map[string]string{"k.md": "token: " + "gh" + "p_" + strings.Repeat("a1B2", 9)}},
		"delete SKILL.md": {Skill: "team/deploy", Files: map[string]string{"SKILL.md": ""}},
		"rename skill":    {Skill: "team/deploy", Files: map[string]string{"SKILL.md": "---\nname: other\ndescription: d\n---\n"}},
		"bad folder":      {Collection: "team", Path: "Bad_Name", Files: map[string]string{"SKILL.md": "---\nname: Bad_Name\ndescription: d\n---\n"}},
		"no SKILL.md":     {Collection: "team", Path: "empty", Files: map[string]string{"a.md": "a"}},
	} {
		if _, err := e.svc.WriteSkill(ctx, editorAgent, w); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
	// A GitHub skill is changed on GitHub.
	if _, err := e.svc.WriteSkill(ctx, editorAgent, SkillWrite{Collection: BuiltinSlug, Files: map[string]string{"SKILL.md": deploySkill(t, "x")}}); err == nil {
		t.Fatal("a write into a source that is not native was taken")
	}
}

func TestNativeSensitiveWritesWaitForTheOwner(t *testing.T) {
	e := newNativeEnv(t, SourceInput{Visibility: VisibleEveryone})
	ctx := context.Background()
	e.write(t, editorAgent, SkillWrite{Collection: "team", Files: map[string]string{"SKILL.md": deploySkill(t, "Deploy")}})

	// A script: the agent's write becomes a change proposal.
	res := e.write(t, editorAgent, SkillWrite{Skill: "team/deploy", Files: map[string]string{"scripts/run.sh": "#!/bin/sh\necho ship\n"}, Note: "add the runner"})
	if res.Status != WritePending || res.Proposal == nil || res.Proposal.Status != "pending" || len(res.ApprovalReasons) != 1 ||
		!strings.Contains(res.ApprovalReasons[0], "scripts/run.sh") || res.Revision != 0 {
		t.Fatalf("sensitive write %+v", res)
	}
	if sk, _ := e.svc.Get(ctx, readerAgent, "team/deploy", true); len(sk.Files) != 1 {
		t.Fatalf("a pending write changed the skill: %+v", sk.Files)
	}
	item, err := e.inbox.Get(ctx, res.Proposal.InboxID)
	if err != nil || !strings.Contains(item.Message, "echo ship") || !strings.Contains(item.Message, "Why it needs your approval") {
		t.Fatalf("inbox item %+v %v", item, err)
	}
	// The owner approves in person: applied at once, as revision 2.
	r, _ := e.inbox.Get(ctx, res.Proposal.InboxID)
	if _, err := e.inbox.Decide(ctx, res.Proposal.InboxID, inbox.Decision{Action: "answer", RequestRevision: r.Revision, SubmissionID: "sub-1",
		Answers: map[string]inbox.Answer{reviewQuestionID: {Value: reviewApprove}},
		Decider: actor.Decider{UserID: "usr_admin", Name: "admin", Via: actor.ViaDashboard}}); err != nil {
		t.Fatal(err)
	}
	p, err := e.changes.GetProposal(ctx, editorAgent, res.Proposal.ID)
	if err != nil || p.Status != "done" || p.Result != "skill://team/deploy/SKILL.md revision 2" {
		t.Fatalf("proposal %+v %v", p, err)
	}
	sk, err := e.svc.Get(ctx, readerAgent, "team/deploy", true)
	if err != nil || len(sk.Files) != 2 || sk.Revision != 2 {
		t.Fatalf("after approval %+v %v", sk, err)
	}
	revs, _ := e.svc.SkillRevisions(ctx, readerAgent, "team/deploy", 0)
	if revs[0].ProposalID != p.ID || revs[0].Agent != "agt_editor" || revs[0].Author != "usr_editor" {
		t.Fatalf("revision %+v", revs[0])
	}
	// Applying the same proposal again writes nothing more.
	done, err := e.svc.applyNative(ctx, nativeApply{sourceID: e.src.ID, skillPath: "deploy", proposalID: p.ID})
	if err != nil || done.revision != 2 {
		t.Fatalf("second apply %+v %v", done, err)
	}

	// A person (the editor in the dashboard) writes a script directly.
	if res := e.write(t, editorPerson, SkillWrite{Skill: "team/deploy", Files: map[string]string{"scripts/run.sh": "#!/bin/sh\necho ship fast\n"}}); res.Status != WriteLive {
		t.Fatalf("person write %+v", res)
	}

	// allowed-tools and runs host are sensitive too; a rejection changes
	// nothing.
	cur, _ := e.svc.Get(ctx, readerAgent, "team/deploy", false)
	md := strings.Replace(cur.Raw, "name: deploy\n", "name: deploy\nallowed-tools: Bash\n", 1)
	res = e.write(t, editorAgent, SkillWrite{Skill: "team/deploy", Files: map[string]string{"SKILL.md": md}})
	if res.Status != WritePending || !strings.Contains(strings.Join(res.ApprovalReasons, ";"), "allowed-tools") {
		t.Fatalf("allowed-tools %+v", res)
	}
	r, _ = e.inbox.Get(ctx, res.Proposal.InboxID)
	if _, err := e.inbox.Decide(ctx, res.Proposal.InboxID, inbox.Decision{Action: "answer", RequestRevision: r.Revision, SubmissionID: "sub-2",
		Answers: map[string]inbox.Answer{reviewQuestionID: {Value: reviewReject}},
		Decider: actor.Decider{UserID: "usr_admin", Name: "admin", Via: actor.ViaDashboard}}); err != nil {
		t.Fatal(err)
	}
	if p, _ := e.changes.GetProposal(ctx, editorAgent, res.Proposal.ID); p.Status != "rejected" {
		t.Fatalf("rejected %+v", p)
	}
	if after, _ := e.svc.Get(ctx, readerAgent, "team/deploy", false); strings.Contains(after.Raw, "allowed-tools") {
		t.Fatal("a rejected write changed the skill")
	}
	if n := e.auditCount(t, "skills.native_propose"); n != 2 {
		t.Errorf("propose audit rows %d", n)
	}
}

func TestNativeApprovalFailsWhenTheSkillMovedOn(t *testing.T) {
	e := newNativeEnv(t, SourceInput{Visibility: VisibleEveryone})
	ctx := context.Background()
	e.write(t, editorAgent, SkillWrite{Collection: "team", Files: map[string]string{"SKILL.md": deploySkill(t, "Deploy")}})
	res := e.write(t, editorAgent, SkillWrite{Skill: "team/deploy", Files: map[string]string{"run.py": "print('x')\n"}})
	if res.Status != WritePending {
		t.Fatalf("%+v", res)
	}
	// Someone changes the skill before the owner decides.
	e.write(t, editorPerson, SkillWrite{Skill: "team/deploy", Files: map[string]string{"notes.md": "n\n"}})
	r, _ := e.inbox.Get(ctx, res.Proposal.InboxID)
	if _, err := e.inbox.Decide(ctx, res.Proposal.InboxID, inbox.Decision{Action: "answer", RequestRevision: r.Revision, SubmissionID: "sub-1",
		Answers: map[string]inbox.Answer{reviewQuestionID: {Value: reviewApprove}},
		Decider: actor.Decider{UserID: "usr_admin", Name: "admin", Via: actor.ViaDashboard}}); err == nil {
		t.Log("decision recorded; the handler reports the failure on the proposal")
	}
	p, err := e.changes.GetProposal(ctx, editorAgent, res.Proposal.ID)
	if err != nil || p.Status != "failed" || !strings.Contains(p.Error, "changed since") {
		t.Fatalf("proposal %+v %v", p, err)
	}
	if sk, _ := e.svc.Get(ctx, readerAgent, "team/deploy", true); len(sk.Files) != 2 {
		t.Fatalf("files %+v", sk.Files)
	}
}

func TestNativeReviewModeAndProposeChange(t *testing.T) {
	e := newNativeEnv(t, SourceInput{Visibility: VisibleEveryone, Mode: ModeReview})
	ctx := context.Background()
	// The editor in person writes directly; their agent waits for approval.
	if res := e.write(t, editorPerson, SkillWrite{Collection: "team", Files: map[string]string{"SKILL.md": deploySkill(t, "Deploy")}}); res.Status != WriteLive {
		t.Fatalf("person %+v", res)
	}
	res := e.write(t, editorAgent, SkillWrite{Skill: "team/deploy", Files: map[string]string{"notes.md": "plain text\n"}})
	if res.Status != WritePending || !strings.Contains(res.ApprovalReasons[0], "every agent change") {
		t.Fatalf("review mode %+v", res)
	}
	// skills.propose_change on a native skill needs no GitHub connection:
	// a reader's agent proposes, the owner decides.
	p, err := e.changes.ProposeChange(ctx, readerAgent, ChangeInput{Skill: "team/deploy", Title: "Clarify step two",
		Files: map[string]string{"SKILL.md": strings.Replace(deploySkill(t, "Deploy"), "2. Ship.", "2. Ship with care.", 1)}})
	if err != nil || p.Status != "pending" {
		t.Fatalf("propose_change %+v %v", p, err)
	}
	item, _ := e.inbox.Get(ctx, p.InboxID)
	if !strings.Contains(item.Message, "not an editor") || !strings.Contains(item.Message, "Ship with care") {
		t.Fatalf("item %s", item.Message)
	}
}

func TestNativeRevisionsOutliveRetention(t *testing.T) {
	e := newNativeEnv(t, SourceInput{Visibility: VisibleEveryone})
	ctx := context.Background()
	e.write(t, editorPerson, SkillWrite{Collection: "team", Files: map[string]string{"SKILL.md": deploySkill(t, "Deploy"), "v.md": "v0\n"}})
	for i := 1; i <= KeepVersions+3; i++ {
		e.advance(time.Second)
		e.write(t, editorPerson, SkillWrite{Skill: "team/deploy", Files: map[string]string{"v.md": strings.Repeat("v", i) + "\n"}})
	}
	var versions int
	if err := e.svc.db.QueryRow(`SELECT COUNT(*) FROM skill_versions WHERE source_id = ?`, e.src.ID).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if versions > KeepVersions+1 {
		t.Fatalf("retention kept %d versions", versions)
	}
	// Past the blob grace, the purge still keeps every revision's bytes.
	e.advance(2 * blobGrace)
	if err := e.svc.purgeBlobs(ctx); err != nil {
		t.Fatal(err)
	}
	f, err := e.svc.ReadRevision(ctx, readerAgent, "skill://team/deploy/v.md?revision=1", 0, 0, 0)
	if err != nil || f.Content != "v0\n" {
		t.Fatalf("revision 1 after retention %+v %v", f, err)
	}
	// Deleting the collection deletes its history.
	if err := e.svc.DeleteSource(ctx, "usr_admin", e.src.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := e.svc.db.QueryRow(`SELECT COUNT(*) FROM skill_revisions WHERE source_id = ?`, e.src.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("revisions left %d %v", n, err)
	}
}

func TestNativeRollbackClearsCurrentRevision(t *testing.T) {
	e := newNativeEnv(t, SourceInput{Visibility: VisibleEveryone})
	ctx := context.Background()
	e.write(t, editorPerson, SkillWrite{Collection: "team", Files: map[string]string{"SKILL.md": deploySkill(t, "Deploy")}})
	first, _ := e.svc.GetSource(ctx, e.src.ID)
	e.advance(time.Second)
	e.write(t, editorPerson, SkillWrite{Skill: "team/deploy", Files: map[string]string{"a.md": "a\n"}})
	// An admin rolls the collection back: revision 1's content is live.
	if err := e.svc.Activate(ctx, "usr_admin", e.src.ID, first.ActiveVersionID); err != nil {
		t.Fatal(err)
	}
	sk, err := e.svc.Get(ctx, readerAgent, "team/deploy", true)
	if err != nil || sk.Revision != 1 || len(sk.Files) != 1 {
		t.Fatalf("after rollback %+v %v", sk, err)
	}
	// A write on top of the rollback is revision 3, based on revision 1.
	res := e.write(t, editorPerson, SkillWrite{Skill: "team/deploy", Files: map[string]string{"b.md": "b\n"}, BaseRevision: 1})
	if res.Revision != 3 {
		t.Fatalf("write after rollback %+v", res)
	}
}

func TestComposeAndPatchSkillMD(t *testing.T) {
	md, err := ComposeSkillMD(SkillDraft{Name: "pdf-fill", Description: "Fill PDF forms: \"quoted\" and: colons",
		Triggers: []string{"the user sends a PDF form", " "}, Tags: []string{"Docs, PDF"}, Metadata: map[string]string{"owner": "ops"}})
	if err != nil {
		t.Fatal(err)
	}
	meta, warns, skip := parseSkillMD("pdf-fill/SKILL.md", "pdf-fill", []byte(md))
	if skip || meta.description != "Fill PDF forms: \"quoted\" and: colons" {
		t.Fatalf("composed %q: %+v %v", md, meta, warns)
	}
	m, _ := meta.frontmatter["metadata"].(map[string]any)
	if m["triggers"] != "the user sends a PDF form" || m["tags"] != "docs  pdf" || m["owner"] != "ops" {
		t.Fatalf("metadata %v", m)
	}
	for _, bad := range []SkillDraft{{Name: "Bad Name", Description: "d"}, {Name: "ok"}, {Name: "ok", Description: strings.Repeat("d", 1025)}} {
		if _, err := ComposeSkillMD(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v: %v", bad, err)
		}
	}

	raw := "---\nname: deploy\nlicense: MIT # keep me\ndescription: old\nmetadata:\n  owner: ops\n  tags: a, b\n---\n# Body\n"
	desc, body := "new words", "# New body\nstep\n"
	triggers, tags := []string{"when shipping"}, []string{}
	out, err := PatchSkillMD(raw, SkillPatch{Description: &desc, Body: &body, Triggers: &triggers, Tags: &tags})
	if err != nil {
		t.Fatal(err)
	}
	meta, _, skip = parseSkillMD("deploy/SKILL.md", "deploy", []byte(out))
	m, _ = meta.frontmatter["metadata"].(map[string]any)
	if skip || meta.description != "new words" || meta.frontmatter["license"] != "MIT" || m["owner"] != "ops" || m["tags"] != nil ||
		m["triggers"] != "when shipping" || !strings.HasSuffix(out, "\n# New body\nstep\n") || !strings.Contains(out, "# keep me") {
		t.Fatalf("patched:\n%s", out)
	}
	// Name stays first: the patch edits in place.
	if !strings.HasPrefix(out, "---\nname: deploy\n") {
		t.Fatalf("order:\n%s", out)
	}
	empty := ""
	if _, err := PatchSkillMD(raw, SkillPatch{Description: &empty}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty description: %v", err)
	}
}

func TestSensitiveReasons(t *testing.T) {
	src := &Source{Mode: ModeAuto}
	cases := []struct {
		name  string
		files []changeFile
		want  string
	}{
		{"doc", []changeFile{{Path: "references/a.md", Op: changeOpAdd, Content: "a"}}, ""},
		{"script ext", []changeFile{{Path: "tool.py", Op: changeOpAdd, Content: "x"}}, "tool.py"},
		{"scripts dir", []changeFile{{Path: "scripts/a.txt", Op: changeOpAdd, Content: "x"}}, "scripts/a.txt"},
		{"shebang", []changeFile{{Path: "run", Op: changeOpAdd, Content: "#!/usr/bin/env bash\n"}}, "run"},
		{"delete script", []changeFile{{Path: "bin/x", Op: changeOpDelete, Old: "x"}}, "bin/x"},
		{"runs host", []changeFile{{Path: skillFile, Op: changeOpAdd, Content: "---\nname: a\ndescription: d\nmetadata:\n  toolyard-runs: host\n---\n"}}, "where the skill runs"},
	}
	for _, c := range cases {
		got := strings.Join(sensitiveReasons(src, nil, c.files), "; ")
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	protected := &skillEntry{frontmatter: []byte(`{"name":"a","metadata":{"toolyard-protected":"true"}}`)}
	if got := sensitiveReasons(src, protected, []changeFile{{Path: "a.md", Op: changeOpAdd, Content: "a"}}); len(got) != 1 || !strings.Contains(got[0], "protected") {
		t.Errorf("protected: %v", got)
	}
	if got := sensitiveReasons(&Source{Mode: ModeReview}, nil, nil); len(got) != 1 {
		t.Errorf("review mode: %v", got)
	}
}

func TestNativeKeepsOneSkillPerFolder(t *testing.T) {
	e := newNativeEnv(t, SourceInput{Visibility: VisibleEveryone})
	ctx := context.Background()
	group := "---\nname: group\ndescription: A group skill\n---\nbody\n"
	e.write(t, editorPerson, SkillWrite{Collection: "team", Files: map[string]string{"SKILL.md": group}})
	for name, w := range map[string]SkillWrite{
		"inside another skill": {Collection: "team", Path: "group/private", Files: map[string]string{"SKILL.md": "---\nname: private\ndescription: d\n---\n"}},
		"a child manifest":     {Collection: "team", Path: "other", Files: map[string]string{"SKILL.md": "---\nname: other\ndescription: d\n---\n", "child/SKILL.md": "---\nname: child\ndescription: d\n---\n"}},
		"a child on update":    {Skill: "team/group", Files: map[string]string{"child/SKILL.md": "---\nname: child\ndescription: d\nallowed-tools: Bash\n---\n"}},
	} {
		if _, err := e.svc.WriteSkill(ctx, editorAgent, w); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
	// Under the lock too: an approved proposal cannot smuggle in a child.
	_, err := e.svc.applyNative(ctx, nativeApply{sourceID: e.src.ID, skillPath: "group", proposalID: "skp_x",
		files: []changeFile{{Path: "child/SKILL.md", Op: changeOpAdd, Content: "---\nname: child\ndescription: d\n---\n"}}})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("apply with a child manifest: %v", err)
	}
	if sk, _ := e.svc.Get(ctx, readerAgent, "team/group", true); len(sk.Files) != 1 {
		t.Fatalf("files %+v", sk.Files)
	}
}

func TestNativeChecksAgainUnderTheLock(t *testing.T) {
	e := newNativeEnv(t, SourceInput{Visibility: VisibleEveryone})
	ctx := context.Background()
	e.write(t, editorPerson, SkillWrite{Collection: "team", Files: map[string]string{"SKILL.md": deploySkill(t, "Deploy"), "run": "#!/bin/sh\necho a\n"}})
	files := []changeFile{{Path: "notes.md", Op: changeOpAdd, Content: "n\n"}}
	// The editor lost the right after WriteSkill checked it.
	if _, err := e.svc.applyNative(ctx, nativeApply{sourceID: e.src.ID, skillPath: "deploy", files: files, viewer: readerAgent,
		author: readerAgent.UserID}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("not an editor: %v", err)
	}
	// The collection switched to review after WriteSkill checked it.
	review := ModeReview
	if _, err := e.svc.UpdateSource(ctx, "usr_admin", e.src.ID, SourceUpdate{Mode: &review}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.applyNative(ctx, nativeApply{sourceID: e.src.ID, skillPath: "deploy", files: files, viewer: editorAgent,
		author: editorAgent.UserID}); !errors.Is(err, ErrConflict) {
		t.Fatalf("now needs approval: %v", err)
	}
	auto := ModeAuto
	if _, err := e.svc.UpdateSource(ctx, "usr_admin", e.src.ID, SourceUpdate{Mode: &auto}); err != nil {
		t.Fatal(err)
	}
	// Dropping a script's #! line still changes the script.
	res := e.write(t, editorAgent, SkillWrite{Skill: "team/deploy", Files: map[string]string{"run": "echo b\n"}})
	if res.Status != WritePending || !strings.Contains(strings.Join(res.ApprovalReasons, ";"), "run") {
		t.Fatalf("shebang removal %+v", res)
	}
}

func TestNativeRevisionHistoryIsAtomicAndRollbackSafe(t *testing.T) {
	e := newNativeEnv(t, SourceInput{Visibility: VisibleEveryone})
	ctx := context.Background()
	e.write(t, editorPerson, SkillWrite{Collection: "team", Files: map[string]string{"SKILL.md": deploySkill(t, "Deploy"), "v.md": "one\n"}})
	first, _ := e.svc.GetSource(ctx, e.src.ID)
	e.advance(time.Second)
	e.write(t, editorPerson, SkillWrite{Skill: "team/deploy", Files: map[string]string{"v.md": "two\n"}})
	second, _ := e.svc.GetSource(ctx, e.src.ID)
	// A failure inside the activation's transaction leaves the live
	// version as it was.
	_, _, err := e.svc.activate(ctx, activation{by: "usr_admin", sourceID: e.src.ID, versionID: first.ActiveVersionID,
		inTx: func(*sql.Tx) error { return errors.New("injected") }})
	if err == nil {
		t.Fatal("the injected failure did not stop the activation")
	}
	if now, _ := e.svc.GetSource(ctx, e.src.ID); now.ActiveVersionID != second.ActiveVersionID {
		t.Fatalf("active %s, want %s", now.ActiveVersionID, second.ActiveVersionID)
	}
	// An older build's purge sees only skill_files: emptying skill_blobs
	// of everything no version holds leaves every revision readable.
	if _, err := e.svc.db.ExecContext(ctx, `DELETE FROM skill_versions WHERE id <> ?`, second.ActiveVersionID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.db.ExecContext(ctx, `DELETE FROM skill_files WHERE version_id <> ?`, second.ActiveVersionID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.db.ExecContext(ctx, `DELETE FROM skill_blobs WHERE NOT EXISTS (SELECT 1 FROM skill_files f WHERE f.sha256 = skill_blobs.sha256)`); err != nil {
		t.Fatal(err)
	}
	f, err := e.svc.ReadRevision(ctx, readerAgent, "skill://team/deploy/v.md?revision=1", 0, 0, 0)
	if err != nil || f.Content != "one\n" {
		t.Fatalf("revision 1 after an old purge %+v %v", f, err)
	}
	if sk, err := e.svc.GetRevision(ctx, readerAgent, "team/deploy", 1, false); err != nil || sk.Description != "Deploy" {
		t.Fatalf("revision 1 SKILL.md %+v %v", sk, err)
	}
}

func TestNativeRoundTwoRules(t *testing.T) {
	e := newNativeEnv(t, SourceInput{Visibility: VisibleMembers, Members: []string{"usr_editor", "usr_reader"}})
	ctx := context.Background()
	// An empty collection is listed for its audience, with its kind, and
	// for nobody else.
	list, err := e.svc.Sources(ctx, readerAgent)
	if err != nil || len(list) != 1 || list[0].Kind != KindNative || list[0].SkillCount != 0 {
		t.Fatalf("sources %+v %v", list, err)
	}
	if list, _ := e.svc.Sources(ctx, Viewer{UserID: "usr_out", AgentID: "agt_out", Delegated: true}); len(list) != 0 {
		t.Fatalf("an outsider lists %+v", list)
	}
	// Under the lock, a create checks the audience again.
	outsider := Viewer{UserID: "usr_out", Admin: false}
	if _, err := e.svc.applyNative(ctx, nativeApply{sourceID: e.src.ID, skillPath: "deploy", create: true, viewer: outsider, author: "usr_out",
		files: []changeFile{{Path: skillFile, Op: changeOpAdd, Content: deploySkill(t, "Deploy")}}}); !errors.Is(err, ErrForbidden) && !errors.Is(err, ErrNotFound) {
		t.Fatalf("outsider create under the lock: %v", err)
	}
	// A revision keeps the commit it made live.
	first := e.write(t, editorPerson, SkillWrite{Collection: "team", Files: map[string]string{"SKILL.md": deploySkill(t, "Deploy")}})
	e.advance(time.Second)
	second := e.write(t, editorPerson, SkillWrite{Skill: "team/deploy", Files: map[string]string{"a.md": "a\n"}})
	old, err := e.svc.GetRevision(ctx, readerAgent, "team/deploy", 1, false)
	if err != nil || old.Commit != first.Commit || old.Commit == second.Commit || !strings.HasSuffix(old.Repo, "@"+first.Commit[:12]) {
		t.Fatalf("revision 1 provenance %+v %v (first %s, second %s)", old.SkillSummary, err, first.Commit, second.Commit)
	}
	// A change of the source after the checks refuses the activation.
	cur, _ := e.svc.GetSource(ctx, e.src.ID)
	_, _, err = e.svc.activate(ctx, activation{by: "usr_admin", sourceID: e.src.ID, versionID: cur.ActiveVersionID, expectUpdated: cur.UpdatedAt.UnixMilli() - 1})
	if !errors.Is(err, errSourceChanged) {
		t.Fatalf("stale expectUpdated: %v", err)
	}
}

func TestNativeChecksTheRowNotTheCachedIndex(t *testing.T) {
	e := newNativeEnv(t, SourceInput{Visibility: VisibleEveryone})
	ctx := context.Background()
	e.write(t, editorPerson, SkillWrite{Collection: "team", Files: map[string]string{"SKILL.md": deploySkill(t, "Deploy")}})
	// Another replica narrows the audience to nobody but an outsider; this
	// node has not heard of it yet (no reload), so its index still says
	// everyone.
	if _, err := e.svc.db.ExecContext(ctx, `UPDATE skill_sources SET visibility = 'members', members_json = '["usr_other"]',
		updated_at = updated_at + 1 WHERE id = ?`, e.src.ID); err != nil {
		t.Fatal(err)
	}
	if si := e.svc.idx.get(e.src.ID); si == nil || si.src.Visibility != VisibleEveryone {
		t.Fatalf("the cached index was reloaded: %+v", si)
	}
	_, err := e.svc.applyNative(ctx, nativeApply{sourceID: e.src.ID, skillPath: "deploy", viewer: editorAgent, author: editorAgent.UserID,
		files: []changeFile{{Path: "notes.md", Op: changeOpAdd, Content: "n\n"}}})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("a write after the audience was revoked elsewhere: %v", err)
	}
}
