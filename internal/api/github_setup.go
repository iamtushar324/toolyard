package api

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/actor"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/oauth"
	"github.com/tusharbhardwaj/toolyard/internal/upstreams"
)

type githubSetupState struct {
	Server  string `json:"server"`
	User    string `json:"user"`
	Created int64  `json:"created"`
}

// githubAppsNewURL is GitHub's "register an App from a manifest" form
// target: the browser POSTs the manifest there (GitHub takes no GET).
const githubAppsNewURL = "https://github.com/settings/apps/new"

// githubSetupBegin returns GitHub's manifest form. The person, rather than
// an agent, creates the App on GitHub. Only the OAuth client secret is stored;
// no installation private key or shared GitHub token is retained.
//
// With {"mode":"link"} ("Copy link for another browser") nothing is bound
// here: the admin gets a one-time GitHub App setup link instead, a connect
// link of its own kind (connect_link_routes.go). Opened in another browser
// profile, its Continue (an admin's toolyard session in that browser)
// binds the setup to that browser and shows the manifest form there, so
// the App is created under the GitHub account signed in to that profile.
func (s *Server) githubSetupBegin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "POST only")
		return
	}
	u, err := s.requireAdmin(r)
	if err != nil {
		writeError(w, 403, "admin_only")
		return
	}
	var body struct {
		Server string `json:"server"`
		Mode   string `json:"mode"`
	}
	if err := decode(r, &body); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if body.Mode != "" && body.Mode != connectModeLink {
		writeError(w, 400, `mode must be empty or "link"`)
		return
	}
	sv, err := s.upstreams.Get(r.Context(), body.Server)
	if err != nil || sv.Transport != "github" {
		writeError(w, 400, "select a per-user GitHub server")
		return
	}
	if ready, _ := s.oauth.HasClient(r.Context(), body.Server); ready {
		writeError(w, 409, "the GitHub App is already configured")
		return
	}
	if body.Mode == connectModeLink {
		if operatorFromContext(r.Context()) != nil {
			writeError(w, http.StatusConflict, "connect links are not handed out to an operator token; copy one in the dashboard")
			return
		}
		s.writeDashboardLink(w, r, u, body.Server, "GitHub App setup", func() (string, error) {
			return s.oauth.IssueGitHubAppSetupTicket(r.Context(), u.ID, body.Server)
		})
		return
	}
	state, manifest, err := s.githubSetupStart(r, body.Server, u.ID)
	if err != nil {
		writeError(w, 500, "cannot create setup state")
		return
	}
	s.setOAuthFlowCookie(w, r, state, u.ID)
	writeJSON(w, 200, map[string]any{"action_url": githubAppsNewURL + "?state=" + url.QueryEscape(state), "manifest": manifest})
}

// githubSetupStart makes the setup state for (server, userID) and the App
// manifest GitHub's form takes. The caller binds the state to the browser
// with the flow cookie; the callback requires it.
func (s *Server) githubSetupStart(r *http.Request, server, userID string) (string, map[string]any, error) {
	nonce := make([]byte, 24)
	if _, err := rand.Read(nonce); err != nil {
		return "", nil, err
	}
	data, _ := json.Marshal(githubSetupState{Server: server, User: userID, Created: time.Now().UnixMilli()})
	state := base64.RawURLEncoding.EncodeToString(nonce) + "." + base64.RawURLEncoding.EncodeToString(data)
	base := strings.TrimSuffix(s.oauthRedirectURI(r), "/v1/mcp-oauth/callback")
	manifest := map[string]any{
		"name": "Toolyard GitHub", "url": base,
		"description":   "Per-user pull request access. Toolyard asks the account owner before every comment or review.",
		"redirect_url":  base + "/v1/mcp-oauth/github-app",
		"callback_urls": []string{s.oauthRedirectURI(r)},
		"setup_url":     base + "/#connections", "public": true,
		"hook_attributes":     map[string]any{"url": base + "/v1/github/webhook", "active": false},
		"default_permissions": map[string]string{"metadata": "read", "contents": "read", "pull_requests": "write"},
		"default_events":      []string{}, "request_oauth_on_install": false,
	}
	return state, manifest, nil
}

