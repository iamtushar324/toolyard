package oauth

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestRefreshUserTokenAfter401: the gateway's answer to a 401. A row with a
// working refresh token gets a new access token and no mark; a row with no
// refresh token, or one the IdP calls invalid_grant, is marked
// needs_reauth; a row already marked is reported as such without an IdP
// call; a transient IdP failure is returned and marks nothing.
func TestRefreshUserTokenAfter401(t *testing.T) {
	f := newUserFixture(t)
	ctx := context.Background()
	f.connect(t, "linear", "u_ada", "ada1")
	f.connect(t, "linear", "u_bob", "bob1")

	if err := f.svc.RefreshUserToken(ctx, "linear", "u_ada"); err != nil {
		t.Fatalf("refreshable: %v", err)
	}
	if got := f.svc.currentUserBearer("linear", "u_ada"); got != "at-refreshed-1" {
		t.Fatalf("bearer after 401 refresh = %q", got)
	}
	if ok, _ := f.svc.UserConnected(ctx, "linear", "u_ada"); !ok || len(f.push.notified()) != 0 {
		t.Fatal("refreshable 401 marked or notified")
	}

	// Transient: a 500 from the IdP. Nothing marked, error returned.
	f.idp.fail500("rt-bob1")
	if err := f.svc.RefreshUserToken(ctx, "linear", "u_bob"); err == nil || errors.Is(err, ErrNeedsReauth) {
		t.Fatalf("transient failure = %v", err)
	}
	bob, _ := f.svc.GetUserToken(ctx, "linear", "u_bob")
	if bob.State != StateActive || bob.AccessToken != "at-bob1" {
		t.Fatalf("bob after transient failure = %+v", bob)
	}
	f.idp.unfail("rt-bob1")

	// No refresh token: marked.
	if _, err := f.db.Exec(`UPDATE oauth_user_tokens SET refresh_token_enc = NULL WHERE user_id = 'u_bob'`); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.RefreshUserToken(ctx, "linear", "u_bob"); !errors.Is(err, ErrNeedsReauth) {
		t.Fatalf("no refresh token = %v", err)
	}
	bob, _ = f.svc.GetUserToken(ctx, "linear", "u_bob")
	if bob.State != StateNeedsReauth || !strings.Contains(bob.LastError, "no refresh token") {
		t.Fatalf("bob after no-refresh 401 = %+v", bob)
	}
	if got := f.push.notified(); len(got) != 1 || got[0] != "u_bob" {
		t.Fatalf("notified = %v", got)
	}
	// Already marked: no IdP call, same answer.
	n := len(f.idp.refreshed())
	if err := f.svc.RefreshUserToken(ctx, "linear", "u_bob"); !errors.Is(err, ErrNeedsReauth) || len(f.idp.refreshed()) != n {
		t.Fatalf("already marked: err=%v idp calls=%d", err, len(f.idp.refreshed())-n)
	}
	// invalid_grant: marked.
	f.connect(t, "linear", "u_bob", "bob2")
	f.idp.reject("rt-bob2")
	if err := f.svc.RefreshUserToken(ctx, "linear", "u_bob"); !errors.Is(err, ErrNeedsReauth) {
		t.Fatalf("invalid_grant = %v", err)
	}
	if err := f.svc.RefreshUserToken(ctx, "linear", "u_nobody"); !errors.Is(err, ErrNoUserToken) {
		t.Fatalf("unknown user = %v", err)
	}
}

// TestFreshenUserToken: before a dial, only a token whose access expiry has
// passed is refreshed; one still alive is left to the refresher's schedule;
// one expired with nothing to refresh with is marked.
func TestFreshenUserToken(t *testing.T) {
	f := newUserFixture(t)
	ctx := context.Background()
	f.connect(t, "linear", "u_ada", "ada1")
	if err := f.svc.FreshenUserToken(ctx, "linear", "u_ada"); err != nil || len(f.idp.refreshed()) != 0 {
		t.Fatalf("alive token: err=%v idp calls=%d", err, len(f.idp.refreshed()))
	}
	past := time.Now().Add(-time.Minute).UnixMilli()
	if _, err := f.db.Exec(`UPDATE oauth_user_tokens SET access_expires_at = ? WHERE user_id = 'u_ada'`, past); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.FreshenUserToken(ctx, "linear", "u_ada"); err != nil {
		t.Fatalf("expired with refresh: %v", err)
	}
	ada, _ := f.svc.GetUserToken(ctx, "linear", "u_ada")
	if ada.AccessToken != "at-refreshed-1" || ada.AccessExpiresAt.Before(time.Now()) || ada.State != StateActive {
		t.Fatalf("ada after freshen = %+v", ada)
	}
	if f.svc.currentUserBearer("linear", "u_ada") != "at-refreshed-1" {
		t.Fatal("bearer cache not updated by freshen")
	}
	// Expired, no refresh token: marked, ErrNeedsReauth.
	f.idp.noExpiry = true
	f.connect(t, "linear", "u_bob", "bob1")
	if _, err := f.db.Exec(`UPDATE oauth_user_tokens SET refresh_token_enc = NULL, access_expires_at = ? WHERE user_id = 'u_bob'`, past); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.FreshenUserToken(ctx, "linear", "u_bob"); !errors.Is(err, ErrNeedsReauth) {
		t.Fatalf("expired without refresh = %v", err)
	}
	bob, _ := f.svc.GetUserToken(ctx, "linear", "u_bob")
	if bob.State != StateNeedsReauth {
		t.Fatalf("bob = %+v", bob)
	}
	// Expired, IdP down: error, not marked.
	f.idp.noExpiry = false
	f.connect(t, "linear", "u_bob", "bob2")
	if _, err := f.db.Exec(`UPDATE oauth_user_tokens SET access_expires_at = ? WHERE user_id = 'u_bob'`, past); err != nil {
		t.Fatal(err)
	}
	f.idp.fail500("rt-bob2")
	if err := f.svc.FreshenUserToken(ctx, "linear", "u_bob"); err == nil || errors.Is(err, ErrNeedsReauth) {
		t.Fatalf("expired, IdP down = %v", err)
	}
	bob, _ = f.svc.GetUserToken(ctx, "linear", "u_bob")
	if bob.State != StateActive {
		t.Fatalf("bob marked on a transient failure: %+v", bob)
	}
}

