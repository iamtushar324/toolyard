package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/access"
	"github.com/tusharbhardwaj/toolyard/internal/approval"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/memory"
	"github.com/tusharbhardwaj/toolyard/internal/policy"
	"github.com/tusharbhardwaj/toolyard/internal/realtime"
	"github.com/tusharbhardwaj/toolyard/internal/skillhub"
	"github.com/tusharbhardwaj/toolyard/internal/store/storetest"
)

// An agent on a fresh gateway finds toolyard's built-in guide with
// skills.search, reads it with skills.get and a reference file with
// skills.read, sees the built-in skills in the MCP Skills extension's
// listing, and the server instructions point it at the guide. The hub is
// the real one, over the database, with no GitHub.
func TestBuiltinSkillsThroughTheGateway(t *testing.T) {
	db := storetest.Open(t)
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the built-in skills called GitHub: %s", r.URL.Path)
		http.Error(w, "no", http.StatusTeapot)
	}))
	t.Cleanup(gh.Close)
	hub, err := skillhub.New(skillhub.Options{DB: db, GitHubAPI: gh.URL, HTTPClient: gh.Client(), BuiltinSkills: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(hub.Wait)
	t.Cleanup(cancel)
	if err := hub.Start(ctx); err != nil {
		t.Fatal(err)
	}
	bus, err := approval.New(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	gw := New(Options{
		Policy: policy.New(db), Approval: bus, Audit: audit.New(db), Hub: realtime.NewHub(), Memory: memory.New(db),
		Access:    &fakeResolver{scopes: map[string]access.Scope{"ag_member": groups()}},
		Owners:    skillOwners{"ag_member": "u_member"},
		PublicURL: "https://toolyard.example",
	})
	gw.RegisterBuiltins()
	t.Cleanup(func() { _ = gw.Close() })
	if hub.BuiltinGuide() != "toolyard/toolyard-basics" {
		t.Fatalf("guide = %q", hub.BuiltinGuide())
	}
	gw.RegisterSkillHub(SkillHub{Reader: hub, Guide: hub.BuiltinGuide()})
	member := WithAgentID(context.Background(), "ag_member")
	call := func(tool string, args map[string]any) map[string]any {
		t.Helper()
		full := map[string]any{ReasonField: testReason}
		for k, v := range args {
			full[k] = v
		}
		res, err := gw.RouteCall(member, "test", tool, full)
		if err != nil || res.IsError {
			t.Fatalf("%s %v: %v %s", tool, args, err, textOf(res))
		}
		return structured(t, res)
	}

	found := call(SkillsSearchTool, map[string]any{"query": "toolyard basics"})
	hit := false
	for _, s := range asList(found["skills"]) {
		m := s.(map[string]any)
		if m["uri"] == "skill://toolyard/toolyard-basics/SKILL.md" {
			hit = m["source"] == "toolyard" && strings.HasPrefix(m["repo"].(string), "toolyard/builtin@") && m["runs"] == "anywhere"
		}
	}
	if !hit {
		t.Fatalf("skills.search did not find the guide: %v", found)
	}
	got := call(SkillsGetTool, map[string]any{"skill": "toolyard/toolyard-basics"})
	if fm, _ := got["frontmatter"].(map[string]any); fm["name"] != "toolyard-basics" || !strings.Contains(got["body"].(string), "_reason") {
		t.Fatalf("skills.get = %v", got)
	}
	var ref string
	for _, f := range asList(got["files"]) {
		if u := f.(map[string]any)["uri"].(string); strings.Contains(u, "/references/") {
			ref = u
			break
		}
	}
	if ref == "" {
		t.Fatalf("the guide lists no reference file: %v", got["files"])
	}
	if read := call(SkillsReadTool, map[string]any{"uri": ref}); strings.TrimSpace(read["content"].(string)) == "" || read["skill"] != "skill://toolyard/toolyard-basics/SKILL.md" {
		t.Fatalf("skills.read %s = %v", ref, read)
	}

	c := newSkillsMCPClient(t, gw, "ag_member", false, false)
	init := c.initialize()
	if instr, _ := init["instructions"].(string); !strings.Contains(instr, `"toolyard/toolyard-basics"`) || !strings.Contains(instr, "SKILLS:") {
		t.Fatalf("instructions: %q", instr)
	}
	list := c.result(MethodSkillsList, map[string]any{})
	listed := false
	for _, s := range asList(list["skills"]) {
		listed = listed || s.(map[string]any)["uri"] == "skill://toolyard/toolyard-basics/SKILL.md"
	}
	if !listed {
		t.Fatalf("skills/list leaves out the built-in guide: %v", list)
	}
}
