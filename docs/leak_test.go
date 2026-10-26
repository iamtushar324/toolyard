package docs

import (
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

// internalHost matches names of one organisation's own hosts and machines:
// none belongs in this public repository's documents, in the skills
// toolyard ships, or in the ty it serves. The words are split so this file
// does not match itself.
var internalHost = regexp.MustCompile(`(?i)` + strings.Join([]string{
	"be" + "known", // the organisation's name and domains
	`\b[a-z0-9-]+(?:\.[a-z0-9-]+)*\.li` + `ve\b`, // hosts under a .live domain
	`\.ts\.ne` + `t\b`,                           // tailnet names
	`\btail` + `ab[0-9a-f]+`,                     // a tailnet id
	`\bip-10` + `-[0-9]+`,                        // cloud host names
	`\bdev-ser` + `ver-[0-9]`,                    // build machines
}, "|"))

// dottedNumber is a run of digits and dots that starts and ends with a
// digit: a candidate IPv4 address (four parts), a version, or a longer
// dotted number.
var dottedNumber = regexp.MustCompile(`[0-9]+(?:\.[0-9]+)+`)

// svgNumbers is the number data of an SVG attribute (path data, points,
// a view box), whose runs like "1.23.82.72" are numbers, not addresses.
var svgNumbers = regexp.MustCompile(`\b(?:d|points|viewBox)="[^"]*"?`)

// cidrSuffix is the prefix length after an address written as a range.
var cidrSuffix = regexp.MustCompile(`^/[0-9]{1,2}\b`)

// docAddresses are the IPv4 ranges RFC 5737 sets aside for documentation:
// the examples in public files use them.
var docAddresses = []netip.Prefix{
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
}

// textbookRanges are special-purpose ranges a document may name as ranges
// (a kind of network, nobody's host).
var textbookRanges = map[string]bool{
	"10.0.0.0/8": true, "172.16.0.0/12": true, "192.168.0.0/16": true, "100.64.0.0/10": true, "169.254.0.0/16": true,
}

// ipv4Literal returns the first IPv4 address in line that may name a real
// host: anything but loopback (127.0.0.0/8), 0.0.0.0, the documentation
// ranges and a textbook range in CIDR notation. Versions ("v1.2.3.4"),
// parts of a longer dotted number ("1.2.3.4.5"), quads with an octet over
// 255 or a leading zero, and SVG number data are not addresses.
func ipv4Literal(line string) string {
	line = svgNumbers.ReplaceAllString(line, "")
	for _, loc := range dottedNumber.FindAllStringIndex(line, -1) {
		run := line[loc[0]:loc[1]]
		if strings.Count(run, ".") != 3 {
			continue
		}
		if loc[0] > 0 {
			if c := line[loc[0]-1]; c == 'v' || c == 'V' || c == '.' {
				continue
			}
		}
		ip, err := netip.ParseAddr(run)
		if err != nil || !ip.Is4() || ip.IsLoopback() || ip.IsUnspecified() {
			continue
		}
		doc := false
		for _, p := range docAddresses {
			doc = doc || p.Contains(ip)
		}
		if doc || textbookRanges[run+cidrSuffix.FindString(line[loc[1]:])] {
			continue
		}
		return run
	}
	return ""
}

func TestIPv4Literal(t *testing.T) {
	for line, want := range map[string]string{
		"listen on 172.31.250.9:18790":             "172.31.250.9",
		"names resolve to 10.11.12.13.":            "10.11.12.13",
		"-ops-addr 127.0.0.1:9090,0.0.0.0:9090":    "",
		"an example host 203.0.113.10":             "",
		"TOOLYARD_TRUSTED_PROXY=198.51.100.0/24":   "",
		"carrier-grade NAT (`100.64.0.0/10`)":      "",
		"a private subnet 10.200.0.0/24":           "10.200.0.0",
		"Electron v1.2.3.4 and Chrome/152.0.7977":  "",
		"version 1.2.3.4.5":                        "",
		`<path d="M8 0C1.23.82.72 1.2 2.2.82.64">`: "",
		"octets 01.2.3.4":                          "",
	} {
		if got := ipv4Literal(line); got != want {
			t.Errorf("ipv4Literal(%q) = %q, want %q", line, got, want)
		}
	}
}

// TestNoInternalHosts scans docs/, the built-in skills and the served ty
// for internal host names and IPv4 addresses that may name a real host.
func TestNoInternalHosts(t *testing.T) {
	check := func(name, text string) {
		for i, line := range strings.Split(text, "\n") {
			if m := internalHost.FindString(line); m != "" {
				t.Errorf("%s:%d mentions %q: keep internal host and organisation names out of public files", name, i+1, m)
			}
			if ip := ipv4Literal(line); ip != "" {
				t.Errorf("%s:%d names the IPv4 address %s: use a documentation address (192.0.2.x, 198.51.100.x, 203.0.113.x) or words", name, i+1, ip)
			}
		}
	}
	for _, root := range []string{".", filepath.Join("..", "internal", "skillhub", "builtin")} {
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			if utf8.Valid(b) {
				check(p, string(b))
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	check("GET /v1/code/ty", ServedTy("https://toolyard.example.com"))
	check("GET /v1/code/openapi.json", ServedCodeOpenAPI("https://toolyard.example.com"))
}

// The served ty and OpenAPI document name the serving toolyard, and only
// a plain, safe base URL.
func TestServedTyAndOpenAPI(t *testing.T) {
	if !strings.Contains(TyScript, TyServedURLLine) || !strings.Contains(CodeOpenAPI, openAPIServerURL) {
		t.Fatal("the placeholders ServedTy and ServedCodeOpenAPI replace are missing")
	}
	got := ServedTy("https://toolyard.acme.test/")
	if !strings.Contains(got, `DEFAULT_URL = "https://toolyard.acme.test"`) || strings.Contains(got, TyServedURLLine) {
		t.Fatal("ServedTy did not set the default URL")
	}
	if !strings.Contains(ServedCodeOpenAPI("https://toolyard.acme.test"), `"url": "https://toolyard.acme.test"`) {
		t.Fatal("ServedCodeOpenAPI did not set the server URL")
	}
	for _, bad := range []string{"", "toolyard.acme.test", "ftp://x.test", `https://x.test/"; import os; "`, "https://u:p@x.test", "https://x.test/?a=1", "https://x.test/#f", "https://x .test"} {
		if ServedTy(bad) != TyScript || ServedCodeOpenAPI(bad) != CodeOpenAPI {
			t.Errorf("%q must leave the documents as they are", bad)
		}
	}
}
