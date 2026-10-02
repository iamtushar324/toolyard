package identity

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
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

// seedIdentityAgent inserts owner's identity agent the way identitykeys
// does (kind = 'identity') and returns its token.
func seedIdentityAgent(t *testing.T, s *Service, owner string) (string, string) {
	t.Helper()
	id := NewAgentID()
	tok, hash, err := NewAgentToken(id)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO agents(id, name, owner_user, token_hash, kind, created_at) VALUES(?,?,?,?,?,?)`,
		id, "Beknown key", owner, hash, AgentKindIdentity, time.Now().UnixMilli()); err != nil {
		t.Fatalf("insert identity agent: %v", err)
	}
	return tok, id
}

func TestNewAgentToken(t *testing.T) {
	tok, hash, err := NewAgentToken("ag_x")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tok, "ag_x.") || len(tok) <= len("ag_x.") {
		t.Errorf("token shape %q", tok)
	}
	if hash != HashToken(tok) || len(hash) != 64 {
		t.Errorf("hash %q does not match HashToken", hash)
	}
	if _, _, err := NewAgentToken(""); !errors.Is(err, ErrAgentTokenInvalid) {
		t.Errorf("empty id: %v", err)
	}
	if !strings.HasPrefix(NewAgentID(), "ag_") {
		t.Error("NewAgentID prefix")
	}
}

func TestIdentityAgentAuthenticatesAndKind(t *testing.T) {
	s, owner := newTestIdentity(t)
	ctx := context.Background()
	tok, id := seedIdentityAgent(t, s, owner)
	_, plain, _ := s.CreateAgentWithToken(ctx, owner, "bot")

	got, err := s.VerifyAgentToken(ctx, tok)
	if err != nil || got.ID != id || got.Kind != AgentKindIdentity || got.Owner != owner {
		t.Fatalf("verify identity token = %+v, %v", got, err)
	}
	if got, _ := s.VerifyAgentToken(ctx, mustToken(t, s, owner, plain.ID)); got != nil && got.Kind != AgentKindAgent {
		t.Errorf("plain agent kind = %q", got.Kind)
	}
	list, err := s.ListAgents(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]string{}
	for _, a := range list {
		kinds[a.ID] = a.Kind
	}
	if kinds[id] != AgentKindIdentity || kinds[plain.ID] != AgentKindAgent {
		t.Errorf("ListAgents kinds = %v", kinds)
	}
}

// mustToken rotates a plain agent to get a token for it (the create token
// was discarded by the caller).
func mustToken(t *testing.T, s *Service, owner, agentID string) string {
	t.Helper()
	tok, err := s.RotateAgentToken(context.Background(), owner, agentID, 0)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	return tok
}

func TestGenericActionsRefuseIdentityAgent(t *testing.T) {
	s, owner := newTestIdentity(t)
	ctx := context.Background()
	tok, id := seedIdentityAgent(t, s, owner)

	if _, err := s.RotateAgentToken(ctx, owner, id, 0); !errors.Is(err, ErrIdentityAgent) {
		t.Errorf("rotate: %v, want ErrIdentityAgent", err)
	}
	if err := s.SetAgentDisabled(ctx, owner, id, true); !errors.Is(err, ErrIdentityAgent) {
		t.Errorf("disable: %v, want ErrIdentityAgent", err)
	}
	if err := s.SetAgentDisabled(ctx, owner, id, false); !errors.Is(err, ErrIdentityAgent) {
		t.Errorf("enable: %v, want ErrIdentityAgent", err)
	}
	if err := s.DeleteAgent(ctx, owner, id); !errors.Is(err, ErrIdentityAgent) {
		t.Errorf("delete: %v, want ErrIdentityAgent", err)
	}
	// Nothing changed: the key still authenticates.
	if _, err := s.VerifyAgentToken(ctx, tok); err != nil {
		t.Errorf("identity token after refused actions: %v", err)
	}
	// Someone else's agent, or an unknown one, is still "invalid" (no leak).
	if _, err := s.RotateAgentToken(ctx, "u_other", id, 0); !errors.Is(err, ErrAgentTokenInvalid) {
		t.Errorf("rotate as other owner: %v", err)
	}
	if err := s.DeleteAgent(ctx, owner, "ag_missing"); !errors.Is(err, ErrAgentTokenInvalid) {
		t.Errorf("delete missing: %v", err)
	}
}

// VerifyAgentToken names the owner in the same lookup that checks their
// status, so the MCP ingress can attribute a call to a person without a
// second query.
func TestVerifyAgentTokenCarriesOwnerDetails(t *testing.T) {
	s, owner := newTestIdentity(t)
	ctx := context.Background()
	tok, ag, err := s.CreateAgentWithToken(ctx, owner, "bot")
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.VerifyAgentToken(ctx, tok)
	if err != nil {
		t.Fatal(err)
	}
	// A password-only admin has a username and nothing else.
	if got.Owner != owner || got.OwnerUsername != "admin" || got.OwnerEmail != "" || got.OwnerDisplayName != "" {
		t.Fatalf("owner fields = %+v", got)
	}
	if got.OwnerName() != "admin" {
		t.Fatalf("OwnerName without display name = %q", got.OwnerName())
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE users SET email = ?, display_name = ? WHERE id = ?`,
		"ada@beknown.work", "Ada Lovelace", owner); err != nil {
		t.Fatal(err)
	}
	got, err = s.VerifyAgentToken(ctx, tok)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != ag.ID || got.OwnerEmail != "ada@beknown.work" || got.OwnerDisplayName != "Ada Lovelace" || got.OwnerUsername != "admin" {
		t.Fatalf("owner fields = %+v", got)
	}
	if got.OwnerName() != "Ada Lovelace" {
		t.Fatalf("OwnerName = %q", got.OwnerName())
	}
	// An agent whose owner row is gone still authenticates, with no owner
	// details.
	if _, err := s.db.ExecContext(ctx, `UPDATE agents SET owner_user = 'u_gone' WHERE id = ?`, ag.ID); err != nil {
		t.Fatal(err)
	}
	got, err = s.VerifyAgentToken(ctx, tok)
	if err != nil || got.Owner != "u_gone" || got.OwnerEmail != "" || got.OwnerUsername != "" {
		t.Fatalf("orphaned agent = %+v, %v", got, err)
	}
}

