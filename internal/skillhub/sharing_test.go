package skillhub

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"sort"
	"strings"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/actor"
	"github.com/tusharbhardwaj/toolyard/internal/inbox"
	"github.com/tusharbhardwaj/toolyard/internal/settings"
	"github.com/tusharbhardwaj/toolyard/internal/store/storetest"
)

// The sharing rule (index.go, srcIndex.skillVisible) over a hand-built
// index: source audience × the skill's own mode × block lists × approval,
// for admins, members, the creator and their agents.
func TestSharingRuleMatrix(t *testing.T) {
	const creator = "usr_creator"
	var (
		adminDash    = Viewer{UserID: "usr_admin", Admin: true}
		adminAgent   = Viewer{UserID: "usr_admin", AgentID: "agt_admin", Admin: true}
		creatorDash  = Viewer{UserID: creator}
		creatorAgent = Viewer{UserID: creator, AgentID: "agt_creator"}
		listed       = Viewer{UserID: "usr_listed", AgentID: "agt_listed"}
		other        = Viewer{UserID: "usr_other", AgentID: "agt_other"}
		blocked      = Viewer{UserID: "usr_blocked", AgentID: "agt_blocked"}
		blockedDash  = Viewer{UserID: "usr_blocked"}
		oauthApp     = Viewer{UserID: "usr_listed", AgentID: "agt_oauth_app"} // an OAuth app's agent is its owner
	)
	type viewers struct{ adminDash, adminAgent, creatorDash, creatorAgent, listed, other, blocked, oauth bool }
	cases := []struct {
		name       string
		vis        Visibility
		approval   Approval
		srcBlocked bool
		share      *SkillSharing
		want       viewers
	}{
		{name: "everyone", vis: VisibleEveryone,
			want: viewers{true, true, true, true, true, true, true, true}},
		{name: "everyone, source block list", vis: VisibleEveryone, srcBlocked: true,
			want: viewers{true, true, true, true, true, true, false, true}},
		{name: "everyone, skill block list", vis: VisibleEveryone, share: &SkillSharing{Blocked: []string{"usr_blocked"}},
			want: viewers{true, true, true, true, true, true, false, true}},
		{name: "members", vis: VisibleMembers,
			want: viewers{true, true, false, false, true, false, false, true}},
		{name: "members, listed person blocked", vis: VisibleMembers, share: &SkillSharing{Blocked: []string{"usr_listed"}},
			want: viewers{true, true, false, false, false, false, false, false}},
		{name: "nobody", vis: VisibleNobody,
			want: viewers{true, false, true, false, false, false, false, false}},
		{name: "everyone, skill nobody", vis: VisibleEveryone, share: &SkillSharing{Mode: VisibleNobody},
			want: viewers{true, false, true, false, false, false, false, false}},
		{name: "nobody, skill everyone", vis: VisibleNobody, share: &SkillSharing{Mode: VisibleEveryone},
			want: viewers{true, true, true, true, true, true, true, true}},
		{name: "nobody, skill everyone, source block list", vis: VisibleNobody, srcBlocked: true, share: &SkillSharing{Mode: VisibleEveryone},
			want: viewers{true, true, true, true, true, true, false, true}},
		{name: "everyone, skill members", vis: VisibleEveryone, share: &SkillSharing{Mode: VisibleMembers, Members: []string{"usr_other"}},
			want: viewers{true, true, false, false, false, true, false, false}},
		{name: "members, skill inherit with a block list", vis: VisibleMembers, share: &SkillSharing{Blocked: []string{"usr_other"}},
			want: viewers{true, true, false, false, true, false, false, true}},
		{name: "pending approval", vis: VisibleEveryone, approval: ApprovalPending,
			want: viewers{true, false, true, false, false, false, false, false}},
		{name: "pending approval, skill everyone", vis: VisibleEveryone, approval: ApprovalPending, share: &SkillSharing{Mode: VisibleEveryone},
			want: viewers{true, false, true, false, false, false, false, false}},
		{name: "rejected", vis: VisibleEveryone, approval: ApprovalRejected,
			want: viewers{true, false, true, false, false, false, false, false}},
		{name: "approved", vis: VisibleEveryone, approval: ApprovalApproved,
			want: viewers{true, true, true, true, true, true, true, true}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := Source{ID: "src_x", Slug: "x", Visibility: c.vis, Members: []string{"usr_listed"}, Approval: c.approval, CreatedBy: creator}
			if c.srcBlocked {
				src.Blocked = []string{"usr_blocked"}
			}
			si := &srcIndex{src: src, versionID: "skv_x", members: setOf(src.Members), blocked: setOf(src.Blocked),
				shares: map[string]*skillShare{}, byPath: map[string]*skillEntry{}}
			e := &skillEntry{path: "s", name: "s"}
			si.skills, si.byPath["s"] = []*skillEntry{e}, e
			if c.share != nil {
				sh, err := normalizeSharing(*c.share)
				if err != nil {
					t.Fatal(err)
				}
				si.shares["s"] = &skillShare{mode: sh.Mode, members: setOf(sh.Members), blocked: setOf(sh.Blocked)}
			}
			got := viewers{
				si.skillVisible(adminDash, "s"), si.skillVisible(adminAgent, "s"),
				si.skillVisible(creatorDash, "s"), si.skillVisible(creatorAgent, "s"),
				si.skillVisible(listed, "s"), si.skillVisible(other, "s"),
				si.skillVisible(blocked, "s"), si.skillVisible(oauthApp, "s"),
			}
			if got != c.want {
				t.Fatalf("got  %+v\nwant %+v", got, c.want)
			}
			// The source is visible exactly when its one skill is.
			if si.visibleTo(other) != c.want.other || si.visibleTo(adminAgent) != c.want.adminAgent {
				t.Fatalf("source visibility differs from its skill's")
			}
			// An admin sees past a block list; a blocked person in the
			// dashboard sees nothing a block list covers.
			if c.srcBlocked && si.skillVisible(blockedDash, "s") && c.vis != VisibleNobody {
				t.Fatal("a blocked person sees the skill in the dashboard")
			}
		})
	}
	// Nothing is visible without an active version.
	si := &srcIndex{src: Source{Visibility: VisibleEveryone}, shares: map[string]*skillShare{}}
	if si.skillVisible(adminDash, "") || si.visibleTo(adminDash) {
		t.Fatal("a source without an active version is visible")
	}

	// An admin's hide lock beats a skill shared with everyone; a
	// delegated caller (an operator token's CLI, the voice assistant) is
	// an agent for audience nobody.
	hidden := &srcIndex{src: Source{Visibility: VisibleEveryone, Hidden: true, CreatedBy: creator}, versionID: "skv_x",
		shares: map[string]*skillShare{"s": {mode: VisibleEveryone}}, byPath: map[string]*skillEntry{}}
	delegatedAdmin := Viewer{UserID: "usr_admin", Admin: true, Delegated: true}
	for name, c := range map[string]struct {
		v    Viewer
		want bool
	}{
		"admin in the dashboard": {adminDash, true}, "admin's agent": {adminAgent, false}, "member": {other, false},
		"creator in the dashboard": {creatorDash, true}, "delegated admin": {delegatedAdmin, false},
		"delegated creator": {Viewer{UserID: creator, Delegated: true}, false},
	} {
		if got := hidden.skillVisible(c.v, "s"); got != c.want {
			t.Errorf("hidden source, %s: %v, want %v", name, got, c.want)
		}
	}
	shared := &srcIndex{src: Source{Visibility: VisibleEveryone}, versionID: "skv_x", shares: map[string]*skillShare{}}
	if !shared.skillVisible(delegatedAdmin, "s") {
		t.Fatal("a delegated caller does not see a skill shared with everyone")
	}
}

