# Access tools: what an agent may see and change about toolyard itself

toolyard gives every agent a small set of tools about toolyard: which tools
run, ask or are denied and why, which servers exist, what the agent has done,
and who it is. They are internal tools (no approval needed for themselves),
scoped to the calling agent's owner, and every change is audited. This page
is for agents and for the admins who read the audit log.

## The tools

| Tool | Who | What |
|---|---|---|
| `policies.list` | everyone | The explicit policies (scope, target, allow/ask/deny, note) and, for every tool you may use, its **effective** access and the rule that decides it. Optional `server` filter. |
| `policies.explain` | everyone | One tool: its effective access and the rule behind it, in plain words you can repeat to a person ("`github.create_issue` needs approval (ask): no explicit policy; its name does not look read-only…"). |
| `policies.set` | admin-owned enrolled agents | Set an explicit policy: `scope` `tool` (one catalog name) or `upstream` (every tool of one server), `target`, `access` `allow`/`ask`/`deny`, optional `note`. Takes effect immediately, like a dashboard edit. See the ask rule below for what is refused. |
| `policies.clear` | admin-owned enrolled agents | Remove an explicit policy so the target falls back to the rules below it. Judged by the same rule. |
| `servers.list` | everyone | The servers your owner may use: name, enabled, status (`ok`, `error`, `waiting_signin`, `disabled`), tool count, auth mode (`shared` or `per_user`) and, for servers with a stored sign-in, whether one exists (on a `per_user` server: whether *your owner* connected it). Never URLs, headers or secrets; a URL inside an error message is reduced to `scheme://host`. |
| `servers.reconnect` | admin-owned enrolled agents | Drop a server's connection and dial again, e.g. after `servers.list` shows an error. Audited (`server.reconnect`). |
| `audit.mine` | enrolled agents | Your own recent calls and how toolyard decided them (tool, event, decision, reason, time, via). Only your rows, never another agent's; no arguments or result bodies. `limit` (default 50, max 200), `since` (RFC 3339) or `since_minutes`. |
| `access.whoami` | everyone | Your agent, its owner (email, role), the servers you were granted, the always-on groups, whether you may change policies, and `agent_limits` when your owner narrowed this agent (Connected apps). |

A member sees only what is in their scope: `policies.list` and
`servers.list` leave out servers they were never granted, and
`policies.explain` answers "tool not found" for a tool on such a server,
exactly as a direct call would.

An agent can also have its own limits, on top of its owner's scope: only
chosen servers and tools, and blocks (every OAuth client gets them from its
consent screen; see [connect-clients.md](connect-clients.md)). A tool its
owner may use but the agent may not is absent from `tools/list` and
search, and a call to it says so plainly: "not allowed for this agent: its
owner limited what it may use". The audit reason is
`access: not allowed for this agent`. A limited agent cannot change
policies, reconnect servers or sign in shared connections, even when its
owner is an admin; neither can an OAuth client unless its admin owner chose
"May manage toolyard" for it (`access.whoami` shows `can_change_policies`).

## The tool list: five tools by default

Every MCP agent's `tools/list` shows five tools by default (the small tool
list):

| Tool | What the agent does with it |
|---|---|
| `tools.search` | Find a tool by what it does. Each result gives its code-mode name (`code`) and whether it runs now, needs approval or is blocked (`run`: `now`, `needs_approval` or `blocked`). |
| `getToolDocs` | Read one tool's full parameters (`server` and `tool`: the two halves of a result's `code`). |
| `executeToolCode` | Run tools in a Starlark or JavaScript script. |
| `inbox.request` | Ask the owner for permission. |
| `inbox.status` | Read the decision, the answers and the grants. |

The list is only what the agent sees. Every other tool the agent may use
still works: called directly (`tools/call`), through `tools.execute`, and
from code mode. Search, code-mode catalogs and the REST catalog routes are
unchanged. The small list is applied after access, so it only hides
tools: one of the five that the agent's own limits deny stays hidden.
A dashboard or voice session picks tools from the whole catalog and keeps
it. A local unauthenticated MCP caller (no agent key) gets the small list
too, like every MCP client; it has no scope to switch, so give it an agent
key for the full list.