// githubSetupLink is what the GitHub App setup link's page and Continue
// need: the ticket, its row, the admin it was made for, the server.
type githubSetupLink struct {
	ticket string
	tk     *oauth.ConnectTicket
	user   *identity.User
}

// lookupGitHubSetupLink reads a GitHub App setup ticket without redeeming
// it and checks what does not depend on who is asking: the admin it was
// made for is still an active admin, and the server is still a GitHub
// server whose App is not registered yet.
func (s *Server) lookupGitHubSetupLink(r *http.Request) (*githubSetupLink, *connectLinkRefusal) {
	ctx := r.Context()
	ticket := connectTicketFromPath(r.URL.Path)
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
	l := &githubSetupLink{ticket: ticket, tk: tk}
	if tk.Kind != oauth.ConnectKindGitHubAppSetup {
		return l, &connectLinkRefusal{reason: "bad_purpose", text: connectLinkDeadText}
	}
	u, err := s.identity.GetUserByID(ctx, tk.UserID)
	if err != nil || u.Status != identity.StatusActive {
		return l, &connectLinkRefusal{reason: "user_inactive", text: "The toolyard account this link was made for is not active, so it can't be used. Ask a toolyard admin."}
	}
	l.user = u
	if u.Role != identity.RoleAdmin {
		return l, &connectLinkRefusal{reason: "not_admin", text: "Only an admin can register the GitHub App, and this link was not made by one. Ask a toolyard admin."}
	}
	sv, err := s.upstreams.Get(ctx, tk.Upstream)
	if err != nil {
		if errors.Is(err, upstreams.ErrNotFound) {
			return l, &connectLinkRefusal{reason: "server_missing", text: "That GitHub server no longer exists in toolyard. Copy a new link in the dashboard if it is added again."}
		}
		return l, &connectLinkRefusal{reason: "error", text: "Something went wrong looking the server up. Copy a new link in the toolyard dashboard."}
	}
	if sv.Transport != "github" {
		return l, &connectLinkRefusal{reason: "bad_purpose", text: connectLinkDeadText}
	}
	ready, err := s.oauth.HasClient(ctx, tk.Upstream)
	if err != nil {
		return l, &connectLinkRefusal{reason: "error", text: "Something went wrong looking the server up. Copy a new link in the toolyard dashboard."}
	}
	if ready {
		return l, &connectLinkRefusal{reason: "already_connected", text: "The GitHub App for " + tk.Upstream + " is already registered, so this link does nothing. Each person now connects their own account on My connections."}
	}
	return l, nil
}

// githubSetupLinkConfirm (GET of a GitHub App setup link) says what
// Continue does. Like every connect link page it redeems and audits
// nothing; it sets the SameSite=Strict nonce cookie the form carries back.
func (s *Server) githubSetupLinkConfirm(w http.ResponseWriter, r *http.Request) {
	l, refusal := s.lookupGitHubSetupLink(r)
	if refusal != nil {
		writeConnectPage(w, connectRefusalStatus(refusal.reason), "This link can't be used",
			"<h1>This link can't be used</h1><p>"+htmlEscape(refusal.text)+"</p>")
		return
	}
	server := htmlEscape(l.tk.Upstream)
	sess, hasSess := s.sessionUser(r)
	if hasSess && (sess.Role != identity.RoleAdmin || sess.Status != identity.StatusActive) {
		writeConnectPage(w, http.StatusForbidden, "Admin sign-in needed",
			"<h1>Sign in to toolyard as an admin to continue</h1><p>This link registers the GitHub App of <strong>"+server+
				"</strong>, and only an admin may do that. This browser is signed in to toolyard as a member.</p>"+s.signInHTML(true))
		return
	}
	nonce, err := randomNonce()
	if err != nil {
		writeConnectPage(w, http.StatusInternalServerError, "Not available", "<h1>Not available</h1><p>Please try again.</p>")
		return
	}
	name, path := s.hostCookie(r, connectNonceName(l.ticket), connectLinkPath)
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: nonce, Path: path, HttpOnly: true,
		Secure: s.security.IsBehindHTTPS(r), SameSite: http.SameSiteStrictMode,
		MaxAge: int(oauth.ConnectTicketTTL.Seconds()),
	})
	var b strings.Builder
	b.WriteString("<h1>Register the GitHub App for " + server + "</h1>")
	b.WriteString("<p>This creates toolyard's GitHub App under the GitHub account signed in to <strong>this browser</strong>. Each person then connects their own GitHub account on My connections.</p>")
	if !hasSess {
		b.WriteString("<p>Continue works only in a browser signed in to toolyard as an admin. Not signed in here? " +
			`<a href="` + htmlEscape(s.signInHref()) + `">Sign in to toolyard</a> first, then open this link again.</p>`)
	}
	b.WriteString("<p>The next page has a <strong>Continue to GitHub</strong> button. GitHub then asks you to name and create the App, and sends you back to toolyard in this browser.</p>")
	b.WriteString(`<form method="post" action="` + htmlEscape(connectLinkPath+l.ticket) + `">` +
		`<input type="hidden" name="nonce" value="` + htmlEscape(nonce) + `">` +
		`<button type="submit">Continue</button></form>`)
	writeConnectPage(w, http.StatusOK, "Register the GitHub App", b.String())
}