func TestGetUserByEmail(t *testing.T) {
	s, owner := newTestIdentity(t)
	ctx := context.Background()
	if _, err := s.GetUserByEmail(ctx, "nobody@beknown.work"); !errors.Is(err, ErrNoUser) {
		t.Fatalf("unknown email err = %v, want ErrNoUser", err)
	}
	if _, err := s.GetUserByEmail(ctx, "  "); !errors.Is(err, ErrNoUser) {
		t.Fatalf("blank email err = %v, want ErrNoUser", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE users SET email = ? WHERE id = ?`, "Ada@BeKnown.work", owner); err != nil {
		t.Fatal(err)
	}
	u, err := s.GetUserByEmail(ctx, "ada@beknown.work")
	if err != nil || u.ID != owner {
		t.Fatalf("GetUserByEmail = %+v, %v", u, err)
	}
}

func TestAgentNameRule(t *testing.T) {
	s, owner := newTestIdentity(t)
	ctx := context.Background()
	for _, name := range []string{"T3 Code (bkt3)", "bot", "claude-code: laptop_1.2"} {
		if _, _, err := s.CreateAgentWithToken(ctx, owner, name); err != nil {
			t.Errorf("CreateAgentWithToken(%q): %v", name, err)
		}
	}
	for _, name := range []string{"bad;name", "a$b", "<script>", "x`y`", strings.Repeat("a", 65)} {
		if _, _, err := s.CreateAgentWithToken(ctx, owner, name); err == nil {
			t.Errorf("CreateAgentWithToken(%q) accepted", name)
		}
	}
}

// A rotate kills a pending enrollment code for the agent: exchanging it
// afterwards must not replace the token the rotate issued.
func TestRotateClearsPendingEnrollment(t *testing.T) {
	for _, grace := range []time.Duration{0, time.Minute} {
		s, owner := newTestIdentity(t)
		ctx := context.Background()
		code, ag, err := s.CreateEnrollment(ctx, owner, "T3 Code (bkt3)", time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		tok, err := s.RotateAgentToken(ctx, owner, ag.ID, grace)
		if err != nil {
			t.Fatalf("grace %s: rotate: %v", grace, err)
		}
		if _, _, err := s.ExchangeEnrollment(ctx, code); !errors.Is(err, ErrEnrollNotFound) {
			t.Errorf("grace %s: exchange after rotate: err = %v, want ErrEnrollNotFound", grace, err)
		}
		if got, err := s.VerifyAgentToken(ctx, tok); err != nil || got.ID != ag.ID {
			t.Errorf("grace %s: rotated token: %+v, %v", grace, got, err)
		}
	}
}
