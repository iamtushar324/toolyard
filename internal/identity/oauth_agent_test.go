package identity

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestOAuthAgentsNeverGetStaticTokens: an agent with an OAuth grant (an MCP
// client that signed in through mcpauth) cannot be given a static token by
// rotation or enrolment, so it stays bound to its grant's revocation,
// refresh rotation and session limit. Ordinary agents are unchanged.
func TestOAuthAgentsNeverGetStaticTokens(t *testing.T) {
	s, owner := newTestIdentity(t)
	ctx := context.Background()
	_, plain, err := s.CreateAgentWithToken(ctx, owner, "plain")
	if err != nil {
		t.Fatal(err)
	}
	code, app, err := s.CreateEnrollment(ctx, owner, "oauth-app", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	if _, err := s.db.Exec(`INSERT INTO mcp_oauth_clients(client_id, kind, redirect_uris, created_at, updated_at) VALUES('mcpc_t', 'dcr', '[]', ?, ?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO mcp_oauth_grants(id, client_id, user_id, agent_id, resource, created_at, updated_at) VALUES('grt_t', 'mcpc_t', ?, ?, 'r', ?, ?)`,
		owner, app.ID, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RotateAgentToken(ctx, owner, app.ID, 0); !errors.Is(err, ErrOAuthAgent) {
		t.Fatalf("rotate an OAuth agent: %v, want ErrOAuthAgent", err)
	}
	if _, _, err := s.ExchangeEnrollment(ctx, code); !errors.Is(err, ErrEnrollNotFound) {
		t.Fatalf("enrol an OAuth agent: %v, want ErrEnrollNotFound", err)
	}
	if _, err := s.RotateAgentToken(ctx, owner, plain.ID, 0); err != nil {
		t.Fatalf("rotate an ordinary agent: %v", err)
	}
}

// TestDeleteAgentRevokesItsOAuthGrant: deleting an OAuth client's agent
// revokes its grant, tokens and unused codes in the same transaction; the
// grant row stays as the agent's tombstone.
func TestDeleteAgentRevokesItsOAuthGrant(t *testing.T) {
	s, owner := newTestIdentity(t)
	ctx := context.Background()
	_, app, err := s.CreateAgentWithToken(ctx, owner, "oauth-app")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	for _, q := range []string{
		`INSERT INTO mcp_oauth_clients(client_id, kind, redirect_uris, created_at, updated_at) VALUES('mcpc_d', 'dcr', '[]', ?, ?)`,
	} {
		if _, err := s.db.Exec(q, now, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`INSERT INTO mcp_oauth_grants(id, client_id, user_id, agent_id, resource, created_at, updated_at) VALUES('grt_d', 'mcpc_d', ?, ?, 'r', ?, ?)`,
		owner, app.ID, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO mcp_oauth_tokens(token_hash, kind, grant_id, client_id, family_id, resource, auth_time, created_at, expires_at)
        VALUES('tok_d', 'access', 'grt_d', 'mcpc_d', 'fam_d', 'r', ?, ?, ?)`, now, now, now+3600000); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO mcp_oauth_codes(code_hash, grant_id, client_id, redirect_uri, code_challenge, resource, family_id, created_at, expires_at)
        VALUES('code_d', 'grt_d', 'mcpc_d', 'https://a.example/cb', 'x', 'r', 'fam_d2', ?, ?)`, now, now+60000); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteAgent(ctx, owner, app.ID); err != nil {
		t.Fatal(err)
	}
	var reason string
	var tokRevoked, codeUsed int
	if err := s.db.QueryRow(`SELECT COALESCE(revoked_reason, '') FROM mcp_oauth_grants WHERE id = 'grt_d'`).Scan(&reason); err != nil || reason != "agent_deleted" {
		t.Fatalf("grant: %q %v", reason, err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM mcp_oauth_tokens WHERE token_hash = 'tok_d' AND revoked_at IS NOT NULL`).Scan(&tokRevoked); err != nil || tokRevoked != 1 {
		t.Fatalf("token revoked: %d %v", tokRevoked, err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM mcp_oauth_codes WHERE code_hash = 'code_d' AND used_at IS NOT NULL`).Scan(&codeUsed); err != nil || codeUsed != 1 {
		t.Fatalf("code used: %d %v", codeUsed, err)
	}
}
