// Package marketplace ships a curated catalog of popular MCP servers so the
// dashboard can offer one-tap install. Entries here are recipes only; the
// dashboard builds an upstream config from the recipe + any env values the
// operator types in.
//
// We deliberately keep this list small and high-signal. PRs welcome to add
// servers with stable command / URL spelling.
package marketplace

// EnvVar describes a single environment variable a recipe needs.
type EnvVar struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Required    bool   `json:"required"`
	Secret      bool   `json:"secret"`
	// Default, if any, is pre-filled in the dashboard's input.
	Default string `json:"default,omitempty"`
}

// Entry is one curated MCP server recipe.
type Entry struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Tagline     string   `json:"tagline"`
	Description string   `json:"description"`
	Category    string   `json:"category"`
	Homepage    string   `json:"homepage"`
	Transport   string   `json:"transport"`
	Command     string   `json:"command,omitempty"`
	Args        []string `json:"args,omitempty"`
	URL         string   `json:"url,omitempty"`
	Env         []EnvVar `json:"env,omitempty"`
	// SuggestedName is the default upstream name we propose; the dashboard
	// can override it before posting to /v1/servers.
	SuggestedName string `json:"suggested_name"`
	// Notes is shown beneath the description in the dashboard.
	Notes string `json:"notes,omitempty"`
}

