package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/access"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/gateway"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/memory"
	"github.com/tusharbhardwaj/toolyard/internal/policy"
	"github.com/tusharbhardwaj/toolyard/internal/realtime"
	"github.com/tusharbhardwaj/toolyard/internal/skillhub"
	"github.com/tusharbhardwaj/toolyard/internal/store"
	"github.com/tusharbhardwaj/toolyard/internal/store/storetest"
)

// restSkills is a small skillhub.Reader: one public skill (team/deploy)
// and one admins-only skill (private/secret). Bundle writes the refs it
// was given, or fails when bundleErr is set.
type restSkills struct {
	mu        sync.Mutex
	viewers   []skillhub.Viewer
	bundleErr error
}

const restSHA = "0123456789abcdef0123456789abcdef01234567"

func restSkill(slug, name string) *skillhub.Skill {
	uri := "skill://" + slug + "/" + name + "/SKILL.md"
	return &skillhub.Skill{
		SkillSummary: skillhub.SkillSummary{URI: uri, Name: name, Description: "The " + name + " playbook", Source: slug, Path: name,
			Repo: skillhub.RepoLabel("acme", slug, restSHA), Commit: restSHA, Runs: skillhub.RunsAnywhere},
		Frontmatter: map[string]any{"name": name, "description": "The " + name + " playbook"},
		Body:        "Steps.\n",
		Files:       []skillhub.FileRef{{URI: uri, Path: "SKILL.md", Digest: "sha256:" + strings.Repeat("a", 64), Size: 7}},
	}
}

func (r *restSkills) visible(v skillhub.Viewer) []*skillhub.Skill {
	r.mu.Lock()
	r.viewers = append(r.viewers, v)
	r.mu.Unlock()
	out := []*skillhub.Skill{restSkill("team", "deploy")}
	if v.Admin {
		out = append(out, restSkill("private", "secret"))
	}
	return out
}

func (r *restSkills) lastViewer() skillhub.Viewer {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.viewers[len(r.viewers)-1]
}

func (r *restSkills) find(v skillhub.Viewer, ref string) (*skillhub.Skill, error) {
	for _, s := range r.visible(v) {
		if ref == s.URI || ref == s.Name || ref == s.Source+"/"+s.Path {
			return s, nil
		}
	}
	return nil, skillhub.ErrNotFound
}

func (r *restSkills) Sources(_ context.Context, v skillhub.Viewer) ([]skillhub.SourceInfo, error) {
	var out []skillhub.SourceInfo
	for _, s := range r.visible(v) {
		out = append(out, skillhub.SourceInfo{Slug: s.Source, Repo: s.Repo, Branch: "main", Commit: s.Commit, SkillCount: 1})
	}
	return out, nil
}

func (r *restSkills) Search(_ context.Context, v skillhub.Viewer, q skillhub.SearchQuery) ([]skillhub.SkillSummary, error) {
	var out []skillhub.SkillSummary
	for _, s := range r.visible(v) {
		if strings.Contains(s.Name+" "+s.Description, q.Text) {
			out = append(out, s.SkillSummary)
		}
	}
	return out, nil
}

func (r *restSkills) Get(_ context.Context, v skillhub.Viewer, ref string, includeFiles bool) (*skillhub.Skill, error) {
	s, err := r.find(v, ref)
	if err != nil {
		return nil, err
	}
	if !includeFiles {
		s.Files = nil
	}
	return s, nil
}

func (r *restSkills) Read(_ context.Context, v skillhub.Viewer, uri string, offset, limit int) (*skillhub.File, error) {
	s, err := r.find(v, uri)
	if err != nil {
		return nil, err
	}
	lines := []string{"line 0\n", "line 1\n", "line 2\n"}
	end := len(lines)
	if limit > 0 && offset+limit < end {
		end = offset + limit
	}
	return &skillhub.File{FileRef: s.Files[0], Skill: s.URI, Repo: s.Repo, Commit: s.Commit, Content: strings.Join(lines[offset:end], ""),
		Offset: offset, TotalLines: len(lines), Truncated: end < len(lines)}, nil
}

func (r *restSkills) ListDir(context.Context, skillhub.Viewer, string) ([]skillhub.DirEntry, error) {
	return nil, skillhub.ErrNotFound
}

