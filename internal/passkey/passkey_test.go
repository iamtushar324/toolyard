package passkey

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-webauthn/webauthn/protocol"

	"github.com/tusharbhardwaj/toolyard/internal/actor"
	"github.com/tusharbhardwaj/toolyard/internal/inbox"
	"github.com/tusharbhardwaj/toolyard/internal/passkey/passkeytest"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

type softAuth = passkeytest.Authenticator

func newSoftAuth(origin string) *softAuth { return passkeytest.New(origin) }

func create(t *testing.T, a *softAuth, opts *protocol.CredentialCreation) []byte {
	t.Helper()
	b, err := a.Create(opts)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func get(t *testing.T, a *softAuth, opts *protocol.CredentialAssertion, user string) []byte {
	t.Helper()
	b, err := a.Get(opts, user)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

const testOrigin = "https://yard.example.com"

func newSvc(t *testing.T) (*Service, *store.DB) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "pk.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return New(db), db
}

var owner = User{ID: "usr_owner", Name: "owner"}

func register(t *testing.T, s *Service, a *softAuth) *Passkey {
	t.Helper()
	ctx := context.Background()
	cid, opts, err := s.BeginRegistration(ctx, owner, testOrigin, "yard.example.com")
	if err != nil {
		t.Fatal(err)
	}
	pk, err := s.FinishRegistration(ctx, owner, cid, "iPhone", create(t, a, opts))
	if err != nil {
		t.Fatalf("finish registration: %v", err)
	}
	return pk
}

func TestRelyingParty(t *testing.T) {
	for _, c := range []struct {
		origin, host string
		ok           bool
	}{
		{"https://yard.example.com", "yard.example.com", true},
		{"https://yard.example.com:8443", "yard.example.com:8443", true},
		{"http://localhost:8787", "localhost:8787", true},
		{"http://yard.example.com", "yard.example.com", false},
		{"https://evil.example.com", "yard.example.com", false},
		{"", "yard.example.com", false},
	} {
		_, _, err := RelyingParty(c.origin, c.host)
		if (err == nil) != c.ok {
			t.Errorf("%s on %s: err=%v", c.origin, c.host, err)
		}
	}
	// Behind a proxy that rewrites Host, X-Forwarded-Host (the second host) matches.
	if _, _, err := RelyingParty("https://yard.example.com", "127.0.0.1:8787", "yard.example.com"); err != nil {
		t.Errorf("forwarded host refused: %v", err)
	}
	if _, _, err := RelyingParty("https://evil.example.com", "127.0.0.1:8787", "yard.example.com"); err == nil {
		t.Error("an origin matching neither host was accepted")
	}
}

func TestRegisterAssertAndBinding(t *testing.T) {
	s, _ := newSvc(t)
	ctx := context.Background()
	if s.Enabled(ctx) {
		t.Fatal("enabled with no passkeys")
	}
	a := newSoftAuth(testOrigin)
	pk := register(t, s, a)
	if !s.Enabled(ctx) || pk.Name != "iPhone" {
		t.Fatalf("after register: %+v", pk)
	}
	// Registering the same authenticator again is excluded by the browser;
	// the server refuses a duplicate ID too.
	if cid, opts, err := s.BeginRegistration(ctx, owner, testOrigin, "yard.example.com"); err == nil {
		if len(opts.Response.CredentialExcludeList) != 1 {
			t.Fatalf("exclude list: %+v", opts.Response.CredentialExcludeList)
		}
		if _, err := s.FinishRegistration(ctx, owner, cid, "again", create(t, a, opts)); err == nil {
			t.Fatal("duplicate credential stored")
		}
	}

	cid, opts, err := s.BeginAssertion(ctx, owner, "decide:aaa", testOrigin, "yard.example.com")
	if err != nil {
		t.Fatal(err)
	}
	resp := get(t, a, opts, owner.ID)
	if err := s.Verify(ctx, "bbb", &inbox.PasskeyAssertion{SessionID: cid, Response: resp}); !errors.Is(err, ErrPurpose) {
		t.Fatalf("assertion for another decision accepted: %v", err)
	}
	// The ceremony is single-use, even after a failed attempt.
	if err := s.Verify(ctx, "aaa", &inbox.PasskeyAssertion{SessionID: cid, Response: resp}); !errors.Is(err, ErrNoCeremony) {
		t.Fatalf("ceremony reused: %v", err)
	}
	cid, opts, _ = s.BeginAssertion(ctx, owner, "decide:aaa", testOrigin, "yard.example.com")
	if err := s.Verify(ctx, "aaa", &inbox.PasskeyAssertion{SessionID: cid, Response: get(t, a, opts, owner.ID)}); err != nil {
		t.Fatalf("valid assertion: %v", err)
	}
	list, _ := s.List(ctx, owner.ID)
	if len(list) != 1 || list[0].LastUsedAt == 0 {
		t.Fatalf("last used not recorded: %+v", list)
	}

	// A phishing origin can't produce a usable assertion.
	evil := *a
	evil.Origin = "https://evil.example.com"
	cid, opts, _ = s.BeginAssertion(ctx, owner, "decide:aaa", testOrigin, "yard.example.com")
	if err := s.Verify(ctx, "aaa", &inbox.PasskeyAssertion{SessionID: cid, Response: get(t, &evil, opts, owner.ID)}); err == nil {
		t.Fatal("assertion from another origin accepted")
	}
	// A different key for the same credential ID fails the signature.
	forged := newSoftAuth(testOrigin)
	forged.CredID = a.CredID
	cid, opts, _ = s.BeginAssertion(ctx, owner, "decide:aaa", testOrigin, "yard.example.com")
	if err := s.Verify(ctx, "aaa", &inbox.PasskeyAssertion{SessionID: cid, Response: get(t, forged, opts, owner.ID)}); err == nil {
		t.Fatal("forged signature accepted")
	}
	// Expired ceremony.
	base := time.Now()
	s.SetClock(func() time.Time { return base })
	cid, opts, _ = s.BeginAssertion(ctx, owner, "decide:aaa", testOrigin, "yard.example.com")
	s.SetClock(func() time.Time { return base.Add(CeremonyTTL + time.Second) })
	if err := s.Verify(ctx, "aaa", &inbox.PasskeyAssertion{SessionID: cid, Response: get(t, a, opts, owner.ID)}); !errors.Is(err, ErrNoCeremony) {
		t.Fatalf("expired ceremony: %v", err)
	}
	s.SetClock(time.Now)

	if err := s.Remove(ctx, owner.ID, pk.ID); err != nil {
		t.Fatal(err)
	}
	if s.Enabled(ctx) {
		t.Fatal("still enabled after removal")
	}
}

func TestRegistrationNeedsUserVerification(t *testing.T) {
	s, _ := newSvc(t)
	ctx := context.Background()
	a := newSoftAuth(testOrigin)
	a.NoUV = true
	cid, opts, _ := s.BeginRegistration(ctx, owner, testOrigin, "yard.example.com")
	if _, err := s.FinishRegistration(ctx, owner, cid, "key", create(t, a, opts)); err == nil {
		t.Fatal("registration without user verification accepted")
	}
}

type catalog map[string]string

func (c catalog) Access(_ context.Context, _ string, tool string, _ map[string]any) (string, string) {
	return c[tool], "up"
}

// The inbox refuses to allow high-risk tools without a passkey once one is
// registered, and accepts a confirmation only for the exact decision.
func TestInboxGate(t *testing.T) {
	s, db := newSvc(t)
	ctx := context.Background()
	svc, err := inbox.New(ctx, inbox.Options{DB: db, Passkeys: s,
		Catalog: catalog{"deploy.run": inbox.AccessRestricted, "github.comment": inbox.AccessRestricted}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Flush)
	submit := func() *inbox.Request {
		res, err := svc.Submit(ctx, "ag_1", &inbox.Submission{Kind: inbox.KindAccess, Title: "Deploy api", Summary: "s",
			Message: "I'd like to deploy the api to production.", Audio: inbox.Audio{Script: "Deploy?"}, Urgency: inbox.UrgencySoon,
			Facts: &inbox.Facts{WhyNow: "w", IfItGoesWrong: "g", Undo: "u"},
			Tools: []inbox.SubmissionTool{
				{Tool: "deploy.run", Required: false, Summary: "Deploy to production.", Params: map[string]any{"env": "prod"}},
				{Tool: "github.comment", Required: false, Summary: "Comment on the PR.", Params: map[string]any{"pr": 12}},
			}})
		if err != nil || !res.OK {
			t.Fatalf("submit: %v %+v", err, res)
		}
		svc.Flush()
		r, _ := svc.Get(ctx, res.RequestID)
		return r
	}

	// No passkey registered: the two-tap confirm in the dashboard is enough.
	r := submit()
	if !inbox.HighRisk(r) {
		t.Fatal("deploy to prod should be high risk")
	}
	if _, err := svc.Decide(ctx, r.ID, inbox.Decision{Action: "approve", Allow: []bool{true, true}}); err != nil {
		t.Fatalf("approve without passkeys registered: %v", err)
	}

	a := newSoftAuth(testOrigin)
	pk := register(t, s, a)
	r = submit()
	d := inbox.Decision{Action: "approve", Allow: []bool{true, true}}
	if _, err := svc.Decide(ctx, r.ID, d); !errors.Is(err, inbox.ErrPasskeyRequired) {
		t.Fatalf("high-risk approve without passkey: %v", err)
	}
	// Low-risk tools alone don't need it.
	low := inbox.Decision{Action: "approve", Allow: []bool{false, true}}
	r2 := submit()
	if _, err := svc.Decide(ctx, r2.ID, low); err != nil {
		t.Fatalf("low-risk approve: %v", err)
	}
	// A confirmation made for "allow only the comment" can't allow the deploy.
	cid, opts, _ := s.BeginAssertion(ctx, owner, "decide:"+inbox.DecisionDigest(r.ID, low), testOrigin, "yard.example.com")
	d.Passkey = &inbox.PasskeyAssertion{SessionID: cid, Response: get(t, a, opts, owner.ID)}
	if _, err := svc.Decide(ctx, r.ID, d); !errors.Is(err, inbox.ErrPasskeyFailed) {
		t.Fatalf("confirmation for another decision: %v", err)
	}
	cid, opts, _ = s.BeginAssertion(ctx, owner, "decide:"+inbox.DecisionDigest(r.ID, inbox.Decision{Action: "approve", Allow: []bool{true, true}}), testOrigin, "yard.example.com")
	d.Passkey = &inbox.PasskeyAssertion{SessionID: cid, Response: get(t, a, opts, owner.ID)}
	got, err := svc.Decide(ctx, r.ID, d)
	if err != nil || got.Status != inbox.StatusApproved {
		t.Fatalf("confirmed approve: %v %+v", err, got)
	}
	// The decision records the passkey that confirmed it.
	if got.DeciderVia != actor.ViaPasskey || got.DeciderRef != pk.ID {
		t.Fatalf("decider via=%q ref=%q, want passkey %s", got.DeciderVia, got.DeciderRef, pk.ID)
	}
	if got.DecidedBy != actor.ViaPasskey+":"+pk.ID {
		t.Fatalf("legacy decided_by = %q", got.DecidedBy)
	}
}

func TestStagePasskeyReadFailureIsClosed(t *testing.T) {
	s, db := newSvc(t)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CheckEnabled(context.Background()); err == nil {
		t.Fatal("database failure treated as passkeys disabled")
	}
	if !s.Enabled(context.Background()) {
		t.Fatal("legacy predicate failed open")
	}
}

func TestPasskeyGateIsUserScoped(t *testing.T) {
	s, _ := newSvc(t)
	a := newSoftAuth(testOrigin)
	register(t, s, a)
	if on, e := s.CheckEnabledFor(t.Context(), owner.ID); e != nil || !on {
		t.Fatal("owner gate disabled", e)
	}
	if on, e := s.CheckEnabledFor(t.Context(), "member_without_key"); e != nil || on {
		t.Fatal("another user's passkey enabled member gate", e)
	}
	id, opts, e := s.BeginAssertion(t.Context(), owner, "decide:test", testOrigin, "yard.example.com")
	if e != nil {
		t.Fatal(e)
	}
	response := get(t, a, opts, owner.ID)
	if _, e = s.VerifyCredentialFor(t.Context(), "another_member", "test", &inbox.PasskeyAssertion{SessionID: id, Response: response}); !errors.Is(e, ErrPurpose) {
		t.Fatal("another owner assertion accepted", e)
	}
}
