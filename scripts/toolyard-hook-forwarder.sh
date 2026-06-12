#!/usr/bin/env bash
# Forward agent hook JSON from stdin to toolyard.
#
# Usage:
#   toolyard-hook-forwarder.sh <source> <toolyard-url> <agent-token>
#
# Env fallbacks:
#   TOOLYARD_HOOK_SOURCE   claude_code|codex|cursor|generic
#   TOOLYARD_URL           http://localhost:8787
#   TOOLYARD_TOKEN         agent bearer token

set -euo pipefail

SOURCE="${1:-${TOOLYARD_HOOK_SOURCE:-generic}}"
TOOLYARD_URL="${2:-${TOOLYARD_URL:-http://localhost:8787}}"
TOOLYARD_TOKEN="${3:-${TOOLYARD_TOKEN:-}}"

payload="$(cat)"
if [[ -z "$payload" ]]; then
  payload='{}'
fi

if [[ -z "$TOOLYARD_TOKEN" ]]; then
  echo "toolyard hook forwarder: TOOLYARD_TOKEN missing" >&2
  exit 0
fi

if command -v python3 >/dev/null 2>&1; then
  body="$(printf '%s' "$payload" | SOURCE="$SOURCE" python3 -c '
import json
import os
import sys

source = os.environ.get("SOURCE", "generic")
raw = sys.stdin.read() or "{}"
try:
    data = json.loads(raw)
except Exception:
    data = {"raw": raw}

def first(*keys):
    for key in keys:
        value = data.get(key)
        if isinstance(value, str) and value:
            return value
    return ""

event = first("event_name", "hook_event_name", "event") or "unknown"
text = first("text", "prompt", "message", "content", "assistant_message", "response")
if not text:
    for key in ("tool_response", "tool_result", "result", "output"):
        value = data.get(key)
        if isinstance(value, str):
            text = value
            break

out = {
    "source": source,
    "event_name": event,
    "session_id": first("session_id", "sessionId"),
    "turn_id": first("turn_id", "turnId"),
    "conversation_id": first("conversation_id", "conversationId"),
    "tool_name": first("tool_name", "toolName"),
    "cwd": first("cwd", "project_dir", "workspace"),
    "text": text,
    "payload": data,
}
print(json.dumps(out, separators=(",", ":")))
')"
else
  # Fallback keeps the raw payload even on very small systems without Python.
  escaped="$(printf '%s' "$payload" | sed 's/\\/\\\\/g; s/"/\\"/g')"
  body="{\"source\":\"$SOURCE\",\"event_name\":\"unknown\",\"payload\":\"$escaped\"}"
fi

curl -fsS \
  -H "Authorization: Bearer $TOOLYARD_TOKEN" \
  -H "Content-Type: application/json" \
  -d "$body" \
  "$TOOLYARD_URL/v1/hooks/ingest?source=$SOURCE" >/dev/null || true

exit 0
