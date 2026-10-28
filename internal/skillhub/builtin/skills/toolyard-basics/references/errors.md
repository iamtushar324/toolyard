# toolyard errors and what to do

Read the whole answer first: most refusals carry structured content with a
`status`, `executed: false` and a `next` or `message` that says what to do.
Quoted texts below are what toolyard answers; `<…>` marks a part that varies.

## Before anything ran

| Answer | Meaning | Do this |
|---|---|---|
| `_reason is required: provide a one-sentence rationale (20-2000 chars)` | No `_reason`. Nothing ran. | Add one sentence on why, 20-2000 bytes (non-ASCII letters count more than one), and call again. |
| `_reason too short (got <n> chars, need 20)`, `_reason too long (…)`, `_reason must be a string` | Bad `_reason`. Nothing ran. The counts are bytes, despite the word "chars". | Fix it and call again. If the tool's schema names `__toolyard_reason`, use that field. |
| `tool "<name>" not found in catalog` (or a protocol error `tool '<name>' not found`) | The tool does not exist, or your owner has no access to its server. Both answer the same way. | Check the name with `tools.search` and `servers.list`. Do not probe other names. Ask your owner to get the server granted. |
| `tool "<name>" is not allowed for this agent: its owner limited what it may use. …` | Your owner narrowed this agent (Connected apps). | Read `agent_limits` in `access.whoami`. Ask your owner to change the agent's access. |
| `server restarting, call again: …` (`status: "server_restarting"`, `retryable: true`, `retry_after_seconds`) | toolyard is draining for a restart. Nothing ran and no permission was used. | Call again with the same arguments after a few seconds. |
| `Audit unavailable. The tool did not run.` | toolyard could not write the audit row, so it refused the call. | Retry later. Tell the person if it persists. |

## Policy and permission

| Answer | Meaning | Do this |
|---|---|---|
| `status: "permission_required"` with a `draft` and `also_restricted_nearby` | The tool needs your owner's permission. Nothing ran and nothing reached your owner. | Do not retry. Fill in the draft, add every other restricted call and every question of this round, and send one `inbox.request` (`dry_run: true` first). See the `toolyard-permissions-and-inbox` skill. |
| `denied by policy: <reason>` | An explicit deny. | Do not ask for it in the Inbox. Use `policies.explain` and tell the person. |
| `denied by policy: policy rules are unavailable (they failed to load); every call is denied until they load` | toolyard fails closed while its rules cannot load. | Wait and retry later. Tell the person. |
| `status: "grant_invalid"` | Your `_grant` did not cover this call. Nothing ran. Read `reason` and `next`. | See the grant rows below. |
| `grant_invalid`, outside the grant's parameters | The arguments differ from what was approved. | Call again within the approved `params`, or send a new request (draft included) that says what changed. |
| `grant_invalid`, already used | The grant has an execution claim. | Read its outcome with `inbox.status`. Never repeat an uncertain write. |
| `grant_invalid`, expired | Grants work once and expire (30 minutes by default). | If you still need the call, send a new request and say it is a re-request. |
| `grant_invalid`, revoked | Your owner revoked it. | Stop. Report where you stopped with `inbox.post`. |
| `grants only work with an enrolled agent token; this connection is anonymous` | The connection has no agent. | Connect with an agent token or OAuth (the `toolyard-connecting-clients` skill). |
| `call parameters no longer match the tool schema; permission was not consumed: …` | The tool's schema changed since the grant. | Check the schema with `tools.search`, then ask again if needed. |
| `status: "pending_approval"` with an `approval_id` | A held call: a GitHub write, or a legacy approval. | Collect it with `tools.poll_approval` or `tools.wait_for_approval` (at most 300 seconds). Never send the write again. See `toolyard-github-writes`. |
| `Per-agent pending-approval budget exceeded: you have <n> pendings (max <m>). …` (`status: "agent_pending_budget_exceeded"`) | Too many of your approvals are waiting (16 by default). | `tools.list_my_pending_approvals`, then poll, or `tools.cancel_my_approval` the ones you no longer need (a GitHub request cannot be cancelled this way). |
| `unknown approval "<id>"` | The id is wrong, not yours, or for another tool. | Use the exact id from the earlier answer, on the same tool. |
| `dry-run limit reached (20 per hour)` | Too many `inbox.request` dry runs. | Fix the problems it listed before, then send the request. |

