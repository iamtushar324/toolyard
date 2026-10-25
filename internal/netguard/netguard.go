// Package netguard decides whether an address is public: the one check
// webhook delivery (internal/callbacks) and Inbox media copies
// (internal/inbox) apply before they connect to an address an agent chose.
package netguard

import (
	"net"
	"net/netip"
)

// globalUnicast6 is the only IPv6 block assigned for global unicast
// (RFC 4291); everything outside it is special-use or unassigned.
var globalUnicast6 = netip.MustParsePrefix("2000::/3")

// denied are the special-use ranges (RFC 6890 and its updates) a public
// address must not be in: this host, private, shared (CGN), loopback,
// link-local, documentation, benchmarking, protocol assignments, AS112,
// 6to4 relay, multicast, reserved and broadcast, and the IPv6 equivalents
// inside 2000::/3 (Teredo and other protocol assignments, documentation,
// 6to4, which embeds an IPv4 address). NAT64 (64:ff9b::/96) and the
// IPv4-compatible block lie outside 2000::/3 and fail that check first.
var denied = func() []netip.Prefix {
	var out []netip.Prefix
	for _, p := range []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12",
		"192.0.0.0/24", "192.0.2.0/24", "192.31.196.0/24", "192.52.193.0/24", "192.88.99.0/24", "192.168.0.0/16",
		"192.175.48.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
		"2001::/23", "2001:db8::/32", "2002::/16", "3fff::/20",
	} {
		out = append(out, netip.MustParsePrefix(p))
	}
	return out
}()

// Public reports whether ip is a public unicast address. An IPv4 address
// mapped into IPv6 is checked as IPv4; any other IPv6 address must be in
// 2000::/3 and outside the special-use ranges.
func Public(ip net.IP) bool {
	a, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	a = a.Unmap()
	if a.Is6() && !globalUnicast6.Contains(a) {
		return false
	}
	for _, p := range denied {
		if p.Contains(a) {
			return false
		}
	}
	return true
}
