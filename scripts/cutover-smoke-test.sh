#!/usr/bin/env bash
# cutover-smoke-test.sh — bring up the new CH+Grafana stack on this host
# and run every check Claude needs to validate Phase 2/4.
#
# Run as root (or via sudo) from the repo root:
#   sudo bash scripts/cutover-smoke-test.sh 2>&1 | tee /tmp/cutover-smoke.log
#
# Re-runnable; will recreate containers and the docker network but won't
# wipe CH data. Safe against an already-deployed toolyard.

set -uo pipefail   # NOT -e: we want to keep going on individual failures
                   # so the final summary captures every check's status.

REPO="$(cd "$(dirname "$0")/.." && pwd)"
CH_COMPOSE="$REPO/deploy/clickhouse/docker-compose.yaml"
GF_COMPOSE="$REPO/deploy/grafana/docker-compose.yaml"
CH_ENV_FILE="/var/lib/toolyard/clickhouse-runtime.env"
LAKE_NETWORK="toolyard_lake_net"

# Pretty-print step headers so the final paste is easy for Claude to grep.
step() { printf '\n\033[1;36m=== %s ===\033[0m\n' "$*"; }
ok()   { printf '\033[1;32mOK\033[0m %s\n' "$*"; }
fail() { printf '\033[1;31mFAIL\033[0m %s\n' "$*"; }

if [[ $EUID -ne 0 ]]; then
  fail "this script must run as root (try: sudo bash $0)"
  exit 1
fi

step "0. environment"
echo "repo:      $REPO"
echo "host:      $(hostname)"
echo "user:      $(whoami) (uid $EUID)"
echo "date:      $(date -Iseconds)"
docker --version
docker compose version
go version 2>&1 || echo "(go missing — lake tests will be skipped)"

step "1. ensure CH runtime env exists"
# Normally the toolyard gateway renders this file at boot (api.WriteClickhouseRuntimeEnv).
# If it's missing (e.g. the running binary predates that code, or this is a
# fresh smoke-test host), auto-seed a throwaway password so the rest of
# the script can proceed. The seeded password is a one-shot — if the
# gateway later renders its own value, just `docker compose ... up -d
# --force-recreate` to pick the new one up.
if [[ ! -s "$CH_ENV_FILE" ]]; then
  echo "WARN  $CH_ENV_FILE missing — seeding a throwaway password."
  echo "      (the toolyard gateway normally renders this on boot; this"
  echo "       smoke-test seed is independent and may diverge from what"
  echo "       the gateway would have written. Rotate from the dashboard"
  echo "       Settings tab once the gateway is reachable to converge.)"
  if ! id toolyard >/dev/null 2>&1; then
    fail "system user 'toolyard' does not exist — has deploy/install.sh ever run on this host?"
    exit 1
  fi
  install -d -m 0700 -o toolyard -g toolyard "$(dirname "$CH_ENV_FILE")"
  # Generate the password and pipe through `install` to land the file
  # with the right mode/owner atomically, without `chmod` having a window
  # where the contents are world-readable.
  if command -v openssl >/dev/null 2>&1; then
    pw="$(openssl rand -hex 24)"
  else
    # Fallback if openssl isn't around for some reason.
    pw="$(head -c 48 /dev/urandom | od -An -tx1 | tr -d ' \n' | head -c 48)"
  fi
  umask 077
  tmp="$(mktemp)"
  printf '# managed by cutover-smoke-test.sh — auto-seeded throwaway.\nTOOLYARD_CH_PASSWORD=%s\n' "$pw" >"$tmp"
  install -m 0600 -o toolyard -g toolyard "$tmp" "$CH_ENV_FILE"
  rm -f "$tmp"
  ok "seeded $CH_ENV_FILE"
