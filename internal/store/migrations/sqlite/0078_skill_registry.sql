-- 0078: the native skills registry (docs/skills-hub.md "Native skills").
--
-- A native source (kind 'native', a "collection") holds skills that agents
-- and people write through toolyard's API instead of a GitHub repository.
-- Each write builds a new version of the collection through the same
-- pipeline as a GitHub commit (skill_versions, skill_entries, skill_files,
-- skill_blobs), so search, get, read, grep, bundles, sharing and the MCP
-- Skills extension serve native skills unchanged. Additive only: no
-- existing row or column changes.
--
-- skill_sources gains:
--   editors_json  JSON list of user IDs who may write the collection's
--                 skills (besides its creator and the admins); '[]' for
--                 every GitHub and built-in source, which ignore it.
--
-- skill_revisions is one write of one native skill: its number (1, 2, ...
-- per source and skill path), the operation ('create' or 'update'), the
-- version the write made live and that version's tree digest (commit_sha,
-- the provenance of the revision after the version is retained away), the digest of the skill's files and of its
-- SKILL.md, who wrote it (the person, and the agent when an agent wrote it
-- for them), their note, and the change proposal it came from when an
-- owner approved it in the Inbox (proposal_id, '' for a direct write; one
-- revision per proposal, so a retried approval never writes twice).
-- Revisions outlive the collection's version retention: their files are
-- kept in skill_revision_files (path in the skill folder, digest, size,
-- media type) and their bytes in skill_revision_blobs, once per digest,
-- apart from skill_blobs: no blob purge of this build or of an older one
-- (after a rollback) touches them. Deleting a collection deletes its
-- revisions and the bytes no other revision holds. Times are Unix
-- milliseconds.
--
-- Same schema as migrations/postgres/0078_skill_registry.sql.
-- Rollback: an older binary ignores the column and the tables, and its
-- poller never syncs a native source (its fixed settings point at no
-- GitHub repository, and its next check is in year 9999), so it serves the
-- collection's last version read-only. Drop the column and the tables by
-- hand only after deleting every native source.
ALTER TABLE skill_sources ADD COLUMN editors_json TEXT NOT NULL DEFAULT '[]';

CREATE TABLE IF NOT EXISTS skill_revisions (
    source_id       TEXT NOT NULL,
    skill_path      TEXT NOT NULL,
    revision        INTEGER NOT NULL,
    op              TEXT NOT NULL,
    version_id      TEXT NOT NULL,
    commit_sha      TEXT NOT NULL,
    name            TEXT NOT NULL,
    files_digest    TEXT NOT NULL,
    skill_md_sha256 TEXT NOT NULL,
    file_count      INTEGER NOT NULL DEFAULT 0,
    total_bytes     INTEGER NOT NULL DEFAULT 0,
    author_user_id  TEXT NOT NULL DEFAULT '',
    author_agent_id TEXT NOT NULL DEFAULT '',
    note            TEXT NOT NULL DEFAULT '',
    proposal_id     TEXT NOT NULL DEFAULT '',
    created_at      INTEGER NOT NULL,
    PRIMARY KEY (source_id, skill_path, revision)
);
CREATE INDEX IF NOT EXISTS idx_skill_revisions_proposal ON skill_revisions (proposal_id);

CREATE TABLE IF NOT EXISTS skill_revision_files (
    source_id  TEXT NOT NULL,
    skill_path TEXT NOT NULL,
    revision   INTEGER NOT NULL,
    path       TEXT NOT NULL,
    sha256     TEXT NOT NULL,
    size       INTEGER NOT NULL,
    media_type TEXT NOT NULL,
    PRIMARY KEY (source_id, skill_path, revision, path)
);
CREATE INDEX IF NOT EXISTS idx_skill_revision_files_sha256 ON skill_revision_files (sha256);

CREATE TABLE IF NOT EXISTS skill_revision_blobs (
    sha256  TEXT PRIMARY KEY,
    size    INTEGER NOT NULL,
    content BLOB NOT NULL
);
