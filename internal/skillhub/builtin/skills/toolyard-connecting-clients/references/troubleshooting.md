# Troubleshooting client connections

Start with `access.whoami` through the connection when it connects at all,
and with the client's own error when it does not.

## The client cannot connect

| Symptom | Cause | Fix |
|---|---|---|
| `401` with `WWW-Authenticate: Bearer resource_metadata=…` | Normal first step: the client has no token yet. | The client must start the OAuth sign-in. A client without MCP authorization needs an agent token header instead. |
| `401` with no OAuth hint, or the `/.well-known/oauth-*` and `/oauth/*` paths answer `404` | OAuth is off: toolyard runs without a public URL, or an admin turned it off (`mcp_oauth_enabled`). Issued OAuth tokens stop working while it is off. | Ask an admin to turn it on (Connected apps), or use an agent token. |
| `401` `toolyard: bearer token required` | The toolyard requires a token on `/mcp` (always when it has a public URL), and the client sent none. | Configure OAuth or an agent token. |
| `401` `toolyard: invalid agent token; re-enroll via the dashboard` | The token is wrong or rotated, its agent is disabled or deleted, or its owner is blocked. | Create or rotate the token on the Agents page, and update the client configuration. |
| `401` `toolyard: the access token is invalid, expired or revoked; sign in again` | An OAuth token expired or its app was revoked. | Sign in again from the client (Claude Code: `/mcp`; Codex: `codex mcp login toolyard`). |
| `503` `toolyard: could not check the agent token right now; retry` | toolyard could not reach its database. The token may be fine. | Retry after a few seconds. Do not discard the token. |
| ChatGPT or Claude.ai cannot reach the URL | They connect from the internet. | Use a public HTTPS URL with a valid certificate. Local clients can use an address only their computer reaches. |
| An error page in the browser before the consent screen | The authorization request is invalid, or its redirect host is not on the admin's allow list (`mcp_oauth_redirect_hosts`). | Ask an admin to add the client's host (for example `claude.ai`, `chatgpt.com`, `localhost`, `127.0.0.1`, or `cursor:` for Cursor). |
| Redirect to a private scheme refused | A private-use redirect scheme must be a reverse-DNS name or listed in `mcp_oauth_custom_schemes` (default `cursor,vscode,vscode-insiders,windsurf,zed`). | Ask an admin to add the scheme. |
| The client reports `access_denied` | The person selected **Deny**. Nothing was created. | Connect again and select **Allow**. |
| The sign-in or the code exchange fails after a delay | The person took more than 10 minutes to sign in and answer, or the client did not exchange the code within 60 seconds. | Start the connection again from the client. |
| `Invalid session ID` (HTTP 404) after a toolyard restart | A stateful MCP session ended with the restart. | Reconnect the client, or restart its MCP connection. |

## It connects, but the tools are wrong

| Symptom | Cause | Fix |
|---|---|---|
| `access.whoami` has a `note`: "unauthenticated local caller: no agent token on this connection" | The client sent no token (or a stdio connection). | Configure OAuth or the `Authorization: Bearer` header. |
| Wrong `agent_name` or `owner` | The client uses another person's or another agent's token. | Each person connects with their own account or token. Never share tokens. |
| Expected tools are missing | The consent screen limited the app (`agent_limits` in `access.whoami`), or the owner has no grant for that server, or the surface mode hides it. | Change access on **Connected apps**; ask an admin to grant the server; find hidden tools with `tools.search` and call them with `tools.execute`. |
| A call says `not allowed for this agent: its owner limited what it may use` | The app's own limits. | The owner changes the app's access on **Connected apps**. |
| A call answers with a connect link | The connection works; that server needs its own sign-in. | Show the link as a clickable Markdown link and retry after the person is done. |
| `policies.set` or `servers.reconnect` is refused for an admin's app | May manage toolyard is off (the default). | Leave it off unless the app must manage toolyard. Only the app's owner, while an admin, can turn it on. |

## It worked, then stopped

| Symptom | Cause | Fix |
|---|---|---|
| Every call fails with `401` | The app was revoked on **Connected apps**, or the 90-day sign-in limit passed. | Connect again from the app. The same agent comes back, with its history. |
| Revoked by itself; the Activity shows `mcp_oauth.refresh_reused` or `mcp_oauth.code_reused` | A refresh token or code was used twice, so toolyard revoked the grant and disabled its agent. A refresh retried within 60 seconds, while its replacement is still unused, does not trigger this. | Connect again. If nobody else could have used the token, the client probably retried badly; otherwise treat it as a leaked token. |
| Pending Inbox requests vanished after a revoke | Revoking cancels the app's pending approvals and Inbox requests. | Ask again after reconnecting, if still needed. |

## Tokens outside MCP

| Symptom | Cause | Fix |
|---|---|---|
| `ty` refuses the token file | The file is readable by group or others. | `chmod 600` the file named by `TOOLYARD_TOKEN_FILE`. |
| `ty` refuses an `http://` URL | Tokens would travel in clear text. | Use `https://`. `TOOLYARD_URL_INSECURE=1` exists only for a network you trust. |
| `403` on `/v1/code/*` | An operator token (`tyop_…`) never works there. | Use an agent token or a session token. |
| An OAuth access token fails on a REST route | OAuth access tokens are valid only on `/mcp`. | Use an agent token or a session token for REST. |
| A session token fails on `/mcp` | Session tokens (`tys_…`) work only on the code, jobs and skills REST routes, `/v1/agents/approvals/…` and `GET /v1/agents/whoami`. | Use OAuth or an agent token for MCP. |
