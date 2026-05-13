#!/usr/bin/env bash
# toolyard install script.
#
# What it does:
#   1. builds the binary from this repo
#   2. installs it at /usr/local/bin/toolyard
#   3. creates a system user `toolyard` with home /var/lib/toolyard
#   4. writes /etc/systemd/system/toolyard.service (sandboxed)
#   5. enables + starts the service
#
# Idempotent — safe to re-run after pulling new code (it rebuilds and
# `systemctl restart`s).
#
# TLS / public-internet posture is intentionally NOT configured here. The
# unit binds 0.0.0.0:18787 so a separate reverse proxy (nginx over
# Tailscale, in your case) can hit it. Clients reaching :18787 directly
# bypass the proxy — make sure your firewall blocks it for the public
# internet, or switch to binding only the Tailscale interface IP.
#
# Usage:
#   sudo ./deploy/install.sh                          # default install
#   sudo ./deploy/install.sh --import-from /tmp/ty-test  # also copy existing DB
#   sudo PUBLIC_URL=https://tool.example.net ./deploy/install.sh
#
# Environment / flags:
#   PUBLIC_URL=...           sets -public-url (enables Origin enforcement on
#                            POST/PATCH/DELETE; required for HSTS-on-HTTPS).
#                            Leave unset if you don't have a stable hostname yet.
#   TRUSTED_PROXY=cidr,...   IPs we trust X-Forwarded-* from. Default covers
#                            loopback + Tailscale + RFC1918.
#   PUSH_SUBJECT=mailto:...  VAPID `sub` claim (your contact email).
#   NO_STDIO_UPSTREAMS=true  refuse subprocess MCP upstreams. Recommended once
#                            you trust the auth surface.
#   MEMPALACE=auto|on|off    MemPalace integration. auto (default) installs
#                            uv + mempalace-mcp to /usr/local/bin if missing
#                            and lets the gateway register the upstream; on
#                            hard-fails when install doesn't land; off skips
#                            the integration entirely. The systemd user's PATH
#                            includes /usr/local/bin, so installing the shim
#                            there is what makes it reachable.
#   NOTES=auto|on|off        Notes workspace integration. auto (default)
#                            creates $NOTES_DIR + registers the filesystem
#                            MCP upstream if `npx` is on PATH; on hard-fails
#                            when npx is missing; off skips it entirely.
#   NOTES_DIR=/path          Override the notes directory (default
#                            $DATA_DIR/notes — lives inside the data root
#                            so existing backups capture it).
#   --import-from <dir>      copy existing toolyard.db + session.key from <dir>.
#   --mempalace <mode>       same as MEMPALACE env var.
#   --notes <mode>           same as NOTES env var.
#
# Run from the repo root.

set -euo pipefail

BINARY_PATH="/usr/local/bin/toolyard"
SERVICE_PATH="/etc/systemd/system/toolyard.service"
DATA_DIR="/var/lib/toolyard"
USER_NAME="toolyard"
LISTEN_ADDR="0.0.0.0:18787"
# ClickHouse stack — analytical SQL server, runs in docker. Compose file
# lives in the repo; data + password env file persist under DATA_DIR so
# the same backup of /var/lib/toolyard/ captures everything.
CH_COMPOSE="$(pwd)/deploy/clickhouse/docker-compose.yaml"
CH_DATA_DIR="$DATA_DIR/clickhouse"
CH_ENV_FILE="$DATA_DIR/clickhouse-runtime.env"
# uid:gid the clickhouse-server image runs as internally. Bind mounts
# need to be owned by this so CH can write to them.
CH_UID=101
CH_GID=101
# Shared docker network for the lake stack. ClickHouse and Grafana both
# attach to it so Grafana can resolve `clickhouse:9000` without exposing
# the native protocol on the host. Declared external in both compose
# files; created here so neither compose owns its lifecycle.
LAKE_NETWORK="toolyard_lake_net"
# Operator-managed env file. systemd reads it on every (re)start, so secrets
# like TOOLYARD_LAKE_TOKEN can change without re-rendering the unit. The
# install script seeds a commented stub on first install and never touches
# it again.
ENV_DIR="/etc/toolyard"
ENV_FILE="$ENV_DIR/toolyard.env"

