package coderuns

import (
	"strings"
	"testing"
)

// A skills bundle link (skills.bundle) is a single-use credential: a
// stored call record, result or log never keeps its ticket.
func TestRedactGrantsRemovesSkillBundleTickets(t *testing.T) {
	ticket := "tyskb_" + strings.Repeat("Ab3_-", 8) + "xyz"
	in := `{"url":"https://toolyard.example/v1/skills/bundle/` + ticket + `","skills":[]}`
	out := redactGrants(in)
	if strings.Contains(out, ticket) || !strings.Contains(out, redactedBundle) || !strings.Contains(out, "/v1/skills/bundle/") {
		t.Fatalf("redacted: %s", out)
	}
	if got := redactGrants(`{"note":"tyskb_ starts a bundle ticket"}`); got != `{"note":"tyskb_ starts a bundle ticket"}` {
		t.Fatalf("a mention of the prefix changed: %s", got)
	}
}
