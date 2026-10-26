package identity

import (
	"context"
	"errors"
	"testing"
	"time"
)

// ownerID is the owner's id, "" when there is none.
func ownerID(t *testing.T, s *Service) string {
	t.Helper()
	u, err := s.Owner(context.Background())
	if errors.Is(err, ErrNoUser) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	if !u.Owner || u.Role != RoleAdmin || u.Status != StatusActive {
		t.Fatalf("owner row = %+v, want an active admin with Owner set", u)
	}
	return u.ID
}

func ownerCount(t *testing.T, s *Service) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM users WHERE is_owner = 1`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func upsertOrg(t *testing.T, s *Service, id, email, orgRole, owner string) *User {
	t.Helper()
	u, err := s.UpsertClerkUser(context.Background(), ClerkProfile{ClerkUserID: id, Email: email, OrgRole: orgRole}, owner)
	if err != nil {
		t.Fatalf("UpsertClerkUser(%s): %v", id, err)
	}
	return u
}

// The first account the password setup creates is the owner.
func TestCreateUserIsOwner(t *testing.T) {
	s, admin := newTestIdentity(t)
	ctx := context.Background()
	if got := ownerID(t, s); got != admin {
		t.Fatalf("owner = %q, want the setup account %q", got, admin)
	}
	u, err := s.GetUserByID(ctx, admin)
	if err != nil || !u.Owner {
		t.Fatalf("setup account = %+v %v, want Owner", u, err)
	}
	// Everyone after is a member, never the owner.
	m := clerkUser(t, s, "user_m", "m@beknown.work", "")
	if m.Owner || m.Role != RoleMember {
		t.Fatalf("later sign-in = %+v", m)
	}
	users, err := s.ListUsers(ctx)
	if err != nil || len(users) != 2 || !users[0].Owner || users[1].Owner {
		t.Fatalf("ListUsers owner flags = %+v %v", users, err)
	}
	if p, _ := s.OwnerOrPrimary(ctx); p == nil || p.ID != admin {
		t.Fatalf("OwnerOrPrimary = %+v", p)
	}
}

// Without an owner email, the first verified Clerk sign-in on an empty
// install becomes the owner and an admin, whatever their org role; later
// sign-ins are members.
func TestFirstClerkSignInBecomesOwner(t *testing.T) {
	s := newEmptyIdentity(t)
	first := upsertOrg(t, s, "user_first", "first@beknown.work", "org:member", "")
	if !first.Owner || first.Role != RoleAdmin {
		t.Fatalf("first sign-in = %+v, want owner and admin", first)
	}
	again := upsertOrg(t, s, "user_first", "first@beknown.work", "org:member", "")
	if again.ID != first.ID || !again.Owner {
		t.Fatalf("returning owner = %+v", again)
	}
	for _, role := range []string{"org:member", "org:admin"} {
		u := upsertOrg(t, s, "user_"+role, role[4:]+"@beknown.work", role, "")
		if u.Owner || u.Role != RoleMember {
			t.Errorf("later %s sign-in = %+v, want a member", role, u)
		}
	}
	if ownerCount(t, s) != 1 || ownerID(t, s) != first.ID {
		t.Fatal("not exactly one owner")
	}
	// The setup route is closed: CreateUser refuses once anyone exists.
	if _, err := s.CreateUser(context.Background(), "late", "long-enough-password"); !errors.Is(err, ErrUserExists) {
		t.Fatalf("CreateUser after a Clerk owner: %v", err)
	}
}

// With an owner email configured, only that person claims the owner role,
// on their first sign-in, whatever route the sign-in takes: a caller that
// passes no owner email (federation) still honours the configured one.
func TestConfiguredOwnerEmailClaimsOwner(t *testing.T) {
	t.Run("through the caller's owner email", func(t *testing.T) {
		s := newEmptyIdentity(t)
		m := upsertOrg(t, s, "user_m", "m@beknown.work", "org:member", "owner@beknown.work")
		if m.Owner || m.Role != RoleMember {
			t.Fatalf("plain member first = %+v", m)
		}
		a := upsertOrg(t, s, "user_a", "a@beknown.work", "org:admin", "owner@beknown.work")
		if a.Owner || a.Role != RoleAdmin {
			t.Fatalf("org admin first = %+v, want a bootstrap admin who is not the owner", a)
		}
		o := upsertOrg(t, s, "user_o", "Owner@Beknown.work", "org:member", "owner@beknown.work")
		if !o.Owner || o.Role != RoleAdmin {
			t.Fatalf("configured owner = %+v, want owner and admin", o)
		}
		if ownerCount(t, s) != 1 {
			t.Fatal("more than one owner")
		}
	})
	t.Run("through SetOwnerEmail", func(t *testing.T) {
		s := newEmptyIdentity(t)
		s.SetOwnerEmail(" owner@beknown.work ")
		m := upsertOrg(t, s, "user_m", "m@beknown.work", "org:member", "")
		if m.Owner || m.Role != RoleMember {
			t.Fatalf("someone else first = %+v, want a member", m)
		}
		o := upsertOrg(t, s, "user_o", "owner@beknown.work", "org:member", "")
		if !o.Owner || o.Role != RoleAdmin {
			t.Fatalf("configured owner without the caller's email = %+v", o)
		}
		// A second identity with the same address does not take it over.
		dup := upsertOrg(t, s, "user_dup", "owner@beknown.work", "org:member", "")
		if dup.Owner || dup.Role != RoleMember {
			t.Fatalf("second owner-email identity = %+v", dup)
		}
	})
}

// The configured owner signing in again on an install without an owner
// takes the vacant role; nobody else does, and a blocked one does not.
func TestReturningConfiguredOwnerClaimsVacantRole(t *testing.T) {
	s, admin := newTestIdentity(t)
	ctx := context.Background()
	o := clerkUser(t, s, "user_o", "o@beknown.work", "")
	other := clerkUser(t, s, "user_x", "x@beknown.work", "")
	clearOwner(t, s)
	if again := clerkUser(t, s, "user_x", "x@beknown.work", "o@beknown.work"); again.Owner {
		t.Fatalf("someone else claimed the vacant role: %+v", again)
	}
	if err := s.SetStatus(ctx, o.ID, StatusBlocked, "x"); err != nil {
		t.Fatal(err)
	}
	if again := clerkUser(t, s, "user_o", "o@beknown.work", "o@beknown.work"); again.Owner {
		t.Fatalf("blocked owner email claimed the role: %+v", again)
	}
	if err := s.SetStatus(ctx, o.ID, StatusActive, ""); err != nil {
		t.Fatal(err)
	}
	again := clerkUser(t, s, "user_o", "o@beknown.work", "o@beknown.work")
	if !again.Owner || again.Role != RoleAdmin {
		t.Fatalf("returning configured owner = %+v", again)
	}
	if ownerCount(t, s) != 1 || ownerID(t, s) == admin || ownerID(t, s) == other.ID {
		t.Fatal("wrong owner")
	}
}

// EnsureOwner gives an install from before 0072 its owner: the active user
// with the configured owner email, else the oldest active admin.
func TestEnsureOwner(t *testing.T) {
	ctx := context.Background()

	if _, err := newEmptyIdentity(t).EnsureOwner(ctx, "owner@beknown.work"); !errors.Is(err, ErrNoUser) {
		t.Fatalf("empty install: %v, want ErrNoUser", err)
	}

	t.Run("keeps an existing owner", func(t *testing.T) {
		s, admin := newTestIdentity(t)
		m := clerkUser(t, s, "user_m", "owner@beknown.work", "")
		u, err := s.EnsureOwner(ctx, "owner@beknown.work")
		if err != nil || u.ID != admin || m.Owner {
			t.Fatalf("EnsureOwner with an owner = %+v %v", u, err)
		}
	})

	t.Run("configured owner email first, made admin", func(t *testing.T) {
		s, admin := newTestIdentity(t)
		m := clerkUser(t, s, "user_m", "Owner@Beknown.work", "")
		clearOwner(t, s)
		u, err := s.EnsureOwner(ctx, "owner@beknown.work")
		if err != nil || u.ID != m.ID || !u.Owner || u.Role != RoleAdmin {
			t.Fatalf("EnsureOwner = %+v %v, want the configured owner %s", u, err, m.ID)
		}
		if a, _ := s.GetUserByID(ctx, admin); a.Owner || a.Role != RoleAdmin {
			t.Fatalf("setup account after = %+v", a)
		}
		// Idempotent.
		if u2, err := s.EnsureOwner(ctx, ""); err != nil || u2.ID != m.ID || ownerCount(t, s) != 1 {
			t.Fatalf("second EnsureOwner = %+v %v", u2, err)
		}
	})

	t.Run("an owner email nobody has leaves it vacant", func(t *testing.T) {
		s, _ := newTestIdentity(t)
		clerkUser(t, s, "user_2", "two@beknown.work", "")
		clearOwner(t, s)
		if _, err := s.EnsureOwner(ctx, "nobody@beknown.work"); !errors.Is(err, ErrNoUser) {
			t.Fatalf("EnsureOwner with an unknown owner email: %v, want ErrNoUser", err)
		}
		if ownerCount(t, s) != 0 {
			t.Fatal("the role went to someone else")
		}
	})

	t.Run("without an owner email, the oldest active admin", func(t *testing.T) {
		s, admin := newTestIdentity(t)
		time.Sleep(2 * time.Millisecond)
		second := clerkUser(t, s, "user_2", "two@beknown.work", "")
		if err := s.SetRole(ctx, second.ID, RoleAdmin); err != nil {
			t.Fatal(err)
		}
		clearOwner(t, s)
		u, err := s.EnsureOwner(ctx, "")
		if err != nil || u.ID != admin {
			t.Fatalf("EnsureOwner = %+v %v, want the oldest admin %s", u, err, admin)
		}
		if auditCount(t, s, "user.owner", ActorStartup) != 1 {
			t.Fatal("the startup claim is not audited")
		}
		// The oldest admin blocked: the next active admin.
		clearOwner(t, s)
		if err := s.SetStatus(ctx, admin, StatusBlocked, "x"); err != nil {
			t.Fatal(err)
		}
		if u, err := s.EnsureOwner(ctx, ""); err != nil || u.ID != second.ID {
			t.Fatalf("EnsureOwner with the oldest blocked = %+v %v", u, err)
		}
	})

	t.Run("no active admin leaves it vacant", func(t *testing.T) {
		s, admin := newTestIdentity(t)
		clearOwner(t, s)
		if _, err := s.db.Exec(`UPDATE users SET status = ? WHERE id = ?`, StatusBlocked, admin); err != nil {
			t.Fatal(err)
		}
		clerkUser(t, s, "user_m", "m@beknown.work", "")
		if _, err := s.EnsureOwner(ctx, ""); !errors.Is(err, ErrNoUser) {
			t.Fatalf("EnsureOwner without an active admin: %v", err)
		}
		if ownerCount(t, s) != 0 {
			t.Fatal("an owner appeared")
		}
		if p, err := s.OwnerOrPrimary(ctx); err != nil || p.ID != admin {
			t.Fatalf("OwnerOrPrimary without an owner = %+v %v, want the oldest account", p, err)
		}
	})
}

// The owner hands the role to another active user, who becomes an admin;
// the previous owner stays an admin and may then be demoted.
func TestTransferOwnership(t *testing.T) {
	s, admin := newTestIdentity(t)
	ctx := context.Background()
	m := clerkUser(t, s, "user_m", "m@beknown.work", "")
	x := clerkUser(t, s, "user_x", "x@beknown.work", "")

	if err := s.TransferOwnership(ctx, m.ID, x.ID); !errors.Is(err, ErrNotOwner) {
		t.Errorf("transfer by a non-owner: %v", err)
	}
	if err := s.TransferOwnership(ctx, admin, admin); !errors.Is(err, ErrAlreadyOwner) {
		t.Errorf("transfer to self: %v", err)
	}
	if err := s.TransferOwnership(ctx, admin, "u_missing"); !errors.Is(err, ErrNoUser) {
		t.Errorf("transfer to an unknown user: %v", err)
	}
	if err := s.SetStatus(ctx, x.ID, StatusBlocked, "x"); err != nil {
		t.Fatal(err)
	}
	if err := s.TransferOwnership(ctx, admin, x.ID); !errors.Is(err, ErrUserBlocked) {
		t.Errorf("transfer to a blocked user: %v", err)
	}
	if ownerID(t, s) != admin {
		t.Fatal("a refused transfer moved the role")
	}

	if err := s.TransferOwnership(ctx, admin, m.ID); err != nil {
		t.Fatalf("transfer: %v", err)
	}
	if ownerID(t, s) != m.ID || ownerCount(t, s) != 1 {
		t.Fatal("the role did not move to m")
	}
	prev, _ := s.GetUserByID(ctx, admin)
	if prev.Owner || prev.Role != RoleAdmin || prev.Status != StatusActive {
		t.Fatalf("previous owner = %+v, want a plain admin", prev)
	}
	// The new owner is protected; the previous one is not any more.
	if err := s.SetRole(ctx, m.ID, RoleMember); !errors.Is(err, ErrOwner) {
		t.Errorf("demote the new owner: %v", err)
	}
	if err := s.SetStatus(ctx, m.ID, StatusBlocked, "x"); !errors.Is(err, ErrOwner) {
		t.Errorf("block the new owner: %v", err)
	}
	if err := s.SetRole(ctx, m.ID, RoleAdmin); err != nil {
		t.Errorf("no-op promotion of the owner: %v", err)
	}
	if err := s.SetRole(ctx, admin, RoleMember); err != nil {
		t.Errorf("demote the previous owner: %v", err)
	}
	// The previous owner can no longer transfer.
	if err := s.TransferOwnership(ctx, admin, x.ID); !errors.Is(err, ErrNotOwner) {
		t.Errorf("transfer by the previous owner: %v", err)
	}
	if p, _ := s.OwnerOrPrimary(ctx); p == nil || p.ID != m.ID {
		t.Fatalf("OwnerOrPrimary after the transfer = %+v", p)
	}
	if p, _ := s.PrimaryUser(ctx); p == nil || p.ID != admin {
		t.Fatalf("PrimaryUser stays the oldest account: %+v", p)
	}
}

// OwnerSignInEmail: the address personal sign-in admits.
func TestOwnerSignInEmail(t *testing.T) {
	ctx := context.Background()
	if got, err := newEmptyIdentity(t).OwnerSignInEmail(ctx, " seed@beknown.work "); err != nil || got != "seed@beknown.work" {
		t.Fatalf("no owner: %q %v", got, err)
	}
	s, admin := newTestIdentity(t)
	// A password owner with no email yet: the seed, which links to them.
	if got, _ := s.OwnerSignInEmail(ctx, "seed@beknown.work"); got != "seed@beknown.work" {
		t.Fatalf("owner without an email: %q", got)
	}
	linked := clerkUser(t, s, "user_owner", "Seed@beknown.work", "seed@beknown.work")
	if linked.ID != admin {
		t.Fatalf("seed did not link to the owner: %+v", linked)
	}
	m := clerkUser(t, s, "user_m", "m@beknown.work", "")
	if got, _ := s.OwnerSignInEmail(ctx, "seed@beknown.work"); got != "Seed@beknown.work" {
		t.Fatalf("linked owner: %q", got)
	}
	if err := s.TransferOwnership(ctx, admin, m.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.OwnerSignInEmail(ctx, "seed@beknown.work"); got != "m@beknown.work" {
		t.Fatalf("after a transfer: %q, want the new owner's address", got)
	}
}

// The operator bootstrap command defaults to the owner.
func TestFindUserForOperatorPrefersOwner(t *testing.T) {
	s, admin := newTestIdentity(t)
	ctx := context.Background()
	m := clerkUser(t, s, "user_m", "m@beknown.work", "")
	if u, err := s.FindUserForOperator(ctx, ""); err != nil || u.ID != admin {
		t.Fatalf("default before a transfer = %+v %v", u, err)
	}
	if err := s.TransferOwnership(ctx, admin, m.ID); err != nil {
		t.Fatal(err)
	}
	if u, err := s.FindUserForOperator(ctx, ""); err != nil || u.ID != m.ID {
		t.Fatalf("default after a transfer = %+v %v, want the new owner", u, err)
	}
}

// auditCount counts audit rows of eventType written by actorID.
func auditCount(t *testing.T, s *Service, eventType, actorID string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM audit_events WHERE event_type = ? AND agent_id = ?`, eventType, actorID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The review's scenario: a fresh organisation install with an owner email.
// An org admin signs in first and bootstraps as an admin, not the owner;
// a restart (EnsureOwner) must not hand them the role; the configured
// owner claims it on their first sign-in.
func TestOwnerEmailIsNeverPassedOver(t *testing.T) {
	ctx := context.Background()
	s := newEmptyIdentity(t)
	s.SetOwnerEmail("alice@beknown.work")
	bob := upsertOrg(t, s, "user_bob", "bob@beknown.work", "org:admin", "alice@beknown.work")
	if bob.Owner || bob.Role != RoleAdmin {
		t.Fatalf("bob = %+v, want a bootstrap admin who is not the owner", bob)
	}
	if _, err := s.EnsureOwner(ctx, "alice@beknown.work"); !errors.Is(err, ErrNoUser) {
		t.Fatalf("restart: %v, want the role still vacant", err)
	}
	if b, _ := s.GetUserByID(ctx, bob.ID); b.Owner {
		t.Fatal("the restart made bob the owner")
	}
	alice := upsertOrg(t, s, "user_alice", "Alice@beknown.work", "org:member", "alice@beknown.work")
	if !alice.Owner || alice.Role != RoleAdmin {
		t.Fatalf("alice = %+v, want the owner and an admin", alice)
	}
	if auditCount(t, s, "user.owner", ActorSignIn) != 1 || auditCount(t, s, "user.role", ActorSignIn) != 2 {
		t.Fatalf("sign-in audits: owner %d role %d, want 1 and 2 (bob's and alice's admin)",
			auditCount(t, s, "user.owner", ActorSignIn), auditCount(t, s, "user.role", ActorSignIn))
	}
}