// Catalog returns the curated list. Built once per call so it stays
// inexpensive to serve.
func Catalog() []Entry {
	return []Entry{
		{
			ID:            "context7",
			Name:          "Context7",
			Tagline:       "Up-to-date code documentation for any library",
			Description:   "Looks up real-time, version-pinned documentation for thousands of libraries (npm, PyPI, Go modules, Crates, etc.) so the model isn't guessing from stale training data.",
			Category:      "Documentation",
			Homepage:      "https://context7.com",
			Transport:     "stdio",
			Command:       "npx",
			Args:          []string{"-y", "@upstash/context7-mcp"},
			SuggestedName: "context7",
		},
		{
			ID:            "sequential-thinking",
			Name:          "Sequential Thinking",
			Tagline:       "Structured chain-of-thought helper",
			Description:   "Lets the model break a hard problem into a numbered, revisable sequence of thoughts. No external network or secrets.",
			Category:      "Reasoning",
			Homepage:      "https://github.com/modelcontextprotocol/servers/tree/main/src/sequentialthinking",
			Transport:     "stdio",
			Command:       "npx",
			Args:          []string{"-y", "@modelcontextprotocol/server-sequential-thinking"},
			SuggestedName: "thinking",
		},
		{
			ID:          "filesystem",
			Name:        "Filesystem",
			Tagline:     "Read/write a chosen directory",
			Description: "Exposes file/directory tools rooted at the path(s) you provide. Writes are gated by toolyard's approval flow, so this is safer than a raw shell.",
			Category:    "Local",
			Homepage:    "https://github.com/modelcontextprotocol/servers/tree/main/src/filesystem",
			Transport:   "stdio",
			Command:     "npx",
			Args:        []string{"-y", "@modelcontextprotocol/server-filesystem", "${ROOT}"},
			Env: []EnvVar{
				{Name: "ROOT", Description: "Absolute path the server is allowed to touch", Required: true, Default: "/tmp"},
			},
			SuggestedName: "fs",
			Notes:         "ROOT is substituted into the args; the FS server itself does not read it from env.",
		},
		{
			ID:          "github",
			Name:        "GitHub",
			Tagline:     "Repos, issues, PRs, code search",
			Description: "Reads and writes against your GitHub account using a Personal Access Token. Toolyard holds writes for approval so the model can draft a PR but you decide whether it ships.",
			Category:    "Dev",
			Homepage:    "https://github.com/modelcontextprotocol/servers/tree/main/src/github",
			Transport:   "stdio",
			Command:     "npx",
			Args:        []string{"-y", "@modelcontextprotocol/server-github"},
			Env: []EnvVar{
				{Name: "GITHUB_PERSONAL_ACCESS_TOKEN", Description: "Fine-grained PAT with at least repo + read:org", Required: true, Secret: true},
			},
			SuggestedName: "github",
		},
		{
			ID:          "brave-search",
			Name:        "Brave Search",
			Tagline:     "Web search via Brave's API",
			Description: "Search the web from the model. Requires a free Brave Search API key.",
			Category:    "Web",
			Homepage:    "https://github.com/modelcontextprotocol/servers/tree/main/src/brave-search",
			Transport:   "stdio",
			Command:     "npx",
			Args:        []string{"-y", "@modelcontextprotocol/server-brave-search"},
			Env: []EnvVar{
				{Name: "BRAVE_API_KEY", Description: "From api.search.brave.com", Required: true, Secret: true},
			},
			SuggestedName: "brave",
		},
		{
			ID:            "fetch",
			Name:          "Fetch",
			Tagline:       "HTTP fetch with rendered text",
			Description:   "Pulls down a URL and returns clean text. Good cheap alternative to a full browser MCP for read-only browsing.",
			Category:      "Web",
			Homepage:      "https://github.com/modelcontextprotocol/servers/tree/main/src/fetch",
			Transport:     "stdio",
			Command:       "uvx",
			Args:          []string{"mcp-server-fetch"},
			SuggestedName: "fetch",
			Notes:         "Requires uv (https://github.com/astral-sh/uv).",
		},
		{
			ID:            "linear",
			Name:          "Linear (hosted)",
			Tagline:       "Linear's official MCP server",
			Description:   "Linear runs a hosted MCP endpoint with full OAuth. Toolyard treats it like any other streamable-HTTP upstream.",
			Category:      "Productivity",
			Homepage:      "https://linear.app/changelog/2025-mcp",
			Transport:     "http",
			URL:           "https://mcp.linear.app/mcp",
			SuggestedName: "linear",
			Notes:         "Linear handles auth — open the URL in a browser to grant access.",
		},
		{
			ID:          "slack",
			Name:        "Slack",
			Tagline:     "Read and post to your workspace",
			Description: "Read channels, search messages, post replies. Posting is held for approval by toolyard.",
			Category:    "Productivity",
			Homepage:    "https://github.com/modelcontextprotocol/servers/tree/main/src/slack",
			Transport:   "stdio",
			Command:     "npx",
			Args:        []string{"-y", "@modelcontextprotocol/server-slack"},
			Env: []EnvVar{
				{Name: "SLACK_BOT_TOKEN", Description: "xoxb-… from a Slack app install", Required: true, Secret: true},
				{Name: "SLACK_TEAM_ID", Description: "T0123456 — find with /team-info", Required: true},
			},
			SuggestedName: "slack",
		},
		{
			ID:          "postgres",
			Name:        "Postgres (read-only)",
			Tagline:     "Schema + queries against a PG DB",
			Description: "Read-only SQL over a Postgres connection string. Useful for letting the model explore your data model without putting writes at risk.",
			Category:    "Data",
			Homepage:    "https://github.com/modelcontextprotocol/servers/tree/main/src/postgres",
			Transport:   "stdio",
			Command:     "npx",
			Args:        []string{"-y", "@modelcontextprotocol/server-postgres", "${POSTGRES_CONNECTION_STRING}"},
			Env: []EnvVar{
				{Name: "POSTGRES_CONNECTION_STRING", Description: "postgresql://user:pass@host:5432/db", Required: true, Secret: true},
			},
			SuggestedName: "postgres",
		},
		{
			ID:            "playwright",
			Name:          "Playwright",
			Tagline:       "Headless browser automation",
			Description:   "Drive a real Chromium for navigation, screenshots, and DOM extraction. Heavier than Fetch but handles JavaScript-rendered pages.",
			Category:      "Web",
			Homepage:      "https://github.com/microsoft/playwright-mcp",
			Transport:     "stdio",
			Command:       "npx",
			Args:          []string{"-y", "@playwright/mcp@latest"},
			SuggestedName: "playwright",
			Notes:         "First run downloads ~250 MB of browser binaries.",
		},
	}
}
