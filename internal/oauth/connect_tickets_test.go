package oauth

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestConnectTicketLifecycle: a ticket is stored as its hash only, redeems
// exactly once with its row, and unknown, used and expired tickets are
// told apart; the purge drops expired rows.
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
	// Single use.
	if _, err := f.svc.RedeemConnectTicket(ctx, ticket); !errors.Is(err, ErrTicketUsed) {
		t.Fatalf("second redeem: %v, want ErrTicketUsed", err)
	}
	if _, err := f.svc.RedeemConnectTicket(ctx, "not-a-ticket"); !errors.Is(err, ErrTicketUnknown) {
		t.Fatalf("unknown: %v, want ErrTicketUnknown", err)
	}
	if _, err := f.svc.RedeemConnectTicket(ctx, ""); !errors.Is(err, ErrTicketUnknown) {
		t.Fatalf("empty: %v, want ErrTicketUnknown", err)
	}

	// Expiry.
	stale, err := f.svc.IssueConnectTicket(ctx, "u_bob", "shared", ConnectPurposeShared, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`UPDATE connect_tickets SET expires_at = ? WHERE id_hash = ?`, time.Now().Add(-time.Second).UnixMilli(), hashConnectTicket(stale)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.RedeemConnectTicket(ctx, stale); !errors.Is(err, ErrTicketExpired) {
		t.Fatalf("expired: %v, want ErrTicketExpired", err)
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

// TestConnectTicketRedeemsOnceUnderContention: many browsers opening the
// same link at once get exactly one sign-in.
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
// links outstanding; redeeming or expiring one frees a slot.
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
}

// TestBeginFromTicketMarksPending: a flow a link starts is browser-bound
// and marked via_ticket, per-user or shared as asked; a dashboard-started
// flow is not marked.
func TestBeginFromTicketMarksPending(t *testing.T) {
	f := newUserFixture(t)
	ctx := context.Background()

	authURL, state, err := f.svc.BeginFromTicket(ctx, "linear", "u_ada", true)
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
	if !p.PerUser || !p.BrowserBound || !p.ViaTicket || p.UserID != "u_ada" || p.UpstreamName != "linear" {
		t.Fatalf("per-user ticket flow pending = %+v", p)
	}

	_, state, err = f.svc.BeginFromTicket(ctx, "shared", "u_ada", false)
	if err != nil {
		t.Fatal(err)
	}
	p, _ = f.svc.LoadPending(ctx, state)
	if p.PerUser || !p.BrowserBound || !p.ViaTicket {
		t.Fatalf("shared ticket flow pending = %+v", p)
	}

	_, state, err = f.svc.BeginForUser(ctx, "linear", "u_ada")
	if err != nil {
		t.Fatal(err)
	}
	p, _ = f.svc.LoadPending(ctx, state)
	if !p.PerUser || !p.BrowserBound || p.ViaTicket {
		t.Fatalf("dashboard flow pending = %+v", p)
	}
	if _, _, err := f.svc.BeginFromTicket(ctx, "linear", "", true); err == nil {
		t.Fatal("ticket flow without a user accepted")
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

	_, state, err := f.svc.BeginFromTicket(ctx, "linear", "u_ada", true)
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
	if err != nil || rec.AccountLabel != "ada@example.test" {
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
