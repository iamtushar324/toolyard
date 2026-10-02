package oauth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// fakeIdP is a token endpoint: an authorization code becomes a token pair
// named after the code (with an id_token carrying an email), a refresh
// token is swapped for the next access token in sequence, and a refresh
// token in rejected answers invalid_grant. revoked records revocations.
type fakeIdP struct {
	srv      *httptest.Server
	mu       sync.Mutex
	rejected map[string]bool
	seq      int
	refreshd []string // refresh tokens presented, in order
	revoked  []string
	noExpiry bool
}

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	f := &fakeIdP{rejected: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			code := r.Form.Get("code")
			out := map[string]any{
				"access_token": "at-" + code, "refresh_token": "rt-" + code, "token_type": "Bearer",
				"scope": "read write", "id_token": idToken(code + "@example.test"),
			}
			if !f.noExpiry {
				out["expires_in"] = 3600
			}
			_ = json.NewEncoder(w).Encode(out)
		case "refresh_token":
			rt := r.Form.Get("refresh_token")
			f.refreshd = append(f.refreshd, rt)
			if f.rejected[rt] {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid_grant", "error_description": "revoked"})
				return
			}
			f.seq++
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": fmt.Sprintf("at-refreshed-%d", f.seq), "token_type": "Bearer", "expires_in": 3600,
			})
		default:
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "unsupported_grant_type"})
		}
	})
	mux.HandleFunc("/revoke", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		f.revoked = append(f.revoked, r.Form.Get("token"))
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// idToken is an unsigned JWT-shaped string with an email claim; the label
// extractor only decodes the payload.
func idToken(email string) string {
	hdr := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	pl, _ := json.Marshal(map[string]string{"email": email, "sub": "x"})
	return hdr + "." + base64.RawURLEncoding.EncodeToString(pl) + ".sig"
}

func (f *fakeIdP) reject(rt string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rejected[rt] = true
}

func (f *fakeIdP) refreshed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.refreshd...)
}

func (f *fakeIdP) revocations() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.revoked...)
}

// recBus and recNotifier record what the service published and whom it
// pushed to.
type recBus struct {
	mu     sync.Mutex
	events []string // "type:user_id:upstream"
}

func (b *recBus) Publish(typ string, data any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	m, _ := data.(map[string]any)
	uid, _ := m["user_id"].(string)
	up, _ := m["upstream"].(string)
	b.events = append(b.events, typ+":"+uid+":"+up)
}

func (b *recBus) list() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.events...)
}

type recNotifier struct {
	mu    sync.Mutex
	users []string
	urls  []string
}

func (n *recNotifier) Notify(_ context.Context, userID string, payload any) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.users = append(n.users, userID)
	if m, ok := payload.(map[string]any); ok {
		u, _ := m["url"].(string)
		n.urls = append(n.urls, u)
	}
	return nil
}

func (n *recNotifier) notified() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.users...)
}

type userFixture struct {
	db   *store.DB
	svc  *Service
	idp  *fakeIdP
	bus  *recBus
	push *recNotifier
}

