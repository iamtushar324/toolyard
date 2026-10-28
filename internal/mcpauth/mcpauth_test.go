package mcpauth

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/access"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/store"
	"github.com/tusharbhardwaj/toolyard/internal/store/storetest"
)

const testPublic = "https://toolyard.example"

type stubSettings map[string]any

func (s stubSettings) GetBoolDefault(k string, d bool) bool {
	if v, ok := s[k].(bool); ok {
		return v
	}
	return d
}
func (s stubSettings) GetString(k, d string) string {
	if v, ok := s[k].(string); ok {
		return v
	}
	return d
}
func (s stubSettings) GetInt(k string, d int) int {
	if v, ok := s[k].(int); ok {
		return v
	}
	return d
}

type fixture struct {
	db       *store.DB
	svc      *Service
	user     *identity.User
	settings stubSettings
	clock    time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db := storetest.Open(t)
	u, err := identity.New(db).CreateUser(context.Background(), "owner", "long-enough-password")
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{db: db, user: u, settings: stubSettings{}, clock: time.Now()}
	f.svc = New(Options{DB: db, PublicURL: testPublic + "/", Settings: f.settings, Access: access.New(db),
		Now: func() time.Time { return f.clock }})
	return f
}

func pkcePair(t *testing.T) (verifier, challenge string) {
	t.Helper()
	b := make([]byte, 40)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	verifier = base64.RawURLEncoding.EncodeToString(b)
	return verifier, pkceS256(verifier)
}

func (f *fixture) register(t *testing.T, method string, redirects ...string) *RegistrationResponse {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"redirect_uris": redirects, "token_endpoint_auth_method": method, "client_name": "Test Client"})
	resp, oerr := f.svc.Register(context.Background(), body, "198.51.100.7")
	if oerr != nil {
		t.Fatalf("register: %v", oerr)
	}
	return resp
}

func authorizeQuery(clientID, redirect, challenge string, extra map[string]string) url.Values {
	q := url.Values{
		"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {redirect}, "state": {"st-123"},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"}, "resource": {testPublic + "/mcp"}, "scope": {"mcp offline_access"},
	}
	for k, v := range extra {
		if v == "" {
			q.Del(k)
		} else {
			q.Set(k, v)
		}
	}
	return q
}

// approve runs authorize, consent and the return link, and returns the
// code.
func (f *fixture) approve(t *testing.T, u *identity.User, clientID, redirect, challenge string, c Consent) string {
	t.Helper()
	req, aerr := f.svc.BeginAuthorize(context.Background(), authorizeQuery(clientID, redirect, challenge, nil), "198.51.100.7")
	if aerr != nil {
		t.Fatalf("authorize: %v", aerr)
	}
	loc := f.approveAndReturn(t, req.ID, u, c)
	parsed, err := url.Parse(loc)
	if err != nil {
		t.Fatal(err)
	}
	q := parsed.Query()
	if q.Get("state") != "st-123" || q.Get("iss") != testPublic || q.Get("code") == "" {
		t.Fatalf("approve redirect %q lacks code, state or iss", loc)
	}
	return q.Get("code")
}

// approveAndReturn answers request id with yes and uses the return link it
// gives, and returns the redirect that carries the code.
func (f *fixture) approveAndReturn(t *testing.T, id string, u *identity.User, c Consent) string {
	t.Helper()
	ticket, err := f.svc.Approve(context.Background(), id, u, c)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	loc, err := f.svc.RedeemReturn(context.Background(), ticket)
	if err != nil {
		t.Fatalf("return link: %v", err)
	}
	return loc
}

func codeForm(code, redirect, verifier string) url.Values {
	return url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirect}, "code_verifier": {verifier}, "resource": {testPublic + "/mcp"}}
}

func refreshForm(rt string) url.Values {
	return url.Values{"grant_type": {"refresh_token"}, "refresh_token": {rt}}
}

func wantOAuthErr(t *testing.T, oerr *Error, code string) {
	t.Helper()
	if oerr == nil || oerr.Code != code {
		t.Fatalf("got error %v, want %s", oerr, code)
	}
}

func TestMetadataDocuments(t *testing.T) {
	f := newFixture(t)
	pr := f.svc.ProtectedResourceMetadata()
	if pr["resource"] != testPublic+"/mcp" || pr["authorization_servers"].([]string)[0] != testPublic {
		t.Fatalf("protected resource metadata: %v", pr)
	}
	as := f.svc.AuthorizationServerMetadata()
	for k, want := range map[string]any{
		"issuer": testPublic, "authorization_endpoint": testPublic + "/oauth/authorize", "token_endpoint": testPublic + "/oauth/token",
		"registration_endpoint": testPublic + "/oauth/register", "revocation_endpoint": testPublic + "/oauth/revoke",
		"client_id_metadata_document_supported": true,
	} {
		if as[k] != want {
			t.Errorf("%s = %v, want %v", k, as[k], want)
		}
	}
	if m := as["code_challenge_methods_supported"].([]string); len(m) != 1 || m[0] != "S256" {
		t.Errorf("code_challenge_methods_supported = %v", m)
	}
	if f.svc.ResourceMetadataURL() != testPublic+"/.well-known/oauth-protected-resource" {
		t.Errorf("ResourceMetadataURL = %q", f.svc.ResourceMetadataURL())
	}
}

func TestEnabledNeedsPublicURLAndTheSwitch(t *testing.T) {
	if New(Options{}).Enabled() {
		t.Fatal("enabled without a public URL")
	}
	f := newFixture(t)
	if !f.svc.Enabled() {
		t.Fatal("off by default with a public URL")
	}
	f.settings[SettingEnabled] = false
	if f.svc.Enabled() || f.svc.ResourceMetadataURL() != "" {
		t.Fatal("the instance switch did not turn it off")
	}
	if _, err := f.svc.VerifyAccessToken(context.Background(), "tyat_x"); err != identity.ErrAgentTokenInvalid {
		t.Fatalf("a token checked while off: %v", err)
	}
}

func TestLifetimeSettingsAreClamped(t *testing.T) {
	f := newFixture(t)
	if f.svc.AccessTTL() != time.Hour || f.svc.RefreshTTL() != 30*24*time.Hour {
		t.Fatalf("defaults: %v %v", f.svc.AccessTTL(), f.svc.RefreshTTL())
	}
	f.settings[SettingAccessTTLMinutes], f.settings[SettingRefreshTTLDays] = 1, 9999
	if f.svc.AccessTTL() != 5*time.Minute || f.svc.RefreshTTL() != 365*24*time.Hour {
		t.Fatalf("clamped: %v %v", f.svc.AccessTTL(), f.svc.RefreshTTL())
	}
}

