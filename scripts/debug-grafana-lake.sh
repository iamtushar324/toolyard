#!/usr/bin/env bash
# debug-grafana-lake.sh — simulate the Grafana Infinity plugin hitting
# the toolyard /v1/lake/exec endpoint and show what comes back.
#
# Run with sudo so we can read /var/lib/toolyard/grafana-runtime.env
# (mode 0640, owned by toolyard) and also exec into the toolyard-grafana
# container to repro from Grafana's actual network context.
#
# Usage:
#   sudo scripts/debug-grafana-lake.sh
#
# Output is verbose on purpose — paste the whole thing back.

set -u
set -o pipefail

GREEN=$'\033[32m'; RED=$'\033[31m'; YELLOW=$'\033[33m'; BOLD=$'\033[1m'; RST=$'\033[0m'

section() { printf '\n%s==> %s%s\n' "$BOLD" "$*" "$RST"; }
ok()      { printf '%s[ok]%s %s\n'    "$GREEN" "$RST" "$*"; }
warn()    { printf '%s[warn]%s %s\n'  "$YELLOW" "$RST" "$*"; }
fail()    { printf '%s[fail]%s %s\n'  "$RED"   "$RST" "$*"; }

if [[ $EUID -ne 0 ]]; then
  fail "must be run as root (sudo) so we can read /var/lib/toolyard/grafana-runtime.env"
  exit 1
fi

section "1) toolyard-managed env file"
RUNTIME_ENV=/var/lib/toolyard/grafana-runtime.env
if [[ ! -f "$RUNTIME_ENV" ]]; then
  fail "$RUNTIME_ENV missing — gateway hasn't bootstrapped the lake token yet"
  exit 1
fi
ls -la "$RUNTIME_ENV"
# Print the file but redact the actual token value, so we can confirm the
# variable is set without leaking the secret to logs.
sed 's/\(TOOLYARD_LAKE_TOKEN=\).*/\1<redacted-len=&>/' "$RUNTIME_ENV" \
  | awk -F= '/TOOLYARD_LAKE_TOKEN/{print $1"=<redacted-len="length($2)">"} !/TOOLYARD_LAKE_TOKEN/{print}'

# shellcheck disable=SC1090
source "$RUNTIME_ENV"
if [[ -z "${TOOLYARD_LAKE_TOKEN:-}" ]]; then
  fail "TOOLYARD_LAKE_TOKEN not set after sourcing $RUNTIME_ENV"
  exit 1
fi
ok "lake token loaded (${#TOOLYARD_LAKE_TOKEN} chars)"

section "2) what does the running grafana container actually see?"
if docker inspect toolyard-grafana >/dev/null 2>&1; then
  GRAFANA_TOKEN_LEN=$(docker exec toolyard-grafana sh -c 'printf %s "$TOOLYARD_LAKE_TOKEN" | wc -c' 2>/dev/null || echo "?")
  echo "TOOLYARD_LAKE_TOKEN length inside container: $GRAFANA_TOKEN_LEN"
  if [[ "$GRAFANA_TOKEN_LEN" != "${#TOOLYARD_LAKE_TOKEN}" ]]; then
    warn "container token length differs from on-disk token — grafana may need a restart to pick up the rotated env_file"
  else
    ok "container token length matches on-disk"
  fi
else
  warn "toolyard-grafana container not found"
fi

QUERY='SELECT 1 AS one'
BODY=$(printf '{"sql":%s,"max_rows":5}' "$(printf '%s' "$QUERY" | python3 -c 'import json,sys; print(json.dumps(sys.stdin.read()))')")

section "3) curl from the host directly to the gateway (loopback)"
echo "POST http://127.0.0.1:18787/v1/lake/exec"
echo "body: $BODY"
curl -sS -i \
  -H "Authorization: Bearer $TOOLYARD_LAKE_TOKEN" \
  -H "Content-Type: application/json" \
  -H "User-Agent: Grafana/12.4.1 (yesoreyeram-infinity-datasource)" \
  --data-binary "$BODY" \
  "http://127.0.0.1:18787/v1/lake/exec" || fail "curl failed"
echo

section "4) curl from inside the toolyard-grafana container (the real path)"
if docker inspect toolyard-grafana >/dev/null 2>&1; then
  # Grafana's image is alpine-based and ships wget but not curl. Exec
  # wget so we replicate exactly what the container can do, then fall
  # back to installing curl on the fly via apk if wget refuses.
  docker exec -e BEARER="$TOOLYARD_LAKE_TOKEN" -e BODY="$BODY" toolyard-grafana sh -c '
    set -e
    URL="http://host.docker.internal:18787/v1/lake/exec"
    echo "POST $URL"
    if command -v curl >/dev/null 2>&1; then
      curl -sS -i \
        -H "Authorization: Bearer $BEARER" \
        -H "Content-Type: application/json" \
        --data-binary "$BODY" \
        "$URL"
    else
      # wget -S writes response headers to stderr; merge for one stream.
      wget -qO- -S \
        --header="Authorization: Bearer $BEARER" \
        --header="Content-Type: application/json" \
        --post-data="$BODY" \
        "$URL" 2>&1
    fi
  ' || fail "docker exec curl failed"
else
  warn "toolyard-grafana container not running, skipping"
fi
echo

section "5) host.docker.internal resolution from inside the container"
if docker inspect toolyard-grafana >/dev/null 2>&1; then
  docker exec toolyard-grafana sh -c '
    getent hosts host.docker.internal || nslookup host.docker.internal 2>&1 || true
    echo "---"
    # Quick TCP reachability test
    (timeout 3 sh -c "</dev/tcp/host.docker.internal/18787" && echo "tcp:18787 reachable" ) 2>&1 || echo "tcp:18787 NOT reachable"
  '
fi
echo

section "6) recent gateway logs around lake/exec"
journalctl -u toolyard -n 80 --no-pager | grep -E 'lake|exec|unauthorized|401|403|panic|error' || echo "(no matching lines in last 80)"
echo

section "7) recent grafana container logs"
docker logs --tail 60 toolyard-grafana 2>&1 | grep -iE 'infinity|lake|toolyard|error|datasource' || echo "(no matching lines)"
echo

section "done"
echo "If step 3 returns 200 but step 4 fails, the gateway is fine and the"
echo "issue is grafana → host networking (host.docker.internal / firewall)."
echo "If both fail with 401, the token in the container env is stale — restart grafana:"
echo "  sudo docker compose -f deploy/grafana/docker-compose.yaml restart grafana"