PUBLIC_URL="${PUBLIC_URL:-}"
PUSH_SUBJECT="${PUSH_SUBJECT:-mailto:ops.nova.21@gmail.com}"
TRUSTED_PROXY="${TRUSTED_PROXY:-127.0.0.1/32,::1/128,100.64.0.0/10,10.0.0.0/8,192.168.0.0/16,172.16.0.0/12}"
NO_STDIO_UPSTREAMS="${NO_STDIO_UPSTREAMS:-false}"
# Stateless MCP: defaults to true here because agents like hermes don't
# auto-reconnect on "Invalid session ID" 404s after a toolyard restart.
# Stateful is more spec-pure (lets the server push tools/list_changed
# notifications) but the operational pain isn't worth it for a single-user
# gateway. Override with STATELESS_MCP=false if you genuinely need server
# push notifications and your agents handle session-invalidation cleanly.
STATELESS_MCP="${STATELESS_MCP:-true}"
# MemPalace integration. auto = install if missing + proceed; on = also
# fail-hard if install doesn't produce /usr/local/bin/mempalace-mcp; off
# = skip the integration entirely. Forwarded to the gateway as -mempalace.
MEMPALACE="${MEMPALACE:-auto}"
# Directory the uv-managed mempalace venv lives in (binary shim still ends
# up at /usr/local/bin/mempalace-mcp). Persistent + root-owned so re-runs
# upgrade in place and the systemd-sandboxed toolyard user only reads it.
UV_TOOL_STATE_DIR="/var/lib/uv-tools"
# Notes workspace — markdown scratchpad agents read/write through the
# @modelcontextprotocol/server-filesystem MCP. Lives inside DATA_DIR so
# the same /var/lib/toolyard snapshot captures it for backups.
NOTES="${NOTES:-auto}"
NOTES_DIR_DEFAULT="$DATA_DIR/notes"
NOTES_DIR="${NOTES_DIR:-$NOTES_DIR_DEFAULT}"
IMPORT_FROM=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --import-from)        IMPORT_FROM="$2"; shift 2 ;;
    --public-url)         PUBLIC_URL="$2"; shift 2 ;;
    --no-stdio-upstreams) NO_STDIO_UPSTREAMS=true; shift ;;
    --mempalace)          MEMPALACE="$2"; shift 2 ;;
    --notes)              NOTES="$2"; shift 2 ;;
    -h|--help)            sed -n '2,52p' "$0"; exit 0 ;;
    *) echo "unknown flag: $1" >&2; exit 1 ;;
  esac
done

case "$MEMPALACE" in
  on|off|auto) ;;
  *) echo "MEMPALACE must be one of: on, off, auto (got: $MEMPALACE)" >&2; exit 1 ;;
esac
case "$NOTES" in
  on|off|auto) ;;
  *) echo "NOTES must be one of: on, off, auto (got: $NOTES)" >&2; exit 1 ;;
esac

if [[ $EUID -ne 0 ]]; then
  echo "must run as root (use sudo)" >&2
  exit 1
fi
if [[ ! -f cmd/gateway/main.go ]]; then
  echo "must run from the toolyard repo root" >&2
  exit 1
fi
if ! command -v go >/dev/null 2>&1; then
  echo "go (1.21+) is required to build" >&2
  exit 1
fi

# Rebuild as the invoking user so go's module cache lives in their home.
BUILDER="${SUDO_USER:-root}"

say() { printf '\033[1;32m==>\033[0m %s\n' "$*"; }

say "building toolyard"
TMP_BIN="$(mktemp /tmp/toolyard.XXXXXX)"
chown "$BUILDER":"$BUILDER" "$TMP_BIN"
# GOTOOLCHAIN=auto so an older system Go (e.g. 1.23) transparently fetches
# the toolchain version pinned in go.mod. Without this, distros pinned at
# GOTOOLCHAIN=local fail with "go.mod requires go >= X (running Y)".
#
# GOSUMDB=sum.golang.org is required because Go refuses to verify a
# downloaded toolchain when GOSUMDB=off; we set it explicitly here so the
# installer works even when the invoking user has GOSUMDB=off in their env.
# GOPROXY=https://proxy.golang.org,direct mirrors that for the module
# fetch path.
sudo -u "$BUILDER" env \
    GOTOOLCHAIN=auto \
    GOSUMDB=sum.golang.org \
    GOPROXY="https://proxy.golang.org,direct" \
  bash -c "cd '$(pwd)' && CGO_ENABLED=1 go build -trimpath -ldflags '-s -w' -o '$TMP_BIN' ./cmd/gateway"

