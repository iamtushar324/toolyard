package api

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/access"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
)

// TestGuestConversionRevokesCredentials: a member who becomes a guest (an
// invite, then a sign-in from outside the organisation) loses their
// identity key, here and at the registry, and their operator tokens; an
// admin cannot give a guest a key again.
func TestGuestConversionRevokesCredentials(t *testing.T) {
	e := guestEnv(t)
	ctx := context.Background()
	reg := withIdentityKeys(t, e)
	admin := e.cookieFor(t, e.admin.ID)
	m := e.member(t, "user_ext", "ext@partner.com")
	keyPath := "/v1/users/" + m.ID + "/identity-key"
	expectStatus(t, "provision", e.do(t, admin, http.MethodPost, keyPath, ""), http.StatusOK, "")
	st, err := e.srv.identityKeys.Status(ctx, m.ID)
	if err != nil || !st.HasKey || !reg.has(st.Fingerprint) {
		t.Fatalf("precondition: status %+v, %v", st, err)
	}
	opTok, _, err := e.id.CreateOperatorToken(ctx, m.ID, "cli", []string{"read"}, 0, e.admin.ID)
	if err != nil {
		t.Fatal(err)
	}

	e.invite(t, access.GuestInvite{Email: "ext@partner.com", Servers: []string{"github"}})
	rec := e.clerkSignIn(t)
	expectStatus(t, "guest sign-in", rec, http.StatusOK, "")
	if u := decodeJSON(t, rec); u["id"] != m.ID || u["role"] != identity.RoleGuest {
		t.Fatalf("signed in = %v, want %s as a guest", u, m.ID)
	}

	if has, err := e.srv.identityKeys.HasKey(ctx, m.ID); err != nil || has {
		t.Errorf("guest still has a key: %v %v", has, err)
	}
	if reg.has(st.Fingerprint) {
		t.Error("the guest's fingerprint is still in the registry")
	}
	if _, _, err := e.id.VerifyOperatorToken(ctx, opTok); !errors.Is(err, identity.ErrOperatorTokenInvalid) {
		t.Errorf("guest's operator token: %v, want invalid", err)
	}
	if e.auditRows(t, "operator_token.revoke", "guest") != 1 {
		t.Error("operator token revoke not audited")
	}
	expectStatus(t, "provision for a guest", e.do(t, admin, http.MethodPost, keyPath, ""), http.StatusConflict, "guest")
	guest := sessionCookie(t, rec)
	expectStatus(t, "guest reveal", e.do(t, guest, http.MethodPost, "/v1/me/identity-key/reveal", ""), http.StatusConflict, "guest")
}

// TestGuestIsNeverASkillContributor: a contributor row left from before
// the person became a guest opens none of the contributor routes, and an
// admin cannot name a guest a contributor.
func TestGuestIsNeverASkillContributor(t *testing.T) {
	e := newSkillsSharingEnv(t)
	// Precondition: the contributor reaches their own source.
	if rec := e.do(t, e.contribCk, http.MethodGet, "/v1/skills/sources/src_mine", ""); rec.Code != http.StatusOK {
		t.Fatalf("contributor before: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := e.db.Exec(`UPDATE users SET is_guest = 1 WHERE id = ?`, e.contrib.ID); err != nil {
		t.Fatal(err)
	}
	for _, rt := range []struct{ method, path, body string }{
		{http.MethodGet, "/v1/skills/admin/sources", ""},
		{http.MethodGet, "/v1/skills/sources/src_mine", ""},
		{http.MethodPatch, "/v1/skills/sources/src_mine", `{"enabled":false}`},
		{http.MethodPut, "/v1/skills/sources/src_mine/skills/sharing", `{"path":"pdf","mode":"nobody"}`},
		{http.MethodDelete, "/v1/skills/sources/src_mine", ""},
	} {
		if rec := e.do(t, e.contribCk, rt.method, rt.path, rt.body); rec.Code != http.StatusForbidden {
			t.Errorf("guest %s %s: %d %s", rt.method, rt.path, rec.Code, rec.Body.String())
		}
	}
	if got := skillsJSON(t, e.do(t, e.contribCk, http.MethodGet, skillMePath, "")); got["contributor"] != false {
		t.Errorf("guest /me = %v, want contributor false", got)
	}
	rec := e.do(t, e.adminCookie, http.MethodPut, skillContributorsPrefix+e.contrib.ID, "")
	if rec.Code != http.StatusConflict {
		t.Errorf("name a guest a contributor: %d %s", rec.Code, rec.Body.String())
	}
}

// TestGuestInviteRefusesMemPalace: the managed MemPalace upstream is a
// server row like any other, but its data is the organisation's: no invite
// may name it.
func TestGuestInviteRefusesMemPalace(t *testing.T) {
	e := guestEnv(t)
	e.seedServer(t, "mempalace", "connected", true)
	admin := e.cookieFor(t, e.admin.ID)
	expectStatus(t, "mempalace", e.do(t, admin, http.MethodPost, "/v1/guests", `{"email":"x@partner.com","servers":["github","mempalace"]}`), http.StatusBadRequest, "")
}

// TestGuestInviteDeleteDecodesOnce: the email in the path is decoded once,
// so a literal percent sign works and an escaped escape names no other
// invite.
func TestGuestInviteDeleteDecodesOnce(t *testing.T) {
	e := guestEnv(t)
	admin := e.cookieFor(t, e.admin.ID)
	e.invite(t, access.GuestInvite{Email: "guest%ops@example.org", Servers: []string{"github"}})
	e.invite(t, access.GuestInvite{Email: "a@example.org", Servers: []string{"github"}})

	// "%2541" is "%41" after one decoding: no such invite, and a@ stays.
	expectStatus(t, "escaped escape", e.do(t, admin, http.MethodDelete, "/v1/guests/%2541@example.org", ""), http.StatusNotFound, "")
	if inv, _ := e.access.GuestInvite(context.Background(), "a@example.org"); inv == nil {
		t.Fatalf("deleting %s removed a@example.org", "%2541@…")
	}
	// A literal % in the address, escaped once by the client.
	expectStatus(t, "literal percent", e.do(t, admin, http.MethodDelete, "/v1/guests/guest%25ops@example.org", ""), http.StatusOK, "")
	if inv, _ := e.access.GuestInvite(context.Background(), "guest%ops@example.org"); inv != nil {
		t.Errorf("%s still invited", "guest%ops@example.org")
	}
}
