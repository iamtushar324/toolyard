package upstreams

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestValidateRefusesGatewayNames: the dashboard cannot create a server
// under a name the gateway owns (a synthetic upstream, a built-in group,
// or toolyard, the code-mode server); existing live names stay valid.
func TestValidateRefusesGatewayNames(t *testing.T) {
	for _, name := range []string{
		"builtin", "fixture", "memory", "tools", "mempalace", "notes", "skills",
		"inbox", "session", "lake", "events", "policies", "servers", "audit", "access", "toolyard", "connections", "jobs", "callbacks",
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

// A server an older version saved under a name v0.1 reserves (jobs,
// callbacks) is not loaded, and its status says why and how to fix it.
func TestLoadAllExplainsNewlyReservedNames(t *testing.T) {
	ctx := context.Background()
	svc, db, _ := newServiceFixture(t)
	now := time.Now().UnixMilli()
	for _, name := range []string{"jobs", "callbacks"} {
		if _, err := db.ExecContext(ctx, `INSERT INTO upstream_servers(name, transport, url, env_json, headers_json, enabled, created_at, updated_at)
        VALUES(?, 'http', 'http://127.0.0.1:1/mcp', '{}', '{}', 1, ?, ?)`, name, now, now); err != nil {
			t.Fatal(err)
		}
	}
	if err := svc.LoadAll(ctx); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"jobs", "callbacks"} {
		got, err := svc.Get(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		if got.LastStatus == "ok" || !strings.Contains(got.LastError, "reserved since toolyard v0.1") || !strings.Contains(got.LastError, "another name") {
			t.Fatalf("%s: status %q, error %q", name, got.LastStatus, got.LastError)
		}
	}
}
