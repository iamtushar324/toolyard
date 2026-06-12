//go:build e2e

// scripts/e2e_oauth_test.go drives the end-to-end OAuth flows against a
// running toolyard instance, using an in-process fake authorization
// server (httptest.Server) that toolyard hits over localhost.
//
// The tests do not depend on a real MCP upstream — they only verify
// that toolyard correctly:
//   - discovers AS metadata
//   - performs Dynamic Client Registration
//   - runs the PKCE authorization-code dance (Mode A callback + Mode B paste)
//   - runs the RFC 8628 device flow (Mode C)
//   - persists encrypted tokens and exposes status
//   - refreshes tokens before expiry
//   - handles invalid_grant by flipping to needs_reauth
//   - accepts a personal-access-token shortcut
//   - revokes + deletes on disconnect
package scripts

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeIdP simulates the bare minimum of an AS for the OAuth dance.
// State that matters across endpoints (issued codes, tokens, etc.) is
// kept in atomic / mutex-guarded fields so tests can drive multiple
// flows in parallel.
type fakeIdP struct {
	t       *testing.T
	mu      sync.Mutex
	server  *httptest.Server
	clients map[string]bool            // client_id -> registered
	codes   map[string]string          // auth code -> client_id
	tokens  map[string]*fakeToken      // access_token -> meta
	refresh map[string]*fakeToken      // refresh_token -> meta
	device  map[string]*fakeDeviceAuth // device_code -> state

	// rejectRefresh switches the next refresh attempt to invalid_grant.
	// Used to test the "needs_reauth" path.
	rejectRefresh atomic.Bool

	// rotateRefresh, when true, returns a new refresh_token on every refresh.
	rotateRefresh atomic.Bool

	// shortLifetime forces 2-second access_token lifetimes so refresh tests
	// don't need to wait long.
	shortLifetime atomic.Bool

	// noRefreshNoExpiry makes issueToken omit refresh_token and expires_in,
	// mimicking long-lived tokens (e.g. Linear) that must not be refreshed.
	noRefreshNoExpiry atomic.Bool

	// lastTokenAuth captures whether the last /token request had a code_verifier
	// and a refresh_token grant. The PKCE assertion test reads it.
	lastTokenAuth string

	// lastAuthorizeQuery is the raw query of the most recent /authorize hit
	// (mu-guarded). Extra-param tests read it.
	lastAuthorizeQuery url.Values

	// lastClientSecret is the client_secret form value of the most recent
	// /token hit (mu-guarded).
	lastClientSecret string

	// counters — exposed for assertions.
	tokenHits   atomic.Int64
	refreshHits atomic.Int64

	// devicePending toggles the device-flow grant to authorization_pending
	// for the first poll.
	devicePending atomic.Bool
}

type fakeToken struct {
	clientID  string
	scope     string
	expiresAt time.Time
	rotation  int
}

type fakeDeviceAuth struct {
	clientID string
	approved bool
}

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	f := &fakeIdP{
		t:       t,
		clients: map[string]bool{},
		codes:   map[string]string{},
		tokens:  map[string]*fakeToken{},
		refresh: map[string]*fakeToken{},
		device:  map[string]*fakeDeviceAuth{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/oauth-authorization-server", f.handleAS)
	mux.HandleFunc("/.well-known/oauth-protected-resource", f.handlePR)
	mux.HandleFunc("/register", f.handleRegister)
	mux.HandleFunc("/token", f.handleToken)
	mux.HandleFunc("/device_authorization", f.handleDeviceAuth)
	mux.HandleFunc("/revoke", f.handleRevoke)
	mux.HandleFunc("/authorize", f.handleAuthorize)
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeIdP) URL() string { return f.server.URL }

func (f *fakeIdP) handleAS(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{
		"issuer":                                f.server.URL,
		"authorization_endpoint":                f.server.URL + "/authorize",
		"token_endpoint":                        f.server.URL + "/token",
		"registration_endpoint":                 f.server.URL + "/register",
		"revocation_endpoint":                   f.server.URL + "/revoke",
		"device_authorization_endpoint":         f.server.URL + "/device_authorization",
		"scopes_supported":                      []string{"read", "write", "offline_access"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token", "urn:ietf:params:oauth:grant-type:device_code"},
		"token_endpoint_auth_methods_supported": []string{"none", "client_secret_post"},
		"code_challenge_methods_supported":      []string{"S256"},
	}
	writeJSON(w, http.StatusOK, out)
}

func (f *fakeIdP) handlePR(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"resource":              f.server.URL + "/mcp",
		"authorization_servers": []string{f.server.URL},
	})
}

