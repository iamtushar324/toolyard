package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/tusharbhardwaj/toolyard/internal/clerk"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/identitykeys"
)

const (
	connectStageOrigin = "https://stagebkt3.dev.beknown.live"
	connectDashOrigin  = "https://toolyard.example.com"
	connectIssuer      = "https://clerk.example.test"
	connectOrg         = "org_beknown"
)

// connectClerkServer is an httptest stand-in for one Clerk instance: the
// Frontend API's JWKS and the Backend API's user memberships. It signs
// session tokens with its own RSA key.
type connectClerkServer struct {
	t   *testing.T
	key *rsa.PrivateKey
	srv *httptest.Server

	mu       sync.Mutex
	jwksDown bool
	apiDown  bool
	// orgs is each Clerk user's organisations; a user missing here is a
	// 404 from the Backend API (unknown user).
	orgs map[string][]string
}

const connectKid = "ins_connect_1"

func newConnectClerkServer(t *testing.T) *connectClerkServer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &connectClerkServer{t: t, key: key, orgs: map[string][]string{"user_ada": {"org_other", connectOrg}}}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/jwks.json", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		down := f.jwksDown
		f.mu.Unlock()
		if down {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		pub := key.Public().(*rsa.PublicKey)
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]any{{
			"kty": "RSA", "use": "sig", "alg": "RS256", "kid": connectKid,
			"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("/v1/users/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk_test_secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.apiDown {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		uid := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/users/"), "/organization_memberships")
		orgs, ok := f.orgs[uid]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		data := []map[string]any{}
		for _, o := range orgs {
			data = append(data, map[string]any{
				"object": "organization_membership", "role": "org:member",
				"organization": map[string]any{"id": o},
				"public_user_data": map[string]any{
					"user_id": uid, "identifier": strings.TrimPrefix(uid, "user_") + "@beknown.work",
					"first_name": "Ada", "last_name": "Lovelace", "image_url": "https://img.clerk.com/ada",
				},
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "total_count": len(data)})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *connectClerkServer) set(fn func(*connectClerkServer)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

// client is a real *clerk.Client on this fake, with the dashboard origin
// as its only authorized party (as clerkFromEnv builds it).
func (f *connectClerkServer) client(t *testing.T) *clerk.Client {
	t.Helper()
	c, err := clerk.New(clerk.Config{
		SecretKey:         "sk_test_secret",
		PublishableKey:    "pk_test_" + base64.StdEncoding.EncodeToString([]byte("clerk.example.test$")),
		OrganizationID:    connectOrg,
		AuthorizedParties: []string{connectDashOrigin},
		HTTPClient:        f.srv.Client(),
		APIBase:           f.srv.URL + "/v1",
		JWKSURL:           f.srv.URL + "/.well-known/jwks.json",
		Issuer:            connectIssuer,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// token signs a session token for user_ada minted for the bkt3 stage
// origin; claims override (nil deletes) the defaults.
func (f *connectClerkServer) token(claims jwt.MapClaims) string {
	return f.tokenWith(f.key, claims)
}

func (f *connectClerkServer) tokenWith(key *rsa.PrivateKey, claims jwt.MapClaims) string {
	now := time.Now()
	base := jwt.MapClaims{
		"iss": connectIssuer, "sub": "user_ada", "sid": "sess_t3", "azp": connectStageOrigin,
		"iat": now.Unix(), "nbf": now.Unix(), "exp": now.Add(time.Minute).Unix(),
	}
	for k, v := range claims {
		if v == nil {
			delete(base, k)
			continue
		}
		base[k] = v
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, base)
	tok.Header["kid"] = connectKid
	s, err := tok.SignedString(key)
	if err != nil {
		f.t.Fatal(err)
	}
	return s
}

type connectEnv struct {
	*accessTestEnv
	fc  *connectClerkServer
	cfg *connectConfig
	reg *registryFake
	// h is the production chain with -public-url set: RoleGuard, HardenAPI
	// and the Origin check.
	h http.Handler
}

func newConnectEnv(t *testing.T) *connectEnv {
	t.Helper()
	e := newAccessTestServer(t)
	reg := withIdentityKeys(t, e)
	fc := newConnectClerkServer(t)
	c := fc.client(t)
	e.srv.clerk = c
	e.srv.connect = newConnectT3(c, []string{connectStageOrigin + "/"})
	e.srv.security = SecurityOptions{PublicURL: connectDashOrigin}
	mux := http.NewServeMux()
	e.srv.Routes(mux)
	var h http.Handler = e.srv.RoleGuard(mux)
	h = e.srv.HardenAPI(h)
	h = e.srv.EnforceOriginOnMutations(h)
	return &connectEnv{accessTestEnv: e, fc: fc, cfg: e.srv.connect, reg: reg, h: h}
}

// post is bkt3's server-to-server call: a JSON body and nothing else (no
// cookie, no Origin, no Referer, no X-Requested-With).
func (e *connectEnv) post(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	return e.postFrom(t, "203.0.113.7:4000", body)
}

func (e *connectEnv) postFrom(t *testing.T, remote, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/connect/t3", strings.NewReader(body))
	req.RemoteAddr = remote
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func connectBody(token string) string {
	b, _ := json.Marshal(map[string]string{"token": token})
	return string(b)
}

type connectResp struct {
	Token   string `json:"token"`
	Email   string `json:"email"`
	AgentID string `json:"agent_id"`
	UserID  string `json:"user_id"`
}

func (e *connectEnv) connect(t *testing.T, token string) connectResp {
	t.Helper()
	rec := e.post(t, connectBody(token))
	if rec.Code != http.StatusOK {
		t.Fatalf("connect: status %d: %s", rec.Code, rec.Body.String())
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	var out connectResp
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func expectConnectError(t *testing.T, name string, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if rec.Code != status {
		t.Errorf("%s: status %d, want %d (%s)", name, rec.Code, status, rec.Body.String())
		return
	}
	if got := decodeJSON(t, rec)["error"]; got != code {
		t.Errorf("%s: error %v, want %q", name, got, code)
	}
}

// bkt3Agents lists the user's agents named like the connect agent.
func (e *connectEnv) bkt3Agents(t *testing.T, uid string) []identity.Agent {
	t.Helper()
	agents, err := e.id.ListAgents(context.Background(), uid)
	if err != nil {
		t.Fatal(err)
	}
	var out []identity.Agent
	for _, a := range agents {
		if a.Name == connectT3AgentName {
			out = append(out, a)
		}
	}
	return out
}

func TestConnectT3CreatesThenRotates(t *testing.T) {
	e := newConnectEnv(t)
	ctx := context.Background()

	first := e.connect(t, e.fc.token(nil))
	if !strings.HasPrefix(first.AgentID, "ag_") || !strings.HasPrefix(first.UserID, "u_") ||
		first.Email != "ada@beknown.work" || !strings.HasPrefix(first.Token, first.AgentID+".") {
		t.Fatalf("first connect = %+v", first)
	}

	// The person is a Clerk-linked member, exactly as a dashboard sign-in
	// would have made them.
	u, err := e.id.GetUserByID(ctx, first.UserID)
	if err != nil {
		t.Fatal(err)
	}
	if u.ClerkUserID != "user_ada" || u.Email != "ada@beknown.work" || u.DisplayName != "Ada Lovelace" ||
		u.Role != identity.RoleMember || u.Status != identity.StatusActive || u.AvatarURL != "https://img.clerk.com/ada" {
		t.Errorf("user = %+v", u)
	}

	// Their identity key was issued (by connect:t3) and registered.
	st, err := e.srv.identityKeys.Status(ctx, first.UserID)
	if err != nil || !st.HasKey || st.Revealed {
		t.Fatalf("identity key status = %+v, %v", st, err)
	}
	if !e.reg.has(st.Fingerprint) {
		t.Errorf("identity key %s was not registered", st.Fingerprint)
	}
	targets, callers := e.reg.take()
	if len(targets) == 0 || targets[0] != "BkCoreServices."+identitykeys.ToolUpsert || callers[0] != "dashboard:"+connectT3Actor {
		t.Errorf("registry calls = %v as %v", targets, callers)
	}
	if n := e.auditRows(t, identitykeys.EventIssue, ""); n != 1 {
		t.Errorf("identity_key.issue rows = %d, want 1", n)
	}

	// The token is the bkt3 agent's, owned by the person, and authenticates
	// (the /mcp verifier and the bearer agent routes).
	ag, err := e.id.VerifyAgentToken(ctx, first.Token)
	if err != nil || ag.ID != first.AgentID || ag.Owner != first.UserID || ag.Name != connectT3AgentName || ag.Kind != identity.AgentKindAgent {
		t.Fatalf("verify first token = %+v, %v", ag, err)
	}
	if rec := e.bearer(t, first.Token, http.MethodGet, "/v1/agents/whoami", ""); rec.Code != http.StatusOK {
		t.Errorf("whoami with the connect token: %d %s", rec.Code, rec.Body.String())
	}

	// Second connect: same agent, new token, the old one dead at once; the
	// identity key is left alone.
	second := e.connect(t, e.fc.token(jwt.MapClaims{"sid": "sess_t3_again"}))
	if second.AgentID != first.AgentID || second.UserID != first.UserID || second.Token == first.Token {
		t.Fatalf("second connect = %+v, first %+v", second, first)
	}
	if _, err := e.id.VerifyAgentToken(ctx, first.Token); !errors.Is(err, identity.ErrAgentTokenInvalid) {
		t.Errorf("old token after rotation: err = %v, want ErrAgentTokenInvalid", err)
	}
	if rec := e.bearer(t, first.Token, http.MethodGet, "/v1/agents/whoami", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("whoami with the old token: %d, want 401", rec.Code)
	}
	if ag, err := e.id.VerifyAgentToken(ctx, second.Token); err != nil || ag.ID != first.AgentID {
		t.Errorf("new token: %+v, %v", ag, err)
	}
	st2, _ := e.srv.identityKeys.Status(ctx, first.UserID)
	if st2.Fingerprint != st.Fingerprint {
		t.Errorf("identity key changed on the second connect")
	}
	if targets, _ := e.reg.take(); len(targets) != 0 {
		t.Errorf("registry calls on the second connect: %v", targets)
	}
	if got := e.bkt3Agents(t, first.UserID); len(got) != 1 {
		t.Errorf("bkt3 agents = %+v, want exactly one", got)
	}

	// One connect.t3 row per call, attributed to the agent and the person.
	if c, r := e.auditRows(t, connectT3EventType, "created"), e.auditRows(t, connectT3EventType, "rotated"); c != 1 || r != 1 {
		t.Errorf("connect.t3 rows: created %d rotated %d, want 1 and 1", c, r)
	}
	var agentID, ownerID, ownerEmail, summary, clientKind string
	if err := e.db.QueryRow(`SELECT agent_id, owner_user_id, owner_email, result_summary, client_kind
		FROM audit_events WHERE event_type = ? AND reason = 'created'`, connectT3EventType).
		Scan(&agentID, &ownerID, &ownerEmail, &summary, &clientKind); err != nil {
		t.Fatal(err)
	}
	if agentID != first.AgentID || ownerID != first.UserID || ownerEmail != "ada@beknown.work" || clientKind != "t3" ||
		!strings.Contains(summary, "azp="+connectStageOrigin) || !strings.Contains(summary, "identity_key=issued") {
		t.Errorf("created row: agent=%s owner=%s email=%s kind=%s summary=%q", agentID, ownerID, ownerEmail, clientKind, summary)
	}
}

// A person who already signed in to the dashboard (and has a key) keeps
// their user row and key; connect adds only the bkt3 agent.
func TestConnectT3ExistingUserAndKey(t *testing.T) {
	e := newConnectEnv(t)
	ctx := context.Background()
	m := e.member(t, "user_ada", "ada@beknown.work")
	if _, err := e.srv.identityKeys.Issue(ctx, e.admin.ID, m.ID); err != nil {
		t.Fatal(err)
	}
	before, _ := e.srv.identityKeys.Status(ctx, m.ID)
	_, _ = e.reg.take()

	got := e.connect(t, e.fc.token(nil))
	if got.UserID != m.ID {
		t.Errorf("connect user = %s, want existing %s", got.UserID, m.ID)
	}
	after, _ := e.srv.identityKeys.Status(ctx, m.ID)
	if after.Fingerprint != before.Fingerprint {
		t.Errorf("existing identity key was replaced")
	}
	if targets, _ := e.reg.take(); len(targets) != 0 {
		t.Errorf("registry calls for a person who has a key: %v", targets)
	}
	var summary string
	_ = e.db.QueryRow(`SELECT result_summary FROM audit_events WHERE event_type = ?`, connectT3EventType).Scan(&summary)
	if !strings.Contains(summary, "identity_key=present") {
		t.Errorf("summary = %q", summary)
	}
}

func TestConnectT3Refusals(t *testing.T) {
	e := newConnectEnv(t)
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-5 * time.Minute)

	cases := []struct {
		name   string
		body   string
		status int
		code   string
	}{
		{"toolyard's own azp", connectBody(e.fc.token(jwt.MapClaims{"azp": connectDashOrigin})), http.StatusUnauthorized, "invalid_token"},
		{"other app's azp", connectBody(e.fc.token(jwt.MapClaims{"azp": "https://evil.example"})), http.StatusUnauthorized, "invalid_token"},
		{"no azp", connectBody(e.fc.token(jwt.MapClaims{"azp": nil})), http.StatusUnauthorized, "invalid_token"},
		{"expired", connectBody(e.fc.token(jwt.MapClaims{"exp": past.Unix(), "nbf": past.Add(-time.Minute).Unix(), "iat": past.Add(-time.Minute).Unix()})), http.StatusUnauthorized, "invalid_token"},
		{"bad signature", connectBody(e.fc.tokenWith(other, nil)), http.StatusUnauthorized, "invalid_token"},
		{"wrong issuer", connectBody(e.fc.token(jwt.MapClaims{"iss": "https://clerk.other.test"})), http.StatusUnauthorized, "invalid_token"},
		{"no sub", connectBody(e.fc.token(jwt.MapClaims{"sub": nil})), http.StatusUnauthorized, "invalid_token"},
		// Still within exp, but minted long ago: a replay, not a fresh token.
		{"old iat", connectBody(e.fc.token(jwt.MapClaims{"iat": past.Unix(), "nbf": past.Unix(), "exp": time.Now().Add(time.Hour).Unix()})), http.StatusUnauthorized, "invalid_token"},
		{"no sid", connectBody(e.fc.token(jwt.MapClaims{"sid": nil})), http.StatusUnauthorized, "invalid_token"},
		{"aud (JWT template)", connectBody(e.fc.token(jwt.MapClaims{"aud": "bkt3"})), http.StatusUnauthorized, "invalid_token"},
		{"garbage token", connectBody("not-a-jwt"), http.StatusUnauthorized, "invalid_token"},
		{"empty token", `{"token":"  "}`, http.StatusBadRequest, "bad_request"},
		{"no token", `{}`, http.StatusBadRequest, "bad_request"},
		{"unknown field", `{"jwt":"x"}`, http.StatusBadRequest, "bad_request"},
		{"not json", `token=x`, http.StatusBadRequest, "bad_request"},
		{"trailing value", `{"token":"x"}{"token":"y"}`, http.StatusBadRequest, "bad_request"},
	}
	for _, c := range cases {
		expectConnectError(t, c.name, e.post(t, c.body), c.status, c.code)
	}
	if n := e.auditRows(t, connectT3EventType, "invalid_token"); n != 11 {
		t.Errorf("invalid_token audit rows = %d, want 11", n)
	}
	if n := e.auditRows(t, connectT3EventType, "bad_request"); n != 5 {
		t.Errorf("bad_request audit rows = %d, want 5", n)
	}

	// Method.
	req := httptest.NewRequest(http.MethodGet, "/v1/connect/t3", nil)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: %d", rec.Code)
	}

	// Not in the org, and unknown to Clerk: 403, nobody created.
	e.fc.set(func(f *connectClerkServer) { f.orgs["user_ada"] = []string{"org_other"} })
	expectConnectError(t, "not a member", e.post(t, connectBody(e.fc.token(nil))), http.StatusForbidden, "not_org_member")
	expectConnectError(t, "unknown clerk user", e.post(t, connectBody(e.fc.token(jwt.MapClaims{"sub": "user_ghost"}))), http.StatusForbidden, "not_org_member")
	if n := e.auditRows(t, connectT3EventType, "not_org_member"); n != 2 {
		t.Errorf("not_org_member rows = %d, want 2", n)
	}
	if users, _ := e.id.ListUsers(context.Background()); len(users) != 1 {
		t.Errorf("users after refusals = %d, want only the admin", len(users))
	}
	e.fc.set(func(f *connectClerkServer) { f.orgs["user_ada"] = []string{connectOrg} })

	// Clerk Backend API down: 503.
	e.fc.set(func(f *connectClerkServer) { f.apiDown = true })
	expectConnectError(t, "backend api down", e.post(t, connectBody(e.fc.token(nil))), http.StatusServiceUnavailable, "clerk_unavailable")
	e.fc.set(func(f *connectClerkServer) { f.apiDown = false })

	// Blocked in toolyard: 403 user_disabled, no agent.
	ok := e.connect(t, e.fc.token(nil))
	if err := e.id.SetStatus(context.Background(), ok.UserID, identity.StatusBlocked, "left_org"); err != nil {
		t.Fatal(err)
	}
	expectConnectError(t, "blocked user", e.post(t, connectBody(e.fc.token(nil))), http.StatusForbidden, "user_disabled")
	if n := e.auditRows(t, connectT3EventType, "user_disabled"); n != 1 {
		t.Errorf("user_disabled rows = %d", n)
	}
	if err := e.id.SetStatus(context.Background(), ok.UserID, identity.StatusActive, ""); err != nil {
		t.Fatal(err)
	}

	// The person disabled their bkt3 agent: connect respects it.
	if err := e.id.SetAgentDisabled(context.Background(), ok.UserID, ok.AgentID, true); err != nil {
		t.Fatal(err)
	}
	expectConnectError(t, "agent disabled", e.post(t, connectBody(e.fc.token(nil))), http.StatusForbidden, "agent_disabled")
	if got := e.bkt3Agents(t, ok.UserID); len(got) != 1 || !got[0].Disabled {
		t.Errorf("bkt3 agents after agent_disabled = %+v", got)
	}

	// Every refusal row is "denied".
	var notDenied int
	_ = e.db.QueryRow(`SELECT count(*) FROM audit_events WHERE event_type = ? AND reason NOT IN ('created','rotated')
		AND COALESCE(decision,'') != 'denied'`, connectT3EventType).Scan(&notDenied)
	if notDenied != 0 {
		t.Errorf("%d refusal rows without decision=denied", notDenied)
	}
}

// The JWKS being down is 503, not 401: bkt3 retries instead of treating
// the person as signed out.
func TestConnectT3JWKSDown(t *testing.T) {
	e := newConnectEnv(t)
	e.fc.set(func(f *connectClerkServer) { f.jwksDown = true })
	expectConnectError(t, "jwks down", e.post(t, connectBody(e.fc.token(nil))), http.StatusServiceUnavailable, "clerk_unavailable")
	if n := e.auditRows(t, connectT3EventType, "clerk_unavailable"); n != 1 {
		t.Errorf("clerk_unavailable rows = %d", n)
	}
}

// The two Clerk flows stay apart: a bkt3 token can't open a dashboard
// session, and (above) a dashboard token can't connect.
func TestConnectT3TokenRejectedByDashboardSignIn(t *testing.T) {
	e := newConnectEnv(t)
	dashboard := func(token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/auth/clerk/session", strings.NewReader(connectBody(token)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Requested-With", "toolyard")
		req.Header.Set("Origin", connectDashOrigin)
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, req)
		return rec
	}
	rec := dashboard(e.fc.token(nil))
	expectConnectError(t, "bkt3 token on dashboard sign-in", rec, http.StatusUnauthorized, "invalid_token")
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName && c.Value != "" {
			t.Errorf("dashboard issued a session for a bkt3 token")
		}
	}
	// The same real client still signs a dashboard token in.
	if rec := dashboard(e.fc.token(jwt.MapClaims{"azp": connectDashOrigin})); rec.Code != http.StatusOK {
		t.Errorf("dashboard token on dashboard sign-in: %d %s", rec.Code, rec.Body.String())
	}
}

func TestConnectT3Disabled(t *testing.T) {
	e := newConnectEnv(t)
	tok := connectBody(e.fc.token(nil))

	e.srv.connect = nil
	expectConnectError(t, "no -connect-azp", e.post(t, tok), http.StatusNotFound, "connect_disabled")

	c := e.fc.client(t)
	if newConnectT3(nil, []string{connectStageOrigin}) != nil {
		t.Error("connect on without Clerk")
	}
	for _, origins := range [][]string{nil, {}, {" ", "/"}} {
		if newConnectT3(c, origins) != nil {
			t.Errorf("connect on with origins %q", origins)
		}
	}
	cfg := newConnectT3(c, []string{" HTTPS://StageBKT3.dev.beknown.live/ ", connectStageOrigin, "https://bkt3.example.com"})
	if cfg == nil || strings.Join(cfg.origins, ",") != connectStageOrigin+",https://bkt3.example.com" {
		t.Fatalf("normalised origins = %+v", cfg)
	}

	// New wires it from Options only with both Clerk and origins.
	off := New(t.Context(), Options{Identity: e.id, Audit: e.audit, Clerk: c})
	on := New(t.Context(), Options{Identity: e.id, Audit: e.audit, Clerk: c, ConnectAZP: []string{connectStageOrigin}})
	noClerk := New(t.Context(), Options{Identity: e.id, Audit: e.audit, ConnectAZP: []string{connectStageOrigin}})
	if off.connect != nil || on.connect == nil || noClerk.connect != nil {
		t.Errorf("connect wiring: off=%v on=%v noClerk=%v", off.connect != nil, on.connect != nil, noClerk.connect != nil)
	}
}

func TestConnectT3RateLimits(t *testing.T) {
	t.Run("per client ip", func(t *testing.T) {
		e := newConnectEnv(t)
		e.cfg.ipMax = 3
		for i := 0; i < 3; i++ {
			expectConnectError(t, "forged", e.post(t, connectBody("forged")), http.StatusUnauthorized, "invalid_token")
		}
		expectConnectError(t, "over ip budget", e.post(t, connectBody(e.fc.token(nil))), http.StatusTooManyRequests, "rate_limited")
		// Another address has its own budget.
		if rec := e.postFrom(t, "198.51.100.9:5000", connectBody(e.fc.token(nil))); rec.Code != http.StatusOK {
			t.Errorf("other ip: %d %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("per clerk subject", func(t *testing.T) {
		e := newConnectEnv(t)
		e.cfg.subjectMax = 2
		e.connect(t, e.fc.token(nil))
		e.connect(t, e.fc.token(nil))
		// Same person from another address: still over their budget.
		expectConnectError(t, "over subject budget", e.postFrom(t, "198.51.100.9:5000", connectBody(e.fc.token(nil))),
			http.StatusTooManyRequests, "rate_limited")
		if n := e.auditRows(t, connectT3EventType, "rate_limited"); n != 1 {
			t.Errorf("rate_limited rows = %d", n)
		}
		// Someone else is unaffected.
		e.fc.set(func(f *connectClerkServer) { f.orgs["user_grace"] = []string{connectOrg} })
		if got := e.connect(t, e.fc.token(jwt.MapClaims{"sub": "user_grace"})); got.Email != "grace@beknown.work" {
			t.Errorf("other person = %+v", got)
		}
	})
}

// Operator tokens can't reach it, and a member's cookie riding along
// doesn't change the answer.
func TestConnectT3GuardsAndExemptions(t *testing.T) {
	e := newConnectEnv(t)
	if _, ok := operatorScopeFor(http.MethodPost, "/v1/connect/t3"); ok {
		t.Error("operator tokens may call /v1/connect/t3")
	}
	op, _, err := e.id.CreateOperatorToken(context.Background(), e.admin.ID, "op", []string{identity.ScopeRead, identity.ScopeWrite, identity.ScopeOwner}, 0, "test")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/connect/t3", strings.NewReader(connectBody(e.fc.token(nil))))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+op)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("operator token: %d %s", rec.Code, rec.Body.String())
	}

	m := e.member(t, "user_other", "other@beknown.work")
	req = httptest.NewRequest(http.MethodPost, "/v1/connect/t3", strings.NewReader(connectBody(e.fc.token(nil))))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(e.cookieFor(t, m.ID))
	rec = httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("with a member cookie: %d %s", rec.Code, rec.Body.String())
	}
	var got connectResp
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Email != "ada@beknown.work" {
		t.Errorf("connected %q; the token, not the cookie, names the person", got.Email)
	}
}

// Neither the Clerk tokens nor any agent token or identity key reaches the
// audit log or the process log.
func TestConnectT3NoSecretsInAuditOrLogs(t *testing.T) {
	var logs bytes.Buffer
	var logsMu sync.Mutex
	prevSlog, prevOut, prevFlags := slog.Default(), log.Writer(), log.Flags()
	slog.SetDefault(slog.New(slog.NewTextHandler(&lockedWriter{w: &logs, mu: &logsMu}, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() {
		slog.SetDefault(prevSlog)
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})

	e := newConnectEnv(t)
	ctx := context.Background()
	secrets := []string{}
	add := func(s string) string { secrets = append(secrets, s); return s }

	first := e.connect(t, add(e.fc.token(nil)))
	second := e.connect(t, add(e.fc.token(jwt.MapClaims{"sid": "sess_2"})))
	add(first.Token)
	add(second.Token)
	// The random half of each agent token on its own, too.
	for _, tok := range []string{first.Token, second.Token} {
		_, raw, _ := strings.Cut(tok, ".")
		add(raw)
	}
	key, err := e.srv.identityKeys.ForwardKey(ctx, "dashboard:"+first.UserID)
	if err != nil || key == "" {
		t.Fatalf("identity key: %q, %v", key, err)
	}
	add(key)
	// Refusals carry tokens too.
	expectConnectError(t, "dashboard azp", e.post(t, connectBody(add(e.fc.token(jwt.MapClaims{"azp": connectDashOrigin})))), http.StatusUnauthorized, "invalid_token")
	e.fc.set(func(f *connectClerkServer) { f.orgs["user_ada"] = nil })
	expectConnectError(t, "not member", e.post(t, connectBody(add(e.fc.token(jwt.MapClaims{"sid": "sess_3"})))), http.StatusForbidden, "not_org_member")

	rows, err := e.db.Query(`SELECT * FROM audit_events`)
	if err != nil {
		t.Fatal(err)
	}
	cols, _ := rows.Columns()
	var auditText strings.Builder
	n := 0
	for rows.Next() {
		vals := make([]sql.RawBytes, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		for _, v := range vals {
			auditText.Write(v)
			auditText.WriteByte('\n')
		}
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	_ = rows.Close()
	if n < 4 || !strings.Contains(auditText.String(), connectT3EventType) {
		t.Fatalf("audit rows = %d; expected the connect.t3 rows", n)
	}

	logsMu.Lock()
	logText := logs.String()
	logsMu.Unlock()
	if !strings.Contains(logText, "connect-t3") || !strings.Contains(logText, "refused") || !strings.Contains(logText, "connected") {
		t.Fatalf("connect log lines were not captured:\n%s", logText)
	}
	for i, s := range secrets {
		if s == "" {
			continue
		}
		if strings.Contains(auditText.String(), s) {
			t.Errorf("secret #%d appears in the audit log", i)
		}
		if strings.Contains(logText, s) {
			t.Errorf("secret #%d appears in the process log", i)
		}
	}
}

type lockedWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// Concurrent connects of one person end with one agent and one key.
func TestConnectT3ConcurrentSamePerson(t *testing.T) {
	e := newConnectEnv(t)
	tok := connectBody(e.fc.token(nil))
	var wg sync.WaitGroup
	codes := make([]int, 4)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = e.post(t, tok).Code
		}()
	}
	wg.Wait()
	for i, c := range codes {
		if c != http.StatusOK {
			t.Errorf("call %d: %d", i, c)
		}
	}
	users, _ := e.id.ListUsers(context.Background())
	var uid string
	for _, u := range users {
		if u.ClerkUserID == "user_ada" {
			uid = u.ID
		}
	}
	if got := e.bkt3Agents(t, uid); len(got) != 1 {
		t.Errorf("bkt3 agents after concurrent connects = %d, want 1", len(got))
	}
	if n := e.auditRows(t, identitykeys.EventIssue, ""); n != 1 {
		t.Errorf("identity keys issued = %d, want 1", n)
	}
}

// An admin's revoke sticks: connect doesn't issue a new key to someone
// who had one, and makes no registry call for them (no upsert, and no
// retry of their pending removals as connect:t3). It still connects.
func TestConnectT3DoesNotUndoRevoke(t *testing.T) {
	e := newConnectEnv(t)
	ctx := context.Background()
	first := e.connect(t, e.fc.token(nil))
	if has, _ := e.srv.identityKeys.HasKey(ctx, first.UserID); !has {
		t.Fatal("precondition: first connect issued no key")
	}

	// The admin revokes; prime refuses the delete, so a removal stays
	// pending (and a registration row stays as revoked).
	e.reg.mu.Lock()
	e.reg.fail["BkCoreServices."+identitykeys.ToolDelete] = true
	e.reg.mu.Unlock()
	if _, err := e.srv.identityKeys.Revoke(ctx, e.admin.ID, first.UserID); err != nil {
		t.Fatal(err)
	}
	st, _ := e.srv.identityKeys.Status(ctx, first.UserID)
	if st.HasKey || len(st.PendingRemovals) != 1 {
		t.Fatalf("precondition after revoke: %+v", st)
	}
	_, _ = e.reg.take()
	e.reg.mu.Lock()
	e.reg.fail = map[string]bool{}
	e.reg.mu.Unlock()

	again := e.connect(t, e.fc.token(jwt.MapClaims{"sid": "sess_after_revoke"}))
	if again.AgentID != first.AgentID || again.Token == "" {
		t.Errorf("connect after revoke = %+v", again)
	}
	if ag, err := e.id.VerifyAgentToken(ctx, again.Token); err != nil || ag.ID != first.AgentID {
		t.Errorf("token after revoke: %+v, %v", ag, err)
	}
	st, _ = e.srv.identityKeys.Status(ctx, first.UserID)
	if st.HasKey {
		t.Error("connect re-issued a revoked key")
	}
	if len(st.PendingRemovals) != 1 {
		t.Errorf("pending removals touched: %+v", st.PendingRemovals)
	}
	if targets, callers := e.reg.take(); len(targets) != 0 {
		t.Errorf("registry calls after revoke: %v as %v", targets, callers)
	}
	if n := e.auditRows(t, identitykeys.EventIssue, ""); n != 1 {
		t.Errorf("identity_key.issue rows = %d, want 1 (the first connect)", n)
	}
	var summary string
	if err := e.db.QueryRow(`SELECT result_summary FROM audit_events WHERE event_type = ? AND reason = 'rotated'`,
		connectT3EventType).Scan(&summary); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(summary, "identity_key=missing") {
		t.Errorf("rotated row summary = %q, want identity_key=missing", summary)
	}

	// Someone who never had a key still gets one on their first connect.
	e.fc.set(func(f *connectClerkServer) { f.orgs["user_grace"] = []string{connectOrg} })
	grace := e.connect(t, e.fc.token(jwt.MapClaims{"sub": "user_grace"}))
	if has, _ := e.srv.identityKeys.HasKey(ctx, grace.UserID); !has {
		t.Error("never-had-a-key person got no key")
	}
	if targets, callers := e.reg.take(); len(targets) != 1 || targets[0] != "BkCoreServices."+identitykeys.ToolUpsert ||
		callers[0] != "dashboard:"+connectT3Actor {
		t.Errorf("registry calls for the new person = %v as %v", targets, callers)
	}
}

// A pending enrollment code on the newest "T3 Code (bkt3)" agent dies when
// connect rotates that agent, so exchanging it can't replace bkt3's token.
func TestConnectT3RotationKillsPendingEnrollment(t *testing.T) {
	e := newConnectEnv(t)
	ctx := context.Background()
	first := e.connect(t, e.fc.token(nil))
	code, pending, err := e.id.CreateEnrollment(ctx, first.UserID, connectT3AgentName, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	again := e.connect(t, e.fc.token(jwt.MapClaims{"sid": "sess_again"}))
	if again.AgentID != pending.ID {
		t.Fatalf("connect rotated %s, want the newest bkt3 agent %s", again.AgentID, pending.ID)
	}
	if _, _, err := e.id.ExchangeEnrollment(ctx, code); !errors.Is(err, identity.ErrEnrollNotFound) {
		t.Errorf("exchange after connect rotation: err = %v, want ErrEnrollNotFound", err)
	}
	if ag, err := e.id.VerifyAgentToken(ctx, again.Token); err != nil || ag.ID != pending.ID {
		t.Errorf("bkt3 token after the attempted exchange: %+v, %v", ag, err)
	}
}
