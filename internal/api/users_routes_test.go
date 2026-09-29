package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/identity"
)

type usersResponse struct {
	Users  []map[string]any `json:"users"`
	Groups []groupInfo      `json:"groups"`
}

func (e *accessTestEnv) listUsers(t *testing.T, cookie *http.Cookie) usersResponse {
	t.Helper()
	rec := e.do(t, cookie, http.MethodGet, "/v1/users", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/users: %d %s", rec.Code, rec.Body.String())
	}
	var out usersResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

func groupNames(gs []groupInfo) map[string]groupInfo {
	out := map[string]groupInfo{}
	for _, g := range gs {
		out[g.Name] = g
	}
	return out
}

func TestUsersListAdminOnly(t *testing.T) {
	e := newAccessTestServer(t)
	e.seedServer(t, "github", "connected", true)
	e.seedServer(t, "jira", "", false)
	// An upstream that shares a name with a built-in group is listed once,
	// as a server.
	e.seedServer(t, "notes", "connected", true)
	admin := e.cookieFor(t, e.admin.ID)
	m := e.member(t, "user_m", "m@beknown.work")
	if _, _, err := e.id.CreateAgentWithToken(t.Context(), m.ID, "bot"); err != nil {
		t.Fatal(err)
	}
	if err := e.access.SetGroups(t.Context(), m.ID, []string{"github", "memory"}, e.admin.ID); err != nil {
		t.Fatal(err)
	}

	out := e.listUsers(t, admin)
	if len(out.Users) != 2 {
		t.Fatalf("users = %v", out.Users)
	}
	a, mem := out.Users[0], out.Users[1]
	if a["id"] != e.admin.ID || a["role"] != identity.RoleAdmin || a["auth"] != identity.AuthPassword || a["agent_count"] != float64(0) {
		t.Errorf("admin row = %v", a)
	}
	if mem["id"] != m.ID || mem["role"] != identity.RoleMember || mem["auth"] != identity.AuthClerk || mem["agent_count"] != float64(1) {
		t.Errorf("member row = %v", mem)
	}
	if servers, _ := mem["servers"].([]any); len(servers) != 2 || servers[0] != "github" || servers[1] != "memory" {
		t.Errorf("member servers = %v", mem["servers"])
	}
	if servers, _ := a["servers"].([]any); servers == nil || len(servers) != 0 {
		t.Errorf("admin servers = %v (want [])", a["servers"])
	}

	gs := groupNames(out.Groups)
	if g := gs["github"]; g.Kind != "server" || g.Status != "connected" || g.ToolCount != 0 {
		t.Errorf("github group = %+v", g)
	}
	if g := gs["jira"]; g.Kind != "server" || g.Status != "disabled" {
		t.Errorf("jira group = %+v", g)
	}
	if g := gs["notes"]; g.Kind != "server" {
		t.Errorf("notes group = %+v, want kind server", g)
	}
	// Built-in memory tools are registered on the test gateway.
	if g := gs["memory"]; g.Kind != "builtin" || g.ToolCount == 0 || g.Status != "ok" {
		t.Errorf("memory group = %+v", g)
	}
	// Always-on groups never appear as grantable.
	for _, name := range []string{"tools", "inbox", "session"} {
		if _, ok := gs[name]; ok {
			t.Errorf("always-on group %q listed", name)
		}
	}
	// Sorted by name.
	for i := 1; i < len(out.Groups); i++ {
		if out.Groups[i-1].Name >= out.Groups[i].Name {
			t.Errorf("groups not sorted: %v", out.Groups)
			break
		}
	}

	// Members and anonymous callers are refused.
	if rec := e.do(t, e.cookieFor(t, m.ID), http.MethodGet, "/v1/users", ""); rec.Code != http.StatusForbidden || decodeJSON(t, rec)["error"] != "admin_only" {
		t.Errorf("member GET /v1/users: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do(t, nil, http.MethodGet, "/v1/users", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("anon GET /v1/users: %d", rec.Code)
	}
}

func TestUsersPatch(t *testing.T) {
	e := newAccessTestServer(t)
	e.seedServer(t, "github", "connected", true)
	admin := e.cookieFor(t, e.admin.ID)
	m := e.member(t, "user_m", "m@beknown.work")
	path := "/v1/users/" + m.ID

	// Servers: unknown group -> 400, nothing written.
	rec := e.do(t, admin, http.MethodPatch, path, `{"servers":["github","nope"]}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("unknown group: %d %s", rec.Code, rec.Body.String())
	}
	if g, _ := e.access.Groups(t.Context(), m.ID); len(g) != 0 {
		t.Errorf("grants written despite 400: %v", g)
	}

	// Servers: a server, a built-in without tools today (lake) and an
	// always-on name (dropped silently).
	rec = e.do(t, admin, http.MethodPatch, path, `{"servers":["github","lake","tools"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("set servers: %d %s", rec.Code, rec.Body.String())
	}
	got := decodeJSON(t, rec)
	if servers, _ := got["servers"].([]any); len(servers) != 2 || servers[0] != "github" || servers[1] != "lake" {
		t.Errorf("servers after patch = %v", got["servers"])
	}
	if got["agent_count"] != float64(0) || got["role"] != identity.RoleMember {
		t.Errorf("patched view = %v", got)
	}
	if e.auditRows(t, "user.servers", "") != 1 {
		t.Error("user.servers not audited")
	}
	// The gateway sees the grant immediately (cache invalidated).
	if sc := e.access.UserScope(t.Context(), m.ID); !sc.Allows("github") || sc.Allows("memory") {
		t.Errorf("scope after grant = %+v", sc)
	}

	// Role: bad value 400; promote 200 + audit.
	if rec := e.do(t, admin, http.MethodPatch, path, `{"role":"owner"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("bad role: %d", rec.Code)
	}
	rec = e.do(t, admin, http.MethodPatch, path, `{"role":"admin"}`)
	if rec.Code != http.StatusOK || decodeJSON(t, rec)["role"] != identity.RoleAdmin {
		t.Errorf("promote: %d %s", rec.Code, rec.Body.String())
	}
	if e.auditRows(t, "user.role", "") != 1 {
		t.Error("user.role not audited")
	}
	// Two admins now: the second may demote the first...
	mCookie := e.cookieFor(t, m.ID)
	rec = e.do(t, mCookie, http.MethodPatch, "/v1/users/"+e.admin.ID, `{"role":"member"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("demote original admin: %d %s", rec.Code, rec.Body.String())
	}
	// ...but not themselves (409), and not the last admin (409 last_admin).
	rec = e.do(t, mCookie, http.MethodPatch, path, `{"role":"member"}`)
	if rec.Code != http.StatusConflict {
		t.Errorf("self demote: %d %s", rec.Code, rec.Body.String())
	}
	rec = e.do(t, mCookie, http.MethodPatch, path, `{"status":"blocked"}`)
	if rec.Code != http.StatusConflict {
		t.Errorf("self block: %d %s", rec.Code, rec.Body.String())
	}
	// A no-op on own role/status is not a change and is allowed.
	if rec := e.do(t, mCookie, http.MethodPatch, path, `{"role":"admin","servers":[]}`); rec.Code != http.StatusOK {
		t.Errorf("self no-op patch: %d %s", rec.Code, rec.Body.String())
	}
	// Re-promote the original admin, then try to block m from the
	// original admin: fine (two admins). Then last-admin protection.
	if rec := e.do(t, mCookie, http.MethodPatch, "/v1/users/"+e.admin.ID, `{"role":"admin"}`); rec.Code != http.StatusOK {
		t.Fatalf("re-promote: %d", rec.Code)
	}
	rec = e.do(t, admin, http.MethodPatch, path, `{"status":"blocked","blocked_reason":"contract ended"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("block m: %d %s", rec.Code, rec.Body.String())
	}
	got = decodeJSON(t, rec)
	if got["status"] != identity.StatusBlocked || got["blocked_reason"] != "contract ended" {
		t.Errorf("blocked view = %v", got)
	}
	if e.auditRows(t, "user.status", "contract ended") != 1 {
		t.Error("user.status not audited with reason")
	}
	// m's cookie is dead; m's scope is denied.
	if rec := e.do(t, mCookie, http.MethodGet, "/v1/auth/me", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("blocked admin cookie: %d", rec.Code)
	}
	if sc := e.access.UserScope(t.Context(), m.ID); !sc.Denied {
		t.Errorf("blocked scope = %+v", sc)
	}
	// Through this route the last-admin rule can only trip when the acting
	// admin is the target (refused earlier as a self-change) or in a race
	// between two admins, so the 409 mapping is asserted on the helper.
	rec2 := newRecorder()
	e.srv.writeUserChangeError(rec2, identity.ErrLastAdmin)
	if rec2.Code != http.StatusConflict || decodeJSON(t, rec2)["error"] != "last_admin" {
		t.Errorf("ErrLastAdmin mapping: %d %s", rec2.Code, rec2.Body.String())
	}

	// Unknown user 404; empty body 400; method 405; revoke-sessions works.
	if rec := e.do(t, admin, http.MethodPatch, "/v1/users/u_missing", `{"role":"admin"}`); rec.Code != http.StatusNotFound {
		t.Errorf("unknown user: %d", rec.Code)
	}
	if rec := e.do(t, admin, http.MethodPatch, path, `{}`); rec.Code != http.StatusBadRequest {
		t.Errorf("empty patch: %d", rec.Code)
	}
	if rec := e.do(t, admin, http.MethodGet, path, ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET user: %d", rec.Code)
	}
}

func TestUsersRevokeSessions(t *testing.T) {
	e := newAccessTestServer(t)
	admin := e.cookieFor(t, e.admin.ID)
	m := e.member(t, "user_m", "m@beknown.work")
	mCookie := e.cookieFor(t, m.ID)
	if rec := e.do(t, mCookie, http.MethodGet, "/v1/auth/me", ""); rec.Code != http.StatusOK {
		t.Fatalf("member /me before revoke: %d", rec.Code)
	}
	rec := e.do(t, admin, http.MethodPost, "/v1/users/"+m.ID+"/revoke-sessions", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do(t, mCookie, http.MethodGet, "/v1/auth/me", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("member /me after revoke: %d", rec.Code)
	}
	if e.auditRows(t, "user.revoke_sessions", "") != 1 {
		t.Error("revoke not audited")
	}
	// Members can't revoke anyone.
	if rec := e.do(t, e.cookieFor(t, m.ID), http.MethodPost, "/v1/users/"+e.admin.ID+"/revoke-sessions", ""); rec.Code != http.StatusForbidden {
		t.Errorf("member revoke: %d", rec.Code)
	}
}

func TestMeServers(t *testing.T) {
	e := newAccessTestServer(t)
	e.seedServer(t, "github", "connected", true)
	e.seedServer(t, "jira", "connected", true)
	m := e.member(t, "user_m", "m@beknown.work")
	if err := e.access.SetGroups(t.Context(), m.ID, []string{"jira", "memory"}, e.admin.ID); err != nil {
		t.Fatal(err)
	}

	var adminGroups []groupInfo
	rec := e.do(t, e.cookieFor(t, e.admin.ID), http.MethodGet, "/v1/me/servers", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("admin: %d %s", rec.Code, rec.Body.String())
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &adminGroups)
	ag := groupNames(adminGroups)
	if _, ok := ag["github"]; !ok {
		t.Errorf("admin missing github: %v", adminGroups)
	}
	if _, ok := ag["memory"]; !ok {
		t.Errorf("admin missing memory: %v", adminGroups)
	}

	var memberGroups []groupInfo
	rec = e.do(t, e.cookieFor(t, m.ID), http.MethodGet, "/v1/me/servers", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("member: %d %s", rec.Code, rec.Body.String())
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &memberGroups)
	if len(memberGroups) != 2 || memberGroups[0].Name != "jira" || memberGroups[1].Name != "memory" {
		t.Errorf("member groups = %v", memberGroups)
	}
	if memberGroups[0].Kind != "server" || memberGroups[0].Status != "connected" || memberGroups[1].Kind != "builtin" {
		t.Errorf("member group details = %+v", memberGroups)
	}

	if rec := e.do(t, nil, http.MethodGet, "/v1/me/servers", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("anon: %d", rec.Code)
	}
	if rec := e.do(t, e.cookieFor(t, m.ID), http.MethodPost, "/v1/me/servers", `{}`); rec.Code != http.StatusForbidden {
		t.Errorf("member POST /v1/me/servers: %d, want 403 from the role guard", rec.Code)
	}
}
