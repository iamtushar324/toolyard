# ADR 0006 — restricted calls coach the agent instead of queuing an approval

Status: **accepted, implemented** behind `approval_mode = inbox` (the
default stays `execute` so existing agents keep working). Details:
[agent-onboarding.md](../guidelines/agent-onboarding.md).

## Context

Today a restricted call is queued as an approval right away, built from the
call's arguments and its `_reason`. With the new inbox, a request is a
first-person message, a voice note, evidence, and every tool the task needs.
A queued call has none of that, so it would render as a thin card that
interrupts the owner without giving them what they need to decide.

## Decision

1. A restricted call without a valid grant **runs nothing and creates
   nothing**. It returns `permission_required` (`isError: true`) with a draft
   request that has the tool and its exact arguments already filled in, plus
   a pointer to `inbox.guide`.
2. Only `inbox.request` / `inbox.ask` put anything in the owner's inbox.
3. Media attachments arrive as links. Toolyard fetches and keeps a copy when
   the request is sent (switchable).
4. Dry runs show the agent every flag, including the judge's. Dry runs are
   logged, and their count is shown on the request.
5. The `toolyard-inbox` skill (or an AGENTS.md block) is offered when an
   agent is enrolled. The protocol file is the single source for the skill,
   `inbox.guide`, `/v1/guide` and the MCP resource.

## Consequences

- The owner's inbox only ever holds requests an agent chose to write up.
- An agent's first restricted call costs one extra round-trip. The draft keeps
  that cheap.
- Agents that ignore coaching show up as a "coached" count in the Sessions
  view, not as noise in the inbox.
- Showing judge flags on dry run helps honest agents but lets a bad one probe.
  The dry-run log makes that visible instead of preventing it.
- `approval_mode = execute` and `scripts/claude-code-hook.sh` are kept for one
  release, then removed.