**Switching one agent to the full list.** The scope editor (Connected apps
page, **Change access**) has a **Tool list** choice: *Small: 5 tools* (the
default) or *Full: every tool it may use*. The REST routes carry it as
`tool_list` (`small` or `full`) in the agent's scope, under the same
permission checks as the rest of the scope: the agent's owner, or an
admin, through `POST /v1/me/agent-scopes` or `POST /v1/me/apps/scope`. A
scope sent without `tool_list` is the small list, so read the scope first
(`GET /v1/me/agent-scopes?agent_id=`) when you change one field. The value
is stored in the agent's scope row (`agent_scopes.rules`), so existing
agents became small without a migration. It is not access: it changes no
access decision, and switching an agent between the lists leaves its
access exactly as it was. Connecting an OAuth client again keeps the tool
list its owner chose.

**The permission and batching rule.** `inbox.request` is the one way an
agent gets permission. In each round the agent puts every restricted call
(search says `run: needs_approval`, or a call returned
`permission_required`) and every question it has for its owner into one
`inbox.request` (`tools[]` and `questions[]` with `schema_version` 3). It
starts another round only for what the answers open. In a script,
`tools.tryCall` (JavaScript) or `try_call` (Starlark) returns
`permission_required` instead of stopping, so the script can collect every
such call; the agent asks once, outside the script, reads the decision with
`inbox.status`, and runs the script again with each grant as the call's
`_grant` argument (`server.tool(param="value", _grant="<token>")`, or
`tools.server.tool({ param: "value", _grant: "<token>" })`), with exactly
the approved parameters. `inbox.check` still answers from code mode, but
search's run status replaces it.

## The ask rule

Policies are global to a toolyard, so **only an enrolled agent whose owner
is an admin may change them** (`ag_…` token; a local unauthenticated caller
or a dashboard session uses the Policies page instead). A member's agent
gets a clear refusal, and every attempt is on record.

For an admin's agent there is one rule, enforced by the gateway on every
`policies.set` and `policies.clear`, never by the caller:

> A change may put tools **into** ask, or move them **between allow and
> deny**, but it is rejected if it would take **any** affected tool **out of
> ask**. Only a person on the dashboard can relax an ask.

Before writing anything the gateway works out every tool the change touches,
evaluates the policy engine before and after on a detached copy (static
evaluation: no declared intent, no arguments, exactly where a real call
starts), and rejects the change if any tool leaves ask. Check and write
happen under the policy engine's write lock, so no dashboard edit lands in
between. The rejection names the tools: `policies.set rejected: it would take
1 tool(s) out of ask: github.create_issue (ask → allow)`.

"Out of ask" is read strictly. Effective access has four states, not three:
`allow`, `ask` by default, `ask` by policy, and `deny`. An ask **by policy**
(an explicit `ask` rule) makes a person decide every call; the learned
auto-approval rules are skipped. An ask **by default** (a write-looking name
with no rule) can be decided by such a rule. So turning an ask by policy into
an ask by default (clearing an explicit `ask`, on a tool or a whole server)
is a relaxation and is rejected too, even though the word does not change.
`policies.list` and `policies.explain` report the distinction as
`require_human`.

Consequences worth knowing:

- `allow → ask`, `deny → ask`, `allow → deny`: fine. `deny → allow` is fine
  for one tool that is not destructive-looking.
- `ask → allow`, `ask → deny`, `ask by policy → ask by default`: rejected.
  This includes a *default* ask: a write-looking tool with no policy is ask,
  so an agent cannot `allow` it.
- **Upstream scope is always judged against three probes beside the tools
  registered right now**: `<server>.get_probe` (a read), `<server>.probe` (a
  write, ask by default) and `<server>.delete_probe` (a destructive write).
  They stand for the tools the server does not have yet, or has not
  re-registered yet after a reconnect. So a server-wide `allow` by an agent is
  **always** refused (it would take the write probe out of ask, or open the
  destructive probe); a server-wide `ask` is always fine; a server-wide `deny`
  is fine only when nothing on the server is at ask.
