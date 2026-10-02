package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/tusharbhardwaj/toolyard/internal/actor"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/oauth"
	"github.com/tusharbhardwaj/toolyard/internal/upstreams"
)

// GET /v1/connect/link/{ticket} is the link an agent shows the person when
// a server needs their sign-in (gateway/connections_tools.go). It needs no
// toolyard session: the ticket, minted for one (user, server, purpose) and
// redeemable once within ten minutes, is the credential. Redeeming it
// starts the ordinary OAuth flow for that user and server, binds the flow
// to this browser with the flow cookie (oauth_flow_cookie.go), and sends
// the browser to the provider; the callback then stores the token and
// says "go back to your agent". A link that is unknown, used or expired
// gets a plain page saying so.
//
// Public GET: no CSRF header or Origin check applies (both are for
// mutations); a member's session cookie riding along is allowed through
// RoleGuard; an operator token can never use it; the unauthenticated
// per-IP throttle applies (security.go).

// connectLinkPath is the route prefix; the ticket is the rest of the path.
// It is oauth.ConnectLinkPath, which the gateway builds links with (a test
// pins the two together); the literal is repeated here because the route
// catalog test reads mux.HandleFunc registrations as string literals.
const connectLinkPath = "/v1/connect/link/"

// Audit event types.
const (
	// connectLinkUsed: a link was redeemed and the sign-in started.
	connectLinkUsed = "connect.link_used"
	// connectLinkRefused: a link was not honoured; Reason says why.
	connectLinkRefused = "connect.link_refused"
	// connectAccountMismatch: a per_user sign-in from a link came back
	// with another account than the person's; nothing was stored.
	connectAccountMismatch = "connect.account_mismatch"
	// connectUnverifiedAccount: a per_user sign-in from a link could not
	// be checked against the person's email (the provider named no
	// account, or the person has no email on file) and was accepted.
	connectUnverifiedAccount = "connect.unverified_account"
)

func (s *Server) connectLinkRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/v1/connect/link/", s.connectLink)
}

