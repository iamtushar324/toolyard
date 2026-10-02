package oauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestConnectTicketLifecycle: a ticket is stored as its hash only, can be
// looked at without being spent, redeems exactly once with its row, and
// unknown, used and expired tickets are told apart; the purge drops
// expired rows.
func TestConnectTicketLifecycle(t *testing.T) {
	f := newUserFixture(t)
	ctx := context.Background()

	ticket, err := f.svc.IssueConnectTicket(ctx, "u_ada", "linear", ConnectPurposePerUser, "ag_1")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if len(ticket) < 40 || strings.ContainsAny(ticket, "/+=") {
		t.Fatalf("ticket %q is not a long base64url string", ticket)
	}
	// Hashed at rest: the plaintext is in no column of the row.
	var n int
	if err := f.db.QueryRow(`SELECT count(*) FROM connect_tickets WHERE id_hash = ?`, ticket).Scan(&n); err != nil || n != 0 {
		t.Fatalf("plaintext ticket stored as id_hash: n=%d err=%v", n, err)
	}
	if err := f.db.QueryRow(`SELECT count(*) FROM connect_tickets WHERE id_hash = ?`, hashConnectTicket(ticket)).Scan(&n); err != nil || n != 1 {
		t.Fatalf("hashed row: n=%d err=%v", n, err)
	}
	var dump string
	_ = f.db.QueryRow(`SELECT id_hash || user_id || upstream || purpose || COALESCE(agent_id,'') FROM connect_tickets`).Scan(&dump)
	if strings.Contains(dump, ticket) {
		t.Fatal("plaintext ticket found in the row")
	}

	// Peeking spends nothing.
	for i := 0; i < 2; i++ {
		tk, err := f.svc.PeekConnectTicket(ctx, ticket)
		if err != nil || tk.UserID != "u_ada" || tk.Upstream != "linear" || tk.Purpose != ConnectPurposePerUser || tk.AgentID != "ag_1" {
			t.Fatalf("peek %d = %+v %v", i, tk, err)
		}
	}
	tk, err := f.svc.RedeemConnectTicket(ctx, ticket)
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}
	if tk.UserID != "u_ada" || tk.Upstream != "linear" || tk.Purpose != ConnectPurposePerUser || tk.AgentID != "ag_1" {
		t.Fatalf("redeemed row = %+v", tk)
	}
	if ttl := time.Until(tk.ExpiresAt); ttl < ConnectTicketTTL-time.Minute || ttl > ConnectTicketTTL {
		t.Fatalf("expires in %s, want about %s", ttl, ConnectTicketTTL)
	}
	// Single use, for peek and redeem alike.
	if _, err := f.svc.RedeemConnectTicket(ctx, ticket); !errors.Is(err, ErrTicketUsed) {
		t.Fatalf("second redeem: %v, want ErrTicketUsed", err)
	}
	if _, err := f.svc.PeekConnectTicket(ctx, ticket); !errors.Is(err, ErrTicketUsed) {
		t.Fatalf("peek after redeem: %v, want ErrTicketUsed", err)
	}
	for _, bad := range []string{"not-a-ticket", ""} {
		if _, err := f.svc.RedeemConnectTicket(ctx, bad); !errors.Is(err, ErrTicketUnknown) {
			t.Fatalf("redeem %q: %v, want ErrTicketUnknown", bad, err)
		}
		if _, err := f.svc.PeekConnectTicket(ctx, bad); !errors.Is(err, ErrTicketUnknown) {
			t.Fatalf("peek %q: %v, want ErrTicketUnknown", bad, err)
		}
	}

	// Expiry.
	stale, err := f.svc.IssueConnectTicket(ctx, "u_bob", "shared", ConnectPurposeShared, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`UPDATE connect_tickets SET expires_at = ? WHERE id_hash = ?`, time.Now().Add(-time.Second).UnixMilli(), hashConnectTicket(stale)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.PeekConnectTicket(ctx, stale); !errors.Is(err, ErrTicketExpired) {
		t.Fatalf("peek expired: %v, want ErrTicketExpired", err)
	}
	if _, err := f.svc.RedeemConnectTicket(ctx, stale); !errors.Is(err, ErrTicketExpired) {
		t.Fatalf("redeem expired: %v, want ErrTicketExpired", err)
	}
	// The purge drops the expired one and keeps the used-but-live one.
	if n, err := f.svc.PurgeExpiredConnectTickets(ctx); err != nil || n != 1 {
		t.Fatalf("purge = %d %v, want 1", n, err)
	}
	if err := f.db.QueryRow(`SELECT count(*) FROM connect_tickets`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("rows after purge = %d (err %v)", n, err)
	}

	// Validation.
	if _, err := f.svc.IssueConnectTicket(ctx, "u_ada", "linear", "whatever", ""); err == nil {
		t.Fatal("bad purpose accepted")
	}
	if _, err := f.svc.IssueConnectTicket(ctx, "", "linear", ConnectPurposePerUser, ""); err == nil {
		t.Fatal("empty user accepted")
	}
	if _, err := f.svc.IssueConnectTicket(ctx, "u_ada", "nope", ConnectPurposePerUser, ""); err == nil {
		t.Fatal("unknown server accepted (foreign key)")
	}
}

