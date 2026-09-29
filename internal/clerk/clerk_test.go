package clerk

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// publishable key for "clerk.example.test$" — the frontend API host Clerk
// encodes into every pk_test_/pk_live_ key.
func testPublishableKey(t *testing.T, host string) string {
	t.Helper()
	return "pk_test_" + base64.StdEncoding.EncodeToString([]byte(host+"$"))
}

func TestFrontendAPI(t *testing.T) {
	cases := []struct {
		key  string
		want string
		ok   bool
	}{
		{testPublishableKey(t, "clerk.example.test"), "clerk.example.test", true},
		{"pk_live_" + base64.StdEncoding.EncodeToString([]byte("accounts.beknown.work$")), "accounts.beknown.work", true},
		// Trailing "$" is optional; unpadded base64 is tolerated.
		{"pk_test_" + base64.RawStdEncoding.EncodeToString([]byte("clerk.example.test")), "clerk.example.test", true},
		{"sk_test_notapublishablekey", "", false},
		{"pk_test_", "", false},
		{"pk_test_!!!", "", false},
		{"pk_test_" + base64.StdEncoding.EncodeToString([]byte("clerk.example.test/evil$")), "", false},
		{"pk_test_" + base64.StdEncoding.EncodeToString([]byte("localhost$")), "", false},
		{"pk_test_" + base64.StdEncoding.EncodeToString([]byte("host:443$")), "", false},
	}
	for _, c := range cases {
		got, err := FrontendAPI(c.key)
		if c.ok && (err != nil || got != c.want) {
			t.Errorf("FrontendAPI(%q) = %q, %v; want %q", c.key, got, err, c.want)
		}
		if !c.ok && err == nil {
			t.Errorf("FrontendAPI(%q) = %q, want error", c.key, got)
		}
	}
}

// fakeClerk is an httptest server standing in for both the Frontend API
// (JWKS) and the Backend API (memberships). It signs tokens with key and
// counts JWKS fetches so the cache behaviour is observable.
type fakeClerk struct {
	t          *testing.T
	key        *rsa.PrivateKey
	kid        string
	srv        *httptest.Server
	jwksFetch  atomic.Int32
	jwksDown   atomic.Bool // when set, the JWKS endpoint answers 503
	membership func(w http.ResponseWriter, r *http.Request)
	members    func(w http.ResponseWriter, r *http.Request)
}

