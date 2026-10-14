package sealbox

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestDecodeKeyTrimsAndAcceptsPadding(t *testing.T) {
	key := bytes.Repeat([]byte{7}, MasterKeySize)
	for _, body := range []string{
		base64.RawStdEncoding.EncodeToString(key),
		base64.RawStdEncoding.EncodeToString(key) + "\n",
		base64.StdEncoding.EncodeToString(key) + "\n",
		"\t " + base64.RawStdEncoding.EncodeToString(key) + " \r\n",
	} {
		got, err := DecodeKey([]byte(body))
		if err != nil || !bytes.Equal(got, key) {
			t.Errorf("DecodeKey(%q) = %v, %v", body, got, err)
		}
	}
	for _, body := range []string{"", "\n", "not base64!", base64.RawStdEncoding.EncodeToString(key[:16])} {
		if _, err := DecodeKey([]byte(body)); !errors.Is(err, ErrBadKey) {
			t.Errorf("DecodeKey(%q) accepted", body)
		}
	}
}

func TestCreateKeyFileNeverOverwrites(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "oauth.key")
	key, err := CreateKeyFile(p)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	info, err := os.Stat(p)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("stat: %v %v", info, err)
	}
	b, _ := os.ReadFile(p)
	if got, err := DecodeKey(b); err != nil || !bytes.Equal(got, key) {
		t.Fatal("file does not hold the returned key")
	}
	if _, err := CreateKeyFile(p); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("second create: %v, want fs.ErrExist", err)
	}
	if b2, _ := os.ReadFile(p); !bytes.Equal(b, b2) {
		t.Fatal("existing key file was overwritten")
	}
}

func TestLoadOrCreateKeyLeavesACorruptFileAlone(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "secrets.key")
	if err := os.WriteFile(p, []byte("garbage\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateKey(dir, "secrets.key"); !errors.Is(err, ErrBadKey) {
		t.Fatalf("corrupt key: %v, want ErrBadKey", err)
	}
	if b, _ := os.ReadFile(p); string(b) != "garbage\n" {
		t.Fatal("corrupt key file was replaced")
	}
}

func TestLoadOrCreateKeyTrimsWhitespace(t *testing.T) {
	dir := t.TempDir()
	key := bytes.Repeat([]byte{9}, MasterKeySize)
	if err := os.WriteFile(filepath.Join(dir, "session.key"), []byte(base64.RawStdEncoding.EncodeToString(key)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadOrCreateKey(dir, "session.key")
	if err != nil || !bytes.Equal(got, key) {
		t.Fatalf("got %v, %v", got, err)
	}
}
