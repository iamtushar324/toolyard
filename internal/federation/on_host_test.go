package federation

import "testing"

// OnHost is true only for an agent with an active host connection or an
// active federation connection.
func TestOnHost(t *testing.T) {
	s, u, _ := federationFixture(t)
	ctx := t.Context()
	now := s.now().UnixMilli()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO host_connections(environment_id, local_user_id, public_key, host_name, platform, user_id, agent_id, credential_enc, credential_hash, status, expires_at, created_at, updated_at)
            VALUES('env-1','local-1','key','T3 host','linux',?,'ag_host','enc','hash','active',?,?,?)`, []any{u.ID, now + 60_000, now, now}},
		{`INSERT INTO host_connections(environment_id, local_user_id, public_key, host_name, platform, user_id, agent_id, credential_enc, credential_hash, status, expires_at, created_at, updated_at)
            VALUES('env-1','local-2','key','T3 host','linux',?,'ag_host_revoked','enc','hash','revoked',?,?,?)`, []any{u.ID, now + 60_000, now, now}},
		{`INSERT INTO federation_connections(issuer, subject, user_id, agent_id, credential_enc, version, status, expires_at)
            VALUES('stage','subject-1',?,'ag_federated','enc',1,'active',?)`, []any{u.ID, now + 60_000}},
	} {
		if _, err := s.db.ExecContext(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	for agent, want := range map[string]bool{
		"ag_host": true, "ag_federated": true,
		"ag_host_revoked": false, "ag_plain": false, "": false,
	} {
		got, err := s.OnHost(ctx, agent)
		if err != nil || got != want {
			t.Errorf("OnHost(%q) = %v, %v; want %v", agent, got, err, want)
		}
	}
}
