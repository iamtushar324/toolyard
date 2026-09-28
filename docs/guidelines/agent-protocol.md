# toolyard — agent protocol

This file is written **to agents**. It's the single source for everything an
agent is told about asking its owner for permission:

- `inbox.guide` returns it (split by topic);
- `GET /v1/guide` serves it to agents that don't use MCP;
- the `toolyard-inbox` skill ([skill/toolyard-inbox/SKILL.md](skill/toolyard-inbox/SKILL.md)) is a shorter version of it.

How agents are taught this, and why it's layered, is in
[agent-onboarding.md](agent-onboarding.md).

---

## The short version

1. Some tools are **restricted**. Calling one without permission does
   nothing and returns `permission_required` with a draft request. Nothing
   reaches your owner until you send a proper request.
2. Before a task, run `inbox.check` on the calls you plan to make. Put
   **every** restricted tool the task needs into **one** `inbox.request`.
3. Write the request as yourself, in the first person: a message, a voice-note
   script, and the evidence. Your owner reads it on a phone and often listens
   first.
4. Run `inbox.request` with `dry_run: true` first. Fix every problem it lists.
5. Keep working on anything that doesn't depend on the answer. Use
   `inbox.wait` when you run out.
6. For each allowed tool you get a **grant**. Call the tool with
   `_grant: "tyg_…"`, exactly within its parameters.
7. When you're done, `inbox.post` an update, also in the first person.
8. A denial is an answer. Ask again only with new information.

---

## 1. Which tools are restricted

You can't tell from the name. Ask:

```json
inbox.check({ "calls": [
  { "tool": "github.merge_pull_request", "args": { "repo": "acme/api", "pull_number": 218 } },
  { "tool": "deploy.run", "args": { "service": "api", "env": "prod" } }
]})
```

Each call comes back as one of:

| status | meaning |
|---|---|
| `open` | You can call it now. |
| `granted` | A live grant already covers it. Pass `_grant`. |
| `restricted` | Include it in a request. |
| `denied` | Your owner has blocked this tool for you. Don't ask. |

## 2. When you call a restricted tool anyway

You get this back (`isError: true`, because nothing ran):

```json
{
  "status": "permission_required",
  "executed": false,
  "message": "deploy.run is restricted. Nothing ran. Ask your owner with inbox.request, starting from the draft below.",
  "draft": {
    "title": "<≤60 chars>",
    "summary": "<≤200 chars, one line for the inbox card>",
    "message": "<first person: what you want to do and why>",
    "facts": { "why_now": "<…>", "if_it_goes_wrong": "<…>", "undo": "<…>" },
    "audio": { "script": "<≤75 words, first person>" },
    "tools": [
      { "tool": "deploy.run", "required": true, "summary": "<what this call does>",
        "params": { "service": { "eq": "api" }, "env": { "eq": "prod" }, "ref": { "eq": "a41c9e2" } } }
    ],
    "attachments": []
  },
  "also_restricted_nearby": ["db.migrate", "flags.set"],
  "guide": "inbox.guide({ \"topic\": \"requests\" })"
}
```

Don't retry the call. Fill in the draft, add any other tools the task needs,
and send it.

## 3. The request

```json
inbox.request({
  "title": "Ship billing v2 to production",
  "summary": "I need 5 tools to merge PR #218, add one table, deploy, and let the team know.",
  "message": "PR #218 is approved and all 42 checks are green, so I'd like to ship the billing v2 webhook handler. It goes out behind the billing_v2 flag, which stays off…",
  "facts": {
    "why_now": "The invoice migration is waiting on this handler.",
    "if_it_goes_wrong": "The API service only. With the flag off there's no user-visible change.",
    "undo": "I redeploy 9f1e2d0, the current prod build. About 4 minutes."
  },
  "audio": { "script": "Hi, it's the billing agent. I'd like to ship billing v2 to production. …" },
  "urgency": "soon",
  "tools": [ … see §4 … ],
  "attachments": [ … see §6 … ],
  "ttl_seconds": 3600,
  "dry_run": true
})
```

