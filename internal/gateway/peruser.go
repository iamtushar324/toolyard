package gateway

// peruser.go: upstreams whose users each sign in with their own account.
//
// A shared upstream holds one MCP connection (one client, one
// Mcp-Session-Id) for everybody. A per_user upstream instead gets one
// connection per (upstream, user), opened lazily on that person's first
// call and initialised with that person's bearer, so no upstream session
// ever carries two people. The tool list comes from the first connected
// user's connection and is cached per upstream; until somebody connects,
// the upstream has no tools and reports ErrWaitingForSignIn. A call from an
// agent whose owner has not connected is refused before the upstream is
// contacted, and a per_user upstream never falls back to a shared token.
//
// Per-user connections live in their own pool (SetMaxLivePerUser): a few
// people signing in never evict a shared stdio server, and vice versa.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tusharbhardwaj/toolyard/internal/actor"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/metrics"
)

// PerUserAuth is what a per_user upstream needs from the token store.
// *oauth.Service satisfies it.
type PerUserAuth interface {
	// ConnectedUsers lists the users who hold a usable token on upstream.
	ConnectedUsers(ctx context.Context, upstream string) ([]string, error)
	// UserConnected reports whether one user does.
	UserConnected(ctx context.Context, upstream, userID string) (bool, error)
	// FreshenUserToken runs before the user's connection is dialled: an
	// access token whose expiry has already passed is refreshed first. An
	// error means the token cannot be used right now.
	FreshenUserToken(ctx context.Context, upstream, userID string) error
	// RefreshUserToken runs after the upstream answered 401: nil means a
	// new token is in place and the request can be retried. The store
	// marks the row for re-auth only when there is nothing to refresh with.
	RefreshUserToken(ctx context.Context, upstream, userID string) error
	// MarkUserUnauthorized records that the upstream rejected even a
	// freshly refreshed token, so the person's row needs a new sign-in.
	MarkUserUnauthorized(ctx context.Context, upstream, userID, msg string)
}

// OwnerResolver maps a gateway caller id (an agent id, "dashboard:<uid>",
// "voice:<uid>") to the dashboard user behind it. *access.Service
// satisfies it. "" means the caller has no user.
type OwnerResolver interface {
	OwnerUser(ctx context.Context, callerID string) (string, error)
}

// ErrWaitingForSignIn is returned by AddUpstream and RefreshPerUser for a
// per_user upstream nobody has connected yet: the upstream is registered
// and will get its tools from the first person who signs in.
var ErrWaitingForSignIn = errors.New("waiting for a first sign-in")

// StatusWaitingSignIn is the upstream_servers.last_status an upstream in
// that state is recorded with.
const StatusWaitingSignIn = "waiting_signin"

// ErrNoUserConnection means the caller's user has not connected the
// per_user upstream (no usable token on file); the call was not made.
var ErrNoUserConnection = errors.New("user has not connected this server")

// ErrUserSignInUnusable means the caller's user has connected, but their
// token could not be made ready for this call (a refresh failed); the
// call was not made.
var ErrUserSignInUnusable = errors.New("the user's sign-in could not be used")

type upstreamUserKey struct{}

// WithUpstreamUser names the user whose bearer a per_user upstream's
// header function should send. Set on every request of that user's
// connection (initialize, tools/list and tools/call alike): the connection
// belongs to that one person.
func WithUpstreamUser(ctx context.Context, userID string) context.Context {
	return context.WithValue(ctx, upstreamUserKey{}, userID)
}

// UpstreamUser returns the user set by WithUpstreamUser.
func UpstreamUser(ctx context.Context) (string, bool) {
	u, ok := ctx.Value(upstreamUserKey{}).(string)
	return u, ok && u != ""
}

// perUserGroup is one per_user upstream: its config and the connections
// opened so far, one per user.
type perUserGroup struct {
	cfg  UpstreamConfig
	pool *Gateway

	mu          sync.Mutex
	conns       map[string]*upstream
	toolsLoaded bool
}

// userCfg is the group's config for one user's connection: the header
// function always runs for that user, whatever the request context says.
func (pu *perUserGroup) userCfg(userID string) UpstreamConfig {
	cfg := pu.cfg
	base := pu.cfg.HeaderFunc
	cfg.HeaderFunc = func(ctx context.Context) map[string]string {
		if base == nil {
			return nil
		}
		return base(WithUpstreamUser(ctx, userID))
	}
	return cfg
}

