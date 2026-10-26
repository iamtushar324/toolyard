# GitHub approvals: states, answers and errors

## The deferred answer

Calling a GitHub write tool answers at once with `status: "pending_approval"`.
Useful fields:

| field | meaning |
|---|---|
| `approval_id` | The `ap_…` ID. Keep it; everything else uses it. |
| `expires_at` | When the request expires without a decision. |
| `reason_to_human` | Your `_reason`, as the owner sees it. |
| `expected_decision` | Typical decision time (`in_seconds_p50`, `in_seconds_p90`, `based_on`, `samples`). |
| `your_other_pending` | Your other pending approval IDs, for one batch poll. |
| `min_poll_interval_seconds` | Do not poll faster than this. |
| `next_steps_for_agent` | Ready calls for polling, waiting, cancelling and listing. |
| `callback_warning` | Present when a `_callback_ref` you passed will get no callback. |

## Collecting the result

| tool | arguments |
|---|---|
| `tools.poll_approval` | `approval_id` |
| `tools.poll_approvals` | `approval_ids` (up to 32) |
| `tools.wait_for_approval` | `approval_id`, `timeout_seconds` (1-300, default 60) |
| `tools.wait_for_approvals` | `approval_ids` (up to 32), `mode` `all` (default) or `any`, `timeout_seconds` (1-300, default 60) |
| `tools.list_my_pending_approvals` | none: your pending approvals |
| `tools.cancel_my_approval` | `approval_id`: refuses a GitHub request (`only the account owner can decide this GitHub request`) |

An answer is a snapshot with `approval_id`, `status`, `raw_status`,
`tool`, `queued_at`, `expires_at`, and, once decided, `decided_at` and
`decision_latency_seconds` (an `outcome_unknown` answer carries only
`approval_id`, `status`, `raw_status`, `execution_state` and
`how_to_proceed`). Follow its `how_to_proceed`, `how_to_get_result`
or `how_to_use_result` hint. Only the agent that raised an owner-only GitHub
request, under the same owner, can read it; any other caller gets
`unknown approval_id`.

## Statuses

| `status` | meaning | do |
|---|---|---|
| `pending` | Waiting for the account owner. | Wait or poll. Do not call the write again. |
| `allowed` | Allowed; toolyard is sending the write now. | Poll again shortly. |
| `executed` | Toolyard sent the write and saved the answer. | Read `result.content` and `result.structured_content`; check `result.is_error` and `execution_error`. |
| `denied` | The owner refused it. | Do not send the same call again. Tell your user and ask for another way forward. |
| `expired` | No decision before `expires_at`. Nothing was posted. | If still relevant, call the write tool again for a fresh request. |
| `cancelled` | Withdrawn. Nothing was posted. | Call again only if it is needed again. |
| `outcome_unknown` | The write may have reached GitHub. | Read the pull request's comments or reviews first. Never repeat it with this approval. |

`unknown` in a batch answer means the ID does not exist or is not yours.

## Errors before a request exists

These come back at once; no approval was created.

| message | cause and fix |
|---|---|
| `unsupported GitHub argument "<name>"` | Pass only the arguments in the tool table. |
| `pull_number must be a positive integer` | Give the number, not a URL or string. |
| `page must be a positive integer` | `page` starts at 1. |
| `body must contain between 1 and 60000 bytes` | Write a non-empty body; trim it below the limit. |
| `event must be COMMENT, REQUEST_CHANGES, or APPROVE` | Use one of the three events. |
| `an inline comment requires path, positive line, and LEFT or RIGHT side` | Give all three for `create_review_comment`. |
| `GitHub writes require an open pull request` | The pull request is closed or merged. |
| `GitHub requires an agent with a user owner` | Your agent has no person as owner; ask the operator. |
| `connect your GitHub account in My connections: …` | Your owner must connect GitHub in toolyard. Never ask for a token. |
| `GitHub approval context is reserved for Toolyard` | Do not pass toolyard's internal `_toolyard_github` field. |
| `denied by policy: …` | An explicit deny rule blocks this tool for you. Do not try another route. |
| `agent_pending_budget_exceeded` | Too many of your approvals are pending. Poll, wait or cancel some first. |

## Errors when toolyard sends an allowed write

Before it posts, toolyard checks the request against the live state. These
come back in the executed result; nothing was posted.

| message | meaning |
|---|---|
| `the pull request changed; request fresh approval` | A new commit was pushed, or the pull request closed, after you proposed. |
| `the GitHub account changed; request fresh approval` | The owner's connection now points at a different GitHub account. |
| `the GitHub connection changed; request fresh approval` | The token behind the owner's GitHub connection is not the one seen when you proposed: a reconnect, or a routine token refresh. |
| `the GitHub write could not be serialized: …` | Toolyard could not lock the write safely. |

After one of these, read the pull request again. Make one new proposal only
if your text still applies to the current head.

## Duplicate protection

Toolyard appends a hidden operation marker to every body it posts. Before it
posts, it looks for an existing post with that marker from the same GitHub
account and returns that post instead of posting again. It refuses the write
if it cannot rule out a duplicate. This protects against restarts; it is not a
reason to re-issue a write.
