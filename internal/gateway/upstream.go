package gateway

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
)

// UpstreamConfig describes one configured upstream MCP server.
type UpstreamConfig struct {
	Name      string            `json:"name"`
	Transport string            `json:"transport"` // "stdio" | "http"
	Command   string            `json:"command,omitempty"`
	Args      []string          `json:"args,omitempty"`
	URL       string            `json:"url,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
}

type upstream struct {
	cfg    UpstreamConfig
	client *client.Client
	mu     sync.Mutex
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
		hc, err := client.NewStreamableHttpClient(cfg.URL)
		if err != nil {
			return nil, fmt.Errorf("http upstream %s: %w", cfg.Name, err)
		}
		c = hc
	default:
		return nil, fmt.Errorf("unsupported transport %q", cfg.Transport)
	}

	startCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
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
	u.mu.Lock()
	defer u.mu.Unlock()
	res, err := u.client.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		return nil, err
	}
	return res.Tools, nil
}

// callTool dispatches a forwarded call to the upstream.
func (u *upstream) callTool(ctx context.Context, name string, args map[string]any) (*mcp.CallToolResult, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	req := mcp.CallToolRequest{}
	req.Params.Name = name
	req.Params.Arguments = args
	return u.client.CallTool(ctx, req)
}

func (u *upstream) close() error {
	if u == nil || u.client == nil {
		return nil
	}
	return u.client.Close()
}
