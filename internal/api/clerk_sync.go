package api

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/logx"
)

// StartClerkSync runs the offboarding sync once now and then every `every`
// until ctx ends: a Clerk-linked user who is no longer in the org gets
// blocked, which revokes their sessions and stops their agents. No-op when
// Clerk sign-in is off.
func (s *Server) StartClerkSync(ctx context.Context, every time.Duration) {
	if s.clerk == nil {
		return
	}
	if every <= 0 {
		every = time.Hour
	}
	go func() {
		s.runClerkSync(ctx)
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.runClerkSync(ctx)
			}
		}
	}()
}

// runClerkSync is one pass; it returns how many users it blocked.
//
// It fails safe: any error listing the org, or an org that reports zero
// members, changes nothing — a Clerk outage must never read as "everyone
// left". Users without a clerk_user_id (password accounts) are never
// touched, and the last active admin is skipped with a log line. An owner
// who left is suspended instead (identity.SuspendDepartedOwner): blocked,
// sessions ended, agents disabled and operator tokens revoked, but still
// the owner, so a host operator moves the role with `toolyard owner
// transfer`; that is logged as an error and audited.
func (s *Server) runClerkSync(ctx context.Context) int {
	lg := logx.For("clerk-sync")
	members, err := s.clerk.OrgMembers(ctx)
	if err != nil {
		lg.Warn("list org members failed; no changes made", "err", err.Error())
		return 0
	}
	if len(members) == 0 {
		lg.Warn("org reports zero members; refusing to block anyone")
		return 0
	}
	users, err := s.identity.ListUsers(ctx)
	if err != nil {
		lg.Warn("list users failed", "err", err.Error())
		return 0
	}
	blocked := 0
	for _, u := range users {
		if u.ClerkUserID == "" || u.Status != identity.StatusActive {
			continue
		}
		if _, ok := members[u.ClerkUserID]; ok {
			continue
		}
		// A guest is never in the organisation: their invite, not the
		// member list, decides whether they stay.
		if u.Role == identity.RoleGuest {
			if s.syncGuest(ctx, lg, u.User) {
				blocked++
			}
			continue
		}
		if s.offboardLeaver(ctx, lg, u.User) {
			blocked++
		}
	}
	if blocked > 0 {
		lg.Info("blocked users who left the org", "count", blocked)
	}
	return blocked
}

// syncGuest checks one active guest against their invite and reports
// whether it blocked them: a removed invite blocks them (invite_removed),
// an expired one too (invite_expired); otherwise the invite's servers are
// mirrored into their grants again, so an edited invite shows on the Users
// page. A failed invite read changes nothing (their scope, read from the
// invite on every call, fails closed anyway).
func (s *Server) syncGuest(ctx context.Context, lg *slog.Logger, u identity.User) bool {
	if s.access == nil {
		return false
	}
	inv, err := s.access.GuestInvite(ctx, u.Email)
	if err != nil {
		lg.Warn("read guest invite failed; guest left as is", "user", u.Username, "err", err.Error())
		return false
	}
	switch {
	case inv == nil:
		return s.blockGuest(ctx, lg, u, "invite_removed", identity.ActorClerkSync)
	case inv.Expired(time.Now()):
		return s.blockGuest(ctx, lg, u, "invite_expired", identity.ActorClerkSync)
	}
	if err := s.mirrorGuestServers(ctx, u.ID, inv); err != nil {
		lg.Warn("mirror guest servers failed", "user", u.Username, "err", err.Error())
	}
	return false
}

// blockGuest blocks a guest for reason (invite_removed, invite_expired),
// which revokes their sessions and stops their agents at once, as
// offboardLeaver does, and audits it as actor. A guest is never the owner
// or an admin, so the owner and last-admin rules do not apply.
func (s *Server) blockGuest(ctx context.Context, lg *slog.Logger, u identity.User, reason, actor string) bool {
	if err := s.identity.SetStatus(ctx, u.ID, identity.StatusBlocked, reason); err != nil {
		lg.Warn("block guest failed", "user", u.Username, "reason", reason, "err", err.Error())
		return false
	}
	s.dropUserCaches(u.ID)
	_ = s.audit.Write(ctx, audit.Event{
		EventType: "user.status", AgentID: actor, Reason: reason,
		ResultSummary: u.Username + " status=blocked",
	})
	return true
}

// offboardLeaver blocks one Clerk-linked user who left the organisation,
// or suspends them when they are the owner, and reports whether it did.
// Ownership may move between the block attempt and the suspension
// (SuspendDepartedOwner then answers ErrNotOwner): the second pass blocks
// them as anyone else, so a departed user never stays active that way.
func (s *Server) offboardLeaver(ctx context.Context, lg *slog.Logger, u identity.User) bool {
	for attempt := 0; attempt < 2; attempt++ {
		err := s.identity.SetStatus(ctx, u.ID, identity.StatusBlocked, "left_org")
		switch {
		case err == nil:
			s.dropUserCaches(u.ID)
			_ = s.audit.Write(ctx, audit.Event{
				EventType: "user.status", AgentID: identity.ActorClerkSync, Reason: "left_org",
				ResultSummary: u.Username + " status=blocked",
			})
			return true
		case errors.Is(err, identity.ErrOwner):
			changed, serr := s.identity.SuspendDepartedOwner(ctx, u.ID)
			switch {
			case errors.Is(serr, identity.ErrNotOwner):
				continue
			case errors.Is(serr, identity.ErrLastAdmin):
				s.warnDepartedSoleOwner(ctx, lg, u)
				return false
			case serr != nil:
				lg.Error("the owner left the org; suspending them failed", "user", u.Username, "err", serr.Error())
				return false
			case !changed:
				return false
			}
			// SuspendDepartedOwner wrote its own audit row.
			lg.Error("the owner left the org: blocked, sessions ended, agents disabled, operator tokens revoked, push subscriptions deleted; they stay the owner until a host operator runs: toolyard owner transfer --to <username|email>", "user", u.Username)
			s.dropUserCaches(u.ID)
			return true
		case errors.Is(err, identity.ErrLastAdmin):
			lg.Warn("user left the org but is the last active admin; left active", "user", u.Username)
			return false
		default:
			lg.Warn("block failed", "user", u.Username, "err", err.Error())
			return false
		}
	}
	return false
}

// warnDepartedSoleOwner: the owner left the organisation but is the only
// active admin, so the sync leaves them active (a partial member list
// must not lock everyone out). It logs an error on every pass and audits
// it once per process.
func (s *Server) warnDepartedSoleOwner(ctx context.Context, lg *slog.Logger, u identity.User) {
	lg.Error("the owner left the org but is the only active admin; left active: make another admin, or move the owner role with: toolyard owner transfer --to <username|email>", "user", u.Username)
	if _, seen := s.clerkOwnerKept.LoadOrStore(u.ID, true); seen {
		return
	}
	_ = s.audit.Write(ctx, audit.Event{
		EventType: "user.owner_left_org", AgentID: identity.ActorClerkSync, Reason: "last_admin",
		ResultSummary: u.Username + " left the organisation but is the only active admin; left active",
	})
}

// dropUserCaches makes a block take effect at once: the access scope and
// the Beknown key are re-read on the next call (as usersPatch does).
func (s *Server) dropUserCaches(uid string) {
	if s.access != nil {
		s.access.Invalidate(uid)
	}
	if s.identityKeys != nil {
		s.identityKeys.Invalidate(uid)
	}
}
