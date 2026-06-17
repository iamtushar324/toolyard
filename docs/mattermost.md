# Mattermost messaging via Toolyard (`mattermost.*`)

Toolyard exposes internal Mattermost messaging to agents (Hermes and any other
enrolled agent) by registering the maintained open-source MCP server
[`cloud-ru-tech/mcp-server-mattermost`](https://github.com/cloud-ru-tech/mcp-server-mattermost)
as a Toolyard **upstream**. Toolyard fetches its tool catalog, wraps every tool
with the required `_reason` field, and routes calls through the same
policy/approval/audit engine as every other upstream — so agents call
`mattermost.*` tools with no source changes.

This is deliberately **not** a Hermes Agent source change and **not** a new
Go-native Mattermost client in Toolyard. It is a standard Toolyard-backed
capability, just like the `github`/`linear` upstreams.

## Why this (the bug it fixes)

A test send to `hermes-home` through Hermes's own messaging surface failed with:

```text
Plugin platform send failed: Timeout context manager should be used inside a task
```

That is an `aiohttp` event-loop defect inside Hermes's plugin platform (an
`async with timeout()` entered outside a running asyncio task) — see
`hermes-agent/plugins/platforms/mattermost/adapter.py`. Routing the send
through the standalone `mcp-server-mattermost` process sidesteps it: that
server runs its own correctly-structured event loop, and Toolyard talks to it
over MCP (then forwards a plain JSON tool call). The broken Hermes code path is
never entered.

> If, in your environment, the upstream server ever reproduces a comparable
> async failure, the documented fallback is a Go-native `mattermost.*` built-in
> in Toolyard (a thin REST client modeled on
> `internal/chatnotify/telegram/client.go`). It is not needed today.

## The server

- Package `mcp-server-mattermost`, pinned to **v0.5.0** (MIT, FastMCP 3.x), 38 tools.
- Runs via `uv`/`pip`/Docker; supports **stdio** (default) and **HTTP** transport
  (`--http --host --port`, default `127.0.0.1:8000`, health at `/health`).
- Auth via `MATTERMOST_URL` + `MATTERMOST_TOKEN` (`MATTERMOST_AUTH_MODE=static_token`).
  It reuses the **same bot token Hermes uses** (`~/.hermes/.env`), so messages are
  attributed to the existing `hermes` bot identity.

On this dev host the checkout lives at `/home/ubuntu/repos/mcp-server-mattermost`
(pinned to the `v0.5.0` tag, deps installed with `uv sync --frozen`).

## Transport choice

| Deployment | Transport | Why |
|---|---|---|
| **Live/systemd gateway (recommended here)** | **HTTP** | The `toolyard` service runs as a dedicated user with `ProtectHome=true` (see `deploy/toolyard.service`) and **cannot read `/home/ubuntu/repos`**. Run the MCP server as a localhost HTTP service (see `deploy/mcp-server-mattermost.service`) and point the gateway at `http://127.0.0.1:8000/mcp`. |
| **Dev / single-user gateway** | stdio | If the gateway runs as a user that can read the checkout, Toolyard can spawn the server directly via `uv run`. |

Example upstream configs: `deploy/upstreams.mattermost.http.example.json`
(recommended) and `deploy/upstreams.mattermost.stdio.example.json`.

## Configuration