say "installing binary -> $BINARY_PATH"
install -m 0755 "$TMP_BIN" "$BINARY_PATH"
rm -f "$TMP_BIN"

say "ensuring system user $USER_NAME"
if ! id "$USER_NAME" >/dev/null 2>&1; then
  useradd --system --home-dir "$DATA_DIR" --shell /usr/sbin/nologin "$USER_NAME"
fi

say "ensuring data dir $DATA_DIR (mode 0700)"
install -d -m 0700 -o "$USER_NAME" -g "$USER_NAME" "$DATA_DIR"

# Provision the env file with a commented stub on first install. The systemd
# unit references it as `EnvironmentFile=-...` (note the `-`) so a missing
# file is non-fatal, but having a discoverable path with examples is the
# whole point of this pattern.
say "ensuring env dir $ENV_DIR (mode 0750, group $USER_NAME)"
install -d -m 0750 -o root -g "$USER_NAME" "$ENV_DIR"
if [[ ! -f "$ENV_FILE" ]]; then
  say "seeding $ENV_FILE (edit then 'systemctl restart toolyard')"
  cat > "$ENV_FILE" <<'EOF'
# toolyard runtime environment. Read by systemd via EnvironmentFile=.
# Edit and `sudo systemctl restart toolyard` to apply.
#
# Toolyard's own runtime config (lake API token, Grafana origin) lives
# in the settings DB and is managed from the dashboard's Settings tab —
# no env vars needed here for the gateway itself. The lake token is
# auto-generated on first boot; reveal/rotate from the UI.
#
# This file is also the env_file for the deploy/grafana docker-compose
# stack, so any GF_*-prefixed variables below are consumed by the
# Grafana container at boot. Only the admin credentials really benefit
# from being here; structural config lives in compose.
#GF_SECURITY_ADMIN_USER=admin
#GF_SECURITY_ADMIN_PASSWORD=changeme
EOF
  chown root:"$USER_NAME" "$ENV_FILE"
  chmod 0640 "$ENV_FILE"
fi

