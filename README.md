# toolyard

**An approval gateway for AI agents.** toolyard sits between your coding
agents and the MCP servers they use. Reads pass straight through; writes
stop and wait for you, with an approval card on your phone that shows what
the agent wants to do and why.

One Go binary, SQLite, and a PWA dashboard. Self-hosted.

```
 Claude Code ─┐                                  ┌─► GitHub MCP
 Cursor      ─┤   ┌──────────┐    ┌──────────┐   ├─► Filesystem MCP
 Codex CLI   ─┼──►│ toolyard │───►│  policy  │───┼─► your own MCPs
 Web Claude  ─┘   └────┬─────┘    └────┬─────┘   │
                       │               ▼
                  audit log     approval ──► phone push (allow / deny)
```

## Why

Every agent (Claude Code, Cursor, Codex CLI, web Claude) ships its own MCP
config, its own approval prompts, and its own audit trail, or none at all.
toolyard gives you one place to:

- **Enroll MCP servers once** and share them with every agent.
- **Approve writes from your phone.** Each call carries the model's
  `_reason`, so you see intent, not just arguments.
- **Keep one audit log** of every tool call from every agent.

## How approval works

1. The agent calls a tool through toolyard over stdio or streamable HTTP
   (`/mcp`).
2. toolyard adds a required `_reason` field (and an optional
   `_intent_category`) to every upstream tool's schema, so the model has to
   explain itself.
3. The policy engine decides: **allow**, **ask**, or **deny**. Explicit
   per-tool and per-server rules set in the dashboard win; otherwise
   read-shaped tools (`get`, `list`, `search`, ...) pass and everything else
   asks.
4. An "ask" is held on the approval bus, pushed to your phone via Web Push,
   and shown live in the dashboard. Decisions are Ed25519-signed and durable
   in SQLite.
5. Long approvals degrade gracefully: the call returns a deferred response
   and the Claude Code hook (`scripts/claude-code-hook.sh`) resumes it once
   you decide.

Optional **auto-approval** learns from your history: calls you have approved
repeatedly with no denials can be auto-allowed per agent. Destructive tools
never auto-approve, a single denial puts a rule on cool-off, and there is a
per-agent hourly cap.

## Quickstart

```bash
go build -o toolyard ./cmd/gateway
./toolyard serve -addr :8787 -data ~/.toolyard
```

Open <http://localhost:8787>, create the local admin account, then:

1. **Agents → Generate code** to get a one-time enrollment code.
2. Exchange it for a token:
   ```bash
   ./toolyard exchange -url http://localhost:8787 -code <code>
   ```
3. Point your MCP-aware agent at `http://localhost:8787/mcp` with
   `Authorization: Bearer <token>`, or run the gateway with `-stdio` for
   stdio-only hosts.
4. **Settings → Enable push** on your phone to get approval cards.
5. Add upstream MCP servers from the dashboard (you can paste an existing
   MCP JSON config).

The full guide, including Docker and wiring each agent, is in
[`docs/self-hosting.md`](docs/self-hosting.md).

## What's included

- Stdio and streamable HTTP MCP server; agent enrollment via short-lived
  codes and sha256-hashed long-lived tokens.
- Policy engine with dashboard-managed allow / ask / deny rules per tool and
  per upstream server.
- Approval bus with signed decision tokens, in-line wait, and deferred
  resume.
- Web Push (VAPID) with one-tap allow / deny in the notification.
- Live SSE dashboard: pending approvals, audit feed, agents, upstream
  servers, push setup.
- Built-in shared memory tools (`memory.set`, `memory.get`, `memory.list`,
  `memory.delete`) so agents can hand context to each other.
- Multi-stage `deploy/Dockerfile` and `deploy/docker-compose.yml`.

Architecture details are in [`docs/architecture.md`](docs/architecture.md).

## Verification

End-to-end smoke test, run while the gateway is up:

```bash
go test -tags=e2e ./scripts -run TestEndToEnd -toolyard=http://localhost:8787 -v
```

It covers the full happy path: enroll, list tools, a read passes, a write
holds, the dashboard approves, the write completes, and the audit log fills.

## License

Apache 2.0. See [`LICENSE`](LICENSE).
