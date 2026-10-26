-- 0070: skills hub Inbox items (TEC-1782, slice C).
--
-- skill_proposals is one skills hub question that waits for a person in the
-- Inbox, with what toolyard does on the answer:
--
--   kind 'source'  an agent's skills.propose_source: agent_id proposed it,
--                  user_id (the agent's owner) decides; input is the
--                  proposed source (JSON SourceInput, never a token);
--                  result is the created source's ID.
--   kind 'review'  a new commit of a review-mode source: user_id (the
--                  source's creator) decides; source_id and version_id
--                  name the staged version; input is the version summary.
--   kind 'change'  reserved for skills.propose_change (slice D).
--
-- status: pending (waits in the Inbox), approved (approved, being
-- applied), done, rejected, failed (error says why) or cancelled (a newer
-- commit or a dashboard decision replaced the item). inbox_id is the
-- Inbox item. reason is the agent's own words. decided_by is the deciding
-- user's ID. Times are Unix milliseconds; decided_at stays NULL until a
-- decision.
--
-- Same table as migrations/sqlite/0070_skill_proposals.sql.
-- Rollback: an older binary ignores the table.
CREATE TABLE IF NOT EXISTS skill_proposals (
    id          TEXT PRIMARY KEY,
    kind        TEXT NOT NULL,
    status      TEXT NOT NULL,
    agent_id    TEXT NOT NULL DEFAULT '',
    user_id     TEXT NOT NULL,
    source_id   TEXT NOT NULL DEFAULT '',
    version_id  TEXT NOT NULL DEFAULT '',
    inbox_id    TEXT NOT NULL DEFAULT '',
    input       TEXT NOT NULL DEFAULT '{}',
    reason      TEXT NOT NULL DEFAULT '',
    result      TEXT NOT NULL DEFAULT '',
    error       TEXT NOT NULL DEFAULT '',
    decided_by  TEXT NOT NULL DEFAULT '',
    created_at  BIGINT NOT NULL,
    updated_at  BIGINT NOT NULL,
    decided_at  BIGINT
);
CREATE INDEX IF NOT EXISTS idx_skill_proposals_agent ON skill_proposals (agent_id, created_at);
CREATE INDEX IF NOT EXISTS idx_skill_proposals_source ON skill_proposals (source_id, kind, status);
CREATE INDEX IF NOT EXISTS idx_skill_proposals_inbox ON skill_proposals (inbox_id);
