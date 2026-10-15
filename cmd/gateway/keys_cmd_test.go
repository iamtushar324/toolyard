package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tusharbhardwaj/toolyard/internal/keyguard"
	"github.com/tusharbhardwaj/toolyard/internal/sealbox"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

func runKeysGenerateForTest(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	err := runKeysGenerate(args, &out, &errOut)
	return out.String(), errOut.String(), err
}

func TestKeysCommand(t *testing.T) {
	if err := runKeys(nil); err == nil {
		t.Fatal("toolyard keys with no subcommand succeeded")
	}
	if err := runKeys([]string{"rotate"}); err == nil || !strings.Contains(err.Error(), "unknown subcommand") {
		t.Fatalf("unknown subcommand: %v", err)
	}
	if err := runKeys([]string{"help"}); err != nil {
		t.Fatalf("help: %v", err)
	}
}

// -name without -out prints exactly one key line on stdout, in the form the
// loader reads, and a different key each time; the key id goes to stderr.
func TestKeysGenerateToStdout(t *testing.T) {
	seen := map[string]bool{}
	for _, name := range []string{"identity", "oauth", "secrets", "session", "identity.key"} {
		out, errOut, err := runKeysGenerateForTest(t, "-name", name)
		if err != nil {
			t.Fatalf("-name %s: %v", name, err)
		}
		if strings.Count(out, "\n") != 1 || !strings.HasSuffix(out, "\n") {
			t.Fatalf("-name %s: stdout is not one line: %q", name, out)
		}
		key, err := sealbox.DecodeKey([]byte(out))
		if err != nil {
			t.Fatalf("-name %s: stdout does not decode as a key: %v", name, err)
		}
		if seen[string(key)] {
			t.Fatal("the same key was generated twice")
		}
		seen[string(key)] = true
		if !strings.Contains(errOut, "key id") || strings.Contains(errOut, strings.TrimSpace(out)) {
			t.Fatalf("stderr %q: want the key id and never the key", errOut)
		}
	}
}

// -out writes a new 0600 file that keyguard reads, and never overwrites.
func TestKeysGenerateToAFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, keyguard.SecretsKey)
	out, _, err := runKeysGenerateForTest(t, "-name", "secrets", "-out", path)
	if err != nil {
		t.Fatal(err)
	}
	if out != "" {
		t.Fatalf("-out printed to stdout: %q", out)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v %v, want mode 0600", info, err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := keyguard.ReadKeys(keyguard.Config{DataDir: dir, Getenv: noEnv}, []keyguard.Spec{{Name: keyguard.SecretsKey}})
	if err != nil || len(keys[keyguard.SecretsKey]) != sealbox.MasterKeySize {
		t.Fatalf("ReadKeys: %v", err)
	}
	if _, _, err := runKeysGenerateForTest(t, "-name", "secrets", "-out", path); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("-out over an existing file: %v", err)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(after, before) {
		t.Fatal("-out overwrote an existing key file")
	}
}

// -all -dir writes the four keys serve needs; serve -keys=require (LoadAll)
// accepts them on a new database; a second run writes nothing.
func TestKeysGenerateAll(t *testing.T) {
	dir := t.TempDir()
	out, errOut, err := runKeysGenerateForTest(t, "-all", "-dir", dir)
	if err != nil {
		t.Fatal(err)
	}
	if out != "" || strings.Count(errOut, "key id") != 4 {
		t.Fatalf("stdout %q, stderr %q", out, errOut)
	}
	for _, sp := range keyguard.ServeKeys() {
		info, err := os.Stat(filepath.Join(dir, sp.Name))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s: %v %v, want mode 0600", sp.Name, info, err)
		}
	}
	keys, err := keyguard.ReadKeys(keyguard.Config{DataDir: dir, Getenv: noEnv}, keyguard.ServeKeys())
	if err != nil || len(keys) != 4 {
		t.Fatalf("ReadKeys: %d keys, %v", len(keys), err)
	}
	distinct := map[string]bool{}
	for _, k := range keys {
		distinct[string(k)] = true
	}
	if len(distinct) != 4 {
		t.Fatalf("%d distinct keys of 4", len(distinct))
	}

	db, err := store.Open(filepath.Join(t.TempDir(), "toolyard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := keyguard.LoadAll(context.Background(), keyguard.Config{
		DB: db.DB, Dialect: db.Dialect, DataDir: t.TempDir(), CredentialsDir: dir,
		Mode: keyguard.Require, Getenv: noEnv, Logf: t.Logf,
	}, keyguard.ServeKeys()); err != nil {
		t.Fatalf("serve -keys=require with generated keys: %v", err)
	}

	snap := snapDir(t, dir)
	if _, _, err := runKeysGenerateForTest(t, "-all", "-dir", dir); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("-all over existing keys: %v", err)
	}
	sameDir(t, snap, snapDir(t, dir))
}

// One existing key file stops -all before it writes anything.
func TestKeysGenerateAllWritesNothingWhenOneExists(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, keyguard.SessionKey), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runKeysGenerateForTest(t, "-all", "-dir", dir); err == nil {
		t.Fatal("-all with an existing session.key succeeded")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("a refused -all wrote files: %v", entries)
	}
}

func TestKeysGenerateFlags(t *testing.T) {
	dir := t.TempDir()
	for _, c := range []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"nothing", nil, "-name"},
		{"unknown name", []string{"-name", "ops"}, "-name must be"},
		{"all without dir", []string{"-all"}, "-all needs -dir"},
		{"all with name", []string{"-all", "-dir", dir, "-name", "identity"}, "neither -name nor -out"},
		{"all with out", []string{"-all", "-dir", dir, "-out", filepath.Join(dir, "x")}, "neither -name nor -out"},
		{"dir without all", []string{"-name", "identity", "-dir", dir}, "-dir goes with -all"},
		{"missing dir", []string{"-all", "-dir", filepath.Join(dir, "missing")}, "missing"},
		{"stray argument", []string{"-name", "identity", "extra"}, "unexpected arguments"},
		{"unknown flag", []string{"-nope"}, "flag provided but not defined"},
	} {
		t.Run(c.name, func(t *testing.T) {
			out, _, err := runKeysGenerateForTest(t, c.args...)
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("error %v, want one containing %q", err, c.wantErr)
			}
			if out != "" {
				t.Fatalf("a refused run printed a key: %q", out)
			}
		})
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("refused runs wrote files: %v", entries)
	}
}
