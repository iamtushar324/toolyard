package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
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

	// EnvFunc, when non-nil on a stdio transport, is invoked at dial time
	// to build the subprocess environment. It mirrors HeaderFunc: the
	// upstreams package installs a closure that resolves secret:// refs in
	// Env to their decrypted values. A resolution error fails the dial
	// (naming the missing secret). Because resume() re-dials with the
	// stored cfg, secret rotation applies automatically on next reconnect.
	EnvFunc func(ctx context.Context) (map[string]string, error) `json:"-"`

	// IdentityHeader, when non-empty on an http transport, names the header
	// that carries the caller's per-person identity key (see
	// IdentityResolver) on every tools/call. A call whose caller has no key
	// is refused. initialize, tools/list and reconnects never carry it.
	IdentityHeader string `json:"identity_header,omitempty"`
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

	// Circuit-breaker state, guarded by mu. When a dial fails we refuse
	// to re-dial until nextRetryAt, so a wedged stdio command (or a dead
	// remote) fails fast in microseconds instead of eating the 180s
	// connect timeout on every single call. Backoff resets on a
	// successful resume.
	consecutiveFailures int
	nextRetryAt         time.Time
	lastDialErr         error

	// recoveries counts the times the remote forgot our session and we
	// re-established it under a tool call (see callTool). Surfaced through
	// Gateway.SessionRecoveries.
	recoveries atomic.Int64
}

// withoutForwardedKey drops any forwarded identity key from ctx. The
// upstream's own protocol traffic (initialize, tools/list, a re-dial after
// an idle suspend) runs under the gateway's shared credentials only; a
// caller's key rides on that caller's tools/call and nothing else.
func withoutForwardedKey(ctx context.Context) context.Context {
	if _, ok := ForwardedKey(ctx); !ok {
		return ctx
	}
	return context.WithValue(ctx, forwardedKeyKey{}, "")
}

