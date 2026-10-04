-- 0012: track which notes have been ingested into mempalace so the
-- background scanner (and notes.publish) don't re-ingest unchanged files.
--
-- One row per relative path under $NOTES_DIR. mtime is the file mtime at
-- last ingest; palace_id is the mempalace entry_id we got back, so a
-- future notes.unpublish / notes.gc step can find and delete the matching
-- drawer.

CREATE TABLE IF NOT EXISTS notes_sync (
    path        TEXT PRIMARY KEY,
    mtime       INTEGER NOT NULL,
    palace_id   TEXT,
    synced_at   INTEGER NOT NULL
);