// A route that only provisions members (federation) never bootstraps an
// admin or the owner, even on an empty install or with the owner email.
func TestNoBootstrapNeverPromotes(t *testing.T) {
	ctx := context.Background()
	s := newEmptyIdentity(t)
	u, err := s.UpsertClerkUser(ctx, ClerkProfile{ClerkUserID: "user_f", Email: "f@beknown.work", OrgRole: "org:admin", NoBootstrap: true}, "")
	if err != nil || u.Owner || u.Role != RoleMember {
		t.Fatalf("federated first user = %+v %v, want a member", u, err)
	}
	s.SetOwnerEmail("owner@beknown.work")
	o, err := s.UpsertClerkUser(ctx, ClerkProfile{ClerkUserID: "user_o", Email: "owner@beknown.work", NoBootstrap: true}, "")
	if err != nil || o.Owner || o.Role != RoleMember {
		t.Fatalf("federated owner email = %+v %v, want a member", o, err)
	}
	again, err := s.UpsertClerkUser(ctx, ClerkProfile{ClerkUserID: "user_o", Email: "owner@beknown.work", NoBootstrap: true}, "")
	if err != nil || again.Owner {
		t.Fatalf("returning federated owner email = %+v %v, want no claim", again, err)
	}
	if ownerCount(t, s) != 0 {
		t.Fatal("an owner appeared")
	}
	// The same person through a sign-in that may bootstrap claims it.
	if c := upsertOrg(t, s, "user_o", "owner@beknown.work", "", ""); !c.Owner {
		t.Fatalf("configured owner's own sign-in = %+v, want the owner", c)
	}
}

