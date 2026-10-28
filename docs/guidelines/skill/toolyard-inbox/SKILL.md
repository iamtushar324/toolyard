---
name: toolyard-inbox
description: Request human permission through Toolyard Inbox, recover scoped grants, and inspect execution outcomes. Use when a restricted tool returns permission_required or grant_invalid.
---

# Request permission in Toolyard Inbox

Restricted calls without permission run nothing. A short `_reason` is an audit
explanation, not a substitute for an Inbox request. The account owner makes the
permission decision. New requests issue grants; the agent executes the calls.

## Prepare one concrete batch

`inbox.request` is the one way to get permission, and `inbox.status` reads the
decision; both are among the five tools you see by default. Other inbox tools
named here run in `executeToolCode` as `toolyard.inbox_<name>`.

1. Find the restricted calls: `tools.search` gives each tool's `run`
   (`now`, `needs_approval` or `blocked`), and a call that answered
   `permission_required` needs approval too. In a script, `tools.tryCall` or
   `try_call` returns that status instead of stopping, so collect every one.
   `inbox.check({ calls })` is optional; it answers for concrete arguments.
2. Put every restricted call of this round, and every question you have for
   your owner (`questions[]` with `schema_version: 3`), into ONE request. Ask
   again only for what the answers open, never one call at a time.
3. Give every proposed call a stable, unique `call_id`. The ID identifies the
   call, not the tool. The same tool can appear repeatedly with different params.
4. Mark `required` only as a planning hint. The owner can reject that call or
   reject the entire batch and still complete the decision.
5. If callbacks are available, create or obtain an authorized receiver. In T3,
   use its actual session webhook capability and keep the signing secret out of
   prompts. Pass the opaque destination reference as `callback_ref`.

## Supply useful context

The server requires these fields for an access request:

- `task.objective`: the task outcome.
- `message`: what permission enables and its relationship to the task.
- `facts.why_now`: why the permission is necessary now. Preserve the existing
  `facts.if_it_goes_wrong` and `facts.undo` request summary fields.
- Each call's `call_id`, `tool`, `summary`, `target`, `operation` (`read` or
  `write`), `expected_effects`, and exact `params` or explicitly bounded scopes.
- For writes, each call's `affected_scope`, `material_risks`, and `undo`:
  give a concrete undo path or state clearly that the action cannot be undone.
- Supporting evidence where applicable. Exclude credentials and secret values.

Use `eq` for known values. Use finite sets, ranges with both bounds, or explicit
string bounds when necessary. Do not use unbounded `any` or a descriptive
`limit` without an enforceable `pattern`. Dependencies refer to call IDs.
Invalid references and cycles are rejected. Dependencies cannot authorize a
rejected call or widen an accepted call's scope.

Use `title` at most 60 characters and `summary` at most 200 characters. An optional
`audio.script` is at most 75 words, in first person, without IDs, hashes or URLs.
Use structured `table`, `chart`, `diff`, `code`, `log`, `markdown`, or `link`
attachments for evidence. For a screenshot, diagram, recording or file you
hold locally, call `inbox.upload` (name, content_type, size, sha256, kind),
`PUT` the bytes with the returned `curl` command within 15 minutes, and cite
`{"type": "image", "upload": "upload:<sha256>", "alt": "…"}`. Linked media
(`url`) uses HTTPS. Keep all context consistent.
Server field validation does not prove the explanation is true.

## Submit and continue

1. Submit with `dry_run: true`. Correct each field-specific problem and review
   the flags. Do not change wording to hide a risk. Dry runs are recorded.
2. Submit without `dry_run`. No checkbox or draft note completes the decision.
3. Continue unrelated work or end the turn. A registered callback announces
   the committed decision, cancellation, or expiry. `inbox.wait` is optional.

The human submits a complete accepted/rejected mapping with optional reasons
and an overall note. Mixed and all-rejected decisions are valid. Only accepted
calls receive grants. One logical callback contains the complete decision.
The default pending deadline is 24 hours; the default approved grant lifetime
is 30 minutes. These are different deadlines.

## Read the decision and execute

- A callback is an announcement, not permission. Read `inbox.status` for current
  verdicts, reasons, `owner_note`, granted scope, expiry, and execution state.
- Execute accepted calls with `_grant: "tyg_…"` and the exact approved scope.
  In `executeToolCode` the grant is one more argument of the call:
  `server.tool(param="value", _grant="tyg_…")` in Starlark,
  `await tools.server.tool({ param: "value", _grant: "tyg_…" })` in JavaScript.
  If `narrowed` is true, use the returned `params`.
- A grant is single-use and bound to the user, agent, call, scope and expiry.
  Rejected calls have no grant. Do not execute them.
- If the grant response is lost, use read-only `inbox.status` to recover the
  same valid unused token. Inspection does not consume the grant or extend it.
  Never log, commit or share grant tokens.
- Decision state and execution state differ. Execution can be not started,
  running, succeeded, failed, or outcome unknown. A used grant cannot create
  another execution claim.
- If a write times out after dispatch, `outcome_unknown` can mean it committed.
  Inspect the upstream state before recovery. Never blindly repeat that write.
- Revocation and expiry do not silently issue replacement permission.
- Existing legacy requests can retain Toolyard execution after acceptance.
  Inspect their saved result instead of repeating the original write.

When appropriate, report the outcome with `inbox.post({ kind: "update", … })`.
If a call is rejected, choose the next task step from the owner's explanation.

## Never bypass the decision

Do not use another credential, another agent, or a direct API to evade a
restriction. Do not split a risky action to hide its scope. Do not invent
urgency. Do not ask for tokens or one-time codes when a connection is absent.
Show its connection link and wait for confirmed access.

Use `inbox.guide()` or `inbox.guide({ topic: "examples" })` for the complete
contract and examples.
