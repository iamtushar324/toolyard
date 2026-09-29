package identity

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/store"
)

func clerkUser(t *testing.T, s *Service, id, email, owner string) *User {
	t.Helper()
	u, err := s.UpsertClerkUser(context.Background(), ClerkProfile{
		ClerkUserID: id, Email: email, DisplayName: "Name " + id, AvatarURL: "https://img.clerk.com/" + id,
	}, owner)
	if err != nil {
		t.Fatalf("UpsertClerkUser(%s): %v", id, err)
	}
	return u
}

func TestUserFieldsPasswordUser(t *testing.T) {
	s, uid := newTestIdentity(t)
	ctx := context.Background()
	for name, get := range map[string]func() (*User, error){
		"GetUserByID":  func() (*User, error) { return s.GetUserByID(ctx, uid) },
		"PrimaryUser":  func() (*User, error) { return s.PrimaryUser(ctx) },
		"Authenticate": func() (*User, error) { return s.Authenticate(ctx, "admin", "pw-correct-horse") },
	} {
		u, err := get()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if u.ID != uid || u.Username != "admin" || u.Role != RoleAdmin || u.Status != StatusActive ||
			u.Auth != AuthPassword || u.CreatedAt == 0 || u.Email != "" || u.ClerkUserID != "" {
			t.Errorf("%s: user = %+v", name, u)
		}
	}
}

func TestUpsertClerkUserCreatesMember(t *testing.T) {
	s, _ := newTestIdentity(t)
	ctx := context.Background()
	before := time.Now().UnixMilli()
	u := clerkUser(t, s, "user_ada", "Ada.Lovelace@beknown.work", "")
	if u.Username != "ada.lovelace" || u.Role != RoleMember || u.Status != StatusActive || u.Auth != AuthClerk ||
		u.Email != "Ada.Lovelace@beknown.work" || u.DisplayName != "Name user_ada" ||
		u.AvatarURL != "https://img.clerk.com/user_ada" || u.ClerkUserID != "user_ada" || u.LastSeenAt < before {
		t.Fatalf("new clerk user = %+v", u)
	}
	// Password login is impossible for a Clerk-only row, whatever the input.
	if _, err := s.Authenticate(ctx, u.Username, ""); !errors.Is(err, ErrInvalidLogin) {
		t.Errorf("Authenticate(clerk user, empty pw) = %v", err)
	}

	// Signing in again refreshes the profile and keeps the id.
	again, err := s.UpsertClerkUser(ctx, ClerkProfile{ClerkUserID: "user_ada", Email: "ada@beknown.work", DisplayName: "Ada L", AvatarURL: ""}, "")
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != u.ID || again.Email != "ada@beknown.work" || again.DisplayName != "Ada L" || again.AvatarURL != "" || again.LastSeenAt < u.LastSeenAt {
		t.Errorf("returning clerk user = %+v", again)
	}
	got, _ := s.GetUserByID(ctx, u.ID)
	if got.Auth != AuthClerk || got.Username != "ada.lovelace" {
		t.Errorf("GetUserByID after upsert = %+v", got)
	}
}

func TestUpsertClerkUserUsernames(t *testing.T) {
	s, _ := newTestIdentity(t) // "admin" already exists
	cases := []struct{ id, email, want string }{
		{"u1", "admin@beknown.work", "admin-2"},
		{"u2", "bob@a.example", "bob"},
		{"u3", "bob@b.example", "bob-2"},
		{"u4", "bob@c.example", "bob-3"},
		{"u5", "J!ne+Doe@x.example", "j-ne-doe"},
		{"u6", "", "user"},
		{"u7", "a@x.example", "user-2"},
		{"u8", "...@x.example", "user-3"},
	}
	for _, c := range cases {
		u := clerkUser(t, s, c.id, c.email, "")
		if u.Username != c.want {
			t.Errorf("%s (%q): username = %q, want %q", c.id, c.email, u.Username, c.want)
		}
	}
	long := clerkUser(t, s, "u9", "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyz@x.example", "")
	if len(long.Username) > 64 || !usernameRE.MatchString(long.Username) {
		t.Errorf("long username = %q", long.Username)
	}
	if _, err := s.UpsertClerkUser(context.Background(), ClerkProfile{}, ""); err == nil {
		t.Error("empty clerk user id accepted")
	}
}

