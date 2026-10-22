package main

import (
	"strings"

	"github.com/tusharbhardwaj/toolyard/internal/gateway"
)

// upstreamProtocolLegacy is the -upstream-protocol default: every
// upstream connection speaks 2025-11-25, as before 2026-07-28 support.
const upstreamProtocolLegacy = "legacy"

// parseUpstreamProtocol checks -upstream-protocol and returns the
// gateway.Options.UpstreamProtocol value: "" for legacy, or
// gateway.ProtocolAuto.
func parseUpstreamProtocol(s string) (string, error) {
	switch v := strings.ToLower(strings.TrimSpace(s)); v {
	case upstreamProtocolLegacy:
		return "", nil
	case gateway.ProtocolAuto:
		return gateway.ProtocolAuto, nil
	}
	return "", configErrorf("-upstream-protocol must be %q or %q, not %q", upstreamProtocolLegacy, gateway.ProtocolAuto, s)
}
