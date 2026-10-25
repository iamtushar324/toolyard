package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/access"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/mcpauth"
	"github.com/tusharbhardwaj/toolyard/internal/mempalace"
)

// TestBearerRoutesHonourAgentToolDenies: the bearer REST twins of the
// notes and mempalace tools follow an agent's own per-tool denies, not
// only its owner's groups.
func TestBearerRoutesHonourAgentToolDenies(t *testing.T) {
	e := newAccessTestServer(t)
	ctx := t.Context()
	tok, agent := e.agentFor(t, e.admin.ID)
	note := `{"path":"admin/note.md","content":"# hi"}`
	ingest := `{"entry":"remember this","topic":"t","wing":"w"}`
	expectOK(t, "notes publish before the deny", e.bearer(t, tok, http.MethodPost, "/v1/notes/publish", note))
	if _, err := e.access.SetAgentScope(ctx, agent, access.AgentScope{Mode: access.ScopeModeAll,
		DenyTools: []access.ToolRef{{Server: notesGroup, Tool: "notes.publish"}}}, e.admin.ID); err != nil {
		t.Fatal(err)
	}
	expectNotGranted(t, "notes publish with notes.publish denied", e.bearer(t, tok, http.MethodPost, "/v1/notes/publish", note))
	expectNotGranted(t, "notes sync with notes.publish denied", e.bearer(t, tok, http.MethodPost, "/v1/notes/sync", ""))
	expectOK(t, "mempalace ingest is another tool", e.bearer(t, tok, http.MethodPost, "/v1/mempalace/ingest", ingest))
	if _, err := e.access.SetAgentScope(ctx, agent, access.AgentScope{Mode: access.ScopeModeAll,
		DenyTools: []access.ToolRef{{Server: mempalaceGroup, Tool: mempalace.DiaryWriteTool}}}, e.admin.ID); err != nil {
		t.Fatal(err)
	}
	expectNotGranted(t, "mempalace ingest with the diary tool denied", e.bearer(t, tok, http.MethodPost, "/v1/mempalace/ingest", ingest))
	got := expectOK(t, "hook with the diary tool denied", e.bearer(t, tok, http.MethodPost, "/v1/hooks/ingest", `{"source":"codex","event_name":"UserPromptSubmit","text":"x"}`))
	if got["memory_ingested"] != false {
		t.Errorf("a hook forwarded to a denied diary tool: %v", got)
	}
}

// TestOperatorTokensNeedOwnerScopeForAppAccess: changing a connected
// app's or agent's access, revoking an app, and the OAuth settings need an
// operator token's owner scope; reading stays read.
func TestOperatorTokensNeedOwnerScopeForAppAccess(t *testing.T) {
	for _, c := range []struct{ method, path, want string }{
		{http.MethodPost, "/v1/me/agent-scopes", identity.ScopeOwner},
		{http.MethodPost, "/v1/me/apps/scope", identity.ScopeOwner},
		{http.MethodPost, "/v1/me/apps/revoke", identity.ScopeOwner},
		{http.MethodGet, "/v1/me/apps", identity.ScopeRead},
		{http.MethodGet, "/v1/me/agent-scopes", identity.ScopeRead},
	} {
		if got, ok := operatorScopeFor(c.method, c.path); !ok || got != c.want {
			t.Errorf("%s %s = %q %v, want %q", c.method, c.path, got, ok, c.want)
		}
	}
	for _, k := range []string{mcpauth.SettingEnabled, mcpauth.SettingRedirectHosts, mcpauth.SettingAccessTTLMinutes,
		mcpauth.SettingRefreshTTLDays, mcpauth.SettingMaxSessionDays, mcpauth.SettingCustomSchemes} {
		if !ownerSettingKeys[k] {
			t.Errorf("setting %s does not need the owner scope", k)
		}
	}
}

func TestConsentCSPAllowsOnlyTheRedirectTarget(t *testing.T) {
	csp := consentCSP("cursor://anysphere.cursor-mcp/oauth/callback", "https://logo.example/x.png")
	for _, want := range []string{"form-action 'self' cursor:", "img-src 'self' data: https://img.clerk.com https://logo.example"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP %q lacks %q", csp, want)
		}
	}
	if strings.Contains(consentCSP("https://a.example/cb;script-src *", ""), "script-src *") {
		t.Error("a redirect URI injected a CSP directive")
	}
}