# MemPalace runtime — install `uv` (if missing) and `mempalace-mcp` so the
# gateway's startup wiring finds them on PATH. The toolyard service runs as
# an unprivileged user whose PATH is /usr/local/sbin:/usr/local/bin:..., so
# we deliberately drop both binaries under /usr/local/bin/ (not the
# operator's ~/.local/bin/). The tool venv data goes to /var/lib/uv-tools/
# so it survives operator-user re-installs and isn't tied to a home dir.
#
# Skipped entirely when MEMPALACE=off; soft-warns on auto when install
# fails; hard-fails on on.
ensure_mempalace() {
  if [[ "$MEMPALACE" == "off" ]]; then
    say "mempalace: integration disabled (MEMPALACE=off)"
    return 0
  fi

  if [[ ! -x /usr/local/bin/uv ]] && ! command -v uv >/dev/null 2>&1; then
    say "installing uv -> /usr/local/bin/uv"
    # The astral installer reads UV_INSTALL_DIR for the destination. We
    # pipe through env so the variable reaches sh inside the pipe.
    if ! curl -LsSf https://astral.sh/uv/install.sh | env UV_INSTALL_DIR=/usr/local/bin sh; then
      if [[ "$MEMPALACE" == "on" ]]; then
        echo "mempalace: uv install failed and MEMPALACE=on; aborting" >&2
        exit 1
      fi
      echo "mempalace: uv install failed; gateway will boot without mempalace" >&2
      return 0
    fi
  fi
  local UV_BIN
  UV_BIN="$(command -v uv 2>/dev/null || echo /usr/local/bin/uv)"

  install -d -m 0755 "$UV_TOOL_STATE_DIR"

  if [[ ! -x /usr/local/bin/mempalace-mcp ]]; then
    say "installing mempalace via $UV_BIN tool install (state: $UV_TOOL_STATE_DIR, shim: /usr/local/bin/mempalace-mcp)"
    # UV_TOOL_DIR controls where the venv lives; UV_TOOL_BIN_DIR is where
    # uv drops the shim. --force keeps re-runs idempotent on upgrades.
    if ! env UV_TOOL_DIR="$UV_TOOL_STATE_DIR" \
            UV_TOOL_BIN_DIR="/usr/local/bin" \
            "$UV_BIN" tool install --force mempalace; then
      if [[ "$MEMPALACE" == "on" ]]; then
        echo "mempalace: install failed and MEMPALACE=on; aborting" >&2
        exit 1
      fi
      echo "mempalace: install failed; gateway will boot without mempalace" >&2
      return 0
    fi
  else
    say "mempalace-mcp already at /usr/local/bin/mempalace-mcp; upgrading"
    env UV_TOOL_DIR="$UV_TOOL_STATE_DIR" \
        UV_TOOL_BIN_DIR="/usr/local/bin" \
        "$UV_BIN" tool upgrade mempalace || true
  fi

  # Hard check: the shim must exist and be executable for the gateway's
  # `exec.LookPath("mempalace-mcp")` probe to succeed.
  if [[ ! -x /usr/local/bin/mempalace-mcp ]]; then
    if [[ "$MEMPALACE" == "on" ]]; then
      echo "mempalace: /usr/local/bin/mempalace-mcp missing after install (MEMPALACE=on); aborting" >&2
      exit 1
    fi
    echo "mempalace: /usr/local/bin/mempalace-mcp missing after install; gateway will boot without mempalace" >&2
    return 0
  fi
  say "mempalace ready: /usr/local/bin/mempalace-mcp"
}

ensure_mempalace

# Notes workspace — see install-time docs for what this enables. The
# gateway also creates $NOTES_DIR on boot, but doing it here lets us
# git-init for free version history (gateway can't, since it runs under
# a sandbox that doesn't include git).
ensure_notes() {
  if [[ "$NOTES" == "off" ]]; then
    say "notes: integration disabled (NOTES=off)"
    return 0
  fi

  say "ensuring notes dir $NOTES_DIR (owned by $USER_NAME)"
  install -d -m 0750 -o "$USER_NAME" -g "$USER_NAME" "$NOTES_DIR"

  # Free version history for every edit, no extra infrastructure. The
  # MCP filesystem server only does write_file/edit_file, so a daily
  # `git add -A && git commit` cron is the simplest audit story — left
  # to the operator to wire up if desired.
  if command -v git >/dev/null 2>&1 && [[ ! -d "$NOTES_DIR/.git" ]]; then
    say "notes: git init $NOTES_DIR"
    sudo -u "$USER_NAME" git -C "$NOTES_DIR" init -q -b main || true
    sudo -u "$USER_NAME" git -C "$NOTES_DIR" config user.email "toolyard@localhost" || true
    sudo -u "$USER_NAME" git -C "$NOTES_DIR" config user.name "toolyard" || true
    sudo -u "$USER_NAME" bash -c "cd '$NOTES_DIR' && printf '# toolyard notes\n\nMarkdown scratchpad shared with toolyard agents.\nEdits land here via the `notes.*` MCP tools or a regular editor.\n' > README.md && git add README.md && git -c gpg.gpgsign=false commit -q -m 'initial' 2>/dev/null" || true
  fi

  if ! command -v npx >/dev/null 2>&1 && [[ ! -x /usr/local/bin/npx ]]; then
    if [[ "$NOTES" == "on" ]]; then
      echo "notes: npx not found and NOTES=on; aborting (install Node.js or set NOTES=off)" >&2
      exit 1
    fi
    echo "notes: npx not found; gateway will boot without notes upstream (install Node.js to enable)" >&2
    return 0
  fi
  say "notes ready: $NOTES_DIR (filesystem MCP exposes it as notes.*)"
}

ensure_notes

