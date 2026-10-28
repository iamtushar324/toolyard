package access

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestAgentLimitsNarrowTheOwnerScope(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	addUser(t, db, "u_admin", "admin", "active")
	addUser(t, db, "u_member", "member", "active")
	addAgent(t, db, "ag_admin", "u_admin")
	addAgent(t, db, "ag_plain", "u_admin")
	addAgent(t, db, "ag_member", "u_member")
	svc := New(db)
	if err := svc.SetGroups(ctx, "u_member", []string{"github", "memory"}, "u_admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetAgentScope(ctx, "ag_admin", AgentScope{
		Mode: ScopeModeListed, AllowServers: []string{"github"},
		AllowTools: []ToolRef{{Server: "linear", Tool: "linear.get_issue"}},
		DenyTools:  []ToolRef{{Server: "github", Tool: "github.delete_repo"}},
	}, "u_admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetAgentScope(ctx, "ag_member", AgentScope{Mode: ScopeModeAll, DenyServers: []string{"memory"}}, "u_member"); err != nil {
		t.Fatal(err)
	}

	admin := svc.ScopeFor(ctx, "ag_admin")
	if admin.All || !admin.OwnerAll || !admin.OwnerIsAdmin() || admin.Limits == nil {
		t.Fatalf("a limited admin agent must not have All: %+v", admin)
	}
	for _, c := range []struct {
		upstream, tool string
		want, limited  bool
	}{
		{"github", "github.create_issue", true, false},
		{"github", "github.delete_repo", false, true}, // denied by name
		{"linear", "linear.get_issue", true, false},   // allowed by name
		{"linear", "linear.create_issue", false, true},
		{"postgres", "postgres.query", false, true},
		{"builtin", "inbox.request", true, false}, // always-on stays
		{"builtin", "tools.search", true, false},
	} {
		if got := admin.AllowsTool(c.upstream, c.tool); got != c.want {
			t.Errorf("admin agent AllowsTool(%s) = %v, want %v", c.tool, got, c.want)
		}
		if got := admin.LimitedOut(c.upstream, c.tool); got != c.limited {
			t.Errorf("admin agent LimitedOut(%s) = %v, want %v", c.tool, got, c.limited)
		}
	}
	if !admin.Allows("linear") || admin.Allows("postgres") || !admin.Allows("inbox") {
		t.Error("group checks: linear has an allowed tool, postgres none, inbox is always on")
	}

	if plain := svc.ScopeFor(ctx, "ag_plain"); !plain.All || plain.Limits != nil {
		t.Fatalf("an agent without limits keeps its owner's scope unchanged: %+v", plain)
	}

	member := svc.ScopeFor(ctx, "ag_member")
	if member.AllowsTool("builtin", "memory.get") || !member.LimitedOut("builtin", "memory.get") {
		t.Error("a denied server must be refused as a limit")
	}
	if !member.AllowsTool("github", "github.create_issue") {
		t.Error("an allowed server of the member was refused")
	}
	// A server the owner was never granted is not a limit: it stays a plain
	// "not granted" (the agent may not even learn it exists).
	if member.LimitedOut("postgres", "postgres.query") || member.AllowsTool("postgres", "postgres.query") {
		t.Error("an ungranted server must not be reported as an agent limit")
	}
	if svc.ScopeFor(ctx, "dashboard:u_admin").Limits != nil {
		t.Error("a person's own scope never has agent limits")
	}
}

