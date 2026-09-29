# toolyard — how agents learn to ask

Status: **draft**. What agents are told is in
[agent-protocol.md](agent-protocol.md). This document is how and when they're
told it, and what has to change in the gateway. The decision record is
[ADR 0006](../adr/0006-coach-dont-queue.md).

## Goal

Any agent, from any provider, running anywhere, sends a good request the
first time it needs one, without a human explaining toolyard to it. "Good"
means the request renders as the review page in the prototype: a first-person
message, a voice note, evidence, and every tool the task needs in one
request.

## Principle: teach at the moment of need

Agents don't read manuals up front, and every client supports different
things. So the lesson comes in layers, each one working even when the others
don't:

| # | Moment | What the agent gets | Works in |
|---|---|---|---|
| 0 | **Enrolment** | The `toolyard-inbox` skill (Claude Code) or an AGENTS.md block (everything else) | Clients with skills or project instructions |
| 1 | **Connect** | Three lines of MCP `instructions` | Every MCP client |
| 2 | **Calls a restricted tool** | A `permission_required` result with a **pre-filled draft** | Every MCP client, plus the CLI and REST |
| 3 | **Wants details** | `inbox.guide(topic)`: the protocol, the format and examples | Every MCP client, plus `GET /v1/guide` |
| 4 | **Drafts a request** | `dry_run: true` returns fixable problems and the flags your owner would see | Every client |
| 5 | **Gets it wrong later** | Validation errors on the real call, and a grant mismatch explained the same way | Every client |

Layer 2 is the one that matters most. It turns the first mistake into a
lesson, and the draft means the agent never has to guess the parameter
format.

## One source, four outputs

`docs/guidelines/agent-protocol.md` is embedded in the binary (`go:embed`) and
served as:

1. **`inbox.guide({ topic })`**, split on `##` headings (`requests`, `tools`,
   `voice`, `attachments`, `dry-run`, `grants`, `examples`, `hosting`).
2. **`GET /v1/guide`**, for agents without MCP. It uses the same
   bearer token.
3. **The MCP resource `toolyard://guide`**, for clients that read resources.
4. **The built-in skill `toolyard-inbox`**, a short version that points to
   `inbox.guide` for detail. Served at `GET /v1/guide/skill` and as the MCP
   resource `toolyard://skill/toolyard-inbox`; `toolyard skills install
   toolyard-inbox` writes it to `~/.claude/skills/`.

Both files are embedded from `docs/` (package `docs`), so the gateway
serves exactly what's in the repo. Tests check that the protocol's worked
examples pass validation and that the protocol and the skill state the same
limits as the code.

## Layer 0: enrolment

The dashboard's agent-enrolment modal already shows setup snippets per client
(CLI, hermes, Claude Code). Each one gets a second step:

