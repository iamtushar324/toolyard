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
	"strconv"
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
		// Only 2000::/3 is public IPv6.
		"4000::1": true, "1::1": true, "e000::1": true, "3ffe:1::1": false,
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
	defer func(g, p int) { registerGlobal, authorizePerIP = g, p }(registerGlobal, authorizePerIP)
	registerGlobal, authorizePerIP = 2, 2
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
	// Authorizations for a known client have no global cap, only the
	// per-address and per-network tiers: five addresses of one /24 at two
	// each make ten, past the network's eight; another /24 is unaffected.
	c := f.register(t, "none", "https://ok.example/cb") // direct Register skips the HTTP limits
	_, challenge := pkcePair(t)
	authorize := func(ip string) int {
		_, aerr := f.svc.BeginAuthorize(context.Background(), authorizeQuery(c.ClientID, "https://ok.example/cb", challenge, nil), ip)
		if aerr != nil {
			return aerr.Status
		}
		return http.StatusOK
	}
	refused := 0
	for i := 1; i <= 5; i++ {
		for j := 0; j < 2; j++ {
			if authorize("203.0.113."+string(rune('0'+i))) == http.StatusTooManyRequests {
				refused++
			}
		}
	}
	if refused != 2 {
		t.Fatalf("%d of ten authorizations from one /24 refused, want 2 past the network tier", refused)
	}
	if got := authorize("198.51.100.77"); got != http.StatusOK {
		t.Fatalf("another network was refused: %d", got)
	}
	if got := authorize("203.0.113.6"); got != http.StatusTooManyRequests {
		t.Fatalf("a fresh address of the full /24: %d, want 429", got)
	}
}

// TestGlobalMemoryBucketIsCheckedBeforeDatabaseWrites: once a global
// bucket is full in memory, a request writes no per-address row.
func TestGlobalMemoryBucketIsCheckedBeforeDatabaseWrites(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	global := limit{bucket: "test:global", max: 1, window: time.Minute, global: true}
	if !f.svc.allowAll(ctx, limit{bucket: "test:ip:a", max: 10, window: time.Minute}, global) {
		t.Fatal("first request refused")
	}
	if f.svc.allowAll(ctx, limit{bucket: "test:ip:b", max: 10, window: time.Minute}, global) {
		t.Fatal("a request past the global cap was allowed")
	}
	var n int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM mcp_oauth_rate_limits WHERE bucket = 'test:ip:b'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("a refused request wrote %d per-address rows (%v)", n, err)
	}
}

// TestRefusedAddressDoesNotUseTheGlobalBudget: requests that one address
// makes past its own limit are refused without counting toward the global
// bucket, so one address cannot lock everyone out.
func TestRefusedAddressDoesNotUseTheGlobalBudget(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	global := limit{bucket: "test:global", max: 3, window: time.Minute, global: true}
	ip := func(name string) limit { return limit{bucket: "test:ip:" + name, max: 1, window: time.Minute} }
	if !f.svc.allowAll(ctx, ip("flood"), global) {
		t.Fatal("the flooding address's first request was refused")
	}
	for i := 0; i < 20; i++ {
		if f.svc.allowAll(ctx, ip("flood"), global) {
			t.Fatal("an address past its limit was allowed")
		}
	}
	if !f.svc.allowAll(ctx, ip("b"), global) || !f.svc.allowAll(ctx, ip("c"), global) {
		t.Fatal("other addresses were locked out by one address's refused requests")
	}
	if f.svc.allowAll(ctx, ip("d"), global) {
		t.Fatal("the global cap did not hold")
	}
	var hits int
	if err := f.db.QueryRow(`SELECT hits FROM mcp_oauth_rate_limits WHERE bucket = 'test:global'`).Scan(&hits); err != nil || hits != 3 {
		t.Fatalf("global hits in the database: %d (%v), want 3", hits, err)
	}
}

