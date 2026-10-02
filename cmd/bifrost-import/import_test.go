package main

import (
	"bytes"
	"database/sql"
	"net/url"
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
		disabled NUMERIC, encryption_status TEXT, oauth_config_id TEXT)`); err != nil {
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
	got, err := readBifrost(path, vectorPassphrase, true)
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
	got, err = readBifrost(path, "", true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got[0].ReadErr, "BIFROST_ENCRYPTION_KEY") {
		t.Fatalf("no key: %q", got[0].ReadErr)
	}
}

func TestPlanRules(t *testing.T) {
	all := []string{"*"}
	opt := planOptions{SecretPrefix: "BIFROST", Existing: map[string]bool{"BkCoreServices": true},
		URLMap: map[string]string{"Mapped": "https://public.example.com/mcp"}}
	cases := []struct {
		c        bifrostClient
		skip     string // substring of the skip reason; "" means imported
		identity bool
		secrets  int
	}{
		{c: bifrostClient{Name: "Shared", ToolsListed: true, ToolsToExecute: all, ConnType: "http", AuthType: "headers", URL: "https://api.example.com/mcp",
			Headers: map[string]string{"X-API-KEY": "k"}}, secrets: 1},
		{c: bifrostClient{Name: "PerPerson", ToolsListed: true, ToolsToExecute: all, ConnType: "http", AuthType: "headers", URL: "https://mcp.example.com/mcp",
			Headers: map[string]string{"x-api-key": "k", "x-bk-bifrost-vk": virtualKeyMarker}}, identity: true, secrets: 1},
		{c: bifrostClient{Name: "BkCoreServices", ToolsListed: true, ToolsToExecute: all, ConnType: "http", URL: "https://mcp.example.com/mcp"}, skip: "already in toolyard"},
		{c: bifrostClient{Name: "Sheets", ConnType: "stdio", StdioCommand: "mcp-google-sheets"}, skip: "local program"},
		{c: bifrostClient{Name: "Old", ConnType: "sse", URL: "https://x.example.com/sse"}, skip: "SSE"},
		{c: bifrostClient{Name: "LinearForUsers", ConnType: "http", AuthType: "per_user_oauth", URL: "https://mcp.linear.app/mcp"}, skip: "own account"},
		{c: bifrostClient{Name: "Hermes", ToolsListed: true, ToolsToExecute: all, ConnType: "http", URL: "http://hermes-mattermost-tool:8000/mcp"}, skip: "private address"},
		{c: bifrostClient{Name: "Mapped", ToolsListed: true, ToolsToExecute: all, ConnType: "http", URL: "http://mcp-server:3100/mcp"}},
		{c: bifrostClient{Name: "Keyed", ToolsListed: true, ToolsToExecute: all, ConnType: "http", URL: "https://x.example.com/mcp?key=abc"}, skip: "carry a credential"},
		{c: bifrostClient{Name: "PathToken", ToolsListed: true, ToolsToExecute: all, ConnType: "http", URL: "https://x.example.com/s/abcdefghijklmnopqrstuvwxyz/mcp"}, skip: "carry a credential"},
		{c: bifrostClient{Name: "Tmpl", ToolsListed: true, ToolsToExecute: all, ConnType: "http", URL: "https://x.example.com/mcp",
			Headers: map[string]string{"X-User": "{{bifrost.user_id}}"}}, skip: "template"},
		{c: bifrostClient{Name: "Broken", ConnType: "http", URL: "https://x.example.com/mcp", ReadErr: "headers: decrypt failed"}, skip: "could not read"},
		{c: bifrostClient{Name: "Off", ConnType: "http", URL: "https://x.example.com/mcp", Disabled: true, ToolsListed: true, ToolsToExecute: all}, skip: "disabled in Bifrost"},
		{c: bifrostClient{Name: "NoTools", ConnType: "http", URL: "https://x.example.com/mcp", ToolsListed: true}, skip: "none of its tools"},
		{c: bifrostClient{Name: "UnsetTools", ConnType: "http", URL: "https://x.example.com/mcp"}, skip: "none of its tools"},
		{c: bifrostClient{Name: "Subset", ConnType: "http", URL: "https://x.example.com/mcp", ToolsListed: true, ToolsToExecute: []string{"a", "b"}}, skip: "-allow-tool-subset"},
		{c: bifrostClient{Name: "OAuthBearer", ConnType: "http", AuthType: "oauth", URL: "https://x.example.com/mcp", ToolsListed: true, ToolsToExecute: all,
			Headers: map[string]string{"Authorization": "Bearer stale", "X-Team": "t1"}}, secrets: 1},
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
	if p := planOne(cases[len(cases)-1].c, opt); p.Server.Headers["Authorization"] != "" {
		t.Error("an Authorization header Bifrost never sends was copied")
	}
	subset := opt
	subset.AllowToolSubset = true
	if p := planOne(bifrostClient{Name: "Subset", ConnType: "http", URL: "https://x.example.com/mcp", ToolsListed: true,
		ToolsToExecute: []string{"a"}}, subset); p.Skip != "" {
		t.Errorf("-allow-tool-subset: still skipped: %s", p.Skip)
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
		{Name: "Shared", ConnType: "http", URL: "https://api.example.com/mcp", ToolsListed: true, ToolsToExecute: []string{"*"},
			Headers: map[string]string{"X-API-KEY": "super-secret-value"}},
		{Name: "Keyed", ConnType: "http", URL: "https://x.example.com/mcp?key=query-secret", ToolsListed: true, ToolsToExecute: []string{"*"}},
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

// Connect warnings quote the upstream URL and sometimes its reply; none of
// the server's credentials survive scrub.
func TestScrubHidesCredentials(t *testing.T) {
	p := planOne(bifrostClient{Name: "Keyed", ConnType: "http", ToolsListed: true, ToolsToExecute: []string{"*"},
		URL:     "https://user:pw12345@x.example.com/s/shorttok9/mcp?key=query-secret",
		Headers: map[string]string{"X-API-KEY": "header-secret-1"}},
		planOptions{SecretPrefix: "BIFROST", AllowURLCredentials: true})
	if p.Skip != "" {
		t.Fatal(p.Skip)
	}
	warning := `init upstream Keyed: failed to send request: Post "https://user:pw12345@x.example.com/s/shorttok9/mcp?key=query-secret": ` +
		`dial tcp: timeout; request failed with status 401: {"error":"bad key header-secret-1","path":"/s/shorttok9/mcp"}`
	out := p.scrub(warning)
	for _, secret := range []string{"pw12345", "query-secret", "shorttok9", "header-secret-1"} {
		if strings.Contains(out, secret) {
			t.Fatalf("scrub left %q in: %s", secret, out)
		}
	}
	if !strings.Contains(out, "dial tcp: timeout") {
		t.Fatalf("scrub removed the useful part: %s", out)
	}
}

// The re-review's cases: a token echoed without its "Bearer ", a bare query
// value, a JSON-escaped URL, and a short token in the path.
func TestScrubEchoesAndEscapes(t *testing.T) {
	p := planOne(bifrostClient{Name: "Echo", ConnType: "http", ToolsListed: true, ToolsToExecute: []string{"*"},
		URL:     "https://x.example.com/mcp?a=1&key=qsecret99",
		Headers: map[string]string{"Authorization": "Bearer sk-live-TOKEN123456"}},
		planOptions{SecretPrefix: "BIFROST", AllowURLCredentials: true})
	if p.Skip != "" {
		t.Fatal(p.Skip)
	}
	for _, text := range []string{
		"request failed with status 401: invalid token sk-live-TOKEN123456",
		"upstream said: bad key qsecret99",
		`Post "https://x.example.com/mcp?a=1\u0026key=qsecret99": dial tcp: timeout`,
	} {
		out := p.scrub(text)
		for _, secret := range []string{"sk-live-TOKEN123456", "qsecret99"} {
			if strings.Contains(out, secret) {
				t.Errorf("scrub left %q in: %s", secret, out)
			}
		}
	}

	short, _ := url.Parse("https://x.example.com/mcp/a1b2c3d4e5f6g7h8")
	if !urlCarriesCredentials(short) {
		t.Error("a 16-character letters-and-digits path segment was not flagged")
	}
	if d := displayURL(short); strings.Contains(d, "a1b2c3d4") {
		t.Errorf("displayURL printed the path token: %s", d)
	}
	plain, _ := url.Parse("https://mcp.example.com/api/mcp")
	if urlCarriesCredentials(plain) {
		t.Error("an ordinary path was flagged")
	}
}

// A long reply is shortened only after scrubbing, so no part of a secret
// that sits on the cut survives.
func TestScrubBeforeClip(t *testing.T) {
	p := planOne(bifrostClient{Name: "Long", ConnType: "http", URL: "https://x.example.com/mcp", ToolsListed: true,
		ToolsToExecute: []string{"*"}, Headers: map[string]string{"X-API-KEY": "sk-live-ABCDEFGHIJKLMNOP"}},
		planOptions{SecretPrefix: "BIFROST"})
	out := p.scrub(strings.Repeat("x", 395) + " sk-live-ABCDEFGHIJKLMNOP")
	if strings.Contains(out, "sk-live") {
		t.Fatalf("part of the secret survived: %s", out[len(out)-40:])
	}
}

func TestReadOAuthClients(t *testing.T) {
	t.Setenv("BK_TEST_CLIENT_ID", "client-from-env")
	path := bifrostDB(t, [][]any{
		{"GoogleDriveForAgent", "http", "https://drivemcp.googleapis.com/mcp/v1", nil, nil, `["*"]`, `{}`, nil, "oauth", nil, 0, "plain_text"},
	})
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE config_mcp_clients SET oauth_config_id = 'cfg-1'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE oauth_configs (id TEXT PRIMARY KEY, client_id TEXT, client_secret TEXT, authorize_url TEXT,
		token_url TEXT, registration_url TEXT, redirect_uri TEXT, scopes TEXT, status TEXT, encryption_status TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO oauth_configs VALUES ('cfg-1', 'env.BK_TEST_CLIENT_ID', ?, 'https://accounts.google.com/o/oauth2/v2/auth',
		'https://oauth2.googleapis.com/token', '', 'https://bifrost/api/oauth/callback', '["drive.readonly"]', 'revoked', 'encrypted')`, vectorURL); err != nil {
		t.Fatal(err)
	}
	db.Close()
	got, err := readBifrost(path, vectorPassphrase, true)
	if err != nil {
		t.Fatal(err)
	}
	o := got[0].OAuth
	if o == nil || o.ReadErr != "" || o.ClientID != "client-from-env" || o.ClientSecret != "https://example.com/mcp" ||
		o.TokenURL != "https://oauth2.googleapis.com/token" || len(o.Scopes) != 1 || o.Status != "revoked" {
		t.Fatalf("oauth client: %+v", o)
	}
}

