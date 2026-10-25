package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/skillhub"
)

// Every SEP-2640 path passes the gate of the matching tool: the caller's
// scope (blocked callers, the agent's own deny list) and explicit deny
// policies. A refusal answers like an unknown URI and is audited.
func TestSkillsMCPPathsShareTheToolGate(t *testing.T) {
	f := newSkillsFixture(t)
	deployURI := "skill://team/ops/deploy/SKILL.md"
	refusedEverywhere := func(t *testing.T, agent string) {
		t.Helper()
		c := newSkillsMCPClient(t, f.gw, agent, false, false)
		c.initialize()
		for _, call := range []struct {
			method string
			params map[string]any
			code   int
		}{
			{MethodSkillsList, map[string]any{}, mcp.INVALID_PARAMS},
			{MethodSkillsGet, map[string]any{"uri": deployURI}, mcp.INVALID_PARAMS},
			{methodResourcesRead, map[string]any{"uri": deployURI}, mcp.RESOURCE_NOT_FOUND},
			{MethodResourcesDirectoryRead, map[string]any{"uri": "skill://team/ops/deploy"}, mcp.INVALID_PARAMS},
		} {
			if out := c.send(call.method, call.params, false); rpcErrorCode(out) != call.code || strings.Contains(asJSON(out), "Deploy") {
				t.Errorf("%s %s = %v, want error %d", agent, call.method, out, call.code)
			}
		}
		for _, method := range []string{MethodSkillsList, MethodSkillsGet, methodResourcesRead, MethodResourcesDirectoryRead} {
			if !auditHas(t, f.aud, EventSkillRead, method, agent, "refused") {
				t.Errorf("%s %s: no refused audit row", agent, method)
			}
		}
		// The in-process template path (stdio) refuses too.
		raw := f.gw.MCPServer().HandleMessage(WithAgentID(context.Background(), agent),
			json.RawMessage(`{"jsonrpc":"2.0","id":9,"method":"resources/read","params":{"uri":"`+deployURI+`"}}`))
		if out := asJSON(raw); !strings.Contains(out, `"error"`) || strings.Contains(out, "Deploy") {
			t.Errorf("%s template resources/read = %s", agent, out)
		}
	}
	t.Run("agent deny list", func(t *testing.T) { refusedEverywhere(t, "ag_limited") })
	t.Run("blocked caller", func(t *testing.T) { refusedEverywhere(t, "ag_blocked") })

	// A deny policy on one tool switches off exactly its paths.
	if _, err := f.pol.Set(context.Background(), "tool", SkillsGetTool, "deny", "no get", false); err != nil {
		t.Fatal(err)
	}
	c := newSkillsMCPClient(t, f.gw, "ag_member", false, false)
	c.initialize()
	if out := c.send(MethodSkillsGet, map[string]any{"uri": deployURI}, false); rpcErrorCode(out) != mcp.INVALID_PARAMS {
		t.Fatalf("skills/get under a deny policy = %v", out)
	}
	if list := c.result(MethodSkillsList, map[string]any{}); len(asList(list["skills"])) != 2 {
		t.Fatalf("skills/list must stay: %v", list)
	}
	if _, err := f.pol.Set(context.Background(), "tool", SkillsReadTool, "deny", "no reads", false); err != nil {
		t.Fatal(err)
	}
	if out := c.send(methodResourcesRead, map[string]any{"uri": deployURI}, false); rpcErrorCode(out) != mcp.RESOURCE_NOT_FOUND {
		t.Fatalf("resources/read under a deny policy = %v", out)
	}
	if out := c.send(MethodResourcesDirectoryRead, map[string]any{"uri": "skill://team/ops/deploy"}, false); rpcErrorCode(out) != mcp.INVALID_PARAMS {
		t.Fatalf("directory read under a deny policy = %v", out)
	}
	// tools.search's skills section follows skills.search's policy.
	if out := f.ok(t, f.member, MetaSearchTool, map[string]any{"query": "deploy"}); out["skills"] == nil {
		t.Fatalf("skills section missing before the deny: %v", out)
	}
	if _, err := f.pol.Set(context.Background(), "tool", SkillsSearchTool, "deny", "no search", false); err != nil {
		t.Fatal(err)
	}
	if out := f.ok(t, f.member, MetaSearchTool, map[string]any{"query": "deploy"}); out["skills"] != nil {
		t.Fatalf("skills section under a skills.search deny: %v", out["skills"])
	}

	// A bundle link issued before a deny no longer downloads.
	out := f.ok(t, f.member, SkillsBundleTool, map[string]any{"skills": []any{"deploy"}})
	ticket := strings.TrimPrefix(out["url"].(string), "https://toolyard.example"+SkillBundlePathPrefix)
	if _, err := f.pol.Set(context.Background(), "upstream", skillsUpstream, "deny", "skills off", false); err != nil {
		t.Fatal(err)
	}
	if _, err := f.gw.OpenSkillBundle(context.Background(), ticket); !errors.Is(err, skillhub.ErrNotFound) {
		t.Fatalf("download after a deny policy: %v", err)
	}
	if !auditHas(t, f.aud, EventSkillRead, bundleAuditTool, "ag_member", "refused") {
		t.Fatal("the refused download wrote no audit row")
	}
}

