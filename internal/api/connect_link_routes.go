package api

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/tusharbhardwaj/toolyard/internal/actor"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/gateway"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/oauth"
	"github.com/tusharbhardwaj/toolyard/internal/upstreams"
)

// /v1/connect/link/{ticket} is the link an agent shows the person when a
// server needs their sign-in (gateway/connections_tools.go). The ticket,
// minted for one (user, server, purpose) and redeemable once within ten
// minutes, is the credential; no toolyard session is needed to open it.
//
//	GET   renders a confirm page ("Connect <server> for <person>") with a
//	      Continue button and changes nothing, so a link previewer or a
//	      drive-by fetch cannot use the ticket up. The response sets a
//	      SameSite=Strict nonce cookie whose value the form carries.
//	POST  (the Continue button) checks the nonce cookie against the form
//	      and Sec-Fetch-Site when the browser sends it, then redeems the
//	      ticket, starts the OAuth flow for that user and server, binds the
//	      flow to this browser with the flow cookie (oauth_flow_cookie.go)
//	      and sends the browser to the provider with a 303. The callback
//	      stores the token and says "go back to your agent".
//	HEAD  answers headers only and never redeems.
//
// Who may continue: a shared-account link needs this browser to hold a
// toolyard session for an active admin (the flow is then theirs), and is
// never honoured for a server whose shared account already works. A
// per-user link is refused in a browser signed in to toolyard as somebody
// else; whether the browser held the person's own session is recorded on
// the flow (opener_verified), because the callback stores a token whose
// account the provider did not name only then.
//
// The confirm page's Content-Security-Policy widens form-action to the
// provider's authorize origin: browsers apply the page's form-action to
// the redirect the POST answers with. The pages use the dashboard's
// stylesheet, since the policy allows no inline style.
//
// Public route: the CSRF header and JSON-body rules of HardenAPI are
// lifted for it (the nonce is the CSRF proof), the Origin check stays, a
// member's session passes RoleGuard, an operator token can never use it,
// and the unauthenticated per-IP throttle applies (security.go).
//
// The dashboard mints the same links ("Copy link for another browser"):
// every sign-in it starts stops first and offers either to continue in
// this browser or to copy a link, so a person can finish the sign-in in
// another browser profile. The begin routes take {"mode":"link"} for that:
// POST /v1/me/connections/{server}/begin (the caller's own account),
// POST /v1/servers/{name}/oauth/begin (admin, a shared account) and
// POST /v1/github/setup/begin (admin, the GitHub App). Such a ticket
// records ConnectMintedBy(person) instead of an agent, and is honoured on
// exactly the terms an agent's link is. A GitHub App setup ticket is a
// kind of its own: its Continue shows GitHub's manifest form in this
// browser instead of starting a sign-in (github_setup.go).

// connectLinkPath is the route prefix; the ticket is the rest of the path.
// It is oauth.ConnectLinkPath, which the gateway builds links with (a test
// pins the two together); the literal is repeated here because the route
// catalog test reads mux.HandleFunc registrations as string literals.
const connectLinkPath = "/v1/connect/link/"

// connectNoncePrefix names the per-ticket nonce cookie; the rest of the
// name is a prefix of the ticket's hash, so two links open in one browser
// do not clobber each other.
const connectNoncePrefix = "toolyard_connect_"

// connectModeLink is the begin routes' mode that mints a connect link for
// another browser instead of starting the flow in this one.
const connectModeLink = "link"

// Audit event types.
const (
	// connectLinkUsed: a link was redeemed and the sign-in started.
	connectLinkUsed = "connect.link_used"
	// connectLinkRefused: a Continue was not honoured; Reason says why.
	// Showing the confirm page (GET) is never audited: it changes nothing.
	connectLinkRefused = "connect.link_refused"
	// connectAccountMismatch: a sign-in from a link came back with another
	// account than the rule allows; nothing was stored.
	connectAccountMismatch = "connect.account_mismatch"
	// connectUnverifiedAccount: a per-user sign-in from a link could not
	// be checked against the person's email and was accepted because the
	// browser that redeemed the link held the person's toolyard session.
	connectUnverifiedAccount = "connect.unverified_account"
)

// Outcomes of connectAccountCheck.
const (
	accountMatch        = "match"
	accountMismatch     = "mismatch"
	accountUnverifiable = "unverifiable"
)