// TestMemLimiterKeepsGlobalBuckets: a flood of fresh addresses evicts old
// per-address buckets, never a global one, and a refused request creates
// no bucket.
func TestMemLimiterKeepsGlobalBuckets(t *testing.T) {
	var l memLimiter
	now := time.Now()
	global := limit{bucket: "global", max: 5, window: time.Hour, global: true}
	for i := 0; i < 3; i++ {
		if !l.admit(now, []limit{global}) {
			t.Fatal("global refused early")
		}
	}
	for i := 0; i < maxMemBuckets+maxMemBuckets/5; i++ {
		l.admit(now, []limit{{bucket: "ip:" + strconv.Itoa(i), max: 10, window: time.Hour}})
	}
	if len(l.m) > maxMemBuckets {
		t.Fatalf("%d buckets, over the cap", len(l.m))
	}
	if !l.admit(now, []limit{global}) || !l.admit(now, []limit{global}) || l.admit(now, []limit{global}) {
		t.Fatal("the global bucket was reset by the flood of addresses")
	}
	if l.admit(now, []limit{{bucket: "ip:new", max: 10, window: time.Hour}, global}) {
		t.Fatal("a request past the full global bucket was admitted")
	}
	if _, ok := l.m["ip:new"]; ok {
		t.Fatal("a refused request created a per-address bucket")
	}
}

func TestAuthorizeHasAGlobalCeiling(t *testing.T) {
	f := newFixture(t)
	defer func(g int) { authorizeGlobal = g }(authorizeGlobal)
	authorizeGlobal = 1
	c := f.register(t, "none", "https://ok.example/cb")
	_, challenge := pkcePair(t)
	if _, aerr := f.svc.BeginAuthorize(context.Background(), authorizeQuery(c.ClientID, "https://ok.example/cb", challenge, nil), "198.51.100.1"); aerr != nil {
		t.Fatal(aerr)
	}
	if _, aerr := f.svc.BeginAuthorize(context.Background(), authorizeQuery(c.ClientID, "https://ok.example/cb", challenge, nil), "203.0.113.1"); aerr == nil || aerr.Status != http.StatusTooManyRequests {
		t.Fatalf("past the global ceiling: %v", aerr)
	}
}

func TestDocumentFetchesHaveAGlobalCap(t *testing.T) {
	f := newFixture(t)
	defer func(g int) { cimdFetchGlobal = g }(cimdFetchGlobal)
	cimdFetchGlobal = 1
	srv, _ := cimdServer(t, func(u string) any {
		return map[string]any{"client_id": u, "redirect_uris": []string{"https://a.example/cb"}}
	}, http.StatusOK)
	svc := cimdService(f, srv, true)
	ctx := context.Background()
	if _, oerr := svc.ResolveClient(ctx, srv.URL+"/one.json", "198.51.100.1"); oerr != nil {
		t.Fatal(oerr)
	}
	if _, oerr := svc.ResolveClient(ctx, srv.URL+"/two.json", "203.0.113.1"); oerr == nil || oerr.Code != "temporarily_unavailable" {
		t.Fatalf("a fetch past the global cap: %v", oerr)
	}
	// A cached client needs no fetch, so the cap does not apply to it.
	if _, oerr := svc.ResolveClient(ctx, srv.URL+"/one.json", "203.0.113.1"); oerr != nil {
		t.Fatalf("a cached client hit the fetch cap: %v", oerr)
	}
}

