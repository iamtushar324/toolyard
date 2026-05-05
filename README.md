# toolyard

Self-hosted MCP gateway with **per-tool-call human approval pushed to your phone** and built-in shared memory. One Go binary + SQLite + a PWA dashboard.

Every coding agent today (Claude Code, Cursor, Codex CLI, web Claude) reimplements its own MCP config, approval UX, and audit log. `toolyard` sits between any number of agents and any number of upstream MCP servers, gives you a single place to enroll servers and approve writes, and pushes approval cards to your phone with the model's reasoning visible.

## Status

Pre-v0.1, under construction. See `docs/architecture.md`.

## Quickstart (after v0.1)

```bash
# Build and run
go build -o toolyard ./cmd/gateway
./toolyard serve

# Open dashboard
open http://localhost:8787
```

## License

Apache 2.0 — see `LICENSE`.
