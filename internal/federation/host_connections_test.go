package federation

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
)

func hostProof(t *testing.T, s *Service, key ed25519.PrivateKey, env, subject, request, action string) string {
	t.Helper()
	c := HostClaims{RegisteredClaims: jwt.RegisteredClaims{Issuer: env, Subject: subject, Audience: jwt.ClaimStrings{s.InstanceID}, ID: uuid.NewString(), IssuedAt: jwt.NewNumericDate(s.now()), ExpiresAt: jwt.NewNumericDate(s.now().Add(time.Minute))}, Action: action, RequestID: request, PublicKey: base64.StdEncoding.EncodeToString(key.Public().(ed25519.PublicKey)), HostName: "Local macOS", Platform: "darwin"}
	raw, e := jwt.NewWithClaims(jwt.SigningMethodEdDSA, c).SignedString(key)
	if e != nil {
		t.Fatal(e)
	}
	return raw
}
func TestHostConsentOwnershipRecoveryAndAudit(t *testing.T) {
	s, u, key := federationFixture(t)
	ctx := t.Context()
	request := uuid.NewString()
	begin := hostProof(t, s, key, "mac", u.ClerkUserID, request, "begin")
	h, e := s.BeginHost(ctx, begin)
	if e != nil {
		t.Fatal(e)
	}
	var count int
	s.db.QueryRow(`SELECT count(*) FROM host_connections`).Scan(&count)
	if count != 0 {
		t.Fatal("agent created before consent")
	}
	if _, e = s.BeginHost(ctx, begin); !errors.Is(e, ErrUnauthorized) {
		t.Fatal("proof replay", e)
	}
	retry, e := s.BeginHost(ctx, hostProof(t, s, key, "mac", u.ClerkUserID, request, "begin"))
	if e != nil || retry.AuthorizationRef != h.AuthorizationRef {
		t.Fatal("begin lost response", e)
	}
	pending, e := s.PollHost(ctx, hostProof(t, s, key, "mac", u.ClerkUserID, request, "poll"), false)
	if e != nil || pending.Status != "pending" || pending.Credential != nil {
		t.Fatal("premature credential", e)
	}
	other, e := s.identity.UpsertClerkUser(ctx, identity.ClerkProfile{ClerkUserID: "user-other", Email: "other@example.test"}, "")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.DecideHost(ctx, h.AuthorizationRef, other.ID, true); !errors.Is(e, ErrUnauthorized) {
		t.Fatal("wrong browser owner", e)
	}
	if _, e = s.DecideHost(ctx, h.AuthorizationRef, u.ID, true); e != nil {
		t.Fatal("consent", e)
	}
	c, e := s.PollHost(ctx, hostProof(t, s, key, "mac", u.ClerkUserID, request, "poll"), false)
	if e != nil || c.Credential == nil || c.UserID != u.ID {
		t.Fatal("claim", e)
	}
	recovered, e := s.PollHost(ctx, hostProof(t, s, key, "mac", u.ClerkUserID, request, "poll"), false)
	if e != nil || recovered.Token != c.Token {
		t.Fatal("lost response", e)
	}
	a, e := s.VerifyAgent(ctx, c.Token)
	if e != nil || a.Owner != u.ID || !strings.Contains(a.Name, "Local macOS") {
		t.Fatal("owner/name", e)
	}
	if !s.LocalEnvironment(ctx, a.ID, "mac") || s.LocalEnvironment(ctx, a.ID, "other") {
		t.Fatal("callback environment")
	}
	var claimed int
	s.db.QueryRow(`SELECT count(*) FROM audit_events WHERE event_type='host.credential_claimed' AND owner_user_id=? AND agent_id=?`, u.ID, a.ID).Scan(&claimed)
	if claimed != 1 {
		t.Fatal("claim audit duplicate", claimed)
	}
	hosts, e := s.ListHosts(ctx, u.ID, false)
	if e != nil || len(hosts) != 1 || hosts[0].OwnerID != u.ID {
		t.Fatal("host list", e)
	}
	if e = s.RevokeOwnedHost(ctx, other.ID, a.ID); !errors.Is(e, ErrUnauthorized) {
		t.Fatal("wrong owner revoke", e)
	}
	code, _, e := s.HostHandoff(ctx, c.Token)
	if e != nil {
		t.Fatal(e)
	}
	if got, e := s.ConsumeHostHandoff(ctx, code); e != nil || got != u.ID {
		t.Fatal("handoff", e)
	}
	if _, e = s.ConsumeHostHandoff(ctx, code); e == nil {
		t.Fatal("handoff replay")
	}
	if e = s.RevokeHost(ctx, c.Token); e != nil {
		t.Fatal("revoke", e)
	}
	if _, e = s.VerifyAgent(ctx, c.Token); e == nil {
		t.Fatal("revoked usable")
	}
	if _, e = s.RenewHost(ctx, c.Token, c.Version); e == nil {
		t.Fatal("renew revived")
	}
	request2 := uuid.NewString()
	h2, e := s.BeginHost(ctx, hostProof(t, s, key, "mac", u.ClerkUserID, request2, "begin"))
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.DecideHost(ctx, h2.AuthorizationRef, u.ID, true); e != nil {
		t.Fatal(e)
	}
	newC, e := s.PollHost(ctx, hostProof(t, s, key, "mac", u.ClerkUserID, request2, "poll"), false)
	if e != nil || newC.AgentID != c.AgentID || newC.Generation != c.Generation+1 || newC.Token == c.Token {
		t.Fatal("stable reconnect", e)
	}
	if _, e = s.PollHost(ctx, hostProof(t, s, key, "mac", u.ClerkUserID, request, "poll"), false); !errors.Is(e, ErrRevoked) {
		t.Fatal("old consent recovered replacement", e)
	}
	if _, e = s.PollHost(ctx, hostProof(t, s, key, "mac", u.ClerkUserID, request, "cancel"), true); e != nil {
		t.Fatal("cancel old lifecycle", e)
	}
	if _, e = s.VerifyAgent(ctx, newC.Token); e != nil {
		t.Fatal("old cancel revoked new lifecycle", e)
	}
}
func TestHostConcurrentConsentAndCancellation(t *testing.T) {
	s, u, key := federationFixture(t)
	request := uuid.NewString()
	h, e := s.BeginHost(t.Context(), hostProof(t, s, key, "mac", "local-user", request, "begin"))
	if e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); _, e := s.DecideHost(t.Context(), h.AuthorizationRef, u.ID, true); errs <- e }()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	var n int
	s.db.QueryRow(`SELECT count(*) FROM host_connections`).Scan(&n)
	if n != 1 {
		t.Fatal("duplicate host", n)
	}
	s.db.QueryRow(`SELECT count(*) FROM audit_events WHERE event_type='host.authorization_approved'`).Scan(&n)
	if n != 1 {
		t.Fatal("duplicate decision", n)
	}
	out, e := s.PollHost(t.Context(), hostProof(t, s, key, "mac", "local-user", request, "cancel"), true)
	if e != nil || out.Status != "cancelled" || out.Credential != nil {
		t.Fatal("cancel approved", e)
	}
	var status string
	s.db.QueryRow(`SELECT status FROM host_connections`).Scan(&status)
	if status != "revoked" {
		t.Fatal("orphan active")
	}
}
func TestHostProofBindingsAndDeadlines(t *testing.T) {
	s, u, key := federationFixture(t)
	ctx := t.Context()
	request := uuid.NewString()
	h, e := s.BeginHost(ctx, hostProof(t, s, key, "mac", u.ClerkUserID, request, "begin"))
	if e != nil {
		t.Fatal(e)
	}
	_, rogue, _ := ed25519.GenerateKey(rand.Reader)
	for _, proof := range []string{hostProof(t, s, rogue, "mac", u.ClerkUserID, request, "poll"), hostProof(t, s, key, "other", u.ClerkUserID, request, "poll"), hostProof(t, s, key, "mac", "other", request, "poll"), hostProof(t, s, key, "mac", u.ClerkUserID, request, "cancel")} {
		if _, e = s.PollHost(ctx, proof, false); !errors.Is(e, ErrUnauthorized) {
			t.Fatal("invalid proof accepted", e)
		}
	}
	s.db.Exec(`UPDATE host_connection_requests SET expires_at=0 WHERE request_id=?`, request)
	if _, e = s.DecideHost(ctx, h.AuthorizationRef, u.ID, true); !errors.Is(e, ErrConflict) {
		t.Fatal("expired consent", e)
	}
	out, e := s.PollHost(ctx, hostProof(t, s, key, "mac", u.ClerkUserID, request, "poll"), false)
	if e != nil || out.Status != "expired" || out.Credential != nil {
		t.Fatal("expired poll", e)
	}
	req2 := uuid.NewString()
	h, e = s.BeginHost(ctx, hostProof(t, s, key, "mac2", u.ClerkUserID, req2, "begin"))
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.DecideHost(ctx, h.AuthorizationRef, u.ID, false); e != nil {
		t.Fatal(e)
	}
	out, e = s.PollHost(ctx, hostProof(t, s, key, "mac2", u.ClerkUserID, req2, "poll"), false)
	if e != nil || out.Status != "rejected" || out.Credential != nil {
		t.Fatal("reject", e)
	}
}
func TestHostRenewDisableAndIsolation(t *testing.T) {
	s, u, key := federationFixture(t)
	ctx := t.Context()
	connect := func(env, profile string, owner *identity.User) *HostResult {
		request := uuid.NewString()
		h, e := s.BeginHost(ctx, hostProof(t, s, key, env, profile, request, "begin"))
		if e != nil {
			t.Fatal(e)
		}
		if _, e = s.DecideHost(ctx, h.AuthorizationRef, owner.ID, true); e != nil {
			t.Fatal(e)
		}
		c, e := s.PollHost(ctx, hostProof(t, s, key, env, profile, request, "poll"), false)
		if e != nil {
			t.Fatal(e)
		}
		return c
	}
	a := connect("mac", "local-user", u)
	b := connect("mini", "local-user", u)
	other, e := s.identity.UpsertClerkUser(ctx, identity.ClerkProfile{ClerkUserID: "user-b", Email: "b@example.test"}, "")
	if e != nil {
		t.Fatal(e)
	}
	c := connect("mini", other.ClerkUserID, other)
	if a.AgentID == b.AgentID || b.AgentID == c.AgentID {
		t.Fatal("identity collision")
	}
	s.db.Exec(`UPDATE host_connections SET expires_at=0 WHERE agent_id=?`, a.AgentID)
	renewed, e := s.RenewHost(ctx, a.Token, a.Version)
	if e != nil || renewed.Token != a.Token || renewed.Version != a.Version+1 {
		t.Fatal("renew recovery", e)
	}
	_, rogue, _ := ed25519.GenerateKey(rand.Reader)
	if _, e = s.BeginHost(ctx, hostProof(t, s, rogue, "mac", "local-user", uuid.NewString(), "begin")); !errors.Is(e, ErrConflict) {
		t.Fatal("key takeover", e)
	}
	if e = s.identity.SetAgentDisabled(ctx, u.ID, a.AgentID, true); e != nil {
		t.Fatal(e)
	}
	if e = s.identity.SetAgentDisabled(ctx, u.ID, a.AgentID, false); e != nil {
		t.Fatal(e)
	}
	if _, e = s.VerifyAgent(ctx, a.Token); e == nil {
		t.Fatal("agent restore revived permission")
	}
	if e = s.identity.SetStatus(ctx, other.ID, identity.StatusBlocked, "test"); e != nil {
		t.Fatal(e)
	}
	if e = s.identity.SetStatus(ctx, other.ID, identity.StatusActive, ""); e != nil {
		t.Fatal(e)
	}
	if _, e = s.VerifyAgent(ctx, c.Token); e == nil {
		t.Fatal("account restore revived permission")
	}
	if _, e = s.VerifyAgent(ctx, b.Token); e != nil {
		t.Fatal("other host revoked", e)
	}
}

