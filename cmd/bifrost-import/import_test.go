package main

import (
	"bytes"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

// Made with maximhq/bifrost framework/encrypt (encrypt.Init + encrypt.Encrypt)
// and this passphrase, so a change on either side shows up here.
const (
	vectorPassphrase = "test-passphrase-for-vector-only"
	vectorURL        = "gLjVTc47Q7S823IU22nCDIboWvVeB88DIrkJp6EYVjvYL2y5oPKKXwUIewtDd+pAZXla"
	vectorHeaders    = "N93Tcup9U/tBJr8avRP4+57wtnCigkgtA1vOQydv+MqoJFRZtagolPZsWjzDH8Nr9QyLQo9+1mGhJP3mfFWAkP1sy9FrISTpAK2l8SY1rj1LsFrGhoqnkpmNYdBs"
)

func TestBifrostDecryptMatchesBifrost(t *testing.T) {
	key := bifrostKey(vectorPassphrase)
	got, err := bifrostDecrypt(key, vectorURL)
	if err != nil || got != "https://example.com/mcp" {
		t.Fatalf("url: %q, %v", got, err)
	}
	got, err = bifrostDecrypt(key, vectorHeaders)
	if err != nil || got != `{"X-API-KEY":"k-123","x-bk-bifrost-vk":"{{bifrost.virtual_key}}"}` {
		t.Fatalf("headers: %q, %v", got, err)
	}
	if _, err := bifrostDecrypt(bifrostKey("another passphrase entirely"), vectorURL); err == nil {
		t.Fatal("wrong key decrypted")
	}
}

// bifrostDB writes a config_mcp_clients table with the columns the import
// reads, in Bifrost's layout.
func bifrostDB(t *testing.T, rows [][]any) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE config_mcp_clients (id INTEGER PRIMARY KEY, name TEXT, connection_type TEXT,
		connection_string TEXT, stdio_config_json TEXT, tls_config_json TEXT, tools_to_execute_json TEXT,
		headers_json TEXT, allowed_extra_headers_json TEXT, auth_type TEXT, per_user_header_keys_json TEXT,
		disabled NUMERIC, encryption_status TEXT)`); err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if _, err := db.Exec(`INSERT INTO config_mcp_clients (name, connection_type, connection_string, stdio_config_json,
			tls_config_json, tools_to_execute_json, headers_json, allowed_extra_headers_json, auth_type,
			per_user_header_keys_json, disabled, encryption_status) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, r...); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func TestReadBifrostDecryptsAndResolves(t *testing.T) {
	t.Setenv("BK_TEST_TOKEN", "tok-from-env")
	path := bifrostDB(t, [][]any{
		{"Encrypted", "http", vectorURL, nil, nil, `["*"]`, vectorHeaders, `[]`, "headers", nil, 0, "encrypted"},
		{"EnvRefs", "http", "env.BK_TEST_URL", nil, nil, `["a","b"]`, `{"Authorization":"env.BK_TEST_TOKEN"}`, nil, "headers", nil, 1, "plain_text"},
		{"Local", "stdio", nil, `{"command":"mcp-google-sheets","args":[],"envs":[]}`, nil, `["*"]`, `{}`, nil, "none", nil, 0, "encrypted"},
	})
	t.Setenv("BK_TEST_URL", "https://env.example.com/mcp")
	got, err := readBifrost(path, vectorPassphrase)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("rows: %d", len(got))
	}
	enc, env, local := got[0], got[1], got[2]
	if enc.ReadErr != "" || enc.URL != "https://example.com/mcp" || enc.Headers["X-API-KEY"] != "k-123" ||
		enc.Headers["x-bk-bifrost-vk"] != virtualKeyMarker {
		t.Fatalf("encrypted row: %+v", enc)
	}
	if env.ReadErr != "" || env.URL != "https://env.example.com/mcp" || env.Headers["Authorization"] != "tok-from-env" ||
		!env.Disabled || len(env.ToolsToExecute) != 2 {
		t.Fatalf("env row: %+v", env)
	}
	if local.StdioCommand != "mcp-google-sheets" || local.ReadErr != "" {
		t.Fatalf("stdio row: %+v", local)
	}

	// Without the key, encrypted rows say so instead of failing the run.
	got, err = readBifrost(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got[0].ReadErr, "BIFROST_ENCRYPTION_KEY") {
		t.Fatalf("no key: %q", got[0].ReadErr)
	}
}

