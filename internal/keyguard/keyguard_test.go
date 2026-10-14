package keyguard

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/sealbox"
	"github.com/tusharbhardwaj/toolyard/internal/store"
	"github.com/tusharbhardwaj/toolyard/internal/store/storetest"
)

type env struct {
	db      *store.DB
	dataDir string
	creds   string // -credentials-dir
	sysd    string // $CREDENTIALS_DIRECTORY
	logs    []string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	return &env{db: storetest.Open(t), dataDir: t.TempDir()}
}

func (e *env) cfg(mode Mode) Config {
	return Config{
		DB:             e.db.DB,
		Dialect:        e.db.Dialect,
		DataDir:        e.dataDir,
		CredentialsDir: e.creds,
		Mode:           mode,
		Getenv: func(k string) string {
			if k == "CREDENTIALS_DIRECTORY" {
				return e.sysd
			}
			return ""
		},
		Logf: func(format string, args ...any) {
			e.logs = append(e.logs, format)
		},
	}
}

func (e *env) load(t *testing.T, mode Mode) (map[string][]byte, error) {
	t.Helper()
	return LoadAll(context.Background(), e.cfg(mode), ServeKeys())
}

func randomKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, sealbox.MasterKeySize)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return k
}

func writeKey(t *testing.T, dir, name string, contents []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), contents, 0o600); err != nil {
		t.Fatal(err)
	}
}

// snapshot records every file in dir with its contents and modification
// time, to prove a load wrote nothing.
type fileState struct {
	data []byte
	mod  time.Time
}

func snapshot(t *testing.T, dir string) map[string]fileState {
	t.Helper()
	out := map[string]fileState{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, en := range entries {
		p := filepath.Join(dir, en.Name())
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		out[en.Name()] = fileState{data: b, mod: info.ModTime()}
	}
	return out
}

func sameFiles(t *testing.T, before, after map[string]fileState) {
	t.Helper()
	if len(before) != len(after) {
		t.Fatalf("files changed: %d before, %d after", len(before), len(after))
	}
	for name, b := range before {
		a, ok := after[name]
		if !ok || !bytes.Equal(a.data, b.data) || !a.mod.Equal(b.mod) {
			t.Fatalf("%s was written", name)
		}
	}
}

type canaryRow struct {
	kid     string
	blob    []byte
	created int64
}

func canaryRows(t *testing.T, e *env) map[string]canaryRow {
	t.Helper()
	rows, err := e.db.Query(`SELECT name, kid, check_blob, created_at FROM key_canaries`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]canaryRow{}
	for rows.Next() {
		var name string
		var c canaryRow
		if err := rows.Scan(&name, &c.kid, &c.blob, &c.created); err != nil {
			t.Fatal(err)
		}
		out[name] = c
	}
	return out
}

func wantConfigError(t *testing.T, err error, contains ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, ErrConfig) {
		t.Fatalf("error does not match ErrConfig: %v", err)
	}
	for _, c := range contains {
		if !strings.Contains(err.Error(), c) {
			t.Fatalf("error %q does not mention %q", err, c)
		}
	}
}