func (r *restSkills) Grep(_ context.Context, v skillhub.Viewer, q skillhub.GrepQuery) ([]skillhub.GrepHit, error) {
	var out []skillhub.GrepHit
	for _, s := range r.visible(v) {
		if strings.Contains("Steps.", q.Pattern) {
			out = append(out, skillhub.GrepHit{URI: s.URI, Skill: s.URI, Line: 1, Text: "Steps.", Repo: s.Repo, Commit: s.Commit})
		}
	}
	return out, nil
}

func (r *restSkills) List(_ context.Context, v skillhub.Viewer, q skillhub.ListQuery) ([]skillhub.Skill, string, error) {
	all := r.visible(v)
	if q.Limit > 0 && q.Limit < len(all) {
		return []skillhub.Skill{*all[0]}, "1", nil
	}
	out := make([]skillhub.Skill, 0, len(all))
	for _, s := range all {
		out = append(out, *s)
	}
	return out, "", nil
}

func (r *restSkills) Bundle(_ context.Context, v skillhub.Viewer, refs []string, w io.Writer) error {
	r.mu.Lock()
	err := r.bundleErr
	r.viewers = append(r.viewers, v)
	r.mu.Unlock()
	if err != nil {
		return err
	}
	_, err = io.WriteString(w, "tar.gz of "+strings.Join(refs, ","))
	return err
}

func (r *restSkills) Subscribe(func(string)) func() { return func() {} }

// skillsResolver gives one agent the admin scope and every other caller a
// member's (no grants).
type skillsResolver struct{ admin string }

func (s skillsResolver) ScopeFor(_ context.Context, caller string) access.Scope {
	if caller == s.admin {
		return access.Scope{All: true}
	}
	return access.Scope{Groups: map[string]bool{}}
}

type skillsEnv struct {
	h       http.Handler
	db      *store.DB
	skills  *restSkills
	member  string // a member's agent token
	admin   string // the admin agent's token
	opTok   string
	agentID string // the member agent's id
}

const skillsRouteReason = "find the deploy playbook before shipping the service"

// newSkillsEnv is a Server behind the production middleware chain with
// the skills hub on (hubOn) over restSkills, and props as its proposals
// when given.
func newSkillsEnv(t *testing.T, hubOn bool, props ...skillhub.Proposals) *skillsEnv {
	t.Helper()
	ctx := context.Background()
	db := storetest.Open(t)
	id := identity.New(db)
	owner, err := id.CreateUser(ctx, "owner", "long-enough-password")
	if err != nil {
		t.Fatal(err)
	}
	member, ag, err := id.CreateAgentWithToken(ctx, owner.ID, "member-agent")
	if err != nil {
		t.Fatal(err)
	}
	admin, adminAg, err := id.CreateAgentWithToken(ctx, owner.ID, "admin-agent")
	if err != nil {
		t.Fatal(err)
	}
	opTok, _, err := id.CreateOperatorToken(ctx, owner.ID, "cli", []string{"owner"}, 0, "cli")
	if err != nil {
		t.Fatal(err)
	}
	auditLog := audit.New(db)
	gw := gateway.New(gateway.Options{
		Policy: policy.New(db), Audit: auditLog, Memory: memory.New(db), Hub: realtime.NewHub(),
		Access: skillsResolver{admin: adminAg.ID}, PublicURL: "https://toolyard.example",
	})
	gw.RegisterBuiltins()
	t.Cleanup(func() { _ = gw.Close() })
	e := &skillsEnv{db: db, skills: &restSkills{}, member: member, admin: admin, opTok: opTok, agentID: ag.ID}
	if hubOn {
		hub := gateway.SkillHub{Reader: e.skills, Tickets: gateway.NewSkillBundleTickets(db)}
		if len(props) > 0 {
			hub.Proposals = props[0]
		}
		gw.RegisterSkillHub(hub)
	}
	srvCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	srv := New(srvCtx, Options{
		Identity: id, Audit: auditLog, Gateway: gw,
		SessionKey: []byte("0123456789abcdef0123456789abcdef"),
		Security:   SecurityOptions{PublicURL: "https://toolyard.example"},
	})
	mux := http.NewServeMux()
	srv.Routes(mux)
	var h http.Handler = srv.RoleGuard(mux)
	h = srv.HardenAPI(h)
	h = srv.EnforceOriginOnMutations(h)
	e.h = h
	return e
}

func (e *skillsEnv) do(t *testing.T, method, path, token, body string, hdr ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("body %q: %v", rec.Body.String(), err)
	}
	return out
}