// newUserFixture: a store with two users and one per_user upstream whose
// OAuth client points at the fake IdP.
func newUserFixture(t *testing.T) *userFixture {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "oauth.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, q := range []string{
		`INSERT INTO users(id, username, password_hash, created_at, updated_at) VALUES('u_ada','ada','x',1,1)`,
		`INSERT INTO users(id, username, password_hash, created_at, updated_at) VALUES('u_bob','bob','x',1,1)`,
		`INSERT INTO upstream_servers(name, transport, url, enabled, auth_mode, created_at, updated_at) VALUES('linear','http','http://127.0.0.1:1/mcp',1,'per_user',1,1)`,
		`INSERT INTO upstream_servers(name, transport, url, enabled, auth_mode, created_at, updated_at) VALUES('shared','http','http://127.0.0.1:1/mcp',1,'shared',1,1)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	key := make([]byte, MasterKeySize)
	_, _ = rand.Read(key)
	cipher, err := NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	idp := newFakeIdP(t)
	bus, push := &recBus{}, &recNotifier{}
	svc := New(db, cipher, bus, push, nil)
	svc.SetHTTPClient(idp.srv.Client())
	for _, name := range []string{"linear", "shared"} {
		if err := svc.PutClient(context.Background(), ClientRecord{
			UpstreamName: name, Issuer: idp.srv.URL, AuthorizationEndpoint: idp.srv.URL + "/authorize",
			TokenEndpoint: idp.srv.URL + "/token", RevocationEndpoint: idp.srv.URL + "/revoke",
			ClientID: "cid", RedirectURI: "https://toolyard.example/v1/mcp-oauth/callback",
			TokenEndpointAuthMethod: "none",
		}); err != nil {
			t.Fatal(err)
		}
	}
	return &userFixture{db: db, svc: svc, idp: idp, bus: bus, push: push}
}

// connect runs the per-user flow for uid end to end: begin, then exchange
// the code the way the callback does.
func (f *userFixture) connect(t *testing.T, upstream, uid, code string) *UserTokenRecord {
	t.Helper()
	ctx := context.Background()
	authURL, state, err := f.svc.BeginForUser(ctx, upstream, uid)
	if err != nil {
		t.Fatalf("BeginForUser: %v", err)
	}
	if !strings.Contains(authURL, "state="+state) || !strings.Contains(authURL, "code_challenge=") {
		t.Fatalf("authorize url: %s", authURL)
	}
	p, err := f.svc.LoadPending(ctx, state)
	if err != nil {
		t.Fatalf("LoadPending: %v", err)
	}
	if !p.PerUser || p.UserID != uid || p.Mode != ModeCallback {
		t.Fatalf("pending = %+v", p)
	}
	rec, err := f.svc.ExchangeCodeForUser(ctx, upstream, p.UserID, code, p.CodeVerifier)
	if err != nil {
		t.Fatalf("ExchangeCodeForUser: %v", err)
	}
	_ = f.svc.DeletePending(ctx, state)
	return rec
}

// TestUserTokenAADRejectsCopyToAnotherUser: ada's sealed row moved onto
// bob's row (or onto the shared row) fails to open, so a copied row can
// never act as somebody else.
func TestUserTokenAADRejectsCopyToAnotherUser(t *testing.T) {
	f := newUserFixture(t)
	ctx := context.Background()
	f.connect(t, "linear", "u_ada", "ada1")

	// Copy ada's ciphertexts into a row for bob.
	if _, err := f.db.Exec(`INSERT INTO oauth_user_tokens(upstream_name, user_id, access_token_enc, refresh_token_enc, state)
        SELECT upstream_name, 'u_bob', access_token_enc, refresh_token_enc, state FROM oauth_user_tokens WHERE user_id = 'u_ada'`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.GetUserToken(ctx, "linear", "u_bob"); err == nil {
		t.Fatal("bob's row opened with ada's ciphertext")
	}
	if f.svc.currentUserBearer("linear", "u_bob") != "" {
		t.Fatal("a bearer was served for the copied row")
	}
	// Ada's own row still opens.
	tok, err := f.svc.GetUserToken(ctx, "linear", "u_ada")
	if err != nil || tok == nil || tok.AccessToken != "at-ada1" || tok.AccountLabel != "ada1@example.test" {
		t.Fatalf("ada's row: %+v %v", tok, err)
	}
	// The same ciphertext on the shared row fails as well.
	if _, err := f.db.Exec(`INSERT INTO oauth_tokens(upstream_name, access_token_enc, state)
        SELECT upstream_name, access_token_enc, state FROM oauth_user_tokens WHERE user_id = 'u_ada'`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.GetToken(ctx, "linear"); err == nil {
		t.Fatal("shared row opened with a per-user ciphertext")
	}
	// And a shared token never shows up as a per-user bearer.
	if f.svc.currentUserBearer("linear", "u_nobody") != "" {
		t.Fatal("bearer for a user with no row")
	}
}

// TestUserConnectionsAndDisconnect: two people on one upstream each get
// their own row and bearer; disconnecting one revokes at the IdP and
// leaves the other untouched; the client record stays.
func TestUserConnectionsAndDisconnect(t *testing.T) {
	f := newUserFixture(t)
	ctx := context.Background()
	f.connect(t, "linear", "u_ada", "ada1")
	f.connect(t, "linear", "u_bob", "bob1")

	if got := f.svc.currentUserBearer("linear", "u_ada"); got != "at-ada1" {
		t.Fatalf("ada bearer = %q", got)
	}
	if got := f.svc.currentUserBearer("linear", "u_bob"); got != "at-bob1" {
		t.Fatalf("bob bearer = %q", got)
	}
	hdr := f.svc.UserHeaderFunc("linear")
	if h := hdr(ctx, "u_bob"); h["Authorization"] != "Bearer at-bob1" {
		t.Fatalf("header for bob = %v", h)
	}
	if h := hdr(ctx, ""); h != nil {
		t.Fatalf("header for no user = %v", h)
	}
	users, err := f.svc.ConnectedUsers(ctx, "linear")
	if err != nil || len(users) != 2 {
		t.Fatalf("ConnectedUsers = %v %v", users, err)
	}
	list, err := f.svc.ListUserConnections(ctx, "linear")
	if err != nil || len(list) != 2 || list[0].State != ConnConnected || list[0].AccountLabel != "ada1@example.test" {
		t.Fatalf("ListUserConnections = %+v %v", list, err)
	}
	mine, err := f.svc.UserConnections(ctx, "u_ada")
	if err != nil || len(mine) != 1 || mine["linear"].State != ConnConnected {
		t.Fatalf("UserConnections = %+v %v", mine, err)
	}
	events := f.bus.list()
	if len(events) != 2 || events[0] != "mcp_oauth_user_done:u_ada:linear" || events[1] != "mcp_oauth_user_done:u_bob:linear" {
		t.Fatalf("events = %v", events)
	}

	if err := f.svc.DisconnectUser(ctx, "linear", "u_ada"); err != nil {
		t.Fatalf("DisconnectUser: %v", err)
	}
	if rv := f.idp.revocations(); len(rv) != 1 || rv[0] != "rt-ada1" {
		t.Fatalf("revocations = %v", rv)
	}
	if ok, _ := f.svc.UserConnected(ctx, "linear", "u_ada"); ok {
		t.Fatal("ada still connected")
	}
	if ok, _ := f.svc.UserConnected(ctx, "linear", "u_bob"); !ok {
		t.Fatal("bob lost his connection")
	}
	if f.svc.currentUserBearer("linear", "u_ada") != "" {
		t.Fatal("ada's bearer survived the disconnect")
	}
	if has, _ := f.svc.HasClient(ctx, "linear"); !has {
		t.Fatal("client record dropped with one user's disconnect")
	}
	if err := f.svc.DisconnectUser(ctx, "linear", "u_ada"); err != ErrNoUserToken {
		t.Fatalf("second disconnect: %v", err)
	}

	// Removing the client (server removed or moved host) drops everyone.
	if err := f.svc.DeleteClient(ctx, "linear"); err != nil {
		t.Fatal(err)
	}
	if users, _ := f.svc.ConnectedUsers(ctx, "linear"); len(users) != 0 {
		t.Fatalf("users after DeleteClient = %v", users)
	}
	if f.svc.currentUserBearer("linear", "u_bob") != "" {
		t.Fatal("bob's bearer survived DeleteClient")
	}
}

// TestRefresherPerUser: the refresher renews one person's token and, on
// invalid_grant for another, marks only that person's row needs_reauth and
// notifies only them (SSE event and push naming My connections).
func TestRefresherPerUser(t *testing.T) {
	f := newUserFixture(t)
	ctx := context.Background()
	f.connect(t, "linear", "u_ada", "ada1")
	f.connect(t, "linear", "u_bob", "bob1")
	// Ada is also connected to a server that is back in shared mode: that row
	// is not in use and must be left alone.
	f.connect(t, "shared", "u_ada", "ada-shared")

	// Make every token due: expiry within the minimum lead.
	due := time.Now().Add(30 * time.Second).UnixMilli()
	if _, err := f.db.Exec(`UPDATE oauth_user_tokens SET access_expires_at = ?`, due); err != nil {
		t.Fatal(err)
	}
	f.idp.reject("rt-bob1")
	before := len(f.bus.list())

	f.svc.refreshUserTokens(ctx, time.Now())

	if got := f.idp.refreshed(); len(got) != 2 || got[0] != "rt-ada1" || got[1] != "rt-bob1" {
		t.Fatalf("refresh tokens presented = %v (the shared-mode row must be skipped)", got)
	}
	ada, _ := f.svc.GetUserToken(ctx, "linear", "u_ada")
	if ada.State != StateActive || ada.AccessToken != "at-refreshed-1" || ada.RefreshToken != "rt-ada1" ||
		ada.LastRefreshAt.IsZero() || ada.AccountLabel != "ada1@example.test" {
		t.Fatalf("ada after refresh = %+v", ada)
	}
	if f.svc.currentUserBearer("linear", "u_ada") != "at-refreshed-1" {
		t.Fatal("ada's cached bearer not updated")
	}
	bob, _ := f.svc.GetUserToken(ctx, "linear", "u_bob")
	if bob.State != StateNeedsReauth || bob.RefreshFailures != 1 || !strings.Contains(bob.LastError, "invalid_grant") {
		t.Fatalf("bob after invalid_grant = %+v", bob)
	}
	if f.svc.currentUserBearer("linear", "u_bob") != "" {
		t.Fatal("bob's dead token still served")
	}
	if ok, _ := f.svc.UserConnected(ctx, "linear", "u_bob"); ok {
		t.Fatal("bob reported connected after needs_reauth")
	}
	shared, _ := f.svc.GetUserToken(ctx, "shared", "u_ada")
	if shared.AccessToken != "at-ada-shared" {
		t.Fatalf("row on a shared-mode server was touched: %+v", shared)
	}

	events := f.bus.list()[before:]
	if len(events) != 1 || events[0] != "mcp_oauth_user_needs_reauth:u_bob:linear" {
		t.Fatalf("events after tick = %v", events)
	}
	if got := f.push.notified(); len(got) != 1 || got[0] != "u_bob" || f.push.urls[0] != "/#connections" {
		t.Fatalf("push notifications = %v urls=%v", got, f.push.urls)
	}

	// A second tick: bob is out of the candidate set; ada is not due.
	f.svc.refreshUserTokens(ctx, time.Now())
	if got := f.idp.refreshed(); len(got) != 2 {
		t.Fatalf("second tick refreshed again: %v", got)
	}

	// Repeated transient failures flip to needs_reauth after the limit,
	// counting per (server, user) and with the user's own backoff key.
	f.idp.reject("rt-ada1")
	ada, _ = f.svc.GetUserToken(ctx, "linear", "u_ada")
	ada.State = StateActive
	for i := 1; i < MaxRefreshFailures; i++ {
		f.svc.handleUserRefreshFailure(ctx, "linear", "u_ada", i, fmt.Errorf("idp down"))
	}
	ada, _ = f.svc.GetUserToken(ctx, "linear", "u_ada")
	if ada.State != StateActive || ada.RefreshFailures != MaxRefreshFailures-1 {
		t.Fatalf("ada before the limit = %+v", ada)
	}
	f.svc.handleUserRefreshFailure(ctx, "linear", "u_ada", MaxRefreshFailures, fmt.Errorf("idp down"))
	ada, _ = f.svc.GetUserToken(ctx, "linear", "u_ada")
	if ada.State != StateNeedsReauth {
		t.Fatalf("ada at the limit = %+v", ada)
	}
	if got := f.push.notified(); len(got) != 2 || got[1] != "u_ada" {
		t.Fatalf("push notifications = %v", got)
	}
	// The user reauth hook names the person, so only their connection goes.
	var hooked []string
	f.svc.SetUserReauthHook(func(up, uid string) { hooked = append(hooked, up+"/"+uid) })
	f.connect(t, "linear", "u_bob", "bob2")
	f.svc.MarkUserUnauthorized(ctx, "linear", "u_bob", "401 from upstream")
	time.Sleep(20 * time.Millisecond)
	if len(hooked) != 1 || hooked[0] != "linear/u_bob" {
		t.Fatalf("reauth hook calls = %v", hooked)
	}
	bob, _ = f.svc.GetUserToken(ctx, "linear", "u_bob")
	if bob.State != StateNeedsReauth || bob.LastError != "401 from upstream" {
		t.Fatalf("bob after 401 = %+v", bob)
	}
	// Idempotent: a second 401 does not notify again.
	n := len(f.push.notified())
	f.svc.MarkUserUnauthorized(ctx, "linear", "u_bob", "401 again")
	if len(f.push.notified()) != n {
		t.Fatal("second 401 notified again")
	}
}

// TestSharedTokenKeepsAccountLabel: the shared flow records the id_token
// email too (the Servers page shows "acts as <account>"), and a refresh
// without an id_token keeps it.
func TestSharedTokenKeepsAccountLabel(t *testing.T) {
	f := newUserFixture(t)
	ctx := context.Background()
	rec, err := f.svc.ExchangeCode(ctx, "shared", "bot", "verifier")
	if err != nil {
		t.Fatal(err)
	}
	if rec.AccountLabel != "bot@example.test" {
		t.Fatalf("label = %q", rec.AccountLabel)
	}
	if _, err := f.svc.Refresh(ctx, "shared"); err != nil {
		t.Fatal(err)
	}
	tok, _ := f.svc.GetToken(ctx, "shared")
	if tok.AccountLabel != "bot@example.test" || tok.AccessToken != "at-refreshed-1" {
		t.Fatalf("after refresh = %+v", tok)
	}
	if got := accountLabelFromIDToken("not-a-jwt"); got != "" {
		t.Fatalf("garbage id_token gave %q", got)
	}
}

// TestConnectionStateExpired: a token with no refresh token that has run
// out, or whose refresh token has run out, reports expired rather than
// connected, and is not a connected user.
func TestConnectionStateExpired(t *testing.T) {
	f := newUserFixture(t)
	ctx := context.Background()
	f.idp.noExpiry = true
	f.connect(t, "linear", "u_ada", "ada1")
	past := time.Now().Add(-time.Hour).UnixMilli()
	if _, err := f.db.Exec(`UPDATE oauth_user_tokens SET refresh_token_enc = NULL, access_expires_at = ? WHERE user_id = 'u_ada'`, past); err != nil {
		t.Fatal(err)
	}
	mine, _ := f.svc.UserConnections(ctx, "u_ada")
	if mine["linear"].State != ConnExpired {
		t.Fatalf("state = %q, want expired", mine["linear"].State)
	}
	if users, _ := f.svc.ConnectedUsers(ctx, "linear"); len(users) != 0 {
		t.Fatalf("expired user counted as connected: %v", users)
	}
	f.connect(t, "linear", "u_bob", "bob1")
	if _, err := f.db.Exec(`UPDATE oauth_user_tokens SET refresh_expires_at = ? WHERE user_id = 'u_bob'`, past); err != nil {
		t.Fatal(err)
	}
	if ok, _ := f.svc.UserConnected(ctx, "linear", "u_bob"); ok {
		t.Fatal("user with an expired refresh token counted as connected")
	}
}
