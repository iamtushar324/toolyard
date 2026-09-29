package api

import (
	"context"
	"errors"
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
// left". Users without a clerk_user_id (the password owner) are never
// touched, and the last active admin is skipped with a log line.
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
		err := s.identity.SetStatus(ctx, u.ID, identity.StatusBlocked, "left_org")
		switch {
		case errors.Is(err, identity.ErrLastAdmin):
			lg.Warn("user left the org but is the last active admin; left active", "user", u.Username)
			continue
		case err != nil:
			lg.Warn("block failed", "user", u.Username, "err", err.Error())
			continue
		}
		if s.access != nil {
			s.access.Invalidate(u.ID)
		}
		// Their Beknown key must stop being forwarded now, not when the
		// resolver's cache expires (as usersPatch does on a block).
		if s.identityKeys != nil {
			s.identityKeys.Invalidate(u.ID)
		}
		_ = s.audit.Write(ctx, audit.Event{
			EventType: "user.status", AgentID: "system:clerk-sync", Reason: "left_org",
			ResultSummary: u.Username + " status=blocked",
		})
		blocked++
	}
	if blocked > 0 {
		lg.Info("blocked users who left the org", "count", blocked)
	}
	return blocked
}
