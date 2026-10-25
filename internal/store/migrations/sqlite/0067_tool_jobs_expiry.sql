-- 0067: a shorter life for recorded task answers (v0.1 slice E2).
--
-- expires_at on tool_jobs (Unix milliseconds) is set on the rows toolyard
-- records only so an MCP client gets a task for a call that was answered
-- at once (refused, held or answered inline): such a row is removed an
-- hour after it was written, not 7 days after it finished, and one agent
-- may hold only so many of them (counted through idx_tool_jobs_expires_agent).
-- NULL for a real job, which keeps the usual retention.
--
-- Same column as migrations/postgres/0067_tool_jobs_expiry.sql.
-- Rollback: an older binary ignores the column and keeps those rows 7 days.
ALTER TABLE tool_jobs ADD COLUMN expires_at INTEGER;
CREATE INDEX IF NOT EXISTS idx_tool_jobs_expires ON tool_jobs(expires_at);
CREATE INDEX IF NOT EXISTS idx_tool_jobs_expires_agent ON tool_jobs(agent_id, expires_at);