// TestTransientFetchFailuresServeTheStaleCopy: a network failure or a
// temporary HTTP answer (408, 429, 5xx) is not cached as a failure, and a
// stale copy keeps serving, for at most cimdStaleMax after it expired; a
// definite failure (the document answered wrongly) is cached and serves no
// stale copy.
func TestTransientFetchFailuresServeTheStaleCopy(t *testing.T) {
	f := newFixture(t)
	var status int32 = http.StatusOK
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if st := int(atomic.LoadInt32(&status)); st != http.StatusOK {
			w.WriteHeader(st)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "max-age=600")
		_, _ = w.Write([]byte(`{"client_id":"` + srv.URL + r.URL.Path + `","client_name":"Stale","redirect_uris":["https://a.example/cb"]}`))
	}))
	defer srv.Close()
	svc := cimdService(f, srv, true)
	ctx := context.Background()
	id := srv.URL + "/stale.json"
	negKey := "neg:cimd:" + hashSecret(id)
	if _, oerr := svc.ResolveClient(ctx, id, "198.51.100.1"); oerr != nil {
		t.Fatal(oerr)
	}
	for _, st := range []int{http.StatusServiceUnavailable, http.StatusTooManyRequests, http.StatusRequestTimeout, http.StatusBadGateway} {
		f.clock = f.clock.Add(11 * time.Minute) // past max-age
		atomic.StoreInt32(&status, int32(st))
		c, oerr := svc.ResolveClient(ctx, id, "198.51.100.1")
		if oerr != nil || c == nil || c.Name != "Stale" {
			t.Fatalf("HTTP %d did not serve the stale copy: %+v %v", st, c, oerr)
		}
		if svc.recalled(ctx, negKey) {
			t.Fatalf("HTTP %d was cached as a failure", st)
		}
	}
	// Past cimdStaleMax the stale copy no longer serves.
	f.clock = f.clock.Add(cimdStaleMax + time.Hour)
	if _, oerr := svc.ResolveClient(ctx, id, "198.51.100.1"); oerr == nil {
		t.Fatal("a copy older than cimdStaleMax still served")
	}
	// A definite failure is cached and serves no stale copy.
	atomic.StoreInt32(&status, http.StatusOK)
	if _, oerr := svc.ResolveClient(ctx, id, "198.51.100.1"); oerr != nil {
		t.Fatal(oerr)
	}
	f.clock = f.clock.Add(11 * time.Minute)
	atomic.StoreInt32(&status, http.StatusNotFound)
	if _, oerr := svc.ResolveClient(ctx, id, "198.51.100.1"); oerr == nil {
		t.Fatal("a definite failure served the stale copy")
	}
	if !svc.recalled(ctx, negKey) {
		t.Fatal("a definite failure (HTTP 404) was not cached")
	}
	// A network failure of a client never seen is refused, not cached.
	srv.Close()
	if _, oerr := svc.ResolveClient(ctx, srv.URL+"/never-seen.json", "198.51.100.1"); oerr == nil {
		t.Fatal("an unknown client with a network failure was accepted")
	}
	if svc.recalled(ctx, "neg:cimd:"+hashSecret(srv.URL+"/never-seen.json")) {
		t.Fatal("a network failure was cached as a failure")
	}
}

// TestFullFetchBudgetServesTheStaleCopy: with every fetch bucket full, a
// known client still works from its expired copy instead of a 429.
func TestFullFetchBudgetServesTheStaleCopy(t *testing.T) {
	f := newFixture(t)
	defer func(g int) { cimdFetchGlobal = g }(cimdFetchGlobal)
	srv, _ := cimdServer(t, func(u string) any {
		return map[string]any{"client_id": u, "client_name": "Known", "redirect_uris": []string{"https://a.example/cb"}}
	}, http.StatusOK)
	svc := cimdService(f, srv, true)
	ctx := context.Background()
	id := srv.URL + "/known.json"
	if _, oerr := svc.ResolveClient(ctx, id, "198.51.100.1"); oerr != nil {
		t.Fatal(oerr)
	}
	cimdFetchGlobal = 0 // every fetch refused from here
	f.clock = f.clock.Add(11 * time.Minute)
	if c, oerr := svc.ResolveClient(ctx, id, "198.51.100.1"); oerr != nil || c.Name != "Known" {
		t.Fatalf("a full fetch budget refused a known client: %+v %v", c, oerr)
	}
	if _, oerr := svc.ResolveClient(ctx, srv.URL+"/unknown.json", "198.51.100.1"); oerr == nil || oerr.Code != "temporarily_unavailable" {
		t.Fatalf("an unknown client with a full fetch budget: %v", oerr)
	}
}

