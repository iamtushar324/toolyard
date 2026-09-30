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

## Code mode (Bifrost-compatible)

Agents and skills written for the Bifrost MCP gateway's code mode work
unchanged against toolyard: the four tools `listToolFiles`, `readToolFile`,
`getToolDocs` and `executeToolCode` are always on, need no `_reason`, and
use the same stub format and output shapes. In code a server is its upstream
name (`BkCoreServices.get_client(clientId="vai-us")`); every call a script
makes goes through the normal pipeline (access, policy, approvals, identity
forwarding, audit) under via `code_mode`, so a held or denied call aborts the
script with the gateway's answer. Names that are not Starlark identifiers are
bound under a sanitised one (`bk-core` becomes `bk_core`; the stubs say so).

### The script worker

Each `executeToolCode` run happens in a child process: the gateway starts its
own binary again as `toolyard codemode-worker`, with an empty environment
(no `.env`, no tokens), and talks to it over stdin and stdout. The child only
asks the parent to make tool calls; the parent makes them. The child caps its
own memory (`RLIMIT_DATA`, default 160 MiB, four workers at once), marks itself
first in line for the kernel's OOM killer, and is killed at the script's wall
clock (5 minutes). A script that allocates past the cap, spins, or crashes the
interpreter costs itself the run and nothing else. Defaults are in
`internal/codemode` (`DefaultLimits`, `DefaultWorker`); the executeToolCode
tool description states the limits that are enforced.

Two things to know when hardening the unit:

- **It is a resource boundary, not a privilege boundary.** The worker runs
  as the same user, in the same namespaces, with the same filesystem view
  as the gateway. Starlark has no filesystem or network access, but a flaw in
  the interpreter itself would let a script read what the gateway can read,
  including the data directory. If that matters, isolate the whole service
  (a container, or `DynamicUser=` and `ProtectSystem=strict`).
- **`SystemCallFilter` must allow `setrlimit` and `prlimit64`.** The shipped
  units (`deploy/toolyard.service`, the strict tier in `deploy/install.sh`)
  deny `@resources` and then allow these two back. Without them the worker
  dies with `SIGSYS` and every script fails with "code mode worker could not
  apply its memory cap".

Size `MemoryMax=` for the gateway plus the workers: four at 160 MiB is
640 MiB, and the shipped units use 1 GiB.

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

## Inbox and permissions

Settings → **Inbox & permissions**:

- **approval_mode**: `execute` (default) queues restricted calls in
  Approvals as before; `inbox` coaches agents to send an `inbox.request`
  instead. Switch when your agents have the `toolyard-inbox` skill or the
  AGENTS.md block from the enrolment modal's "Teach it the rules" tab.
- **Copy linked media** (default on): images, videos and files an agent
  links to are downloaded once when the request arrives and served from
  toolyard, under `<data-dir>/inbox/blobs/`. Limits: image 10 MB, video
  50 MB, file 25 MB. Links to loopback or private-network addresses are
  refused unless you start the gateway with
  `-inbox-fetch-private-networks` (for evidence hosted on your own LAN).
  The fetcher connects directly, never through `HTTP(S)_PROXY`.
- **Judge model** (default off): needs `GEMINI_API_KEY`. Request text is
  sent to Gemini to check each tool call against the agent's own words.
  `inbox_judge_model` overrides the model (default `gemini-2.5-flash`;
  read at startup).
- **Where agents should host files**: shown to agents by
  `inbox.guide({"topic": "hosting"})`.

- **Voice notes** (default off): with `GEMINI_API_KEY` set, toolyard records
  each request's voice-note script once (Gemini text-to-speech,
  `inbox_voice_name`, default Kore; `TOOLYARD_INBOX_VOICE_MODEL` overrides the
  model) and stores it with the attachments. Off, or if recording fails,
  the browser reads the script.
- **When your phone buzzes**: `now` requests push at once (each agent gets
  `inbox_now_per_hour`, default 3; more are lowered to `soon`), `soon` ones
  are grouped per session after 90 seconds, `digest` ones wait for the
  digest (`inbox_digest_times`, default `09:30,13:30,18:30`; empty for none),
  and updates never push. **Quiet hours** (`inbox_quiet_hours`, e.g.
  `22:00-07:00`) hold everything except `now` requests whose tools are all on
  `inbox_quiet_allow` (comma-separated, `deploy.*` matches a prefix). Times
  use `inbox_timezone` (the dashboard offers your browser's). A request whose
  agent is blocked on you gets one reminder; a snoozed one comes back when
  the snooze ends.
- **Push details** (default off): pushes carry only the agent's name and the
  kind of request. On, they include the title, summary and a one-line scope,
  which is what an Apple Watch shows. Notification buttons are Deny, Snooze
  1 h, and the two answers of a two-option question. A notification never
  approves tools.
- **Passkeys**: add one per device. With a passkey registered, allowing
  production or red-flagged tools needs it (Face ID on an iPhone), and the
  confirmation is bound to exactly what you approve. Passkeys need the
  dashboard on `https` (or `localhost`). Removing one needs a passkey; if
  you lose every device, start the gateway once with
  `-inbox-reset-passkeys`.

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

## Sign in with Google (Clerk)

Staff can sign in with their Google account through [Clerk](https://clerk.com).
"Internal user" means a member of one Clerk organisation; nobody else gets
past the login page, whatever their email domain. The local password account
stays as the break-glass login.

1. In the Clerk dashboard, enable Google as a social connection and note
   the instance's **secret key**, **publishable key** and the
   **organisation ID** (`org_…`) whose members may sign in.
2. Put the three values in the systemd `EnvironmentFile` (or the `.env`
   next to the binary). They must never be passed on the command line:

   ```bash
   TOOLYARD_CLERK_SECRET_KEY=sk_live_…
   TOOLYARD_CLERK_PUBLISHABLE_KEY=pk_live_…
   TOOLYARD_CLERK_ORGANIZATION_ID=org_…
   ```

   Clerk sign-in is on only when all three are set; setting one or two is
   a startup error. It also requires `-public-url`, because a Clerk session
   token is only accepted when it was minted for that exact origin.
3. Start with the owner's email so the first Google sign-in with that
   address attaches to the existing password account (keeping its admin
   role, agents, passkeys and push subscriptions):

   ```bash
   ./toolyard serve -public-url https://toolyard.example.com \
     -owner-email you@example.com
   ```

Everyone else who signs in becomes a **member**: they see their own profile,
manage their own agents, and their agents reach only the servers and data
groups (memory, lake, events, notes, skills) an admin grants them on the
**Users** page. Every approval and inbox decision stays with admins.

Once an hour (`-clerk-sync-interval`) toolyard lists the organisation's
members; anyone who left is blocked (`left_org`), their dashboard sessions
are revoked and their agents stop authenticating. A Clerk outage never
blocks anyone: on any error the sync logs and changes nothing.