## The outcome is unknown

| Answer | Meaning | Do this |
|---|---|---|
| `outcome_unknown` (execution state, job status, code-mode status) | A write may have committed before the answer was lost. | Read the upstream state with read tools, `audit.mine` and `inbox.status`. Never repeat the write blindly. |
| `Tool outcome unknown: the upstream call <succeeded \| returned an error \| failed: …>, but its audit record was not saved. Do not repeat the write. Check it with audit.mine dispatch_id="<id>" or with the target system.` | A write was sent and the upstream answered as shown, but toolyard could not save its audit record. | Do not repeat the write. Run `audit.mine` with that `dispatch_id`, or read the target system, before another action. |
| A result that ends with `Warning: the audit record for dispatch <id> was not saved. The result above is complete.` | A read-only call ran and its result is complete; only its audit record is missing. | Use the result. Nothing to reconcile. |
| `execution outcome could not be recorded; do not repeat this call; inspect Inbox status` | A granted call ran but its outcome could not be saved. | Read `inbox.status` and the upstream state. Do not repeat it. |

## Sign-ins and connections

| Answer | Meaning | Do this |
|---|---|---|
| `<server> runs as you and isn't connected yet. Open this link to connect it (one-time, 10 minutes): <link> — then ask me to retry. Nothing was run.` | Your owner has not signed in to this `per_user` server. | Show the link as a clickable Markdown link. Retry after the person says they are done. |
| `<server> runs as you, and your sign-in to it has expired. …` | The sign-in needs renewing. | The same, with the new link. |
| `<server> runs as each person's own account, and the person who owns this agent has not connected theirs yet. Open <page>, connect <server>, then retry. …` | No link could be made. | Give the person the My connections page it names. |
| `<server> uses one shared account and …` | The shared sign-in is missing or dead. | An admin owner gets a link to sign it in. A member asks an admin. |
| `upstream "<server>" not connected` | toolyard's own connection to that server is down. | Check `servers.list` (`status`, `error`). An admin-managed agent may call `servers.reconnect`. Otherwise tell the person. |
| A refusal that the server records who made each change and your user has no identity key yet | The server needs a per-person identity key that an admin provisions. | Ask your owner to have an admin provision one. Nothing ran. |
| `connect links are not available on this gateway; connect servers from the toolyard dashboard` | Connect links are not set up here. | Ask the person to connect the server in the dashboard. |

Never ask the person for a password, token or one-time code. The sign-in
happens at the provider, in their browser.

## Identity and admin-only tools

| Answer | Meaning | Do this |
|---|---|---|
| `the inbox needs an enrolled agent: connect with an agent token (Authorization: Bearer …) …` (similar texts from `audit.mine`, `connections.*`, `policies.set`, `servers.reconnect`) | The connection is anonymous. | Connect with an agent token or OAuth (`toolyard-connecting-clients`). |
| `policies.set` or `policies.clear` with `status: "refused"`, `reason: "admin_only"` | Your owner is a member, or did not let this agent manage toolyard. | Do not retry. Use `policies.explain` and ask your owner or an admin. |
| `status: "rejected"` with `would_change` | The change would take a tool out of `ask`, allow a whole server, open a destructive-looking tool, or `allow` a tool that already runs. | Do not retry. A person changes it on the dashboard. |
| `servers.reconnect refused: …` | Only an admin-managed agent may reconnect. | Tell your owner the server's status from `servers.list`. |

## tools.execute and code mode

| Answer | Meaning | Do this |
|---|---|---|
| `tool is required` | `tools.execute` without `tool`. | Pass the catalog name from `tools.search`. |
| `tools.execute cannot target itself` | Recursion is refused. | Call the target tool by name. |
| `<tool> cannot be called from code mode: call the tools you need directly in the script` | A script tried to reach a code-mode tool. | Call the tools directly in the script. |
| An answer with a `job_id` instead of the result | The tool runs as an async job. | `jobs.wait`, `jobs.status` or `jobs.result`. Never call the tool again for the same work. See `toolyard-async-jobs`. |
| Code mode busy (HTTP 429 over REST) | Every script slot is taken. | Wait for `Retry-After`, then run it again. See `toolyard-code-mode`. |
