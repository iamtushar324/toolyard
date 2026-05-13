#!/usr/bin/env bash
# debug-toolyard.sh — one script for the build / deploy / observe loop
# while we hunt down the hangs.
#
# Usage:
#   scripts/debug-toolyard.sh deploy   # build, install, restart, show health
#   scripts/debug-toolyard.sh health   # one-shot /v1/health
#   scripts/debug-toolyard.sh watch    # tail journal for slow-call/timeout lines
#   scripts/debug-toolyard.sh dump     # send SIGUSR1, then print recent journal
#   scripts/debug-toolyard.sh pprof    # capture goroutine profile to /tmp
#   scripts/debug-toolyard.sh poll     # 1Hz health snapshot until Ctrl-C
#   scripts/debug-toolyard.sh all      # deploy + watch
#
# Env overrides (defaults in []):
#   TOOLYARD_BIN_SRC   [./cmd/gateway]    go build target
#   TOOLYARD_BIN_DEST  [/usr/local/bin/toolyard]
#   TOOLYARD_UNIT      [toolyard]         systemd unit name
#   TOOLYARD_URL       [http://127.0.0.1:18787]   loopback base URL

set -u
set -o pipefail

BIN_SRC="${TOOLYARD_BIN_SRC:-./cmd/gateway}"
BIN_DEST="${TOOLYARD_BIN_DEST:-/usr/local/bin/toolyard}"
UNIT="${TOOLYARD_UNIT:-toolyard}"
URL="${TOOLYARD_URL:-http://127.0.0.1:18787}"

cmd="${1:-help}"

die() { echo "error: $*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

pid() {
  systemctl show -p MainPID --value "$UNIT" 2>/dev/null
}

cmd_build() {
  echo "==> building $BIN_SRC"
  local out
  out="$(mktemp)"
  if go build -o "$out" "$BIN_SRC" 2>&1; then
    echo "    built $(stat -c %s "$out") bytes"
    BUILT_BIN="$out"
  else
    rm -f "$out"
    die "build failed (see errors above)"
  fi
}

cmd_deploy() {
  cmd_build
  echo "==> installing to $BIN_DEST and restarting $UNIT"
  sudo install -m 0755 "$BUILT_BIN" "$BIN_DEST" || die "install failed"
  rm -f "$BUILT_BIN"
  sudo systemctl restart "$UNIT" || die "restart failed"
  # Wait until /v1/health responds (up to 30s).
  local i
  for i in $(seq 1 30); do
    if curl -fsS -m 2 "$URL/v1/health" >/dev/null 2>&1; then
      echo "==> up after ${i}s"
      cmd_health
      return 0
    fi
    sleep 1
  done
  echo "==> still not responding after 30s — last 30 journal lines:" >&2
  sudo journalctl -u "$UNIT" -n 30 --no-pager >&2
  return 1
}

cmd_health() {
  local out
  out="$(curl -fsS -m 5 "$URL/v1/health" 2>&1)" || die "/v1/health unreachable: $out"
  if have jq; then
    echo "$out" | jq .
  else
    echo "$out"
  fi
}

cmd_watch() {
  echo "==> tailing $UNIT journal for slow-call / upstream-timeout / goroutine-dump (Ctrl-C to stop)"
  sudo journalctl -u "$UNIT" -f --no-pager \
    | grep --line-buffered -E 'slow-call|upstream-timeout|goroutine-dump|approval sweep|reason scorer|anomaly:'
}

cmd_dump() {
  local p; p="$(pid)"
  [[ -n "$p" && "$p" != "0" ]] || die "$UNIT main pid not found"
  echo "==> SIGUSR1 -> pid $p (goroutine dump goes to journal)"
  sudo kill -USR1 "$p"
  sleep 1
  echo "==> last 80 journal lines:"
  sudo journalctl -u "$UNIT" -n 80 --no-pager
}

cmd_pprof() {
  local stamp; stamp="$(date +%Y%m%d-%H%M%S)"
  local dir="/tmp/toolyard-pprof-$stamp"
  mkdir -p "$dir"
  echo "==> writing profiles to $dir"
  curl -fsS -m 10 "$URL/debug/pprof/goroutine?debug=2" -o "$dir/goroutine-debug2.txt" \
    && echo "    goroutine-debug2.txt ($(wc -l <"$dir/goroutine-debug2.txt") lines)"
  curl -fsS -m 10 "$URL/debug/pprof/goroutine"        -o "$dir/goroutine.pb.gz"      \
    && echo "    goroutine.pb.gz       ($(stat -c %s "$dir/goroutine.pb.gz") bytes)"
  curl -fsS -m 10 "$URL/debug/pprof/heap"             -o "$dir/heap.pb.gz"           \
    && echo "    heap.pb.gz            ($(stat -c %s "$dir/heap.pb.gz") bytes)"
  echo "==> top blocked goroutines:"
  awk '/^goroutine / {flag=$0; next}
       /^$/ {flag=""; next}
       flag && /chan receive|chan send|select|sync\.|IO wait/ {print flag" :: "$0; flag=""}' \
    "$dir/goroutine-debug2.txt" | sort | uniq -c | sort -rn | head -20
  echo
  echo "    open profile interactively: go tool pprof $dir/goroutine.pb.gz"
}

cmd_poll() {
  echo "==> polling /v1/health every 1s (Ctrl-C to stop)"
  printf '%-19s  %-3s  %-5s  %-9s  %-7s\n' time http goroutines inflight dropped
  while :; do
    local body status
    body="$(curl -sS -m 2 -w '\n%{http_code}' "$URL/v1/health" 2>/dev/null || echo $'\n000')"
    status="$(printf '%s' "$body" | tail -n1)"
    body="$(printf '%s' "$body" | sed '$d')"
    if [[ "$status" == "200" ]] && have jq; then
      printf '%-19s  %-3s  %-5s  %-9s  %-7s\n' \
        "$(date '+%F %T')" "$status" \
        "$(jq -r '.goroutines // "?"'      <<<"$body")" \
        "$(jq -r '.inflight_calls // "?"'  <<<"$body")" \
        "$(jq -r '.metrics_dropped // "?"' <<<"$body")"
    else
      printf '%-19s  %-3s  (no jq or non-200)\n' "$(date '+%F %T')" "$status"
    fi
    sleep 1
  done
}

cmd_all() {
  cmd_deploy
  cmd_watch
}

cmd_help() {
  sed -n '2,18p' "$0"
}

case "$cmd" in
  build)   cmd_build ;;
  deploy)  cmd_deploy ;;
  health)  cmd_health ;;
  watch)   cmd_watch ;;
  dump)    cmd_dump ;;
  pprof)   cmd_pprof ;;
  poll)    cmd_poll ;;
  all)     cmd_all ;;
  help|-h|--help) cmd_help ;;
  *) cmd_help; exit 2 ;;
esac
