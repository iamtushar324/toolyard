package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/clerk"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
)

func decodeJSON(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return out
}

func sessionCookie(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName && c.Value != "" {
			return c
		}
	}
	t.Fatalf("no %s cookie in response", sessionCookieName)
	return nil
}

func TestAuthConfig(t *testing.T) {
	e := newAccessTestServer(t)
	rec := e.do(t, nil, http.MethodGet, "/v1/auth/config", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	got := decodeJSON(t, rec)
	c, _ := got["clerk"].(map[string]any)
	if c["publishable_key"] != "pk_test_Y2xlcmsuZXhhbXBsZS50ZXN0JA" || c["frontend_api"] != "clerk.example.test" || got["password_login"] != true {
		t.Errorf("config = %v", got)
	}

	// Clerk off: null, and the session route is not there.
	e.srv.clerk = nil
	got = decodeJSON(t, e.do(t, nil, http.MethodGet, "/v1/auth/config", ""))
	if got["clerk"] != nil || got["password_login"] != true {
		t.Errorf("config with clerk off = %v", got)
	}
	if rec := e.do(t, nil, http.MethodPost, "/v1/auth/clerk/session", `{"token":"good-token"}`); rec.Code != http.StatusNotFound {
		t.Errorf("clerk off: status %d, want 404", rec.Code)
	}
}

func TestAuthClerkSessionHappyPath(t *testing.T) {
	e := newAccessTestServer(t)
	rec := e.do(t, nil, http.MethodPost, "/v1/auth/clerk/session", `{"token":"good-token"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	u := decodeJSON(t, rec)
	if u["role"] != identity.RoleMember || u["status"] != identity.StatusActive || u["auth"] != identity.AuthClerk ||
		u["email"] != "ada@beknown.work" || u["display_name"] != "Ada Lovelace" || u["username"] != "ada" ||
		u["avatar_url"] != "https://img.clerk.com/ada" {
		t.Errorf("user = %v", u)
	}
	cookie := sessionCookie(t, rec)

	me := e.do(t, cookie, http.MethodGet, "/v1/auth/me", "")
	if me.Code != http.StatusOK {
		t.Fatalf("/me status %d: %s", me.Code, me.Body.String())
	}
	got := decodeJSON(t, me)
	if got["id"] != u["id"] || got["role"] != identity.RoleMember || got["auth"] != identity.AuthClerk {
		t.Errorf("/me = %v", got)
	}
	if n := e.auditRows(t, "user.login", "clerk"); n != 1 {
		t.Errorf("user.login/clerk audit rows = %d, want 1", n)
	}

	// Second sign-in: same user, profile refreshed.
	e.clerk.set(func(f *fakeClerk) { f.member.FirstName, f.member.LastName = "Ada", "King" })
	again := decodeJSON(t, e.do(t, nil, http.MethodPost, "/v1/auth/clerk/session", `{"token":"good-token"}`))
	if again["id"] != u["id"] || again["display_name"] != "Ada King" {
		t.Errorf("second sign-in = %v", again)
	}
}

func TestAuthClerkSessionErrors(t *testing.T) {
	e := newAccessTestServer(t)
	post := func(body string) *httptest.ResponseRecorder {
		return e.do(t, nil, http.MethodPost, "/v1/auth/clerk/session", body)
	}
	expect := func(name string, rec *httptest.ResponseRecorder, status int, code string) {
		t.Helper()
		if rec.Code != status {
			t.Errorf("%s: status %d, want %d (%s)", name, rec.Code, status, rec.Body.String())
			return
		}
		if code != "" && decodeJSON(t, rec)["error"] != code {
			t.Errorf("%s: body %s, want error %q", name, rec.Body.String(), code)
		}
	}

	expect("bad token", post(`{"token":"forged"}`), http.StatusUnauthorized, "invalid_token")
	expect("empty token", post(`{"token":""}`), http.StatusBadRequest, "")
	expect("unknown field", post(`{"jwt":"x"}`), http.StatusBadRequest, "")
	expect("GET", e.do(t, nil, http.MethodGet, "/v1/auth/clerk/session", ""), http.StatusMethodNotAllowed, "")

	e.clerk.set(func(f *fakeClerk) { f.verifyErr = clerk.ErrUnavailable })
	expect("jwks down", post(`{"token":"good-token"}`), http.StatusServiceUnavailable, "clerk_unavailable")
	e.clerk.set(func(f *fakeClerk) { f.verifyErr = nil })

	e.clerk.set(func(f *fakeClerk) { f.memberErr = clerk.ErrNotMember })
	expect("not member", post(`{"token":"good-token"}`), http.StatusForbidden, "not_org_member")
	if n := e.auditRows(t, "user.login", "not_org_member"); n != 1 {
		t.Errorf("not_org_member audit rows = %d", n)
	}
	e.clerk.set(func(f *fakeClerk) { f.memberErr = clerk.ErrUnavailable })
	expect("api down", post(`{"token":"good-token"}`), http.StatusServiceUnavailable, "clerk_unavailable")
	e.clerk.set(func(f *fakeClerk) { f.memberErr = nil })

	// Sign in once, block, sign in again.
	ok := post(`{"token":"good-token"}`)
	expect("first sign-in", ok, http.StatusOK, "")
	uid, _ := decodeJSON(t, ok)["id"].(string)
	if err := e.id.SetStatus(t.Context(), uid, identity.StatusBlocked, "left_org"); err != nil {
		t.Fatal(err)
	}
	expect("blocked", post(`{"token":"good-token"}`), http.StatusForbidden, "blocked")
	if n := e.auditRows(t, "user.login", "blocked"); n != 1 {
		t.Errorf("blocked audit rows = %d", n)
	}
	// No user row was created or modified for a non-member; the blocked
	// user still exists exactly once.
	users, _ := e.id.ListUsers(t.Context())
	if len(users) != 2 {
		t.Errorf("users after errors = %d, want 2 (admin + ada)", len(users))
	}

	// Same-origin CSRF rules apply: no X-Requested-With -> HardenAPI 403.
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/clerk/session", strings.NewReader(`{"token":"good-token"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "X-Requested-With") {
		t.Errorf("without CSRF header: %d %s", rec.Code, rec.Body.String())
	}
}

func TestAuthClerkSessionLinksOwner(t *testing.T) {
	e := newAccessTestServer(t)
	e.clerk.set(func(f *fakeClerk) {
		f.claims.Subject = "user_owner"
		f.member.Email = "Owner@Beknown.work"
	})
	rec := e.do(t, nil, http.MethodPost, "/v1/auth/clerk/session", `{"token":"good-token"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	u := decodeJSON(t, rec)
	if u["id"] != e.admin.ID || u["role"] != identity.RoleAdmin || u["auth"] != identity.AuthClerk || u["username"] != "admin" {
		t.Errorf("owner sign-in = %v (admin id %s)", u, e.admin.ID)
	}
	// The linked owner is an admin: an admin-only route works with the cookie.
	cookie := sessionCookie(t, rec)
	if rec := e.do(t, cookie, http.MethodGet, "/v1/users", ""); rec.Code != http.StatusOK {
		t.Errorf("owner GET /v1/users: %d %s", rec.Code, rec.Body.String())
	}
}

func TestAuthClerkSessionThrottle(t *testing.T) {
	e := newAccessTestServer(t)
	for i := 0; i < clerkSessionMaxAttempts; i++ {
		if rec := e.do(t, nil, http.MethodPost, "/v1/auth/clerk/session", `{"token":"forged"}`); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status %d", i, rec.Code)
		}
	}
	if rec := e.do(t, nil, http.MethodPost, "/v1/auth/clerk/session", `{"token":"good-token"}`); rec.Code != http.StatusTooManyRequests {
		t.Errorf("attempt %d: status %d, want 429", clerkSessionMaxAttempts+1, rec.Code)
	}
}

func TestPasswordLoginBlocked(t *testing.T) {
	e := newAccessTestServer(t)
	// Promote a member so the admin can be blocked.
	m := e.member(t, "user_m", "m@beknown.work")
	if err := e.id.SetRole(t.Context(), m.ID, identity.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if err := e.id.SetStatus(t.Context(), e.admin.ID, identity.StatusBlocked, "x"); err != nil {
		t.Fatal(err)
	}
	rec := e.do(t, nil, http.MethodPost, "/v1/auth/login", `{"Username":"admin","Password":"long-enough-password"}`)
	if rec.Code != http.StatusForbidden || decodeJSON(t, rec)["error"] != "blocked" {
		t.Errorf("blocked password login: %d %s", rec.Code, rec.Body.String())
	}
	rec = e.do(t, nil, http.MethodPost, "/v1/auth/login", `{"Username":"admin","Password":"wrong-password-here"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("blocked + wrong password: %d, want 401", rec.Code)
	}
	// Existing sessions of a blocked user stop working.
	cookie := e.cookieFor(t, e.admin.ID)
	if rec := e.do(t, cookie, http.MethodGet, "/v1/auth/me", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("blocked user's cookie: %d, want 401", rec.Code)
	}
}
