package secrets

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/sealbox"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

func newTestService(t *testing.T) *Service {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	key, err := sealbox.LoadOrCreateKey(dir, "secrets.key")
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	c, err := sealbox.NewCipher(key)
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	return New(db, c, audit.New(db))
}

func TestCRUD(t *testing.T) {
	ctx := context.Background()
	s := newTestService(t)

	if _, err := s.Create(ctx, "API_KEY", "sk-live-123", "prod key"); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Duplicate.
	if _, err := s.Create(ctx, "API_KEY", "x", ""); err != ErrExists {
		t.Fatalf("dup create err = %v, want ErrExists", err)
	}
	// Bad name.
	if _, err := s.Create(ctx, "bad-name", "x", ""); err == nil {
		t.Fatal("expected bad name to fail")
	}
	// List never carries values.
	metas, err := s.List(ctx)
	if err != nil || len(metas) != 1 {
		t.Fatalf("list: %v len=%d", err, len(metas))
	}
	if metas[0].Name != "API_KEY" || metas[0].Description != "prod key" {
		t.Fatalf("meta mismatch: %+v", metas[0])
	}
	// Resolve returns plaintext.
	v, err := s.Resolve(ctx, "API_KEY")
	if err != nil || v != "sk-live-123" {
		t.Fatalf("resolve = %q err %v", v, err)
	}
	// Rotate.
	if _, err := s.Update(ctx, "API_KEY", "sk-live-456", nil); err != nil {
		t.Fatalf("update: %v", err)
	}
	v, _ = s.Resolve(ctx, "API_KEY")
	if v != "sk-live-456" {
		t.Fatalf("after rotate = %q", v)
	}
	// last_used should be set after resolve.
	m, _ := s.Get(ctx, "API_KEY")
	if m.LastUsedAt == 0 {
		t.Fatal("last_used_at not updated by Resolve")
	}
	// Delete.
	if err := s.Delete(ctx, "API_KEY"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.Get(ctx, "API_KEY"); err != ErrNotFound {
		t.Fatalf("get after delete err = %v", err)
	}
}

func TestParseRef(t *testing.T) {
	cases := []struct {
		in   string
		name string
		ok   bool
	}{
		{"secret://API_KEY", "API_KEY", true},
		{"secret://A", "A", true},
		{"secret://lower", "", false},    // lowercase invalid
		{"secret://", "", false},         // empty name
		{"Bearer secret://X", "", false}, // not whole-value
		{"plainvalue", "", false},        // no prefix
		{"secret://9LEAD", "", false},    // must start with letter
		{"secret://A B", "", false},      // space invalid
	}
	for _, c := range cases {
		name, ok := ParseRef(c.in)
		if ok != c.ok || name != c.name {
			t.Errorf("ParseRef(%q) = (%q,%v), want (%q,%v)", c.in, name, ok, c.name, c.ok)
		}
	}
}

func TestResolveMap(t *testing.T) {
	ctx := context.Background()
	s := newTestService(t)
	if _, err := s.Create(ctx, "TOKEN", "tok-secret", ""); err != nil {
		t.Fatal(err)
	}
	in := map[string]string{
		"AUTH":  "secret://TOKEN",
		"PLAIN": "literal-value",
	}
	out, err := s.ResolveMap(ctx, in)
	if err != nil {
		t.Fatalf("resolvemap: %v", err)
	}
	if out["AUTH"] != "tok-secret" || out["PLAIN"] != "literal-value" {
		t.Fatalf("resolvemap out = %+v", out)
	}
	// Unknown ref errors and the message names the ref, never the value.
	_, err = s.ResolveMap(ctx, map[string]string{"X": "secret://MISSING"})
	if err == nil {
		t.Fatal("expected error for missing secret")
	}
	if got := err.Error(); !contains(got, "MISSING") || contains(got, "tok-secret") {
		t.Fatalf("error %q should name ref, not value", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