func TestRegisterValidatesClients(t *testing.T) {
	f := newFixture(t)
	for _, bad := range []string{
		"http://evil.example/cb", "javascript:alert(1)", "https://x.example/cb#frag", "https://user@x.example/cb",
		"data:text/html,hi", "file:///etc/passwd", "/relative", "",
		"https://*.example.com/cb", "https://exa_mple.com/cb", "https://-x.example/cb", "https:///cb",
		"ms-msdt:/id", "search-ms:query=x", "ms-officecmd:x", "jar:https://x.example/a!/b",
		"myapp:/cb", "vscode-foo://x/cb", "com.example.app://*/cb",
	} {
		body, _ := json.Marshal(map[string]any{"redirect_uris": []string{bad}, "token_endpoint_auth_method": "none"})
		if _, oerr := f.svc.Register(context.Background(), body, "198.51.100.7"); oerr == nil || oerr.Code != "invalid_redirect_uri" {
			t.Errorf("redirect %q: got %v, want invalid_redirect_uri", bad, oerr)
		}
	}
	for _, good := range []string{"https://claude.ai/api/mcp/auth_callback", "http://127.0.0.1:33418/callback", "http://localhost:9999/cb",
		"http://[::1]:8080/cb", "cursor://anysphere.cursor-mcp/oauth/callback", "vscode://vscode.github-authentication/did-authenticate",
		"com.example.app:/oauth/cb", "https://192.0.2.10/cb"} {
		f.register(t, "none", good)
	}
	// The exception list is the admin's: without cursor on it, cursor://
	// is not a reverse-DNS scheme and is refused; with myapp it is allowed.
	f.settings[SettingCustomSchemes] = "myapp"
	body, _ := json.Marshal(map[string]any{"redirect_uris": []string{"cursor://anysphere.cursor-mcp/oauth/callback"}, "token_endpoint_auth_method": "none"})
	if _, oerr := f.svc.Register(context.Background(), body, "198.51.100.7"); oerr == nil {
		t.Error("cursor:// allowed though the admin's exception list leaves it out")
	}
	f.register(t, "none", "myapp:/cb")
	delete(f.settings, SettingCustomSchemes)
	body, _ = json.Marshal(map[string]any{"redirect_uris": []string{"https://a.example/cb"}, "token_endpoint_auth_method": "private_key_jwt"})
	if _, oerr := f.svc.Register(context.Background(), body, "198.51.100.7"); oerr == nil || oerr.Code != "invalid_client_metadata" {
		t.Errorf("private_key_jwt: %v", oerr)
	}
	body, _ = json.Marshal(map[string]any{"redirect_uris": []string{"https://a.example/cb"}, "grant_types": []string{"password"}})
	if _, oerr := f.svc.Register(context.Background(), body, "198.51.100.7"); oerr == nil || oerr.Code != "invalid_client_metadata" {
		t.Errorf("password grant: %v", oerr)
	}
	pub := f.register(t, "none", "https://a.example/cb")
	if pub.ClientSecret != "" || pub.ClientSecretExpiresAt != nil || !strings.HasPrefix(pub.ClientID, clientPrefix) {
		t.Fatalf("public client: %+v", pub)
	}
	conf := f.register(t, "", "https://a.example/cb")
	if conf.TokenEndpointAuthMethod != AuthSecretBasic || conf.ClientSecret == "" || conf.ClientSecretExpiresAt == nil {
		t.Fatalf("omitted method must default to client_secret_basic with a secret: %+v", conf)
	}
	var stored string
	if err := f.db.QueryRow(`SELECT COALESCE(secret_hash,'') FROM mcp_oauth_clients WHERE client_id = ?`, conf.ClientID).Scan(&stored); err != nil || stored == "" || strings.Contains(stored, conf.ClientSecret) {
		t.Fatalf("the secret must be stored hashed: %q %v", stored, err)
	}
}

func TestRedirectHostAllowList(t *testing.T) {
	f := newFixture(t)
	f.settings[SettingRedirectHosts] = "claude.ai, *.example.com ; cursor:"
	f.register(t, "none", "https://claude.ai/cb")
	f.register(t, "none", "https://app.example.com/cb")
	f.register(t, "none", "cursor://anysphere.cursor-mcp/cb")
	for _, bad := range []string{"https://evil.example/cb", "https://example.com/cb", "vscode://x/cb"} {
		body, _ := json.Marshal(map[string]any{"redirect_uris": []string{bad}, "token_endpoint_auth_method": "none"})
		if _, oerr := f.svc.Register(context.Background(), body, "198.51.100.7"); oerr == nil {
			t.Errorf("%s registered past the allow list", bad)
		}
	}
}

func TestAuthorizeChecks(t *testing.T) {
	f := newFixture(t)
	redirect := "http://127.0.0.1:41111/callback"
	c := f.register(t, "none", redirect)
	_, challenge := pkcePair(t)
	ctx := context.Background()
	// Errors the person sees: the client or its redirect can't be trusted.
	for name, q := range map[string]url.Values{
		"unknown client":      authorizeQuery("mcpc_nope", redirect, challenge, nil),
		"unregistered target": authorizeQuery(c.ClientID, "https://evil.example/cb", challenge, nil),
		"missing redirect":    authorizeQuery(c.ClientID, redirect, challenge, map[string]string{"redirect_uri": ""}),
	} {
		_, aerr := f.svc.BeginAuthorize(ctx, q, "198.51.100.7")
		if aerr == nil || aerr.Location() != "" {
			t.Errorf("%s: got %v, want an error shown to the person", name, aerr)
		}
	}
	// A response type other than code is shown, not sent back: until the
	// response type is valid the request is not trusted with a redirect.
	if _, aerr := f.svc.BeginAuthorize(ctx, authorizeQuery(c.ClientID, redirect, challenge, map[string]string{"response_type": "token"}), "198.51.100.7"); aerr == nil ||
		aerr.Code != "unsupported_response_type" || aerr.Location() != "" {
		t.Errorf("token response: got %v, want an error shown to the person", aerr)
	}
	// Request errors: shown to the person while the redirect URI is not
	// trusted (a self-registered client's never is), sent back to the
	// client with state and iss once the admin's allow list names it.
	cases := map[string]struct {
		extra map[string]string
		code  string
	}{
		"missing challenge": {map[string]string{"code_challenge": ""}, "invalid_request"},
		"plain method":      {map[string]string{"code_challenge_method": "plain"}, "invalid_request"},
		"short challenge":   {map[string]string{"code_challenge": "abc"}, "invalid_request"},
		"wrong resource":    {map[string]string{"resource": "https://other.example/mcp"}, "invalid_target"},
	}
	for name, tc := range cases {
		_, aerr := f.svc.BeginAuthorize(ctx, authorizeQuery(c.ClientID, redirect, challenge, tc.extra), "198.51.100.7")
		if aerr == nil || aerr.Code != tc.code || aerr.Location() != "" {
			t.Errorf("%s, untrusted redirect: got %v, want %s shown to the person", name, aerr, tc.code)
		}
	}
	f.settings[SettingRedirectHosts] = "127.0.0.1"
	for name, tc := range cases {
		_, aerr := f.svc.BeginAuthorize(ctx, authorizeQuery(c.ClientID, redirect, challenge, tc.extra), "198.51.100.7")
		if aerr == nil || aerr.Code != tc.code {
			t.Errorf("%s: got %v, want %s", name, aerr, tc.code)
			continue
		}
		loc, _ := url.Parse(aerr.Location())
		if loc == nil || !strings.HasPrefix(aerr.Location(), redirect) || loc.Query().Get("error") != tc.code || loc.Query().Get("iss") != testPublic {
			t.Errorf("%s, trusted redirect: redirect %q", name, aerr.Location())
		}
	}
	delete(f.settings, SettingRedirectHosts)
	// A loopback redirect may change its port (RFC 8252 section 7.3), and
	// the resource may carry a trailing slash.
	if _, aerr := f.svc.BeginAuthorize(ctx, authorizeQuery(c.ClientID, "http://127.0.0.1:52222/callback", challenge,
		map[string]string{"resource": testPublic + "/mcp/"}), "198.51.100.7"); aerr != nil {
		t.Fatalf("loopback port change: %v", aerr)
	}
	if _, aerr := f.svc.BeginAuthorize(ctx, authorizeQuery(c.ClientID, "http://127.0.0.1:52222/other", challenge, nil), "198.51.100.7"); aerr == nil {
		t.Fatal("a loopback redirect with another path was accepted")
	}
	// state is optional (PKCE protects the code), and a missing resource
	// means toolyard's only resource.
	req, aerr := f.svc.BeginAuthorize(ctx, authorizeQuery(c.ClientID, redirect, challenge, map[string]string{"state": "", "resource": ""}), "198.51.100.7")
	if aerr != nil || req.State != "" || req.Resource != testPublic+"/mcp" {
		t.Fatalf("no state, no resource: %+v %v", req, aerr)
	}
	if loc := f.approveAndReturn(t, req.ID, f.user, Consent{SameBrowser: true, Nickname: "No state"}); strings.Contains(loc, "state=") {
		t.Fatalf("approve without state: %q", loc)
	}
}

