-- 0043: execution leases for high availability (TEC-1630, v0.1 slice A).
--
-- Several replicas may share one Postgres database. A row that a node
-- executes names that node (owner_instance, its cluster instance ID) and
-- holds a lease (lease_until, Unix milliseconds) that the node renews
-- while the work runs. Startup and periodic recovery take only rows whose
-- lease ended (or that have none: rows from before this migration), so a
-- node that restarts never marks another live node's work
-- outcome_unknown or runs it twice.
--
-- Covered: Inbox executions of granted calls, legacy execution claims,
-- code-mode runs over REST and memory-webhook ingest jobs. Both columns
-- stay NULL on rows that never ran.
--
-- Same columns as migrations/sqlite/0043_ha_work_leases.sql.
-- Rollback: an older binary ignores the columns.
ALTER TABLE inbox_executions ADD COLUMN owner_instance TEXT;
ALTER TABLE inbox_executions ADD COLUMN lease_until BIGINT;
ALTER TABLE legacy_execution_claims ADD COLUMN owner_instance TEXT;
ALTER TABLE legacy_execution_claims ADD COLUMN lease_until BIGINT;
ALTER TABLE code_runs ADD COLUMN owner_instance TEXT;
ALTER TABLE code_runs ADD COLUMN lease_until BIGINT;
ALTER TABLE memory_webhook_jobs ADD COLUMN owner_instance TEXT;
ALTER TABLE memory_webhook_jobs ADD COLUMN lease_until BIGINT;
