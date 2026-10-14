// Package keyguard loads toolyard's master keys and fails closed.
//
// A key is searched for in $CREDENTIALS_DIRECTORY (systemd LoadCredential),
// then in -credentials-dir (for example /run/secrets for Docker secrets),
// then in the data directory, where older versions created it. The first
// copy found is used; surrounding whitespace is ignored. A copy that
// exists but cannot be read or decoded is an error: it is never replaced
// and the search never falls through to an older copy behind it.
//
// Each key has a canary in the database (key_canaries): a constant sealed
// with the key. At startup every key must open its canary, so a wrong key
// file stops the start instead of silently failing to decrypt. A key
// without a canary gets one, which adopts the keys of an existing
// installation on its first start with this version, but only after it
// opens a value the database already holds sealed with it (verify.go), so
// a wrong key on disk at upgrade time is refused rather than locked in.
//
// A missing key is created only in create-if-fresh mode and only for a
// fresh database: no canaries and no sealed rows. The one exception is a
// recreatable key (the session key): losing it loses nothing stored, so
// it is created with a warning and its canary is replaced. In require mode
// nothing is ever created.
package keyguard

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/sealbox"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// Mode is the -keys flag.
type Mode string

const (
	// CreateIfFresh creates missing keys for a fresh database only (the
	// self-hosting default).
	CreateIfFresh Mode = "create-if-fresh"
	// Require never creates a key: every key must be provided.
	Require Mode = "require"
)

// ParseMode parses the -keys flag.
func ParseMode(s string) (Mode, error) {
	switch m := Mode(strings.ToLower(strings.TrimSpace(s))); m {
	case CreateIfFresh, Require:
		return m, nil
	}
	return "", configErrorf("-keys must be %q or %q, not %q", CreateIfFresh, Require, s)
}

// Key file names.
const (
	IdentityKey = "identity.key"
	OAuthKey    = "oauth.key"
	SecretsKey  = "secrets.key"
	SessionKey  = "session.key"
)

// Spec names one key.
type Spec struct {
	Name string
	// Recreatable marks a key whose loss loses nothing stored: in
	// create-if-fresh mode a missing recreatable key is created even for
	// a database in use, with a warning, and its canary is replaced.
	Recreatable bool
}

// ServeKeys are the keys `toolyard serve` loads. The session key only
// signs dashboard cookies: a new one signs everyone out, nothing more.
func ServeKeys() []Spec {
	return []Spec{
		{Name: IdentityKey},
		{Name: OAuthKey},
		{Name: SecretsKey},
		{Name: SessionKey, Recreatable: true},
	}
}

// ErrConfig matches (errors.Is) every key and credential configuration
// error from this package: a missing, unreadable, corrupt or wrong key, a
// refused creation, a bad -keys value, or a bad *_FILE variable.
// cmd/gateway exits with code 78 (EX_CONFIG) on it.
var ErrConfig = errors.New("keyguard: configuration error")

// ConfigError is a configuration error with an operator-facing message.
type ConfigError struct{ msg string }

func (e *ConfigError) Error() string { return e.msg }

// Is makes errors.Is(err, ErrConfig) true.
func (e *ConfigError) Is(target error) bool { return target == ErrConfig }

func configErrorf(format string, args ...any) error {
	return &ConfigError{msg: fmt.Sprintf(format, args...)}
}

// Config is where and how keys are loaded.
type Config struct {
	// DB holds the key_canaries table and the sealed rows.
	DB *sql.DB
	// Dialect is DB's SQL engine; the zero value is SQLite.
	Dialect store.Dialect
	// DataDir is the data directory (-data): the last place searched and
	// the only place a key is created.
	DataDir string
	// CredentialsDir is -credentials-dir, searched after
	// $CREDENTIALS_DIRECTORY. Empty skips it.
	CredentialsDir string
	// Mode is CreateIfFresh or Require.
	Mode Mode
	// Getenv reads CREDENTIALS_DIRECTORY; nil uses os.Getenv.
	Getenv func(string) string
	// Logf logs what was adopted or created, and warnings; nil uses
	// log.Printf.
	Logf func(format string, args ...any)
}

