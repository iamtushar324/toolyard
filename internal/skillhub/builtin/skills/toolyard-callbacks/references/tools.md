# callbacks.* tools: arguments, answers and errors

Every call also takes `_reason` (20 to 2000 bytes), like any toolyard
call. In code mode the tools are `toolyard.callbacks_register(...)`,
`toolyard.callbacks_list(...)` and so on; `tools.search` lists them under
the server `callbacks`. Each tool acts only on your own receivers.

## A receiver as the tools show it

| Field | Meaning |
|---|---|
| `callback_ref` | the opaque reference (`cb_…`) you pass on items, calls and jobs |
| `destination` | the HTTPS URL toolyard POSTs to (fixed) |
| `transport` | `push` (toolyard POSTs) or `pull` (a host or local connection fetches events over REST) |
| `status` | `active`, `disabled` or `removed` |
| `revision` | starts at 1 and rises with each change you make; pass it as `expected_revision` |
| `client_receiver_id` | your stable key for this receiver |
| `default` | whether it is your default receiver |
| `events` | the event types it accepts, sorted |
| `v3` | whether it accepts the schema version 3 decision shape |
| `environment_id` | only for a federated client environment's own session webhook |
| `disabled_reason` | `idle` when the 30-day idle sweep disabled it; using it again wakes it |

## callbacks.register

| Argument | Type | Notes |
|---|---|---|
| `destination` | string, required | public `https` URL on port 443, no credentials or fragment |
| `secret` | string | Standard Webhooks secret: `whsec_` and 24 to 64 random bytes in Base64. Omitted: toolyard generates 32 random bytes and returns the secret once |
| `client_receiver_id` | string, at most 120 | your stable key, so a retry returns the same `callback_ref`. Default: derived from the destination |
| `environment_id` | string | only for a federated client environment registering its own session-webhook URL |
| `make_default` | boolean | also make it your default receiver |
| `events` | array of strings, at most 24 | names, `"<prefix>.*"` or `"*"`. Default: `inbox.decision`, `inbox.expired`, `inbox.cancelled` |
| `v3` | boolean | accept the version 3 decision shape (`answers`, more than 12 calls). Default false |

Known event names: `approval.decided`, `approval.executed`,
`execution.completed`, `inbox.cancelled`, `inbox.decision`,
`inbox.expired`, `job.completed`, `job.failed`, `ping`.

Answer: the receiver, plus `next`. When toolyard generated the secret, also
`secret` and `secret_note` ("Shown once: store it now …").

Repeats:

- The identical registration (same `client_receiver_id`, destination,
  events, `v3`, and the same secret if you give one) returns the same
  `callback_ref`. Given with the same `secret`, it also wakes a receiver
  the idle sweep disabled.
- A retry without `secret` (same `client_receiver_id` and settings) returns
  the existing active receiver with `secret_returned: false`: the secret is
  never shown again. Rotate if you lost it. Without `secret` it does not
  wake an idle receiver (that is a `conflict`): give the same `secret`, or
  use the receiver again in another way.
- The same `client_receiver_id` with any other setting is a `conflict`.

## callbacks.list

| Argument | Type | Notes |
|---|---|---|
| `callback_ref` | string | also return this receiver's recent deliveries |
| `include_removed` | boolean | also list removed receivers |