// githubSetupLinkContinue (POST of a GitHub App setup link, nonce and
// Sec-Fetch-Site already checked by connectLinkContinue) needs an admin's
// toolyard session in this browser: the setup is then theirs. It redeems
// the ticket, binds a fresh setup state to this browser with the flow
// cookie (the callback requires it, as for a dashboard-started setup) and
// shows the manifest form with a Continue to GitHub button. Nothing is
// submitted on its own: the person always presses the button. The page's
// form-action allows GitHub's form target and nothing else.
func (s *Server) githubSetupLinkContinue(w http.ResponseWriter, r *http.Request, ticket string) {
	ctx := r.Context()
	l, refusal := s.lookupGitHubSetupLink(r)
	if refusal != nil {
		upstream := ""
		var u *identity.User
		if l != nil && l.tk != nil {
			upstream, u = l.tk.Upstream, l.user
		}
		s.connectLinkRefuse(w, r, upstream, u, refusal)
		return
	}
	sess, hasSess := s.sessionUser(r)
	if !hasSess || sess.Role != identity.RoleAdmin || sess.Status != identity.StatusActive {
		s.connectLinkRefuse(w, r, l.tk.Upstream, l.user, &connectLinkRefusal{reason: "admin_session",
			text: "Sign in to toolyard as an admin to continue. This link registers the GitHub App of " + l.tk.Upstream + ".", signIn: true})
		return
	}
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
	state, manifest, err := s.githubSetupStart(r, l.tk.Upstream, sess.ID)
	if err != nil {
		s.connectLinkRefuse(w, r, l.tk.Upstream, sess, &connectLinkRefusal{reason: "begin_failed",
			text: "The GitHub App setup could not be started. Copy a new link in the toolyard dashboard."})
		return
	}
	manifestJSON, _ := json.Marshal(manifest)
	s.setOAuthFlowCookie(w, r, state, sess.ID)
	summary := "GitHub App setup for " + l.tk.Upstream + " continued from a connect link"
	if sess.ID != l.tk.UserID {
		summary += " minted for another admin"
	}
	_ = s.audit.Write(ctx, audit.Event{
		EventType: connectLinkUsed, AgentID: l.tk.AgentID, UpstreamName: l.tk.Upstream,
		ResultSummary: summary,
		Raiser:        actor.Raiser{OwnerUserID: sess.ID, OwnerEmail: sess.Email, OwnerName: sess.Label()},
	})
	w.Header().Set("Content-Security-Policy", connectCSP(githubAppsNewURL))
	writeConnectPage(w, http.StatusOK, "Continue to GitHub",
		"<h1>Continue to GitHub</h1><p>GitHub opens its page for a new App with toolyard's settings filled in for <strong>"+htmlEscape(l.tk.Upstream)+
			"</strong>. Create the App there; GitHub then sends you back to toolyard in this browser.</p>"+
			`<form method="post" action="`+htmlEscape(githubAppsNewURL+"?state="+url.QueryEscape(state))+`">`+
			`<input type="hidden" name="manifest" value="`+htmlEscape(string(manifestJSON))+`">`+
			`<button type="submit">Continue to GitHub</button></form>`)
}

