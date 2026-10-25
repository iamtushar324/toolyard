package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/skillhub"
)

// skillsTokenSentinel is the read token every leak check looks for.
const skillsTokenSentinel = "github_pat_SENTINEL_skills_0123456789"

// skillsAdminFake implements skillhub.Admin for the route tests. It
// records every call; err, when set, is what every method answers.
type skillsAdminFake struct {
	mu    sync.Mutex
	err   error
	calls []string
	// createErrEcho makes CreateSource fail with an error that quotes the
	// token, as a careless implementation might.
	createErrEcho bool
	webhookErr    error
	webhook       struct {
		id, event, sig string
		body           []byte
	}
	lastInput  skillhub.SourceInput
	lastUpdate skillhub.SourceUpdate
	lastRuns   [2]string
	lastNote   string
	lastBy     string
	lastLimit  int
	// extra are more sources ListSources returns (a recent create).
	extra []skillhub.Source
}

var _ skillhub.Admin = (*skillsAdminFake)(nil)

func (f *skillsAdminFake) record(call string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
	return f.err
}

func (f *skillsAdminFake) called(call string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c == call {
			return true
		}
	}
	return false
}

func skillsTestSource(id string) *skillhub.Source {
	return &skillhub.Source{ID: id, Slug: "team", Kind: "github", Owner: "acme", Repo: "skills", Branch: "main", Path: "skills",
		Mode: skillhub.ModeReview, Auth: skillhub.AuthToken, HasToken: true, Enabled: true, Visibility: skillhub.VisibleEveryone,
		DefaultRuns: skillhub.RunsAnywhere, CreatedBy: "u_admin", WebhookURLPath: "/v1/skills/sources/" + id + "/webhook", HasWebhookSecret: true}
}

func (f *skillsAdminFake) ParseURL(raw string) (skillhub.RepoRef, error) {
	return skillhub.RepoRef{Owner: "acme", Repo: "skills"}, f.record("ParseURL")
}

func (f *skillsAdminFake) Inspect(_ context.Context, url, token string) (*skillhub.RepoInfo, error) {
	if err := f.record("Inspect"); err != nil {
		return nil, err
	}
	return &skillhub.RepoInfo{RepoRef: skillhub.RepoRef{Owner: "acme", Repo: "skills", Branch: "dev", Path: "skills"},
		Private: token != "", DefaultBranch: "main", Branches: []string{"main", "dev"}, HTMLURL: "https://github.com/acme/skills"}, nil
}

func (f *skillsAdminFake) Preview(_ context.Context, by string, in skillhub.SourceInput) (*skillhub.Preview, error) {
	f.mu.Lock()
	f.lastInput, f.lastBy = in, by
	f.mu.Unlock()
	if err := f.record("Preview"); err != nil {
		return nil, err
	}
	return &skillhub.Preview{Source: *skillsTestSource(""), SHA: "0123456789abcdef0123456789abcdef01234567", SkillCount: 2,
		Warnings: []skillhub.Warning{{Path: "skills/x/SKILL.md", Code: "name_mismatch", Severity: "skip"}}}, nil
}

func (f *skillsAdminFake) CreateSource(_ context.Context, by string, in skillhub.SourceInput) (*skillhub.Source, error) {
	f.mu.Lock()
	f.lastInput, f.lastBy = in, by
	echo := f.createErrEcho
	f.mu.Unlock()
	if err := f.record("CreateSource"); err != nil {
		return nil, err
	}
	if echo {
		return nil, fmt.Errorf("%w: GitHub refused token %s", skillhub.ErrNeedsToken, in.Token)
	}
	return skillsTestSource("src_1"), nil
}

func (f *skillsAdminFake) UpdateSource(_ context.Context, by, id string, in skillhub.SourceUpdate) (*skillhub.Source, error) {
	f.mu.Lock()
	f.lastUpdate, f.lastBy = in, by
	f.mu.Unlock()
	if err := f.record("UpdateSource " + id); err != nil {
		return nil, err
	}
	src := skillsTestSource(id)
	if in.Mode != nil {
		src.Mode = *in.Mode
	}
	if in.Branch != nil {
		src.Branch = *in.Branch
	}
	if in.Path != nil {
		src.Path = *in.Path
	}
	if in.PinnedSHA != nil {
		src.PinnedSHA = *in.PinnedSHA
	}
	return src, nil
}

func (f *skillsAdminFake) DeleteSource(_ context.Context, by, id string) error {
	return f.record("DeleteSource " + id)
}

