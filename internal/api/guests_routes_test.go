package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/access"
	"github.com/tusharbhardwaj/toolyard/internal/clerk"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
)

// VerifySessionTokenFor makes fakeClerk a connectVerifier too, so the
// connect route can run on it in these tests.
func (f *fakeClerk) VerifySessionTokenFor(ctx context.Context, token string, _ []string) (clerk.Claims, error) {
	return f.VerifySessionToken(ctx, token)
}

// guestEnv is the access test server with two upstream servers and a
// Clerk user outside the organisation (user_ext, Ext@Partner.com as the
// verified primary address).
func guestEnv(t *testing.T) *accessTestEnv {
	t.Helper()
	e := newAccessTestServer(t)
	e.seedServer(t, "github", "connected", true)
	e.seedServer(t, "linear", "connected", true)
	e.clerk.set(func(f *fakeClerk) {
		f.claims.Subject = "user_ext"
		f.memberErr = clerk.ErrNotMember
		f.member = clerk.Member{Email: "Ext@Partner.com"}
	})
	return e
}

func (e *accessTestEnv) clerkSignIn(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	return e.do(t, nil, http.MethodPost, "/v1/auth/clerk/session", `{"token":"good-token"}`)
}

func (e *accessTestEnv) invite(t *testing.T, inv access.GuestInvite) {
	t.Helper()
	if _, _, err := e.access.PutGuestInvite(context.Background(), inv, e.admin.ID); err != nil {
		t.Fatalf("put invite: %v", err)
	}
}

func expectStatus(t *testing.T, name string, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("%s: status %d, want %d (%s)", name, rec.Code, status, rec.Body.String())
	}
	if code != "" && decodeJSON(t, rec)["error"] != code {
		t.Fatalf("%s: body %s, want error %q", name, rec.Body.String(), code)
	}
}

