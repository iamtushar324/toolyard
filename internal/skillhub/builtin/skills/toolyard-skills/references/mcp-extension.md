# MCP Skills extension

Some MCP clients read skills natively, through the MCP Skills extension
(SEP-2640), instead of calling the `skills.*` tools. toolyard supports it
while its skills hub is on. If your client does not use these methods, use
the tools; both serve the same catalog with the same access checks.

## Capability

On its HTTP MCP endpoint (`/mcp`), toolyard declares, next to the
`resources` capability:

```json
{ "capabilities": { "extensions": { "io.modelcontextprotocol/skills": { "directoryRead": true } } } }
```

It does so in the `initialize` result (protocol 2025-11-25) and the
`server/discover` result (protocol 2026-07-28). A gateway run over stdio
does not declare the extension: it serves the tools and `resources/read`,
not the extension's methods.

## Methods

| Method | Params | Answer |
|---|---|---|
| `skills/list` | `cursor` (optional) | `skills`: entries `{uri, frontmatter, resources: [{uri, digest, size}]}` with every file of the skill, `SKILL.md` included; `nextCursor` while more pages exist. 2026-07-28 results also carry `ttlMs` and `cacheScope: "private"` |
| `skills/get` | `uri` of a `SKILL.md` | `skill`: one entry |
| `resources/read` | `uri` of any `skill://` file | the whole file as text, with its `mimeType` |
| `resources/templates/list` | none | the template `skill://{+path}` |
| `resources/directory/read` | `uri` of a skill folder or a folder in it | its direct children; folders have `mimeType: "inode/directory"` |

`resources/read` always returns the whole file, whose bytes match its
digest; the `skills.read` tool returns large files in parts instead.

## URIs

- `skill://<source>/<skill path>/SKILL.md`, files beside it, and folder
  URIs without a trailing slash.
- Each path segment is percent-encoded: a space is `%20`, `#` is `%23`,
  `%` is `%25`, non-ASCII text is its UTF-8 bytes.
- A skill at a source's own folder is served as
  `skill://<source>/<name>/SKILL.md`, so the last segment is always the
  skill's name; `skill://<source>/SKILL.md` still resolves.
- Listing a source's folder, or a folder above skills, leaves out the
  folders of skills marked `runs: host` unless you run on that host; a
  skill's own folder lists everything.

## Access and errors

- Each method passes the same checks as the matching tool (`skills.list`,
  `skills.get`, and `skills.read` for `resources/read` and
  `resources/directory/read`): your access, your agent's deny list, and an
  explicit deny policy.
- An unknown URI, one you cannot see, or a refused call is JSON-RPC error
  `-32602` (invalid params). `resources/read` on protocol 2025-11-25
  answers `-32002` (resource not found) instead, as for any resource.
- Reads of skill content outside a tool call (`skills/get`,
  `resources/read`, `resources/directory/read`, a bundle download) write a
  `skill.read` audit row with the URI and `owner/repo@commit`, also when
  refused.

## Change notifications

When a source's active version changes, toolyard sends
`notifications/resources/list_changed`, best effort. A stateless MCP
connection never hears it: list again when you need fresh digests.
