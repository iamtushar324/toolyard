# Decisions, grants and execution

`inbox.guide({"topic": "grants"})` serves the same contract from the server.

## What the owner submits

The owner reviews the item and explicitly submits ONE complete decision. No
checkbox, draft or note reaches you before that.

- A verdict for every call: `accepted` or `rejected`, each with an optional
  written reason (at most 4000 characters).
- An optional overall note (at most 10000 characters). You read it as
  `owner_note`.
- Optional narrowing of an accepted call: fewer values, an exact value
  instead of a `limit` or range, a tighter range, or a shorter grant lifetime
  (60 seconds up to what you asked). The owner can never widen a call.
- Answers to the item's questions, if it has any.

The owner can also deny the whole item, send it back to replan (`returned`),
or snooze it (it stays `pending` with `snoozed_until`). `required` is only a
planning hint: rejecting a required call, or every call, is a complete and
valid decision. The decision, the grants for accepted calls, the audit record
and the callback event commit together.

## The status view

`inbox.status` takes `ids` (a list of up to 32 `rq_…` IDs; `request_id` works
for one) and never blocks. `inbox.wait` returns the same view.

```json
{"requests": [{
  "request_id": "rq_…", "kind": "access", "status": "approved", "revision": 1,
  "owner_note": "Do not touch the flag; I will roll it out myself.",
  "expires_at": 1790620000000, "grants_expire_at": 1790621520000,
  "tools": [
    {"call_id": "deploy_api", "tool": "deploy.run", "verdict": "accepted", "decision": "allowed",
     "reason": "The release is ready.", "grant_id": "gr_…", "grant": "tyg_…",
     "narrowed": true, "params": {"service": {"eq": "api"}, "env": {"eq": "prod"}, "ref": {"eq": "7c1d2e9"}},
     "execution": {"grant_id": "gr_…", "request_id": "rq_…", "call_id": "deploy_api", "state": "not_started"}},
    {"call_id": "set_flag", "tool": "flags.set", "verdict": "rejected", "decision": "refused",
     "reason": "I will flip the flag myself."}],
  "next": "Call each allowed tool with its grant as `_grant`, …"}]}
```

| field | meaning |
|---|---|
| `status` | `pending`, `approved`, `denied`, `returned`, `answered`, `cancelled`, `expired` (`not_found` for an ID that is not yours). |
| `next` | What to do now. Always read it. |
| `owner_note` | Follow it, also for accepted calls. |
| `grants_expire_at` | When the grants of this item lapse (milliseconds since the epoch). |
| `tools[].verdict` | `accepted` or `rejected`. `decision` says the same as `allowed`, `refused` or `pending`. |
| `tools[].reason` | The owner's reason for this call, if written. |
| `tools[].grant` | The token, present while the grant is valid and unused. |
| `tools[].grant_note` | Present instead of `grant` when the permission is used, expired or revoked: inspect `execution`. |
| `tools[].narrowed`, `params` | When `narrowed` is true, `params` is what the grant allows. Use it. |
| `tools[].execution` | The execution record of the granted call (below). |
| `answers` | Answers to the item's questions, keyed by question ID. |

## Using a grant

Call the accepted tool with `_grant` added to its arguments:

- Pass exactly the approved arguments: the values you requested, or the
  narrowed `params`. Every argument you pass must have been listed in
  `params`, and every constrained argument must be present.
- A `limit` parameter is checked against its `pattern` with the real value.
- One grant, one execution: `max_uses` is 1. Make one call per accepted
  `call_id`.
- The grant is bound to your agent, the tool, the call and its scope. Another
  agent cannot use it, and neither can a call to a different tool.
- The scope lives on the server, so a revocation or narrowing takes effect at
  once.
- A grant only matters for a call that needs permission. An explicit deny
  still wins, and an open tool does not use a grant up.
- Grants need an enrolled agent token; an anonymous connection cannot use one.
- If the tool is asynchronous, the granted call answers with a `job_id`
  instead of the result. Read it with `jobs.wait`, `jobs.status` or
  `jobs.result` and never call the tool again for the same work. See the
  `toolyard-async-jobs` skill (`skills.get toolyard/toolyard-async-jobs`).

Lifetimes are separate: a pending item waits `pending_ttl_seconds` for a
decision (default 24 hours); a grant lasts `ttl_seconds` after acceptance
(default 1800 seconds, at most 86400). An expired grant does not revive.

Lost the response? Read `inbox.status` again. It returns the same still-valid,
unused token. Reading never consumes, renews or replaces permission. Keep
tokens out of logs, commits, chat, callbacks and Inbox items.

## When the call answers grant_invalid

Nothing ran. The answer has `status: "grant_invalid"`, `reason`, `next`, and,
when known, `grant_id`, `grant_tool` and `grant_expires_at`.

