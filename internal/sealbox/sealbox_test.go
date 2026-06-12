package sealbox

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func newTestCipher(t *testing.T) *Cipher {
	t.Helper()
	key := make([]byte, MasterKeySize)
	for i := range key {
		key[i] = byte(i)
	}
	c, err := NewCipher(key)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	return c
}

func TestSealOpenRoundTrip(t *testing.T) {
	c := newTestCipher(t)
	aad := []byte("toolyard.secrets.v1|API_KEY|value")
	plain := []byte("super-secret-token")
	sealed, err := c.Seal(plain, aad)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if sealed == "" {
		t.Fatal("sealed value is empty for non-empty input")
	}
	if bytes.Contains([]byte(sealed), plain) {
		t.Fatal("ciphertext leaks plaintext")
	}
	got, err := c.Open(sealed, aad)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("round-trip mismatch: got %q want %q", got, plain)
	}
}

func TestEmptyRoundTrip(t *testing.T) {
	c := newTestCipher(t)
	sealed, err := c.Seal(nil, []byte("aad"))
	if err != nil {
		t.Fatalf("Seal empty: %v", err)
	}
	if sealed != "" {
		t.Fatalf("empty input should seal to empty, got %q", sealed)
	}
	got, err := c.Open("", []byte("aad"))
	if err != nil || got != nil {
		t.Fatalf("Open empty: got %v err %v", got, err)
	}
}

func TestAADMismatchFails(t *testing.T) {
	c := newTestCipher(t)
	sealed, err := c.Seal([]byte("value"), []byte("aad-one"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if _, err := c.Open(sealed, []byte("aad-two")); err == nil {
		t.Fatal("Open with wrong AAD should fail the GCM tag")
	}
}

func TestNewCipherRejectsShortKey(t *testing.T) {
	if _, err := NewCipher(make([]byte, 16)); err == nil {
		t.Fatal("expected error for 16-byte key")
	}
}

func TestLoadOrCreateKeyIdempotent(t *testing.T) {
	dir := t.TempDir()
	k1, err := LoadOrCreateKey(dir, "secrets.key")
	if err != nil {
		t.Fatalf("first LoadOrCreateKey: %v", err)
	}
	if len(k1) != MasterKeySize {
		t.Fatalf("key size %d", len(k1))
	}
	// File created with 0600.
	info, err := os.Stat(filepath.Join(dir, "secrets.key"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("key file perms %v, want 0600", info.Mode().Perm())
	}
	k2, err := LoadOrCreateKey(dir, "secrets.key")
	if err != nil {
		t.Fatalf("second LoadOrCreateKey: %v", err)
	}
	if !bytes.Equal(k1, k2) {
		t.Fatal("LoadOrCreateKey not idempotent — returned a different key")
	}
}
