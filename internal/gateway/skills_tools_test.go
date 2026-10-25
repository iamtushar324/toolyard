package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tusharbhardwaj/toolyard/internal/access"
	"github.com/tusharbhardwaj/toolyard/internal/actor"
	"github.com/tusharbhardwaj/toolyard/internal/approval"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/memory"
	"github.com/tusharbhardwaj/toolyard/internal/policy"
	"github.com/tusharbhardwaj/toolyard/internal/realtime"
	"github.com/tusharbhardwaj/toolyard/internal/skillhub"
	"github.com/tusharbhardwaj/toolyard/internal/store"
	"github.com/tusharbhardwaj/toolyard/internal/store/storetest"
)

// skillsFixture is a gateway with access control and the skills hub over
// the fake reader. Callers: ag_member (no grants, owner u_member),
// ag_admin (an admin), ag_host (a member's T3 host agent), ag_oauth (a
// member's OAuth client), ag_limited (denied skills by its own limits)
// and ag_blocked.
type skillsFixture struct {
	gw      *Gateway
	db      *store.DB
	aud     *audit.Logger
	pol     *policy.Engine
	res     *fakeResolver
	reader  *fakeSkillReader
	props   *fakeProposals
	hosts   *fakeHosts
	tickets *SkillBundleTickets

	member, admin, host, oauth, limited, blocked context.Context
}

