package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/approval"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/gateway"
	"github.com/tusharbhardwaj/toolyard/internal/memory"
	"github.com/tusharbhardwaj/toolyard/internal/policy"
	"github.com/tusharbhardwaj/toolyard/internal/previewassertion"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

func previewStartupFixture(t *testing.T) (*gateway.Gateway, *store.DB, string) {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal("test database unavailable")
	}
	t.Cleanup(func() { db.Close() })
	bus, err := approval.New(context.Background(), db)
	if err != nil {
		t.Fatal("test approval bus unavailable")
	}
	gw := gateway.New(gateway.Options{Policy: policy.New(db), Approval: bus, Audit: audit.New(db), Memory: memory.New(db)})
	gw.RegisterBuiltins()
	t.Cleanup(func() { gw.Close() })
	return gw, db, dir
}

func TestBKSPreviewStartupDefaultOffAndMissingConfiguration(t *testing.T) {
	gw, db, dir := previewStartupFixture(t)
	if err := configureBKSPreview(gw, db, dir, false); err != nil {
		t.Fatal("default-off startup failed")
	}
	if gw.HasTool("bks_preview.create") {
		t.Fatal("default-off exposed preview tools")
	}
	if _, err := os.Stat(filepath.Join(dir, "bks-preview")); !os.IsNotExist(err) {
		t.Fatal("default-off created runtime files")
	}
	if err := configureBKSPreview(gw, db, dir, true); !errors.Is(err, previewassertion.ErrIdentity) {
		t.Fatal("missing configuration did not refuse preview")
	}
	if gw.HasTool("bks_preview.create") || !gw.HasTool("tools.execute") {
		t.Fatal("configuration failure affected ordinary catalog")
	}
}

func TestBKSPreviewStartupEmptyRegistryRefusesAllCallers(t *testing.T) {
	gw, db, dir := previewStartupFixture(t)
	private := filepath.Join(dir, "bks-preview")
	if os.Mkdir(private, 0700) != nil {
		t.Fatal("test directory failed")
	}
	if os.WriteFile(filepath.Join(private, "assertion.key"), []byte(strings.Repeat("q", 32)), 0600) != nil {
		t.Fatal("synthetic test key failed")
	}
	if os.WriteFile(filepath.Join(private, "bindings.json"), []byte(`{"version":1,"bindings":[]}`), 0600) != nil {
		t.Fatal("test registry failed")
	}
	client, err := preparedPreviewClient(db, dir)
	if err != nil {
		t.Fatal("secure empty configuration refused")
	}
	if err = configureBKSPreview(gw, db, dir, true); err != nil {
		t.Fatal("configured startup failed")
	}
	if !gw.HasTool("bks_preview.create") {
		t.Fatal("fixed catalog missing")
	}
	for _, id := range []string{"", "u_dashboard", "ag_528cc45c-2f89-41dc-9e9b-89ee3b78c8b9"} {
		if client.Preflight(context.Background(), id, "preview_snapshots", nil) == nil {
			t.Fatal("empty registry accepted a caller")
		}
	}
	if !gw.HasTool("tools.execute") {
		t.Fatal("ordinary tools disappeared")
	}
}

func TestBKSPreviewStartupRejectsUnsafeOrMalformedFiles(t *testing.T) {
	for _, scenario := range []string{"short-key", "malformed-registry", "unsafe-directory", "unsafe-key", "unsafe-registry", "symlink"} {
		t.Run(scenario, func(t *testing.T) {
			gw, db, dir := previewStartupFixture(t)
			private := filepath.Join(dir, "bks-preview")
			if os.Mkdir(private, 0700) != nil {
				t.Fatal("test directory failed")
			}
			kp := filepath.Join(private, "assertion.key")
			rp := filepath.Join(private, "bindings.json")
			if os.WriteFile(kp, []byte(strings.Repeat("q", 32)), 0600) != nil || os.WriteFile(rp, []byte(`{"version":1,"bindings":[]}`), 0600) != nil {
				t.Fatal("test files failed")
			}
			switch scenario {
			case "short-key":
				os.WriteFile(kp, []byte("test"), 0600)
			case "malformed-registry":
				os.WriteFile(rp, []byte(`{"version":1,"version":1,"bindings":[]}`), 0600)
			case "unsafe-directory":
				os.Chmod(private, 0777)
			case "unsafe-key":
				os.Chmod(kp, 0644)
			case "unsafe-registry":
				os.Chmod(rp, 0644)
			case "symlink":
				retained := filepath.Join(dir, "retained")
				if os.Rename(private, retained) != nil || os.Symlink(retained, private) != nil {
					t.Fatal("test replacement failed")
				}
			}
			if configureBKSPreview(gw, db, dir, true) == nil {
				t.Fatal("unsafe configuration accepted")
			}
			if gw.HasTool("bks_preview.create") || !gw.HasTool("tools.execute") {
				t.Fatal("unsafe configuration affected normal startup")
			}
		})
	}
}
