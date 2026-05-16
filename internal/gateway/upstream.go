package gateway

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
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
	cfg UpstreamConfig

	// pool is a back-pointer to the owning Gateway, used for admission
	// control on resume() (LRU eviction when the live-upstream cap is
	// hit). May be nil in tests that construct upstreams directly.
	pool *Gateway

	// mu guards the client field for all lifecycle transitions:
	// connect, suspend, resume, and close. It is NOT held during
	// tool RPCs — mcp-go transports are explicitly goroutine-safe.
	mu     sync.Mutex
	client *client.Client // nil when suspended

	// lastUsed is unix nanos of the most recent callTool (or the time
	// the upstream connected, whichever is later). The idle sweeper
	// reads this to decide when to suspend. Initialised to time.Now()
	// in newUpstream so a never-called upstream ages naturally.
	lastUsed atomic.Int64

	// cachedTools holds the most-recent tool list. Kept across
	// suspend/resume so the gateway catalog stays populated while
	// the subprocess is idle-killed.
	cachedToolsMu sync.RWMutex
	cachedTools   []mcp.Tool
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
	u := &upstream{cfg: cfg, client: c}
	u.lastUsed.Store(time.Now().UnixNano())
	return u, nil
}

// listTools fetches the upstream's tool catalog. When the upstream is
// suspended it returns the cached tool list so the gateway catalog stays
// populated without restarting the subprocess.
func (u *upstream) listTools(ctx context.Context) ([]mcp.Tool, error) {
	u.mu.Lock()
	c := u.client
	u.mu.Unlock()

	if c == nil {
		u.cachedToolsMu.RLock()
		defer u.cachedToolsMu.RUnlock()
		if u.cachedTools != nil {
			return u.cachedTools, nil
		}
		return nil, errors.New("upstream is suspended and has no cached tools")
	}

	res, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		return nil, err
	}
	u.cachedToolsMu.Lock()
	u.cachedTools = res.Tools
	u.cachedToolsMu.Unlock()
	return res.Tools, nil
}

// callTool dispatches a forwarded call to the upstream. If the upstream is
// currently suspended (idle-killed) it is restarted transparently before the
// call is dispatched. mcp-go transports multiplex concurrent requests
// internally, so we do not serialize callers — multiple agents can hit the
// same upstream in parallel.
func (u *upstream) callTool(ctx context.Context, name string, args map[string]any) (*mcp.CallToolResult, error) {
	u.lastUsed.Store(time.Now().UnixNano())

	u.mu.Lock()
	c := u.client
	u.mu.Unlock()

	if c == nil {
		// Upstream was idle-killed; restart it. Use a background context so
		// a short per-call deadline doesn't abort the reconnect — npm needs
		// up to a minute on a warm cache, longer on a cold one.
		resumeCtx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
		defer cancel()
		if err := u.resume(resumeCtx); err != nil {
			return nil, fmt.Errorf("reconnect upstream %s: %w", u.cfg.Name, err)
		}
		u.mu.Lock()
		c = u.client
		u.mu.Unlock()
		if c == nil {
			return nil, fmt.Errorf("upstream %s: client unavailable after reconnect", u.cfg.Name)
		}
	}

	req := mcp.CallToolRequest{}
	req.Params.Name = name
	req.Params.Arguments = args
	return c.CallTool(ctx, req)
}

// suspend closes the subprocess but keeps the upstream's catalog entry and
// tool cache intact. The upstream restarts automatically on the next callTool.
func (u *upstream) suspend() {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.client != nil {
		_ = u.client.Close()
		u.client = nil
	}
}

// resume reconnects a suspended upstream. Safe to call concurrently: the
// lock ensures only one goroutine does the work; subsequent callers return
// immediately once the connection is live.
//
// Before reconnecting we ask the pool for a slot — if the live-upstream
// cap is full, the least-recently-used live upstream is suspended to make
// room. This is the load-bearing piece of the pool: any upstream can be
// transparently re-dialed on next use, so eviction is free.
func (u *upstream) resume(ctx context.Context) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.client != nil {
		return nil // already running
	}
	if u.pool != nil {
		u.pool.acquireSlot(u)
	}
	fresh, err := newUpstream(ctx, u.cfg)
	if err != nil {
		return err
	}
	u.client = fresh.client
	u.lastUsed.Store(time.Now().UnixNano()) // reset idle clock after reconnect
	return nil
}

// suspended reports whether the upstream's subprocess is currently killed.
func (u *upstream) suspended() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.client == nil
}

// idleSince returns how long ago this upstream last served a tool call (or
// was first connected, whichever is more recent).
func (u *upstream) idleSince() time.Duration {
	return time.Since(time.Unix(0, u.lastUsed.Load()))
}

func (u *upstream) close() error {
	if u == nil || u.client == nil {
		return nil
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.client.Close()
}
