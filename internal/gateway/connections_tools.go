package gateway

// connections_tools.go: signing in to a server without leaving the chat.
//
// When a call cannot run because the agent's owner has not signed in (their
// own account on a per_user server, or the shared account of a shared OAuth
// server), the refusal carries a one-time connect link the agent shows the
// person. The link (oauth.ConnectLinkPath) redeems a ticket, starts the
// sign-in for that very person and server in their browser, and brings
// them back to a page that says "tell your agent to retry". Nobody signs in
// to toolyard's dashboard on the way.
//
// connections.status lists the OAuth servers the owner may use with their
// sign-in state and a fresh link where one is needed; connections.link
// mints one for a named server. A shared server's link is given only to an
// admin owner; a member is told which admins to ask. Both tools are in the
// always-on "connections" access group (access.alwaysOn), as the inbox
// tools are, so a member's agent can always fetch its owner's own links.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tusharbhardwaj/toolyard/internal/actor"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/metrics"
	"github.com/tusharbhardwaj/toolyard/internal/oauth"
	"github.com/tusharbhardwaj/toolyard/internal/policy"
)

// connectionsGroup is the synthetic upstream the connections.* tools are
// registered under, and their access group.
const connectionsGroup = "connections"

// The connections tools.
const (
	ConnectionsStatusTool = "connections.status"
	ConnectionsLinkTool   = "connections.link"
)

// Audit event types written here.
const (
	// EventConnectLinkIssued: a connect link was minted for an owner. The
	// row names the server, the agent and the owner, never the ticket.
	EventConnectLinkIssued = "connect.link_issued"
)

func init() {
	for _, n := range []string{ConnectionsStatusTool, ConnectionsLinkTool} {
		PinnedTools[n] = struct{}{}
	}
}

// ConnectProvider is what the connections tools and the sign-in refusals
// need from the OAuth store. *oauth.Service satisfies it.
type ConnectProvider interface {
	// IssueConnectTicket mints the ticket a connect link carries, for
	// userID to connect upstream for purpose (oauth.ConnectPurpose*),
	// noting the agent that asked. Returned once; stored hashed.
	IssueConnectTicket(ctx context.Context, userID, upstream, purpose, agentID string) (string, error)
	// OAuthServers lists every server with an OAuth client and its auth
	// mode.
	OAuthServers(ctx context.Context) ([]oauth.OAuthServer, error)
	// SharedConnection reports a shared server's sign-in; nil when the
	// server has no OAuth client.
	SharedConnection(ctx context.Context, upstream string) (*oauth.SharedConnection, error)
	// UserConnectionOf reports one person's sign-in to a per_user server;
	// nil when they never connected it.
	UserConnectionOf(ctx context.Context, upstream, userID string) (*oauth.UserConnection, error)
}

// ConnectDirectory names the people who can sign a shared server in.
// *identity.Service satisfies it.
type ConnectDirectory interface {
	// AdminEmails lists the active admins' emails.
	AdminEmails(ctx context.Context) ([]string, error)
}

// SetConnect wires connect links. Until it is called the connections tools
// answer that links are not available and the refusals point at the
// dashboard, as before.
func (g *Gateway) SetConnect(p ConnectProvider, d ConnectDirectory) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.connect = p
	g.directory = d
}

func (g *Gateway) connectProvider() ConnectProvider {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.connect
}

// connectionsTools returns the two entries; registered by RegisterBuiltins.
func (g *Gateway) connectionsTools() []toolEntry {
	allow := policy.ActionAllow
	entry := func(name, desc string, props map[string]any, required ...string) toolEntry {
		return toolEntry{
			tool: mcp.Tool{
				Name:        name,
				Description: desc,
				InputSchema: mcp.ToolInputSchema{Type: "object", Required: append([]string{ReasonField}, required...), Properties: addMetaProps(props)},
			},
			upstream:     connectionsGroup,
			originalName: strings.TrimPrefix(name, connectionsGroup+"."),
			reasonField:  ReasonField,
			forcedAction: &allow,
		}
	}
	status := entry(ConnectionsStatusTool,
		"Which servers your owner has signed in to, and how to get them connected without leaving the chat. "+
			"Lists every OAuth server your owner may use (their own account on per_user servers, the shared account on shared ones) with its state "+
			"(connected, needs_signin, needs_reauth), the account it acts as, and for each one that needs a sign-in a one-time connect link (10 minutes). "+
			connectGuidance,
		map[string]any{})
	status.handle = g.handleConnectionsStatus
	link := entry(ConnectionsLinkTool,
		"Get a fresh one-time connect link for one server (input: server), or the reason none can be made. "+
			"A per_user server's link signs your owner's own account in; a shared server's link is only given to an admin owner, and a member is told which admins to ask. "+
			connectGuidance,
		map[string]any{"server": map[string]any{"type": "string", "description": "The server name as it appears in tool names (the part before the dot)."}},
		"server")
	link.handle = g.handleConnectionsLink
	return []toolEntry{status, link}
}

