---
name: toolyard-connecting-clients
description: Help a person connect an MCP client (Claude.ai, ChatGPT, Claude Code, Codex, Cursor or any other MCP client) to toolyard and check that it worked. Covers the MCP URL (https://<your-toolyard>/mcp), the OAuth 2.1 browser sign-in and the consent screen (nickname, what the app may use, May manage toolyard), scopes, the Connected apps page (rename, change access, revoke, activity), agent tokens and identity keys for local clients, TOOLYARD_TOKEN_FILE for the HTTPS fallback, stdio mode, and checking the result with access.whoami. Use when a person asks how to add or reconnect toolyard in a client, when a client gets 401, access_denied, a redirect error or "Invalid session ID", when a connection shows no tools or the wrong tools, when access.whoami says there is no agent token, or when a connection stopped working after a revoke, a token reuse or 90 days.
metadata:
  toolyard-runs: anywhere
---

# Connect an MCP client to toolyard

You help a person connect their MCP client to toolyard. The person does every
browser step and keeps every token. Never ask them to paste a token, password
or one-time code into the chat, and never print one yourself.

Every connection runs as an **agent** that the person owns. Their normal
policies and Inbox apply to it: connecting a client grants no extra
permission.

## When to use

- A person wants to add toolyard to a client, or connect it again.
- A client fails to connect, or connects but shows no tools or the wrong ones.
- `access.whoami` shows no agent, or the wrong agent or owner.
- A connection stopped working.

## The URL

The MCP endpoint is the toolyard's public address followed by `/mcp`:

```text
https://<your-toolyard>/mcp
```

The dashboard's **Connected apps** page shows the exact URL with a Copy
button. Ask the person for it if you do not know it.

## Pick a method

| Method | Use it for | What the person needs |
|---|---|---|
| OAuth by URL (recommended) | Claude.ai, ChatGPT, Claude Code, Codex, Cursor, and any client that implements MCP authorization | A toolyard account. toolyard must run with a public URL and OAuth on (the default with a public URL). |
| Agent token (`ag_…`) | A local client or script that can send an `Authorization: Bearer` header, or a toolyard where OAuth is off | Create it on the dashboard's **Agents** page (**+ Add new agent**). The token is shown once. |
| Identity key | Toolyards that give each person one identity key for servers that record who made each change | An admin provisions it. The person reveals it once in the dashboard. |

ChatGPT and Claude.ai connect from their own servers, so the URL must be public
HTTPS with a valid certificate. Claude Code, Codex and Cursor connect from the
person's computer, so an address only that computer can reach is enough.

Step-by-step setup for each client: [client setup](references/client-setup.md).

## Recipe: OAuth

1. Give the person the URL and the steps for their client.
2. The client opens toolyard in their browser. They sign in with their
   toolyard account (password, or Google sign-in where the toolyard offers it).
3. They read the consent screen and select **Allow**:
   - **Who the app is**: "Identified by `<host>`" or "Self-registered app".
     The name and logo are the app's own claim and are not checked.
   - **Where toolyard sends them back to** (for example `claude.ai`,
     `chatgpt.com`, `127.0.0.1:<port>`, `cursor://…`). Tell them to check it:
     an app can choose any name, but not where the code goes.
   - **Nickname**: the agent's name. Suggest one that tells devices apart,
     such as "Laptop Claude Code".
   - **What it may use**: everything they can use, or only chosen servers and
     tools, with optional blocks.
   - **May manage toolyard (policies, servers, shared connections)**: admins
     only, off by default. Tell them to leave it off unless the app must change
     toolyard itself.
4. **Allow** creates an agent they own, named by the nickname. **Deny** creates
   nothing and the client gets `access_denied`.
5. Check it worked (below).

toolyard grants the scope `mcp`, plus `offline_access` when the client asks
for it. It ignores other scopes: tool access is chosen on the consent screen,
not by scope. Every token is valid only for this toolyard's `/mcp`.

## Recipe: agent token

1. The person opens **Agents → + Add new agent**, names it, and copies the
   token. It cannot be shown again. The dialog also shows setup snippets.
2. They put the token in the client's MCP configuration as the header
   `Authorization: Bearer <token>`. A file readable only by them is better
   than a command line, which shell history keeps.
3. Check it worked (below).

A person with an enrolment code can exchange it with the toolyard CLI:
`toolyard auth login <code> --server https://<your-toolyard>`. The CLI keeps
the token in `~/.toolyard/config.json` (mode 0600).

## Check that it worked

Call `access.whoami` through the new connection:

```text
access.whoami
{"_reason": "Confirm the new client connection runs as the person's own agent"}
```

- `caller_id` is an `ag_…` id, and `owner.email` is the person's.
- An OAuth app shows `agent_kind` `oauth_client` and its nickname as
  `agent_name`.
- `agent_limits` appears when the consent screen limited the app.
- `can_change_policies` is false unless they turned on May manage toolyard.
- A `note` that says "unauthenticated local caller: no agent token on this
  connection" means the client sent no token. Fix the configuration.

Then run `tools.search` with a few words to confirm that the expected tools are
there. If a server's tools answer with a connect link, that is a separate
sign-in to that server: show the link and wait (see the `toolyard-basics`
skill, `skills.get toolyard/toolyard-basics`).

## Manage connections (Connected apps)

On **Connected apps** the person can, for each app:

- **Rename** it. The next call uses the new nickname.
- **Change access**: everything or chosen servers and tools, and blocks. An
  admin can also turn May manage toolyard on or off. Changes apply within a
  few seconds.
- **Revoke** it. Its tokens stop at once and its agent is disabled. Revoking
  also cancels what it had pending in the Inbox. To use the app again, connect
  it again from the app.
- Open **Activity** to see the app's recent calls.

Connecting the same app again asks again and keeps the same agent, so its
history stays in one place. Another device that shares the app's sign-in keeps
working unless the person ticks **Sign out this app's other sessions** on the
consent screen. The **Limit your other agents** card on the same page limits
token-based agents the same way. Admins can select **Show everyone's apps**.

## Tokens outside MCP

- `TOOLYARD_TOKEN_FILE`: a file that holds a toolyard token, used by the HTTPS
  fallback (`POST /v1/code/run`, or the `ty` command from `GET /v1/code/ty`)
  when MCP fails. It holds an agent token or a session token. `ty` refuses a
  file that group or others can read: `chmod 600` it. See the
  `toolyard-code-mode` skill.
- A session token (`tys_…`) is minted with an agent token at
  `POST /v1/agents/session-tokens` for one work session (60-3600 seconds). It
  works on the code, jobs and skills REST routes, `/v1/agents/approvals/…`
  and `GET /v1/agents/whoami`, not on `/mcp`.
- Operator tokens (`tyop_…`) never work on the code routes. OAuth access tokens
  work only on `/mcp`.
- `GET /v1/agents/whoami` with `Authorization: Bearer` returns the token's
  `agent_id` and `name`: a quick check that a token is live.

## stdio mode

`toolyard serve -stdio` runs a toolyard gateway that also speaks MCP on stdin
and stdout, for an agent host wired directly to a gateway it runs. Calls on
stdio carry no agent token: they are an unauthenticated local caller, so the
Inbox, grants, sessions, `audit.mine` and connect links do not work there. For
normal use, connect over HTTP with OAuth or an agent token.

## Pitfalls

- Never ask for or repeat a token, password or code in chat. If a token leaked,
  the person rotates it on the Agents page or revokes the app.
- Do not suggest May manage toolyard to get past an approval. Approvals stay
  the person's decision.
- Admin control never comes with "everything you can use": even an admin's app
  cannot change policies, reconnect servers or sign in shared connections
  without May manage toolyard.
- Problems and fixes: [troubleshooting](references/troubleshooting.md).
