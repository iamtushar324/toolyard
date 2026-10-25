-- 0062: the destination host of each push receiver (v0.1 slice D follow-up).
--
-- callback_receivers.host is the lowercase host name of destination (an
-- IPv6 literal without its brackets; '' for a pull receiver). The delivery
-- picker ranks due events per host, so one busy shared host (for example
-- one T3 server with many receivers) cannot fill the due window and hold
-- back the others. New receivers get it from Go (url.Hostname); existing
-- push receivers get it here, from the text after "://" up to the first
-- "/", "?", "#" or ":".
--
-- Rollback: a binary older than this migration ignores host and inserts
-- receivers without it (''); after an upgrade again the picker counts such
-- a receiver's destination as its host.
--
-- Same schema as migrations/sqlite/0062_callback_receiver_host.sql.

ALTER TABLE callback_receivers ADD COLUMN host TEXT NOT NULL DEFAULT '';

UPDATE callback_receivers SET host = lower(COALESCE(CASE
  WHEN destination ~ '^[A-Za-z][A-Za-z0-9+.-]*://\[' THEN
    substring(destination from '^[A-Za-z][A-Za-z0-9+.-]*://\[([^]]*)\]')
  ELSE
    substring(destination from '^[A-Za-z][A-Za-z0-9+.-]*://([^/?#:]*)')
  END, ''))
 WHERE transport = 'push';
