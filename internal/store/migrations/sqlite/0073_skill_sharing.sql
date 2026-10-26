-- 0073: skills hub sharing controls and contributor approval (TEC-1782,
-- worker F2; docs/skills-hub.md "Sharing" and "Contributors").
--
-- skill_sources gains:
--   blocked_json   JSON list of user IDs who never see the source's skills,
--                  whatever its audience (visibility everyone, members or
--                  the new nobody: only admins and the source's creator, in
--                  the dashboard; agents never).
--   hidden         1 when an admin hid the source: no agent sees any of its
--                  skills, whatever its audience or its skills' own
--                  settings; only an admin sets or clears it.
--   approval       '' (added by an admin: nothing to approve), 'pending' (a
--                  skill contributor added it: it syncs, agents see
--                  nothing), 'approved' or 'rejected' (disabled, agents see
--                  nothing). approval_note is the reason of a rejection;
--                  approval_by the deciding admin's user ID.
-- Existing sources keep their visibility and members and get approval ''.
--
-- skill_sharing is one skill's own sharing, by source and skill path (the
-- folder path relative to the source folder, as skill_runs_overrides), so
-- it survives new versions; a skill that disappears keeps its row
-- harmlessly. mode '' follows the source's audience; 'everyone',
-- 'members' (members_json) or 'nobody' replace it for that skill.
-- blocked_json adds to the source's block list. Times are Unix
-- milliseconds.
--
-- Same schema as migrations/postgres/0073_skill_sharing.sql.
-- Rollback: an older binary ignores the new columns and the table, and
-- its rule knows only 'members': it shows a 'nobody', hidden, blocked or
-- pending contributor source to everyone. So do not hide, block or set
-- 'nobody', and add no skill contributors, while an older replica runs;
-- before a rollback, delete or set to 'members' the sources that rely on
-- them. Drop the columns and the table by hand only after the rollback.
ALTER TABLE skill_sources ADD COLUMN blocked_json TEXT NOT NULL DEFAULT '[]';
ALTER TABLE skill_sources ADD COLUMN hidden INTEGER NOT NULL DEFAULT 0;
ALTER TABLE skill_sources ADD COLUMN approval TEXT NOT NULL DEFAULT '';
ALTER TABLE skill_sources ADD COLUMN approval_note TEXT NOT NULL DEFAULT '';
ALTER TABLE skill_sources ADD COLUMN approval_by TEXT NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS skill_sharing (
    source_id    TEXT NOT NULL,
    skill_path   TEXT NOT NULL,
    mode         TEXT NOT NULL DEFAULT '',
    members_json TEXT NOT NULL DEFAULT '[]',
    blocked_json TEXT NOT NULL DEFAULT '[]',
    set_by       TEXT NOT NULL DEFAULT '',
    updated_at   INTEGER NOT NULL,
    PRIMARY KEY (source_id, skill_path)
);
