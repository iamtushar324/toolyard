---
name: toolyard-inbox
description: How to ask your owner for permission through toolyard. Use when a toolyard tool returns permission_required or grant_invalid, or before starting a task that will deploy, migrate a database, send email or messages, spend money, change production, or touch secrets.
---

# Asking your owner through toolyard

Some toolyard tools are **restricted**. Calling one without permission runs
nothing and returns `permission_required` with a draft request. Your owner
approves requests from a phone, usually listening to your voice note first,
then reading your evidence, and deciding at the end.

## Before a task

1. List the calls you plan to make and run `inbox.check({ calls })`.
2. Put **every** `restricted` tool into **one** `inbox.request`. Mark each
   one `required` or optional.
3. Collect your evidence now, while you have it: the PR link, test results,
   the diff that matters, a screenshot or a short screen recording.

## Writing the request

- `title`: at most 60 characters. `summary`: at most 200.
- `message`: first person. What you want to do and why.
- `facts`: `why_now`, `if_it_goes_wrong`, `undo`, with concrete numbers.
- `audio.script`: **at most 75 words, first person, written to be heard.** No
  IDs, hashes or URLs. Order: what you want, why it's safe, one or two numbers,
  then the exact ask.
- Each tool: your own one-line `summary`, and `params` with `eq` wherever you
  know the value. Use `limit` only for values that don't exist yet.
- `attachments`: send data, not pictures of data (`table`, `chart`, `diff`,
  `code`, `log`, `markdown`, `link`). Send media as `https` links (`image`,
  `video` as MP4, `file`). Toolyard copies them when you send. Add a
  first-person `caption` to each.
- Keep the voice note, the message and the tools consistent. Toolyard flags
  contradictions.

## Sending

1. `inbox.request({ …, dry_run: true })`. Fix every problem. Read the flags;
   add evidence where a flag is fair. **Don't reword to hide a flag**: dry
   runs are logged and your owner sees the count.
2. Send it without `dry_run`. Keep working on anything that doesn't depend on
   the answer.
3. `inbox.wait({ ids, mode: "any", timeout_seconds: 300 })` when you run out
   of other work.

## After the decision

- Each allowed tool comes with a grant. Call the tool with `_grant: "tyg_…"`,
  inside the parameters you asked for. The token is shown once; never log it,
  commit it or share it.
- If a tool says `"narrowed": true`, your owner tightened it: call it with
  the values in its `params`, not the ones you asked for.
- Follow the `owner_note`.
- If the request comes back as `returned`, a required tool was refused. Replan,
  and say what changed.
- When done: `inbox.post({ kind: "update", … })` with what happened and a
  short voice note.

## Never

- Never get around a restricted tool (other credentials, direct API calls,
  another agent).
- Never split a risky action to avoid a flag, or claim urgency you don't have.
  `now` is limited per agent per hour; past the limit it becomes `soon`.

Full details: `inbox.guide()`, or `inbox.guide({ topic: "examples" })` for
worked examples.
