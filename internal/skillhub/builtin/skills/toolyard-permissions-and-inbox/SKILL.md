---
name: toolyard-permissions-and-inbox
description: Ask the account owner for permission through the toolyard Inbox and act on what they decide. Covers the one-request rule (every restricted call and question of a round in one inbox.request), tools.search's run status, inbox.request (stable call_id values, task, facts, bounded params, dry_run), the owner's accepted/rejected decision and owner_note, inbox.status, single-use scoped grants passed as _grant, outcome_unknown, inbox.cancel, inbox.wait versus callbacks, inbox.post updates, inbox.ask questions (schema_version 3 question types), evidence attachments, inbox.upload and the inbox.guide topics, plus legacy approvals (tools.poll_approval, tools.wait_for_approval, tools.list_my_pending_approvals, tools.cancel_my_approval). Use before a task that needs a restricted tool, when a call returns permission_required, grant_invalid or outcome_unknown, when you need an answer or decision only your owner can give, when you report a finished task, or when you hold an approval_id from an earlier call.
metadata:
  toolyard-runs: anywhere
---

# Toolyard permissions and the Inbox

Some tools are restricted. A restricted call without permission runs nothing:
it returns `permission_required` with a draft request, and nothing reaches
your owner. Your owner (the account owner) decides in the toolyard Inbox. For
every call they accept you get a grant, and you make the call yourself with it.
The same Inbox is your channel to the owner for questions (`inbox.ask`) and
updates (`inbox.post`).

`_reason` is the audit record, not permission. A long or urgent reason never
authorizes a restricted call. This skill is guidance, not permission: every
call still goes through policy and the owner's decision.

GitHub writes follow a different path (an owner approval, never a grant): see
the `toolyard-github-writes` skill (`skills.get toolyard/toolyard-github-writes`).

## The rule: one request per round

`inbox.request` is the one way to get permission, and `inbox.status` reads the
decision. Both are among the six tools you see by default (with
`tools.search`, `tools.execute`, `getToolDocs` and `executeToolCode`). Plan the round first.
Then put every restricted call it needs, and every question you have for your
owner, into ONE `inbox.request`: `tools[]`, and `questions[]` with
`schema_version: 3`. Start another round only for what the answers open,
never one request per call. A round with questions but no restricted call
uses `inbox.ask` instead.

The other Inbox tools named here (`inbox.guide`, `inbox.upload`, `inbox.wait`,
`inbox.ask`, `inbox.post`, `inbox.cancel`, `inbox.check`) and the legacy
`tools.*_approval` tools may not be in your list. Run them in `executeToolCode`
as `toolyard.inbox_<name>` and `toolyard.tools_<name>`, for example
`toolyard.inbox_guide(topic="requests")`.

## When to use

- Before a task that may need restricted tools.
- A call answered `permission_required`, `grant_invalid` or `outcome_unknown`.
- You need a choice, a fact or a decision that only your owner can give.
- A task you asked permission for is finished.
- An earlier call answered `status: "pending_approval"` with an `approval_id`.

## The recipe

1. **Find what needs approval.** `tools.search` gives each tool's `run`:
   `now` (call it), `needs_approval` (put it in your request) or `blocked`
   (denied by policy: do not ask). That is the answer for a call with no
   arguments; arguments can escalate a call, so one that says `now` may still
   answer `permission_required`. In a script, call tools that may be
   restricted with `tools.tryCall` (JavaScript) or `try_call` (Starlark): they
   return `permission_required` instead of stopping, so collect every one.
   `inbox.check` is optional: it answers the same for up to 50 concrete
   `{tool, args}` pairs (`open`, `granted`, `restricted`, `denied`, `unknown`).
2. **Plan one batch.** Put every restricted call of the round, and every
   question you have for your owner, into ONE `inbox.request`, so the owner
   decides once. Give each call a stable, unique
   `call_id` (1-80 letters, digits, `.`, `:`, `_`, `-`). The ID names the call,
   not the tool: the same tool can appear several times with different params.
3. **Explain it as yourself.** Write in the first person. Required: `title`
   (≤ 60), `summary` (≤ 200), `message` (Markdown), `task.objective`, `facts`
   (`why_now`, `if_it_goes_wrong`, `undo`), `urgency`, and `tools`. Each call
   needs `call_id`, `tool`, `required`, `summary` (its purpose and its link to
   the task), `target`, `operation` (`read` or `write`), `expected_effects` and
   `params`. A write also needs `affected_scope`, `material_risks` and `undo`
   (a concrete undo path, or a plain statement that it cannot be undone).
