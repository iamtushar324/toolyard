package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// newSkillsMCPClient serves the gateway's MCP endpoint with MCPSkills in
// front, as cmd/gateway wires it, with every request from agent. stateless
// is the -stateless-mcp transport a 2026-07-28 client uses.
func newSkillsMCPClient(t *testing.T, g *Gateway, agent string, stateless, localhostProtection bool) *tasksClient {
	t.Helper()
	ctxFunc := func(ctx context.Context, _ *http.Request) context.Context { return WithAgentID(ctx, agent) }
	streamable := server.NewStreamableHTTPServer(g.MCPServer(), server.WithHTTPContextFunc(ctxFunc),
		server.WithStateLess(stateless), server.WithDisableLocalhostProtection(!localhostProtection))
	srv := httptest.NewServer(PreserveToolCallNumbers(g.MCPSkills(g.MCPTasks(streamable, ctxFunc, localhostProtection), ctxFunc, localhostProtection)))
	t.Cleanup(srv.Close)
	return &tasksClient{t: t, url: srv.URL + "/mcp"}
}

// modernParams are params of a 2026-07-28 request, with extra fields.
func modernParams(extra map[string]any) map[string]any {
	p := map[string]any{"_meta": map[string]any{
		mcp.MetaKeyProtocolVersion:                   mcp.ProtocolVersion20260728,
		mcp.MetaKeyClientInfo:                        map[string]any{"name": "skills-test", "version": "1.0.0"},
		"io.modelcontextprotocol/clientCapabilities": map[string]any{},
	}}
	for k, v := range extra {
		p[k] = v
	}
	return p
}

// modernHeaders are the headers of a 2026-07-28 request for method (and
// the Mcp-Name a resources/read needs).
func modernHeaders(method, name string) http.Header {
	h := http.Header{mcp.HeaderProtocolVersion: {mcp.ProtocolVersion20260728}, mcp.HeaderMethod: {method}}
	if name != "" {
		enc, _ := mcp.EncodeHeaderValue(name)
		h.Set(mcp.HeaderName, enc)
	}
	return h
}

// rpcErrorCode is a JSON-RPC answer's error code (0 when it succeeded).
func rpcErrorCode(out map[string]any) int {
	e, _ := out["error"].(map[string]any)
	code, _ := e["code"].(float64)
	return int(code)
}

func skillsExtension(t *testing.T, res map[string]any) map[string]any {
	t.Helper()
	caps, _ := res["capabilities"].(map[string]any)
	if caps["resources"] == nil {
		t.Errorf("the resources capability is missing: %v", caps)
	}
	ext, _ := caps["extensions"].(map[string]any)
	got, _ := ext[SkillsExtensionID].(map[string]any)
	return got
}

// The extension is declared on both protocol versions, with directory
// reads, next to the resources capability; the server instructions point
// agents at the catalog. Without the hub nothing of it shows.
func TestSkillsExtensionDeclaredOnBothVersions(t *testing.T) {
	f := newSkillsFixture(t)
	legacy := newSkillsMCPClient(t, f.gw, "ag_member", false, false)
	init := legacy.initialize()
	if ext := skillsExtension(t, init); ext == nil || ext["directoryRead"] != true {
		t.Fatalf("2025-11-25 initialize: %v", init["capabilities"])
	}
	if instr, _ := init["instructions"].(string); !strings.Contains(instr, "SKILLS:") || !strings.Contains(instr, "skills.search") || !strings.Contains(instr, "PERMISSIONS:") {
		t.Fatalf("instructions: %q", instr)
	}

	modern := newSkillsMCPClient(t, f.gw, "ag_member", true, false)
	modern.header = modernHeaders("server/discover", "")
	disc := modern.result("server/discover", modernParams(nil))
	if ext := skillsExtension(t, disc); ext == nil || ext["directoryRead"] != true {
		t.Fatalf("2026-07-28 server/discover: %v", disc["capabilities"])
	}
	if instr, _ := disc["instructions"].(string); !strings.Contains(instr, "SKILLS:") {
		t.Fatalf("discover instructions: %q", instr)
	}

	off := newAccessFixture(t, nil)
	offInit := newSkillsMCPClient(t, off.gw, memberID, false, false).initialize()
	caps, _ := offInit["capabilities"].(map[string]any)
	if ext, _ := caps["extensions"].(map[string]any); ext[SkillsExtensionID] != nil {
		t.Fatalf("hub off still declares the extension: %v", caps)
	}
	if instr, _ := offInit["instructions"].(string); strings.Contains(instr, "SKILLS:") {
		t.Fatal("hub off still mentions skills in the instructions")
	}
}

