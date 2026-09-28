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
   `inbox.guide` for detail. It's published into the skills store at startup
   through the existing `skills.publish` path.

A test checks that the skill and the protocol agree on field names and
limits, so the two can't drift apart.

## Layer 0: enrolment

The dashboard's agent-enrolment modal already shows setup snippets per client
(CLI, hermes, Claude Code). Each one gets a second step:

- **Claude Code:** `toolyard skills install toolyard-inbox` (new CLI command,
  wrapping the existing install logic). It writes
  `~/.claude/skills/toolyard-inbox`.
- **Codex, Cursor, others:** a 10-line AGENTS.md / rules block to paste. It
  says restricted tools exist, names `inbox.check` and `inbox.request`, and
  points to `inbox.guide`.
- **hermes:** the same block, added to the system prompt in
  `~/.hermes/config.yaml`.

This layer is a head start, not a requirement. Everything still works through
layers 1–5 if the agent skips it.

## Layer 1: the connection instructions

This replaces the long paragraph `buildInstructions` returns today
(`internal/gateway/server.go:1431`):

> toolyard gateway. Every call needs `_reason`. Some tools are restricted and
> need your owner's approval: check with `inbox.check`, then ask for all of
> them in one `inbox.request`. Call `inbox.guide` before your first request.

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
| `internal/gateway/server.go:1431` `buildInstructions` | Replace with the three-line version. |
| `internal/gateway/server.go:1237` `deferredResponse` | Replace for restricted tools with the `permission_required` coaching result. Keep the old path behind `approval_mode = execute` for one release. |
| `internal/gateway/schema_wrap.go` | Add the optional `_grant` field. Update `descriptionBanner` to name `inbox.check` / `inbox.request`. |
| `internal/gateway/approval_tools.go` | Keep as aliases. Point descriptions at the `inbox.*` tools. |
| **new** `internal/inbox/` | Requests, validation, the dry-run log, the attachment fetcher and store, rule flags. Judge flags come later, behind an interface. |
| **new** `internal/inbox/guide/` | Embeds `agent-protocol.md` and the skill, splits topics, and has a drift test. |
| `internal/skills/` | Publish `toolyard-inbox` at startup. |
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