// TestNoOpenRedirectBeforeTheRequestIsTrusted: a client that registered
// someone else's site as its redirect URI cannot use an invalid request to
// bounce a browser there without the person (RFC 9700 section 4.11.2).
func TestNoOpenRedirectBeforeTheRequestIsTrusted(t *testing.T) {
	f := newFixture(t)
	evil := f.register(t, "none", "https://evil.example/landing")
	_, challenge := pkcePair(t)
	for name, extra := range map[string]map[string]string{
		"bad response_type":     {"response_type": "token"},
		"missing response_type": {"response_type": ""},
		"missing challenge":     {"code_challenge": ""},
		"plain challenge":       {"code_challenge_method": "plain"},
		"wrong resource":        {"resource": "https://other.example/mcp"},
	} {
		_, aerr := f.svc.BeginAuthorize(context.Background(), authorizeQuery(evil.ClientID, "https://evil.example/landing", challenge, extra), "198.51.100.7")
		if aerr == nil || aerr.Location() != "" {
			t.Errorf("%s: got %v, want an error page and no redirect", name, aerr)
		}
	}
	// A metadata document does not make its own host trusted either:
	// anyone can host a document that lists their own site (RFC 9700
	// section 4.11.2, attack 1). Only the admin's allow list does.
	srv, _ := cimdServer(t, func(u string) any {
		return map[string]any{"client_id": u, "redirect_uris": []string{"https://127.0.0.1/cb"}}
	}, http.StatusOK)
	svc := cimdService(f, srv, true)
	id := srv.URL + "/own-host.json"
	_, aerr := svc.BeginAuthorize(context.Background(), authorizeQuery(id, "https://127.0.0.1/cb", challenge, map[string]string{"code_challenge": ""}), "198.51.100.7")
	if aerr == nil || aerr.Location() != "" {
		t.Errorf("an error for a document client's own host: %v, want an error page", aerr)
	}
	f.settings[SettingRedirectHosts] = "127.0.0.1"
	svc = New(Options{DB: f.db, PublicURL: testPublic, Settings: f.settings, Now: func() time.Time { return f.clock }, AllowPrivateCIMD: true, CIMDRootCAs: svc.cimdRoots})
	_, aerr = svc.BeginAuthorize(context.Background(), authorizeQuery(id, "https://127.0.0.1/cb", challenge, map[string]string{"code_challenge": ""}), "198.51.100.7")
	if aerr == nil || !strings.HasPrefix(aerr.Location(), "https://127.0.0.1/cb?") {
		t.Errorf("an error for a host on the admin's allow list: %v, want a redirect", aerr)
	}
}

func TestCodeExchangeChecks(t *testing.T) {
	f := newFixture(t)
	redirect := "http://127.0.0.1:41111/callback"
	c := f.register(t, "none", redirect)
	other := f.register(t, "none", redirect)
	ctx := context.Background()
	ca := ClientAuth{ClientID: c.ClientID}
	consent := Consent{SameBrowser: true, Nickname: "Laptop Claude", Scope: access.AgentScope{Mode: access.ScopeModeAll}}

	t.Run("PKCE mismatch uses the code up", func(t *testing.T) {
		verifier, challenge := pkcePair(t)
		code := f.approve(t, f.user, c.ClientID, redirect, challenge, consent)
		wrong, _ := pkcePair(t)
		_, oerr := f.svc.Exchange(ctx, codeForm(code, redirect, wrong), ca)
		wantOAuthErr(t, oerr, "invalid_grant")
		_, oerr = f.svc.Exchange(ctx, codeForm(code, redirect, verifier), ca)
		wantOAuthErr(t, oerr, "invalid_grant")
	})
	t.Run("wrong redirect", func(t *testing.T) {
		verifier, challenge := pkcePair(t)
		code := f.approve(t, f.user, c.ClientID, redirect, challenge, consent)
		_, oerr := f.svc.Exchange(ctx, codeForm(code, "http://127.0.0.1:41111/other", verifier), ca)
		wantOAuthErr(t, oerr, "invalid_grant")
	})
	t.Run("wrong resource", func(t *testing.T) {
		verifier, challenge := pkcePair(t)
		code := f.approve(t, f.user, c.ClientID, redirect, challenge, consent)
		form := codeForm(code, redirect, verifier)
		form.Set("resource", "https://other.example/mcp")
		_, oerr := f.svc.Exchange(ctx, form, ca)
		wantOAuthErr(t, oerr, "invalid_target")
	})
	t.Run("another client", func(t *testing.T) {
		verifier, challenge := pkcePair(t)
		code := f.approve(t, f.user, c.ClientID, redirect, challenge, consent)
		_, oerr := f.svc.Exchange(ctx, codeForm(code, redirect, verifier), ClientAuth{ClientID: other.ClientID})
		wantOAuthErr(t, oerr, "invalid_grant")
	})
	t.Run("expired", func(t *testing.T) {
		verifier, challenge := pkcePair(t)
		code := f.approve(t, f.user, c.ClientID, redirect, challenge, consent)
		f.clock = f.clock.Add(CodeTTL + time.Second)
		defer func() { f.clock = f.clock.Add(-CodeTTL - time.Second) }()
		_, oerr := f.svc.Exchange(ctx, codeForm(code, redirect, verifier), ca)
		wantOAuthErr(t, oerr, "invalid_grant")
	})
	t.Run("missing grant type and unknown client", func(t *testing.T) {
		_, oerr := f.svc.Exchange(ctx, url.Values{}, ca)
		wantOAuthErr(t, oerr, "invalid_request")
		_, oerr = f.svc.Exchange(ctx, url.Values{"grant_type": {"password"}}, ca)
		wantOAuthErr(t, oerr, "unsupported_grant_type")
		_, oerr = f.svc.Exchange(ctx, codeForm("tyoc_x", redirect, "v"), ClientAuth{ClientID: "mcpc_nope"})
		wantOAuthErr(t, oerr, "invalid_client")
	})
	t.Run("reused code revokes its tokens", func(t *testing.T) {
		verifier, challenge := pkcePair(t)
		code := f.approve(t, f.user, c.ClientID, redirect, challenge, consent)
		tok, oerr := f.svc.Exchange(ctx, codeForm(code, redirect, verifier), ca)
		if oerr != nil {
			t.Fatal(oerr)
		}
		if _, err := f.svc.VerifyAccessToken(ctx, tok.AccessToken); err != nil {
			t.Fatalf("fresh token: %v", err)
		}
		_, oerr = f.svc.Exchange(ctx, codeForm(code, redirect, verifier), ca)
		wantOAuthErr(t, oerr, "invalid_grant")
		if _, err := f.svc.VerifyAccessToken(ctx, tok.AccessToken); err != identity.ErrAgentTokenInvalid {
			t.Fatalf("a token from a replayed code still works: %v", err)
		}
		if _, oerr = f.svc.Exchange(ctx, refreshForm(tok.RefreshToken), ca); oerr == nil {
			t.Fatal("a refresh token from a replayed code still works")
		}
	})
}

