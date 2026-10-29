# toolyard built-in tools

toolyard's own tools, by family. Some families exist only when the operator
turned the feature on (noted below). Upstream servers add their own tools as
`<server>.<tool>`. Every call needs `_reason`. Read a tool's full schema with
`getToolDocs` or `tools.search` (`detail: "full"`).

By default your tool list shows six tools: `tools.search`, `tools.execute`,
`getToolDocs`, `executeToolCode`, `inbox.request` and `inbox.status`. Every other tool here
still works: run it in `executeToolCode` as `toolyard.<group>_<name>` (for
example `toolyard.inbox_guide(topic="requests")`), or call it directly where
your client allows. An agent its owner switched to the full list sees them
all.

Which built-in groups you have: `access.whoami` lists them under `always_on`
(`tools`, `inbox`, `session`, `policies`, `servers`, `audit`, `access`,
`connections`, `jobs`, `callbacks`, `skills`). Every active caller has these
without a grant. `memory`, `events` and `notes` are shared data groups that an
admin grants like a server, so a member's agent sees them only after that
grant.

On the full list, pinned tools stay in your catalog in every surface mode
(`top_n`, `router_only`): the discovery, code-mode, approval, Inbox, session, access,
connections, jobs, callbacks and skills read tools, plus `memory.*` and
`events.*` when you have those groups.

## Discovery and proxy (`tools`)

| Tool | What it does |
|---|---|
| `tools.search` | Find the tools you may use, ranked by meaning (Jev, where the operator turned it on), keyword or RE2 regex. Each result says whether a call runs now, needs approval or is blocked (`run`). May also name up to 3 matching skills. |
| `tools.execute` | Call any tool by catalog name under its normal access, policy and approval rules. |

## Code mode (`tools` group)