// conn returns the user's connection, creating a suspended one on first
// use; the dial happens on the first call through it.
func (pu *perUserGroup) conn(userID string) *upstream {
	pu.mu.Lock()
	defer pu.mu.Unlock()
	if c, ok := pu.conns[userID]; ok {
		return c
	}
	c := &upstream{cfg: pu.userCfg(userID), pool: pu.pool, userID: userID}
	c.lastUsed.Store(time.Now().UnixNano())
	pu.conns[userID] = c
	return c
}

// drop retires and closes one user's connection. A call still holding the
// old connection finds it retired on its next dial and resolves the
// replacement through conn (see callAs), so nothing lives on untracked.
func (pu *perUserGroup) drop(userID string) {
	pu.mu.Lock()
	c := pu.conns[userID]
	delete(pu.conns, userID)
	pu.mu.Unlock()
	if c != nil {
		c.retire()
	}
}

// all snapshots the group's connections.
func (pu *perUserGroup) all() []*upstream {
	pu.mu.Lock()
	defer pu.mu.Unlock()
	out := make([]*upstream, 0, len(pu.conns))
	for _, c := range pu.conns {
		out = append(out, c)
	}
	return out
}

func (pu *perUserGroup) closeAll() {
	for _, c := range pu.all() {
		c.retire()
	}
}

// callAs runs one tool call on userID's connection. If the connection it
// picked was retired under it (a drop raced the call), it resolves the
// replacement once and retries there.
func (pu *perUserGroup) callAs(ctx context.Context, userID, tool string, args map[string]any) (*mcp.CallToolResult, error) {
	res, err := pu.conn(userID).callTool(ctx, tool, args)
	if errors.Is(err, errRetired) {
		return pu.conn(userID).callTool(ctx, tool, args)
	}
	return res, err
}

// listToolsAs is callAs for the tool list.
func (pu *perUserGroup) listToolsAs(ctx context.Context, userID string) ([]mcp.Tool, error) {
	tools, err := pu.conn(userID).listToolsLive(ctx)
	if errors.Is(err, errRetired) {
		return pu.conn(userID).listToolsLive(ctx)
	}
	return tools, err
}

// bearerOnFile reports whether the upstream's header function yields an
// Authorization header for userID right now. The per-user header function
// never emits a static Authorization (see upstreams.toCfg), so this is the
// person's own bearer or nothing.
func (pu *perUserGroup) bearerOnFile(ctx context.Context, userID string) bool {
	if pu.cfg.HeaderFunc == nil {
		return false
	}
	for k := range pu.cfg.HeaderFunc(WithUpstreamUser(ctx, userID)) {
		if strings.EqualFold(k, "Authorization") {
			return true
		}
	}
	return false
}

// readyUser checks that userID can be dialled as, before any request goes
// out: connected, token fresh (refreshed now if its access expiry has
// passed), and a bearer actually on file. Anything short of that refuses
// the call; the upstream is never contacted without the person's bearer.
func (pu *perUserGroup) readyUser(ctx context.Context, userID string) error {
	ok, err := pu.cfg.PerUserAuth.UserConnected(ctx, pu.cfg.Name, userID)
	if err != nil {
		return fmt.Errorf("check connection: %w", err)
	}
	if !ok {
		return ErrNoUserConnection
	}
	if err := pu.cfg.PerUserAuth.FreshenUserToken(ctx, pu.cfg.Name, userID); err != nil {
		return fmt.Errorf("%w: %v", ErrUserSignInUnusable, err)
	}
	if !pu.bearerOnFile(ctx, userID) {
		return fmt.Errorf("%w: no bearer on file", ErrNoUserConnection)
	}
	return nil
}

// addPerUserUpstream registers a per_user upstream and tries to load its
// tool list from a connected user. The group is kept even when nobody has
// connected (ErrWaitingForSignIn) so the first sign-in can finish the job.
func (g *Gateway) addPerUserUpstream(ctx context.Context, cfg UpstreamConfig) error {
	switch cfg.Transport {
	case "http", "streamable-http", "":
	default:
		return fmt.Errorf("per-user sign-in needs an http upstream, not %q", cfg.Transport)
	}
	if cfg.PerUserAuth == nil {
		return errors.New("per-user sign-in is not wired on this gateway")
	}
	pu := &perUserGroup{cfg: cfg, pool: g, conns: map[string]*upstream{}}
	g.mu.Lock()
	if old := g.perUser[cfg.Name]; old != nil {
		defer old.closeAll()
	}
	g.perUser[cfg.Name] = pu
	g.mu.Unlock()
	return g.loadPerUserTools(ctx, pu)
}