- **Claude Code:** a `curl` of `/v1/guide/skill` into
  `~/.claude/skills/toolyard-inbox/SKILL.md` (shown with the agent's token),
  or `toolyard skills install toolyard-inbox`.
- **Codex, Cursor, others:** a 10-line AGENTS.md / rules block to paste. It
  says restricted tools exist, names `inbox.check` and `inbox.request`, and
  points to `inbox.guide`.
- **hermes:** the same block, added to the system prompt in
  `~/.hermes/config.yaml`.

This layer is a head start, not a requirement. Everything still works through
layers 1–5 if the agent skips it.

## Layer 1: the connection instructions

`buildInstructions` (`internal/gateway/server.go`) now opens with a
PERMISSIONS paragraph, ahead of the existing approval text (which still
applies in `execute` mode):

> PERMISSIONS: some tools are restricted and need your owner's approval.
> Before a task, run `inbox.check` on the calls you plan, then ask for every
> restricted tool in ONE `inbox.request` (call `inbox.guide` first …). When
> approved, call each tool with `_grant` set to its token. If a call returns
> status `permission_required`, nothing ran: fill in the draft it gives you.

## Layer 2: the coaching result

When a restricted tool is called without a valid `_grant`, the gateway:

1. **Does not run the call** and **does not create a request.**
   Nothing reaches the owner's inbox.
2. Returns `isError: true` with `status: "permission_required"`, a message
   saying nothing ran, and a `draft`:
   - the tool and its **exact arguments, already converted to `params` with
     `eq`**;
   - placeholders for the title, message, facts, voice-note script and
     attachments;
   - `also_restricted_nearby`: other restricted tools on the same upstream,
     so the agent thinks about bundling;
   - a pointer to `inbox.guide`.
3. Writes an audit row `call.coached`. The owner's Sessions view can show
   "coached 3 times" per agent, which is useful for spotting agents that ignore
   the guide.

The full shape is in [agent-protocol.md §2](agent-protocol.md#2-when-you-call-a-restricted-tool-anyway).

Grant problems get the same treatment. A call with an expired, used-up or
out-of-scope `_grant` returns `grant_invalid`, says which check failed, and
includes a draft for a new request. That draft uses the call's actual
arguments, and says what differed from the grant.

## Layer 4: dry run

`inbox.request({ …, dry_run: true })` runs everything except delivery:

- **Validation:** limits, required fields, word count, and whether each
  attachment URL is reachable and within size.
- **Flags:** all of them, rules and judge alike (owner decision). The agent sees
  what the owner will see.
- **A preview:** the card title and the voice-note length.

**Safeguard for showing all flags:** every dry run is stored with the request's
draft history. The owner's review page shows "3 dry runs before sending",
and "Ask toolyard" can show what changed between them. An agent that rewords
its request until the judge stops flagging it leaves a visible trail.

Dry runs are rate-limited per agent (default 20 per hour).

## Attachments by link, with a snapshot

Owner decision: agents send media (image, video, file) as **links**. Inline
data types (table, chart, diff, code, log, markdown) stay in the JSON.

A plain link has two problems:

- links into a cloud sandbox die when the sandbox does;
- the owner's phone would load files from any host an agent chooses.

So toolyard **fetches each link once when the request is sent and serves its
own copy**:

- fetched by the gateway with a timeout, a size cap per type, and a
  content-type check;
- stored under `<data>/attachments/<sha256>`, and served by the dashboard
  from its own origin;
- never followed to `localhost` or private networks (so it can't be abused to
  reach internal services);
- videos accepted as MP4 (H.264), which plays on iPhone. A poster frame is
  taken from `poster_url`, or left blank.

The setting `attachments.snapshot = true | false` (default `true`) lets the
owner turn copying off and have the phone load the original URLs directly.

## New tools, all under `inbox.*`

All of them are always visible, need no approval, and act only on the calling
agent's own requests.

| tool | purpose |
|---|---|
| `inbox.guide(topic?)` | The protocol, split by topic |
| `inbox.check(calls[])` | For each call: `open`, `granted`, `restricted` or `denied` |
| `inbox.request({…, dry_run?})` | An access request with 1–12 tools |
| `inbox.ask({…, options, kind?})` | A question, or a blocker |
| `inbox.post({kind: "update", …})` | An outcome report |
| `inbox.status(ids)` / `inbox.wait(ids, mode, timeout)` | Decisions, per-tool grants and the owner's note |
| `inbox.cancel(id)` | Withdraw a request |
| `session.start` / `session.update` | Name the session and report status (feeds the Sessions tab) |

`tools.poll_approval(s)`, `tools.wait_for_approval(s)`,
`tools.list_my_pending_approvals` and `tools.cancel_my_approval` stay as
aliases for one release.

The CLI mirrors them for agents without MCP: `toolyard guide [topic]`,
`toolyard check`, `toolyard request --from request.json [--dry-run]`,
`toolyard wait <id>`.

## Where the code changes

| where | change |
|---|---|
| `internal/gateway/server.go` `buildInstructions` | Prepend the PERMISSIONS paragraph. |
| `internal/gateway/server.go:1237` `deferredResponse` | Replace for restricted tools with the `permission_required` coaching result. Keep the old path behind `approval_mode = execute` for one release. |
| `internal/gateway/schema_wrap.go` | Add the optional `_grant` field. Update `descriptionBanner` to name `inbox.check` / `inbox.request`. |
| `internal/gateway/approval_tools.go` | Keep as aliases. Point descriptions at the `inbox.*` tools. |
| **new** `internal/inbox/` | Requests, validation, the dry-run log, the attachment fetcher and store, rule flags. Judge flags come later, behind an interface. |
| **new** `docs/embed.go`, `internal/inbox/guide.go` | Embed `agent-protocol.md` and the skill, split topics; drift tests in `internal/inbox`. |
| `internal/api/inbox_routes.go` | Owner inbox, sessions, grants, blobs, `/v1/guide`, `/v1/guide/skill`. |
| `cmd/toolyard/` | `guide`, `check`, `request`, `wait`, `skills install`. |
| `web/dashboard/` | The inbox from the prototype. The enrolment modal gets the skill / AGENTS.md step. |
| `scripts/claude-code-hook.sh` | Retire once `approval_mode = execute` goes. |

## Phases

| phase | scope | done when |
|---|---|---|
| 1 | `inbox.guide`, `inbox.check`, the coaching result, `inbox.request` with inline attachments, grants, `inbox.wait` | A fresh Claude Code agent with no skill, told only "deploy this", ends up sending a valid bundled request |
| 2 | Link attachments with snapshots, voice notes, `inbox.ask` / `inbox.post`, sessions | The prototype's billing request can be produced end to end by an agent |
| 3 | Rule flags, dry run with flags, the dry-run log shown on the card | The owner sees dry-run counts |
| 4 | Judge flags, the skill + AGENTS.md at enrolment, the CLI | The same test passes with Codex and hermes |

**The phase 1 test is the real acceptance test.** Run a clean agent against a
toolyard with one restricted tool and no instructions beyond the task, and
check that it gets from "permission_required" to a valid request unaided.
Repeat this with each client we support whenever the protocol changes.

## What shipped in v1

Everything above is implemented, with these specifics and differences:

| area | v1 behaviour |
|---|---|
| Mode switch | `approval_mode` setting. `execute` (default) keeps the queue-and-run flow; `inbox` coaches. The `inbox.*` tools and `_grant` work in both. Anonymous (tokenless) callers always get the legacy flow, since grants are bound to an agent. |
| Grants | One per allowed tool, single use, Ed25519-signed, bound to the agent, expire after the request's `ttl_seconds` (default 30 min). The token is handed to the agent once, by the first `inbox.status`/`inbox.wait` after approval. Explicit deny policies beat grants. |
| Parameters | `eq` (or a bare value), `in`, `prefix`, `gte`/`lte`, `limit` with optional `pattern`, `any`. A call may not pass arguments the request didn't list. `limit` without a pattern is recorded, not checked. |
| Attachments | Inline: markdown, table, chart, diff, code, log, link. Media by link, copied once with an SSRF guard (dial-time IP check, no proxy, 3 redirects max, type and size caps). |
| Flags | Rule flags always; judge flags ("Doesn't match the request") only when the owner enables the Gemini judge. Dry runs return both and are logged; the review page shows the dry-run count and any flags that disappeared between the dry runs and the final request. |
| Toolyard's own reading | "Summarize with toolyard" / "Ask toolyard" use the judge when it's on, otherwise a rule-based reading that says so. |
| Voice notes | Recorded on the server with Gemini text-to-speech when the owner turns it on (`inbox_voice_enabled`, needs `GEMINI_API_KEY`) and stored with the attachments; otherwise, or if recording fails, spoken by the browser (Web Speech API). Agents only ever send the script. |
| Sessions | `session.start` / `session.update`; the Sessions tab derives blocked / waiting / stale (no heartbeat for 15 min) / working, shows live permissions per agent, "told to ask N× today" from coaching events, and a revoke-all kill switch. |
| Scope editor | Before approving, the owner can narrow any tool: fewer `in` values, an exact value for a `limit` or `any`, a longer `prefix`, a tighter range, and a shorter TTL. The server refuses anything wider than the request. The agent sees `"narrowed": true` and the granted `params`. |
| Confirmation | Approving production-flagged or red-flagged tools asks for a second tap. Once the owner registers a passkey (Settings → Inbox & permissions), it needs a passkey assertion (Face ID) whose challenge commits to the exact decision (tools, narrowed parameters, TTL). Removing a passkey needs a passkey; `-inbox-reset-passkeys` is the operator's recovery path. |
| Notifications | inbox-spec §6: `now` pushes at once (3 per agent per hour, then lowered to `soon` with a warning), `soon` is grouped per session after 90 s, `digest` goes into the owner's digest (default 09:30 / 13:30 / 18:30, owner's time zone), `fyi` never pushes. Quiet hours hold everything except `now` requests whose tools are all on the allow list. A request whose session is `blocked_on_owner` (or a blocker) gets one reminder after max(15 min, the owner's median decision time). Snoozed requests push again when the snooze ends. Pushes carry no agent text unless the owner turns on `inbox_push_details`. Notification actions are Deny / Snooze 1 h, and the answer options for a two-option question; **no notification ever approves tools**. |
| Expiry | Pending requests expire after 24 h, updates after 7 days. |
| CLI | `toolyard guide`, `check`, `request --from file.json [--dry-run]`, `wait`, `skills install toolyard-inbox`. |
| Batch | `POST /v1/inbox/batch` reads, denies or snoozes many requests at once ("Mark all read" on Updates). Approving is never batched. |
| Hook | `scripts/claude-code-hook.sh` adds a one-line next step for `permission_required` / `grant_invalid` and never blocks; the old polling path only runs for execute-mode deferred responses. |

Tests: `internal/inbox` (validation, flags, grants incl. concurrent
redemption, narrowing, attention policy incl. quiet hours / grouping /
digest / reminder / snooze, notification taps, voice notes, snapshots,
judge parsing, guide/doc drift), `internal/passkey` (registration and
assertions against a software authenticator, decision binding, replay,
wrong origin, forged signature, expiry), `internal/gateway`
(coaching, grant lifecycle through routing, deny beats grant),
`internal/api` (owner routes, auth, guide), and `scripts/e2e_inbox_test.go`
against a running gateway with a real MCP client.
