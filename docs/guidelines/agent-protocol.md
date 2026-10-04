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
5. Obtain an authorized callback reference before the request when your client supports it.
   Pass `callback_ref`, continue independent work, and retrieve authoritative `inbox.status` after the callback.
   `inbox.wait` remains optional.
6. For each allowed tool you get a **grant**. Call the tool with
   `_grant: "tyg_…"`, exactly within its parameters.
7. When you're done, `inbox.post` an update, also in the first person.
8. A denial is an answer. Ask again only with new information.
9. When a call answers that a server isn't connected (your owner has to
   sign in), show the person the connect link it gives you as a clickable
   Markdown link, and retry after they say they're done; `connections.status`
   lists every sign-in and `connections.link` gets a fresh link. Never ask
   them for a password, token or one-time code: the sign-in happens at the
   provider, in their browser.

---

## 1. Which tools are restricted

You can't tell from the name. Ask:

```json
inbox.check({ "calls": [
  { "tool": "github.merge_pull_request", "args": { "repo": "acme/api", "pull_number": 218 } },
  { "call_id": "deploy_api",
  "tool": "deploy.run",
  "target": "Production API service",
  "operation": "write",
  "expected_effects": "The API uses the new billing handler with its flag disabled.",
  "affected_scope": "Only the production API deployment",
  "material_risks": "A wrong release can interrupt invoice requests.",
  "undo": "Redeploy the current known good release 9f1e2d0.", "args": { "service": "api", "env": "prod" } }
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
  "task": {"objective": "Deliver the billing handler with the rollout flag disabled."},
  "client_request_id": "billing-v2-permission-1",
  "callback_ref": "opaque-authorized-receiver-reference",
  "pending_ttl_seconds": 86400,
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
| `task.objective` | yes | The concrete objective this batch advances. |
| `client_request_id` | no | Stable per-agent retry key; changed content conflicts. |
| `callback_ref` | no | Opaque registered receiver reference; no credentials. |
| `pending_ttl_seconds` | no | Deadline for the human decision. Default 86400 seconds (24 hours); administrator limits apply. |
| `title` | yes | ≤ 60 characters. Verb + object + target. |
| `summary` | yes | ≤ 200 characters. The line on the inbox card. |
| `message` | yes | First person. What you want to do, and the context your owner needs. |
| `facts` | yes | `why_now`, `if_it_goes_wrong`, `undo`. Concrete; numbers over adjectives. |
| `audio.script` | no | Optional audio summary. See §5 for limits. |
| `urgency` | yes | `now` \| `soon` \| `digest` \| `fyi`. See §7. |
| `tools` | yes | 1–12 entries. |
| `attachments` | no | Up to 12. Strongly recommended. |
| `ttl_seconds` | no | How long grants last once approved. Default 1800, max 86400. Ask for the shortest that works. |
| `session_id` | no | From `session.start`. Attaches task context to related requests. |
| `request_id` | no | On `inbox.post` updates: the request this update closes. |

For a question, use the version 2 `inbox.ask` contract. Audio is optional.

```json
{
  "schema_version": 2,
  "client_request_id": "stable-key-for-this-question",
  "prompt": "Which checks must run?",
  "context": "Choose the checks for this stage release.",
  "question": {
    "type": "multiple_choice",
    "min_selections": 1,
    "max_selections": 2,
    "options": [
      {"id": "mobile", "label": "Mobile layout"},
      {"id": "keyboard", "label": "Keyboard access"},
      {"id": "none", "label": "Neither", "exclusive": true}
    ]
  },
  "task": {"title": "Review stage"},
  "blocking": false
}
```

Question types are `free_text`, `single_choice`, and `multiple_choice`.
Free-text questions have no options. Choice questions accept 2–12 options with
unique stable IDs and labels. An exclusive option cannot accompany another
selection. A recommendation is a label, never a default selection.
The owner can always submit custom text without a selection. Text can also
accompany selected options. Selection alone never sends an answer.

`inbox.status` and `inbox.wait` return `response.selected_option_ids`,
`response.selected_labels`, and the exact `response.text`. The legacy `answer`
string remains available. A saved answer becomes retrieved when the agent reads
it through those tools. An answer grants no tool permission.

The dashboard submits a `request_revision`, a stable `submission_id`, and
`response` through the decision endpoint. A retry of the same answer from the
same actor returns the saved result. A conflicting retry returns 409. Expired
requests return 410. Drafts remain local to the user and browser tab for up to
24 hours, and logout clears them.

Older `inbox.ask` submissions with `title`, `summary`, `message`, and 2–4
`options` remain supported. They appear as single-choice questions with a
custom text field. Set `kind: "blocker"` when the question blocks progress.

## 4. Tools and parameters

```json
{ "tool": "deploy.run",
  "required": true,
  "summary": "Deploy the api service to prod at the merge commit from step 1, once the migration has succeeded.",
  "params": {
    "service": { "eq": "api" },
    "env":     { "eq": "prod" },
    "ref":     { "limit": "merge commit of PR #218", "pattern": "^[0-9a-f]{7,40}$" }
  },
  "after": ["migrate_billing"] }