func (f *skillsAdminFake) GetSource(_ context.Context, id string) (*skillhub.Source, error) {
	if err := f.record("GetSource " + id); err != nil {
		return nil, err
	}
	return skillsTestSource(id), nil
}

func (f *skillsAdminFake) ListSources(context.Context) ([]skillhub.Source, error) {
	if err := f.record("ListSources"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]skillhub.Source{*skillsTestSource("src_1")}, f.extra...), nil
}

// skillsEventually waits up to two seconds for cond (a background first
// sync).
func skillsEventually(t *testing.T, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}

func (f *skillsAdminFake) SyncNow(_ context.Context, id string) (*skillhub.SyncResult, error) {
	if err := f.record("SyncNow " + id); err != nil {
		return nil, err
	}
	return &skillhub.SyncResult{SourceID: id, SHA: "0123456789abcdef0123456789abcdef01234567", Activated: true, Full: true}, nil
}

func (f *skillsAdminFake) Versions(_ context.Context, id string, limit int) ([]skillhub.Version, error) {
	f.mu.Lock()
	f.lastLimit = limit
	f.mu.Unlock()
	if err := f.record("Versions " + id); err != nil {
		return nil, err
	}
	return []skillhub.Version{{ID: "skv_1", SourceID: id, SHA: "abc", Status: skillhub.VersionStaged}}, nil
}

func (f *skillsAdminFake) GetVersion(context.Context, string, string) (*skillhub.Version, error) {
	return nil, skillhub.ErrNotFound
}

func (f *skillsAdminFake) Activate(_ context.Context, by, id, vid string) error {
	f.mu.Lock()
	f.lastBy = by
	f.mu.Unlock()
	return f.record("Activate " + id + " " + vid)
}

func (f *skillsAdminFake) Reject(_ context.Context, by, id, vid, note string) error {
	f.mu.Lock()
	f.lastBy, f.lastNote = by, note
	f.mu.Unlock()
	return f.record("Reject " + id + " " + vid)
}

func (f *skillsAdminFake) Skills(_ context.Context, id string) ([]skillhub.SkillUsage, error) {
	if err := f.record("Skills " + id); err != nil {
		return nil, err
	}
	return []skillhub.SkillUsage{{Path: "pdf", Name: "pdf", Runs: skillhub.RunsAnywhere, Reads30d: 4}}, nil
}

func (f *skillsAdminFake) SetRuns(_ context.Context, by, id, path string, runs skillhub.Runs) error {
	f.mu.Lock()
	f.lastRuns = [2]string{path, string(runs)}
	f.mu.Unlock()
	return f.record("SetRuns " + id)
}

func (f *skillsAdminFake) RotateWebhookSecret(_ context.Context, by, id string) (string, error) {
	if err := f.record("RotateWebhookSecret " + id); err != nil {
		return "", err
	}
	return "whsec-test-value", nil
}

func (f *skillsAdminFake) HandleWebhook(_ context.Context, id, event string, body []byte, sig string) error {
	f.mu.Lock()
	f.webhook.id, f.webhook.event, f.webhook.sig, f.webhook.body = id, event, sig, body
	err := f.webhookErr
	f.mu.Unlock()
	_ = f.record("HandleWebhook " + id)
	return err
}

func (f *skillsAdminFake) SetReviewHook(skillhub.ReviewHook)   {}
func (f *skillsAdminFake) SetTokenSource(skillhub.TokenSource) {}
func (f *skillsAdminFake) TrustedOwner(_ context.Context, owner string) bool {
	return owner == "acme"
}

// skillsSettler records Settle calls.
type skillsSettler struct {
	mu    sync.Mutex
	calls []string
}

func (s *skillsSettler) Settle(_ context.Context, sourceID, versionID, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, sourceID+"|"+versionID+"|"+reason)
	return nil
}

func newSkillsAdminEnv(t *testing.T) (*accessTestEnv, *skillsAdminFake, *skillsSettler, *http.Cookie) {
	t.Helper()
	e := newAccessTestServer(t)
	fake, settler := &skillsAdminFake{}, &skillsSettler{}
	e.srv.skillsAdmin, e.srv.skillReviews = fake, settler
	return e, fake, settler, e.cookieFor(t, e.admin.ID)
}

func skillsJSON(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("body %q: %v", rec.Body.String(), err)
	}
	return out
}