func (s *Server) connectLinkRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/v1/connect/link/", s.connectLink)
}

// connectTicketFromPath returns the ticket in the path, "" when the path
// is not exactly one segment under the prefix.
func connectTicketFromPath(path string) string {
	ticket := strings.TrimPrefix(path, connectLinkPath)
	if ticket == "" || strings.ContainsAny(ticket, "/?#") {
		return ""
	}
	return ticket
}

func (s *Server) connectLink(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	switch r.Method {
	case http.MethodGet:
		s.connectLinkConfirm(w, r)
	case http.MethodHead:
		// Previewers and link checkers: nothing to see, nothing changes.
		w.WriteHeader(http.StatusOK)
	case http.MethodPost:
		s.connectLinkContinue(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, "GET or POST")
	}
}

// connectLinkLookup is what both the page and the Continue need to know
// about a ticket: its row, the person, the server and its OAuth client.
type connectLinkLookup struct {
	ticket string
	tk     *oauth.ConnectTicket
	user   *identity.User
	server *upstreams.Server
	client *oauth.ClientRecord
}

// connectLinkRefusal is a precondition that failed: the audit reason, the
// text for the person, and whether a toolyard sign-in would help.
type connectLinkRefusal struct {
	reason string
	text   string
	signIn bool
}

// lookupConnectLink reads the ticket without redeeming it and checks
// everything about it that does not depend on who is asking: the person
// is active, the server exists, is enabled, still granted to the person,
// has a real browser sign-in, and the ticket's purpose fits its mode.
func (s *Server) lookupConnectLink(r *http.Request) (*connectLinkLookup, *connectLinkRefusal) {
	ctx := r.Context()
	ticket := connectTicketFromPath(r.URL.Path)
	if ticket == "" {
		return nil, &connectLinkRefusal{reason: "unknown", text: connectLinkDeadText}
	}
	tk, err := s.oauth.PeekConnectTicket(ctx, ticket)
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
		return nil, &connectLinkRefusal{reason: reason, text: connectLinkDeadText}
	}
	l := &connectLinkLookup{ticket: ticket, tk: tk}
	if tk.Kind != oauth.ConnectKindSignIn {
		// A GitHub App setup ticket never starts a sign-in (the handlers
		// send it to github_setup.go before this runs).
		return l, &connectLinkRefusal{reason: "bad_purpose", text: connectLinkDeadText}
	}
	u, err := s.identity.GetUserByID(ctx, tk.UserID)
	if err != nil || u.Status != identity.StatusActive {
		return l, &connectLinkRefusal{reason: "user_inactive", text: "The toolyard account this link was made for is not active, so it can't be used. Ask a toolyard admin."}
	}
	l.user = u
	sv, err := s.upstreams.Get(ctx, tk.Upstream)
	if err != nil {
		if errors.Is(err, upstreams.ErrNotFound) {
			return l, &connectLinkRefusal{reason: "server_missing", text: "That server no longer exists in toolyard. Ask your agent what it needs now."}
		}
		return l, &connectLinkRefusal{reason: "error", text: "Something went wrong looking the server up. Ask your agent for a new link."}
	}
	l.server = sv
	if !sv.Enabled {
		return l, &connectLinkRefusal{reason: "server_disabled", text: "That server is disabled in toolyard. Ask an admin to enable it, then ask your agent for a new link."}
	}
	// The grant may have gone since the link was minted.
	allowed, err := s.mayUseServer(ctx, u, tk.Upstream)
	if err != nil {
		return l, &connectLinkRefusal{reason: "error", text: "Something went wrong checking access. Ask your agent for a new link."}
	}
	if !allowed {
		return l, &connectLinkRefusal{reason: "not_granted", text: "The person this link was made for no longer has access to that server in toolyard. Ask an admin, then ask your agent for a new link."}
	}
	// A pasted token has no browser sign-in to start.
	cli, err := s.oauth.GetClient(ctx, tk.Upstream)
	if err != nil {
		if errors.Is(err, oauth.ErrClientNotFound) {
			return l, &connectLinkRefusal{reason: "no_client", text: "An admin still has to finish this server's OAuth setup in the toolyard dashboard (Servers → Auth). Ask them, then ask your agent for a new link."}
		}
		return l, &connectLinkRefusal{reason: "error", text: "Something went wrong looking the server's sign-in up. Ask your agent for a new link."}
	}
	if cli.AuthorizationEndpoint == "" || cli.AuthorizationEndpoint == "(pat)" {
		return l, &connectLinkRefusal{reason: "token_only", text: "That server uses a pasted token, not a browser sign-in. An admin sets a new token in the toolyard dashboard (Servers → Auth)."}
	}
	l.client = cli
	switch tk.Purpose {
	case oauth.ConnectPurposePerUser:
		if sv.AuthMode != upstreams.AuthPerUser {
			return l, &connectLinkRefusal{reason: "mode_changed", text: "That server's sign-in changed since the link was made. Ask your agent for a new link."}
		}
	case oauth.ConnectPurposeShared:
		if sv.AuthMode == upstreams.AuthPerUser {
			return l, &connectLinkRefusal{reason: "mode_changed", text: "That server's sign-in changed since the link was made. Ask your agent for a new link."}
		}
		// The gateway mints shared links for admin owners only; a ticket
		// for anyone else is not honoured whoever opens it.
		if u.Role != identity.RoleAdmin {
			return l, &connectLinkRefusal{reason: "not_admin", text: "Only an admin can sign in this server's shared account, and this link was not made for one. Ask a toolyard admin to connect it."}
		}
	default:
		return l, &connectLinkRefusal{reason: "bad_purpose", text: connectLinkDeadText}
	}
	return l, nil
}

