-- Retry keys belong to an authenticated agent and survive decisions/restarts.
-- NULL preserves the existing create-on-every-call behavior.
ALTER TABLE inbox_requests ADD COLUMN idempotency_key TEXT;
ALTER TABLE inbox_requests ADD COLUMN submission_hash TEXT;
CREATE UNIQUE INDEX idx_inbox_agent_idempotency ON inbox_requests(agent_id, idempotency_key);

ALTER TABLE agent_sessions ADD COLUMN idempotency_key TEXT;
ALTER TABLE agent_sessions ADD COLUMN submission_hash TEXT;
CREATE UNIQUE INDEX idx_agent_sessions_idempotency ON agent_sessions(agent_id, idempotency_key);