func TestHostProofAudienceExpiryContentAndAtomicAudit(t *testing.T) {
	s, u, key := federationFixture(t)
	ctx := t.Context()
	request := uuid.NewString()
	good := hostProof(t, s, key, "mac", u.ClerkUserID, request, "begin")
	c := new(HostClaims)
	jwt.NewParser().ParseUnverified(good, c)
	c.Audience = jwt.ClaimStrings{"wrong-instance"}
	bad, _ := jwt.NewWithClaims(jwt.SigningMethodEdDSA, c).SignedString(key)
	if _, e := s.BeginHost(ctx, bad); !errors.Is(e, ErrUnauthorized) {
		t.Fatal("wrong instance accepted", e)
	}
	c.Audience = jwt.ClaimStrings{s.InstanceID}
	c.ExpiresAt = jwt.NewNumericDate(s.now().Add(-time.Second))
	bad, _ = jwt.NewWithClaims(jwt.SigningMethodEdDSA, c).SignedString(key)
	if _, e := s.BeginHost(ctx, bad); !errors.Is(e, ErrUnauthorized) {
		t.Fatal("expired proof accepted", e)
	}
	h, e := s.BeginHost(ctx, good)
	if e != nil {
		t.Fatal(e)
	}
	c.ExpiresAt = jwt.NewNumericDate(s.now().Add(time.Minute))
	c.ID = uuid.NewString()
	c.HostName = "Changed Mac"
	bad, _ = jwt.NewWithClaims(jwt.SigningMethodEdDSA, c).SignedString(key)
	if _, e = s.BeginHost(ctx, bad); !errors.Is(e, ErrConflict) {
		t.Fatal("changed content reused request", e)
	}
	if _, e = s.db.Exec(`CREATE TRIGGER host_audit_failure BEFORE INSERT ON audit_events WHEN NEW.event_type='host.authorization_approved' BEGIN SELECT RAISE(ABORT,'synthetic audit failure'); END`); e != nil {
		t.Fatal(e)
	}
	if _, e = s.DecideHost(ctx, h.AuthorizationRef, u.ID, true); e == nil {
		t.Fatal("audit failure committed permission")
	}
	var count int
	s.db.QueryRow(`SELECT count(*) FROM host_connections`).Scan(&count)
	if count != 0 {
		t.Fatal("orphan connection after audit failure")
	}
	var status string
	s.db.QueryRow(`SELECT status FROM host_connection_requests WHERE request_id=?`, request).Scan(&status)
	if status != "pending" {
		t.Fatal("decision escaped transaction")
	}
	s.db.Exec(`DROP TRIGGER host_audit_failure`)
	if _, e = s.DecideHost(ctx, h.AuthorizationRef, u.ID, true); e != nil {
		t.Fatal("retry after audit failure", e)
	}
}

