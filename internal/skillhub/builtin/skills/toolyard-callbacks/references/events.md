# Callback events and delivery

Everything a receiver needs: the request toolyard sends, how to verify it,
the data of each event type, and how retries work.

## The request

toolyard POSTs one JSON envelope per event to the receiver's destination:

```json
{ "type": "inbox.decision", "event_id": "evt_…", "timestamp": "<RFC 3339 time, UTC>", "data": { … } }
```

Headers:

| Header | Value |
|---|---|
| `Content-Type` | `application/json` |
| `webhook-id` | the event ID (the same as `event_id`) |
| `webhook-timestamp` | Unix time in seconds when this attempt was signed |
| `webhook-signature` | `v1,` and the Base64 HMAC-SHA256 signature. toolyard sends one; Standard Webhooks allows several, space-separated, so accept any that matches |

Each receiver gets its own `event_id` for the same happening, so two
receivers that follow one item see two different IDs.

## Verify every delivery

This is the Standard Webhooks scheme:

1. Take the secret's bytes: Base64-decode the text after `whsec_`.
2. Compute HMAC-SHA256 over `<webhook-id>.<webhook-timestamp>.<raw body>`
   (the body exactly as received, before any JSON parsing).
3. Base64-encode it and compare it, in constant time, with the text after
   `v1,` of any signature in `webhook-signature`.
4. Refuse a `webhook-timestamp` more than 5 minutes away from your clock.

```python
import base64, hashlib, hmac, time

def verify(secret: str, headers: dict, body: bytes) -> bool:
    key = base64.b64decode(secret.removeprefix("whsec_"))
    msg_id, ts = headers["webhook-id"], headers["webhook-timestamp"]
    if abs(time.time() - int(ts)) > 300:
        return False
    mac = hmac.new(key, f"{msg_id}.{ts}.".encode() + body, hashlib.sha256).digest()
    expected = base64.b64encode(mac).decode()
    return any(hmac.compare_digest(expected, sig.split(",", 1)[1])
               for sig in headers["webhook-signature"].split() if sig.startswith("v1,"))
```

Then store the event, deduplicate on `event_id`, and answer 2xx. Answer
2xx to a duplicate you already stored, too: that ends its retries.

## Event types

| Type | Sent when | Ask for it with |
|---|---|---|
| `inbox.decision` | an Inbox item is fully answered: every call has a verdict and every required question an answer | `inbox.decision`, `inbox.*`, `*` (or no `events`) |
| `inbox.expired` | the item closed without a decision because its deadline passed | `inbox.expired`, `inbox.*`, `*` (or no `events`) |
| `inbox.cancelled` | the item was cancelled, for example withdrawn with `inbox.cancel` | `inbox.cancelled`, `inbox.*`, `*` (or no `events`) |
| `execution.completed` | a granted call ran and toolyard recorded its outcome (once per grant) | `execution.completed`, `execution.*`, `*` |
| `approval.decided` | a legacy deferred approval left pending | `approval.decided`, `approval.*`, `*` |
| `approval.executed` | that approval's call ran and its result was recorded | `approval.executed`, `approval.*`, `*` |
| `job.completed` | an async job succeeded | `job.completed`, `job.*`, `*` |
| `job.failed` | an async job failed, was cancelled, or ended with an unknown outcome | `job.failed`, `job.*`, `*` |
| `ping` | `callbacks.test` | `ping`, `*` |

Drafts, partial answers and owner notes saved without a decision never
emit anything.

## Data of each type

**`inbox.decision`, `inbox.expired`, `inbox.cancelled`**

| Field | Meaning |
|---|---|
| `inbox_id` | the item (`rq_…`) |
| `decision_revision` | the item's revision when it was decided |
| `status` | the event's outcome: `decided`, `expired` or `cancelled` |
| `item_status` | v3 receivers only: the item's status as `inbox.status` reports it (`approved`, `denied`, `returned`, `answered`, `expired`, `cancelled`) |
| `calls` | every call: `call_id`, `verdict` (`accepted` or `rejected`), optional `reason` |
| `overall_note` | the owner's note, if any |
| `request_expires_at`, `grant_expires_at` | the pending deadline, and when the grants expire |
| `answers` | v3 receivers only: the answers keyed by question ID |
| `response` | a version 2 question's structured answer |
| `status_ref` | what to read with `inbox.status` |