// connectGuidance is the agent's instruction on every connect link, here
// and in the refusals: how to hand it to the person.
const connectGuidance = "Show the person the link as a clickable Markdown link and retry after they say they are done; the sign-in happens at the provider, in their browser. Never ask them for a password, token or one-time code."

// connectCaller resolves who is asking: the caller id, the dashboard user
// behind it, whether that user is an admin (the access scope says), and
// the scope itself. errRes is the tool error to return when there is no
// user or no provider.
func (g *Gateway) connectCaller(ctx context.Context) (callerID, uid string, admin bool, errRes *mcp.CallToolResult) {
	callerID = agentIDFromContext(ctx)
	if g.connectProvider() == nil {
		return callerID, "", false, mcp.NewToolResultError("connect links are not available on this gateway; connect servers from the toolyard dashboard")
	}
	uid, err := g.ownerUser(ctx, callerID)
	if err != nil {
		return callerID, "", false, mcp.NewToolResultErrorFromErr("resolve caller's user", err)
	}
	if uid == "" {
		return callerID, "", false, mcp.NewToolResultError("the connections tools need an enrolled agent with an owner: connect with an agent token (Authorization: Bearer …)")
	}
	// Scope.All is an active admin (or a local anonymous caller, who has
	// no user and was refused above).
	return callerID, uid, g.scopeFor(ctx, callerID).All, nil
}

// connectionView is one server on connections.status.
type connectionView struct {
	Server string `json:"server"`
	// Mode: per_user (the owner's own account) or shared.
	Mode string `json:"mode"`
	// State: connected | needs_signin | needs_reauth | needs_setup.
	State string `json:"state"`
	// Account is the account the server acts as, when the provider said.
	Account string `json:"account,omitempty"`
	// ConnectLink is a fresh one-time link, only when a sign-in is needed
	// and this owner may do it.
	ConnectLink string `json:"connect_link,omitempty"`
	// LinkExpiresIn is how long the link can be opened, in seconds.
	LinkExpiresIn int `json:"link_expires_in,omitempty"`
	// Note says what to do when there is no link, or what the link does.
	Note string `json:"note,omitempty"`
}

// Connection states as the tools report them. connected and needs_reauth
// are oauth's; needs_signin covers no token at all; needs_setup means an
// admin has not finished the server's OAuth setup.
const (
	connStateConnected   = oauth.ConnConnected
	connStateNeedsSignIn = oauth.ConnNeedsSignIn
	connStateNeedsReauth = oauth.StateNeedsReauth
	connStateNeedsSetup  = "needs_setup"
)

func (g *Gateway) handleConnectionsStatus(ctx context.Context, _ map[string]any) (*mcp.CallToolResult, error) {
	callerID, uid, admin, errRes := g.connectCaller(ctx)
	if errRes != nil {
		return errRes, nil
	}
	views, err := g.connectionViews(ctx, callerID, uid, admin)
	if err != nil {
		return mcp.NewToolResultErrorFromErr("connections.status", err), nil
	}
	var b strings.Builder
	if len(views) == 0 {
		b.WriteString("No server your owner may use needs a sign-in, and none is signed in through OAuth.")
	}
	for _, v := range views {
		fmt.Fprintf(&b, "%s (%s): %s", v.Server, v.Mode, v.State)
		if v.Account != "" {
			fmt.Fprintf(&b, " as %s", v.Account)
		}
		if v.ConnectLink != "" {
			fmt.Fprintf(&b, " — connect link (one-time, %d minutes): %s", v.LinkExpiresIn/60, v.ConnectLink)
		}
		if v.Note != "" {
			fmt.Fprintf(&b, " — %s", v.Note)
		}
		b.WriteString("\n")
	}
	b.WriteString("\n" + connectGuidance)
	res := mcp.NewToolResultText(b.String())
	res.StructuredContent = map[string]any{"servers": views, "guidance": connectGuidance}
	return res, nil
}

