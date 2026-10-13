-- Additive server trust, environment credentials, and durable decision delivery.
CREATE TABLE federation_instance (id TEXT PRIMARY KEY);
CREATE TABLE federation_issuers (
 issuer TEXT PRIMARY KEY, public_key TEXT NOT NULL, kid TEXT NOT NULL,
 origin TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'active',
 registered_by TEXT NOT NULL, created_at INTEGER NOT NULL
);
CREATE TABLE federation_replays (issuer TEXT NOT NULL, jti TEXT NOT NULL, expires_at INTEGER NOT NULL, PRIMARY KEY(issuer,jti));
CREATE TABLE federation_connections (
 issuer TEXT NOT NULL, subject TEXT NOT NULL, user_id TEXT NOT NULL,
 agent_id TEXT NOT NULL, credential_enc TEXT NOT NULL, version INTEGER NOT NULL,
 status TEXT NOT NULL DEFAULT 'active', expires_at INTEGER NOT NULL,
 PRIMARY KEY(issuer,subject), UNIQUE(agent_id)
);
CREATE TABLE federation_handoffs (
 code_hash TEXT PRIMARY KEY, issuer TEXT NOT NULL, user_id TEXT NOT NULL,
 return_url TEXT NOT NULL, expires_at INTEGER NOT NULL, consumed_at INTEGER
);
CREATE TABLE callback_receivers (
 id TEXT PRIMARY KEY, agent_id TEXT NOT NULL, environment_id TEXT NOT NULL DEFAULT '',
 client_receiver_id TEXT NOT NULL, destination TEXT NOT NULL,
 secret_enc TEXT NOT NULL, revision INTEGER NOT NULL DEFAULT 1,
 status TEXT NOT NULL DEFAULT 'active', private_allowed INTEGER NOT NULL DEFAULT 0,
 created_at INTEGER NOT NULL, UNIQUE(agent_id,client_receiver_id)
);
CREATE TABLE callback_subscriptions (
 request_id TEXT PRIMARY KEY REFERENCES inbox_requests(id),
 receiver_id TEXT NOT NULL REFERENCES callback_receivers(id),
 receiver_revision INTEGER NOT NULL
);
CREATE TABLE callback_outbox (
 id TEXT PRIMARY KEY, request_id TEXT NOT NULL, decision_revision INTEGER NOT NULL,
 receiver_id TEXT NOT NULL, receiver_revision INTEGER NOT NULL, payload TEXT NOT NULL,
 status TEXT NOT NULL DEFAULT 'pending', attempts INTEGER NOT NULL DEFAULT 0, retry_base INTEGER NOT NULL DEFAULT 0,
 next_attempt_at INTEGER NOT NULL, claim_until INTEGER NOT NULL DEFAULT 0,
 delivered_at INTEGER, terminal_reason TEXT NOT NULL DEFAULT '',
 UNIQUE(request_id,decision_revision)
);
CREATE INDEX callback_outbox_due ON callback_outbox(status,next_attempt_at);
CREATE TABLE callback_attempts (
 id INTEGER PRIMARY KEY AUTOINCREMENT, event_id TEXT NOT NULL REFERENCES callback_outbox(id),
 attempt INTEGER NOT NULL, attempted_at INTEGER NOT NULL, http_status INTEGER,
 outcome TEXT NOT NULL, UNIQUE(event_id,attempt)
);
