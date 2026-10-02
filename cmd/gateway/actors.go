package main

// actors.go is where the gateway binary learns WHO is behind a request
// at its edges: the /mcp ingress (agent token → agent, owner, client and
// session), the push notifications it sends (tokens bound to the person
// they go to) and the Telegram buttons (the paired Telegram user, mapped
// to the owner when the install names one). Everything here is plain
// functions over http.Request and the identity/approval types, so
// main_test.go can exercise them without starting the server.

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/tusharbhardwaj/toolyard/internal/actor"
	"github.com/tusharbhardwaj/toolyard/internal/api"
	"github.com/tusharbhardwaj/toolyard/internal/approval"
	"github.com/tusharbhardwaj/toolyard/internal/gateway"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/inbox"
)

// Headers the /mcp ingress reads besides the credential.
const (
	// headerMCPSession is the MCP transport's session id.
	headerMCPSession = "Mcp-Session-Id"
	// headerT3Session and headerBKAgentSession carry the client's own
	// session id (a T3 thread, a bkt3 agent session). A trusted proxy
	// stamps them; from anyone else they are the caller's claim.
	headerT3Session      = "x-t3-session-id"
	headerBKAgentSession = "x-bk-agent-session"
	// headerClientKind lets a client name itself outright (t3,
	// claude_code, codex, cursor, opencode, cli, browser) instead of being
	// sniffed from the User-Agent.
	headerClientKind = "x-toolyard-client"
)

// mcpIngress is what the /mcp guard learned about a request: the
// verified agent and the raiser built from it and the request.
type mcpIngress struct {
	agent  *identity.Agent
	raiser actor.Raiser
}

type mcpIngressKey struct{}

func withMCPIngress(ctx context.Context, in mcpIngress) context.Context {
	return context.WithValue(ctx, mcpIngressKey{}, in)
}

func mcpIngressFrom(ctx context.Context) (mcpIngress, bool) {
	in, ok := ctx.Value(mcpIngressKey{}).(mcpIngress)
	return in, ok
}

// mcpAuth is the /mcp authentication layer. guard verifies the agent
// token once per request and puts the agent and its raiser on the
// request context; contextFunc, installed with
// server.WithHTTPContextFunc, hands them to the MCP server's per-call
// context as the gateway caller id and the actor.Raiser every audit row,
// approval and metric below will carry.
type mcpAuth struct {
	verify      func(ctx context.Context, token string) (*identity.Agent, error)
	requireAuth bool
	sec         api.SecurityOptions
}

// guard rejects calls that carry a credential we can't verify, so a
// stale token surfaces as a clear 401 instead of silently falling through
// to the anonymous bucket. With requireAuth (on when -public-url is set)
// anonymous traffic is rejected too: the dashboard is exposed publicly so
// unauthenticated callers have no business calling our tools.
func (a mcpAuth) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, ok := mcpCredential(r)
		if !ok {
			if a.requireAuth {
				writeMCPUnauthorized(w, "toolyard: bearer token required")
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		ag, err := a.verify(r.Context(), raw)
		if errors.Is(err, identity.ErrAgentTokenInvalid) {
			writeMCPUnauthorized(w, "toolyard: invalid agent token; re-enroll via the dashboard")
			return
		}
		if err != nil {
			// Not a verdict on the token (the database could not answer):
			// a 401 here would tell clients to throw away a good token.
			log.Printf("mcp-auth: WARN token check failed: %v", err)
			writeMCPUnavailable(w, "toolyard: could not check the agent token right now; retry")
			return
		}
		in := mcpIngress{agent: ag, raiser: mcpRaiser(r, ag, a.sec)}
		next.ServeHTTP(w, r.WithContext(withMCPIngress(r.Context(), in)))
	})
}

// contextFunc reads what guard verified. mcp-go derives the call context
// from the request's, so the ingress is on ctx; r is checked too in case
// a transport rebuilt the context.
func (a mcpAuth) contextFunc(ctx context.Context, r *http.Request) context.Context {
	in, ok := mcpIngressFrom(ctx)
	if !ok && r != nil {
		in, ok = mcpIngressFrom(r.Context())
	}
	if !ok || in.agent == nil {
		return ctx
	}
	return actor.WithRaiser(gateway.WithAgentID(ctx, in.agent.ID), in.raiser)
}

func writeMCPUnavailable(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", "5")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte(`{"jsonrpc":"2.0","error":{"code":-32002,"message":"` + msg + `"}}`))
}

