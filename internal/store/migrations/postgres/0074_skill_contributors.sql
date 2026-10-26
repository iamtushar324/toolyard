-- 0074: skill contributors (TEC-1782, worker F2; docs/skills-hub.md
-- "Contributors").
--
-- skill_contributors names the members an admin allowed to add skill
-- sources: one row per user ID, with the admin who added them and when
-- (Unix milliseconds). A contributor's sources wait for an admin's
-- approval before agents see them. Removing a row keeps the person's
-- sources; only admins can change them then.
--
-- Same table as migrations/sqlite/0074_skill_contributors.sql.
-- Rollback: an older binary ignores the table (contributors then have a
-- member's access only); drop it by hand after the rollback.
CREATE TABLE IF NOT EXISTS skill_contributors (
    user_id  TEXT PRIMARY KEY,
    added_by TEXT NOT NULL DEFAULT '',
    added_at BIGINT NOT NULL
);
