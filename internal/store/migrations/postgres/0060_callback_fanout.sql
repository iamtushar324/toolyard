-- 0060: callback fan-out, generic events and default receivers (v0.1 slice D).
--
-- callback_subscriptions: several receivers may follow one Inbox item, so
-- the primary key becomes (request_id, receiver_id). Existing rows keep
-- their one receiver.
--
-- callback_outbox: one row per event and receiver. event_type names the
-- event ('' on rows from before this migration, whose payload says it).
-- request_id is the subject the event is about (an Inbox item, an approval,
-- a job; '' for a ping). dedupe_key replaces UNIQUE (request_id,
-- decision_revision): one logical event per receiver (for example
-- inbox:<id>:<revision>); NULL on events that need no deduplication.
-- Existing rows get inbox:<request_id>:<decision_revision>.
--
-- callback_subject_subscriptions: receivers that follow something other
-- than an Inbox item, keyed by its id: a legacy deferred approval.
--
-- callback_defaults: an agent's default receiver, used when an Inbox call
-- names none.
--
-- callback_receivers.events lists the event types a receiver accepts;
-- every existing receiver keeps the three types it was built for. v3 = 1
-- opts it into the schema version 3 decision shape.
--
-- callback_outbox.created_at is when the event was queued (retention and
-- the ping quota); existing rows take their delivery or first-attempt time.
--
-- The constraints to replace are found by their columns, not by name.
--
-- Same schema as migrations/sqlite/0060_callback_fanout.sql.

DO $$
DECLARE c text;
BEGIN
  SELECT conname INTO c FROM pg_constraint
   WHERE conrelid = 'callback_subscriptions'::regclass AND contype = 'p';
  IF c IS NOT NULL THEN
    EXECUTE format('ALTER TABLE callback_subscriptions DROP CONSTRAINT %I', c);
  END IF;
  FOR c IN SELECT k.conname FROM pg_constraint k
   WHERE k.conrelid = 'callback_outbox'::regclass AND k.contype = 'u'
     AND array_length(k.conkey, 1) = 2
     AND k.conkey[1] = (SELECT attnum FROM pg_attribute WHERE attrelid = 'callback_outbox'::regclass AND attname = 'request_id')
     AND k.conkey[2] = (SELECT attnum FROM pg_attribute WHERE attrelid = 'callback_outbox'::regclass AND attname = 'decision_revision')
  LOOP
    EXECUTE format('ALTER TABLE callback_outbox DROP CONSTRAINT %I', c);
  END LOOP;
END $$;

ALTER TABLE callback_subscriptions ADD PRIMARY KEY (request_id, receiver_id);

ALTER TABLE callback_receivers ADD COLUMN events TEXT NOT NULL DEFAULT 'inbox.decision,inbox.expired,inbox.cancelled';
ALTER TABLE callback_receivers ADD COLUMN v3 BIGINT NOT NULL DEFAULT 0;

ALTER TABLE callback_outbox ADD COLUMN event_type TEXT NOT NULL DEFAULT '';
ALTER TABLE callback_outbox ADD COLUMN dedupe_key TEXT;
UPDATE callback_outbox SET dedupe_key = 'inbox:' || request_id || ':' || decision_revision;
ALTER TABLE callback_outbox ADD UNIQUE (receiver_id, dedupe_key);
CREATE INDEX callback_outbox_request ON callback_outbox (request_id);
ALTER TABLE callback_outbox ADD COLUMN created_at BIGINT NOT NULL DEFAULT 0;
UPDATE callback_outbox SET created_at = COALESCE(delivered_at, next_attempt_at);
CREATE INDEX callback_outbox_created ON callback_outbox (created_at);

CREATE TABLE callback_subject_subscriptions (
    subject_id        TEXT NOT NULL,
    receiver_id       TEXT NOT NULL REFERENCES callback_receivers(id),
    receiver_revision BIGINT NOT NULL,
    created_at        BIGINT NOT NULL,
    PRIMARY KEY (subject_id, receiver_id)
);

CREATE TABLE callback_defaults (
    agent_id    TEXT PRIMARY KEY,
    receiver_id TEXT NOT NULL REFERENCES callback_receivers(id),
    updated_at  BIGINT NOT NULL
);