// An address the caller could not verify never links the setup account and
// never claims the owner role.
func TestUnverifiedEmailNeverClaims(t *testing.T) {
	ctx := context.Background()
	s, admin := newTestIdentity(t)
	clearOwner(t, s)
	u, err := s.UpsertClerkUser(ctx, ClerkProfile{ClerkUserID: "user_x", Email: "owner@beknown.work", EmailUnverified: true}, "owner@beknown.work")
	if err != nil || u.ID == admin || u.Owner {
		t.Fatalf("unverified owner email = %+v %v, want a separate member", u, err)
	}
	if ownerCount(t, s) != 0 {
		t.Fatal("an unverified address claimed the role")
	}
}

// Only ASCII letters fold: a Unicode look-alike never matches.
func TestEmailEqual(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"Owner@Example.com", "owner@example.com", true},
		{" owner@example.com ", "owner@example.com", true},
		{"owner@example.com", "owner@example.org", false},
		{"owner@\u212Aexample.com", "owner@kexample.com", false},
		{"\u00c9ve@example.com", "\u00e9ve@example.com", false},
		{"", "", false},
		{"a@b", "", false},
	} {
		if got := EmailEqual(c.a, c.b); got != c.want {
			t.Errorf("EmailEqual(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// A demoted owner (an older binary in a rolling deploy) is made an admin
// again at startup, audited; a blocked owner stays blocked.
func TestRepromoteOwner(t *testing.T) {
	ctx := context.Background()
	s, admin := newTestIdentity(t)
	if changed, err := s.RepromoteOwner(ctx); err != nil || changed {
		t.Fatalf("an admin owner: %v %v", changed, err)
	}
	if _, err := s.db.Exec(`UPDATE users SET role = ? WHERE id = ?`, RoleMember, admin); err != nil {
		t.Fatal(err)
	}
	if changed, err := s.RepromoteOwner(ctx); err != nil || !changed {
		t.Fatalf("a demoted owner: %v %v", changed, err)
	}
	if u, _ := s.GetUserByID(ctx, admin); u.Role != RoleAdmin || !u.Owner {
		t.Fatalf("owner after repromotion = %+v", u)
	}
	if auditCount(t, s, "user.role", ActorStartup) != 1 {
		t.Fatal("the repromotion is not audited")
	}
	if _, err := s.db.Exec(`UPDATE users SET status = ? WHERE id = ?`, StatusBlocked, admin); err != nil {
		t.Fatal(err)
	}
	if changed, err := s.RepromoteOwner(ctx); err != nil || changed {
		t.Fatalf("a blocked admin owner: %v %v", changed, err)
	}
	if u, _ := s.GetUserByID(ctx, admin); u.Status != StatusBlocked {
		t.Fatalf("a blocked owner was re-activated: %+v", u)
	}
}

// The host break-glass moves the role from any owner, even a blocked one.
func TestForceTransferOwnership(t *testing.T) {
	ctx := context.Background()
	s, admin := newTestIdentity(t)
	m := clerkUser(t, s, "user_m", "m@beknown.work", "")
	x := clerkUser(t, s, "user_x", "x@beknown.work", "")
	// The owner left and was suspended.
	if changed, err := s.SuspendDepartedOwner(ctx, admin); err != nil || !changed {
		t.Fatalf("suspend: %v %v", changed, err)
	}
	if _, err := s.ForceTransferOwnership(ctx, "u_missing", ActorCLI, false); !errors.Is(err, ErrNoUser) {
		t.Errorf("unknown target: %v", err)
	}
	if err := s.SetStatus(ctx, x.ID, StatusBlocked, "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ForceTransferOwnership(ctx, x.ID, ActorCLI, false); !errors.Is(err, ErrUserBlocked) {
		t.Errorf("blocked target without unblock: %v", err)
	}
	prev, err := s.ForceTransferOwnership(ctx, m.ID, ActorCLI, false)
	if err != nil || prev != admin {
		t.Fatalf("break-glass: prev %q %v", prev, err)
	}
	if ownerID(t, s) != m.ID || ownerCount(t, s) != 1 {
		t.Fatal("the role did not move to m")
	}
	if a, _ := s.GetUserByID(ctx, admin); a.Owner || a.Status != StatusBlocked {
		t.Fatalf("previous owner = %+v, want blocked and not the owner", a)
	}
	if auditCount(t, s, "user.owner", ActorCLI) != 1 || auditCount(t, s, "user.role", ActorCLI) != 1 {
		t.Fatal("the break-glass is not audited")
	}
	if _, err := s.ForceTransferOwnership(ctx, m.ID, ActorCLI, false); !errors.Is(err, ErrAlreadyOwner) {
		t.Errorf("to the owner: %v", err)
	}
	// --unblock re-activates the target as part of the move.
	if _, err := s.ForceTransferOwnership(ctx, x.ID, ActorCLI, true); err != nil {
		t.Fatalf("break-glass with unblock: %v", err)
	}
	if u, _ := s.GetUserByID(ctx, x.ID); !u.Owner || u.Status != StatusActive || u.Role != RoleAdmin {
		t.Fatalf("unblocked new owner = %+v", u)
	}
}

// Agent names resolve whoever owns the agent.
func TestAgentName(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestIdentity(t)
	m := clerkUser(t, s, "user_m", "m@beknown.work", "")
	_, ag, err := s.CreateAgentWithToken(ctx, m.ID, "member-bot")
	if err != nil {
		t.Fatal(err)
	}
	if got := s.AgentName(ctx, ag.ID); got != "member-bot" {
		t.Fatalf("AgentName = %q", got)
	}
	if got := s.AgentName(ctx, "ag_missing"); got != "" {
		t.Fatalf("AgentName(missing) = %q", got)
	}
}
