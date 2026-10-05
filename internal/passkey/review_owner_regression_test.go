package passkey

import (
	"context"
	"errors"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/inbox"
)

func TestReviewPasskeyGateAndAssertionRemainOwnerScoped(t *testing.T) {
	s, _ := newSvc(t)
	ctx := context.Background()
	auth := newSoftAuth(testOrigin)
	register(t, s, auth)
	on, err := s.CheckEnabledFor(ctx, owner.ID)
	if err != nil || !on {
		t.Fatalf("owner gate: %v %v", on, err)
	}
	on, err = s.CheckEnabledFor(ctx, "another-member")
	if err != nil || on {
		t.Fatalf("member gate inherited another key: %v %v", on, err)
	}
	id, opts, err := s.BeginAssertion(ctx, owner, "decide:review", testOrigin, "yard.example.com")
	if err != nil {
		t.Fatal(err)
	}
	assertion := &inbox.PasskeyAssertion{SessionID: id, Response: get(t, auth, opts, owner.ID)}
	if _, err = s.VerifyCredentialFor(ctx, "another-member", "review", assertion); !errors.Is(err, ErrPurpose) {
		t.Fatalf("another owner's assertion accepted: %v", err)
	}
	id, opts, err = s.BeginAssertion(ctx, owner, "decide:review", testOrigin, "yard.example.com")
	if err != nil {
		t.Fatal(err)
	}
	assertion = &inbox.PasskeyAssertion{SessionID: id, Response: get(t, auth, opts, owner.ID)}
	if _, err = s.VerifyCredentialFor(ctx, owner.ID, "review", assertion); err != nil {
		t.Fatalf("owner assertion: %v", err)
	}
}
