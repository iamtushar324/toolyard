package api

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/skillhub"
)

// skillsSharingFake is skillsAdminFake with the sharing, approval and
// contributor parts, and sources whose creator the test chooses.
type skillsSharingFake struct {
	*skillsAdminFake
	smu          sync.Mutex
	owners       map[string]string // source ID -> creator
	contributors map[string]bool
	approval     map[string]skillhub.Approval
	modes        map[string]skillhub.UpdateMode
	hidden       map[string]bool
	ghOwners     map[string]string // source ID -> repository owner (default acme, trusted)
	kinds        map[string]string // source ID -> kind (default github)
	lastSharing  skillhub.SkillSharing
	lastPath     string
	decisions    []string
}

var (
	_ skillhub.SharingAdmin      = (*skillsSharingFake)(nil)
	_ skillhub.ContributorsAdmin = (*skillsSharingFake)(nil)
)

// skillsFakeUpdatedAt is every fake source's updated_at.
var skillsFakeUpdatedAt = time.Unix(1_800_000_000, 0).UTC()

func (f *skillsSharingFake) source(id string) *skillhub.Source {
	src := skillsTestSource(id)
	src.UpdatedAt = skillsFakeUpdatedAt
	f.smu.Lock()
	defer f.smu.Unlock()
	if o, ok := f.owners[id]; ok {
		src.CreatedBy = o
	}
	src.Approval = f.approval[id]
	if m, ok := f.modes[id]; ok {
		src.Mode = m
	}
	src.Hidden = f.hidden[id]
	if o, ok := f.ghOwners[id]; ok {
		src.Owner = o
	}
	if k, ok := f.kinds[id]; ok {
		src.Kind = k
	}
	return src
}

// ParseURL reads "owner/repo", so a test can name an untrusted owner (only
// "acme" is trusted).
func (f *skillsSharingFake) ParseURL(raw string) (skillhub.RepoRef, error) {
	owner, repo, _ := strings.Cut(strings.TrimSpace(raw), "/")
	return skillhub.RepoRef{Owner: owner, Repo: repo}, f.record("ParseURL")
}

func (f *skillsSharingFake) GetSource(_ context.Context, id string) (*skillhub.Source, error) {
	if err := f.record("GetSource " + id); err != nil {
		return nil, err
	}
	return f.source(id), nil
}

func (f *skillsSharingFake) ListSources(context.Context) ([]skillhub.Source, error) {
	if err := f.record("ListSources"); err != nil {
		return nil, err
	}
	f.smu.Lock()
	ids := make([]string, 0, len(f.owners))
	for id := range f.owners {
		ids = append(ids, id)
	}
	f.smu.Unlock()
	var out []skillhub.Source
	for _, id := range ids {
		out = append(out, *f.source(id))
	}
	return out, nil
}

func (f *skillsSharingFake) CreateSource(ctx context.Context, by string, in skillhub.SourceInput) (*skillhub.Source, error) {
	if _, err := f.skillsAdminFake.CreateSource(ctx, by, in); err != nil {
		return nil, err
	}
	f.smu.Lock()
	f.owners["src_new"] = by
	if in.PendingApproval {
		f.approval["src_new"] = skillhub.ApprovalPending
	}
	f.smu.Unlock()
	return f.source("src_new"), nil
}

func (f *skillsSharingFake) UpdateSource(ctx context.Context, by, id string, in skillhub.SourceUpdate) (*skillhub.Source, error) {
	if _, err := f.skillsAdminFake.UpdateSource(ctx, by, id, in); err != nil {
		return nil, err
	}
	src := f.source(id)
	if in.Visibility != nil {
		src.Visibility = *in.Visibility
	}
	if in.Members != nil {
		src.Members = *in.Members
	}
	if in.Blocked != nil {
		src.Blocked = *in.Blocked
	}
	if in.Hidden != nil {
		src.Hidden = *in.Hidden
	}
	if in.Branch != nil {
		src.Branch = *in.Branch
	}
	return src, nil
}

func (f *skillsSharingFake) PreviewSource(_ context.Context, id string) (*skillhub.Preview, error) {
	if err := f.record("PreviewSource " + id); err != nil {
		return nil, err
	}
	return &skillhub.Preview{Source: *f.source(id), SHA: strings.Repeat("b", 40), SkillCount: 3}, nil
}

func (f *skillsSharingFake) InspectAnonymous(_ context.Context, url string) (*skillhub.RepoInfo, error) {
	if err := f.record("InspectAnonymous"); err != nil {
		return nil, err
	}
	return &skillhub.RepoInfo{RepoRef: skillhub.RepoRef{Owner: "acme", Repo: "skills"}, DefaultBranch: "main", Branches: []string{"main"}}, nil
}

func (f *skillsSharingFake) SetSkillSharing(_ context.Context, by, id, path string, sh skillhub.SkillSharing) (skillhub.SkillSharing, error) {
	if err := f.record("SetSkillSharing " + id); err != nil {
		return skillhub.SkillSharing{}, err
	}
	f.smu.Lock()
	defer f.smu.Unlock()
	before := f.lastSharing
	f.lastSharing, f.lastPath = sh, path
	return before, nil
}

