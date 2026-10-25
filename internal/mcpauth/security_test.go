package mcpauth

// Tests for the fixes of the security review of PR #50: document fetches
// (addresses, ports, where fetches may start, negative cache, single
// flight), rate-limit keys and caps, refresh grace and races, the absolute
// session limit, revocation side effects, the manage flag, the agent
// tombstone and the audit client id.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/access"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
)

func TestBlockedAddresses(t *testing.T) {
	for addr, want := range map[string]bool{
		"8.8.8.8": false, "1.1.1.1": false, "2606:4700:4700::1111": false,
		"0.0.0.1": true, "10.1.2.3": true, "100.64.0.1": true, "100.127.255.254": true, "127.0.0.1": true,
		"169.254.169.254": true, "172.16.0.1": true, "192.0.0.8": true, "192.0.2.1": true, "192.88.99.1": true,
		"192.168.1.1": true, "198.18.0.1": true, "198.19.255.255": true, "198.51.100.1": true, "203.0.113.9": true,
		"224.0.0.1": true, "240.0.0.1": true, "255.255.255.255": true,
		"::": true, "::1": true, "::ffff:10.0.0.1": true, "::ffff:8.8.8.8": false, "::a00:1": true,
		"64:ff9b::808:808": false, "64:ff9b::a00:1": true, "64:ff9b::7f00:1": true, "64:ff9b:1::1": true,
		"2002:808:808::1": false, "2002:a00:1::1": true, "2002:a9fe:a9fe::1": true,
		"2001::1": true, "2001:db8::1": true, "3fff::1": true, "fc00::1": true, "fd12::1": true, "fe80::1": true,
		"fec0::1": true, "ff02::1": true, "100::1": true, "2001:2::1": true,
	} {
		if got := blockedAddr(netip.MustParseAddr(addr)); got != want {
			t.Errorf("blockedAddr(%s) = %v, want %v", addr, got, want)
		}
	}
	if !blockedAddr(netip.Addr{}) || !blockedAddr(netip.MustParseAddr("fe80::1%eth0")) {
		t.Error("an invalid or zoned address must be blocked")
	}
}

// TestUnknownDocumentClientsAreNeverFetchedAtTheTokenEndpoint: only an
// authorization request reads a document; the token and revocation
// endpoints, which anyone may call, only use a cached copy.
func TestUnknownDocumentClientsAreNeverFetchedAtTheTokenEndpoint(t *testing.T) {
	f := newFixture(t)
	srv, hits := cimdServer(t, func(u string) any {
		return map[string]any{"client_id": u, "redirect_uris": []string{"https://a.example/cb"}}
	}, http.StatusOK)
	svc := cimdService(f, srv, true)
	ctx := context.Background()
	id := srv.URL + "/never-authorized.json"
	_, oerr := svc.Exchange(ctx, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {"tyrt_x"}}, ClientAuth{ClientID: id})
	wantOAuthErr(t, oerr, "invalid_client")
	_, oerr = svc.Exchange(ctx, url.Values{"grant_type": {"authorization_code"}, "code": {"tyoc_x"}}, ClientAuth{ClientID: id})
	wantOAuthErr(t, oerr, "invalid_client")
	wantOAuthErr(t, svc.Revoke(ctx, "tyat_x", ClientAuth{ClientID: id}), "invalid_client")
	if n := atomic.LoadInt32(hits); n != 0 {
		t.Fatalf("the token and revocation endpoints fetched %d documents", n)
	}
}

