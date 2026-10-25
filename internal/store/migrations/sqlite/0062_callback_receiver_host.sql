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
-- Same schema as migrations/postgres/0062_callback_receiver_host.sql.

ALTER TABLE callback_receivers ADD COLUMN host TEXT NOT NULL DEFAULT '';

UPDATE callback_receivers SET host = lower(CASE
  WHEN substr(destination, instr(destination, '://') + 3, 1) = '[' THEN
    substr(destination, instr(destination, '://') + 4, instr(destination, ']') - instr(destination, '://') - 4)
  ELSE
    substr(replace(replace(replace(substr(destination, instr(destination, '://') + 3), '?', '/'), '#', '/'), ':', '/') || '/', 1,
      instr(replace(replace(replace(substr(destination, instr(destination, '://') + 3), '?', '/'), '#', '/'), ':', '/') || '/', '/') - 1)
  END)
 WHERE transport = 'push' AND instr(destination, '://') > 0;