func TestAgentScopeStorageAndCache(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	addUser(t, db, "u_admin", "admin", "active")
	addAgent(t, db, "ag_1", "u_admin")
	svc := New(db)
	clock := time.Now()
	svc.now = func() time.Time { return clock }

	sc, err := svc.AgentScope(ctx, "ag_1")
	if err != nil || sc.Mode != ScopeModeAll || !sc.Unlimited() || sc.AllowServers == nil {
		t.Fatalf("default scope: %+v %v", sc, err)
	}
	if !svc.ScopeFor(ctx, "ag_1").All {
		t.Fatal("unlimited before any write")
	}
	saved, err := svc.SetAgentScope(ctx, "ag_1", AgentScope{Mode: " listed ", AllowServers: []string{"b", "a", "a"},
		DenyTools: []ToolRef{{Server: "a", Tool: "a.x"}, {Server: "a", Tool: "a.x"}}}, "u_admin")
	if err != nil {
		t.Fatal(err)
	}
	if saved.Mode != ScopeModeListed || len(saved.AllowServers) != 2 || saved.AllowServers[0] != "a" || len(saved.DenyTools) != 1 || saved.UpdatedBy != "u_admin" {
		t.Fatalf("normalized scope: %+v", saved)
	}
	read, err := svc.AgentScope(ctx, "ag_1")
	if err != nil || read.Mode != ScopeModeListed || len(read.AllowServers) != 2 || read.Summary() != "only 2 servers and 0 tools, except 0 denied servers and 1 denied tool" {
		t.Fatalf("read back: %+v %q %v", read, read.Summary(), err)
	}
	if svc.ScopeFor(ctx, "ag_1").All {
		t.Fatal("SetAgentScope did not drop the cached scope")
	}
	// A write that skips the service (another node) is seen once the cache
	// expires.
	if _, err := db.Exec(`DELETE FROM agent_scopes WHERE agent_id = 'ag_1'`); err != nil {
		t.Fatal(err)
	}
	if svc.ScopeFor(ctx, "ag_1").All {
		t.Fatal("the cache expired early")
	}
	clock = clock.Add(cacheTTL + time.Second)
	if !svc.ScopeFor(ctx, "ag_1").All {
		t.Fatal("the cache never expired")
	}
	// Writing the unlimited scope deletes the row.
	if _, err := svc.SetAgentScope(ctx, "ag_1", AgentScope{Mode: ScopeModeListed}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetAgentScope(ctx, "ag_1", AgentScope{}, ""); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM agent_scopes`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rows after an unlimited write: %d %v", n, err)
	}
	// Deleting the agent deletes its scope.
	if _, err := svc.SetAgentScope(ctx, "ag_1", AgentScope{Mode: ScopeModeAll, DenyServers: []string{"x"}}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM agents WHERE id = 'ag_1'`); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM agent_scopes`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("scope rows after the agent was deleted: %d %v", n, err)
	}
}

func TestAgentScopeValidation(t *testing.T) {
	for name, sc := range map[string]AgentScope{
		"mode":           {Mode: "some"},
		"server name":    {AllowServers: []string{"bad name"}},
		"tool server":    {DenyTools: []ToolRef{{Tool: "x.y"}}},
		"tool name":      {DenyTools: []ToolRef{{Server: "x", Tool: "has space"}}},
		"empty tool":     {AllowTools: []ToolRef{{Server: "x"}}},
		"too many lists": {DenyServers: make([]string, maxScopeEntries+1)},
	} {
		if _, err := sc.Normalize(); !errors.Is(err, ErrInvalidScope) {
			t.Errorf("%s: got %v, want ErrInvalidScope", name, err)
		}
	}
	db := openTestDB(t)
	if _, err := WriteAgentScope(context.Background(), db, "dashboard:u_1", AgentScope{Mode: ScopeModeListed}, ""); !errors.Is(err, ErrInvalidScope) {
		t.Errorf("a non-agent caller id: %v", err)
	}
	var nilLimits *Limits
	if !nilLimits.allowsTool("x", "x.y") || !nilLimits.allowsGroup("x") || nilLimits.Describe() != nil {
		t.Error("nil limits must allow everything")
	}
	d := AgentScope{Mode: ScopeModeListed, AllowTools: []ToolRef{{Server: "s", Tool: "s.b"}, {Server: "s", Tool: "s.a"}}}.Compile().Describe()
	if tools := d["allow_tools"].([]string); len(tools) != 2 || tools[0] != "s.a" || d["mode"] != ScopeModeListed {
		t.Errorf("Describe: %v", d)
	}
}

// TestOAuthClientsNeverInheritAdminControl: an agent with an OAuth grant
// has limits even when its scope is "everything", so it uses every tool its
// admin owner can but gets no admin control unless the grant says
// may_manage; an ordinary admin agent keeps both.
func TestOAuthClientsNeverInheritAdminControl(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	addUser(t, db, "u_admin", "admin", "active")
	addAgent(t, db, "ag_plain", "u_admin")
	addAgent(t, db, "ag_app", "u_admin")
	addAgent(t, db, "ag_managed", "u_admin")
	now := time.Now().UnixMilli()
	for _, g := range []struct {
		id, agent string
		manage    int
	}{{"app", "ag_app", 0}, {"managed", "ag_managed", 1}} {
		if _, err := db.Exec(`INSERT INTO mcp_oauth_clients(client_id, kind, redirect_uris, created_at, updated_at) VALUES(?, 'dcr', '[]', ?, ?)`,
			"mcpc_"+g.id, now, now); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO mcp_oauth_grants(id, client_id, user_id, agent_id, resource, created_at, updated_at, may_manage)
            VALUES(?, ?, 'u_admin', ?, 'r', ?, ?, ?)`, "grt_"+g.id, "mcpc_"+g.id, g.agent, now, now, g.manage); err != nil {
			t.Fatal(err)
		}
	}
	svc := New(db)
	plain, app, managed := svc.ScopeFor(ctx, "ag_plain"), svc.ScopeFor(ctx, "ag_app"), svc.ScopeFor(ctx, "ag_managed")
	if !plain.All || !plain.CanAdminister() {
		t.Fatalf("an admin's ordinary agent lost admin control: %+v", plain)
	}
	if app.All || app.CanAdminister() || !app.OwnerIsAdmin() || !app.AllowsTool("postgres", "postgres.query") || app.Limits == nil || !app.Limits.OAuthClient {
		t.Fatalf("an OAuth client without may_manage: %+v", app)
	}
	if !managed.CanAdminister() {
		t.Fatalf("an OAuth client with may_manage has no admin control: %+v", managed)
	}
	// A limited ordinary agent has no admin control either.
	if _, err := svc.SetAgentScope(ctx, "ag_plain", AgentScope{Mode: ScopeModeAll, DenyServers: []string{"x"}}, ""); err != nil {
		t.Fatal(err)
	}
	if svc.ScopeFor(ctx, "ag_plain").CanAdminister() {
		t.Fatal("a limited ordinary agent kept admin control")
	}
}

