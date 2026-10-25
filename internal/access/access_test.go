package access

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/store"
	"github.com/tusharbhardwaj/toolyard/internal/store/storetest"
)

func openTestDB(t *testing.T) *store.DB {
	t.Helper()
	return storetest.Open(t)
}

func addUser(t *testing.T, db *store.DB, id, role, status string) {
	t.Helper()
	now := time.Now().UnixMilli()
	if _, err := db.Exec(`INSERT INTO users(id, username, password_hash, role, status, created_at, updated_at)
        VALUES(?,?,'',?,?,?,?)`, id, id, role, status, now, now); err != nil {
		t.Fatal(err)
	}
}

func addAgent(t *testing.T, db *store.DB, id, owner string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO agents(id, name, owner_user, token_hash, created_at) VALUES(?,?,?,'',?)`,
		id, id, owner, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
}

func TestScopeFor(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	addUser(t, db, "u_admin", "admin", "active")
	addUser(t, db, "u_member", "member", "active")
	addUser(t, db, "u_blocked", "member", "blocked")
	addAgent(t, db, "ag_admin", "u_admin")
	addAgent(t, db, "ag_member", "u_member")
	addAgent(t, db, "ag_blocked", "u_blocked")
	svc := New(db)
	if err := svc.SetGroups(ctx, "u_member", []string{"github", "memory", "github", "inbox"}, "u_admin"); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetGroups(ctx, "u_blocked", []string{"github"}, "u_admin"); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		caller, group string
		want          bool
	}{
		{"", "github", true}, // local unauthenticated MCP
		{"ag_admin", "postgres", true},
		{"dashboard:u_admin", "events", true},
		{"ag_member", "github", true},
		{"ag_member", "memory", true},
		{"ag_member", "postgres", false},
		{"ag_member", "events", false},
		{"ag_member", "tools", true},
		{"ag_member", "inbox", true},
		{"ag_member", "session", true},
		{"voice:u_member", "github", true},
		{"dashboard:u_member", "postgres", false},
		{"ag_blocked", "github", false},
		{"ag_blocked", "tools", false},
		{"ag_unknown", "tools", false},
		{"dashboard:u_nobody", "tools", false},
		{"internal:something", "tools", false},
	}
	for _, c := range cases {
		if got := svc.ScopeFor(ctx, c.caller).Allows(c.group); got != c.want {
			t.Errorf("ScopeFor(%q).Allows(%q) = %v, want %v", c.caller, c.group, got, c.want)
		}
	}

	got, err := svc.Groups(ctx, "u_member")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "github" || got[1] != "memory" {
		t.Fatalf("Groups = %v, want [github memory] (deduped, always-on dropped)", got)
	}
}

func TestSetGroupsInvalidatesCache(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	addUser(t, db, "u_member", "member", "active")
	addAgent(t, db, "ag_member", "u_member")
	svc := New(db)

	if svc.ScopeFor(ctx, "ag_member").Allows("github") {
		t.Fatal("member reaches github before any grant")
	}
	if err := svc.SetGroups(ctx, "u_member", []string{"github"}, ""); err != nil {
		t.Fatal(err)
	}
	if !svc.ScopeFor(ctx, "ag_member").Allows("github") {
		t.Fatal("grant not visible right after SetGroups")
	}
	if err := svc.SetGroups(ctx, "u_member", nil, ""); err != nil {
		t.Fatal(err)
	}
	if svc.ScopeFor(ctx, "ag_member").Allows("github") {
		t.Fatal("revoked grant still visible right after SetGroups")
	}

	// A role change made elsewhere shows after Invalidate.
	if _, err := db.Exec(`UPDATE users SET role = 'admin' WHERE id = 'u_member'`); err != nil {
		t.Fatal(err)
	}
	svc.Invalidate("u_member")
	if !svc.ScopeFor(ctx, "ag_member").Allows("postgres") {
		t.Fatal("promotion to admin not visible after Invalidate")
	}
}

func TestSetGroupsRejectsBadNames(t *testing.T) {
	db := openTestDB(t)
	addUser(t, db, "u_member", "member", "active")
	svc := New(db)
	for _, bad := range []string{"", "a.b", "has space", "x;drop"} {
		if err := svc.SetGroups(context.Background(), "u_member", []string{bad}, ""); err == nil {
			t.Errorf("SetGroups(%q) accepted a bad name", bad)
		}
	}
}

func TestGroupOf(t *testing.T) {
	cases := map[[2]string]string{
		{"builtin", "memory.get"}:     "memory",
		{"builtin", "skills.install"}: "skills",
		{"warehouse", "wh.query"}:     "warehouse",
		{"github", "github.search"}:   "github",
		{"tools", "tools.execute"}:    "tools",
	}
	for in, want := range cases {
		if got := GroupOf(in[0], in[1]); got != want {
			t.Errorf("GroupOf(%q, %q) = %q, want %q", in[0], in[1], got, want)
		}
	}
}

// A stored grant for a retired group (lake) grants nothing, is not listed,
// and a new one is dropped like an always-on name.
func TestRetiredGroupGrantsIgnored(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	addUser(t, db, "u_member", "member", "active")
	addAgent(t, db, "ag_member", "u_member")
	if _, err := db.Exec(`INSERT INTO user_server_access(user_id, server, created_at) VALUES('u_member','lake',0),('u_member','github',0)`); err != nil {
		t.Fatal(err)
	}
	svc := New(db)
	if got, err := svc.Groups(ctx, "u_member"); err != nil || len(got) != 1 || got[0] != "github" {
		t.Fatalf("Groups = %v %v, want [github]", got, err)
	}
	sc := svc.ScopeFor(ctx, "ag_member")
	if sc.Allows("lake") || !sc.Allows("github") {
		t.Fatalf("scope = %+v, want github only", sc)
	}
	if err := svc.SetGroups(ctx, "u_member", []string{"lake", "memory"}, ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := svc.Groups(ctx, "u_member"); len(got) != 1 || got[0] != "memory" {
		t.Fatalf("Groups after SetGroups = %v, want [memory]", got)
	}
	if IsBuiltinGroup("lake") {
		t.Error("lake is still a built-in group")
	}
}

// skills is an always-on group since the skills hub: every active caller
// has the skills tools without a grant (the hub filters what each one
// sees). A grant stored while skills was a built-in data group is
// harmless: not listed, dropped by the next SetGroups. An agent's own deny
// list still switches the group off for that agent.
func TestSkillsGroupIsAlwaysOn(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	addUser(t, db, "u_member", "member", "active")
	addUser(t, db, "u_blocked", "member", "blocked")
	addAgent(t, db, "ag_member", "u_member")
	addAgent(t, db, "ag_blocked", "u_blocked")
	if _, err := db.Exec(`INSERT INTO user_server_access(user_id, server, created_at) VALUES('u_member','skills',0),('u_member','github',0)`); err != nil {
		t.Fatal(err)
	}
	svc := New(db)
	if !AlwaysOn("skills") || IsBuiltinGroup("skills") {
		t.Fatalf("skills: always-on %v, built-in %v; want always-on only", AlwaysOn("skills"), IsBuiltinGroup("skills"))
	}
	found := false
	for _, g := range AlwaysOnGroups() {
		found = found || g == "skills"
	}
	if !found {
		t.Fatalf("AlwaysOnGroups = %v, want skills listed", AlwaysOnGroups())
	}
	if got, err := svc.Groups(ctx, "u_member"); err != nil || len(got) != 1 || got[0] != "github" {
		t.Fatalf("Groups = %v %v, want [github] (the stored skills grant is not listed)", got, err)
	}
	for _, caller := range []string{"ag_member", "dashboard:u_member", "voice:u_member"} {
		sc := svc.ScopeFor(ctx, caller)
		if !sc.Allows("skills") || !sc.AllowsTool("skills", "skills.search") {
			t.Errorf("%s: skills not allowed (%+v)", caller, sc)
		}
	}
	if svc.ScopeFor(ctx, "ag_blocked").Allows("skills") || svc.ScopeFor(ctx, "ag_unknown").Allows("skills") {
		t.Error("a blocked or unknown caller reaches skills")
	}
	if err := svc.SetGroups(ctx, "u_member", []string{"skills", "memory"}, ""); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM user_server_access WHERE user_id = 'u_member' AND server = 'skills'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("SetGroups kept a skills grant (%d rows, %v)", n, err)
	}
	// An agent's deny list beats an always-on group; an allow list that
	// does not name skills leaves it on.
	denied := Scope{Groups: map[string]bool{}}.withLimits(&Limits{Listed: true, DenyServers: map[string]bool{"skills": true}})
	if denied.Allows("skills") || denied.AllowsTool("skills", "skills.get") || !denied.LimitedOut("skills", "skills.get") {
		t.Fatalf("a denied agent still reaches skills: %+v", denied)
	}
	listed := Scope{Groups: map[string]bool{}}.withLimits(&Limits{Listed: true, AllowServers: map[string]bool{"github": true}})
	if !listed.AllowsTool("skills", "skills.get") {
		t.Fatal("an allow list without skills switched the always-on group off")
	}
}

func TestNewUserDefaults(t *testing.T) {
	// Rows inserted after 0023 without a role are members, active.
	db := openTestDB(t)
	now := time.Now().UnixMilli()
	if _, err := db.Exec(`INSERT INTO users(id, username, password_hash, created_at, updated_at) VALUES('u_new','new','',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	var role, status string
	if err := db.QueryRow(`SELECT role, status FROM users WHERE id='u_new'`).Scan(&role, &status); err != nil {
		t.Fatal(err)
	}
	if role != "member" || status != "active" {
		t.Fatalf("new row defaults = %s/%s, want member/active", role, status)
	}
}

// Concurrent replacements of one user's grants: the result is exactly one
// of the requested sets, never a mix, and no call fails.
func TestSetGroupsConcurrentLastWins(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	addUser(t, db, "u_member", "member", "active")
	svc := New(db)
	sets := [][]string{
		{"alpha", "beta"},
		{"beta", "gamma"},
		{"delta"},
		{"alpha", "delta", "gamma"}, // sorted, as Groups returns them
	}
	for round := 0; round < 5; round++ {
		errs := make([]error, len(sets))
		barrier := make(chan struct{})
		var wg sync.WaitGroup
		for i := range sets {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-barrier
				errs[i] = svc.SetGroups(ctx, "u_member", sets[i], "u_admin")
			}(i)
		}
		close(barrier)
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("round %d: SetGroups(%v): %v", round, sets[i], err)
			}
		}
		got, err := svc.Groups(ctx, "u_member")
		if err != nil {
			t.Fatal(err)
		}
		match := false
		for _, want := range sets {
			if fmt.Sprint(got) == fmt.Sprint(want) {
				match = true
			}
		}
		if !match {
			t.Fatalf("round %d: groups = %v, want exactly one of %v", round, got, sets)
		}
	}
}