// loadPerUserTools fetches the upstream's tool list over the first
// connected user's connection and registers it. A user whose token is
// rejected gets one refresh and one retry, as a call would; the next user
// is tried when that fails.
func (g *Gateway) loadPerUserTools(ctx context.Context, pu *perUserGroup) error {
	pu.mu.Lock()
	loaded := pu.toolsLoaded
	pu.mu.Unlock()
	if loaded {
		return nil
	}
	users, err := pu.cfg.PerUserAuth.ConnectedUsers(ctx, pu.cfg.Name)
	if err != nil {
		return fmt.Errorf("per-user upstream %s: list connected users: %w", pu.cfg.Name, err)
	}
	if len(users) == 0 {
		return ErrWaitingForSignIn
	}
	var last error
	for _, uid := range users {
		if err := pu.readyUser(ctx, uid); err != nil {
			last = err
			continue
		}
		tools, err := pu.listToolsAs(ctx, uid)
		if err != nil && isUnauthorized(err) {
			tools, err = retryAfter401(ctx, pu, uid, func() ([]mcp.Tool, error) { return pu.listToolsAs(ctx, uid) })
		}
		if err != nil {
			last = err
			continue
		}
		pu.mu.Lock()
		already := pu.toolsLoaded
		pu.toolsLoaded = true
		pu.mu.Unlock()
		if !already {
			g.registerUpstreamTools(pu.cfg.Name, tools, nil)
		}
		return nil
	}
	return fmt.Errorf("per-user upstream %s: could not list tools over any connected user's session: %w", pu.cfg.Name, last)
}

// retryAfter401 is the one recovery a per-user request gets when the
// upstream answers 401: the access token may simply have expired, so the
// token is refreshed once and the request retried once on a fresh
// connection. A refresh that cannot happen (nothing to refresh with,
// invalid_grant) is reported by the store, which marks the row itself; a
// 401 on the retry means the upstream rejects even a fresh token, and the
// row is marked then. In every failure the person's connection is dropped
// so the next call starts clean.
func retryAfter401[T any](ctx context.Context, pu *perUserGroup, userID string, again func() (T, error)) (T, error) {
	var zero T
	name, auth := pu.cfg.Name, pu.cfg.PerUserAuth
	if rerr := auth.RefreshUserToken(ctx, name, userID); rerr != nil {
		pu.drop(userID)
		return zero, fmt.Errorf("%s rejected the sign-in of this agent's owner (401) and it could not be refreshed: %w", name, rerr)
	}
	pu.drop(userID) // the old session was opened with the old bearer
	out, err := again()
	if err != nil && isUnauthorized(err) {
		auth.MarkUserUnauthorized(ctx, name, userID, "upstream rejected a freshly refreshed token (401)")
		pu.drop(userID)
	}
	return out, err
}

// RefreshPerUser is called after a person connects a per_user upstream:
// if the upstream has no tools yet, they are loaded now (over that first
// connection). Nothing else changes; the person's calls open their own
// connection lazily. ErrUpstreamNotFound when the upstream is not a
// registered per_user upstream, ErrWaitingForSignIn when still nobody is
// connected.
func (g *Gateway) RefreshPerUser(ctx context.Context, name string) error {
	g.mu.RLock()
	pu := g.perUser[name]
	g.mu.RUnlock()
	if pu == nil {
		return ErrUpstreamNotFound
	}
	return g.loadPerUserTools(ctx, pu)
}

// DropUserConnection closes one person's connection to a per_user upstream
// (their token was revoked, disconnected or rejected). A later call from
// them dials again, with whatever token they hold then.
func (g *Gateway) DropUserConnection(name, userID string) {
	g.mu.RLock()
	pu := g.perUser[name]
	g.mu.RUnlock()
	if pu != nil {
		pu.drop(userID)
	}
}

// IsPerUser reports whether name is registered as a per_user upstream.
func (g *Gateway) IsPerUser(name string) bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.perUser[name] != nil
}