func writeMCPUnauthorized(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"jsonrpc":"2.0","error":{"code":-32001,"message":"` + msg + `"}}`))
}

// mcpRaiser is who raised an /mcp call: the verified agent and its owner,
// the client IP (X-Forwarded-For only behind a trusted proxy), the MCP
// session, the client's own session id (always the caller's claim), and
// the client kind when the request says. Via is left for the gateway,
// which knows the path in.
func mcpRaiser(r *http.Request, ag *identity.Agent, sec api.SecurityOptions) actor.Raiser {
	raiser := actor.Raiser{
		CallerID:     ag.ID,
		AgentName:    ag.Name,
		AgentKind:    ag.Kind,
		OwnerUserID:  ag.Owner,
		OwnerEmail:   ag.OwnerEmail,
		OwnerName:    ag.OwnerName(),
		ClientIP:     sec.ClientIP(r),
		MCPSessionID: r.Header.Get(headerMCPSession),
		ClientKind:   clientKindFromRequest(r),
	}
	sid := r.Header.Get(headerT3Session)
	if sid == "" {
		sid = r.Header.Get(headerBKAgentSession)
	}
	if sid = actor.Clean(sid); sid != "" {
		raiser.ClientSessionID = sid
		// Always the caller's word. Our trusted proxies (Traefik, socat)
		// pass client headers through unchanged, so sitting behind one
		// says nothing about who set this header; a verified mode would
		// need T3 to sign it (future work). X-Forwarded-For is different:
		// the proxy writes that itself, so ClientIP still trusts it.
		raiser.ClientSessionClaimed = true
	}
	return cleanRaiser(raiser)
}

// cleanRaiser runs every field through actor.Clean so a header value is
// safe to store as given.
func cleanRaiser(r actor.Raiser) actor.Raiser {
	for _, p := range []*string{&r.CallerID, &r.AgentName, &r.AgentKind, &r.OwnerUserID, &r.OwnerEmail, &r.OwnerName,
		&r.MCPSessionID, &r.AgentSessionID, &r.ClientSessionID, &r.ClientKind, &r.ClientName, &r.ClientIP, &r.Via} {
		*p = actor.Clean(*p)
	}
	return r
}

// knownClientKinds are the Raiser.ClientKind values a client may claim
// outright in x-toolyard-client.
var knownClientKinds = map[string]bool{
	"t3": true, "claude_code": true, "codex": true, "cursor": true, "opencode": true, "cli": true, "browser": true,
}

// clientKindFromRequest is the client kind the request declares: the
// x-toolyard-client header when present (a known kind as is, anything
// else through the clientInfo mapping), else a User-Agent sniff, else
// empty so the gateway can fill it from the MCP clientInfo.
func clientKindFromRequest(r *http.Request) string {
	if v := strings.ToLower(actor.Clean(r.Header.Get(headerClientKind))); v != "" {
		if knownClientKinds[v] {
			return v
		}
		return gateway.ClientKindOf(v)
	}
	return clientKindFromUserAgent(r.UserAgent())
}

// userAgentKinds is the User-Agent sniffing table; first match wins.
// Needles are specific on purpose: "t3" alone would match a version hash.
var userAgentKinds = []struct{ needle, kind string }{
	{"t3-code", "t3"},
	{"t3code", "t3"},
	{"claude-code", "claude_code"},
	{"claude code", "claude_code"},
	{"opencode", "opencode"},
	{"codex", "codex"},
	{"cursor", "cursor"},
	{"toolyard-cli", "cli"},
	{"mozilla/", "browser"},
}

// clientKindFromUserAgent maps a User-Agent to a Raiser.ClientKind, or ""
// when it names no client we know.
func clientKindFromUserAgent(ua string) string {
	ua = strings.ToLower(ua)
	for _, e := range userAgentKinds {
		if strings.Contains(ua, e.needle) {
			return e.kind
		}
	}
	return ""
}

// inboxTapToken is the notification tap token for one recipient: bound to
// them when the inbox can mint one, so a tap is recorded as theirs; else
// the unbound legacy token.
func inboxTapToken(p inbox.Push, recipientID string) string {
	if p.TapTokenFor != nil {
		return p.TapTokenFor(recipientID)
	}
	return p.TapToken
}

// telegramDecider is who decided through a Telegram button. decidedBy is
// the poller's "telegram:<telegram user id>" (the paired identity it
// verified), which is the instrument and the only identity we have: the
// pairing settings store a Telegram chat id and user id, never a link to
// a toolyard user, so UserID stays empty rather than guessing a person.
// The name says what the row knows.
func telegramDecider(decidedBy string) actor.Decider {
	d := approval.DeciderFromLegacy(decidedBy)
	d.UserID, d.Email, d.Via = "", "", actor.ViaTelegram
	if d.Ref != "" {
		d.Name = "Telegram user " + d.Ref
	}
	return d
}
