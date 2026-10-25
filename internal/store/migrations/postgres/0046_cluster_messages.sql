-- 0046: large cluster messages (TEC-1630, v0.1 slice A).
--
-- Replicas sharing one Postgres database tell each other about changes
-- with LISTEN/NOTIFY (internal/cluster). A NOTIFY payload is small, so a
-- larger message (a dashboard live event that carries an approval or an
-- audit row) is stored here for a few minutes and the NOTIFY names its
-- row; every replica reads it from here. SQLite (one replica) never
-- writes the table.
--
-- id is a random hex ID, topic the cluster topic, payload the message
-- (at most 1 MiB), created_at Unix milliseconds. The publisher deletes
-- rows older than five minutes.
--
-- Same table as migrations/sqlite/0046_cluster_messages.sql.
-- Rollback: an older binary ignores the table.
CREATE TABLE IF NOT EXISTS cluster_messages (
    id         TEXT PRIMARY KEY,
    topic      TEXT NOT NULL,
    payload    TEXT NOT NULL,
    created_at BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_cluster_messages_created ON cluster_messages (created_at);