// TestAgentToolList: the tool list lives in the scope's rules JSON (no
// migration), is small for every agent by default (rows written before it
// existed included), keeps a row of its own when it is full, and never
// changes an access decision. Callers without an agent scope get the full
// list.
func TestAgentToolList(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	addUser(t, db, "u_admin", "admin", "active")
	addAgent(t, db, "ag_1", "u_admin")
	addAgent(t, db, "ag_old", "u_admin")
	svc := New(db)

	if got := svc.ToolListFor(ctx, "ag_1"); got != ToolListSmall {
		t.Fatalf("default tool list = %q, want small", got)
	}
	if sc, err := svc.AgentScope(ctx, "ag_1"); err != nil || sc.ToolList != ToolListSmall || sc.FullToolList() {
		t.Fatalf("default scope: %+v %v", sc, err)
	}
	// A row written before the tool list existed is the small list.
	if _, err := db.Exec(`INSERT INTO agent_scopes(agent_id, mode, rules, updated_at) VALUES('ag_old', 'all', '{"deny_servers":["x"]}', 1)`); err != nil {
		t.Fatal(err)
	}
	if got := svc.ToolListFor(ctx, "ag_old"); got != ToolListSmall {
		t.Fatalf("an older row's tool list = %q, want small", got)
	}

	// Full alone keeps a row, and the agent keeps exactly its owner's scope.
	saved, err := svc.SetAgentScope(ctx, "ag_1", AgentScope{ToolList: " full "}, "u_admin")
	if err != nil {
		t.Fatal(err)
	}
	if saved.ToolList != ToolListFull || saved.Mode != ScopeModeAll || saved.Summary() != "everything its owner can use, full tool list" {
		t.Fatalf("saved: %+v %q", saved, saved.Summary())
	}
	var rules string
	if err := db.QueryRow(`SELECT rules FROM agent_scopes WHERE agent_id = 'ag_1'`).Scan(&rules); err != nil || !strings.Contains(rules, `"tool_list":"full"`) {
		t.Fatalf("stored rules: %q %v", rules, err)
	}
	if read, err := svc.AgentScope(ctx, "ag_1"); err != nil || !read.FullToolList() {
		t.Fatalf("read back: %+v %v", read, err)
	}
	if got := svc.ToolListFor(ctx, "ag_1"); got != ToolListFull {
		t.Fatalf("tool list after the switch = %q, want full", got)
	}
	if sc := svc.ScopeFor(ctx, "ag_1"); !sc.All || sc.Limits != nil || !sc.CanAdminister() {
		t.Fatalf("the full tool list changed access: %+v", sc)
	}
	// With limits, the limits apply and the tool list stays.
	if _, err := svc.SetAgentScope(ctx, "ag_1", AgentScope{Mode: ScopeModeAll, DenyServers: []string{"x"}, ToolList: ToolListFull}, "u_admin"); err != nil {
		t.Fatal(err)
	}
	if sc := svc.ScopeFor(ctx, "ag_1"); sc.Limits == nil || sc.AllowsTool("x", "x.y") || svc.ToolListFor(ctx, "ag_1") != ToolListFull {
		t.Fatalf("limits with the full tool list: %+v", sc)
	}
	// Back to small with no limits deletes the row again.
	if _, err := svc.SetAgentScope(ctx, "ag_1", AgentScope{ToolList: ToolListSmall}, "u_admin"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM agent_scopes WHERE agent_id = 'ag_1'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rows after switching back: %d %v", n, err)
	}
	if got := svc.ToolListFor(ctx, "ag_1"); got != ToolListSmall {
		t.Fatalf("tool list after switching back = %q, want small", got)
	}

	for _, id := range []string{"dashboard:u_admin", "voice:u_admin"} {
		if got := svc.ToolListFor(ctx, id); got != ToolListFull {
			t.Errorf("ToolListFor(%q) = %q, want full", id, got)
		}
	}
	// A local unauthenticated MCP caller ("") has no scope to switch: it
	// gets the default, as every MCP client does.
	for _, id := range []string{"", "someone-else"} {
		if got := svc.ToolListFor(ctx, id); got != ToolListSmall {
			t.Errorf("ToolListFor(%q) = %q, want small", id, got)
		}
	}
	if _, err := (AgentScope{ToolList: "tiny"}).Normalize(); !errors.Is(err, ErrInvalidScope) {
		t.Errorf("an unknown tool list: %v", err)
	}
	if full := (AgentScope{ToolList: ToolListFull}); !full.Unlimited() || full.Compile() != nil {
		t.Error("the tool list must not count as a limit")
	}
}

// TestDisabledAgentsReachNothing: a disabled (revoked) agent is denied, so
// a call it queued before the revoke (an approval, an Inbox grant) cannot
// run when it is decided later.
func TestDisabledAgentsReachNothing(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	addUser(t, db, "u_admin", "admin", "active")
	addAgent(t, db, "ag_1", "u_admin")
	svc := New(db)
	if svc.ScopeFor(ctx, "ag_1").Denied {
		t.Fatal("an enabled agent is denied")
	}
	if _, err := db.Exec(`UPDATE agents SET disabled = 1 WHERE id = 'ag_1'`); err != nil {
		t.Fatal(err)
	}
	svc.InvalidateAgent("ag_1")
	if sc := svc.ScopeFor(ctx, "ag_1"); !sc.Denied || sc.AllowsTool("builtin", "tools.search") {
		t.Fatalf("a disabled agent still reaches tools: %+v", sc)
	}
	if owner, _ := svc.OwnerUser(ctx, "ag_1"); owner != "" {
		t.Fatalf("a disabled agent still has an owner: %q", owner)
	}
}
