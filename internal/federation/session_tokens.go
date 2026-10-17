package federation

import (
	"context"
	"database/sql"
	"errors"

	"github.com/tusharbhardwaj/toolyard/internal/identity"
)

// VerifySessionToken is identity.VerifySessionToken plus the connection
// checks VerifyAgent applies to an agent token, run for the session token's
// agent: a federation connection must still be active, its environment
// trusted and its credential unexpired; a host or local connection must
// still be valid. So a session token stops working as soon as the agent
// token it was minted with would be refused at an ingress. A failed check
// is identity.ErrSessionTokenInvalid; any other error is a database
// failure.
func (s *Service) VerifySessionToken(ctx context.Context, token string) (*identity.SessionToken, *identity.Agent, error) {
	t, a, err := s.identity.VerifySessionToken(ctx, token)
	if err != nil {
		return nil, nil, err
	}
	if err := s.agentConnectionValid(ctx, a.ID); err != nil {
		return nil, nil, err
	}
	return t, a, nil
}

// agentConnectionValid is the connection half of VerifyAgent, for an agent
// whose credential the caller has already checked: nil when the agent has
// no connection or a valid one, identity.ErrSessionTokenInvalid when its
// connection is revoked, removed or expired.
func (s *Service) agentConnectionValid(ctx context.Context, agentID string) error {
	var status, trust string
	var expiry int64
	err := s.db.QueryRowContext(ctx, `SELECT c.status,c.expires_at,i.status FROM federation_connections c JOIN federation_issuers i ON i.issuer=c.issuer WHERE c.agent_id=?`, agentID).Scan(&status, &expiry, &trust)
	if errors.Is(err, sql.ErrNoRows) {
		var hostCount int
		if e := s.db.QueryRowContext(ctx, `SELECT count(*) FROM host_connections WHERE agent_id=?`, agentID).Scan(&hostCount); e != nil {
			return e
		}
		if hostCount > 0 && s.hostValid(ctx, agentID, false) != nil {
			return identity.ErrSessionTokenInvalid
		}
		var localCount int
		if e := s.db.QueryRowContext(ctx, `SELECT count(*) FROM local_connections WHERE agent_id=?`, agentID).Scan(&localCount); e != nil {
			return e
		}
		if localCount > 0 && s.localValid(ctx, agentID, false) != nil {
			return identity.ErrSessionTokenInvalid
		}
		return nil
	}
	if err != nil {
		return err
	}
	if status != "active" || trust != "active" || expiry <= s.now().UnixMilli() {
		return identity.ErrSessionTokenInvalid
	}
	return nil
}