func TestNormalizeSharing(t *testing.T) {
	if _, err := normalizeSharing(SkillSharing{Mode: "public"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad mode: %v", err)
	}
	sh, err := normalizeSharing(SkillSharing{Mode: "inherit", Members: []string{"a"}, Blocked: []string{" b ", "b", ""}})
	if err != nil || sh.Mode != VisibleInherit || sh.Members != nil || len(sh.Blocked) != 1 || sh.Blocked[0] != "b" {
		t.Fatalf("inherit: %+v %v", sh, err)
	}
	if !(SkillSharing{}).IsZero() || (SkillSharing{Blocked: []string{"x"}}).IsZero() || (SkillSharing{Mode: VisibleNobody}).IsZero() {
		t.Fatal("IsZero")
	}
	many := make([]string, maxMembers+1)
	for i := range many {
		many[i] = "u" + strings.Repeat("x", i%7) + string(rune('a'+i%26)) + string(rune('a'+i/26%26)) + string(rune('a'+i/676))
	}
	if _, err := normalizeSharing(SkillSharing{Mode: VisibleMembers, Members: many}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("too many members: %v", err)
	}
}

// sharingFixture is one public source ("skills": pr-review with the nested
// skill pr-review/nested-helper, and deploy) synced once.
func sharingFixture(t *testing.T) (*Service, *fakeGitHub, *Source) {
	t.Helper()
	gh := newFakeGitHub(t, "acme", "skills")
	tree := baseTree()
	tree["skills/pr-review/references/style.md"] = "Prefer small diffs.\n"
	tree["skills/pr-review/nested-helper/SKILL.md"] = skillMD("nested-helper", "A helper inside pr-review.", "", "helper body secret-word\n")
	tree["skills/pr-review/nested-helper/notes.md"] = "helper notes secret-word\n"
	gh.commit("main", tree)
	svc := newTestService(t, gh)
	src, res := mustCreate(t, svc, SourceInput{URL: "acme/skills", Path: "skills", Mode: ModeAuto})
	if !res.Activated {
		t.Fatalf("sync %+v", res)
	}
	return svc, gh, src
}

// bundleNames is the file names in a bundle.
func bundleNames(t *testing.T, data []byte) []string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var out []string
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, h.Name)
	}
	sort.Strings(out)
	return out
}

