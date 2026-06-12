# Self-hosting toolyard

## Quickstart (binary)

```bash
git clone https://github.com/tusharbhardwaj/toolyard.git
cd toolyard
go build -o toolyard ./cmd/gateway
./toolyard serve -addr :8787 -data ~/.toolyard
```

Open <http://localhost:8787>. The first visit asks you to create the local
admin account. Subsequent visits land on `/Approvals`.

## Quickstart (Docker)

```bash
docker compose -f deploy/docker-compose.yml up --build
```

Persistent data lives in the named volume `toolyard-data`. Pass
`-e TOOLYARD_DATA_DIR=/var/lib/toolyard/data` to use a custom path.

## Wiring an agent

1. In the dashboard, **Agents → Generate code**. You get a one-time code
   like `Fipvh28M-lQ`, valid for 15 minutes.
2. From your machine:

   ```bash
   ./toolyard exchange -url http://localhost:8787 -code Fipvh28M-lQ
   ```

   You get back an `agent_id` and a long-lived `token`.

3. Configure your MCP client to talk to toolyard. Examples:

   - **Streamable HTTP** (Claude Code, Cursor, etc.):

     ```json
     {
       "mcpServers": {
         "toolyard": {
           "url": "http://localhost:8787/mcp",
           "headers": { "Authorization": "Bearer <token>" }
         }
       }
     }
     ```

   - **stdio**: run `./toolyard serve -stdio` from inside the agent's
     stdio config so the gateway speaks MCP on stdin/stdout in addition
     to listening on HTTP.

4. (Optional) install the deferred-approval hook:

   ```bash
   cp scripts/claude-code-hook.sh ~/.claude/hooks/
   ```

   Add to `~/.claude/settings.json`:

   ```json
   {
     "hooks": {
       "PostToolUse": [
         { "type": "command", "command": "~/.claude/hooks/claude-code-hook.sh" }
       ]
     }
   }
   ```

5. (Optional) install interaction logging hooks. These send agent lifecycle
   events to `POST /v1/hooks/ingest`, where toolyard stores redacted payloads
   and forwards useful prompt/stop/tool summaries into MemPalace when it is
   enabled.

   - Claude Code can use direct HTTP hooks in `~/.claude/settings.json`:

     ```json
     {
       "hooks": {
         "UserPromptSubmit": [
           {
             "hooks": [
               {
                 "type": "http",
                 "url": "http://localhost:8787/v1/hooks/ingest?source=claude_code",
                 "headers": { "Authorization": "Bearer <token>" }
               }
             ]
           }
         ]
       }
     }
     ```

   - Codex and Cursor can call the generic command forwarder:

     ```bash
     mkdir -p ~/.toolyard/hooks
     cp scripts/toolyard-hook-forwarder.sh ~/.toolyard/hooks/
     chmod +x ~/.toolyard/hooks/toolyard-hook-forwarder.sh
     ```

     Then point their hook config at:

     ```bash
     ~/.toolyard/hooks/toolyard-hook-forwarder.sh codex http://localhost:8787 <token>
     ~/.toolyard/hooks/toolyard-hook-forwarder.sh cursor http://localhost:8787 <token>
     ```

   Conductor workspaces use the same hook config as the agent type they run
   (Claude Code or Codex); there is no separate Conductor hook endpoint.

## Adding upstream MCP servers

Create `upstreams.json`:

```json
[
  {
    "name": "github",
    "transport": "stdio",
    "command": "uvx",
    "args": ["mcp-server-github"],
    "env": { "GITHUB_PERSONAL_ACCESS_TOKEN": "ghp_…" }
  },
  {
    "name": "linear",
    "transport": "http",
    "url": "https://mcp.linear.app/mcp"
  }
]
```

Then:

```bash
./toolyard serve -upstreams upstreams.json
```

Upstreams' tool catalogs are fetched at startup, schema-wrapped, and
registered under `<upstream-name>.<original-tool-name>`.

## Web Push (PWA notifications)

1. Open the dashboard and visit **Settings → Enable push**.
2. Grant permission. The browser registers a Web Push subscription with
   your toolyard's VAPID public key.
3. iOS (16.4+): "Add to Home Screen" first, then enable push from the
   installed app.

A new pending approval pushes a notification with **Allow** / **Deny**
buttons. Tapping one POSTs to `/v1/approvals/decide-by-token` with the
Ed25519-signed token from the payload, so a deciding tap doesn't even need
your dashboard session.