func TestUpsertClerkUserLinksOwner(t *testing.T) {
	s, primary := newTestIdentity(t)
	ctx := context.Background()
	_, ag, err := s.CreateAgentWithToken(ctx, primary, "bot")
	if err != nil {
		t.Fatal(err)
	}

	// Someone else with the owner email but... no: a different email first,
	// to prove non-owners never attach to the primary row.
	other := clerkUser(t, s, "user_other", "other@beknown.work", "owner@beknown.work")
	if other.ID == primary || other.Role != RoleMember {
		t.Fatalf("non-owner attached to primary: %+v", other)
	}

	// The owner signs in: case-insensitive email match attaches to the
	// existing password admin, keeping id, role and agents.
	owner := clerkUser(t, s, "user_owner", "Owner@Beknown.work", "owner@beknown.work")
	if owner.ID != primary || owner.Role != RoleAdmin || owner.Auth != AuthClerk || owner.Username != "admin" ||
		owner.ClerkUserID != "user_owner" || owner.Email != "Owner@Beknown.work" {
		t.Fatalf("owner link = %+v (primary %s)", owner, primary)
	}
	agents, _ := s.ListAgents(ctx, primary)
	if len(agents) != 1 || agents[0].ID != ag.ID {
		t.Errorf("owner's agents after link = %+v", agents)
	}
	// The break-glass password still works for the linked owner.
	if u, err := s.Authenticate(ctx, "admin", "pw-correct-horse"); err != nil || u.ID != primary {
		t.Errorf("password login after link: %v, %v", u, err)
	}

	// Primary is already linked: a second Clerk identity with the owner
	// email becomes a plain member instead of hijacking the admin row.
	imposter := clerkUser(t, s, "user_imposter", "owner@beknown.work", "owner@beknown.work")
	if imposter.ID == primary || imposter.Role != RoleMember {
		t.Fatalf("second owner-email identity = %+v", imposter)
	}

	// No -owner-email configured: nobody links.
	s2, primary2 := newTestIdentity(t)
	u := clerkUser(t, s2, "user_x", "owner@beknown.work", "")
	if u.ID == primary2 {
		t.Errorf("linked without owner email")
	}
}

func TestUpsertClerkUserReturnsBlockedAsIs(t *testing.T) {
	s, _ := newTestIdentity(t)
	ctx := context.Background()
	u := clerkUser(t, s, "user_b", "b@beknown.work", "")
	if err := s.SetStatus(ctx, u.ID, StatusBlocked, "left_org"); err != nil {
		t.Fatal(err)
	}
	again := clerkUser(t, s, "user_b", "b@beknown.work", "")
	if again.Status != StatusBlocked || again.BlockedReason != "left_org" {
		t.Errorf("blocked user on sign-in = %+v", again)
	}
}

func TestListUsers(t *testing.T) {
	s, admin := newTestIdentity(t)
	ctx := context.Background()
	m := clerkUser(t, s, "user_m", "m@beknown.work", "")
	for i := 0; i < 2; i++ {
		if _, _, err := s.CreateAgentWithToken(ctx, m.ID, "bot"); err != nil {
			t.Fatal(err)
		}
	}
	users, err := s.ListUsers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 2 || users[0].ID != admin || users[1].ID != m.ID {
		t.Fatalf("users = %+v", users)
	}
	if users[0].AgentCount != 0 || users[1].AgentCount != 2 {
		t.Errorf("agent counts = %d, %d", users[0].AgentCount, users[1].AgentCount)
	}
	if users[1].Auth != AuthClerk || users[1].Role != RoleMember {
		t.Errorf("member row = %+v", users[1])
	}
}