| Tool | What it does |
|---|---|
| `listToolFiles` | Tree of the stub files for the servers you may use. |
| `readToolFile` | One tool's or one server's stub (compact signatures). |
| `getToolDocs` | Full parameter docs and a usage example for one tool (`server` and `tool`: the two halves of a search result's `code`). |
| `executeToolCode` | Run a script (Starlark, or JavaScript) that calls tools; each nested call is checked like a direct one. |

Details: the `toolyard-code-mode` skill.

## Legacy approvals (`tools` group)

For calls that answered `pending_approval` with an `approval_id` (GitHub
writes, and approvals that were already pending before the Inbox).

| Tool | What it does |
|---|---|
| `tools.poll_approval` | Status, and the executed result when ready, of one approval. Non-blocking. |
| `tools.poll_approvals` | The same for up to 32 approvals. |
| `tools.wait_for_approval` | Block up to 300 seconds for one approval to finish. |
| `tools.wait_for_approvals` | Block on up to 32 approvals, mode `all` or `any`. |
| `tools.list_my_pending_approvals` | Your approvals still waiting. |
| `tools.cancel_my_approval` | Withdraw one of your pending approvals (not a GitHub request: only its account owner can decide it). |
| `tools.approval_stats` | How long reviewers recently took to decide, so you can pace your polling. |

Details: the `toolyard-permissions-and-inbox` and `toolyard-github-writes`
skills.

## Inbox and sessions (`inbox`, `session`)

Present when the Inbox is on. They need an enrolled agent.

| Tool | What it does |
|---|---|
| `inbox.guide` | The agent protocol, by topic (for example `requests`, `grants`, `examples`, `fallback`, `callbacks`). |
| `inbox.check` | Optional (search's `run` says the same per tool): for each planned call with its arguments, `open`, `granted`, `restricted`, `denied` or `unknown`. |
| `inbox.request` | The one way to get permission: every restricted call and every question of a round in one request (`dry_run: true` first). |
| `inbox.ask` | Ask your owner a question, or report a blocker. |
| `inbox.post` | Report an outcome (`kind: "update"`). |
| `inbox.status` | Decisions, answers, grants and execution state. Non-blocking. |
| `inbox.wait` | Block until items are decided. Optional when you have a callback. |
| `inbox.cancel` | Withdraw a request. |
| `inbox.upload` | A single-use URL to upload a local screenshot, recording or file as evidence. |
| `session.start` | Name a piece of work; returns its `id` (`ses_…`). |
| `session.update` | Report the session's status; also a heartbeat. |

Details: the `toolyard-permissions-and-inbox` skill.

## Access (`policies`, `servers`, `audit`, `access`)

| Tool | What it does |
|---|---|
| `access.whoami` | Your agent, owner, role, granted servers, limits and what you may change. |
| `policies.list` | Explicit policies, and every visible tool's effective access and deciding rule. |
| `policies.explain` | Why one tool runs, needs approval or is denied, in plain words. |
| `policies.set` | Admin-managed agents only: set allow, ask or deny on a tool or server, under the ask rule. |
| `policies.clear` | Admin-managed agents only: remove an explicit policy, under the same rule. |
| `servers.list` | The servers your owner may use, with status, tool count and auth mode. |
| `servers.reconnect` | Admin-managed agents only: drop a server's connection and dial again. Audited. |
| `audit.mine` | Your own recent calls and how toolyard decided them. |

## Sign-ins (`connections`)

| Tool | What it does |
|---|---|
| `connections.status` | Every OAuth server your owner may use, its sign-in state, and a connect link where needed. |
| `connections.link` | A fresh one-time connect link for one server, or why none can be made. |

## Async jobs (`jobs`)

| Tool | What it does |
|---|---|
| `jobs.start` | Run any tool you may call as an async job; returns a `job_id`. |
| `jobs.status` | Status, last progress and (small) result of one job. Non-blocking. |
| `jobs.wait` | Block up to 60 seconds for one or up to 32 jobs. |
| `jobs.result` | The result of a job that ended, exactly as the tool returned it. |
| `jobs.cancel` | Cancel one of your jobs. |
| `jobs.list` | Your jobs, newest first. |

Details: the `toolyard-async-jobs` skill.

## Webhook receivers (`callbacks`)

Present when callbacks are on.

| Tool | What it does |
|---|---|
| `callbacks.register` | Register an https receiver for signed events; returns a `callback_ref`. |
| `callbacks.list` | Your receivers, and one receiver's recent deliveries. |
| `callbacks.update` | Disable, rotate the secret of, or remove a receiver. |
| `callbacks.set_default` | Make one receiver follow every request that names none. |
| `callbacks.test` | Send a signed ping to a receiver. |

Details: the `toolyard-callbacks` skill.

## Skills (`skills`)

Present when the skills hub is on.

| Tool | What it does |
|---|---|
| `skills.search` | Find a playbook for a task. |
| `skills.get` | Read one skill's SKILL.md and its file list. |
| `skills.read` | Read one file of a skill by URI. |
| `skills.grep` | Search skill file contents with an RE2 pattern. |
| `skills.sources` | The skill sources you can see. |
| `skills.list` | Every skill you can see, a page at a time. |
| `skills.propose_source` | Ask your owner to add a repository as a skill source (Inbox item). |
| `skills.propose_change` | Propose an edit to a skill (Inbox item for the source's owner). |
| `skills.proposal` | Read one of your proposals. |
| `skills.bundle` | A short-lived download of skills as one archive for a local client. |

Details: the `toolyard-skills` skill.

## Shared data (`memory`, `events`)

Granted by an admin. Shared by every agent that has the grant: never store
secrets in them.

| Tool | What it does |
|---|---|
| `memory.get` | Read a value by `key` (optional `scope`, default `global`). |
| `memory.set` | Write a string value under a key. Policy applies like any write. |
| `memory.list` | List keys that match a `prefix`. |
| `memory.delete` | Delete a key. |
| `events.brief` | Markdown digest of unacknowledged events across agents and systems. |
| `events.query` | Search the events feed by source, type, time, text or unacked state. |
| `events.get` | One event with its full payload. |
| `events.ack` | Acknowledge events you handled. |
| `events.publish` | Tell other agents what you did or noticed. |

## Others

- `notes.publish`: when the operator enabled notes, write a markdown note to
  the notes workspace.
- `fixture.echo`: a test fixture that echoes a message. Ignore it.
- A GitHub connector, when an admin installed one, adds pull request tools
  under its server name (`get_pull_request`, `list_pull_request_files`,
  `create_pull_request_comment`, `submit_pull_request_review`, …). Its writes
  always wait for the account owner: see the `toolyard-github-writes` skill.
