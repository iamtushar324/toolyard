package identity

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/store/storetest"
)

// These tests run on the engine the CI job selects (TOOLYARD_TEST_DB), so
// on SQLite and on Postgres.

// newSessionTokenTestService is a store with one owner and one agent.
func newSessionTokenTestService(t *testing.T) (*Service, *User, string, *Agent) {
	t.Helper()
	ctx := context.Background()
	s := New(storetest.Open(t))
	u, err := s.CreateUser(ctx, "owner", "long-enough-password")
	if err != nil {
		t.Fatal(err)
	}
	tok, ag, err := s.CreateAgentWithToken(ctx, u.ID, "claude")
	if err != nil {
		t.Fatal(err)
	}
	return s, u, tok, ag
}

func mintSessionToken(t *testing.T, s *Service, agentID, parent, sessionID string, ttl time.Duration) (string, *SessionToken) {
	t.Helper()
	tok, rec, err := s.MintSessionToken(context.Background(), agentID, parent, sessionID, ttl)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	return tok, rec
}

func TestSessionTokenMintVerifyRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, u, agentTok, ag := newSessionTokenTestService(t)
	if _, err := s.db.ExecContext(ctx, `UPDATE users SET email = ?, display_name = ? WHERE id = ?`, "ada@example.test", "Ada", u.ID); err != nil {
		t.Fatal(err)
	}
	before := time.Now().UnixMilli()
	tok, rec := mintSessionToken(t, s, ag.ID, agentTok, "t3-thread-123", 0)
	if !strings.HasPrefix(tok, "tys_st_") || !strings.HasPrefix(rec.ID, "st_") || !IsSessionToken(tok) {
		t.Fatalf("shape: token %.10s… id %s", tok, rec.ID)
	}
	if id, ok := SessionTokenID(tok); !ok || id != rec.ID {
		t.Fatalf("SessionTokenID = %q %v, want %q", id, ok, rec.ID)
	}
	if rec.AgentID != ag.ID || rec.SessionID != "t3-thread-123" {
		t.Fatalf("record = %+v", rec)
	}
	if rec.CreatedAt < before || rec.ExpiresAt-rec.CreatedAt != DefaultSessionTokenTTL.Milliseconds() {
		t.Fatalf("created %d expires %d: want the default lifetime from now", rec.CreatedAt, rec.ExpiresAt)
	}

	got, gag, err := s.VerifySessionToken(ctx, tok)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != rec.ID || got.SessionID != "t3-thread-123" || got.AgentID != ag.ID || got.ExpiresAt != rec.ExpiresAt {
		t.Fatalf("verify record = %+v", got)
	}
	if gag.ID != ag.ID || gag.Owner != u.ID || gag.OwnerEmail != "ada@example.test" || gag.OwnerName() != "Ada" || gag.Kind != AgentKindAgent {
		t.Fatalf("verify agent = %+v", gag)
	}
	if got.LastUsedAt == 0 {
		t.Fatal("first use did not record last_used_at")
	}

	// Only hashes are stored: neither the session token, its secret, nor
	// the parent agent token appear in the row.
	var tokenHash, parentHash, sessionID string
	if err := s.db.QueryRowContext(ctx, `SELECT token_hash, parent_token_hash, session_id FROM agent_session_tokens WHERE id = ?`, rec.ID).
		Scan(&tokenHash, &parentHash, &sessionID); err != nil {
		t.Fatal(err)
	}
	secret := tok[strings.IndexByte(tok, '.')+1:]
	if tokenHash != HashToken(tok) || parentHash != HashToken(agentTok) || sessionID != "t3-thread-123" {
		t.Fatalf("stored hashes: token %v parent %v", tokenHash == HashToken(tok), parentHash == HashToken(agentTok))
	}
	for _, v := range []string{tokenHash, parentHash} {
		if strings.Contains(v, secret) || strings.Contains(v, tok) || strings.Contains(v, agentTok) {
			t.Fatal("a token is stored in clear")
		}
	}

	// Malformed, tampered and foreign tokens are all invalid.
	for _, bad := range []string{
		"", "tys_", "tys_st_", "tys_" + rec.ID, "tys_" + rec.ID + ".", tok + "x",
		strings.Replace(tok, ".", ".A", 1), "tys_st_zz." + secret, "tys_ST_00." + secret,
		agentTok, rec.ID, "Bearer " + tok,
	} {
		if _, _, err := s.VerifySessionToken(ctx, bad); !errors.Is(err, ErrSessionTokenInvalid) {
			t.Errorf("verify(%.24q) = %v, want ErrSessionTokenInvalid", bad, err)
		}
	}
	// A session token is not an agent token.
	if _, err := s.VerifyAgentToken(ctx, tok); err == nil {
		t.Fatal("a session token verified as an agent token")
	}
}

