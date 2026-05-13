package gateway

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

// HeaderFunc provides dynamic per-request HTTP headers. Used by remote
// upstreams to attach an OAuth Authorization: Bearer header that may
// rotate while the connection is alive.
type HeaderFunc func(ctx context.Context) map[string]string

// UpstreamConfig describes one configured upstream MCP server. The
// HeaderFunc field is in-memory only — it is wired by the upstreams
// package when an OAuth client + token are registered.
type UpstreamConfig struct {
	Name      string            `json:"name"`
	Transport string            `json:"transport"` // "stdio" | "http"
	Command   string            `json:"command,omitempty"`
	Args      []string          `json:"args,omitempty"`
	URL       string            `json:"url,omitempty"`
	Env       map[string]string `json:"env,omitempty"`

	// HeaderFunc, when non-nil on an http transport, is invoked per
	// request to obtain Authorization (and any other) headers. Populated
	// by the upstreams.Service when an OAuth client is registered.
	HeaderFunc HeaderFunc `json:"-"`
}

type upstream struct {
	cfg    UpstreamConfig
	client *client.Client
	// closeMu guards lifecycle (Close) from racing with in-flight RPCs.
	// It is NOT held during ListTools/CallTool: mcp-go's transports
	// (stdio + streamable HTTP) are explicitly goroutine-safe — see
	// client/transport/stdio.go SendRequest, which uses a per-request
	// response channel and only takes its internal stdinMu around the
	// frame write. Holding a per-upstream mutex around the full RPC
	// turned every upstream into a serial bottleneck: one slow call
	// (e.g. context7 search) would block every other call to the same
	// upstream and the dashboard's tool catalog refresh as well.
	closeMu sync.Mutex
}

// newUpstream connects to one upstream MCP server using the configured transport.
func newUpstream(ctx context.Context, cfg UpstreamConfig) (*upstream, error) {
	var c *client.Client
	switch cfg.Transport {
	case "stdio":
		if cfg.Command == "" {
			return nil, errors.New("stdio upstream requires command")
		}
		envSlice := make([]string, 0, len(cfg.Env))
		for k, v := range cfg.Env {
			envSlice = append(envSlice, k+"="+v)
		}
		stdio, err := client.NewStdioMCPClient(cfg.Command, envSlice, cfg.Args...)
		if err != nil {
			return nil, fmt.Errorf("stdio upstream %s: %w", cfg.Name, err)
		}
		c = stdio
	case "http", "streamable-http", "":
		if cfg.URL == "" {
			return nil, errors.New("http upstream requires url")
		}
		var opts []transport.StreamableHTTPCOption
		if cfg.HeaderFunc != nil {
			opts = append(opts, transport.WithHTTPHeaderFunc(transport.HTTPHeaderFunc(cfg.HeaderFunc)))
		}
		hc, err := client.NewStreamableHttpClient(cfg.URL, opts...)
		if err != nil {
			return nil, fmt.Errorf("http upstream %s: %w", cfg.Name, err)
		}
		c = hc
	default:
		return nil, fmt.Errorf("unsupported transport %q", cfg.Transport)
	}

	// First-run starts that need to npm/pnpm/uv install can take well over a
	// minute. We give the connect a generous deadline; once the package cache
	// is warm subsequent runs return in seconds.
	startCtx, cancel := context.WithTimeout(ctx, 180*time.Second)
	defer cancel()
	if err := c.Start(startCtx); err != nil {
		return nil, fmt.Errorf("start upstream %s: %w", cfg.Name, err)
	}
	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{Name: "toolyard-gateway", Version: "0.1.0"}
	if _, err := c.Initialize(startCtx, initReq); err != nil {
		return nil, fmt.Errorf("init upstream %s: %w", cfg.Name, err)
	}
	return &upstream{cfg: cfg, client: c}, nil
}

// listTools fetches the upstream's tool catalog.
func (u *upstream) listTools(ctx context.Context) ([]mcp.Tool, error) {
	res, err := u.client.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		return nil, err
	}
	return res.Tools, nil
}

// callTool dispatches a forwarded call to the upstream. mcp-go's transports
// multiplex concurrent requests internally, so we deliberately do not
// serialize callers here — multiple agents (and tools.execute) can hit
// the same upstream in parallel.
func (u *upstream) callTool(ctx context.Context, name string, args map[string]any) (*mcp.CallToolResult, error) {
	req := mcp.CallToolRequest{}
	req.Params.Name = name
	req.Params.Arguments = args
	return u.client.CallTool(ctx, req)
}

func (u *upstream) close() error {
	if u == nil || u.client == nil {
		return nil
	}
	u.closeMu.Lock()
	defer u.closeMu.Unlock()
	return u.client.Close()
}