// The extension is declared only where its methods are served: on the
// answers MCPSkills fronts, never in-process or on stdio.
func TestSkillsExtensionOnlyWhereServed(t *testing.T) {
	f := newSkillsFixture(t)
	raw := f.gw.MCPServer().HandleMessage(f.member, json.RawMessage(
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"stdio-test","version":"1"}}}`))
	var out struct {
		Result struct {
			Capabilities struct {
				Resources  map[string]any `json:"resources"`
				Extensions map[string]any `json:"extensions"`
			} `json:"capabilities"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(asJSON(raw)), &out); err != nil {
		t.Fatal(err)
	}
	if out.Result.Capabilities.Extensions[SkillsExtensionID] != nil {
		t.Fatalf("in-process initialize declares the extension: %s", asJSON(raw))
	}
	if out.Result.Capabilities.Resources == nil {
		t.Fatalf("the resources capability is missing: %s", asJSON(raw))
	}
	// The template still serves resources/read there.
	read := f.gw.MCPServer().HandleMessage(f.member, json.RawMessage(
		`{"jsonrpc":"2.0","id":2,"method":"resources/read","params":{"uri":"skill://team/review/SKILL.md"}}`))
	if !strings.Contains(asJSON(read), "Read the diff.") {
		t.Fatalf("template resources/read = %s", asJSON(read))
	}
	missing := f.gw.MCPServer().HandleMessage(f.member, json.RawMessage(
		`{"jsonrpc":"2.0","id":3,"method":"resources/read","params":{"uri":"skill://team/nope/SKILL.md"}}`))
	if !strings.Contains(asJSON(missing), fmt.Sprint(mcp.RESOURCE_NOT_FOUND)) {
		t.Fatalf("template resources/read of a missing file on 2025-11-25 = %s", asJSON(missing))
	}
}

