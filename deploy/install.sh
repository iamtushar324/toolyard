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
#   --import-from <dir>      copy existing toolyard.db + session.key from <dir>.
#
# Run from the repo root.

set -euo pipefail

BINARY_PATH="/usr/local/bin/toolyard"
SERVICE_PATH="/etc/systemd/system/toolyard.service"
DATA_DIR="/var/lib/toolyard"
USER_NAME="toolyard"
LISTEN_ADDR="0.0.0.0:18787"

PUBLIC_URL="${PUBLIC_URL:-}"
PUSH_SUBJECT="${PUSH_SUBJECT:-mailto:admin@example.invalid}"
TRUSTED_PROXY="${TRUSTED_PROXY:-127.0.0.1/32,::1/128,100.64.0.0/10,10.0.0.0/8,192.168.0.0/16,172.16.0.0/12}"
NO_STDIO_UPSTREAMS="${NO_STDIO_UPSTREAMS:-false}"
IMPORT_FROM=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --import-from)        IMPORT_FROM="$2"; shift 2 ;;
    --public-url)         PUBLIC_URL="$2"; shift 2 ;;
    --no-stdio-upstreams) NO_STDIO_UPSTREAMS=true; shift ;;
    -h|--help)            sed -n '2,40p' "$0"; exit 0 ;;
    *) echo "unknown flag: $1" >&2; exit 1 ;;
  esac
done

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

if [[ -n "$IMPORT_FROM" ]]; then
  if [[ -f "$IMPORT_FROM/toolyard.db" ]]; then
    say "importing existing data from $IMPORT_FROM"
    cp -a "$IMPORT_FROM/toolyard.db" "$DATA_DIR/toolyard.db"
    [[ -f "$IMPORT_FROM/session.key" ]] && cp -a "$IMPORT_FROM/session.key" "$DATA_DIR/session.key"
    chown -R "$USER_NAME":"$USER_NAME" "$DATA_DIR"
  else
    echo "warning: no toolyard.db at $IMPORT_FROM; skipping import" >&2
  fi
fi

# Compose the ExecStart flag string.
EXEC_FLAGS=(
  serve
  -addr "$LISTEN_ADDR"
  -data "$DATA_DIR"
  -trusted-proxy "$TRUSTED_PROXY"
  -push-subject "$PUSH_SUBJECT"
)
[[ -n "$PUBLIC_URL" ]] && EXEC_FLAGS+=( -public-url "$PUBLIC_URL" )
[[ "$NO_STDIO_UPSTREAMS" == "true" ]] && EXEC_FLAGS+=( -no-stdio-upstreams )

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

# Restart on crash with backoff.
Restart=on-failure
RestartSec=5
StartLimitIntervalSec=60
StartLimitBurst=10

# systemd manages the data + log directories.
StateDirectory=toolyard
StateDirectoryMode=0700
LogsDirectory=toolyard

# Sandbox.
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
ProtectProc=invisible
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX AF_NETLINK
RestrictNamespaces=true
RestrictRealtime=true
RestrictSUIDSGID=true
LockPersonality=true
MemoryDenyWriteExecute=true
SystemCallArchitectures=native
SystemCallFilter=@system-service
SystemCallFilter=~@privileged @resources
ReadWritePaths=$DATA_DIR

# Resource caps.
LimitNOFILE=8192
TasksMax=512
MemoryMax=512M

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
