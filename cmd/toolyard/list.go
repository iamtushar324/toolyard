package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
)

// runList prints the gateway's tool catalog. The dashboard shows the
// same shape; we render a compact table here.
func runList(argv []string) error {
	cfg, err := requireConfig()
	if err != nil {
		return err
	}
	if len(argv) > 0 && (argv[0] == "--json" || argv[0] == "-j") {
		return listJSON(cfg)
	}

	client := newClient(cfg.Server, cfg.Token)
	var entries []map[string]any
	if err := client.doJSON("GET", "/v1/agents/tools", nil, &entries); err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool {
		return asString(entries[i]["name"]) < asString(entries[j]["name"])
	})

	fmt.Printf("%-50s  %s\n", "TOOL", "DESCRIPTION")
	for _, e := range entries {
		name := asString(e["name"])
		desc := truncate(asString(e["description"]), 80)
		fmt.Printf("%-50s  %s\n", name, desc)
	}
	fmt.Printf("\n%d tool(s)\n", len(entries))
	return nil
}

func listJSON(cfg *Config) error {
	client := newClient(cfg.Server, cfg.Token)
	var entries []map[string]any
	if err := client.doJSON("GET", "/v1/agents/tools", nil, &entries); err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(entries)
}

func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n < 1 {
		return ""
	}
	return s[:n-1] + "…"
}
