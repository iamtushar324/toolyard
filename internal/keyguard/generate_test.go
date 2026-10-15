package keyguard

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/sealbox"
	"github.com/tusharbhardwaj/toolyard/internal/store/storetest"
)

func TestGenerateKeyDecodesAndDiffers(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 8; i++ {
		key, encoded, err := GenerateKey()
		if err != nil {
			t.Fatal(err)
		}
		back, err := sealbox.DecodeKey(encoded)
		if err != nil || !bytes.Equal(back, key) || len(key) != sealbox.MasterKeySize {
			t.Fatalf("generated key does not decode to itself: %v", err)
		}
		// With the newline a secret store keeps, it still decodes.
		if back, err := sealbox.DecodeKey(append(append([]byte{}, encoded...), '\n')); err != nil || !bytes.Equal(back, key) {
			t.Fatalf("generated key with a trailing newline: %v", err)
		}
		if seen[string(key)] {
			t.Fatal("GenerateKey returned the same key twice")
		}
		seen[string(key)] = true
	}
}

func TestSpecByName(t *testing.T) {
	for _, n := range []string{"identity", "identity.key", " OAuth ", "secrets", "session.key"} {
		if _, ok := SpecByName(n); !ok {
			t.Errorf("SpecByName(%q) found nothing", n)
		}
	}
	if sp, _ := SpecByName("session"); !sp.Recreatable {
		t.Error("session.key is not recreatable")
	}
	for _, n := range []string{"", "ops", "identity.pem", "../identity"} {
		if _, ok := SpecByName(n); ok {
			t.Errorf("SpecByName(%q) found a key", n)
		}
	}
}

// Generated key files load with ReadKeys and with LoadAll in require mode on
// a new database, each is mode 0600, and each run makes different keys.
func TestWriteNewKeyFilesLoad(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	genA, err := WriteNewKeyFiles(dirA, ServeKeys())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := WriteNewKeyFiles(dirB, ServeKeys()); err != nil {
		t.Fatal(err)
	}
	if len(genA) != 4 {
		t.Fatalf("wrote %d keys, want 4", len(genA))
	}
	for _, g := range genA {
		info, err := os.Stat(g.Path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s has mode %v, want 0600", g.Path, info.Mode().Perm())
		}
		if g.KeyID == "" || filepath.Base(g.Path) != g.Name {
			t.Errorf("generated key %+v", g)
		}
	}
	noEnv := func(string) string { return "" }
	keysA, err := ReadKeys(Config{DataDir: dirA, Getenv: noEnv}, ServeKeys())
	if err != nil {
		t.Fatalf("ReadKeys: %v", err)
	}
	keysB, err := ReadKeys(Config{DataDir: dirB, Getenv: noEnv}, ServeKeys())
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for name, key := range keysA {
		if bytes.Equal(key, keysB[name]) {
			t.Errorf("%s is the same in two runs", name)
		}
		if other, dup := seen[string(key)]; dup {
			t.Errorf("%s and %s are the same key", name, other)
		}
		seen[string(key)] = name
	}

	// serve -keys=require accepts them on a new database (the keys sit in
	// the credentials directory, as Docker secrets do).
	db := storetest.Open(t)
	loaded, err := LoadAll(context.Background(), Config{
		DB: db.DB, Dialect: db.Dialect, DataDir: t.TempDir(), CredentialsDir: dirA,
		Mode: Require, Getenv: noEnv, Logf: t.Logf,
	}, ServeKeys())
	if err != nil {
		t.Fatalf("LoadAll -keys=require: %v", err)
	}
	for name, key := range keysA {
		if !bytes.Equal(loaded[name], key) {
			t.Errorf("LoadAll loaded a different %s", name)
		}
	}
}

// WriteNewKeyFiles writes nothing when any key file already exists, and
// WriteNewKeyFile never overwrites one.
func TestWriteNewKeyFilesNeverOverwrite(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, OAuthKey)
	if err := os.WriteFile(existing, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := WriteNewKeyFiles(dir, ServeKeys())
	if err == nil || !strings.Contains(err.Error(), existing) {
		t.Fatalf("WriteNewKeyFiles over an existing key: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("a refused run wrote files: %v", entries)
	}
	if b, _ := os.ReadFile(existing); string(b) != "keep" {
		t.Fatalf("the existing file changed: %q", b)
	}

	if _, err := WriteNewKeyFile(existing, OAuthKey); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("WriteNewKeyFile over an existing file: %v", err)
	}
	if b, _ := os.ReadFile(existing); string(b) != "keep" {
		t.Fatalf("the existing file changed: %q", b)
	}

	if _, err := WriteNewKeyFiles(filepath.Join(dir, "missing"), ServeKeys()); err == nil {
		t.Fatal("WriteNewKeyFiles into a missing directory succeeded")
	}
	if _, err := WriteNewKeyFiles(existing, ServeKeys()); err == nil {
		t.Fatal("WriteNewKeyFiles into a file succeeded")
	}
}
