-- Inbox attention: when (and whether) the owner's phone buzzes.
--
-- inbox_pushes is the outbox of pending notifications. A row is written
-- when a request arrives (now / soon), when the owner snoozes one (due at
-- the end of the snooze) and when a blocked request earns its one
-- reminder. The dispatcher sends due rows, grouping `soon` rows that share
-- a group_key (the session) into one notification, and holds rows during
-- quiet hours by moving due_at to the end of them.
CREATE TABLE IF NOT EXISTS inbox_pushes (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    request_id  TEXT    NOT NULL,
    agent_id    TEXT    NOT NULL,
    group_key   TEXT    NOT NULL,
    reason      TEXT    NOT NULL,           -- new | reminder | snooze_end
    urgency     TEXT    NOT NULL,
    due_at      INTEGER NOT NULL,
    sent_at     INTEGER,
    outcome     TEXT                        -- sent | grouped | skipped
);
CREATE INDEX IF NOT EXISTS inbox_pushes_due ON inbox_pushes(sent_at, due_at);
CREATE INDEX IF NOT EXISTS inbox_pushes_request ON inbox_pushes(request_id);

-- One row per digest slot ("2026-09-28T09:30" in the owner's time zone),
-- so a restart never sends the same digest twice.
CREATE TABLE IF NOT EXISTS inbox_digests (
    slot     TEXT    PRIMARY KEY,
    sent_at  INTEGER NOT NULL,
    items    INTEGER NOT NULL
);

-- Passkeys (WebAuthn) the owner registered for confirming high-risk
-- approvals. credential is the go-webauthn Credential as JSON.
CREATE TABLE IF NOT EXISTS inbox_passkeys (
    id          TEXT    PRIMARY KEY,        -- base64url credential ID
    user_id     TEXT    NOT NULL,
    name        TEXT    NOT NULL,
    credential  TEXT    NOT NULL,
    created_at  INTEGER NOT NULL,
    last_used_at INTEGER
);
