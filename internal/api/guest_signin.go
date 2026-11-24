package api

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/access"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/clerk"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/identitykeys"
)

// Guest sign-in (TEC-2058). A verified Clerk user who is not in the
// organisation may still sign in when an admin saved their verified
// primary email in guest_invites and the invite has not expired. Every
// Clerk route (the dashboard sign-in, POST /v1/connect/t3 and the T3
// federation's membership check) asks admitClerkSubject, so the rule
// lives in one place. The refusals below wrap clerk.ErrNotMember: each
// route keeps answering exactly what it answered to a non-member before
// (not_org_member), so the browser never learns whether an invite exists,
// and only the audit row tells the reason apart.
var (
	// errGuestNotInvited: no invite for the person's verified primary
	// email, no verified primary email, or a mode that admits no guest.
	errGuestNotInvited = fmt.Errorf("%w: not invited", clerk.ErrNotMember)
	// errGuestInviteExpired: the invite's expiry has passed.
	errGuestInviteExpired = fmt.Errorf("%w: invite expired", clerk.ErrNotMember)
	// errGuestAdminAccount: the invite matched an admin's or the owner's
	// row (identity.ErrGuestAdmin); an admin is never quietly made a guest.
	errGuestAdminAccount = fmt.Errorf("%w: admin account", clerk.ErrNotMember)
)

// guestDenyReason is the audit reason of a refused Clerk sign-in.
func guestDenyReason(err error) string {
	switch {
	case errors.Is(err, errGuestInviteExpired):
		return "invite_expired"
	case errors.Is(err, errGuestNotInvited):
		return "not_invited"
	case errors.Is(err, errGuestAdminAccount):
		return "guest_admin_account"
	}
	return "not_org_member"
}

// clerkAdmission is who a verified Clerk subject is to toolyard: an
// organisation member (guest nil), or a guest admitted by an invite.
type clerkAdmission struct {
	member clerk.Member
	guest  *access.GuestInvite
}

// admitClerkSubject is the one sign-in decision of every Clerk route.
// membership is the route's own organisation check (OrgMembership of the
// Clerk client it holds). A member is admitted as a member: membership
// wins over an invite. A non-member is admitted as a guest only when
// guest sign-in can apply (not personal sign-in, where only the owner
// signs in; Clerk and access control wired), Clerk reports a verified
// primary email, and an unexpired invite exists for it. Every refusal
// wraps clerk.ErrNotMember (see guestDenyReason). It fails closed: a
// Clerk or database error is returned as it is, never read as "invited".
func (s *Server) admitClerkSubject(ctx context.Context, subject string, membership func(context.Context, string) (clerk.Member, error)) (clerkAdmission, error) {
	member, err := membership(ctx, subject)
	if err == nil {
		return clerkAdmission{member: member}, nil
	}
	if !errors.Is(err, clerk.ErrNotMember) {
		return clerkAdmission{}, err
	}
	if s.clerkOwnerOnly || s.clerk == nil || s.access == nil {
		return clerkAdmission{}, errGuestNotInvited
	}
	// Only the address Clerk verified counts: an organisation identifier
	// or a browser-supplied email is never matched against an invite.
	email, err := s.clerk.VerifiedPrimaryEmail(ctx, subject)
	if errors.Is(err, clerk.ErrNotMember) {
		return clerkAdmission{}, errGuestNotInvited
	}
	if err != nil {
		return clerkAdmission{}, err
	}
	inv, err := s.access.GuestInvite(ctx, email)
	if err != nil {
		return clerkAdmission{}, err
	}
	if inv == nil {
		return clerkAdmission{}, errGuestNotInvited
	}
	if inv.Expired(time.Now()) {
		return clerkAdmission{}, errGuestInviteExpired
	}
	return clerkAdmission{member: clerk.Member{Email: inv.Email, EmailVerified: true}, guest: inv}, nil
}