// skills.read returns at most 2,000 lines and 256 KiB per call; the MCP
// resources/read always returns the whole file.
func TestSkillsReadToolPagesLargeFiles(t *testing.T) {
	f := newSkillsFixture(t)
	uri := "skill://team/ops/deploy/references/deep/notes.md"
	var b strings.Builder
	for i := 0; i < 2500; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	f.reader.setContent(uri, b.String())
	first := f.ok(t, f.member, SkillsReadTool, map[string]any{"uri": uri})
	if lineCount(first["content"].(string)) != skillsReadMaxLines || first["truncated"] != true || first["next_offset"] != float64(skillsReadMaxLines) {
		t.Fatalf("first page: truncated=%v next=%v lines=%d", first["truncated"], first["next_offset"], lineCount(first["content"].(string)))
	}
	rest := f.ok(t, f.member, SkillsReadTool, map[string]any{"uri": uri, "offset": skillsReadMaxLines, "limit": 100000})
	if lineCount(rest["content"].(string)) != 500 || rest["truncated"] == true || rest["next_offset"] != nil {
		t.Fatalf("second page: %v %v", rest["truncated"], rest["next_offset"])
	}

	line := strings.Repeat("x", 1023) + "\n"
	f.reader.setContent(uri, strings.Repeat(line, 300))
	capped := f.ok(t, f.member, SkillsReadTool, map[string]any{"uri": uri})
	content := capped["content"].(string)
	if len(content) > skillsReadMaxBytes || capped["truncated"] != true || capped["next_offset"] != float64(lineCount(content)) || lineCount(content) != 256 {
		t.Fatalf("byte cap: %d bytes, %d lines, truncated=%v next=%v", len(content), lineCount(content), capped["truncated"], capped["next_offset"])
	}

	c := newSkillsMCPClient(t, f.gw, "ag_member", false, false)
	c.initialize()
	whole := c.result(methodResourcesRead, map[string]any{"uri": uri})
	if text := asList(whole["contents"])[0].(map[string]any)["text"].(string); len(text) != 300*1024 {
		t.Fatalf("resources/read returned %d bytes, want the whole file", len(text))
	}
}

// skills.bundle refuses at issue time what the archive could not hold:
// two different skills with one name, or more than 64 MiB of files.
func TestSkillsBundleChecksNamesAndSize(t *testing.T) {
	f := newSkillsFixture(t)
	if code := f.refused(t, f.admin, SkillsBundleTool, map[string]any{"skills": []any{"team/review", "private/review"}}); code != SkillsErrConflict {
		t.Fatalf("two skills named review: code %q", code)
	}
	f.reader.setSize("skill://team/ops/deploy/scripts/rollback.sh", skillsBundleMaxBytes+1)
	if code := f.refused(t, f.member, SkillsBundleTool, map[string]any{"skills": []any{"deploy"}}); code != SkillsErrLimit {
		t.Fatalf("a bundle over 64 MiB: code %q", code)
	}
	var n int
	if err := f.db.QueryRow(`SELECT count(*) FROM skill_bundle_tickets`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("refused bundles stored %d tickets (%v)", n, err)
	}
}

// A download audits every skill and its version as a list in the
// arguments, and the ticket never lands in plain text in the audit log.
func TestSkillsBundleAuditListsVersionsAndHidesTicket(t *testing.T) {
	f := newSkillsFixture(t)
	out := f.ok(t, f.member, SkillsBundleTool, map[string]any{"skills": []any{"deploy", "review"}})
	ticket := strings.TrimPrefix(out["url"].(string), "https://toolyard.example"+SkillBundlePathPrefix)
	if !validBundleTicket(ticket) {
		t.Fatalf("ticket %q", ticket)
	}
	b, err := f.gw.OpenSkillBundle(context.Background(), ticket)
	if err != nil {
		t.Fatal(err)
	}
	var buf strings.Builder
	if err := b.Write(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	var args string
	if err := f.db.QueryRow(`SELECT arguments FROM audit_events WHERE event_type = ? AND tool_name = ? AND decision = 'allow'`,
		EventSkillRead, bundleAuditTool).Scan(&args); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Skills []struct{ URI, Repo string } `json:"skills"`
	}
	if err := json.Unmarshal([]byte(args), &got); err != nil || len(got.Skills) != 2 ||
		got.Skills[1].URI != "skill://team/review/SKILL.md" || got.Skills[1].Repo != "acme/playbooks@0123456789ab" {
		t.Fatalf("bundle audit arguments %s %v", args, err)
	}
	var leaked int
	if err := f.db.QueryRow(`SELECT count(*) FROM audit_events WHERE result_summary LIKE ? OR arguments LIKE ? OR reason LIKE ?`,
		"%"+ticket+"%", "%"+ticket+"%", "%"+ticket+"%").Scan(&leaked); err != nil || leaked != 0 {
		t.Fatalf("the bundle ticket is in %d audit rows (%v)", leaked, err)
	}
	// Persisted results (approvals, jobs) keep the rest and drop the ticket.
	res := mcp.NewToolResultText(`{"url": "https://toolyard.example/v1/skills/bundle/` + ticket + `"}`)
	if enc := encodeJobResult(res); strings.Contains(enc, ticket) || !strings.Contains(enc, "/v1/skills/bundle/") {
		t.Fatalf("job result keeps the ticket: %s", enc)
	}
	if enc, err := encodeApprovalResult(res); err != nil || strings.Contains(enc, ticket) {
		t.Fatalf("approval result keeps the ticket: %s %v", enc, err)
	}
	if strings.Contains(audit.RedactString("see "+ticket), ticket) {
		t.Fatal("RedactString keeps the ticket")
	}
}