func TestFreshDatabaseCreatesKeysAndCanaries(t *testing.T) {
	e := newEnv(t)
	keys, err := e.load(t, CreateIfFresh)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	names := []string{IdentityKey, OAuthKey, SecretsKey, SessionKey}
	for _, n := range names {
		if len(keys[n]) != sealbox.MasterKeySize {
			t.Fatalf("%s: %d bytes", n, len(keys[n]))
		}
		info, err := os.Stat(filepath.Join(e.dataDir, n))
		if err != nil {
			t.Fatalf("%s not created: %v", n, err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode %v, want 0600", n, info.Mode().Perm())
		}
		b, _ := os.ReadFile(filepath.Join(e.dataDir, n))
		if got, err := sealbox.DecodeKey(b); err != nil || !bytes.Equal(got, keys[n]) {
			t.Fatalf("%s on disk differs from the key returned", n)
		}
	}
	rows := canaryRows(t, e)
	if len(rows) != len(names) {
		t.Fatalf("%d canaries, want %d", len(rows), len(names))
	}
	for _, n := range names {
		if rows[n].kid != KeyID(n, keys[n]) || !openCanary(n, keys[n], rows[n].blob) {
			t.Fatalf("canary for %s does not match its key", n)
		}
	}
}

func TestLoadingNeverWrites(t *testing.T) {
	e := newEnv(t)
	first, err := e.load(t, CreateIfFresh)
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	files := snapshot(t, e.dataDir)
	canaries := canaryRows(t, e)
	for _, mode := range []Mode{CreateIfFresh, Require} {
		again, err := e.load(t, mode)
		if err != nil {
			t.Fatalf("%s load: %v", mode, err)
		}
		for n, k := range first {
			if !bytes.Equal(again[n], k) {
				t.Fatalf("%s: a different key on the second load", n)
			}
		}
		sameFiles(t, files, snapshot(t, e.dataDir))
		after := canaryRows(t, e)
		for n, c := range canaries {
			if a := after[n]; a.kid != c.kid || !bytes.Equal(a.blob, c.blob) || a.created != c.created {
				t.Fatalf("canary for %s was rewritten", n)
			}
		}
	}
}

func TestCorruptKeyIsNeverOverwritten(t *testing.T) {
	e := newEnv(t)
	garbage := []byte("not a key at all\n")
	writeKey(t, e.dataDir, SecretsKey, garbage)
	_, err := e.load(t, CreateIfFresh)
	wantConfigError(t, err, SecretsKey, filepath.Join(e.dataDir, SecretsKey))
	b, _ := os.ReadFile(filepath.Join(e.dataDir, SecretsKey))
	if !bytes.Equal(b, garbage) {
		t.Fatal("corrupt key file was changed")
	}
	// Nothing else was created either: a failed start writes nothing.
	if entries, _ := os.ReadDir(e.dataDir); len(entries) != 1 {
		t.Fatalf("%d files in the data dir after a failed load, want 1", len(entries))
	}
	if n := len(canaryRows(t, e)); n != 0 {
		t.Fatalf("%d canaries written by a failed load", n)
	}
}

func TestUnreadableCredentialsCopyDoesNotFallThrough(t *testing.T) {
	e := newEnv(t)
	e.creds = t.TempDir()
	// A directory where the key file should be cannot be read as a key.
	if err := os.Mkdir(filepath.Join(e.creds, OAuthKey), 0o700); err != nil {
		t.Fatal(err)
	}
	writeKey(t, e.dataDir, OAuthKey, sealbox.EncodeKey(randomKey(t)))
	_, err := e.load(t, CreateIfFresh)
	wantConfigError(t, err, OAuthKey, filepath.Join(e.creds, OAuthKey))
}

func TestCreateKeyFileRefusesAnExistingFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, IdentityKey)
	writeKey(t, dir, IdentityKey, []byte("keep me"))
	if _, err := sealbox.CreateKeyFile(p); err == nil || !errors.Is(err, os.ErrExist) {
		t.Fatalf("CreateKeyFile over an existing file: %v", err)
	}
	if b, _ := os.ReadFile(p); string(b) != "keep me" {
		t.Fatal("existing file was overwritten")
	}
}

func TestCredentialsDirectoriesWin(t *testing.T) {
	e := newEnv(t)
	e.creds = t.TempDir()
	e.sysd = t.TempDir()
	dataKey, credsKey, sysdKey := randomKey(t), randomKey(t), randomKey(t)
	for _, n := range []string{IdentityKey, OAuthKey, SecretsKey, SessionKey} {
		writeKey(t, e.dataDir, n, sealbox.EncodeKey(dataKey))
	}
	writeKey(t, e.creds, SecretsKey, sealbox.EncodeKey(credsKey))
	writeKey(t, e.creds, OAuthKey, sealbox.EncodeKey(credsKey))
	writeKey(t, e.sysd, OAuthKey, sealbox.EncodeKey(sysdKey))
	keys, err := e.load(t, Require)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !bytes.Equal(keys[OAuthKey], sysdKey) {
		t.Fatal("$CREDENTIALS_DIRECTORY did not win over -credentials-dir")
	}
	if !bytes.Equal(keys[SecretsKey], credsKey) {
		t.Fatal("-credentials-dir did not win over the data dir")
	}
	if !bytes.Equal(keys[IdentityKey], dataKey) {
		t.Fatal("the data dir is not the last fallback")
	}
	if got := e.cfg(Require).SearchDirs(); len(got) != 3 || got[0] != filepath.Clean(e.sysd) || got[1] != filepath.Clean(e.creds) || got[2] != filepath.Clean(e.dataDir) {
		t.Fatalf("search order %v", got)
	}
}