func TestSkillsRoutesAuth(t *testing.T) {
	e := newSkillsEnv(t, true)
	for _, p := range []string{"/v1/skills/search?query=deploy", "/v1/skills/get?skill=deploy", "/v1/skills/sources", "/v1/skills/list"} {
		if rec := e.do(t, http.MethodGet, p, "", ""); rec.Code != http.StatusUnauthorized && rec.Code != http.StatusNotFound {
			t.Errorf("GET %s without a token = %d", p, rec.Code)
		}
		if rec := e.do(t, http.MethodGet, p, "not-a-token", ""); rec.Code != http.StatusUnauthorized && rec.Code != http.StatusNotFound {
			t.Errorf("GET %s with a bad token = %d", p, rec.Code)
		}
	}
	if rec := e.do(t, http.MethodGet, "/v1/skills/search?query=deploy", "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("search without a token = %d", rec.Code)
	}
	for _, p := range []string{"/v1/skills/search?query=deploy", "/v1/skills/bundle"} {
		method := http.MethodGet
		if p == "/v1/skills/bundle" {
			method = http.MethodPost
		}
		if rec := e.do(t, method, p, e.opTok, `{"skills":["deploy"]}`); rec.Code != http.StatusForbidden {
			t.Errorf("%s %s with an operator token = %d %s", method, p, rec.Code, rec.Body)
		}
	}
	// On /v1/skills/sources only a GET with an agent bearer is the agent
	// listing; the rest of the path belongs to the admin routes.
	for _, r := range []*http.Request{
		httptest.NewRequest(http.MethodPost, "/v1/skills/sources", strings.NewReader(`{}`)),
		httptest.NewRequest(http.MethodGet, "/v1/skills/sources", nil),
	} {
		if r.Method == http.MethodPost {
			r.Header.Set("Authorization", "Bearer "+e.member)
		}
		rec := httptest.NewRecorder()
		(&Server{}).skillsSources(rec, r)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s /v1/skills/sources = %d", r.Method, rec.Code)
		}
	}
	opReq := httptest.NewRequest(http.MethodGet, "/v1/skills/sources", nil)
	opReq.Header.Set("Authorization", "Bearer "+e.opTok)
	if agentBearer(opReq) {
		t.Error("an operator token counts as an agent bearer")
	}

	off := newSkillsEnv(t, false)
	rec := off.do(t, http.MethodGet, "/v1/skills/search?query=deploy", off.member, "")
	if rec.Code != http.StatusServiceUnavailable || decodeBody(t, rec)["code"] != "unavailable" {
		t.Fatalf("hub off = %d %s", rec.Code, rec.Body)
	}
	if rec := off.do(t, http.MethodGet, "/v1/skills/search?query=deploy", "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("hub off without a token = %d (auth comes first)", rec.Code)
	}
}

