-- 0061: at most one open ping per receiver (v0.1 slice D follow-up).
--
-- callbacks.test returns the open ping of a receiver instead of queuing a
-- second one. The partial unique index makes that hold when two nodes
-- test the same receiver at once: the second insert conflicts instead of
-- queuing a duplicate. Existing open pings beyond one per receiver (the
-- smallest id stays) are failed first (terminal_reason duplicate_ping), so
-- the index can be built.
--
-- Rollback: a binary older than this migration never queues pings, so the
-- index never refuses its inserts; it ignores the index otherwise.
--
-- Same schema as migrations/postgres/0061_callback_one_ping.sql.

UPDATE callback_outbox SET status = 'failed', terminal_reason = 'duplicate_ping'
 WHERE event_type = 'ping' AND status IN ('pending', 'delivering')
   AND id NOT IN (SELECT min(id) FROM callback_outbox
                   WHERE event_type = 'ping' AND status IN ('pending', 'delivering')
                   GROUP BY receiver_id);

CREATE UNIQUE INDEX callback_outbox_one_ping ON callback_outbox (receiver_id)
 WHERE event_type = 'ping' AND status IN ('pending', 'delivering');
