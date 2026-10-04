# Inbox federation and callback contract

New permission requests use `inbox.submit` and agent execution. The human submits one complete call-ID verdict mapping in Inbox. A callback only announces the committed state. Agents retrieve `inbox.status` before execution and pass the recovered single-use grant to the approved call.

## Server trust

`GET /.well-known/toolyard-instance` returns the persistent instance ID, protocol `toolyard-federation-v1`, and capabilities. A T3 administrator registers an Ed25519 key through `POST /v1/federation/register` with issuer, key ID, public key, origin, and a fresh Clerk session token. Toolyard independently verifies the administrator and organization membership.

`POST /v1/federation/connect` accepts an EdDSA assertion and expected credential version. Assertions contain `iss`, instance-ID `aud`, verified Clerk subject `sub`, `iat`, `exp`, and unique `jti`. Their lifetime is at most 120 seconds. Credentials belong to the environment, instance, and subject. Renewal uses compare-and-swap and preserves the agent ID. A lost response can recover the same valid credential. Membership outages return 503. Revoked trust, revoked connections, and disabled accounts stop access.

`POST /v1/federation/handoff` accepts an assertion and allowed return URL. Its response contains a one-minute, single-use dashboard URL. The exchange verifies the current connection and account. Long-lived agent credentials do not enter the browser.

`POST /v1/federation/revoke` accepts an assertion and `scope` of `user` or `environment`. Environment revocation requires the independently verified administrator. Removal revokes grants and receiver registrations. An explicit administrator trust registration can reconnect a removed environment. It does not restore individually revoked connections, grants, or receivers.

## Generic receivers

Agents register a receiver with `POST /v1/callbacks/receivers` and their Bearer credential. Fields are `destination`, `secret`, and `client_receiver_id`. The secret uses Standard Webhooks `whsec_` encoding. The response contains an opaque `callback_ref`. A repeated identical registration returns the existing reference. Destinations are immutable.

Public HTTPS receivers require safe DNS resolution. Redirects are disabled. DNS addresses are pinned for each delivery. Private destinations require administrator registration. Registered T3 environments may use only their fixed `/api/session-webhooks/<id>` destination. Other private receivers use `POST /v1/admin/callback-receivers` with the same fields and an administrator-owned `agent_id`. This cookie route retains normal CSRF protection. A changed destination requires a new registration.

`GET /v1/callbacks/receivers/<ref>` returns receiver status and delivery history. `PATCH` accepts `disable`, `rotate`, or `remove`, with `expected_revision`. Rotation includes the new secret. Matching retries are idempotent. Agent-owned manual retry uses `POST /v1/callbacks/retry/<event-id>`. The Inbox detail provides an owner-authorized retry for failed deliveries.

## Decision delivery

Inbox creation and callback registration commit atomically. Final verdicts, reasons, notes, accepted-call grants, audit records, and one outbox event commit atomically. Physical delivery can repeat. Submission retries do not create another logical event.

The signed JSON envelope contains `type`, stable `event_id`, `timestamp`, and `data`. Data includes `inbox_id`, `decision_revision`, status, every call ID/verdict/reason, overall note, expiry metadata, and authenticated status reference. Questions can include their structured answer response. Credentials, grants, approval tokens, and tool results are excluded.

Headers are `webhook-id`, `webhook-timestamp`, and `webhook-signature`. Signature input is `id.timestamp.raw-body`. Version `v1` uses HMAC-SHA256 and Base64. Delivery uses persistent claims, bounded retry delays, attempt history, and terminal failure state. Rotation rebinds pending delivery to the new receiver revision. Disabled, removed, revoked, or expired connections stop delivery.

T3 persists the event and content fingerprint before acknowledgment. Matching duplicate events return success without another turn. Changed content under an existing event ID is rejected. Deterministic command IDs and durable dispatch receipts reconcile lost replies. Busy destinations queue. Human prompts are not answered or interrupted. Paused, archived, deleted, or unauthorized destinations stop dispatch.

## Execution and legacy records

Status inspection never consumes a grant. The authorized agent can recover the same unused valid token. Execution claims are durable and single-use. A write with an uncertain upstream outcome stays `outcome_unknown`; it is not repeated automatically.

Additive migrations represent legacy approvals in Inbox. They preserve original IDs, deadlines, owner evidence, historical results, and Toolyard execution mode. Historical calls receive no new executable grants. Claimed writes remain unknown after a restart. Compatibility decisions go through the Inbox transaction and require the original owner.
