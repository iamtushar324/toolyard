-- 0068: the skills hub store (TEC-1782, docs/design/skills-hub.md slice A).
--
-- skill_sources is one GitHub repository + branch (+ folder) that holds
-- Agent Skills. slug is the skill:// URI prefix. exclude_json, members_json
-- and limits_json are JSON (a list of globs, a list of user IDs, the
-- Limits object). token_sealed and webhook_secret_sealed are sealed with
-- secrets.key (internal/sealbox, AAD "skillhub-token|<id>" and
-- "skillhub-webhook|<id>"); they never leave the server. head_sha and
-- head_etag are the branch head and ETag the last check saw (a 304 answer
-- costs no rate limit). last_checked_at, next_check_at (the poll loop's
-- schedule) and the times are Unix milliseconds; failures counts the syncs
-- that failed in a row, for the backoff.
--
-- skill_versions is the skills of a source at one commit: staged, active
-- (at most one per source, also named by skill_sources.active_version_id),
-- retired, rejected or failed. config is a digest of the settings it was
-- built with (folder, excludes, limits): an incremental update starts only
-- from a version with the current config. warnings_json and diff_json are
-- JSON (Warning list, Diff).
--
-- skill_entries is one skill of a version: its folder path relative to the
-- source folder ('' for a skill at the folder itself), the frontmatter as
-- JSON (verbatim, SEP-2640), runs from frontmatter metadata.toolyard-runs
-- ('' when unset), the SKILL.md digest and a digest of the skill's file
-- set (for the diff). skill_files is one file of a version: its path
-- relative to the source folder, the innermost skill folder that holds it,
-- its digest, size and media type. skill_blobs holds each file's bytes
-- once, by digest ("sha256:<64 hex>"); touched_at protects a blob a sync
-- is about to reference from the unreferenced-blob purge.
--
-- skill_usage counts reads per skill per day (days since the Unix epoch,
-- UTC). skill_runs_overrides is an admin's choice of where one skill runs.
--
-- The old skills_sync table (the removed local skills folder) is left as
-- it is.
--
-- Same tables as migrations/postgres/0068_skill_hub.sql.
-- Rollback: an older binary ignores these tables; drop them by hand
-- (DROP TABLE skill_sources, skill_versions, skill_entries, skill_files,
-- skill_blobs, skill_usage, skill_runs_overrides) only after the rollback.
CREATE TABLE IF NOT EXISTS skill_sources (
    id                    TEXT PRIMARY KEY,          -- src_<hex>
    slug                  TEXT NOT NULL,
    kind                  TEXT NOT NULL DEFAULT 'github',
    owner                 TEXT NOT NULL,
    repo                  TEXT NOT NULL,
    branch                TEXT NOT NULL,
    path                  TEXT NOT NULL DEFAULT '',
    exclude_json          TEXT NOT NULL DEFAULT '[]',
    mode                  TEXT NOT NULL,
    pinned_sha            TEXT NOT NULL DEFAULT '',
    auth                  TEXT NOT NULL DEFAULT 'none',
    token_sealed          TEXT NOT NULL DEFAULT '',
    poll_seconds          INTEGER NOT NULL DEFAULT 300,
    enabled               INTEGER NOT NULL DEFAULT 1,
    visibility            TEXT NOT NULL DEFAULT 'everyone',
    members_json          TEXT NOT NULL DEFAULT '[]',
    default_runs          TEXT NOT NULL DEFAULT 'anywhere',
    limits_json           TEXT NOT NULL DEFAULT '{}',
    webhook_secret_sealed TEXT NOT NULL DEFAULT '',
    created_by            TEXT NOT NULL,
    created_at            INTEGER NOT NULL,
    updated_at            INTEGER NOT NULL,
    active_version_id     TEXT NOT NULL DEFAULT '',
    pending_version_id    TEXT NOT NULL DEFAULT '',
    head_sha              TEXT NOT NULL DEFAULT '',
    head_etag             TEXT NOT NULL DEFAULT '',
    last_checked_at       INTEGER,
    next_check_at         INTEGER,
    last_error            TEXT NOT NULL DEFAULT '',
    failures              INTEGER NOT NULL DEFAULT 0,
    UNIQUE (slug)
);
CREATE INDEX IF NOT EXISTS idx_skill_sources_due ON skill_sources(enabled, next_check_at);

CREATE TABLE IF NOT EXISTS skill_versions (
    id            TEXT PRIMARY KEY,                  -- skv_<hex>
    source_id     TEXT NOT NULL,
    sha           TEXT NOT NULL,
    status        TEXT NOT NULL,
    config        TEXT NOT NULL DEFAULT '',
    created_at    INTEGER NOT NULL,
    activated_at  INTEGER,
    decided_by    TEXT NOT NULL DEFAULT '',
    note          TEXT NOT NULL DEFAULT '',
    skill_count   INTEGER NOT NULL DEFAULT 0,
    file_count    INTEGER NOT NULL DEFAULT 0,
    total_bytes   INTEGER NOT NULL DEFAULT 0,
    warning_count INTEGER NOT NULL DEFAULT 0,
    warnings_json TEXT NOT NULL DEFAULT '[]',
    diff_json     TEXT NOT NULL DEFAULT '',
    full_sync     INTEGER NOT NULL DEFAULT 1,
    error         TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_skill_versions_source ON skill_versions(source_id, created_at);
CREATE INDEX IF NOT EXISTS idx_skill_versions_sha ON skill_versions(source_id, sha);

CREATE TABLE IF NOT EXISTS skill_entries (
    version_id       TEXT NOT NULL,
    path             TEXT NOT NULL,
    name             TEXT NOT NULL,
    description      TEXT NOT NULL,
    frontmatter_json TEXT NOT NULL,
    runs             TEXT NOT NULL DEFAULT '',
    skill_md_sha256  TEXT NOT NULL,
    files_digest     TEXT NOT NULL,
    PRIMARY KEY (version_id, path)
);

CREATE TABLE IF NOT EXISTS skill_files (
    version_id TEXT NOT NULL,
    path       TEXT NOT NULL,
    skill_path TEXT NOT NULL,
    sha256     TEXT NOT NULL,
    size       INTEGER NOT NULL,
    media_type TEXT NOT NULL,
    PRIMARY KEY (version_id, path)
);
CREATE INDEX IF NOT EXISTS idx_skill_files_sha256 ON skill_files(sha256);

CREATE TABLE IF NOT EXISTS skill_blobs (
    sha256     TEXT PRIMARY KEY,
    size       INTEGER NOT NULL,
    content    BLOB NOT NULL,
    touched_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS skill_usage (
    source_id  TEXT NOT NULL,
    skill_path TEXT NOT NULL,
    day        INTEGER NOT NULL,
    reads      INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (source_id, skill_path, day)
);

CREATE TABLE IF NOT EXISTS skill_runs_overrides (
    source_id  TEXT NOT NULL,
    skill_path TEXT NOT NULL,
    runs       TEXT NOT NULL,
    set_by     TEXT NOT NULL DEFAULT '',
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (source_id, skill_path)
);