func TestSkillsListAndGetMethods(t *testing.T) {
	f := newSkillsFixture(t)
	c := newSkillsMCPClient(t, f.gw, "ag_member", false, false)
	c.initialize()

	list := c.result(MethodSkillsList, map[string]any{})
	skills, _ := list["skills"].([]any)
	if len(skills) != 2 || list["nextCursor"] != nil || list["resultType"] != nil {
		t.Fatalf("member skills/list = %v", list)
	}
	entry := skills[0].(map[string]any)
	if len(entry) != 3 || entry["uri"] != "skill://team/ops/deploy/SKILL.md" {
		t.Fatalf("an entry is {uri, frontmatter, resources} only: %v", entry)
	}
	if fm, _ := entry["frontmatter"].(map[string]any); fm["name"] != "deploy" || fm["description"] != "Deploy a service safely" {
		t.Fatalf("frontmatter = %v", entry["frontmatter"])
	}
	resources, _ := entry["resources"].([]any)
	found := false
	for _, r := range resources {
		m := r.(map[string]any)
		if len(m) != 3 || !strings.HasPrefix(m["digest"].(string), "sha256:") || len(m["digest"].(string)) != len("sha256:")+64 {
			t.Fatalf("resource = %v", m)
		}
		if m["uri"] == entry["uri"] {
			found = true
			content := fakeSkillSources[0].skills[0].files["SKILL.md"]
			if m["digest"] != digestOf(content) || m["size"] != float64(len(content)) {
				t.Fatalf("SKILL.md resource = %v", m)
			}
		}
	}
	if !found || len(resources) != 4 {
		t.Fatalf("resources must list every file, SKILL.md included: %v", resources)
	}
	if page := c.result(MethodSkillsList, map[string]any{"cursor": "1"}); len(page["skills"].([]any)) != 1 {
		t.Fatalf("skills/list from cursor 1 = %v", page)
	}
	if out := c.send(MethodSkillsList, map[string]any{"cursor": "nope"}, false); rpcErrorCode(out) != mcp.INVALID_PARAMS {
		t.Fatalf("bad cursor = %v", out)
	}
	admin := newSkillsMCPClient(t, f.gw, "ag_admin", false, false)
	admin.initialize()
	if all := admin.result(MethodSkillsList, nil); len(all["skills"].([]any)) != 4 {
		t.Fatalf("admin skills/list = %v", all)
	}

	got := c.result(MethodSkillsGet, map[string]any{"uri": "skill://team/ops/deploy/SKILL.md"})
	skill, _ := got["skill"].(map[string]any)
	if skill["uri"] != "skill://team/ops/deploy/SKILL.md" || len(skill["resources"].([]any)) != 4 {
		t.Fatalf("skills/get = %v", got)
	}
	if !auditHas(t, f.aud, EventSkillRead, MethodSkillsGet, "ag_member", "skill://team/ops/deploy/SKILL.md acme/playbooks@0123456789ab") {
		t.Fatal("skills/get wrote no audit row with the version")
	}
	// Unknown, invisible, not a SKILL.md, or not a URI: -32602.
	for _, uri := range []string{"skill://team/nope/SKILL.md", "skill://private/secret-sauce/SKILL.md", "skill://team/ops/deploy/references/checklist.md", "deploy", ""} {
		if out := c.send(MethodSkillsGet, map[string]any{"uri": uri}, false); rpcErrorCode(out) != mcp.INVALID_PARAMS {
			t.Errorf("skills/get %q = %v", uri, out)
		}
	}
	if got := admin.result(MethodSkillsGet, map[string]any{"uri": "skill://private/secret-sauce/SKILL.md"}); got["skill"] == nil {
		t.Fatalf("admin skills/get private = %v", got)
	}

	// 2026-07-28: resultType and the list-caching attributes; errors carry
	// HTTP 400.
	m := newSkillsMCPClient(t, f.gw, "ag_member", true, false)
	m.header = modernHeaders(MethodSkillsList, "")
	ml := m.result(MethodSkillsList, modernParams(nil))
	if ml["resultType"] != "complete" || ml["cacheScope"] != "private" || ml["ttlMs"] == nil || len(ml["skills"].([]any)) != 2 {
		t.Fatalf("2026-07-28 skills/list = %v", ml)
	}
	m.header = modernHeaders(MethodSkillsGet, "")
	if mg := m.result(MethodSkillsGet, modernParams(map[string]any{"uri": "skill://team/review/SKILL.md"})); mg["resultType"] != "complete" || mg["skill"] == nil {
		t.Fatalf("2026-07-28 skills/get = %v", mg)
	}
	out := m.send(MethodSkillsGet, modernParams(map[string]any{"uri": "skill://team/nope/SKILL.md"}), false)
	if m.status != http.StatusBadRequest || rpcErrorCode(out) != mcp.INVALID_PARAMS {
		t.Fatalf("2026-07-28 unknown skill = %d %v", m.status, out)
	}
	// The 2026-07-28 header rules hold here too.
	m.header = modernHeaders(MethodSkillsGet, "")
	if out := m.send(MethodSkillsList, modernParams(nil), false); m.status != http.StatusBadRequest || out["error"] == nil {
		t.Fatalf("mismatched Mcp-Method = %d %v", m.status, out)
	}
}