func newSkillsFixture(t *testing.T) *skillsFixture {
	t.Helper()
	db := storetest.Open(t)
	t.Cleanup(func() { _ = db.Close() })
	bus, err := approval.New(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	limited := access.Scope{Groups: map[string]bool{}, Limits: &access.Limits{Listed: true, DenyServers: map[string]bool{"skills": true}}}
	f := &skillsFixture{
		db: db, aud: audit.New(db), pol: policy.New(db),
		reader: &fakeSkillReader{}, props: &fakeProposals{}, hosts: &fakeHosts{on: map[string]bool{"ag_host": true, "ag_oauth": true}},
		res: &fakeResolver{scopes: map[string]access.Scope{
			"ag_member": groups(), "ag_admin": {All: true}, "ag_host": groups(), "ag_oauth": groups(),
			"ag_lookup_fails": groups(), "ag_limited": limited, "ag_blocked": {Denied: true},
			"dashboard:u_member": groups(),
		}},
	}
	f.gw = New(Options{
		Policy: f.pol, Approval: bus, Audit: f.aud, Hub: realtime.NewHub(), Memory: memory.New(db), Access: f.res,
		Owners:    skillOwners{"ag_member": "u_member", "ag_admin": "u_admin", "ag_host": "u_member", "ag_oauth": "u_member", "ag_limited": "u_member"},
		PublicURL: "https://toolyard.example",
	})
	f.gw.RegisterBuiltins()
	t.Cleanup(func() { _ = f.gw.Close() })
	f.tickets = NewSkillBundleTickets(db)
	f.gw.RegisterSkillHub(SkillHub{Reader: f.reader, Proposals: f.props, Tickets: f.tickets, OnHost: f.hosts.OnHost})
	bg := context.Background()
	f.member = WithAgentID(bg, "ag_member")
	f.admin = WithAgentID(bg, "ag_admin")
	f.host = WithAgentID(bg, "ag_host")
	f.oauth = actor.WithRaiser(WithAgentID(bg, "ag_oauth"), actor.Raiser{CallerID: "ag_oauth", AgentKind: "oauth_client"})
	f.limited = WithAgentID(bg, "ag_limited")
	f.blocked = WithAgentID(bg, "ag_blocked")
	return f
}

func (f *skillsFixture) call(t *testing.T, ctx context.Context, tool string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	full := map[string]any{ReasonField: testReason}
	for k, v := range args {
		full[k] = v
	}
	res, err := f.gw.RouteCall(ctx, "test", tool, full)
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	return res
}

// ok calls tool and returns its structured answer, failing on an error.
func (f *skillsFixture) ok(t *testing.T, ctx context.Context, tool string, args map[string]any) map[string]any {
	t.Helper()
	res := f.call(t, ctx, tool, args)
	if res.IsError {
		t.Fatalf("%s %v: %s", tool, args, textOf(res))
	}
	if !strings.HasPrefix(strings.TrimSpace(textOf(res)), "{") {
		t.Fatalf("%s: text is not JSON: %s", tool, textOf(res))
	}
	return structured(t, res)
}

// refused calls tool and returns its error code, failing on success.
func (f *skillsFixture) refused(t *testing.T, ctx context.Context, tool string, args map[string]any) string {
	t.Helper()
	res := f.call(t, ctx, tool, args)
	if !res.IsError {
		t.Fatalf("%s %v should be refused: %s", tool, args, textOf(res))
	}
	code, _ := structured(t, res)["error"].(string)
	return code
}

func TestSkillsToolsAreAlwaysOnAndAllowed(t *testing.T) {
	f := newSkillsFixture(t)
	listed := mcpToolsList(t, f.gw, f.member)
	for _, n := range SkillToolNames {
		if !has(listed, n) {
			t.Errorf("a member without grants does not see %s", n)
		}
		e := f.gw.tools[n]
		if e.upstream != skillsUpstream || e.forcedAction == nil || *e.forcedAction != policy.ActionAllow || !e.denyOverridesForced {
			t.Errorf("%s entry: upstream %q forced %v denyOverrides %v", n, e.upstream, e.forcedAction, e.denyOverridesForced)
		}
		if !strings.HasPrefix(e.tool.Description, descriptionBanner) || e.tool.InputSchema.Required[0] != ReasonField {
			t.Errorf("%s schema: %v", n, e.tool.InputSchema.Required)
		}
	}
	for _, n := range []string{SkillsSearchTool, SkillsGetTool, SkillsReadTool} {
		if !IsPinned(n) {
			t.Errorf("%s is not pinned", n)
		}
	}
	if blocked := mcpToolsList(t, f.gw, f.blocked); len(blocked) != 0 {
		t.Fatalf("a blocked caller sees %v", blocked)
	}
	// The agent's own deny list beats the always-on group.
	if limited := mcpToolsList(t, f.gw, f.limited); has(limited, SkillsSearchTool) {
		t.Fatalf("a limited agent still lists skills: %v", limited)
	}
	if res := f.call(t, f.limited, SkillsSearchTool, map[string]any{"query": "deploy"}); !res.IsError {
		t.Fatalf("a limited agent searched skills: %s", textOf(res))
	}

	// An explicit deny policy switches them off; other skills tools stay.
	if _, err := f.pol.Set(context.Background(), "tool", SkillsGrepTool, "deny", "no grep", false); err != nil {
		t.Fatal(err)
	}
	mustBeDenied(t, f.call(t, f.member, SkillsGrepTool, map[string]any{"pattern": "x"}), "skills.grep deny")
	f.ok(t, f.member, SkillsSearchTool, map[string]any{"query": "deploy"})

	// Hub off: no tools, nothing registered.
	off := newAccessFixture(t, nil)
	off.gw.RegisterSkillHub(SkillHub{})
	if off.gw.SkillHubEnabled() || off.gw.HasTool(SkillsSearchTool) {
		t.Fatal("a nil reader turned the hub on")
	}
}

func TestSkillsViewerAndOnHost(t *testing.T) {
	f := newSkillsFixture(t)
	f.ok(t, f.member, SkillsSearchTool, map[string]any{"query": "deploy"})
	if v := f.reader.lastViewer(t); v != (skillhub.Viewer{UserID: "u_member", AgentID: "ag_member"}) {
		t.Fatalf("member viewer = %+v", v)
	}
	f.ok(t, f.admin, SkillsSearchTool, map[string]any{"query": "deploy"})
	if v := f.reader.lastViewer(t); !v.Admin || v.UserID != "u_admin" || v.OnHost {
		t.Fatalf("admin viewer = %+v", v)
	}
	// A host-connection agent is on the host, and sees host skills
	// without asking; the answer is cached.
	for i := 0; i < 3; i++ {
		out := f.ok(t, f.host, SkillsSearchTool, map[string]any{"query": "host"})
		if out["total"] != float64(1) {
			t.Fatalf("host agent search = %v", out)
		}
	}
	if v := f.reader.lastViewer(t); !v.OnHost || v.UserID != "u_member" || v.Admin {
		t.Fatalf("host viewer = %+v", v)
	}
	if n := f.hosts.count("ag_host"); n != 1 {
		t.Fatalf("OnHost looked up %d times, want 1 (cached)", n)
	}
	// A member's ordinary agent is not, unless it asks.
	if out := f.ok(t, f.member, SkillsSearchTool, map[string]any{"query": "host"}); out["total"] != float64(0) {
		t.Fatalf("member saw a host skill without asking: %v", out)
	}
	if out := f.ok(t, f.member, SkillsSearchTool, map[string]any{"query": "host", "include_host": true}); out["total"] != float64(1) {
		t.Fatalf("member asking for host skills: %v", out)
	}
	// An OAuth client is never on the host, even where the lookup would
	// say so; it is not even looked up.
	f.ok(t, f.oauth, SkillsSearchTool, map[string]any{"query": "deploy"})
	if v := f.reader.lastViewer(t); v.OnHost || v.AgentID != "ag_oauth" {
		t.Fatalf("oauth viewer = %+v", v)
	}
	if n := f.hosts.count("ag_oauth"); n != 0 {
		t.Fatalf("an OAuth client was looked up %d times", n)
	}
	// A failed lookup hides host skills and is not cached.
	fails := WithAgentID(context.Background(), "ag_lookup_fails")
	f.ok(t, fails, SkillsSourcesTool, nil)
	f.ok(t, fails, SkillsSourcesTool, nil)
	if v := f.reader.lastViewer(t); v.OnHost || f.hosts.count("ag_lookup_fails") != 2 {
		t.Fatalf("failed lookup viewer = %+v after %d lookups", v, f.hosts.count("ag_lookup_fails"))
	}
	// A dashboard user is that user, never an agent on the host.
	f.ok(t, WithAgentID(context.Background(), "dashboard:u_member"), SkillsSourcesTool, nil)
	if v := f.reader.lastViewer(t); v != (skillhub.Viewer{UserID: "u_member"}) {
		t.Fatalf("dashboard viewer = %+v", v)
	}
	if got := f.gw.SkillViewer(f.admin); !got.Admin || got.AgentID != "ag_admin" {
		t.Fatalf("SkillViewer = %+v", got)
	}
}

func TestSkillsSearchTool(t *testing.T) {
	f := newSkillsFixture(t)
	out := f.ok(t, f.member, SkillsSearchTool, map[string]any{"query": "deploy"})
	skills, _ := out["skills"].([]any)
	if out["total"] != float64(1) || len(skills) != 1 || out["mode"] != "keyword" {
		t.Fatalf("member search = %v", out)
	}
	hit := skills[0].(map[string]any)
	for k, want := range map[string]any{
		"uri": "skill://team/ops/deploy/SKILL.md", "source": "team", "repo": "acme/playbooks@0123456789ab",
		"commit": fakeTeamSHA, "name": "deploy", "runs": "anywhere",
	} {
		if hit[k] != want {
			t.Errorf("hit[%s] = %v, want %v", k, hit[k], want)
		}
	}
	// The admin also sees the private source.
	if out := f.ok(t, f.admin, SkillsSearchTool, map[string]any{"query": "deploy"}); out["total"] != float64(2) {
		t.Fatalf("admin search = %v", out)
	}
	f.ok(t, f.member, SkillsSearchTool, map[string]any{"query": "^rev", "mode": "regex", "source": "team", "limit": 500})
	f.reader.mu.Lock()
	last := f.reader.searches[len(f.reader.searches)-1]
	f.reader.mu.Unlock()
	if last != (skillhub.SearchQuery{Text: "^rev", Mode: "regex", Source: "team", Limit: skillsSearchMax}) {
		t.Fatalf("search query = %+v (limit capped at %d)", last, skillsSearchMax)
	}
	f.ok(t, f.member, SkillsSearchTool, map[string]any{"query": "x"})
	f.reader.mu.Lock()
	last = f.reader.searches[len(f.reader.searches)-1]
	f.reader.mu.Unlock()
	if last.Limit != skillsSearchDefault || last.Mode != "keyword" {
		t.Fatalf("default search query = %+v", last)
	}
	for _, bad := range []map[string]any{{}, {"query": "  "}, {"query": "x", "mode": "semantic"}, {"query": "x", "limit": 0}} {
		if code := f.refused(t, f.member, SkillsSearchTool, bad); code != SkillsErrInvalid {
			t.Errorf("search %v: code %q, want invalid", bad, code)
		}
	}
	if code := f.refused(t, f.member, SkillsSearchTool, map[string]any{"query": "(", "mode": "regex"}); code != SkillsErrInvalid {
		t.Errorf("bad regex: code %q", code)
	}
	f.reader.setFail("Search", errors.New("index rebuilding"))
	res := f.call(t, f.member, SkillsSearchTool, map[string]any{"query": "deploy"})
	if code, _ := structured(t, res)["error"].(string); !res.IsError || code != SkillsErrInternal || !strings.Contains(textOf(res), "index rebuilding") {
		t.Fatalf("a failing catalog: %v %s", code, textOf(res))
	}
}

func TestSkillsGetReadAndGrep(t *testing.T) {
	f := newSkillsFixture(t)
	got := f.ok(t, f.member, SkillsGetTool, map[string]any{"skill": "deploy"})
	if got["uri"] != "skill://team/ops/deploy/SKILL.md" || got["root"] != "skill://team/ops/deploy" || got["source"] != "team" ||
		got["repo"] != "acme/playbooks@0123456789ab" || got["commit"] != fakeTeamSHA || !strings.Contains(got["body"].(string), "# Deploy") {
		t.Fatalf("skills.get = %v", got)
	}
	files, _ := got["files"].([]any)
	if len(files) != 4 {
		t.Fatalf("files = %v", files)
	}
	first := files[0].(map[string]any)
	if !strings.HasPrefix(first["digest"].(string), "sha256:") || first["uri"] == "" || first["size"] == nil {
		t.Fatalf("file ref = %v", first)
	}
	if fm, _ := got["frontmatter"].(map[string]any); fm["name"] != "deploy" {
		t.Fatalf("frontmatter = %v", got["frontmatter"])
	}
	if _, has := got["raw"]; has {
		t.Fatal("skills.get leaks the raw field")
	}
	if got := f.ok(t, f.member, SkillsGetTool, map[string]any{"skill": "team/ops/deploy", "include_files": false}); got["files"] != nil {
		t.Fatalf("include_files false still lists files: %v", got["files"])
	}
	// Visibility: the private source is not found for a member, a name in
	// two visible sources is ambiguous for the admin.
	if code := f.refused(t, f.member, SkillsGetTool, map[string]any{"skill": "skill://private/secret-sauce/SKILL.md"}); code != SkillsErrNotFound {
		t.Fatalf("member reading a private skill: code %q", code)
	}
	if code := f.refused(t, f.member, SkillsGetTool, map[string]any{"skill": "secret-sauce"}); code != SkillsErrNotFound {
		t.Fatalf("member naming a private skill: code %q", code)
	}
	if got := f.ok(t, f.member, SkillsGetTool, map[string]any{"skill": "review"}); got["source"] != "team" {
		t.Fatalf("member review = %v", got)
	}
	if code := f.refused(t, f.admin, SkillsGetTool, map[string]any{"skill": "review"}); code != SkillsErrConflict {
		t.Fatalf("admin review: code %q, want conflict", code)
	}
	if code := f.refused(t, f.member, SkillsGetTool, map[string]any{}); code != SkillsErrInvalid {
		t.Fatalf("no skill: code %q", code)
	}

	// skills.read: whole files and line ranges.
	read := f.ok(t, f.member, SkillsReadTool, map[string]any{"uri": "skill://team/ops/deploy/references/checklist.md"})
	if read["content"] != "1. build\n2. ship\n3. verify\n" || read["source"] != "team" || read["repo"] != "acme/playbooks@0123456789ab" ||
		read["skill"] != "skill://team/ops/deploy/SKILL.md" || read["total_lines"] != float64(3) || read["next_offset"] != nil {
		t.Fatalf("skills.read = %v", read)
	}
	part := f.ok(t, f.member, SkillsReadTool, map[string]any{"uri": "skill://team/ops/deploy/references/checklist.md", "offset": 1, "limit": 1})
	if part["content"] != "2. ship\n" || part["truncated"] != true || part["next_offset"] != float64(2) {
		t.Fatalf("skills.read part = %v", part)
	}
	for _, bad := range []map[string]any{{}, {"uri": "skill://team/ops/deploy/SKILL.md", "offset": -1}, {"uri": "skill://team/ops/deploy/SKILL.md", "limit": -2}} {
		if code := f.refused(t, f.member, SkillsReadTool, bad); code != SkillsErrInvalid {
			t.Errorf("read %v: code %q", bad, code)
		}
	}
	if code := f.refused(t, f.member, SkillsReadTool, map[string]any{"uri": "skill://private/secret-sauce/SKILL.md"}); code != SkillsErrNotFound {
		t.Fatalf("member reading a private file: code %q", code)
	}

	// skills.grep: hits name their source, repo and commit.
	grep := f.ok(t, f.member, SkillsGrepTool, map[string]any{"pattern": "VERIFY", "ignore_case": true, "path": "references/*"})
	hits, _ := grep["hits"].([]any)
	if grep["total"] != float64(1) || len(hits) != 1 {
		t.Fatalf("grep = %v", grep)
	}
	h := hits[0].(map[string]any)
	if h["uri"] != "skill://team/ops/deploy/references/checklist.md" || h["line"] != float64(3) || h["source"] != "team" ||
		h["repo"] != "acme/playbooks@0123456789ab" || h["commit"] != fakeTeamSHA {
		t.Fatalf("grep hit = %v", h)
	}
	f.ok(t, f.member, SkillsGrepTool, map[string]any{"pattern": "x", "limit": 9999, "source": "team", "include_host": true})
	f.reader.mu.Lock()
	lastGrep := f.reader.greps[len(f.reader.greps)-1]
	f.reader.mu.Unlock()
	if lastGrep.Limit != skillsGrepMax || lastGrep.Source != "team" || !lastGrep.IncludeHost {
		t.Fatalf("grep query = %+v", lastGrep)
	}
	if code := f.refused(t, f.member, SkillsGrepTool, map[string]any{"pattern": ""}); code != SkillsErrInvalid {
		t.Fatalf("empty pattern: code %q", code)
	}
}

func TestSkillsSourcesAndList(t *testing.T) {
	f := newSkillsFixture(t)
	src := f.ok(t, f.member, SkillsSourcesTool, nil)
	list, _ := src["sources"].([]any)
	if src["total"] != float64(1) || len(list) != 1 {
		t.Fatalf("member sources = %v", src)
	}
	s := list[0].(map[string]any)
	if s["source"] != "team" || s["slug"] != "team" || s["uri"] != "skill://team" || s["repo"] != "acme/playbooks@0123456789ab" || s["commit"] != fakeTeamSHA {
		t.Fatalf("source = %v", s)
	}
	if out := f.ok(t, f.admin, SkillsSourcesTool, nil); out["total"] != float64(2) {
		t.Fatalf("admin sources = %v", out)
	}

	page := f.ok(t, f.admin, SkillsListTool, map[string]any{"limit": 2})
	entries, _ := page["skills"].([]any)
	if len(entries) != 2 || page["next_cursor"] != "2" {
		t.Fatalf("first page = %v", page)
	}
	e := entries[0].(map[string]any)
	res, _ := e["resources"].([]any)
	if e["uri"] != "skill://team/ops/deploy/SKILL.md" || e["source"] != "team" || e["repo"] == "" || e["commit"] != fakeTeamSHA || len(res) != 4 {
		t.Fatalf("entry = %v", e)
	}
	if fm, _ := e["frontmatter"].(map[string]any); fm["description"] != "Deploy a service safely" {
		t.Fatalf("frontmatter = %v", e["frontmatter"])
	}
	r0 := res[0].(map[string]any)
	if r0["uri"] == "" || !strings.HasPrefix(r0["digest"].(string), "sha256:") || r0["size"] == nil {
		t.Fatalf("resource = %v", r0)
	}
	rest := f.ok(t, f.admin, SkillsListTool, map[string]any{"cursor": "2", "limit": 500})
	if more, _ := rest["skills"].([]any); len(more) != 2 || rest["next_cursor"] != nil {
		t.Fatalf("second page = %v (host skill left out for an admin off the host)", rest)
	}
	if code := f.refused(t, f.admin, SkillsListTool, map[string]any{"cursor": "zzz"}); code != SkillsErrInvalid {
		t.Fatalf("bad cursor: code %q", code)
	}
	f.ok(t, f.member, SkillsListTool, map[string]any{"limit": 10_000, "source": "team", "include_host": true})
	f.reader.mu.Lock()
	lastList := f.reader.lists[len(f.reader.lists)-1]
	f.reader.mu.Unlock()
	if lastList.Limit != skillsListMax || lastList.Source != "team" || !lastList.IncludeHost {
		t.Fatalf("list query = %+v", lastList)
	}
}

func TestSkillsProposeTools(t *testing.T) {
	f := newSkillsFixture(t)
	out := f.ok(t, f.member, SkillsProposeSourceTool, map[string]any{"url": "acme/more-skills", "branch": "main", "path": "skills"})
	p, _ := out["proposal"].(map[string]any)
	if p["id"] != "skp_source" || p["status"] != "pending" || out["url"] != "acme/more-skills" {
		t.Fatalf("propose_source = %v", out)
	}
	f.props.mu.Lock()
	in, reason, v := f.props.sources[0], f.props.reasons[0], f.props.viewers[0]
	f.props.mu.Unlock()
	if in.URL != "acme/more-skills" || in.Branch != "main" || in.Path != "skills" || reason != testReason || v.AgentID != "ag_member" || v.UserID != "u_member" {
		t.Fatalf("ProposeSource(%+v, %q) by %+v", in, reason, v)
	}
	f.ok(t, f.member, SkillsProposeSourceTool, map[string]any{"url": "acme/x", "reason": "the team keeps its release playbooks there"})
	f.props.mu.Lock()
	reason = f.props.reasons[1]
	f.props.mu.Unlock()
	if reason != "the team keeps its release playbooks there" {
		t.Fatalf("explicit reason = %q", reason)
	}
	if code := f.refused(t, f.member, SkillsProposeSourceTool, map[string]any{"url": " "}); code != SkillsErrInvalid {
		t.Fatalf("no url: code %q", code)
	}
	if code := f.refused(t, WithAgentID(context.Background(), "dashboard:u_member"), SkillsProposeSourceTool, map[string]any{"url": "acme/x"}); code != SkillsErrInvalid {
		t.Fatalf("a dashboard proposal: code %q", code)
	}

	change := f.ok(t, f.member, SkillsProposeChangeTool, map[string]any{
		"skill": "deploy", "title": "Add a smoke test step", "summary": "Verify needs a smoke test.",
		"files": map[string]any{"references/checklist.md": "1. build\n2. ship\n3. smoke test\n4. verify\n", "scripts/rollback.sh": ""},
	})
	sk, _ := change["skill"].(map[string]any)
	if sk["uri"] != "skill://team/ops/deploy/SKILL.md" || sk["repo"] != "acme/playbooks@0123456789ab" || sk["commit"] != fakeTeamSHA || sk["source"] != "team" {
		t.Fatalf("propose_change skill = %v", change)
	}
	f.props.mu.Lock()
	ch := f.props.changes[0]
	f.props.mu.Unlock()
	if ch.Skill != "skill://team/ops/deploy/SKILL.md" || ch.Title != "Add a smoke test step" || len(ch.Files) != 2 || ch.Files["scripts/rollback.sh"] != "" {
		t.Fatalf("ProposeChange(%+v)", ch)
	}
	for _, bad := range []map[string]any{
		{"skill": "deploy", "title": "t", "files": map[string]any{}},
		{"skill": "deploy", "title": "t", "files": map[string]any{"a.md": 3}},
		{"skill": "deploy", "files": map[string]any{"a.md": "x"}},
	} {
		if code := f.refused(t, f.member, SkillsProposeChangeTool, bad); code != SkillsErrInvalid {
			t.Errorf("propose_change %v: code %q", bad, code)
		}
	}
	if code := f.refused(t, f.member, SkillsProposeChangeTool, map[string]any{"skill": "secret-sauce", "title": "t", "files": map[string]any{"a.md": "x"}}); code != SkillsErrNotFound {
		t.Fatalf("a change to an invisible skill: code %q", code)
	}
	f.props.mu.Lock()
	n := len(f.props.changes)
	f.props.mu.Unlock()
	if n != 1 {
		t.Fatalf("%d change proposals made, want 1", n)
	}

	// Without a Proposals service the tools say so.
	noProps := newAccessFixture(t, nil)
	noProps.gw.RegisterSkillHub(SkillHub{Reader: &fakeSkillReader{}})
	res := noProps.call(t, noProps.member, SkillsProposeSourceTool, map[string]any{"url": "acme/x"})
	if code, _ := structured(t, res)["error"].(string); !res.IsError || code != SkillsErrUnavailable {
		t.Fatalf("propose without a service: %s", textOf(res))
	}
}

func TestSkillsBundleTool(t *testing.T) {
	f := newSkillsFixture(t)
	out := f.ok(t, f.member, SkillsBundleTool, map[string]any{"skills": []any{"deploy", "team/review", "skill://team/ops/deploy/SKILL.md"}})
	url, _ := out["url"].(string)
	ticket, ok := strings.CutPrefix(url, "https://toolyard.example"+SkillBundlePathPrefix)
	skills, _ := out["skills"].([]any)
	if !ok || !validBundleTicket(ticket) || len(skills) != 2 || out["expires_at"] == "" || !strings.Contains(out["next"].(string), "tar -xzf") {
		t.Fatalf("skills.bundle = %v", out)
	}
	if s := skills[0].(map[string]any); s["uri"] != "skill://team/ops/deploy/SKILL.md" || s["repo"] != "acme/playbooks@0123456789ab" || s["source"] != "team" {
		t.Fatalf("bundled skill = %v", s)
	}
	var stored string
	if err := f.db.QueryRow(`SELECT skills FROM skill_bundle_tickets`).Scan(&stored); err != nil || strings.Contains(stored, ticket) {
		t.Fatalf("stored ticket row: %q %v", stored, err)
	}

	// The download works once, as the member, with each skill's version
	// in the audit row.
	b, err := f.gw.OpenSkillBundle(context.Background(), ticket)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := b.Write(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "bundle:skill://team/ops/deploy/SKILL.md,skill://team/review/SKILL.md" {
		t.Fatalf("bundle bytes = %q", buf.String())
	}
	if v := f.reader.lastViewer(t); v.AgentID != "ag_member" || v.UserID != "u_member" || v.Admin {
		t.Fatalf("download viewer = %+v", v)
	}
	if _, err := f.gw.OpenSkillBundle(context.Background(), ticket); !errors.Is(err, ErrSkillBundleTicket) {
		t.Fatalf("second download: %v", err)
	}
	if !auditHas(t, f.aud, EventSkillRead, "GET "+SkillBundlePathPrefix, "ag_member", "acme/playbooks@0123456789ab") {
		t.Fatal("the download wrote no audit row with the version")
	}

	// Expired and unknown tickets answer alike.
	out = f.ok(t, f.member, SkillsBundleTool, map[string]any{"skills": []any{"deploy"}})
	late := strings.TrimPrefix(out["url"].(string), "https://toolyard.example"+SkillBundlePathPrefix)
	f.tickets.now = func() time.Time { return time.Now().Add(SkillBundleTTL + time.Minute) }
	if _, err := f.gw.OpenSkillBundle(context.Background(), late); !errors.Is(err, ErrSkillBundleTicket) {
		t.Fatalf("expired download: %v", err)
	}
	f.tickets.now = time.Now
	for _, bad := range []string{"", "tyskb_short", strings.Repeat("x", 49), "tyskb_" + strings.Repeat("!", 43)} {
		if _, err := f.gw.OpenSkillBundle(context.Background(), bad); !errors.Is(err, ErrSkillBundleTicket) {
			t.Errorf("ticket %q: %v", bad, err)
		}
	}

	// An invisible or unknown skill fails the whole bundle, before any
	// ticket exists; so do an empty or oversized list.
	if code := f.refused(t, f.member, SkillsBundleTool, map[string]any{"skills": []any{"deploy", "secret-sauce"}}); code != SkillsErrNotFound {
		t.Fatalf("bundle with a private skill: code %q", code)
	}
	tooMany := make([]any, skillsBundleMax+1)
	for i := range tooMany {
		tooMany[i] = "deploy"
	}
	for _, bad := range [][]any{nil, {}, tooMany} {
		if code := f.refused(t, f.member, SkillsBundleTool, map[string]any{"skills": bad}); code != SkillsErrInvalid {
			t.Errorf("bundle of %d: code %q", len(bad), code)
		}
	}
	// A caller holds at most maxLiveBundleTickets unused links.
	for i := 0; i < maxLiveBundleTickets; i++ {
		res := f.call(t, f.admin, SkillsBundleTool, map[string]any{"skills": []any{"deploy"}})
		if res.IsError {
			t.Fatalf("bundle %d: %s", i, textOf(res))
		}
	}
	if code := f.refused(t, f.admin, SkillsBundleTool, map[string]any{"skills": []any{"deploy"}}); code != SkillsErrLimit {
		t.Fatalf("bundle past the cap: code %q", code)
	}
	// Visibility is checked again at download time: a ticket whose caller
	// lost sight of a skill fails.
	out = f.ok(t, f.host, SkillsBundleTool, map[string]any{"skills": []any{"host-ops"}})
	hostTicket := strings.TrimPrefix(out["url"].(string), "https://toolyard.example"+SkillBundlePathPrefix)
	f.res.set("ag_host", access.Scope{Denied: true})
	f.reader.setFail("Get", skillhub.ErrNotFound)
	if _, err := f.gw.OpenSkillBundle(context.Background(), hostTicket); !errors.Is(err, skillhub.ErrNotFound) {
		t.Fatalf("download after losing access: %v", err)
	}

	// Without a ticket store the tool says bundles are unavailable.
	noTickets := newAccessFixture(t, nil)
	noTickets.gw.RegisterSkillHub(SkillHub{Reader: &fakeSkillReader{}})
	res := noTickets.call(t, noTickets.member, SkillsBundleTool, map[string]any{"skills": []any{"deploy"}})
	if code, _ := structured(t, res)["error"].(string); !res.IsError || code != SkillsErrUnavailable {
		t.Fatalf("bundle without tickets: %s", textOf(res))
	}
}

// asList is v as a JSON array (nil when it is not one).
func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

// auditHas reports whether a recent audit row of eventType for tool and
// agent mentions want in its summary.
func auditHas(t *testing.T, aud *audit.Logger, eventType, tool, agent, want string) bool {
	t.Helper()
	evs, err := aud.Recent(context.Background(), 200)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		if e.EventType == eventType && e.ToolName == tool && e.AgentID == agent && strings.Contains(e.ResultSummary, want) {
			return true
		}
	}
	return false
}

func TestToolsSearchAddsSkills(t *testing.T) {
	f := newSkillsFixture(t)
	out := f.ok(t, f.member, MetaSearchTool, map[string]any{"query": "deploy"})
	skills, _ := out["skills"].([]any)
	if len(skills) != 1 {
		t.Fatalf("tools.search skills = %v", out["skills"])
	}
	s := skills[0].(map[string]any)
	if s["name"] != "deploy" || s["uri"] != "skill://team/ops/deploy/SKILL.md" || s["repo"] != "acme/playbooks@0123456789ab" || s["description"] != "Deploy a service safely" {
		t.Fatalf("tools.search skill = %v", s)
	}
	// At most three, by the caller's view.
	if out := f.ok(t, f.admin, MetaSearchTool, map[string]any{"query": "e"}); len(asList(out["skills"])) != searchSkillHits {
		t.Fatalf("admin tools.search skills = %v", out["skills"])
	}
	f.reader.mu.Lock()
	last := f.reader.searches[len(f.reader.searches)-1]
	f.reader.mu.Unlock()
	if last.Limit != searchSkillHits || last.Mode != "keyword" {
		t.Fatalf("tools.search skills query = %+v", last)
	}
	f.ok(t, f.member, MetaSearchTool, map[string]any{"query": "^dep", "mode": "regex"})
	f.reader.mu.Lock()
	last = f.reader.searches[len(f.reader.searches)-1]
	f.reader.mu.Unlock()
	if last.Mode != "regex" {
		t.Fatalf("regex tools.search skills query = %+v", last)
	}
	// No query, no match, a limited agent or a failing catalog: no section.
	for _, c := range []struct {
		ctx  context.Context
		args map[string]any
	}{
		{f.member, map[string]any{"query": ""}},
		{f.member, map[string]any{"query": "zzz-nothing"}},
		{f.limited, map[string]any{"query": "deploy"}},
	} {
		if out := f.ok(t, c.ctx, MetaSearchTool, c.args); out["skills"] != nil {
			t.Errorf("tools.search %v: skills = %v", c.args, out["skills"])
		}
	}
	f.reader.setFail("Search", errors.New("down"))
	if out := f.ok(t, f.member, MetaSearchTool, map[string]any{"query": "deploy"}); out["skills"] != nil || out["results"] == nil {
		t.Fatalf("tools.search with a failing catalog = %v", out)
	}
	// Hub off: tools.search is as before.
	off := newAccessFixture(t, nil)
	if out := structured(t, off.call(t, off.member, MetaSearchTool, map[string]any{"query": "deploy"})); out["skills"] != nil {
		t.Fatalf("tools.search without the hub = %v", out)
	}
}

func TestSkillsCodeModeStubsAndCalls(t *testing.T) {
	f := newSkillsFixture(t)
	py := textOf(route(t, f.gw, f.member, CodeModeListToolFiles, map[string]any{}))
	for _, n := range SkillToolNames {
		want := "    " + strings.Replace(n, ".", "_", 1) + ".pyi\n"
		if !strings.Contains(py, want) {
			t.Errorf("listToolFiles misses %q:\n%s", want, py)
		}
	}
	if strings.Contains(py, "  skills/") {
		t.Fatalf("skills tools must be bound under toolyard:\n%s", py)
	}
	stub := route(t, f.gw, f.member, CodeModeReadToolFile, map[string]any{"fileName": "servers/toolyard/skills_search.pyi"})
	if stub.IsError || !strings.Contains(textOf(stub), "def skills_search(query: str") || strings.Contains(textOf(stub), ReasonField) {
		t.Fatalf("skills_search stub:\n%s", textOf(stub))
	}
	js := textOf(route(t, f.gw, f.member, CodeModeListToolFiles, map[string]any{"language": "javascript"}))
	if !strings.Contains(js, "    skills_get.d.ts\n") {
		t.Fatalf("JavaScript listing:\n%s", js)
	}
	jsStub := route(t, f.gw, f.member, CodeModeReadToolFile, map[string]any{"fileName": "servers/toolyard/skills_get.d.ts"})
	if jsStub.IsError || !strings.Contains(textOf(jsStub), "export interface Input") || !strings.Contains(textOf(jsStub), "skill") {
		t.Fatalf("skills_get .d.ts:\n%s", textOf(jsStub))
	}

	// Callable from Starlark and JavaScript, as the member.
	res := route(t, f.gw, f.member, CodeModeExecuteToolCode, map[string]any{
		"code": "# find the deploy playbook\nresult = toolyard.skills_search(query=\"deploy\")", ReasonField: testReason,
	})
	if res.IsError || !strings.Contains(textOf(res), "skill://team/ops/deploy/SKILL.md") {
		t.Fatalf("Starlark skills_search:\n%s", textOf(res))
	}
	res = route(t, f.gw, f.member, CodeModeExecuteToolCode, map[string]any{
		"language": "javascript", ReasonField: testReason,
		"code": "const s = await tools.toolyard.skills_get({skill: \"deploy\", include_files: false});\nreturn s;",
	})
	if res.IsError || !strings.Contains(textOf(res), "acme/playbooks@0123456789ab") {
		t.Fatalf("JavaScript skills_get:\n%s", textOf(res))
	}
	if v := f.reader.lastViewer(t); v.AgentID != "ag_member" || v.UserID != "u_member" {
		t.Fatalf("code-mode viewer = %+v", v)
	}
}

// TestSkillsProposalTool: an agent polls its own proposal; another agent,
// an unknown id or a missing id is refused, and without proposals the
// tool says it is not available.
func TestSkillsProposalTool(t *testing.T) {
	f := newSkillsFixture(t)
	out := f.ok(t, f.member, SkillsProposalTool, map[string]any{"id": "skp_source"})
	p, _ := out["proposal"].(map[string]any)
	if p["id"] != "skp_source" || p["status"] != "done" || p["result"] != "src_new" {
		t.Fatalf("skills.proposal = %v", out)
	}
	if next, _ := out["next"].(string); !strings.Contains(next, "src_new") {
		t.Fatalf("next = %q", next)
	}
	if code := f.refused(t, f.admin, SkillsProposalTool, map[string]any{"id": "skp_source"}); code != SkillsErrNotFound {
		t.Fatalf("another agent's proposal: code %q", code)
	}
	if code := f.refused(t, f.member, SkillsProposalTool, map[string]any{"id": "skp_other"}); code != SkillsErrNotFound {
		t.Fatalf("unknown proposal: code %q", code)
	}
	if code := f.refused(t, f.member, SkillsProposalTool, map[string]any{"id": " "}); code != SkillsErrInvalid {
		t.Fatalf("missing id: code %q", code)
	}
	if code := f.refused(t, WithAgentID(context.Background(), "dashboard:u_member"), SkillsProposalTool, map[string]any{"id": "skp_source"}); code != SkillsErrInvalid {
		t.Fatalf("a dashboard caller: code %q", code)
	}
	for status, want := range map[string]string{"pending": "not decided", "rejected": "did not take it", "failed": "could not be applied", "approved": "applying"} {
		if got := skillsProposalNext(&skillhub.Proposal{Kind: "source", Status: status, Error: "x"}); !strings.Contains(got, want) {
			t.Errorf("%s: next %q", status, got)
		}
	}
}

// TestSkillsProposeSourceRateLimited: GitHub's rate limit during the dry
// run is a rate_limited refusal with when to try again, never internal.
func TestSkillsProposeSourceRateLimited(t *testing.T) {
	f := newSkillsFixture(t)
	until := time.Now().Add(5 * time.Minute)
	f.props.mu.Lock()
	f.props.err = fmt.Errorf("skillhub: preview: %w", skillhub.RateLimitError(until))
	f.props.mu.Unlock()
	res := f.call(t, f.member, SkillsProposeSourceTool, map[string]any{"url": "acme/x"})
	if !res.IsError {
		t.Fatalf("a rate-limited proposal succeeded: %s", textOf(res))
	}
	got := structured(t, res)
	secs, _ := got["retry_after"].(float64) // JSON numbers
	if got["error"] != SkillsErrRateLimited || secs < 290 || secs > 301 || got["reset_at"] != until.UTC().Format(time.RFC3339) {
		t.Fatalf("refusal = %v", got)
	}
	if msg, _ := got["message"].(string); !strings.Contains(msg, "GitHub access") {
		t.Fatalf("message = %q", msg)
	}
}

// TestToolsSearchNamesSkillsOnlyWithTheHub: the tools.search description
// mentions skills only while the hub is registered.
func TestToolsSearchNamesSkillsOnlyWithTheHub(t *testing.T) {
	off := newAccessFixture(t, nil)
	off.gw.mu.RLock()
	desc := off.gw.tools[MetaSearchTool].tool.Description
	off.gw.mu.RUnlock()
	if strings.Contains(desc, "skills") || desc != metaSearchDescription(false) {
		t.Fatalf("hub off: %q", desc)
	}
	on := newSkillsFixture(t)
	on.gw.mu.RLock()
	desc = on.gw.tools[MetaSearchTool].tool.Description
	on.gw.mu.RUnlock()
	if !strings.Contains(desc, metaSearchSkillsSentence) {
		t.Fatalf("hub on: %q", desc)
	}
}