func TestSkillsReadRoutes(t *testing.T) {
	e := newSkillsEnv(t, true)
	rec := e.do(t, http.MethodGet, "/v1/skills/search?query=deploy&limit=5&mode=keyword", e.member, "")
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("search = %d %s", rec.Code, rec.Body)
	}
	out := decodeBody(t, rec)
	hits, _ := out["skills"].([]any)
	if len(hits) != 1 || hits[0].(map[string]any)["repo"] != "acme/team@0123456789ab" || hits[0].(map[string]any)["uri"] != "skill://team/deploy/SKILL.md" {
		t.Fatalf("search body = %v", out)
	}
	if v := e.skills.lastViewer(); v.AgentID != e.agentID || v.Admin {
		t.Fatalf("REST viewer = %+v", v)
	}
	got := decodeBody(t, e.do(t, http.MethodGet, "/v1/skills/get?skill=deploy&include_files=false", e.member, ""))
	if got["uri"] != "skill://team/deploy/SKILL.md" || got["commit"] != restSHA || got["source"] != "team" || got["files"] != nil {
		t.Fatalf("get body = %v", got)
	}
	read := decodeBody(t, e.do(t, http.MethodGet, "/v1/skills/read?"+url.Values{"uri": {"skill://team/deploy/SKILL.md"}, "offset": {"1"}, "limit": {"1"}}.Encode(), e.member, ""))
	if read["content"] != "line 1\n" || read["next_offset"] != float64(2) || read["source"] != "team" {
		t.Fatalf("read body = %v", read)
	}
	grep := decodeBody(t, e.do(t, http.MethodGet, "/v1/skills/grep?pattern=Steps&ignore_case=true", e.member, ""))
	if grep["total"] != float64(1) {
		t.Fatalf("grep body = %v", grep)
	}
	src := decodeBody(t, e.do(t, http.MethodGet, "/v1/skills/sources", e.member, ""))
	if src["total"] != float64(1) {
		t.Fatalf("sources body = %v", src)
	}
	if all := decodeBody(t, e.do(t, http.MethodGet, "/v1/skills/sources", e.admin, "")); all["total"] != float64(2) {
		t.Fatalf("admin sources body = %v", all)
	}
	page := decodeBody(t, e.do(t, http.MethodGet, "/v1/skills/list?limit=1", e.admin, ""))
	if page["next_cursor"] != "1" || len(page["skills"].([]any)) != 1 {
		t.Fatalf("list body = %v", page)
	}

	// Refusals: status and code.
	for _, c := range []struct {
		method, path string
		status       int
		code         string
	}{
		{http.MethodGet, "/v1/skills/get?skill=secret", http.StatusNotFound, "not_found"},
		{http.MethodGet, "/v1/skills/search", http.StatusBadRequest, "invalid"},
		{http.MethodGet, "/v1/skills/search?query=deploy&colour=blue", http.StatusBadRequest, "invalid"},
		{http.MethodGet, "/v1/skills/search?query=deploy&limit=ten", http.StatusBadRequest, "invalid"},
		{http.MethodGet, "/v1/skills/search?query=deploy&include_host=maybe", http.StatusBadRequest, "invalid"},
		{http.MethodGet, "/v1/skills/search?query=deploy&query=again", http.StatusBadRequest, "invalid"},
		{http.MethodGet, "/v1/skills/search?query=deploy&reason=short", http.StatusBadRequest, "invalid"},
		{http.MethodGet, "/v1/skills/read?uri=skill://private/secret/SKILL.md", http.StatusNotFound, "not_found"},
	} {
		rec := e.do(t, c.method, c.path, e.member, "")
		if rec.Code != c.status || decodeBody(t, rec)["code"] != c.code {
			t.Errorf("%s %s = %d %s, want %d %s", c.method, c.path, rec.Code, rec.Body, c.status, c.code)
		}
	}
	if rec := e.do(t, http.MethodPost, "/v1/skills/search?query=deploy", e.member, `{}`); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST search = %d", rec.Code)
	}
	if rec := e.do(t, http.MethodGet, "/v1/skills/get?skill=secret", e.admin, ""); rec.Code != http.StatusOK {
		t.Fatalf("admin get secret = %d %s", rec.Code, rec.Body)
	}

	// The calls are audited like tool calls, under via rest, with the
	// reason given (or the route).
	e.do(t, http.MethodGet, "/v1/skills/get?"+url.Values{"skill": {"deploy"}, "reason": {skillsRouteReason}}.Encode(), e.member, "")
	var n int
	if err := e.db.QueryRow(`SELECT count(*) FROM audit_events WHERE tool_name = ? AND event_type = ? AND via = ? AND reason = ?`,
		gateway.SkillsGetTool, audit.EventCallSucceeded, "rest", skillsRouteReason).Scan(&n); err != nil || n != 1 {
		t.Fatalf("audit rows for the REST get with a reason: %d %v", n, err)
	}
	if err := e.db.QueryRow(`SELECT count(*) FROM audit_events WHERE tool_name = ? AND event_type = ? AND reason = ?`,
		gateway.SkillsSourcesTool, audit.EventCallSucceeded, "GET /v1/skills/sources (skills over REST)").Scan(&n); err != nil || n != 2 {
		t.Fatalf("audit rows for REST sources with the default reason: %d %v", n, err)
	}
}

