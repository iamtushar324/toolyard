---
name: toolyard-basics
description: Start here on a toolyard MCP gateway. Explains what toolyard is (one gateway in front of many upstream MCP servers, with per-user access, policies, the Inbox and an audit log), who you are to it (access.whoami), the _reason field every call needs, how to find tools (tools.search, servers.list, tools.execute), how policy decides allow, ask or deny (policies.list, policies.explain), sign-ins (connections.status, connections.link), work sessions (session.start, session.update, _session_id) and your own audit trail (audit.mine). Use it when you first connect to toolyard, before you pick another toolyard skill, when a tool you expect is missing, or when a call fails with "_reason is required", "tool not found in catalog", "not allowed for this agent", "denied by policy", permission_required, grant_invalid, outcome_unknown, "server restarting", a pending-approval budget error, "upstream not connected" or a connect link that says the server is not connected yet.
metadata:
  toolyard-runs: anywhere
---

# toolyard basics

toolyard is one MCP gateway in front of many upstream MCP servers. You connect
to it once. It lists the tools of every server your owner may use, named
`<server>.<tool>`, beside its own built-in tools (`tools.*`, `inbox.*`,
`access.*`, …). Upstream credentials stay inside toolyard: you never see them
and you never need them.

Every call takes the same path:

1. **Access**: may your owner use this server, and does this agent's own limit
   allow the tool? If not, the tool looks like it does not exist.
2. **Policy**: the tool is `allow` (runs), `ask` (needs your owner's
   permission) or `deny` (never runs).
3. **Permission**: an `ask` call runs only with a grant from the Inbox.
4. **Upstream**: toolyard makes the call with its own credentials.
5. **Audit**: the call, your `_reason` and the decision are recorded.

This skill is guidance, not permission: every call it suggests still goes
through policy and the Inbox.

## First five minutes

1. Learn who you are: `access.whoami`. Check `owner.role`,
   `granted_servers`, `agent_limits` and `can_change_policies`.
2. Name your work: `session.start`, then pass its `id` as `_session_id`.
3. Find a playbook and the tools: `skills.search` (when toolyard serves
   skills) and `tools.search` with a few words about the task.
4. Before restricted work, run `inbox.check` on the calls you plan and ask once
   with `inbox.request`. See the `toolyard-permissions-and-inbox` skill
   (`skills.get toolyard/toolyard-permissions-and-inbox`).
5. When a call fails, read its message and structured content first, then
   [errors](references/errors.md). Finish with `session.update` status `done`.

```text
access.whoami
{"_reason": "Learn which agent and owner this connection runs as before I start the task"}

session.start
{"title": "Fix the flaky billing export test", "repo": "acme/api", "branch": "fix/export", "host": "laptop",
 "_reason": "Name this piece of work so my owner can see it among their agents"}

tools.search
{"query": "create issue", "detail": "summary", "limit": 5, "_session_id": "ses_…",
 "_reason": "Find the tool that files a tracker issue for the bug the user reported"}
```

## When to use

- You are new to this gateway, or a new connection lost its context.
- A tool you expect is missing, or a call failed and you do not know why.
- You must explain to a person why a call needs approval or was denied.
- A server needs a sign-in before its tools work.

## Who you are

Each authenticated connection runs as one **agent** (an `ag_…` id) owned by
one person, your **owner**. An MCP client that signed in with OAuth is an
agent of kind `oauth_client`, named by the nickname chosen on the consent
screen. `access.whoami` returns:

| Field | Meaning |
|---|---|
| `caller_id`, `agent_name`, `agent_kind` | This agent. |
| `owner` | `user_id`, `email`, `name`, `role` (`admin`, `member` or `blocked`). |
| `granted_servers` | `"all"` for an admin's agent, else the servers granted to your owner. |
| `always_on` | Built-in groups every active caller has. |
| `agent_limits` | Present when your owner narrowed this agent (Connected apps). |
| `can_change_policies`, `can_reconnect_servers` | Whether `policies.set` and `servers.reconnect` will work for you. |
| `client` | `kind`, `name` and `via` of the client you connect through. |
| `session_id` | Present when this call carried a valid `_session_id`. |
| `note` | Present for a connection with no agent token, or a gateway without per-user access control. |