func TestSessionTokenTTLClamp(t *testing.T) {
	for in, want := range map[time.Duration]time.Duration{
		-time.Second:     DefaultSessionTokenTTL,
		0:                DefaultSessionTokenTTL,
		time.Second:      MinSessionTokenTTL,
		59 * time.Second: MinSessionTokenTTL,
		time.Minute:      time.Minute,
		10 * time.Minute: 10 * time.Minute,
		time.Hour:        time.Hour,
		time.Hour + 1:    MaxSessionTokenTTL,
		72 * time.Hour:   MaxSessionTokenTTL,
	} {
		if got := ClampSessionTokenTTL(in); got != want {
			t.Errorf("ClampSessionTokenTTL(%v) = %v, want %v", in, got, want)
		}
	}
	if MinSessionTokenTTL != 60*time.Second || MaxSessionTokenTTL != 3600*time.Second || DefaultSessionTokenTTL != MaxSessionTokenTTL {
		t.Fatal("the contract is 60..3600 seconds, default 3600")
	}
	s, _, agentTok, ag := newSessionTokenTestService(t)
	for ttl, want := range map[time.Duration]time.Duration{
		10 * time.Second: time.Minute,
		2 * time.Minute:  2 * time.Minute,
		5 * time.Hour:    time.Hour,
	} {
		_, rec := mintSessionToken(t, s, ag.ID, agentTok, "s", ttl)
		if got := rec.ExpiresAt - rec.CreatedAt; got != want.Milliseconds() {
			t.Errorf("mint ttl %v: lifetime %d ms, want %d", ttl, got, want.Milliseconds())
		}
	}
}

