-- 0079: guest invites (TEC-2058).
--
-- A person outside the Clerk organisation may sign in only after an admin
-- saved their email here. Their verified primary Clerk address must match
-- a row that has not expired. The row says which servers the guest may
-- use and, optionally, which tools of them (full tool names, as tools/list
-- shows them). The built-in data groups (memory, events, notes) are never
-- given to a guest.
--
-- guest_invites:
--   email       the invited address, lower case (the key)
--   servers     JSON list of server names the guest may use
--   tools       JSON list of full tool names, '[]' for every tool of
--               those servers
--   expires_at  Unix milliseconds after which the invite admits nobody,
--               NULL for never
--   note        the admin's note, '' for none
--   created_*, updated_*  when and by whom (a user id)
--
-- users gains:
--   is_guest    1 for a person admitted by an invite. users.role stays
--               'member' for them, so the role CHECK is unchanged.
--
-- Same schema as migrations/postgres/0079_guest_invites.sql.
-- Rollback: an older binary ignores the table and the column. It sees a
-- guest as a plain member with the invite's servers, refuses their next
-- sign-in (they are not in the organisation), and its hourly sync blocks
-- them as left_org. Drop the table and the column by hand after the
-- rollback if wanted.
ALTER TABLE users ADD COLUMN is_guest INTEGER NOT NULL DEFAULT 0 CHECK (is_guest IN (0, 1));

CREATE TABLE IF NOT EXISTS guest_invites (
    email      TEXT PRIMARY KEY CHECK (email = lower(email)),
    servers    TEXT NOT NULL DEFAULT '[]',
    tools      TEXT NOT NULL DEFAULT '[]',
    expires_at INTEGER,
    note       TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    created_by TEXT,
    updated_at INTEGER NOT NULL,
    updated_by TEXT
);
