package mcpauth

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// beginFor starts an authorization request for the fixture's client.
func (f *fixture) beginFor(t *testing.T, clientID, redirect, challenge string) *AuthorizeRequest {
	t.Helper()
	req, aerr := f.svc.BeginAuthorize(context.Background(), authorizeQuery(clientID, redirect, challenge, nil), "198.51.100.7")
	if aerr != nil {
		t.Fatalf("authorize: %v", aerr)
	}
	return req
}

// TestApproveStoresAReturnTicketNotACode: the person's yes stores a return
// ticket (its hash only) and mints no code; the code comes when the link
// is used, and its 60 seconds start then, while the sign-in time the token
// endpoint records stays the consent's.
func TestApproveStoresAReturnTicketNotACode(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	redirect := "https://client.example/cb"
	c := f.register(t, "none", redirect)
	verifier, challenge := pkcePair(t)
	req := f.beginFor(t, c.ClientID, redirect, challenge)
	consentAt := f.clock.UnixMilli()
	ticket, err := f.svc.Approve(ctx, req.ID, f.user, Consent{Nickname: "Laptop"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ticket, returnPrefix) || !ValidReturnTicket(ticket) {
		t.Fatalf("ticket %q has the wrong form", ticket)
	}
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := f.db.QueryRow(q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(`SELECT COUNT(*) FROM mcp_oauth_codes`); n != 0 {
		t.Fatalf("approve minted %d codes before the return link was used", n)
	}
	if count(`SELECT COUNT(*) FROM mcp_oauth_return_tickets WHERE ticket_hash = ?`, hashSecret(ticket)) != 1 {
		t.Fatal("the ticket's hash is not stored")
	}
	if n := count(`SELECT COUNT(*) FROM mcp_oauth_return_tickets WHERE ticket_hash = ? OR redirect_uri LIKE ? OR state LIKE ?`,
		ticket, "%"+ticket+"%", "%"+ticket+"%"); n != 0 {
		t.Fatal("the raw ticket is stored")
	}
	var created, expires int64
	if err := f.db.QueryRow(`SELECT created_at, expires_at FROM mcp_oauth_return_tickets WHERE ticket_hash = ?`, hashSecret(ticket)).Scan(&created, &expires); err != nil {
		t.Fatal(err)
	}
	if created != consentAt || expires != consentAt+ReturnTTL.Milliseconds() {
		t.Fatalf("ticket times %d..%d, want %d..%d", created, expires, consentAt, consentAt+ReturnTTL.Milliseconds())
	}

	// The page reads it without using it.
	info, err := f.svc.ReturnTicket(ctx, ticket)
	if err != nil || info.Client.ID != c.ClientID || info.RedirectURI != redirect || info.Nickname != "Laptop" || info.Account != f.user.Label() {
		t.Fatalf("return info: %+v %v", info, err)
	}
	if _, err := f.svc.ReturnTicket(ctx, ticket); err != nil {
		t.Fatalf("reading the ticket twice: %v", err)
	}

	// Nine minutes later the link still works, and the code it mints has
	// its own 60 seconds from now.
	f.clock = f.clock.Add(9 * time.Minute)
	redeemAt := f.clock.UnixMilli()
	loc, err := f.svc.RedeemReturn(ctx, ticket)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(loc)
	if err != nil || !strings.HasPrefix(loc, redirect+"?") || u.Query().Get("state") != "st-123" || u.Query().Get("iss") != testPublic || u.Query().Get("code") == "" {
		t.Fatalf("return redirect %q", loc)
	}
	var codeCreated, codeExpires int64
	if err := f.db.QueryRow(`SELECT created_at, expires_at FROM mcp_oauth_codes WHERE code_hash = ?`, hashSecret(u.Query().Get("code"))).Scan(&codeCreated, &codeExpires); err != nil {
		t.Fatal(err)
	}
	if codeCreated != consentAt || codeExpires != redeemAt+CodeTTL.Milliseconds() {
		t.Fatalf("code times: created %d (want the consent's %d), expires %d (want %d)", codeCreated, consentAt, codeExpires, redeemAt+CodeTTL.Milliseconds())
	}
	f.clock = f.clock.Add(CodeTTL - time.Second)
	tok, oerr := f.svc.Exchange(ctx, codeForm(u.Query().Get("code"), redirect, verifier), ClientAuth{ClientID: c.ClientID})
	if oerr != nil {
		t.Fatalf("exchange: %v", oerr)
	}
	if ag, err := f.svc.VerifyAccessToken(ctx, tok.AccessToken); err != nil || ag.Name != "Laptop" {
		t.Fatalf("verify: %+v %v", ag, err)
	}
	var authTime int64
	if err := f.db.QueryRow(`SELECT MIN(auth_time) FROM mcp_oauth_tokens`).Scan(&authTime); err != nil || authTime != consentAt {
		t.Fatalf("auth_time %d, want the consent's %d (%v)", authTime, consentAt, err)
	}
}

// TestReturnLinkWorksOnce: a second use, an expired link and an unknown or
// malformed one are refused, and the page names the app for a used or
// expired link.
func TestReturnLinkWorksOnce(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	redirect := "https://client.example/cb"
	c := f.register(t, "none", redirect)
	_, challenge := pkcePair(t)

	ticket, err := f.svc.Approve(ctx, f.beginFor(t, c.ClientID, redirect, challenge).ID, f.user, Consent{Nickname: "Once"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.RedeemReturn(ctx, ticket); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.RedeemReturn(ctx, ticket); !errors.Is(err, ErrReturnUsed) {
		t.Fatalf("second use: %v, want ErrReturnUsed", err)
	}
	if info, err := f.svc.ReturnTicket(ctx, ticket); !errors.Is(err, ErrReturnUsed) || info == nil || info.Client.ID != c.ClientID {
		t.Fatalf("page of a used link: %+v %v", info, err)
	}
	var codes int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM mcp_oauth_codes`).Scan(&codes); err != nil || codes != 1 {
		t.Fatalf("%d codes after two uses, want 1 (%v)", codes, err)
	}

	ticket, err = f.svc.Approve(ctx, f.beginFor(t, c.ClientID, redirect, challenge).ID, f.user, Consent{Nickname: "Late"})
	if err != nil {
		t.Fatal(err)
	}
	f.clock = f.clock.Add(ReturnTTL + time.Second)
	if info, err := f.svc.ReturnTicket(ctx, ticket); !errors.Is(err, ErrReturnExpired) || info == nil {
		t.Fatalf("page of an expired link: %+v %v", info, err)
	}
	if _, err := f.svc.RedeemReturn(ctx, ticket); !errors.Is(err, ErrReturnExpired) {
		t.Fatalf("expired link: %v, want ErrReturnExpired", err)
	}

	for _, bad := range []string{"", "tyrl_short", returnPrefix + strings.Repeat("A", 43), "tyoc_" + strings.Repeat("A", 43), returnPrefix + strings.Repeat("A", 42) + "/"} {
		if _, err := f.svc.RedeemReturn(ctx, bad); !errors.Is(err, ErrReturnUnknown) {
			t.Errorf("RedeemReturn(%q): %v, want ErrReturnUnknown", bad, err)
		}
		if info, err := f.svc.ReturnTicket(ctx, bad); !errors.Is(err, ErrReturnUnknown) || info != nil {
			t.Errorf("ReturnTicket(%q): %+v %v, want ErrReturnUnknown", bad, info, err)
		}
	}
}

// TestANewerConsentOrARevocationEndsTheReturnLink: connecting the app
// again ends the unused return link of the earlier consent, and a
// connection revoked after the consent gives no code.
func TestANewerConsentOrARevocationEndsTheReturnLink(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	redirect := "https://client.example/cb"
	c := f.register(t, "none", redirect)
	_, challenge := pkcePair(t)

	first, err := f.svc.Approve(ctx, f.beginFor(t, c.ClientID, redirect, challenge).ID, f.user, Consent{Nickname: "First"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.svc.Approve(ctx, f.beginFor(t, c.ClientID, redirect, challenge).ID, f.user, Consent{Nickname: "Second"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.RedeemReturn(ctx, first); !errors.Is(err, ErrReturnExpired) {
		t.Fatalf("the earlier consent's link: %v, want ErrReturnExpired", err)
	}
	if _, err := f.svc.RedeemReturn(ctx, second); err != nil {
		t.Fatalf("the newest link: %v", err)
	}

	third, err := f.svc.Approve(ctx, f.beginFor(t, c.ClientID, redirect, challenge).ID, f.user, Consent{Nickname: "Third"})
	if err != nil {
		t.Fatal(err)
	}
	apps, err := f.svc.ListApps(ctx, f.user.ID)
	if err != nil || len(apps) != 1 {
		t.Fatalf("apps: %+v %v", apps, err)
	}
	app, err := f.svc.App(ctx, apps[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.svc.RevokeApp(ctx, app, f.user); err != nil {
		t.Fatal(err)
	}
	if info, err := f.svc.ReturnTicket(ctx, third); !errors.Is(err, ErrReturnRevoked) || info == nil {
		t.Fatalf("page after a revoke: %+v %v", info, err)
	}
	var before int
	_ = f.db.QueryRow(`SELECT COUNT(*) FROM mcp_oauth_codes`).Scan(&before)
	if _, err := f.svc.RedeemReturn(ctx, third); !errors.Is(err, ErrReturnRevoked) {
		t.Fatalf("link after a revoke: %v, want ErrReturnRevoked", err)
	}
	var after int
	_ = f.db.QueryRow(`SELECT COUNT(*) FROM mcp_oauth_codes`).Scan(&after)
	if after != before {
		t.Fatal("a revoked connection's return link minted a code")
	}
	if _, err := f.svc.RedeemReturn(ctx, third); !errors.Is(err, ErrReturnUsed) {
		t.Fatalf("the refused link is not used up: %v", err)
	}
}

// TestRacingReturnLinkUsesMintOneCode: of several uses of one link at
// once, exactly one gets a code.
func TestRacingReturnLinkUsesMintOneCode(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	redirect := "https://client.example/cb"
	c := f.register(t, "none", redirect)
	_, challenge := pkcePair(t)
	ticket, err := f.svc.Approve(ctx, f.beginFor(t, c.ClientID, redirect, challenge).ID, f.user, Consent{Nickname: "Race"})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins, used := 0, 0
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.svc.RedeemReturn(ctx, ticket)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				wins++
			case errors.Is(err, ErrReturnUsed):
				used++
			default:
				t.Errorf("racing use: %v", err)
			}
		}()
	}
	wg.Wait()
	var codes int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM mcp_oauth_codes`).Scan(&codes); err != nil {
		t.Fatal(err)
	}
	if wins != 1 || codes != 1 {
		t.Fatalf("%d uses got a code, %d codes minted (%d refused as used), want exactly one", wins, codes, used)
	}
}
