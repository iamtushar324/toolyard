#!/usr/bin/env bash
# setup-mattermost.sh — register the Mattermost MCP server as a toolyard upstream
# and lock its surface down to send/discover/resolve/read (TEC-441).
#
# Idempotent: safe to re-run. It (1) logs in, (2) creates the MATTERMOST_TOKEN
# secret if missing, (3) registers the `mattermost` upstream on /v1/servers,
# (4) applies `deny` policies for out-of-scope/destructive tools.
#
# Prereqs: the Mattermost MCP server is already running (HTTP mode) — see
# deploy/mcp-server-mattermost.service — and you have a toolyard admin login.
#
# Usage:
#   TOOLYARD_URL=http://127.0.0.1:18787 \
#   TOOLYARD_USER=admin TOOLYARD_PASS=… \
#   MATTERMOST_TOKEN=…                     \
#   scripts/setup-mattermost.sh
#
# Env:
#   TOOLYARD_URL    gateway base URL            (default http://127.0.0.1:18787)
#   TOOLYARD_USER   dashboard username          (prompted if unset)
#   TOOLYARD_PASS   dashboard password          (prompted if unset)
#   MATTERMOST_TOKEN  bot token for the secret  (prompted if unset; skipped if
#                     the secret already exists)
#   MM_TRANSPORT    http | stdio                (default http)
#   MM_HTTP_URL     upstream URL for http       (default http://127.0.0.1:8000/mcp)
#   MM_SERVER_URL   Mattermost URL for stdio    (default https://chat.beknown.live)
#   MM_PROJECT      stdio checkout path         (default /home/ubuntu/repos/mcp-server-mattermost)
set -euo pipefail

TOOLYARD_URL="${TOOLYARD_URL:-http://127.0.0.1:18787}"
MM_TRANSPORT="${MM_TRANSPORT:-http}"
MM_HTTP_URL="${MM_HTTP_URL:-http://127.0.0.1:8000/mcp}"
MM_SERVER_URL="${MM_SERVER_URL:-https://chat.beknown.live}"
MM_PROJECT="${MM_PROJECT:-/home/ubuntu/repos/mcp-server-mattermost}"

JAR="$(mktemp)"; trap 'rm -f "$JAR"' EXIT
api() { curl -fsS -b "$JAR" -c "$JAR" -H 'Content-Type: application/json' "$@"; }

# --- 1. login ----------------------------------------------------------------
if [ -z "${TOOLYARD_USER:-}" ]; then read -rp 'toolyard username: ' TOOLYARD_USER; fi
if [ -z "${TOOLYARD_PASS:-}" ]; then read -rsp 'toolyard password: ' TOOLYARD_PASS; echo; fi
echo "→ logging in to $TOOLYARD_URL"
api -X POST "$TOOLYARD_URL/v1/auth/login" \
  --data "$(python3 -c 'import json,os;print(json.dumps({"Username":os.environ["TOOLYARD_USER"],"Password":os.environ["TOOLYARD_PASS"]}))')" \
  >/dev/null
export TOOLYARD_USER TOOLYARD_PASS

# --- 2. secret ---------------------------------------------------------------
if api "$TOOLYARD_URL/v1/secrets" | grep -q '"MATTERMOST_TOKEN"'; then
  echo "✓ secret MATTERMOST_TOKEN already exists — leaving as-is"
else
  if [ -z "${MATTERMOST_TOKEN:-}" ]; then read -rsp 'MATTERMOST_TOKEN (bot token): ' MATTERMOST_TOKEN; echo; fi
  echo "→ creating secret MATTERMOST_TOKEN"
  MATTERMOST_TOKEN="$MATTERMOST_TOKEN" api -X POST "$TOOLYARD_URL/v1/secrets" \
    --data "$(python3 -c 'import json,os;print(json.dumps({"name":"MATTERMOST_TOKEN","value":os.environ["MATTERMOST_TOKEN"],"description":"Mattermost bot token (TEC-441)"}))')" \
    >/dev/null
fi

# --- 3. upstream -------------------------------------------------------------
echo "→ registering upstream 'mattermost' ($MM_TRANSPORT)"
if [ "$MM_TRANSPORT" = "stdio" ]; then
  SERVER_JSON="$(MM_SERVER_URL="$MM_SERVER_URL" MM_PROJECT="$MM_PROJECT" python3 -c '
import json,os
print(json.dumps({
 "name":"mattermost","transport":"stdio","command":"uv",
 "args":["run","--frozen","--project",os.environ["MM_PROJECT"],"mcp-server-mattermost"],
 "env":{"MATTERMOST_URL":os.environ["MM_SERVER_URL"],"MATTERMOST_TOKEN":"secret://MATTERMOST_TOKEN",
        "MATTERMOST_AUTH_MODE":"static_token","MATTERMOST_LOG_LEVEL":"WARNING"}}))')"
else
  SERVER_JSON="$(MM_HTTP_URL="$MM_HTTP_URL" python3 -c '
import json,os
print(json.dumps({"name":"mattermost","transport":"http","url":os.environ["MM_HTTP_URL"]}))')"
fi
api -X POST "$TOOLYARD_URL/v1/servers" --data "$SERVER_JSON" >/dev/null \
  || echo "  (server may already exist — continuing)"

# --- 4. deny policies (out-of-scope / destructive) ---------------------------
DENY=(
  delete_message update_message create_channel join_channel leave_channel
  add_user_to_channel upload_file create_bookmark update_bookmark delete_bookmark
  update_bookmark_sort_order add_reaction remove_reaction pin_message
  unpin_message mark_channel_viewed
)
echo "→ applying ${#DENY[@]} deny policies"
for t in "${DENY[@]}"; do
  api -X POST "$TOOLYARD_URL/v1/policies" \
    --data "$(TARGET="mattermost.$t" python3 -c 'import json,os;print(json.dumps({"scope":"tool","target":os.environ["TARGET"],"action":"deny","note":"TEC-441 out-of-scope"}))')" \
    >/dev/null || echo "  policy for mattermost.$t may already exist"
done

echo "✅ done. In-scope mattermost.* tools: post_message, create_direct_channel,"
echo "   search_users, get_user*, list_*_channels, get_channel*, get_channel_members,"
echo "   list_teams, get_team, get_me, get_channel_messages, get_thread, search_messages."
echo "   Verify: GET $TOOLYARD_URL/v1/servers  (expect mattermost: connected, tools > 0)"