func newFakeClerk(t *testing.T) *fakeClerk {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeClerk{t: t, key: key, kid: "ins_key_1"}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/jwks.json", func(w http.ResponseWriter, r *http.Request) {
		f.jwksFetch.Add(1)
		if f.jwksDown.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		pub := key.Public().(*rsa.PublicKey)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]any{{
				"kty": "RSA", "use": "sig", "alg": "RS256", "kid": f.kid,
				"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
				"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
			}},
		})
	})
	mux.HandleFunc("/v1/users/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk_test_secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if f.membership == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		f.membership(w, r)
	})
	mux.HandleFunc("/v1/organizations/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk_test_secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if f.members == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		f.members(w, r)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeClerk) client(t *testing.T) *Client {
	t.Helper()
	c, err := New(Config{
		SecretKey:         "sk_test_secret",
		PublishableKey:    testPublishableKey(t, "clerk.example.test"),
		OrganizationID:    "org_beknown",
		AuthorizedParties: []string{"https://toolyard.example.com"},
		HTTPClient:        f.srv.Client(),
		APIBase:           f.srv.URL + "/v1",
		JWKSURL:           f.srv.URL + "/.well-known/jwks.json",
		Issuer:            "https://clerk.example.test",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// token signs claims with the fake's RSA key. mutate lets a test poke the
// claims (or the header via kid) before signing.
func (f *fakeClerk) token(claims jwt.MapClaims, kid string) string {
	now := time.Now()
	base := jwt.MapClaims{
		"iss": "https://clerk.example.test",
		"sub": "user_123",
		"sid": "sess_abc",
		"azp": "https://toolyard.example.com",
		"iat": now.Unix(),
		"nbf": now.Unix(),
		"exp": now.Add(60 * time.Second).Unix(),
	}
	for k, v := range claims {
		if v == nil {
			delete(base, k)
			continue
		}
		base[k] = v
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, base)
	tok.Header["kid"] = kid
	s, err := tok.SignedString(f.key)
	if err != nil {
		f.t.Fatal(err)
	}
	return s
}

func TestNewRejectsIncompleteConfig(t *testing.T) {
	pk := testPublishableKey(t, "clerk.example.test")
	bad := []Config{
		{PublishableKey: pk, OrganizationID: "org", AuthorizedParties: []string{"https://a"}},
		{SecretKey: "sk", OrganizationID: "org", AuthorizedParties: []string{"https://a"}},
		{SecretKey: "sk", PublishableKey: pk, AuthorizedParties: []string{"https://a"}},
		{SecretKey: "sk", PublishableKey: pk, OrganizationID: "org"},
		{SecretKey: "sk", PublishableKey: "pk_test_!!", OrganizationID: "org", AuthorizedParties: []string{"https://a"}},
	}
	for i, cfg := range bad {
		if _, err := New(cfg); err == nil {
			t.Errorf("case %d: New accepted incomplete config", i)
		}
	}
	c, err := New(Config{SecretKey: "sk", PublishableKey: pk, OrganizationID: "org", AuthorizedParties: []string{"https://a"}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.FrontendAPI() != "clerk.example.test" || c.PublishableKey() != pk {
		t.Errorf("accessors = %q, %q", c.FrontendAPI(), c.PublishableKey())
	}
	if c.jwksURL != "https://clerk.example.test/.well-known/jwks.json" || c.issuer != "https://clerk.example.test" {
		t.Errorf("defaults: jwks=%q issuer=%q", c.jwksURL, c.issuer)
	}
}

func TestVerifySessionToken(t *testing.T) {
	f := newFakeClerk(t)
	ctx := context.Background()

	t.Run("valid", func(t *testing.T) {
		c := f.client(t)
		got, err := c.VerifySessionToken(ctx, f.token(nil, f.kid))
		if err != nil {
			t.Fatalf("verify: %v", err)
		}
		if got.Subject != "user_123" || got.SessionID != "sess_abc" || got.AuthorizedParty != "https://toolyard.example.com" {
			t.Errorf("claims = %+v", got)
		}
	})

	t.Run("wrong key", func(t *testing.T) {
		c := f.client(t)
		other, _ := rsa.GenerateKey(rand.Reader, 2048)
		tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
			"iss": "https://clerk.example.test", "sub": "user_123", "azp": "https://toolyard.example.com",
			"exp": time.Now().Add(time.Minute).Unix(),
		})
		tok.Header["kid"] = f.kid
		s, _ := tok.SignedString(other)
		if _, err := c.VerifySessionToken(ctx, s); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("wrong key: err = %v, want ErrInvalidToken", err)
		}
	})

	t.Run("expired", func(t *testing.T) {
		c := f.client(t)
		tok := f.token(jwt.MapClaims{"exp": time.Now().Add(-2 * time.Minute).Unix(), "nbf": time.Now().Add(-3 * time.Minute).Unix()}, f.kid)
		if _, err := c.VerifySessionToken(ctx, tok); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("expired: err = %v", err)
		}
	})

	t.Run("not yet valid", func(t *testing.T) {
		c := f.client(t)
		tok := f.token(jwt.MapClaims{"nbf": time.Now().Add(2 * time.Minute).Unix(), "exp": time.Now().Add(3 * time.Minute).Unix()}, f.kid)
		if _, err := c.VerifySessionToken(ctx, tok); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("nbf in future: err = %v", err)
		}
	})

	t.Run("missing exp", func(t *testing.T) {
		c := f.client(t)
		if _, err := c.VerifySessionToken(ctx, f.token(jwt.MapClaims{"exp": nil}, f.kid)); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("missing exp: err = %v", err)
		}
	})

	t.Run("wrong iss", func(t *testing.T) {
		c := f.client(t)
		if _, err := c.VerifySessionToken(ctx, f.token(jwt.MapClaims{"iss": "https://other.example.test"}, f.kid)); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("wrong iss: err = %v", err)
		}
	})

	t.Run("wrong azp", func(t *testing.T) {
		c := f.client(t)
		if _, err := c.VerifySessionToken(ctx, f.token(jwt.MapClaims{"azp": "https://evil.example"}, f.kid)); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("wrong azp: err = %v", err)
		}
	})

	t.Run("missing azp", func(t *testing.T) {
		c := f.client(t)
		if _, err := c.VerifySessionToken(ctx, f.token(jwt.MapClaims{"azp": nil}, f.kid)); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("missing azp: err = %v", err)
		}
	})

	t.Run("empty sub", func(t *testing.T) {
		c := f.client(t)
		if _, err := c.VerifySessionToken(ctx, f.token(jwt.MapClaims{"sub": "  "}, f.kid)); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("empty sub: err = %v", err)
		}
		if _, err := c.VerifySessionToken(ctx, f.token(jwt.MapClaims{"sub": nil}, f.kid)); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("missing sub: err = %v", err)
		}
	})

	t.Run("HS256 refused", func(t *testing.T) {
		c := f.client(t)
		// Sign with the JWKS modulus bytes as an HMAC secret: the classic
		// key-confusion attack. Must be refused by method, not by key.
		pub := f.key.Public().(*rsa.PublicKey)
		tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
			"iss": "https://clerk.example.test", "sub": "user_123", "azp": "https://toolyard.example.com",
			"exp": time.Now().Add(time.Minute).Unix(),
		})
		tok.Header["kid"] = f.kid
		s, _ := tok.SignedString(pub.N.Bytes())
		if _, err := c.VerifySessionToken(ctx, s); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("HS256: err = %v", err)
		}
	})

	t.Run("garbage", func(t *testing.T) {
		c := f.client(t)
		for _, s := range []string{"", "abc", "a.b.c", strings.Repeat("x", 5000)} {
			if _, err := c.VerifySessionToken(ctx, s); !errors.Is(err, ErrInvalidToken) {
				t.Errorf("garbage %q: err = %v", s[:min(len(s), 8)], err)
			}
		}
	})
}

