---
name: toolyard-callbacks
description: "Register and manage toolyard webhook receivers so that toolyard calls you when an Inbox decision, a granted call's outcome, a legacy approval or an async job is ready, instead of you polling. Covers callbacks.register, callbacks.list, callbacks.update, callbacks.test and callbacks.set_default; the opaque callback_ref you pass to inbox.request, inbox.ask, jobs.start or as _callback_ref; the events (inbox.decision, inbox.expired, inbox.cancelled, execution.completed, approval.decided, approval.executed, job.completed, job.failed, ping); Standard Webhooks signatures (webhook-id, webhook-timestamp, webhook-signature); retries, idle receivers and pull delivery. Use before you send an Inbox item you do not want to wait on, when you build or debug a webhook endpoint, or when an answer shows callback_warning, a callback_ref problem or warning, disabled_reason idle, too many active receivers, or conflict from a callbacks tool."
metadata:
  toolyard-runs: anywhere
---

# Callbacks: let toolyard call you

A callback is toolyard POSTing a signed JSON event to an HTTPS URL you
registered, when something you wait on happens. With a receiver you send an
Inbox item, continue unrelated work or end your turn, and resume when the
event arrives. `inbox.wait` stays available for callers without callbacks.

A callback is an announcement, never permission. It carries no grant
tokens, no credentials and no tool results. Before you act, read the
authoritative state: `inbox.status` for an Inbox item, `tools.poll_approval`
for a legacy approval, `jobs.status` / `jobs.result` for a job.

## When to use

- Before `inbox.request` or `inbox.ask` when the owner may take a while.
- Before a slow async job (`jobs.start`), to hear `job.completed` / `job.failed`.
- Before a call that may answer with a deferred `approval_id` (for example a
  GitHub write), to hear `approval.decided` / `approval.executed`.
- When you build the endpoint that receives toolyard events.
- When an answer shows `callback_warning`, a `callback_ref` problem or
  warning, `disabled_reason: "idle"`, or a callbacks tool says `conflict`.

Without a reachable HTTPS endpoint, skip this: poll with `inbox.wait`
(blocks up to 300 s) or `inbox.status`.

## Who may register

- Any enrolled agent, connected with an agent token. Anonymous callers get
  "the inbox needs an enrolled agent". Each tool acts only on your own
  receivers. The tools are always available (the always-on `callbacks`
  group); code mode names them `toolyard.callbacks_<name>`.
- The destination is a public `https` URL on port 443, with no credentials
  or fragment. toolyard checks DNS at registration and at every delivery,
  pins the address, and never follows redirects. A private address needs an
  administrator's registration, and an administrator can register one only
  for an agent they own. Such a receiver gets only `inbox.decision`,
  `inbox.expired` and `inbox.cancelled`, in the version 2 shape.
- If your host offers a session webhook (a receiver the host provides that
  wakes your session), prefer it: create it with the host's own tool and
  pass the `callback_ref` it gives you. Keep its signing secret out of
  prompts, logs and Inbox items.
- An agent keeps at most 500 active receivers by default (an administrator
  may change it). Disabled, idle and removed receivers do not count.

## Recipe

1. Register once, with `ping` in `events` so you can test it:

   ```text
   callbacks.register
   {"destination": "https://<your-endpoint>/hooks/toolyard", "client_receiver_id": "release-agent-main",
    "events": ["inbox.*", "execution.completed", "job.*", "ping"], "v3": true, "make_default": true,
    "_reason": "Get a webhook when my owner decides the release request, so I can end my turn instead of polling"}
   ```

2. Store `secret` at once. When toolyard generates it, it is shown once and
   never again (`secret_note` says so). Lost it: rotate it with
   `callbacks.update {"callback_ref": "cb_…", "action": "rotate", "expected_revision": <revision>}`.
3. Test delivery: `callbacks.test {"callback_ref": "cb_…"}` queues a signed
   `ping`; read its attempts with `callbacks.list {"callback_ref": "cb_…"}`.
4. Name the receiver on the work that should report to it. On
   `inbox.request` or `inbox.ask`, add `"callback_ref": "cb_…"` to the
   complete request (up to 5 more in `callback_refs`). With neither, your
   default receiver (`callbacks.set_default`) follows the item. For a job:

   ```text
   jobs.start
   {"tool": "<server>.<tool>", "arguments": {"account": "<id>"}, "callback_ref": "cb_…",
    "_reason": "Run the slow backfill my owner asked for as a job and get a webhook when it ends"}
   ```

5. Continue unrelated work or end the turn. Do not also poll in a loop.
6. When an event arrives: verify the signature, refuse a stale timestamp,
   store the event, deduplicate on `event_id`, and answer 2xx fast.
