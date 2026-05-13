-- Lake control-plane metadata tables. These live in the SQLite control
-- plane (toolyard.db), NOT inside the DuckDB warehouse — they record
-- what was loaded and when, so the dashboard can show "last bootstrap
-- run", and so a future scheduler can decide whether a source is due
-- for re-ingest. The lake itself stays as just-the-data.

CREATE TABLE IF NOT EXISTS lake_ingest_runs (
    id            TEXT PRIMARY KEY,
    source_path   TEXT NOT NULL,
    source_type   TEXT NOT NULL,    -- 'sqlite' | 'json' | 'jsonl' | 'csv' | 'parquet' | 'http' | 'markdown'
    target_table  TEXT NOT NULL,
    rows_loaded   INTEGER,
    started_at    INTEGER NOT NULL,
    finished_at   INTEGER,
    error         TEXT
);
CREATE INDEX IF NOT EXISTS idx_lake_ingest_runs_started ON lake_ingest_runs(started_at DESC);

CREATE TABLE IF NOT EXISTS lake_saved_queries (
    id              TEXT PRIMARY KEY,
    name            TEXT NOT NULL,
    description     TEXT,
    sql             TEXT NOT NULL,
    created_by_agent TEXT,
    created_at      INTEGER NOT NULL,
    last_run_at     INTEGER,
    run_count       INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_lake_saved_queries_name ON lake_saved_queries(name);
