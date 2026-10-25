-- 0052: what a code run's call returned besides a value (v0.1, TEC-1629).
--
-- A resume answers each recorded call from its record, so the record keeps
-- what the script saw for every call, not only for a succeeded one:
--
--   sent         0 for a failed call that never reached the gateway (the
--                script or its clock stopped first): a resume sends it.
--                Rows from before this migration count as sent.
--   error_kind   a failed call's error kind (tool_error, grant_invalid,
--                limit): a resume gives the script the same failure,
--                never a second attempt, since an upstream error may come
--                after a write took effect.
--   error_text   the text the script saw for a failed or held call (at
--                most 64 KiB, grant tokens redacted).
--   approval_id  a deferred call's legacy approval id: a resume asks that
--                approval again instead of making a second one.
--   hold_content a held call's structured content (the Inbox draft), at
--                most 1 MiB: a resume without a grant for the call gives
--                the script the same hold.
--
-- From this version on, a later execution that does not reach a call
-- leaves its record in place (nothing deletes records past the calls it
-- made), so the highest seq of a run is the highest call it ever reached.
--
-- Rolling update or rollback: a binary older than this migration still
-- deletes, when a resume it runs ends, the records past the last call it
-- reached (which lowers the run's mark), and it sends a failed or held
-- call again on a resume. Do not let old and new nodes resume code runs at
-- the same time: drain the old nodes before the new ones serve resumes,
-- and after a rollback start held runs again rather than resume them.
--
-- Same change as migrations/sqlite/0052_code_run_calls_outcome.sql.
ALTER TABLE code_run_calls ADD COLUMN IF NOT EXISTS sent BIGINT NOT NULL DEFAULT 1;
ALTER TABLE code_run_calls ADD COLUMN IF NOT EXISTS error_kind TEXT;
ALTER TABLE code_run_calls ADD COLUMN IF NOT EXISTS error_text TEXT;
ALTER TABLE code_run_calls ADD COLUMN IF NOT EXISTS approval_id TEXT;
ALTER TABLE code_run_calls ADD COLUMN IF NOT EXISTS hold_content TEXT;