// upsertClerkAdmitted records an admitted Clerk user: a member as
// upsertClerkMember does, a guest through upsertClerkGuest.
func (s *Server) upsertClerkAdmitted(ctx context.Context, subject string, adm clerkAdmission) (*identity.User, error) {
	if adm.guest == nil {
		return s.upsertClerkMember(ctx, subject, adm.member)
	}
	return s.upsertClerkGuest(ctx, subject, adm.guest)
}

// upsertClerkGuest creates or refreshes the guest row for an invite
// (identity.ClerkProfile.Guest: never an admin or the owner) and mirrors
// the invite's servers into the person's grants for the Users page. The
// scope itself is read from the invite (access.guestScope). An invite that
// matched an admin's row is errGuestAdminAccount.
func (s *Server) upsertClerkGuest(ctx context.Context, subject string, inv *access.GuestInvite) (*identity.User, error) {
	u, err := s.identity.UpsertClerkUser(ctx, identity.ClerkProfile{
		ClerkUserID: subject, Email: inv.Email, DisplayName: inv.Email, Guest: true,
	}, "")
	if errors.Is(err, identity.ErrGuestAdmin) {
		return nil, errGuestAdminAccount
	}
	if err != nil {
		return nil, err
	}
	if err := s.settleGuest(ctx, u.ID, inv); err != nil {
		return nil, err
	}
	return u, nil
}

// settleGuest brings a guest's account in line with their invite: it
// revokes an identity key left from before they were a guest (the key is
// the organisation's identity in its registries, and a forwarded key would
// work outside the invite's limits), then replaces their grants with the
// invite's servers (only when they differ) and drops their caches. The
// revoke runs the same code path as an admin's revoke, as the admin who
// last saved the invite; a registry that cannot be reached now keeps the
// removal pending for the background retry, while the key itself is gone
// at once.
func (s *Server) settleGuest(ctx context.Context, userID string, inv *access.GuestInvite) error {
	if s.identityKeys != nil {
		has, err := s.identityKeys.HasKey(ctx, userID)
		if err != nil {
			return err
		}
		if has {
			actor := inv.UpdatedBy
			if actor == "" {
				actor = inv.CreatedBy
			}
			if _, err := s.identityKeys.Revoke(ctx, actor, userID); err != nil && !errors.Is(err, identitykeys.ErrNoKey) {
				return err
			}
		}
	}
	if s.access == nil {
		s.dropUserCaches(userID)
		return nil
	}
	have, err := s.access.Groups(ctx, userID)
	if err != nil {
		return err
	}
	if !sameStrings(have, inv.Servers) {
		if err := s.access.SetGroups(ctx, userID, inv.Servers, inv.UpdatedBy); err != nil {
			return err
		}
	}
	s.dropUserCaches(userID)
	return nil
}

// sameStrings reports whether two sorted lists hold the same names.
func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// syncGuestGrants mirrors the current invite of userID, when that user is
// a guest, into their grants (the federation's step after its sign-in
// upsert). It re-reads the user and the invite, so it never trusts a
// stale copy.
func (s *Server) syncGuestGrants(ctx context.Context, userID string) error {
	if s.access == nil {
		return nil
	}
	u, err := s.identity.GetUserByID(ctx, userID)
	if err != nil {
		return err
	}
	if u.Role != identity.RoleGuest {
		return nil
	}
	inv, err := s.access.GuestInvite(ctx, u.Email)
	if err != nil || inv == nil {
		return err
	}
	return s.settleGuest(ctx, u.ID, inv)
}

// auditClerkDenied writes the denial row of a refused Clerk sign-in with
// the reason guestDenyReason gives (not_invited, invite_expired, ...).
// Only the Clerk subject is recorded: never the email the person signed
// in with, which an outsider could use to probe the invite list.
func (s *Server) auditClerkDenied(ctx context.Context, eventType, subject string, err error) {
	if s.audit == nil {
		return
	}
	_ = s.audit.Write(context.WithoutCancel(ctx), audit.Event{
		EventType: eventType, Decision: "denied", Reason: guestDenyReason(err), ResultSummary: subject,
	})
}