| field | required | rules |
|---|---|---|
| `title` | yes | ≤ 60 characters. Verb + object + target. |
| `summary` | yes | ≤ 200 characters. The line on the inbox card. |
| `message` | yes | First person. What you want to do, and the context your owner needs. |
| `facts` | yes | `why_now`, `if_it_goes_wrong`, `undo`. Concrete; numbers over adjectives. |
| `audio.script` | yes | See §5. |
| `urgency` | yes | `now` \| `soon` \| `digest` \| `fyi`. See §7. |
| `tools` | yes | 1–12 entries. |
| `attachments` | no | Up to 12. Strongly recommended. |
| `ttl_seconds` | no | How long grants last once approved. Default 1800, max 86400. Ask for the shortest that works. |
| `session_id` | no | From `session.start`. Groups your requests in your owner's Sessions view. |

For a decision rather than a permission, use `inbox.ask` with the same
`title`, `summary`, `message`, `audio` and `attachments`, plus `options`
(2–4 entries of `{ "label", "detail" }`). Set `kind: "blocker"` if you're
stuck, rather than choosing between options.

## 4. Tools and parameters

```json
{ "tool": "deploy.run",
  "required": true,
  "summary": "Deploy the api service to prod at the merge commit from step 1, once the migration has succeeded.",
  "params": {
    "service": { "eq": "api" },
    "env":     { "eq": "prod" },
    "ref":     { "limit": "merge_commit_of", "pr": 218 }
  },
  "after": ["db.migrate"] }
```

- **`summary` is yours**, and your owner reads it. Describe what the call
  does in plain words, not the tool's description.
- **`required`**: without this tool, the task fails. If your owner unticks a
  required tool, the whole request comes back to you to replan. Optional
  tools can be dropped individually.
- **`params`**: give every argument you'll pass.
  - `eq` is an exact value. Prefer it.
  - `in`, `prefix`, `lte` and `gte` narrow a value.
  - `limit` is for a value that doesn't exist yet (a commit a merge will
    create). Toolyard checks it when you make the call.
  - Arguments you leave out are pinned to what you'd pass by default. Your
    grant won't cover anything else.
- **`after`**: the order you'll run things in. It's shown to your owner as
  a plan.

## 5. The voice note

Your owner often listens before reading. Toolyard turns your script into
speech; you never handle audio files.

- **75 words at most** (about 30 seconds).
- **First person**: "I'd like to…", "I need…".
- **Written for the ear**: no hashes, IDs, URLs or code. Round numbers, and
  say units in words ("0.2 percent").
- **In this order**: what you want, why it's safe, the one or two numbers
  that matter, then the exact ask.
- **Consistent with your message.** If you say a flag stays off, don't
  request a tool that turns it on. Toolyard flags contradictions.

## 6. Attachments: show, don't describe

Send **data** when you can. Toolyard renders it natively, so it's sharp on a
phone and readable in both themes.

| type | fields | use it for |
|---|---|---|
| `markdown` | `title`, `body` | notes, lists, short explanations |
| `table` | `title`, `columns`, `rows` | test results, row counts, recipients |
| `chart` | `title`, `chart` (`line` \| `bar`), `unit`, `x`, `series: [{ name, values }]` | error rates, traffic, anything over time |
| `diff` | `file`, `patch` (unified diff) | the core of a code change; keep it to the part that matters |
| `code` | `file`, `language`, `body` | a migration, a config change |
| `log` | `title`, `body` | terminal output, error messages |
| `image` | `url`, `alt` | screenshots |
| `video` | `url`, `poster_url?` | screen recordings of the thing working (MP4 H.264, ≤ 2 min) |
| `file` | `url`, `name` | CSVs, reports |
| `link` | `label`, `url` | PRs, CI runs, dashboards |