func TestSessionTokenExpiry(t *testing.T) {
	ctx := context.Background()
	s, _, agentTok, ag := newSessionTokenTestService(t)
	tok, rec := mintSessionToken(t, s, ag.ID, agentTok, "s", time.Minute)
	if _, _, err := s.VerifySessionToken(ctx, tok); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE agent_session_tokens SET expires_at = ? WHERE id = ?`, time.Now().UnixMilli()-1, rec.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.VerifySessionToken(ctx, tok); !errors.Is(err, ErrSessionTokenInvalid) {
		t.Fatalf("expired token: %v", err)
	}
}

func TestSessionTokenRevoke(t *testing.T) {
	ctx := context.Background()
	s, u, agentTok, ag := newSessionTokenTestService(t)
	otherTok, other, err := s.CreateAgentWithToken(ctx, u.ID, "other")
	if err != nil {
		t.Fatal(err)
	}
	tok, rec := mintSessionToken(t, s, ag.ID, agentTok, "s1", 0)
	keep, _ := mintSessionToken(t, s, ag.ID, agentTok, "s2", 0)

	// Another agent cannot revoke it, and the attempt changes nothing.
	if err := s.RevokeSessionToken(ctx, other.ID, rec.ID); !errors.Is(err, ErrSessionTokenNotFound) {
		t.Fatalf("revoke by another agent: %v", err)
	}
	if _, _, err := s.VerifySessionToken(ctx, tok); err != nil {
		t.Fatalf("a refused revoke changed the token: %v", err)
	}
	if err := s.RevokeSessionToken(ctx, ag.ID, rec.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.VerifySessionToken(ctx, tok); !errors.Is(err, ErrSessionTokenInvalid) {
		t.Fatalf("revoked token: %v", err)
	}
	// Idempotent: again, an unknown id and a malformed id are not errors.
	for _, id := range []string{rec.ID, "st_00112233445566778899aabb", "st_", "nope", ""} {
		if err := s.RevokeSessionToken(ctx, ag.ID, id); err != nil {
			t.Errorf("revoke %q: %v", id, err)
		}
	}
	var revokedAt int64
	if err := s.db.QueryRowContext(ctx, `SELECT revoked_at FROM agent_session_tokens WHERE id = ?`, rec.ID).Scan(&revokedAt); err != nil || revokedAt == 0 {
		t.Fatalf("revoked_at = %d %v", revokedAt, err)
	}
	// The other token of the same agent is untouched.
	if _, _, err := s.VerifySessionToken(ctx, keep); err != nil {
		t.Fatalf("sibling token: %v", err)
	}

	// The holder revokes its own token, also again and after expiry.
	own, ownRec := mintSessionToken(t, s, other.ID, otherTok, "s3", 0)
	if _, err := s.RevokeOwnSessionToken(ctx, strings.Replace(own, ".", ".X", 1)); !errors.Is(err, ErrSessionTokenInvalid) {
		t.Fatalf("revoke with a wrong secret: %v", err)
	}
	if _, _, err := s.VerifySessionToken(ctx, own); err != nil {
		t.Fatalf("a wrong secret revoked the token: %v", err)
	}
	got, err := s.RevokeOwnSessionToken(ctx, own)
	if err != nil || got.ID != ownRec.ID || got.RevokedAt == 0 || got.SessionID != "s3" {
		t.Fatalf("revoke own: %+v %v", got, err)
	}
	if _, _, err := s.VerifySessionToken(ctx, own); !errors.Is(err, ErrSessionTokenInvalid) {
		t.Fatalf("self-revoked token: %v", err)
	}
	if _, err := s.RevokeOwnSessionToken(ctx, own); err != nil {
		t.Fatalf("revoke own twice: %v", err)
	}
	expired, expiredRec := mintSessionToken(t, s, other.ID, otherTok, "s4", 0)
	if _, err := s.db.ExecContext(ctx, `UPDATE agent_session_tokens SET expires_at = 1 WHERE id = ?`, expiredRec.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RevokeOwnSessionToken(ctx, expired); err != nil {
		t.Fatalf("revoke own expired token: %v", err)
	}
	if _, err := s.RevokeOwnSessionToken(ctx, "tys_st_00112233445566778899aabb.secret"); !errors.Is(err, ErrSessionTokenInvalid) {
		t.Fatalf("revoke own unknown token: %v", err)
	}
}

func TestSessionTokenCannotMintAnother(t *testing.T) {
	ctx := context.Background()
	s, u, agentTok, ag := newSessionTokenTestService(t)
	tok, _ := mintSessionToken(t, s, ag.ID, agentTok, "s", 0)
	if _, _, err := s.MintSessionToken(ctx, ag.ID, tok, "s2", 0); !errors.Is(err, ErrAgentTokenInvalid) {
		t.Fatalf("a session token minted another: %v", err)
	}
	opTok, _, err := s.CreateOperatorToken(ctx, u.ID, "cli", nil, 0, "cli")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.MintSessionToken(ctx, ag.ID, opTok, "s2", 0); !errors.Is(err, ErrAgentTokenInvalid) {
		t.Fatalf("an operator token minted a session token: %v", err)
	}
	otherTok, _, err := s.CreateAgentWithToken(ctx, u.ID, "other")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.MintSessionToken(ctx, ag.ID, otherTok, "s2", 0); !errors.Is(err, ErrAgentTokenInvalid) {
		t.Fatalf("another agent's token minted for this agent: %v", err)
	}
	if _, _, err := s.MintSessionToken(ctx, ag.ID, ag.ID+".wrong-secret", "s2", 0); !errors.Is(err, ErrAgentTokenInvalid) {
		t.Fatalf("a wrong agent token minted: %v", err)
	}
	if _, _, err := s.MintSessionToken(ctx, "", agentTok, "s2", 0); !errors.Is(err, ErrAgentTokenInvalid) {
		t.Fatalf("no agent id: %v", err)
	}
	for _, sid := range []string{"", strings.Repeat("a", MaxSessionIDLen+1), "a\nb", "tab\there", "café", "nul\x00"} {
		if _, _, err := s.MintSessionToken(ctx, ag.ID, agentTok, sid, 0); !errors.Is(err, ErrSessionIDInvalid) {
			t.Errorf("session id %q: %v, want ErrSessionIDInvalid", sid, err)
		}
	}
	for _, sid := range []string{"a", strings.Repeat("z", MaxSessionIDLen), "t3 thread ~ 42!", "thr_01HZX"} {
		if _, _, err := s.MintSessionToken(ctx, ag.ID, agentTok, sid, 0); err != nil {
			t.Errorf("session id %q: %v", sid, err)
		}
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM agent_session_tokens`).Scan(&n); err != nil || n != 5 {
		t.Fatalf("rows = %d %v, want only the five valid mints", n, err)
	}
}

