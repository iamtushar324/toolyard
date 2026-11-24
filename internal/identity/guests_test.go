package identity

import (
	"context"
	"errors"
	"testing"
)

func guestUser(t *testing.T, s *Service, id, email string) *User {
	t.Helper()
	u, err := s.UpsertClerkUser(context.Background(), ClerkProfile{ClerkUserID: id, Email: email, DisplayName: email, Guest: true}, "")
	if err != nil {
		t.Fatalf("UpsertClerkUser(%s, guest): %v", id, err)
	}
	return u
}

// TestGuestSignInRole: a guest sign-in records a guest, never an admin or
// the owner, even on a store with no admin and with the guest's address
// as the owner email; a later member sign-in (they joined the
// organisation) makes them a plain member.
func TestGuestSignInRole(t *testing.T) {
	ctx := context.Background()
	// Empty store, owner email = the guest's address: a non-guest sign-in
	// would bootstrap the owner here.
	s := newEmptyIdentity(t)
	s.SetOwnerEmail("ext@partner.com")
	u, err := s.UpsertClerkUser(ctx, ClerkProfile{ClerkUserID: "user_ext", Email: "ext@partner.com", OrgRole: "org:admin", Guest: true}, "ext@partner.com")
	if err != nil {
		t.Fatal(err)
	}
	if u.Role != RoleGuest || u.Owner || u.Status != StatusActive {
		t.Fatalf("guest sign-in = %+v, want an active guest, not the owner", u)
	}
	got, err := s.GetUserByID(ctx, u.ID)
	if err != nil || got.Role != RoleGuest {
		t.Fatalf("read back = %+v, %v", got, err)
	}

	// On a store with an owner, the same person later signs in as an
	// organisation member: membership wins, and the row is a plain member.
	s2, _ := newTestIdentity(t)
	g := guestUser(t, s2, "user_ext", "ext@partner.com")
	m, err := s2.UpsertClerkUser(ctx, ClerkProfile{ClerkUserID: "user_ext", Email: "ext@partner.com"}, "")
	if err != nil || m.ID != g.ID || m.Role != RoleMember {
		t.Fatalf("member sign-in = %+v, %v; want the same row as a member", m, err)
	}
}

// TestGuestCannotBecomeAdminOrOwner: SetRole, an ownership transfer and
// break-glass all refuse a guest; an admin's row is never turned into a
// guest by an invite.
func TestGuestCannotBecomeAdminOrOwner(t *testing.T) {
	ctx := context.Background()
	s, owner := newTestIdentity(t)
	g := guestUser(t, s, "user_g", "g@partner.com")
	for _, role := range []string{RoleAdmin, RoleMember} {
		if err := s.SetRole(ctx, g.ID, role); !errors.Is(err, ErrGuest) {
			t.Errorf("SetRole(%s) = %v, want ErrGuest", role, err)
		}
	}
	if err := s.TransferOwnership(ctx, owner, g.ID); !errors.Is(err, ErrGuest) {
		t.Errorf("TransferOwnership = %v, want ErrGuest", err)
	}
	if _, err := s.ForceTransferOwnership(ctx, g.ID, ActorCLI, true); !errors.Is(err, ErrGuest) {
		t.Errorf("ForceTransferOwnership = %v, want ErrGuest", err)
	}
	if got, _ := s.GetUserByID(ctx, g.ID); got.Role != RoleGuest || got.Owner {
		t.Errorf("guest after refusals = %+v", got)
	}

	// An admin (and the owner) matched by an invite stays as they are.
	a := clerkUser(t, s, "user_a", "a@partner.com", "")
	if err := s.SetRole(ctx, a.ID, RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertClerkUser(ctx, ClerkProfile{ClerkUserID: "user_a", Email: "a@partner.com", Guest: true}, ""); !errors.Is(err, ErrGuestAdmin) {
		t.Errorf("guest sign-in on an admin = %v, want ErrGuestAdmin", err)
	}
	if got, _ := s.GetUserByID(ctx, a.ID); got.Role != RoleAdmin {
		t.Errorf("admin after a guest sign-in = %+v", got)
	}
}

// TestGuestSignInLiftsOnlyItsOwnBlocks: a new invite lets a guest blocked
// for a removed or expired invite (or for leaving the organisation) in
// again; an admin's own block stays.
func TestGuestSignInLiftsOnlyItsOwnBlocks(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestIdentity(t)
	cases := []struct {
		reason     string
		wantStatus string
	}{
		{"invite_removed", StatusActive},
		{"invite_expired", StatusActive},
		{"left_org", StatusActive},
		{"manual", StatusBlocked},
		{"abuse", StatusBlocked},
	}
	for i, c := range cases {
		id := "user_" + c.reason
		g := guestUser(t, s, id, c.reason+"@partner.com")
		if err := s.SetStatus(ctx, g.ID, StatusBlocked, c.reason); err != nil {
			t.Fatalf("%d block: %v", i, err)
		}
		again := guestUser(t, s, id, c.reason+"@partner.com")
		if again.ID != g.ID || again.Status != c.wantStatus {
			t.Errorf("%s: after a guest sign-in status = %s, want %s", c.reason, again.Status, c.wantStatus)
		}
	}
}