Every attachment takes an optional **`caption`**: one first-person sentence
on why it matters ("I replayed a failing delivery. It retried twice and got
through."). A chart takes an optional **`spoken`** phrase: the words your
voice note uses for it.

**Media is sent as links.**
- `image`, `video` and `file` take an `https` URL that toolyard can reach.
- Toolyard **fetches each link once when you send the request, and keeps a
  copy**, so it still works after your sandbox is gone.
- A `localhost` or private-network URL fails the dry run.
- Limits: image 10 MB, video 50 MB, file 25 MB.
- If you have nowhere to host a file, ask your owner which store to upload to
  (`inbox.guide({ "topic": "hosting" })`).

## 7. Urgency is a promise

| urgency | use when |
|---|---|
| `now` | Production is broken, or money or data is being lost right now. |
| `soon` | You'll be blocked within the hour. |
| `digest` | You need it today, not this hour. |
| `fyi` | No action needed. Updates only. |

Toolyard rate-limits `now` and lowers the urgency of agents that overuse it.

## 8. Dry run

`dry_run: true` sends nothing to your owner. You get back:

```json
{
  "ok": false,
  "problems": [
    { "path": "audio.script", "message": "112 words; max 75. Keep the ask and one number." },
    { "path": "attachments[1].url", "message": "localhost isn't reachable from toolyard." }
  ],
  "flags": [
    { "tool": "flags.set", "level": "red", "label": "Doesn't match the request",
      "why": "Your message says the flag stays off. This call turns it on." }
  ],
  "preview": { "card_title": "Ship billing v2 to production", "audio_seconds": 28 }
}
```

- **Fix every problem.** A request with problems is rejected.
- **Flags aren't errors.** They're what your owner will see. Where a flag is
  fair, add evidence for it. Where it's caused by a mistake, fix the mistake.
- **Don't reword a request to make a flag go away.** Dry runs are logged, and
  your owner sees how many you ran.

## 9. Waiting, grants and using them

`inbox.request` returns `{ "request_id": "rq_…", "expected_decision_in_seconds": … }`.

- Keep working on anything that doesn't depend on the answer.
- `inbox.status({ "ids": [...] })` never blocks.
- `inbox.wait({ "ids": [...], "mode": "any" | "all", "timeout_seconds": ≤300 })`
  blocks. Don't poll in a tight loop.

A decision looks like this:

```json
{ "request_id": "rq_…", "status": "decided",
  "tools": [
    { "tool": "github.merge_pull_request", "allowed": true,  "grant": "tyg_…" },
    { "tool": "flags.set",                 "allowed": false }
  ],
  "owner_note": "Don't touch the flag; I'll roll it out myself.",
  "expires_at": "2026-09-28T19:12:00Z" }
```

- Each grant **token is shown once**. Keep it in memory, never in logs, commits
  or PRs, and never pass it to another agent. It only works for you anyway.
- Call the tool with `_grant` added to its arguments. Stay inside the
  parameters you asked for, or the call is blocked.
- Read `owner_note`, and honour it even for tools that were allowed.
- If `status` is `returned`, a required tool was refused. Replan, and say in
  your next request what changed.

## 10. Close the loop

When the task is done, send an update with what happened and how you checked
it, again with a voice note:

```json
inbox.post({ "kind": "update", "request_id": "rq_…",
  "title": "Billing v2 is live in prod, flag still off",
  "message": "Done. PR #218 is merged as 7c1d2e9, …",
  "audio": { "script": "All done. …" },
  "attachments": [ { "type": "chart", … } ] })
```

Withdraw requests you no longer need with `inbox.cancel`.

## 11. Never

- Never get around a restricted tool: no other credentials, no direct API
  calls, no asking another agent to do it.
- Never split one risky action into small requests to avoid a flag.
- Never tune your wording across dry runs to hide what a call does.
- Never claim urgency you don't have.
