-- 0005: coalesce duplicate pending approvals so a retrying agent / hook
-- doesn't pile up identical cards. Fingerprint is a hex-encoded sha256 of
-- (agent_id, upstream, tool, canonical_args). Filled in by the bus on
-- create; nullable so older rows stay valid.

ALTER TABLE approval_requests ADD COLUMN fingerprint TEXT;

-- Partial unique index — only enforces uniqueness for rows still pending,
-- so reusing the same fingerprint for a future call is fine once the
-- previous one has been decided.
CREATE UNIQUE INDEX IF NOT EXISTS idx_approvals_fingerprint_pending
    ON approval_requests(fingerprint)
    WHERE status = 'pending' AND fingerprint IS NOT NULL;