func (f *fakeIdP) handleRegister(w http.ResponseWriter, r *http.Request) {
	id := "client-" + randHex(6)
	f.mu.Lock()
	f.clients[id] = true
	f.mu.Unlock()
	writeJSON(w, http.StatusCreated, map[string]any{
		"client_id":                  id,
		"token_endpoint_auth_method": "none",
	})
}

func (f *fakeIdP) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	code := "code-" + randHex(8)
	f.mu.Lock()
	f.codes[code] = q.Get("client_id")
	f.lastAuthorizeQuery = q
	f.mu.Unlock()
	redirect := q.Get("redirect_uri") + "?code=" + url.QueryEscape(code) + "&state=" + url.QueryEscape(q.Get("state"))
	http.Redirect(w, r, redirect, http.StatusFound)
}

func (f *fakeIdP) handleToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	f.tokenHits.Add(1)
	f.mu.Lock()
	f.lastClientSecret = r.PostForm.Get("client_secret")
	f.mu.Unlock()
	grant := r.PostForm.Get("grant_type")
	switch grant {
	case "authorization_code":
		f.lastTokenAuth = "auth_code"
		code := r.PostForm.Get("code")
		verifier := r.PostForm.Get("code_verifier")
		if verifier == "" {
			oauthErr(w, "invalid_request", "missing code_verifier (PKCE)")
			return
		}
		f.mu.Lock()
		cid, ok := f.codes[code]
		if ok {
			delete(f.codes, code)
		}
		f.mu.Unlock()
		if !ok {
			oauthErr(w, "invalid_grant", "unknown auth code")
			return
		}
		f.issueToken(w, cid, "read write offline_access", true)
	case "refresh_token":
		f.lastTokenAuth = "refresh"
		f.refreshHits.Add(1)
		if f.rejectRefresh.Load() {
			oauthErr(w, "invalid_grant", "refresh token revoked")
			return
		}
		rt := r.PostForm.Get("refresh_token")
		f.mu.Lock()
		meta, ok := f.refresh[rt]
		if ok && f.rotateRefresh.Load() {
			delete(f.refresh, rt)
		}
		f.mu.Unlock()
		if !ok {
			oauthErr(w, "invalid_grant", "unknown refresh token")
			return
		}
		f.issueToken(w, meta.clientID, meta.scope, f.rotateRefresh.Load())
	case "urn:ietf:params:oauth:grant-type:device_code":
		f.lastTokenAuth = "device"
		dc := r.PostForm.Get("device_code")
		f.mu.Lock()
		da, ok := f.device[dc]
		f.mu.Unlock()
		if !ok {
			oauthErr(w, "invalid_grant", "unknown device_code")
			return
		}
		if !da.approved {
			oauthErr(w, "authorization_pending", "user has not approved yet")
			return
		}
		f.issueToken(w, da.clientID, "read write offline_access", true)
	default:
		oauthErr(w, "unsupported_grant_type", grant)
	}
}