func TestVerifySessionTokenUnknownKidRefetchesOnce(t *testing.T) {
	f := newFakeClerk(t)
	c := f.client(t)
	ctx := context.Background()
	now := time.Now()
	c.now = func() time.Time { return now }

	if _, err := c.VerifySessionToken(ctx, f.token(nil, f.kid)); err != nil {
		t.Fatalf("initial verify: %v", err)
	}
	if n := f.jwksFetch.Load(); n != 1 {
		t.Fatalf("jwks fetches after first verify = %d, want 1", n)
	}

	// Unknown kid right after a fetch: refused without another round trip.
	if _, err := c.VerifySessionToken(ctx, f.token(nil, "ins_key_2")); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("unknown kid: err = %v", err)
	}
	if n := f.jwksFetch.Load(); n != 1 {
		t.Fatalf("jwks fetches after unknown kid inside the minute = %d, want 1", n)
	}

	// A minute later Clerk has rotated: the unknown kid triggers exactly one
	// refetch, which now serves the new key.
	now = now.Add(61 * time.Second)
	f.kid = "ins_key_2"
	if _, err := c.VerifySessionToken(ctx, f.token(nil, "ins_key_2")); err != nil {
		t.Fatalf("verify after rotation: %v", err)
	}
	if n := f.jwksFetch.Load(); n != 2 {
		t.Fatalf("jwks fetches after rotation = %d, want 2", n)
	}
	// Another unknown kid straight after: still no extra fetch.
	if _, err := c.VerifySessionToken(ctx, f.token(nil, "ins_key_3")); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("second unknown kid: err = %v", err)
	}
	if n := f.jwksFetch.Load(); n != 2 {
		t.Fatalf("jwks fetches after second unknown kid = %d, want 2", n)
	}
}

