-- 0066: durable jobs for async tool calls (v0.1 slice E).
--
-- tool_jobs is one tool call that runs in the background: the agent that
-- made it, the agent's owner, the verified or claimed client session, the
-- wrapped tool name and its upstream, the sha256 of the canonical JSON of
-- its arguments (never the arguments themselves), the reason, the path in
-- (direct, tools.execute, code_mode, rest, mcp_task) and the status:
-- queued, running, succeeded, failed, cancelled or outcome_unknown.
--
-- progress, progress_total, progress_message and progress_at hold the last
-- notifications/progress the upstream sent for the call. The result is the
-- encoded tool result: inline in result up to a limit, else in the blob
-- store under result_blob (its sha256); result_bytes is its size and
-- result_is_error the tool's isError. error explains a failed, cancelled
-- or outcome_unknown job. callback_ref is the opaque receiver the agent
-- named with _callback_ref; callback_error records why the job.completed or
-- job.failed event could not be queued. idempotency_key is the optional
-- Idempotency-Key of POST /v1/jobs (unique per agent; NULL keys never
-- collide). cancel_requested is set by jobs.cancel for the node that runs
-- the job.
--
-- High availability: owner_instance is the process that runs the job and
-- lease_until (Unix milliseconds) the end of its lease, renewed by that
-- process while the job is queued or running. Recovery takes only rows
-- whose lease ended: a running one becomes outcome_unknown (its call may
-- have run; it is never run again), a queued one failed (nothing ran).
--
-- Times are Unix milliseconds; started_at and finished_at stay NULL until
-- set. Jobs are kept 7 days after they finish (idx_tool_jobs_finished).
--
-- Same table as migrations/sqlite/0066_tool_jobs.sql.
CREATE TABLE IF NOT EXISTS tool_jobs (
    id               TEXT PRIMARY KEY,
    agent_id         TEXT NOT NULL,
    owner_user_id    TEXT NOT NULL DEFAULT '',
    session_id       TEXT,
    tool             TEXT NOT NULL,
    upstream         TEXT NOT NULL,
    params_hash      TEXT NOT NULL,
    reason           TEXT NOT NULL,
    via              TEXT NOT NULL,
    status           TEXT NOT NULL,
    progress         DOUBLE PRECISION,
    progress_total   DOUBLE PRECISION,
    progress_message TEXT,
    progress_at      BIGINT,
    result           TEXT,
    result_blob      TEXT,
    result_bytes     BIGINT NOT NULL DEFAULT 0,
    result_is_error  BIGINT NOT NULL DEFAULT 0,
    error            TEXT,
    callback_ref     TEXT,
    callback_error   TEXT,
    idempotency_key  TEXT,
    cancel_requested BIGINT NOT NULL DEFAULT 0,
    owner_instance   TEXT NOT NULL,
    lease_until      BIGINT NOT NULL,
    created_at       BIGINT NOT NULL,
    started_at       BIGINT,
    finished_at      BIGINT,
    updated_at       BIGINT NOT NULL,
    UNIQUE (agent_id, idempotency_key)
);
CREATE INDEX IF NOT EXISTS idx_tool_jobs_agent ON tool_jobs (agent_id, created_at);
CREATE INDEX IF NOT EXISTS idx_tool_jobs_owner ON tool_jobs (owner_user_id, created_at);
CREATE INDEX IF NOT EXISTS idx_tool_jobs_active ON tool_jobs (status, lease_until);
CREATE INDEX IF NOT EXISTS idx_tool_jobs_finished ON tool_jobs (finished_at);