func TestTokensRefreshRotateAndReuseRevokes(t *testing.T) {
	f := newFixture(t)
	redirect := "https://client.example/cb"
	c := f.register(t, "none", redirect)
	ca := ClientAuth{ClientID: c.ClientID}
	ctx := context.Background()
	verifier, challenge := pkcePair(t)
	code := f.approve(t, f.user, c.ClientID, redirect, challenge, Consent{SameBrowser: true, Nickname: "Web ChatGPT", Scope: access.AgentScope{Mode: access.ScopeModeListed, AllowServers: []string{"memory"}}})
	t1, oerr := f.svc.Exchange(ctx, codeForm(code, redirect, verifier), ca)
	if oerr != nil {
		t.Fatal(oerr)
	}
	if t1.TokenType != "Bearer" || t1.ExpiresIn != 3600 || !strings.HasPrefix(t1.AccessToken, accessPrefix) || !strings.HasPrefix(t1.RefreshToken, refreshPrefix) || t1.Scope != "mcp offline_access" {
		t.Fatalf("token response: %+v", t1)
	}
	ag, err := f.svc.VerifyAccessToken(ctx, t1.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	if ag.Kind != AgentKindOAuthClient || ag.Name != "Web ChatGPT" || ag.Owner != f.user.ID || ag.OwnerUsername != "owner" {
		t.Fatalf("agent: %+v", ag)
	}
	t2, oerr := f.svc.Exchange(ctx, refreshForm(t1.RefreshToken), ca)
	if oerr != nil {
		t.Fatal(oerr)
	}
	if t2.RefreshToken == t1.RefreshToken || t2.AccessToken == t1.AccessToken {
		t.Fatal("refresh did not rotate")
	}
	if _, err := f.svc.VerifyAccessToken(ctx, t2.AccessToken); err != nil {
		t.Fatalf("rotated token: %v", err)
	}
	// The used refresh token again, within the grace and while its
	// successor is unused: refused, nothing revoked (a client retry).
	_, oerr = f.svc.Exchange(ctx, refreshForm(t1.RefreshToken), ca)
	wantOAuthErr(t, oerr, "invalid_grant")
	if _, err := f.svc.VerifyAccessToken(ctx, t2.AccessToken); err != nil {
		t.Fatalf("a retry within the grace revoked the grant: %v", err)
	}
	// After the grace: one of its holders is not the client.
	f.clock = f.clock.Add(refreshReuseGrace + time.Second)
	_, oerr = f.svc.Exchange(ctx, refreshForm(t1.RefreshToken), ca)
	wantOAuthErr(t, oerr, "invalid_grant")
	if _, err := f.svc.VerifyAccessToken(ctx, t2.AccessToken); err != identity.ErrAgentTokenInvalid {
		t.Fatalf("reuse did not revoke the grant: %v", err)
	}
	if _, oerr = f.svc.Exchange(ctx, refreshForm(t2.RefreshToken), ca); oerr == nil {
		t.Fatal("the newest refresh token survived reuse detection")
	}
	apps, err := f.svc.ListApps(ctx, f.user.ID)
	if err != nil || len(apps) != 1 || apps[0].Status != "revoked" || apps[0].RevokedReason != "refresh_token_reused" {
		t.Fatalf("apps after reuse: %+v %v", apps, err)
	}
	var n int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE event_type = 'mcp_oauth.refresh_reused'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("reuse audit rows: %d %v", n, err)
	}
	// Connecting again re-enables the same agent under the new nickname.
	verifier, challenge = pkcePair(t)
	code = f.approve(t, f.user, c.ClientID, redirect, challenge, Consent{SameBrowser: true, Nickname: "ChatGPT again"})
	t3, oerr := f.svc.Exchange(ctx, codeForm(code, redirect, verifier), ca)
	if oerr != nil {
		t.Fatal(oerr)
	}
	ag3, err := f.svc.VerifyAccessToken(ctx, t3.AccessToken)
	if err != nil || ag3.ID != ag.ID || ag3.Name != "ChatGPT again" {
		t.Fatalf("reconnect: %+v %v", ag3, err)
	}
	apps, _ = f.svc.ListApps(ctx, "")
	if len(apps) != 1 || apps[0].Status != "active" || apps[0].Scope.Mode != access.ScopeModeAll {
		t.Fatalf("apps after reconnect: %+v", apps)
	}
}

func TestRefreshScopeMayOnlyNarrow(t *testing.T) {
	f := newFixture(t)
	redirect := "https://client.example/cb"
	c := f.register(t, "none", redirect)
	ca := ClientAuth{ClientID: c.ClientID}
	verifier, challenge := pkcePair(t)
	code := f.approve(t, f.user, c.ClientID, redirect, challenge, Consent{SameBrowser: true, Nickname: "x"})
	tok, oerr := f.svc.Exchange(context.Background(), codeForm(code, redirect, verifier), ca)
	if oerr != nil {
		t.Fatal(oerr)
	}
	form := refreshForm(tok.RefreshToken)
	form.Set("scope", "mcp admin")
	_, oerr = f.svc.Exchange(context.Background(), form, ca)
	wantOAuthErr(t, oerr, "invalid_scope")
}

