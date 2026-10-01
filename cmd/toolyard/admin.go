package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Operator commands drive the dashboard's /v1 API with an operator token
// (see docs/guidelines/operator.md). Credentials the server returns
// (agent tokens, operator tokens) are only ever written to an --out file
// with mode 0600; they are never printed.

const operatorEnv = "TOOLYARD_OPERATOR_TOKEN"
const operatorFileEnv = "TOOLYARD_OPERATOR_TOKEN_FILE"

type opClient struct {
	server string
	token  string
	hc     *http.Client
}

// newOpClient resolves the server and operator token: environment first
// (TOOLYARD_SERVER, TOOLYARD_OPERATOR_TOKEN or TOOLYARD_OPERATOR_TOKEN_FILE),
// then ~/.toolyard/config.json.
func newOpClient() (*opClient, error) {
	cfg, _ := loadConfig()
	if cfg == nil {
		cfg = &Config{}
	}
	server := coalesce(os.Getenv("TOOLYARD_SERVER"), cfg.Server)
	if server == "" {
		return nil, errors.New("no server; run `toolyard server <url>` or set TOOLYARD_SERVER")
	}
	token := strings.TrimSpace(os.Getenv(operatorEnv))
	if token == "" {
		if f := os.Getenv(operatorFileEnv); f != "" {
			b, err := os.ReadFile(f)
			if err != nil {
				return nil, err
			}
			token = strings.TrimSpace(string(b))
		}
	}
	if token == "" {
		token = cfg.OperatorToken
	}
	if token == "" {
		return nil, errors.New("no operator token; run `toolyard admin login --token-file FILE` (see `toolyard admin guide`)")
	}
	return &opClient{server: strings.TrimRight(server, "/"), token: token, hc: &http.Client{Timeout: 120 * time.Second}}, nil
}

// do sends one request. body may be nil. It returns the status and body.
func (c *opClient) do(method, path string, body []byte) (int, []byte, error) {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, c.server+path, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("X-Requested-With", "toolyard-cli")
	req.Header.Set("User-Agent", "toolyard-cli/"+version)
	req.Header.Set("x-toolyard-client", "cli")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	return resp.StatusCode, data, err
}

