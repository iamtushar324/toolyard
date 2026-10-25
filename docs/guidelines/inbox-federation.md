# Inbox federation and callback contract

New permission requests use `inbox.submit` and agent execution. The human submits one complete call-ID verdict mapping in Inbox. A callback only announces the committed state. Agents retrieve `inbox.status` before execution and pass the recovered single-use grant to the approved call.

## Server trust

`GET /.well-known/toolyard-instance` returns the persistent instance ID, protocol `toolyard-federation-v1`, and capabilities. A T3 administrator registers an Ed25519 key through `POST /v1/federation/register` with issuer, key ID, public key, origin, and a fresh Clerk session token. Toolyard independently verifies the administrator and organization membership.

`POST /v1/federation/connect` accepts an EdDSA assertion and expected credential version. Assertions contain `iss`, instance-ID `aud`, verified Clerk subject `sub`, `iat`, `exp`, and unique `jti`. Their lifetime is at most 120 seconds. Credentials belong to the environment, instance, and subject. Renewal uses compare-and-swap and preserves the agent ID. A lost response can recover the same valid credential. Membership outages return 503. Revoked trust, revoked connections, and disabled accounts stop access.

`POST /v1/federation/handoff` accepts an assertion and allowed return URL. Its response contains a one-minute, single-use dashboard URL. Opening the URL verifies the current connection and account, then redirects to the Inbox. The link never signs a browser in: it sets no cookie and creates no session. A browser that already has a Toolyard session shows the Inbox. A browser without one shows the normal sign-in, and the person signs in there. Long-lived agent credentials do not enter the browser. The local and host handoff links behave the same way; the flow is drawn in [dashboard-handoff-design.excalidraw](dashboard-handoff-design.excalidraw).

`POST /v1/federation/revoke` accepts an assertion and `scope` of `user` or `environment`. Environment revocation requires the independently verified administrator. Removal revokes grants and receiver registrations. An explicit administrator trust registration can reconnect a removed environment. It does not restore individually revoked connections, grants, or receivers.

## Generic receivers

Agents register a receiver with the MCP tool `callbacks.register`, or with `POST /v1/callbacks/receivers` and their Bearer credential. Fields are `destination`, `secret`, `client_receiver_id`, and optionally `events` and `v3`.

A receiver gets only what it asked for. `events` lists event types (`inbox.decision`, `inbox.expired`, `inbox.cancelled`, `execution.completed`, `approval.decided`, `approval.executed`, `ping`, `job.completed`, `job.failed`), `"<prefix>.*"`, or `"*"`. Without `events`, over REST and MCP alike, and for every receiver registered before v0.1, it is the three legacy types. `v3: true` accepts the schema version 3 decision shape: `answers` keyed by question ID and more than 12 calls. A receiver without it gets the version 2 shape (no `answers`); an item with more than 12 calls refuses it when named and skips it as a default. `/.well-known/toolyard-instance` advertises `callbacks.events.v1` and `inbox.items.v3`. A repeated registration must repeat the same `events` and `v3`. An agent keeps at most 20 receivers. The secret uses Standard Webhooks `whsec_` encoding. Over MCP the secret may be omitted: toolyard then generates 32 random bytes and returns the secret once, and a retry with the same `client_receiver_id` returns the receiver without it. The response contains an opaque `callback_ref`. A repeated identical registration returns the existing reference. Destinations are immutable.

The MCP tools apply the same validation as the REST routes and act only on the calling agent's receivers:

| tool | REST equivalent | does |
|---|---|---|
| `callbacks.register` | `POST /v1/callbacks/receivers` | register a push receiver; `make_default: true` also makes it the default |
| `callbacks.list` | `GET /v1/callbacks/receivers/<ref>` | list receivers, the default, and (with `callback_ref`) recent deliveries |
| `callbacks.update` | `PATCH /v1/callbacks/receivers/<ref>` | `disable`, `rotate` (a generated secret when none is given), or `remove`, with `expected_revision` |
| `callbacks.set_default` | none | the receiver that follows Inbox items and legacy approvals that name none; an empty `callback_ref` clears it |
| `callbacks.test` | none | queue a signed `ping` event (the receiver's `events` must include `ping`; one pending ping per receiver; 10 pings a minute per agent) |

```json
callbacks.register({ "destination": "https://agent.example.com/hooks/toolyard", "client_receiver_id": "billing-agent",
                     "events": ["inbox.*", "execution.completed", "ping"], "v3": true, "make_default": true })
→ { "callback_ref": "cb_…", "status": "active", "revision": 1, "default": true,
    "events": ["execution.completed", "inbox.*", "ping"], "v3": true,
    "secret": "whsec_…", "secret_note": "Shown once: store it now …" }
```

Several receivers may follow one Inbox item: `callback_ref` plus up to five `callback_refs`. With neither, the agent's default receiver follows the item. Removing a receiver also clears it as the default. Secrets are redacted from audit records.

Public HTTPS receivers require safe DNS resolution. Redirects are disabled. DNS addresses are pinned for each delivery. Private destinations require administrator registration. Registered T3 environments may use only their fixed `/api/session-webhooks/<id>` destination. Other private receivers use `POST /v1/admin/callback-receivers` with the same fields and an administrator-owned `agent_id`. This cookie route retains normal CSRF protection. A changed destination requires a new registration.

`GET /v1/callbacks/receivers/<ref>` returns receiver status and delivery history. `PATCH` accepts `disable`, `rotate`, or `remove`, with `expected_revision`. Rotation includes the new secret. Matching retries are idempotent. Agent-owned manual retry uses `POST /v1/callbacks/retry/<event-id>`; a failed `ping` is not retried (`409 conflict`): send a new one with `callbacks.test`. The Inbox detail provides an owner-authorized retry for failed deliveries.

## Decision delivery

Inbox creation and callback registration commit atomically. Final verdicts, reasons, notes, answers, accepted-call grants, audit records, and one outbox event per following receiver commit atomically. Physical delivery can repeat. Submission retries do not create another logical event: each receiver's event carries a deduplication key (`inbox:<id>:<revision>`).

The signed JSON envelope contains `type`, stable `event_id`, `timestamp`, and `data`. Each receiver gets its own `event_id`. Data includes `inbox_id`, `decision_revision`, status, every call ID/verdict/reason, overall note, expiry metadata, and authenticated status reference. A schema version 3 item adds `answers`, keyed by question ID; a version 2 question includes its structured answer `response`. Credentials, grants, approval tokens, and tool results are excluded.

`inbox.decision` fires once, after the complete decision: every call has a verdict and every required question an answer. Drafts and partial answers never emit. The other event types share the envelope:

| type | emitted | data |
|---|---|---|
| `inbox.decision` | the item is fully answered | as above |
| `inbox.expired`, `inbox.cancelled` | the item closed without a decision | as above |
| `execution.completed` | a granted call's outcome is recorded, in the same transaction | `inbox_id`, `call_id`, `grant_id`, `tool`, `state` (`succeeded`, `failed`, `outcome_unknown`), `started_at`, `finished_at`, `detail`, `status_ref` |
| `approval.decided` | a legacy deferred approval leaves pending, through the bus or the Inbox ledger | `approval_id`, `status`, `tool`, `upstream`, `decided_at`, `owner_note`, `status_ref` |
| `approval.executed` | that approval's call result is recorded | the same, plus `execution_state`, `is_error`, `error`, `executed_at` |
| `ping` | `callbacks.test` | `callback_ref`, `message` |
| `job.completed`, `job.failed` | slice E's async jobs, through `EmitForAgentTx` | the job's data |

Legacy approvals are followed by the call's `_callback_ref` argument (stripped before the tool runs) or the agent's default receiver.

Other packages queue events with `callbacks.Service.EmitForAgentTx(ctx, tx, agentID, ref, eventType, data)` inside their own transaction (`tx` is a `*sql.Tx` of the store): the event commits or rolls back with their write and is then signed, retried and recorded like a decision. `ref` must be an active receiver of `agentID` (else `ErrReceiver`); `ref ""` is the agent's default receiver, read through `tx`, and nothing when there is none. A receiver that did not opt into `eventType` gets nothing.

Delivery runs up to 8 deliveries at once per process. Each event is claimed atomically with a lease, and the claim keeps at most 2 events of one receiver in flight across every node. The picker also keeps about 4 events of one agent and about 4 of one destination host in flight (counted when a node picks, so nodes that pick at the same moment can pass these two by a few). It ranks the due events per receiver, per agent and per host (at most 2, 4 and 4 of each), so a receiver, an agent or a shared host (for example one T3 server) with a long backlog cannot fill the due window and hold back the others. Delivered and failed events and their attempts are deleted after 30 days. Receiver addresses must be public: IPv6 only in global unicast `2000::/3`, and never in a special-use IPv4 or IPv6 range (private, shared, loopback, link-local, documentation, benchmarking, multicast, reserved, and the NAT64, 6to4 and Teredo prefixes that embed IPv4). They are checked at registration and at each delivery.

Events have no order. Retries, leases and parallel deliveries can deliver a later event before an earlier one, for one item or across items (for example `execution.completed` before a retried `inbox.decision`). Do not infer state from the order of arrival: deduplicate on `event_id`, then read the current state from the event's `status_ref` (`inbox.status` for an Inbox item, `tools.poll_approval` for an approval) before acting.

A receiver verifies every delivery before acting on it:

```js
import crypto from "node:crypto";

function verify(secret, headers, rawBody) {
  const key = Buffer.from(secret.replace(/^whsec_/, ""), "base64");
  const id = headers["webhook-id"], ts = headers["webhook-timestamp"];
  if (Math.abs(Date.now() / 1000 - Number(ts)) > 300) return false;
  const expected = crypto.createHmac("sha256", key).update(`${id}.${ts}.${rawBody}`).digest("base64");
  return headers["webhook-signature"].split(" ").some((sig) => {
    const [version, value] = sig.split(",");
    return version === "v1" && value.length === expected.length &&
      crypto.timingSafeEqual(Buffer.from(value), Buffer.from(expected));
  });
}
```

Headers are `webhook-id`, `webhook-timestamp`, and `webhook-signature`. Signature input is `id.timestamp.raw-body`. Version `v1` uses HMAC-SHA256 and Base64. Delivery uses persistent claims, bounded retry delays, attempt history, and terminal failure state. Rotation rebinds pending delivery to the new receiver revision. Disabled, removed, revoked, or expired connections stop delivery.

T3 persists the event and content fingerprint before acknowledgment. Matching duplicate events return success without another turn. Changed content under an existing event ID is rejected. Deterministic command IDs and durable dispatch receipts reconcile lost replies. Busy destinations queue. Human prompts are not answered or interrupted. Paused, archived, deleted, or unauthorized destinations stop dispatch.

## Execution and legacy records

Status inspection never consumes a grant. The authorized agent can recover the same unused valid token. Execution claims are durable and single-use. A write with an uncertain upstream outcome stays `outcome_unknown`; it is not repeated automatically.

Additive migrations represent legacy approvals in Inbox. They preserve original IDs, deadlines, owner evidence, historical results, and Toolyard execution mode. Historical calls receive no new executable grants. Claimed writes remain unknown after a restart. Compatibility decisions go through the Inbox transaction and require the original owner.