Set on the **MCP server** process (the systemd unit's `EnvironmentFile`, or the
stdio upstream's `env`):

| Var | Required | Notes |
|---|---|---|
| `MATTERMOST_URL` | yes | e.g. `https://chat.beknown.live` |
| `MATTERMOST_TOKEN` | yes | bot/personal access token. Store in the Toolyard **secrets broker** (`secret://MATTERMOST_TOKEN`) — never in plaintext config. |
| `MATTERMOST_AUTH_MODE` | no | defaults to `static_token` |
| `MATTERMOST_LOG_LEVEL` | no | `WARNING`/`ERROR` to quiet stdout |
| `MATTERMOST_TIMEOUT`, `MATTERMOST_MAX_RETRIES`, `MATTERMOST_VERIFY_SSL` | no | request tuning |

`secret://` refs in a stdio upstream's `env` are resolved by the gateway at dial
time. For an HTTP upstream the secret is consumed by the server process directly,
so inject it via the systemd `EnvironmentFile` (root-readable, mode `600`).

Use a **dedicated, least-privilege bot account** scoped to the teams/channels
agents need. Mattermost enforces that scope server-side; anything out of scope
returns `403`, surfaced to the agent as a clear error.

## Tool surface

Tools surface as `mattermost.<tool>`. Toolyard's policy engine classifies them
by leading verb (`internal/policy/policy.go`): reads auto-allow; everything else
holds for human approval.

- **Send (approval-gated):** `post_message` (channel; thread reply via `root_id`),
  `create_direct_channel` (+ `post_message`) for DMs.
- **Discover (auto-allow):** `search_users`, `get_user_by_username`, `get_user`,
  `list_public_channels`, `list_my_channels`, `get_channel_by_name`,
  `get_channel`, `get_channel_members`, `list_teams`, `get_team`, `get_me`.
- **Context (auto-allow):** `get_channel_messages` (recent window; `since`/unread),
  `get_thread`.
- **Search / index (auto-allow):** `search_messages` (keyword across a team;
  Mattermost modifiers `from:`, `in:`, `on:`, `before:`, `after:`). This covers
  capability 5 for the first pass; a deeper events-lake index is future work.

### Permission boundaries (deny the rest)

The server also exposes mutating tools outside this ticket's scope. They are
already approval-gated by default, but register explicit **`deny`** policies so
agents can't invoke them at all. `scripts/setup-mattermost.sh` applies these:

`mattermost.delete_message`, `mattermost.update_message`,
`mattermost.create_channel`, `mattermost.join_channel`,
`mattermost.leave_channel`, `mattermost.add_user_to_channel`,
`mattermost.upload_file`, `mattermost.create_bookmark`,
`mattermost.update_bookmark`, `mattermost.delete_bookmark`,
`mattermost.update_bookmark_sort_order`, `mattermost.add_reaction`,
`mattermost.remove_reaction`, `mattermost.pin_message`,
`mattermost.unpin_message`, `mattermost.mark_channel_viewed`.

The effective surface is then **send / discover / resolve / read**.

## Link & reference resolution

The server has no dedicated link resolver, but every Mattermost link embeds the
ID needed. Map references to the existing tools:

| Input | Canonical target |
|---|---|
| Post/thread permalink `…/<team>/pl/<post_id>` | extract `post_id` → `mattermost.get_thread {post_id}`; reply with `mattermost.post_message {root_id: post_id}` |
| Channel link `…/<team>/channels/<name>` | `mattermost.get_channel_by_name {team, name}` → `channel_id` |
| `#channel` | `mattermost.get_channel_by_name` |
| `@member` | `mattermost.get_user_by_username` |
| raw 26-char channel/post/user ID | used directly |

Guidance for agents: validate that a pasted permalink's host equals the
configured `MATTERMOST_URL` (anti-spoofing), and when a name search returns more
than one match, **ask the user to disambiguate — do not guess**.

## Recommended / memo channels

`hermes-home` is the standard memo/home channel (private channel `P`, id
`g3k118btkbr3pq98z76gjkc83c`, team `su7i8nc4mf8a7fsewjn6djm3mo`). Discover it with
`mattermost.get_channel_by_name {team, name:"hermes-home"}`. The curated list is
seeded into Toolyard shared memory under key `mattermost/memo-channels`
(via `scripts/setup-mattermost.sh`), readable by agents through `memory.get`.

## Setup

1. **Clone + install the server** (one time, as the user that will run it):
   ```bash
   cd ~/repos
   git clone https://github.com/cloud-ru-tech/mcp-server-mattermost.git
   cd mcp-server-mattermost && git checkout v0.5.0 && uv sync --frozen
   ```
2. **Run it as a localhost HTTP service** — install `deploy/mcp-server-mattermost.service`
   (adjust `User=`, `WorkingDirectory=`, and `EnvironmentFile=`), then
   `sudo systemctl enable --now mcp-server-mattermost` and check
   `curl http://127.0.0.1:8000/health`.
3. **Register the upstream + policies + secret** in the gateway:
   ```bash
   TOOLYARD_URL=http://127.0.0.1:18787 \
   MATTERMOST_TOKEN=… \
   scripts/setup-mattermost.sh
   ```
   (The script logs in, creates the `MATTERMOST_TOKEN` secret, POSTs the
   `mattermost` upstream to `/v1/servers`, and applies the deny policies.)

## Verification

Verified on this host (2026-06-17):

- `toolyard probe -config deploy/upstreams.mattermost.stdio.example.json` →
  `upstream mattermost: connected ok`.
- Direct MCP handshake against the checkout → **38 tools** discovered;
  `get_me` → `username=hermes`; `get_channel(hermes-home)` → `name=hermes-home`,
  type `P`. **All reads succeeded with no `Timeout context manager` error.**

Acceptance checklist (run after the upstream is registered):

1. **Send to `hermes-home`** — `mattermost.post_message {channel_id:"g3k118btkbr3pq98z76gjkc83c", message:"…"}` → approval card → approve → message lands. (This is the bug-fix proof; it replaces the failing Hermes path.)
2. **DM a member** — `mattermost.search_users {term}` → `create_direct_channel {user_ids}` → `post_message`.
3. **Channel link** — `get_channel_by_name` from a `…/channels/<name>` link, then read/send.
4. **Thread/post permalink** — extract `post_id`, `get_thread`, reply with `root_id`.
5. **Bounded context** — `get_channel_messages` (recent window), `get_thread`.
6. **Discovery** — `search_users`, `search_messages`, `get_channel_by_name hermes-home`.
7. **Permissions** — a send to a channel the bot isn't in → clear `403`; a deny-listed tool → rejected.
8. All of the above performed by Hermes over its existing enrollment, **no Hermes source changes**.