// connectLinkConfirm (GET) shows whose link this is and what Continue
// does. It redeems nothing and audits nothing.
func (s *Server) connectLinkConfirm(w http.ResponseWriter, r *http.Request) {
	if s.oauth == nil || s.upstreams == nil {
		writeConnectPage(w, http.StatusServiceUnavailable, "Not available",
			"<h1>Not available</h1><p>toolyard is not set up for sign-in links. Ask an admin to connect the server from the dashboard.</p>")
		return
	}
	if s.connectLinkKind(r) == oauth.ConnectKindGitHubAppSetup {
		s.githubSetupLinkConfirm(w, r)
		return
	}
	l, refusal := s.lookupConnectLink(r)
	if refusal != nil {
		writeConnectPage(w, connectRefusalStatus(refusal.reason), "This link can't be used",
			"<h1>This link can't be used</h1><p>"+htmlEscape(refusal.text)+"</p>"+s.signInHTML(refusal.signIn))
		return
	}
	sess, hasSess := s.sessionUser(r)
	shared := l.tk.Purpose == oauth.ConnectPurposeShared
	// What the browser's own session says already: a member cannot
	// continue a shared link, and a per-user link is somebody else's.
	// Without a session nothing is known yet (the dashboard's session
	// cookie is SameSite=Strict, so a click from the chat does not carry
	// it; the Continue POST, same-site, will).
	switch {
	case shared && hasSess && (sess.Role != identity.RoleAdmin || sess.Status != identity.StatusActive):
		writeConnectPage(w, http.StatusForbidden, "Admin sign-in needed",
			"<h1>Sign in to toolyard as an admin to continue</h1><p>This link signs in the shared account of <strong>"+htmlEscape(l.tk.Upstream)+
				"</strong> that everyone's agents use, and only an admin may do that. This browser is signed in to toolyard as a member.</p>"+s.signInHTML(true))
		return
	case !shared && hasSess && sess.ID != l.user.ID:
		writeConnectPage(w, http.StatusForbidden, "This link is for someone else",
			"<h1>This link is for someone else</h1><p>This browser is signed in to toolyard as a different person than the one this link was made for. Nothing happened. Ask your agent for a link of your own.</p>")
		return
	}
	nonce, err := randomNonce()
	if err != nil {
		writeConnectPage(w, http.StatusInternalServerError, "Not available", "<h1>Not available</h1><p>Please try again.</p>")
		return
	}
	// Behind HTTPS the cookie is __Host- (see hostCookie): only this host
	// can have set it.
	name, path := s.hostCookie(r, connectNonceName(l.ticket), connectLinkPath)
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    nonce,
		Path:     path,
		HttpOnly: true,
		Secure:   s.security.IsBehindHTTPS(r),
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(oauth.ConnectTicketTTL.Seconds()),
	})
	w.Header().Set("Content-Security-Policy", connectCSP(l.client.AuthorizationEndpoint))
	writeConnectPage(w, http.StatusOK, "Connect "+l.tk.Upstream, s.connectConfirmHTML(l, nonce, shared, hasSess))
}