// skillsAdminRoutes is every admin route with a body that passes decoding.
var skillsAdminRoutes = []struct{ method, path, body string }{
	{http.MethodPost, "/v1/skills/sources/inspect", `{"url":"acme/skills"}`},
	{http.MethodPost, "/v1/skills/sources/preview", `{"url":"acme/skills"}`},
	{http.MethodGet, "/v1/skills/admin/sources", ""},
	{http.MethodPost, "/v1/skills/sources", `{"url":"acme/skills"}`},
	{http.MethodGet, "/v1/skills/sources/src_1", ""},
	{http.MethodPatch, "/v1/skills/sources/src_1", `{"enabled":false}`},
	{http.MethodDelete, "/v1/skills/sources/src_1", ""},
	{http.MethodPost, "/v1/skills/sources/src_1/sync", ""},
	{http.MethodGet, "/v1/skills/sources/src_1/versions", ""},
	{http.MethodPost, "/v1/skills/sources/src_1/versions/skv_1/activate", ""},
	{http.MethodPost, "/v1/skills/sources/src_1/versions/skv_1/reject", `{"note":"no"}`},
	{http.MethodGet, "/v1/skills/sources/src_1/skills", ""},
	{http.MethodPut, "/v1/skills/sources/src_1/skills/runs", `{"path":"pdf","runs":"host"}`},
	{http.MethodPost, "/v1/skills/sources/src_1/webhook-secret", ""},
}

func TestSkillsAdminRoutesNeedAnAdmin(t *testing.T) {
	e, fake, _, adminCookie := newSkillsAdminEnv(t)
	m := e.member(t, "user_m", "m@beknown.work")
	memberCookie := e.cookieFor(t, m.ID)
	for _, rt := range skillsAdminRoutes {
		if rec := e.do(t, memberCookie, rt.method, rt.path, rt.body); rec.Code != http.StatusForbidden || skillsJSON(t, rec)["error"] != "admin_only" {
			t.Errorf("member %s %s: %d %s", rt.method, rt.path, rec.Code, rec.Body.String())
		}
		if rec := e.do(t, nil, rt.method, rt.path, rt.body); rec.Code != http.StatusUnauthorized {
			t.Errorf("anonymous %s %s: %d %s", rt.method, rt.path, rec.Code, rec.Body.String())
		}
	}
	// An agent token is not an admin session either.
	tok, _ := e.agentFor(t, e.admin.ID)
	if rec := e.bearer(t, tok, http.MethodGet, "/v1/skills/admin/sources", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("agent token: %d %s", rec.Code, rec.Body.String())
	}
	if len(fake.calls) != 0 {
		t.Fatalf("the service was called for a refused request: %v", fake.calls)
	}
	// With the hub off, an admin gets 404 and a plain reason.
	e.srv.skillsAdmin = nil
	for _, rt := range skillsAdminRoutes {
		rec := e.do(t, adminCookie, rt.method, rt.path, rt.body)
		if rec.Code != http.StatusNotFound || skillsJSON(t, rec)["error"] != "skills hub is off" {
			t.Errorf("hub off %s %s: %d %s", rt.method, rt.path, rec.Code, rec.Body.String())
		}
	}
}