// connectLink redeems the ticket in the path and starts the sign-in.
func (s *Server) connectLink(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if s.oauth == nil || s.upstreams == nil {
		connectHTMLPage(w, http.StatusServiceUnavailable, "Not available",
			"toolyard is not set up for sign-in links. Ask an admin to connect the server from the dashboard.")
		return
	}
	ctx := r.Context()
	ticket := strings.TrimPrefix(r.URL.Path, connectLinkPath)
	if ticket == "" || strings.ContainsAny(ticket, "/?#") {
		s.connectLinkRefuse(w, r, "unknown", "", nil, connectLinkDeadText)
		return
	}
	tk, err := s.oauth.RedeemConnectTicket(ctx, ticket)
	if err != nil {
		reason := "error"
		switch {
		case errors.Is(err, oauth.ErrTicketUnknown):
			reason = "unknown"
		case errors.Is(err, oauth.ErrTicketUsed):
			reason = "used"
		case errors.Is(err, oauth.ErrTicketExpired):
			reason = "expired"
		}
		s.connectLinkRefuse(w, r, reason, "", nil, connectLinkDeadText)
		return
	}
	u, err := s.identity.GetUserByID(ctx, tk.UserID)
	if err != nil || u.Status != identity.StatusActive {
		s.connectLinkRefuse(w, r, "user_inactive", tk.Upstream, nil,
			"Your toolyard account is not active, so this link can't be used. Ask a toolyard admin.")
		return
	}
	sv, err := s.upstreams.Get(ctx, tk.Upstream)
	if err != nil {
		if errors.Is(err, upstreams.ErrNotFound) {
			s.connectLinkRefuse(w, r, "server_missing", tk.Upstream, u, "That server no longer exists in toolyard. Ask your agent what it needs now.")
			return
		}
		s.connectLinkRefuse(w, r, "error", tk.Upstream, u, "Something went wrong looking the server up. Ask your agent for a new link.")
		return
	}
	if !sv.Enabled {
		s.connectLinkRefuse(w, r, "server_disabled", tk.Upstream, u, "That server is disabled in toolyard. Ask an admin to enable it, then ask your agent for a new link.")
		return
	}
	// The grant may have gone since the link was minted.
	allowed, err := s.mayUseServer(ctx, u, tk.Upstream)
	if err != nil {
		s.connectLinkRefuse(w, r, "error", tk.Upstream, u, "Something went wrong checking your access. Ask your agent for a new link.")
		return
	}
	if !allowed {
		s.connectLinkRefuse(w, r, "not_granted", tk.Upstream, u, "You no longer have access to that server in toolyard. Ask an admin, then ask your agent for a new link.")
		return
	}
	// A pasted token has no browser sign-in to start.
	cli, err := s.oauth.GetClient(ctx, tk.Upstream)
	if err != nil {
		if errors.Is(err, oauth.ErrClientNotFound) {
			s.connectLinkRefuse(w, r, "no_client", tk.Upstream, u, "An admin still has to finish this server's OAuth setup in the toolyard dashboard (Servers → Auth). Ask them, then ask your agent for a new link.")
			return
		}
		s.connectLinkRefuse(w, r, "error", tk.Upstream, u, "Something went wrong looking the server's sign-in up. Ask your agent for a new link.")
		return
	}
	if cli.AuthorizationEndpoint == "" || cli.AuthorizationEndpoint == "(pat)" {
		s.connectLinkRefuse(w, r, "token_only", tk.Upstream, u, "That server uses a pasted token, not a browser sign-in. An admin sets a new token in the toolyard dashboard (Servers → Auth).")
		return
	}
	var perUser bool
	switch tk.Purpose {
	case oauth.ConnectPurposePerUser:
		if sv.AuthMode != upstreams.AuthPerUser {
			s.connectLinkRefuse(w, r, "mode_changed", tk.Upstream, u, "That server's sign-in changed since the link was made. Ask your agent for a new link.")
			return
		}
		perUser = true
	case oauth.ConnectPurposeShared:
		if sv.AuthMode == upstreams.AuthPerUser {
			s.connectLinkRefuse(w, r, "mode_changed", tk.Upstream, u, "That server's sign-in changed since the link was made. Ask your agent for a new link.")
			return
		}
		if u.Role != identity.RoleAdmin {
			s.connectLinkRefuse(w, r, "not_admin", tk.Upstream, u, "Only an admin can sign in this server's shared account. Ask a toolyard admin to connect it.")
			return
		}
	default:
		s.connectLinkRefuse(w, r, "bad_purpose", tk.Upstream, u, connectLinkDeadText)
		return
	}
	authURL, state, err := s.oauth.BeginFromTicket(ctx, tk.Upstream, u.ID, perUser)
	if err != nil {
		s.connectLinkRefuse(w, r, "begin_failed", tk.Upstream, u, "The sign-in could not be started. Ask your agent for a new link; if it keeps failing, tell a toolyard admin.")
		return
	}
	// The return from the provider proves itself with this cookie, as a
	// dashboard-started flow does.
	s.setOAuthFlowCookie(w, r, state, u.ID)
	_ = s.audit.Write(ctx, audit.Event{
		EventType: connectLinkUsed, AgentID: tk.AgentID, UpstreamName: tk.Upstream,
		ResultSummary: tk.Purpose + " sign-in to " + tk.Upstream + " started from a connect link",
		Raiser:        actor.Raiser{OwnerUserID: u.ID, OwnerEmail: u.Email, OwnerName: u.Label()},
	})
	http.Redirect(w, r, authURL, http.StatusFound)
}

// connectLinkDeadText is what the person reads for a link that cannot be
// told apart from a stale one.
const connectLinkDeadText = "This link has expired or was already used — ask your agent for a new one."

// connectLinkRefuse audits why a link was not honoured and shows the page.
// The ticket itself is never recorded.
func (s *Server) connectLinkRefuse(w http.ResponseWriter, r *http.Request, reason, upstream string, u *identity.User, text string) {
	ev := audit.Event{
		EventType: connectLinkRefused, UpstreamName: upstream, Reason: reason,
		ResultSummary: "connect link refused: " + reason,
	}
	if u != nil {
		ev.Raiser = actor.Raiser{OwnerUserID: u.ID, OwnerEmail: u.Email, OwnerName: u.Label()}
	}
	_ = s.audit.Write(r.Context(), ev)
	status := http.StatusGone
	switch reason {
	case "not_admin", "user_inactive", "not_granted":
		status = http.StatusForbidden
	case "error", "begin_failed":
		status = http.StatusBadGateway
	}
	connectHTMLPage(w, status, "This link can't be used", text)
}