func TestVerifySessionTokenJWKSDown(t *testing.T) {
	f := newFakeClerk(t)
	c := f.client(t)
	c.jwksURL = f.srv.URL + "/missing"
	if _, err := c.VerifySessionToken(context.Background(), f.token(nil, f.kid)); !errors.Is(err, ErrUnavailable) {
		t.Errorf("JWKS 404: err = %v, want ErrUnavailable", err)
	}
}

// A failed JWKS fetch must not be mistaken for a bad token: while Clerk is
// down, verifications that need the keys say ErrUnavailable (the login page
// retries) and the fetch is retried after a few seconds, not a minute. Only
// a successful fetch starts the one-minute unknown-kid rate limit.
func TestVerifySessionTokenJWKSFailureBackoff(t *testing.T) {
	f := newFakeClerk(t)
	c := f.client(t)
	ctx := context.Background()
	now := time.Now()
	c.now = func() time.Time { return now }
	fetches := func() int32 { return f.jwksFetch.Load() }
	// Tokens are minted against the fake clock, which this test moves
	// well past a real token's 60 s life.
	tok := func(kid string) string {
		return f.token(jwt.MapClaims{"iat": now.Unix(), "nbf": now.Unix(), "exp": now.Add(time.Minute).Unix()}, kid)
	}

	// Clerk is down before we ever hold a key.
	f.jwksDown.Store(true)
	if _, err := c.VerifySessionToken(ctx, tok(f.kid)); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("first verify with JWKS down: %v, want ErrUnavailable", err)
	}
	if n := fetches(); n != 1 {
		t.Fatalf("fetches = %d, want 1", n)
	}
	// Inside the backoff: still unavailable, no extra round trip.
	now = now.Add(2 * time.Second)
	if _, err := c.VerifySessionToken(ctx, tok(f.kid)); !errors.Is(err, ErrUnavailable) {
		t.Errorf("inside backoff: %v, want ErrUnavailable (not ErrInvalidToken)", err)
	}
	if n := fetches(); n != 1 {
		t.Errorf("fetches inside backoff = %d, want 1", n)
	}
	// After the backoff: retried (still down), still unavailable.
	now = now.Add(4 * time.Second)
	if _, err := c.VerifySessionToken(ctx, tok(f.kid)); !errors.Is(err, ErrUnavailable) {
		t.Errorf("after backoff, still down: %v", err)
	}
	if n := fetches(); n != 2 {
		t.Errorf("fetches after backoff = %d, want 2", n)
	}
	// Clerk recovers: the next attempt after the backoff succeeds.
	f.jwksDown.Store(false)
	now = now.Add(6 * time.Second)
	if _, err := c.VerifySessionToken(ctx, tok(f.kid)); err != nil {
		t.Fatalf("after recovery: %v", err)
	}
	if n := fetches(); n != 3 {
		t.Errorf("fetches after recovery = %d, want 3", n)
	}
	// Now the success-based rule applies: an unknown kid within the minute
	// is refused as invalid without a round trip.
	if _, err := c.VerifySessionToken(ctx, tok("ins_key_2")); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("unknown kid after success: %v, want ErrInvalidToken", err)
	}
	if n := fetches(); n != 3 {
		t.Errorf("fetches after unknown kid inside the minute = %d, want 3", n)
	}

	// A minute later Clerk rotates but is down again: the unknown kid is
	// "unavailable" not "invalid", known kids keep verifying from the
	// cache, and the retry comes after seconds.
	now = now.Add(61 * time.Second)
	f.jwksDown.Store(true)
	if _, err := c.VerifySessionToken(ctx, tok("ins_key_2")); !errors.Is(err, ErrUnavailable) {
		t.Errorf("rotation with JWKS down: %v, want ErrUnavailable", err)
	}
	if n := fetches(); n != 4 {
		t.Errorf("fetches at rotation = %d, want 4", n)
	}
	if _, err := c.VerifySessionToken(ctx, tok(f.kid)); err != nil {
		t.Errorf("known kid during outage: %v", err)
	}
	now = now.Add(2 * time.Second)
	if _, err := c.VerifySessionToken(ctx, tok("ins_key_2")); !errors.Is(err, ErrUnavailable) {
		t.Errorf("inside backoff at rotation: %v", err)
	}
	if n := fetches(); n != 4 {
		t.Errorf("fetches inside backoff at rotation = %d, want 4", n)
	}
	f.jwksDown.Store(false)
	f.kid = "ins_key_2"
	now = now.Add(4 * time.Second)
	if _, err := c.VerifySessionToken(ctx, tok("ins_key_2")); err != nil {
		t.Errorf("rotated key after recovery: %v", err)
	}
	if n := fetches(); n != 5 {
		t.Errorf("fetches after rotation recovery = %d, want 5", n)
	}
}

