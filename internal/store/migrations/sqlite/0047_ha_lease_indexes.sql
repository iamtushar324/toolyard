-- 0047: indexes for the work-lease scans (TEC-1630, v0.1 slice A).
--
-- Each replica renews its own running rows every 15 seconds (WHERE
-- owner_instance = ... AND the row runs), and the leader looks for running
-- rows whose lease ended every 30 seconds. Only running rows matter, and
-- they are few, so each index is partial: it holds the running rows only,
-- by owner and lease end.
--
-- Same indexes as migrations/postgres/0047_ha_lease_indexes.sql.
-- Rollback: an older binary ignores the indexes.
CREATE INDEX IF NOT EXISTS idx_inbox_executions_lease ON inbox_executions (owner_instance, lease_until) WHERE state = 'running';
CREATE INDEX IF NOT EXISTS idx_legacy_claims_lease ON legacy_execution_claims (owner_instance, lease_until) WHERE state = 'running';
CREATE INDEX IF NOT EXISTS idx_code_runs_lease ON code_runs (owner_instance, lease_until) WHERE status IN ('queued', 'running');
CREATE INDEX IF NOT EXISTS idx_mwj_lease ON memory_webhook_jobs (owner_instance, lease_until) WHERE status = 'running';