// call is do + error on non-2xx + JSON decode into out (if non-nil).
func (c *opClient) call(method, path string, in any, out any) error {
	var body []byte
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = b
	}
	status, data, err := c.do(method, path, body)
	if err != nil {
		return err
	}
	if status >= 300 {
		return fmt.Errorf("%s %s: %d: %s", method, path, status, strings.TrimSpace(string(redactJSON(data))))
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

// show prints a response for a human or agent: pretty JSON, credentials redacted.
func (c *opClient) show(method, path string, in any) error {
	var raw json.RawMessage
	if err := c.call(method, path, in, &raw); err != nil {
		return err
	}
	printRedacted(raw)
	return nil
}

func printRedacted(data []byte) {
	data = redactJSON(data)
	var buf bytes.Buffer
	if json.Indent(&buf, data, "", "  ") == nil {
		fmt.Println(buf.String())
		return
	}
	fmt.Println(string(data))
}

// redactJSON replaces every "token" field with a placeholder so a
// credential never lands in an agent's transcript.
func redactJSON(data []byte) []byte {
	var v any
	if json.Unmarshal(data, &v) != nil {
		return data
	}
	var walk func(any) any
	walk = func(x any) any {
		switch t := x.(type) {
		case map[string]any:
			for k, val := range t {
				if k == "token" {
					if s, ok := val.(string); ok && s != "" {
						t[k] = "<redacted: use --out FILE>"
						continue
					}
				}
				t[k] = walk(val)
			}
		case []any:
			for i := range t {
				t[i] = walk(t[i])
			}
		}
		return x
	}
	out, err := json.Marshal(walk(v))
	if err != nil {
		return data
	}
	return out
}

// takeToken writes resp["token"] to path (0600, no overwrite), removes it
// from resp, and prints the rest.
func takeToken(resp map[string]any, path string) error {
	tok, _ := resp["token"].(string)
	if tok == "" {
		return errors.New("response carried no token")
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("%w (the credential was created; revoke or rotate it if you can't recover)", err)
	}
	if _, err := f.WriteString(tok + "\n"); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	delete(resp, "token")
	resp["token_file"] = path
	b, _ := json.Marshal(resp)
	printRedacted(b)
	return nil
}

// readData turns -d's argument into a body: "@file", "-" (stdin) or literal JSON.
func readData(arg string) ([]byte, error) {
	switch {
	case arg == "":
		return nil, nil
	case arg == "-":
		return io.ReadAll(os.Stdin)
	case strings.HasPrefix(arg, "@"):
		return os.ReadFile(arg[1:])
	}
	return []byte(arg), nil
}

func readJSONFile(path string) (map[string]any, error) {
	b, err := readData("@" + path)
	if path == "-" {
		b, err = readData("-")
	}
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return m, nil
}

// splitFlags lets flags follow positional args ("agents create NAME --out F").
func splitFlags(fs *flag.FlagSet, argv []string) ([]string, error) {
	var pos []string
	for len(argv) > 0 {
		if err := fs.Parse(argv); err != nil {
			return nil, err
		}
		argv = fs.Args()
		if len(argv) == 0 {
			break
		}
		pos = append(pos, argv[0])
		argv = argv[1:]
	}
	return pos, nil
}

// ---- toolyard api ----------------------------------------------------------

func runAPI(argv []string) error {
	fs := flag.NewFlagSet("api", flag.ExitOnError)
	method := fs.String("X", "", "HTTP method (default GET, or POST with -d)")
	data := fs.String("d", "", "JSON body, @file, or - for stdin")
	pos, err := splitFlags(fs, argv)
	if err != nil {
		return err
	}
	if len(pos) == 2 {
		*method, pos = pos[0], pos[1:]
	}
	if len(pos) != 1 {
		return errors.New("usage: toolyard api [-X METHOD | METHOD] /v1/PATH [-d JSON|@file|-]")
	}
	body, err := readData(*data)
	if err != nil {
		return err
	}
	m := strings.ToUpper(*method)
	if m == "" {
		m = http.MethodGet
		if body != nil {
			m = http.MethodPost
		}
	}
	c, err := newOpClient()
	if err != nil {
		return err
	}
	status, resp, err := c.do(m, pos[0], body)
	if err != nil {
		return err
	}
	if len(resp) > 0 {
		printRedacted(resp)
	}
	if status >= 300 {
		return fmt.Errorf("HTTP %d", status)
	}
	return nil
}

// ---- toolyard admin ----------------------------------------------------------

func runAdmin(argv []string) error {
	if len(argv) == 0 {
		printAdminUsage()
		return nil
	}
	cmd, rest := argv[0], argv[1:]
	switch cmd {
	case "login":
		return adminLogin(rest)
	case "logout":
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		cfg.OperatorToken = ""
		return saveConfig(cfg)
	case "guide":
		return adminGet("/v1/operator/guide", true)
	case "routes":
		return adminGet("/v1/operator/routes", false)
	case "whoami":
		return adminWhoami()
	case "servers":
		return adminServers(rest)
	case "secrets":
		return adminSecrets(rest)
	case "agents":
		return adminAgents(rest)
	case "tokens":
		return adminTokens(rest)
	case "approvals":
		return adminSimple(rest, "approvals", map[string][2]string{"list": {"GET", "/v1/approvals"}})
	case "tools":
		return adminTools(rest)
	case "settings":
		return adminSettings(rest)
	case "audit":
		fs := flag.NewFlagSet("audit", flag.ExitOnError)
		limit := fs.Int("limit", 50, "rows")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		c, err := newOpClient()
		if err != nil {
			return err
		}
		return c.show("GET", fmt.Sprintf("/v1/audit?limit=%d", *limit), nil)
	case "help", "-h", "--help":
		printAdminUsage()
		return nil
	}
	return fmt.Errorf("unknown admin command %q (try `toolyard admin help`)", cmd)
}

func printAdminUsage() {
	fmt.Println(`toolyard admin — operate Toolyard with an operator token (see: toolyard admin guide)

  login --token-file FILE | --token-stdin   store the operator token in ~/.toolyard/config.json
  whoami | guide | routes
  servers   list | get NAME | add --from FILE | patch NAME --from FILE | reconnect NAME | remove NAME
  secrets   list | request NAME [--description TEXT]
  agents    list | create NAME --out FILE | rotate ID --out FILE | disable ID | enable ID | remove ID
  tokens    list | create NAME --out FILE [--scopes read,write] [--ttl-hours N] | revoke ID
  approvals list
  tools     list | run TOOL [--args JSON]
  settings  get | set KEY VALUE
  audit     [--limit N]

Any other route: toolyard api METHOD /v1/PATH [-d JSON|@file|-]
Env: TOOLYARD_SERVER, TOOLYARD_OPERATOR_TOKEN, TOOLYARD_OPERATOR_TOKEN_FILE`)
}

func adminLogin(argv []string) error {
	fs := flag.NewFlagSet("admin login", flag.ExitOnError)
	file := fs.String("token-file", "", "file holding the operator token")
	stdin := fs.Bool("token-stdin", false, "read the operator token from stdin")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	var raw []byte
	var err error
	switch {
	case *file != "":
		raw, err = os.ReadFile(*file)
	case *stdin:
		raw, err = io.ReadAll(os.Stdin)
	default:
		return errors.New("usage: toolyard admin login --token-file FILE | --token-stdin")
	}
	if err != nil {
		return err
	}
	tok := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(tok, "tyop_") {
		return errors.New("that is not an operator token (tyop_…)")
	}
	cfg, _ := loadConfig()
	if cfg == nil {
		cfg = &Config{}
	}
	cfg.OperatorToken = tok
	if err := saveConfig(cfg); err != nil {
		return err
	}
	return adminWhoami()
}

func adminWhoami() error {
	c, err := newOpClient()
	if err != nil {
		return err
	}
	var me map[string]any
	if err := c.call("GET", "/v1/auth/me", nil, &me); err != nil {
		return err
	}
	fmt.Printf("server %s\nuser   %v (%v, %v)\n", c.server, me["username"], me["email"], me["role"])
	return nil
}

func adminGet(path string, raw bool) error {
	c, err := newOpClient()
	if err != nil {
		return err
	}
	status, data, err := c.do("GET", path, nil)
	if err != nil {
		return err
	}
	if status >= 300 {
		return fmt.Errorf("GET %s: %d: %s", path, status, data)
	}
	if raw {
		fmt.Print(string(data))
		return nil
	}
	printRedacted(data)
	return nil
}

func adminSimple(argv []string, name string, verbs map[string][2]string) error {
	if len(argv) == 0 {
		argv = []string{"list"}
	}
	v, ok := verbs[argv[0]]
	if !ok {
		return fmt.Errorf("unknown %s command %q", name, argv[0])
	}
	c, err := newOpClient()
	if err != nil {
		return err
	}
	return c.show(v[0], v[1], nil)
}

func need(pos []string, n int, usage string) error {
	if len(pos) != n {
		return errors.New("usage: toolyard admin " + usage)
	}
	return nil
}

func adminServers(argv []string) error {
	fs := flag.NewFlagSet("servers", flag.ExitOnError)
	from := fs.String("from", "", "JSON file (or - for stdin) with the server definition")
	pos, err := splitFlags(fs, argv)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		pos = []string{"list"}
	}
	c, err := newOpClient()
	if err != nil {
		return err
	}
	name := ""
	if len(pos) > 1 {
		name = url.PathEscape(pos[1])
	}
	switch pos[0] {
	case "list":
		return c.show("GET", "/v1/servers", nil)
	case "get":
		if err := need(pos, 2, "servers get NAME"); err != nil {
			return err
		}
		var all []map[string]any
		if err := c.call("GET", "/v1/servers", nil, &all); err != nil {
			return err
		}
		for _, s := range all {
			if s["name"] == pos[1] {
				b, _ := json.Marshal(s)
				printRedacted(b)
				return nil
			}
		}
		return fmt.Errorf("no server %q", pos[1])
	case "add":
		if *from == "" {
			return errors.New("usage: toolyard admin servers add --from FILE")
		}
		def, err := readJSONFile(*from)
		if err != nil {
			return err
		}
		return c.show("POST", "/v1/servers", def)
	case "patch":
		if err := need(pos, 2, "servers patch NAME --from FILE"); err != nil {
			return err
		}
		if *from == "" {
			return errors.New("usage: toolyard admin servers patch NAME --from FILE")
		}
		def, err := readJSONFile(*from)
		if err != nil {
			return err
		}
		return c.show("PATCH", "/v1/servers/"+name, def)
	case "reconnect":
		if err := need(pos, 2, "servers reconnect NAME"); err != nil {
			return err
		}
		return c.show("POST", "/v1/servers/"+name+"/reconnect", nil)
	case "remove":
		if err := need(pos, 2, "servers remove NAME"); err != nil {
			return err
		}
		return c.show("DELETE", "/v1/servers/"+name, nil)
	}
	return fmt.Errorf("unknown servers command %q", pos[0])
}