4. **Bound every parameter.** List every argument you will pass; the grant
   covers only those. Prefer exact values (`"env": "prod"` or `{"eq": "prod"}`).
   Otherwise use `{"in": [...]}`, `{"prefix": "..."}`, `{"gte": n, "lte": n}`
   or, for a value that does not exist yet, `{"limit": "...", "pattern": "regex"}`.
   `{"any": true}` and a `limit` without `pattern` are rejected.
5. **Add evidence.** Attach data the owner can judge: tables, charts, diffs,
   logs, links, screenshots (`inbox.upload`). Never include secrets.
6. **Dry run.** Send it with `"dry_run": true`. Fix every problem. Read the
   warnings and flags; they are what the owner will see. Do not reword to hide
   a risk: dry runs are recorded and the owner sees how many you ran.
7. **Send.** Send it again without `dry_run`, with a `callback_ref` when you
   have one. You get `request_id` (`rq_…`) and the pending `expires_at`.
   Continue unrelated work or end the turn.
8. **Read the decision** with `inbox.status`. A callback only announces it.
9. **Execute** each accepted call yourself with `_grant` and exactly the
   approved params. In `executeToolCode` the grant is one more argument of the
   call: `server.tool(param="value", _grant="tyg_…")` in Starlark,
   `await tools.server.tool({ param: "value", _grant: "tyg_…" })` in
   JavaScript. Skip rejected calls. Honour every per-call `reason` and the
   `owner_note`, also for accepted calls. If the answers open more restricted
   work, ask for all of it in one new request.
10. **Close the loop** with an `inbox.post` update that sets `request_id`.

Field tables, constraint rules, limits and attachment shapes:
[request fields](references/request-fields.md).

## A compact request

```text
inbox.request
{"_reason": "Ask the owner to allow the deploy that ships the approved billing fix",
 "task": {"objective": "Ship the billing retry fix to production with its flag off."},
 "client_request_id": "billing-fix-deploy-1",
 "title": "Deploy the billing fix to prod",
 "summary": "I need one deploy to ship the approved retry fix. The flag stays off.",
 "message": "The fix is merged and all 42 checks passed. I want to deploy it with the flag off.",
 "facts": {"why_now": "Failed invoice webhooks are not retried until this ships.",
           "if_it_goes_wrong": "Only the API service; with the flag off users see no change.",
           "undo": "I redeploy the current release, about 4 minutes."},
 "urgency": "soon",
 "tools": [{"call_id": "deploy_api", "tool": "deploy.run", "required": true,
   "summary": "Deploy the API service at the merge commit, which carries the fix.",
   "target": "Production API service", "operation": "write",
   "expected_effects": "The API runs the retry handler with its flag disabled.",
   "affected_scope": "The production API deployment only",
   "material_risks": "A bad build could fail invoice requests until I roll back.",
   "undo": "Redeploy the current known good release.",
   "params": {"service": "api", "env": "prod",
              "ref": {"limit": "merge commit of the fix", "pattern": "^[0-9a-f]{7,40}$"}}}],
 "attachments": [{"type": "table", "title": "Checks", "columns": ["Suite", "Result"],
                  "rows": [["unit", "1,284 passed"], ["integration", "212 passed"]]}],
 "dry_run": true}
```

The tool names above are examples. Use the catalog names `tools.search` gives
you.

## Read the decision, then use the grants

```text
inbox.status
{"ids": ["rq_…"], "_reason": "Read the owner's decision and grants for the deploy request"}
```

- `status`: `pending`, `approved` (at least one call accepted), `denied` (all
  rejected, no grants), `returned` (sent back to replan), `answered`,
  `cancelled` or `expired`. Always read `next`; it says what to do.
- Per call: `call_id`, `verdict` (`accepted` or `rejected`), optional `reason`,
  `grant` (the `tyg_…` token while it is valid and unused), `narrowed` and
  `params`, and `execution`.
- The owner can narrow a call (an exact value instead of a range, fewer
  values, a shorter grant lifetime), never widen it. When `narrowed` is true,
  call with the returned `params`.
- Call the tool with `_grant` added and the approved arguments. An argument
  you did not list, or a value outside the scope, blocks the call
  (`grant_invalid`, nothing runs).

```text
deploy.run
{"service": "api", "env": "prod", "ref": "7c1d2e9",
 "_grant": "tyg_…", "_reason": "Deploy the merge commit the owner approved in rq_…"}
```