// userFlowError shows a per-user callback failure in words that fit how
// the flow started: a dashboard flow points back at My connections, a
// link-started flow back at the agent.
func (s *Server) userFlowError(w http.ResponseWriter, viaTicket bool, dashboardMsg, linkMsg string) {
	if viaTicket {
		connectHTMLPage(w, http.StatusBadRequest, "Sign-in not completed", linkMsg)
		return
	}
	oauthHTMLError(w, dashboardMsg)
}

// connectAccountMatches is the rule for a per_user sign-in from a link:
// the account the provider reports must be the person's own email,
// case-insensitively. With nothing to compare (no email reported, or none
// on file for the person) it passes; the caller audits that.
func connectAccountMatches(u *identity.User, account string) bool {
	if account == "" || u.Email == "" {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(account), strings.TrimSpace(u.Email))
}

// connectAccountVerified reports whether the match above was a real
// comparison.
func connectAccountVerified(u *identity.User, account string) bool {
	return account != "" && u.Email != ""
}

// connectAccountMismatchPage is shown when the person signed in to the
// provider as somebody else; nothing was stored.
func (s *Server) connectAccountMismatchPage(w http.ResponseWriter, r *http.Request, u *identity.User, upstream, other string) {
	_ = s.audit.Write(r.Context(), audit.Event{
		EventType: connectAccountMismatch, AgentID: "user:" + u.ID, UpstreamName: upstream,
		ResultSummary: upstream + ": the provider reported a different account than the person's email; no token stored",
		Raiser:        actor.Raiser{OwnerUserID: u.ID, OwnerEmail: u.Email, OwnerName: u.Label()},
	})
	connectHTMLPage(w, http.StatusBadRequest, "Wrong account",
		"This link was made for "+u.Email+"; you signed in as "+other+". Sign in with the right account. "+
			"Nothing was stored. Ask your agent for a new link and sign in as "+u.Email+".")
}

// connectAuditUnverified records a per_user sign-in from a link that
// could not be checked against the person's email.
func (s *Server) connectAuditUnverified(r *http.Request, u *identity.User, upstream, account string) {
	why := "the provider named no account"
	if account != "" {
		why = "the person has no email on file"
	}
	_ = s.audit.Write(r.Context(), audit.Event{
		EventType: connectUnverifiedAccount, AgentID: "user:" + u.ID, UpstreamName: upstream,
		Decision:      "unverified_account",
		ResultSummary: upstream + " connected from a link without an account check: " + why,
		Raiser:        actor.Raiser{OwnerUserID: u.ID, OwnerEmail: u.Email, OwnerName: u.Label()},
	})
}

// connectDonePage is the success page for a flow a link started: the
// person came from their agent, so that is where they go back to.
func connectDonePage(w http.ResponseWriter, upstream, account string) {
	as := ""
	if account != "" {
		as = " as <strong>" + htmlEscape(account) + "</strong>"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`<!doctype html><html><head><meta charset="utf-8"><title>Connected — toolyard</title>
<style>body{font:14px system-ui,sans-serif;color:#222;background:#f6f8fa;display:flex;align-items:center;justify-content:center;height:100vh;margin:0}div{background:#fff;border-radius:8px;padding:32px;box-shadow:0 4px 12px rgba(0,0,0,0.08);max-width:420px}h1{margin:0 0 8px;font-size:18px;color:#0a7}</style>
</head><body><div>
<h1>✓ Connected</h1>
<p><strong>` + htmlEscape(upstream) + `</strong> is connected` + as + `.</p>
<p>Go back to T3 Code and tell your agent to retry. You can close this tab.</p>
</div></body></html>`))
}

// connectHTMLPage is the small plain page for anything that is not a
// success: a stale link, a refusal, a wrong account.
func connectHTMLPage(w http.ResponseWriter, status int, title, text string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`<!doctype html><html><head><meta charset="utf-8"><title>` + htmlEscape(title) + ` — toolyard</title>
<style>body{font:14px system-ui,sans-serif;color:#222;background:#fff5f5;display:flex;align-items:center;justify-content:center;height:100vh;margin:0}div{background:#fff;border:1px solid #f99;border-radius:8px;padding:32px;max-width:420px}h1{margin:0 0 8px;font-size:18px;color:#c33}</style>
</head><body><div>
<h1>` + htmlEscape(title) + `</h1>
<p>` + htmlEscape(text) + `</p>
</div></body></html>`))
}