func TestDocumentFailuresAreCachedAndFetchesShared(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	// A document that answers 404: fetched once, then refused from the
	// negative cache for cimdFailureTTL, then fetched again.
	bad, badHits := cimdServer(t, nil, http.StatusNotFound)
	svc := cimdService(f, bad, true)
	id := bad.URL + "/gone.json"
	for i := 0; i < 3; i++ {
		if _, oerr := svc.ResolveClient(ctx, id, "198.51.100.7"); oerr == nil || oerr.Description != errCIMDUnusable().Description {
			t.Fatalf("attempt %d: %v", i, oerr)
		}
	}
	if n := atomic.LoadInt32(badHits); n != 1 {
		t.Fatalf("a failing document was fetched %d times within the negative cache", n)
	}
	f.clock = f.clock.Add(cimdFailureTTL + time.Second)
	_, _ = svc.ResolveClient(ctx, id, "198.51.100.7")
	if n := atomic.LoadInt32(badHits); n != 2 {
		t.Fatalf("the negative cache did not expire (%d fetches)", n)
	}

	// Concurrent requests for one client_id share one fetch.
	release := make(chan struct{})
	var slowHits int32
	var slow *httptest.Server
	slow = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&slowHits, 1)
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"client_id":"` + slow.URL + r.URL.Path + `","redirect_uris":["https://a.example/cb"]}`))
	}))
	defer slow.Close()
	slowSvc := cimdService(f, slow, true)
	var wg sync.WaitGroup
	errs := make(chan *Error, 5)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, oerr := slowSvc.ResolveClient(ctx, slow.URL+"/shared.json", "198.51.100.9")
			errs <- oerr
		}()
	}
	time.Sleep(300 * time.Millisecond)
	close(release)
	wg.Wait()
	close(errs)
	for oerr := range errs {
		if oerr != nil {
			t.Errorf("a waiting request failed: %v", oerr)
		}
	}
	if n := atomic.LoadInt32(&slowHits); n != 1 {
		t.Fatalf("%d fetches for one client_id, want 1", n)
	}
}

func TestDocumentFetchesAreLimitedPerAddress(t *testing.T) {
	f := newFixture(t)
	srv, _ := cimdServer(t, nil, http.StatusNotFound)
	svc := cimdService(f, srv, true)
	ctx := context.Background()
	refused := 0
	for i := 0; i < cimdFetchesPerIP+3; i++ {
		_, oerr := svc.ResolveClient(ctx, srv.URL+"/doc-"+strings.Repeat("x", i+1)+".json", "2001:db8:1:2::5")
		if oerr != nil && oerr.Code == "temporarily_unavailable" {
			refused++
		}
	}
	if refused != 3 {
		t.Fatalf("%d fetches refused, want 3 past the per-address limit", refused)
	}
	// Another address in the same IPv6 /64 shares the bucket.
	if _, oerr := svc.ResolveClient(ctx, srv.URL+"/other.json", "2001:db8:1:2::99"); oerr == nil || oerr.Code != "temporarily_unavailable" {
		t.Fatalf("another host of the /64: %v", oerr)
	}
}

func TestRateKeys(t *testing.T) {
	for ip, want := range map[string]string{
		"198.51.100.7": "198.51.100.7", "::ffff:198.51.100.7": "198.51.100.7",
		"2001:db8:1:2::5": "2001:db8:1:2::/64", "2001:db8:1:2:aaaa::1": "2001:db8:1:2::/64",
		"": "unknown", "not-an-ip": "unknown",
	} {
		if got := rateKey(ip); got != want {
			t.Errorf("rateKey(%q) = %q, want %q", ip, got, want)
		}
	}
	if limitFor("unknown", 60) != 15 || limitFor("unknown", 2) != 1 || limitFor("198.51.100.7", 60) != 60 {
		t.Error("the unknown bucket must be a strict quarter of the limit")
	}
}

func TestGlobalCapsOnRegistrationsAndRequests(t *testing.T) {
	f := newFixture(t)
	defer func(g, a int) { registerGlobal, authorizeGlobal = g, a }(registerGlobal, authorizeGlobal)
	registerGlobal, authorizeGlobal = 2, 3
	mux := http.NewServeMux()
	mux.HandleFunc(RegisterPath, func(w http.ResponseWriter, r *http.Request) {
		f.svc.HandleRegister(w, r, r.Header.Get("X-Test-IP"))
	})
	codes := []int{}
	for i, ip := range []string{"198.51.100.1", "198.51.100.2", "198.51.100.3"} {
		r := httptest.NewRequest(http.MethodPost, RegisterPath, strings.NewReader(`{"redirect_uris":["https://a`+strings.Repeat("a", i)+`.example/cb"],"token_endpoint_auth_method":"none"}`))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Test-IP", ip)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		codes = append(codes, w.Code)
	}
	if codes[0] != http.StatusCreated || codes[1] != http.StatusCreated || codes[2] != http.StatusTooManyRequests {
		t.Fatalf("registrations from three addresses: %v, want the third refused by the global cap", codes)
	}
	c := f.register(t, "none", "https://ok.example/cb") // direct Register skips the HTTP limits
	_, challenge := pkcePair(t)
	refused := false
	for i := 0; i < 5; i++ {
		_, aerr := f.svc.BeginAuthorize(context.Background(), authorizeQuery(c.ClientID, "https://ok.example/cb", challenge, nil), "198.51.100."+string(rune('1'+i)))
		if aerr != nil && aerr.Status == http.StatusTooManyRequests {
			refused = true
		}
	}
	if !refused {
		t.Fatal("pending requests past the global cap were accepted")
	}
}

// TestRefreshRaceAndGrace: two refreshes of one token racing (or a client
// retry) give exactly one new pair and revoke nothing.
func TestRefreshRaceAndGrace(t *testing.T) {
	f := newFixture(t)
	redirect := "https://client.example/cb"
	c := f.register(t, "none", redirect)
	ca := ClientAuth{ClientID: c.ClientID}
	ctx := context.Background()
	verifier, challenge := pkcePair(t)
	code := f.approve(t, f.user, c.ClientID, redirect, challenge, Consent{Nickname: "Racer"})
	tok, oerr := f.svc.Exchange(ctx, codeForm(code, redirect, verifier), ca)
	if oerr != nil {
		t.Fatal(oerr)
	}
	const n = 6
	var wg sync.WaitGroup
	results := make(chan *TokenResponse, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, oerr := f.svc.Exchange(ctx, refreshForm(tok.RefreshToken), ca)
			if oerr != nil && oerr.Code != "invalid_grant" {
				t.Errorf("racing refresh: %v", oerr)
			}
			results <- resp
		}()
	}
	wg.Wait()
	close(results)
	var winners []*TokenResponse
	for r := range results {
		if r != nil {
			winners = append(winners, r)
		}
	}
	if len(winners) != 1 {
		t.Fatalf("%d racing refreshes succeeded, want exactly 1", len(winners))
	}
	if _, err := f.svc.VerifyAccessToken(ctx, winners[0].AccessToken); err != nil {
		t.Fatalf("the race revoked the grant: %v", err)
	}
	if _, oerr := f.svc.Exchange(ctx, refreshForm(winners[0].RefreshToken), ca); oerr != nil {
		t.Fatalf("the winner's refresh token does not work: %v", oerr)
	}
}

func TestConcurrentCodeExchangeHasOneWinner(t *testing.T) {
	f := newFixture(t)
	redirect := "https://client.example/cb"
	c := f.register(t, "none", redirect)
	ca := ClientAuth{ClientID: c.ClientID}
	verifier, challenge := pkcePair(t)
	code := f.approve(t, f.user, c.ClientID, redirect, challenge, Consent{Nickname: "Racer"})
	const n = 6
	var wg sync.WaitGroup
	var ok int32
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, oerr := f.svc.Exchange(context.Background(), codeForm(code, redirect, verifier), ca); oerr == nil {
				atomic.AddInt32(&ok, 1)
			} else if oerr.Code != "invalid_grant" {
				t.Errorf("racing exchange: %v", oerr)
			}
		}()
	}
	wg.Wait()
	if ok != 1 {
		t.Fatalf("%d exchanges of one code succeeded, want exactly 1", ok)
	}
}

func TestAbsoluteSessionLimit(t *testing.T) {
	f := newFixture(t)
	f.settings[SettingMaxSessionDays] = 1
	f.settings[SettingRefreshTTLDays] = 30
	redirect := "https://client.example/cb"
	c := f.register(t, "none", redirect)
	ca := ClientAuth{ClientID: c.ClientID}
	ctx := context.Background()
	verifier, challenge := pkcePair(t)
	code := f.approve(t, f.user, c.ClientID, redirect, challenge, Consent{Nickname: "Short"})
	tok, oerr := f.svc.Exchange(ctx, codeForm(code, redirect, verifier), ca)
	if oerr != nil {
		t.Fatal(oerr)
	}
	var refreshExpires int64
	if err := f.db.QueryRow(`SELECT expires_at FROM mcp_oauth_tokens WHERE token_hash = ?`, hashSecret(tok.RefreshToken)).Scan(&refreshExpires); err != nil {
		t.Fatal(err)
	}
	if limit := f.clock.Add(24 * time.Hour).UnixMilli(); refreshExpires > limit {
		t.Fatalf("the refresh token outlives the session limit: %d > %d", refreshExpires, limit)
	}
	// Refreshing near the limit caps the new tokens at it; past it, the
	// person signs in again.
	f.clock = f.clock.Add(23*time.Hour + 30*time.Minute)
	t2, oerr := f.svc.Exchange(ctx, refreshForm(tok.RefreshToken), ca)
	if oerr != nil {
		t.Fatal(oerr)
	}
	if t2.ExpiresIn > 1800 {
		t.Fatalf("the access token outlives the session limit: expires_in %d", t2.ExpiresIn)
	}
	f.clock = f.clock.Add(31 * time.Minute)
	_, oerr = f.svc.Exchange(ctx, refreshForm(t2.RefreshToken), ca)
	wantOAuthErr(t, oerr, "invalid_grant")
}

func TestRevocationDisablesTheAgentAndCallsOnRevoke(t *testing.T) {
	f := newFixture(t)
	acc := access.New(f.db)
	var revoked []string
	var mu sync.Mutex
	f.svc = New(Options{DB: f.db, PublicURL: testPublic, Settings: f.settings, Access: acc, Now: func() time.Time { return f.clock },
		OnRevoke: func(_ context.Context, agentID string) { mu.Lock(); revoked = append(revoked, agentID); mu.Unlock() }})
	redirect := "https://client.example/cb"
	c := f.register(t, "none", redirect)
	ca := ClientAuth{ClientID: c.ClientID}
	ctx := context.Background()
	issue := func() *TokenResponse {
		verifier, challenge := pkcePair(t)
		code := f.approve(t, f.user, c.ClientID, redirect, challenge, Consent{Nickname: "Revoked"})
		tok, oerr := f.svc.Exchange(ctx, codeForm(code, redirect, verifier), ca)
		if oerr != nil {
			t.Fatal(oerr)
		}
		return tok
	}
	tok := issue()
	ag, err := f.svc.VerifyAccessToken(ctx, tok.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	if acc.ScopeFor(ctx, ag.ID).Denied {
		t.Fatal("an active app is denied")
	}
	// By the owner.
	app, _ := f.svc.AppByAgent(ctx, ag.ID)
	if err := f.svc.RevokeApp(ctx, app, f.user); err != nil {
		t.Fatal(err)
	}
	if !acc.ScopeFor(ctx, ag.ID).Denied {
		t.Fatal("a revoked app's agent still reaches tools (a queued approval would run)")
	}
	// By its client.
	tok = issue()
	if oerr := f.svc.Revoke(ctx, tok.RefreshToken, ca); oerr != nil {
		t.Fatal(oerr)
	}
	// By reuse detection.
	tok = issue()
	t2, _ := f.svc.Exchange(ctx, refreshForm(tok.RefreshToken), ca)
	f.clock = f.clock.Add(refreshReuseGrace + time.Second)
	_, _ = f.svc.Exchange(ctx, refreshForm(tok.RefreshToken), ca)
	if _, err := f.svc.VerifyAccessToken(ctx, t2.AccessToken); err == nil {
		t.Fatal("reuse did not revoke")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(revoked) != 3 || revoked[0] != ag.ID || revoked[1] != ag.ID || revoked[2] != ag.ID {
		t.Fatalf("OnRevoke calls: %v, want the agent three times", revoked)
	}
}

func TestReconnectAfterRevokeRevivesNothingOld(t *testing.T) {
	f := newFixture(t)
	redirect := "https://client.example/cb"
	c := f.register(t, "none", redirect)
	ca := ClientAuth{ClientID: c.ClientID}
	ctx := context.Background()
	verifier, challenge := pkcePair(t)
	code := f.approve(t, f.user, c.ClientID, redirect, challenge, Consent{Nickname: "Old"})
	tok, oerr := f.svc.Exchange(ctx, codeForm(code, redirect, verifier), ca)
	if oerr != nil {
		t.Fatal(oerr)
	}
	staleVerifier, staleChallenge := pkcePair(t)
	staleCode := f.approve(t, f.user, c.ClientID, redirect, staleChallenge, Consent{Nickname: "Old"})
	// A grant marked revoked while its tokens were not (another node's
	// half-finished revoke, an operator's SQL): reconnecting must not wake
	// them up.
	if _, err := f.db.Exec(`UPDATE mcp_oauth_grants SET revoked_at = ? WHERE client_id = ?`, f.clock.UnixMilli(), c.ClientID); err != nil {
		t.Fatal(err)
	}
	verifier2, challenge2 := pkcePair(t)
	code2 := f.approve(t, f.user, c.ClientID, redirect, challenge2, Consent{Nickname: "New"})
	if _, err := f.svc.VerifyAccessToken(ctx, tok.AccessToken); err == nil {
		t.Fatal("an access token from before the revoke works again")
	}
	if _, oerr := f.svc.Exchange(ctx, refreshForm(tok.RefreshToken), ca); oerr == nil {
		t.Fatal("a refresh token from before the revoke works again")
	}
	if _, oerr := f.svc.Exchange(ctx, codeForm(staleCode, redirect, staleVerifier), ca); oerr == nil {
		t.Fatal("a code from before the revoke works again")
	}
	if _, oerr := f.svc.Exchange(ctx, codeForm(code2, redirect, verifier2), ca); oerr != nil {
		t.Fatalf("the new consent's code: %v", oerr)
	}
}

func TestManageFlagGivesAdminControlOnlyWhenChosen(t *testing.T) {
	f := newFixture(t)
	acc := access.New(f.db)
	ctx := context.Background()
	now := time.Now().UnixMilli()
	if _, err := f.db.Exec(`INSERT INTO users(id, username, password_hash, role, status, created_at, updated_at) VALUES('u_member','member','','member','active',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	member, _ := identity.New(f.db).GetUserByID(ctx, "u_member")
	agentFor := func(u *identity.User, redirect string, manage bool) string {
		c := f.register(t, "none", redirect)
		verifier, challenge := pkcePair(t)
		code := f.approve(t, u, c.ClientID, redirect, challenge, Consent{Nickname: "App", MayManage: manage})
		tok, oerr := f.svc.Exchange(ctx, codeForm(code, redirect, verifier), ClientAuth{ClientID: c.ClientID})
		if oerr != nil {
			t.Fatal(oerr)
		}
		ag, err := f.svc.VerifyAccessToken(ctx, tok.AccessToken)
		if err != nil {
			t.Fatal(err)
		}
		return ag.ID
	}
	plain := agentFor(f.user, "https://a.example/cb", false)
	sc := acc.ScopeFor(ctx, plain)
	if sc.All || !sc.OwnerIsAdmin() || sc.CanAdminister() || !sc.AllowsTool("builtin", "memory.delete") || sc.LimitedOut("builtin", "memory.delete") {
		t.Fatalf("an admin's app with scope 'everything': %+v, want every tool but no admin control", sc)
	}
	managed := agentFor(f.user, "https://b.example/cb", true)
	if !acc.ScopeFor(ctx, managed).CanAdminister() {
		t.Fatal("an app the admin let manage toolyard has no admin control")
	}
	memberApp := agentFor(member, "https://c.example/cb", true)
	if acc.ScopeFor(ctx, memberApp).CanAdminister() {
		t.Fatal("a member's app got admin control")
	}
	var stored int
	if err := f.db.QueryRow(`SELECT may_manage FROM mcp_oauth_grants WHERE agent_id = ?`, memberApp).Scan(&stored); err != nil || stored != 0 {
		t.Fatalf("a member's consent stored may_manage = %d (%v)", stored, err)
	}
	app, _ := f.svc.AppByAgent(ctx, managed)
	if !app.MayManage {
		t.Fatal("App.MayManage not reported")
	}
	if err := f.svc.SetAppManage(ctx, app, false, f.user); err != nil {
		t.Fatal(err)
	}
	acc.InvalidateAgent(managed)
	if acc.ScopeFor(ctx, managed).CanAdminister() {
		t.Fatal("turning may_manage off kept admin control")
	}
}