// TestRefreshDoesNotResurrectDisconnect: a refresh that is waiting on the
// provider when the person disconnects does not bring the row back. The
// disconnect waits for the person's lock; the refresh then writes (its row
// is still there) and the disconnect deletes it. Agents no longer act as
// the person afterwards.
func TestRefreshDoesNotResurrectDisconnect(t *testing.T) {
	f := newUserFixture(t)
	ctx := context.Background()
	f.connect(t, "linear", "u_ada", "ada1")

	gate := make(chan struct{})
	f.idp.gateRefresh(gate)
	var wg sync.WaitGroup
	var refreshErr, disconnectErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, refreshErr = f.svc.RefreshUser(ctx, "linear", "u_ada")
	}()
	// Let the refresh reach the provider, then disconnect while it waits.
	time.Sleep(50 * time.Millisecond)
	go func() {
		defer wg.Done()
		disconnectErr = f.svc.DisconnectUser(ctx, "linear", "u_ada")
	}()
	time.Sleep(50 * time.Millisecond)
	close(gate)
	wg.Wait()
	if refreshErr != nil || disconnectErr != nil {
		t.Fatalf("refresh=%v disconnect=%v", refreshErr, disconnectErr)
	}
	if tok, _ := f.svc.GetUserToken(ctx, "linear", "u_ada"); tok != nil {
		t.Fatalf("row resurrected after disconnect: %+v", tok)
	}
	if f.svc.currentUserBearer("linear", "u_ada") != "" {
		t.Fatal("bearer still served after disconnect")
	}
	if ok, _ := f.svc.UserConnected(ctx, "linear", "u_ada"); ok {
		t.Fatal("still connected")
	}
}

// TestRefreshWriteSuperseded: the refresh writes back only while the row
// still carries the refresh token it read; a row replaced meanwhile (a
// fresh sign-in) is left alone and the cache keeps the fresh token.
func TestRefreshWriteSuperseded(t *testing.T) {
	f := newUserFixture(t)
	ctx := context.Background()
	f.connect(t, "linear", "u_ada", "ada1")
	cur, _ := f.svc.GetUserToken(ctx, "linear", "u_ada")
	// A fresh sign-in replaces the row (new refresh token) while an old
	// refresh is in flight.
	f.connect(t, "linear", "u_ada", "ada2")
	stale := &UserTokenRecord{UpstreamName: "linear", UserID: "u_ada", AccessToken: "at-stale", RefreshToken: "rt-ada1", State: StateActive}
	if err := f.svc.updateUserTokenIfUnchanged(ctx, stale, cur.refreshEnc); !errors.Is(err, ErrSuperseded) {
		t.Fatalf("stale write = %v", err)
	}
	now, _ := f.svc.GetUserToken(ctx, "linear", "u_ada")
	if now.AccessToken != "at-ada2" || now.RefreshToken != "rt-ada2" {
		t.Fatalf("row after stale write = %+v", now)
	}
	if f.svc.currentUserBearer("linear", "u_ada") != "at-ada2" {
		t.Fatal("cache overwritten by the stale write")
	}
	// The refresher treats a superseded refresh as nothing to report.
	f.idp.gateRefresh(nil)
	due := time.Now().Add(30 * time.Second).UnixMilli()
	if _, err := f.db.Exec(`UPDATE oauth_user_tokens SET access_expires_at = ?`, due); err != nil {
		t.Fatal(err)
	}
	f.svc.refreshUserTokens(ctx, time.Now())
	after, _ := f.svc.GetUserToken(ctx, "linear", "u_ada")
	if after.RefreshFailures != 0 || after.State != StateActive || after.AccessToken != "at-refreshed-1" {
		t.Fatalf("after a normal refresh = %+v", after)
	}
}