func adminSecrets(argv []string) error {
	fs := flag.NewFlagSet("secrets", flag.ExitOnError)
	desc := fs.String("description", "", "what the secret is for (shown to the owner)")
	pos, err := splitFlags(fs, argv)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		pos = []string{"list"}
	}
	c, err := newOpClient()
	if err != nil {
		return err
	}
	switch pos[0] {
	case "list":
		return c.show("GET", "/v1/secrets", nil)
	case "request":
		if err := need(pos, 2, "secrets request NAME [--description TEXT]"); err != nil {
			return err
		}
		return c.show("POST", "/v1/secrets", map[string]any{"name": pos[1], "description": *desc, "request": true})
	}
	return fmt.Errorf("unknown secrets command %q (values are set by the owner in the dashboard)", pos[0])
}

func adminAgents(argv []string) error {
	fs := flag.NewFlagSet("agents", flag.ExitOnError)
	out := fs.String("out", "", "file for the new agent token (mode 0600)")
	pos, err := splitFlags(fs, argv)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		pos = []string{"list"}
	}
	c, err := newOpClient()
	if err != nil {
		return err
	}
	id := ""
	if len(pos) > 1 {
		id = url.PathEscape(pos[1])
	}
	switch pos[0] {
	case "list":
		return c.show("GET", "/v1/agents", nil)
	case "create", "rotate":
		if err := need(pos, 2, "agents "+pos[0]+" NAME|ID --out FILE"); err != nil {
			return err
		}
		if *out == "" {
			return errors.New("--out FILE is required: the agent token is never printed")
		}
		var resp map[string]any
		if pos[0] == "create" {
			err = c.call("POST", "/v1/agents", map[string]any{"name": pos[1]}, &resp)
		} else {
			err = c.call("POST", "/v1/agents/"+id+"/rotate", map[string]any{}, &resp)
		}
		if err != nil {
			return err
		}
		return takeToken(resp, *out)
	case "disable", "enable":
		if err := need(pos, 2, "agents "+pos[0]+" ID"); err != nil {
			return err
		}
		return c.show("POST", "/v1/agents/"+id+"/"+pos[0], nil)
	case "remove":
		if err := need(pos, 2, "agents remove ID"); err != nil {
			return err
		}
		return c.show("DELETE", "/v1/agents/"+id, nil)
	}
	return fmt.Errorf("unknown agents command %q", pos[0])
}

