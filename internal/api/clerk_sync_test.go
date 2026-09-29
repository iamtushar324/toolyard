package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/clerk"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
)

func TestClerkSyncBlocksLeavers(t *testing.T) {
	e := newAccessTestServer(t)
	ctx := context.Background()
	stay := e.member(t, "user_stay", "stay@beknown.work")
	left := e.member(t, "user_left", "left@beknown.work")
	leftCookie := e.cookieFor(t, left.ID)
	tok, _, err := e.id.CreateAgentWithToken(ctx, left.ID, "bot")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.access.SetGroups(ctx, left.ID, []string{"memory"}, e.admin.ID); err != nil {
		t.Fatal(err)
	}
	if sc := e.access.UserScope(ctx, left.ID); !sc.Allows("memory") {
		t.Fatalf("precondition: scope = %+v", sc)
	}

	e.clerk.set(func(f *fakeClerk) {
		f.members = map[string]clerk.Member{"user_stay": {IsMember: true, Email: "stay@beknown.work"}}
	})
	if n := e.srv.runClerkSync(ctx); n != 1 {
		t.Fatalf("blocked = %d, want 1", n)
	}
	got, _ := e.id.GetUserByID(ctx, left.ID)
	if got.Status != identity.StatusBlocked || got.BlockedReason != "left_org" {
		t.Errorf("leaver = %+v", got)
	}
	if s, _ := e.id.GetUserByID(ctx, stay.ID); s.Status != identity.StatusActive {
		t.Errorf("stayer = %+v", s)
	}
	// The password owner has no clerk_user_id and is never touched.
	if a, _ := e.id.GetUserByID(ctx, e.admin.ID); a.Status != identity.StatusActive {
		t.Errorf("password admin = %+v", a)
	}
	// Sessions revoked, agents stopped, scope denied, audit written.
	if rec := e.do(t, leftCookie, "GET", "/v1/auth/me", ""); rec.Code != 401 {
		t.Errorf("leaver's cookie: %d", rec.Code)
	}
	if _, err := e.id.VerifyAgentToken(ctx, tok); !errors.Is(err, identity.ErrAgentTokenInvalid) {
		t.Errorf("leaver's agent token: %v", err)
	}
	if sc := e.access.UserScope(ctx, left.ID); !sc.Denied {
		t.Errorf("leaver's scope = %+v", sc)
	}
	if e.auditRows(t, "user.status", "left_org") != 1 {
		t.Error("left_org not audited")
	}
	// Idempotent: a second pass changes nothing.
	if n := e.srv.runClerkSync(ctx); n != 0 {
		t.Errorf("second pass blocked %d", n)
	}
}

func TestClerkSyncFailsSafe(t *testing.T) {
	e := newAccessTestServer(t)
	ctx := context.Background()
	m := e.member(t, "user_m", "m@beknown.work")

	e.clerk.set(func(f *fakeClerk) { f.membersErr = clerk.ErrUnavailable })
	if n := e.srv.runClerkSync(ctx); n != 0 {
		t.Errorf("blocked %d during outage", n)
	}
	e.clerk.set(func(f *fakeClerk) { f.membersErr = nil; f.members = map[string]clerk.Member{} })
	if n := e.srv.runClerkSync(ctx); n != 0 {
		t.Errorf("blocked %d on an empty org listing", n)
	}
	if got, _ := e.id.GetUserByID(ctx, m.ID); got.Status != identity.StatusActive {
		t.Errorf("member after fail-safe passes = %+v", got)
	}
	if e.auditRows(t, "user.status", "") != 0 {
		t.Error("audit rows written during fail-safe passes")
	}
}

func TestClerkSyncKeepsLastAdmin(t *testing.T) {
	e := newAccessTestServer(t)
	ctx := context.Background()
	// The only active admin is Clerk-linked (owner signed in via Clerk).
	owner, err := e.id.UpsertClerkUser(ctx, identity.ClerkProfile{ClerkUserID: "user_owner", Email: "owner@beknown.work"}, "owner@beknown.work")
	if err != nil {
		t.Fatal(err)
	}
	if owner.ID != e.admin.ID || owner.Role != identity.RoleAdmin {
		t.Fatalf("owner link precondition: %+v", owner)
	}
	other := e.member(t, "user_other", "other@beknown.work")
	e.clerk.set(func(f *fakeClerk) {
		f.members = map[string]clerk.Member{"user_someone_else": {IsMember: true}}
	})
	if n := e.srv.runClerkSync(ctx); n != 1 {
		t.Errorf("blocked = %d, want 1 (the member, not the last admin)", n)
	}
	if a, _ := e.id.GetUserByID(ctx, e.admin.ID); a.Status != identity.StatusActive {
		t.Errorf("last admin was blocked: %+v", a)
	}
	if o, _ := e.id.GetUserByID(ctx, other.ID); o.Status != identity.StatusBlocked {
		t.Errorf("member not blocked: %+v", o)
	}
}

func TestStartClerkSyncRunsImmediately(t *testing.T) {
	e := newAccessTestServer(t)
	left := e.member(t, "user_left", "left@beknown.work")
	e.clerk.set(func(f *fakeClerk) {
		f.members = map[string]clerk.Member{"user_stay": {IsMember: true}}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.srv.StartClerkSync(ctx, time.Hour)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if u, _ := e.id.GetUserByID(ctx, left.ID); u.Status == identity.StatusBlocked {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("StartClerkSync did not run its first pass")
}

func TestStartClerkSyncNoopWhenOff(t *testing.T) {
	e := newAccessTestServer(t)
	e.srv.clerk = nil
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.srv.StartClerkSync(ctx, time.Millisecond)
	time.Sleep(20 * time.Millisecond)
	if e.clerk.calls != 0 {
		t.Errorf("sync ran with Clerk off")
	}
}
