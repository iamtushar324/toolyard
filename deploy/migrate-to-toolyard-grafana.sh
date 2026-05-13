#!/usr/bin/env bash
# Migrate the local Grafana stack from nova-grafana onto toolyard-grafana.
#
# What it does, in order:
#   1. Rebuilds & reinstalls toolyard via deploy/install.sh
#       (idempotent — picks up new settings/UI code, regenerates systemd
#       unit with the --grafana-runtime-env flag, restarts the gateway).
#   2. Waits for the gateway to be healthy on 18787.
#   3. Verifies /var/lib/toolyard/grafana-runtime.env was written by the
#       gateway's first-boot lake-API-token bootstrap.
#   4. Stops the running nova-grafana container so port 3030 is free.
#   5. Regenerates dashboard JSONs from the current lake schema so any
#       column renames the migration agent shipped get reflected.
#   6. Brings up the toolyard-grafana compose stack.
#   7. Waits for Grafana to be healthy on 3030.
#   8. Smoke-tests the Toolyard Lake datasource by POSTing a SELECT 1
#       through it via Grafana's /api/datasources/proxy.
#   9. Prints a summary with the token-reveal URL and next steps.
#
# Idempotent — safe to re-run. Each step skips work that's already done
# (e.g. nova-grafana already stopped, toolyard-grafana already up).
#
# Usage:
#   sudo ./deploy/migrate-to-toolyard-grafana.sh                # interactive, prompts before destructive steps
#   sudo ./deploy/migrate-to-toolyard-grafana.sh --yes          # non-interactive, assumes yes
#   sudo ./deploy/migrate-to-toolyard-grafana.sh --skip-install # skip step 1 (gateway already on new code)
#
# Run from the repo root.

set -euo pipefail

# ------- config ------------------------------------------------------
REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GRAFANA_COMPOSE="$REPO_DIR/deploy/grafana/docker-compose.yaml"
NOVA_COMPOSE="/home/tusharbhardwaj/.nova/main_agent_workspace/repo/nova/docker-compose.yaml"
RUNTIME_ENV="/var/lib/toolyard/grafana-runtime.env"
TOOLYARD_HEALTH="http://192.168.1.179:18787/v1/health"
GRAFANA_HEALTH="http://localhost:3030/api/health"
LOG_FILE="/var/log/toolyard/migrate-to-toolyard-grafana-$(date +%Y%m%d-%H%M%S).log"

ASSUME_YES=0
SKIP_INSTALL=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --yes|-y)         ASSUME_YES=1; shift ;;
    --skip-install)   SKIP_INSTALL=1; shift ;;
    -h|--help)        sed -n '2,32p' "$0"; exit 0 ;;
    *) echo "unknown flag: $1" >&2; exit 1 ;;
  esac
done

# ------- guards ------------------------------------------------------
if [[ $EUID -ne 0 ]]; then
  echo "must run as root (use sudo)" >&2
  exit 1
fi
if [[ ! -f "$REPO_DIR/cmd/gateway/main.go" ]]; then
  echo "could not locate the toolyard repo at $REPO_DIR" >&2
  exit 1
fi
if ! command -v docker >/dev/null 2>&1; then
  echo "docker is required" >&2
  exit 1
fi
if ! command -v curl >/dev/null 2>&1; then
  echo "curl is required" >&2
  exit 1
fi

# ------- logging -----------------------------------------------------
mkdir -p "$(dirname "$LOG_FILE")"
# Tee everything (stdout + stderr) to the log so a future debug session
# can replay what happened. The console still gets coloured output.
exec > >(tee -a "$LOG_FILE") 2>&1

say() { printf '\033[1;32m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m!!\033[0m %s\n' "$*"; }
die() { printf '\033[1;31m!!\033[0m %s\n' "$*"; exit 1; }
confirm() {
  local prompt="$1"
  if [[ $ASSUME_YES -eq 1 ]]; then return 0; fi
  read -r -p "$prompt [y/N] " resp
  [[ "$resp" =~ ^[Yy]$ ]]
}

