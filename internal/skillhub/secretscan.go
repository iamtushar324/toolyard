package skillhub

import (
	"regexp"
	"strings"
)

// secretPatterns are content shapes that mean a file holds a credential.
// Such a file is never served: it is omitted with a "secret" warning, and
// the warning never repeats the match.
var secretPatterns = []struct {
	what string
	re   *regexp.Regexp
}{
	// A PEM or PGP private key block, with key material after the header
	// (a header alone, in a how-to, does not count).
	{"a private key", regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----[\s\S]{0,300}?[A-Za-z0-9+/]{40}`)},
	{"a GitHub token", regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{36,255}\b`)},
	{"a GitHub token", regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{22,255}\b`)},
	{"an AWS access key", regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{"a Slack token", regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9-]{10,}`)},
	{"an Anthropic API key", regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{20,}`)},
	{"an OpenAI API key", regexp.MustCompile(`\bsk-proj-[A-Za-z0-9_-]{20,}`)},
	{"an API key", regexp.MustCompile(`\bsk-[A-Za-z0-9]{32,}\b`)},
}

// findSecret returns what kind of credential content seems to hold, or "".
// AWS's documented example keys (they contain "EXAMPLE") do not count.
func findSecret(content []byte) string {
	for _, p := range secretPatterns {
		for _, m := range p.re.FindAll(content, 8) {
			if strings.Contains(string(m), "EXAMPLE") {
				continue
			}
			return p.what
		}
	}
	return ""
}