Answer: `receivers` (newest first, at most 100), `default` (the default
receiver's `callback_ref`, or empty), and with `callback_ref` also
`deliveries` (its last 100): `event_id`, `inbox_id` (the subject: an
Inbox item, approval or job ID; empty for a ping), `type`, `status`,
`attempts`, `terminal_reason`, and `history` of attempts (`attempt`, `at`,
`http_status`, `outcome`).

## callbacks.update

| Argument | Type | Notes |
|---|---|---|
| `callback_ref` | string, required | the receiver |
| `action` | `disable`, `rotate` or `remove`, required | |
| `expected_revision` | integer >= 1, required | the receiver's current `revision` from `callbacks.list` |
| `secret` | string | `rotate` only: the new `whsec_` secret. Omitted: toolyard generates one and returns it once |

- `rotate` needs an active receiver. Pending events follow the new secret.
- `disable` and `remove` fail the receiver's pending events. `remove` also
  clears it as your default. A receiver you disabled is never woken by use.
- The same change repeated with the old `expected_revision` answers as the
  first one did (for `rotate`, only when you gave the `secret` yourself).

Answer: the receiver after the change, plus `secret` and `secret_note` when
toolyard generated a new secret.

## callbacks.set_default

| Argument | Type | Notes |
|---|---|---|
| `callback_ref` | string, required | the receiver; an empty string clears the default |

The default follows every `inbox.request` / `inbox.ask` and every call that
waits for a legacy approval that names no receiver, and every job without a
`callback_ref`, as long as it accepts that event type. Answer: `default`
and `next`.

## callbacks.test

| Argument | Type | Notes |
|---|---|---|
| `callback_ref` | string, required | the receiver; its `events` must include `ping` (or `*`) |

Queues a signed `ping` and answers `event_id`, `type` (`ping`) and `next`.
A receiver has at most one pending ping: a second test returns the pending
one. Each agent may send 10 pings a minute.

## Errors and what to do

| Message (start) | Meaning | Next step |
|---|---|---|
| `the inbox needs an enrolled agent` | you are not connected with an agent token | reconnect with an agent token |
| `destination: give the https URL …` | no destination | pass `destination` |
| `destination must be an HTTPS URL without credentials or fragment` / `must use HTTPS port 443` | bad URL | fix the URL |
| `destination DNS resolution failed` | the host name did not resolve | fix DNS or the URL |
| `private destinations require administrator registration` | the address is not public | an administrator can register it only for an agent they own; otherwise use your host's session webhook |
| `environment_id: … (environment_destination_mismatch)` | the URL is not that environment's session webhook | leave `environment_id` out |
| `secret must use Standard Webhooks whsec_ encoding` / `must contain 24 to 64 random bytes` | bad secret | fix it, or omit it |
| `events: … is not an event type` / `events: at most 24 entries` | bad `events` | use the known names, `"<prefix>.*"` or `"*"` |
| `conflict: the receiver changed since you read it …` | stale `expected_revision`, or the `client_receiver_id` exists with other settings | read `callbacks.list` and retry, or use another `client_receiver_id` |
| `no active receiver of yours has this callback_ref …` | unknown, inactive or someone else's receiver, or your connection is revoked | list yours with `callbacks.list` |
| `too many active receivers: … at most N …` | you hit the cap | remove or disable unused receivers |
| `this receiver does not accept ping events …` | `ping` is not in its `events` | register a receiver whose `events` include `ping` |
| `at most 10 pings a minute …` | ping quota | wait, then test again |

On `inbox.request` / `inbox.ask`, problems and warnings on `callback_ref`
say why a receiver cannot follow the item: it is not one of your active
receivers, it takes no `inbox.*` event, or it takes the version 2 shape and
the item has more than 12 calls. A named
receiver's problem stops the item; a default receiver's warning does not.

## REST (agent token as the bearer)

| Route | Does |
|---|---|
| `POST /v1/callbacks/receivers` | register: `destination`, `secret`, `client_receiver_id`, optional `environment_id`, `events`, `v3` (over REST, give `secret` and `client_receiver_id` yourself) |
| `GET /v1/callbacks/receivers/<ref>` | `{receiver, deliveries}` |
| `PATCH /v1/callbacks/receivers/<ref>` | `{action, expected_revision, secret}`: `rotate` needs `secret` here |
| `POST /v1/callbacks/retry/<event-id>` | retry one of your failed deliveries (not a ping) |

REST errors: registration answers `409 too_many_receivers`,
`403 environment_destination_mismatch`, or `400` with the validation
message; `GET` and `PATCH` answer `409 conflict` or
`404 receiver_unavailable`, and `PATCH` answers
`400 invalid_change: <reason>` for an unknown `action` or a malformed
`secret`. There is no REST route for `set_default` or
`test`: use the tools.

## Pull delivery (host and local connections only)

A host or local connection whose machine has no public address can
register a pull receiver for its own environment: `POST /v1/callbacks/receivers` with
`transport: "pull"`, its `environment_id`, `client_receiver_id`, `secret`,
and no `destination`. It then reads up to 10 pending signed events at a
time with `GET /v1/callbacks/receivers/<ref>/events` (each with `event_id`,
`body` and the three `webhook-*` headers; reading consumes nothing),
verifies and stores each one, and acknowledges it with
`POST /v1/callbacks/receivers/<ref>/ack {"event_id": "evt_…"}`. A repeated
acknowledgement is safe; an unacknowledged event stays and may be read
again. The idle sweep never disables a pull receiver. Ordinary agents use
push receivers.