func (f *skillsSharingFake) DecideApproval(_ context.Context, by, id string, approve bool, note, sha string) (*skillhub.Source, error) {
	if err := f.record("DecideApproval " + id); err != nil {
		return nil, err
	}
	f.smu.Lock()
	f.decisions = append(f.decisions, by+"|"+id+"|"+map[bool]string{true: "approve", false: "reject"}[approve]+"|"+note+"|"+sha)
	if approve {
		f.approval[id] = skillhub.ApprovalApproved
	} else {
		f.approval[id] = skillhub.ApprovalRejected
	}
	f.smu.Unlock()
	return f.source(id), nil
}

func (f *skillsSharingFake) DecideApprovalFrom(ctx context.Context, by, id string, from skillhub.Approval, approve bool, note, sha string) (*skillhub.Source, error) {
	f.smu.Lock()
	cur := f.approval[id]
	f.smu.Unlock()
	if cur != from {
		return nil, skillhub.ErrConflict
	}
	return f.DecideApproval(ctx, by, id, approve, note, sha)
}

func (f *skillsSharingFake) Contributors(context.Context) ([]skillhub.Contributor, error) {
	f.smu.Lock()
	defer f.smu.Unlock()
	out := []skillhub.Contributor{}
	for uid := range f.contributors {
		out = append(out, skillhub.Contributor{UserID: uid, AddedBy: "x", AddedAt: time.Unix(0, 0)})
	}
	return out, nil
}

func (f *skillsSharingFake) IsContributor(_ context.Context, uid string) (bool, error) {
	f.smu.Lock()
	defer f.smu.Unlock()
	return f.contributors[uid], nil
}

func (f *skillsSharingFake) SetContributor(_ context.Context, by, uid string, on bool) error {
	if err := f.record("SetContributor " + uid); err != nil {
		return err
	}
	f.smu.Lock()
	defer f.smu.Unlock()
	if on {
		f.contributors[uid] = true
	} else {
		delete(f.contributors, uid)
	}
	return nil
}

// skillsApprovalSettler is skillsSettler that also asks for and closes
// approvals.
type skillsApprovalSettler struct {
	skillsSettler
	requests []string
	settled  []string
}

func (s *skillsApprovalSettler) RequestApproval(_ context.Context, src skillhub.Source, prev *skillhub.Preview, contributor string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := -1
	if prev != nil {
		n = prev.SkillCount
	}
	s.requests = append(s.requests, src.ID+"|"+string(src.Approval)+"|"+contributor+"|"+strings.Repeat("x", max(n, 0)))
	return nil
}

func (s *skillsApprovalSettler) SettleApproval(_ context.Context, sourceID, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.settled = append(s.settled, sourceID+"|"+reason)
	return nil
}

type skillsSharingEnv struct {
	*accessTestEnv
	fake        *skillsSharingFake
	settler     *skillsApprovalSettler
	adminCookie *http.Cookie
	contrib     *identity.User
	contribCk   *http.Cookie
	memberCk    *http.Cookie
}

// newSkillsSharingEnv: src_admin was added by the admin, src_mine by the
// contributor (a member the admin named; approved, so it has an approval
// state: a contributor manages only such sources).
func newSkillsSharingEnv(t *testing.T) *skillsSharingEnv {
	t.Helper()
	e := newAccessTestServer(t)
	c := e.member(t, "user_c", "casey@beknown.work")
	m := e.member(t, "user_m", "morgan@beknown.work")
	fake := &skillsSharingFake{skillsAdminFake: &skillsAdminFake{},
		owners:       map[string]string{"src_admin": e.admin.ID, "src_mine": c.ID},
		contributors: map[string]bool{c.ID: true}, approval: map[string]skillhub.Approval{"src_mine": skillhub.ApprovalApproved},
		modes: map[string]skillhub.UpdateMode{}, hidden: map[string]bool{}, ghOwners: map[string]string{}, kinds: map[string]string{}}
	settler := &skillsApprovalSettler{}
	e.srv.skillsAdmin, e.srv.skillReviews = fake, settler
	return &skillsSharingEnv{accessTestEnv: e, fake: fake, settler: settler, adminCookie: e.cookieFor(t, e.admin.ID),
		contrib: c, contribCk: e.cookieFor(t, c.ID), memberCk: e.cookieFor(t, m.ID)}
}

func (e *skillsSharingEnv) calls() []string {
	e.fake.mu.Lock()
	defer e.fake.mu.Unlock()
	return append([]string(nil), e.fake.calls...)
}

func (e *skillsSharingEnv) resetCalls() {
	e.fake.mu.Lock()
	e.fake.calls = nil
	e.fake.mu.Unlock()
}

func TestSkillsMeSaysWhoMayManage(t *testing.T) {
	e := newSkillsSharingEnv(t)
	for _, c := range []struct {
		name               string
		cookie             *http.Cookie
		admin, contributor bool
	}{{"admin", e.adminCookie, true, false}, {"contributor", e.contribCk, false, true}, {"member", e.memberCk, false, false}} {
		rec := e.do(t, c.cookie, http.MethodGet, skillMePath, "")
		got := skillsJSON(t, rec)
		if rec.Code != http.StatusOK || got["admin"] != c.admin || got["contributor"] != c.contributor {
			t.Errorf("%s: %d %v", c.name, rec.Code, got)
		}
	}
	if rec := e.do(t, nil, http.MethodGet, skillMePath, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d", rec.Code)
	}
}