func TestPlanRules(t *testing.T) {
	opt := planOptions{SecretPrefix: "BIFROST", Existing: map[string]bool{"BkCoreServices": true},
		URLMap: map[string]string{"Mapped": "https://public.example.com/mcp"}}
	cases := []struct {
		c        bifrostClient
		skip     string // substring of the skip reason; "" means imported
		identity bool
		secrets  int
	}{
		{c: bifrostClient{Name: "Shared", ConnType: "http", AuthType: "headers", URL: "https://api.example.com/mcp",
			Headers: map[string]string{"X-API-KEY": "k"}}, secrets: 1},
		{c: bifrostClient{Name: "PerPerson", ConnType: "http", AuthType: "headers", URL: "https://mcp.example.com/mcp",
			Headers: map[string]string{"x-api-key": "k", "x-bk-bifrost-vk": virtualKeyMarker}}, identity: true, secrets: 1},
		{c: bifrostClient{Name: "BkCoreServices", ConnType: "http", URL: "https://mcp.example.com/mcp"}, skip: "already in toolyard"},
		{c: bifrostClient{Name: "Sheets", ConnType: "stdio", StdioCommand: "mcp-google-sheets"}, skip: "local program"},
		{c: bifrostClient{Name: "Old", ConnType: "sse", URL: "https://x.example.com/sse"}, skip: "SSE"},
		{c: bifrostClient{Name: "LinearForUsers", ConnType: "http", AuthType: "per_user_oauth", URL: "https://mcp.linear.app/mcp"}, skip: "own account"},
		{c: bifrostClient{Name: "Hermes", ConnType: "http", URL: "http://hermes-mattermost-tool:8000/mcp"}, skip: "private address"},
		{c: bifrostClient{Name: "Mapped", ConnType: "http", URL: "http://mcp-server:3100/mcp"}},
		{c: bifrostClient{Name: "Keyed", ConnType: "http", URL: "https://x.example.com/mcp?key=abc"}, skip: "carry a credential"},
		{c: bifrostClient{Name: "PathToken", ConnType: "http", URL: "https://x.example.com/s/abcdefghijklmnopqrstuvwxyz/mcp"}, skip: "carry a credential"},
		{c: bifrostClient{Name: "Tmpl", ConnType: "http", URL: "https://x.example.com/mcp",
			Headers: map[string]string{"X-User": "{{bifrost.user_id}}"}}, skip: "template"},
		{c: bifrostClient{Name: "Broken", ConnType: "http", URL: "https://x.example.com/mcp", ReadErr: "headers: decrypt failed"}, skip: "could not read"},
	}
	for _, tc := range cases {
		p := planOne(tc.c, opt)
		if tc.skip == "" && p.Skip != "" || tc.skip != "" && !strings.Contains(p.Skip, tc.skip) {
			t.Errorf("%s: skip = %q, want %q", tc.c.Name, p.Skip, tc.skip)
			continue
		}
		if tc.skip != "" {
			continue
		}
		if (p.Server.Identity != nil) != tc.identity || len(p.Secrets) != tc.secrets {
			t.Errorf("%s: identity %v secrets %d", tc.c.Name, p.Server.Identity, len(p.Secrets))
		}
		for h, v := range p.Server.Headers {
			if !strings.HasPrefix(v, "secret://") {
				t.Errorf("%s: header %s is not a secret ref", tc.c.Name, h)
			}
		}
	}
	if p := planOne(cases[1].c, opt); p.Server.Headers["x-bk-bifrost-vk"] != "" {
		t.Error("the key-forwarding marker must not be copied as a header")
	}
	if p := planOne(cases[7].c, opt); p.Server.URL != "https://public.example.com/mcp" {
		t.Errorf("url-map not applied: %q", p.Server.URL)
	}
}

func TestSecretName(t *testing.T) {
	for in, want := range map[[2]string]string{
		{"HeyReach", "X-API-KEY"}:     "BIFROST_HEYREACH_X_API_KEY",
		{"Context7", "authorization"}: "BIFROST_CONTEXT7_AUTHORIZATION",
		{"bk docs", "x-api-key"}:      "BIFROST_BK_DOCS_X_API_KEY",
	} {
		if got := secretName("BIFROST", in[0], in[1]); got != want {
			t.Errorf("%v: %s, want %s", in, got, want)
		}
	}
	long := secretName("BIFROST", strings.Repeat("a", 80), "x")
	if len(long) > 64 {
		t.Errorf("too long: %d", len(long))
	}
}

// The plan never prints a header value or a URL's query.
func TestPlanPrintsNoSecrets(t *testing.T) {
	plan := buildPlan([]bifrostClient{
		{Name: "Shared", ConnType: "http", URL: "https://api.example.com/mcp", Headers: map[string]string{"X-API-KEY": "super-secret-value"}},
		{Name: "Keyed", ConnType: "http", URL: "https://x.example.com/mcp?key=query-secret"},
	}, planOptions{SecretPrefix: "BIFROST"})
	var buf bytes.Buffer
	printPlan(&buf, plan, false)
	out := buf.String()
	for _, secret := range []string{"super-secret-value", "query-secret"} {
		if strings.Contains(out, secret) {
			t.Fatalf("plan printed %q:\n%s", secret, out)
		}
	}
	if !strings.Contains(out, "secret://BIFROST_SHARED_X_API_KEY") {
		t.Fatalf("plan missing the secret ref:\n%s", out)
	}
}