// TestSessionTokenFollowsParentRotation: a session token works while the
// agent token that minted it is accepted, so within a rotation grace and
// not after it, and never after a rotation without grace.
func TestSessionTokenFollowsParentRotation(t *testing.T) {
	ctx := context.Background()
	s, u, agentTok, ag := newSessionTokenTestService(t)
	tok, _ := mintSessionToken(t, s, ag.ID, agentTok, "s", 0)

	newTok, err := s.RotateAgentToken(ctx, u.ID, ag.ID, DefaultRotateGrace)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.VerifySessionToken(ctx, tok); err != nil {
		t.Fatalf("inside the rotation grace: %v", err)
	}
	// The old agent token can still mint inside its grace window.
	graceTok, _ := mintSessionToken(t, s, ag.ID, agentTok, "s-grace", 0)
	if _, err := s.db.ExecContext(ctx, `UPDATE agents SET prev_token_expires = ? WHERE id = ?`, time.Now().UnixMilli()-1, ag.ID); err != nil {
		t.Fatal(err)
	}
	for _, old := range []string{tok, graceTok} {
		if _, _, err := s.VerifySessionToken(ctx, old); !errors.Is(err, ErrSessionTokenInvalid) {
			t.Fatalf("past the rotation grace: %v", err)
		}
	}
	if _, _, err := s.MintSessionToken(ctx, ag.ID, agentTok, "s", 0); !errors.Is(err, ErrAgentTokenInvalid) {
		t.Fatalf("a rotated-away agent token minted: %v", err)
	}

	// A token minted with the new agent token dies with a rotation that
	// has no grace.
	fresh, _ := mintSessionToken(t, s, ag.ID, newTok, "s", 0)
	if _, _, err := s.VerifySessionToken(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RotateAgentToken(ctx, u.ID, ag.ID, 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.VerifySessionToken(ctx, fresh); !errors.Is(err, ErrSessionTokenInvalid) {
		t.Fatalf("after a rotation without grace: %v", err)
	}
}

func TestSessionTokenStopsWithItsAgentAndOwner(t *testing.T) {
	ctx := context.Background()
	s, u, agentTok, ag := newSessionTokenTestService(t)
	tok, _ := mintSessionToken(t, s, ag.ID, agentTok, "s", 0)

	if err := s.SetAgentDisabled(ctx, u.ID, ag.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.VerifySessionToken(ctx, tok); !errors.Is(err, ErrSessionTokenInvalid) {
		t.Fatalf("disabled agent: %v", err)
	}
	if _, _, err := s.MintSessionToken(ctx, ag.ID, agentTok, "s", 0); !errors.Is(err, ErrAgentTokenInvalid) {
		t.Fatalf("a disabled agent minted: %v", err)
	}
	if err := s.SetAgentDisabled(ctx, u.ID, ag.ID, false); err != nil {
		t.Fatal(err)
	}

	if _, err := s.db.ExecContext(ctx, `UPDATE users SET status = ? WHERE id = ?`, StatusBlocked, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.VerifySessionToken(ctx, tok); !errors.Is(err, ErrSessionTokenInvalid) {
		t.Fatalf("blocked owner: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE users SET status = ? WHERE id = ?`, StatusActive, u.ID); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteAgent(ctx, u.ID, ag.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.VerifySessionToken(ctx, tok); !errors.Is(err, ErrSessionTokenInvalid) {
		t.Fatalf("deleted agent: %v", err)
	}
}

func TestPurgeExpiredSessionTokens(t *testing.T) {
	ctx := context.Background()
	s, _, agentTok, ag := newSessionTokenTestService(t)
	_, old := mintSessionToken(t, s, ag.ID, agentTok, "old", 0)
	_, recent := mintSessionToken(t, s, ag.ID, agentTok, "recent", 0)
	live, _ := mintSessionToken(t, s, ag.ID, agentTok, "live", 0)
	now := time.Now()
	if _, err := s.db.ExecContext(ctx, `UPDATE agent_session_tokens SET expires_at = ? WHERE id = ?`, now.Add(-2*time.Hour).UnixMilli(), old.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE agent_session_tokens SET expires_at = ? WHERE id = ?`, now.Add(-time.Minute).UnixMilli(), recent.ID); err != nil {
		t.Fatal(err)
	}
	n, err := s.PurgeExpiredSessionTokens(ctx, time.Hour)
	if err != nil || n != 1 {
		t.Fatalf("purge = %d %v, want 1", n, err)
	}
	var left int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM agent_session_tokens`).Scan(&left); err != nil || left != 2 {
		t.Fatalf("rows left = %d %v, want 2", left, err)
	}
	if _, _, err := s.VerifySessionToken(ctx, live); err != nil {
		t.Fatalf("purge touched a live token: %v", err)
	}
}

func TestVerifyAgentTokenHash(t *testing.T) {
	ctx := context.Background()
	s, u, agentTok, ag := newSessionTokenTestService(t)
	got, err := s.VerifyAgentTokenHash(ctx, ag.ID, HashToken(agentTok))
	if err != nil || got.ID != ag.ID || got.Owner != u.ID {
		t.Fatalf("current hash: %+v %v", got, err)
	}
	for name, c := range map[string][2]string{
		"wrong hash":  {ag.ID, HashToken(agentTok + "x")},
		"empty hash":  {ag.ID, ""},
		"no agent":    {"", HashToken(agentTok)},
		"other agent": {"ag_missing", HashToken(agentTok)},
		"raw token":   {ag.ID, agentTok},
	} {
		if _, err := s.VerifyAgentTokenHash(ctx, c[0], c[1]); !errors.Is(err, ErrAgentTokenInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	newTok, err := s.RotateAgentToken(ctx, u.ID, ag.ID, DefaultRotateGrace)
	if err != nil {
		t.Fatal(err)
	}
	for _, tok := range []string{agentTok, newTok} {
		if _, err := s.VerifyAgentTokenHash(ctx, ag.ID, HashToken(tok)); err != nil {
			t.Errorf("after a rotation with grace: %v", err)
		}
	}
}
