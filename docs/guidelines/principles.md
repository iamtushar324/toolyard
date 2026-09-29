# toolyard — product principles

toolyard is the one place every agent goes to do things that matter,
wherever it runs: a laptop, a cloud sandbox, a remote runner. Agents get
tools. The owner keeps authority, and gives it out in small, short-lived
pieces from a phone or a watch, when it suits them.

These principles settle design arguments. If a feature breaks one, the
feature changes, not the principle.

## 1. Human attention is the scarcest resource

Ten or more agents running in parallel can easily ask for more attention
than one person has. Every notification must earn its interruption.

- Only the agent that asked, the right urgency and the right moment get
  pushed. Everything else waits in the inbox.
- A request that can't be understood in ten seconds on a watch isn't ready
  to send. The agent must summarise it better (see
  [agent-protocol.md](agent-protocol.md)).
- toolyard can **lower** an agent's requested urgency. It never raises it.
- Repeated, identical, low-risk approvals are a system failure. Turn them
  into an auto-approval rule instead of asking again.

## 2. Authority is granted, never held

No agent has standing access to anything restricted. Access is a **grant**:

- **Scoped**: one tool, or a small set of tools, with constraints on the
  arguments (`env=prod`, `ref=abc123`).
- **Bounded**: it expires and has a fixed number of uses.
- **Bound**: it only works for the agent identity it was issued to. A
  leaked grant is useless to anyone else.
- **Revocable**: the owner can revoke one grant, all of one agent's grants,
  or every grant (kill switch) at any time.

Approving a request means issuing a grant. The agent then uses the grant
when it's ready. The owner approves an *intent with a scope*, not a
function call to run on the spot.

## 3. Credentials never leave the yard

A grant is permission to act *through* toolyard, not a copy of the real
credential. Agents send the grant with their call. toolyard checks it,
fills in the real secret on its side, and makes the upstream call.

If an agent ever truly needs a raw credential (for example, a CLI that
must run inside its sandbox), toolyard issues a **short-lived credential
created for that job**, such as a GitHub installation token or a cloud
STS session, limited to the grant's scope. It never hands out the
long-lived secret. This mode is opt-in per tool and always needs human
approval.

## 4. Async by default, blocking by exception

Agents ask for permission as soon as they know they'll need it, then keep
working. The owner decides when it suits them. Blocking on a person is
the last resort, and when an agent does block, it says so, so the inbox
can show "this session is stuck on you."

## 5. One inbox, every agent, any host

Where an agent runs doesn't matter; its identity does. Every agent, on any
host, reports to the same inbox and the same session list. The owner never
has to remember which terminal, tab or cloud console a question came from.

## 6. Everything is explained up front and auditable afterwards

- **Up front**: every request carries a title, a one-line summary, why it's
  needed, the exact scope, the blast radius, how to roll back, and links to
  the evidence (PR, diff, CI run).
- **Afterwards**: every request, decision, grant use and outcome is in the
  audit log, with the agent, session and host attached.

## 7. Close the loop

A grant isn't finished when it's used. It's finished when the agent has
reported what happened ("deployed abc123 to prod, health checks green").
Grants that are issued but never reported on are flagged in the inbox.

## 8. Deny by default, fail closed

- Unknown tools and anything that isn't a read need approval.
- Destructive tools are never auto-approved, whatever the learned history
  (already enforced by `internal/autoapproval`).
- If toolyard is down, restricted actions stop. Agents must never look for
  a way around the gateway (another credential, a direct API call, asking
  a different agent to do it). A workaround is treated as a security
  incident, not as initiative.

## 9. Trust is earned and can be taken back

Auto-approval rules come from a record of approvals with no denials. A
single denial pauses them. Trust applies per agent and per kind of action,
never across the board. Higher-risk grants need a stronger confirmation
step (a passkey on the phone, not a tap on the watch).

## 10. Boring and self-hosted

One Go binary, SQLite, a PWA. The owner can read all of it, run it on a
home server, and back it up with `cp`. Every new dependency has to justify
itself against that.