// connectConfirmHTML is the confirm page's body.
func (s *Server) connectConfirmHTML(l *connectLinkLookup, nonce string, shared, hasSess bool) string {
	server := htmlEscape(l.tk.Upstream)
	person := htmlEscape(nonEmpty(l.user.Email, l.user.Label()))
	provider := ""
	if u, err := url.Parse(l.client.AuthorizationEndpoint); err == nil && u.Host != "" {
		provider = " at <strong>" + htmlEscape(u.Host) + "</strong>"
	}
	var b strings.Builder
	b.WriteString("<h1>Connect " + server + " for " + person + "</h1>")
	if shared {
		b.WriteString("<p>This signs in the <strong>shared account</strong> of " + server + " that everyone's agents use.</p>")
		if !hasSess {
			b.WriteString("<p>Continue works only in a browser signed in to toolyard as an admin. Not signed in here? " +
				`<a href="` + htmlEscape(s.signInHref()) + `">Sign in to toolyard</a> first, then open this link again.</p>`)
		}
	} else {
		b.WriteString("<p>This signs in the account only " + person + "'s agents use.</p>")
	}
	if l.tk.MintedInDashboard() {
		// The person copied it from the dashboard for this browser.
		if !shared && l.user.Email != "" {
			b.WriteString("<p>Sign in there with the account for <strong>" + htmlEscape(l.user.Email) + "</strong>: an account with another email is refused.</p>")
		}
		b.WriteString("<p>You'll be sent" + provider + " to sign in, then come back here. The toolyard dashboard then shows the connection.</p>")
	} else {
		b.WriteString("<p>You'll be sent" + provider + " to sign in, then come back here. Afterwards, tell your agent to retry.</p>")
	}
	b.WriteString(`<form method="post" action="` + htmlEscape(connectLinkPath+l.ticket) + `">` +
		`<input type="hidden" name="nonce" value="` + htmlEscape(nonce) + `">` +
		`<button type="submit">Continue</button></form>`)
	return b.String()
}

