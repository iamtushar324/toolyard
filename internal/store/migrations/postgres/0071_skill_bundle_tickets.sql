-- 0071: single-use download links for skill bundles (skills hub, slice B).
--
-- skills.bundle and POST /v1/skills/bundle answer with a URL that carries a
-- random ticket; only the ticket's sha256 is stored here (id_hash), so any
-- replica serves the download. caller_id is the gateway caller that asked
-- ("ag_<uuid>" or "dashboard:<uid>"): the download runs as that caller, and
-- its visibility is checked again then. skills is the JSON array of the
-- bundled skill URIs. Times are Unix milliseconds; a ticket works until
-- expires_at (10 minutes) and once (used_at). Expired rows are swept when
-- the next ticket is issued; idx_skill_bundle_tickets_caller counts one
-- caller's live tickets for the per-caller cap.
--
-- Same table as migrations/sqlite/0071_skill_bundle_tickets.sql.
-- Rollback: an older binary ignores the table; drop it to remove it.
CREATE TABLE IF NOT EXISTS skill_bundle_tickets (
    id_hash    TEXT PRIMARY KEY,
    caller_id  TEXT NOT NULL,
    skills     TEXT NOT NULL,
    created_at BIGINT NOT NULL,
    expires_at BIGINT NOT NULL,
    used_at    BIGINT
);
CREATE INDEX IF NOT EXISTS idx_skill_bundle_tickets_expires ON skill_bundle_tickets (expires_at);
CREATE INDEX IF NOT EXISTS idx_skill_bundle_tickets_caller ON skill_bundle_tickets (caller_id, expires_at);
