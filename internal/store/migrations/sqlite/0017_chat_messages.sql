-- Chat-channel approval notifications (Telegram first). One row per
-- (approval, channel) tracks the message we posted so we can edit it in place
-- when the approval is decided/expired/cancelled and reconcile after a restart.
CREATE TABLE IF NOT EXISTS chat_messages (
  approval_id TEXT NOT NULL,
  channel     TEXT NOT NULL,           -- 'telegram' (Slack later)
  chat_id     TEXT NOT NULL,
  message_id  TEXT NOT NULL,
  created_at  INTEGER NOT NULL,
  resolved_at INTEGER,
  PRIMARY KEY (approval_id, channel)
);

-- Reconciler scans unresolved rows; a partial index keeps that cheap.
CREATE INDEX IF NOT EXISTS idx_chat_messages_unresolved
  ON chat_messages(channel, approval_id) WHERE resolved_at IS NULL;
