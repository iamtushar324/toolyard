# Operating Toolyard from a CLI agent

An **operator token** lets a CLI agent (Claude Code, Codex, a script) do what the
dashboard does: add and fix MCP servers, create agents, manage events, memory,
notes and settings, and run tools. It acts as the Toolyard user who owns it,
limited to its scopes. It is a different credential from an **agent token**:
an agent token calls tools through `/mcp`; an operator token administers
Toolyard.

## Scopes

| Scope | Allows |
|---|---|
| `read` | every GET |
| `write` | the rest of the dashboard: servers, agents, settings, events, memory, notes, tool runs, secret **requests** |
| `owner` | what decides or pre-authorizes calls, or holds credentials: approvals, inbox decisions and grants, policies, auto-approval rules, per-tool policy, users, passkeys, push subscriptions, approval/channel/retention settings, secret **values**, audit purges |

New tokens get `read write`. `owner` must be asked for explicitly, and a
token can only mint tokens with scopes it holds. Tool runs made with an
operator token go through the same policy and approval queue as any other
call; the approval card names the token.

Never available to operator tokens, whatever the scope: `/reveal` routes,
login, logout and first-user setup. Secret values are never returned by any
route.

## Getting a token

On the Toolyard host (shell access to the data directory is already full
operator trust):

```bash
toolyard-gateway operator-token create --data /var/lib/toolyard \
  --name "claude-code on hypnos" --out /path/token   # file is written 0600
toolyard-gateway operator-token list   --data /var/lib/toolyard
toolyard-gateway operator-token revoke --data /var/lib/toolyard op_…
```

`--user` picks the user (username, email or id; default: the oldest active
admin), `--scopes read,write[,owner]`, `--ttl 720h` sets an expiry.

From the API, an admin can mint one with `POST /v1/operator-tokens
{"name":"…","scopes":["read","write"],"ttl_hours":0}`. The plaintext is
returned once.

## Using the CLI

```bash
toolyard server https://toolyard.example
toolyard admin login --token-file /path/token   # or TOOLYARD_OPERATOR_TOKEN
toolyard admin whoami
toolyard admin routes            # every route, method and required scope
toolyard api GET /v1/servers     # any route; -d '{"json":1}' or -d @file
```

Shortcuts:

```bash
toolyard admin servers list | get NAME | add --from server.json | patch NAME --from patch.json
toolyard admin servers reconnect NAME | remove NAME
toolyard admin secrets list
toolyard admin secrets request NAME --description "GitHub PAT, repo scope"
toolyard admin agents list | create NAME --out FILE | rotate ID --out FILE
toolyard admin agents disable ID | enable ID | remove ID
toolyard admin tokens list | create NAME [--scopes read,write] --out FILE | revoke ID
toolyard admin approvals list
toolyard admin tools list | run TOOL --args '{"k":"v"}'
toolyard admin settings get | set KEY VALUE
toolyard admin audit [--limit 50]
```

Commands that produce a credential (`agents create`, `agents rotate`,
`tokens create`) write it to `--out` with mode 0600 and never print it.

## Adding an MCP server that needs a credential

You never handle the credential. Request it, wire it, ask the owner:

```bash
toolyard admin secrets request GITHUB_TOKEN --description "GitHub PAT for the github MCP server"
cat > github.json <<'JSON'
{"name":"github","transport":"http","url":"https://api.githubcopilot.com/mcp/",
 "headers":{"Authorization":"Bearer ${secret://GITHUB_TOKEN}"},"enabled":true}
JSON
toolyard admin servers add --from github.json
```

Then tell the owner: "Please set `GITHUB_TOKEN` in Toolyard → Settings →
Secrets." The server shows a connect error until then. When the owner saves
the value, every server referencing it reconnects automatically. Check it
with `toolyard admin servers get github`.

References:

- `secret://NAME`: the whole value is the secret (env vars, raw API keys).
- `${secret://NAME}`: embedded in a larger value (`Bearer ${secret://NAME}`).

Use an existing secret by name (`toolyard admin secrets list` shows names,
descriptions, `pending`, and which servers use each). Values are resolved
only when Toolyard dials the server, and are never returned by any route.

For OAuth-based servers, add the server, run `POST /v1/servers/NAME/oauth/discover`,
then `POST /v1/servers/NAME/oauth/begin` (or `oauth/device-begin` +
`oauth/device-poll`). Give the owner the authorization URL it returns to approve
in their browser; the tokens are stored server-side and never returned.

## When you hit `operator_scope`

The response names `required_scope`. Don't look for another way in: ask the
owner to do it in the dashboard, or to mint you a token with that scope.
