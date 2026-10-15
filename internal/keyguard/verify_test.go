package keyguard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/oauth"
	"github.com/tusharbhardwaj/toolyard/internal/sealbox"
	"github.com/tusharbhardwaj/toolyard/internal/secrets"
)

func cipherFor(t *testing.T, key []byte) *sealbox.Cipher {
	t.Helper()
	c, err := sealbox.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// sealSecret stores a secret through the secrets package itself (so its
// real AAD is used), sealed with key, last updated at at.
func sealSecret(t *testing.T, e *env, name string, key []byte, at time.Time) {
	t.Helper()
	if _, err := secrets.New(e.db, cipherFor(t, key), nil).Create(context.Background(), name, "value-of-"+name, ""); err != nil {
		t.Fatalf("create secret %s: %v", name, err)
	}
	if _, err := e.db.Exec(`UPDATE secrets SET updated_at = ? WHERE name = ?`, at.UnixMilli(), name); err != nil {
		t.Fatal(err)
	}
}

// sealIdentityKey stores a user's identity key row as identitykeys.Issue
// does: sealed with key, AAD = the user id.
func sealIdentityKey(t *testing.T, e *env, key []byte) {
	t.Helper()
	ctx := context.Background()
	u, err := identity.New(e.db).CreateUser(ctx, "owner", "long-enough-password")
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := cipherFor(t, key).Seal([]byte("ag_identity.secret-token"), []byte(u.ID))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	if _, err := e.db.Exec(`INSERT INTO agents(id, name, owner_user, token_hash, kind, created_at) VALUES('ag_identity', 'identity', ?, 'hash-1', 'identity', ?)`, u.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Exec(`INSERT INTO user_identity_keys(user_id, agent_id, key_hash, key_sealed, created_at, updated_at) VALUES(?, 'ag_identity', 'hash-1', ?, ?, ?)`, u.ID, sealed, now, now); err != nil {
		t.Fatal(err)
	}
}

// sealOAuthToken stores an upstream's access token as internal/oauth
// does: sealed with key under oauth.AAD(upstream, "access").
func sealOAuthToken(t *testing.T, e *env, key []byte) {
	t.Helper()
	now := time.Now().UnixMilli()
	if _, err := e.db.Exec(`INSERT INTO upstream_servers(name, transport, url, enabled, last_status, created_at, updated_at) VALUES('github', 'http', 'http://127.0.0.1:1/mcp', 1, 'ok', ?, ?)`, now, now); err != nil {
		t.Fatal(err)
	}
	sealed, err := cipherFor(t, key).Seal([]byte("gho_token"), oauth.AAD("github", "access"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Exec(`INSERT INTO oauth_tokens(upstream_name, access_token_enc, obtained_at) VALUES('github', ?, ?)`, sealed, now); err != nil {
		t.Fatal(err)
	}
}

// sealTelegramToken stores the bot token setting as internal/chatnotify/
// telegram does: sealed under its AAD, saved as a JSON string.
func sealTelegramToken(t *testing.T, e *env, key []byte) {
	t.Helper()
	sealed, err := cipherFor(t, key).Seal([]byte("123:bot"), []byte("toolyard.chat.v1|telegram|bot_token"))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(sealed)
	if _, err := e.db.Exec(`INSERT INTO system_settings(key, value, updated_at) VALUES('telegram_bot_token', ?, 1)`, string(body)); err != nil {
		t.Fatal(err)
	}
}

// A key without a canary is adopted only when it opens the data sealed
// with it; a wrong key on disk is refused before anything is written.
func TestAdoptionRequiresTheKeyToOpenExistingData(t *testing.T) {
	cases := []struct {
		name string
		key  string
		seal func(t *testing.T, e *env, key []byte)
	}{
		{"identity key row", IdentityKey, sealIdentityKey},
		{"oauth token", OAuthKey, sealOAuthToken},
		{"secret", SecretsKey, func(t *testing.T, e *env, key []byte) { sealSecret(t, e, "API_KEY", key, time.Now()) }},
		{"telegram bot token", SecretsKey, sealTelegramToken},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			keys := map[string][]byte{}
			for _, n := range []string{IdentityKey, OAuthKey, SecretsKey, SessionKey} {
				keys[n] = randomKey(t)
				writeKey(t, e.dataDir, n, sealbox.EncodeKey(keys[n]))
			}
			// The data was sealed with a key other than the one on disk.
			c.seal(t, e, randomKey(t))
			before := snapshot(t, e.dataDir)
			for _, mode := range []Mode{Require, CreateIfFresh} {
				_, err := e.load(t, mode)
				if !errors.Is(err, ErrConfig) || !strings.Contains(err.Error(), "does not open existing data") || !strings.Contains(err.Error(), c.key) {
					t.Fatalf("%s: wrong %s adopted or wrong error: %v", mode, c.key, err)
				}
				sameFiles(t, before, snapshot(t, e.dataDir))
				if n := len(canaryRows(t, e)); n != 0 {
					t.Fatalf("%s: %d canaries written for a refused start", mode, n)
				}
			}
			reports, err := Check(context.Background(), e.cfg(Require), ServeKeys())
			if !errors.Is(err, ErrConfig) {
				t.Fatalf("Check passed a wrong key: %v", err)
			}
			for _, r := range reports {
				wantFail := r.Name == c.key
				if (r.Status == StatusFail) != wantFail {
					t.Errorf("Check %s: %s %s", r.Name, r.Status, r.Detail)
				}
				if wantFail && !strings.Contains(r.Detail, "does not open existing data") {
					t.Errorf("Check %s: %s", r.Name, r.Detail)
				}
			}
		})
		t.Run(c.name+", right key", func(t *testing.T) {
			e := newEnv(t)
			keys := map[string][]byte{}
			for _, n := range []string{IdentityKey, OAuthKey, SecretsKey, SessionKey} {
				keys[n] = randomKey(t)
				writeKey(t, e.dataDir, n, sealbox.EncodeKey(keys[n]))
			}
			c.seal(t, e, keys[c.key])
			reports, err := Check(context.Background(), e.cfg(Require), ServeKeys())
			if err != nil {
				t.Fatalf("Check refused the right key: %v", err)
			}
			for _, r := range reports {
				if r.Name == c.key && !strings.Contains(r.Detail, "opens existing data") {
					t.Errorf("Check %s: %s", r.Name, r.Detail)
				}
			}
			if _, err := e.load(t, Require); err != nil {
				t.Fatalf("right key refused: %v", err)
			}
			if row, ok := canaryRows(t, e)[c.key]; !ok || !openCanary(c.key, keys[c.key], row.blob) {
				t.Fatalf("no canary written for the adopted %s", c.key)
			}
		})
	}
}

// One value sealed long ago under a key since lost does not refuse the
// right key: the newest values are tried, and any one that opens passes.
func TestAdoptionToleratesAnOldValueUnderALostKey(t *testing.T) {
	e := newEnv(t)
	keys := map[string][]byte{}
	for _, n := range []string{IdentityKey, OAuthKey, SecretsKey, SessionKey} {
		keys[n] = randomKey(t)
		writeKey(t, e.dataDir, n, sealbox.EncodeKey(keys[n]))
	}
	now := time.Now()
	sealSecret(t, e, "CURRENT", keys[SecretsKey], now.Add(-time.Hour))
	sealSecret(t, e, "STALE", randomKey(t), now) // the newest row
	if _, err := e.load(t, Require); err != nil {
		t.Fatalf("right key refused because of one stale value: %v", err)
	}
}

// The session key seals nothing: it is adopted without a data check.
func TestSessionKeyHasNoDataCheck(t *testing.T) {
	for _, p := range sealedProbes {
		if p.key == SessionKey {
			t.Fatalf("a sealed-data probe names %s", SessionKey)
		}
	}
	e := newEnv(t)
	d, err := checkSealedData(context.Background(), e.db.DB, e.db.Dialect, SessionKey, randomKey(t))
	if err != nil || d.tried != 0 || !d.ok() {
		t.Fatalf("session key data check: %+v %v", d, err)
	}
}

// Every probe query runs against the migrated schema (a misspelt column
// would otherwise stop every upgrade), and every probe's table exists.
func TestSealedProbesMatchTheSchema(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for _, p := range sealedProbes {
		if ok, err := tableExists(ctx, e.db.DB, e.db.Dialect, p.table); err != nil || !ok {
			t.Fatalf("probe table %s is not in the schema (%v)", p.table, err)
		}
		rows, err := e.db.QueryContext(ctx, p.query)
		if err != nil {
			t.Fatalf("probe query for %s: %v", p.table, err)
		}
		_ = rows.Close()
	}
	for _, n := range []string{IdentityKey, OAuthKey, SecretsKey} {
		if d, err := checkSealedData(ctx, e.db.DB, e.db.Dialect, n, randomKey(t)); err != nil || d.tried != 0 {
			t.Fatalf("%s on an empty database: %+v %v", n, d, err)
		}
	}
}

// The AAD formats in sealedProbes are copied from the packages that seal
// the data (oauth's are called directly). Fail when one of them changes.
func TestSealedProbeFormatsMatchTheirPackages(t *testing.T) {
	sources := map[string][]string{
		"../secrets/secrets.go":              {`"toolyard.secrets.v1|" + name + "|value"`},
		"../callbacks/service.go":            {`[]byte("callback|"+r.ID)`, `[]byte("callback|"+id)`},
		"../federation/service.go":           {`[]byte("federation|"+p.Issuer+"|"+p.Subject)`},
		"../federation/local_connections.go": {`[]byte("local|" + env + "|" + user)`},
		"../federation/host_connections.go":  {`[]byte("host|" + env + "|" + user + "|" + key)`},
		"../chatnotify/telegram/telegram.go": {`"toolyard.chat.v1|telegram|bot_token"`, `s.cipher.Open(sealed, []byte(tokenAAD))`},
		"../identitykeys/identitykeys.go":    {`s.cipher.Seal([]byte(token), []byte(userID))`, `s.cipher.Open(sealed, []byte(userID))`},
	}
	for path, wants := range sources {
		b, err := os.ReadFile(filepath.FromSlash(path))
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, w := range wants {
			if !strings.Contains(string(b), w) {
				t.Errorf("%s no longer contains %s: update sealedProbes in verify.go to match", path, w)
			}
		}
	}
	// And the copies build those formats.
	built := map[string]bool{}
	for _, p := range sealedProbes {
		built[string(p.aad([]string{"A", "B", "C"}))] = true
	}
	for _, want := range []string{
		"A", // identity keys: the user id
		"toolyard.secrets.v1|A|value", "callback|A", "federation|A|B", "local|A|B", "host|A|B|C",
		"toolyard.chat.v1|telegram|bot_token",
		string(oauth.AAD("A", "access")), string(oauth.UserAAD("A", "B", "refresh")),
	} {
		if !built[want] {
			t.Errorf("no probe builds the AAD %q", want)
		}
	}
}

// SealedColumns selects every sealed row: no ORDER BY, no LIMIT. Each
// adoption probe is its column's query plus an ORDER BY and LIMIT 3, so
// adoption tries the newest values exactly as before. The slice is a copy.
func TestSealedColumnsSelectEveryRow(t *testing.T) {
	cols := SealedColumns()
	if len(cols) != len(sealedProbes) {
		t.Fatalf("%d sealed columns, %d probes", len(cols), len(sealedProbes))
	}
	for i, c := range cols {
		up := strings.ToUpper(c.Query)
		if strings.Contains(up, "ORDER BY") || strings.Contains(up, "LIMIT") {
			t.Errorf("%s: column query is not every row: %s", c.Table, c.Query)
		}
		if c.Key == SessionKey || c.AAD == nil || c.Table == "" {
			t.Errorf("entry %d: %+v", i, c)
		}
		p := sealedProbes[i]
		if p.key != c.Key || p.table != c.Table || p.json != c.JSON {
			t.Errorf("probe %d does not match its column: %+v", i, p)
		}
		if !strings.HasPrefix(p.query, c.Query+" ") || !strings.HasSuffix(p.query, " LIMIT 3") {
			t.Errorf("probe %d is not its column query plus ORDER BY/LIMIT: %s", i, p.query)
		}
	}
	cols[0].Query = "changed"
	if SealedColumns()[0].Query == "changed" {
		t.Fatal("SealedColumns returned the package's own slice")
	}

	e := newEnv(t)
	ctx := context.Background()
	key := randomKey(t)
	now := time.Now()
	for i := 0; i < 5; i++ {
		sealSecret(t, e, fmt.Sprintf("SECRET_%d", i), key, now.Add(time.Duration(i)*time.Minute))
	}
	count := func(q string) int {
		t.Helper()
		rows, err := e.db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		defer rows.Close()
		n := 0
		for rows.Next() {
			n++
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return n
	}
	for i, c := range SealedColumns() {
		if c.Table != "secrets" {
			_ = count(c.Query) // runs on the schema
			continue
		}
		if n := count(c.Query); n != 5 {
			t.Fatalf("secrets column query: %d rows, want 5", n)
		}
		if n := count(sealedProbes[i].query); n != probeRows {
			t.Fatalf("secrets probe: %d rows, want %d", n, probeRows)
		}
	}
}

// ReadKeys finds keys as LoadAll does but writes nothing: no key file, no
// canary. A missing sealing key is an error; a missing session key is not.
func TestReadKeysNeverWrites(t *testing.T) {
	e := newEnv(t)
	e.creds = t.TempDir()
	e.sysd = t.TempDir()
	keys := map[string][]byte{}
	for _, n := range []string{IdentityKey, OAuthKey, SecretsKey} {
		keys[n] = randomKey(t)
		writeKey(t, e.creds, n, append(sealbox.EncodeKey(keys[n]), '\n'))
	}
	sysdOAuth := randomKey(t)
	writeKey(t, e.sysd, OAuthKey, sealbox.EncodeKey(sysdOAuth))
	before := map[string]map[string]fileState{}
	for _, d := range []string{e.dataDir, e.creds, e.sysd} {
		before[d] = snapshot(t, d)
	}

	cfg := e.cfg(Require)
	cfg.DB = nil // ReadKeys never touches the database
	got, err := ReadKeys(cfg, ServeKeys())
	if err != nil {
		t.Fatalf("ReadKeys: %v", err)
	}
	if !bytes.Equal(got[IdentityKey], keys[IdentityKey]) || !bytes.Equal(got[SecretsKey], keys[SecretsKey]) {
		t.Fatal("ReadKeys returned a different key")
	}
	if !bytes.Equal(got[OAuthKey], sysdOAuth) {
		t.Fatal("$CREDENTIALS_DIRECTORY did not win, as it does for serve")
	}
	if _, ok := got[SessionKey]; ok || len(got) != 3 {
		t.Fatalf("ReadKeys returned %d keys", len(got))
	}
	for d, b := range before {
		sameFiles(t, b, snapshot(t, d))
	}
	if n := len(canaryRows(t, e)); n != 0 {
		t.Fatalf("ReadKeys wrote %d canaries", n)
	}

	if err := os.Remove(filepath.Join(e.creds, SecretsKey)); err != nil {
		t.Fatal(err)
	}
	_, err = ReadKeys(cfg, ServeKeys())
	wantConfigError(t, err, SecretsKey, filepath.Join(e.creds, SecretsKey))
	if _, err := os.Stat(filepath.Join(e.dataDir, SecretsKey)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("ReadKeys created a missing key")
	}

	writeKey(t, e.creds, SecretsKey, []byte("not a key"))
	_, err = ReadKeys(cfg, ServeKeys())
	wantConfigError(t, err, SecretsKey, "not a base64-encoded 32-byte key")
}

func TestCanaryMatches(t *testing.T) {
	e := newEnv(t)
	keys, err := e.load(t, CreateIfFresh)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	rows := canaryRows(t, e)
	for n, k := range keys {
		if !CanaryMatches(n, k, rows[n].blob) {
			t.Fatalf("%s does not match its own canary", n)
		}
		if CanaryMatches(n, randomKey(t), rows[n].blob) {
			t.Fatalf("a random key matches the %s canary", n)
		}
	}
	if CanaryMatches(OAuthKey, keys[SecretsKey], rows[SecretsKey].blob) {
		t.Fatal("a canary matched under another key's name")
	}
}
