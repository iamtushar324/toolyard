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
own memory (`RLIMIT_DATA`, default 256 MiB, four workers at once), marks itself
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

Size `MemoryMax=` for the gateway plus the workers: four at 256 MiB is
1 GiB, and the shipped units use `MemoryMax=1536M` / `MemoryHigh=1280M`.
Decoding a tool result into script values takes many times the JSON's size;
256 MiB is what lets a result at the 6 MiB cap decode, so lowering the cap
means smaller results, not just fewer of them.

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

### Connecting bkt3 (T3 Code)

bkt3 signs people in through the same Clerk instance and organisation, and
can connect each of them to toolyard without a copy-pasted token. Its
server posts a fresh Clerk session token that its browser minted (so the
token's `azp` is the bkt3 origin) and gets back that person's agent token,
which it then sends as `Authorization: Bearer` on `/mcp`:

```http
POST /v1/connect/t3
Content-Type: application/json

{"token": "<Clerk session JWT>"}
```

```json
{"token": "ag_….…", "email": "ada@example.com", "agent_id": "ag_…", "user_id": "u_…"}
```

Turn it on by listing bkt3's origins (Clerk sign-in must be on):

```bash
./toolyard serve -public-url https://toolyard.example.com \
  -connect-azp https://stagebkt3.dev.beknown.live
```

- Comma-separated; compared lowercase without a trailing slash. Empty
  (the default) leaves the endpoint off: `404 {"error":"connect_disabled"}`.
  The dashboard's own origin is refused at startup, and the two flows stay
  apart: a bkt3 token never opens a dashboard session, and a token minted
  for the dashboard never connects.
- It is a server-to-server call: no cookie, Origin or `X-Requested-With`
  is needed (the Clerk token is the credential), but the body must be sent
  as `Content-Type: application/json`.
- Each call checks the token (signature, expiry, issuer, `azp`) and that
  it is a fresh Clerk session token: it carries a `sid`, was issued
  (`iat`) at most 2 minutes ago and not in the future (10 s clock-skew
  leeway either way), and has no `aud` claim (JWT templates do). bkt3
  should mint a token right before each call. Then it checks the
  organisation membership, creates or refreshes the person's toolyard user
  exactly as a Google sign-in does, issues their Beknown identity key if
  they have none (never rotating an existing one), and creates their one
  `T3 Code (bkt3)` agent or rotates its token. The previous token stops
  working on `/mcp` at once, so bkt3 must keep only the latest.
- Errors are `{"error":"<code>"}`: `400 bad_request`, `401 invalid_token`,
  `403 not_org_member`, `403 user_disabled` (blocked in toolyard),
  `403 agent_disabled` (the person disabled their `T3 Code (bkt3)` agent;
  re-enable it on the Agents page), `429 rate_limited` (per client IP and
  per person), `503 clerk_unavailable` (retry later).
- Follow-up, not built: a shared secret between bkt3's server and
  toolyard (for example an `Authorization` header checked alongside the
  Clerk token), so a session token lifted from a bkt3 browser within its
  2-minute window can't be swapped for an agent token by anyone else.
- Every attempt is one `connect.t3` audit row (outcome `created` or
  `rotated`, or `denied` with the code) naming the person, the `azp`
  origin and the agent. No token is ever logged or audited.

The identity key it issues is registered in each registry upstream as
`connect:t3`, not as an admin, so in production prime-service's actor
registry will usually refuse it: the key shows a registry error on the
**Users** page. Registering a new person's key there stays a human step,
as it is for keys an admin issues: an admin opens the person on the
**Users** page and presses **Retry registration**
(`POST /v1/users/{id}/identity-key/register`, which runs as that admin),
or registers the fingerprint by hand.
