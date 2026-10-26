package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/clerk"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
)

// makeOwner posts a transfer of ownership to uid with the typed confirm.
func (e *accessTestEnv) makeOwner(t *testing.T, cookie *http.Cookie, uid, confirm string) (int, map[string]any) {
	t.Helper()
	body, err := json.Marshal(map[string]string{"confirm": confirm})
	if err != nil {
		t.Fatal(err)
	}
	rec := e.do(t, cookie, http.MethodPost, "/v1/users/"+uid+"/make-owner", string(body))
	out := map[string]any{}
	if rec.Body.Len() > 0 {
		out = decodeJSON(t, rec)
	}
	return rec.Code, out
}

func (e *accessTestEnv) ownerID(t *testing.T) string {
	t.Helper()
	u, err := e.id.Owner(context.Background())
	if err != nil {
		t.Fatalf("owner: %v", err)
	}
	return u.ID
}

// The first account the setup route creates is the owner.
func TestAuthSetupCreatesOwner(t *testing.T) {
	e := newAccessTestServer(t)
	if _, err := e.db.Exec(`DELETE FROM users`); err != nil {
		t.Fatal(err)
	}
	rec := e.do(t, nil, http.MethodPost, "/v1/auth/setup", `{"Username":"first","Password":"long-enough-password"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("setup: %d %s", rec.Code, rec.Body.String())
	}
	u := decodeJSON(t, rec)
	if u["is_owner"] != true || u["role"] != identity.RoleAdmin {
		t.Fatalf("first account = %v, want the owner", u)
	}
	me := decodeJSON(t, e.do(t, sessionCookie(t, rec), http.MethodGet, "/v1/auth/me", ""))
	if me["is_owner"] != true {
		t.Fatalf("/me = %v", me)
	}
}

// With no owner email configured, the first verified Clerk sign-in is the
// owner and an admin, even as a plain organisation member.
func TestFirstClerkSignInIsOwner(t *testing.T) {
	e := newAccessTestServer(t)
	e.srv.ownerEmail = ""
	if _, err := e.db.Exec(`DELETE FROM users`); err != nil {
		t.Fatal(err)
	}
	e.clerk.set(func(f *fakeClerk) { f.member.Role = "org:member" })
	rec := e.do(t, nil, http.MethodPost, "/v1/auth/clerk/session", `{"token":"good-token"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("sign-in: %d %s", rec.Code, rec.Body.String())
	}
	if u := decodeJSON(t, rec); u["is_owner"] != true || u["role"] != identity.RoleAdmin {
		t.Fatalf("first sign-in = %v, want the owner", u)
	}
	// The next person is a member.
	e.clerk.set(func(f *fakeClerk) { f.claims.Subject = "user_bob"; f.member.Email = "bob@beknown.work" })
	if u := decodeJSON(t, e.do(t, nil, http.MethodPost, "/v1/auth/clerk/session", `{"token":"good-token"}`)); u["is_owner"] != false || u["role"] != identity.RoleMember {
		t.Fatalf("second sign-in = %v, want a member", u)
	}
}

// With an owner email configured, someone else signing in first never
// becomes the owner; the configured person does, on their first sign-in.
func TestConfiguredOwnerEmailWins(t *testing.T) {
	e := newAccessTestServer(t)
	if _, err := e.db.Exec(`DELETE FROM users`); err != nil {
		t.Fatal(err)
	}
	e.clerk.set(func(f *fakeClerk) { f.member.Role = "org:admin" })
	if u := decodeJSON(t, e.do(t, nil, http.MethodPost, "/v1/auth/clerk/session", `{"token":"good-token"}`)); u["is_owner"] != false {
		t.Fatalf("org admin first = %v, want not the owner", u)
	}
	e.clerk.set(func(f *fakeClerk) {
		f.claims.Subject = "user_owner"
		f.member.Email = "Owner@beknown.work"
		f.member.Role = "org:member"
	})
	if u := decodeJSON(t, e.do(t, nil, http.MethodPost, "/v1/auth/clerk/session", `{"token":"good-token"}`)); u["is_owner"] != true || u["role"] != identity.RoleAdmin {
		t.Fatalf("configured owner = %v, want the owner", u)
	}
}

// The owner transfers ownership with a typed confirmation; the new owner
// is an admin, the previous one stays an admin, and an audit row says so.
func TestMakeOwnerByOwner(t *testing.T) {
	e := newAccessTestServer(t)
	owner := e.cookieFor(t, e.admin.ID)
	m := e.member(t, "user_m", "m@beknown.work")

	if code, out := e.makeOwner(t, owner, m.ID, "someone else"); code != http.StatusBadRequest || out["error"] != "confirm_mismatch" {
		t.Fatalf("wrong confirm: %d %v", code, out)
	}
	if code, out := e.makeOwner(t, owner, m.ID, ""); code != http.StatusBadRequest || out["error"] != "confirm_mismatch" {
		t.Fatalf("empty confirm: %d %v", code, out)
	}
	if code, out := e.makeOwner(t, owner, e.admin.ID, "admin"); code != http.StatusConflict || out["error"] != "already_owner" {
		t.Fatalf("to self: %d %v", code, out)
	}
	if code, _ := e.makeOwner(t, owner, "u_missing", "x"); code != http.StatusNotFound {
		t.Fatalf("unknown user: %d", code)
	}
	if e.ownerID(t) != e.admin.ID || e.auditRows(t, "user.owner", "") != 0 {
		t.Fatal("a refused transfer changed something")
	}

	code, out := e.makeOwner(t, owner, m.ID, " M@BEKNOWN.WORK ")
	if code != http.StatusOK || out["is_owner"] != true || out["role"] != identity.RoleAdmin || out["id"] != m.ID {
		t.Fatalf("transfer: %d %v", code, out)
	}
	if e.ownerID(t) != m.ID {
		t.Fatal("ownership did not move")
	}
	prev := decodeJSON(t, e.do(t, owner, http.MethodGet, "/v1/auth/me", ""))
	if prev["is_owner"] != false || prev["role"] != identity.RoleAdmin {
		t.Fatalf("previous owner = %v, want a plain admin", prev)
	}
	if e.auditRows(t, "user.owner", "transfer") != 1 {
		t.Fatal("transfer not audited")
	}
	// The previous owner is an admin, not the owner: no second transfer,
	// and the new owner may now demote them.
	x := e.member(t, "user_x", "x@beknown.work")
	if code, out := e.makeOwner(t, owner, x.ID, "x@beknown.work"); code != http.StatusForbidden || out["error"] != "not_owner" {
		t.Fatalf("transfer by the previous owner: %d %v", code, out)
	}
	mCookie := e.cookieFor(t, m.ID)
	if rec := e.do(t, mCookie, http.MethodPatch, "/v1/users/"+e.admin.ID, `{"role":"member"}`); rec.Code != http.StatusOK {
		t.Fatalf("new owner demotes the previous owner: %d %s", rec.Code, rec.Body.String())
	}
	// A blocked person cannot become the owner.
	if err := e.id.SetStatus(context.Background(), x.ID, identity.StatusBlocked, "x"); err != nil {
		t.Fatal(err)
	}
	if code, out := e.makeOwner(t, mCookie, x.ID, "x@beknown.work"); code != http.StatusConflict || out["error"] != "user_blocked" {
		t.Fatalf("to a blocked user: %d %v", code, out)
	}
}

// Only the owner in a browser session transfers: not another admin, not a
// member, not an operator token (whatever its scopes), not an agent.
func TestMakeOwnerRefusedToOthers(t *testing.T) {
	e := newAccessTestServer(t)
	ctx := context.Background()
	m := e.member(t, "user_m", "m@beknown.work")
	a := e.member(t, "user_a", "a@beknown.work")
	if err := e.id.SetRole(ctx, a.ID, identity.RoleAdmin); err != nil {
		t.Fatal(err)
	}

	if code, out := e.makeOwner(t, e.cookieFor(t, a.ID), m.ID, "m@beknown.work"); code != http.StatusForbidden || out["error"] != "not_owner" {
		t.Errorf("other admin: %d %v", code, out)
	}
	if code, out := e.makeOwner(t, e.cookieFor(t, m.ID), a.ID, "a@beknown.work"); code != http.StatusForbidden || out["error"] != "admin_only" {
		t.Errorf("member: %d %v", code, out)
	}
	if code, _ := e.makeOwner(t, nil, m.ID, "m@beknown.work"); code != http.StatusUnauthorized {
		t.Errorf("anonymous: %d", code)
	}
	// The owner's own operator token, with every scope.
	tok, _, err := e.id.CreateOperatorToken(ctx, e.admin.ID, "cli", []string{identity.ScopeRead, identity.ScopeWrite, identity.ScopeOwner}, 0, "test")
	if err != nil {
		t.Fatal(err)
	}
	if rec := e.bearer(t, tok, http.MethodPost, "/v1/users/"+m.ID+"/make-owner", `{"confirm":"m@beknown.work"}`); rec.Code != http.StatusForbidden {
		t.Errorf("owner's operator token: %d %s", rec.Code, rec.Body.String())
	}
	// The owner's agent.
	agentTok, _ := e.agentFor(t, e.admin.ID)
	if rec := e.bearer(t, agentTok, http.MethodPost, "/v1/users/"+m.ID+"/make-owner", `{"confirm":"m@beknown.work"}`); rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
		t.Errorf("owner's agent: %d %s", rec.Code, rec.Body.String())
	}
	if e.ownerID(t) != e.admin.ID || e.auditRows(t, "user.owner", "") != 0 {
		t.Fatal("a refused caller moved ownership")
	}
	// Defence in depth: the handler refuses an operator token too.
	if sc, ok := operatorScopeFor(http.MethodPost, "/v1/users/x/make-owner"); ok {
		t.Errorf("operatorScopeFor(make-owner) = %q, true; want refused", sc)
	}
}