func TestHostCancelMissingRequestAndDeletedAgentRecovery(t *testing.T) {
	s, u, key := federationFixture(t)
	ctx := t.Context()
	request := uuid.NewString()
	out, e := s.PollHost(ctx, hostProof(t, s, key, "offline", "local-user", request, "cancel"), true)
	if e != nil || out.Status != "cancelled" {
		t.Fatal("missing cancel", e)
	}
	h, e := s.BeginHost(ctx, hostProof(t, s, key, "offline", "local-user", request, "begin"))
	if e != nil || h.Status != "cancelled" {
		t.Fatal("delayed begin ignored tombstone", e)
	}
	var n int
	s.db.QueryRow(`SELECT count(*) FROM host_connections`).Scan(&n)
	if n != 0 {
		t.Fatal("delayed begin created agent")
	}
	request = uuid.NewString()
	h, e = s.BeginHost(ctx, hostProof(t, s, key, "offline", "local-user", request, "begin"))
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.DecideHost(ctx, h.AuthorizationRef, u.ID, true); e != nil {
		t.Fatal(e)
	}
	out, e = s.PollHost(ctx, hostProof(t, s, key, "offline", "local-user", request, "poll"), false)
	if e != nil {
		t.Fatal(e)
	}
	old := out.Credential
	if e = s.identity.DeleteAgent(ctx, u.ID, out.AgentID); e != nil {
		t.Fatal(e)
	}
	if e = s.RevokeHost(ctx, old.Token); e != nil {
		t.Fatal("deleted matching credential cleanup", e)
	}
	if e = s.RevokeHost(ctx, old.AgentID+".wrong"); !errors.Is(e, ErrUnauthorized) {
		t.Fatal("guessed cleanup", e)
	}
	request = uuid.NewString()
	h, e = s.BeginHost(ctx, hostProof(t, s, key, "offline", "local-user", request, "begin"))
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.DecideHost(ctx, h.AuthorizationRef, u.ID, true); e != nil {
		t.Fatal("reconsent deleted agent", e)
	}
	out, e = s.PollHost(ctx, hostProof(t, s, key, "offline", "local-user", request, "poll"), false)
	if e != nil || out.AgentID != old.AgentID || out.Token == old.Token || out.Generation != old.Generation+1 {
		t.Fatal("deleted agent not recoverable", e)
	}
	if e = s.RevokeHost(ctx, old.Token); !errors.Is(e, ErrUnauthorized) {
		t.Fatal("old cleanup revoked new lifecycle", e)
	}
	if e = s.identity.SetAgentDisabled(ctx, u.ID, out.AgentID, true); e != nil {
		t.Fatal(e)
	}
	if e = s.RevokeHost(ctx, out.Token); e != nil {
		t.Fatal("disabled matching credential cleanup", e)
	}
}
