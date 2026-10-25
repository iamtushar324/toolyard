-- 0067: a shorter life for recorded task answers, and nothing_ran (v0.1 slice E2).
--
-- expires_at on tool_jobs (Unix milliseconds) is set on the rows toolyard
-- records only so an MCP client gets a task for a call that was answered
-- at once (refused, held or answered inline): such a row is removed an
-- hour after it was written, not 7 days after it finished, and one agent
-- may hold only so many of them (counted through idx_tool_jobs_expires_agent).
-- NULL for a real job, which keeps the usual retention.
--
-- nothing_ran is 1 when toolyard knows the job's call was never sent (it
-- never started, its recheck refused it, or it was cancelled first), so a
-- cancelled job reports may_have_run false; 0 when it may have run.
--
-- Same columns as migrations/postgres/0067_tool_jobs_expiry.sql.
-- Rollback: an older binary ignores the column and keeps those rows 7 days.
ALTER TABLE tool_jobs ADD COLUMN expires_at INTEGER;
ALTER TABLE tool_jobs ADD COLUMN nothing_ran INTEGER NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS idx_tool_jobs_expires ON tool_jobs(expires_at);
CREATE INDEX IF NOT EXISTS idx_tool_jobs_expires_agent ON tool_jobs(agent_id, expires_at);
