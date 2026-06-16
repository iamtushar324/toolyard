-- TEC-481: authenticated, wing-locked memory-ingestion webhooks for n8n.
--
-- Each webhook is bound to exactly one MemPalace wing at creation time and
-- authenticated by its own bearer token (sha256-hash-only; plaintext shown once
-- on create/rotate). Callers can never override the bound wing — the wing is
-- read from this row, never from the request. Within the locked wing each
-- webhook carries its own payload_spec so different n8n automations can
-- structure their data differently.

CREATE TABLE IF NOT EXISTS memory_webhooks (
  id           TEXT PRIMARY KEY,          -- 'mwh_'+uuid
  name         TEXT NOT NULL UNIQUE,
  wing         TEXT NOT NULL,             -- LOCKED at creation; never overridable by callers
  source       TEXT,                      -- default source-automation label
  token_hash   TEXT NOT NULL,             -- sha256 hex; plaintext shown once on create/rotate
  payload_spec TEXT,                      -- JSON PayloadSpec (mapping + optional validation)
  notes        TEXT,
  enabled      INTEGER NOT NULL DEFAULT 1,
  last_used_at INTEGER,
  ingest_count INTEGER NOT NULL DEFAULT 0,
  fail_count   INTEGER NOT NULL DEFAULT 0,
  created_at   INTEGER NOT NULL,
  updated_at   INTEGER NOT NULL
);

-- Per-ingestion ledger: powers the Memory panel metrics + audit/debugging.
-- Rows are kept after a webhook is deleted (name/wing denormalized), so there
-- is intentionally no foreign key cascade.
CREATE TABLE IF NOT EXISTS memory_webhook_ingestions (
  id            TEXT PRIMARY KEY,         -- request id 'req_'+uuid (returned to caller)
  webhook_id    TEXT NOT NULL,
  webhook_name  TEXT NOT NULL,
  wing          TEXT NOT NULL,
  source        TEXT,
  received_at   INTEGER NOT NULL,
  payload_size  INTEGER NOT NULL,
  status        TEXT NOT NULL,            -- 'ok' | 'failed' | 'rejected' | 'too_large'
  detail        TEXT,                     -- error summary / mempalace detail (no secrets)
  mempalace_ok  INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_mwi_received ON memory_webhook_ingestions(received_at DESC);
CREATE INDEX IF NOT EXISTS idx_mwi_webhook  ON memory_webhook_ingestions(webhook_id, received_at DESC);
CREATE INDEX IF NOT EXISTS idx_mwi_wing     ON memory_webhook_ingestions(wing, received_at DESC);
CREATE INDEX IF NOT EXISTS idx_mwi_status   ON memory_webhook_ingestions(status, received_at DESC);