// TestConnectTicketRedeemsOnceUnderContention: many browsers submitting
// the same link at once get exactly one sign-in.
func TestConnectTicketRedeemsOnceUnderContention(t *testing.T) {
	f := newUserFixture(t)
	ctx := context.Background()
	ticket, err := f.svc.IssueConnectTicket(ctx, "u_ada", "linear", ConnectPurposePerUser, "")
	if err != nil {
		t.Fatal(err)
	}
	const n = 12
	results := make(chan error, n)
	var start sync.WaitGroup
	start.Add(1)
	for i := 0; i < n; i++ {
		go func() {
			start.Wait()
			_, err := f.svc.RedeemConnectTicket(ctx, ticket)
			results <- err
		}()
	}
	start.Done()
	ok, used := 0, 0
	for i := 0; i < n; i++ {
		switch err := <-results; {
		case err == nil:
			ok++
		case errors.Is(err, ErrTicketUsed):
			used++
		default:
			t.Fatalf("unexpected redeem error: %v", err)
		}
	}
	if ok != 1 || used != n-1 {
		t.Fatalf("redeemed %d times, %d told used; want 1 and %d", ok, used, n-1)
	}
}

// TestConnectTicketCap: one (user, server) can have only so many unused
// links outstanding, the cap and the insert being one statement so racing
// issuers cannot both slip under it; redeeming or expiring one frees a
// slot, and ConsumeConnectTickets closes every live one at once.
func TestConnectTicketCap(t *testing.T) {
	f := newUserFixture(t)
	ctx := context.Background()
	var last string
	for i := 0; i < MaxLiveConnectTickets; i++ {
		tk, err := f.svc.IssueConnectTicket(ctx, "u_ada", "linear", ConnectPurposePerUser, "")
		if err != nil {
			t.Fatalf("issue %d: %v", i, err)
		}
		last = tk
	}
	if _, err := f.svc.IssueConnectTicket(ctx, "u_ada", "linear", ConnectPurposePerUser, ""); !errors.Is(err, ErrTooManyTickets) {
		t.Fatalf("over the cap: %v, want ErrTooManyTickets", err)
	}
	// Racing issuers at the cap: none gets through.
	const racers = 8
	var wg sync.WaitGroup
	errs := make(chan error, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.svc.IssueConnectTicket(ctx, "u_ada", "linear", ConnectPurposePerUser, "")
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if !errors.Is(err, ErrTooManyTickets) {
			t.Fatalf("a racing issue slipped under the cap: %v", err)
		}
	}
	// Another server, or another person, has its own budget.
	if _, err := f.svc.IssueConnectTicket(ctx, "u_ada", "shared", ConnectPurposeShared, ""); err != nil {
		t.Fatalf("other server: %v", err)
	}
	if _, err := f.svc.IssueConnectTicket(ctx, "u_bob", "linear", ConnectPurposePerUser, ""); err != nil {
		t.Fatalf("other person: %v", err)
	}
	if _, err := f.svc.RedeemConnectTicket(ctx, last); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.IssueConnectTicket(ctx, "u_ada", "linear", ConnectPurposePerUser, ""); err != nil {
		t.Fatalf("after redeeming one: %v", err)
	}
	// The person connected: every live link of theirs for that server
	// closes; other people's and other servers' stay.
	if n, err := f.svc.ConsumeConnectTickets(ctx, "u_ada", "linear"); err != nil || n != MaxLiveConnectTickets {
		t.Fatalf("consume = %d %v, want %d", n, err, MaxLiveConnectTickets)
	}
	var live int
	_ = f.db.QueryRow(`SELECT count(*) FROM connect_tickets WHERE used_at IS NULL`).Scan(&live)
	if live != 2 {
		t.Fatalf("live tickets after consume = %d, want bob's and the shared one", live)
	}
	if n, _ := f.svc.ConsumeConnectTickets(ctx, "u_ada", "linear"); n != 0 {
		t.Fatalf("second consume closed %d", n)
	}
}