| reason | what to do |
|---|---|
| `the call is outside the grant's parameters` | Call again within the grant's params, or send a new `inbox.request` (a draft is attached) and say what changed. |
| `this grant has already been used` | It already has an execution claim. Inspect its execution in `inbox.status`. A timeout or lost response can mean the action completed: never repeat an uncertain write. |
| `this grant has expired` | If you still need it, send a new `inbox.request` (draft attached) and say it is a re-request. |
| `your owner revoked this grant` | Stop, and report where you stopped with `inbox.post`. |
| `the grant token is malformed`, `no such grant`, `this grant was issued to a different agent`, `this grant is for a different tool` | Use the exact token `inbox.status` gave you, for the tool it was issued for. |

`call parameters no longer match the tool schema; permission was not consumed`
means the tool's schema changed since the request. The grant is still unused.

## Execution state

| state | meaning |
|---|---|
| `not_started` | The grant exists and no call has claimed it. |
| `running` | The call claimed the grant and runs now. |
| `succeeded` | The call finished; `result` holds what the tool returned. |
| `failed` | The tool returned an error. The grant is used. |
| `outcome_unknown` | The write may have committed: the upstream transport failed after dispatch, no outcome came back, or the process stopped after the claim. |

On `outcome_unknown`, or on `execution outcome could not be recorded; do not
repeat this call; inspect Inbox status`:

1. Do not call the tool again with the same intent.
2. Inspect the upstream state with a read tool (does the deploy, row, message
   or file exist?).
3. Report what you found with `inbox.post`. If the action did not happen and
   you still need it, send a new `inbox.request` that explains the unknown
   outcome and the evidence that it did not commit.

A registered receiver can also get an `execution.completed` event (`state`
`succeeded`, `failed` or `outcome_unknown`). It carries no tool result: read
`inbox.status`.

## Legacy approvals

Some calls answer with a deferred approval instead of `permission_required`:
`status: "pending_approval"`, an `approval_id` (`ap_…`), `expires_at`,
`reason_to_human` (your `_reason`), `expected_decision` and
`next_steps_for_agent`. This is the older approval path: approvals that were
already pending when the Inbox took over, gateways that run without the
Inbox, and owner-only GitHub writes (see the `toolyard-github-writes` skill).
Toolyard runs the original call itself the moment it is allowed, with the
arguments it saved. The owner cannot change those arguments.

| tool | use |
|---|---|
| `tools.poll_approval` | `approval_id`. Non-blocking status and, once executed, the result. |
| `tools.poll_approvals` | `approval_ids` (up to 32) in one call. |
| `tools.wait_for_approval` | `approval_id`, `timeout_seconds` (1-300, default 60). Blocks until executed, denied, expired or cancelled. |
| `tools.wait_for_approvals` | `approval_ids` (up to 32), `mode` `all` (default) or `any`, `timeout_seconds` (1-300, default 60). |
| `tools.list_my_pending_approvals` | Your still-pending approvals, after a restart or lost IDs. |
| `tools.cancel_my_approval` | `approval_id`. Withdraw one of your own pending approvals. |
| `tools.approval_stats` | Recent decision times (optional `tool_name`, `upstream`) and your pending count. |

```text
tools.wait_for_approval
{"approval_id": "ap_…", "timeout_seconds": 120,
 "_reason": "Collect the saved result of the approved call instead of calling it again"}
```

| `status` | meaning and next step |
|---|---|
| `pending` | Still waiting. Keep the ID; poll or wait. Do not call the tool again. |
| `allowed` | Allowed and executing now. Poll again shortly. |
| `executed` | Done. `result.content` and `result.structured_content` (with `is_error`) are what the tool returned; `execution_error` is set when it failed. |
| `denied` | Do not fire the same call again. Tell the user what was declined and ask for an alternative. |
| `expired` | Nobody decided in time. The call never ran. If still needed, ask again (for a restricted tool on an Inbox gateway that means a new `inbox.request`). |
| `cancelled` | Withdrawn by you or an operator. The call never ran. |
| `outcome_unknown` | The upstream outcome is uncertain. Inspect upstream state. Do not repeat the write with this approval. |

Every snapshot carries a `how_to_proceed`, `how_to_get_result` or
`how_to_use_result` hint; follow it. Never reissue the original write while
an approval is pending, allowed, executed or `outcome_unknown`: the poll
answer holds the saved result. An identical call made while one is pending
joins the same approval rather than creating a second one, but do not rely on
that. Too many pending approvals answers `agent_pending_budget_exceeded`:
poll, wait or cancel some first. Re-calling a tool with `_approval_id` still
works but is deprecated; use the polling tools.

To get an `approval.decided` and `approval.executed` callback instead of
polling, add `_callback_ref` to the original call. See the
`toolyard-callbacks` skill.