// connectLinkContinue (POST) redeems the ticket and starts the sign-in.
func (s *Server) connectLinkContinue(w http.ResponseWriter, r *http.Request) {
	if s.oauth == nil || s.upstreams == nil {
		writeConnectPage(w, http.StatusServiceUnavailable, "Not available",
			"<h1>Not available</h1><p>toolyard is not set up for sign-in links. Ask an admin to connect the server from the dashboard.</p>")
		return
	}
	ctx := r.Context()
	ticket := connectTicketFromPath(r.URL.Path)
	if ticket == "" {
		s.connectLinkRefuse(w, r, "", nil, &connectLinkRefusal{reason: "unknown", text: connectLinkDeadText})
		return
	}
	// The form came from this origin's confirm page: the browser says so
	// (Sec-Fetch-Site), and the nonce it carries is the one that page's
	// response set as a cookie.
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" {
		s.connectLinkRefuse(w, r, "", nil, &connectLinkRefusal{reason: "cross_site", text: "This page only works from toolyard's own confirm page. Open the link again."})
		return
	}
	if err := r.ParseForm(); err != nil {
		s.connectLinkRefuse(w, r, "", nil, &connectLinkRefusal{reason: "bad_form", text: "The form could not be read. Open the link again."})
		return
	}
	nonceName, _ := s.hostCookie(r, connectNonceName(ticket), connectLinkPath)
	nonceCookie, err := r.Cookie(nonceName)
	formNonce := r.PostForm.Get("nonce")
	if err != nil || formNonce == "" || !hmac.Equal([]byte(nonceCookie.Value), []byte(formNonce)) {
		s.connectLinkRefuse(w, r, "", nil, &connectLinkRefusal{reason: "nonce", text: "This page is stale or did not come from toolyard's confirm page. Open the link again."})
		return
	}
	s.clearConnectNonce(w, r, ticket)

	if s.connectLinkKind(r) == oauth.ConnectKindGitHubAppSetup {
		s.githubSetupLinkContinue(w, r, ticket)
		return
	}
	l, refusal := s.lookupConnectLink(r)
	if refusal != nil {
		upstream := ""
		var u *identity.User
		if l != nil && l.tk != nil {
			upstream = l.tk.Upstream
			u = l.user
		}
		s.connectLinkRefuse(w, r, upstream, u, refusal)
		return
	}
	sess, hasSess := s.sessionUser(r)
	flowUser := l.user
	perUser := l.tk.Purpose == oauth.ConnectPurposePerUser
	openerVerified := false
	if perUser {
		if hasSess && sess.ID != l.user.ID {
			s.connectLinkRefuse(w, r, l.tk.Upstream, l.user, &connectLinkRefusal{reason: "other_user",
				text: "This browser is signed in to toolyard as a different person than the one this link was made for. Nothing happened. Ask your agent for a link of your own."})
			return
		}
		openerVerified = hasSess && sess.ID == l.user.ID
	} else {
		// The org-wide account: an admin, here, now. The flow is theirs.
		if !hasSess || sess.Role != identity.RoleAdmin || sess.Status != identity.StatusActive {
			s.connectLinkRefuse(w, r, l.tk.Upstream, l.user, &connectLinkRefusal{reason: "admin_session",
				text: "Sign in to toolyard as an admin to continue. This link signs in the shared account of " + l.tk.Upstream + " that everyone's agents use.", signIn: true})
			return
		}
		conn, err := s.oauth.SharedConnection(ctx, l.tk.Upstream)
		if err != nil {
			s.connectLinkRefuse(w, r, l.tk.Upstream, sess, &connectLinkRefusal{reason: "error", text: "Something went wrong looking the server's sign-in up. Ask your agent for a new link."})
			return
		}
		if conn.Usable() {
			// Never replaced through a link: a link can be forwarded.
			as := ""
			if conn.AccountLabel != "" {
				as = " as " + conn.AccountLabel
			}
			s.connectLinkRefuse(w, r, l.tk.Upstream, sess, &connectLinkRefusal{reason: "already_connected",
				text: l.tk.Upstream + " is already signed in" + as + ", so this link does nothing. Switching the shared account is done by an admin in the toolyard dashboard (Servers → Auth)."})
			return
		}
		flowUser = sess
		openerVerified = true
	}
	// Everything checked: the ticket is spent now, once.
	if _, err := s.oauth.RedeemConnectTicket(ctx, ticket); err != nil {
		reason := "error"
		switch {
		case errors.Is(err, oauth.ErrTicketUnknown):
			reason = "unknown"
		case errors.Is(err, oauth.ErrTicketUsed):
			reason = "used"
		case errors.Is(err, oauth.ErrTicketExpired):
			reason = "expired"
		}
		s.connectLinkRefuse(w, r, l.tk.Upstream, l.user, &connectLinkRefusal{reason: reason, text: connectLinkDeadText})
		return
	}
	authURL, state, err := s.oauth.BeginFromTicket(ctx, l.tk.Upstream, flowUser.ID, perUser, openerVerified)
	if err != nil {
		s.connectLinkRefuse(w, r, l.tk.Upstream, flowUser, &connectLinkRefusal{reason: "begin_failed",
			text: "The sign-in could not be started. Ask your agent for a new link; if it keeps failing, tell a toolyard admin."})
		return
	}
	// The return from the provider proves itself with this cookie, as a
	// dashboard-started flow does.
	s.setOAuthFlowCookie(w, r, state, flowUser.ID)
	summary := l.tk.Purpose + " sign-in to " + l.tk.Upstream + " started from a connect link"
	if flowUser.ID != l.tk.UserID {
		summary += " minted for another admin"
	}
	_ = s.audit.Write(ctx, audit.Event{
		EventType: connectLinkUsed, AgentID: l.tk.AgentID, UpstreamName: l.tk.Upstream,
		ResultSummary: summary,
		Raiser:        actor.Raiser{OwnerUserID: flowUser.ID, OwnerEmail: flowUser.Email, OwnerName: flowUser.Label()},
	})
	w.Header().Set("Content-Security-Policy", connectCSP(l.client.AuthorizationEndpoint))
	http.Redirect(w, r, authURL, http.StatusSeeOther)
}

// connectLinkDeadText is what the person reads for a link that cannot be
// told apart from a stale one.
const connectLinkDeadText = "This link has expired or was already used — ask your agent for a new one, or copy a new link in the toolyard dashboard."