var manifestCode = regexp.MustCompile(`^[A-Za-z0-9_-]{1,200}$`)

func (s *Server) githubSetupCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, 405, "GET only")
		return
	}
	state, code := r.URL.Query().Get("state"), r.URL.Query().Get("code")
	_, encoded, ok := strings.Cut(state, ".")
	data, err := base64.RawURLEncoding.DecodeString(encoded)
	var setup githubSetupState
	if !ok || err != nil || json.Unmarshal(data, &setup) != nil || !manifestCode.MatchString(code) || setup.Created > time.Now().UnixMilli() || time.Now().UnixMilli()-setup.Created > oauth.PendingTTL.Milliseconds() || !s.oauthFlowCookieValid(r, state, setup.User) {
		oauthHTMLError(w, "GitHub setup expired or came from a different browser. Start again in My connections.")
		return
	}
	s.clearOAuthFlowCookie(w, r, state)
	u, err := s.identity.GetUserByID(r.Context(), setup.User)
	if err != nil || u.Status != identity.StatusActive || u.Role != identity.RoleAdmin {
		oauthHTMLError(w, "The App creator must still be an active Toolyard admin.")
		return
	}
	if current, ok := s.sessionUser(r); ok && current.ID != u.ID {
		oauthHTMLError(w, "This setup belongs to a different user.")
		return
	}
	sv, err := s.upstreams.Get(r.Context(), setup.Server)
	ready, _ := s.oauth.HasClient(r.Context(), setup.Server)
	if err != nil || sv.Transport != "github" || ready {
		oauthHTMLError(w, "This GitHub server is unavailable or already configured.")
		return
	}
	client := s.githubHTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	clone := *client
	clone.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, "https://api.github.com/app-manifests/"+code+"/conversions", nil)
	if err != nil {
		oauthHTMLError(w, "Cannot complete GitHub setup.")
		return
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	res, err := clone.Do(req)
	if err != nil {
		oauthHTMLError(w, "GitHub did not complete App setup.")
		return
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		oauthHTMLError(w, "GitHub refused the App setup code. Start again.")
		return
	}
	var app struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
		Slug         string `json:"slug"`
	}
	if json.NewDecoder(io.LimitReader(res.Body, 1024*1024)).Decode(&app) != nil || app.ClientID == "" || app.ClientSecret == "" || !manifestCode.MatchString(app.Slug) {
		oauthHTMLError(w, "GitHub returned incomplete App credentials.")
		return
	}
	err = s.oauth.PutClient(r.Context(), oauth.ClientRecord{
		UpstreamName: setup.Server, Issuer: "https://github.com",
		AuthorizationEndpoint: "https://github.com/login/oauth/authorize", TokenEndpoint: "https://github.com/login/oauth/access_token",
		ClientID: app.ClientID, ClientSecret: app.ClientSecret, RedirectURI: s.oauthRedirectURI(r),
		TokenEndpointAuthMethod: "client_secret_post", ExtraAuthorizeParams: map[string]string{},
	})
	if err != nil {
		oauthHTMLError(w, "Toolyard could not save the App credentials.")
		return
	}
	_ = s.audit.Write(r.Context(), audit.Event{EventType: "oauth.github_app_setup", ResultSummary: setup.Server})
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, `<!doctype html><html><meta name="viewport" content="width=device-width"><title>GitHub App ready</title><body><h1>GitHub App ready</h1><p>Install the App on the repositories you select.</p><p><a href="https://github.com/apps/`+html.EscapeString(app.Slug)+`/installations/new">Install GitHub App</a></p><p>Then select Connect in Toolyard. Each user connects their own account.</p><p><a href="/#connections">Open My connections</a></p></body></html>`)
}
