package audit

import (
	"encoding/json"
	"regexp"
)

// secretPatterns are obvious-looking credentials we redact at write time
// before they land in audit_events.arguments / audit_events.reason. The
// list errs on the side of false positives — an over-redacted reason is
// only a UX problem; a leaked token is a security problem.
var secretPatterns = []*regexp.Regexp{
	// Anthropic + OpenAI + Cohere
	regexp.MustCompile(`sk-(?:ant|proj)?-[A-Za-z0-9_-]{20,}`),
	regexp.MustCompile(`sk-[A-Za-z0-9_-]{20,}`),
	// AWS access keys
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
	regexp.MustCompile(`ASIA[0-9A-Z]{16}`),
	// GitHub tokens
	regexp.MustCompile(`ghp_[A-Za-z0-9]{30,}`),
	regexp.MustCompile(`gho_[A-Za-z0-9]{30,}`),
	regexp.MustCompile(`ghs_[A-Za-z0-9]{30,}`),
	regexp.MustCompile(`github_pat_[A-Za-z0-9_]{30,}`),
	// JWTs (three base64url segments) — lossy but high signal
	regexp.MustCompile(`eyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`),
	// Slack
	regexp.MustCompile(`xox[baprs]-[A-Za-z0-9-]{10,}`),
	// Generic high-entropy "Authorization: Bearer ..."
	regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._-]{20,}`),
	// 32+ char hex blob (sha-style hashes / hex secrets)
	regexp.MustCompile(`\b[a-f0-9]{40,}\b`),
	connectLinkRE,
}

// connectLinkRE matches a toolyard connect link's ticket (the path is
// oauth.ConnectLinkPath): a one-time credential an agent shows the person
// once, which must not land in result_summary or any other stored text.
var connectLinkRE = regexp.MustCompile(`/v1/connect/link/[A-Za-z0-9_-]{16,}`)

// redactionPlaceholder is the literal substituted in for matched secrets.
const redactionPlaceholder = "«redacted»"

// RedactConnectLinks replaces only connect-link tickets in s, for text
// that is otherwise stored verbatim and read back by an agent (a persisted
// approval result), where the broad patterns above would damage legitimate
// values such as commit hashes.
func RedactConnectLinks(s string) string {
	return connectLinkRE.ReplaceAllString(s, redactionPlaceholder)
}

// RedactString runs every pattern over s, replacing matches with the
// placeholder. Idempotent — running it twice on the same string is safe.
func RedactString(s string) string {
	if s == "" {
		return s
	}
	out := s
	for _, re := range secretPatterns {
		out = re.ReplaceAllString(out, redactionPlaceholder)
	}
	return out
}

// RedactJSONBytes walks a json.RawMessage, redacting all string-valued
// leaves. Numbers / bools / nulls pass through. Returns the original bytes
// unchanged if parsing fails (we never want to drop audit data because of a
// redactor bug).
func RedactJSONBytes(in json.RawMessage) json.RawMessage {
	if len(in) == 0 {
		return in
	}
	var v any
	if err := json.Unmarshal(in, &v); err != nil {
		return in
	}
	v = redactAny(v)
	out, err := json.Marshal(v)
	if err != nil {
		return in
	}
	return out
}

func redactAny(v any) any {
	switch x := v.(type) {
	case string:
		return RedactString(x)
	case map[string]any:
		for k, vv := range x {
			x[k] = redactAny(vv)
		}
		return x
	case []any:
		for i := range x {
			x[i] = redactAny(x[i])
		}
		return x
	default:
		return x
	}
}