// connectLinkKind is the kind of the ticket in the path, or
// ConnectKindSignIn when it cannot be read (lookupConnectLink then says
// why). It redeems nothing.
func (s *Server) connectLinkKind(r *http.Request) string {
	tk, err := s.oauth.PeekConnectTicket(r.Context(), connectTicketFromPath(r.URL.Path))
	if err != nil {
		return oauth.ConnectKindSignIn
	}
	return tk.Kind
}

// connectLinkBase is the origin a link minted in the dashboard points at:
// PublicURL, else the host the dashboard was reached on (the person who
// copies the link is on it).
func (s *Server) connectLinkBase(r *http.Request) string {
	if base := strings.TrimRight(s.security.PublicURL, "/"); base != "" {
		return base
	}
	scheme := "http"
	if s.security.IsBehindHTTPS(r) {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// linkableClient reports whether the server's sign-in can be handed to
// another browser: it needs an OAuth client with a real authorization
// endpoint (a pasted token has no browser flow). It answers the request
// when not.
func (s *Server) linkableClient(w http.ResponseWriter, r *http.Request, name string) bool {
	cli, err := s.oauth.GetClient(r.Context(), name)
	switch {
	case errors.Is(err, oauth.ErrClientNotFound):
		writeError(w, http.StatusConflict, "an admin still has to finish this server's OAuth setup (Servers → Auth…)")
		return false
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
		return false
	case cli.AuthorizationEndpoint == "" || cli.AuthorizationEndpoint == "(pat)":
		writeError(w, http.StatusConflict, "this server uses a pasted token: there is no browser sign-in to finish elsewhere")
		return false
	}
	return true
}

// writeDashboardLink answers a begin in link mode: mint runs the ticket's
// insert (the caller has checked that u may do this sign-in), the link is
// audited the way the gateway audits an agent's (the ticket never is), and
// the absolute link goes back to the dashboard, never cached.
func (s *Server) writeDashboardLink(w http.ResponseWriter, r *http.Request, u *identity.User, upstream, what string, mint func() (string, error)) {
	ticket, err := mint()
	if errors.Is(err, oauth.ErrTooManyTickets) {
		writeError(w, http.StatusTooManyRequests, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.audit.Write(r.Context(), audit.Event{
		EventType: gateway.EventConnectLinkIssued, AgentID: oauth.ConnectMintedBy(u.ID), UpstreamName: upstream,
		ResultSummary: what + " connect link for " + upstream + ", copied in the dashboard",
		Raiser:        actor.Raiser{OwnerUserID: u.ID, OwnerEmail: u.Email, OwnerName: u.Label()},
	})
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"mode":         connectModeLink,
		"connect_link": s.connectLinkBase(r) + connectLinkPath + ticket,
		"expires_in":   int(oauth.ConnectTicketTTL.Seconds()),
	})
}

// connectLinkRefuse audits why a Continue was not honoured and shows the
// page. The ticket itself is never recorded.
func (s *Server) connectLinkRefuse(w http.ResponseWriter, r *http.Request, upstream string, u *identity.User, ref *connectLinkRefusal) {
	ev := audit.Event{
		EventType: connectLinkRefused, UpstreamName: upstream, Reason: ref.reason,
		ResultSummary: "connect link refused: " + ref.reason,
	}
	if u != nil {
		ev.Raiser = actor.Raiser{OwnerUserID: u.ID, OwnerEmail: u.Email, OwnerName: u.Label()}
	}
	_ = s.audit.Write(r.Context(), ev)
	title := "This link can't be used"
	if ref.reason == "admin_session" {
		title = "Admin sign-in needed"
	}
	writeConnectPage(w, connectRefusalStatus(ref.reason), title,
		"<h1>"+htmlEscape(title)+"</h1><p>"+htmlEscape(ref.text)+"</p>"+s.signInHTML(ref.signIn))
}

// connectRefusalStatus maps a refusal reason to the page's status.
func connectRefusalStatus(reason string) int {
	switch reason {
	case "not_admin", "admin_session", "user_inactive", "not_granted", "other_user", "nonce", "cross_site", "already_connected":
		return http.StatusForbidden
	case "error", "begin_failed":
		return http.StatusBadGateway
	case "bad_form":
		return http.StatusBadRequest
	}
	return http.StatusGone
}

// connectNonceName is the nonce cookie for one ticket.
func connectNonceName(ticket string) string {
	sum := sha256.Sum256([]byte(ticket))
	return connectNoncePrefix + hex.EncodeToString(sum[:8])
}

func randomNonce() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// clearConnectNonce drops the nonce once the form came back.
func (s *Server) clearConnectNonce(w http.ResponseWriter, r *http.Request, ticket string) {
	name, path := s.hostCookie(r, connectNonceName(ticket), connectLinkPath)
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: "", Path: path, HttpOnly: true,
		Secure: s.security.IsBehindHTTPS(r), SameSite: http.SameSiteStrictMode, MaxAge: -1,
	})
}