if [[ -n "$IMPORT_FROM" ]]; then
  if [[ -f "$IMPORT_FROM/toolyard.db" ]]; then
    say "importing existing data from $IMPORT_FROM"
    cp -a "$IMPORT_FROM/toolyard.db" "$DATA_DIR/toolyard.db"
    [[ -f "$IMPORT_FROM/session.key" ]]   && cp -a "$IMPORT_FROM/session.key"   "$DATA_DIR/session.key"
    [[ -f "$IMPORT_FROM/lake.duckdb" ]]   && cp -a "$IMPORT_FROM/lake.duckdb"   "$DATA_DIR/lake.duckdb"
    [[ -f "$IMPORT_FROM/oauth.key" ]]     && cp -a "$IMPORT_FROM/oauth.key"     "$DATA_DIR/oauth.key"
    chown -R "$USER_NAME":"$USER_NAME" "$DATA_DIR"
  else
    echo "warning: no toolyard.db at $IMPORT_FROM; skipping import" >&2
  fi
fi

# Compose the ExecStart flag string.
#
# in-line-wait defaults to 0s — every approval-required call returns a
# self-describing deferred-response envelope immediately, and agents
# coordinate via tools.poll_approval / tools.wait_for_approval. This
# scales much better than holding HTTP connections (multiple parallel
# approvals don't compete for the same request slot, nginx timeouts
# become irrelevant, restart-resilience improves). Operators with old
# MCP clients that don't understand the envelope can set IN_LINE_WAIT=
# 90s to fall back to the legacy block-and-hold mode.
EXEC_FLAGS=(
  serve
  -addr "$LISTEN_ADDR"
  -data "$DATA_DIR"
  -trusted-proxy "$TRUSTED_PROXY"
  -push-subject "$PUSH_SUBJECT"
  -in-line-wait "${IN_LINE_WAIT:-0s}"
  -mempalace "$MEMPALACE"
  -notes "$NOTES"
  -notes-dir "$NOTES_DIR"
)
[[ -n "$PUBLIC_URL" ]] && EXEC_FLAGS+=( -public-url "$PUBLIC_URL" )
[[ "$NO_STDIO_UPSTREAMS" == "true" ]] && EXEC_FLAGS+=( -no-stdio-upstreams )
[[ "$STATELESS_MCP" == "true" ]] && EXEC_FLAGS+=( -stateless-mcp )

# Sandbox tier picked by stdio upstream policy.
#
# When stdio upstreams are allowed (the default for the LAN box) we keep
# the kernel sandbox loose enough that Node, Python, and other JIT'd /
# namespace-using runtimes don't crash at startup. The hardening then
# lives at the application layer (per-route body caps, custom-header
# CSRF, content-type enforcement, rate limits) — see internal/api.
#
# When stdio upstreams are off we tighten everything: the toolyard binary
# is the only thing in the sandbox, it doesn't JIT, doesn't need
# namespaces, and never reads /proc/$PID outside its own.
if [[ "$NO_STDIO_UPSTREAMS" == "true" ]]; then
  # Strict tier — public-deployment posture.
  TIER_FLAGS="MemoryDenyWriteExecute=true
LockPersonality=true
RestrictNamespaces=true
ProtectProc=invisible
SystemCallArchitectures=native
SystemCallFilter=@system-service
SystemCallFilter=~@privileged @resources"
else
  # Loose tier — stdio upstreams compatible.
  # Dropped vs strict tier:
  #   MemoryDenyWriteExecute  (V8 JIT needs PROT_WRITE|PROT_EXEC)
  #   LockPersonality         (some Node ABI probes use personality(2))
  #   RestrictNamespaces      (npm/uv install can clone-namespace)
  #   ProtectProc=invisible   (Node libraries inspect /proc/self/fd)
  #   SystemCallFilter        (Node uses many syscalls outside @system-service)
  TIER_FLAGS="# Loose-tier kernel sandbox: defenders live at the app layer.
# Re-run with NO_STDIO_UPSTREAMS=true to harden if you drop stdio upstreams."
fi

# Render the unit file. Heredoc, not a template, so failures fail loudly.
say "writing $SERVICE_PATH"
cat > "$SERVICE_PATH" <<EOF
[Unit]
Description=toolyard MCP gateway
Documentation=https://github.com/tusharbhardwaj/toolyard
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=$USER_NAME
Group=$USER_NAME
ExecStart=$BINARY_PATH ${EXEC_FLAGS[*]}

