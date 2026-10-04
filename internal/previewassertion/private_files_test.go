//go:build linux || darwin

package previewassertion

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPreviewPrivateFilesSecureHierarchyAndDenials(t *testing.T) {
	dir := privateTestDir(t)
	path := filepath.Join(dir, "private")
	if err := os.WriteFile(path, []byte("synthetic-test-data"), 0600); err != nil {
		t.Fatal(err)
	}
	uid := uint32(os.Geteuid())
	if _, err := PrivateFile(path, uid, 128); err != nil {
		t.Fatal("secure test hierarchy refused; CI TMPDIR must use trusted ancestors")
	}
	for _, tc := range []struct {
		name string
		path string
		uid  uint32
		max  int64
	}{
		{"relative", "private", uid, 128}, {"wrong-owner", path, uid + 1, 128}, {"oversize", path, uid, 2}, {"directory", dir, uid, 128},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := PrivateFile(tc.path, tc.uid, tc.max); err == nil {
				t.Fatal("unsafe file accepted")
			}
		})
	}
	if os.Chmod(path, 0640) != nil {
		t.Fatal("test chmod failed")
	}
	if _, err := PrivateFile(path, uid, 128); err == nil {
		t.Fatal("group-readable file accepted")
	}
	if os.Chmod(path, 0600) != nil || os.Chmod(dir, 0755) != nil {
		t.Fatal("test chmod failed")
	}
	if _, err := PrivateFile(path, uid, 128); err == nil {
		t.Fatal("non-private service directory accepted")
	}
	if os.Chmod(dir, 0700) != nil {
		t.Fatal("test chmod failed")
	}
	link := filepath.Join(dir, "link")
	if os.Symlink(path, link) != nil {
		t.Fatal("test symlink failed")
	}
	if _, err := PrivateFile(link, uid, 128); err == nil {
		t.Fatal("leaf symlink accepted")
	}
	hard := filepath.Join(dir, "hard")
	if os.Link(path, hard) != nil {
		t.Fatal("test hard link failed")
	}
	if _, err := PrivateFile(path, uid, 128); err == nil {
		t.Fatal("multiply linked issuer file accepted")
	}
}

func TestPreviewPrivateFilesRejectUnsafeAncestorAndReplacement(t *testing.T) {
	dir := t.TempDir()
	parent := filepath.Join(dir, "parent")
	private := filepath.Join(parent, "private")
	if os.MkdirAll(private, 0700) != nil {
		t.Fatal("test directory failed")
	}
	path := filepath.Join(private, "value")
	if os.WriteFile(path, []byte("trusted"), 0600) != nil {
		t.Fatal("test file failed")
	}
	uid := uint32(os.Geteuid())
	if _, err := PrivateFile(path, uid, 32); err != nil {
		t.Fatal("secure hierarchy refused")
	}
	if os.Chmod(parent, 0777) != nil {
		t.Fatal("test chmod failed")
	}
	if _, err := PrivateFile(path, uid, 32); err == nil {
		t.Fatal("writable ancestor accepted")
	}
	if os.Chmod(parent, 0700) != nil {
		t.Fatal("test chmod failed")
	}
	retained := filepath.Join(dir, "retained")
	if os.Rename(parent, retained) != nil {
		t.Fatal("test replacement failed")
	}
	if os.Symlink(retained, parent) != nil {
		t.Fatal("test replacement symlink failed")
	}
	if _, err := PrivateFile(path, uid, 32); err == nil {
		t.Fatal("replaced ancestor symlink accepted")
	}
}

func TestPreviewRegistryReloadRejectsMalformedAndExpiredBindings(t *testing.T) {
	dir := privateTestDir(t)
	path := filepath.Join(dir, "bindings.json")
	r := &FileRegistry{Path: path, OwnerUID: uint32(os.Geteuid())}
	ctx := context.Background()
	b := ledgerBinding()
	writeLedgerRegistry(t, path, []Binding{b})
	if _, err := r.Lookup(ctx, ledgerAgent, ledgerNow); err != nil {
		t.Fatal("valid registry refused")
	}
	for _, raw := range []string{
		`{"version":1,"version":1,"bindings":[]}`,
		`{"version":1,"Version":1,"bindings":[]}`,
		`{"Version":1,"bindings":[]}`,
		`{"version":1,"bindings":[],"unexpected":true}`,
		`{"version":1,"bindings":[]} {}`,
		`{"version":2,"bindings":[]}`,
	} {
		if os.WriteFile(path, []byte(raw), 0600) != nil {
			t.Fatal("test write failed")
		}
		if _, err := r.Lookup(ctx, ledgerAgent, ledgerNow); err == nil {
			t.Fatal("malformed registry accepted")
		}
	}
	// Case aliases are not canonical authority, even without exact duplicates.
	writeLedgerRegistry(t, path, []Binding{b})
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("test registry read failed")
	}
	alias := strings.Replace(string(raw), `"principal_id"`, `"Principal_ID"`, 1)
	if os.WriteFile(path, []byte(alias), 0600) != nil {
		t.Fatal("test alias write failed")
	}
	if _, err := r.Lookup(ctx, ledgerAgent, ledgerNow); err == nil {
		t.Fatal("case-aliased binding authority accepted")
	}
	writeLedgerRegistry(t, path, []Binding{b, b})
	if _, err := r.Lookup(ctx, ledgerAgent, ledgerNow); err == nil {
		t.Fatal("duplicate agent accepted")
	}
	writeLedgerRegistry(t, path, []Binding{b})
	if _, err := r.Lookup(ctx, ledgerAgent, b.ExpiresAt); err == nil {
		t.Fatal("expired registry accepted")
	}
	if _, err := r.Lookup(ctx, ledgerAgent, b.IssuedAt-1); err == nil {
		t.Fatal("future registry accepted")
	}
	writeLedgerRegistry(t, path, nil)
	if _, err := r.Lookup(ctx, ledgerAgent, ledgerNow); err == nil {
		t.Fatal("removed registry entry accepted")
	}
}
