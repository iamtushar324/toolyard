# Toolyard stage

This stage runs one gateway and two synthetic MCP connectors on the same host.
It uses a separate Linux account, SQLite database, signing keys, and proxy.
The identity provider is shared. Application sessions and application data are separate.

## Build and install

Build only the two targets with at most two Go workers:

```sh
GOMAXPROCS=2 go build -p 1 -ldflags "-X main.version=stage-$(git rev-parse --short HEAD)" -o /tmp/toolyard-stage ./cmd/gateway
GOMAXPROCS=2 go build -p 1 -o /tmp/stage-fixtures ./cmd/stage-fixtures
```

Install binaries into a new `/opt/toolyard-stage/releases/<revision>/` directory.
Every directory above the binaries must permit traversal by the `toolyard-stage` user.
Use `/opt/toolyard-stage/current` as the release symlink.
The service account owns `/var/lib/toolyard-stage`, with mode 0700.

Create `/etc/toolyard-stage/stage.env` with mode 0600. It must contain:

```dotenv
TOOLYARD_ENVIRONMENT=stage
TOOLYARD_STAGE_OWNER_EMAIL=<verified-owner-email>
TOOLYARD_CLERK_SECRET_KEY=<identity-provider-secret>
TOOLYARD_CLERK_PUBLISHABLE_KEY=<identity-provider-publishable-key>
TOOLYARD_CLERK_ORGANIZATION_ID=<identity-provider-organization>
```

Use only the identity provider configuration. Do not copy a live database,
application session key, connector token, notification subscription, or unrelated environment variable.

Before the first start, create the fixtures and a random bootstrap account:

```sh
sudo /opt/toolyard-stage/current/stage-fixtures -seed \
  -data /var/lib/toolyard-stage \
  -credentials /etc/toolyard-stage/bootstrap.json
sudo chown -R toolyard-stage:toolyard-stage /var/lib/toolyard-stage
```

The seed refuses a database that already contains users. It never resets data.
Protect the generated credential file. It is for automated stage checks and emergency access.
The owner uses the existing Google sign-in flow; the verified owner email attaches to this local owner account.

Install both service files from this directory. Enable them with systemd.
The fixture readiness check prevents the gateway from starting before its test services answer.
Run `proxy.py` once as root to create the separate Swarm router on the existing `bk-dev` network.
The router uses the existing `le` certificate resolver. It does not change the live Toolyard service.

## Validation

```sh
sudo python3 scripts/stage-smoke.py \
  --credentials /etc/toolyard-stage/bootstrap.json \
  --output /tmp/toolyard-stage-smoke.json
```

This check uses HTTPS and the real MCP endpoint. It verifies authentication,
request deduplication, permission refusal, exact parameter matching,
one-use grants, and synthetic connector failures. It creates a pending
question for a browser test. It never prints credentials or grant tokens.

Run the settings behavior checks with `node --test scripts/settings-ui.test.cjs`.
Test Settings Save, Discard, failed saves, draft retention, and category deep links.
Check keyboard focus, the settings categories on a narrow screen, and table scrolling.

Test browser answers separately: multiple choices, custom text, empty submission,
drafts after an event and reload, keyboard input, and a 390-pixel layout.
The browser preview resize operation may be unavailable. A same-origin frame
with the actual application assets can verify CSS layout at that width.
That frame does not replace a real-device passkey or Google sign-in test.

## Release and rollback

Before each update, save a consistent SQLite backup with the SQLite backup API.
Record the current release symlink and the health version.
Install the candidate in a new release directory, switch the symlink atomically,
and restart only `toolyard-stage` and `toolyard-stage-fixtures`.
Verify HTTPS health, both connections, saved answers, and the MCP permission flow.

For an application rollback, point `current` at the recorded prior release,
restart the two stage services, and verify their health version. Migration 0031
only adds an index; the previous stage binary can read the database. Do not
restore an old database over new feedback as part of a normal code rollback.
No command in this workflow restarts or alters the live Toolyard service.
