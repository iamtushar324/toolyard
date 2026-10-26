package gateway

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
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
	"github.com/tusharbhardwaj/toolyard/internal/store/storetest"
)

// sharingGitHub serves one public repository, acme/playbooks, at one
// commit: the part of the GitHub API a first sync uses (repository, branch
// head, tarball and its archive redirect).
func sharingGitHub(t *testing.T, tree map[string]string) *httptest.Server {
	t.Helper()
	const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	var srv *httptest.Server
	mux := http.NewServeMux()
	writeJSON := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("GET /repos/acme/playbooks", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"name": "playbooks", "private": false, "default_branch": "main",
			"html_url": "https://github.com/acme/playbooks", "owner": map[string]any{"login": "acme"}})
	})
	mux.HandleFunc("GET /repos/acme/playbooks/git/ref/heads/main", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"`+sha+`"`)
		writeJSON(w, map[string]any{"ref": "refs/heads/main", "object": map[string]any{"sha": sha, "type": "commit"}})
	})
	mux.HandleFunc("GET /repos/acme/playbooks/tarball/{sha}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL+"/_codeload/acme/playbooks/"+r.PathValue("sha"), http.StatusFound)
	})
	mux.HandleFunc("GET /_codeload/acme/playbooks/{sha}", func(w http.ResponseWriter, r *http.Request) {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		top := "acme-playbooks-aaaaaaa"
		_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: top + "/", Mode: 0o755, ModTime: time.Unix(0, 0)})
		paths := make([]string, 0, len(tree))
		for p := range tree {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		for _, p := range paths {
			_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: top + "/" + p, Size: int64(len(tree[p])), Mode: 0o644, ModTime: time.Unix(0, 0)})
			_, _ = tw.Write([]byte(tree[p]))
		}
		_ = tw.Close()
		_ = gz.Close()
		_, _ = w.Write(buf.Bytes())
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func sharingSkillMD(name, desc, body string) string {
	return "---\nname: " + name + "\ndescription: " + desc + "\n---\n" + body
}

// newSharingFixture is the skills fixture over the real skills hub engine
// with one source, "playbooks": review (everyone) and secret, which the
// test hides. Callers: ag_member (owner u_member), ag_admin (an admin) and
// ag_blk (owner u_blk, whom the test blocks).
func newSharingFixture(t *testing.T) (*skillsFixture, *skillhub.Service, *skillhub.Source) {
	t.Helper()
	db := storetest.Open(t)
	t.Cleanup(func() { _ = db.Close() })
	gh := sharingGitHub(t, map[string]string{
		"skills/review/SKILL.md":  sharingSkillMD("review", "Review a change carefully", "Read the diff.\n"),
		"skills/review/notes.md":  "open-word in the review notes\n",
		"skills/secret/SKILL.md":  sharingSkillMD("secret", "Secret procedure for incidents", "Follow steps.md.\n"),
		"skills/secret/steps.md":  "classified-word step one\n",
		"skills/secret/script.sh": "#!/bin/sh\necho classified-word\n",
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	svc, err := skillhub.New(skillhub.Options{DB: db, GitHubAPI: gh.URL, HTTPClient: gh.Client()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Wait)
	if err := svc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	src, err := svc.CreateSource(ctx, "u_admin", skillhub.SourceInput{URL: "acme/playbooks", Path: "skills", Mode: skillhub.ModeAuto})
	if err != nil {
		t.Fatal(err)
	}
	if res, err := svc.SyncNow(ctx, src.ID); err != nil || !res.Activated {
		t.Fatalf("sync %+v %v", res, err)
	}
	bus, err := approval.New(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	f := &skillsFixture{
		db: db, aud: audit.New(db), pol: policy.New(db), props: &fakeProposals{}, hosts: &fakeHosts{on: map[string]bool{}},
		res: &fakeResolver{scopes: map[string]access.Scope{"ag_member": groups(), "ag_admin": {All: true}, "ag_blk": groups()}},
	}
	f.gw = New(Options{
		Policy: f.pol, Approval: bus, Audit: f.aud, Hub: realtime.NewHub(), Memory: memory.New(db), Access: f.res,
		Owners:    skillOwners{"ag_member": "u_member", "ag_admin": "u_admin", "ag_blk": "u_blk"},
		PublicURL: "https://toolyard.example",
	})
	f.gw.RegisterBuiltins()
	t.Cleanup(func() { _ = f.gw.Close() })
	f.tickets = NewSkillBundleTickets(db)
	f.gw.RegisterSkillHub(SkillHub{Reader: svc, Proposals: f.props, Tickets: f.tickets, OnHost: f.hosts.OnHost})
	bg := context.Background()
	f.member = WithAgentID(bg, "ag_member")
	f.admin = WithAgentID(bg, "ag_admin")
	f.blocked = actor.WithRaiser(WithAgentID(bg, "ag_blk"), actor.Raiser{CallerID: "ag_blk"})
	return f, svc, src
}

const (
	sharingHidden     = "skill://playbooks/secret/SKILL.md"
	sharingHiddenFile = "skill://playbooks/secret/steps.md"
	sharingOpen       = "skill://playbooks/review/SKILL.md"
)

// A skill set to nobody is hidden on every agent path: the skills.* tools
// (search, get, read, grep, list, sources, bundle create and redeem),
// tools.search's skills section and the MCP Skills extension (skills/list,
// skills/get, resources/read). An admin's agent does not see it either.
func TestSkillSetToNobodyOnEveryAgentPath(t *testing.T) {
	f, svc, src := newSharingFixture(t)
	ctx := context.Background()
	// Before: the member sees both skills.
	if out := textOf(f.call(t, f.member, SkillsSearchTool, map[string]any{"query": "secret procedure"})); !strings.Contains(out, sharingHidden) {
		t.Fatalf("before hiding, search = %s", out)
	}
	// A bundle link issued now is redeemed after the skill is hidden.
	early := f.ok(t, f.member, SkillsBundleTool, map[string]any{"skills": []any{sharingHidden}})
	earlyTicket := strings.TrimPrefix(early["url"].(string), "https://toolyard.example"+SkillBundlePathPrefix)

	if _, err := svc.SetSkillSharing(ctx, "u_admin", src.ID, "secret", skillhub.SkillSharing{Mode: skillhub.VisibleNobody}); err != nil {
		t.Fatal(err)
	}
	for name, who := range map[string]context.Context{"member": f.member, "admin's agent": f.admin} {
		if out := textOf(f.call(t, who, SkillsSearchTool, map[string]any{"query": "secret procedure"})); strings.Contains(out, "secret") && strings.Contains(out, sharingHidden) {
			t.Errorf("%s search shows the hidden skill: %s", name, out)
		}
		if res := f.call(t, who, SkillsGetTool, map[string]any{"skill": sharingHidden}); !res.IsError {
			t.Errorf("%s get: %s", name, textOf(res))
		}
		if res := f.call(t, who, SkillsGetTool, map[string]any{"skill": "secret"}); !res.IsError {
			t.Errorf("%s get by name: %s", name, textOf(res))
		}
		if res := f.call(t, who, SkillsReadTool, map[string]any{"uri": sharingHiddenFile}); !res.IsError {
			t.Errorf("%s read: %s", name, textOf(res))
		}
		if out := textOf(f.call(t, who, SkillsGrepTool, map[string]any{"pattern": "classified-word"})); strings.Contains(out, "secret/") {
			t.Errorf("%s grep: %s", name, out)
		}
		if out := textOf(f.call(t, who, SkillsListTool, map[string]any{})); strings.Contains(out, sharingHidden) || !strings.Contains(out, sharingOpen) {
			t.Errorf("%s list: %s", name, out)
		}
		if res := f.call(t, who, SkillsBundleTool, map[string]any{"skills": []any{sharingHidden}}); !res.IsError {
			t.Errorf("%s bundle: %s", name, textOf(res))
		}
		out := f.ok(t, who, MetaSearchTool, map[string]any{"query": "secret procedure incidents"})
		if b, _ := json.Marshal(out["skills"]); strings.Contains(string(b), sharingHidden) {
			t.Errorf("%s tools.search skills = %s", name, b)
		}
		srcs := f.ok(t, who, SkillsSourcesTool, map[string]any{})
		if b, _ := json.Marshal(srcs); !strings.Contains(string(b), `"skill_count":1`) {
			t.Errorf("%s sources = %s (want one visible skill)", name, b)
		}
	}
	// The link issued before the change no longer downloads the skill.
	if _, err := f.gw.OpenSkillBundle(ctx, earlyTicket); !errors.Is(err, skillhub.ErrNotFound) {
		t.Fatalf("redeeming a bundle of a now hidden skill: %v", err)
	}

	// The MCP Skills extension.
	c := newSkillsMCPClient(t, f.gw, "ag_member", false, false)
	c.initialize()
	list := c.result(MethodSkillsList, map[string]any{})
	if b, _ := json.Marshal(list); strings.Contains(string(b), "playbooks/secret") || !strings.Contains(string(b), sharingOpen) {
		t.Fatalf("skills/list = %s", b)
	}
	if out := c.send(MethodSkillsGet, map[string]any{"uri": sharingHidden}, false); rpcErrorCode(out) != mcp.INVALID_PARAMS {
		t.Fatalf("skills/get of the hidden skill = %v", out)
	}
	if out := c.send(string(mcp.MethodResourcesRead), map[string]any{"uri": sharingHiddenFile}, false); rpcErrorCode(out) != mcp.RESOURCE_NOT_FOUND {
		t.Fatalf("resources/read of the hidden file = %v", out)
	}
	if read := c.result(string(mcp.MethodResourcesRead), map[string]any{"uri": "skill://playbooks/review/notes.md"}); read["contents"] == nil {
		t.Fatalf("resources/read of a shared file = %v", read)
	}
}

// A blocked person's agent sees nothing of a source on any agent path;
// another member's agent is unaffected.
func TestBlockedPersonOnEveryAgentPath(t *testing.T) {
	f, svc, src := newSharingFixture(t)
	ctx := context.Background()
	ticket := strings.TrimPrefix(f.ok(t, f.blocked, SkillsBundleTool, map[string]any{"skills": []any{sharingOpen}})["url"].(string),
		"https://toolyard.example"+SkillBundlePathPrefix)
	blocked := []string{"u_blk"}
	if _, err := svc.UpdateSource(ctx, "u_admin", src.ID, skillhub.SourceUpdate{Blocked: &blocked}); err != nil {
		t.Fatal(err)
	}
	if out := textOf(f.call(t, f.blocked, SkillsSearchTool, map[string]any{"query": "review"})); strings.Contains(out, sharingOpen) {
		t.Errorf("blocked search: %s", out)
	}
	if res := f.call(t, f.blocked, SkillsGetTool, map[string]any{"skill": sharingOpen}); !res.IsError {
		t.Errorf("blocked get: %s", textOf(res))
	}
	if res := f.call(t, f.blocked, SkillsReadTool, map[string]any{"uri": "skill://playbooks/review/notes.md"}); !res.IsError {
		t.Errorf("blocked read: %s", textOf(res))
	}
	if out := textOf(f.call(t, f.blocked, SkillsGrepTool, map[string]any{"pattern": "open-word"})); strings.Contains(out, "notes.md") {
		t.Errorf("blocked grep: %s", out)
	}
	if out := textOf(f.call(t, f.blocked, SkillsListTool, map[string]any{})); strings.Contains(out, "playbooks") {
		t.Errorf("blocked list: %s", out)
	}
	if out := textOf(f.call(t, f.blocked, SkillsSourcesTool, map[string]any{})); strings.Contains(out, "playbooks") {
		t.Errorf("blocked sources: %s", out)
	}
	if res := f.call(t, f.blocked, SkillsBundleTool, map[string]any{"skills": []any{sharingOpen}}); !res.IsError {
		t.Errorf("blocked bundle: %s", textOf(res))
	}
	if _, err := f.gw.OpenSkillBundle(ctx, ticket); !errors.Is(err, skillhub.ErrNotFound) {
		t.Errorf("blocked bundle redeem: %v", err)
	}
	out := f.ok(t, f.blocked, MetaSearchTool, map[string]any{"query": "review change"})
	if b, _ := json.Marshal(out["skills"]); strings.Contains(string(b), "playbooks") {
		t.Errorf("blocked tools.search skills = %s", b)
	}
	c := newSkillsMCPClient(t, f.gw, "ag_blk", false, false)
	c.initialize()
	if list := c.result(MethodSkillsList, map[string]any{}); len(list["skills"].([]any)) != 0 {
		t.Errorf("blocked skills/list = %v", list)
	}
	if out := c.send(string(mcp.MethodResourcesRead), map[string]any{"uri": "skill://playbooks/review/notes.md"}, false); rpcErrorCode(out) != mcp.RESOURCE_NOT_FOUND {
		t.Errorf("blocked resources/read = %v", out)
	}
	// Another member is unaffected.
	if res := f.call(t, f.member, SkillsGetTool, map[string]any{"skill": sharingOpen}); res.IsError {
		t.Fatalf("member get: %s", textOf(res))
	}
}
