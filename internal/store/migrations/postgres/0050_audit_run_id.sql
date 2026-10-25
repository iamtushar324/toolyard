-- 0050: the code-mode run of an audit row (v0.1, TEC-1629).
--
-- run_id ties together the rows of one code-mode run: the outer
-- executeToolCode call ("cm_" id) or REST run ("cr_" id, code_runs.id) and
-- every nested call its script made. NULL for a call outside code mode.
-- The partial index serves the audit filter by run (GET /v1/audit?run_id=).
-- It holds only the rows that have a run id. It is built in the migration,
-- without CONCURRENTLY, so on an existing database it scans audit_events
-- once while writes to the table wait. A fresh database sees no wait.
--
-- Same change as migrations/sqlite/0050_audit_run_id.sql.
-- Rollback: an older binary neither writes nor reads the column.
ALTER TABLE audit_events ADD COLUMN IF NOT EXISTS run_id TEXT;
CREATE INDEX IF NOT EXISTS idx_audit_run ON audit_events (run_id) WHERE run_id IS NOT NULL;