wait_for_url() {
  # Polls a URL until it returns 2xx, or fails after timeout seconds.
  # Locals declared on separate lines because bash evaluates the
  # arithmetic on the right-hand side of a multi-var `local` before the
  # earlier names in the same statement are bound — under `set -u`
  # that trips on an "unbound variable" for the second arg.
  local url="$1"
  local timeout="${2:-60}"
  local deadline=$(( $(date +%s) + timeout ))
  while (( $(date +%s) < deadline )); do
    if curl -fsS -m 3 -o /dev/null "$url"; then return 0; fi
    sleep 1
  done
  return 1
}

say "logging to $LOG_FILE"

# ------- step 1: rebuild + reinstall toolyard ------------------------
if [[ $SKIP_INSTALL -eq 1 ]]; then
  say "[step 1/8] skipping toolyard rebuild (--skip-install)"
else
  say "[step 1/8] rebuilding & reinstalling toolyard (deploy/install.sh)"
  # install.sh is itself idempotent — runs go build, installs the
  # binary, regenerates the systemd unit, restarts the service.
  ( cd "$REPO_DIR" && bash ./deploy/install.sh )
fi

# ------- step 2: wait for gateway health -----------------------------
say "[step 2/8] waiting for toolyard gateway to be healthy at $TOOLYARD_HEALTH"
if ! wait_for_url "$TOOLYARD_HEALTH" 60; then
  die "toolyard gateway did not return healthy within 60s — check 'journalctl -u toolyard'"
fi
say "    gateway healthy"

# ------- step 3: verify runtime env file -----------------------------
say "[step 3/8] verifying $RUNTIME_ENV exists (written by first-boot bootstrap)"
if [[ ! -s "$RUNTIME_ENV" ]]; then
  warn "$RUNTIME_ENV is missing or empty"
  warn "the gateway should write this on start; check that --grafana-runtime-env"
  warn "is set in /etc/systemd/system/toolyard.service ExecStart, then 'systemctl restart toolyard'."
  die "cannot proceed without the runtime env file"
fi
if ! grep -q '^TOOLYARD_LAKE_TOKEN=' "$RUNTIME_ENV"; then
  die "$RUNTIME_ENV does not contain TOOLYARD_LAKE_TOKEN — check gateway logs"
fi
say "    runtime env file looks good"

# ------- step 4: stop nova-grafana -----------------------------------
say "[step 4/8] checking for a running nova-grafana container"
NOVA_GRAFANA_RUNNING=0
if docker ps --format '{{.Names}}' | grep -qx 'nova-grafana-1'; then
  NOVA_GRAFANA_RUNNING=1
fi
if [[ $NOVA_GRAFANA_RUNNING -eq 1 ]]; then
  if ! confirm "stop nova-grafana-1 to free port 3030?"; then
    die "user declined to stop nova-grafana — aborting"
  fi
  say "    stopping nova-grafana-1"
  # Plain `docker stop` instead of `docker compose stop`: the nova
  # compose file references env vars (NOVA_WORKSPACE, NOVA_CONFIG_HOME)
  # that aren't in the sudo environment, which makes compose fail
  # validation of the whole stack with "invalid spec: ::z" before
  # ever running the stop command. We only need the container down,
  # not compose-level state, so the lower-level call is fine.
  # `unless-stopped` restart policy means the daemon won't auto-revive
  # a manually-stopped container.
  docker stop nova-grafana-1
else
  say "    nova-grafana-1 is not running — nothing to stop"
fi

# ------- step 5: regenerate dashboard JSONs --------------------------
say "[step 5/8] regenerating dashboard JSONs from the current lake schema"
if [[ -x "$REPO_DIR/deploy/grafana/build_dashboards.py" ]] || [[ -f "$REPO_DIR/deploy/grafana/build_dashboards.py" ]]; then
  # Run as the invoking user so /tmp + caches stay in their home.
  sudo -u "${SUDO_USER:-root}" python3 "$REPO_DIR/deploy/grafana/build_dashboards.py"
else
  warn "build_dashboards.py not found; using existing JSONs as-is"
fi

# ------- step 6: bring up toolyard-grafana ---------------------------
say "[step 6/8] bringing up toolyard-grafana"
if [[ ! -f "$GRAFANA_COMPOSE" ]]; then
  die "compose file missing: $GRAFANA_COMPOSE"
fi
docker compose -f "$GRAFANA_COMPOSE" up -d --remove-orphans

