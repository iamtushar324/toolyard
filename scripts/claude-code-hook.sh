#!/usr/bin/env bash
# claude-code-hook.sh — Claude Code PreToolUse hook for toolyard.
#
# When the toolyard gateway can't get an approval inside its in-line wait
# window it returns a "deferred" tool result like:
#
#   { "isError": false,
#     "_meta": { "toolyard.deferred": true },
#     "structuredContent": {
#       "status": "pending_approval",
#       "approval_id": "ap_…",
#       "retry_after_seconds": 60,
#       "expires_at": "…"
#     },
#     "content": [{ "type": "text", "text": "Approval pending. …" }] }
#
# This hook detects that response, polls the toolyard REST API for the
# approval's status, and once it resolves either:
#   - re-issues the tool call with `_approval_id=...` so the gateway
#     short-circuits straight to dispatch, or
#   - reports a denial back to Claude Code.
#
# Configure in ~/.claude/settings.json under hooks.PostToolUse, e.g.:
#
#   { "type": "command", "command": "/path/to/claude-code-hook.sh" }
#
# Environment variables:
#   TOOLYARD_URL       default: http://localhost:8787
#   TOOLYARD_TOKEN     agent token for /v1/agents/exchange
#   TOOLYARD_TIMEOUT   total wait per call in seconds (default: 3600)
#   TOOLYARD_INTERVAL  poll interval seconds (default: 5)

set -euo pipefail

TOOLYARD_URL="${TOOLYARD_URL:-http://localhost:8787}"
TOOLYARD_TIMEOUT="${TOOLYARD_TIMEOUT:-3600}"
TOOLYARD_INTERVAL="${TOOLYARD_INTERVAL:-5}"

# Read the entire hook payload from stdin (Claude Code passes JSON).
payload="$(cat)"

# Extract whether this was a deferred toolyard response. We rely on `jq` if
# available; otherwise grep for the marker.
have_jq=0
if command -v jq >/dev/null 2>&1; then have_jq=1; fi

is_deferred() {
  if [[ $have_jq -eq 1 ]]; then
    jq -e '.tool_response._meta["toolyard.deferred"] == true' >/dev/null 2>&1 <<<"$payload"
  else
    grep -q '"toolyard.deferred"[[:space:]]*:[[:space:]]*true' <<<"$payload"
  fi
}

if ! is_deferred; then
  exit 0   # not a deferred toolyard response; let Claude Code continue normally
fi

if [[ $have_jq -eq 1 ]]; then
  approval_id="$(jq -r '.tool_response.structuredContent.approval_id // empty' <<<"$payload")"
  tool_name="$(jq -r '.tool_name // empty' <<<"$payload")"
else
  approval_id="$(sed -n 's/.*"approval_id"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' <<<"$payload" | head -1)"
  tool_name="$(sed -n 's/.*"tool_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' <<<"$payload" | head -1)"
fi

if [[ -z "$approval_id" ]]; then
  echo "toolyard hook: deferred response without approval_id; nothing to do" >&2
  exit 0
fi

end=$(( $(date +%s) + TOOLYARD_TIMEOUT ))
status="pending"

echo "toolyard hook: waiting for approval $approval_id ($tool_name)" >&2

while (( $(date +%s) < end )); do
  resp="$(curl -fsS -H "Authorization: Bearer ${TOOLYARD_TOKEN:-}" \
        "$TOOLYARD_URL/v1/approvals/$approval_id" 2>/dev/null || true)"
  if [[ -n "$resp" ]]; then
    if [[ $have_jq -eq 1 ]]; then
      status="$(jq -r '.status // "pending"' <<<"$resp")"
    else
      status="$(sed -n 's/.*"status"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' <<<"$resp" | head -1)"
    fi
    case "$status" in
      allowed) echo "toolyard hook: approved." >&2; break ;;
      denied)  echo "toolyard hook: denied."   >&2; break ;;
      expired) echo "toolyard hook: expired."  >&2; break ;;
    esac
  fi
  sleep "$TOOLYARD_INTERVAL"
done

case "$status" in
  allowed)
    cat <<EOF
{
  "decision": "approve",
  "reason": "toolyard approved $approval_id; re-call with _approval_id to resume.",
  "additionalContext": "Re-call $tool_name with _approval_id=\"$approval_id\" to fetch the actual result."
}
EOF
    ;;
  denied|expired)
    cat <<EOF
{
  "decision": "block",
  "reason": "toolyard $status the call ($approval_id)."
}
EOF
    ;;
  *)
    cat <<EOF
{
  "decision": "block",
  "reason": "toolyard approval timed out after ${TOOLYARD_TIMEOUT}s."
}
EOF
    ;;
esac