# Operator-managed runtime env (TOOLYARD_LAKE_TOKEN, TOOLYARD_GRAFANA_ORIGIN,
# etc). The leading `-` means "ignore if the file doesn't exist" so a fresh
# install boots even before the file is written.
EnvironmentFile=-$ENV_FILE

# systemd's default PATH for services is just /usr/local/sbin:/usr/local/bin:
# /usr/sbin:/usr/bin which usually has npx and uvx, but explicit is better
# than waiting for "command not found" on the first stdio upstream.
Environment=PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/snap/bin
Environment=HOME=$DATA_DIR

# Restart on crash with backoff.
Restart=on-failure
RestartSec=5
StartLimitIntervalSec=60
StartLimitBurst=10

# systemd manages the data + log directories.
StateDirectory=toolyard
StateDirectoryMode=0700
LogsDirectory=toolyard

# Sandbox — kernel-level hygiene that's safe for any subprocess.
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
PrivateDevices=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectKernelLogs=true
ProtectControlGroups=true
ProtectClock=true
ProtectHostname=true
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX AF_NETLINK
RestrictRealtime=true
RestrictSUIDSGID=true
$TIER_FLAGS
ReadWritePaths=$DATA_DIR

# Resource caps.
LimitNOFILE=8192
TasksMax=1024
MemoryMax=1G
MemoryHigh=768M

# Graceful stop: SIGINT triggers context cancel + 5s drain in main.go.
KillSignal=SIGINT
TimeoutStopSec=10s

[Install]
WantedBy=multi-user.target
EOF

say "stopping any toolyard process not under systemd control"
# Look for a non-systemd listener on the port. systemd processes show up
# under cgroup /system.slice/toolyard.service so we can tell them apart.
if ss -H -tnlp "sport = :18787" 2>/dev/null | grep -q .; then
  pid=$(ss -H -tnlp "sport = :18787" 2>/dev/null | grep -oE 'pid=[0-9]+' | head -1 | cut -d= -f2)
  if [[ -n "${pid:-}" ]]; then
    cgrp=$(cat "/proc/$pid/cgroup" 2>/dev/null || true)
    if ! grep -q "toolyard.service" <<<"$cgrp"; then
      echo "    found non-systemd toolyard at pid=$pid; stopping it"
      kill "$pid" || true
      for i in {1..10}; do
        sleep 0.5
        ss -H -tnlp "sport = :18787" 2>/dev/null | grep -q . || break
      done
    fi
  fi
fi

say "reloading systemd + enabling toolyard.service"
systemctl daemon-reload
systemctl enable toolyard >/dev/null
systemctl restart toolyard

say "waiting for /v1/health"
for i in {1..30}; do
  if curl -sf http://127.0.0.1:18787/v1/health >/dev/null 2>&1; then
    echo "    healthy"
    break
  fi
  sleep 0.5
done

systemctl --no-pager status toolyard | head -15 || true

# Mempalace registration check. The gateway logs "mempalace: connected" or
# "mempalace: disabled" / "mempalace: binary ... not found" on boot. Parse
# the most recent boot's log to decide if the integration is live and fail
# the install when MEMPALACE=on but the upstream didn't come up.
if [[ "$MEMPALACE" != "off" ]]; then
  say "checking mempalace upstream registration in toolyard journal"
  mp_line="$(journalctl -u toolyard -n 200 --no-pager 2>/dev/null | grep -E 'mempalace:' | tail -1 || true)"
  if [[ -z "$mp_line" ]]; then
    echo "    no mempalace boot line yet; the upstream may still be connecting"
  elif echo "$mp_line" | grep -q 'connected'; then
    echo "    $mp_line"
    echo "    mempalace upstream is live; expect ~30 mempalace.* tools in /v1/tools"
  else
    echo "    $mp_line"
    if [[ "$MEMPALACE" == "on" ]]; then
      echo "mempalace: upstream did not connect (MEMPALACE=on); aborting" >&2
      exit 1
    fi
  fi
fi