fi
echo "ls -la $CH_ENV_FILE:"
ls -la "$CH_ENV_FILE"
# Pull password without printing it.
# shellcheck disable=SC1090
source "$CH_ENV_FILE"
if [[ -z "${TOOLYARD_CH_PASSWORD:-}" ]]; then
  fail "TOOLYARD_CH_PASSWORD not set in $CH_ENV_FILE"
  exit 1
fi
echo "TOOLYARD_CH_PASSWORD: <redacted, ${#TOOLYARD_CH_PASSWORD} chars>"
ok "env file present"

step "2. create shared docker network (idempotent)"
if docker network inspect "$LAKE_NETWORK" >/dev/null 2>&1; then
  echo "$LAKE_NETWORK already exists"
else
  docker network create "$LAKE_NETWORK"
fi
docker network inspect "$LAKE_NETWORK" --format 'id={{.Id}} driver={{.Driver}} scope={{.Scope}}'
ok "network ready"

step "3. (re)bring up CH compose"
echo "compose file: $CH_COMPOSE"
docker compose -f "$CH_COMPOSE" up -d --force-recreate
echo
docker compose -f "$CH_COMPOSE" ps

step "4. wait for CH /ping"
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
  ver=$(curl -sf -u "default:$TOOLYARD_CH_PASSWORD" --data-binary 'SELECT version()' http://127.0.0.1:18123/)
  ok "CH responsive (version=$ver)"
else
  fail "CH did not respond on 127.0.0.1:18123 within 20s"
  echo "--- last 40 lines of docker logs toolyard-clickhouse ---"
  docker logs --tail 40 toolyard-clickhouse 2>&1 || true
  exit 1
fi

step "5. ensure standard databases exist (raw, mart, app)"
# Normally lake.Open() creates these on gateway boot, but until Phase 3
# wires the gateway to CH the databases won't exist on a fresh host.
# Create them idempotently so the provisioned datasource has somewhere
# to land. Safe to repeat; CH treats CREATE DATABASE IF NOT EXISTS as a
# no-op when the database is already present.
for db in raw mart app; do
  curl -sf -u "default:$TOOLYARD_CH_PASSWORD" \
    --data-binary "CREATE DATABASE IF NOT EXISTS \`$db\`" \
    http://127.0.0.1:18123/ && echo "  $db ready"
done
echo
echo "current databases:"
curl -sf -u "default:$TOOLYARD_CH_PASSWORD" \
  --data-binary "SELECT name FROM system.databases ORDER BY name FORMAT CSV" \
  http://127.0.0.1:18123/

step "6. CH table inventory per database"
curl -sf -u "default:$TOOLYARD_CH_PASSWORD" \
  --data-binary "SELECT database, count() AS n, sum(total_rows) AS rows FROM system.tables WHERE database IN ('raw','mart','app') GROUP BY database ORDER BY database FORMAT TSVWithNames" \
  http://127.0.0.1:18123/

step "7. dashboards' tables exist?"
for t in mart.fact_net_worth_snapshot mart.dim_investment mart.dim_account \
         mart.fact_investment_valuation mart.fact_account_balance \
         mart.fact_recurring_schedule mart.dim_physical_asset \
         mart.fact_credit_card_bill mart.fact_deployment_plan; do
  res=$(curl -sf -u "default:$TOOLYARD_CH_PASSWORD" \
    --data-binary "EXISTS TABLE ${t}" http://127.0.0.1:18123/ 2>&1)
  printf '  %-40s exists=%s\n' "$t" "$res"
done

step "8. go test ./internal/lake/... against this CH"
if command -v go >/dev/null 2>&1; then
  cd "$REPO"
  # GOTOOLCHAIN=auto so an older system Go (e.g. 1.23) transparently
  # fetches the toolchain version pinned in go.mod. Mirrors what
  # deploy/install.sh does for the gateway build. The test step needs
  # network egress to proxy.golang.org for the toolchain fetch on the
  # first run; cached locally afterwards.
  GOTOOLCHAIN=auto \
  GOSUMDB=sum.golang.org \
  GOPROXY="https://proxy.golang.org,direct" \
  TOOLYARD_CH_TEST_ADDR=127.0.0.1:19000 \
  TOOLYARD_CH_TEST_USER=default \
  TOOLYARD_CH_TEST_PASSWORD="$TOOLYARD_CH_PASSWORD" \
    go test -count=1 -v ./internal/lake/... 2>&1 | tail -80
else
  echo "(go missing; skipped)"
fi

step "9. (re)bring up Grafana compose"
echo "compose file: $GF_COMPOSE"
docker compose -f "$GF_COMPOSE" down >/dev/null 2>&1 || true
docker compose -f "$GF_COMPOSE" up -d
echo
docker compose -f "$GF_COMPOSE" ps

step "10. wait for Grafana /api/health"
gf_ok="no"
for i in {1..60}; do
  if curl -sf http://127.0.0.1:3030/api/health >/dev/null 2>&1; then
    gf_ok="yes"
    break
  fi
  sleep 1
done
if [[ "$gf_ok" == "yes" ]]; then
  curl -s http://127.0.0.1:3030/api/health
  echo
  ok "Grafana healthy"
else
  fail "Grafana did not come up within 60s"
  docker logs --tail 60 toolyard-grafana 2>&1 || true
  exit 1
fi

step "11. confirm grafana-clickhouse-datasource plugin installed"
docker exec toolyard-grafana ls /var/lib/grafana/plugins/ 2>&1 | head -20

step "12. probe Grafana datasource health (proxy through admin API)"
# Pull admin creds from /etc/toolyard/toolyard.env via the container's env.
# If those aren't set, the API call will 401 — that's still a useful signal.
GF_USER=$(docker exec toolyard-grafana sh -c 'echo $GF_SECURITY_ADMIN_USER' 2>/dev/null | tr -d '\r')
GF_PASS=$(docker exec toolyard-grafana sh -c 'echo $GF_SECURITY_ADMIN_PASSWORD' 2>/dev/null | tr -d '\r')
if [[ -n "$GF_USER" && -n "$GF_PASS" ]]; then
  echo "datasource list:"
  curl -sf -u "$GF_USER:$GF_PASS" http://127.0.0.1:3030/api/datasources \
    | python3 -c 'import sys,json; [print(f"  uid={d[\"uid\"]:20} type={d[\"type\"]:35} name={d[\"name\"]}") for d in json.load(sys.stdin)]' 2>&1
  echo
  echo "Toolyard Lake (uid=toolyard-lake) health check:"
  curl -s -u "$GF_USER:$GF_PASS" -X POST -H 'Content-Type: application/json' \
    -d '{"queries":[{"refId":"A","datasource":{"uid":"toolyard-lake"},"rawSql":"SELECT 1","format":1,"queryType":"sql","editorType":"sql"}]}' \
    http://127.0.0.1:3030/api/ds/query | head -c 1000
  echo
else
  echo "(GF admin creds not set in container env; skipping API probe — datasource health is visible in the UI at http://127.0.0.1:3030/connections/datasources)"
fi

step "13. dashboard files mounted in container"
docker exec toolyard-grafana ls /var/lib/grafana/dashboards/ 2>&1

step "14. summary"
echo "CH stack:        $(docker inspect -f '{{.State.Status}}' toolyard-clickhouse 2>/dev/null || echo absent)"
echo "Grafana stack:   $(docker inspect -f '{{.State.Status}}' toolyard-grafana 2>/dev/null || echo absent)"
echo "Network:         $(docker network inspect $LAKE_NETWORK --format '{{len .Containers}} containers attached' 2>/dev/null)"
echo
echo "Grafana UI:      http://127.0.0.1:3030"
echo "                 (or http://192.168.1.179:3030 from another host)"
echo
echo "Next manual step: open Grafana → Dashboards → 'Finance Overview (lake)'"
echo "                  and confirm at least the first panel renders."
echo
echo "Log saved if you ran with tee:  /tmp/cutover-smoke.log"
