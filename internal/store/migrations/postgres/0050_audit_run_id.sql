-- 0050: the code-mode run of an audit row (v0.1, TEC-1629).
--
-- run_id ties together the rows of one code-mode run: the outer
-- executeToolCode call ("cm_" id) or REST run ("cr_" id, code_runs.id) and
-- every nested call its script made. NULL for a call outside code mode.
-- The partial index serves the audit filter by run; it holds only the rows
-- that have one, so it builds quickly on an existing audit table.
--
-- Same change as migrations/sqlite/0050_audit_run_id.sql.
ALTER TABLE audit_events ADD COLUMN IF NOT EXISTS run_id TEXT;
CREATE INDEX IF NOT EXISTS idx_audit_run ON audit_events (run_id) WHERE run_id IS NOT NULL;