A connection without an agent token has no identity: the Inbox, grants,
sessions, `audit.mine` and the connections tools refuse it. Help the person
connect properly with the `toolyard-connecting-clients` skill.

## The `_reason` field

- Send `_reason` on **every** call: a string, 20-2000 characters after
  trimming. Say why this call, for this task, in one sentence.
- toolyard records it in the audit log and shows it verbatim to a person who
  reviews the call. It is never permission: a restricted call still needs a
  grant, however long or urgent the reason is.
- A missing or short reason fails at once and nothing runs:
  `_reason too short (got 12 chars, need 20)`.
- If an upstream tool already has its own `_reason` parameter, toolyard's
  field for that tool is named `__toolyard_reason`. Read the input schema.

toolyard adds other optional fields to every tool and strips them before the
upstream sees them: `_grant` (a `tyg_…` grant from the Inbox), `_session_id`,
`_intent_category`, `_approval_id` (resume a held legacy call) and
`_callback_ref` (a receiver from `callbacks.register`). Declaring
`_intent_category` `write`, `destructive`, `external_communication`,
`financial` or `privileged_admin` makes a call need approval unless an
explicit rule allows it. Declaring `read` never lets a call skip approval.

## Find tools

- Your catalog shows only the tools you may use. An administrator can also set
  a surface mode: `full` (everything), `top_n` (the pinned tools plus the
  tools used most, 20 by default) or `router_only` (the pinned tools only).
  The built-in tools are pinned, so `tools.search` and `tools.execute` are
  always there.
- `tools.search`: `query`, `mode` (`keyword` default, `regex` with an RE2
  pattern of at most 200 characters, `semantic` only where an operator
  configured it), `detail` (`full` default with input and output schemas,
  `summary`, `name`), `upstream` (one server), `limit` (default 50, at most
  200). The answer has `total`, `matched`, `mode` and `results` (`name`,
  `upstream`, `description`, `code`, `score`, schemas). When toolyard serves
  skills it also names up to 3 matching skills under `skills`: read one with
  `skills.get`.
- `tools.execute`: call any tool by its catalog name, including one your
  catalog does not show. The target's access, policy and approval rules apply
  unchanged. The outer `_reason` is used when `arguments` has none.
- `servers.list`: each server you may use, with `status` (`ok`, `error`,
  `waiting_signin`, `disabled`, `idle`, `unknown`), `tool_count`,
  `auth_mode` (`shared` or `per_user`) and sign-in state. Never URLs or secrets.
- Code mode (`listToolFiles`, `readToolFile`, `getToolDocs`,
  `executeToolCode`) runs a script that calls many tools in one turn. See the
  `toolyard-code-mode` skill.

```text
tools.execute
{"tool": "github.list_pull_requests", "arguments": {"owner": "acme", "repo": "api", "state": "open"},
 "_reason": "List the open pull requests to pick the next one to review"}
```

A tool outside your access answers exactly like a tool that does not exist:
`tool "x" not found in catalog`. Do not probe names. Ask your owner to get the
server granted, or check `agent_limits`.

## How policy decides

For a call with no grant, toolyard takes the first rule that applies:

1. An explicit policy on the tool.
2. The `tools.*` group runs unless a tool policy says otherwise.
3. An explicit policy on the tool's server.
4. A declared `_intent_category` that escalates (above).
5. A name that starts with a read verb (`get`, `list`, `search`, `find`,
   `read`, `show`, `describe`, `fetch`, `view`, `lookup`, `query`, `head`,
   `ls`) runs.
6. Everything else needs approval.

toolyard's own built-in tools have a fixed decision. The results:

- `allow`: the call runs.
- `ask`: the call returns `permission_required` with a draft request. Nothing
  ran and nothing reached your owner. Do not retry it: follow the
  `toolyard-permissions-and-inbox` skill.