- **No destructive-looking tool is ever opened by an agent**, whatever the
  verb and scope: a change after which a tool named `delete`, `remove`,
  `drop`, `purge`, … (or the destructive probe) ends at `allow` without
  having been `allow` before is rejected. That covers `policies.clear` on a
  destructive tool under a server-wide allow, and `deny → allow` on a server
  that has one. A destructive tool a person force-allowed on the dashboard
  stays as it is; an agent's unrelated change does not touch it.
- **`allow` only lifts a `deny`.** An explicit `allow` on a tool that already
  runs (allow by its read-looking name) would change nothing today and pin
  the tool open against a later server-wide `ask`, so it is refused.
- Clearing a policy is judged the same way as setting one: clearing an
  explicit `ask` on a read-looking tool would make it `allow`, and clearing
  an explicit `ask` on a write would make it an ask by default, so both are
  rejected. Clearing a `deny` (`deny → ask`) is fine.
- A tool that is not registered yet is judged by its name, as the engine will
  judge it once it appears.
- An explicit `ask` or `deny`, on a tool or on a whole server, also disables
  the learned auto-approval rule of every tool it covers, as the dashboard's
  policy editor does for one tool.

Every change, rejection and refusal is an audit row of type `policy.change`
(decision `allow`/`ask`/`deny`/`clear`, `rejected` or `refused`) carrying the
agent, its owner, the agent's `_reason`, and the before/after access of every
affected tool in `arguments`. An applied change is also published on the
dashboard's live feed as a `policy` event.

## Code mode: the `toolyard` server

In code mode (`listToolFiles` → `readToolFile` → `executeToolCode`) every
internal toolyard tool, these included, hangs off **one** virtual server,
`toolyard`, as `toolyard.<group>_<name>`:

```python
who = toolyard.access_whoami()
why = toolyard.policies_explain(tool="github.create_issue")
toolyard.policies_set(scope="tool", target="github.create_issue", access="ask",
                      note="always ask before issues are filed")
mine = toolyard.audit_mine(limit=20)
req = toolyard.inbox_request(...)
```

The stubs are `servers/toolyard/<group>_<name>.pyi` (`servers/toolyard.pyi`
for all of them) and are derived from the registered tools, so an internal
tool added later appears by itself. Upstream servers keep their own key
(`BkCoreServices.get_client(...)`). A call from a script goes through exactly
the same scoping, policy, approval and audit path as a direct call (recorded
with via `code_mode`), so `toolyard.policies_set` enforces the ask rule word
for word and a member's agent gets the same refusal. The four code-mode tools
themselves are the one thing a script cannot reach, and
`toolyard.tools_execute` refuses to target them. An approval wait inside a
script (`toolyard.tools_wait_for_approval`, `…_wait_for_approvals`) ends
with the script's clock (5 minutes by default) and answers with the normal
timed-out snapshot.

Which groups are in `toolyard`: `inbox`, `session`, `memory`, `events`,
`tools` (the approval pollers, `tools.search`, `tools.execute`),
`policies`, `servers`, `audit`, `access`, and the built-ins toolyard
registers itself such as `notes.publish` and the `skills.*` tools
(`toolyard.notes_publish`, `toolyard.skills_list`). The `notes` and `skills`
MCP servers that startup also connects are upstreams like any other and keep
their own keys (`notes.<tool>`, `skills.<tool>`); a member needs a grant for
them, as for `memory` and `events`.

**Migration note.** Before this namespace, code mode bound each internal
group as its own server. A script that called `inbox.request(...)`,
`memory.get(...)`, `events.brief()` or `session.start(...)` now calls
`toolyard.inbox_request(...)`, `toolyard.memory_get(...)`,
`toolyard.events_brief()` and `toolyard.session_start(...)`; the old names
fail with `undefined: inbox` and the error lists the available server keys.
Direct MCP calls (`inbox.request` as a tool name) are unchanged.