func TestSkillsAdminRoutesCallTheService(t *testing.T) {
	e, fake, settler, cookie := newSkillsAdminEnv(t)

	rec := e.do(t, cookie, http.MethodPost, "/v1/skills/sources/inspect", `{"url":"https://github.com/acme/skills/tree/dev/skills"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("inspect: %d %s", rec.Code, rec.Body.String())
	}
	got := skillsJSON(t, rec)
	if got["default_branch"] != "main" || got["branch"] != "dev" || got["path"] != "skills" || got["trusted_owner"] != true {
		t.Fatalf("inspect body %v", got)
	}
	if rec := e.do(t, cookie, http.MethodPost, "/v1/skills/sources/inspect", `{"url":" "}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("inspect without url: %d", rec.Code)
	}

	rec = e.do(t, cookie, http.MethodPost, "/v1/skills/sources/preview", `{"url":"acme/skills","branch":"main","path":"skills"}`)
	if rec.Code != http.StatusOK || skillsJSON(t, rec)["skill_count"] != float64(2) || fake.lastInput.Branch != "main" || fake.lastBy != e.admin.ID {
		t.Fatalf("preview: %d %s by %s", rec.Code, rec.Body.String(), fake.lastBy)
	}

	rec = e.do(t, cookie, http.MethodPost, "/v1/skills/sources", `{"url":"acme/skills","mode":"auto","visibility":"members","members":["u_a"],"default_runs":"host"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	got = skillsJSON(t, rec)
	if got["source"].(map[string]any)["id"] != "src_1" || got["status"] != "syncing" || got["sync"] != nil {
		t.Fatalf("create body %v", got)
	}
	// The first sync runs in the background, after the answer.
	if !skillsEventually(t, func() bool { return fake.called("SyncNow src_1") }) {
		t.Fatalf("no background first sync: calls %v", fake.calls)
	}
	if fake.lastInput.Mode != skillhub.ModeAuto || fake.lastInput.Visibility != skillhub.VisibleMembers || fake.lastInput.DefaultRuns != skillhub.RunsHost {
		t.Fatalf("create input %+v", fake.lastInput)
	}
	if rec := e.do(t, cookie, http.MethodPost, "/v1/skills/sources", `{"url":"acme/skills","surprise":1}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown field: %d", rec.Code)
	}

	rec = e.do(t, cookie, http.MethodGet, "/v1/skills/admin/sources", "")
	if rec.Code != http.StatusOK || len(skillsJSON(t, rec)["sources"].([]any)) != 1 {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do(t, cookie, http.MethodGet, "/v1/skills/sources/src_1", ""); rec.Code != http.StatusOK || skillsJSON(t, rec)["slug"] != "team" {
		t.Fatalf("get: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do(t, cookie, http.MethodPatch, "/v1/skills/sources/src_1", `{"enabled":false,"mode":"pinned","pinned_sha":"abc"}`); rec.Code != http.StatusOK ||
		fake.lastUpdate.Enabled == nil || *fake.lastUpdate.Enabled || *fake.lastUpdate.Mode != skillhub.ModePinned {
		t.Fatalf("patch: %d %s %+v", rec.Code, rec.Body.String(), fake.lastUpdate)
	}
	// The mode and the pinned commit changed: the open review closes.
	settler.mu.Lock()
	if len(settler.calls) != 1 || settler.calls[0] != "src_1||the source's update mode, pinned commit changed on the Skills page" {
		t.Fatalf("patch settle calls %v", settler.calls)
	}
	settler.calls = nil
	settler.mu.Unlock()
	if rec := e.do(t, cookie, http.MethodPost, "/v1/skills/sources/src_1/sync", ""); rec.Code != http.StatusOK || skillsJSON(t, rec)["full"] != true {
		t.Fatalf("sync: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do(t, cookie, http.MethodGet, "/v1/skills/sources/src_1/versions?limit=500", ""); rec.Code != http.StatusOK || fake.lastLimit != 100 {
		t.Fatalf("versions: %d limit %d", rec.Code, fake.lastLimit)
	}
	if rec := e.do(t, cookie, http.MethodGet, "/v1/skills/sources/src_1/versions", ""); rec.Code != http.StatusOK || fake.lastLimit != 20 {
		t.Fatalf("versions default: %d limit %d", rec.Code, fake.lastLimit)
	}

	rec = e.do(t, cookie, http.MethodPost, "/v1/skills/sources/src_1/versions/skv_1/activate", "")
	if rec.Code != http.StatusOK || !fake.called("Activate src_1 skv_1") || fake.lastBy != e.admin.ID {
		t.Fatalf("activate: %d %s", rec.Code, rec.Body.String())
	}
	rec = e.do(t, cookie, http.MethodPost, "/v1/skills/sources/src_1/versions/skv_2/reject", `{"note":"breaks the release flow"}`)
	if rec.Code != http.StatusOK || !fake.called("Reject src_1 skv_2") || fake.lastNote != "breaks the release flow" {
		t.Fatalf("reject: %d %s note %q", rec.Code, rec.Body.String(), fake.lastNote)
	}
	if rec := e.do(t, cookie, http.MethodPost, "/v1/skills/sources/src_1/versions/skv_3/reject", ""); rec.Code != http.StatusOK || fake.lastNote != "rejected on the Skills page" {
		t.Fatalf("reject without a note: %d note %q", rec.Code, fake.lastNote)
	}
	if len(settler.calls) != 3 || !strings.HasPrefix(settler.calls[0], "src_1|skv_1|made live on the Skills page") || !strings.HasPrefix(settler.calls[1], "src_1|skv_2|rejected") {
		t.Fatalf("settle calls %v", settler.calls)
	}

	if rec := e.do(t, cookie, http.MethodGet, "/v1/skills/sources/src_1/skills", ""); rec.Code != http.StatusOK || len(skillsJSON(t, rec)["skills"].([]any)) != 1 {
		t.Fatalf("skills: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do(t, cookie, http.MethodPut, "/v1/skills/sources/src_1/skills/runs", `{"path":"/pdf/","runs":"host"}`); rec.Code != http.StatusOK || fake.lastRuns != [2]string{"pdf", "host"} {
		t.Fatalf("runs: %d %v", rec.Code, fake.lastRuns)
	}
	if rec := e.do(t, cookie, http.MethodPut, "/v1/skills/sources/src_1/skills/runs", `{"path":"pdf","runs":""}`); rec.Code != http.StatusOK || fake.lastRuns != [2]string{"pdf", ""} {
		t.Fatalf("clear runs: %d %v", rec.Code, fake.lastRuns)
	}
	for _, body := range []string{`{"path":"pdf","runs":"laptop"}`, `{"path":"","runs":"host"}`} {
		if rec := e.do(t, cookie, http.MethodPut, "/v1/skills/sources/src_1/skills/runs", body); rec.Code != http.StatusBadRequest {
			t.Fatalf("runs %s: %d", body, rec.Code)
		}
	}

	rec = e.do(t, cookie, http.MethodPost, "/v1/skills/sources/src_1/webhook-secret", "")
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("rotate: %d %v", rec.Code, rec.Header())
	}
	got = skillsJSON(t, rec)
	if got["secret"] != "whsec-test-value" || got["payload_url"] != "http://example.com/v1/skills/sources/src_1/webhook" || got["content_type"] != "application/json" {
		t.Fatalf("rotate body %v", got)
	}
	e.srv.security.PublicURL = "https://toolyard.example/"
	rec = e.do(t, cookie, http.MethodPost, "/v1/skills/sources/src_1/webhook-secret", "")
	if skillsJSON(t, rec)["payload_url"] != "https://toolyard.example/v1/skills/sources/src_1/webhook" {
		t.Fatalf("payload url with a public URL: %s", rec.Body.String())
	}
	e.srv.security.PublicURL = ""

	if rec := e.do(t, cookie, http.MethodDelete, "/v1/skills/sources/src_1", ""); rec.Code != http.StatusOK || !fake.called("DeleteSource src_1") {
		t.Fatalf("delete: %d", rec.Code)
	}
	if last := settler.calls[len(settler.calls)-1]; last != "src_1||the source was deleted" {
		t.Fatalf("delete settle %q", last)
	}

	// Writes are audited; the rotate row never carries the secret.
	var n int
	if err := e.db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE event_type LIKE 'skills.%'`).Scan(&n); err != nil || n < 9 {
		t.Fatalf("skills audit rows %d %v", n, err)
	}
	var leaked int
	_ = e.db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE COALESCE(reason,'') LIKE '%whsec%' OR COALESCE(result_summary,'') LIKE '%whsec%'`).Scan(&leaked)
	if leaked != 0 {
		t.Fatal("the webhook secret reached the audit log")
	}

	// Wrong methods, unknown routes and malformed IDs.
	for _, rt := range []struct{ method, path string }{
		{http.MethodGet, "/v1/skills/sources/inspect"},
		{http.MethodPost, "/v1/skills/sources/src_1/versions"},
		{http.MethodPost, "/v1/skills/admin/sources"},
		{http.MethodPut, "/v1/skills/sources/src_1"},
	} {
		if rec := e.do(t, cookie, rt.method, rt.path, ""); rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s: %d", rt.method, rt.path, rec.Code)
		}
	}
	for _, p := range []string{"/v1/skills/sources/src_1/nope", "/v1/skills/sources/src%201", "/v1/skills/sources/src_1/versions/v%20x/activate", "/v1/skills/sources/"} {
		if rec := e.do(t, cookie, http.MethodPost, p, ""); rec.Code != http.StatusNotFound {
			t.Errorf("POST %s: %d", p, rec.Code)
		}
	}
}

func TestSkillsAdminErrorMapping(t *testing.T) {
	e, fake, _, cookie := newSkillsAdminEnv(t)
	for _, c := range []struct {
		err    error
		status int
		code   string
	}{
		{skillhub.ErrNotFound, http.StatusNotFound, "not_found"},
		{fmt.Errorf("%w: branch nope does not exist", skillhub.ErrInvalid), http.StatusBadRequest, "invalid"},
		{skillhub.ErrConflict, http.StatusConflict, "conflict"},
		{skillhub.ErrNeedsToken, http.StatusUnprocessableEntity, "needs_token"},
		{skillhub.ErrLimit, http.StatusUnprocessableEntity, "limit"},
		{skillhub.ErrDisabled, http.StatusNotFound, "skills_hub_off"},
		{errors.New("database is locked"), http.StatusInternalServerError, "internal"},
	} {
		fake.err = c.err
		rec := e.do(t, cookie, http.MethodPost, "/v1/skills/sources/src_1/sync", "")
		got := skillsJSON(t, rec)
		if rec.Code != c.status || got["code"] != c.code {
			t.Errorf("%v: %d %v", c.err, rec.Code, got)
		}
		if msg, _ := got["error"].(string); strings.HasPrefix(msg, "skillhub: ") {
			t.Errorf("%v: message keeps the package prefix: %q", c.err, msg)
		}
	}
	fake.err = fmt.Errorf("%w: branch nope does not exist", skillhub.ErrInvalid)
	rec := e.do(t, cookie, http.MethodPost, "/v1/skills/sources/preview", `{"url":"acme/skills","branch":"nope"}`)
	if rec.Code != http.StatusBadRequest || skillsJSON(t, rec)["error"] != "invalid input: branch nope does not exist" {
		t.Fatalf("preview error: %d %s", rec.Code, rec.Body.String())
	}
}

func TestSkillsAdminNeverLeaksTheToken(t *testing.T) {
	e, fake, _, cookie := newSkillsAdminEnv(t)
	var bodies []string
	check := func(rec *httptest.ResponseRecorder) {
		t.Helper()
		bodies = append(bodies, rec.Body.String())
	}
	check(e.do(t, cookie, http.MethodPost, "/v1/skills/sources/inspect", `{"url":"acme/private","token":"`+skillsTokenSentinel+`"}`))
	check(e.do(t, cookie, http.MethodPost, "/v1/skills/sources/preview", `{"url":"acme/private","token":"`+skillsTokenSentinel+`"}`))
	rec := e.do(t, cookie, http.MethodPost, "/v1/skills/sources", `{"url":"acme/private","token":"`+skillsTokenSentinel+`"}`)
	if rec.Code != http.StatusCreated || fake.lastInput.Token != skillsTokenSentinel {
		t.Fatalf("create with a token: %d %s", rec.Code, rec.Body.String())
	}
	check(rec)
	check(e.do(t, cookie, http.MethodPatch, "/v1/skills/sources/src_1", `{"token":"`+skillsTokenSentinel+`"}`))
	check(e.do(t, cookie, http.MethodGet, "/v1/skills/sources/src_1", ""))
	check(e.do(t, cookie, http.MethodGet, "/v1/skills/admin/sources", ""))
	// An implementation error that quotes the token is redacted.
	fake.createErrEcho = true
	rec = e.do(t, cookie, http.MethodPost, "/v1/skills/sources", `{"url":"acme/private","token":"`+skillsTokenSentinel+`"}`)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "[redacted]") {
		t.Fatalf("echoed error: %d %s", rec.Code, rec.Body.String())
	}
	check(rec)
	// A malformed body that carried a token never echoes it either.
	check(e.do(t, cookie, http.MethodPost, "/v1/skills/sources", `{"url":"acme/private","token":"`+skillsTokenSentinel+`","x":1}`))
	for i, b := range bodies {
		if strings.Contains(b, skillsTokenSentinel) {
			t.Errorf("response %d leaks the token: %s", i, b)
		}
	}
	var n int
	_ = e.db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE COALESCE(reason,'') LIKE ? OR COALESCE(result_summary,'') LIKE ? OR COALESCE(arguments,'') LIKE ?`,
		"%SENTINEL%", "%SENTINEL%", "%SENTINEL%").Scan(&n)
	if n != 0 {
		t.Fatalf("the token reached %d audit rows", n)
	}
	var updated int
	_ = e.db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE event_type = 'skills.source_update' AND reason = 'changed: token'`).Scan(&updated)
	if updated != 1 {
		t.Fatalf("token update audit rows %d", updated)
	}
}