func TestSetRole(t *testing.T) {
	s, admin := newTestIdentity(t)
	ctx := context.Background()
	m := clerkUser(t, s, "user_m", "m@beknown.work", "")

	if err := s.SetRole(ctx, admin, RoleMember); !errors.Is(err, ErrLastAdmin) {
		t.Errorf("demote last admin: %v, want ErrLastAdmin", err)
	}
	if err := s.SetRole(ctx, m.ID, "owner"); !errors.Is(err, ErrInvalidRole) {
		t.Errorf("bad role: %v", err)
	}
	if err := s.SetRole(ctx, "u_missing", RoleAdmin); !errors.Is(err, ErrNoUser) {
		t.Errorf("unknown user: %v", err)
	}
	if err := s.SetRole(ctx, m.ID, RoleAdmin); err != nil {
		t.Fatalf("promote: %v", err)
	}
	// Two active admins: demoting one is fine now.
	if err := s.SetRole(ctx, admin, RoleMember); err != nil {
		t.Errorf("demote with another admin present: %v", err)
	}
	u, _ := s.GetUserByID(ctx, admin)
	if u.Role != RoleMember {
		t.Errorf("role after demote = %q", u.Role)
	}
	// Now m is the last admin.
	if err := s.SetRole(ctx, m.ID, RoleMember); !errors.Is(err, ErrLastAdmin) {
		t.Errorf("demote new last admin: %v", err)
	}
}