// Every read path honours a skill set to nobody: search, get (and its file
// list), read, directory reads, grep, list, bundle and sources; an admin in
// the dashboard still sees it, an admin's agent does not.
func TestSkillNobodyOnEveryReadPath(t *testing.T) {
	svc, gh, src := sharingFixture(t)
	ctx := context.Background()
	adminAgent := Viewer{UserID: "usr_admin", AgentID: "agt_admin", Admin: true}
	before, err := svc.SetSkillSharing(ctx, "usr_admin", src.ID, "pr-review/nested-helper", SkillSharing{Mode: VisibleNobody})
	if err != nil || !before.IsZero() {
		t.Fatalf("set sharing: %+v %v", before, err)
	}
	const hidden = "skill://skills/pr-review/nested-helper/SKILL.md"
	for name, v := range map[string]Viewer{"member": memberViewer, "admin's agent": adminAgent} {
		if hits, err := svc.Search(ctx, v, SearchQuery{Text: "helper"}); err != nil || len(hits) != 0 {
			t.Errorf("%s search: %+v %v", name, hits, err)
		}
		for _, ref := range []string{hidden, "skills/pr-review/nested-helper", "nested-helper"} {
			if _, err := svc.Get(ctx, v, ref, true); !errors.Is(err, ErrNotFound) {
				t.Errorf("%s get %s: %v", name, ref, err)
			}
		}
		if _, err := svc.Read(ctx, v, "skill://skills/pr-review/nested-helper/notes.md", 0, 0); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s read: %v", name, err)
		}
		if _, err := svc.ListDir(ctx, v, "skill://skills/pr-review/nested-helper"); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s list dir of the hidden skill: %v", name, err)
		}
		// A hidden file is not even named as a file.
		if _, err := svc.ListDir(ctx, v, "skill://skills/pr-review/nested-helper/notes.md"); !errors.Is(err, ErrNotFound) || strings.Contains(err.Error(), "is a file") {
			t.Errorf("%s list dir of a hidden file: %v", name, err)
		}
		dir, err := svc.ListDir(ctx, v, "skill://skills/pr-review")
		if err != nil {
			t.Fatalf("%s list dir: %v", name, err)
		}
		for _, d := range dir {
			if d.Name == "nested-helper" {
				t.Errorf("%s list dir shows the hidden skill's folder", name)
			}
		}
		sk, err := svc.Get(ctx, v, "skill://skills/pr-review/SKILL.md", true)
		if err != nil {
			t.Fatalf("%s get pr-review: %v", name, err)
		}
		for _, f := range sk.Files {
			if strings.HasPrefix(f.Path, "nested-helper/") {
				t.Errorf("%s: pr-review's file list holds the hidden %s", name, f.Path)
			}
		}
		if hits, err := svc.Grep(ctx, v, GrepQuery{Pattern: "secret-word", IncludeHost: true}); err != nil || len(hits) != 0 {
			t.Errorf("%s grep: %+v %v", name, hits, err)
		}
		list, _, err := svc.List(ctx, v, ListQuery{IncludeHost: true})
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range list {
			if s.URI == hidden {
				t.Errorf("%s list holds the hidden skill", name)
			}
			for _, f := range s.Files {
				if strings.Contains(f.URI, "nested-helper") {
					t.Errorf("%s list: %s lists %s", name, s.Name, f.URI)
				}
			}
		}
		var buf bytes.Buffer
		if err := svc.Bundle(ctx, v, []string{"skills/pr-review"}, &buf); err != nil {
			t.Fatalf("%s bundle: %v", name, err)
		}
		for _, n := range bundleNames(t, buf.Bytes()) {
			if strings.Contains(n, "nested-helper") {
				t.Errorf("%s bundle holds %s", name, n)
			}
		}
		if err := svc.Bundle(ctx, v, []string{hidden}, io.Discard); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s bundle of the hidden skill: %v", name, err)
		}
		srcs, _ := svc.Sources(ctx, v)
		if len(srcs) != 1 || srcs[0].SkillCount != 2 {
			t.Errorf("%s sources: %+v (want 2 visible skills)", name, srcs)
		}
	}
	// An admin in the dashboard sees it everywhere.
	if _, err := svc.Get(ctx, adminViewer, hidden, true); err != nil {
		t.Fatalf("admin get: %v", err)
	}
	if _, err := svc.ListDir(ctx, adminViewer, "skill://skills/pr-review/nested-helper/notes.md"); err == nil || !strings.Contains(err.Error(), "is a file") {
		t.Fatalf("admin list dir of a file: %v", err)
	}
	if hits, _ := svc.Grep(ctx, adminViewer, GrepQuery{Pattern: "secret-word"}); len(hits) != 2 {
		t.Fatalf("admin grep: %+v", hits)
	}
	// The admin listing shows the setting.
	usage, err := svc.Skills(ctx, src.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, u := range usage {
		if u.Path == "pr-review/nested-helper" {
			found = u.Sharing != nil && u.Sharing.Mode == VisibleNobody
		} else if u.Sharing != nil {
			t.Errorf("%s has a sharing setting %+v", u.Path, u.Sharing)
		}
	}
	if !found {
		t.Fatalf("usage rows %+v", usage)
	}
	// The setting survives a new version.
	gh.commit("main", map[string]string{"skills/pr-review/nested-helper/notes.md": "v2 secret-word\n"})
	if res, err := svc.SyncNow(ctx, src.ID); err != nil || !res.Activated {
		t.Fatalf("second sync %+v %v", res, err)
	}
	if _, err := svc.Get(ctx, memberViewer, hidden, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after a new version: %v", err)
	}
	// Clearing it shows it again; the replaced setting comes back.
	prev, err := svc.SetSkillSharing(ctx, "usr_admin", src.ID, "pr-review/nested-helper", SkillSharing{Mode: "inherit"})
	if err != nil || prev.Mode != VisibleNobody {
		t.Fatalf("clear: %+v %v", prev, err)
	}
	if _, err := svc.Get(ctx, memberViewer, hidden, false); err != nil {
		t.Fatalf("after clearing: %v", err)
	}
	// Setting one for a skill the version lacks is refused; clearing any
	// path works.
	if _, err := svc.SetSkillSharing(ctx, "usr_admin", src.ID, "nope", SkillSharing{Mode: VisibleNobody}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing skill: %v", err)
	}
	if _, err := svc.SetSkillSharing(ctx, "usr_admin", src.ID, "nope", SkillSharing{}); err != nil {
		t.Fatalf("clearing a missing skill: %v", err)
	}
	// Deleting the source drops its settings.
	if _, err := svc.SetSkillSharing(ctx, "usr_admin", src.ID, "deploy", SkillSharing{Blocked: []string{"usr_member"}}); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteSource(ctx, "usr_admin", src.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := svc.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM skill_sharing WHERE source_id = ?`, src.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("sharing rows after delete: %d %v", n, err)
	}
}

// A blocked person (source or skill block list) is refused on every read
// path; everyone else is not.
func TestBlockedPersonOnEveryReadPath(t *testing.T) {
	svc, _, src := sharingFixture(t)
	ctx := context.Background()
	blocked := Viewer{UserID: "usr_blocked", AgentID: "agt_blocked", OnHost: true}
	list := []string{"usr_blocked"}
	if _, err := svc.UpdateSource(ctx, "usr_admin", src.ID, SourceUpdate{Blocked: &list}); err != nil {
		t.Fatal(err)
	}
	got, err := svc.GetSource(ctx, src.ID)
	if err != nil || len(got.Blocked) != 1 || got.Blocked[0] != "usr_blocked" {
		t.Fatalf("blocked stored: %+v %v", got, err)
	}
	if srcs, _ := svc.Sources(ctx, blocked); len(srcs) != 0 {
		t.Fatalf("blocked sources: %+v", srcs)
	}
	if hits, _ := svc.Search(ctx, blocked, SearchQuery{Text: "review"}); len(hits) != 0 {
		t.Fatalf("blocked search: %+v", hits)
	}
	if _, err := svc.Get(ctx, blocked, "pr-review", false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("blocked get: %v", err)
	}
	if _, err := svc.Read(ctx, blocked, "skill://skills/pr-review/checks.md", 0, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("blocked read: %v", err)
	}
	if _, err := svc.ListDir(ctx, blocked, "skill://skills"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("blocked list dir: %v", err)
	}
	if hits, _ := svc.Grep(ctx, blocked, GrepQuery{Pattern: "diff"}); len(hits) != 0 {
		t.Fatalf("blocked grep: %+v", hits)
	}
	if l, _, _ := svc.List(ctx, blocked, ListQuery{}); len(l) != 0 {
		t.Fatalf("blocked list: %+v", l)
	}
	if err := svc.Bundle(ctx, blocked, []string{"pr-review"}, io.Discard); !errors.Is(err, ErrNotFound) {
		t.Fatalf("blocked bundle: %v", err)
	}
	if _, err := svc.Get(ctx, memberViewer, "pr-review", false); err != nil {
		t.Fatalf("another member: %v", err)
	}
	if _, err := svc.Get(ctx, Viewer{UserID: "usr_blocked", Admin: true}, "pr-review", false); err != nil {
		t.Fatalf("an admin is never blocked: %v", err)
	}

	// A skill's own block list adds to the source's.
	none := []string{}
	if _, err := svc.UpdateSource(ctx, "usr_admin", src.ID, SourceUpdate{Blocked: &none}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetSkillSharing(ctx, "usr_admin", src.ID, "pr-review", SkillSharing{Blocked: []string{"usr_member"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Get(ctx, memberViewer, "pr-review", false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("skill block list: %v", err)
	}
	if _, err := svc.Get(ctx, memberViewer, "skill://skills/deploy/SKILL.md", false); err != nil {
		t.Fatalf("another skill of the source: %v", err)
	}
	if _, err := svc.Get(ctx, blocked, "pr-review", false); err != nil {
		t.Fatalf("no longer blocked: %v", err)
	}
	// The nested skill's own files stay visible through it (only pr-review
	// itself is blocked).
	if _, err := svc.Get(ctx, memberViewer, "nested-helper", true); err != nil {
		t.Fatalf("nested skill of a blocked skill: %v", err)
	}
}

// A members-only source with one skill shared with everyone shows that
// skill (and the source, counting one skill) to every member; a source
// shared with nobody shows nothing to agents, and its skills to admins and
// its creator in the dashboard.
func TestSourceAudienceAndSkillOverride(t *testing.T) {
	svc, _, src := sharingFixture(t)
	ctx := context.Background()
	members := VisibleMembers
	list := []string{"usr_team"}
	if _, err := svc.UpdateSource(ctx, "usr_admin", src.ID, SourceUpdate{Visibility: &members, Members: &list}); err != nil {
		t.Fatal(err)
	}
	if srcs, _ := svc.Sources(ctx, memberViewer); len(srcs) != 0 {
		t.Fatalf("members-only source shown to a member: %+v", srcs)
	}
	if _, err := svc.SetSkillSharing(ctx, "usr_admin", src.ID, "deploy", SkillSharing{Mode: VisibleEveryone}); err != nil {
		t.Fatal(err)
	}
	srcs, _ := svc.Sources(ctx, memberViewer)
	if len(srcs) != 1 || srcs[0].SkillCount != 1 {
		t.Fatalf("sources with one shared skill: %+v", srcs)
	}
	if _, err := svc.Get(ctx, memberViewer, "deploy", false); err != nil {
		t.Fatalf("shared skill: %v", err)
	}
	if _, err := svc.Get(ctx, memberViewer, "pr-review", false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other skill: %v", err)
	}
	team := Viewer{UserID: "usr_team", AgentID: "agt_team"}
	if srcs, _ := svc.Sources(ctx, team); len(srcs) != 1 || srcs[0].SkillCount != 3 {
		t.Fatalf("team member sees %+v", srcs)
	}

	nobody := VisibleNobody
	if _, err := svc.UpdateSource(ctx, "usr_admin", src.ID, SourceUpdate{Visibility: &nobody}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetSkillSharing(ctx, "usr_admin", src.ID, "deploy", SkillSharing{}); err != nil {
		t.Fatal(err)
	}
	for name, v := range map[string]Viewer{
		"team agent": team, "admin's agent": {UserID: "usr_admin", AgentID: "agt_admin", Admin: true},
		"creator's agent": {UserID: "usr_admin", AgentID: "agt_creator"},
	} {
		if srcs, _ := svc.Sources(ctx, v); len(srcs) != 0 {
			t.Errorf("%s sees a nobody source: %+v", name, srcs)
		}
	}
	if srcs, _ := svc.Sources(ctx, adminViewer); len(srcs) != 1 {
		t.Fatalf("admin in the dashboard: %+v", srcs)
	}
	// mustCreate adds sources as usr_admin: the creator, in the dashboard
	// and without the admin role, still sees it.
	if _, err := svc.Get(ctx, Viewer{UserID: "usr_admin"}, "pr-review", false); err != nil {
		t.Fatalf("creator in the dashboard: %v", err)
	}
	if _, err := svc.CreateSource(ctx, "usr_admin", SourceInput{URL: "acme/skills", Slug: "bad", Path: "skills", Visibility: "public"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad visibility: %v", err)
	}
}

// reviewCounter counts review hook calls.
type reviewCounter struct{ n int }

func (r *reviewCounter) PendingVersion(context.Context, Source, Version) error { r.n++; return nil }

// A contributor's source syncs but serves nothing until approved; its
// waiting version raises no review question and goes live on approval.
// Rejecting disables it; an admin may still approve it later.
func TestContributorSourceApproval(t *testing.T) {
	gh := newFakeGitHub(t, "acme", "skills")
	gh.commit("main", baseTree())
	svc := newTestService(t, gh, trustAcme)
	hook := &reviewCounter{}
	svc.SetReviewHook(hook)
	ctx := context.Background()
	src, err := svc.CreateSource(ctx, "usr_contrib", SourceInput{URL: "acme/skills", Path: "skills", Mode: ModeReview, PendingApproval: true})
	if err != nil || src.Approval != ApprovalPending {
		t.Fatalf("create: %+v %v", src, err)
	}
	res, err := svc.SyncNow(ctx, src.ID)
	if err != nil || !res.Staged {
		t.Fatalf("sync: %+v %v", res, err)
	}
	if hook.n != 0 {
		t.Fatalf("the review hook ran %d times for a source pending approval", hook.n)
	}
	if _, err := svc.SyncNow(ctx, src.ID); err != nil || hook.n != 0 {
		t.Fatalf("second sync asked for review: %d %v", hook.n, err)
	}
	if srcs, _ := svc.Sources(ctx, memberViewer); len(srcs) != 0 {
		t.Fatalf("pending source shown: %+v", srcs)
	}
	// A waiting source is checked at most hourly.
	if p, _ := svc.GetSource(ctx, src.ID); p.NextCheckAt == nil || p.LastCheckedAt == nil || p.NextCheckAt.Sub(*p.LastCheckedAt) < pendingPoll {
		t.Fatalf("pending source checks: last %v next %v", p.LastCheckedAt, p.NextCheckAt)
	}
	got, err := svc.DecideApproval(ctx, "usr_admin", src.ID, true, "", res.SHA)
	if err != nil || got.Approval != ApprovalApproved || got.ApprovalBy != "usr_admin" || got.ActiveVersionID == "" || got.PendingVersionID != "" {
		t.Fatalf("approve: %+v %v", got, err)
	}
	if srcs, _ := svc.Sources(ctx, memberViewer); len(srcs) != 1 {
		t.Fatalf("approved source hidden: %+v", srcs)
	}
	if _, err := svc.DecideApproval(ctx, "usr_admin", src.ID, true, "", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("approve twice: %v", err)
	}
	// An approval whose question showed another commit makes the source
	// live but leaves the waiting version to a normal review question.
	stale, err := svc.CreateSource(ctx, "usr_contrib", SourceInput{URL: "acme/skills", Slug: "stale", Path: "skills", Mode: ModeReview, PendingApproval: true})
	if err != nil {
		t.Fatal(err)
	}
	if res, err := svc.SyncNow(ctx, stale.ID); err != nil || !res.Staged {
		t.Fatalf("stale sync: %+v %v", res, err)
	}
	before := hook.n
	st, err := svc.DecideApproval(ctx, "usr_admin", stale.ID, true, "", strings.Repeat("e", 40))
	if err != nil || st.Approval != ApprovalApproved || st.ActiveVersionID != "" || st.PendingVersionID == "" || hook.n != before+1 {
		t.Fatalf("approve with another commit: %+v %v (hook %d -> %d)", st, err, before, hook.n)
	}

	// Reject, then approve after all. An automatic source makes no commit
	// live while it waits: the commit waits for the approval.
	two, err := svc.CreateSource(ctx, "usr_contrib", SourceInput{URL: "acme/skills", Slug: "two", Path: "skills", Mode: ModeAuto, PendingApproval: true})
	if err != nil {
		t.Fatal(err)
	}
	twoRes, err := svc.SyncNow(ctx, two.ID)
	if err != nil || !twoRes.Staged || twoRes.Activated {
		t.Fatalf("auto sync of a pending source: %+v %v", twoRes, err)
	}
	if p, _ := svc.GetSource(ctx, two.ID); p.ActiveVersionID != "" || p.PendingVersionID != twoRes.VersionID {
		t.Fatalf("pending auto source: active %q pending %q", p.ActiveVersionID, p.PendingVersionID)
	}
	if srcs, _ := svc.Sources(ctx, memberViewer); len(srcs) != 1 {
		t.Fatalf("an auto-mode source pending approval is shown: %+v", srcs)
	}
	rej, err := svc.DecideApproval(ctx, "usr_admin", two.ID, false, "not ours", "")
	if err != nil || rej.Approval != ApprovalRejected || rej.Enabled || rej.ApprovalNote != "not ours" {
		t.Fatalf("reject: %+v %v", rej, err)
	}
	if _, err := svc.DecideApproval(ctx, "usr_admin", two.ID, false, "", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("reject twice: %v", err)
	}
	// An approval that names no commit, or another one, would make an
	// unseen commit live: refused.
	for _, sha := range []string{"", strings.Repeat("e", 40)} {
		if _, err := svc.DecideApproval(ctx, "usr_admin", two.ID, true, "", sha); !errors.Is(err, ErrConflict) {
			t.Fatalf("approve an automatic source with commit %q: %v", sha, err)
		}
	}
	again, err := svc.DecideApproval(ctx, "usr_admin", two.ID, true, "", strings.ToUpper(twoRes.SHA))
	if err != nil || again.Approval != ApprovalApproved || !again.Enabled || again.ActiveVersionID != twoRes.VersionID || again.PendingVersionID != "" {
		t.Fatalf("approve after a rejection: %+v %v", again, err)
	}
	// An admin's source needs no approval.
	adminSrc, _ := mustCreate(t, svc, SourceInput{URL: "acme/skills", Slug: "admins", Path: "skills", Mode: ModeAuto})
	if adminSrc.Approval != ApprovalNone {
		t.Fatalf("admin source approval %q", adminSrc.Approval)
	}
	if _, err := svc.DecideApproval(ctx, "usr_admin", adminSrc.ID, true, "", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("approving an admin's source: %v", err)
	}
	// After approval a review-mode source's next commit asks for review
	// as usual.
	gh.commit("main", map[string]string{"skills/pr-review/checks.md": "v2\n"})
	before = hook.n
	if res, err := svc.SyncNow(ctx, src.ID); err != nil || !res.Staged || hook.n != before+1 {
		t.Fatalf("next commit of an approved source: %+v %v (hook %d -> %d)", res, err, before, hook.n)
	}
}

// trustAcme makes the GitHub owner acme a trusted owner.
func trustAcme(o *Options) {
	o.Settings = staticSettings{settings.SkillsTrustedGitHubOwners: "acme"}
}

// The engine bounds a contributor's choices too: no pin, automatic only
// for a trusted owner (nobody is trusted here), no limits.
func TestContributorSourceBounds(t *testing.T) {
	gh := newFakeGitHub(t, "acme", "skills")
	gh.commit("main", baseTree())
	svc := newTestService(t, gh)
	ctx := context.Background()
	for name, in := range map[string]SourceInput{
		"pinned": {Mode: ModePinned, PinnedSHA: strings.Repeat("a", 40)},
		"auto":   {Mode: ModeAuto},
		"limits": {Mode: ModeReview, Limits: &Limits{MaxSkills: 5}},
	} {
		in.URL, in.Slug, in.Path, in.PendingApproval = "acme/skills", "bound-"+name, "skills", true
		if _, err := svc.CreateSource(ctx, "usr_contrib", in); !errors.Is(err, ErrInvalid) {
			t.Errorf("contributor %s: %v", name, err)
		}
	}
	// The default for a contributor is review; an admin may choose any.
	src, err := svc.CreateSource(ctx, "usr_contrib", SourceInput{URL: "acme/skills", Slug: "default", Path: "skills", PendingApproval: true})
	if err != nil || src.Mode != ModeReview {
		t.Fatalf("contributor default mode: %+v %v", src, err)
	}
	if _, err := svc.CreateSource(ctx, "usr_admin", SourceInput{URL: "acme/skills", Slug: "admins", Path: "skills", Mode: ModeAuto}); err != nil {
		t.Fatalf("admin auto: %v", err)
	}
}

// An admin's hide lock hides every skill of a source from agents, even
// one shared with everyone; showing it again restores them.
func TestAdminHideLock(t *testing.T) {
	svc, _, src := sharingFixture(t)
	ctx := context.Background()
	if _, err := svc.SetSkillSharing(ctx, "usr_admin", src.ID, "pr-review", SkillSharing{Mode: VisibleEveryone}); err != nil {
		t.Fatal(err)
	}
	on := true
	got, err := svc.UpdateSource(ctx, "usr_admin", src.ID, SourceUpdate{Hidden: &on})
	if err != nil || !got.Hidden {
		t.Fatalf("hide: %+v %v", got, err)
	}
	adminAgent := Viewer{UserID: "usr_admin", AgentID: "agt_admin", Admin: true}
	for name, v := range map[string]Viewer{"member": memberViewer, "admin's agent": adminAgent} {
		if srcs, _ := svc.Sources(ctx, v); len(srcs) != 0 {
			t.Errorf("%s sees a hidden source: %+v", name, srcs)
		}
		if _, err := svc.Get(ctx, v, "pr-review", false); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s gets a skill of a hidden source: %v", name, err)
		}
	}
	if _, err := svc.Get(ctx, adminViewer, "pr-review", false); err != nil {
		t.Fatalf("admin in the dashboard: %v", err)
	}
	off := false
	if _, err := svc.UpdateSource(ctx, "usr_admin", src.ID, SourceUpdate{Hidden: &off}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Get(ctx, memberViewer, "pr-review", false); err != nil {
		t.Fatalf("after showing it again: %v", err)
	}
}

// A contributor never reads with the admins' default token: their dry run,
// create and syncs read anonymously, so a private repository needs their
// own token or GitHub connection. An admin's source still reads with it.
func TestContributorReadsAnonymously(t *testing.T) {
	gh := newFakeGitHub(t, "acme", "skills")
	gh.commit("main", baseTree())
	svc, _ := newAccessTest(t, gh)
	ctx := context.Background()
	if _, err := svc.SetGitHubToken(ctx, "usr_admin", accessDefaultToken); err != nil {
		t.Fatal(err)
	}
	mark := gh.authMark()
	in := SourceInput{URL: "acme/skills", Path: "skills", Mode: ModeReview, PendingApproval: true}
	if _, err := svc.Preview(ctx, "usr_contrib", in); err != nil {
		t.Fatal(err)
	}
	src, err := svc.CreateSource(ctx, "usr_contrib", in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SyncNow(ctx, src.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.InspectAnonymous(ctx, "acme/skills"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PreviewSource(ctx, src.ID); err != nil {
		t.Fatal(err)
	}
	for _, a := range gh.auths(mark) {
		if a != "" {
			t.Fatalf("a contributor's read sent %q", a)
		}
	}
	mark = gh.authMark()
	if _, err := svc.Inspect(ctx, "acme/skills", ""); err != nil {
		t.Fatal(err)
	}
	if got := gh.auths(mark); len(got) == 0 || got[0] != "Bearer "+accessDefaultToken {
		t.Fatalf("an admin's inspect sent %v", got)
	}

	// A private repository: the default token could read it, a
	// contributor cannot.
	gh.mu.Lock()
	gh.private, gh.token = true, accessDefaultToken
	gh.mu.Unlock()
	info, err := svc.InspectAnonymous(ctx, "acme/skills")
	if err != nil || !info.NeedsToken {
		t.Fatalf("anonymous inspect of a private repository: %+v %v", info, err)
	}
	if _, err := svc.Preview(ctx, "usr_contrib", SourceInput{URL: "acme/skills", Path: "skills", PendingApproval: true}); !errors.Is(err, ErrNeedsToken) {
		t.Fatalf("contributor preview of a private repository: %v", err)
	}
	if res, err := svc.SyncNow(ctx, src.ID); err == nil && res.Error == "" {
		t.Fatalf("a contributor's source synced a private repository: %+v", res)
	}
	if _, err := svc.Inspect(ctx, "acme/skills", ""); err != nil {
		t.Fatalf("admin inspect with the default token: %v", err)
	}
}

func TestContributorsStore(t *testing.T) {
	gh := newFakeGitHub(t, "acme", "skills")
	svc := newTestService(t, gh)
	ctx := context.Background()
	if ok, err := svc.IsContributor(ctx, "usr_c"); err != nil || ok {
		t.Fatalf("before: %v %v", ok, err)
	}
	for i := 0; i < 2; i++ {
		if err := svc.SetContributor(ctx, "usr_admin", "usr_c", true); err != nil {
			t.Fatal(err)
		}
	}
	list, err := svc.Contributors(ctx)
	if err != nil || len(list) != 1 || list[0].UserID != "usr_c" || list[0].AddedBy != "usr_admin" || list[0].AddedAt.IsZero() {
		t.Fatalf("list: %+v %v", list, err)
	}
	if ok, _ := svc.IsContributor(ctx, "usr_c"); !ok {
		t.Fatal("not a contributor after adding")
	}
	for i := 0; i < 2; i++ {
		if err := svc.SetContributor(ctx, "usr_admin", "usr_c", false); err != nil {
			t.Fatal(err)
		}
	}
	if ok, _ := svc.IsContributor(ctx, "usr_c"); ok {
		t.Fatal("still a contributor after removing")
	}
	if err := svc.SetContributor(ctx, "usr_admin", " ", true); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty user: %v", err)
	}
	if ok, err := svc.IsContributor(ctx, ""); ok || err != nil {
		t.Fatalf("empty user is a contributor: %v %v", ok, err)
	}
}

// The approval question goes to the owner in the Inbox; approving it makes
// the source live, rejecting it disables the source, and a decision on the
// Skills page (SettleApproval) withdraws it.
func TestApprovalInboxItem(t *testing.T) {
	gh := newFakeGitHub(t, "acme", "skills")
	gh.commit("main", baseTree())
	svc := newTestService(t, gh, trustAcme)
	ctx := context.Background()
	ib, err := inbox.New(ctx, inbox.Options{DB: svc.db})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ib.Flush)
	admins := map[string]bool{"u_owner": true}
	ri, err := NewReviewInbox(ReviewOptions{DB: svc.db, Inbox: ib, Admin: svc, DashboardURL: "https://toolyard.example",
		IsAdmin:          func(_ context.Context, uid string) bool { return admins[uid] },
		FallbackReviewer: func(context.Context) (string, error) { return "u_owner", nil },
		// The sync asks the first question (no question is open yet); it
		// names the contributor.
		UserLabel: func(_ context.Context, uid string) string {
			if uid == "usr_contrib" {
				return "Casey Contributor"
			}
			return ""
		}})
	if err != nil {
		t.Fatal(err)
	}
	svc.SetReviewHook(ri)
	in := SourceInput{URL: "acme/skills", Path: "skills", Mode: ModeReview, Visibility: VisibleMembers, Members: []string{"usr_team"}, PendingApproval: true}
	prev, err := svc.Preview(ctx, "usr_contrib", in)
	if err != nil {
		t.Fatal(err)
	}
	src, err := svc.CreateSource(ctx, "usr_contrib", in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SyncNow(ctx, src.ID); err != nil {
		t.Fatal(err)
	}
	src, _ = svc.GetSource(ctx, src.ID)
	if err := ri.RequestApproval(ctx, *src, prev, "Casey Contributor"); err != nil {
		t.Fatal(err)
	}
	// A second request adds nothing.
	if err := ri.RequestApproval(ctx, *src, prev, "Casey Contributor"); err != nil {
		t.Fatal(err)
	}
	rows, err := ri.store.pendingForSource(ctx, proposalKindApproval, src.ID)
	if err != nil || len(rows) != 1 || rows[0].UserID != "u_owner" || rows[0].InboxID == "" {
		t.Fatalf("approval rows %+v %v", rows, err)
	}
	var reviews int
	_ = svc.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM skill_proposals WHERE kind = ?`, proposalKindReview).Scan(&reviews)
	if reviews != 0 {
		t.Fatalf("%d review questions for a source pending approval", reviews)
	}
	item, err := ib.Get(ctx, rows[0].InboxID)
	if err != nil {
		t.Fatal(err)
	}
	if item.System == nil || item.System.Kind != ApprovalItemKind || item.AgentID != inbox.SystemAgentID("u_owner") {
		t.Fatalf("item %+v", item.System)
	}
	for _, want := range []string{"Casey Contributor", "acme/skills", "1 chosen member", "Found: 2 skills", "https://toolyard.example/#skills/" + src.ID} {
		if !strings.Contains(item.Message, want) {
			t.Errorf("message lacks %q:\n%s", want, item.Message)
		}
	}
	decide := func(id, uid, choice string) {
		t.Helper()
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
	decide(rows[0].InboxID, "u_owner", reviewApprove)
	got, _ := svc.GetSource(ctx, src.ID)
	if got.Approval != ApprovalApproved || got.ActiveVersionID == "" {
		t.Fatalf("after the Inbox approval: %+v", got)
	}
	row, _ := ri.store.get(ctx, rows[0].ID)
	if row.Status != proposalDone {
		t.Fatalf("row %+v", row)
	}
	team := Viewer{UserID: "usr_team", AgentID: "agt_team"}
	if srcs, _ := svc.Sources(ctx, team); len(srcs) != 1 {
		t.Fatalf("approved source hidden from its member: %+v", srcs)
	}

	// Reject in the Inbox.
	two, err := svc.CreateSource(ctx, "usr_contrib", SourceInput{URL: "acme/skills", Slug: "two", Path: "skills", Mode: ModeAuto, PendingApproval: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := ri.RequestApproval(ctx, *two, nil, ""); err != nil {
		t.Fatal(err)
	}
	rows, _ = ri.store.pendingForSource(ctx, proposalKindApproval, two.ID)
	decide(rows[0].InboxID, "u_owner", reviewReject)
	if got, _ := svc.GetSource(ctx, two.ID); got.Approval != ApprovalRejected || got.Enabled {
		t.Fatalf("after the Inbox rejection: %+v", got)
	}

	// A commit pushed after the question was asked: the sync makes it the
	// waiting version, the old question is withdrawn and the question is
	// asked again for the new commit, whose approval makes it live.
	four, err := svc.CreateSource(ctx, "usr_contrib", SourceInput{URL: "acme/skills", Slug: "four", Path: "skills", Mode: ModeReview, PendingApproval: true})
	if err != nil {
		t.Fatal(err)
	}
	fourPrev, err := svc.PreviewSource(ctx, four.ID)
	if err != nil || fourPrev.SHA == "" || fourPrev.SkillCount != 2 {
		t.Fatalf("preview of a stored source: %+v %v", fourPrev, err)
	}
	if err := ri.RequestApproval(ctx, *four, fourPrev, "Casey"); err != nil {
		t.Fatal(err)
	}
	oldRows, _ := ri.store.pendingForSource(ctx, proposalKindApproval, four.ID)
	pushed := gh.commit("main", map[string]string{"skills/pr-review/checks.md": "pushed after the question\n"})
	if res, err := svc.SyncNow(ctx, four.ID); err != nil || !res.Staged || res.SHA != pushed {
		t.Fatalf("four sync: %+v %v", res, err)
	}
	if it, _ := ib.Get(ctx, oldRows[0].InboxID); it.Status != inbox.StatusCancelled {
		t.Fatalf("the question of the old commit: %s", it.Status)
	}
	rows, _ = ri.store.pendingForSource(ctx, proposalKindApproval, four.ID)
	if len(rows) != 1 || !strings.Contains(rows[0].Input, pushed) || !strings.Contains(rows[0].Input, "Casey") {
		t.Fatalf("question asked again: %+v", rows)
	}
	if item, _ := ib.Get(ctx, rows[0].InboxID); !strings.Contains(item.Message, reviewShort(pushed)) {
		t.Fatalf("the new question does not show the new commit:\n%s", item.Message)
	}
	decide(rows[0].InboxID, "u_owner", reviewApprove)
	fourNow, _ := svc.GetSource(ctx, four.ID)
	if fourNow.Approval != ApprovalApproved || fourNow.ActiveSHA != pushed || fourNow.PendingVersionID != "" {
		t.Fatalf("approval of the asked-again question: %+v", fourNow)
	}

	// Decided on the Skills page: the open question is withdrawn.
	three, err := svc.CreateSource(ctx, "usr_contrib", SourceInput{URL: "acme/skills", Slug: "three", Path: "skills", Mode: ModeAuto, PendingApproval: true})
	if err != nil {
		t.Fatal(err)
	}
	threeRes, err := svc.SyncNow(ctx, three.ID)
	if err != nil || !threeRes.Staged {
		t.Fatalf("three sync: %+v %v", threeRes, err)
	}
	if err := ri.RequestApproval(ctx, *three, nil, "Casey"); err != nil {
		t.Fatal(err)
	}
	rows, _ = ri.store.pendingForSource(ctx, proposalKindApproval, three.ID)
	if _, err := svc.DecideApproval(ctx, "usr_admin", three.ID, true, "", threeRes.SHA); err != nil {
		t.Fatal(err)
	}
	if err := ri.SettleApproval(ctx, three.ID, "approved on the Skills page"); err != nil {
		t.Fatal(err)
	}
	if it, _ := ib.Get(ctx, rows[0].InboxID); it.Status != inbox.StatusCancelled {
		t.Fatalf("item after a page decision: %s", it.Status)
	}
	// The sweep leaves settled rows alone and runs clean.
	if err := ri.Sweep(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	// Only a pending source can be asked about.
	if err := ri.RequestApproval(ctx, *got, nil, ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("approval for an approved source: %v", err)
	}
}

// An admin can hide the built-in source and set who sees its skills (its
// repository fields stay read-only); it needs no approval.
func TestBuiltinSourceSharingAndHide(t *testing.T) {
	svc := newBuiltinService(t, storetest.Open(t), nil, nil)
	startService(t, svc)
	ctx := context.Background()
	if srcs, _ := svc.Sources(ctx, memberViewer); len(srcs) != 1 {
		t.Fatalf("built-in source not served: %+v", srcs)
	}
	on := true
	if got, err := svc.UpdateSource(ctx, "usr_admin", BuiltinSourceID, SourceUpdate{Hidden: &on}); err != nil || !got.Hidden {
		t.Fatalf("hide the built-in source: %+v %v", got, err)
	}
	if srcs, _ := svc.Sources(ctx, memberViewer); len(srcs) != 0 {
		t.Fatalf("hidden built-in source shown: %+v", srcs)
	}
	off := false
	blocked := []string{"usr_member"}
	if _, err := svc.UpdateSource(ctx, "usr_admin", BuiltinSourceID, SourceUpdate{Hidden: &off, Blocked: &blocked}); err != nil {
		t.Fatalf("block list on the built-in source: %v", err)
	}
	if srcs, _ := svc.Sources(ctx, memberViewer); len(srcs) != 0 {
		t.Fatalf("blocked member sees the built-in source: %+v", srcs)
	}
	if srcs, _ := svc.Sources(ctx, Viewer{UserID: "usr_other", AgentID: "agt_other"}); len(srcs) != 1 {
		t.Fatalf("another member: %+v", srcs)
	}
	if _, err := svc.DecideApproval(ctx, "usr_admin", BuiltinSourceID, true, "", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("approving the built-in source: %v", err)
	}
}
