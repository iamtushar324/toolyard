package federation

import (
	"errors"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/identity"
)

// TestSessionTokenFollowsFederationConnection: a session token minted with
// a federation credential works while that credential would pass the
// ingress: not past the rotation grace of a renewal, not after the
// connection expires, not after the user's connection is revoked.
func TestSessionTokenFollowsFederationConnection(t *testing.T) {
	s, _, key := federationFixture(t)
	ctx := t.Context()
	p := principal(t, s, key, "stage", "alice")
	c, err := s.Connect(ctx, p, 0)
	if err != nil {
		t.Fatal(err)
	}
	tok, rec, err := s.identity.MintSessionToken(ctx, c.AgentID, c.Token, "t3-thread-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	got, ag, err := s.VerifySessionToken(ctx, tok)
	if err != nil || got.ID != rec.ID || ag.ID != c.AgentID || got.SessionID != "t3-thread-1" {
		t.Fatalf("verify: %+v %+v %v", got, ag, err)
	}

	// The connection expires: refused, as VerifyAgent refuses the credential.
	s.now = func() time.Time { return time.Now().Add(31 * 24 * time.Hour) }
	if _, err := s.VerifyAgent(ctx, c.Token); err == nil {
		t.Fatal("expired credential passed the ingress")
	}
	if _, _, err := s.VerifySessionToken(ctx, tok); !errors.Is(err, identity.ErrSessionTokenInvalid) {
		t.Fatalf("expired connection: %v", err)
	}

	// A renewal rotates the credential: the old one stays accepted for the
	// rotation grace, and so does the session token minted with it.
	s.now = func() time.Time { return time.Now().Add(29*24*time.Hour + time.Hour) }
	renewed, err := s.Connect(ctx, p, c.Version)
	if err != nil {
		t.Fatal(err)
	}
	if renewed.Token == c.Token {
		t.Fatal("renewal kept the credential")
	}
	s.now = time.Now
	if _, _, err := s.VerifySessionToken(ctx, tok); err != nil {
		t.Fatalf("inside the renewal grace: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE agents SET prev_token_expires = ? WHERE id = ?`, time.Now().UnixMilli()-1, c.AgentID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.VerifySessionToken(ctx, tok); !errors.Is(err, identity.ErrSessionTokenInvalid) {
		t.Fatalf("past the renewal grace: %v", err)
	}

	// A token minted with the renewed credential dies with the connection.
	fresh, _, err := s.identity.MintSessionToken(ctx, c.AgentID, renewed.Token, "t3-thread-2", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.VerifySessionToken(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	if err := s.Revoke(ctx, p, "user"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.VerifySessionToken(ctx, fresh); !errors.Is(err, identity.ErrSessionTokenInvalid) {
		t.Fatalf("revoked connection: %v", err)
	}
	// The agent row itself is untouched by the revocation: only the
	// connection check refuses the token.
	if _, _, err := s.identity.VerifySessionToken(ctx, fresh); err != nil {
		t.Fatalf("identity check alone: %v", err)
	}
}

// TestSessionTokenFollowsLocalConnectionParent: a session token of a local
// connection's derived agent stops when the bootstrap key (the derived
// agent's parent) is rotated, as the derived credential does.
func TestSessionTokenFollowsLocalConnectionParent(t *testing.T) {
	s, u, _ := federationFixture(t)
	ctx := t.Context()
	key, parent, err := s.identity.CreateAgentWithToken(ctx, u.ID, "Mac bootstrap")
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.ConnectLocal(ctx, key, LocalBinding{EnvironmentID: "mac-A", LocalUserID: "local-owner", InstanceID: s.InstanceID})
	if err != nil {
		t.Fatal(err)
	}
	tok, _, err := s.identity.MintSessionToken(ctx, c.AgentID, c.Token, "t3-local-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, ag, err := s.VerifySessionToken(ctx, tok); err != nil || ag.ID != c.AgentID {
		t.Fatalf("verify: %v", err)
	}
	if _, err := s.identity.RotateAgentToken(ctx, u.ID, parent.ID, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.VerifyAgent(ctx, c.Token); err == nil {
		t.Fatal("parent rotation left derived access")
	}
	if _, _, err := s.VerifySessionToken(ctx, tok); !errors.Is(err, identity.ErrSessionTokenInvalid) {
		t.Fatalf("after the parent rotation: %v", err)
	}
}

// TestSessionTokenWithoutConnection: an ordinary agent (no federation,
// host or local connection) needs only the identity checks.
func TestSessionTokenWithoutConnection(t *testing.T) {
	s, u, _ := federationFixture(t)
	ctx := t.Context()
	agentTok, ag, err := s.identity.CreateAgentWithToken(ctx, u.ID, "plain")
	if err != nil {
		t.Fatal(err)
	}
	tok, _, err := s.identity.MintSessionToken(ctx, ag.ID, agentTok, "s", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.VerifySessionToken(ctx, tok); err != nil {
		t.Fatal(err)
	}
	if err := s.identity.SetAgentDisabled(ctx, u.ID, ag.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.VerifySessionToken(ctx, tok); !errors.Is(err, identity.ErrSessionTokenInvalid) {
		t.Fatalf("disabled agent: %v", err)
	}
	if _, _, err := s.VerifySessionToken(ctx, "tys_st_00.nope"); !errors.Is(err, identity.ErrSessionTokenInvalid) {
		t.Fatalf("unknown token: %v", err)
	}
}
