package federation

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/sealbox"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

func federationFixture(t *testing.T) (*Service, *identity.User, ed25519.PrivateKey) {
	t.Helper()
	db, e := store.Open(filepath.Join(t.TempDir(), "db.sqlite"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { db.Close() })
	id := identity.New(db)
	u, e := id.UpsertClerkUser(t.Context(), identity.ClerkProfile{ClerkUserID: "user-admin", Email: "admin@example.test"}, "")
	if e != nil {
		t.Fatal(e)
	}
	_, e = db.Exec(`UPDATE users SET role='admin' WHERE id=?`, u.ID)
	if e != nil {
		t.Fatal(e)
	}
	u.Role = identity.RoleAdmin
	cipher, _ := sealbox.NewCipher(make([]byte, 32))
	s, e := New(t.Context(), db, id, cipher)
	if e != nil {
		t.Fatal(e)
	}
	s.Membership = func(ctx context.Context, subject string) (Profile, error) {
		if subject == "not-member" {
			return Profile{}, ErrUnauthorized
		}
		return Profile{Email: subject + "@example.test", Name: subject}, nil
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	for _, issuer := range []string{"stage", "stable"} {
		if e = s.Register(t.Context(), Trust{Issuer: issuer, PublicKey: base64.StdEncoding.EncodeToString(pub), Kid: "key-1", Origin: "https://" + issuer + ".example.test"}, u.ID); e != nil {
			t.Fatal(e)
		}
	}
	return s, u, priv
}
func assertion(t *testing.T, s *Service, key ed25519.PrivateKey, issuer, subject string) string {
	t.Helper()
	now := s.now()
	token := jwt.NewWithClaims(jwt.SigningMethodEdDSA, Claims{jwt.RegisteredClaims{Issuer: issuer, Subject: subject, Audience: jwt.ClaimStrings{s.InstanceID}, ID: uuid.NewString(), IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(time.Minute))}})
	token.Header["kid"] = "key-1"
	raw, e := token.SignedString(key)
	if e != nil {
		t.Fatal(e)
	}
	return raw
}
func principal(t *testing.T, s *Service, key ed25519.PrivateKey, issuer, subject string) *Principal {
	t.Helper()
	p, e := s.Verify(t.Context(), assertion(t, s, key, issuer, subject))
	if e != nil {
		t.Fatal(e)
	}
	return p
}
func TestFederationIsolationReplayAndLostResponse(t *testing.T) {
	s, _, key := federationFixture(t)
	ctx := t.Context()
	raw := assertion(t, s, key, "stage", "alice")
	p, e := s.Verify(ctx, raw)
	if e != nil {
		t.Fatal(e)
	}
	if p.User.Role != identity.RoleMember {
		t.Fatal("provision elevated role")
	}
	if _, e = s.Verify(ctx, raw); !errors.Is(e, ErrUnauthorized) {
		t.Fatal("assertion replay", e)
	}
	c, e := s.Connect(ctx, p, 0)
	if e != nil {
		t.Fatal(e)
	}
	again, e := s.Connect(ctx, p, 0)
	if e != nil || again.Token != c.Token || again.Version != c.Version {
		t.Fatal("lost response minted replacement", e)
	}
	stable, e := s.Connect(ctx, principal(t, s, key, "stable", "alice"), 0)
	if e != nil || stable.AgentID == c.AgentID || stable.Token == c.Token {
		t.Fatal("environment collision", e)
	}
	bob, e := s.Connect(ctx, principal(t, s, key, "stage", "bob"), 0)
	if e != nil || bob.AgentID == c.AgentID {
		t.Fatal("user collision", e)
	}
	if _, e = s.Verify(ctx, assertion(t, s, key, "stage", "not-member")); !errors.Is(e, ErrUnauthorized) {
		t.Fatal("unverified member provisioned", e)
	}
	if _, e = s.Connect(ctx, p, c.Version+1); !errors.Is(e, ErrConflict) {
		t.Fatal("future version accepted", e)
	}
	var count int
	s.db.QueryRow(`SELECT count(*) FROM federation_connections`).Scan(&count)
	if count != 3 {
		t.Fatal(count)
	}
}
func TestFederationRenewalCASAndRevocation(t *testing.T) {
	s, admin, key := federationFixture(t)
	ctx := t.Context()
	p := principal(t, s, key, "stage", "alice")
	c, e := s.Connect(ctx, p, 0)
	if e != nil {
		t.Fatal(e)
	}
	s.now = func() time.Time { return time.Now().Add(29*24*time.Hour + time.Hour) }
	var wg sync.WaitGroup
	tokens := make(chan string, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			next, e := s.Connect(ctx, p, c.Version)
			if e != nil {
				t.Error(e)
				return
			}
			tokens <- next.Token
		}()
	}
	wg.Wait()
	close(tokens)
	var renewed string
	for tok := range tokens {
		if renewed != "" && renewed != tok {
			t.Fatal("renewal race issued multiple tokens")
		}
		renewed = tok
	}
	var version int
	s.db.QueryRow(`SELECT version FROM federation_connections WHERE agent_id=?`, c.AgentID).Scan(&version)
	if version != 2 {
		t.Fatal(version)
	}
	s.now = time.Now
	if e = s.Revoke(ctx, p, "user"); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Connect(ctx, p, 0); !errors.Is(e, ErrRevoked) {
		t.Fatal("revocation recreated access", e)
	}
	ap := &Principal{Issuer: "stage", Subject: admin.ClerkUserID, Origin: "https://stage.example.test", User: admin}
	if e = s.Revoke(ctx, ap, "environment"); e != nil {
		t.Fatal(e)
	}
	var trust Trust
	s.db.QueryRow(`SELECT issuer,public_key,kid,origin FROM federation_issuers WHERE issuer='stage'`).Scan(&trust.Issuer, &trust.PublicKey, &trust.Kid, &trust.Origin)
	if e = s.Register(ctx, trust, admin.ID); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Connect(ctx, p, 0); !errors.Is(e, ErrRevoked) {
		t.Fatal("admin restore revived user revocation", e)
	}
}
func TestFederationEnvironmentRestoreAndHandoff(t *testing.T) {
	s, admin, key := federationFixture(t)
	ctx := t.Context()
	p := principal(t, s, key, "stage", "alice")
	c, e := s.Connect(ctx, p, 0)
	if e != nil {
		t.Fatal(e)
	}
	if _, _, e = s.Handoff(ctx, p, "https://evil.example/"); !errors.Is(e, ErrUnauthorized) {
		t.Fatal("handoff destination unbound")
	}
	code, _, e := s.Handoff(ctx, p, p.Origin+"/settings")
	if e != nil {
		t.Fatal(e)
	}
	user, e := s.ConsumeHandoff(ctx, code)
	if e != nil || user != p.User.ID {
		t.Fatal(e)
	}
	if _, e = s.ConsumeHandoff(ctx, code); !errors.Is(e, ErrUnauthorized) {
		t.Fatal("handoff reused")
	}
	ap := &Principal{Issuer: "stage", Subject: admin.ClerkUserID, Origin: p.Origin, User: admin}
	if e = s.Revoke(ctx, ap, "environment"); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Verify(ctx, assertion(t, s, key, "stage", "user-admin")); !errors.Is(e, ErrTrustRevoked) {
		t.Fatal("removed trust accepted connect")
	}
	if _, e = s.VerifyRevocation(ctx, assertion(t, s, key, "stage", "user-admin")); e != nil {
		t.Fatal("cleanup retry blocked", e)
	}
	var trust Trust
	s.db.QueryRow(`SELECT issuer,public_key,kid,origin FROM federation_issuers WHERE issuer='stage'`).Scan(&trust.Issuer, &trust.PublicKey, &trust.Kid, &trust.Origin)
	if e = s.Register(ctx, trust, admin.ID); e != nil {
		t.Fatal(e)
	}
	restored, e := s.Connect(ctx, p, 0)
	if e != nil || restored.AgentID != c.AgentID || restored.Token == c.Token {
		t.Fatal("restore identity or permission", e)
	}
	if _, e = s.VerifyAgent(ctx, c.Token); e == nil {
		t.Fatal("old credential restored")
	}
}