// connectCSP is strictCSP with form-action widened to the provider's
// authorize origin: the browser applies the confirm page's form-action to
// the redirect that answers the form's POST.
func connectCSP(authorizeEndpoint string) string {
	formAction := "form-action 'self'"
	if u, err := url.Parse(authorizeEndpoint); err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" {
		formAction += " " + u.Scheme + "://" + u.Host
	}
	return strings.Replace(strictCSP, "form-action 'self'", formAction, 1)
}

// signInHref is where a person signs in to toolyard: the Clerk page when
// Sign in with Google is on, else the dashboard's own login.
func (s *Server) signInHref() string {
	if s.clerk != nil {
		return "/login"
	}
	return "/"
}

// signInHTML is the sign-in link paragraph, or nothing.
func (s *Server) signInHTML(show bool) string {
	if !show {
		return ""
	}
	return `<p><a href="` + htmlEscape(s.signInHref()) + `">Sign in to toolyard</a>, then open the link again.</p>`
}

// connectAccountCheck applies a connect link's account rule to a per-user
// token: the account the provider reports must be the person's own email.
// Both sides are compared ASCII-lowercased. A label the provider did not
// vouch for (email_verified false), no email on either side, or anything
// non-ASCII on either side leaves the rule unable to run.
func connectAccountCheck(u *identity.User, rec *oauth.UserTokenRecord) string {
	label, email := strings.TrimSpace(rec.AccountLabel), strings.TrimSpace(u.Email)
	if label == "" || email == "" || !rec.AccountVerified || !isASCII(label) || !isASCII(email) {
		return accountUnverifiable
	}
	if asciiLower(label) == asciiLower(email) {
		return accountMatch
	}
	return accountMismatch
}

// sameEmail compares two account labels the way connectAccountCheck does;
// comparable is false when either is empty or non-ASCII.
func sameEmail(a, b string) (same, comparable bool) {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	if a == "" || b == "" || !isASCII(a) || !isASCII(b) {
		return false, false
	}
	return asciiLower(a) == asciiLower(b), true
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

func asciiLower(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return r
	}, s)
}

// userFlowError shows a per-user callback failure in words that fit how
// the flow started: a dashboard flow points back at My connections, a
// link-started flow back at the agent.
func (s *Server) userFlowError(w http.ResponseWriter, viaTicket bool, dashboardMsg, linkMsg string) {
	if viaTicket {
		writeConnectPage(w, http.StatusBadRequest, "Sign-in not completed",
			"<h1>Sign-in not completed</h1><p>"+htmlEscape(linkMsg)+"</p>")
		return
	}
	oauthHTMLError(w, dashboardMsg)
}

// connectAccountMismatchPage is shown when the person signed in to the
// provider as somebody else; nothing was stored. It names neither account:
// the opener may not be the person the link was made for.
func (s *Server) connectAccountMismatchPage(w http.ResponseWriter, r *http.Request, u *identity.User, upstream string) {
	_ = s.audit.Write(r.Context(), audit.Event{
		EventType: connectAccountMismatch, AgentID: "user:" + u.ID, UpstreamName: upstream,
		ResultSummary: upstream + ": the provider reported a different account than the person's email; no token stored",
		Raiser:        actor.Raiser{OwnerUserID: u.ID, OwnerEmail: u.Email, OwnerName: u.Label()},
	})
	writeConnectPage(w, http.StatusBadRequest, "Wrong account",
		"<h1>Wrong account</h1><p>You signed in with a different account than this link is for. Nothing was stored. Ask your agent for a new link and sign in with your own account.</p>")
}