```

- **`summary` is yours**, and your owner reads it. Describe what the call
  does in plain words, not the tool's description.
- **`call_id`** is a stable unique identifier, distinct from the tool name. Repeated tools need distinct call IDs.
- **`summary`** explains the purpose and its relationship to the task.
- **`target`**, **`operation`**, and **`expected_effects`** are required for every call.
- For writes, supply **`affected_scope`**, **`material_risks`**, and **`undo`**.
  State explicitly when the action cannot be undone. Catalog metadata determines whether a tool is read-only.
- **`required`** is only a planning hint. The human can reject any call and complete the decision.
  Return rejected calls to the agent without permission.
- **`params`**: give every argument you'll pass. **Your grant covers only
  the arguments you list**; a call that passes anything else is blocked.
  - `{"eq": value}` is an exact value. Prefer it. A bare value (`"env": "prod"`)
    means the same.
  - `{"in": [...]}`, `{"prefix": "..."}`, `{"gte": n, "lte": n}` narrow a value.
  - `{"limit": "what it will be", "pattern": "regex"}` is for a value that
    doesn't exist yet (a commit a merge will create). Toolyard checks the
    actual value against `pattern` when you make the call. Without an enforced pattern this scope is rejected.
  - `{"any": true}` is unbounded and is rejected.
- **`after`** references call IDs, never tool names. Invalid references and cycles are rejected.
  Dependencies do not authorize rejected calls or widen accepted parameters.

## 5. The voice note

Your owner often listens before reading. Toolyard turns your script into
speech (recorded on the server, or read by the owner's browser); you never
handle audio files. The script is read aloud exactly as written.

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

Your owner decides when their phone buzzes, not you: `now` pushes at once,
`soon` is grouped with your session's other requests for a minute and a half,
`digest` waits for the owner's next digest (by default 09:30, 13:30 and
18:30), and `fyi` never pushes. Quiet hours hold everything except the tools
your owner allows through.

Each agent gets **3 `now` requests an hour** (your owner can change this).
Past that, toolyard lowers the request to `soon`, shows your owner it was
lowered, and returns a warning with `path: "urgency"`.

## 8. Dry run

`dry_run: true` sends nothing to your owner. You get back:

```json
{
  "ok": false,
  "problems": [
    { "path": "audio.script", "message": "112 words; max 75. Keep the ask and one number." },
    { "path": "attachments[1].url", "message": "localhost isn't reachable from toolyard." }
  ],
  "warnings": [],
  "flags": [
    { "tool": "flags.set", "level": "red", "label": "Doesn't match the request",
      "why": "Your message says the flag stays off. This call turns it on." }
  ],
  "preview": { "card_title": "Ship billing v2 to production", "audio_seconds": 28 }
}
```

- **Fix every problem.** A request with problems is rejected.
- **Read the warnings.** They don't block sending, but your owner will notice
  what they point at.
- **Flags aren't errors.** They're what your owner will see. Judge flags
  ("Doesn't match the request") only appear when your owner has the judge
  model switched on. Where a flag is
  fair, add evidence for it. Where it's caused by a mistake, fix the mistake.
- **Don't reword a request to make a flag go away.** Dry runs are logged, and
  your owner sees how many you ran.

## 9. Waiting, grants and using them

`inbox.request` returns `request_id`, `status`, and the pending `expires_at`.

- Keep working on anything that doesn't depend on the answer.
- `inbox.status({ "ids": [...] })` never blocks.
- `inbox.wait({ "ids": [...], "mode": "any" | "all", "timeout_seconds": ≤300 })`
  blocks. Don't poll in a tight loop.

A decision looks like this:

```json
{ "request_id": "rq_…", "kind": "access", "status": "approved",
  "tools": [
    { "tool": "github.merge_pull_request", "decision": "allowed", "grant_id": "gr_…", "grant": "tyg_…" },
    { "tool": "deploy.run", "decision": "allowed", "grant_id": "gr_…", "grant": "tyg_…",
      "narrowed": true, "params": { "env": { "eq": "prod" }, "ref": { "eq": "7c1d2e9" } } },
    { "tool": "flags.set",                 "decision": "refused" }
  ],
  "owner_note": "Don't touch the flag; I'll roll it out myself.",
  "grants_expire_at": 1790621520000,
  "next": "Call each allowed tool with its grant as `_grant`, …" }