func skillsWebhookRequest(path, event, sig, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if event != "" {
		req.Header.Set("X-GitHub-Event", event)
	}
	if sig != "" {
		req.Header.Set("X-Hub-Signature-256", sig)
	}
	return req
}

func TestSkillWebhookIsPublicAndSigned(t *testing.T) {
	e, fake, _, _ := newSkillsAdminEnv(t)
	serve := func(req *http.Request) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		e.handler.ServeHTTP(rec, req)
		return rec
	}
	// No cookie, no CSRF header: GitHub's delivery shape.
	payload := `{"ref":"refs/heads/main","after":"0123456789abcdef0123456789abcdef01234567"}`
	rec := serve(skillsWebhookRequest("/v1/skills/sources/src_1/webhook", "push", "sha256=good", payload))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("accepted: %d %s", rec.Code, rec.Body.String())
	}
	if fake.webhook.id != "src_1" || fake.webhook.event != "push" || fake.webhook.sig != "sha256=good" || string(fake.webhook.body) != payload {
		t.Fatalf("webhook call %+v", fake.webhook)
	}
	// A bad signature is 401.
	fake.webhookErr = fmt.Errorf("%w", skillhub.ErrBadSignature)
	if rec := serve(skillsWebhookRequest("/v1/skills/sources/src_1/webhook", "push", "sha256=bad", payload)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad signature: %d %s", rec.Code, rec.Body.String())
	}
	fake.webhookErr = skillhub.ErrNotFound
	if rec := serve(skillsWebhookRequest("/v1/skills/sources/src_9/webhook", "push", "sha256=x", payload)); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown source: %d", rec.Code)
	}
	fake.webhookErr = nil
	// Missing headers.
	if rec := serve(skillsWebhookRequest("/v1/skills/sources/src_1/webhook", "", "sha256=x", payload)); rec.Code != http.StatusBadRequest {
		t.Fatalf("no event: %d", rec.Code)
	}
	if rec := serve(skillsWebhookRequest("/v1/skills/sources/src_1/webhook", "push", "", payload)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no signature: %d", rec.Code)
	}
	// A large push (past the 16 KiB default cap) goes through; past 5 MiB
	// it is refused.
	big := `{"pad":"` + strings.Repeat("a", 1<<20) + `"}`
	if rec := serve(skillsWebhookRequest("/v1/skills/sources/src_1/webhook", "push", "sha256=good", big)); rec.Code != http.StatusAccepted {
		t.Fatalf("1 MiB push: %d %s", rec.Code, rec.Body.String())
	}
	huge := `{"pad":"` + strings.Repeat("a", int(SkillWebhookMaxBytes)) + `"}`
	if rec := serve(skillsWebhookRequest("/v1/skills/sources/src_1/webhook", "push", "sha256=good", huge)); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("over 5 MiB: %d", rec.Code)
	}
	// Only POST; and off while the hub is off.
	if rec := serve(httptest.NewRequest(http.MethodGet, "/v1/skills/sources/src_1/webhook", nil)); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: %d", rec.Code)
	}
	e.srv.skillsAdmin = nil
	if rec := serve(skillsWebhookRequest("/v1/skills/sources/src_1/webhook", "push", "sha256=good", payload)); rec.Code != http.StatusNotFound {
		t.Fatalf("hub off: %d", rec.Code)
	}
	// With a public URL the Origin check still lets GitHub's delivery
	// through, but not a cookie-less admin route.
	e.srv.skillsAdmin = fake
	e.srv.security.PublicURL = "https://toolyard.example"
	chain := e.srv.EnforceOriginOnMutations(e.handler)
	rec = httptest.NewRecorder()
	chain.ServeHTTP(rec, skillsWebhookRequest("/v1/skills/sources/src_1/webhook", "push", "sha256=good", payload))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("origin check on the webhook: %d %s", rec.Code, rec.Body.String())
	}
}

