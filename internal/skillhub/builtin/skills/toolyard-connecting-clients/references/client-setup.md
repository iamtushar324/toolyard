# Client setup

Replace `https://<your-toolyard>/mcp` with the URL from the dashboard's
**Connected apps** page. With OAuth, the person signs in and selects **Allow**
on the consent screen at the end of each recipe. Client menus change between
versions: if a label differs, look for the client's "custom MCP server" or
"connector" setting.

## ChatGPT (web)

Custom MCP servers need developer mode, which needs a paid ChatGPT plan. The
URL must be public HTTPS with a valid certificate.

1. Open **Settings → Apps & Connectors → Advanced settings**.
2. Turn on **Developer mode**.
3. Go back to **Apps & Connectors** and select **Create**.
4. Type a name (for example `toolyard`) and paste the URL in **MCP server URL**.
5. Set **Authentication** to **OAuth**, then select **Create**.
6. The browser opens toolyard. Sign in, read the consent screen, select
   **Allow**.
7. In a chat, open the **+** menu, select **Developer mode**, and turn on the
   toolyard connector.

## Claude.ai (web and desktop)

The URL must be public HTTPS with a valid certificate.

1. Open **Settings → Connectors**.
2. Select **Add custom connector**.
3. Type a name (for example `toolyard`), paste the URL, select **Add**.
4. Select **Connect**. Sign in to toolyard and select **Allow**.
5. In a chat, open the tools menu and turn on toolyard.

On Team and Enterprise plans, an owner adds the connector first in the
organization's connector settings. Then each person connects it.

## Claude Code

With OAuth:

```bash
claude mcp add --transport http toolyard https://<your-toolyard>/mcp
```

Then start Claude Code, type `/mcp`, select **toolyard**, then
**Authenticate**. Claude Code keeps and refreshes the tokens. Use `/mcp`
again to sign out or sign in again.

With an agent token instead, the server entry carries the header. In a
project's `.mcp.json` (keep the file out of version control):

```json
{
  "mcpServers": {
    "toolyard": {
      "type": "http",
      "url": "https://<your-toolyard>/mcp",
      "headers": { "Authorization": "Bearer <token>" }
    }
  }
}
```

The dashboard's new-agent dialog shows the same entry for `~/.claude.json`
and a `claude mcp add … --header "Authorization: Bearer <token>"` command.

## Codex

1. Add the server to `~/.codex/config.toml`:

   ```toml
   [mcp_servers.toolyard]
   url = "https://<your-toolyard>/mcp"
   ```

   Recent versions can also run
   `codex mcp add toolyard --url https://<your-toolyard>/mcp`.
2. Sign in: `codex mcp login toolyard`.
3. The browser opens toolyard. Sign in and select **Allow**.

## Cursor

1. Open **Cursor Settings → Tools & Integrations** (or **MCP**) and select
   **Add Custom MCP**. Cursor opens `~/.cursor/mcp.json`.
2. Add the server:

   ```json
   {
     "mcpServers": {
       "toolyard": { "url": "https://<your-toolyard>/mcp" }
     }
   }
   ```

3. In the settings, toolyard shows **Needs login**. Select it, sign in, and
   select **Allow**.

Cursor returns to a `cursor://` address after sign-in. If an admin set the
allow list of client redirect hosts, it must include `cursor:`.

## Any other MCP client

A client that implements the MCP authorization specification finds everything
from the URL:

1. Its first request to `/mcp` without a token gets `401` with
   `WWW-Authenticate: Bearer resource_metadata="https://<your-toolyard>/.well-known/oauth-protected-resource"`.
2. That document names toolyard as the authorization server.
   `/.well-known/oauth-authorization-server` lists the endpoints.
3. The client registers itself (`POST /oauth/register`, RFC 7591), or uses a
   client ID metadata document (an `https` `client_id` that serves its own
   registration as JSON).
4. It sends the person to `/oauth/authorize` with PKCE (`S256`, required) and
   `resource=https://<your-toolyard>/mcp`, then exchanges the code at
   `/oauth/token`. Revocation is at `/oauth/revoke` (RFC 7009).

A client without MCP authorization can still connect if it can send a header:
use an agent token as `Authorization: Bearer <token>`. A proxy that strips the
`Authorization` header can send the token in an `x-bf-vk` header instead.
toolyard reads `x-bf-vk` only when the request has no `Authorization` header
at all: any `Authorization` header, even one that is not a bearer token,
hides it.

## Lifetimes

| What | Default | Notes |
|---|---|---|
| Access token (`tyat_…`) | 1 hour | An admin can set 5-1440 minutes. |
| Refresh token (`tyrt_…`) | 30 days | Renewed and replaced on every use. |
| Whole sign-in | 90 days from consent | The person signs in again after it. |
| Authorization code | 60 seconds | Single use. |
| Sign-in and consent | 10 minutes | Start again from the client after that. |
| Agent token (`ag_…`) | Long-lived | Stops when it is rotated, or its agent is disabled or deleted, on the Agents page. |
