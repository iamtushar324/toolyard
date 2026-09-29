# ADR 0005 — approval issues a scoped grant instead of running the call

Status: **accepted, implemented** (`internal/inbox/grants.go`). Grants work
in both approval modes. Details and differences from the original spec:
[agent-onboarding.md](../guidelines/agent-onboarding.md#what-shipped-in-v1).

## Context

Since migration 0010, approving a held call makes the gateway run the
exact call at once and store the result for the agent to collect. This
works for one-off writes, but it:

- ties approval to one exact call, so multi-step jobs (deploy, check
  health, retry once) need several approvals;
- runs the action when the owner taps Approve, not when the agent is
  ready (the branch may have moved, CI may be mid-run);
- leaves no way for the owner to approve something *narrower* than what
  the agent asked for.

## Decision

Approving a request issues a **grant**: an Ed25519-signed token that is
bound to the requesting agent, limited to specific tools and argument
constraints, time-limited, use-limited and revocable. The agent redeems it
by passing `_grant` on a normal tool call. The gateway checks it and
still makes the upstream call itself, so real credentials never leave
toolyard. Handing out short-lived credentials created for a job (broker
mode) is a separate, later, opt-in extension.

Today's behaviour is the special case of a single-use grant pinned to the
exact arguments, which gives a clean migration path behind an
`approval_mode` setting.

## Consequences

- One more round-trip after approval: the agent has to call the tool
  again. That's acceptable, because agents are expected to work async
  anyway.
- Grant redemption becomes the first check in the policy hot path, so the
  use-count increment must be atomic.
- Explicit `deny` policies still override a grant.
- `claude-code-hook.sh` and the meta-tool descriptions have to change.
