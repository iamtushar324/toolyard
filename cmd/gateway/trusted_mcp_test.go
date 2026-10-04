package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/approval"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/gateway"
	"github.com/tusharbhardwaj/toolyard/internal/policy"
	"github.com/tusharbhardwaj/toolyard/internal/store"
	"github.com/tusharbhardwaj/toolyard/internal/upstreams"
)

func TestTrustedMCPStartupDefaultOffAndPrivateProfiles(t *testing.T) {
	for _, scenario := range []string{"default-off", "missing", "empty", "unsafe-key", "unsafe-profile", "malformed", "symlink"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			if os.Chmod(dir, 0700) != nil {
				t.Fatal("private directory failed")
			}
			db, err := store.Open(filepath.Join(dir, "test.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { db.Close() })
			bus, err := approval.New(context.Background(), db)
			if err != nil {
				t.Fatal(err)
			}
			gw := gateway.New(gateway.Options{Policy: policy.New(db), Approval: bus, Audit: audit.New(db)})
			gw.RegisterBuiltins()
			t.Cleanup(func() { gw.Close() })
			svc := upstreams.New(db, gw)
			configureAssertionProvider(svc, db, dir, scenario != "default-off")
			private := filepath.Join(dir, "trusted-mcp", "pilot")
			if scenario != "missing" {
				if os.MkdirAll(private, 0700) != nil {
					t.Fatal("private profile directory failed")
				}
				raw, err := os.ReadFile("../../config/preview-mcp.profile.json")
				if err != nil {
					t.Fatal(err)
				}
				if os.WriteFile(filepath.Join(private, "profile.json"), raw, 0600) != nil || os.WriteFile(filepath.Join(private, "assertion.key"), []byte(strings.Repeat("q", 32)), 0600) != nil || os.WriteFile(filepath.Join(private, "bindings.json"), []byte(`{"version":1,"bindings":[]}`), 0600) != nil {
					t.Fatal("private fixtures failed")
				}
			}
			switch scenario {
			case "unsafe-key":
				os.Chmod(filepath.Join(private, "assertion.key"), 0644)
			case "unsafe-profile":
				os.Chmod(filepath.Join(private, "profile.json"), 0644)
			case "malformed":
				os.WriteFile(filepath.Join(private, "profile.json"), []byte(`{}`), 0600)
			case "symlink":
				retained := filepath.Join(dir, "retained")
				if os.Rename(private, retained) != nil || os.Symlink(retained, private) != nil {
					t.Fatal("symlink fixture failed")
				}
			}
			srv, err := svc.Add(context.Background(), upstreams.Server{Name: "pilot", Transport: "http", URL: "http://127.0.0.1:18791/mcp", AuthMode: upstreams.AuthPerUser, AssertionProfile: "pilot"})
			if err != nil {
				t.Fatal(err)
			}
			if srv.Enabled || gw.HasTool("pilot.create") {
				t.Fatal("profile creation activated runtime")
			}
			enabled := true
			if _, err := svc.Update(context.Background(), "pilot", upstreams.Patch{Enabled: &enabled}); scenario == "empty" {
				if err != nil {
					t.Fatal("secure empty profile activation refused")
				}
				client, err := preparedAssertionClient(db, dir, "pilot", srv.URL)
				if err != nil {
					t.Fatal(err)
				}
				for _, id := range []string{"", "u_dashboard", "ag_528cc45c-2f89-41dc-9e9b-89ee3b78c8b9"} {
					if client.Preflight(context.Background(), id, "preview_snapshots", nil) == nil {
						t.Fatal("empty registry authorized caller")
					}
				}
			} else if err == nil {
				t.Fatal("unconfigured or unsafe profile activated")
			}
			if !gw.HasTool("tools.execute") {
				t.Fatal("ordinary startup affected")
			}
		})
	}
}