```

`status` is one of `pending`, `approved`, `denied`, `returned`, `answered`,
`cancelled`, `expired`. Always read `next`: it says what to do.

- Status reads do not consume grants. The authorized agent can recover the same still-valid unused token after a lost response.
  Recovery adds no use and never extends or replaces permission. Keep tokens out of logs, commits, and callbacks.
- Call the tool with `_grant` added to its arguments. Stay inside the
  parameters you asked for, or the call is blocked.
- Your owner can **narrow** a tool before allowing it: fewer values, an
  exact value instead of a `limit`, a tighter range, or a shorter
  `grants_expire_at`. They can never widen it. A narrowed tool has
  `"narrowed": true` and its `params` are what the grant allows: use those.
- Read `owner_note`, and honour it even for tools that were allowed.
- Read each call's `call_id`, `verdict` (`accepted` or `rejected`), and optional written `reason`.
  An all-rejected batch has status `denied` and no grants. Mixed batches grant only accepted calls.
- Execution is separate: `not_started`, `running`, `succeeded`, `failed`, or `outcome_unknown`.
  A claimed grant never executes again. If an upstream write times out or the process stops after the claim, inspect upstream state.
  Do not blindly repeat a write with an unknown outcome.
- A callback announces one committed decision, with every verdict and note, and no grant tokens or sensitive tool results.
  Retrieve current authoritative status after the callback; it is not permission itself.
- A pending request expires after 24 hours by default. Grant lifetime defaults to 1800 seconds after acceptance.
  Expired or revoked grants cannot be revived.

A human submission supplies the Inbox request ID in the URL and this complete contract:

```json
{
  "action": "submit",
  "request_revision": 1,
  "submission_id": "stable-decision-retry-key",
  "verdicts": {
    "migrate_billing": {"verdict": "rejected", "reason": "The migration is not ready."},
    "deploy_api": {"verdict": "accepted", "reason": "The release is ready."}
  },
  "note": "Keep the rollout flag disabled."
}
```

Every requested call ID must appear exactly once. No callback is emitted for draft selections or notes.
The decision, accepted-call grants, audit, and callback event commit together.
The same submission ID and content return the saved result. Changed content or a stale revision returns a conflict.
A competing decision cannot overwrite a committed decision. Drafts remain available after a failed or conflicting submission.

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
- Never ask your owner for a password, token or one-time code to get past a
  sign-in; give them the connect link and wait.

## 12. Examples

**An access request.** Evidence first, every tool in one request, and the
voice note in the first person.

```json
inbox.request({
  "title": "Ship billing v2 to production",
  "summary": "I need 3 tools to merge PR #218, add one table, and deploy.",
  "message": "PR #218 is approved and all 42 checks are green, so I'd like to ship the billing v2 webhook handler. It goes out behind the billing_v2 flag, which stays off.",
  "facts": {
    "why_now": "The invoice migration is waiting on this handler.",
    "if_it_goes_wrong": "The API service only. With the flag off there's no user-visible change.",
    "undo": "I redeploy 9f1e2d0, the current prod build. About 4 minutes."
  },
  "audio": {
    "script": "Hi, it's the billing agent. I'd like to ship billing v2 to production. I need to merge the approved pull request, add one new table, and deploy. The new code sits behind a flag that stays off. All 42 checks passed. I'm asking for one merge, one migration and one deploy, valid for 30 minutes."
  },
  "urgency": "soon",
  "tools": [
    {
      "tool": "github.merge_pull_request",
      "required": true,
      "summary": "Squash-merge PR #218 into main.",
      "params": {
        "repo": "acme/api",
        "pull_number": 218,
        "merge_method": "squash"
      },
      "call_id": "merge_billing",
      "target": "Production billing API",
      "operation": "write",
      "expected_effects": "Squash-merge PR #218 into main.",
      "affected_scope": "Billing API release and configuration",
      "material_risks": "An incorrect release can interrupt invoice creation.",
      "undo": "Redeploy release 9f1e2d0 and restore its configuration.",
      "after": []
    },
    {
      "tool": "db.migrate",
      "required": true,
      "summary": "Apply migration 0042, which adds the webhook_events table.",
      "params": {
        "env": "prod",
        "migration": "0042_webhook_events",
        "direction": "up"
      },
      "call_id": "migrate_billing",
      "target": "Production billing API",
      "operation": "write",
      "expected_effects": "Apply migration 0042, which adds the webhook_events table.",
      "affected_scope": "Billing API release and configuration",
      "material_risks": "An incorrect release can interrupt invoice creation.",
      "undo": "Redeploy release 9f1e2d0 and restore its configuration.",
      "after": []
    },
    {
      "tool": "deploy.run",
      "required": true,
      "summary": "Deploy the api service to prod at the merge commit.",
      "params": {
        "service": "api",
        "env": "prod",
        "ref": {
          "limit": "merge commit of PR #218",
          "pattern": "^[0-9a-f]{7,40}$"
        }
      },
      "after": [
        "merge_billing",
        "migrate_billing"
      ],
      "call_id": "deploy_api",
      "target": "Production billing API",
      "operation": "write",
      "expected_effects": "Deploy the api service to prod at the merge commit.",
      "affected_scope": "Billing API release and configuration",
      "material_risks": "An incorrect release can interrupt invoice creation.",
      "undo": "Redeploy release 9f1e2d0 and restore its configuration."
    }
  ],
  "attachments": [
    {
      "type": "table",
      "title": "Checks",
      "columns": [
        "Check",
        "Result"
      ],
      "rows": [
        [
          "Unit tests",
          "1,284 passed"
        ],
        [
          "Integration",
          "212 passed"
        ]
      ]
    },
    {
      "type": "chart",
      "chart": "line",
      "title": "Error rate, canary vs prod",
      "unit": "%",
      "x": [
        "-30m",
        "-15m",
        "now"
      ],
      "series": [
        {
          "name": "canary",
          "values": [
            0.19,
            0.21,
            0.2
          ]
        },
        {
          "name": "prod",
          "values": [
            0.2,
            0.2,
            0.2
          ]
        }
      ],
      "spoken": "the canary has been flat for 30 minutes"
    },
    {
      "type": "diff",
      "file": "handlers/webhook.go",
      "patch": "@@ -41,4 +41,6 @@\n-\treturn err\n+\treturn h.retry(ev)\n",
      "caption": "The core of the change: failed deliveries now retry."
    },
    {
      "type": "link",
      "label": "PR #218",
      "url": "https://github.com/acme/api/pull/218"
    }
  ],
  "ttl_seconds": 1800,
  "task": {
    "objective": "Deliver the billing handler with its rollout flag disabled."
  }
})
```

**A question.**

```json
inbox.ask({
  "title": "Retire /v1/invoices now, or in 30 days?",
  "summary": "Traffic to the old endpoint fell about 80% in two weeks. Three callers are left.",
  "message": "The migration to /v2/invoices is done on our side. Three callers are left; the biggest is our own reporting job, which I can update myself.",
  "audio": { "script": "I need a decision on the old invoices endpoint. Its traffic fell by about 80 percent in two weeks. Should I retire it now, or give it 30 more days?" },
  "urgency": "soon",
  "options": [
    { "label": "Retire now", "detail": "Returns 410 Gone. I update the reporting job first." },
    { "label": "Deprecate, retire in 30 days", "detail": "Adds a Sunset header and emails the 2 external callers." }
  ]
})
```

**An update that closes a request.**

```json
inbox.post({
  "kind": "update", "request_id": "rq_…",
  "title": "Billing v2 is live in prod, flag still off",
  "summary": "Merged, migrated and deployed. Errors are flat.",
  "message": "Done. PR #218 is merged, the webhook_events table exists, and the API is deployed. Error rate has been flat since.",
  "audio": { "script": "All done. The pull request is merged, the new table is in place, and the API is deployed. Errors have been flat since." },
  "urgency": "fyi"
})
```
