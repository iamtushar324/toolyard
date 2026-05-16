package main

import (
	"errors"
	"flag"
	"fmt"
	"strings"
)

// runAuth dispatches `toolyard auth <subcmd>`.
func runAuth(argv []string) error {
	if len(argv) == 0 {
		return errors.New("auth requires a subcommand: login | status | logout")
	}
	sub := argv[0]
	rest := argv[1:]
	switch sub {
	case "login":
		return runAuthLogin(rest)
	case "status":
		return runAuthStatus(rest)
	case "logout":
		return runAuthLogout(rest)
	default:
		return fmt.Errorf("unknown auth subcommand %q", sub)
	}
}

func runAuthLogin(argv []string) error {
	// Pull positional first so `auth login <code> --server URL` works as
	// naturally as `auth login --server URL <code>` (stdlib flag stops at
	// the first positional).
	var code string
	var rest []string
	for _, a := range argv {
		if code == "" && !strings.HasPrefix(a, "-") {
			code = strings.TrimSpace(a)
			continue
		}
		rest = append(rest, a)
	}
	fs := flag.NewFlagSet("auth login", flag.ExitOnError)
	server := fs.String("server", "", "gateway base URL (overrides existing config; required on first login)")
	_ = fs.Parse(rest)
	if code == "" {
		return errors.New("usage: toolyard auth login <enrollment-code> [--server URL]")
	}

	// If --server isn't given, reuse what's already configured. If neither
	// is available, error out — we have nowhere to POST.
	cfg, _ := loadConfig()
	if cfg == nil {
		cfg = &Config{}
	}
	if *server != "" {
		cfg.Server = *server
	}
	if cfg.Server == "" {
		return errors.New("no server configured; pass --server https://host:port")
	}

	client := newClient(cfg.Server, "")
	var out struct {
		AgentID string `json:"agent_id"`
		Token   string `json:"token"`
	}
	if err := client.doJSON("POST", "/v1/agents/exchange", map[string]string{"code": code}, &out); err != nil {
		return fmt.Errorf("exchange: %w", err)
	}
	cfg.Token = out.Token
	cfg.AgentID = out.AgentID

	// Best-effort: resolve the agent's display name for status output.
	authed := newClient(cfg.Server, cfg.Token)
	var who struct {
		AgentID string `json:"agent_id"`
		Name    string `json:"name"`
	}
	if err := authed.doJSON("GET", "/v1/agents/whoami", nil, &who); err == nil {
		cfg.Name = who.Name
	}

	if err := saveConfig(cfg); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	fmt.Printf("logged in as %s (agent_id=%s, server=%s)\n",
		coalesce(cfg.Name, "(unnamed)"), cfg.AgentID, cfg.Server)
	return nil
}

func runAuthStatus(argv []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if cfg.Token == "" {
		fmt.Println("not logged in. Run `toolyard auth login <code>`.")
		return nil
	}
	fmt.Printf("server:   %s\n", cfg.Server)
	fmt.Printf("agent_id: %s\n", cfg.AgentID)
	fmt.Printf("name:     %s\n", coalesce(cfg.Name, "(unknown)"))
	// Verify the token is still good. A 401 here means the gateway
	// revoked the agent (or the token rotation policy expired it).
	client := newClient(cfg.Server, cfg.Token)
	var who struct {
		Name string `json:"name"`
	}
	if err := client.doJSON("GET", "/v1/agents/whoami", nil, &who); err != nil {
		fmt.Println("token check: FAILED —", err)
		return nil
	}
	fmt.Println("token check: ok")
	return nil
}

func runAuthLogout(argv []string) error {
	if err := deleteConfig(); err != nil {
		return err
	}
	fmt.Println("logged out (config deleted).")
	return nil
}

// runServer is `toolyard server <url>` — set the configured gateway URL
// without touching the token. Used when the operator moves their gateway
// to a new host without re-issuing the agent token.
func runServer(argv []string) error {
	if len(argv) != 1 || strings.TrimSpace(argv[0]) == "" {
		return errors.New("usage: toolyard server <url>")
	}
	cfg, _ := loadConfig()
	if cfg == nil {
		cfg = &Config{}
	}
	cfg.Server = strings.TrimRight(argv[0], "/")
	if err := saveConfig(cfg); err != nil {
		return err
	}
	fmt.Printf("server set to %s\n", cfg.Server)
	return nil
}

func coalesce(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
