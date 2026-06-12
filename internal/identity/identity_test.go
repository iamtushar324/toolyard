package identity

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/store"
)

func newTestIdentity(t *testing.T) (*Service, string) {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	s := New(db)
	u, err := s.CreateUser(context.Background(), "admin", "pw-correct-horse")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return s, u.ID
}

func TestVerifyAgentToken_Basic(t *testing.T) {
	s, owner := newTestIdentity(t)
	ctx := context.Background()
	tok, ag, err := s.CreateAgentWithToken(ctx, owner, "bot")
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	got, err := s.VerifyAgentToken(ctx, tok)
	if err != nil || got.ID != ag.ID {
		t.Fatalf("verify = %v, %v; want agent %s", got, err, ag.ID)
	}
}

func TestRotateGrace(t *testing.T) {
	s, owner := newTestIdentity(t)
	ctx := context.Background()
	oldTok, ag, _ := s.CreateAgentWithToken(ctx, owner, "bot")

	// Rotate with a generous grace: both old and new tokens authenticate.
	newTok, err := s.RotateAgentToken(ctx, owner, ag.ID, 10*time.Minute)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if _, err := s.VerifyAgentToken(ctx, newTok); err != nil {
		t.Errorf("new token rejected after rotate: %v", err)
	}
	if _, err := s.VerifyAgentToken(ctx, oldTok); err != nil {
		t.Errorf("old token rejected inside grace window: %v", err)
	}

	// Rotate again with zero grace: the previous (newTok) dies immediately.
	newer, err := s.RotateAgentToken(ctx, owner, ag.ID, 0)
	if err != nil {
		t.Fatalf("rotate zero-grace: %v", err)
	}
	if _, err := s.VerifyAgentToken(ctx, newer); err != nil {
		t.Errorf("newer token rejected: %v", err)
	}
	if _, err := s.VerifyAgentToken(ctx, newTok); !errors.Is(err, ErrAgentTokenInvalid) {
		t.Errorf("prior token still valid after zero-grace rotate: %v", err)
	}
}

func TestRotateGraceExpired(t *testing.T) {
	s, owner := newTestIdentity(t)
	ctx := context.Background()
	oldTok, ag, _ := s.CreateAgentWithToken(ctx, owner, "bot")

	// A negative-equivalent: rotate with a 1ns grace, then the old token is
	// past its window almost immediately.
	if _, err := s.RotateAgentToken(ctx, owner, ag.ID, time.Nanosecond); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	time.Sleep(2 * time.Millisecond)
	if _, err := s.VerifyAgentToken(ctx, oldTok); !errors.Is(err, ErrAgentTokenInvalid) {
		t.Errorf("old token valid past grace window: %v", err)
	}
}

func TestDisabledRejects(t *testing.T) {
	s, owner := newTestIdentity(t)
	ctx := context.Background()
	tok, ag, _ := s.CreateAgentWithToken(ctx, owner, "bot")

	if err := s.SetAgentDisabled(ctx, owner, ag.ID, true); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, err := s.VerifyAgentToken(ctx, tok); !errors.Is(err, ErrAgentTokenInvalid) {
		t.Errorf("disabled agent's token still valid: %v", err)
	}
	if err := s.SetAgentDisabled(ctx, owner, ag.ID, false); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if _, err := s.VerifyAgentToken(ctx, tok); err != nil {
		t.Errorf("re-enabled agent's token rejected: %v", err)
	}
}
