package keyguard

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/tusharbhardwaj/toolyard/internal/oauth"
	"github.com/tusharbhardwaj/toolyard/internal/sealbox"
)

// A key that has no canary yet is adopted only after it opens a value the
// database already holds sealed with it. Otherwise a wrong key file on
// disk at upgrade time would get its canary, and the mistake would be
// locked in.

// probeRows is how many sealed values of one table are tried, newest
// first. The key passes when it opens any one of them: one value written
// by a much older run under a key since lost must not refuse the right
// key, while a wrong key opens none.
const probeRows = 3

// sealedProbe reads values sealed with one key, with the AAD the owning
// package uses for them, so the key can be tried against real data. query
// selects the AAD parts, then the sealed value. The formats are copied from
// the owning packages; TestSealedProbeFormatsMatchTheirPackages fails when
// one of them changes.
type sealedProbe struct {
	key   string
	table string
	query string
	aad   func(parts []string) []byte
	// json: the sealed value is stored as a JSON string (system_settings).
	json bool
}

var sealedProbes = []sealedProbe{
	// internal/identitykeys: AAD = user id.
	{key: IdentityKey, table: "user_identity_keys",
		query: `SELECT user_id, key_sealed FROM user_identity_keys WHERE COALESCE(key_sealed, '') <> '' ORDER BY updated_at DESC LIMIT 3`,
		aad:   func(p []string) []byte { return []byte(p[0]) }},
	// internal/oauth: AAD(upstream, kind) and UserAAD(upstream, user, kind).
	{key: OAuthKey, table: "oauth_tokens",
		query: `SELECT upstream_name, access_token_enc FROM oauth_tokens WHERE COALESCE(access_token_enc, '') <> '' ORDER BY COALESCE(last_refresh_at, obtained_at, 0) DESC LIMIT 3`,
		aad:   func(p []string) []byte { return oauth.AAD(p[0], "access") }},
	{key: OAuthKey, table: "oauth_tokens",
		query: `SELECT upstream_name, refresh_token_enc FROM oauth_tokens WHERE COALESCE(refresh_token_enc, '') <> '' ORDER BY COALESCE(last_refresh_at, obtained_at, 0) DESC LIMIT 3`,
		aad:   func(p []string) []byte { return oauth.AAD(p[0], "refresh") }},
	{key: OAuthKey, table: "oauth_clients",
		query: `SELECT upstream_name, client_secret_enc FROM oauth_clients WHERE COALESCE(client_secret_enc, '') <> '' ORDER BY updated_at DESC LIMIT 3`,
		aad:   func(p []string) []byte { return oauth.AAD(p[0], "client_secret") }},
	{key: OAuthKey, table: "oauth_user_tokens",
		query: `SELECT upstream_name, user_id, access_token_enc FROM oauth_user_tokens WHERE COALESCE(access_token_enc, '') <> '' ORDER BY COALESCE(last_refresh_at, obtained_at, 0) DESC LIMIT 3`,
		aad:   func(p []string) []byte { return oauth.UserAAD(p[0], p[1], "access") }},
	{key: OAuthKey, table: "oauth_user_tokens",
		query: `SELECT upstream_name, user_id, refresh_token_enc FROM oauth_user_tokens WHERE COALESCE(refresh_token_enc, '') <> '' ORDER BY COALESCE(last_refresh_at, obtained_at, 0) DESC LIMIT 3`,
		aad:   func(p []string) []byte { return oauth.UserAAD(p[0], p[1], "refresh") }},
	// internal/secrets: aad(name).
	{key: SecretsKey, table: "secrets",
		query: `SELECT name, value_enc FROM secrets WHERE COALESCE(value_enc, '') <> '' ORDER BY updated_at DESC LIMIT 3`,
		aad:   func(p []string) []byte { return []byte("toolyard.secrets.v1|" + p[0] + "|value") }},
	// internal/callbacks: "callback|" + receiver id.
	{key: SecretsKey, table: "callback_receivers",
		query: `SELECT id, secret_enc FROM callback_receivers WHERE COALESCE(secret_enc, '') <> '' ORDER BY created_at DESC LIMIT 3`,
		aad:   func(p []string) []byte { return []byte("callback|" + p[0]) }},
	// internal/federation: "federation|issuer|subject", localAAD, hostAAD.
	{key: SecretsKey, table: "federation_connections",
		query: `SELECT issuer, subject, credential_enc FROM federation_connections WHERE COALESCE(credential_enc, '') <> '' ORDER BY expires_at DESC LIMIT 3`,
		aad:   func(p []string) []byte { return []byte("federation|" + p[0] + "|" + p[1]) }},
	{key: SecretsKey, table: "local_connections",
		query: `SELECT environment_id, local_user_id, credential_enc FROM local_connections WHERE COALESCE(credential_enc, '') <> '' ORDER BY expires_at DESC LIMIT 3`,
		aad:   func(p []string) []byte { return []byte("local|" + p[0] + "|" + p[1]) }},
	{key: SecretsKey, table: "host_connections",
		query: `SELECT environment_id, local_user_id, public_key, credential_enc FROM host_connections WHERE COALESCE(credential_enc, '') <> '' ORDER BY updated_at DESC LIMIT 3`,
		aad:   func(p []string) []byte { return []byte("host|" + p[0] + "|" + p[1] + "|" + p[2]) }},
	// internal/chatnotify/telegram: the bot token, sealed under tokenAAD
	// and stored as a JSON string setting.
	{key: SecretsKey, table: "system_settings",
		query: `SELECT value FROM system_settings WHERE key = 'telegram_bot_token' AND value NOT IN ('', '""', 'null') LIMIT 1`,
		aad:   func([]string) []byte { return []byte("toolyard.chat.v1|telegram|bot_token") },
		json:  true},
}

