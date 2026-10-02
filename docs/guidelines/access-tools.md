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
| `policies.set` | admin-owned agents | Set an explicit policy: `scope` `tool` (one catalog name) or `upstream` (every tool of one server), `target`, `access` `allow`/`ask`/`deny`, optional `note`. Takes effect immediately, like a dashboard edit. |
| `policies.clear` | admin-owned agents | Remove an explicit policy so the target falls back to the rules below it. |
| `servers.list` | everyone | The servers your owner may use: name, enabled, status (`ok`, `error`, `waiting_signin`, `disabled`), tool count, auth mode (`shared` or `per_user`) and, for servers with a stored sign-in, whether one exists (on a `per_user` server: whether *your owner* connected it). Never URLs, headers or secrets. |
| `servers.reconnect` | admin-owned agents | Drop a server's connection and dial again, e.g. after `servers.list` shows an error. Audited (`server.reconnect`). |
| `audit.mine` | enrolled agents | Your own recent calls and how toolyard decided them (tool, event, decision, reason, time, via). Only your rows, never another agent's; no arguments or result bodies. `limit` (default 50, max 200), `since` (RFC 3339) or `since_minutes`. |
| `access.whoami` | everyone | Your agent, its owner (email, role), the servers you were granted, the always-on groups, and whether you may change policies. |

A member sees only what is in their scope: `policies.list` and
`servers.list` leave out servers they were never granted, and
`policies.explain` answers "tool not found" for a tool on such a server,
exactly as a direct call would.

## The ask rule

Policies are global to a toolyard, so **only an admin's agent may change
them**; a member's agent gets a clear refusal, and the attempt is on record.

For an admin's agent there is one rule, enforced by the gateway on every
`policies.set` and `policies.clear`, never by the caller:

> A change may put tools **into** ask, or move them **between allow and
> deny**, but it is rejected if it would take **any** affected tool **out of
> ask**. Only a person on the dashboard can relax an ask.

Before writing anything the gateway works out every tool the change touches
(tool scope: that tool; upstream scope: every tool registered for that
server), evaluates the policy engine before and after on a detached copy
(static evaluation: no declared intent, no arguments, exactly where a real
call starts), and rejects the change if any tool goes from ask to allow or
deny. The rejection names the tools: `policies.set rejected: it would take 1
tool(s) out of ask: github.create_issue (ask → allow)`.

Consequences worth knowing:

- `allow → ask`, `deny → ask`, `allow → deny`, `deny → allow`: fine.
- `ask → allow`, `ask → deny`: rejected. This includes a *default* ask: a
  write-looking tool with no policy is ask, so an agent cannot `allow` it.
- An upstream-scope `allow` is rejected as soon as the server has one
  default-ask tool. An upstream-scope `ask` is always fine.
- Clearing a policy is judged the same way: clearing an explicit `ask` on a
  read-looking tool would make it `allow`, so it is rejected.
- A tool that is not registered yet is judged by its name, as the engine will
  judge it once it appears; a server with no registered tools is judged by a
  read and a write probe (`<server>.get_probe`, `<server>.probe`).
- `allow` on a destructive-looking name (`delete`, `remove`, `drop`, `purge`,
  …) is refused for agents even when the rule would permit it; a person can
  force it from the dashboard.
- An explicit `ask` or `deny` on a tool also disables that tool's learned
  auto-approval rule, as the dashboard's policy editor does.

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
`toolyard.tools_execute` refuses to target them.
