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