func TestSkillResourcesReadAndDirectory(t *testing.T) {
	f := newSkillsFixture(t)
	f.gw.MCPServer().AddResource(mcp.NewResource("test://plain", "plain"),
		func(context.Context, mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
			return []mcp.ResourceContents{mcp.TextResourceContents{URI: "test://plain", Text: "plain"}}, nil
		})
	c := newSkillsMCPClient(t, f.gw, "ag_member", false, false)
	c.initialize()

	read := c.result(string(mcp.MethodResourcesRead), map[string]any{"uri": "skill://team/ops/deploy/references/checklist.md"})
	contents, _ := read["contents"].([]any)
	if len(contents) != 1 {
		t.Fatalf("resources/read = %v", read)
	}
	first := contents[0].(map[string]any)
	if first["uri"] != "skill://team/ops/deploy/references/checklist.md" || first["mimeType"] != "text/markdown" || first["text"] != "1. build\n2. ship\n3. verify\n" {
		t.Fatalf("resources/read contents = %v", first)
	}
	if !auditHas(t, f.aud, EventSkillRead, string(mcp.MethodResourcesRead), "ag_member", "acme/playbooks@0123456789ab") {
		t.Fatal("resources/read wrote no audit row with the version")
	}
	if sh := c.result(string(mcp.MethodResourcesRead), map[string]any{"uri": "skill://team/ops/deploy/scripts/rollback.sh"}); sh["contents"].([]any)[0].(map[string]any)["mimeType"] != "text/x-shellscript" {
		t.Fatalf("a script's mimeType = %v", sh)
	}
	// A missing or invisible skill file on 2025-11-25 is -32002, as mcp-go
	// answers for any other resource (skills/get keeps -32602).
	for _, uri := range []string{"skill://team/ops/deploy/nope.md", "skill://private/secret-sauce/SKILL.md"} {
		if out := c.send(string(mcp.MethodResourcesRead), map[string]any{"uri": uri}, false); rpcErrorCode(out) != mcp.RESOURCE_NOT_FOUND {
			t.Errorf("resources/read %q = %v", uri, out)
		}
	}
	// Other resources keep mcp-go's own handling.
	if plain := c.result(string(mcp.MethodResourcesRead), map[string]any{"uri": "test://plain"}); plain["contents"].([]any)[0].(map[string]any)["text"] != "plain" {
		t.Fatalf("a plain resource = %v", plain)
	}
	templates := c.result(string(mcp.MethodResourcesTemplatesList), map[string]any{})
	foundTemplate := false
	for _, tpl := range templates["resourceTemplates"].([]any) {
		if tpl.(map[string]any)["uriTemplate"] == "skill://{+path}" {
			foundTemplate = true
		}
	}
	if !foundTemplate {
		t.Fatalf("resources/templates/list = %v", templates)
	}

	dir := c.result(MethodResourcesDirectoryRead, map[string]any{"uri": "skill://team/ops/deploy"})
	children, _ := dir["resources"].([]any)
	want := []struct{ uri, name, mime string }{
		{"skill://team/ops/deploy/SKILL.md", "SKILL.md", "text/markdown"},
		{"skill://team/ops/deploy/references", "references", mimeDirectory},
		{"skill://team/ops/deploy/scripts", "scripts", mimeDirectory},
	}
	if len(children) != len(want) {
		t.Fatalf("resources/directory/read = %v", dir)
	}
	for i, w := range want {
		got := children[i].(map[string]any)
		if got["uri"] != w.uri || got["name"] != w.name || got["mimeType"] != w.mime {
			t.Errorf("child %d = %v, want %+v", i, got, w)
		}
	}
	if size := children[0].(map[string]any)["size"]; size == nil {
		t.Errorf("a file child has no size: %v", children[0])
	}
	sub := c.result(MethodResourcesDirectoryRead, map[string]any{"uri": "skill://team/ops/deploy/references"})
	if len(sub["resources"].([]any)) != 2 {
		t.Fatalf("subdirectory = %v", sub)
	}
	if !auditHas(t, f.aud, EventSkillRead, MethodResourcesDirectoryRead, "ag_member", "skill://team/ops/deploy acme/playbooks@0123456789ab") {
		t.Fatal("resources/directory/read wrote no audit row with the version")
	}
	for _, uri := range []string{"skill://team/ops/deploy/SKILL.md", "skill://team/nope", "skill://private/secret-sauce", "test://plain", ""} {
		if out := c.send(MethodResourcesDirectoryRead, map[string]any{"uri": uri}, false); rpcErrorCode(out) != mcp.INVALID_PARAMS {
			t.Errorf("resources/directory/read %q = %v", uri, out)
		}
	}

	// 2026-07-28: resources/read through mcp-go with Mcp-Name, directory
	// reads through MCPSkills.
	m := newSkillsMCPClient(t, f.gw, "ag_member", true, false)
	uri := "skill://team/review/SKILL.md"
	m.header = modernHeaders(string(mcp.MethodResourcesRead), uri)
	mr := m.result(string(mcp.MethodResourcesRead), modernParams(map[string]any{"uri": uri}))
	if mr["resultType"] != "complete" || !strings.Contains(asJSON(mr["contents"]), "Read the diff.") {
		t.Fatalf("2026-07-28 resources/read = %v", mr)
	}
	m.header = modernHeaders(MethodResourcesDirectoryRead, "")
	if md := m.result(MethodResourcesDirectoryRead, modernParams(map[string]any{"uri": "skill://team/review"})); md["resultType"] != "complete" || len(md["resources"].([]any)) != 1 {
		t.Fatalf("2026-07-28 directory read = %v", md)
	}

	// A version change is announced (best effort); with no session to
	// hear it nothing breaks.
	f.reader.changed("team")
}

