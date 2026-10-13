-- 0037: native uploads for Inbox attachments.
--
-- An agent calls inbox.upload with the file's name, type, size and sha256
-- and gets a single-use upload ticket. It PUTs the bytes to
-- /v1/uploads/{ticket}; the ticket is the credential. Only the ticket's
-- sha256 is stored here. A ticket is bound to one agent, one sha256, one
-- size and one content type, expires after 15 minutes and works once.
-- claimed_until marks a PUT in progress: a second PUT on the same ticket is
-- refused until the first one stores the file (used_at) or fails (the
-- claim is released, so the agent can retry until the ticket expires).
-- claim_token identifies the claim holder: release and completion act only
-- on the caller's own claim, never on a newer one.
CREATE TABLE IF NOT EXISTS inbox_upload_tickets (
    token_hash   TEXT PRIMARY KEY,
    agent_id     TEXT NOT NULL,
    sha256       TEXT NOT NULL,
    size         INTEGER NOT NULL,
    content_type TEXT NOT NULL,
    kind         TEXT NOT NULL CHECK (kind IN ('image', 'video', 'file')),
    name         TEXT NOT NULL,
    created_at   INTEGER NOT NULL,
    expires_at   INTEGER NOT NULL,
    claimed_until INTEGER,
    claim_token  TEXT,
    used_at      INTEGER
);
CREATE INDEX IF NOT EXISTS idx_inbox_upload_tickets_agent ON inbox_upload_tickets(agent_id, expires_at);
CREATE INDEX IF NOT EXISTS idx_inbox_upload_tickets_expires ON inbox_upload_tickets(expires_at);

-- A finished upload, owned by the agent that uploaded it. The bytes live
-- in inbox_blobs (source_url 'upload:<sha256>') and the blob directory.
-- An attachment may cite upload:<sha256> only when a row here names the
-- calling agent. used_at is when an Inbox request first cited it;
-- touched_at is the last time a submission resolved it (so the sweep never
-- deletes an upload a submission is about to cite). Rows never cited are
-- removed after 24 hours with their blob, unless something else still
-- references the blob.
CREATE TABLE IF NOT EXISTS inbox_uploads (
    sha256       TEXT NOT NULL,
    agent_id     TEXT NOT NULL,
    name         TEXT NOT NULL,
    content_type TEXT NOT NULL,
    kind         TEXT NOT NULL,
    size         INTEGER NOT NULL,
    created_at   INTEGER NOT NULL,
    used_at      INTEGER,
    touched_at   INTEGER,
    PRIMARY KEY (sha256, agent_id)
);
CREATE INDEX IF NOT EXISTS idx_inbox_uploads_unused ON inbox_uploads(used_at, created_at);
