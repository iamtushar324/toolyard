-- 0060: callback fan-out, generic events and default receivers (v0.1 slice D).
--
-- callback_subscriptions: several receivers may follow one Inbox item, so
-- the key becomes (request_id, receiver_id). Existing rows keep their one
-- receiver.
--
-- callback_outbox: one row per event and receiver. event_type names the
-- event (inbox.decision, inbox.expired, inbox.cancelled,
-- execution.completed, approval.decided, approval.executed, ping, job.*;
-- '' on rows from before this migration, whose payload says it).
-- request_id is the subject the event is about (an Inbox item, an approval,
-- a job; '' for a ping). dedupe_key replaces UNIQUE(request_id,
-- decision_revision): one logical event per receiver (for example
-- inbox:<id>:<revision>); NULL on events that need no deduplication.
-- Existing rows get inbox:<request_id>:<decision_revision>.
--
-- callback_subject_subscriptions: receivers that follow something other
-- than an Inbox item, keyed by its id: a legacy deferred approval
-- (approval.decided, approval.executed).
--
-- callback_defaults: an agent's default receiver, used when an Inbox call
-- names none (callbacks.set_default).
--
-- callback_receivers.events is the comma-separated list of event types a
-- receiver accepts ("*", exact names, or "prefix.*"); every existing
-- receiver keeps the three types it was built for. v3 = 1 opts it into the
-- schema version 3 decision shape (answers, more than 12 calls). Events a
-- receiver did not opt into are never queued for it.
--
-- callback_outbox.created_at is when the event was queued (retention and
-- the ping quota); existing rows take their delivery or first-attempt time.
--
-- SQLite cannot change a table's keys in place: the three tables are
-- rebuilt. callback_attempts is rebuilt with callback_outbox, so its
-- foreign key follows the renamed table and no child row is ever without
-- its parent.
--
-- Same schema as migrations/postgres/0060_callback_fanout.sql.

ALTER TABLE callback_receivers ADD COLUMN events TEXT NOT NULL DEFAULT 'inbox.decision,inbox.expired,inbox.cancelled';
ALTER TABLE callback_receivers ADD COLUMN v3 INTEGER NOT NULL DEFAULT 0;

CREATE TABLE callback_subscriptions_v2 (
 request_id TEXT NOT NULL REFERENCES inbox_requests(id),
 receiver_id TEXT NOT NULL REFERENCES callback_receivers(id),
 receiver_revision INTEGER NOT NULL,
 PRIMARY KEY(request_id, receiver_id)
);
INSERT INTO callback_subscriptions_v2(request_id, receiver_id, receiver_revision)
 SELECT request_id, receiver_id, receiver_revision FROM callback_subscriptions;
DROP TABLE callback_subscriptions;
ALTER TABLE callback_subscriptions_v2 RENAME TO callback_subscriptions;

CREATE TABLE callback_outbox_v2 (
 id TEXT PRIMARY KEY, request_id TEXT NOT NULL, decision_revision INTEGER NOT NULL,
 receiver_id TEXT NOT NULL, receiver_revision INTEGER NOT NULL, payload TEXT NOT NULL,
 status TEXT NOT NULL DEFAULT 'pending', attempts INTEGER NOT NULL DEFAULT 0, retry_base INTEGER NOT NULL DEFAULT 0,
 next_attempt_at INTEGER NOT NULL, claim_until INTEGER NOT NULL DEFAULT 0,
 delivered_at INTEGER, terminal_reason TEXT NOT NULL DEFAULT '',
 event_type TEXT NOT NULL DEFAULT '', dedupe_key TEXT, created_at INTEGER NOT NULL DEFAULT 0,
 UNIQUE(receiver_id, dedupe_key)
);
INSERT INTO callback_outbox_v2(id, request_id, decision_revision, receiver_id, receiver_revision, payload, status,
 attempts, retry_base, next_attempt_at, claim_until, delivered_at, terminal_reason, event_type, dedupe_key, created_at)
 SELECT id, request_id, decision_revision, receiver_id, receiver_revision, payload, status,
 attempts, retry_base, next_attempt_at, claim_until, delivered_at, terminal_reason, '',
 'inbox:' || request_id || ':' || decision_revision, COALESCE(delivered_at, next_attempt_at) FROM callback_outbox;

CREATE TABLE callback_attempts_v2 (
 id INTEGER PRIMARY KEY AUTOINCREMENT, event_id TEXT NOT NULL REFERENCES callback_outbox_v2(id),
 attempt INTEGER NOT NULL, attempted_at INTEGER NOT NULL, http_status INTEGER,
 outcome TEXT NOT NULL, UNIQUE(event_id, attempt)
);
INSERT INTO callback_attempts_v2(id, event_id, attempt, attempted_at, http_status, outcome)
 SELECT id, event_id, attempt, attempted_at, http_status, outcome FROM callback_attempts;

DROP TABLE callback_attempts;
DROP TABLE callback_outbox;
ALTER TABLE callback_outbox_v2 RENAME TO callback_outbox;
ALTER TABLE callback_attempts_v2 RENAME TO callback_attempts;
CREATE INDEX callback_outbox_due ON callback_outbox(status, next_attempt_at);
CREATE INDEX callback_outbox_request ON callback_outbox(request_id);
CREATE INDEX callback_outbox_created ON callback_outbox(created_at);

CREATE TABLE callback_subject_subscriptions (
 subject_id TEXT NOT NULL,
 receiver_id TEXT NOT NULL REFERENCES callback_receivers(id),
 receiver_revision INTEGER NOT NULL,
 created_at INTEGER NOT NULL,
 PRIMARY KEY(subject_id, receiver_id)
);

CREATE TABLE callback_defaults (
 agent_id TEXT PRIMARY KEY,
 receiver_id TEXT NOT NULL REFERENCES callback_receivers(id),
 updated_at INTEGER NOT NULL
);