func (c Config) logf(format string, args ...any) {
	if c.Logf != nil {
		c.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

// SearchDirs is the key search order, without blanks or repeats:
// $CREDENTIALS_DIRECTORY, then CredentialsDir, then DataDir.
func (c Config) SearchDirs() []string {
	getenv := c.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	var out []string
	seen := map[string]bool{}
	for _, d := range []string{getenv("CREDENTIALS_DIRECTORY"), c.CredentialsDir, c.DataDir} {
		d = strings.TrimSpace(d)
		if d == "" {
			continue
		}
		d = filepath.Clean(d)
		if !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	return out
}

type canary struct {
	kid  string
	blob []byte
}

type loadedKey struct {
	spec    Spec
	key     []byte
	path    string
	created bool
}

// LoadAll loads every key in specs and checks it against its canary. It
// returns the keys by name, or an error matching ErrConfig that names the
// key and the paths tried. Nothing is written until every key found has
// matched its canary and every missing key is allowed to be created; then
// missing keys are created in DataDir and missing canaries are written.
func LoadAll(ctx context.Context, cfg Config, specs []Spec) (map[string][]byte, error) {
	if cfg.DB == nil {
		return nil, errors.New("keyguard: no database")
	}
	mode, err := ParseMode(string(cfg.Mode))
	if err != nil {
		return nil, err
	}
	canaries, err := readCanaries(ctx, cfg.DB)
	if err != nil {
		return nil, err
	}
	dirs := cfg.SearchDirs()

	var found []loadedKey
	var missing []Spec
	for _, sp := range specs {
		key, path, err := find(cfg, sp.Name, dirs)
		if err != nil {
			return nil, err
		}
		if key == nil {
			missing = append(missing, sp)
			continue
		}
		found = append(found, loadedKey{spec: sp, key: key, path: path})
	}

	// 1. Every key found must match its canary before anything is written.
	// A key without a canary must open the data already sealed with it
	// (if any) before it is adopted.
	for _, l := range found {
		c, ok := canaries[l.spec.Name]
		if ok && !openCanary(l.spec.Name, l.key, c.blob) {
			return nil, configErrorf("key %s does not match this database: %s has key id %s, but the database was set up with key id %s. "+
				"Restore the matching %s, or point -data at the database this key belongs to. Nothing was changed.",
				l.spec.Name, l.path, KeyID(l.spec.Name, l.key), c.kid, l.spec.Name)
		}
		if ok {
			continue
		}
		d, err := checkSealedData(ctx, cfg.DB, cfg.Dialect, l.spec.Name, l.key)
		if err != nil {
			return nil, err
		}
		if !d.ok() {
			return nil, configErrorf("key %s (%s, key id %s) does not open existing data: it opened none of the %d sealed values tried, starting with %s. "+
				"It is not the key this database was sealed with. Restore the matching %s, or point -data at the database this key belongs to. Nothing was changed.",
				l.spec.Name, l.path, KeyID(l.spec.Name, l.key), d.tried, d.failed, l.spec.Name)
		}
	}

	// 2. A missing key may be created only when that loses nothing.
	if len(missing) > 0 {
		if mode == Require {
			names := make([]string, len(missing))
			for i, sp := range missing {
				names[i] = sp.Name
			}
			return nil, configErrorf("key %s not found (searched %s); -keys=require never creates a key. "+
				"Provide the key file, or use -keys=create-if-fresh on a new installation.",
				strings.Join(names, ", "), strings.Join(dirs, ", "))
		}
		var sealed []string
		if len(canaries) == 0 {
			if sealed, err = sealedTables(ctx, cfg.DB, cfg.Dialect); err != nil {
				return nil, err
			}
		}
		for _, sp := range missing {
			tried := triedPaths(sp.Name, dirs)
			c, hasCanary := canaries[sp.Name]
			switch {
			case sp.Recreatable:
				// Allowed: see Spec.Recreatable.
			case hasCanary:
				return nil, configErrorf("key %s not found (tried %s), but this database was set up with it (key id %s). "+
					"Restore the key file: without it, what was sealed with it cannot be read. Nothing was created.",
					sp.Name, tried, c.kid)
			case len(canaries) > 0:
				return nil, configErrorf("key %s not found (tried %s), and this database is already in use (it has key canaries). "+
					"Restore the key file; a key is created only for a new database. Nothing was created.",
					sp.Name, tried)
			case len(sealed) > 0:
				return nil, configErrorf("key %s not found (tried %s), and this database already holds sealed data (%s). "+
					"Restore the key file; a key is created only for a new database. Nothing was created.",
					sp.Name, tried, strings.Join(sealed, ", "))
			}
		}
		if strings.TrimSpace(cfg.DataDir) == "" {
			return nil, configErrorf("keys %s not found and no data directory to create them in", specNames(missing))
		}
		fresh := len(canaries) == 0 && len(sealed) == 0
		for _, sp := range missing {
			path := filepath.Join(cfg.DataDir, sp.Name)
			key, err := sealbox.CreateKeyFile(path)
			if err != nil {
				return nil, configErrorf("create key %s at %s: %v", sp.Name, path, err)
			}
			if fresh {
				cfg.logf("keys: created %s at %s for a new database (key id %s)", sp.Name, path, KeyID(sp.Name, key))
			} else {
				cfg.logf("warning: keys: %s was missing (tried %s); created a new one at %s (key id %s). Everyone must sign in again.",
					sp.Name, triedPaths(sp.Name, dirs), path, KeyID(sp.Name, key))
			}
			found = append(found, loadedKey{spec: sp, key: key, path: path, created: true})
		}
	}

	// 3. Write the canaries that are missing; replace a recreated key's.
	now := time.Now().UnixMilli()
	for _, l := range found {
		_, has := canaries[l.spec.Name]
		if has && !l.created {
			continue // matched in step 1
		}
		blob, err := sealCanary(l.spec.Name, l.key)
		if err != nil {
			return nil, err
		}
		kid := KeyID(l.spec.Name, l.key)
		if has {
			if _, err := cfg.DB.ExecContext(ctx,
				`UPDATE key_canaries SET kid = ?, check_blob = ?, created_at = ? WHERE name = ?`,
				kid, blob, now, l.spec.Name); err != nil {
				return nil, fmt.Errorf("keyguard: replace canary for %s: %w", l.spec.Name, err)
			}
			continue
		}
		if _, err := cfg.DB.ExecContext(ctx,
			`INSERT INTO key_canaries(name, kid, check_blob, created_at) VALUES(?, ?, ?, ?)
			 ON CONFLICT(name) DO NOTHING`,
			l.spec.Name, kid, blob, now); err != nil {
			return nil, fmt.Errorf("keyguard: write canary for %s: %w", l.spec.Name, err)
		}
		// Read it back: another process may have written its own first.
		var got canary
		if err := cfg.DB.QueryRowContext(ctx,
			`SELECT kid, check_blob FROM key_canaries WHERE name = ?`, l.spec.Name).Scan(&got.kid, &got.blob); err != nil {
			return nil, fmt.Errorf("keyguard: read canary for %s: %w", l.spec.Name, err)
		}
		if !openCanary(l.spec.Name, l.key, got.blob) {
			return nil, configErrorf("key %s does not match this database: %s has key id %s, but another process set the database up with key id %s",
				l.spec.Name, l.path, kid, got.kid)
		}
		if !l.created {
			cfg.logf("keys: adopted %s from %s (key id %s); wrote its canary", l.spec.Name, l.path, kid)
		}
	}

	out := make(map[string][]byte, len(found))
	for _, l := range found {
		out[l.spec.Name] = l.key
	}
	return out, nil
}

// ReadKeys reads the key files named in specs for a read-only tool
// (toolyard db verify): the same search order ($CREDENTIALS_DIRECTORY,
// CredentialsDir, DataDir), whitespace trimming and decoding as LoadAll. It
// never creates a key, never writes a canary and never touches the
// database: cfg.DB, cfg.Dialect and cfg.Mode are ignored, and no canary is
// checked (the caller compares what it needs). A copy that exists but
// cannot be read or decoded is an error, as in LoadAll. A missing key is an
// error too, unless its spec is Recreatable (the session key seals
// nothing); a missing recreatable key is left out of the map. Every error
// matches ErrConfig and names paths, never key material.
func ReadKeys(cfg Config, specs []Spec) (map[string][]byte, error) {
	dirs := cfg.SearchDirs()
	out := make(map[string][]byte, len(specs))
	var missing []string
	for _, sp := range specs {
		key, _, err := find(cfg, sp.Name, dirs)
		if err != nil {
			return nil, err
		}
		if key == nil {
			if !sp.Recreatable {
				missing = append(missing, fmt.Sprintf("%s (tried %s)", sp.Name, triedPaths(sp.Name, dirs)))
			}
			continue
		}
		out[sp.Name] = key
	}
	if len(missing) > 0 {
		return nil, configErrorf("key %s not found; nothing was created", strings.Join(missing, "; "))
	}
	return out, nil
}

// find returns the first copy of name in dirs. A copy that exists but
// cannot be read or decoded is an error; a missing key is (nil, "", nil).
func find(cfg Config, name string, dirs []string) ([]byte, string, error) {
	for i, d := range dirs {
		path := filepath.Join(d, name)
		b, err := os.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, "", configErrorf("key %s at %s cannot be read: %v", name, path, err)
		}
		key, err := sealbox.DecodeKey(b)
		if err != nil {
			return nil, "", configErrorf("key %s at %s is not a base64-encoded 32-byte key. The file was left unchanged; restore it from a backup.", name, path)
		}
		// A different copy further down the search order is ignored;
		// say so, since it usually means a half-finished migration.
		for _, other := range dirs[i+1:] {
			op := filepath.Join(other, name)
			if ob, err := os.ReadFile(op); err == nil {
				if okey, err := sealbox.DecodeKey(ob); err != nil || string(okey) != string(key) {
					cfg.logf("warning: keys: %s differs from %s; using %s", op, path, path)
				}
			}
		}
		return key, path, nil
	}
	return nil, "", nil
}

func triedPaths(name string, dirs []string) string {
	paths := make([]string, len(dirs))
	for i, d := range dirs {
		paths[i] = filepath.Join(d, name)
	}
	return strings.Join(paths, ", ")
}

func specNames(specs []Spec) string {
	names := make([]string, len(specs))
	for i, sp := range specs {
		names[i] = sp.Name
	}
	return strings.Join(names, ", ")
}

func readCanaries(ctx context.Context, db *sql.DB) (map[string]canary, error) {
	rows, err := db.QueryContext(ctx, `SELECT name, kid, check_blob FROM key_canaries`)
	if err != nil {
		return nil, fmt.Errorf("keyguard: read key canaries: %w", err)
	}
	defer rows.Close()
	out := map[string]canary{}
	for rows.Next() {
		var name string
		var c canary
		if err := rows.Scan(&name, &c.kid, &c.blob); err != nil {
			return nil, fmt.Errorf("keyguard: read key canaries: %w", err)
		}
		out[name] = c
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("keyguard: read key canaries: %w", err)
	}
	return out, nil
}

// sealedColumns are the places that hold values sealed with a master key.
// A database with any of them non-empty is not fresh. Static SQL only.
var sealedColumns = []struct{ table, where string }{
	{"user_identity_keys", `COALESCE(key_sealed, '') <> ''`},
	{"oauth_clients", `COALESCE(client_secret_enc, '') <> ''`},
	{"oauth_tokens", `COALESCE(access_token_enc, '') <> '' OR COALESCE(refresh_token_enc, '') <> ''`},
	{"oauth_user_tokens", `COALESCE(access_token_enc, '') <> '' OR COALESCE(refresh_token_enc, '') <> ''`},
	{"secrets", `COALESCE(value_enc, '') <> ''`},
	{"callback_receivers", `COALESCE(secret_enc, '') <> ''`},
	{"federation_connections", `COALESCE(credential_enc, '') <> ''`},
	{"local_connections", `COALESCE(credential_enc, '') <> ''`},
	{"host_connections", `COALESCE(credential_enc, '') <> ''`},
	{"system_settings", `key = 'telegram_bot_token' AND value NOT IN ('', '""', 'null')`},
}

// sealedTables names the tables that hold sealed values. A table that
// does not exist (an older schema, before its migration) holds none. Any
// other query error is returned: an unknown state is never treated as
// fresh.
func sealedTables(ctx context.Context, db *sql.DB, d store.Dialect) ([]string, error) {
	var out []string
	for _, c := range sealedColumns {
		ok, err := tableExists(ctx, db, d, c.table)
		if err != nil {
			return nil, fmt.Errorf("keyguard: check %s for sealed rows: %w", c.table, err)
		}
		if !ok {
			continue
		}
		var has bool
		q := `SELECT EXISTS(SELECT 1 FROM ` + c.table + ` WHERE ` + c.where + `)`
		if err := db.QueryRowContext(ctx, q).Scan(&has); err != nil {
			return nil, fmt.Errorf("keyguard: check %s for sealed rows: %w", c.table, err)
		}
		if has {
			out = append(out, c.table)
		}
	}
	return out, nil
}

// tableExists reports whether table exists, on either engine.
func tableExists(ctx context.Context, db *sql.DB, d store.Dialect, table string) (bool, error) {
	return store.TableExists(ctx, db, d, table)
}

const canaryPlaintext = "toolyard key canary v1"

func canaryAAD(name string) []byte { return []byte("toolyard.key-canary.v1|" + name) }

// KeyID is a short, non-secret fingerprint of a key, shown in logs and
// errors and stored with its canary so an operator can tell key files
// apart. It is a domain-separated SHA-256 and reveals nothing usable.
func KeyID(name string, key []byte) string {
	h := sha256.New()
	h.Write([]byte("toolyard.key-id.v1\x00"))
	h.Write([]byte(name))
	h.Write([]byte{0})
	h.Write(key)
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func sealCanary(name string, key []byte) ([]byte, error) {
	c, err := sealbox.NewCipher(key)
	if err != nil {
		return nil, err
	}
	s, err := c.Seal([]byte(canaryPlaintext), canaryAAD(name))
	if err != nil {
		return nil, err
	}
	return []byte(s), nil
}

// CanaryMatches reports whether key opens blob, a key_canaries.check_blob
// written for the key named name: the key is the one the database was set
// up with.
func CanaryMatches(name string, key, blob []byte) bool { return openCanary(name, key, blob) }

func openCanary(name string, key, blob []byte) bool {
	c, err := sealbox.NewCipher(key)
	if err != nil {
		return false
	}
	pt, err := c.Open(string(blob), canaryAAD(name))
	return err == nil && string(pt) == canaryPlaintext
}
