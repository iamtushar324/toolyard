-- 0012: track which skills have been ingested into mempalace so the
-- background scanner (and skills.publish) don't re-ingest unchanged SKILL.md
-- files.
--
-- One row per skill slug. The path indexed is always "<slug>/SKILL.md"
-- relative to $SKILLS_DIR; we key by slug because that's the natural unit
-- a delete/install/gc step works on. mtime is the SKILL.md mtime at last
-- ingest; palace_id is the mempalace entry_id we got back so a future
-- skills.gc can find and remove the matching drawer.

CREATE TABLE IF NOT EXISTS skills_sync (
    slug        TEXT PRIMARY KEY,
    mtime       INTEGER NOT NULL,
    palace_id   TEXT,
    synced_at   INTEGER NOT NULL
);