func TestSkillWebhookSecurityRules(t *testing.T) {
	if !skillWebhookPath("/v1/skills/sources/src_1/webhook") || skillWebhookPath("/v1/skills/sources/src_1/webhook-secret") ||
		skillWebhookPath("/v1/skills/sources/src_1") || skillWebhookPath("/v1/skills/sources//webhook") || skillWebhookPath("/v1/skills/sources/a/b/webhook") {
		t.Fatal("skillWebhookPath")
	}
	if !exemptFromCSRFHeader("/v1/skills/sources/src_1/webhook") || !exemptFromOriginCheck("/v1/skills/sources/src_1/webhook") {
		t.Fatal("the webhook must not need the dashboard's CSRF header or Origin")
	}
	for _, p := range []string{"/v1/skills/sources", "/v1/skills/sources/src_1", "/v1/skills/sources/src_1/webhook-secret", "/v1/skills/sources/src_1/sync"} {
		if exemptFromCSRFHeader(p) || exemptFromOriginCheck(p) {
			t.Errorf("%s must keep the CSRF and Origin checks", p)
		}
	}
	if perRouteBodyCap("/v1/skills/sources/src_1/webhook") != SkillWebhookMaxBytes || perRouteBodyCap("/v1/skills/sources") != 64<<10 ||
		perRouteBodyCap("/v1/skills/sources/src_1/webhook-secret") != 64<<10 {
		t.Fatal("body caps")
	}
	if rl := unauthRouteLimit("/v1/skills/sources/src_1/webhook"); rl == nil || rl.key != "skill-webhook" {
		t.Fatal("the webhook needs a per-IP limit")
	}
	if _, ok := operatorScopeFor(http.MethodPost, "/v1/skills/sources/src_1/webhook-secret"); ok {
		t.Fatal("operator tokens must not rotate a webhook secret")
	}
	if scope, ok := operatorScopeFor(http.MethodPost, "/v1/skills/sources"); !ok || scope != "owner" {
		t.Fatalf("creating a source needs the owner scope: %q %v", scope, ok)
	}
}