// perUserConnections counts the connections open for a per_user upstream
// (for tests and status).
func (g *Gateway) perUserConnections(name string) int {
	g.mu.RLock()
	pu := g.perUser[name]
	g.mu.RUnlock()
	if pu == nil {
		return 0
	}
	return len(pu.all())
}

// ownerUser resolves the dashboard user behind a call: the resolver when
// one is wired (an agent's owner from the store), else the user the
// ingress put on the raiser.
func (g *Gateway) ownerUser(ctx context.Context, callerID string) (string, error) {
	if g.owners != nil {
		uid, err := g.owners.OwnerUser(ctx, callerID)
		if err != nil || uid != "" {
			return uid, err
		}
	}
	raiser := g.resolveRaiser(ctx, callerID, "")
	return raiser.OwnerUserID, nil
}

// perUserHandle resolves the connection a call to a per_user upstream must
// run on: the caller's user, who must have connected it and whose token is
// ready. The returned handler gives a 401 one refresh and one retry
// (retryAfter401) and otherwise fails closed.
func (g *Gateway) perUserHandle(ctx context.Context, pu *perUserGroup, entry toolEntry, callerID string) (directHandler, string, error) {
	uid, err := g.ownerUser(ctx, callerID)
	if err != nil {
		return nil, "", fmt.Errorf("resolve caller's user: %w", err)
	}
	if uid == "" {
		return nil, "", errors.New("caller has no toolyard user")
	}
	if err := pu.readyUser(ctx, uid); err != nil {
		return nil, uid, err
	}
	handle := func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
		res, err := pu.callAs(ctx, uid, entry.originalName, args)
		if err == nil || !isUnauthorized(err) {
			return res, err
		}
		return retryAfter401(ctx, pu, uid, func() (*mcp.CallToolResult, error) {
			return pu.callAs(ctx, uid, entry.originalName, args)
		})
	}
	return handle, uid, nil
}

// refusePerUser is the terminal outcome of a call to a per_user upstream
// that cannot run as the caller: the upstream is never contacted, the
// audit row says why, and the agent gets a message naming the page where
// the person connects their account.
func (g *Gateway) refusePerUser(ctx context.Context, entry toolEntry, agentID, reason, approvalID, uid string,
	cause error, ev *metrics.Event) *mcp.CallToolResult {
	var msg string
	switch {
	case errors.Is(cause, ErrNoUserConnection):
		msg = fmt.Sprintf("%s runs as each person's own account, and the person who owns this agent has not connected theirs yet. "+
			"Open %s, connect %s, then retry. The call was not made.", entry.upstream, g.connectionsURL(), entry.upstream)
	case errors.Is(cause, ErrUserSignInUnusable):
		msg = fmt.Sprintf("%s runs as each person's own account, and the sign-in of the person who owns this agent could not be used right now (%v). "+
			"If this keeps happening, open %s and reconnect %s. The call was not made.", entry.upstream, cause, g.connectionsURL(), entry.upstream)
	default:
		msg = fmt.Sprintf("%s runs as each person's own account and toolyard could not tell whose agent this is; the call was not made.", entry.upstream)
		log.Printf("per-user: tool=%s upstream=%s agent=%s refused: %v", entry.tool.Name, entry.upstream, agentID, cause)
	}
	_ = g.audit.Write(ctx, audit.Event{
		EventType:     audit.EventCallFailed,
		AgentID:       agentID,
		UpstreamName:  entry.upstream,
		ToolName:      entry.tool.Name,
		Reason:        reason,
		ApprovalID:    approvalID,
		ResultSummary: "per_user: " + cause.Error(),
		Raiser:        actor.Raiser{OwnerUserID: uid},
	})
	ev.Outcome = metrics.OutcomeError
	ev.ErrorClass = "per_user"
	return mcp.NewToolResultError(msg)
}

// connectionsURL is where a person connects their accounts: the public
// dashboard's My connections page when the public URL is known.
func (g *Gateway) connectionsURL() string {
	if g.publicURL == "" {
		return "the toolyard dashboard's My connections page (#connections)"
	}
	return strings.TrimRight(g.publicURL, "/") + "/#connections"
}

// isUnauthorized reports whether err is the transport's answer to an HTTP
// 401 from the upstream, on a dial or on a call. mcp-go wraps it in a
// transport.Error that unwraps, so the sentinel is enough.
func isUnauthorized(err error) bool {
	return errors.Is(err, transport.ErrAuthorizationRequired)
}
