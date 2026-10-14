package keyguard

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/sealbox"
)

func insertSealedSecret(t *testing.T, e *env) {
	t.Helper()
	now := time.Now().UnixMilli()
	if _, err := e.db.Exec(`INSERT INTO secrets(name, value_enc, created_at, updated_at) VALUES('API_KEY', 'sealed-bytes', ?, ?)`, now, now); err != nil {
		t.Fatal(err)
	}
}

func loadOrFail(t *testing.T, e *env) {
	t.Helper()
	if _, err := e.load(t, CreateIfFresh); err != nil {
		t.Fatalf("setup load: %v", err)
	}
}

// Check reports per key what LoadAll would do, writes nothing, and fails
// exactly when LoadAll would.
func TestCheckWritesNothingAndAgreesWithLoadAll(t *testing.T) {
	const ok, warn, fail = StatusOK, StatusWarn, StatusFail
	all := func(s string) map[string]string {
		return map[string]string{IdentityKey: s, OAuthKey: s, SecretsKey: s, SessionKey: s}
	}
	with := func(base map[string]string, name, s string) map[string]string {
		out := map[string]string{}
		for k, v := range base {
			out[k] = v
		}
		out[name] = s
		return out
	}
	cases := []struct {
		name   string
		mode   Mode
		setup  func(t *testing.T, e *env)
		want   map[string]string
		detail map[string]string // key -> substring its line must contain
	}{
		{name: "new database", mode: CreateIfFresh, want: all(ok),
			detail: map[string]string{OAuthKey: "first start creates it"}},
		{name: "new database, require", mode: Require, want: all(fail),
			detail: map[string]string{OAuthKey: "-keys=require"}},
		{name: "loaded before", mode: Require, setup: loadOrFail, want: all(ok),
			detail: map[string]string{SecretsKey: "matches its canary"}},
		{name: "existing keys, no canaries", mode: Require,
			setup: func(t *testing.T, e *env) {
				for _, n := range []string{IdentityKey, OAuthKey, SecretsKey, SessionKey} {
					writeKey(t, e.dataDir, n, sealbox.EncodeKey(randomKey(t)))
				}
				insertSealedSecret(t, e)
			},
			want:   all(ok),
			detail: map[string]string{IdentityKey: "adopted on the next start"}},
		{name: "wrong key", mode: CreateIfFresh,
			setup: func(t *testing.T, e *env) {
				loadOrFail(t, e)
				_ = os.Remove(filepath.Join(e.dataDir, SecretsKey))
				writeKey(t, e.dataDir, SecretsKey, sealbox.EncodeKey(randomKey(t)))
			},
			want:   with(all(ok), SecretsKey, fail),
			detail: map[string]string{SecretsKey: "does not match this database"}},
		{name: "missing key with its canary", mode: CreateIfFresh,
			setup: func(t *testing.T, e *env) {
				loadOrFail(t, e)
				_ = os.Remove(filepath.Join(e.dataDir, OAuthKey))
			},
			want:   with(all(ok), OAuthKey, fail),
			detail: map[string]string{OAuthKey: "set up with it"}},
		{name: "missing keys next to sealed rows", mode: CreateIfFresh,
			setup: insertSealedSecret,
			want:  with(all(fail), SessionKey, warn),
			detail: map[string]string{
				IdentityKey: "sealed data (secrets)",
				SessionKey:  "everyone signs in again",
			}},
		{name: "missing session key", mode: CreateIfFresh,
			setup: func(t *testing.T, e *env) {
				loadOrFail(t, e)
				_ = os.Remove(filepath.Join(e.dataDir, SessionKey))
			},
			want: with(all(ok), SessionKey, warn)},
		{name: "corrupt key", mode: CreateIfFresh,
			setup: func(t *testing.T, e *env) {
				writeKey(t, e.dataDir, SecretsKey, []byte("garbage"))
			},
			want:   with(all(ok), SecretsKey, fail),
			detail: map[string]string{SecretsKey: "not a base64-encoded 32-byte key"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			if c.setup != nil {
				c.setup(t, e)
			}
			files := snapshot(t, e.dataDir)
			canaries := canaryRows(t, e)

			reports, err := Check(context.Background(), e.cfg(c.mode), ServeKeys())

			sameFiles(t, files, snapshot(t, e.dataDir))
			after := canaryRows(t, e)
			if len(after) != len(canaries) {
				t.Fatalf("Check changed the canaries: %d before, %d after", len(canaries), len(after))
			}
			for n, row := range canaries {
				if a := after[n]; a.kid != row.kid || !bytes.Equal(a.blob, row.blob) || a.created != row.created {
					t.Fatalf("Check rewrote the canary for %s", n)
				}
			}
			if len(reports) != 4 {
				t.Fatalf("%d reports, want 4", len(reports))
			}
			anyFail := false
			for _, r := range reports {
				if r.Status != c.want[r.Name] {
					t.Errorf("%s: %s (%s), want %s", r.Name, r.Status, r.Detail, c.want[r.Name])
				}
				if sub := c.detail[r.Name]; sub != "" && !strings.Contains(r.Detail, sub) {
					t.Errorf("%s: detail %q does not mention %q", r.Name, r.Detail, sub)
				}
				anyFail = anyFail || r.Status == StatusFail
			}
			if anyFail != (err != nil) {
				t.Fatalf("a FAIL line and the error disagree: %v", err)
			}
			if err != nil && !errors.Is(err, ErrConfig) {
				t.Fatalf("error does not match ErrConfig: %v", err)
			}

			_, loadErr := e.load(t, c.mode)
			if (err == nil) != (loadErr == nil) {
				t.Fatalf("Check error %v, but LoadAll error %v", err, loadErr)
			}
		})
	}
}