// newUpstream connects to one upstream MCP server using the configured transport.
func newUpstream(ctx context.Context, cfg UpstreamConfig) (*upstream, error) {
	ctx = withoutForwardedKey(ctx)
	var c *client.Client
	switch cfg.Transport {
	case "stdio":
		if cfg.Command == "" {
			return nil, errors.New("stdio upstream requires command")
		}
		// Resolve the subprocess environment. When EnvFunc is set (secrets
		// broker wired) it yields the env with secret:// refs decrypted; a
		// resolution failure fails the dial naming the missing secret. When
		// unset we fall back to the raw cfg.Env.
		env := cfg.Env
		if cfg.EnvFunc != nil {
			resolved, err := cfg.EnvFunc(ctx)
			if err != nil {
				return nil, fmt.Errorf("stdio upstream %s: resolve env: %w", cfg.Name, err)
			}
			env = resolved
		}
		envSlice := make([]string, 0, len(env))
		for k, v := range env {
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

	res, err := c.ListTools(withoutForwardedKey(ctx), mcp.ListToolsRequest{})
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
//
// A stateful streamable-HTTP server can forget the shared session (idle
// timeout, max age, a restart) and then reject every request on it before
// any tool runs. That is recovered here: the dead session is replaced and
// the call retried once. Only a session rejection qualifies; a tool error,
// a timeout or any other transport failure is returned as is, since the
// tool may already have run.
func (u *upstream) callTool(ctx context.Context, name string, args map[string]any) (*mcp.CallToolResult, error) {
	u.lastUsed.Store(time.Now().UnixNano())

	c, err := u.liveClient()
	if err != nil {
		return nil, err
	}
	req := mcp.CallToolRequest{}
	req.Params.Name = name
	req.Params.Arguments = args
	res, err := c.CallTool(ctx, req)
	if err == nil {
		return res, nil
	}
	reason, ok := sessionErrorReason(err)
	if !ok {
		return nil, err
	}
	if rerr := u.recoverSession(c, reason); rerr != nil {
		return nil, fmt.Errorf("upstream %s: session lost (%s) and reconnect failed: %w", u.cfg.Name, reason, rerr)
	}
	c, err = u.liveClient()
	if err != nil {
		return nil, err
	}
	// Same ctx as the first attempt: the forwarded identity key (if any)
	// rides on this retry exactly as it did on the rejected request.
	return c.CallTool(ctx, req)
}

// liveClient returns the connected client, re-dialing first when the
// upstream is suspended (idle-killed, or retired by recoverSession).
func (u *upstream) liveClient() (*client.Client, error) {
	u.mu.Lock()
	c := u.client
	u.mu.Unlock()
	if c != nil {
		return c, nil
	}
	// Use a background context so a short per-call deadline doesn't abort
	// the reconnect — npm needs up to a minute on a warm cache, longer on
	// a cold one.
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
	return c, nil
}

// sessionErrorMarkers are the phrases a streamable-HTTP server uses when
// it no longer knows the session: the mcp-go server's own 404 texts, and
// the JSON-RPC -32000 the beknown-services mcp-server sends with HTTP 400.
// Matched case-insensitively against the whole error chain's text.
var sessionErrorMarkers = []string{
	"invalid session",
	"session not found",
	"missing initialization",
	"session terminated",
}

// sessionErrorReason reports whether err means the upstream rejected the
// request because our session is invalid or gone, i.e. before running the
// tool. The reason is a fixed phrase safe to log.
func sessionErrorReason(err error) (string, bool) {
	if err == nil {
		return "", false
	}
	if errors.Is(err, transport.ErrSessionTerminated) {
		return "session terminated (404)", true
	}
	msg := strings.ToLower(err.Error())
	for _, m := range sessionErrorMarkers {
		if strings.Contains(msg, m) {
			return m, true
		}
	}
	return "", false
}

// sessionRetireGrace is how long a retired client stays open after its
// session was found dead. Closing it at once would cancel the requests
// other callers still have in flight on it; they must get the upstream's
// own rejection (and recover the same way) or, if one is genuinely
// executing, finish. Nothing new is sent on a retired client.
const sessionRetireGrace = 30 * time.Second

// recoverSession replaces the client whose session the upstream no longer
// recognises. Callers that failed on the same client at the same time
// collapse into one re-dial: the first to arrive retires it (so resume has
// something to do), the rest find it already retired and just join the
// resume, which serialises on u.mu and returns as soon as the connection
// is live. The re-dial runs through the same circuit breaker and backoff
// as an idle-kill restart; no lock is held while the old client closes.
func (u *upstream) recoverSession(dead *client.Client, reason string) error {
	u.mu.Lock()
	retire := u.client == dead
	if retire {
		u.client = nil
	}
	u.mu.Unlock()
	if retire {
		u.recoveries.Add(1)
		log.Printf("upstream-session-recover: %q re-establishing session: %s", u.cfg.Name, reason)
		name := u.cfg.Name
		time.AfterFunc(sessionRetireGrace, func() { closeWithTimeout(name, dead, 5*time.Second) })
	}
	resumeCtx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	return u.resume(resumeCtx)
}

// suspend closes the subprocess but keeps the upstream's catalog entry and
// tool cache intact. The upstream restarts automatically on the next callTool.
func (u *upstream) suspend() {
	u.mu.Lock()
	c := u.client
	u.client = nil
	u.mu.Unlock()
	if c != nil {
		// Close OUTSIDE u.mu: a hung stdio child would otherwise block
		// this upstream's callers and — when suspend() is the LRU
		// eviction victim — the whole admission path in acquireSlot.
		closeWithTimeout(u.cfg.Name, c, 5*time.Second)
	}
}

// closeWithTimeout closes a transport off the caller's goroutine (and
// off the upstream lock). A wedged stdio child or a remote that never
// ACKs shutdown would otherwise block indefinitely. On timeout we log a
// WARN and abandon the goroutine; the OS reaps any child when the
// gateway process exits.
func closeWithTimeout(name string, c io.Closer, timeout time.Duration) {
	done := make(chan error, 1)
	go func() { done <- c.Close() }()
	select {
	case err := <-done:
		if err != nil {
			log.Printf("upstream-close: %q close error: %v", name, err)
		}
	case <-time.After(timeout):
		log.Printf("upstream-close-timeout: WARN %q did not close within %s; abandoning", name, timeout)
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
	// Circuit breaker: while a recent dial failure's backoff window is
	// still open, fail fast with the last error rather than re-dialing.
	if !u.nextRetryAt.IsZero() && time.Now().Before(u.nextRetryAt) {
		return fmt.Errorf("upstream %s in backoff for %s after %d failures: %w",
			u.cfg.Name, time.Until(u.nextRetryAt).Round(time.Second),
			u.consecutiveFailures, u.lastDialErr)
	}
	if u.pool != nil {
		u.pool.acquireSlot(u)
	}
	fresh, err := newUpstream(ctx, u.cfg)
	if err != nil {
		u.consecutiveFailures++
		u.lastDialErr = err
		u.nextRetryAt = time.Now().Add(upstreamBackoff(u.consecutiveFailures))
		return err
	}
	u.client = fresh.client
	u.consecutiveFailures = 0
	u.nextRetryAt = time.Time{}
	u.lastDialErr = nil
	u.lastUsed.Store(time.Now().UnixNano()) // reset idle clock after reconnect
	return nil
}

// upstreamBackoff returns how long to wait before re-dialing an upstream
// after N consecutive dial failures: 10s, 30s, 2m, 10m, then capped at
// 30m. Mirrors the refresher backoff idiom in internal/oauth.
func upstreamBackoff(failures int) time.Duration {
	switch failures {
	case 0:
		return 0
	case 1:
		return 10 * time.Second
	case 2:
		return 30 * time.Second
	case 3:
		return 2 * time.Minute
	case 4:
		return 10 * time.Minute
	default:
		return 30 * time.Minute
	}
}

// inBackoff reports whether the upstream is currently suspended and within
// its circuit-breaker backoff window (a recent dial failed and the retry
// window hasn't elapsed). Surfaced via /v1/health.
func (u *upstream) inBackoff() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.client == nil && !u.nextRetryAt.IsZero() && time.Now().Before(u.nextRetryAt)
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
	if u == nil {
		return nil
	}
	u.mu.Lock()
	c := u.client
	u.client = nil
	u.mu.Unlock()
	if c == nil {
		return nil
	}
	closeWithTimeout(u.cfg.Name, c, 5*time.Second)
	return nil
}