- `deny`: `denied by policy: <reason>`. Do not ask for it in the Inbox.
- GitHub writes always wait for the account owner's own approval and answer
  with an `approval_id`. See the `toolyard-github-writes` skill.

Use `policies.explain` to tell a person why a call needs approval, in words
you can repeat. `policies.list` shows the explicit policies and, for every tool
you may use, its effective access and `require_human` (an ask by policy, which
a person must decide every time).

```text
policies.explain
{"tool": "github.create_issue", "_reason": "Explain to my owner why filing the issue needs their approval"}
```

`policies.set` and `policies.clear` exist, but they work only for an agent
whose admin owner let it manage toolyard (`can_change_policies` is true).
Even then a change may put tools into `ask`, move them between `allow` and
`deny`, or lift a `deny` on one tool. It is always rejected if it takes any
tool out of `ask`, allows a whole server or opens a destructive-looking tool.
Only a person on the dashboard can relax an ask. Never use these tools to
unblock your own task: ask in the Inbox instead.

## Sign-ins (connections)

A server runs either as one shared account or as each person's own account
(`per_user`). When the needed sign-in is missing or expired, the call runs
nothing and answers with a one-time connect link (valid 10 minutes).

1. Show the person the link as a clickable Markdown link.
2. Wait until they say they are done, then retry the call.
3. Never ask for a password, token or one-time code. The sign-in happens at
   the provider, in their browser.

`connections.status` lists every OAuth server your owner may use with its
`state` (`connected`, `needs_signin`, `needs_reauth`, `needs_setup`) and a
fresh link where one is needed. `connections.link` makes a new link for one
`server`. Pass `replace: true` only when the person asked to switch accounts on
a `per_user` server. A shared account is signed in only by an admin; a
member's agent is told which admins to ask.

## Sessions

- `session.start`: `title` (required, at most 120 characters, as a person
  would say it), optional `repo`, `branch`, `host`. It returns the session
  with `id` (`ses_…`).
- Pass that id as `_session_id` on your calls and as `session_id` on
  `inbox.request`. An id that is not yours is ignored. Calls that carry it also
  keep the session's heartbeat fresh.
- `session.update`: `session_id`, `status` (`working`, `waiting_on_owner`,
  `blocked_on_owner`, `idle`, `done`) and an optional `note` (at most 300
  characters). Your owner's Sessions view marks a session stale after 15
  minutes without a heartbeat.
- A session id is attribution only. It grants nothing.

## Your audit trail

`audit.mine` (`limit` default 50, at most 200; `since` as RFC 3339, or
`since_minutes`) returns your own recent rows: `ts`, `tool`, `server`,
`event`, `decision`, `reason`, `via`, `approval_id`, `decided_via`,
`decided_by`. It never shows another agent's rows, arguments or results. Use it
to check what ran after a lost response, before you decide to repeat anything.

## Other built-in tools

One line each in the [tool catalog](references/tool-catalog.md). The ones with
their own skill:

- Async jobs (`jobs.*`): `toolyard-async-jobs`.
- Webhook receivers (`callbacks.*`): `toolyard-callbacks`.
- Skills catalog (`skills.*`): `toolyard-skills`.
- Code mode and the HTTPS fallback (`POST /v1/code/run`, the `ty` command, or
  `inbox.guide` topic `fallback`) when your MCP connection fails:
  `toolyard-code-mode`.

## Pitfalls

- Never get around a restricted or denied tool: no other credential, no direct
  API call, no other agent. A workaround is treated as a security incident.
- Never retry a `permission_required` call without a grant. It answers the
  same way again.
- Never repeat a write that answered `outcome_unknown`. It may have committed.
  Check the upstream state, `audit.mine` and `inbox.status` first.
- Never put a token, password or grant in `_reason`, arguments, logs or
  `memory.set`. Shared memory is readable by every agent granted it.
- A missing tool is usually missing access, not a bug. Check `access.whoami`
  and `servers.list` before you report it.
