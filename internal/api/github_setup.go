package api

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/oauth"
)

type githubSetupState struct {
	Server  string `json:"server"`
	User    string `json:"user"`
	Created int64  `json:"created"`
}

// githubSetupBegin returns GitHub's manifest form. The person, rather than
// an agent, creates the App on GitHub. Only the OAuth client secret is stored;
// no installation private key or shared GitHub token is retained.
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
	}
	if err := decode(r, &body); err != nil {
		writeError(w, 400, err.Error())
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
	nonce := make([]byte, 24)
	if _, err := rand.Read(nonce); err != nil {
		writeError(w, 500, "cannot create setup state")
		return
	}
	data, _ := json.Marshal(githubSetupState{Server: body.Server, User: u.ID, Created: time.Now().UnixMilli()})
	state := base64.RawURLEncoding.EncodeToString(nonce) + "." + base64.RawURLEncoding.EncodeToString(data)
	s.setOAuthFlowCookie(w, r, state, u.ID)
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
	writeJSON(w, 200, map[string]any{"action_url": "https://github.com/settings/apps/new?state=" + url.QueryEscape(state), "manifest": manifest})
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