// Proposal errors map to what the agent can do about them.
func TestSkillsProposalErrorMapping(t *testing.T) {
	f := newSkillsFixture(t)
	change := map[string]any{"skill": "deploy", "title": "Fix a step", "files": map[string]any{"a.md": "x\n"}}
	for _, c := range []struct {
		err  error
		code string
		text string
	}{
		{skillhub.ErrChangeProposalsUnavailable, SkillsErrUnavailable, "not available"},
		{fmt.Errorf("%w: the owner of the skill source team has no usable toolyard GitHub connection", skillhub.ErrNoGitHubConnection), SkillsErrUnavailable, "has no usable toolyard GitHub connection"},
		{fmt.Errorf("%w: you already have 3 change proposals waiting; wait for a decision first", skillhub.ErrConflict), SkillsErrConflict, "you already have 3 change proposals waiting"},
		{fmt.Errorf("%w: files: at most 50 files in one proposal", skillhub.ErrLimit), SkillsErrLimit, "at most 50 files"},
		{fmt.Errorf("%w: title is required", skillhub.ErrInvalid), SkillsErrInvalid, "title is required"},
	} {
		f.props.mu.Lock()
		f.props.err = c.err
		f.props.mu.Unlock()
		res := f.call(t, f.member, SkillsProposeChangeTool, change)
		sc := structured(t, res)
		msg, _ := sc["message"].(string)
		if !res.IsError || sc["error"] != c.code || !strings.Contains(msg, c.text) || strings.HasPrefix(msg, "skillhub:") || strings.Contains(msg, "call again") {
			t.Errorf("%v: code %v message %q", c.err, sc["error"], msg)
		}
	}
}

// A 2026-07-28 resources/read of a skill file is answered by MCPSkills
// with resultType, cache hints and the file's text.
func TestSkillsModernResourcesRead(t *testing.T) {
	f := newSkillsFixture(t)
	m := newSkillsMCPClient(t, f.gw, "ag_member", true, false)
	uri := "skill://team/ops/deploy/references/checklist.md"
	m.header = modernHeaders(methodResourcesRead, uri)
	got := m.result(methodResourcesRead, modernParams(map[string]any{"uri": uri}))
	if got["resultType"] != "complete" || got["cacheScope"] != "private" || !strings.Contains(asJSON(got["contents"]), "3. verify") {
		t.Fatalf("modern resources/read = %v", got)
	}
	missing := "skill://team/ops/deploy/nope.md"
	m.header = modernHeaders(methodResourcesRead, missing)
	out := m.send(methodResourcesRead, modernParams(map[string]any{"uri": missing}), false)
	if m.status != http.StatusBadRequest || rpcErrorCode(out) != mcp.INVALID_PARAMS {
		t.Fatalf("modern missing file = %d %v", m.status, out)
	}
}
