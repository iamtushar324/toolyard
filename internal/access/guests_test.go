package access

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// addGuest inserts an active guest row with email.
func addGuest(t *testing.T, db *store.DB, id, email string) {
	t.Helper()
	now := time.Now().UnixMilli()
	if _, err := db.Exec(`INSERT INTO users(id, username, password_hash, role, status, created_at, updated_at, email, is_guest)
        VALUES(?,?,'','member','active',?,?,?,1)`, id, id, now, now, email); err != nil {
		t.Fatal(err)
	}
}

// TestGuestInviteNormalize: what an invite may hold. The built-in data
// groups and the skills hub are refused (shared organisation data), as are
// always-on and malformed names, a past expiry and a bad email.
func TestGuestInviteNormalize(t *testing.T) {
	now := time.Now()
	future := now.Add(24 * time.Hour).UnixMilli()
	cases := []struct {
		name    string
		in      GuestInvite
		wantErr bool
		check   func(t *testing.T, out GuestInvite)
	}{
		{name: "valid, cleaned", in: GuestInvite{Email: "  Ext@Partner.COM ", Servers: []string{"linear", "github", "github"}, Tools: []string{"github.get", " github.get", "linear.list"}, ExpiresAt: future, Note: " hi "},
			check: func(t *testing.T, out GuestInvite) {
				if out.Email != "ext@partner.com" || len(out.Servers) != 2 || out.Servers[0] != "github" || len(out.Tools) != 2 || out.Note != "hi" || out.ExpiresAt != future {
					t.Errorf("normalized = %+v", out)
				}
			}},
		{name: "no tools is every tool", in: GuestInvite{Email: "a@b.co", Servers: []string{"github"}},
			check: func(t *testing.T, out GuestInvite) {
				if out.Tools == nil || len(out.Tools) != 0 || out.ExpiresAt != 0 {
					t.Errorf("normalized = %+v", out)
				}
			}},
		{name: "memory refused", in: GuestInvite{Email: "a@b.co", Servers: []string{"github", "memory"}}, wantErr: true},
		{name: "events refused", in: GuestInvite{Email: "a@b.co", Servers: []string{"events"}}, wantErr: true},
		{name: "notes refused", in: GuestInvite{Email: "a@b.co", Servers: []string{"notes"}}, wantErr: true},
		{name: "skills refused", in: GuestInvite{Email: "a@b.co", Servers: []string{"skills"}}, wantErr: true},
		{name: "always-on refused", in: GuestInvite{Email: "a@b.co", Servers: []string{"tools"}}, wantErr: true},
		{name: "retired refused", in: GuestInvite{Email: "a@b.co", Servers: []string{"lake"}}, wantErr: true},
		{name: "bad server name", in: GuestInvite{Email: "a@b.co", Servers: []string{"git hub"}}, wantErr: true},
		{name: "no servers", in: GuestInvite{Email: "a@b.co"}, wantErr: true},
		{name: "bad tool name", in: GuestInvite{Email: "a@b.co", Servers: []string{"github"}, Tools: []string{"two words"}}, wantErr: true},
		{name: "past expiry", in: GuestInvite{Email: "a@b.co", Servers: []string{"github"}, ExpiresAt: now.Add(-time.Minute).UnixMilli()}, wantErr: true},
		{name: "negative expiry", in: GuestInvite{Email: "a@b.co", Servers: []string{"github"}, ExpiresAt: -1}, wantErr: true},
		{name: "not an email", in: GuestInvite{Email: "partner.com", Servers: []string{"github"}}, wantErr: true},
		{name: "display name form", in: GuestInvite{Email: "Ext <ext@partner.com>", Servers: []string{"github"}}, wantErr: true},
		{name: "non-ascii email", in: GuestInvite{Email: "Kate@partner.com", Servers: []string{"github"}}, wantErr: true},
		{name: "two at signs", in: GuestInvite{Email: "a@b@c.co", Servers: []string{"github"}}, wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := c.in.Normalize(now)
			if c.wantErr {
				if !errors.Is(err, ErrInvalidInvite) {
					t.Fatalf("err = %v, want ErrInvalidInvite", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if c.check != nil {
				c.check(t, out)
			}
		})
	}
}

// TestGuestScopeFollowsInvite: a guest reaches exactly their invite's
// servers, and with a tool list only those tools of them (the always-on
// groups stay); never a built-in data group or the skills hub, even when
// a stored row names one; nothing once the invite is removed or expired.
// The agent of a guest gets the same answer as the guest.
func TestGuestScopeFollowsInvite(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	addUser(t, db, "u_admin", "admin", "active")
	addGuest(t, db, "u_guest", "Ext@Partner.com")
	addAgent(t, db, "ag_guest", "u_guest")
	svc := New(db)

	// No invite yet: nothing, not even the always-on groups.
	if sc := svc.ScopeFor(ctx, "ag_guest"); !sc.Denied {
		t.Fatalf("guest without invite: %+v, want denied", sc)
	}
	if _, created, err := svc.PutGuestInvite(ctx, GuestInvite{Email: "ext@partner.com", Servers: []string{"github"}, Tools: []string{"github.get_issue"}}, "u_admin"); err != nil || !created {
		t.Fatalf("put invite: created=%v err=%v", created, err)
	}
	cases := []struct {
		upstream, tool string
		want           bool
	}{
		{"github", "github.get_issue", true},
		{"github", "github.delete_repo", false}, // same server, not on the list
		{"linear", "linear.list", false},        // not an invite server
		{"builtin", "tools.search", true},       // always on
		{"builtin", "inbox.request", true},      // always on
		{"builtin", "memory.get", false},        // shared data
		{"builtin", "events.list", false},
		{"skills", "skills.search", false}, // the organisation's skills
	}
	for _, caller := range []string{"ag_guest", "dashboard:u_guest"} {
		sc := svc.ScopeFor(ctx, caller)
		if !sc.Guest || sc.OwnerIsAdmin() || sc.CanAdminister() {
			t.Fatalf("%s scope = %+v, want a guest without admin reach", caller, sc)
		}
		for _, c := range cases {
			if got := sc.AllowsTool(c.upstream, c.tool); got != c.want {
				t.Errorf("%s AllowsTool(%s, %s) = %v, want %v", caller, c.upstream, c.tool, got, c.want)
			}
			if sc.LimitedOut(c.upstream, c.tool) {
				t.Errorf("%s LimitedOut(%s, %s): a guest's own list is not an agent limit", caller, c.upstream, c.tool)
			}
		}
		if got := sc.GuestToolList(); len(got) != 1 || got[0] != "github.get_issue" {
			t.Errorf("%s GuestToolList = %v", caller, got)
		}
	}

	// An agent limit narrows further, never widens past the guest list.
	if _, err := svc.SetAgentScope(ctx, "ag_guest", AgentScope{Mode: ScopeModeListed, AllowServers: []string{"github", "linear"}}, "u_guest"); err != nil {
		t.Fatal(err)
	}
	sc := svc.ScopeFor(ctx, "ag_guest")
	if !sc.AllowsTool("github", "github.get_issue") || sc.AllowsTool("github", "github.delete_repo") || sc.AllowsTool("linear", "linear.list") {
		t.Errorf("limited guest agent scope wrong: %+v", sc)
	}

	// No tool list: every tool of the servers.
	if _, created, err := svc.PutGuestInvite(ctx, GuestInvite{Email: "EXT@partner.com", Servers: []string{"github"}}, "u_admin"); err != nil || created {
		t.Fatalf("replace invite: created=%v err=%v", created, err)
	}
	sc = svc.ScopeFor(ctx, "dashboard:u_guest")
	if !sc.AllowsTool("github", "github.delete_repo") || sc.GuestTools != nil {
		t.Errorf("guest without a tool list: %+v", sc)
	}

	// A row naming a built-in group (written around the validation) still
	// grants nothing of it.
	if _, err := db.Exec(`UPDATE guest_invites SET servers = '["github","memory","notes"]' WHERE email = 'ext@partner.com'`); err != nil {
		t.Fatal(err)
	}
	svc.Invalidate("")
	sc = svc.ScopeFor(ctx, "dashboard:u_guest")
	if sc.Allows("memory") || sc.Allows("notes") || !sc.Allows("github") {
		t.Errorf("guest with a built-in group in the row: %+v", sc)
	}

	// Expired: denied.
	if _, err := db.Exec(`UPDATE guest_invites SET expires_at = ? WHERE email = 'ext@partner.com'`, time.Now().Add(-time.Second).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	svc.Invalidate("")
	if sc := svc.ScopeFor(ctx, "ag_guest"); !sc.Denied {
		t.Errorf("expired invite: %+v, want denied", sc)
	}

	// Removed: denied (DeleteGuestInvite drops the cache itself).
	if _, err := db.Exec(`UPDATE guest_invites SET expires_at = NULL`); err != nil {
		t.Fatal(err)
	}
	svc.Invalidate("")
	if sc := svc.ScopeFor(ctx, "ag_guest"); sc.Denied {
		t.Fatalf("restored invite: denied")
	}
	if ok, err := svc.DeleteGuestInvite(ctx, "ext@partner.com"); err != nil || !ok {
		t.Fatalf("delete: %v %v", ok, err)
	}
	if sc := svc.ScopeFor(ctx, "ag_guest"); !sc.Denied {
		t.Errorf("removed invite: %+v, want denied", sc)
	}
	if ok, err := svc.DeleteGuestInvite(ctx, "ext@partner.com"); err != nil || ok {
		t.Errorf("second delete: %v %v, want false", ok, err)
	}

	// A member's scope is unchanged by an invite for their address.
	addUser(t, db, "u_member", "member", "active")
	if _, err := db.Exec(`UPDATE users SET email = 'm@partner.com' WHERE id = 'u_member'`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.PutGuestInvite(ctx, GuestInvite{Email: "m@partner.com", Servers: []string{"github"}, Tools: []string{"github.get_issue"}}, "u_admin"); err != nil {
		t.Fatal(err)
	}
	msc := svc.ScopeFor(ctx, "dashboard:u_member")
	if msc.Guest || msc.GuestTools != nil || msc.Allows("github") || !msc.AllowsTool("skills", "skills.search") {
		t.Errorf("member with an invite for their address: %+v", msc)
	}
}

// TestGuestInviteStorage: list order, created vs updated, and that an
// update keeps who created the invite.
func TestGuestInviteStorage(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	addUser(t, db, "u_a", "admin", "active")
	addUser(t, db, "u_b", "admin", "active")
	svc := New(db)
	for _, e := range []string{"zed@x.io", "amy@x.io"} {
		if _, _, err := svc.PutGuestInvite(ctx, GuestInvite{Email: e, Servers: []string{"github"}}, "u_a"); err != nil {
			t.Fatal(err)
		}
	}
	got, created, err := svc.PutGuestInvite(ctx, GuestInvite{Email: "amy@x.io", Servers: []string{"linear"}, Note: "contractor"}, "u_b")
	if err != nil || created {
		t.Fatalf("update: created=%v err=%v", created, err)
	}
	if got.CreatedBy != "u_a" || got.UpdatedBy != "u_b" || got.Note != "contractor" || len(got.Servers) != 1 || got.Servers[0] != "linear" {
		t.Errorf("updated invite = %+v", got)
	}
	list, err := svc.ListGuestInvites(ctx)
	if err != nil || len(list) != 2 || list[0].Email != "amy@x.io" || list[1].Email != "zed@x.io" {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if inv, err := svc.GuestInvite(ctx, "not an email"); inv != nil || err != nil {
		t.Errorf("lookup of a malformed address = %v, %v; want nil, nil", inv, err)
	}
}