func TestMCPSkillsPassesThroughWhenOffAndChecksHost(t *testing.T) {
	off := newAccessFixture(t, nil)
	c := newSkillsMCPClient(t, off.gw, memberID, false, false)
	c.initialize()
	if out := c.send(MethodSkillsList, map[string]any{}, false); rpcErrorCode(out) != mcp.METHOD_NOT_FOUND {
		t.Fatalf("skills/list with the hub off = %v", out)
	}
	if tools := c.result("tools/list", map[string]any{}); tools["tools"] == nil {
		t.Fatalf("tools/list through MCPSkills = %v", tools)
	}

	f := newSkillsFixture(t)
	guarded := newSkillsMCPClient(t, f.gw, "ag_member", true, true)
	guarded.host = "evil.example"
	guarded.send(MethodSkillsList, map[string]any{}, false)
	if guarded.status != http.StatusForbidden {
		t.Fatalf("rebinding skills/list = %d", guarded.status)
	}
	open := newSkillsMCPClient(t, f.gw, "ag_member", true, false)
	open.host = "evil.example"
	if out := open.send(MethodSkillsList, map[string]any{}, false); open.status != http.StatusOK || out["result"] == nil {
		t.Fatalf("skills/list with the protection off = %d %v", open.status, out)
	}
}