func TestConfidentialClientAuthentication(t *testing.T) {
	f := newFixture(t)
	redirect := "https://client.example/cb"
	c := f.register(t, AuthSecretPost, redirect)
	ctx := context.Background()
	verifier, challenge := pkcePair(t)
	code := f.approve(t, f.user, c.ClientID, redirect, challenge, Consent{SameBrowser: true, Nickname: "Secret client"})
	_, oerr := f.svc.Exchange(ctx, codeForm(code, redirect, verifier), ClientAuth{ClientID: c.ClientID})
	wantOAuthErr(t, oerr, "invalid_client")
	_, oerr = f.svc.Exchange(ctx, codeForm(code, redirect, verifier), ClientAuth{ClientID: c.ClientID, Secret: "tycs_wrong"})
	wantOAuthErr(t, oerr, "invalid_client")
	// A failed client authentication does not use the code up.
	if _, oerr = f.svc.Exchange(ctx, codeForm(code, redirect, verifier), ClientAuth{ClientID: c.ClientID, Secret: c.ClientSecret, Basic: true}); oerr != nil {
		t.Fatalf("basic auth: %v", oerr)
	}
	pub := f.register(t, "none", redirect)
	_, oerr = f.svc.Exchange(ctx, codeForm("tyoc_x", redirect, verifier), ClientAuth{ClientID: pub.ClientID, Secret: "anything"})
	wantOAuthErr(t, oerr, "invalid_client")
}

func TestRevoke(t *testing.T) {
	f := newFixture(t)
	redirect := "https://client.example/cb"
	c := f.register(t, "none", redirect)
	other := f.register(t, "none", redirect)
	ca := ClientAuth{ClientID: c.ClientID}
	ctx := context.Background()
	verifier, challenge := pkcePair(t)
	code := f.approve(t, f.user, c.ClientID, redirect, challenge, Consent{SameBrowser: true, Nickname: "Revoke me"})
	tok, oerr := f.svc.Exchange(ctx, codeForm(code, redirect, verifier), ca)
	if oerr != nil {
		t.Fatal(oerr)
	}
	if oerr := f.svc.Revoke(ctx, "tyat_unknown", ca); oerr != nil {
		t.Fatalf("unknown token: %v", oerr)
	}
	if oerr := f.svc.Revoke(ctx, tok.AccessToken, ClientAuth{ClientID: other.ClientID}); oerr != nil {
		t.Fatalf("another client's token: %v", oerr)
	}
	if _, err := f.svc.VerifyAccessToken(ctx, tok.AccessToken); err != nil {
		t.Fatal("another client revoked this client's token")
	}
	if oerr := f.svc.Revoke(ctx, tok.AccessToken, ca); oerr != nil {
		t.Fatal(oerr)
	}
	if _, err := f.svc.VerifyAccessToken(ctx, tok.AccessToken); err != identity.ErrAgentTokenInvalid {
		t.Fatalf("revoked access token: %v", err)
	}
	// The refresh token still works after its access token is revoked;
	// revoking it ends the grant.
	t2, oerr := f.svc.Exchange(ctx, refreshForm(tok.RefreshToken), ca)
	if oerr != nil {
		t.Fatal(oerr)
	}
	if oerr := f.svc.Revoke(ctx, t2.RefreshToken, ca); oerr != nil {
		t.Fatal(oerr)
	}
	if _, err := f.svc.VerifyAccessToken(ctx, t2.AccessToken); err != identity.ErrAgentTokenInvalid {
		t.Fatalf("access token after refresh revocation: %v", err)
	}
	apps, _ := f.svc.ListApps(ctx, f.user.ID)
	if len(apps) != 1 || apps[0].Status != "revoked" || apps[0].RevokedReason != "revoked_by_client" {
		t.Fatalf("apps: %+v", apps)
	}
}