func TestCheckNeverShowsKeyMaterial(t *testing.T) {
	e := newEnv(t)
	key := randomKey(t)
	for _, n := range []string{IdentityKey, OAuthKey, SecretsKey, SessionKey} {
		writeKey(t, e.dataDir, n, sealbox.EncodeKey(key))
	}
	reports, err := Check(context.Background(), e.cfg(Require), ServeKeys())
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	enc := string(sealbox.EncodeKey(key))
	for _, r := range reports {
		line := r.Name + r.Path + r.KeyID + r.Detail
		if strings.Contains(line, enc) || strings.Contains(line, enc[:12]) {
			t.Fatalf("%s: report shows key material: %q", r.Name, line)
		}
	}
}

// A database from before canaries has no key_canaries table: every key
// found is reported as adopted on the next start, and nothing is created.
func TestCheckWithoutTheCanaryTable(t *testing.T) {
	e := newEnv(t)
	for _, n := range []string{IdentityKey, OAuthKey, SecretsKey, SessionKey} {
		writeKey(t, e.dataDir, n, sealbox.EncodeKey(randomKey(t)))
	}
	if _, err := e.db.Exec(`DROP TABLE key_canaries`); err != nil {
		t.Fatal(err)
	}
	reports, err := Check(context.Background(), e.cfg(CreateIfFresh), ServeKeys())
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	for _, r := range reports {
		if r.Status != StatusOK || !strings.Contains(r.Detail, "no canary yet") {
			t.Errorf("%s: %s %s", r.Name, r.Status, r.Detail)
		}
	}
	if ok, err := tableExists(context.Background(), e.db.DB, "key_canaries"); err != nil || ok {
		t.Fatalf("Check created the canary table: %v %v", ok, err)
	}
}

func TestCheckWithoutADatabase(t *testing.T) {
	dataDir := t.TempDir()
	cfg := Config{DataDir: dataDir, Mode: CreateIfFresh, Getenv: func(string) string { return "" }}
	reports, err := Check(context.Background(), cfg, ServeKeys())
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	for _, r := range reports {
		if r.Status != StatusOK || !strings.Contains(r.Detail, "first start creates it") {
			t.Errorf("%s: %s %s", r.Name, r.Status, r.Detail)
		}
	}
	if entries, _ := os.ReadDir(dataDir); len(entries) != 0 {
		t.Fatalf("Check created %d files", len(entries))
	}
	cfg.Mode = Require
	if _, err := Check(context.Background(), cfg, ServeKeys()); !errors.Is(err, ErrConfig) {
		t.Fatalf("require without keys: %v", err)
	}
}

func TestCheckNotesAnIgnoredCopy(t *testing.T) {
	e := newEnv(t)
	e.creds = t.TempDir()
	for _, n := range []string{IdentityKey, OAuthKey, SecretsKey, SessionKey} {
		writeKey(t, e.dataDir, n, sealbox.EncodeKey(randomKey(t)))
	}
	writeKey(t, e.creds, OAuthKey, sealbox.EncodeKey(randomKey(t)))
	reports, err := Check(context.Background(), e.cfg(Require), ServeKeys())
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	for _, r := range reports {
		hasNote := strings.Contains(r.Detail, "note:") && strings.Contains(r.Detail, "differs")
		if (r.Name == OAuthKey) != hasNote {
			t.Errorf("%s: %q", r.Name, r.Detail)
		}
		if r.Name == OAuthKey && r.Path != filepath.Join(e.creds, OAuthKey) {
			t.Errorf("oauth.key found at %s, want the credentials dir", r.Path)
		}
	}
	if len(e.logs) != 0 {
		t.Fatalf("Check logged instead of reporting: %q", e.logs)
	}
}
