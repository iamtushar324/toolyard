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

// AuthChoice is one selectable value for an AuthOption.
type AuthChoice struct {
	Value       string `json:"value"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// AuthOption is a user-facing toggle in the install modal that maps to a
// single extra authorize-URL query param (e.g. Linear's actor=app|user).
type AuthOption struct {
	Param   string       `json:"param"`
	Label   string       `json:"label"`
	Default string       `json:"default"`
	Choices []AuthChoice `json:"choices"`
}

// AuthPreset describes how a catalog entry authenticates. Endpoints are
// compiled in so fresh deployments can complete OAuth with zero discovery.
type AuthPreset struct {
	Issuer                string `json:"issuer,omitempty"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	// Scope is the exact value of the authorize scope param. Opaque to
	// toolyard and MUST NOT contain spaces (Linear separates with commas).
	Scope   string       `json:"scope,omitempty"`
	Options []AuthOption `json:"options,omitempty"`
	// ClientSetupURL is where the operator creates their own OAuth app.
	ClientSetupURL string `json:"client_setup_url,omitempty"`
	// SupportsManaged: the provider supports discovery + dynamic client
	// registration via the existing /oauth/discover flow.
	SupportsManaged bool `json:"supports_managed"`
	// SupportsPAT: a personal API key works via the /oauth/pat flow.
	SupportsPAT bool   `json:"supports_pat"`
	PATHint     string `json:"pat_hint,omitempty"`
	// MultiInstance: it makes sense to install this entry several times
	// (one per account/workspace); the dashboard keeps offering "Add".
	MultiInstance bool `json:"multi_instance"`
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
	AuthMode    string   `json:"auth_mode,omitempty"`
	Command     string   `json:"command,omitempty"`
	Args        []string `json:"args,omitempty"`
	URL         string   `json:"url,omitempty"`
	Env         []EnvVar `json:"env,omitempty"`
	// SuggestedName is the default upstream name we propose; the dashboard
	// can override it before posting to /v1/servers.
	SuggestedName string `json:"suggested_name"`
	// Notes is shown beneath the description in the dashboard.
	Notes string `json:"notes,omitempty"`
	// Auth, when set, drives the OAuth section of the install modal.
	Auth *AuthPreset `json:"auth,omitempty"`
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
			ID: "github", Name: "GitHub", Tagline: "Your GitHub account with approval for each write",
			Description: "Read pull requests, add comments, and submit COMMENT, REQUEST_CHANGES, or APPROVE reviews. Each user connects their own GitHub account. Only that user can permit a write.",
			Category:    "Dev", Homepage: "https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/authenticating-with-a-github-app-on-behalf-of-a-user",
			Transport: "github", URL: "https://api.github.com", AuthMode: "per_user", SuggestedName: "GitHubForUsers",
			Notes: "Install the connector, register the GitHub App, and select its repositories. Each user then connects in My connections. Every write, including an APPROVE review, requires the account owner's explicit approval in Toolyard.",
			Auth:  &AuthPreset{Issuer: "https://github.com", AuthorizationEndpoint: "https://github.com/login/oauth/authorize", TokenEndpoint: "https://github.com/login/oauth/access_token", ClientSetupURL: "https://github.com/settings/apps/new"},
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
			Description:   "Linear runs a hosted MCP endpoint at mcp.linear.app. Authorize with Linear-managed OAuth, your own Linear OAuth app (optionally as the app itself, service-account style), or a plain API key.",
			Category:      "Productivity",
			Homepage:      "https://linear.app/docs/mcp",
			Transport:     "http",
			URL:           "https://mcp.linear.app/mcp",
			SuggestedName: "linear",
			Notes:         "Install once per Linear account/workspace — each instance keeps its own credentials. With \"Authorize as: App\", issues and comments are attributed to your OAuth app instead of a user.",
			Auth: &AuthPreset{
				Issuer:                "https://linear.app",
				AuthorizationEndpoint: "https://linear.app/oauth/authorize",
				TokenEndpoint:         "https://api.linear.app/oauth/token",
				Scope:                 "read,write,issues:create,comments:create",
				Options: []AuthOption{{
					Param:   "actor",
					Label:   "Authorize as",
					Default: "app",
					Choices: []AuthChoice{
						{Value: "app", Label: "App", Description: "Issues/comments attributed to the OAuth app (service account)"},
						{Value: "user", Label: "User", Description: "Attributed to the authorizing user"},
					},
				}},
				ClientSetupURL:  "https://linear.app/settings/api/applications",
				SupportsManaged: true,
				SupportsPAT:     true,
				PATHint:         "Linear API key (lin_api_…) from linear.app/settings/api",
				MultiInstance:   true,
			},
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