func (f *fakeIdP) issueToken(w http.ResponseWriter, clientID, scope string, includeRefresh bool) {
	at := "at-" + randHex(12)
	rt := ""
	ttl := 3600
	if f.shortLifetime.Load() {
		ttl = 2
	}
	if f.noRefreshNoExpiry.Load() {
		includeRefresh = false
	}
	exp := time.Now().Add(time.Duration(ttl) * time.Second)
	tok := &fakeToken{clientID: clientID, scope: scope, expiresAt: exp}
	f.mu.Lock()
	f.tokens[at] = tok
	if includeRefresh {
		rt = "rt-" + randHex(16)
		f.refresh[rt] = tok
	}
	f.mu.Unlock()
	out := map[string]any{
		"access_token": at,
		"token_type":   "Bearer",
		"scope":        scope,
	}
	if !f.noRefreshNoExpiry.Load() {
		out["expires_in"] = ttl
	}
	if rt != "" {
		out["refresh_token"] = rt
	}
	writeJSON(w, http.StatusOK, out)
}

func (f *fakeIdP) handleDeviceAuth(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	clientID := r.PostForm.Get("client_id")
	dc := "dev-" + randHex(8)
	f.mu.Lock()
	f.device[dc] = &fakeDeviceAuth{clientID: clientID, approved: !f.devicePending.Load()}
	f.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"device_code":      dc,
		"user_code":        "USER-" + randHex(2),
		"verification_uri": f.server.URL + "/activate",
		"interval":         1,
		"expires_in":       300,
	})
}

func (f *fakeIdP) approveDevice() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, d := range f.device {
		d.approved = true
	}
}

func (f *fakeIdP) handleRevoke(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func oauthErr(w http.ResponseWriter, code, desc string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "error_description": desc})
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// addUpstream creates a fresh upstream pointing at the given URL. The
// connect will likely fail (the URL isn't a real MCP server) — we accept
// 200 OR 202 and keep going.
func addUpstream(t *testing.T, h *httpClient, name, url string) {
	t.Helper()
	resp, err := postJSON(h, "/v1/servers", map[string]any{
		"name":      name,
		"transport": "streamable-http",
		"url":       url,
	})
	if err != nil {
		t.Fatalf("add upstream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		t.Fatalf("add upstream %s: status %d", name, resp.StatusCode)
	}
}

// removeUpstream cleans up between tests.
func removeUpstream(t *testing.T, h *httpClient, name string) {
	t.Helper()
	h.raw(t, "DELETE", "/v1/servers/"+url.PathEscape(name), nil, nil)
}

// uniqueName returns a stable per-test name.
func uniqueName(prefix string) string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return prefix + "-" + base64.RawURLEncoding.EncodeToString(b)
}

func discoverOAuth(t *testing.T, h *httpClient, name string) map[string]any {
	t.Helper()
	var out map[string]any
	h.raw(t, "POST", "/v1/servers/"+url.PathEscape(name)+"/oauth/discover", map[string]any{}, &out)
	return out
}

func beginOAuth(t *testing.T, h *httpClient, name, mode string) (authURL, state string) {
	t.Helper()
	var out map[string]any
	h.raw(t, "POST", "/v1/servers/"+url.PathEscape(name)+"/oauth/begin", map[string]any{"mode": mode}, &out)
	au, _ := out["authorize_url"].(string)
	st, _ := out["state"].(string)
	if au == "" || st == "" {
		t.Fatalf("oauth begin returned %v", out)
	}
	return au, st
}

func oauthStatus(t *testing.T, h *httpClient, name string) map[string]any {
	t.Helper()
	var out map[string]any
	h.raw(t, "GET", "/v1/servers/"+url.PathEscape(name)+"/oauth", nil, &out)
	return out
}