// TestBeginFromTicketMarksPending: a flow a link starts is browser-bound
// and marked via_ticket, per-user or shared as asked, and remembers
// whether the opener's browser vouched for the person; a dashboard-started
// flow carries neither mark.
func TestBeginFromTicketMarksPending(t *testing.T) {
	f := newUserFixture(t)
	ctx := context.Background()

	authURL, state, err := f.svc.BeginFromTicket(ctx, "linear", "u_ada", true, false)
	if err != nil {
		t.Fatalf("BeginFromTicket: %v", err)
	}
	if !strings.Contains(authURL, "state="+state) || !strings.Contains(authURL, "redirect_uri=") {
		t.Fatalf("authorize url: %s", authURL)
	}
	p, err := f.svc.LoadPending(ctx, state)
	if err != nil {
		t.Fatal(err)
	}
	if !p.PerUser || !p.BrowserBound || !p.ViaTicket || p.OpenerVerified || p.UserID != "u_ada" || p.UpstreamName != "linear" {
		t.Fatalf("per-user ticket flow pending = %+v", p)
	}

	_, state, err = f.svc.BeginFromTicket(ctx, "linear", "u_ada", true, true)
	if err != nil {
		t.Fatal(err)
	}
	p, _ = f.svc.LoadPending(ctx, state)
	if !p.OpenerVerified {
		t.Fatalf("opener-verified flow pending = %+v", p)
	}

	_, state, err = f.svc.BeginFromTicket(ctx, "shared", "u_ada", false, true)
	if err != nil {
		t.Fatal(err)
	}
	p, _ = f.svc.LoadPending(ctx, state)
	if p.PerUser || !p.BrowserBound || !p.ViaTicket || !p.OpenerVerified {
		t.Fatalf("shared ticket flow pending = %+v", p)
	}

	_, state, err = f.svc.BeginForUser(ctx, "linear", "u_ada")
	if err != nil {
		t.Fatal(err)
	}
	p, _ = f.svc.LoadPending(ctx, state)
	if !p.PerUser || !p.BrowserBound || p.ViaTicket || p.OpenerVerified {
		t.Fatalf("dashboard flow pending = %+v", p)
	}
	if _, _, err := f.svc.BeginFromTicket(ctx, "linear", "", true, false); err == nil {
		t.Fatal("ticket flow without a user accepted")
	}
}

