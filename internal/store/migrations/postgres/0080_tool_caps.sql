-- 0080: daily caps on successful tool calls (TEC-2060).
--
-- An admin may cap any tool: at most per_caller_daily successful calls by
-- one caller, and at most total_daily successful calls by everyone, each
-- UTC day. 0 means no cap. A call over a cap is refused before the
-- upstream is contacted, until 00:00 UTC.
--
-- tool_caps (one row per capped tool):
--   upstream          the tool's server (or built-in group) name
--   tool              the full catalog name, as tools/list shows it
--   per_caller_daily  successful calls one caller may make per UTC day
--   total_daily       successful calls everyone together may make per day
--   note              the admin's note, '' for none
--   created_*, updated_*  when (Unix ms) and by whom (a user id)
--
-- tool_cap_usage (one row per day, tool and caller; caller '*' is the
-- total of everyone):
--   day       the UTC day, YYYY-MM-DD
--   calls     successful calls, plus calls in flight (a call takes its
--             slot before it runs and gives it back if it fails)
--   updated_at  Unix ms of the last change
-- Old days are deleted by the retention compactor.
--
-- Same schema as migrations/sqlite/0080_tool_caps.sql.
-- Rollback: an older binary ignores both tables and enforces no cap.
-- Drop them by hand after the rollback if wanted.
CREATE TABLE IF NOT EXISTS tool_caps (
    upstream         TEXT NOT NULL,
    tool             TEXT NOT NULL,
    per_caller_daily BIGINT NOT NULL DEFAULT 0 CHECK (per_caller_daily >= 0),
    total_daily      BIGINT NOT NULL DEFAULT 0 CHECK (total_daily >= 0),
    note             TEXT NOT NULL DEFAULT '',
    created_at       BIGINT NOT NULL,
    created_by       TEXT,
    updated_at       BIGINT NOT NULL,
    updated_by       TEXT,
    PRIMARY KEY (upstream, tool)
);

CREATE TABLE IF NOT EXISTS tool_cap_usage (
    day        TEXT NOT NULL,
    upstream   TEXT NOT NULL,
    tool       TEXT NOT NULL,
    caller     TEXT NOT NULL,
    calls      BIGINT NOT NULL DEFAULT 0 CHECK (calls >= 0),
    updated_at BIGINT NOT NULL,
    PRIMARY KEY (day, upstream, tool, caller)
);
