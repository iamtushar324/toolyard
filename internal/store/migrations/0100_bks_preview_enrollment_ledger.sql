-- A preview agent ID is dedicated to one operator-verified owner/session forever.
-- No foreign key: deletion of an agent/user must not erase its commitment.
CREATE TABLE bks_preview_enrollment_ledger (
    agent_id TEXT PRIMARY KEY NOT NULL,
    owner_user_id TEXT NOT NULL,
    session_id TEXT NOT NULL,
    proof_sha256 TEXT NOT NULL,
    credential_mode TEXT NOT NULL CHECK (credential_mode = 'dedicated-per-session'),
    approved_by TEXT NOT NULL CHECK (approved_by = owner_user_id),
    recorded_at INTEGER NOT NULL -- UTC milliseconds; must predate deferred approval
);

CREATE TRIGGER bks_preview_ledger_no_update
BEFORE UPDATE ON bks_preview_enrollment_ledger
BEGIN
    SELECT RAISE(ABORT, 'immutable preview enrollment');
END;

CREATE TRIGGER bks_preview_ledger_no_delete
BEFORE DELETE ON bks_preview_enrollment_ledger
BEGIN
    SELECT RAISE(ABORT, 'immutable preview enrollment');
END;

-- REPLACE can otherwise delete a conflicting row without a DELETE trigger.
CREATE TRIGGER bks_preview_ledger_no_replace
BEFORE INSERT ON bks_preview_enrollment_ledger
WHEN EXISTS (SELECT 1 FROM bks_preview_enrollment_ledger WHERE agent_id = NEW.agent_id)
BEGIN
    SELECT RAISE(ABORT, 'immutable preview enrollment');
END;