# ------- step 7: wait for Grafana health -----------------------------
say "[step 7/8] waiting for Grafana to be healthy at $GRAFANA_HEALTH"
if ! wait_for_url "$GRAFANA_HEALTH" 90; then
  warn "Grafana did not return healthy within 90s — checking container logs:"
  docker compose -f "$GRAFANA_COMPOSE" logs --tail=80 grafana
  die "Grafana never came up — fix the underlying issue and re-run"
fi
say "    Grafana healthy"

# ------- step 8: smoke-test the lake datasource ----------------------
say "[step 8/8] smoke-testing Toolyard Lake datasource via Grafana proxy"
# Resolve the Grafana datasource numeric ID (the proxy URL needs it).
ADMIN_USER="${GF_SECURITY_ADMIN_USER:-admin}"
ADMIN_PASS="${GF_SECURITY_ADMIN_PASSWORD:-nova}"
# Try to read the configured admin password from the env file the
# operator may have edited; fall back to defaults if absent.
if [[ -r /etc/toolyard/toolyard.env ]]; then
  while IFS='=' read -r k v; do
    case "$k" in
      GF_SECURITY_ADMIN_USER) ADMIN_USER="$v" ;;
      GF_SECURITY_ADMIN_PASSWORD) ADMIN_PASS="$v" ;;
    esac
  done < <(grep -E '^(GF_SECURITY_ADMIN_USER|GF_SECURITY_ADMIN_PASSWORD)=' /etc/toolyard/toolyard.env || true)
fi
DS_INFO=$(curl -fsS -u "$ADMIN_USER:$ADMIN_PASS" \
  "http://localhost:3030/api/datasources/uid/toolyard-lake" 2>/dev/null || echo '{}')
DS_ID=$(printf '%s' "$DS_INFO" | python3 -c "import sys,json; print(json.loads(sys.stdin.read() or '{}').get('id',''))")
if [[ -z "$DS_ID" ]]; then
  warn "could not resolve datasource toolyard-lake (admin auth may be wrong)"
  warn "the dashboards are still installed — log in to Grafana and verify by hand"
else
  # Hit the lake through the Grafana proxy. We expect 200 + a payload
  # containing 'columns' and 'rows' if the Bearer token from the
  # runtime env file is being applied correctly.
  PROXY_URL="http://localhost:3030/api/datasources/proxy/$DS_ID/v1/lake/exec"
  RESP=$(curl -sS -u "$ADMIN_USER:$ADMIN_PASS" -X POST "$PROXY_URL" \
    -H 'Content-Type: application/json' \
    -d '{"sql":"SELECT 1 AS ok","max_rows":1}' || true)
  if printf '%s' "$RESP" | grep -q '"rows"'; then
    say "    datasource working — proxy returned a {columns,rows} payload"
  else
    warn "datasource probe did not return rows. Response was:"
    printf '    %s\n' "$RESP"
    warn "verify the Bearer token in $RUNTIME_ENV matches the lake_api_token in the toolyard settings DB."
  fi
fi

# ------- summary -----------------------------------------------------
echo
say "migration complete"
cat <<'EOF'

  Toolyard:    http://192.168.1.179:18787/         (dashboard, login if not already)
  Grafana:     http://localhost:3030/              (admin / GF_SECURITY_ADMIN_PASSWORD)
  Dashboards:  Toolyard folder → "Finance Overview (lake)" / "Investment Portfolio (lake)"
  Spending:    stub dashboard pinned to the lake migration of fin_transactions

  Next steps the operator should consider:
    * Visit the toolyard dashboard's Settings tab → "Lake & Grafana integration"
      and set Grafana Origin to http://localhost:3030 if you want to iframe
      panels back into /lake/.
    * If the admin password is still the default "nova", set
      GF_SECURITY_ADMIN_PASSWORD in /etc/toolyard/toolyard.env, then
      'sudo docker compose -f deploy/grafana/docker-compose.yaml up -d'
      to re-apply it.
    * Rotate the lake API token from the dashboard at any time; the runtime
      env file is re-rendered automatically. Restart Grafana with
      'sudo docker compose -f deploy/grafana/docker-compose.yaml restart grafana'
      to pick up the new value.
EOF