// dataCheck is what trying a key against the data sealed with it found.
type dataCheck struct {
	// tried is how many sealed values were tried; 0 means the database
	// holds none for this key, so there is nothing to check against.
	tried int
	// opened names the table of the value the key opened; "" when none.
	opened string
	// failed names the first table whose values the key could not open.
	failed string
}

// ok reports whether the key may be adopted: it opened a value, or there
// was nothing to try.
func (d dataCheck) ok() bool { return d.tried == 0 || d.opened != "" }

// checkSealedData tries key against the newest values sealed with it. It
// only reads. A table that does not exist (an older schema) is skipped.
func checkSealedData(ctx context.Context, db *sql.DB, name string, key []byte) (dataCheck, error) {
	var d dataCheck
	c, err := sealbox.NewCipher(key)
	if err != nil {
		return d, err
	}
	for _, p := range sealedProbes {
		if p.key != name {
			continue
		}
		exists, err := tableExists(ctx, db, p.table)
		if err != nil {
			return d, fmt.Errorf("keyguard: read %s: %w", p.table, err)
		}
		if !exists {
			continue
		}
		tried, opened, err := tryProbe(ctx, db, c, p)
		if err != nil {
			return d, fmt.Errorf("keyguard: read %s: %w", p.table, err)
		}
		d.tried += tried
		if opened {
			d.opened = p.table
			return d, nil
		}
		if tried > 0 && d.failed == "" {
			d.failed = p.table
		}
	}
	return d, nil
}

func tryProbe(ctx context.Context, db *sql.DB, c *sealbox.Cipher, p sealedProbe) (tried int, opened bool, err error) {
	rows, err := db.QueryContext(ctx, p.query)
	if err != nil {
		return 0, false, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return 0, false, err
	}
	if len(cols) == 0 {
		return 0, false, nil
	}
	for rows.Next() {
		vals := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return tried, false, err
		}
		parts := make([]string, len(cols)-1)
		for i := range parts {
			parts[i] = vals[i].String
		}
		sealed := vals[len(cols)-1].String
		if p.json {
			var s string
			if json.Unmarshal([]byte(sealed), &s) != nil {
				continue // not a stored string: nothing to try
			}
			sealed = s
		}
		if sealed == "" {
			continue
		}
		tried++
		if _, err := c.Open(sealed, p.aad(parts)); err == nil {
			return tried, true, nil
		}
	}
	return tried, false, rows.Err()
}