func TestSetStatusBlocksRevokesAndProtectsLastAdmin(t *testing.T) {
	s, admin := newTestIdentity(t)
	ctx := context.Background()
	m := clerkUser(t, s, "user_m", "m@beknown.work", "")

	if err := s.SetStatus(ctx, admin, StatusBlocked, "x"); !errors.Is(err, ErrLastAdmin) {
		t.Errorf("block last admin: %v, want ErrLastAdmin", err)
	}
	if err := s.SetStatus(ctx, m.ID, "suspended", ""); !errors.Is(err, ErrInvalidStatus) {
		t.Errorf("bad status: %v", err)
	}
	if err := s.SetStatus(ctx, "u_missing", StatusBlocked, ""); !errors.Is(err, ErrNoUser) {
		t.Errorf("unknown user: %v", err)
	}

	sid, err := s.CreateSession(ctx, m.ID, "ua", "127.0.0.1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !s.IsSessionValid(ctx, sid) {
		t.Fatal("fresh session invalid")
	}
	if err := s.SetStatus(ctx, m.ID, StatusBlocked, "left_org"); err != nil {
		t.Fatalf("block member: %v", err)
	}
	if s.IsSessionValid(ctx, sid) {
		t.Error("session still valid after block")
	}
	u, _ := s.GetUserByID(ctx, m.ID)
	if u.Status != StatusBlocked || u.BlockedReason != "left_org" {
		t.Errorf("blocked user = %+v", u)
	}

	// Unblocking clears the reason but does not resurrect revoked sessions.
	if err := s.SetStatus(ctx, m.ID, StatusActive, "ignored"); err != nil {
		t.Fatal(err)
	}
	u, _ = s.GetUserByID(ctx, m.ID)
	if u.Status != StatusActive || u.BlockedReason != "" {
		t.Errorf("unblocked user = %+v", u)
	}
	if s.IsSessionValid(ctx, sid) {
		t.Error("revoked session came back after unblock")
	}

	// A blocked admin is not "active", so blocking them doesn't count
	// against the last-admin rule for the other one.
	if err := s.SetRole(ctx, m.ID, RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if err := s.SetStatus(ctx, admin, StatusBlocked, "rotate"); err != nil {
		t.Fatalf("block first admin with a second active admin: %v", err)
	}
	if err := s.SetStatus(ctx, m.ID, StatusBlocked, "x"); !errors.Is(err, ErrLastAdmin) {
		t.Errorf("block the only remaining active admin: %v", err)
	}
	// Blocked password users can't log in.
	if _, err := s.Authenticate(ctx, "admin", "pw-correct-horse"); !errors.Is(err, ErrUserBlocked) {
		t.Errorf("Authenticate(blocked) = %v, want ErrUserBlocked", err)
	}
	if _, err := s.Authenticate(ctx, "admin", "wrong-password-x"); !errors.Is(err, ErrInvalidLogin) {
		t.Errorf("Authenticate(blocked, wrong pw) = %v, want ErrInvalidLogin (no status leak)", err)
	}
}

func TestVerifyAgentTokenBlockedOwner(t *testing.T) {
	s, _ := newTestIdentity(t)
	ctx := context.Background()
	m := clerkUser(t, s, "user_m", "m@beknown.work", "")
	tok, _, err := s.CreateAgentWithToken(ctx, m.ID, "bot")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.VerifyAgentToken(ctx, tok); err != nil {
		t.Fatalf("active owner: %v", err)
	}
	if err := s.SetStatus(ctx, m.ID, StatusBlocked, "left_org"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.VerifyAgentToken(ctx, tok); !errors.Is(err, ErrAgentTokenInvalid) {
		t.Errorf("blocked owner's agent token accepted: %v", err)
	}
	if err := s.SetStatus(ctx, m.ID, StatusActive, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.VerifyAgentToken(ctx, tok); err != nil {
		t.Errorf("unblocked owner's agent token rejected: %v", err)
	}

	// Agents whose owner row doesn't exist keep working (other packages'
	// tests enrol agents under fake owners).
	tok2, _, err := s.CreateAgentWithToken(ctx, "u_ghost", "orphan")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.VerifyAgentToken(ctx, tok2); err != nil {
		t.Errorf("orphan agent token rejected: %v", err)
	}
}

// newEmptyIdentity is a store with no users at all: nobody ran the
// password setup before the first Clerk sign-in.
func newEmptyIdentity(t *testing.T) *Service {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "empty.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return New(db)
}

func TestUpsertClerkUserBootstrapsFirstAdmin(t *testing.T) {
	ctx := context.Background()
	upsert := func(t *testing.T, s *Service, id, email, orgRole, owner string) *User {
		t.Helper()
		u, err := s.UpsertClerkUser(ctx, ClerkProfile{ClerkUserID: id, Email: email, OrgRole: orgRole}, owner)
		if err != nil {
			t.Fatalf("UpsertClerkUser(%s): %v", id, err)
		}
		return u
	}

	t.Run("owner email on an empty store", func(t *testing.T) {
		s := newEmptyIdentity(t)
		u := upsert(t, s, "user_owner", "Owner@Beknown.work", "org:member", "owner@beknown.work")
		if u.Role != RoleAdmin || u.Status != StatusActive || u.Auth != AuthClerk {
			t.Fatalf("owner on empty store = %+v", u)
		}
		// With an active admin present, later org admins are members, and
		// a returning user never changes role.
		if u2 := upsert(t, s, "user_2", "two@beknown.work", "org:admin", "owner@beknown.work"); u2.Role != RoleMember {
			t.Errorf("org admin after bootstrap = %+v", u2)
		}
		if again := upsert(t, s, "user_2", "two@beknown.work", "org:admin", "owner@beknown.work"); again.Role != RoleMember {
			t.Errorf("returning user changed role: %+v", again)
		}
	})

	t.Run("org admin on an empty store without owner email", func(t *testing.T) {
		for _, role := range []string{"org:admin", "admin", " ORG:ADMIN "} {
			s := newEmptyIdentity(t)
			if u := upsert(t, s, "user_a", "a@beknown.work", role, ""); u.Role != RoleAdmin {
				t.Errorf("org role %q on empty store = %+v", role, u)
			}
		}
	})

	t.Run("plain member on an empty store stays a member", func(t *testing.T) {
		s := newEmptyIdentity(t)
		if u := upsert(t, s, "user_m", "m@beknown.work", "org:member", "owner@beknown.work"); u.Role != RoleMember {
			t.Errorf("plain member on empty store = %+v", u)
		}
		// The next org admin still bootstraps: there is no active admin yet.
		if u := upsert(t, s, "user_a", "a@beknown.work", "org:admin", ""); u.Role != RoleAdmin {
			t.Errorf("org admin after a member on empty store = %+v", u)
		}
	})

	t.Run("password admin present means no bootstrap", func(t *testing.T) {
		s, _ := newTestIdentity(t)
		if u := upsert(t, s, "user_a", "a@beknown.work", "org:admin", ""); u.Role != RoleMember {
			t.Errorf("org admin with a password admin present = %+v", u)
		}
	})
}
