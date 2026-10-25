# Connect an MCP client to toolyard by URL

You can add toolyard to ChatGPT, Claude.ai, Claude Code, Codex, Cursor and
other MCP clients with one URL. The client signs you in through your
browser with OAuth. You do not copy an agent token.

The URL is always your toolyard's public address followed by `/mcp`:

```text
https://toolyard.example.com/mcp
```

The **Connected apps** page in the dashboard shows the exact URL for your
toolyard, with a Copy button.

## Before you start

- Your toolyard must run with `-public-url` (see
  [self-hosting](../self-hosting.md#oauth-for-mcp-clients)). OAuth is on by
  default when it is set. An admin can turn it off on the Connected apps
  page.
- ChatGPT and Claude.ai connect from their own servers on the internet.
  For these two clients, the URL must be public HTTPS with a valid
  certificate. Claude Code, Codex and Cursor connect from your computer, so
  an address that only your computer can reach is enough.
- You need a toolyard account (password or Google sign-in).

## ChatGPT (web)

Custom MCP servers need developer mode, which needs a paid ChatGPT plan.

1. Open **Settings → Apps & Connectors → Advanced settings**.
2. Turn on **Developer mode**.
3. Go back to **Apps & Connectors** and select **Create**.
4. Type a name (for example `toolyard`), and paste `https://toolyard.example.com/mcp`
   in **MCP server URL**.
5. Set **Authentication** to **OAuth**, then select **Create**.
6. Your browser opens toolyard. Sign in, read the consent screen, and select
   **Allow**.
7. In a chat, open the **+** menu, select **Developer mode**, and turn on
   the toolyard connector.

## Claude.ai (web and desktop)

1. Open **Settings → Connectors**.
2. Select **Add custom connector**.
3. Type a name (for example `toolyard`) and paste `https://toolyard.example.com/mcp`
   as the URL. Select **Add**.
4. Select **Connect**. Sign in to toolyard and select **Allow**.
5. In a chat, open the tools menu and turn on toolyard.

On Team and Enterprise plans, an owner adds the connector first in the
organization's connector settings. Then each person connects it.

## Claude Code

1. Add the server:

   ```bash
   claude mcp add --transport http toolyard https://toolyard.example.com/mcp
   ```

2. Start Claude Code and type `/mcp`.
3. Select **toolyard**, then **Authenticate**. Your browser opens toolyard.
   Sign in and select **Allow**.

Claude Code keeps the tokens and refreshes them. Use `/mcp` again to sign
out or to sign in again.

## Codex

1. Add the server to `~/.codex/config.toml`:

   ```toml
   [mcp_servers.toolyard]
   url = "https://toolyard.example.com/mcp"
   ```

   Recent versions can also do this with
   `codex mcp add toolyard --url https://toolyard.example.com/mcp`.
2. Sign in:

   ```bash
   codex mcp login toolyard
   ```

3. Your browser opens toolyard. Sign in and select **Allow**.

## Cursor

1. Open **Cursor Settings → Tools & Integrations** (or **MCP**) and select
   **Add Custom MCP**. Cursor opens `~/.cursor/mcp.json`.
2. Add the server:

   ```json
   {
     "mcpServers": {
       "toolyard": { "url": "https://toolyard.example.com/mcp" }
     }
   }
   ```

3. Back in the settings, toolyard shows **Needs login**. Select it. Your
   browser opens toolyard. Sign in and select **Allow**.

Cursor returns to a `cursor://` address after sign-in. If your admin set
the allow list of client redirect hosts, it must include `cursor:`.

## Other clients

Any client that implements the MCP authorization specification works. It
finds everything from the URL:

1. The first request to `/mcp` without a token gets `401` with
   `WWW-Authenticate: Bearer resource_metadata="https://toolyard.example.com/.well-known/oauth-protected-resource"`.
2. That document names toolyard as the authorization server.
   `/.well-known/oauth-authorization-server` lists the endpoints.
3. The client registers itself (`POST /oauth/register`, RFC 7591), or uses a
   client ID metadata document (an `https` `client_id` that serves its own
   registration as JSON).
4. The client sends you to `/oauth/authorize` with PKCE (`S256`) and
   `resource=https://toolyard.example.com/mcp`, then exchanges the code at
   `/oauth/token`.

Access tokens last one hour. Refresh tokens last 30 days and change on every
use. After 90 days from your consent, you sign in again. An admin can change
these lifetimes.

## What the consent screen does

After you sign in, toolyard shows:

- **who the app is**: "Identified by `<host>`" for an app that uses a client
  ID metadata document (toolyard read its registration from that host), or
  "Self-registered app" for an app that registered itself. The name and
  logo are the app's own claim: toolyard does not check them. If an
  identified app sends you back to another web host, toolyard warns you;
- **where toolyard sends you back to**, for example `claude.ai`,
  `chatgpt.com`, `127.0.0.1:33418` or `cursor://…`. Check this address: an
  app that registered itself can choose any name, but it cannot change
  where the code goes;
- a **nickname** for this connection. The default is the app's name. Use a
  name that tells your connections apart, for example "Laptop Claude Code";
- **what it may use**: everything you can use, or only the servers and
  tools you allow. You can also block servers or tools in both cases.
- for an admin only, **May manage toolyard (policies, servers, shared
  connections)**, off by default. Leave it off unless the app must change
  toolyard itself: an app never gets admin control otherwise, even an
  admin's app. Only you can turn it on for your own app; no other admin
  can turn it on for you.

When you select **Allow**, toolyard creates an agent that you own, named by
the nickname. Every call that the app makes runs as this agent. Your
normal approval rules still apply: a call that needs approval still comes
to your Inbox. When you select **Deny**, nothing is created and the app gets
`access_denied`.

If you connect the same app again (for example after you revoked it),
toolyard asks again and keeps the same agent, so its activity history
stays in one place. If the app is still connected elsewhere (for example
on your other device), that session keeps working, unless you tick **Sign
out this app's other sessions** on the consent screen.

## Restrict a connection

Open **Connected apps** in the dashboard. For each app you can:

- **Rename** it. The next call uses the new nickname.
- **Change access**: everything you can use or only chosen servers and
  tools, and blocks. An admin can also turn **May manage toolyard** on or
  off here. The change applies within a few seconds.
- **Revoke** it. Its tokens stop at once and its agent is disabled. To use
  the app again, connect it again from the app.

A tool that the connection may not use is not in its `tools/list` or
`tools.search` results. A direct call to it gets a clear error: "not allowed
for this agent: its owner limited what it may use". Search, the Inbox and
the other always-on tools stay available, so the app can still ask you for
approval.

The same limits work for ordinary agents: the **Limit your other agents**
card on the same page, or `POST /v1/me/agent-scopes`. An agent can see its
own limits in `access.whoami` (`agent_limits`).

Admins can select **Show everyone's apps** to see and manage the apps of
every person.

## Read a connection's activity

- On **Connected apps**, select **Activity** on the app. Admins go to the
  Activity page, filtered to the app's agent. Members see the app's recent
  rows in a list.
- Every audit row of a call carries the agent id, the nickname
  (`agent_name`), `agent_kind` `oauth_client`, the owner, and the app's
  OAuth client id (`oauth_client_id`).
- `GET /v1/audit` and the audit export (`GET /v1/audit/export`, CSV or
  JSON) return `oauth_client_id`. Add `?oauth_client_id=<client id>` to
  keep only the rows of one app.
- The connection's own events have their own rows:
  `mcp_oauth.authorized`, `mcp_oauth.denied`, `mcp_oauth.renamed`,
  `mcp_oauth.scope_changed`, `mcp_oauth.revoked`, and
  `mcp_oauth.refresh_reused` or `mcp_oauth.code_reused` when toolyard
  revoked the connection because a token or code was used twice. They
  carry `oauth_client_id`, and every row but `mcp_oauth.denied` names the
  app's agent, so the app's **Activity** shows them, including a
  revocation by the app itself (`/oauth/revoke`).
- `GET /v1/agents` and the **Agents** page list an app's agent with kind
  `oauth_client`. Manage it on **Connected apps**.
- An agent can read its own rows with the `audit.mine` tool.
