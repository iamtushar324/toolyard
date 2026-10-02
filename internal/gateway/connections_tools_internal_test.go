package gateway

import (
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// TestApprovalResultDropsConnectLinks: a refusal that carried a connect
// link, persisted as an approval result (read by admins and operator
// tokens, replayed on every poll), loses the ticket and nothing else.
func TestApprovalResultDropsConnectLinks(t *testing.T) {
	ticket := "hfBgdyrV9-_i2uWngcS73fFI0ETfR8guODHjVdkKgvQ"
	link := "https://toolyard.example/v1/connect/link/" + ticket
	res := mcp.NewToolResultError("linear runs as you and isn't connected yet. Open this link to connect it (one-time, 10 minutes): " + link + " — then ask me to retry. Nothing was run.")
	res.StructuredContent = map[string]any{
		"servers": []any{map[string]any{"server": "linear", "connect_link": link, "sha": "0123456789abcdef0123456789abcdef01234567"}},
	}
	env, err := encodeApprovalResult(res)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(env, ticket) {
		t.Fatalf("ticket persisted in the approval envelope: %s", env)
	}
	for _, keep := range []string{"isn't connected yet", "then ask me to retry", "0123456789abcdef0123456789abcdef01234567", `"server":"linear"`} {
		if !strings.Contains(env, keep) {
			t.Errorf("envelope lost %q: %s", keep, env)
		}
	}
	back, err := rehydrateApprovalResult(env)
	if err != nil || back == nil || !back.IsError {
		t.Fatalf("rehydrate: %v %+v", err, back)
	}
	sc, _ := back.StructuredContent.(map[string]any)
	if sc == nil || sc["servers"] == nil {
		t.Fatalf("structured content lost: %+v", back.StructuredContent)
	}
}
