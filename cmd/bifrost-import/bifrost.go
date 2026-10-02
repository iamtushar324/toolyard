package main

import (
	"crypto/aes"
	"crypto/cipher"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Bifrost's at-rest envelope (framework/encrypt in maximhq/bifrost): the
// passphrase in BIFROST_ENCRYPTION_KEY goes through argon2id with a fixed
// salt, and each value is base64(nonce || AES-256-GCM ciphertext).
const bifrostSalt = "bifrost-encryption-v1-salt-2024"

// virtualKeyHeader and virtualKeyMarker are how our Bifrost fork marks a
// server that gets each caller's own key (TEC-760). toolyard does the same
// job with identity forwarding on the same header.
const (
	virtualKeyHeader = "x-bk-bifrost-vk"
	virtualKeyMarker = "{{bifrost.virtual_key}}"
)

func bifrostKey(passphrase string) []byte {
	return argon2.IDKey([]byte(passphrase), []byte(bifrostSalt), 1, 64*1024, 4, 32)
}

func bifrostDecrypt(key []byte, ciphertext string) (string, error) {
	if key == nil {
		return "", errors.New("BIFROST_ENCRYPTION_KEY is not set")
	}
	data, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		return "", fmt.Errorf("decode base64: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(data) < gcm.NonceSize() {
		return "", errors.New("ciphertext too short")
	}
	plain, err := gcm.Open(nil, data[:gcm.NonceSize()], data[gcm.NonceSize():], nil)
	if err != nil {
		return "", errors.New("decrypt failed: wrong BIFROST_ENCRYPTION_KEY or damaged value")
	}
	return string(plain), nil
}

// resolveRef turns a stored Bifrost value into the one Bifrost would send:
// "env.NAME" reads the process environment (the Bifrost container's own, when
// run there), and vault references are not supported.
func resolveRef(v string) (string, error) {
	if name, ok := strings.CutPrefix(v, "env."); ok {
		val, ok := os.LookupEnv(name)
		if !ok || val == "" {
			return "", fmt.Errorf("environment variable %s is not set here", name)
		}
		return val, nil
	}
	if strings.HasPrefix(v, "vault.") {
		return "", errors.New("vault references are not supported")
	}
	return v, nil
}

// bifrostClient is one row of config_mcp_clients with its secrets resolved.
// URL and Headers hold live credentials: never print them.
type bifrostClient struct {
	Name                string
	ConnType            string
	AuthType            string
	URL                 string
	Headers             map[string]string
	StdioCommand        string
	StdioArgs           []string
	StdioEnvs           []string
	OAuthConfigID       string
	OAuth               *bifrostOAuthClient
	ToolsToExecute      []string
	ToolsListed         bool
	AllowedExtraHeaders []string
	PerUserHeaderKeys   []string
	HasTLSConfig        bool
	Disabled            bool
	// ReadErr is set when the row could not be read or decrypted; the other
	// fields may then be partial.
	ReadErr string
}

func readBifrost(dbPath, passphrase string) ([]bifrostClient, error) {
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?mode=ro&_busy_timeout=5000")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT name, connection_type, COALESCE(connection_string, ''),
		COALESCE(stdio_config_json, ''), COALESCE(tls_config_json, ''),
		COALESCE(tools_to_execute_json, ''), COALESCE(headers_json, ''),
		COALESCE(allowed_extra_headers_json, ''), COALESCE(auth_type, ''),
		COALESCE(per_user_header_keys_json, ''), COALESCE(disabled, 0),
		COALESCE(encryption_status, ''), COALESCE(oauth_config_id, '')
		FROM config_mcp_clients ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("read config_mcp_clients: %w", err)
	}
	type rawRow struct {
		name, connType, connString, stdio, tls, tools, headers, extra, auth, perUser, encStatus, oauthID string
		disabled                                                                                         int64
	}
	var raws []rawRow
	for rows.Next() {
		var r rawRow
		if err := rows.Scan(&r.name, &r.connType, &r.connString, &r.stdio, &r.tls, &r.tools,
			&r.headers, &r.extra, &r.auth, &r.perUser, &r.disabled, &r.encStatus, &r.oauthID); err != nil {
			rows.Close()
			return nil, err
		}
		raws = append(raws, r)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var key []byte
	if passphrase != "" {
		key = bifrostKey(passphrase)
	}
	oauthClients, err := readOAuthClients(db, key)
	if err != nil {
		return nil, err
	}
	out := make([]bifrostClient, 0, len(raws))
	for _, r := range raws {
		c := bifrostClient{
			Name:     r.name,
			ConnType: r.connType,
			AuthType: r.auth,
			Disabled: r.disabled != 0,
		}
		if r.oauthID != "" {
			c.OAuthConfigID = r.oauthID
			c.OAuth = oauthClients[r.oauthID]
		}
		encrypted := r.encStatus == "encrypted"
		var errs []string

		if cs := r.connString; cs != "" {
			if strings.HasPrefix(cs, "env.") || strings.HasPrefix(cs, "vault.") {
				v, err := resolveRef(cs)
				if err != nil {
					errs = append(errs, "url: "+err.Error())
				}
				c.URL = v
			} else if cs = strings.Trim(cs, `"`); encrypted {
				v, err := bifrostDecrypt(key, cs)
				if err != nil {
					errs = append(errs, "url: "+err.Error())
				}
				c.URL = v
			} else {
				c.URL = cs
			}
		}

		if h := r.headers; h != "" && h != "{}" {
			if encrypted {
				v, err := bifrostDecrypt(key, h)
				if err != nil {
					errs = append(errs, "headers: "+err.Error())
					h = ""
				} else {
					h = v
				}
			}
			if h != "" {
				stored := map[string]string{}
				if err := json.Unmarshal([]byte(h), &stored); err != nil {
					errs = append(errs, "headers: not a JSON object of strings")
				}
				c.Headers = make(map[string]string, len(stored))
				for k, v := range stored {
					val, err := resolveRef(v)
					if err != nil {
						errs = append(errs, "header "+k+": "+err.Error())
						continue
					}
					c.Headers[k] = val
				}
			}
		}

		if r.stdio != "" && r.stdio != "null" {
			var s struct {
				Command string   `json:"command"`
				Args    []string `json:"args"`
				Envs    []string `json:"envs"`
			}
			_ = json.Unmarshal([]byte(r.stdio), &s)
			c.StdioCommand, c.StdioArgs, c.StdioEnvs = s.Command, s.Args, s.Envs
		}
		c.HasTLSConfig = r.tls != "" && r.tls != "null" && r.tls != "{}"
		if r.tools != "" && r.tools != "null" {
			c.ToolsListed = true
			_ = json.Unmarshal([]byte(r.tools), &c.ToolsToExecute)
		}
		if r.extra != "" && r.extra != "null" {
			_ = json.Unmarshal([]byte(r.extra), &c.AllowedExtraHeaders)
		}
		if r.perUser != "" && r.perUser != "null" {
			_ = json.Unmarshal([]byte(r.perUser), &c.PerUserHeaderKeys)
		}
		c.ReadErr = strings.Join(errs, "; ")
		out = append(out, c)
	}
	return out, nil
}

// bifrostOAuthClient is one oauth_configs row: the OAuth app Bifrost signs
// in with, not anyone's token. ClientSecret is a credential: never print it.
type bifrostOAuthClient struct {
	ClientID        string
	ClientSecret    string
	AuthorizeURL    string
	TokenURL        string
	RegistrationURL string
	Scopes          []string
	Status          string
	ReadErr         string
}

// readOAuthClients reads every oauth_configs row by id. client_secret is
// encrypted when it is a literal and encryption is on; client_id and the
// URLs are plain, and either may be an env. reference. Rows whose sign-in
// was "revoked" still hold the client settings, so none are skipped here.
func readOAuthClients(db *sql.DB, key []byte) (map[string]*bifrostOAuthClient, error) {
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'oauth_configs'`).Scan(&n); err != nil || n == 0 {
		return map[string]*bifrostOAuthClient{}, err
	}
	rows, err := db.Query(`SELECT id, COALESCE(client_id, ''), COALESCE(client_secret, ''),
		COALESCE(authorize_url, ''), COALESCE(token_url, ''), COALESCE(registration_url, ''),
		COALESCE(scopes, ''), COALESCE(status, ''), COALESCE(encryption_status, '')
		FROM oauth_configs`)
	if err != nil {
		return nil, fmt.Errorf("read oauth_configs: %w", err)
	}
	type rawRow struct{ id, clientID, secret, authURL, tokenURL, regURL, scopes, status, encStatus string }
	var raws []rawRow
	for rows.Next() {
		var r rawRow
		if err := rows.Scan(&r.id, &r.clientID, &r.secret, &r.authURL, &r.tokenURL, &r.regURL,
			&r.scopes, &r.status, &r.encStatus); err != nil {
			rows.Close()
			return nil, err
		}
		raws = append(raws, r)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make(map[string]*bifrostOAuthClient, len(raws))
	for _, r := range raws {
		o := &bifrostOAuthClient{AuthorizeURL: r.authURL, TokenURL: r.tokenURL, RegistrationURL: r.regURL, Status: r.status}
		var errs []string
		var err error
		if o.ClientID, err = resolveRef(strings.Trim(r.clientID, `"`)); err != nil {
			errs = append(errs, "client_id: "+err.Error())
		}
		switch sec := r.secret; {
		case sec == "":
		case strings.HasPrefix(sec, "env.") || strings.HasPrefix(sec, "vault."):
			if o.ClientSecret, err = resolveRef(sec); err != nil {
				errs = append(errs, "client_secret: "+err.Error())
			}
		case r.encStatus == "encrypted":
			if o.ClientSecret, err = bifrostDecrypt(key, strings.Trim(sec, `"`)); err != nil {
				errs = append(errs, "client_secret: "+err.Error())
			}
		default:
			o.ClientSecret = strings.Trim(sec, `"`)
		}
		if r.scopes != "" && r.scopes != "null" {
			_ = json.Unmarshal([]byte(r.scopes), &o.Scopes)
		}
		o.ReadErr = strings.Join(errs, "; ")
		out[r.id] = o
	}
	return out, nil
}