// TestSkillsCreateRetryAnswersWithTheSameSource: the same create by the
// same person within two minutes returns the source it made, instead of
// a second source with a "-2" slug.
func TestSkillsCreateRetryAnswersWithTheSameSource(t *testing.T) {
	e, fake, _, cookie := newSkillsAdminEnv(t)
	recent := *skillsTestSource("src_recent")
	recent.CreatedBy, recent.CreatedAt = e.admin.ID, time.Now().Add(-30*time.Second)
	fake.mu.Lock()
	fake.extra = []skillhub.Source{recent}
	fake.mu.Unlock()

	rec := e.do(t, cookie, http.MethodPost, "/v1/skills/sources", `{"url":"acme/skills","branch":"main","path":"/skills/"}`)
	got := skillsJSON(t, rec)
	if rec.Code != http.StatusOK || got["existing"] != true || got["source"].(map[string]any)["id"] != "src_recent" {
		t.Fatalf("retry: %d %v", rec.Code, got)
	}
	if fake.called("CreateSource") {
		t.Fatal("a retry created a second source")
	}
	// Without a branch in the request, any branch of that create matches.
	if rec := e.do(t, cookie, http.MethodPost, "/v1/skills/sources", `{"url":"acme/skills","path":"skills"}`); rec.Code != http.StatusOK {
		t.Fatalf("retry without a branch: %d %s", rec.Code, rec.Body.String())
	}
	// Another branch, another folder, another slug or an older source is
	// a new source.
	for _, body := range []string{
		`{"url":"acme/skills","branch":"dev","path":"skills"}`,
		`{"url":"acme/skills","branch":"main","path":"other"}`,
		`{"url":"acme/skills","branch":"main","path":"skills","slug":"second"}`,
	} {
		if rec := e.do(t, cookie, http.MethodPost, "/v1/skills/sources", body); rec.Code != http.StatusCreated {
			t.Fatalf("%s: %d %s", body, rec.Code, rec.Body.String())
		}
	}
	fake.mu.Lock()
	fake.extra[0].CreatedAt = time.Now().Add(-3 * time.Minute)
	fake.mu.Unlock()
	if rec := e.do(t, cookie, http.MethodPost, "/v1/skills/sources", `{"url":"acme/skills","branch":"main","path":"skills"}`); rec.Code != http.StatusCreated {
		t.Fatalf("after the window: %d", rec.Code)
	}
}