// connectUnverifiedPage is shown when the provider's account could not be
// checked against the person's email and the browser that redeemed the
// link had no toolyard session for them; nothing was stored.
func (s *Server) connectUnverifiedPage(w http.ResponseWriter, r *http.Request, u *identity.User, upstream string) {
	_ = s.audit.Write(r.Context(), audit.Event{
		EventType: connectAccountMismatch, AgentID: "user:" + u.ID, UpstreamName: upstream,
		Decision:      "unverifiable",
		ResultSummary: upstream + ": the provider named no checkable account and the browser held no toolyard session for the person; no token stored",
		Raiser:        actor.Raiser{OwnerUserID: u.ID, OwnerEmail: u.Email, OwnerName: u.Label()},
	})
	writeConnectPage(w, http.StatusForbidden, "Sign-in not verified",
		"<h1>This sign-in can't be verified automatically</h1><p>The provider did not say which account you used, so nothing was stored. "+
			"Sign in to toolyard once in this browser, then open the link again.</p>"+s.signInHTML(true))
}

// connectSharedMismatchPage is shown when a shared-account sign-in from a
// link used a different account than the one on file; nothing changed.
func (s *Server) connectSharedMismatchPage(w http.ResponseWriter, r *http.Request, u *identity.User, upstream, prev string) {
	_ = s.audit.Write(r.Context(), audit.Event{
		EventType: connectAccountMismatch, AgentID: "user:" + u.ID, UpstreamName: upstream,
		Decision:      "shared",
		ResultSummary: upstream + ": a connect link signed the shared account in as a different account than the one on file; nothing changed",
		Raiser:        actor.Raiser{OwnerUserID: u.ID, OwnerEmail: u.Email, OwnerName: u.Label()},
	})
	writeConnectPage(w, http.StatusForbidden, "Different account",
		"<h1>Different account</h1><p>"+htmlEscape(upstream)+" is signed in as <strong>"+htmlEscape(prev)+
			"</strong>, and this sign-in used a different account, so nothing changed. Switching the shared account is done by an admin in the toolyard dashboard (Servers → Auth).</p>")
}

// connectAuditUnverified records a per-user sign-in from a link that
// could not be checked against the person's email and was accepted on the
// strength of the person's own toolyard session.
func (s *Server) connectAuditUnverified(r *http.Request, u *identity.User, upstream, account string) {
	why := "the provider named no account"
	switch {
	case account != "" && u.Email == "":
		why = "the person has no email on file"
	case account != "":
		why = "the provider did not vouch for the account, or it could not be compared"
	}
	_ = s.audit.Write(r.Context(), audit.Event{
		EventType: connectUnverifiedAccount, AgentID: "user:" + u.ID, UpstreamName: upstream,
		Decision:      "unverified_account",
		ResultSummary: upstream + " connected from a link on the person's own toolyard session; the account was not checked: " + why,
		Raiser:        actor.Raiser{OwnerUserID: u.ID, OwnerEmail: u.Email, OwnerName: u.Label()},
	})
}

// connectDonePage is the success page for a flow a link started: the
// person came from their agent, so that is where they go back to, or from
// the dashboard's "Copy link for another browser", which picks the change
// up on its own (the flow does not record which).
func connectDonePage(w http.ResponseWriter, upstream, account string) {
	as := ""
	if account != "" {
		as = " as <strong>" + htmlEscape(account) + "</strong>"
	}
	writeConnectPage(w, http.StatusOK, "Connected",
		"<h1>Connected</h1><p><strong>"+htmlEscape(upstream)+"</strong> is connected"+as+".</p>"+
			"<p>Go back to T3 Code and tell your agent to retry. You can close this tab.</p>"+
			"<p>Copied this link from the toolyard dashboard? Nothing more to do: the dashboard shows the connection.</p>")
}

// writeConnectPage renders one of the connect pages. body is HTML the
// caller built with htmlEscape on every dynamic part. The dashboard's
// stylesheet is linked rather than styling inline, which the
// Content-Security-Policy forbids.
func writeConnectPage(w http.ResponseWriter, status int, title, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">` +
		`<title>` + htmlEscape(title) + ` — toolyard</title><link rel="stylesheet" href="/style.css"></head><body><main>` + body + `</main></body></html>`))
}
