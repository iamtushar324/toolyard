package upstreams

import (
	"errors"
	"testing"
)

// TestValidateRefusesGatewayNames: the dashboard cannot create a server
// under a name the gateway owns (a synthetic upstream, a built-in group,
// or toolyard, the code-mode server); existing live names stay valid.
func TestValidateRefusesGatewayNames(t *testing.T) {
	for _, name := range []string{
		"builtin", "fixture", "memory", "tools", "mempalace", "notes", "skills",
		"inbox", "session", "lake", "events", "policies", "servers", "audit", "access", "toolyard", "connections",
	} {
		err := validate(Server{Name: name, Transport: "http", URL: "http://127.0.0.1:1/mcp"})
		if !errors.Is(err, ErrReserved) {
			t.Errorf("validate(%q) = %v, want ErrReserved", name, err)
		}
	}
	for _, name := range []string{"fs", "BkCoreServices", "LinearForUsers", "GoogleDrive", "Context7"} {
		if err := validate(Server{Name: name, Transport: "http", URL: "http://127.0.0.1:1/mcp"}); err != nil {
			t.Errorf("validate(%q) = %v, want ok", name, err)
		}
	}
}
