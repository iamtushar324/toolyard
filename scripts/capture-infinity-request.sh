#!/usr/bin/env bash
# capture-infinity-request.sh — tcpdump one request from the Grafana
# Infinity plugin to the gateway and print the raw HTTP payload.
#
# Run as root (sudo), then reload the failing Grafana panel in the
# browser. The first POST to port 18787 is printed and the script exits.
#
# Usage:
#   sudo scripts/capture-infinity-request.sh

set -u
set -o pipefail

if [[ $EUID -ne 0 ]]; then
  echo "must be run as root (sudo)" >&2
  exit 1
fi

IFACE=""
# pick the bridge interface the docker containers use
for iface in docker0 br-toolyard br+ virbr0 eth0 ens3 enp0s3; do
  if ip link show "$iface" >/dev/null 2>&1; then
    IFACE="$iface"
    break
  fi
done
if [[ -z "$IFACE" ]]; then
  # fall back: any interface that has a 172.x address
  IFACE=$(ip -o -4 addr | awk '/172\./ {print $2}' | head -1)
fi
if [[ -z "$IFACE" ]]; then
  IFACE="any"
fi
echo "==> listening on interface: $IFACE (port 18787)"
echo "==> reload the failing Grafana panel now..."
echo

# -A prints ASCII payload; -c 1 exits after the first matching packet.
# We grab the first TCP data packet to dst port 18787 that looks like an
# HTTP POST (body will follow in subsequent packets but usually fits in
# the first segment for sub-4KB requests).
tcpdump -i "$IFACE" -A -s 0 'tcp dst port 18787 and (tcp[tcpflags] & tcp-push != 0)' -c 4 2>/dev/null \
  | grep -A 200 'POST /v1/lake'

echo
echo "==> done. Paste the output above back to Claude."
