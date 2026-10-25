package federation

import "context"

// OnHost reports whether agentID runs on the T3 host: its credential is an
// active T3 host connection or an active federation connection, both of
// which toolyard mints for a T3 environment whose sessions run on that
// host. Every other agent (an ordinary agent key, an identity key, an
// OAuth client, a local connection) is not on the host. The skills hub
// uses it to show host-only skills to the agents that can follow them
// (gateway.SkillHub.OnHost).
func (s *Service) OnHost(ctx context.Context, agentID string) (bool, error) {
	if agentID == "" {
		return false, nil
	}
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT
            (SELECT count(*) FROM host_connections WHERE agent_id = ? AND status = 'active') +
            (SELECT count(*) FROM federation_connections WHERE agent_id = ? AND status = 'active')`,
		agentID, agentID).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}
