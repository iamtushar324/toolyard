-- Events Hub: external systems push events in via per-source webhook tokens;
-- pollers ("scrapers") watch URLs/JSON fields and emit events on change;
-- agents publish natural-language events to each other. Events land in this
-- SQLite hot store, sink async into the ClickHouse lake, and fan out to
-- push/SSE + MCP tools. Every event carries a natural-language summary.

CREATE TABLE IF NOT EXISTS event_sources (
  id            TEXT PRIMARY KEY,        -- 'evs_'+uuid
  name          TEXT NOT NULL UNIQUE,
  kind          TEXT NOT NULL,           -- 'webhook' | 'poller' | 'agent'
  token_hash    TEXT,                    -- sha256 hex (webhook sources only)
  poller_config TEXT,                    -- JSON {url, interval_sec, mode, json_path, headers}
  poller_state  TEXT,                    -- JSON {last_hash, last_value, last_polled_at, consecutive_failures}
  last_error    TEXT,
  last_event_at INTEGER,
  notify        INTEGER NOT NULL DEFAULT 0,
  notify_types  TEXT,                    -- JSON array; empty/null = all types
  enabled       INTEGER NOT NULL DEFAULT 1,
  created_at    INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS events (
  id          TEXT PRIMARY KEY,          -- 'ev_'+uuid
  source_id   TEXT NOT NULL REFERENCES event_sources(id) ON DELETE CASCADE,
  type        TEXT NOT NULL,
  title       TEXT,
  summary     TEXT NOT NULL,             -- natural-language sentence
  payload     TEXT,                      -- JSON, capped
  dedup_key   TEXT,
  created_at  INTEGER NOT NULL,          -- producer-claimed, clamped
  received_at INTEGER NOT NULL,          -- gateway clock
  acked_at    INTEGER,
  acked_by    TEXT,
  notified    INTEGER NOT NULL DEFAULT 0,
  lake_synced INTEGER NOT NULL DEFAULT 0
);

-- Dedup: INSERT OR IGNORE collides on (source, dedup_key) when a key is set.
CREATE UNIQUE INDEX IF NOT EXISTS idx_events_dedup
  ON events(source_id, dedup_key) WHERE dedup_key IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_events_received      ON events(received_at DESC);
CREATE INDEX IF NOT EXISTS idx_events_source_recv   ON events(source_id, received_at);
CREATE INDEX IF NOT EXISTS idx_events_type_recv     ON events(type, received_at);
CREATE INDEX IF NOT EXISTS idx_events_unacked       ON events(received_at) WHERE acked_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_events_unsynced      ON events(id) WHERE lake_synced = 0;