if [[ "$NOTES" != "off" ]]; then
  say "checking notes upstream registration in toolyard journal"
  notes_line="$(journalctl -u toolyard -n 200 --no-pager 2>/dev/null | grep -E '^[^@]*notes:' | tail -1 || true)"
  if [[ -z "$notes_line" ]]; then
    echo "    no notes boot line yet; the upstream may still be connecting"
  elif echo "$notes_line" | grep -q 'connected'; then
    echo "    $notes_line"
    echo "    notes upstream is live; agents can use notes.read_file / write_file / list_directory etc."
  else
    echo "    $notes_line"
    if [[ "$NOTES" == "on" ]]; then
      echo "notes: upstream did not connect (NOTES=on); aborting" >&2
      exit 1
    fi
  fi
fi

# ----- ClickHouse stack ------------------------------------------------
#
# Soft-required: if docker isn't installed we warn and skip rather than
# fail the whole install. The gateway works fine without CH; this stack
# is the analytical SQL surface we'll wire Grafana onto.
ch_status="skipped"
if ! command -v docker >/dev/null 2>&1; then
  say "clickhouse: docker not found; skipping (install docker + re-run to enable)"
elif ! docker compose version >/dev/null 2>&1; then
  say "clickhouse: 'docker compose' plugin not found; skipping"
elif [[ ! -f "$CH_COMPOSE" ]]; then
  say "clickhouse: $CH_COMPOSE missing; skipping"