func TestVerifyRefusesExpiredDisabledAndBlocked(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()
	if _, err := f.db.Exec(`INSERT INTO users(id, username, password_hash, role, status, created_at, updated_at) VALUES('u_member','member','','member','active',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	member, err := identity.New(f.db).GetUserByID(ctx, "u_member")
	if err != nil {
		t.Fatal(err)
	}
	redirect := "https://client.example/cb"
	c := f.register(t, "none", redirect)
	ca := ClientAuth{ClientID: c.ClientID}
	issue := func(u *identity.User) *TokenResponse {
		verifier, challenge := pkcePair(t)
		code := f.approve(t, u, c.ClientID, redirect, challenge, Consent{SameBrowser: true, Nickname: "Member app"})
		tok, oerr := f.svc.Exchange(ctx, codeForm(code, redirect, verifier), ca)
		if oerr != nil {
			t.Fatal(oerr)
		}
		return tok
	}
	tok := issue(member)
	f.clock = f.clock.Add(2 * time.Hour)
	if _, err := f.svc.VerifyAccessToken(ctx, tok.AccessToken); err != identity.ErrAgentTokenInvalid {
		t.Fatalf("expired: %v", err)
	}
	f.clock = f.clock.Add(-2 * time.Hour)
	if _, err := f.db.Exec(`UPDATE users SET status = 'blocked' WHERE id = 'u_member'`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.VerifyAccessToken(ctx, tok.AccessToken); err != identity.ErrAgentTokenInvalid {
		t.Fatalf("blocked owner: %v", err)
	}
	if _, oerr := f.svc.Exchange(ctx, refreshForm(tok.RefreshToken), ca); oerr == nil {
		t.Fatal("a blocked owner's client refreshed")
	}
	tok = issue(f.user)
	ag, err := f.svc.VerifyAccessToken(ctx, tok.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`UPDATE agents SET disabled = 1 WHERE id = ?`, ag.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.VerifyAccessToken(ctx, tok.AccessToken); err != identity.ErrAgentTokenInvalid {
		t.Fatalf("disabled agent: %v", err)
	}
}

func TestDenyAndAnsweredRequests(t *testing.T) {
	f := newFixture(t)
	redirect := "https://client.example/cb"
	c := f.register(t, "none", redirect)
	_, challenge := pkcePair(t)
	ctx := context.Background()
	req, aerr := f.svc.BeginAuthorize(ctx, authorizeQuery(c.ClientID, redirect, challenge, nil), "198.51.100.7")
	if aerr != nil {
		t.Fatal(aerr)
	}
	loc, err := f.svc.Deny(ctx, req.ID, f.user)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(loc)
	if u.Query().Get("error") != "access_denied" || u.Query().Get("state") != "st-123" {
		t.Fatalf("deny redirect %q", loc)
	}
	if _, err := f.svc.Approve(ctx, req.ID, f.user, Consent{SameBrowser: true, Nickname: "late"}); err != ErrRequestNotFound {
		t.Fatalf("approve after deny: %v", err)
	}
	if _, err := f.svc.PendingRequest(ctx, req.ID); err != ErrRequestNotFound {
		t.Fatalf("answered request still pending: %v", err)
	}
	req, _ = f.svc.BeginAuthorize(ctx, authorizeQuery(c.ClientID, redirect, challenge, nil), "198.51.100.7")
	if _, err := f.svc.Approve(ctx, req.ID, f.user, Consent{SameBrowser: true, Nickname: "bad/name"}); err != ErrInvalidNickname {
		t.Fatalf("bad nickname: %v", err)
	}
	if _, err := f.svc.Approve(ctx, req.ID, f.user, Consent{SameBrowser: true, Nickname: "ok", Scope: access.AgentScope{Mode: "some"}}); err == nil {
		t.Fatal("a bad scope was accepted")
	}
	f.clock = f.clock.Add(RequestTTL + time.Second)
	if _, err := f.svc.Approve(ctx, req.ID, f.user, Consent{SameBrowser: true, Nickname: "ok"}); err != ErrRequestNotFound {
		t.Fatalf("expired request: %v", err)
	}
}

func TestDefaultNickname(t *testing.T) {
	for in, want := range map[string]string{
		"Claude Code": "Claude Code", "ChatGPT’s connector": "ChatGPT s connector", "": "MCP client", "日本": "MCP client",
		"cursor-vscode/1.2": "cursor-vscode 1.2",
	} {
		if got := DefaultNickname(in); got != want {
			t.Errorf("DefaultNickname(%q) = %q, want %q", in, got, want)
		}
		if _, err := CheckNickname(DefaultNickname(in)); err != nil {
			t.Errorf("DefaultNickname(%q) is not a valid nickname", in)
		}
	}
}

func TestRateLimitIsSharedThroughTheDatabase(t *testing.T) {
	f := newFixture(t)
	other := New(Options{DB: f.db, PublicURL: testPublic, Now: func() time.Time { return f.clock }})
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		s := f.svc
		if i%2 == 1 {
			s = other // a second replica counts into the same bucket
		}
		if !s.allow(ctx, "test:ip", 3, time.Minute) {
			t.Fatalf("hit %d refused", i+1)
		}
	}
	if f.svc.allow(ctx, "test:ip", 3, time.Minute) {
		t.Fatal("fourth hit allowed")
	}
	f.clock = f.clock.Add(time.Minute + time.Second)
	if !other.allow(ctx, "test:ip", 3, time.Minute) {
		t.Fatal("a new window was refused")
	}
}

func TestSweepKeepsGrants(t *testing.T) {
	f := newFixture(t)
	redirect := "https://client.example/cb"
	c := f.register(t, "none", redirect)
	unused := f.register(t, "none", redirect)
	verifier, challenge := pkcePair(t)
	code := f.approve(t, f.user, c.ClientID, redirect, challenge, Consent{SameBrowser: true, Nickname: "kept"})
	if _, oerr := f.svc.Exchange(context.Background(), codeForm(code, redirect, verifier), ClientAuth{ClientID: c.ClientID}); oerr != nil {
		t.Fatal(oerr)
	}
	f.clock = f.clock.Add(40 * 24 * time.Hour)
	if err := f.svc.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	count := func(q string, args ...any) int {
		var n int
		if err := f.db.QueryRow(q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(`SELECT COUNT(*) FROM mcp_oauth_tokens`); n != 0 {
		t.Errorf("%d expired tokens left", n)
	}
	if n := count(`SELECT COUNT(*) FROM mcp_oauth_codes`) + count(`SELECT COUNT(*) FROM mcp_oauth_requests`) + count(`SELECT COUNT(*) FROM mcp_oauth_return_tickets`); n != 0 {
		t.Errorf("%d expired codes, requests or return links left", n)
	}
	if n := count(`SELECT COUNT(*) FROM mcp_oauth_grants`); n != 1 {
		t.Errorf("grants: %d, want 1", n)
	}
	if n := count(`SELECT COUNT(*) FROM mcp_oauth_clients WHERE client_id = ?`, unused.ClientID); n != 0 {
		t.Error("an unused registration survived the sweep")
	}
	if n := count(`SELECT COUNT(*) FROM mcp_oauth_clients WHERE client_id = ?`, c.ClientID); n != 1 {
		t.Error("a client with a grant was swept")
	}
}

// cimdServer serves a client metadata document over TLS on loopback.
func cimdServer(t *testing.T, doc func(url string) any, status int) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if status != http.StatusOK {
			w.Header().Set("Location", "https://elsewhere.example/doc")
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "max-age=600")
		_ = json.NewEncoder(w).Encode(doc(srv.URL + r.URL.Path))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func cimdService(f *fixture, srv *httptest.Server, allowPrivate bool) *Service {
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return New(Options{DB: f.db, PublicURL: testPublic, Access: access.New(f.db), Now: func() time.Time { return f.clock },
		AllowPrivateCIMD: allowPrivate, CIMDRootCAs: pool})
}

func TestClientIDMetadataDocument(t *testing.T) {
	f := newFixture(t)
	redirect := "http://127.0.0.1:43999/cb"
	srv, hits := cimdServer(t, func(u string) any {
		return map[string]any{"client_id": u, "client_name": "CIMD Client", "redirect_uris": []string{redirect}, "token_endpoint_auth_method": "none"}
	}, http.StatusOK)
	svc := cimdService(f, srv, true)
	ctx := context.Background()
	id := srv.URL + "/oauth/client.json"
	// Client never fetches: before an authorization request read the
	// document, the client is unknown.
	if _, oerr := svc.Client(ctx, id); oerr == nil || atomic.LoadInt32(hits) != 0 {
		t.Fatalf("Client fetched or knew an unread document (%d fetches, %v)", atomic.LoadInt32(hits), oerr)
	}
	c, oerr := svc.ResolveClient(ctx, id, "198.51.100.7")
	if oerr != nil {
		t.Fatal(oerr)
	}
	if c.Kind != ClientKindCIMD || c.Name != "CIMD Client" || !c.Public() {
		t.Fatalf("client: %+v", c)
	}
	if _, oerr := svc.ResolveClient(ctx, id, "198.51.100.7"); oerr != nil || atomic.LoadInt32(hits) != 1 {
		t.Fatalf("the cached document was fetched again (%d fetches, %v)", atomic.LoadInt32(hits), oerr)
	}
	f.clock = f.clock.Add(11 * time.Minute)
	// An expired copy still serves the token endpoint (Client)...
	if _, oerr := svc.Client(ctx, id); oerr != nil || atomic.LoadInt32(hits) != 1 {
		t.Fatalf("Client refused or refetched a stale copy (%d fetches, %v)", atomic.LoadInt32(hits), oerr)
	}
	// ...and an authorization request fetches it again.
	if _, oerr := svc.ResolveClient(ctx, id, "198.51.100.7"); oerr != nil || atomic.LoadInt32(hits) != 2 {
		t.Fatalf("an expired copy was not fetched again (%d fetches, %v)", atomic.LoadInt32(hits), oerr)
	}
	// The whole flow works for a document client.
	verifier, challenge := pkcePair(t)
	req, aerr := svc.BeginAuthorize(ctx, authorizeQuery(id, redirect, challenge, nil), "198.51.100.7")
	if aerr != nil {
		t.Fatal(aerr)
	}
	ticket, err := svc.Approve(ctx, req.ID, f.user, Consent{SameBrowser: true, Nickname: "Doc client"})
	if err != nil {
		t.Fatal(err)
	}
	loc, err := svc.RedeemReturn(ctx, ticket)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(loc)
	tok, oerr := svc.Exchange(ctx, codeForm(u.Query().Get("code"), redirect, verifier), ClientAuth{ClientID: id})
	if oerr != nil {
		t.Fatal(oerr)
	}
	if ag, err := svc.VerifyAccessToken(ctx, tok.AccessToken); err != nil || ag.Name != "Doc client" {
		t.Fatalf("verify: %+v %v", ag, err)
	}
	// Outside tests: port 443 only, and private addresses are refused,
	// with a generic answer (the detail goes to the log).
	strict := cimdService(f, srv, false)
	if _, oerr := strict.ResolveClient(ctx, srv.URL+"/another.json", "198.51.100.7"); oerr == nil || !strings.Contains(oerr.Description, "port 443") {
		t.Fatalf("a document on another port was allowed: %v", oerr)
	}
	for _, private := range []string{"10.0.0.5", "100.100.1.1", "::ffff:127.0.0.1", "64:ff9b::a00:1", "2002:a00:1::1", "fd00::1"} {
		strict.resolver = func(context.Context, string) ([]net.IPAddr, error) {
			return []net.IPAddr{{IP: net.ParseIP(private)}}, nil
		}
		_, _, err := strict.fetchMetadataDocument(ctx, "https://client.example/doc.json")
		if err == nil || !strings.Contains(err.Error(), "not on the public internet") {
			t.Errorf("DNS answer %s: %v", private, err)
		}
		before := atomic.LoadInt32(hits)
		if _, oerr := strict.ResolveClient(ctx, "https://client-"+strings.NewReplacer(":", "-", ".", "-").Replace(private)+".example/doc.json", "198.51.100.8"); oerr == nil ||
			oerr.Description != errCIMDUnusable().Description || atomic.LoadInt32(hits) != before {
			t.Errorf("DNS answer %s: got %v, want the generic refusal and no fetch", private, oerr)
		}
	}
	for _, bad := range []string{"https://client.example/", "https://client.example", "https://u@client.example/x", "https://client.example/a/../b"} {
		if _, oerr := svc.Client(ctx, bad); oerr == nil {
			t.Errorf("client_id %q accepted", bad)
		}
	}
}

func TestClientIDMetadataDocumentChecks(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for name, tc := range map[string]struct {
		doc    func(u string) any
		status int
	}{
		"client_id mismatch": {func(string) any {
			return map[string]any{"client_id": "https://other.example/doc", "redirect_uris": []string{"https://a.example/cb"}}
		}, http.StatusOK},
		"secret in the document": {func(u string) any {
			return map[string]any{"client_id": u, "redirect_uris": []string{"https://a.example/cb"}, "client_secret": "x"}
		}, http.StatusOK},
		"bad redirect": {func(u string) any {
			return map[string]any{"client_id": u, "redirect_uris": []string{"http://evil.example/cb"}}
		}, http.StatusOK},
		"secret auth method": {func(u string) any {
			return map[string]any{"client_id": u, "redirect_uris": []string{"https://a.example/cb"}, "token_endpoint_auth_method": "client_secret_post"}
		}, http.StatusOK},
		"redirect not followed": {nil, http.StatusFound},
	} {
		srv, _ := cimdServer(t, tc.doc, tc.status)
		if _, oerr := cimdService(f, srv, true).ResolveClient(ctx, srv.URL+"/doc.json", "198.51.100.7"); oerr == nil || oerr.Code != "invalid_client" ||
			oerr.Description != errCIMDUnusable().Description {
			t.Errorf("%s: got %v, want the generic invalid_client", name, oerr)
		}
	}
}

// Claude.ai's metadata document lists jwt-bearer next to authorization_code
// and refresh_token. toolyard ignores grant types it does not support and
// accepts the client, for documents and for registration alike; a client
// that leaves out authorization_code is still refused.
func TestClientsMayListUnsupportedGrantTypes(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	srv, _ := cimdServer(t, func(u string) any {
		return map[string]any{
			"client_id": u, "client_name": "Claude", "redirect_uris": []string{"https://a.example/cb"},
			"grant_types":                []string{"authorization_code", "refresh_token", "urn:ietf:params:oauth:grant-type:jwt-bearer"},
			"response_types":             []string{"code"},
			"token_endpoint_auth_method": "none",
		}
	}, http.StatusOK)
	c, oerr := cimdService(f, srv, true).ResolveClient(ctx, srv.URL+"/doc.json", "198.51.100.7")
	if oerr != nil {
		t.Fatalf("a document that also lists jwt-bearer was refused: %v", oerr)
	}
	if c.Name != "Claude" || !c.Public() {
		t.Fatalf("client: %+v", c)
	}
	noCode, _ := cimdServer(t, func(u string) any {
		return map[string]any{"client_id": u, "redirect_uris": []string{"https://a.example/cb"}, "grant_types": []string{"urn:ietf:params:oauth:grant-type:jwt-bearer"}}
	}, http.StatusOK)
	if _, oerr := cimdService(f, noCode, true).ResolveClient(ctx, noCode.URL+"/doc.json", "198.51.100.8"); oerr == nil || oerr.Code != "invalid_client" {
		t.Fatalf("a document without authorization_code was accepted: %v", oerr)
	}
	body, _ := json.Marshal(map[string]any{"redirect_uris": []string{"https://a.example/cb"}, "token_endpoint_auth_method": "none",
		"grant_types": []string{"authorization_code", "refresh_token", "client_credentials"}, "response_types": []string{"code", "token"}})
	if _, oerr := f.svc.Register(ctx, body, "198.51.100.9"); oerr != nil {
		t.Fatalf("a registration that also lists client_credentials was refused: %v", oerr)
	}
	body, _ = json.Marshal(map[string]any{"redirect_uris": []string{"https://a.example/cb"}, "response_types": []string{"token"}})
	if _, oerr := f.svc.Register(ctx, body, "198.51.100.9"); oerr == nil || oerr.Code != "invalid_client_metadata" {
		t.Fatalf("a registration without the code response type was accepted: %v", oerr)
	}
}

// ChatGPT's metadata document prefers private_key_jwt but lists none among
// the methods it can use (token_endpoint_auth_methods_supported); it picks
// from the methods both sides list, so toolyard takes it as a public
// client. A document that offers only private_key_jwt is still refused.
func TestDocumentsMayOfferNoneAmongAuthMethods(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	srv, _ := cimdServer(t, func(u string) any {
		return map[string]any{
			"client_id": u, "client_name": "ChatGPT", "redirect_uris": []string{"https://a.example/cb"},
			"token_endpoint_auth_method":            "private_key_jwt",
			"token_endpoint_auth_methods_supported": []string{"none", "private_key_jwt"},
			"token_endpoint_auth_signing_alg":       "RS256",
			"jwks_uri":                              u + "/jwks.json",
			"grant_types":                           []string{"authorization_code", "refresh_token"},
			"response_types":                        []string{"code"},
		}
	}, http.StatusOK)
	svc := cimdService(f, srv, true)
	c, oerr := svc.ResolveClient(ctx, srv.URL+"/doc.json", "198.51.100.7")
	if oerr != nil {
		t.Fatalf("a document that also offers none was refused: %v", oerr)
	}
	if c.Name != "ChatGPT" || !c.Public() || c.AuthMethod != AuthNone {
		t.Fatalf("client: %+v", c)
	}
	if stored, oerr := svc.Client(ctx, srv.URL+"/doc.json"); oerr != nil || !stored.Public() {
		t.Fatalf("stored client: %+v %v", stored, oerr)
	}
	for name, tc := range map[string]struct {
		preferred string
		supported any
		ok        bool
	}{
		"only private_key_jwt listed":    {"private_key_jwt", []string{"private_key_jwt"}, false},
		"no list":                        {"private_key_jwt", nil, false},
		"a secret method preferred":      {"client_secret_post", []string{"none"}, false},
		"a list of the wrong type":       {"none", "none", true},
		"private_key_jwt, list a string": {"private_key_jwt", "none", false},
	} {
		docSrv, _ := cimdServer(t, func(u string) any {
			doc := map[string]any{"client_id": u, "redirect_uris": []string{"https://a.example/cb"}, "token_endpoint_auth_method": tc.preferred}
			if tc.supported != nil {
				doc["token_endpoint_auth_methods_supported"] = tc.supported
			}
			return doc
		}, http.StatusOK)
		_, oerr := cimdService(f, docSrv, true).ResolveClient(ctx, docSrv.URL+"/doc.json", "198.51.100.8")
		if tc.ok && oerr != nil {
			t.Errorf("%s: refused: %v", name, oerr)
		}
		if !tc.ok && (oerr == nil || oerr.Code != "invalid_client") {
			t.Errorf("%s: accepted: %v", name, oerr)
		}
	}
	for _, tc := range []struct {
		preferred string
		supported []string
		want      string
	}{
		{"", nil, AuthNone},
		{"none", []string{"private_key_jwt"}, AuthNone},
		{"private_key_jwt", []string{" none "}, AuthNone},
		{"private_key_jwt", []string{"client_secret_post"}, "private_key_jwt"},
		{"client_secret_post", []string{"none"}, "client_secret_post"},
	} {
		if got := cimdAuthMethod(tc.preferred, tc.supported); got != tc.want {
			t.Errorf("cimdAuthMethod(%q, %q) = %q, want %q", tc.preferred, tc.supported, got, tc.want)
		}
	}
}

func TestHTTPEndpoints(t *testing.T) {
	f := newFixture(t)
	mux := http.NewServeMux()
	mux.HandleFunc(RegisterPath, func(w http.ResponseWriter, r *http.Request) { f.svc.HandleRegister(w, r, "198.51.100.7") })
	mux.HandleFunc(TokenPath, f.svc.HandleToken)
	mux.HandleFunc(RevokePath, f.svc.HandleRevoke)
	mux.HandleFunc(AuthorizationServerPath, f.svc.HandleAuthorizationServer)
	do := func(method, path, ctype, body string, hdr map[string]string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if ctype != "" {
			r.Header.Set("Content-Type", ctype)
		}
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	if w := do(http.MethodOptions, TokenPath, "", "", nil); w.Code != http.StatusNoContent || w.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("preflight: %d %v", w.Code, w.Header())
	}
	if w := do(http.MethodGet, AuthorizationServerPath, "", "", nil); w.Code != 200 || !strings.Contains(w.Body.String(), `"client_id_metadata_document_supported":true`) {
		t.Fatalf("metadata: %d %s", w.Code, w.Body)
	}
	if w := do(http.MethodPost, RegisterPath, "text/plain", `{}`, nil); w.Code != 400 || !strings.Contains(w.Body.String(), "invalid_client_metadata") {
		t.Fatalf("register text/plain: %d %s", w.Code, w.Body)
	}
	w := do(http.MethodPost, RegisterPath, "application/json", `{"redirect_uris":["https://a.example/cb"],"token_endpoint_auth_method":"client_secret_basic","client_name":"Basic"}`, nil)
	if w.Code != http.StatusCreated || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("register: %d %s", w.Code, w.Body)
	}
	var reg RegistrationResponse
	_ = json.Unmarshal(w.Body.Bytes(), &reg)
	if w := do(http.MethodPost, TokenPath, "application/json", `{}`, nil); w.Code != 400 || !strings.Contains(w.Body.String(), "invalid_request") {
		t.Fatalf("token json: %d %s", w.Code, w.Body)
	}
	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte(url.QueryEscape(reg.ClientID)+":wrong"))
	w = do(http.MethodPost, TokenPath, "application/x-www-form-urlencoded", "grant_type=authorization_code&code=x&redirect_uri=https%3A%2F%2Fa.example%2Fcb&code_verifier=v", map[string]string{"Authorization": basic})
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Header().Get("WWW-Authenticate"), "Basic") || !strings.Contains(w.Body.String(), `"error":"invalid_client"`) {
		t.Fatalf("bad secret: %d %v %s", w.Code, w.Header(), w.Body)
	}
	w = do(http.MethodPost, TokenPath, "application/x-www-form-urlencoded", "grant_type=refresh_token&grant_type=refresh_token", nil)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "repeated") {
		t.Fatalf("repeated parameter: %d %s", w.Code, w.Body)
	}
	basic = "Basic " + base64.StdEncoding.EncodeToString([]byte(url.QueryEscape(reg.ClientID)+":"+url.QueryEscape(reg.ClientSecret)))
	if w := do(http.MethodPost, RevokePath, "application/x-www-form-urlencoded", "token=tyrt_unknown", map[string]string{"Authorization": basic}); w.Code != 200 {
		t.Fatalf("revoke unknown: %d %s", w.Code, w.Body)
	}
	f.settings[SettingEnabled] = false
	if w := do(http.MethodGet, AuthorizationServerPath, "", "", nil); w.Code != 404 {
		t.Fatalf("metadata while off: %d", w.Code)
	}
}