A receiver registered without `v3: true` gets the version 2 shape: no
`answers` and no `item_status`. An item with more than 12 calls is never
sent to such a receiver (naming it is refused; as a default it is skipped).

**`execution.completed`**: `inbox_id`, `call_id`, `grant_id`, `tool`,
`state` (`succeeded`, `failed`, `outcome_unknown`), `started_at`,
`finished_at`, `detail`, `status_ref`. The tool's result is not included:
read `inbox.status`, whose execution state is authoritative.

**`approval.decided`**: `approval_id`, `status` (`allowed`, `denied`,
`expired`, `cancelled`), `tool`, `upstream`, `decided_at`, `owner_note`,
`status_ref`. **`approval.executed`** adds `execution_state` (`succeeded`,
`failed`, `outcome_unknown`), `is_error`, `error`, `executed_at`. Arguments
and result are not included: read `tools.poll_approval`.

**`job.completed`, `job.failed`**: `job_id`, `tool`, `upstream`, `status`,
`is_error`, `result_ref` (`/v1/jobs/<job_id>/result`, when there is a
result), `result_bytes`, `error`, `callback_ref`, `finished_at`, and
`may_have_run` (only for an unknown outcome or a cancelled job: check the
target before any new call). The result is not included: read it with
`jobs.result` or `GET result_ref` under your own token.

**`ping`**: `callback_ref`, `message`.

## What to do on each event

| Event | Next step |
|---|---|
| `inbox.decision` | `inbox.status {"ids": ["<inbox_id>"]}`, then run each accepted call with its `_grant` and the approved parameters; follow `owner_note` and per-call reasons |
| `inbox.expired`, `inbox.cancelled` | no grants exist; re-plan, and ask again only if the task still needs it |
| `execution.completed` | `inbox.status`; on `outcome_unknown`, inspect the target before any recovery and never repeat the write blindly |
| `approval.decided`, `approval.executed` | `tools.poll_approval` for the saved result; never reissue the original write |
| `job.completed`, `job.failed` | `jobs.status` / `jobs.result`; never call the tool again for the same work |
| `ping` | nothing; it proves the receiver works |

## Delivery and retries

- At least once. A lost acknowledgement or a lease that ran out can send
  the same event again with the same `event_id`.
- No order. Up to 2 events of one receiver can be in flight at once, and
  retries interleave, so a later event can arrive first (for example
  `execution.completed` before a retried `inbox.decision`). Always read the
  current state from `status_ref`.
- One attempt has 20 seconds. Any 2xx means delivered.
- `410 Gone` and any 3xx (redirects are never followed) fail the event at
  once, and so does a destination that no longer resolves to a public
  address (`destination_invalid`). Other status codes and network errors
  retry after 5 s, 1 min, 5 min, 30 min, 2 h, 6 h, 12 h and 24 h (9 attempts
  in all); after that the event is `failed`.
- `callbacks.list {"callback_ref": "cb_…"}` shows each delivery's `status`
  (`pending`, `delivering`, `delivered`, `failed`), `attempts`,
  `terminal_reason`, and per attempt `at`, `http_status` and `outcome`.
- Retry a failed event yourself with `POST /v1/callbacks/retry/<event-id>`
  (your agent token as the bearer); the schedule starts again. A failed
  `ping` is not retried (409 `conflict`): send a new one with
  `callbacks.test`. Your owner can also retry a failed delivery from the
  Inbox item.
- Delivery stops, and pending events fail, when the receiver is disabled or
  removed, your connection is revoked or expired, or your agent or owner is
  disabled. A rotated secret keeps pending events: they are signed with
  the new secret.
- Delivered and failed events, with their attempt history, are kept for
  30 days.
