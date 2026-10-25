package audit

import (
	"strings"
	"testing"
)

// A skills bundle download ticket is redacted wherever the audit log or a
// persisted result would keep it.
func TestRedactSkillBundleTickets(t *testing.T) {
	ticket := "tyskb_" + strings.Repeat("Ab3_-", 8) + "xyz"
	if len(ticket) != len("tyskb_")+43 {
		t.Fatalf("ticket length %d", len(ticket))
	}
	text := `{"url":"https://toolyard.example/v1/skills/bundle/` + ticket + `"}`
	for name, got := range map[string]string{
		"RedactString":       RedactString(text),
		"RedactConnectLinks": RedactConnectLinks(text),
		"RedactJSONBytes":    string(RedactJSONBytes([]byte(text))),
	} {
		if strings.Contains(got, ticket) || !strings.Contains(got, redactionPlaceholder) {
			t.Errorf("%s kept the ticket: %s", name, got)
		}
	}
	// RedactConnectLinks leaves other values, such as commit hashes, alone.
	sha := strings.Repeat("ab", 20)
	if got := RedactConnectLinks("commit " + sha); got != "commit "+sha {
		t.Fatalf("RedactConnectLinks changed a commit hash: %s", got)
	}
}