else
  # Ensure the shared network exists before either compose tries to
  # attach to it. `docker network create` is not idempotent so we guard
  # on inspect first; this also avoids spurious stderr noise on re-runs.
  if ! docker network inspect "$LAKE_NETWORK" >/dev/null 2>&1; then
    say "clickhouse: creating shared docker network $LAKE_NETWORK"
    docker network create "$LAKE_NETWORK" >/dev/null
  fi
  say "clickhouse: ensuring data dirs at $CH_DATA_DIR (owned $CH_UID:$CH_GID)"
  install -d -m 0750 "$CH_DATA_DIR"
  install -d -m 0750 -o "$CH_UID" -g "$CH_GID" "$CH_DATA_DIR/data"
  install -d -m 0750 -o "$CH_UID" -g "$CH_GID" "$CH_DATA_DIR/logs"

  # The CH password lives in toolyard's settings DB and is rendered into
  # $CH_ENV_FILE by the gateway during boot (see EnsureClickhousePassword
  # + WriteClickhouseRuntimeEnv). Since systemd already restarted the
  # gateway above and we waited for /v1/health, the file is guaranteed
  # to exist by the time we get here — but we double-check before
  # touching docker so a missing file fails loudly rather than mystery
  # auth errors against CH.
  if [[ ! -s "$CH_ENV_FILE" ]]; then
    echo "    expected $CH_ENV_FILE to be rendered by the toolyard gateway, but it's missing or empty."
    echo "    check 'sudo journalctl -u toolyard -n 50' for an EnsureClickhousePassword warning."
    ch_status="env-missing"
  else
    say "clickhouse: bringing up the compose stack (force-recreate so CH re-reads the env)"
    # --force-recreate: the env_file's *contents* aren't part of compose's
    # config-hash, so a rotated password would otherwise be ignored until
    # the operator manually restarted the container. Forcing the recreate
    # makes install.sh idempotently leave CH in sync with whatever
    # toolyard just wrote. Cost: ~3-4s of CH downtime per install.
    if docker compose -f "$CH_COMPOSE" up -d --force-recreate >/dev/null; then
      say "clickhouse: waiting for /ping"
      # shellcheck disable=SC1090
      source "$CH_ENV_FILE"
      ch_ok="no"
      for i in {1..40}; do
        if curl -sf -u "default:$TOOLYARD_CH_PASSWORD" \
             --data-binary 'SELECT 1' \
             http://127.0.0.1:18123/ >/dev/null 2>&1; then
          ch_ok="yes"
          break
        fi
        sleep 0.5
      done
      if [[ "$ch_ok" == "yes" ]]; then
        ver=$(curl -sf -u "default:$TOOLYARD_CH_PASSWORD" \
                --data-binary 'SELECT version()' \
                http://127.0.0.1:18123/ 2>/dev/null || echo unknown)
        echo "    clickhouse healthy (version $ver)"
        # Apply versioned mart views idempotently. CREATE OR REPLACE
        # VIEW is metadata-only, so this is safe to re-run on every
        # install. The toolyard gateway has already created the raw,
        # mart, app databases at boot (via lake.Open), so the views
        # have somewhere to live.
        mart_views="$(pwd)/deploy/clickhouse/mart-views.sql"
        if [[ -s "$mart_views" ]]; then
          if curl -sf -u "default:$TOOLYARD_CH_PASSWORD" \
               --data-binary "@$mart_views" \
               'http://127.0.0.1:18123/?multi_statements=1' >/dev/null 2>&1; then
            echo "    mart views applied from $mart_views"
          else
            echo "    warning: failed to apply mart views from $mart_views — apply manually if dashboards need them"
          fi
        fi
        ch_status="up"
      else
        echo "    clickhouse did not respond on 127.0.0.1:18123 within 20s — check 'docker logs toolyard-clickhouse'"
        ch_status="degraded"
      fi
    else
      echo "    docker compose up failed — check 'docker compose -f $CH_COMPOSE logs'"
      ch_status="failed"
    fi
  fi
fi

# Restart toolyard once more so the gateway opens the lake against the
# now-running CH. On a fresh install (or when toolyard minted a new CH
# password on this boot), the first systemctl restart above happened
# *before* the CH compose recreate, so the gateway's lake.Open() saw
# the old/missing password and disabled lake.* tools. Replaying the
# restart after CH is healthy lets the gateway re-attempt with the
# current password and register /v1/lake/* + lake.* MCP tools.
#
# Idempotent: when the password was already in sync (subsequent
# re-installs), this is a no-op-shaped restart that costs ~2s. Worth it
# to guarantee the lake is wired up by the time install.sh exits.
if [[ "$ch_status" == "up" ]]; then
  say "restarting toolyard so it re-opens the lake against the running CH"
  systemctl restart toolyard
  for i in {1..30}; do
    if curl -sf http://127.0.0.1:18787/v1/health >/dev/null 2>&1; then
      break
    fi
    sleep 0.5
  done
fi

# Detect first-run vs. existing setup.
need_setup="no"
if curl -sf http://127.0.0.1:18787/v1/auth/me 2>/dev/null | grep -q '"setup_required":true'; then
  need_setup="yes"
fi

cat <<EOF

────────────────────────────────────────────────────────────────────────
toolyard is up.

  status      : sudo systemctl status toolyard
  logs        : sudo journalctl -u toolyard -f
  edit flags  : sudo systemctl edit --full toolyard && sudo systemctl restart toolyard
  upgrade     : pull, then re-run this script

  binary      : $BINARY_PATH
  data        : $DATA_DIR (toolyard.db, session.key, server keys)
  unit        : $SERVICE_PATH
  bind        : $LISTEN_ADDR

  clickhouse  : $ch_status
    compose   : $CH_COMPOSE
    data      : $CH_DATA_DIR/data
    logs      : $CH_DATA_DIR/logs (also: docker logs toolyard-clickhouse)
    bind      : 127.0.0.1:18123 (HTTP), 127.0.0.1:19000 (native)
    password  : managed by toolyard (settings key 'clickhouse_password')
                rendered to $CH_ENV_FILE for the compose stack
                reveal/rotate from the dashboard's Settings tab

  reminders:
    - RESTART your MCP agents (Claude Code, etc.) after this. Their MCP
      streamable-http sessions don't survive a toolyard restart and most
      clients don't auto-reconnect on session-invalid.
    - iPhone PWA: if dashboard buttons look stale, long-press the Home
      Screen icon → Delete App, then Safari → toolyard URL → Share → Add
      to Home Screen again. iOS caches the PWA shell aggressively.
EOF

if [[ "$need_setup" == "yes" ]]; then
cat <<'EOF'

  first-time setup (loopback-only — must run from this server):
    curl -X POST http://127.0.0.1:18787/v1/auth/setup \
         -H 'Content-Type: application/json' \
         -d '{"Username":"<you>","Password":"<a long passphrase>"}'
EOF
else
cat <<'EOF'

  setup already complete; log in at the dashboard.
EOF
fi

cat <<EOF
────────────────────────────────────────────────────────────────────────
EOF