func TestWhitespaceAndPaddingAreAccepted(t *testing.T) {
	e := newEnv(t)
	e.creds = t.TempDir()
	keys := map[string][]byte{}
	for i, n := range []string{IdentityKey, OAuthKey, SecretsKey, SessionKey} {
		k := randomKey(t)
		keys[n] = k
		var body string
		switch i % 3 {
		case 0: // a Docker secret written with echo: trailing newline
			body = base64.RawStdEncoding.EncodeToString(k) + "\n"
		case 1: // openssl rand -base64 32: padded, with a newline
			body = base64.StdEncoding.EncodeToString(k) + "\n"
		default: // stray spaces and a CRLF
			body = "  " + base64.RawStdEncoding.EncodeToString(k) + " \r\n"
		}
		writeKey(t, e.creds, n, []byte(body))
	}
	got, err := e.load(t, Require)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	for n, k := range keys {
		if !bytes.Equal(got[n], k) {
			t.Fatalf("%s decoded to a different key", n)
		}
	}
}

func TestMissingKeyWithCanariesRefuses(t *testing.T) {
	e := newEnv(t)
	if _, err := e.load(t, CreateIfFresh); err != nil {
		t.Fatalf("first load: %v", err)
	}
	if err := os.Remove(filepath.Join(e.dataDir, OAuthKey)); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, e.dataDir)
	_, err := e.load(t, CreateIfFresh)
	wantConfigError(t, err, OAuthKey, "set up with it")
	sameFiles(t, before, snapshot(t, e.dataDir))
	if _, err := os.Stat(filepath.Join(e.dataDir, OAuthKey)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a missing key was recreated for a database in use")
	}
}

func TestMissingKeyWithAnyCanaryRefuses(t *testing.T) {
	e := newEnv(t)
	if _, err := e.load(t, CreateIfFresh); err != nil {
		t.Fatalf("first load: %v", err)
	}
	// The database knows other keys but has no canary for this one.
	if _, err := e.db.Exec(`DELETE FROM key_canaries WHERE name = ?`, IdentityKey); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(e.dataDir, IdentityKey)); err != nil {
		t.Fatal(err)
	}
	_, err := e.load(t, CreateIfFresh)
	wantConfigError(t, err, IdentityKey, "already in use")
}

func TestMissingKeyWithSealedRowsRefuses(t *testing.T) {
	e := newEnv(t)
	now := time.Now().UnixMilli()
	if _, err := e.db.Exec(`INSERT INTO secrets(name, value_enc, created_at, updated_at) VALUES('API_KEY', 'sealed-bytes', ?, ?)`, now, now); err != nil {
		t.Fatal(err)
	}
	_, err := e.load(t, CreateIfFresh)
	wantConfigError(t, err, "sealed data", "secrets")
	if entries, _ := os.ReadDir(e.dataDir); len(entries) != 0 {
		t.Fatalf("%d key files created next to sealed rows", len(entries))
	}
	if n := len(canaryRows(t, e)); n != 0 {
		t.Fatalf("%d canaries written by a refused load", n)
	}
}

// Every sealedColumns query runs against the migrated schema (SQLite
// rejects an unknown table or column even on empty tables), and a cleared
// bot token is not sealed data.
func TestSealedTablesMatchTheSchema(t *testing.T) {
	e := newEnv(t)
	// sealedTables skips a table that does not exist (an older schema), so
	// a misspelt table name would be skipped silently: every listed table
	// must exist once the migrations ran.
	for _, c := range sealedColumns {
		if ok, err := tableExists(context.Background(), e.db.DB, e.db.Dialect, c.table); err != nil || !ok {
			t.Fatalf("sealed table %s is not in the schema (%v)", c.table, err)
		}
	}
	got, err := sealedTables(context.Background(), e.db.DB, e.db.Dialect)
	if err != nil || len(got) != 0 {
		t.Fatalf("fresh database: %v %v", got, err)
	}
	// An empty or cleared bot token is not sealed data.
	if _, err := e.db.Exec(`INSERT INTO system_settings(key, value, updated_at) VALUES('telegram_bot_token', '""', 1)`); err != nil {
		t.Fatal(err)
	}
	if got, _ := sealedTables(context.Background(), e.db.DB, e.db.Dialect); len(got) != 0 {
		t.Fatalf("cleared bot token counted as sealed: %v", got)
	}
	if _, err := e.db.Exec(`UPDATE system_settings SET value = '"c2VhbGVk"' WHERE key = 'telegram_bot_token'`); err != nil {
		t.Fatal(err)
	}
	got, err = sealedTables(context.Background(), e.db.DB, e.db.Dialect)
	if err != nil || len(got) != 1 || got[0] != "system_settings" {
		t.Fatalf("sealed bot token: %v %v", got, err)
	}
}

