-- 0063: idle push receivers (v0.1 slice D follow-up).
--
-- The hourly callback sweep disables an active push receiver that had no
-- successful delivery and nothing new to follow (no Inbox item, legacy
-- approval or event addressed to it, no default change) for 30 days:
-- status 'disabled', disabled_reason 'idle'. An idle receiver does not
-- count toward the per-agent cap. When its agent names it again (or makes
-- the identical registration again), it is active again and woken_at
-- records when, so the next sweep does not disable it at once. A
-- receiver its agent or owner disabled keeps disabled_reason '' and stays
-- disabled.
--
-- The two indexes let the sweep find a receiver's subscriptions.
--
-- Rollback: a binary older than this migration ignores both columns, so
-- an idle receiver reads as plainly disabled and is never woken.
--
-- Same schema as migrations/postgres/0063_callback_receiver_idle.sql.

ALTER TABLE callback_receivers ADD COLUMN disabled_reason TEXT NOT NULL DEFAULT '';
ALTER TABLE callback_receivers ADD COLUMN woken_at INTEGER NOT NULL DEFAULT 0;
CREATE INDEX callback_subscriptions_receiver ON callback_subscriptions (receiver_id);
CREATE INDEX callback_subject_subscriptions_receiver ON callback_subject_subscriptions (receiver_id);