// TestGuestDashboardSignIn: an outsider without an invite is refused as
// any non-member (not_org_member) with not_invited on record; with an
// invite they sign in as a guest whose grants are exactly the invite's
// servers; an expired invite refuses them again (invite_expired). A guest
// is never made an admin, and the guest routes stay closed to them.
func TestGuestDashboardSignIn(t *testing.T) {
	e := guestEnv(t)
	ctx := context.Background()
	admin := e.cookieFor(t, e.admin.ID)

	expectStatus(t, "not invited", e.clerkSignIn(t), http.StatusForbidden, "not_org_member")
	if n := e.auditRows(t, "user.login", "not_invited"); n != 1 {
		t.Errorf("not_invited rows = %d, want 1", n)
	}
	if users, _ := e.id.ListUsers(ctx); len(users) != 1 {
		t.Fatalf("users after the refusal = %d, want only the admin", len(users))
	}

	rec := e.do(t, admin, http.MethodPost, "/v1/guests", `{"email":"ext@partner.com","servers":["github"],"note":"contractor"}`)
	expectStatus(t, "create invite", rec, http.StatusCreated, "")
	if e.auditRows(t, EventGuestInvite, "created") != 1 {
		t.Error("invite creation not audited")
	}

	rec = e.clerkSignIn(t)
	expectStatus(t, "invited", rec, http.StatusOK, "")
	u := decodeJSON(t, rec)
	if u["role"] != identity.RoleGuest || u["is_owner"] != false || u["email"] != "ext@partner.com" {
		t.Fatalf("guest = %v", u)
	}
	uid, _ := u["id"].(string)
	if g, err := e.access.Groups(ctx, uid); err != nil || len(g) != 1 || g[0] != "github" {
		t.Errorf("guest grants = %v, %v; want [github]", g, err)
	}
	if e.auditRows(t, "user.login", "guest_invite") != 1 {
		t.Error("guest sign-in not audited as guest_invite")
	}
	guest := sessionCookie(t, rec)
	if me := decodeJSON(t, e.do(t, guest, http.MethodGet, "/v1/auth/me", "")); me["role"] != identity.RoleGuest {
		t.Errorf("/me = %v", me)
	}
	for _, p := range []string{"/v1/guests", "/v1/users"} {
		expectStatus(t, "guest on "+p, e.do(t, guest, http.MethodGet, p, ""), http.StatusForbidden, "admin_only")
	}

	// Never an admin, nor given other servers through the Users page.
	expectStatus(t, "promote guest", e.do(t, admin, http.MethodPatch, "/v1/users/"+uid, `{"role":"admin"}`), http.StatusConflict, "guest_invite_managed")
	expectStatus(t, "grant guest", e.do(t, admin, http.MethodPatch, "/v1/users/"+uid, `{"servers":["linear"]}`), http.StatusConflict, "guest_invite_managed")
	if got, _ := e.id.GetUserByID(ctx, uid); got.Role != identity.RoleGuest {
		t.Errorf("guest after refusals = %+v", got)
	}

	// Expired.
	if _, err := e.db.Exec(`UPDATE guest_invites SET expires_at = ?`, time.Now().Add(-time.Minute).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	e.access.Invalidate("")
	expectStatus(t, "expired", e.clerkSignIn(t), http.StatusForbidden, "not_org_member")
	if n := e.auditRows(t, "user.login", "invite_expired"); n != 1 {
		t.Errorf("invite_expired rows = %d, want 1", n)
	}
	if sc := e.access.UserScope(ctx, uid); !sc.Denied {
		t.Errorf("expired guest scope = %+v, want denied", sc)
	}
}

// TestGuestSignInRefusals: personal sign-in admits no guest; Clerk
// without a verified primary email admits no guest; a Clerk outage fails
// closed.
func TestGuestSignInRefusals(t *testing.T) {
	e := guestEnv(t)
	e.invite(t, access.GuestInvite{Email: "ext@partner.com", Servers: []string{"github"}})

	e.srv.clerkOwnerOnly = true
	expectStatus(t, "personal mode", e.clerkSignIn(t), http.StatusForbidden, "not_org_member")
	e.srv.clerkOwnerOnly = false

	e.clerk.set(func(f *fakeClerk) { f.verifiedErr = clerk.ErrNotMember })
	expectStatus(t, "unverified email", e.clerkSignIn(t), http.StatusForbidden, "not_org_member")
	e.clerk.set(func(f *fakeClerk) { f.verifiedErr = clerk.ErrUnavailable })
	expectStatus(t, "clerk down", e.clerkSignIn(t), http.StatusServiceUnavailable, "clerk_unavailable")

	if n := e.auditRows(t, "user.login", "not_invited"); n != 2 {
		t.Errorf("not_invited rows = %d, want 2", n)
	}
	if users, _ := e.id.ListUsers(context.Background()); len(users) != 1 {
		t.Errorf("users after refusals = %d, want only the admin", len(users))
	}
}

// TestOrgMemberWithInviteStaysMember: membership wins over an invite for
// the same address.
func TestOrgMemberWithInviteStaysMember(t *testing.T) {
	e := newAccessTestServer(t)
	e.seedServer(t, "github", "connected", true)
	e.invite(t, access.GuestInvite{Email: "ada@beknown.work", Servers: []string{"github"}})
	rec := e.clerkSignIn(t)
	expectStatus(t, "member", rec, http.StatusOK, "")
	if u := decodeJSON(t, rec); u["role"] != identity.RoleMember {
		t.Errorf("member with an invite = %v, want a member", u)
	}
}

// TestGuestConnectT3: POST /v1/connect/t3 takes the same decision: no
// invite or an expired one is not_org_member (with the reason on record),
// an invite gives the guest their bkt3 agent and no identity key.
func TestGuestConnectT3(t *testing.T) {
	e := guestEnv(t)
	e.srv.connect = &connectConfig{clerk: e.clerk, origins: []string{"https://t3.example.test"}, ipMax: 100, subjectMax: 100, window: time.Minute}
	post := func() *httptest.ResponseRecorder {
		return e.do(t, nil, http.MethodPost, "/v1/connect/t3", `{"token":"good-token"}`)
	}
	expectStatus(t, "not invited", post(), http.StatusForbidden, "not_org_member")
	if n := e.auditRows(t, connectT3EventType, "not_invited"); n != 1 {
		t.Errorf("not_invited rows = %d, want 1", n)
	}

	e.invite(t, access.GuestInvite{Email: "ext@partner.com", Servers: []string{"github"}})
	rec := post()
	expectStatus(t, "invited", rec, http.StatusOK, "")
	out := decodeJSON(t, rec)
	uid, _ := out["user_id"].(string)
	agentID, _ := out["agent_id"].(string)
	if out["token"] == "" || uid == "" || agentID == "" {
		t.Fatalf("connect = %v", out)
	}
	if u, _ := e.id.GetUserByID(context.Background(), uid); u == nil || u.Role != identity.RoleGuest {
		t.Fatalf("connected user = %+v, want a guest", u)
	}
	if sc := e.access.ScopeFor(context.Background(), agentID); !sc.Guest || !sc.Allows("github") || sc.Allows("linear") {
		t.Errorf("guest agent scope = %+v", sc)
	}

	if _, err := e.db.Exec(`UPDATE guest_invites SET expires_at = ?`, time.Now().Add(-time.Minute).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	expectStatus(t, "expired", post(), http.StatusForbidden, "not_org_member")
	if n := e.auditRows(t, connectT3EventType, "invite_expired"); n != 1 {
		t.Errorf("invite_expired rows = %d, want 1", n)
	}
}

// TestGuestInviteRoutes: admin only, validated, audited; deleting an
// invite blocks the guest at once.
func TestGuestInviteRoutes(t *testing.T) {
	e := guestEnv(t)
	ctx := context.Background()
	admin := e.cookieFor(t, e.admin.ID)
	member := e.member(t, "user_mem", "mem@beknown.work")
	memberCookie := e.cookieFor(t, member.ID)

	expectStatus(t, "no session", e.do(t, nil, http.MethodGet, "/v1/guests", ""), http.StatusUnauthorized, "")
	expectStatus(t, "member list", e.do(t, memberCookie, http.MethodGet, "/v1/guests", ""), http.StatusForbidden, "admin_only")
	expectStatus(t, "member create", e.do(t, memberCookie, http.MethodPost, "/v1/guests", `{"email":"x@partner.com","servers":["github"]}`), http.StatusForbidden, "admin_only")
	expectStatus(t, "member delete", e.do(t, memberCookie, http.MethodDelete, "/v1/guests/x@partner.com", ""), http.StatusForbidden, "admin_only")

	past := time.Now().Add(-time.Hour).UnixMilli()
	for name, body := range map[string]string{
		"memory":         `{"email":"x@partner.com","servers":["memory"]}`,
		"notes":          `{"email":"x@partner.com","servers":["github","notes"]}`,
		"events":         `{"email":"x@partner.com","servers":["events"]}`,
		"unknown server": `{"email":"x@partner.com","servers":["nope"]}`,
		"no servers":     `{"email":"x@partner.com","servers":[]}`,
		"bad email":      `{"email":"partner.com","servers":["github"]}`,
		"unknown tool":   `{"email":"x@partner.com","servers":["github"],"tools":["github.nope"]}`,
		"other's tool":   `{"email":"x@partner.com","servers":["github"],"tools":["memory.get"]}`,
		"past expiry":    `{"email":"x@partner.com","servers":["github"],"expires_at":` + strconv.FormatInt(past, 10) + `}`,
		"unknown field":  `{"email":"x@partner.com","servers":["github"],"role":"admin"}`,
	} {
		expectStatus(t, name, e.do(t, admin, http.MethodPost, "/v1/guests", body), http.StatusBadRequest, "")
	}
	if n := e.auditRows(t, EventGuestInvite, ""); n != 0 {
		t.Fatalf("refused invites wrote %d audit rows", n)
	}

	future := time.Now().Add(48 * time.Hour).UnixMilli()
	expectStatus(t, "create", e.do(t, admin, http.MethodPost, "/v1/guests",
		`{"email":"Ext@Partner.com","servers":["github"],"expires_at":`+strconv.FormatInt(future, 10)+`}`), http.StatusCreated, "")
	expectStatus(t, "update", e.do(t, admin, http.MethodPost, "/v1/guests",
		`{"email":"ext@partner.com","servers":["github","linear"]}`), http.StatusOK, "")
	if e.auditRows(t, EventGuestInvite, "created") != 1 || e.auditRows(t, EventGuestInvite, "updated") != 1 {
		t.Error("create and update not audited")
	}

	// The guest signs in and has an agent.
	rec := e.clerkSignIn(t)
	expectStatus(t, "guest sign-in", rec, http.StatusOK, "")
	uid, _ := decodeJSON(t, rec)["id"].(string)
	guestCookie := sessionCookie(t, rec)
	_, agentID := e.agentFor(t, uid)
	if g, _ := e.access.Groups(ctx, uid); len(g) != 2 {
		t.Errorf("guest grants = %v, want github and linear", g)
	}

	list := decodeJSON(t, e.do(t, admin, http.MethodGet, "/v1/guests", ""))
	invites, _ := list["invites"].([]any)
	if len(invites) != 1 {
		t.Fatalf("list = %v", list)
	}
	first, _ := invites[0].(map[string]any)
	if first["email"] != "ext@partner.com" || first["user_id"] != uid || first["user_status"] != identity.StatusActive || first["expired"] != false {
		t.Errorf("listed invite = %v", first)
	}

	expectStatus(t, "delete unknown", e.do(t, admin, http.MethodDelete, "/v1/guests/nobody@partner.com", ""), http.StatusNotFound, "")
	rec = e.do(t, admin, http.MethodDelete, "/v1/guests/ext%40partner.com", "")
	expectStatus(t, "delete", rec, http.StatusOK, "")
	if blocked, _ := decodeJSON(t, rec)["blocked_users"].([]any); len(blocked) != 1 || blocked[0] != uid {
		t.Errorf("delete answer = %s", rec.Body.String())
	}
	got, _ := e.id.GetUserByID(ctx, uid)
	if got.Status != identity.StatusBlocked || got.BlockedReason != "invite_removed" {
		t.Errorf("guest after delete = %+v", got)
	}
	if e.auditRows(t, EventGuestInvite, "deleted") != 1 || e.auditRows(t, "user.status", "invite_removed") != 1 {
		t.Error("delete not audited")
	}
	if sc := e.access.ScopeFor(ctx, agentID); !sc.Denied {
		t.Errorf("guest agent after delete = %+v, want denied", sc)
	}
	if rec := e.do(t, guestCookie, http.MethodGet, "/v1/auth/me", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("guest session after delete: %d", rec.Code)
	}
}

// TestClerkSyncGuests: the hourly sync never blocks a guest for not being
// in the organisation; it blocks one whose invite was removed or expired,
// and keeps (and re-mirrors) one whose invite stands.
func TestClerkSyncGuests(t *testing.T) {
	e := guestEnv(t)
	ctx := context.Background()
	mk := func(sub, email string) *identity.User {
		u, err := e.id.UpsertClerkUser(ctx, identity.ClerkProfile{ClerkUserID: sub, Email: email, Guest: true}, "")
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	keep := mk("user_keep", "keep@partner.com")
	gone := mk("user_gone", "gone@partner.com")
	old := mk("user_old", "old@partner.com")
	e.invite(t, access.GuestInvite{Email: "keep@partner.com", Servers: []string{"linear"}})
	e.invite(t, access.GuestInvite{Email: "old@partner.com", Servers: []string{"github"}})
	if _, err := e.db.Exec(`UPDATE guest_invites SET expires_at = ? WHERE email = 'old@partner.com'`, time.Now().Add(-time.Minute).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	e.clerk.set(func(f *fakeClerk) {
		f.members = map[string]clerk.Member{"user_ada": {IsMember: true, Email: "ada@beknown.work"}}
	})
	if n := e.srv.runClerkSync(ctx); n != 2 {
		t.Fatalf("blocked = %d, want 2", n)
	}
	cases := []struct {
		u      *identity.User
		status string
		reason string
	}{
		{keep, identity.StatusActive, ""},
		{gone, identity.StatusBlocked, "invite_removed"},
		{old, identity.StatusBlocked, "invite_expired"},
	}
	for _, c := range cases {
		got, _ := e.id.GetUserByID(ctx, c.u.ID)
		if got.Status != c.status || got.BlockedReason != c.reason {
			t.Errorf("%s = %s/%s, want %s/%s", c.u.Email, got.Status, got.BlockedReason, c.status, c.reason)
		}
	}
	if g, _ := e.access.Groups(ctx, keep.ID); len(g) != 1 || g[0] != "linear" {
		t.Errorf("kept guest grants = %v, want [linear]", g)
	}
	if e.auditRows(t, "user.status", "left_org") != 0 {
		t.Error("a guest was blocked as left_org")
	}
	if n := e.srv.runClerkSync(ctx); n != 0 {
		t.Errorf("second pass blocked %d, want 0", n)
	}
}

// TestAdmitClerkSubject: the one decision every Clerk route asks.
func TestAdmitClerkSubject(t *testing.T) {
	e := guestEnv(t)
	ctx := context.Background()
	e.invite(t, access.GuestInvite{Email: "ext@partner.com", Servers: []string{"github"}})
	e.invite(t, access.GuestInvite{Email: "late@partner.com", Servers: []string{"github"}})
	if _, err := e.db.Exec(`UPDATE guest_invites SET expires_at = ? WHERE email = 'late@partner.com'`, time.Now().Add(-time.Minute).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	member := func(context.Context, string) (clerk.Member, error) {
		return clerk.Member{IsMember: true, Email: "ext@partner.com"}, nil
	}
	notMember := func(context.Context, string) (clerk.Member, error) { return clerk.Member{}, clerk.ErrNotMember }
	down := func(context.Context, string) (clerk.Member, error) { return clerk.Member{}, clerk.ErrUnavailable }
	cases := []struct {
		name       string
		membership func(context.Context, string) (clerk.Member, error)
		email      string
		verified   error
		ownerOnly  bool
		wantGuest  bool
		wantReason string // "" = admitted
		wantDown   bool
	}{
		{name: "member wins", membership: member, email: "ext@partner.com"},
		{name: "invited", membership: notMember, email: "EXT@partner.com", wantGuest: true},
		{name: "not invited", membership: notMember, email: "who@partner.com", wantReason: "not_invited"},
		{name: "expired", membership: notMember, email: "late@partner.com", wantReason: "invite_expired"},
		{name: "unverified", membership: notMember, email: "ext@partner.com", verified: clerk.ErrNotMember, wantReason: "not_invited"},
		{name: "personal mode", membership: notMember, email: "ext@partner.com", ownerOnly: true, wantReason: "not_invited"},
		{name: "clerk down", membership: down, email: "ext@partner.com", wantDown: true},
		{name: "email lookup down", membership: notMember, email: "ext@partner.com", verified: clerk.ErrUnavailable, wantDown: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e.clerk.set(func(f *fakeClerk) { f.member.Email, f.verifiedErr = c.email, c.verified })
			e.srv.clerkOwnerOnly = c.ownerOnly
			defer func() { e.srv.clerkOwnerOnly = false }()
			adm, err := e.srv.admitClerkSubject(ctx, "user_ext", c.membership)
			switch {
			case c.wantDown:
				if err == nil || errors.Is(err, clerk.ErrNotMember) {
					t.Fatalf("err = %v, want an outage error", err)
				}
			case c.wantReason != "":
				if !errors.Is(err, clerk.ErrNotMember) || guestDenyReason(err) != c.wantReason {
					t.Fatalf("err = %v (reason %s), want %s", err, guestDenyReason(err), c.wantReason)
				}
			default:
				if err != nil || (adm.guest != nil) != c.wantGuest {
					t.Fatalf("admission = %+v, %v; want guest %v", adm, err, c.wantGuest)
				}
			}
		})
	}
}