7. Read the current state, then act on it: `inbox.status` for
   `data.status_ref` (an Inbox item), `tools.poll_approval` for an
   approval, `jobs.status` for `data.job_id`. Execute accepted calls with
   the grant from `inbox.status`; a callback never carries one.

Signature check, payload fields and retries: [events and delivery](references/events.md).
Every argument, answer and error: [tools reference](references/tools.md).

## Which receiver hears what

| Work | How you name receivers | Without a name | Receiver must accept |
|---|---|---|---|
| Inbox item | `callback_ref` plus up to 5 `callback_refs` on `inbox.request` / `inbox.ask` | your default receiver | an `inbox.*` event; `v3: true` for more than 12 calls |
| Granted call outcome | follows the item's receivers | same | `execution.completed` |
| Legacy approval (deferred `approval_id`) | `_callback_ref` in the call's arguments | your default receiver | `approval.decided` or `approval.executed` |
| Async job | `callback_ref` on `jobs.start` (or its `_callback_ref`), `_callback_ref` on a call that runs as a job, `callback_ref` in `POST /v1/jobs` | your default receiver | `job.completed`, `job.failed`, `job.*` or `*` |

- A named receiver that cannot carry an Inbox item (not active, no `inbox.*`
  event, or no `v3` for more than 12 calls) is a problem on `callback_ref`
  (or `callback_refs[i]`): the item is not sent. A default that cannot carry it is skipped, with a
  warning on `callback_ref`: name another receiver or poll.
- A refused `_callback_ref` on a legacy approval does not stop the call: its
  deferred answer carries `callback_warning`, and you poll `tools.poll_approval`.
- A job whose `callback_ref` is not one of your active receivers sends
  nothing and records why in `callback_error`.
- A receiver never gets an event type it did not ask for. Registering with
  no `events` gives only `inbox.decision`, `inbox.expired` and `inbox.cancelled`.
- `callback_ref` and `_callback_ref` are at most 512 characters.

## Manage receivers

- `callbacks.list` shows your receivers (`callback_ref`, `destination`,
  `status`, `revision`, `events`, `v3`, `default`); with `callback_ref` also
  its last 100 deliveries and their attempts.
- `callbacks.update` takes `action` (`disable`, `rotate`, `remove`) and
  `expected_revision` from `callbacks.list`. Rotation keeps pending events
  (they are signed with the new secret); disable and remove fail them.
  Remove also clears the receiver as your default.
- `callbacks.set_default` with an empty `callback_ref` clears the default.
- Destination, `events` and `v3` never change. To change one, register a new
  receiver (with a new `client_receiver_id`) and remove the old one.
- Idle rule: an active push receiver with no successful delivery and nothing
  new to follow for 30 days is disabled with `disabled_reason: "idle"` and
  stops counting toward the cap. Using it again wakes it: name it on an item
  or call, make it the default, test it, register it again the same way
  (with the same `secret`), or
  let a new item follow it as the default. A receiver you disabled yourself
  stays disabled.

## Pitfalls

- Treating a callback as approval. It is not: read `inbox.status` first.
- Trusting arrival order. Events have no order; a retry can bring a later
  event first. Read the current state from `status_ref`.
- Acting twice on a repeated delivery. Delivery is at least once:
  deduplicate on `event_id` (also the `webhook-id` header).
- Doing slow work before answering. Each attempt times out after 20 s:
  store the event, answer 2xx, then work.
- Testing a receiver without `ping` in `events`: `callbacks.test` refuses it.
- Registering the same URL twice with other `events` and no
  `client_receiver_id`: the default key comes from the URL, so it is a
  `conflict`. Give each receiver its own `client_receiver_id`.
- Expecting an outer `_callback_ref` to cover nested calls. Each call names
  its own; a call inside code mode or `tools.execute` does not inherit it. A
  tool that declares its own `_callback_ref` parameter keeps it, and
  toolyard neither reads nor strips that argument.
- Logging the secret or a grant token. Never.

## Related skills

- `toolyard-permissions-and-inbox` (`skills.get toolyard/toolyard-permissions-and-inbox`): the request, grants and execution.
- `toolyard-async-jobs` (`skills.get toolyard/toolyard-async-jobs`): jobs and their results.
- `toolyard-github-writes` (`skills.get toolyard/toolyard-github-writes`): deferred approvals for GitHub writes.
- `toolyard-code-mode` (`skills.get toolyard/toolyard-code-mode`): calling `toolyard.callbacks_<name>` from a script.
- `inbox.guide {"topic": "callbacks"}`: the agent protocol's section on receivers and events.
