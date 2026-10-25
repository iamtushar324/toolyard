package netguard

import (
	"net"
	"testing"
)

func TestPublic(t *testing.T) {
	for _, a := range []string{
		"0.0.0.1", "10.1.2.3", "100.64.0.1", "100.127.255.254", "127.0.0.1", "169.254.169.254", "172.16.5.4", "192.0.0.8",
		"192.0.2.1", "192.88.99.1", "192.168.1.1", "198.18.0.1", "198.19.255.1", "198.51.100.7", "203.0.113.9", "224.0.0.1",
		"240.0.0.1", "255.255.255.255", "::", "::1", "::ffff:10.0.0.1", "::ffff:127.0.0.1", "::a00:1", "64:ff9b::a00:1", "64:ff9b:1::1",
		"100::1", "2001::1", "2001:db8::1", "2002:a00:1::1", "3fff::1", "4000::1", "fc00::1", "fd12::1", "fe80::1", "fec0::1", "ff02::1",
	} {
		if Public(net.ParseIP(a)) {
			t.Errorf("%s was treated as public", a)
		}
	}
	for _, a := range []string{"93.184.216.34", "8.8.8.8", "1.1.1.1", "100.128.0.1", "198.20.0.1", "2606:4700:4700::1111", "2a00:1450::1", "::ffff:93.184.216.34"} {
		if !Public(net.ParseIP(a)) {
			t.Errorf("%s was treated as private", a)
		}
	}
	if Public(nil) {
		t.Error("a missing address was treated as public")
	}
}
