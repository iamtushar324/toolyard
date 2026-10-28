package mcpauth

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// waitFixture is a fixture with one registered client and its PKCE pair.
type waitFixture struct {
	*fixture
	redirect            string
	clientID            string
	verifier, challenge string
}

func newWaitFixture(t *testing.T) *waitFixture {
	t.Helper()
	f := &waitFixture{fixture: newFixture(t), redirect: "https://client.example/cb"}
	f.clientID = f.register(t, "none", f.redirect).ClientID
	f.verifier, f.challenge = pkcePair(t)
	return f
}

func (f *waitFixture) begin(t *testing.T) *AuthorizeRequest {
	t.Helper()
	return f.beginFor(t, f.clientID, f.redirect, f.challenge)
}

func (f *waitFixture) count(t *testing.T, q string, args ...any) int {
	t.Helper()
	var n int
	if err := f.db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (f *waitFixture) wantStatus(t *testing.T, req *AuthorizeRequest, want string) *WaitInfo {
	t.Helper()
	info, err := f.svc.Wait(context.Background(), req.ID, req.Waiter)
	if err != nil || info.Status != want {
		t.Fatalf("Wait: %+v %v, want %s", info, err, want)
	}
	return info
}

// TestBeginAuthorizeGivesTheStopPageAWaiter: each request gets its own
// waiter secret, returned once; only its SHA-256 is stored, and the
// consent screen's view of the request (PendingRequest) never has it.
func TestBeginAuthorizeGivesTheStopPageAWaiter(t *testing.T) {
	f := newWaitFixture(t)
	ctx := context.Background()
	req := f.begin(t)
	if !ValidWaiter(req.Waiter) || !strings.HasPrefix(req.Waiter, waiterPrefix) {
		t.Fatalf("waiter %q has the wrong form", req.Waiter)
	}
	var stored sql.NullString
	if err := f.db.QueryRow(`SELECT waiter_hash FROM mcp_oauth_requests WHERE id = ?`, req.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored.String != hashSecret(req.Waiter) {
		t.Fatalf("stored waiter %q, want its SHA-256", stored.String)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM mcp_oauth_requests WHERE waiter_hash = ? OR id = ? OR state = ?`, req.Waiter, req.Waiter, req.Waiter); n != 0 {
		t.Fatal("the raw waiter is stored")
	}
	if strings.Contains(req.ID, req.Waiter) || strings.Contains(req.Waiter, req.ID) {
		t.Fatal("the waiter and the request id overlap")
	}
	pending, err := f.svc.PendingRequest(ctx, req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if pending.Waiter != "" {
		t.Fatal("PendingRequest returns the waiter")
	}
	if other := f.begin(t); other.Waiter == req.Waiter {
		t.Fatal("two requests share a waiter")
	}
	for _, bad := range []string{"", "tywt_short", waiterPrefix + strings.Repeat("A", 42) + "/", returnPrefix + strings.Repeat("A", 43)} {
		if ValidWaiter(bad) {
			t.Errorf("ValidWaiter(%q) = true", bad)
		}
	}
}

// TestWaitRevealsNothingWithoutTheWaiter: a missing, malformed or wrong
// waiter, and an unknown request, all get the same answer (expired, no
// app), before and after the approval; Continue with them is refused the
// same way and mints nothing.
func TestWaitRevealsNothingWithoutTheWaiter(t *testing.T) {
	f := newWaitFixture(t)
	ctx := context.Background()
	req := f.begin(t)
	other := f.begin(t)
	wrong := []struct{ id, waiter string }{
		{req.ID, ""},
		{req.ID, "tywt_short"},
		{req.ID, waiterPrefix + strings.Repeat("A", 43)},
		{req.ID, other.Waiter},
		{strings.Repeat("A", 43), req.Waiter},
		{"", req.Waiter},
	}
	check := func(stage string) {
		t.Helper()
		for _, w := range wrong {
			info, err := f.svc.Wait(ctx, w.id, w.waiter)
			if err != nil || info.Status != WaitExpired || info.Client != nil {
				t.Errorf("%s: Wait(%q, %q) = %+v %v, want expired with no app", stage, w.id, w.waiter, info, err)
			}
			if _, err := f.svc.RedeemWait(ctx, w.id, w.waiter); !errors.Is(err, ErrWaitUnknown) {
				t.Errorf("%s: RedeemWait(%q, %q) = %v, want ErrWaitUnknown", stage, w.id, w.waiter, err)
			}
		}
	}
	check("pending")
	info := f.wantStatus(t, req, WaitPending)
	if info.Client == nil || info.Client.ID != f.clientID {
		t.Fatalf("the right waiter does not learn its app: %+v", info)
	}
	if _, err := f.svc.Approve(ctx, req.ID, f.user, Consent{Nickname: "Waiting"}); err != nil {
		t.Fatal(err)
	}
	check("approved")
	if n := f.count(t, `SELECT COUNT(*) FROM mcp_oauth_codes`) + f.count(t, `SELECT COUNT(*) FROM mcp_oauth_return_tickets WHERE used_at IS NOT NULL`); n != 0 {
		t.Fatal("a wrong waiter used the approval")
	}
}

// TestWaitingPageContinuesOnce: pending until the person allows the app,
// then approved; Continue mints the code then (its 60 seconds start there,
// the sign-in time stays the consent's) and sends it to the client with
// state and iss; the code works with PKCE; the approval is then used, for
// the waiting page and the return link alike.
func TestWaitingPageContinuesOnce(t *testing.T) {
	f := newWaitFixture(t)
	ctx := context.Background()
	req := f.begin(t)
	f.wantStatus(t, req, WaitPending)
	if _, err := f.svc.RedeemWait(ctx, req.ID, req.Waiter); !errors.Is(err, ErrWaitPending) {
		t.Fatalf("Continue before the answer: %v, want ErrWaitPending", err)
	}

	consentAt := f.clock.UnixMilli()
	ticket, err := f.svc.Approve(ctx, req.ID, f.user, Consent{Nickname: "Popup"})
	if err != nil {
		t.Fatal(err)
	}
	var requestID, verdict string
	if err := f.db.QueryRow(`SELECT request_id FROM mcp_oauth_return_tickets WHERE ticket_hash = ?`, hashSecret(ticket)).Scan(&requestID); err != nil || requestID != req.ID {
		t.Fatalf("the return ticket names request %q (%v), want %q", requestID, err, req.ID)
	}
	if err := f.db.QueryRow(`SELECT verdict FROM mcp_oauth_requests WHERE id = ?`, req.ID).Scan(&verdict); err != nil || verdict != verdictAllow {
		t.Fatalf("verdict %q (%v), want allow", verdict, err)
	}
	info := f.wantStatus(t, req, WaitApproved)
	if info.Client == nil || info.Client.DisplayName() != "Test Client" {
		t.Fatalf("approved without the app: %+v", info)
	}
	if f.count(t, `SELECT COUNT(*) FROM mcp_oauth_codes`) != 0 {
		t.Fatal("asking minted a code")
	}

	f.clock = f.clock.Add(5 * time.Minute)
	redeemAt := f.clock.UnixMilli()
	loc, err := f.svc.RedeemWait(ctx, req.ID, req.Waiter)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(loc)
	if err != nil || !strings.HasPrefix(loc, f.redirect+"?") || u.Query().Get("state") != "st-123" || u.Query().Get("iss") != testPublic || u.Query().Get("code") == "" {
		t.Fatalf("Continue redirect %q", loc)
	}
	var codeCreated, codeExpires int64
	if err := f.db.QueryRow(`SELECT created_at, expires_at FROM mcp_oauth_codes WHERE code_hash = ?`, hashSecret(u.Query().Get("code"))).Scan(&codeCreated, &codeExpires); err != nil {
		t.Fatal(err)
	}
	if codeCreated != consentAt || codeExpires != redeemAt+CodeTTL.Milliseconds() {
		t.Fatalf("code times: created %d (want the consent's %d), expires %d (want %d)", codeCreated, consentAt, codeExpires, redeemAt+CodeTTL.Milliseconds())
	}
	tok, oerr := f.svc.Exchange(ctx, codeForm(u.Query().Get("code"), f.redirect, f.verifier), ClientAuth{ClientID: f.clientID})
	if oerr != nil {
		t.Fatalf("exchange with the PKCE verifier: %v", oerr)
	}
	if ag, err := f.svc.VerifyAccessToken(ctx, tok.AccessToken); err != nil || ag.Name != "Popup" {
		t.Fatalf("verify: %+v %v", ag, err)
	}
	if _, err := f.svc.RedeemWait(ctx, req.ID, req.Waiter); !errors.Is(err, ErrReturnUsed) {
		t.Fatalf("second Continue: %v, want ErrReturnUsed", err)
	}
	if _, err := f.svc.RedeemReturn(ctx, ticket); !errors.Is(err, ErrReturnUsed) {
		t.Fatalf("the return link after the waiting page: %v, want ErrReturnUsed", err)
	}
	f.wantStatus(t, req, WaitUsed)
	if n := f.count(t, `SELECT COUNT(*) FROM mcp_oauth_codes`); n != 1 {
		t.Fatalf("%d codes, want 1", n)
	}
}

// TestReturnLinkFirstRefusesTheWaitingPage: when the return link is used
// first, the waiting page reports used and its Continue is refused.
func TestReturnLinkFirstRefusesTheWaitingPage(t *testing.T) {
	f := newWaitFixture(t)
	ctx := context.Background()
	req := f.begin(t)
	ticket, err := f.svc.Approve(ctx, req.ID, f.user, Consent{Nickname: "Tab"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.RedeemReturn(ctx, ticket); err != nil {
		t.Fatal(err)
	}
	f.wantStatus(t, req, WaitUsed)
	if _, err := f.svc.RedeemWait(ctx, req.ID, req.Waiter); !errors.Is(err, ErrReturnUsed) {
		t.Fatalf("Continue after the return link: %v, want ErrReturnUsed", err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM mcp_oauth_codes`); n != 1 {
		t.Fatalf("%d codes, want 1", n)
	}
}

// TestWaitAfterDenyExpiryOrRevocation: a deny, an unanswered request past
// its ten minutes, an approval past its ten minutes or replaced by a newer
// consent, and a connection revoked after the approval.
func TestWaitAfterDenyExpiryOrRevocation(t *testing.T) {
	ctx := context.Background()
	t.Run("denied", func(t *testing.T) {
		f := newWaitFixture(t)
		req := f.begin(t)
		loc, err := f.svc.Deny(ctx, req.ID, f.user)
		if err != nil {
			t.Fatal(err)
		}
		f.wantStatus(t, req, WaitDenied)
		if got := f.svc.DenyLocation(req); got != loc {
			t.Fatalf("DenyLocation %q, want the deny page's link %q", got, loc)
		}
		if _, err := f.svc.RedeemWait(ctx, req.ID, req.Waiter); !errors.Is(err, ErrWaitDenied) {
			t.Fatalf("Continue after a deny: %v, want ErrWaitDenied", err)
		}
		if f.count(t, `SELECT COUNT(*) FROM mcp_oauth_codes`)+f.count(t, `SELECT COUNT(*) FROM mcp_oauth_return_tickets`) != 0 {
			t.Fatal("a deny left a code or a return link")
		}
	})
	t.Run("unanswered", func(t *testing.T) {
		f := newWaitFixture(t)
		req := f.begin(t)
		f.clock = f.clock.Add(RequestTTL + time.Second)
		f.wantStatus(t, req, WaitExpired)
		if _, err := f.svc.RedeemWait(ctx, req.ID, req.Waiter); !errors.Is(err, ErrReturnExpired) {
			t.Fatalf("Continue after the request expired: %v, want ErrReturnExpired", err)
		}
	})
	t.Run("approval expired", func(t *testing.T) {
		f := newWaitFixture(t)
		req := f.begin(t)
		if _, err := f.svc.Approve(ctx, req.ID, f.user, Consent{Nickname: "Slow"}); err != nil {
			t.Fatal(err)
		}
		f.clock = f.clock.Add(ReturnTTL + time.Second)
		f.wantStatus(t, req, WaitExpired)
		if _, err := f.svc.RedeemWait(ctx, req.ID, req.Waiter); !errors.Is(err, ErrReturnExpired) {
			t.Fatalf("Continue after the approval expired: %v, want ErrReturnExpired", err)
		}
		if f.count(t, `SELECT COUNT(*) FROM mcp_oauth_codes`) != 0 {
			t.Fatal("an expired approval minted a code")
		}
	})
	t.Run("replaced", func(t *testing.T) {
		f := newWaitFixture(t)
		first, second := f.begin(t), f.begin(t)
		if _, err := f.svc.Approve(ctx, first.ID, f.user, Consent{Nickname: "First"}); err != nil {
			t.Fatal(err)
		}
		if _, err := f.svc.Approve(ctx, second.ID, f.user, Consent{Nickname: "Second"}); err != nil {
			t.Fatal(err)
		}
		f.wantStatus(t, first, WaitExpired)
		f.wantStatus(t, second, WaitApproved)
		if _, err := f.svc.RedeemWait(ctx, first.ID, first.Waiter); !errors.Is(err, ErrReturnExpired) {
			t.Fatalf("the replaced approval: %v, want ErrReturnExpired", err)
		}
		if _, err := f.svc.RedeemWait(ctx, second.ID, second.Waiter); err != nil {
			t.Fatalf("the newest approval: %v", err)
		}
	})
	t.Run("revoked", func(t *testing.T) {
		f := newWaitFixture(t)
		req := f.begin(t)
		if _, err := f.svc.Approve(ctx, req.ID, f.user, Consent{Nickname: "Gone"}); err != nil {
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
		if _, err := f.svc.RedeemWait(ctx, req.ID, req.Waiter); !errors.Is(err, ErrReturnRevoked) {
			t.Fatalf("Continue after a revoke: %v, want ErrReturnRevoked", err)
		}
		if f.count(t, `SELECT COUNT(*) FROM mcp_oauth_codes`) != 0 {
			t.Fatal("a revoked connection's approval minted a code")
		}
		f.wantStatus(t, req, WaitUsed)
	})
}

// TestRacingWaitingPageAndReturnLinkMintOneCode: of the waiting page and
// the return link used at once, several times each, exactly one gets a
// code.
func TestRacingWaitingPageAndReturnLinkMintOneCode(t *testing.T) {
	f := newWaitFixture(t)
	ctx := context.Background()
	req := f.begin(t)
	ticket, err := f.svc.Approve(ctx, req.ID, f.user, Consent{Nickname: "Race"})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins, used := 0, 0
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var err error
			if i%2 == 0 {
				_, err = f.svc.RedeemWait(ctx, req.ID, req.Waiter)
			} else {
				_, err = f.svc.RedeemReturn(ctx, ticket)
			}
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
		}(i)
	}
	wg.Wait()
	if codes := f.count(t, `SELECT COUNT(*) FROM mcp_oauth_codes`); wins != 1 || codes != 1 {
		t.Fatalf("%d uses got a code, %d codes minted (%d refused as used), want exactly one", wins, codes, used)
	}
}
