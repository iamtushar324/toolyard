-- 0042: code-mode runs over REST (TEC-1627).
--
-- POST /v1/code/run starts a code-mode script that keeps running on the
-- server after the HTTP answer; GET /v1/code/runs/{id} reads it and POST
-- /v1/code/runs/{id}/resume runs a held script again after the owner
-- decided (docs/design/code-rest-route.md).
--
-- code_runs is one run: the agent that started it, the verified client
-- session (a session token's session id, NULL with an agent token), the
-- optional Idempotency-Key (unique per agent; NULL keys never collide),
-- the sha256 fingerprint of the script and reason that key was used with,
-- the script itself (at most 1 MiB, kept so a resume can run it again),
-- the reason, and the outcome: status (queued, running, succeeded,
-- failed, permission_required, deferred, outcome_unknown), the JSON
-- result, the error kind and message, the print lines (a JSON array) and
-- the held call (a JSON object). Times are Unix milliseconds; finished_at
-- stays NULL until the run reaches a final status.
--
-- code_run_calls is one tool call of a run, by its 1-based place in the
-- script: the tool, the gateway target, the sha256 of the canonical JSON
-- of the script's own arguments (never a grant token), the call status,
-- whether a resume answered it from this record, and replay_text, the
-- value the script received (a JSON string) so a resume can answer the
-- same call again without sending it.
--
-- The startup recovery reads idx_code_runs_status; the 7-day purge reads
-- idx_code_runs_created.
--
-- Same tables as migrations/postgres/0042_code_runs.sql.
-- Rollback: an older binary ignores the tables.
CREATE TABLE IF NOT EXISTS code_runs (
    id              TEXT PRIMARY KEY,          -- cr_<hex>
    agent_id        TEXT NOT NULL,
    session_id      TEXT,
    idempotency_key TEXT,
    fingerprint     TEXT NOT NULL,
    code            TEXT NOT NULL,
    reason          TEXT NOT NULL,
    status          TEXT NOT NULL,
    result          TEXT,
    error_kind      TEXT,
    error_message   TEXT,
    logs            TEXT,
    held            TEXT,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    finished_at     INTEGER,
    UNIQUE (agent_id, idempotency_key)
);
CREATE INDEX IF NOT EXISTS idx_code_runs_status ON code_runs(status);
CREATE INDEX IF NOT EXISTS idx_code_runs_created ON code_runs(created_at);

CREATE TABLE IF NOT EXISTS code_run_calls (
    run_id      TEXT NOT NULL,
    seq         INTEGER NOT NULL,
    tool        TEXT NOT NULL,
    target      TEXT NOT NULL,
    args_hash   TEXT NOT NULL,
    status      TEXT NOT NULL,
    replayed    INTEGER NOT NULL DEFAULT 0,
    replay_text TEXT,
    started_at  INTEGER NOT NULL,
    finished_at INTEGER,
    PRIMARY KEY (run_id, seq)
);