func TestDeletedAgentKeepsItsGrantAsATombstone(t *testing.T) {
	f := newFixture(t)
	redirect := "https://client.example/cb"
	c := f.register(t, "none", redirect)
	ca := ClientAuth{ClientID: c.ClientID}
	ctx := context.Background()
	verifier, challenge := pkcePair(t)
	code := f.approve(t, f.user, c.ClientID, redirect, challenge, Consent{Nickname: "Tomb"})
	tok, oerr := f.svc.Exchange(ctx, codeForm(code, redirect, verifier), ca)
	if oerr != nil {
		t.Fatal(oerr)
	}
	ag, _ := f.svc.VerifyAccessToken(ctx, tok.AccessToken)
	// Audit rows written for the agent carry the OAuth client id.
	if _, err := f.db.Exec(`INSERT INTO audit_events(id, ts, event_type, agent_id) VALUES('ev_t1', 1, 'call.succeeded', ?)`, ag.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`DELETE FROM agents WHERE id = ?`, ag.ID); err != nil {
		t.Fatal(err)
	}
	var clientOnRow string
	if err := f.db.QueryRow(`SELECT COALESCE(oauth_client_id, '') FROM audit_events WHERE id = 'ev_t1'`).Scan(&clientOnRow); err != nil || clientOnRow != c.ClientID {
		t.Fatalf("audit row client id after the agent was deleted: %q %v", clientOnRow, err)
	}
	if _, err := f.svc.VerifyAccessToken(ctx, tok.AccessToken); err == nil {
		t.Fatal("a deleted agent's token works")
	}
	_, oerr = f.svc.Exchange(ctx, refreshForm(tok.RefreshToken), ca)
	wantOAuthErr(t, oerr, "invalid_grant")
	apps, err := f.svc.ListApps(ctx, f.user.ID)
	if err != nil || len(apps) != 1 || apps[0].Status != "deleted" || apps[0].AgentID != ag.ID {
		t.Fatalf("apps after the agent was deleted: %+v %v", apps, err)
	}
	// Connecting again recreates the agent under the same id.
	verifier, challenge = pkcePair(t)
	code = f.approve(t, f.user, c.ClientID, redirect, challenge, Consent{Nickname: "Back"})
	tok, oerr = f.svc.Exchange(ctx, codeForm(code, redirect, verifier), ca)
	if oerr != nil {
		t.Fatal(oerr)
	}
	if back, err := f.svc.VerifyAccessToken(ctx, tok.AccessToken); err != nil || back.ID != ag.ID || back.Name != "Back" {
		t.Fatalf("reconnect after delete: %+v %v", back, err)
	}
}