func TestExistingKeysAreAdopted(t *testing.T) {
	e := newEnv(t)
	// An installation from before canaries: key files and sealed rows.
	keys := map[string][]byte{}
	for _, n := range []string{IdentityKey, OAuthKey, SecretsKey, SessionKey} {
		keys[n] = randomKey(t)
		writeKey(t, e.dataDir, n, sealbox.EncodeKey(keys[n]))
	}
	// A secret sealed with the secrets key on disk: adoption first checks
	// that the key opens it.
	sealSecret(t, e, "API_KEY", keys[SecretsKey], time.Now())
	before := snapshot(t, e.dataDir)
	got, err := e.load(t, Require)
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	sameFiles(t, before, snapshot(t, e.dataDir))
	rows := canaryRows(t, e)
	for n, k := range keys {
		if !bytes.Equal(got[n], k) {
			t.Fatalf("%s: a different key was returned", n)
		}
		if !openCanary(n, k, rows[n].blob) || rows[n].kid != KeyID(n, k) {
			t.Fatalf("no matching canary written for %s", n)
		}
	}
}

func TestKeyThatDoesNotMatchItsCanaryRefuses(t *testing.T) {
	e := newEnv(t)
	if _, err := e.load(t, CreateIfFresh); err != nil {
		t.Fatalf("first load: %v", err)
	}
	canaries := canaryRows(t, e)
	other := sealbox.EncodeKey(randomKey(t))
	if err := os.Remove(filepath.Join(e.dataDir, SecretsKey)); err != nil {
		t.Fatal(err)
	}
	writeKey(t, e.dataDir, SecretsKey, other)
	for _, mode := range []Mode{CreateIfFresh, Require} {
		_, err := e.load(t, mode)
		wantConfigError(t, err, "key secrets.key does not match this database", canaries[SecretsKey].kid)
		if b, _ := os.ReadFile(filepath.Join(e.dataDir, SecretsKey)); !bytes.Equal(b, other) {
			t.Fatal("the mismatched key file was changed")
		}
		if after := canaryRows(t, e); !bytes.Equal(after[SecretsKey].blob, canaries[SecretsKey].blob) {
			t.Fatal("the canary was replaced by a mismatched key")
		}
	}
	// The session key is checked too: a replica with another session key
	// would reject every other replica's cookies.
	if err := os.Remove(filepath.Join(e.dataDir, SessionKey)); err != nil {
		t.Fatal(err)
	}
	writeKey(t, e.dataDir, SessionKey, sealbox.EncodeKey(randomKey(t)))
	if err := os.Remove(filepath.Join(e.dataDir, SecretsKey)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Exec(`DELETE FROM key_canaries WHERE name = ?`, SecretsKey); err != nil {
		t.Fatal(err)
	}
	_, err := e.load(t, CreateIfFresh)
	wantConfigError(t, err, SessionKey)
}

func TestRequireNeverCreates(t *testing.T) {
	e := newEnv(t)
	_, err := e.load(t, Require)
	wantConfigError(t, err, "-keys=require", IdentityKey, SessionKey, e.dataDir)
	if entries, _ := os.ReadDir(e.dataDir); len(entries) != 0 {
		t.Fatalf("-keys=require created %d files", len(entries))
	}
	if n := len(canaryRows(t, e)); n != 0 {
		t.Fatalf("-keys=require wrote %d canaries", n)
	}
}

func TestMissingSessionKeyIsRecreatedWithAWarning(t *testing.T) {
	e := newEnv(t)
	first, err := e.load(t, CreateIfFresh)
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	if err := os.Remove(filepath.Join(e.dataDir, SessionKey)); err != nil {
		t.Fatal(err)
	}
	// -keys=require still refuses.
	if _, err := e.load(t, Require); err == nil {
		t.Fatal("-keys=require recreated the session key")
	}
	e.logs = nil
	second, err := e.load(t, CreateIfFresh)
	if err != nil {
		t.Fatalf("recreate session key: %v", err)
	}
	if bytes.Equal(second[SessionKey], first[SessionKey]) {
		t.Fatal("session key was not replaced")
	}
	for _, n := range []string{IdentityKey, OAuthKey, SecretsKey} {
		if !bytes.Equal(second[n], first[n]) {
			t.Fatalf("%s changed", n)
		}
	}
	warned := false
	for _, l := range e.logs {
		warned = warned || strings.HasPrefix(l, "warning:")
	}
	if !warned {
		t.Fatalf("no warning logged: %q", e.logs)
	}
	// Its canary now matches the new key, so the next start is clean.
	if row := canaryRows(t, e)[SessionKey]; !openCanary(SessionKey, second[SessionKey], row.blob) {
		t.Fatal("session key canary was not replaced")
	}
	if _, err := e.load(t, Require); err != nil {
		t.Fatalf("load after recreation: %v", err)
	}
}

func TestParseMode(t *testing.T) {
	for in, want := range map[string]Mode{"create-if-fresh": CreateIfFresh, " Require ": Require} {
		if got, err := ParseMode(in); err != nil || got != want {
			t.Errorf("ParseMode(%q) = %q, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "create", "always"} {
		if _, err := ParseMode(in); !errors.Is(err, ErrConfig) {
			t.Errorf("ParseMode(%q) accepted", in)
		}
	}
}

func TestKeyIDIsStableAndPerName(t *testing.T) {
	k := randomKey(t)
	if KeyID(OAuthKey, k) != KeyID(OAuthKey, k) {
		t.Fatal("KeyID not stable")
	}
	if KeyID(OAuthKey, k) == KeyID(SecretsKey, k) {
		t.Fatal("KeyID does not depend on the key name")
	}
	if strings.Contains(KeyID(OAuthKey, k), base64.RawStdEncoding.EncodeToString(k)[:8]) {
		t.Fatal("KeyID shows key material")
	}
}

func TestEnvOrFile(t *testing.T) {
	dir := t.TempDir()
	const key = "TOOLYARD_KEYGUARD_TEST_SECRET"
	t.Setenv(key, "")
	t.Setenv(key+"_FILE", "")
	if v, err := EnvOrFile(key); err != nil || v != "" {
		t.Fatalf("neither set: %q %v", v, err)
	}
	t.Setenv(key, "  from-env \n")
	if v, err := EnvOrFile(key); err != nil || v != "from-env" {
		t.Fatalf("env: %q %v", v, err)
	}
	p := filepath.Join(dir, "secret")
	if err := os.WriteFile(p, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(key+"_FILE", p)
	if _, err := EnvOrFile(key); !errors.Is(err, ErrConfig) {
		t.Fatalf("both set: %v", err)
	}
	t.Setenv(key, "")
	if v, err := EnvOrFile(key); err != nil || v != "from-file" {
		t.Fatalf("file: %q %v", v, err)
	}
	if err := os.WriteFile(p, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := EnvOrFile(key); !errors.Is(err, ErrConfig) {
		t.Fatalf("empty file: %v", err)
	}
	t.Setenv(key+"_FILE", filepath.Join(dir, "missing"))
	if _, err := EnvOrFile(key); !errors.Is(err, ErrConfig) {
		t.Fatalf("missing file: %v", err)
	}
}

func TestLoadEnvFilesNeverOverridesTheEnvironment(t *testing.T) {
	sysd, creds := t.TempDir(), t.TempDir()
	const a, b, c = "TOOLYARD_KEYGUARD_TEST_A", "TOOLYARD_KEYGUARD_TEST_B", "TOOLYARD_KEYGUARD_TEST_C"
	t.Setenv("CREDENTIALS_DIRECTORY", sysd)
	t.Setenv(a, "from-env")
	// Registers cleanup for b and c; they start unset.
	t.Setenv(b, "")
	t.Setenv(c, "")
	if err := os.Unsetenv(b); err != nil {
		t.Fatal(err)
	}
	if err := os.Unsetenv(c); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sysd, EnvFileName), []byte(a+"=from-sysd\n"+b+"=from-sysd\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(creds, EnvFileName), []byte(b+"=from-creds\n"+c+"=from-creds\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadEnvFiles(creds)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	sort.Strings(loaded)
	if len(loaded) != 2 {
		t.Fatalf("loaded %v", loaded)
	}
	if os.Getenv(a) != "from-env" || os.Getenv(b) != "from-sysd" || os.Getenv(c) != "from-creds" {
		t.Fatalf("a=%q b=%q c=%q", os.Getenv(a), os.Getenv(b), os.Getenv(c))
	}
	// A missing directory or file is fine.
	if loaded, err := LoadEnvFiles(filepath.Join(creds, "nope")); err != nil || len(loaded) != 1 {
		t.Fatalf("missing dir: %v %v", loaded, err)
	}
}