func TestPlanOAuth(t *testing.T) {
	manual := &bifrostOAuthClient{ClientID: "cid", ClientSecret: "csecret-123456", AuthorizeURL: "https://accounts.google.com/o/oauth2/v2/auth",
		TokenURL: "https://oauth2.googleapis.com/token", Scopes: []string{"s1"}}
	dcr := &bifrostOAuthClient{ClientID: "dyn", AuthorizeURL: "https://mcp.linear.app/authorize", TokenURL: "https://mcp.linear.app/token",
		RegistrationURL: "https://mcp.linear.app/register"}
	drive := "https://drivemcp.googleapis.com/mcp/v1"
	clients := []bifrostClient{
		{Name: "Drive", ConnType: "http", URL: drive, AuthType: "oauth", OAuth: manual},
		{Name: "Sheets", ConnType: "http", URL: drive, AuthType: "oauth", OAuth: manual},
		{Name: "Linear", ConnType: "http", URL: "https://mcp.linear.app/mcp", AuthType: "per_user_oauth", OAuth: dcr},
		{Name: "Missing", ConnType: "http", URL: drive, AuthType: "oauth", OAuth: manual},
		{Name: "NoApp", ConnType: "http", URL: drive, AuthType: "oauth"},
		{Name: "Elsewhere", ConnType: "http", URL: drive, AuthType: "oauth", OAuth: manual},
		{Name: "Unchecked", ConnType: "http", URL: drive, AuthType: "oauth", OAuth: manual},
		{Name: "Local", ConnType: "stdio", AuthType: "oauth", OAuth: manual},
		{Name: "Mapped", ConnType: "http", URL: "http://mcp-server:3100/mcp", AuthType: "oauth", OAuth: manual},
		{Name: "Plain", ConnType: "http", URL: drive, AuthType: "headers"},
	}
	targets := map[string]oauthTarget{
		"Drive": {URL: drive}, "Sheets": {URL: drive, HasClient: true}, "Linear": {URL: "https://mcp.linear.app/mcp"},
		"NoApp": {URL: drive}, "Elsewhere": {URL: "https://someone-else.example.com/mcp"},
		"Unchecked": {URL: drive, CheckErr: "GET /v1/servers/Unchecked/oauth: 500"}, "Local": {URL: drive},
		"Mapped": {URL: "https://mcp.beknown.live/mcp"},
	}
	items := planOAuth(clients, targets, map[string]string{"Mapped": "https://mcp.beknown.live/mcp"})
	want := map[string]string{"Drive": "", "Sheets": "already has", "Linear": "registration endpoint", "Missing": "not in toolyard",
		"NoApp": "no OAuth app", "Elsewhere": "points somewhere else", "Unchecked": "could not check", "Local": "not an HTTP server", "Mapped": ""}
	if len(items) != len(want) {
		t.Fatalf("items: %d", len(items))
	}
	for _, it := range items {
		w := want[it.Server]
		if w == "" && it.Skip != "" || w != "" && !strings.Contains(it.Skip, w) {
			t.Errorf("%s: skip %q, want %q", it.Server, it.Skip, w)
		}
	}
	d := items[0]
	extra, _ := d.Body["extra_authorize_params"].(map[string]string)
	if d.Body["client_secret"] != "csecret-123456" || extra["access_type"] != "offline" || d.Host != "oauth2.googleapis.com" {
		t.Fatalf("drive body: %v host %s", d.Body, d.Host)
	}
	if out := d.scrub("bad client csecret-123456"); strings.Contains(out, "csecret-123456") {
		t.Fatalf("scrub left the client secret: %s", out)
	}
}