// Admins make anyone an admin and demote admins; an operator token cannot
// change a role, whatever its scopes; nobody demotes or blocks the owner.
func TestUsersRoleChanges(t *testing.T) {
	e := newAccessTestServer(t)
	ctx := context.Background()
	owner := e.cookieFor(t, e.admin.ID)
	m := e.member(t, "user_m", "m@beknown.work")
	x := e.member(t, "user_x", "x@beknown.work")

	// The owner promotes m; m (an admin, not the owner) promotes x and
	// demotes x again.
	if rec := e.do(t, owner, http.MethodPatch, "/v1/users/"+m.ID, `{"role":"admin"}`); rec.Code != http.StatusOK {
		t.Fatalf("owner promotes m: %d %s", rec.Code, rec.Body.String())
	}
	mCookie := e.cookieFor(t, m.ID)
	if rec := e.do(t, mCookie, http.MethodPatch, "/v1/users/"+x.ID, `{"role":"admin"}`); rec.Code != http.StatusOK {
		t.Fatalf("admin promotes x: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do(t, mCookie, http.MethodPatch, "/v1/users/"+x.ID, `{"role":"member"}`); rec.Code != http.StatusOK {
		t.Fatalf("admin demotes x: %d %s", rec.Code, rec.Body.String())
	}
	if e.auditRows(t, "user.role", "") != 3 {
		t.Errorf("user.role rows = %d, want 3", e.auditRows(t, "user.role", ""))
	}
	for _, body := range []string{`{"role":"member"}`, `{"status":"blocked"}`} {
		rec := e.do(t, mCookie, http.MethodPatch, "/v1/users/"+e.admin.ID, body)
		if rec.Code != http.StatusConflict || decodeJSON(t, rec)["error"] != "owner_protected" {
			t.Errorf("admin %s on the owner: %d %s", body, rec.Code, rec.Body.String())
		}
	}

	// An operator token with every scope cannot change a role.
	tok, _, err := e.id.CreateOperatorToken(ctx, e.admin.ID, "cli", []string{identity.ScopeRead, identity.ScopeWrite, identity.ScopeOwner}, 0, "test")
	if err != nil {
		t.Fatal(err)
	}
	rec := e.bearer(t, tok, http.MethodPatch, "/v1/users/"+x.ID, `{"role":"admin"}`)
	if rec.Code != http.StatusForbidden || decodeJSON(t, rec)["error"] != "session_required" {
		t.Fatalf("operator token role change: %d %s", rec.Code, rec.Body.String())
	}
	if u, _ := e.id.GetUserByID(ctx, x.ID); u.Role != identity.RoleMember {
		t.Fatalf("operator token changed the role: %+v", u)
	}
}

// Personal sign-in (-clerk-owner-only) follows the stored owner: after a
// transfer the new owner signs in, and the previous owner no longer can.
func TestPersonalSignInFollowsTransfer(t *testing.T) {
	e := newAccessTestServer(t)
	e.srv.clerkOwnerOnly = true
	signIn := func(subject, email string) (int, map[string]any, *http.Cookie) {
		t.Helper()
		e.clerk.set(func(f *fakeClerk) { f.claims.Subject = subject; f.member.Email = email })
		rec := e.do(t, nil, http.MethodPost, "/v1/auth/clerk/session", `{"token":"good-token"}`)
		if rec.Code != http.StatusOK {
			return rec.Code, nil, nil
		}
		return rec.Code, decodeJSON(t, rec), sessionCookie(t, rec)
	}
	code, u, ownerCookie := signIn("user_owner", "owner@beknown.work")
	if code != http.StatusOK || u["id"] != e.admin.ID || u["is_owner"] != true {
		t.Fatalf("owner sign-in: %d %v", code, u)
	}
	// m signed in before (in organisation mode) and has an email; plain has
	// no email to sign in with.
	m := e.member(t, "user_m", "m@beknown.work")
	now := time.Now().UnixMilli()
	if _, err := e.db.Exec(`INSERT INTO users(id, username, password_hash, role, status, created_at, updated_at)
		VALUES('u_plain','plain','x','member','active',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := signIn("user_m", "m@beknown.work"); code != http.StatusForbidden {
		t.Fatalf("m before the transfer: %d", code)
	}
	if code, out := e.makeOwner(t, ownerCookie, "u_plain", "plain"); code != http.StatusConflict || out["error"] != "no_sign_in" {
		t.Fatalf("transfer to a user without an email: %d %v", code, out)
	}
	if code, out := e.makeOwner(t, ownerCookie, m.ID, "m@beknown.work"); code != http.StatusOK || out["is_owner"] != true {
		t.Fatalf("transfer: %d %v", code, out)
	}
	// The previous owner's session ends; the new owner signs in.
	if rec := e.do(t, ownerCookie, http.MethodGet, "/v1/auth/me", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("previous owner's session after the transfer: %d", rec.Code)
	}
	code, u, mCookie := signIn("user_m", "m@beknown.work")
	if code != http.StatusOK || u["id"] != m.ID || u["is_owner"] != true {
		t.Fatalf("new owner sign-in: %d %v", code, u)
	}
	if rec := e.do(t, mCookie, http.MethodGet, "/v1/users", ""); rec.Code != http.StatusOK {
		t.Fatalf("new owner GET /v1/users: %d %s", rec.Code, rec.Body.String())
	}
	if code, _, _ := signIn("user_owner", "owner@beknown.work"); code != http.StatusForbidden {
		t.Fatalf("previous owner sign-in after the transfer: %d, want 403", code)
	}
}

// The owner who left the Clerk organisation stays active (transfer first).
func TestClerkSyncSkipsOwner(t *testing.T) {
	e := newAccessTestServer(t)
	ctx := context.Background()
	m := e.member(t, "user_m", "m@beknown.work")
	if err := e.id.TransferOwnership(ctx, e.admin.ID, m.ID); err != nil {
		t.Fatal(err)
	}
	e.clerk.set(func(f *fakeClerk) {
		f.members = map[string]clerk.Member{"user_someone": {IsMember: true}}
	})
	if n := e.srv.runClerkSync(ctx); n != 0 {
		t.Errorf("blocked %d, want 0 (the only Clerk user is the owner)", n)
	}
	if u, _ := e.id.GetUserByID(ctx, m.ID); u.Status != identity.StatusActive || !u.Owner {
		t.Errorf("owner after the sync = %+v", u)
	}
}
