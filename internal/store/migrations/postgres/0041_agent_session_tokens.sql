-- 0041: agent session tokens (code mode over REST, TEC-1627).
--
-- A session token is a short-lived bearer (at most one hour) that an agent
-- mints with its agent token for one client session, so a T3 thread can call
-- /v1/code/* without holding the long-lived agent token. The token is
-- tys_<id>.<secret>; only its sha256 is stored. session_id is the client's
-- session id, recorded as the verified client session in the audit of every
-- call made with the token. parent_token_hash is the sha256 of the agent
-- token that minted it: the session token stops working once that agent
-- token is no longer accepted (rotated past its grace period, the agent
-- disabled or deleted, its owner blocked, or its connection revoked).
-- Times are Unix milliseconds; revoked_at and last_used_at stay NULL until
-- set. Expired rows are purged by a sweeper.
--
-- Same table as migrations/sqlite/0041_agent_session_tokens.sql.
CREATE TABLE IF NOT EXISTS agent_session_tokens (
    id                TEXT PRIMARY KEY,
    agent_id          TEXT NOT NULL,
    token_hash        TEXT NOT NULL UNIQUE,
    session_id        TEXT NOT NULL,
    parent_token_hash TEXT NOT NULL,
    created_at        BIGINT NOT NULL,
    expires_at        BIGINT NOT NULL,
    revoked_at        BIGINT,
    last_used_at      BIGINT
);
CREATE INDEX IF NOT EXISTS idx_agent_session_tokens_agent ON agent_session_tokens (agent_id);
CREATE INDEX IF NOT EXISTS idx_agent_session_tokens_expires ON agent_session_tokens (expires_at);