// TestAccountFromIDToken: the email claim and whether the provider vouched
// for it; a missing email_verified counts as vouched, false does not.
func TestAccountFromIDToken(t *testing.T) {
	mk := func(claims map[string]any) string {
		hdr := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
		pl, _ := json.Marshal(claims)
		return hdr + "." + base64.RawURLEncoding.EncodeToString(pl) + ".sig"
	}
	cases := []struct {
		token    string
		email    string
		verified bool
	}{
		{mk(map[string]any{"email": "ada@example.test"}), "ada@example.test", true},
		{mk(map[string]any{"email": "ada@example.test", "email_verified": true}), "ada@example.test", true},
		{mk(map[string]any{"email": "ada@example.test", "email_verified": false}), "ada@example.test", false},
		{mk(map[string]any{"sub": "x"}), "", false},
		{"not.a.jwt.at.all", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		email, verified := accountFromIDToken(c.token)
		if email != c.email || verified != c.verified {
			t.Errorf("accountFromIDToken(%q) = %q %v, want %q %v", c.token, email, verified, c.email, c.verified)
		}
	}
	if rec := userTokenRecordFromResponse("linear", "u", &tokenResponse{AccessToken: "a", IDToken: cases[2].token}); rec.AccountLabel != "ada@example.test" || rec.AccountVerified {
		t.Fatalf("record from an unverified email = %+v", rec)
	}
}

// TestExchangeCodeForUserCheckedRefusesBeforeStoring: a token the accept
// hook rejects is never written, the bearer cache stays empty, nothing is
// published, and the token is revoked at the provider; an accepted one
// is stored as usual.
func TestExchangeCodeForUserCheckedRefusesBeforeStoring(t *testing.T) {
	f := newUserFixture(t)
	ctx := context.Background()
	onlyAda := func(rec *UserTokenRecord) error {
		if rec.AccountLabel != "ada@example.test" {
			return errors.New("not ada: " + rec.AccountLabel)
		}
		return nil
	}

	_, state, err := f.svc.BeginFromTicket(ctx, "linear", "u_ada", true, false)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := f.svc.LoadPending(ctx, state)
	_, err = f.svc.ExchangeCodeForUserChecked(ctx, "linear", "u_ada", "mallory", p.CodeVerifier, onlyAda)
	if !errors.Is(err, ErrAccountMismatch) || !strings.Contains(err.Error(), "not ada") {
		t.Fatalf("mismatch: %v", err)
	}
	if tok, err := f.svc.GetUserToken(ctx, "linear", "u_ada"); err != nil || tok != nil {
		t.Fatalf("token stored after refusal: %v %v", tok, err)
	}
	if h := f.svc.UserHeaderFunc("linear")(ctx, "u_ada"); len(h) != 0 {
		t.Fatalf("bearer cached after refusal: %v", h)
	}
	if got := f.idp.revocations(); len(got) != 1 || got[0] != "rt-mallory" {
		t.Fatalf("revocations = %v, want the refused refresh token", got)
	}
	for _, ev := range f.bus.list() {
		if strings.HasPrefix(ev, "mcp_oauth_user_done") {
			t.Fatalf("published %s after a refusal", ev)
		}
	}

	rec, err := f.svc.ExchangeCodeForUserChecked(ctx, "linear", "u_ada", "ada", p.CodeVerifier, onlyAda)
	if err != nil || rec.AccountLabel != "ada@example.test" || !rec.AccountVerified {
		t.Fatalf("accepted exchange: %v %+v", err, rec)
	}
	if tok, _ := f.svc.GetUserToken(ctx, "linear", "u_ada"); tok == nil || tok.AccessToken != "at-ada" {
		t.Fatalf("accepted token not stored: %+v", tok)
	}
	// nil accept is the plain exchange.
	if _, err := f.svc.ExchangeCodeForUserChecked(ctx, "linear", "u_bob", "bob", p.CodeVerifier, nil); err != nil {
		t.Fatalf("plain exchange: %v", err)
	}
}

// TestExchangeCodeCheckedKeepsTheSharedRow: a shared sign-in the accept
// hook refuses (it saw the account on file and the new one) leaves the
// existing row, its state and the bearer cache untouched and revokes the
// new token; the hook sees an empty label when nothing is on file.
func TestExchangeCodeCheckedKeepsTheSharedRow(t *testing.T) {
	f := newUserFixture(t)
	ctx := context.Background()
	begin := func() string {
		_, state, err := f.svc.BeginCallback(ctx, "shared", "u_ada", ModeCallback, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		p, err := f.svc.LoadPending(ctx, state)
		if err != nil {
			t.Fatal(err)
		}
		return p.CodeVerifier
	}
	var seenPrev []string
	remember := func(prev string, _ *TokenRecord) error {
		seenPrev = append(seenPrev, prev)
		return nil
	}
	if _, err := f.svc.ExchangeCodeChecked(ctx, "shared", "ops", begin(), remember); err != nil {
		t.Fatal(err)
	}
	f.svc.MarkReauthExternal(ctx, "shared", "revoked")
	refuseOthers := func(prev string, rec *TokenRecord) error {
		seenPrev = append(seenPrev, prev)
		if prev != "" && rec.AccountLabel != prev {
			return errors.New("different account")
		}
		return nil
	}
	_, err := f.svc.ExchangeCodeChecked(ctx, "shared", "intruder", begin(), refuseOthers)
	if !errors.Is(err, ErrAccountMismatch) {
		t.Fatalf("different account: %v", err)
	}
	tok, _ := f.svc.GetToken(ctx, "shared")
	if tok == nil || tok.AccountLabel != "ops@example.test" || tok.State != StateNeedsReauth || tok.AccessToken != "at-ops" {
		t.Fatalf("shared row after refusal = %+v, want the old one untouched", tok)
	}
	if h := f.svc.HeaderFunc("shared")(ctx); len(h) != 0 {
		t.Fatalf("bearer cached after refusal: %v", h)
	}
	if got := f.idp.revocations(); len(got) != 1 || got[0] != "rt-intruder" {
		t.Fatalf("revocations = %v", got)
	}
	if _, err := f.svc.ExchangeCodeChecked(ctx, "shared", "ops", begin(), refuseOthers); err != nil {
		t.Fatalf("same account: %v", err)
	}
	tok, _ = f.svc.GetToken(ctx, "shared")
	if tok.State != StateActive || tok.AccessToken != "at-ops" {
		t.Fatalf("shared row after renewal = %+v", tok)
	}
	if strings.Join(seenPrev, ",") != ",ops@example.test,ops@example.test" {
		t.Fatalf("labels the hook saw = %q", seenPrev)
	}
}

// TestConnectionViews: what the agent-facing tools read about servers and
// sign-ins, without any token value.
func TestConnectionViews(t *testing.T) {
	f := newUserFixture(t)
	ctx := context.Background()

	servers, err := f.svc.OAuthServers(ctx)
	if err != nil || len(servers) != 2 {
		t.Fatalf("OAuthServers = %+v %v", servers, err)
	}
	if servers[0].Name != "linear" || servers[0].AuthMode != "per_user" || !servers[0].Enabled ||
		servers[1].Name != "shared" || servers[1].AuthMode != "shared" {
		t.Fatalf("OAuthServers = %+v", servers)
	}

	c, err := f.svc.SharedConnection(ctx, "shared")
	if err != nil || c == nil || c.HasToken || c.Usable() || !c.CanAuthorize || c.State != "" {
		t.Fatalf("shared before sign-in = %+v %v", c, err)
	}
	if c, err := f.svc.SharedConnection(ctx, "nope"); err != nil || c != nil {
		t.Fatalf("unknown server = %+v %v, want nil", c, err)
	}
	_, state, err := f.svc.BeginCallback(ctx, "shared", "u_ada", ModeCallback, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := f.svc.LoadPending(ctx, state)
	if _, err := f.svc.ExchangeCode(ctx, "shared", "ops", p.CodeVerifier); err != nil {
		t.Fatal(err)
	}
	c, _ = f.svc.SharedConnection(ctx, "shared")
	if !c.HasToken || !c.Usable() || c.State != StateActive || c.AccountLabel != "ops@example.test" || c.IsPAT {
		t.Fatalf("shared after sign-in = %+v", c)
	}
	f.svc.MarkReauthExternal(ctx, "shared", "revoked upstream")
	c, _ = f.svc.SharedConnection(ctx, "shared")
	if c.Usable() || c.State != StateNeedsReauth || c.AccountLabel != "ops@example.test" {
		t.Fatalf("shared after reauth mark = %+v", c)
	}
	// The PAT placeholder client cannot start a browser sign-in.
	if _, err := f.db.Exec(`INSERT INTO upstream_servers(name, transport, url, enabled, auth_mode, created_at, updated_at) VALUES('pat','http','http://127.0.0.1:1/mcp',1,'shared',1,1)`); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.PutClient(ctx, ClientRecord{UpstreamName: "pat", Issuer: "(personal-access-token)", AuthorizationEndpoint: "(pat)", TokenEndpoint: "(pat)", ClientID: "(pat)", RedirectURI: "(pat)", TokenEndpointAuthMethod: "none"}); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.PutPAT(ctx, "pat", "secret-pat"); err != nil {
		t.Fatal(err)
	}
	c, _ = f.svc.SharedConnection(ctx, "pat")
	if !c.Usable() || !c.IsPAT || c.CanAuthorize {
		t.Fatalf("pat server = %+v", c)
	}

	if uc, err := f.svc.UserConnectionOf(ctx, "linear", "u_ada"); err != nil || uc != nil {
		t.Fatalf("user connection before sign-in = %+v %v", uc, err)
	}
	f.connect(t, "linear", "u_ada", "ada")
	uc, err := f.svc.UserConnectionOf(ctx, "linear", "u_ada")
	if err != nil || uc == nil || uc.State != ConnConnected || uc.AccountLabel != "ada@example.test" {
		t.Fatalf("user connection after sign-in = %+v %v", uc, err)
	}
	if uc, _ := f.svc.UserConnectionOf(ctx, "linear", "u_bob"); uc != nil {
		t.Fatalf("bob has a connection: %+v", uc)
	}
}