// connectionViews builds the owner's list: every OAuth server (and every
// per_user server, client or not) within the caller's access scope.
func (g *Gateway) connectionViews(ctx context.Context, callerID, uid string, admin bool) ([]connectionView, error) {
	p := g.connectProvider()
	scope := g.scopeFor(ctx, callerID)
	servers, err := p.OAuthServers(ctx)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	type cand struct {
		name    string
		perUser bool
		client  bool
	}
	var cands []cand
	for _, s := range servers {
		if !s.Enabled {
			continue
		}
		seen[s.Name] = true
		cands = append(cands, cand{name: s.Name, perUser: s.AuthMode == "per_user", client: true})
	}
	// A per_user server registered on the gateway but without an OAuth
	// client yet: listed so the agent can say what is missing.
	g.mu.RLock()
	for name := range g.perUser {
		if !seen[name] {
			cands = append(cands, cand{name: name, perUser: true})
		}
	}
	g.mu.RUnlock()
	sort.Slice(cands, func(i, j int) bool { return cands[i].name < cands[j].name })

	var out []connectionView
	for _, c := range cands {
		if !scope.Allows(c.name) {
			continue
		}
		v := connectionView{Server: c.name, Mode: "shared"}
		if c.perUser {
			v.Mode = oauth.ConnectPurposePerUser
		}
		switch {
		case c.perUser && !c.client:
			v.State = connStateNeedsSetup
			v.Note = "an admin still has to finish this server's OAuth setup in the dashboard (Servers → Auth)"
		case c.perUser:
			conn, err := p.UserConnectionOf(ctx, c.name, uid)
			if err != nil {
				return nil, err
			}
			v.State, v.Account = perUserState(conn)
			if v.State != connStateConnected {
				g.fillLink(ctx, &v, uid, c.name, oauth.ConnectPurposePerUser, callerID, "opening it signs your owner's own account in")
			}
		default:
			conn, err := p.SharedConnection(ctx, c.name)
			if err != nil {
				return nil, err
			}
			if conn == nil {
				continue
			}
			v.State, v.Account = sharedState(conn)
			if v.State != connStateConnected {
				switch {
				case !admin:
					v.Note = g.askAdminNote(ctx, c.name)
				case !conn.CanAuthorize:
					v.Note = "this server uses a pasted token; an admin sets a new one in the dashboard (Servers → Auth)"
				default:
					g.fillLink(ctx, &v, uid, c.name, oauth.ConnectPurposeShared, callerID, sharedSignInAs(conn))
				}
			}
		}
		out = append(out, v)
	}
	return out, nil
}

// fillLink mints a link onto v, or explains why none could be made.
func (g *Gateway) fillLink(ctx context.Context, v *connectionView, uid, upstream, purpose, callerID, note string) {
	link, err := g.connectLink(ctx, uid, upstream, purpose, callerID)
	if err != nil {
		v.Note = err.Error()
		return
	}
	v.ConnectLink = link
	v.LinkExpiresIn = int(oauth.ConnectTicketTTL.Seconds())
	v.Note = note
}

// perUserState maps a person's row to the reported state and account.
func perUserState(c *oauth.UserConnection) (state, account string) {
	if c == nil {
		return connStateNeedsSignIn, ""
	}
	switch c.State {
	case oauth.ConnConnected:
		return connStateConnected, c.AccountLabel
	case oauth.StateNeedsReauth, oauth.ConnExpired:
		return connStateNeedsReauth, c.AccountLabel
	}
	return connStateNeedsSignIn, c.AccountLabel
}

// sharedState maps a shared server's token to the reported state and
// account.
func sharedState(c *oauth.SharedConnection) (state, account string) {
	switch {
	case c.Usable():
		return connStateConnected, c.AccountLabel
	case c.HasToken && c.State == oauth.StateNeedsReauth:
		return connStateNeedsReauth, c.AccountLabel
	}
	return connStateNeedsSignIn, c.AccountLabel
}