// TestSkillsPatchSettlesReviewsOnlyWhenWhatIsFetchedChanges.
func TestSkillsPatchSettlesReviewsOnlyWhenWhatIsFetchedChanges(t *testing.T) {
	e, _, settler, cookie := newSkillsAdminEnv(t)
	for _, body := range []string{`{"enabled":false}`, `{"visibility":"members","members":["u_a"]}`, `{"mode":"review"}`, `{"branch":"main"}`} {
		if rec := e.do(t, cookie, http.MethodPatch, "/v1/skills/sources/src_1", body); rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", body, rec.Code, rec.Body.String())
		}
	}
	settler.mu.Lock()
	n := len(settler.calls)
	settler.mu.Unlock()
	if n != 0 {
		t.Fatalf("an unchanged mode, branch or folder closed reviews: %v", settler.calls)
	}
	for body, what := range map[string]string{
		`{"branch":"dev"}`:   "branch",
		`{"path":"other"}`:   "folder",
		`{"mode":"auto"}`:    "update mode",
		`{"pinned_sha":"x"}`: "pinned commit",
	} {
		if rec := e.do(t, cookie, http.MethodPatch, "/v1/skills/sources/src_1", body); rec.Code != http.StatusOK {
			t.Fatalf("%s: %d", body, rec.Code)
		}
		settler.mu.Lock()
		last := settler.calls[len(settler.calls)-1]
		settler.mu.Unlock()
		if last != "src_1||the source's "+what+" changed on the Skills page" {
			t.Fatalf("%s: settle %q", body, last)
		}
	}
}