// A contributor sees and changes only their own sources; admin-only routes
// stay admin-only; a plain member reaches nothing.
func TestSkillContributorScope(t *testing.T) {
	e := newSkillsSharingEnv(t)

	rec := e.do(t, e.contribCk, http.MethodGet, "/v1/skills/admin/sources", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("contributor list: %d %s", rec.Code, rec.Body.String())
	}
	list, _ := skillsJSON(t, rec)["sources"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["id"] != "src_mine" {
		t.Fatalf("contributor list = %v", list)
	}
	rec = e.do(t, e.adminCookie, http.MethodGet, "/v1/skills/admin/sources", "")
	if all, _ := skillsJSON(t, rec)["sources"].([]any); len(all) != 2 {
		t.Fatalf("admin list = %v", all)
	}

	own := []struct{ method, path, body string }{
		{http.MethodGet, "/v1/skills/sources/src_mine", ""},
		{http.MethodPatch, "/v1/skills/sources/src_mine", `{"enabled":false}`},
		{http.MethodPost, "/v1/skills/sources/src_mine/sync", ""},
		{http.MethodGet, "/v1/skills/sources/src_mine/versions", ""},
		{http.MethodGet, "/v1/skills/sources/src_mine/skills", ""},
		{http.MethodPut, "/v1/skills/sources/src_mine/skills/runs", `{"path":"pdf","runs":"host"}`},
		{http.MethodPut, "/v1/skills/sources/src_mine/skills/sharing", `{"path":"pdf","mode":"nobody"}`},
		{http.MethodPost, "/v1/skills/sources/src_mine/webhook-secret", ""},
		{http.MethodDelete, "/v1/skills/sources/src_mine", ""},
	}
	for _, rt := range own {
		if rec := e.do(t, e.contribCk, rt.method, rt.path, rt.body); rec.Code != http.StatusOK {
			t.Errorf("contributor on own %s %s: %d %s", rt.method, rt.path, rec.Code, rec.Body.String())
		}
	}
	// Someone else's source is not found, and nothing reaches the service
	// beyond the ownership read.
	e.resetCalls()
	for _, rt := range own {
		path := strings.Replace(rt.path, "src_mine", "src_admin", 1)
		if rec := e.do(t, e.contribCk, rt.method, path, rt.body); rec.Code != http.StatusNotFound {
			t.Errorf("contributor on another's %s %s: %d %s", rt.method, path, rec.Code, rec.Body.String())
		}
	}
	for _, c := range e.calls() {
		if c != "GetSource src_admin" {
			t.Fatalf("a refused request reached the service: %v", e.calls())
		}
	}
	// Admin-only, even on their own source.
	for _, rt := range []struct{ method, path, body string }{
		{http.MethodPost, "/v1/skills/sources/src_mine/versions/skv_1/activate", ""},
		{http.MethodPost, "/v1/skills/sources/src_mine/versions/skv_1/reject", `{"note":"no"}`},
		{http.MethodPost, "/v1/skills/sources/src_mine/approval", `{"decision":"approve"}`},
		{http.MethodGet, "/v1/skills/admin/github-token", ""},
		{http.MethodGet, skillContributorsPath, ""},
		{http.MethodPut, skillContributorsPrefix + e.contrib.ID, ""},
	} {
		rec := e.do(t, e.contribCk, rt.method, rt.path, rt.body)
		if rec.Code != http.StatusForbidden || skillsJSON(t, rec)["error"] != "admin_only" {
			t.Errorf("contributor %s %s: %d %s", rt.method, rt.path, rec.Code, rec.Body.String())
		}
	}
	// A plain member reaches none of the Skills page routes.
	e.resetCalls()
	for _, rt := range append(own, struct{ method, path, body string }{http.MethodGet, "/v1/skills/admin/sources", ""},
		struct{ method, path, body string }{http.MethodGet, skillPeoplePath, ""},
		struct{ method, path, body string }{http.MethodPost, "/v1/skills/sources", `{"url":"acme/skills"}`}) {
		rec := e.do(t, e.memberCk, rt.method, rt.path, rt.body)
		if rec.Code != http.StatusForbidden || skillsJSON(t, rec)["error"] != "admin_only" {
			t.Errorf("member %s %s: %d %s", rt.method, rt.path, rec.Code, rec.Body.String())
		}
	}
	if c := e.calls(); len(c) != 0 {
		t.Fatalf("a member's request reached the service: %v", c)
	}
	// Removing the contributor keeps their source, which they can no
	// longer change; an admin still can.
	if rec := e.do(t, e.adminCookie, http.MethodDelete, skillContributorsPrefix+e.contrib.ID, ""); rec.Code != http.StatusOK {
		t.Fatalf("remove contributor: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do(t, e.contribCk, http.MethodPatch, "/v1/skills/sources/src_mine", `{"enabled":true}`); rec.Code != http.StatusForbidden {
		t.Fatalf("former contributor edits: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do(t, e.adminCookie, http.MethodPatch, "/v1/skills/sources/src_mine", `{"enabled":true}`); rec.Code != http.StatusOK {
		t.Fatalf("admin edits a former contributor's source: %d %s", rec.Code, rec.Body.String())
	}
}

// A contributor's new source is checked with a dry run, created pending
// approval, and the owner is asked; their choice of mode and limits is
// bounded.
func TestSkillContributorCreate(t *testing.T) {
	e := newSkillsSharingEnv(t)
	rec := e.do(t, e.contribCk, http.MethodPost, "/v1/skills/sources", `{"url":"acme/skills","visibility":"nobody","blocked":["u_x"]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	if got := skillsJSON(t, rec)["source"].(map[string]any); got["approval"] != "pending" {
		t.Fatalf("created source %v", got)
	}
	e.fake.mu.Lock()
	in, by := e.fake.lastInput, e.fake.lastBy
	e.fake.mu.Unlock()
	if !in.PendingApproval || by != e.contrib.ID || in.Visibility != skillhub.VisibleNobody || len(in.Blocked) != 1 {
		t.Fatalf("create input %+v by %s", in, by)
	}
	calls := strings.Join(e.calls(), ",")
	if !strings.Contains(calls, "Preview") || strings.Index(calls, "Preview") > strings.Index(calls, "CreateSource") {
		t.Fatalf("calls %s: the dry run must come first", calls)
	}
	e.settler.mu.Lock()
	reqs := append([]string(nil), e.settler.requests...)
	e.settler.mu.Unlock()
	if len(reqs) != 1 || !strings.HasPrefix(reqs[0], "src_new|pending|") || !strings.HasSuffix(reqs[0], "|xx") {
		t.Fatalf("approval requests %v", reqs)
	}
	if n := e.auditRows(t, "skills.source_create", "skill contributor: waits for an admin's approval"); n != 1 {
		t.Fatalf("create audit rows %d", n)
	}
	// An admin's source needs no approval and no dry run.
	e.resetCalls()
	if rec := e.do(t, e.adminCookie, http.MethodPost, "/v1/skills/sources", `{"url":"other/skills","branch":"dev"}`); rec.Code != http.StatusCreated {
		t.Fatalf("admin create: %d %s", rec.Code, rec.Body.String())
	}
	e.fake.mu.Lock()
	adminIn := e.fake.lastInput
	e.fake.mu.Unlock()
	if adminIn.PendingApproval || strings.Contains(strings.Join(e.calls(), ","), "Preview") {
		t.Fatalf("admin create input %+v calls %v", adminIn, e.calls())
	}

	for _, c := range []struct {
		body string
		ok   bool
	}{
		{`{"url":"acme/skills","mode":"auto","branch":"b1"}`, true}, // a trusted owner
		{`{"url":"acme/skills","mode":"review","branch":"b2"}`, true},
		{`{"url":"other/skills","mode":"auto"}`, false},
		{`{"url":"acme/skills","mode":"pinned","pinned_sha":"0123456789abcdef0123456789abcdef01234567"}`, false},
		{`{"url":"acme/skills","limits":{"max_skills":5}}`, false},
	} {
		rec := e.do(t, e.contribCk, http.MethodPost, "/v1/skills/sources", c.body)
		if c.ok && rec.Code != http.StatusCreated {
			t.Errorf("%s: %d %s", c.body, rec.Code, rec.Body.String())
		}
		if !c.ok && (rec.Code != http.StatusForbidden || skillsJSON(t, rec)["code"] != "contributor_limit") {
			t.Errorf("%s: %d %s", c.body, rec.Code, rec.Body.String())
		}
	}
	// Edits are bounded the same way; an unchanged mode passes.
	for _, c := range []struct {
		body string
		code int
	}{
		{`{"mode":"pinned","pinned_sha":"0123456789abcdef0123456789abcdef01234567"}`, http.StatusForbidden},
		{`{"limits":{"max_files":10}}`, http.StatusForbidden},
		{`{"mode":"review","visibility":"members","members":["u_a"]}`, http.StatusOK},
		{`{"mode":"auto"}`, http.StatusOK}, // acme is trusted
	} {
		if rec := e.do(t, e.contribCk, http.MethodPatch, "/v1/skills/sources/src_mine", c.body); rec.Code != c.code {
			t.Errorf("PATCH %s: %d %s", c.body, rec.Code, rec.Body.String())
		}
	}
}

// Sharing changes are audited with the setting before and after.
func TestSkillSharingRoutesAudit(t *testing.T) {
	e := newSkillsSharingEnv(t)
	rec := e.do(t, e.adminCookie, http.MethodPut, "/v1/skills/sources/src_admin/skills/sharing",
		`{"path":"/pdf/","mode":"members","members":["u_b","u_a"],"blocked":["u_c"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("set sharing: %d %s", rec.Code, rec.Body.String())
	}
	e.fake.smu.Lock()
	sh, path := e.fake.lastSharing, e.fake.lastPath
	e.fake.smu.Unlock()
	if path != "pdf" || sh.Mode != skillhub.VisibleMembers || len(sh.Members) != 2 || len(sh.Blocked) != 1 {
		t.Fatalf("set sharing got %q %+v", path, sh)
	}
	var args string
	if err := e.db.QueryRow(`SELECT COALESCE(arguments,'') FROM audit_events WHERE event_type = ?`, "skills.skill_sharing").Scan(&args); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(args, `"before"`) || !strings.Contains(args, `"mode":"inherit"`) || !strings.Contains(args, `"mode":"members"`) || !strings.Contains(args, `"u_c"`) {
		t.Fatalf("sharing audit arguments %s", args)
	}
	// Clearing with mode "inherit" answers the cleared setting.
	rec = e.do(t, e.adminCookie, http.MethodPut, "/v1/skills/sources/src_admin/skills/sharing", `{"path":"pdf","mode":"inherit"}`)
	if got := skillsJSON(t, rec)["sharing"].(map[string]any); rec.Code != http.StatusOK || got["mode"] != "" {
		t.Fatalf("clear: %d %v", rec.Code, got)
	}
	// The source's own sharing (PATCH) is audited too.
	rec = e.do(t, e.adminCookie, http.MethodPatch, "/v1/skills/sources/src_admin", `{"visibility":"nobody","blocked":["u_z"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body.String())
	}
	if err := e.db.QueryRow(`SELECT COALESCE(arguments,'') FROM audit_events WHERE event_type = ?`, "skills.source_sharing").Scan(&args); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(args, `"visibility":"everyone"`) || !strings.Contains(args, `"visibility":"nobody"`) || !strings.Contains(args, `"u_z"`) {
		t.Fatalf("source sharing audit %s", args)
	}
	e.fake.mu.Lock()
	upd := e.fake.lastUpdate
	e.fake.mu.Unlock()
	if upd.Blocked == nil || len(*upd.Blocked) != 1 {
		t.Fatalf("PATCH blocked not passed: %+v", upd)
	}
	// A change of something else writes no sharing row.
	before := e.auditRows(t, "skills.source_sharing", "")
	_ = e.do(t, e.adminCookie, http.MethodPatch, "/v1/skills/sources/src_admin", `{"poll_seconds":600}`)
	if e.auditRows(t, "skills.source_sharing", "") != before {
		t.Fatal("a PATCH without sharing changes wrote a sharing audit row")
	}
}

func TestSkillApprovalRoute(t *testing.T) {
	e := newSkillsSharingEnv(t)
	e.fake.smu.Lock()
	e.fake.approval["src_mine"] = skillhub.ApprovalPending
	e.fake.smu.Unlock()
	if rec := e.do(t, e.adminCookie, http.MethodPost, "/v1/skills/sources/src_mine/approval", `{"decision":"maybe"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad decision: %d", rec.Code)
	}
	rec := e.do(t, e.adminCookie, http.MethodPost, "/v1/skills/sources/src_mine/approval", `{"decision":"approve","sha":" abc "}`)
	if rec.Code != http.StatusOK || skillsJSON(t, rec)["source"].(map[string]any)["approval"] != "approved" {
		t.Fatalf("approve: %d %s", rec.Code, rec.Body.String())
	}
	rec = e.do(t, e.adminCookie, http.MethodPost, "/v1/skills/sources/src_mine/approval", `{"decision":"reject","note":"duplicate"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("reject: %d %s", rec.Code, rec.Body.String())
	}
	e.fake.smu.Lock()
	decisions := append([]string(nil), e.fake.decisions...)
	e.fake.smu.Unlock()
	if len(decisions) != 2 || decisions[0] != e.admin.ID+"|src_mine|approve||abc" || decisions[1] != e.admin.ID+"|src_mine|reject|duplicate|" {
		t.Fatalf("decisions %v", decisions)
	}
	e.settler.mu.Lock()
	settled := append([]string(nil), e.settler.settled...)
	e.settler.mu.Unlock()
	if len(settled) != 2 || !strings.HasPrefix(settled[0], "src_mine|approved on the Skills page") {
		t.Fatalf("settled %v", settled)
	}
	if e.auditRows(t, "skills.source_approve", "") != 1 || e.auditRows(t, "skills.source_reject", "") != 1 {
		t.Fatal("approval decisions not audited")
	}
}

func TestSkillContributorsRoutes(t *testing.T) {
	e := newSkillsSharingEnv(t)
	m := e.member(t, "user_n", "nico@beknown.work")
	if rec := e.do(t, e.adminCookie, http.MethodPut, skillContributorsPrefix+m.ID, ""); rec.Code != http.StatusOK {
		t.Fatalf("add: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do(t, e.adminCookie, http.MethodPut, skillContributorsPrefix+"u_nobody", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("add an unknown person: %d", rec.Code)
	}
	if err := e.id.SetStatus(context.Background(), m.ID, identity.StatusBlocked, "gone"); err != nil {
		t.Fatal(err)
	}
	b := e.member(t, "user_b", "bo@beknown.work")
	if err := e.id.SetStatus(context.Background(), b.ID, identity.StatusBlocked, "gone"); err != nil {
		t.Fatal(err)
	}
	if rec := e.do(t, e.adminCookie, http.MethodPut, skillContributorsPrefix+b.ID, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("add a blocked person: %d", rec.Code)
	}
	rec := e.do(t, e.adminCookie, http.MethodGet, skillContributorsPath, "")
	if list, _ := skillsJSON(t, rec)["contributors"].([]any); rec.Code != http.StatusOK || len(list) != 2 {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do(t, e.adminCookie, http.MethodDelete, skillContributorsPrefix+m.ID, ""); rec.Code != http.StatusOK {
		t.Fatalf("remove: %d", rec.Code)
	}
	if e.auditRows(t, "skills.contributor_add", "") != 1 || e.auditRows(t, "skills.contributor_remove", "") != 1 {
		t.Fatal("contributor changes not audited")
	}
	// An agent token is no session; an operator token needs the owner
	// scope to change contributors.
	tok, _ := e.agentFor(t, e.admin.ID)
	if rec := e.bearer(t, tok, http.MethodGet, skillContributorsPath, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("agent token: %d", rec.Code)
	}
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		if scope, ok := operatorScopeFor(method, skillContributorsPrefix+"u_x"); !ok || scope != identity.ScopeOwner {
			t.Fatalf("operator %s contributors: %q %v", method, scope, ok)
		}
	}
}

func TestSkillPeopleRoute(t *testing.T) {
	e := newSkillsSharingEnv(t)
	rec := e.do(t, e.contribCk, http.MethodGet, skillPeoplePath, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("contributor people: %d %s", rec.Code, rec.Body.String())
	}
	people, _ := skillsJSON(t, rec)["people"].([]any)
	if len(people) < 3 {
		t.Fatalf("people %v", people)
	}
	for _, p := range people {
		if _, has := p.(map[string]any)["email"]; has {
			t.Fatalf("a contributor sees emails: %v", p)
		}
	}
	rec = e.do(t, e.adminCookie, http.MethodGet, skillPeoplePath, "")
	found := false
	for _, p := range skillsJSON(t, rec)["people"].([]any) {
		if p.(map[string]any)["email"] == "casey@beknown.work" {
			found = true
		}
	}
	if !found {
		t.Fatalf("admin people lack emails: %s", rec.Body.String())
	}
}

// The review fixes: what a contributor may not change on their own source,
// the pending cap, anonymous inspect, a new approval question when a
// pending source's fetch settings change, and the admin's hide lock.
func TestSkillContributorGuards(t *testing.T) {
	e := newSkillsSharingEnv(t)
	for _, c := range []struct {
		name, body string
		setup      func()
	}{
		{"hide", `{"hidden":true}`, nil},
		{"pinned commit", `{"pinned_sha":"0123456789abcdef0123456789abcdef01234567"}`, nil},
		{"branch of a pinned source", `{"branch":"other"}`, func() { e.fake.modes["src_mine"] = skillhub.ModePinned }},
		{"folder of an untrusted automatic source", `{"path":"x"}`, func() {
			e.fake.modes["src_mine"] = skillhub.ModeAuto
			e.fake.ghOwners["src_mine"] = "other"
		}},
		{"turn on a rejected source", `{"enabled":true}`, func() { e.fake.approval["src_mine"] = skillhub.ApprovalRejected }},
	} {
		e.fake.smu.Lock()
		e.fake.modes, e.fake.ghOwners = map[string]skillhub.UpdateMode{}, map[string]string{}
		e.fake.approval = map[string]skillhub.Approval{"src_mine": skillhub.ApprovalApproved}
		if c.setup != nil {
			c.setup()
		}
		e.fake.smu.Unlock()
		rec := e.do(t, e.contribCk, http.MethodPatch, "/v1/skills/sources/src_mine", c.body)
		if rec.Code != http.StatusForbidden || skillsJSON(t, rec)["code"] != "contributor_limit" {
			t.Errorf("%s: %d %s", c.name, rec.Code, rec.Body.String())
		}
	}
	// skillsTestSource's owner is acme (trusted): a branch change of an
	// automatic source is the contributor's to make; so is a branch change
	// that switches a pinned source to review.
	e.fake.smu.Lock()
	e.fake.modes = map[string]skillhub.UpdateMode{"src_mine": skillhub.ModePinned}
	e.fake.approval, e.fake.ghOwners = map[string]skillhub.Approval{"src_mine": skillhub.ApprovalApproved}, map[string]string{}
	e.fake.smu.Unlock()
	if rec := e.do(t, e.contribCk, http.MethodPatch, "/v1/skills/sources/src_mine", `{"branch":"other","mode":"review"}`); rec.Code != http.StatusOK {
		t.Errorf("pinned to review with a branch: %d %s", rec.Code, rec.Body.String())
	}

	// An admin hides and shows a source; the audit row names the lock.
	if rec := e.do(t, e.adminCookie, http.MethodPatch, "/v1/skills/sources/src_mine", `{"hidden":true}`); rec.Code != http.StatusOK ||
		skillsJSON(t, rec)["hidden"] != true {
		t.Fatalf("admin hide: %d %s", rec.Code, rec.Body.String())
	}
	var args string
	if err := e.db.QueryRow(`SELECT COALESCE(arguments,'') FROM audit_events WHERE event_type = ?`, "skills.source_sharing").Scan(&args); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(args, `"hidden":false`) || !strings.Contains(args, `"hidden":true`) {
		t.Fatalf("hide audit %s", args)
	}

	// A contributor's inspect without a token never uses the default one.
	e.resetCalls()
	if rec := e.do(t, e.contribCk, http.MethodPost, "/v1/skills/sources/inspect", `{"url":"acme/skills"}`); rec.Code != http.StatusOK {
		t.Fatalf("contributor inspect: %d %s", rec.Code, rec.Body.String())
	}
	if c := strings.Join(e.calls(), ","); !strings.Contains(c, "InspectAnonymous") || strings.Contains(c, "Inspect,") || strings.HasSuffix(c, ",Inspect") {
		t.Fatalf("contributor inspect calls %s", c)
	}
	e.resetCalls()
	if rec := e.do(t, e.adminCookie, http.MethodPost, "/v1/skills/sources/inspect", `{"url":"acme/skills"}`); rec.Code != http.StatusOK {
		t.Fatalf("admin inspect: %d", rec.Code)
	}
	if c := strings.Join(e.calls(), ","); strings.Contains(c, "InspectAnonymous") {
		t.Fatalf("admin inspect calls %s", c)
	}
	// A contributor's dry run is marked as theirs (anonymous reads, the
	// contributor's bounds); an admin's is not.
	if rec := e.do(t, e.contribCk, http.MethodPost, "/v1/skills/sources/preview", `{"url":"acme/skills","path":"skills"}`); rec.Code != http.StatusOK {
		t.Fatalf("contributor preview: %d %s", rec.Code, rec.Body.String())
	}
	e.fake.mu.Lock()
	contribIn := e.fake.lastInput
	e.fake.mu.Unlock()
	if !contribIn.PendingApproval {
		t.Fatalf("a contributor's preview reads as an admin: %+v", contribIn)
	}
	if rec := e.do(t, e.contribCk, http.MethodPost, "/v1/skills/sources/preview", `{"url":"acme/skills","mode":"pinned","pinned_sha":"0123456789abcdef0123456789abcdef01234567"}`); rec.Code != http.StatusForbidden ||
		skillsJSON(t, rec)["code"] != "contributor_limit" {
		t.Fatalf("contributor preview of a pinned source: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do(t, e.adminCookie, http.MethodPost, "/v1/skills/sources/preview", `{"url":"acme/skills","path":"skills"}`); rec.Code != http.StatusOK {
		t.Fatalf("admin preview: %d %s", rec.Code, rec.Body.String())
	}
	e.fake.mu.Lock()
	adminIn := e.fake.lastInput
	e.fake.mu.Unlock()
	if adminIn.PendingApproval {
		t.Fatalf("an admin's preview is marked as a contributor's: %+v", adminIn)
	}

	// A pending source whose branch changes gets a new approval question.
	e.fake.smu.Lock()
	e.fake.modes = map[string]skillhub.UpdateMode{}
	e.fake.approval = map[string]skillhub.Approval{"src_mine": skillhub.ApprovalPending}
	e.fake.smu.Unlock()
	if rec := e.do(t, e.contribCk, http.MethodPatch, "/v1/skills/sources/src_mine", `{"branch":"next"}`); rec.Code != http.StatusOK {
		t.Fatalf("pending branch change: %d %s", rec.Code, rec.Body.String())
	}
	e.settler.mu.Lock()
	settled, reqs := append([]string(nil), e.settler.settled...), append([]string(nil), e.settler.requests...)
	e.settler.mu.Unlock()
	if len(settled) != 1 || !strings.Contains(settled[0], "branch changed") || len(reqs) != 1 || !strings.HasPrefix(reqs[0], "src_mine|pending|casey@beknown.work|xxx") {
		t.Fatalf("re-ask: settled %v requests %v", settled, reqs)
	}
	// A sharing change of a pending source asks nothing new.
	_ = e.do(t, e.contribCk, http.MethodPatch, "/v1/skills/sources/src_mine", `{"visibility":"nobody"}`)
	e.settler.mu.Lock()
	n := len(e.settler.requests)
	e.settler.mu.Unlock()
	if n != 1 {
		t.Fatalf("a sharing change asked again: %d requests", n)
	}

	// At most three sources wait for approval per contributor.
	e.fake.smu.Lock()
	for _, id := range []string{"src_p1", "src_p2", "src_p3"} {
		e.fake.owners[id], e.fake.approval[id] = e.contrib.ID, skillhub.ApprovalPending
	}
	e.fake.smu.Unlock()
	rec := e.do(t, e.contribCk, http.MethodPost, "/v1/skills/sources", `{"url":"acme/skills","branch":"cap"}`)
	if rec.Code != http.StatusConflict || !strings.Contains(skillsJSON(t, rec)["error"].(string), "waiting for an admin") {
		t.Fatalf("pending cap: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do(t, e.adminCookie, http.MethodPost, "/v1/skills/sources", `{"url":"acme/skills","branch":"admin-cap"}`); rec.Code != http.StatusCreated {
		t.Fatalf("admins have no cap: %d %s", rec.Code, rec.Body.String())
	}
}

// A contributor never reaches the built-in source, even if it named them as
// its creator; an admin can hide it.
func TestSkillContributorNeverOwnsBuiltin(t *testing.T) {
	e := newSkillsSharingEnv(t)
	id := skillhub.BuiltinSourceID
	e.fake.smu.Lock()
	e.fake.owners[id], e.fake.kinds[id] = e.contrib.ID, skillhub.KindBuiltin
	e.fake.smu.Unlock()
	for _, rt := range []struct{ method, path, body string }{
		{http.MethodGet, "/v1/skills/sources/" + id, ""},
		{http.MethodPatch, "/v1/skills/sources/" + id, `{"visibility":"nobody"}`},
		{http.MethodPut, "/v1/skills/sources/" + id + "/skills/sharing", `{"path":"x","mode":"nobody"}`},
	} {
		if rec := e.do(t, e.contribCk, rt.method, rt.path, rt.body); rec.Code != http.StatusNotFound {
			t.Errorf("contributor %s %s: %d %s", rt.method, rt.path, rec.Code, rec.Body.String())
		}
	}
	rec := e.do(t, e.contribCk, http.MethodGet, "/v1/skills/admin/sources", "")
	for _, src := range skillsJSON(t, rec)["sources"].([]any) {
		if src.(map[string]any)["id"] == id {
			t.Fatal("a contributor lists the built-in source")
		}
	}
	if rec := e.do(t, e.adminCookie, http.MethodPatch, "/v1/skills/sources/"+id, `{"hidden":true}`); rec.Code != http.StatusOK || skillsJSON(t, rec)["hidden"] != true {
		t.Fatalf("admin hides the built-in source: %d %s", rec.Code, rec.Body.String())
	}
}

// A contributor who added a source while they were an admin (no approval
// state) does not manage it once they lost the role: it reads with the
// admins' default token and its commits go live without an approval, so
// it stays admin-managed. Every source route answers 404, and the list
// leaves it out; their contributor sources stay theirs.
func TestSkillContributorDoesNotOwnSourcesAddedAsAdmin(t *testing.T) {
	e := newSkillsSharingEnv(t)
	e.fake.smu.Lock()
	e.fake.owners["src_old"] = e.contrib.ID // no approval state: added while an admin
	e.fake.smu.Unlock()
	rec := e.do(t, e.contribCk, http.MethodGet, "/v1/skills/admin/sources", "")
	list, _ := skillsJSON(t, rec)["sources"].([]any)
	if rec.Code != http.StatusOK || len(list) != 1 || list[0].(map[string]any)["id"] != "src_mine" {
		t.Fatalf("contributor list: %d %v", rec.Code, list)
	}
	e.resetCalls()
	for _, rt := range []struct{ method, path, body string }{
		{http.MethodGet, "/v1/skills/sources/src_old", ""},
		{http.MethodPatch, "/v1/skills/sources/src_old", `{"branch":"other"}`},
		{http.MethodPost, "/v1/skills/sources/src_old/sync", ""},
		{http.MethodGet, "/v1/skills/sources/src_old/versions", ""},
		{http.MethodGet, "/v1/skills/sources/src_old/skills", ""},
		{http.MethodPut, "/v1/skills/sources/src_old/skills/runs", `{"path":"pdf","runs":"host"}`},
		{http.MethodPut, "/v1/skills/sources/src_old/skills/sharing", `{"path":"pdf","mode":"everyone"}`},
		{http.MethodPost, "/v1/skills/sources/src_old/webhook-secret", ""},
		{http.MethodDelete, "/v1/skills/sources/src_old", ""},
	} {
		if rec := e.do(t, e.contribCk, rt.method, rt.path, rt.body); rec.Code != http.StatusNotFound {
			t.Errorf("contributor %s %s on a source added as an admin: %d %s", rt.method, rt.path, rec.Code, rec.Body.String())
		}
	}
	for _, c := range e.calls() {
		if c != "GetSource src_old" {
			t.Fatalf("a refused request reached the service: %v", e.calls())
		}
	}
	if rec := e.do(t, e.adminCookie, http.MethodPatch, "/v1/skills/sources/src_old", `{"branch":"other"}`); rec.Code != http.StatusOK {
		t.Fatalf("admin edits it: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do(t, e.contribCk, http.MethodGet, "/v1/skills/sources/src_mine", ""); rec.Code != http.StatusOK {
		t.Fatalf("contributor reads their own source: %d %s", rec.Code, rec.Body.String())
	}
}

// A PATCH applies only to the source its checks read: the handler passes
// that read's updated_at, so the engine refuses (409, ErrConflict) a
// source that changed since, for example one an admin rejected meanwhile
// (TestUpdateSourceChecksTheCallersRead in skillhub).
func TestSkillSourcePatchIsBoundToItsRead(t *testing.T) {
	e := newSkillsSharingEnv(t)
	if rec := e.do(t, e.contribCk, http.MethodPatch, "/v1/skills/sources/src_mine", `{"visibility":"nobody"}`); rec.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body.String())
	}
	e.fake.mu.Lock()
	upd := e.fake.lastUpdate
	e.fake.mu.Unlock()
	if !upd.IfUpdatedAt.Equal(skillsFakeUpdatedAt) {
		t.Fatalf("the update is not bound to the source it was checked against: %v", upd.IfUpdatedAt)
	}
}
