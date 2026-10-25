-- 0045: rate limits shared by every replica (TEC-1630, v0.1 slice A).
--
-- With several replicas on one Postgres database, a per-process counter
-- lets a client multiply its budget by the number of replicas. These
-- limits now count in one row per bucket: the login throttle, the
-- per-address limits of the unauthenticated routes, the connect-t3
-- limits and the auto-approval budget per agent. SQLite (one replica)
-- keeps counting in memory and leaves the table empty.
--
-- bucket is the limit's key (for example login:<address>:<user>);
-- window_start is when the current window began and expires_at when the
-- row may be deleted (Unix milliseconds); hits counts the window's
-- requests, one past the limit once it refused one.
--
-- Same table as migrations/sqlite/0045_rate_limit_buckets.sql.
-- Rollback: an older binary ignores the table.
CREATE TABLE IF NOT EXISTS rate_limit_buckets (
    bucket       TEXT PRIMARY KEY,
    window_start BIGINT NOT NULL,
    hits         BIGINT NOT NULL,
    expires_at   BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_rate_limit_buckets_expires ON rate_limit_buckets (expires_at);
