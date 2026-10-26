package docs

import (
	"io/fs"
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

// TestNoInternalHosts scans docs/, the built-in skills and the served ty.
func TestNoInternalHosts(t *testing.T) {
	check := func(name, text string) {
		for i, line := range strings.Split(text, "\n") {
			if m := internalHost.FindString(line); m != "" {
				t.Errorf("%s:%d mentions %q: keep internal host and organisation names out of public files", name, i+1, m)
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