func TestTailnetIsPrivate(t *testing.T) {
	for host, want := range map[string]bool{"100.70.17.52": true, "100.64.0.1": true, "100.127.255.254": true,
		"100.63.0.1": false, "100.128.0.1": false, "13.127.140.53": false} {
		if got := privateHost(host); got != want {
			t.Errorf("%s: %v, want %v", host, got, want)
		}
	}
}

func TestStdioSummaryHidesValues(t *testing.T) {
	out := stdioSummary(bifrostClient{StdioCommand: "uvx",
		StdioArgs: []string{"mcp-grafana", "--api-key=glsa_abcdefgh12345678", "-t", "abcdef1234567890xyz",
			"Authorization: Basic dXNlcjpwYXNzd29yZA==", "postgres://app:S3cretPass@db.internal/app?sslmode=require",
			"-k=hunter2", "--password", "correcthorse", "--api-key", "ABCDEFGHIJKLMNOPQRS", "--token", "-Abc123def456ghi789",
			"--Zx9Yq8Wv7Ut6Sr5", "serve", "--debug"},
		StdioEnvs: []string{"GRAFANA_URL", "GRAFANA_API_KEY=glsa_secret_value"}})
	for _, secret := range []string{"glsa_abcdefgh12345678", "abcdef1234567890xyz", "dXNlcjpwYXNzd29yZA", "S3cretPass",
		"hunter2", "correcthorse", "ABCDEFGHIJKLMNOPQRS", "Abc123def456ghi789", "Zx9Yq8Wv7Ut6Sr5", "glsa_secret_value"} {
		if strings.Contains(out, secret) {
			t.Fatalf("stdio summary printed %q: %s", secret, out)
		}
	}
	for _, keep := range []string{"uvx mcp-grafana --api-key=…", "-k=…", "--password …", "serve --debug", "env: GRAFANA_URL, GRAFANA_API_KEY"} {
		if !strings.Contains(out, keep) {
			t.Fatalf("stdio summary lost %q: %s", keep, out)
		}
	}
}

func TestQuotedEnvClientIDIsLiteral(t *testing.T) {
	t.Setenv("BK_TEST_CID", "from-env")
	path := bifrostDB(t, nil)
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE oauth_configs (id TEXT PRIMARY KEY, client_id TEXT, client_secret TEXT, authorize_url TEXT,
		token_url TEXT, registration_url TEXT, redirect_uri TEXT, scopes TEXT, status TEXT, encryption_status TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO oauth_configs (id, client_id) VALUES ('a', '"env.BK_TEST_CID"'), ('b', 'env.BK_TEST_CID')`); err != nil {
		t.Fatal(err)
	}
	got, err := readOAuthClients(db, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got["a"].ClientID != "env.BK_TEST_CID" || got["b"].ClientID != "from-env" {
		t.Fatalf("quoted %q, ref %q", got["a"].ClientID, got["b"].ClientID)
	}
}

func TestPlainHTTPOnlyForLocalTest(t *testing.T) {
	for host, want := range map[string]bool{"172.23.0.1": true, "127.0.0.1": true, "100.70.17.52": false, "toolyard.dev.beknown.live": false} {
		if got := localTestHost(host); got != want {
			t.Errorf("%s: %v, want %v", host, got, want)
		}
	}
}
