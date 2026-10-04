-- 0010: MemPalace integration.
--
-- The palace itself (Chroma vectors + KG SQLite) lives under
-- <data-dir>/palace and is owned by the mempalace-mcp subprocess. Toolyard
-- only needs a small index to answer dashboard questions like "which agents
-- have actually ingested anything, and how recently?" without having to fan
-- out to the upstream on every page load.

CREATE TABLE IF NOT EXISTS mempalace_agents (
    agent_id    TEXT PRIMARY KEY,
    first_seen  INTEGER NOT NULL,
    last_seen   INTEGER NOT NULL,
    entry_count INTEGER NOT NULL DEFAULT 0
);
