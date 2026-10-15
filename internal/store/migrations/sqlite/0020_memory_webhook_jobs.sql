-- TEC-482: make memory-webhook ingest asynchronous.
--
-- The ingest endpoint (TEC-481) used to block the HTTP request on the MemPalace
-- embed/index/store write, so large transcript payloads could hang for ~90s and
-- die on upstream timeouts while MemPalace was memory-throttled. This table is a
-- durable job queue: the HTTP handler validates + maps the payload, persists a
-- 'queued' job, and returns 202 immediately; a background worker writes the
-- mapped entry into MemPalace under the webhook's LOCKED wing and records the
-- terminal outcome in memory_webhook_ingestions (the existing audit ledger).
--
-- The payload/entry lives on disk here (not an in-memory queue): it survives
-- restarts and keeps RAM pressure off the very process the ticket found above
-- MemoryHigh. The mapped entry is NULLed once the job is terminal; the ledger
-- keeps the long-term history, so old terminal job rows are purged on retention.

CREATE TABLE IF NOT EXISTS memory_webhook_jobs (
  id              TEXT PRIMARY KEY,             -- 'req_'+uuid; reused as the ledger row id at terminal time
  webhook_id      TEXT NOT NULL,
  webhook_name    TEXT NOT NULL,
  wing            TEXT NOT NULL,                -- snapshot of the LOCKED wing; never overridable by callers
  source          TEXT,
  entry           TEXT,                         -- mapped entry text; NULLed when terminal to reclaim space
  topic           TEXT,
  payload_size    INTEGER NOT NULL,             -- raw request body size in bytes
  status          TEXT NOT NULL,                -- 'queued' | 'running' | 'succeeded' | 'failed'
  attempts        INTEGER NOT NULL DEFAULT 0,
  chunk_count     INTEGER NOT NULL DEFAULT 0,
  chunks_done     INTEGER NOT NULL DEFAULT 0,   -- resume point so retries never duplicate written chunks
  detail          TEXT,                         -- aggregate mempalace detail (<=500 chars, no secrets)
  error           TEXT,                         -- failure reason surfaced via the status endpoint
  mempalace_ok    INTEGER NOT NULL DEFAULT 0,
  queued_at       INTEGER NOT NULL,
  started_at      INTEGER,
  finished_at     INTEGER,
  next_attempt_at INTEGER NOT NULL DEFAULT 0,   -- backoff gate; a job is claimable only when this <= now
  created_at      INTEGER NOT NULL,
  updated_at      INTEGER NOT NULL
);

-- Worker claim path: oldest queued job whose backoff gate has elapsed.
CREATE INDEX IF NOT EXISTS idx_mwj_claim   ON memory_webhook_jobs(status, next_attempt_at);
-- Status endpoint + per-webhook listing.
CREATE INDEX IF NOT EXISTS idx_mwj_webhook ON memory_webhook_jobs(webhook_id, queued_at DESC);