// sharedSignInAs tells an admin which account a shared server's link
// should be signed in with, when the last one is known.
func sharedSignInAs(c *oauth.SharedConnection) string {
	if c.AccountLabel != "" {
		return "sign in as " + c.AccountLabel
	}
	return "opening it signs the server's shared account in"
}

func (g *Gateway) handleConnectionsLink(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	callerID, uid, admin, errRes := g.connectCaller(ctx)
	if errRes != nil {
		return errRes, nil
	}
	server := strings.TrimSpace(stringArg(args, "server"))
	if server == "" {
		return mcp.NewToolResultError("server is required"), nil
	}
	if !g.scopeFor(ctx, callerID).Allows(server) {
		// Same answer as a server that does not exist: an owner learns
		// nothing about servers not granted to them.
		return mcp.NewToolResultErrorf("unknown server %q", server), nil
	}
	p := g.connectProvider()
	servers, err := p.OAuthServers(ctx)
	if err != nil {
		return mcp.NewToolResultErrorFromErr("connections.link", err), nil
	}
	var found *oauth.OAuthServer
	for i := range servers {
		if servers[i].Name == server {
			found = &servers[i]
			break
		}
	}
	switch {
	case found == nil && g.IsPerUser(server):
		return mcp.NewToolResultErrorf("%s has no sign-in set up yet: an admin still has to finish its OAuth setup in the dashboard (Servers → Auth). Nothing to open.", server), nil
	case found == nil:
		return mcp.NewToolResultErrorf("unknown server %q, or it does not use an OAuth sign-in (nothing to connect)", server), nil
	case !found.Enabled:
		return mcp.NewToolResultErrorf("%s is disabled; an admin has to enable it before anyone signs in", server), nil
	}
	v := connectionView{Server: server, Mode: "shared"}
	if found.AuthMode == "per_user" {
		v.Mode = oauth.ConnectPurposePerUser
		conn, err := p.UserConnectionOf(ctx, server, uid)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("connections.link", err), nil
		}
		v.State, v.Account = perUserState(conn)
		note := "opening it signs your owner's own account in"
		if v.State == connStateConnected {
			note = "already connected; opening it signs in again and replaces the current sign-in"
		}
		g.fillLink(ctx, &v, uid, server, oauth.ConnectPurposePerUser, callerID, note)
	} else {
		conn, err := p.SharedConnection(ctx, server)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("connections.link", err), nil
		}
		if conn == nil {
			return mcp.NewToolResultErrorf("%s does not use an OAuth sign-in (nothing to connect)", server), nil
		}
		v.State, v.Account = sharedState(conn)
		switch {
		case !admin:
			return mcp.NewToolResultErrorf("%s uses one shared account, and only an admin can sign it in: %s. Nothing to open.", server, g.askAdminNote(ctx, server)), nil
		case !conn.CanAuthorize:
			return mcp.NewToolResultErrorf("%s uses a pasted token; set a new one in the dashboard (Servers → Auth). Nothing to open.", server), nil
		}
		note := sharedSignInAs(conn)
		if v.State == connStateConnected {
			note = "already signed in; opening it signs in again and replaces the current sign-in (" + note + ")"
		}
		g.fillLink(ctx, &v, uid, server, oauth.ConnectPurposeShared, callerID, note)
	}
	if v.ConnectLink == "" {
		return mcp.NewToolResultErrorf("no connect link for %s: %s", server, v.Note), nil
	}
	text := fmt.Sprintf("%s (%s, %s): connect link (one-time, %d minutes): %s — %s.\n\n%s",
		v.Server, v.Mode, v.State, v.LinkExpiresIn/60, v.ConnectLink, v.Note, connectGuidance)
	res := mcp.NewToolResultText(text)
	res.StructuredContent = map[string]any{"server": v, "guidance": connectGuidance}
	return res, nil
}

// askAdminNote tells a member's agent who can sign a shared server in.
func (g *Gateway) askAdminNote(ctx context.Context, upstream string) string {
	emails := g.adminEmails(ctx)
	if len(emails) == 0 {
		return fmt.Sprintf("%s needs an admin to sign it in; ask a toolyard admin", upstream)
	}
	return fmt.Sprintf("%s needs an admin to sign it in; ask an admin (%s)", upstream, strings.Join(emails, ", "))
}

