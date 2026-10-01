package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Config is the per-user CLI state. Stored at ~/.toolyard/config.json
// with mode 0600 — same dir + permissions story as ssh keys.
type Config struct {
	Server  string `json:"server"`
	Token   string `json:"token"`
	AgentID string `json:"agent_id,omitempty"`
	Name    string `json:"name,omitempty"`
	// OperatorToken (tyop_…) authenticates `toolyard admin` and `toolyard api`.
	OperatorToken string `json:"operator_token,omitempty"`
}

func configPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".toolyard", "config.json"), nil
}

func loadConfig() (*Config, error) {
	p, err := configPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errNoConfig
		}
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", p, err)
	}
	return &cfg, nil
}

func saveConfig(cfg *Config) error {
	p, err := configPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func deleteConfig() error {
	p, err := configPath()
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// errNoConfig signals "config file missing." Returned from loadConfig so
// callers can suggest the right next step ("run `toolyard auth login`").
var errNoConfig = errors.New("no toolyard config; run `toolyard auth login <code>`")

// requireConfig loads the config and errors with a helpful message if it
// hasn't been set up.
func requireConfig() (*Config, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	if cfg.Server == "" {
		return nil, errors.New("config has no server; run `toolyard server <url>`")
	}
	if cfg.Token == "" {
		return nil, errors.New("config has no token; run `toolyard auth login <code>`")
	}
	return cfg, nil
}
