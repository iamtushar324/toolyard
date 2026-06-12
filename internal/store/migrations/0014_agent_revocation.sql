-- Agent token rotation grace + revocation. `disabled` hard-revokes an agent
-- (token stops authenticating immediately); prev_token_hash/prev_token_expires
-- let a rotated-away token keep working for a short grace window so a running
-- agent isn't killed mid-task by a rotation.
ALTER TABLE agents ADD COLUMN disabled INTEGER NOT NULL DEFAULT 0;
ALTER TABLE agents ADD COLUMN prev_token_hash TEXT;
ALTER TABLE agents ADD COLUMN prev_token_expires INTEGER;