func adminTokens(argv []string) error {
	fs := flag.NewFlagSet("tokens", flag.ExitOnError)
	out := fs.String("out", "", "file for the new token (mode 0600)")
	scopes := fs.String("scopes", "read,write", "read, write, owner")
	ttl := fs.Float64("ttl-hours", 0, "expiry in hours; 0 never expires")
	pos, err := splitFlags(fs, argv)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		pos = []string{"list"}
	}
	c, err := newOpClient()
	if err != nil {
		return err
	}
	switch pos[0] {
	case "list":
		return c.show("GET", "/v1/operator-tokens", nil)
	case "create":
		if err := need(pos, 2, "tokens create NAME --out FILE [--scopes read,write] [--ttl-hours N]"); err != nil {
			return err
		}
		if *out == "" {
			return errors.New("--out FILE is required: the token is never printed")
		}
		var resp map[string]any
		if err := c.call("POST", "/v1/operator-tokens", map[string]any{
			"name": pos[1], "scopes": strings.Split(*scopes, ","), "ttl_hours": *ttl,
		}, &resp); err != nil {
			return err
		}
		return takeToken(resp, *out)
	case "revoke":
		if err := need(pos, 2, "tokens revoke ID"); err != nil {
			return err
		}
		return c.show("DELETE", "/v1/operator-tokens/"+url.PathEscape(pos[1]), nil)
	}
	return fmt.Errorf("unknown tokens command %q", pos[0])
}

func adminTools(argv []string) error {
	fs := flag.NewFlagSet("tools", flag.ExitOnError)
	args := fs.String("args", "{}", "tool arguments as JSON, @file or -")
	pos, err := splitFlags(fs, argv)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		pos = []string{"list"}
	}
	c, err := newOpClient()
	if err != nil {
		return err
	}
	switch pos[0] {
	case "list":
		return c.show("GET", "/v1/tools", nil)
	case "run":
		if err := need(pos, 2, "tools run TOOL [--args JSON]"); err != nil {
			return err
		}
		raw, err := readData(*args)
		if err != nil {
			return err
		}
		var a map[string]any
		if err := json.Unmarshal(raw, &a); err != nil {
			return fmt.Errorf("--args: %w", err)
		}
		return c.show("POST", "/v1/tools/run", map[string]any{"tool": pos[1], "arguments": a})
	}
	return fmt.Errorf("unknown tools command %q", pos[0])
}

func adminSettings(argv []string) error {
	if len(argv) == 0 {
		argv = []string{"get"}
	}
	c, err := newOpClient()
	if err != nil {
		return err
	}
	switch argv[0] {
	case "get":
		return c.show("GET", "/v1/settings", nil)
	case "set":
		if len(argv) != 3 {
			return errors.New("usage: toolyard admin settings set KEY VALUE")
		}
		var v any
		if json.Unmarshal([]byte(argv[2]), &v) != nil {
			v = argv[2]
		}
		return c.show("PATCH", "/v1/settings", map[string]any{argv[1]: v})
	}
	return fmt.Errorf("unknown settings command %q", argv[0])
}