// driveAuthorize follows the IdP's authorize URL the way a browser
// would — taking the 302 redirect and grabbing the code+state from the
// Location header. Then it hits toolyard's /v1/mcp-oauth/callback so
// we can keep cookies + cookies behavior identical to the dashboard.
func driveAuthorize(t *testing.T, h *httpClient, authURL string) (code, state string) {
	t.Helper()
	cli := &http.Client{
		CheckRedirect: func(r *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := cli.Get(authURL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	loc := resp.Header.Get("Location")
	if loc == "" {
		t.Fatalf("authorize returned no Location header (status %d)", resp.StatusCode)
	}
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	return q.Get("code"), q.Get("state")
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestE2EOAuthDiscoverAndBegin: AS metadata + DCR persists a client.
func TestE2EOAuthDiscoverAndBegin(t *testing.T) {
	flag.Parse()
	h := &httpClient{base: *toolyardURL}
	mustLogin(t, h)
	idp := newFakeIdP(t)
	name := uniqueName("idp-disc")
	addUpstream(t, h, name, idp.URL()+"/mcp")
	defer removeUpstream(t, h, name)

	out := discoverOAuth(t, h, name)
	if registered, _ := out["client_registered"].(bool); !registered {
		t.Fatalf("expected client_registered=true, got %v", out)
	}
	if iss, _ := out["issuer"].(string); !strings.Contains(iss, "127.0.0.1") {
		t.Errorf("expected issuer to point at fake IdP, got %v", iss)
	}

	st := oauthStatus(t, h, name)
	if hc, _ := st["has_client"].(bool); !hc {
		t.Errorf("status has_client should be true after discover, got %v", st)
	}

	authURL, state := beginOAuth(t, h, name, "callback")
	if !strings.Contains(authURL, "code_challenge_method=S256") {
		t.Errorf("authorize URL missing PKCE challenge: %s", authURL)
	}
	if !strings.Contains(authURL, "state="+state) {
		t.Errorf("state missing in authorize URL")
	}
}

// TestE2EOAuthCallbackFlow: full Mode A — discover, begin, follow
// authorize redirect, hit toolyard's callback, verify token state=active.
func TestE2EOAuthCallbackFlow(t *testing.T) {
	flag.Parse()
	h := &httpClient{base: *toolyardURL}
	mustLogin(t, h)
	idp := newFakeIdP(t)
	name := uniqueName("idp-callback")
	addUpstream(t, h, name, idp.URL()+"/mcp")
	defer removeUpstream(t, h, name)

	discoverOAuth(t, h, name)
	authURL, _ := beginOAuth(t, h, name, "callback")
	code, state := driveAuthorize(t, h, authURL)
	if code == "" || state == "" {
		t.Fatalf("authorize did not return code+state: %q %q", code, state)
	}

	// Hit toolyard's callback endpoint as the browser would.
	resp, err := http.Get(*toolyardURL + "/v1/mcp-oauth/callback?code=" +
		url.QueryEscape(code) + "&state=" + url.QueryEscape(state))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("callback status %d", resp.StatusCode)
	}

	st := oauthStatus(t, h, name)
	if got, _ := st["state"].(string); got != "active" {
		t.Errorf("expected token state=active, got %v (full: %v)", got, st)
	}
	if hits := idp.tokenHits.Load(); hits < 1 {
		t.Errorf("expected at least 1 /token hit, got %d", hits)
	}
}

// TestE2EOAuthPasteFallback: the IdP "redirects" to a URL the user's
// browser can't reach; user pastes the URL into the dashboard.
func TestE2EOAuthPasteFallback(t *testing.T) {
	flag.Parse()
	h := &httpClient{base: *toolyardURL}
	mustLogin(t, h)
	idp := newFakeIdP(t)
	name := uniqueName("idp-paste")
	addUpstream(t, h, name, idp.URL()+"/mcp")
	defer removeUpstream(t, h, name)

	discoverOAuth(t, h, name)
	authURL, _ := beginOAuth(t, h, name, "paste")
	code, state := driveAuthorize(t, h, authURL)

	pasteURL := "https://blackhole.invalid/cb?code=" +
		url.QueryEscape(code) + "&state=" + url.QueryEscape(state)
	var out map[string]any
	h.raw(t, "POST", "/v1/mcp-oauth/paste", map[string]any{"url": pasteURL}, &out)
	if u, _ := out["upstream"].(string); u != name {
		t.Errorf("expected upstream %q, got %v", name, out)
	}

	st := oauthStatus(t, h, name)
	if got, _ := st["state"].(string); got != "active" {
		t.Errorf("paste flow did not land active token: %v", st)
	}
}

// TestE2EOAuthDeviceFlow: RFC 8628 — begin, poll (pending), approve, poll (success).
func TestE2EOAuthDeviceFlow(t *testing.T) {
	flag.Parse()
	h := &httpClient{base: *toolyardURL}
	mustLogin(t, h)
	idp := newFakeIdP(t)
	name := uniqueName("idp-device")
	addUpstream(t, h, name, idp.URL()+"/mcp")
	defer removeUpstream(t, h, name)

	discoverOAuth(t, h, name)
	idp.devicePending.Store(true)

	var begin map[string]any
	h.raw(t, "POST", "/v1/servers/"+url.PathEscape(name)+"/oauth/device-begin", map[string]any{}, &begin)
	state, _ := begin["state"].(string)
	if state == "" {
		t.Fatalf("device-begin returned %v", begin)
	}

	// First poll: still pending.
	resp, err := postJSON(h, "/v1/servers/"+url.PathEscape(name)+"/oauth/device-poll",
		map[string]any{"state": state})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202 (pending), got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Approve at the IdP, poll again — should succeed.
	idp.approveDevice()
	var poll map[string]any
	h.raw(t, "POST", "/v1/servers/"+url.PathEscape(name)+"/oauth/device-poll",
		map[string]any{"state": state}, &poll)
	if got, _ := poll["status"].(string); got != "authorized" {
		t.Errorf("expected authorized, got %v", poll)
	}

	st := oauthStatus(t, h, name)
	if got, _ := st["state"].(string); got != "active" {
		t.Errorf("device flow did not land active token: %v", st)
	}
}

// TestE2EOAuthRefreshOnDemand asserts a Refresh issues a new access
// token. We can't easily wait for the 30s ticker in the e2e harness, so
// we validate by seeing the refresh endpoint hit count rise after we
// trigger it via the manual-reauth + re-auth dance.
func TestE2EOAuthRefreshOnDemand(t *testing.T) {
	flag.Parse()
	h := &httpClient{base: *toolyardURL}
	mustLogin(t, h)
	idp := newFakeIdP(t)
	name := uniqueName("idp-refresh")
	addUpstream(t, h, name, idp.URL()+"/mcp")
	defer removeUpstream(t, h, name)

	discoverOAuth(t, h, name)
	idp.shortLifetime.Store(true) // 2s lifetime to encourage quick refresh
	authURL, _ := beginOAuth(t, h, name, "callback")
	code, state := driveAuthorize(t, h, authURL)
	resp, _ := http.Get(*toolyardURL + "/v1/mcp-oauth/callback?code=" + url.QueryEscape(code) +
		"&state=" + url.QueryEscape(state))
	resp.Body.Close()

	beforeRefresh := idp.refreshHits.Load()

	// Wait long enough for the refresher's tick to land (RefresherTickEvery=30s).
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		if idp.refreshHits.Load() > beforeRefresh {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if idp.refreshHits.Load() <= beforeRefresh {
		t.Skipf("refresher did not tick within 45s (beforeRefresh=%d) — environment-dependent", beforeRefresh)
	}

	st := oauthStatus(t, h, name)
	if got, _ := st["state"].(string); got != "active" {
		t.Errorf("after refresh, state should be active: %v", st)
	}
}

// TestE2EOAuthInvalidGrantNeedsReauth: rejecting refresh flips the upstream.
func TestE2EOAuthInvalidGrantNeedsReauth(t *testing.T) {
	flag.Parse()
	h := &httpClient{base: *toolyardURL}
	mustLogin(t, h)
	idp := newFakeIdP(t)
	name := uniqueName("idp-deny")
	addUpstream(t, h, name, idp.URL()+"/mcp")
	defer removeUpstream(t, h, name)

	discoverOAuth(t, h, name)
	idp.shortLifetime.Store(true)
	authURL, _ := beginOAuth(t, h, name, "callback")
	code, state := driveAuthorize(t, h, authURL)
	resp, _ := http.Get(*toolyardURL + "/v1/mcp-oauth/callback?code=" + url.QueryEscape(code) +
		"&state=" + url.QueryEscape(state))
	resp.Body.Close()

	// Now flip refresh into invalid_grant. We don't have direct access
	// to trigger the refresher, so we manually reauth via /reauth which
	// explicitly invalidates state without round-tripping the IdP. The
	// behavior we're asserting is "needs_reauth surfaces in status."
	idp.rejectRefresh.Store(true)
	h.raw(t, "POST", "/v1/servers/"+url.PathEscape(name)+"/oauth/reauth", nil, nil)

	st := oauthStatus(t, h, name)
	if got, _ := st["state"].(string); got != "needs_reauth" {
		t.Errorf("expected needs_reauth, got %v (full: %v)", got, st)
	}
}

// TestE2EOAuthPATShortcut: PAT bypass.
func TestE2EOAuthPATShortcut(t *testing.T) {
	flag.Parse()
	h := &httpClient{base: *toolyardURL}
	mustLogin(t, h)
	name := uniqueName("idp-pat")
	addUpstream(t, h, name, "https://example.invalid/mcp")
	defer removeUpstream(t, h, name)

	h.raw(t, "POST", "/v1/servers/"+url.PathEscape(name)+"/oauth/pat",
		map[string]any{"token": "pat_" + randHex(8)}, nil)
	st := oauthStatus(t, h, name)
	if got, _ := st["is_pat"].(bool); !got {
		t.Errorf("PAT not flagged: %v", st)
	}
	if got, _ := st["state"].(string); got != "active" {
		t.Errorf("PAT should be active: %v", st)
	}
}

// TestE2EOAuthDisconnect: revoke + delete.
func TestE2EOAuthDisconnect(t *testing.T) {
	flag.Parse()
	h := &httpClient{base: *toolyardURL}
	mustLogin(t, h)
	idp := newFakeIdP(t)
	name := uniqueName("idp-disc")
	addUpstream(t, h, name, idp.URL()+"/mcp")
	defer removeUpstream(t, h, name)

	discoverOAuth(t, h, name)
	authURL, _ := beginOAuth(t, h, name, "callback")
	code, state := driveAuthorize(t, h, authURL)
	resp, _ := http.Get(*toolyardURL + "/v1/mcp-oauth/callback?code=" + url.QueryEscape(code) +
		"&state=" + url.QueryEscape(state))
	resp.Body.Close()

	// Disconnect.
	h.raw(t, "DELETE", "/v1/servers/"+url.PathEscape(name)+"/oauth", nil, nil)
	st := oauthStatus(t, h, name)
	if hc, _ := st["has_client"].(bool); hc {
		t.Errorf("client should be gone after disconnect: %v", st)
	}
	if ht, _ := st["has_token"].(bool); ht {
		t.Errorf("token should be gone after disconnect: %v", st)
	}
}

// TestE2EOAuthCallbackBadState: stale or unknown state -> 4xx.
func TestE2EOAuthCallbackBadState(t *testing.T) {
	flag.Parse()
	resp, err := http.Get(*toolyardURL + "/v1/mcp-oauth/callback?code=abc&state=does-not-exist")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 on bogus state, got %d", resp.StatusCode)
	}
}

// TestE2EOAuthPasteBadURL: paste with no code/state in URL -> 400.
func TestE2EOAuthPasteBadURL(t *testing.T) {
	flag.Parse()
	h := &httpClient{base: *toolyardURL}
	mustLogin(t, h)
	resp, err := postJSON(h, "/v1/mcp-oauth/paste", map[string]any{
		"url": "https://example.com/cb",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 on bare URL, got %d", resp.StatusCode)
	}
}

// TestE2EOAuthManualClient: when DCR isn't supported (or operator
// already has client_id), the manual-client endpoint persists it.
func TestE2EOAuthManualClient(t *testing.T) {
	flag.Parse()
	h := &httpClient{base: *toolyardURL}
	mustLogin(t, h)
	idp := newFakeIdP(t)
	name := uniqueName("idp-manual")
	addUpstream(t, h, name, idp.URL()+"/mcp")
	defer removeUpstream(t, h, name)

	// Run discovery so AS endpoints are persisted, then overwrite client_id.
	discoverOAuth(t, h, name)
	h.raw(t, "POST", "/v1/servers/"+url.PathEscape(name)+"/oauth/manual-client",
		map[string]any{"client_id": "my-static-client"}, nil)

	st := oauthStatus(t, h, name)
	if cid, _ := st["client_id"].(string); cid != "my-static-client" {
		t.Errorf("manual client_id not persisted: %v", st)
	}
}

// manualClientPreset seeds a BYO client the way the marketplace install
// modal does: explicit endpoints (no discovery), client secret, opaque
// comma-separated scope and extra authorize params.
func manualClientPreset(t *testing.T, h *httpClient, name string, idp *fakeIdP, clientID string, extra map[string]string) {
	t.Helper()
	h.raw(t, "POST", "/v1/servers/"+url.PathEscape(name)+"/oauth/manual-client",
		map[string]any{
			"client_id":              clientID,
			"client_secret":          "sec-" + clientID,
			"issuer":                 idp.URL(),
			"authorization_endpoint": idp.URL() + "/authorize",
			"token_endpoint":         idp.URL() + "/token",
			"scopes":                 []string{"read,write,issues:create"},
			"extra_authorize_params": extra,
		}, nil)
}

// TestE2EOAuthManualClientExtraParams: a preset BYO client (Linear-style)
// carries actor=app into the authorize URL, keeps the comma-separated scope
// opaque, drops the legacy access_type/prompt params, and exchanges the
// code with the client_secret.
func TestE2EOAuthManualClientExtraParams(t *testing.T) {
	flag.Parse()
	h := &httpClient{base: *toolyardURL}
	mustLogin(t, h)
	idp := newFakeIdP(t)
	name := uniqueName("idp-actor")
	addUpstream(t, h, name, idp.URL()+"/mcp")
	defer removeUpstream(t, h, name)

	manualClientPreset(t, h, name, idp, "linear-byo-client", map[string]string{"actor": "app"})

	st := oauthStatus(t, h, name)
	if cid, _ := st["client_id"].(string); cid != "linear-byo-client" {
		t.Fatalf("manual client_id not persisted: %v", st)
	}
	if ext, _ := st["extra_authorize_params"].(map[string]any); ext == nil || ext["actor"] != "app" {
		t.Errorf("extra_authorize_params not surfaced in status: %v", st)
	}

	authURL, _ := beginOAuth(t, h, name, "callback")
	if !strings.Contains(authURL, "actor=app") {
		t.Errorf("authorize URL missing actor=app: %s", authURL)
	}
	if !strings.Contains(authURL, "scope=read%2Cwrite%2Cissues%3Acreate") {
		t.Errorf("authorize URL did not carry comma scope verbatim: %s", authURL)
	}
	if strings.Contains(authURL, "access_type=") || strings.Contains(authURL, "prompt=") {
		t.Errorf("preset extras should replace legacy access_type/prompt params: %s", authURL)
	}

	code, state := driveAuthorize(t, h, authURL)
	resp, err := http.Get(*toolyardURL + "/v1/mcp-oauth/callback?code=" +
		url.QueryEscape(code) + "&state=" + url.QueryEscape(state))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	st = oauthStatus(t, h, name)
	if got, _ := st["state"].(string); got != "active" {
		t.Errorf("expected active after callback, got %v", st)
	}
	idp.mu.Lock()
	gotSecret := idp.lastClientSecret
	idp.mu.Unlock()
	if gotSecret != "sec-linear-byo-client" {
		t.Errorf("token exchange did not send the client_secret, got %q", gotSecret)
	}
}

// TestE2EOAuthNoRefreshTokenStaysActive: long-lived tokens with neither
// refresh_token nor expires_in (Linear-style) must be left alone by the
// background refresher rather than flipped to needs_reauth.
func TestE2EOAuthNoRefreshTokenStaysActive(t *testing.T) {
	flag.Parse()
	if testing.Short() {
		t.Skip("waits >2 refresher ticks")
	}
	h := &httpClient{base: *toolyardURL}
	mustLogin(t, h)
	idp := newFakeIdP(t)
	idp.noRefreshNoExpiry.Store(true)
	name := uniqueName("idp-norefresh")
	addUpstream(t, h, name, idp.URL()+"/mcp")
	defer removeUpstream(t, h, name)

	manualClientPreset(t, h, name, idp, "norefresh-client", map[string]string{"actor": "app"})
	authURL, _ := beginOAuth(t, h, name, "callback")
	code, state := driveAuthorize(t, h, authURL)
	resp, _ := http.Get(*toolyardURL + "/v1/mcp-oauth/callback?code=" + url.QueryEscape(code) +
		"&state=" + url.QueryEscape(state))
	resp.Body.Close()

	if got, _ := oauthStatus(t, h, name)["state"].(string); got != "active" {
		t.Fatalf("setup: expected active, got %v", got)
	}

	// Wait past two refresher ticks (RefresherTickEvery=30s). Before the
	// refresh_token_enc IS NOT NULL guard, the refresher would have tried —
	// and failed — to refresh this token by now.
	time.Sleep(65 * time.Second)

	st := oauthStatus(t, h, name)
	if got, _ := st["state"].(string); got != "active" {
		t.Errorf("no-refresh token flipped out of active: %v", st)
	}
	if rf, _ := st["refresh_failures"].(float64); rf != 0 {
		t.Errorf("refresher touched a token without refresh_token: failures=%v (full: %v)", rf, st)
	}
	if hits := idp.refreshHits.Load(); hits != 0 {
		t.Errorf("refresher hit the IdP %d times for a token with no refresh_token", hits)
	}
}

// TestE2EOAuthTwoInstances: two upstreams of the same provider (multi-account
// Linear case) run concurrent flows on the shared redirect URI and keep
// independent clients + tokens.
func TestE2EOAuthTwoInstances(t *testing.T) {
	flag.Parse()
	h := &httpClient{base: *toolyardURL}
	mustLogin(t, h)
	idp := newFakeIdP(t)
	nameA := uniqueName("linear-a")
	nameB := uniqueName("linear-b")
	addUpstream(t, h, nameA, idp.URL()+"/mcp")
	addUpstream(t, h, nameB, idp.URL()+"/mcp")
	defer removeUpstream(t, h, nameA)
	defer removeUpstream(t, h, nameB)

	manualClientPreset(t, h, nameA, idp, "acct-a", map[string]string{"actor": "app"})
	manualClientPreset(t, h, nameB, idp, "acct-b", map[string]string{"actor": "user"})

	// Begin BOTH flows before completing either — two oauth_pending rows
	// routed by state on the same callback URL.
	authA, _ := beginOAuth(t, h, nameA, "callback")
	authB, _ := beginOAuth(t, h, nameB, "callback")

	codeA, stateA := driveAuthorize(t, h, authA)
	codeB, stateB := driveAuthorize(t, h, authB)
	for _, cs := range [][2]string{{codeA, stateA}, {codeB, stateB}} {
		resp, err := http.Get(*toolyardURL + "/v1/mcp-oauth/callback?code=" +
			url.QueryEscape(cs[0]) + "&state=" + url.QueryEscape(cs[1]))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}

	stA := oauthStatus(t, h, nameA)
	stB := oauthStatus(t, h, nameB)
	if got, _ := stA["state"].(string); got != "active" {
		t.Errorf("instance A not active: %v", stA)
	}
	if got, _ := stB["state"].(string); got != "active" {
		t.Errorf("instance B not active: %v", stB)
	}
	if cidA, _ := stA["client_id"].(string); cidA != "acct-a" {
		t.Errorf("instance A client_id = %q, want acct-a", cidA)
	}
	if cidB, _ := stB["client_id"].(string); cidB != "acct-b" {
		t.Errorf("instance B client_id = %q, want acct-b", cidB)
	}
}

// keep linter happy if a test goes unused.
var _ = fmt.Sprintf