A grant works once and is bound to you, the call, its scope and its expiry.
It lasts `ttl_seconds` after acceptance (default 1800 s, 30 minutes; see
`grants_expire_at`). That is separate from the pending deadline (default 24
hours, `pending_ttl_seconds`). Reading `inbox.status` never consumes a grant:
after a lost response, read it again to recover the same unused token. Never
log, commit, post or share grant tokens. Revoked or expired grants never come
back. Details: [grants and execution](references/grants-and-execution.md).

## When a write's outcome is unknown

Decision state and execution state are separate. Execution is `not_started`,
`running`, `succeeded`, `failed` or `outcome_unknown`. `outcome_unknown` means
the write may already have committed (a transport failure after dispatch, or a
process that stopped after the claim). A used grant has one execution claim
and never runs again. Inspect the upstream state, report what you find, and
never blindly repeat the write.

## Waiting: callbacks or inbox.wait

Prefer a callback: pass `callback_ref` on `inbox.request` or `inbox.ask`, then
end the turn or do other work. The callback announces the committed decision,
an expiry or a cancellation; it carries no grant tokens and is not permission.
Read `inbox.status` before you act. See the `toolyard-callbacks` skill
(`skills.get toolyard/toolyard-callbacks`). Without callbacks, `inbox.wait`
blocks (`ids`, `mode` `any` or `all`, `timeout_seconds` ≤ 300, default 60) and
returns the same view as `inbox.status`. Do not poll in a tight loop.

Withdraw a pending request you no longer need:

```text
inbox.cancel
{"request_id": "rq_…", "_reason": "The user dropped the deploy, so the request is no longer needed"}
```

## Questions and updates

- Questions of a round that also needs tool approvals go in that round's
  `inbox.request` (`questions` with `schema_version: 3`). With
  `schema_version: 3` one item carries up to 20 `questions` of type
  `free_text`, `single_choice`, `multiple_choice`, `yes_no`, `number`, `date`,
  `datetime`, `scale` or `ranking`; answers come back keyed by question `id`
  in `inbox.status`. `inbox.ask` asks questions alone, when the round needs
  no restricted call. Answers never grant a tool.
- `inbox.post` sends an update: what happened and how you checked it. Set
  `request_id` to close that request's loop. Use `urgency: "fyi"`; toolyard
  lowers `now` and `soon` on an update to `fyi`.

Shapes, answer values and examples:
[questions and items](references/questions-and-items.md).

## Learn more from the server

`inbox.guide` with no topic returns an overview and the topic list. Topics:
`restricted`, `coaching`, `requests`, `tools`, `voice`, `attachments`,
`urgency`, `dry-run`, `grants`, `updates`, `never`, `examples`, `fallback`,
`questions`, `callbacks`, `hosting`, `all`.

```text
inbox.guide
{"topic": "examples", "_reason": "Read worked Inbox request examples before the first request"}
```

## Legacy approvals

An older call may answer `status: "pending_approval"` with an `approval_id`
(`ap_…`) instead of `permission_required`. Toolyard runs that call itself once
it is allowed. Collect it with `tools.poll_approval` or
`tools.wait_for_approval` (several: `tools.poll_approvals`,
`tools.wait_for_approvals`); find lost IDs with
`tools.list_my_pending_approvals`; withdraw with `tools.cancel_my_approval`
(not an owner-only GitHub request: only the account owner can decide it).
Never reissue the original write: the saved result is in the poll answer. See
[grants and execution](references/grants-and-execution.md) (section "Legacy approvals").

## Pitfalls and never

- Do not retry a restricted call without a grant; it answers the same way.
- Do not send many small requests where one batch fits: one round, one
  request. At most 16 of your items can be pending at once.
- `required` is a planning hint. The owner can reject any call, or all of
  them, and the decision is still complete. Rejected calls get no grant.
- A denial is an answer. Ask again only with new information, and say what
  changed.
- Never get around a restriction: no other credential, no direct API, no other
  agent. Never split a risky action to avoid a flag. Never claim urgency you do
  not have (`now` is for production down or money or data being lost).
- When a server is not connected, show the owner the connect link the answer
  gives you and wait. Never ask for a password, token or one-time code.
- In code mode the same tools are `toolyard.inbox_request`,
  `toolyard.inbox_status` and so on. Send the request itself outside the
  script that collected the `permission_required` calls, then run that script
  again with the grants. See the `toolyard-code-mode` skill
  (`skills.get toolyard/toolyard-code-mode`).