func TestSkillsBundleRoutes(t *testing.T) {
	e := newSkillsEnv(t, true)
	rec := e.do(t, http.MethodPost, "/v1/skills/bundle", e.member, `{"skills":["deploy"],"reason":"`+skillsRouteReason+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST bundle = %d %s", rec.Code, rec.Body)
	}
	out := decodeBody(t, rec)
	link, _ := out["url"].(string)
	path, ok := strings.CutPrefix(link, "https://toolyard.example")
	if !ok || !strings.HasPrefix(path, gateway.SkillBundlePathPrefix) || out["expires_at"] == "" {
		t.Fatalf("bundle body = %v", out)
	}
	if exp, err := time.Parse(time.RFC3339, out["expires_at"].(string)); err != nil || time.Until(exp) > gateway.SkillBundleTTL+time.Minute {
		t.Fatalf("expires_at = %v %v", out["expires_at"], err)
	}

	// The download needs no token, works once, and runs as the member.
	dl := e.do(t, http.MethodGet, path, "", "")
	if dl.Code != http.StatusOK || dl.Header().Get("Content-Type") != "application/gzip" || dl.Body.String() != "tar.gz of skill://team/deploy/SKILL.md" ||
		!strings.Contains(dl.Header().Get("Content-Disposition"), "skills.tar.gz") {
		t.Fatalf("download = %d %v %q", dl.Code, dl.Header(), dl.Body.String())
	}
	if v := e.skills.lastViewer(); v.AgentID != e.agentID {
		t.Fatalf("download viewer = %+v", v)
	}
	if again := e.do(t, http.MethodGet, path, "", ""); again.Code != http.StatusNotFound || decodeBody(t, again)["code"] != "not_found" {
		t.Fatalf("second download = %d %s", again.Code, again.Body)
	}
	for _, p := range []string{gateway.SkillBundlePathPrefix + "tyskb_unknown", gateway.SkillBundlePathPrefix + "a/b"} {
		if rec := e.do(t, http.MethodGet, p, "", ""); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d", p, rec.Code)
		}
	}

	// A bundle that fails before its first byte answers with an error.
	link2, _ := decodeBody(t, e.do(t, http.MethodPost, "/v1/skills/bundle", e.member, `{"skills":["deploy"]}`))["url"].(string)
	e.skills.mu.Lock()
	e.skills.bundleErr = errors.New("archive failed")
	e.skills.mu.Unlock()
	if rec := e.do(t, http.MethodGet, strings.TrimPrefix(link2, "https://toolyard.example"), "", ""); rec.Code != http.StatusInternalServerError {
		t.Fatalf("failed bundle = %d %s", rec.Code, rec.Body)
	}

	// Bad requests.
	for _, c := range []struct {
		body, ctype string
		status      int
	}{
		{`{"skills":["secret"]}`, "application/json", http.StatusNotFound},
		{`{"skills":[]}`, "application/json", http.StatusBadRequest},
		{`{"skills":["deploy"],"extra":1}`, "application/json", http.StatusBadRequest},
		{`not json`, "application/json", http.StatusBadRequest},
		{`{"skills":["deploy"]}`, "text/plain", http.StatusUnsupportedMediaType},
	} {
		rec := e.do(t, http.MethodPost, "/v1/skills/bundle", e.member, c.body, "Content-Type", c.ctype)
		if rec.Code != c.status {
			t.Errorf("POST bundle %s (%s) = %d %s, want %d", c.body, c.ctype, rec.Code, rec.Body, c.status)
		}
	}
	if rec := e.do(t, http.MethodGet, "/v1/skills/bundle", e.member, ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /v1/skills/bundle = %d", rec.Code)
	}
}

// The route helpers the guards share: which paths are the agent's.
func TestSkillsAgentPaths(t *testing.T) {
	for p, want := range map[string]bool{
		"/v1/skills/search": true, "/v1/skills/get": true, "/v1/skills/read": true, "/v1/skills/grep": true, "/v1/skills/list": true,
		"/v1/skills/bundle": true, "/v1/skills/bundle/tyskb_x": true,
		"/v1/skills/sources": false, "/v1/skills/sources/src_1": false, "/v1/skills/bundle/": false, "/v1/skills/bundle/a/b": false, "/v1/skills": false,
	} {
		if got := skillsAgentPath(p); got != want {
			t.Errorf("skillsAgentPath(%q) = %v, want %v", p, got, want)
		}
	}
	for p, want := range map[string]bool{"/v1/skills/search": true, "/v1/skills/sources": true, "/v1/skills/bundle/tyskb_x": true, "/v1/skills/sources/src_1": false} {
		if got := sessionTokenAllowed(p); got != want {
			t.Errorf("sessionTokenAllowed(%q) = %v, want %v", p, got, want)
		}
	}
	if _, ok := operatorScopeFor(http.MethodGet, "/v1/skills/search"); ok {
		t.Error("an operator token may call /v1/skills/search")
	}
	if !exemptFromCSRFHeader("/v1/skills/bundle") || exemptFromCSRFHeader("/v1/skills/sources") {
		t.Error("CSRF exemptions: the bundle POST is bearer-only, the shared sources path is not exempt")
	}
	if unauthRouteLimit("/v1/skills/bundle/tyskb_x") == nil {
		t.Error("bundle downloads have no rate limit")
	}
}