func (g *Gateway) adminEmails(ctx context.Context) []string {
	g.mu.RLock()
	d := g.directory
	g.mu.RUnlock()
	if d == nil {
		return nil
	}
	emails, err := d.AdminEmails(ctx)
	if err != nil {
		return nil
	}
	return emails
}

// connectLink mints a one-time connect link for uid on upstream. It needs
// the provider and the public URL; the error says which is missing in
// words the agent can pass on.
func (g *Gateway) connectLink(ctx context.Context, uid, upstream, purpose, callerID string) (string, error) {
	p := g.connectProvider()
	if p == nil {
		return "", errors.New("connect links are not available on this gateway; connect from the toolyard dashboard")
	}
	base := strings.TrimRight(g.publicURL, "/")
	if base == "" {
		return "", errors.New("toolyard has no public URL, so it cannot make a connect link; connect from the toolyard dashboard")
	}
	ticket, err := p.IssueConnectTicket(ctx, uid, upstream, purpose, callerID)
	if err != nil {
		return "", fmt.Errorf("could not make a connect link: %w", err)
	}
	_ = g.audit.Write(ctx, audit.Event{
		EventType:     EventConnectLinkIssued,
		AgentID:       callerID,
		UpstreamName:  upstream,
		ResultSummary: purpose + " connect link for " + upstream,
		Raiser:        actor.Raiser{OwnerUserID: uid},
	})
	return base + oauth.ConnectLinkPath + ticket, nil
}

// perUserConnectMessage words the refusal of a per_user call whose owner
// is not signed in (or whose sign-in cannot be used), with a fresh connect
// link when one can be made; without one it falls back to the dashboard's
// My connections page, as before.
func (g *Gateway) perUserConnectMessage(ctx context.Context, upstream, uid, callerID string, cause error) string {
	link, lerr := g.connectLink(ctx, uid, upstream, oauth.ConnectPurposePerUser, callerID)
	if lerr != nil {
		if errors.Is(cause, ErrUserSignInUnusable) {
			return fmt.Sprintf("%s runs as each person's own account, and the sign-in of the person who owns this agent could not be used right now (%v). "+
				"If this keeps happening, open %s and reconnect %s. The call was not made.", upstream, cause, g.connectionsURL(), upstream)
		}
		return fmt.Sprintf("%s runs as each person's own account, and the person who owns this agent has not connected theirs yet. "+
			"Open %s, connect %s, then retry. The call was not made.", upstream, g.connectionsURL(), upstream)
	}
	minutes := int(oauth.ConnectTicketTTL.Minutes())
	switch {
	case errors.Is(cause, ErrUserSignInUnusable):
		return fmt.Sprintf("%s runs as you, and your sign-in could not be used right now (%v). "+
			"Open this link to sign in again (one-time, %d minutes): %s — then ask me to retry. Nothing was run.", upstream, cause, minutes, link)
	case g.perUserNeedsReauth(ctx, upstream, uid):
		return fmt.Sprintf("%s runs as you, and your sign-in to it has expired. "+
			"Open this link to sign in again (one-time, %d minutes): %s — then ask me to retry. Nothing was run.", upstream, minutes, link)
	}
	return fmt.Sprintf("%s runs as you and isn't connected yet. "+
		"Open this link to connect it (one-time, %d minutes): %s — then ask me to retry. Nothing was run.", upstream, minutes, link)
}

// perUserNeedsReauth reports whether uid's row on upstream says a new
// sign-in is needed (as opposed to never having connected).
func (g *Gateway) perUserNeedsReauth(ctx context.Context, upstream, uid string) bool {
	p := g.connectProvider()
	if p == nil {
		return false
	}
	conn, err := p.UserConnectionOf(ctx, upstream, uid)
	if err != nil || conn == nil {
		return false
	}
	state, _ := perUserState(conn)
	return state == connStateNeedsReauth
}

