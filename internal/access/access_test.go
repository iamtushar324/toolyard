package access

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/store"
)

func openTestDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "access.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
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
		{"ag_admin", "clickhouse", true},
		{"dashboard:u_admin", "lake", true},
		{"ag_member", "github", true},
		{"ag_member", "memory", true},
		{"ag_member", "clickhouse", false},
		{"ag_member", "lake", false},
		{"ag_member", "tools", true},
		{"ag_member", "inbox", true},
		{"ag_member", "session", true},
		{"voice:u_member", "github", true},
		{"dashboard:u_member", "clickhouse", false},
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
	if !svc.ScopeFor(ctx, "ag_member").Allows("clickhouse") {
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
		{"lake", "lake.query"}:        "lake",
		{"github", "github.search"}:   "github",
		{"tools", "tools.execute"}:    "tools",
	}
	for in, want := range cases {
		if got := GroupOf(in[0], in[1]); got != want {
			t.Errorf("GroupOf(%q, %q) = %q, want %q", in[0], in[1], got, want)
		}
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