func membershipPage(orgIDs ...string) map[string]any {
	data := make([]map[string]any, 0, len(orgIDs))
	for _, id := range orgIDs {
		data = append(data, map[string]any{
			"object":       "organization_membership",
			"id":           "orgmem_" + id,
			"role":         "org:member",
			"organization": map[string]any{"id": id, "name": "Org " + id},
			"public_user_data": map[string]any{
				"user_id": "user_123", "identifier": "ada@beknown.work",
				"first_name": "Ada", "last_name": "Lovelace",
				"image_url": "https://img.clerk.com/ada", "has_image": true,
			},
		})
	}
	return map[string]any{"data": data, "total_count": len(orgIDs)}
}

func TestOrgMembership(t *testing.T) {
	f := newFakeClerk(t)
	c := f.client(t)
	ctx := context.Background()

	t.Run("member", func(t *testing.T) {
		f.membership = func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/users/user_123/organization_memberships" {
				t.Errorf("path = %s", r.URL.Path)
			}
			_ = json.NewEncoder(w).Encode(membershipPage("org_other", "org_beknown"))
		}
		m, err := c.OrgMembership(ctx, "user_123")
		if err != nil {
			t.Fatalf("OrgMembership: %v", err)
		}
		if !m.IsMember || m.Role != "org:member" || m.Email != "ada@beknown.work" ||
			m.FirstName != "Ada" || m.LastName != "Lovelace" || m.ImageURL != "https://img.clerk.com/ada" {
			t.Errorf("member = %+v", m)
		}
	})

	t.Run("non-member", func(t *testing.T) {
		f.membership = func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(membershipPage("org_other"))
		}
		m, err := c.OrgMembership(ctx, "user_123")
		if !errors.Is(err, ErrNotMember) || m.IsMember {
			t.Errorf("non-member: %+v, %v", m, err)
		}
	})

	t.Run("unknown user", func(t *testing.T) {
		f.membership = func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"errors":[{"message":"not found"}]}`))
		}
		if _, err := c.OrgMembership(ctx, "user_nobody"); !errors.Is(err, ErrNotMember) {
			t.Errorf("404: err = %v, want ErrNotMember", err)
		}
	})

	t.Run("api 500", func(t *testing.T) {
		f.membership = func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}
		if _, err := c.OrgMembership(ctx, "user_123"); !errors.Is(err, ErrUnavailable) {
			t.Errorf("500: err = %v, want ErrUnavailable", err)
		}
	})

	t.Run("paginates", func(t *testing.T) {
		f.membership = func(w http.ResponseWriter, r *http.Request) {
			// Page 1 is full but has only other orgs; the match sits on page 2.
			q := r.URL.Query()
			switch q.Get("offset") {
			case "0", "":
				ids := make([]string, 0, membershipPageSize)
				for i := 0; i < membershipPageSize; i++ {
					ids = append(ids, "org_other")
				}
				page := membershipPage(ids...)
				page["total_count"] = membershipPageSize + 1
				_ = json.NewEncoder(w).Encode(page)
			default:
				page := membershipPage("org_beknown")
				page["total_count"] = membershipPageSize + 1
				_ = json.NewEncoder(w).Encode(page)
			}
		}
		m, err := c.OrgMembership(ctx, "user_123")
		if err != nil || !m.IsMember {
			t.Errorf("paginated: %+v, %v", m, err)
		}
	})

	t.Run("bad secret", func(t *testing.T) {
		f.membership = func(w http.ResponseWriter, r *http.Request) { t.Error("reached handler") }
		bad := f.client(t)
		bad.secretKey = "sk_wrong"
		if _, err := bad.OrgMembership(ctx, "user_123"); !errors.Is(err, ErrUnavailable) {
			t.Errorf("401: err = %v, want ErrUnavailable", err)
		}
	})
}

func TestOrgMembers(t *testing.T) {
	f := newFakeClerk(t)
	c := f.client(t)
	ctx := context.Background()

	t.Run("two pages", func(t *testing.T) {
		f.members = func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/organizations/org_beknown/memberships" {
				t.Errorf("path = %s", r.URL.Path)
			}
			q := r.URL.Query()
			if q.Get("limit") != "500" {
				t.Errorf("limit = %q", q.Get("limit"))
			}
			row := func(uid, role string, withPUD bool) map[string]any {
				m := map[string]any{
					"object": "organization_membership", "id": "orgmem_" + uid, "role": role,
					"organization": map[string]any{"id": "org_beknown"},
				}
				if withPUD {
					m["public_user_data"] = map[string]any{
						"user_id": uid, "identifier": uid + "@beknown.work",
						"first_name": "F", "last_name": nil, "image_url": "https://img.clerk.com/" + uid,
					}
				}
				return m
			}
			switch q.Get("offset") {
			case "0", "":
				// Deliberately short first page with total_count > len: the
				// client must keep paging until offset >= total_count.
				_ = json.NewEncoder(w).Encode(map[string]any{
					"data": []any{row("user_a", "org:admin", true), row("user_ghost", "org:member", false)}, "total_count": 3,
				})
			default:
				_ = json.NewEncoder(w).Encode(map[string]any{
					"data": []any{row("user_b", "org:member", true)}, "total_count": 3,
				})
			}
		}
		got, err := c.OrgMembers(ctx)
		if err != nil {
			t.Fatalf("OrgMembers: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("members = %v, want user_a and user_b", got)
		}
		if a := got["user_a"]; !a.IsMember || a.Role != "org:admin" || a.Email != "user_a@beknown.work" || a.LastName != "" {
			t.Errorf("user_a = %+v", a)
		}
		if b := got["user_b"]; !b.IsMember || b.Role != "org:member" {
			t.Errorf("user_b = %+v", b)
		}
	})

	t.Run("api 500", func(t *testing.T) {
		f.members = func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusBadGateway) }
		if _, err := c.OrgMembers(ctx); !errors.Is(err, ErrUnavailable) {
			t.Errorf("502: err = %v, want ErrUnavailable", err)
		}
	})

	t.Run("malformed body", func(t *testing.T) {
		f.members = func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"data": "nope"`)) }
		if _, err := c.OrgMembers(ctx); !errors.Is(err, ErrUnavailable) {
			t.Errorf("bad json: err = %v, want ErrUnavailable", err)
		}
	})
}