// refuseSharedSignIn is the terminal outcome of a call to a shared OAuth
// server that has no usable token: before the dial, when the request
// would carry no Authorization at all and the token row says why; or
// after it, when the upstream answered 401 (cause). nil means "not that
// case, carry on": the server is not an OAuth server, or links are not
// wired. The upstream is never contacted with nothing in hand, the audit
// row says why, and the agent gets a link (an admin owner) or the admins
// to ask (a member).
func (g *Gateway) refuseSharedSignIn(ctx context.Context, u *upstream, entry toolEntry, agentID, reason, approvalID string,
	cause error, ev *metrics.Event) *mcp.CallToolResult {
	p := g.connectProvider()
	if p == nil {
		return nil
	}
	if cause == nil {
		// Only a request that would go out with no Authorization at all
		// is looked at: a cached bearer, a static key or a PAT means the
		// call proceeds as it always did, without a database read.
		if u.cfg.HeaderFunc == nil || hasAuthorization(u.cfg.HeaderFunc(ctx)) {
			return nil
		}
	}
	conn, err := p.SharedConnection(ctx, entry.upstream)
	if err != nil || conn == nil {
		return nil
	}
	if cause == nil && conn.Usable() {
		// The row says active but no bearer came out (a cold cache that
		// failed to load, a decrypt error): let the call fail on its own
		// terms rather than send the person to sign in for nothing.
		return nil
	}
	uid, _ := g.ownerUser(ctx, agentID)
	msg := g.sharedConnectMessage(ctx, entry.upstream, uid, agentID, conn, cause)
	summary := "shared_signin: no usable token (" + sharedWhy(conn, cause) + ")"
	_ = g.audit.Write(ctx, audit.Event{
		EventType:     audit.EventCallFailed,
		AgentID:       agentID,
		UpstreamName:  entry.upstream,
		ToolName:      entry.tool.Name,
		Reason:        reason,
		ApprovalID:    approvalID,
		ResultSummary: summary,
		Raiser:        actor.Raiser{OwnerUserID: uid},
	})
	ev.Outcome = metrics.OutcomeError
	ev.ErrorClass = "shared_signin"
	return mcp.NewToolResultError(msg)
}

// sharedWhy is the short reason for the audit row and the message.
func sharedWhy(conn *oauth.SharedConnection, cause error) string {
	switch {
	case cause != nil:
		return "the upstream rejected its token (401)"
	case conn.HasToken && conn.State == oauth.StateNeedsReauth:
		return "its sign-in needs to be renewed"
	case conn.HasToken:
		return "its token is not usable (" + conn.State + ")"
	}
	return "it was never signed in"
}

// sharedConnectMessage words the refusal for the agent: an admin owner
// gets a link (and the account to sign in as, when known); a member is
// told which admins can do it.
func (g *Gateway) sharedConnectMessage(ctx context.Context, upstream, uid, callerID string, conn *oauth.SharedConnection, cause error) string {
	why := sharedWhy(conn, cause)
	admin := uid != "" && g.scopeFor(ctx, callerID).All
	if !admin {
		return fmt.Sprintf("%s uses one shared account and %s; %s, then ask me to retry. Nothing was run.",
			upstream, why, g.askAdminNote(ctx, upstream))
	}
	if !conn.CanAuthorize {
		return fmt.Sprintf("%s uses a pasted token and %s. Set a new token in the dashboard (Servers → Auth), then ask me to retry. Nothing was run.", upstream, why)
	}
	link, err := g.connectLink(ctx, uid, upstream, oauth.ConnectPurposeShared, callerID)
	if err != nil {
		return fmt.Sprintf("%s uses one shared account and %s. Sign it in from the dashboard (Servers → Auth), then ask me to retry. Nothing was run.", upstream, why)
	}
	as := ""
	if conn.AccountLabel != "" {
		as = fmt.Sprintf(" Sign in as %s.", conn.AccountLabel)
	}
	return fmt.Sprintf("%s uses one shared account and %s. You're an admin: open this link to sign it in (one-time, %d minutes): %s —%s then ask me to retry. Nothing was run.",
		upstream, why, int(oauth.ConnectTicketTTL.Minutes()), link, as)
}

// hasAuthorization reports whether the headers carry an Authorization
// value, in any letter case.
func hasAuthorization(h map[string]string) bool {
	for k, v := range h {
		if strings.EqualFold(k, "Authorization") && v != "" {
			return true
		}
	}
	return false
}