// TestSharedFetchSurvivesTheFirstCallerLeaving: the request that started a
// fetch going away does not fail the requests waiting on it.
func TestSharedFetchSurvivesTheFirstCallerLeaving(t *testing.T) {
	f := newFixture(t)
	release := make(chan struct{})
	var slow *httptest.Server
	slow = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"client_id":"` + slow.URL + r.URL.Path + `","redirect_uris":["https://a.example/cb"]}`))
	}))
	defer slow.Close()
	svc := cimdService(f, slow, true)
	id := slow.URL + "/leave.json"
	first, cancel := context.WithCancel(context.Background())
	firstDone := make(chan *Error, 1)
	go func() {
		_, oerr := svc.ResolveClient(first, id, "198.51.100.1")
		firstDone <- oerr
	}()
	time.Sleep(150 * time.Millisecond)
	waiterDone := make(chan *Error, 1)
	go func() {
		_, oerr := svc.ResolveClient(context.Background(), id, "198.51.100.2")
		waiterDone <- oerr
	}()
	time.Sleep(150 * time.Millisecond)
	cancel() // the first requester disconnects
	time.Sleep(50 * time.Millisecond)
	close(release)
	if oerr := <-waiterDone; oerr != nil {
		t.Fatalf("the waiting request failed because the first one left: %v", oerr)
	}
	<-firstDone
	if _, oerr := svc.Client(context.Background(), id); oerr != nil {
		t.Fatalf("the shared fetch was not stored: %v", oerr)
	}
}

// TestTwoDevicesDoNotSignEachOtherOut: a client metadata document client
// shares one client_id across installs, so one person's two devices share
// a grant. A consent on the second device keeps the first one's session;
// unused codes from before stop; "sign out this app's other sessions"
// stops them all.
func TestTwoDevicesDoNotSignEachOtherOut(t *testing.T) {
	f := newFixture(t)
	redirect := "https://client.example/cb"
	c := f.register(t, "none", redirect)
	ca := ClientAuth{ClientID: c.ClientID}
	ctx := context.Background()
	device := func(c Consent) *TokenResponse {
		verifier, challenge := pkcePair(t)
		code := f.approve(t, f.user, ca.ClientID, redirect, challenge, c)
		tok, oerr := f.svc.Exchange(ctx, codeForm(code, redirect, verifier), ca)
		if oerr != nil {
			t.Fatal(oerr)
		}
		return tok
	}
	laptop := device(Consent{Nickname: "App"})
	_, unusedChallenge := pkcePair(t)
	unused := f.approve(t, f.user, ca.ClientID, redirect, unusedChallenge, Consent{Nickname: "App"})
	phone := device(Consent{Nickname: "App"})
	for name, tok := range map[string]*TokenResponse{"laptop": laptop, "phone": phone} {
		if _, err := f.svc.VerifyAccessToken(ctx, tok.AccessToken); err != nil {
			t.Fatalf("the %s was signed out by the other device's consent: %v", name, err)
		}
	}
	if _, oerr := f.svc.Exchange(ctx, codeForm(unused, redirect, "x"), ca); oerr == nil {
		t.Fatal("an unused code from before the new consent works")
	}
	if _, oerr := f.svc.Exchange(ctx, refreshForm(laptop.RefreshToken), ca); oerr != nil {
		t.Fatalf("the laptop cannot refresh: %v", oerr)
	}
	tablet := device(Consent{Nickname: "App", SignOutOthers: true})
	if _, err := f.svc.VerifyAccessToken(ctx, phone.AccessToken); err == nil {
		t.Fatal("sign out this app's other sessions kept the phone")
	}
	if _, err := f.svc.VerifyAccessToken(ctx, tablet.AccessToken); err != nil {
		t.Fatalf("the consenting device: %v", err)
	}
}

// TestDeleteAgentRevokesItsGrant: deleting an OAuth client's agent revokes
// its grant, tokens and codes at once, and reconnecting brings none back.
func TestDeleteAgentRevokesItsGrant(t *testing.T) {
	f := newFixture(t)
	redirect := "https://client.example/cb"
	c := f.register(t, "none", redirect)
	ca := ClientAuth{ClientID: c.ClientID}
	ctx := context.Background()
	verifier, challenge := pkcePair(t)
	code := f.approve(t, f.user, c.ClientID, redirect, challenge, Consent{Nickname: "Deleted"})
	tok, oerr := f.svc.Exchange(ctx, codeForm(code, redirect, verifier), ca)
	if oerr != nil {
		t.Fatal(oerr)
	}
	apps, err := f.svc.ListApps(ctx, f.user.ID)
	if err != nil || len(apps) != 1 {
		t.Fatalf("apps: %+v %v", apps, err)
	}
	// An unused code of the grant, as if a second sign-in were under way.
	now := f.clock.UnixMilli()
	if _, err := f.db.Exec(`INSERT INTO mcp_oauth_codes(code_hash, grant_id, client_id, redirect_uri, code_challenge, resource, scope, family_id, created_at, expires_at)
        VALUES('code_pending', ?, ?, ?, 'x', ?, 'mcp', 'fam_pending', ?, ?)`, apps[0].ID, c.ClientID, redirect, testPublic+"/mcp", now, now+60000); err != nil {
		t.Fatal(err)
	}
	if err := identity.New(f.db).DeleteAgent(ctx, f.user.ID, apps[0].AgentID); err != nil {
		t.Fatal(err)
	}
	var reason string
	var live, unused int
	if err := f.db.QueryRow(`SELECT COALESCE(revoked_reason, '') FROM mcp_oauth_grants WHERE agent_id = ?`, apps[0].AgentID).Scan(&reason); err != nil || reason != "agent_deleted" {
		t.Fatalf("grant after delete: %q %v", reason, err)
	}
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM mcp_oauth_tokens WHERE revoked_at IS NULL`).Scan(&live); err != nil || live != 0 {
		t.Fatalf("%d tokens still live after delete (%v)", live, err)
	}
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM mcp_oauth_codes WHERE used_at IS NULL`).Scan(&unused); err != nil || unused != 0 {
		t.Fatalf("%d codes still usable after delete (%v)", unused, err)
	}
	// Reconnecting recreates the agent under its id; nothing from before
	// comes back.
	verifier, challenge = pkcePair(t)
	code = f.approve(t, f.user, c.ClientID, redirect, challenge, Consent{Nickname: "Back"})
	if _, err := f.svc.VerifyAccessToken(ctx, tok.AccessToken); err == nil {
		t.Fatal("an access token from before the delete works")
	}
	if _, oerr := f.svc.Exchange(ctx, refreshForm(tok.RefreshToken), ca); oerr == nil {
		t.Fatal("a refresh token from before the delete works")
	}
	fresh, oerr := f.svc.Exchange(ctx, codeForm(code, redirect, verifier), ca)
	if oerr != nil {
		t.Fatal(oerr)
	}
	if back, err := f.svc.VerifyAccessToken(ctx, fresh.AccessToken); err != nil || back.ID != apps[0].AgentID {
		t.Fatalf("reconnect after delete: %+v %v", back, err)
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
	// Three revocations, and two revivals (each new consent after a
	// revoke cancels whatever was still pending from before): five calls.
	if len(revoked) != 5 {
		t.Fatalf("OnRevoke calls: %v, want 5", revoked)
	}
	for _, id := range revoked {
		if id != ag.ID {
			t.Fatalf("OnRevoke called for %q, want %q", id, ag.ID)
		}
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
	if !app.MayManage || !app.OwnerIsAdmin || !app.ManageActive {
		t.Fatalf("App flags: %+v", app)
	}
	// Only the app's owner, while an admin, may turn it on: not another
	// admin, and never for a member's app.
	if _, err := f.db.Exec(`INSERT INTO users(id, username, password_hash, role, status, created_at, updated_at) VALUES('u_admin2','admin2','','admin','active',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	admin2, _ := identity.New(f.db).GetUserByID(ctx, "u_admin2")
	if err := f.svc.SetAppManage(ctx, app, true, admin2); err != ErrManageOwnerOnly {
		t.Fatalf("another admin turned may_manage on: %v", err)
	}
	mApp, _ := f.svc.AppByAgent(ctx, memberApp)
	if err := f.svc.SetAppManage(ctx, mApp, true, f.user); err != ErrManageOwnerOnly {
		t.Fatalf("an admin turned may_manage on for a member's app: %v", err)
	}
	if err := f.svc.SetAppManage(ctx, mApp, true, member); err != ErrManageOwnerOnly {
		t.Fatalf("a member turned may_manage on: %v", err)
	}
	// A stored flag of a member's app has no effect and says so.
	if _, err := f.db.Exec(`UPDATE mcp_oauth_grants SET may_manage = 1 WHERE agent_id = ?`, memberApp); err != nil {
		t.Fatal(err)
	}
	mApp, _ = f.svc.AppByAgent(ctx, memberApp)
	acc.InvalidateAgent(memberApp)
	if !mApp.MayManage || mApp.OwnerIsAdmin || mApp.ManageActive || acc.ScopeFor(ctx, memberApp).CanAdminister() {
		t.Fatalf("a member's app with the flag stored: %+v", mApp)
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

// TestAgentDeletedRunsTheCancelHook: deleting an app's agent through the
// generic routes cancels what it had pending, on a context the request
// ending does not cancel; an ordinary agent's delete runs nothing.
func TestAgentDeletedRunsTheCancelHook(t *testing.T) {
	f := newFixture(t)
	var got []string
	var liveCtx bool
	f.svc = New(Options{DB: f.db, PublicURL: testPublic, Settings: f.settings, Now: func() time.Time { return f.clock },
		OnRevoke: func(ctx context.Context, agentID string) { got = append(got, agentID); liveCtx = ctx.Err() == nil }})
	redirect := "https://client.example/cb"
	c := f.register(t, "none", redirect)
	verifier, challenge := pkcePair(t)
	code := f.approve(t, f.user, c.ClientID, redirect, challenge, Consent{Nickname: "Gone"})
	tok, oerr := f.svc.Exchange(context.Background(), codeForm(code, redirect, verifier), ClientAuth{ClientID: c.ClientID})
	if oerr != nil {
		t.Fatal(oerr)
	}
	ag, _ := f.svc.VerifyAccessToken(context.Background(), tok.AccessToken)
	if err := identity.New(f.db).DeleteAgent(context.Background(), f.user.ID, ag.ID); err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	f.svc.AgentDeleted(cancelled, ag.ID)
	if len(got) != 1 || got[0] != ag.ID || !liveCtx {
		t.Fatalf("OnRevoke after delete: %v (live context %v)", got, liveCtx)
	}
	_, plain, err := identity.New(f.db).CreateAgentWithToken(context.Background(), f.user.ID, "plain")
	if err != nil {
		t.Fatal(err)
	}
	f.svc.AgentDeleted(context.Background(), plain.ID)
	if len(got) != 1 {
		t.Fatalf("OnRevoke ran for an ordinary agent: %v", got)
	}
}

// TestGrantLocksComeFirst: code exchange and refresh lock the grant row
// before their code or token, like Approve; a refresh racing a consent on
// the same grant completes both (no deadlock on Postgres).
func TestGrantLocksComeFirst(t *testing.T) {
	f := newFixture(t)
	redirect := "https://client.example/cb"
	c := f.register(t, "none", redirect)
	ca := ClientAuth{ClientID: c.ClientID}
	ctx := context.Background()
	verifier, challenge := pkcePair(t)
	code := f.approve(t, f.user, c.ClientID, redirect, challenge, Consent{Nickname: "Locks"})
	tok, oerr := f.svc.Exchange(ctx, codeForm(code, redirect, verifier), ca)
	if oerr != nil {
		t.Fatal(oerr)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	refresh := tok.RefreshToken
	var mu sync.Mutex
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			mu.Lock()
			rt := refresh
			mu.Unlock()
			if resp, oerr := f.svc.Exchange(ctx, refreshForm(rt), ca); oerr == nil {
				mu.Lock()
				refresh = resp.RefreshToken
				mu.Unlock()
			} else if oerr.Code != "invalid_grant" {
				errs <- oerr
			}
		}()
		go func() {
			defer wg.Done()
			_, ch := pkcePair(t)
			req, aerr := f.svc.BeginAuthorize(ctx, authorizeQuery(c.ClientID, redirect, ch, nil), "198.51.100.7")
			if aerr != nil {
				errs <- aerr.Error
				return
			}
			if _, err := f.svc.Approve(ctx, req.ID, f.user, Consent{Nickname: "Locks"}); err != nil {
				errs <- err
			}
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("a refresh racing a consent did not finish (deadlock)")
	}
	close(errs)
	for err := range errs {
		t.Errorf("racing refresh and consent: %v", err)
	}
}
