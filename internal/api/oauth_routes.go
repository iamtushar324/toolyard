package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/oauth"
)

// oauthRoutes wires the public OAuth callback + paste routes. The
// serversItem handler dispatches the per-upstream OAuth subpaths
// (discover, begin, device-poll, reauth, pat, disconnect, status).
func (s *Server) oauthRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/v1/mcp-oauth/callback", s.oauthCallback)
	mux.HandleFunc("/v1/mcp-oauth/paste", s.oauthPaste)
}

// oauthDiscover runs discovery + DCR against the upstream's URL and
// persists the resulting client. If the AS does not expose DCR, the
// caller must follow up with /oauth/manual-client. Body (optional):
// {"redirect_uri":"…","scopes":["…"]}.
func (s *Server) oauthDiscover(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	if _, err := s.requireUser(r); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if s.oauth == nil {
		writeError(w, http.StatusServiceUnavailable, "oauth service not wired")
		return
	}
	srv, err := s.upstreams.Get(r.Context(), name)
	if err != nil || srv == nil || srv.URL == "" {
		writeError(w, http.StatusBadRequest, "upstream must have a URL to discover OAuth")
		return
	}
	var body struct {
		RedirectURI string   `json:"redirect_uri"`
		Scopes      []string `json:"scopes"`
	}
	if r.ContentLength > 0 {
		_ = decode(r, &body)
	}
	if body.RedirectURI == "" {
		body.RedirectURI = s.oauthRedirectURI(r)
	}
	md, err := s.oauth.Discover(r.Context(), srv.URL)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	authMethod := pickAuthMethod(md)
	rec := oauth.ClientRecord{
		UpstreamName:                name,
		Issuer:                      md.Issuer,
		AuthorizationEndpoint:       md.AuthorizationEndpoint,
		TokenEndpoint:               md.TokenEndpoint,
		RegistrationEndpoint:        md.RegistrationEndpoint,
		RevocationEndpoint:          md.RevocationEndpoint,
		DeviceAuthorizationEndpoint: md.DeviceAuthorizationEndpoint,
		RedirectURI:                 body.RedirectURI,
		Scopes:                      pickScopes(md, body.Scopes),
		TokenEndpointAuthMethod:     authMethod,
		Metadata:                    md,
	}
	dcrTried := md.RegistrationEndpoint != ""
	dcrErr := ""
	if dcrTried {
		cid, secret, method, err := s.oauth.Register(r.Context(), md, rec.RedirectURI, rec.Scopes)
		if err != nil {
			dcrErr = err.Error()
		} else {
			rec.ClientID = cid
			rec.ClientSecret = secret
			rec.TokenEndpointAuthMethod = method
		}
	}
	// If we got a client out of DCR, persist now so the operator can hit
	// /oauth/begin immediately. Otherwise return the metadata so the UI
	// can show the manual-client form.
	persisted := false
	if rec.ClientID != "" {
		if err := s.oauth.PutClient(r.Context(), rec); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		persisted = true
		_ = s.audit.Write(r.Context(), audit.Event{
			EventType:     "oauth.discover",
			ResultSummary: name,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"upstream":                      name,
		"issuer":                        md.Issuer,
		"authorization_endpoint":        md.AuthorizationEndpoint,
		"token_endpoint":                md.TokenEndpoint,
		"registration_endpoint":         md.RegistrationEndpoint,
		"revocation_endpoint":           md.RevocationEndpoint,
		"device_authorization_endpoint": md.DeviceAuthorizationEndpoint,
		"scopes_supported":              md.ScopesSupported,
		"redirect_uri":                  rec.RedirectURI,
		"scopes":                        rec.Scopes,
		"client_registered":             persisted,
		"dcr_attempted":                 dcrTried,
		"dcr_error":                     dcrErr,
	})
}

// oauthManualClient lets the operator paste a pre-registered client_id
// (and optional secret) when DCR isn't available. Discovery must have
// been run first so we know the AS endpoints.
func (s *Server) oauthManualClient(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	if _, err := s.requireUser(r); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var body struct {
		ClientID                    string            `json:"client_id"`
		ClientSecret                string            `json:"client_secret"`
		RedirectURI                 string            `json:"redirect_uri"`
		Scopes                      []string          `json:"scopes"`
		AuthorizationEndpoint       string            `json:"authorization_endpoint"`
		TokenEndpoint               string            `json:"token_endpoint"`
		RevocationEndpoint          string            `json:"revocation_endpoint"`
		DeviceAuthorizationEndpoint string            `json:"device_authorization_endpoint"`
		Issuer                      string            `json:"issuer"`
		TokenEndpointAuthMethod     string            `json:"token_endpoint_auth_method"`
		ExtraAuthorizeParams        map[string]string `json:"extra_authorize_params"`
	}
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(body.ClientID) == "" {
		writeError(w, http.StatusBadRequest, "client_id is required")
		return
	}
	existing, _ := s.oauth.GetClient(r.Context(), name)
	if existing != nil {
		// Merge: preserve discovery info if caller didn't include it.
		if body.AuthorizationEndpoint == "" {
			body.AuthorizationEndpoint = existing.AuthorizationEndpoint
		}
		if body.TokenEndpoint == "" {
			body.TokenEndpoint = existing.TokenEndpoint
		}
		if body.RevocationEndpoint == "" {
			body.RevocationEndpoint = existing.RevocationEndpoint
		}
		if body.DeviceAuthorizationEndpoint == "" {
			body.DeviceAuthorizationEndpoint = existing.DeviceAuthorizationEndpoint
		}
		if body.Issuer == "" {
			body.Issuer = existing.Issuer
		}
	}
	if body.AuthorizationEndpoint == "" || body.TokenEndpoint == "" {
		writeError(w, http.StatusBadRequest, "authorization_endpoint and token_endpoint are required (run discover first or supply them)")
		return
	}
	if body.RedirectURI == "" {
		body.RedirectURI = s.oauthRedirectURI(r)
	}
	if len(body.Scopes) == 0 && existing != nil {
		body.Scopes = existing.Scopes
	}
	authMethod := "none"
	if body.ClientSecret != "" {
		authMethod = "client_secret_post"
	}
	if body.TokenEndpointAuthMethod != "" {
		switch body.TokenEndpointAuthMethod {
		case "none", "client_secret_post", "client_secret_basic":
			authMethod = body.TokenEndpointAuthMethod
		default:
			writeError(w, http.StatusBadRequest, "token_endpoint_auth_method must be none, client_secret_post or client_secret_basic")
			return
		}
	}
	rec := oauth.ClientRecord{
		UpstreamName:                name,
		Issuer:                      body.Issuer,
		AuthorizationEndpoint:       body.AuthorizationEndpoint,
		TokenEndpoint:               body.TokenEndpoint,
		RevocationEndpoint:          body.RevocationEndpoint,
		DeviceAuthorizationEndpoint: body.DeviceAuthorizationEndpoint,
		ClientID:                    body.ClientID,
		ClientSecret:                body.ClientSecret,
		RedirectURI:                 body.RedirectURI,
		Scopes:                      body.Scopes,
		TokenEndpointAuthMethod:     authMethod,
		ExtraAuthorizeParams:        body.ExtraAuthorizeParams,
	}
	if err := s.oauth.PutClient(r.Context(), rec); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.audit.Write(r.Context(), audit.Event{
		EventType:     "oauth.manual_client",
		ResultSummary: name,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// oauthBegin starts a Mode A or Mode B flow and returns the authorize URL.
func (s *Server) oauthBegin(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	uid, err := s.requireUser(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var body struct {
		Mode   string   `json:"mode"`
		Scopes []string `json:"scopes"`
	}
	if r.ContentLength > 0 {
		_ = decode(r, &body)
	}
	if body.Mode == "" {
		body.Mode = oauth.ModeCallback
	}
	if body.Mode == oauth.ModeDevice {
		// Use device-begin instead.
		writeError(w, http.StatusBadRequest, "use /device-begin for the device flow")
		return
	}
	authURL, state, err := s.oauth.BeginCallback(r.Context(), name, uid, body.Mode, body.Scopes)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	_ = s.audit.Write(r.Context(), audit.Event{
		EventType:     "oauth.begin",
		ResultSummary: fmt.Sprintf("%s mode=%s", name, body.Mode),
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"authorize_url": authURL,
		"state":         state,
		"mode":          body.Mode,
		"expires_in":    int(oauth.PendingTTL.Seconds()),
	})
}

// oauthDeviceBegin kicks off RFC 8628.
func (s *Server) oauthDeviceBegin(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	uid, err := s.requireUser(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var body struct {
		Scopes []string `json:"scopes"`
	}
	if r.ContentLength > 0 {
		_ = decode(r, &body)
	}
	p, err := s.oauth.BeginDevice(r.Context(), name, uid, body.Scopes)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	_ = s.audit.Write(r.Context(), audit.Event{
		EventType:     "oauth.device_begin",
		ResultSummary: name,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"state":            p.State,
		"user_code":        p.UserCode,
		"verification_uri": p.VerificationURI,
		"interval":         p.IntervalS,
		"expires_at":       p.ExpiresAt.UnixMilli(),
	})
}

// oauthDevicePoll runs one poll tick on a device flow.
func (s *Server) oauthDevicePoll(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	if _, err := s.requireUser(r); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var body struct {
		State string `json:"state"`
	}
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	rec, err := s.oauth.PollDevice(r.Context(), body.State)
	if err != nil {
		if errors.Is(err, oauth.ErrPendingNotFound) {
			writeError(w, http.StatusGone, err.Error())
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if rec == nil {
		writeJSON(w, http.StatusAccepted, map[string]any{"status": "pending"})
		return
	}
	go s.upstreams.ReconnectAfterAuth(context.Background(), name)
	writeJSON(w, http.StatusOK, map[string]any{"status": "authorized"})
}

// oauthCallback is the GET endpoint the IdP redirects the user's
// browser back to in Mode A. We exchange code for tokens, render a tiny
// "you can close this tab" page, and SSE-publish the success event.
func (s *Server) oauthCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	q := r.URL.Query()
	state := q.Get("state")
	code := q.Get("code")
	idpErr := q.Get("error")
	desc := q.Get("error_description")
	if state == "" || (code == "" && idpErr == "") {
		oauthHTMLError(w, "Missing code/state in callback URL.")
		return
	}
	if idpErr != "" {
		oauthHTMLError(w, fmt.Sprintf("Authorization server returned %s: %s", idpErr, desc))
		return
	}
	p, err := s.oauth.LoadPending(r.Context(), state)
	if err != nil {
		oauthHTMLError(w, "This authorization request expired or was already used. Please try again from the dashboard.")
		return
	}
	if p.Mode != oauth.ModeCallback {
		oauthHTMLError(w, "Wrong mode for this state. Use 'paste' instead.")
		return
	}
	rec, err := s.oauth.ExchangeCode(r.Context(), p.UpstreamName, code, p.CodeVerifier)
	if err != nil {
		oauthHTMLError(w, "Token exchange failed: "+err.Error())
		return
	}
	_ = s.oauth.DeletePending(r.Context(), state)
	go s.upstreams.ReconnectAfterAuth(context.Background(), p.UpstreamName)
	_ = s.audit.Write(r.Context(), audit.Event{
		EventType:     "oauth.success",
		ResultSummary: p.UpstreamName,
	})
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(oauthSuccessHTML(p.UpstreamName, rec.AccessExpiresAt)))
}

// oauthPaste accepts the post-redirect URL pasted by the user when the
// IdP redirected them to a page their browser couldn't reach. Body:
// {"url":"https://blackhole.invalid/cb?code=…&state=…"}.
func (s *Server) oauthPaste(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	if _, err := s.requireUser(r); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var body struct {
		URL string `json:"url"`
	}
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	parsed, err := url.Parse(strings.TrimSpace(body.URL))
	if err != nil {
		writeError(w, http.StatusBadRequest, "could not parse URL: "+err.Error())
		return
	}
	q := parsed.Query()
	code := q.Get("code")
	state := q.Get("state")
	idpErr := q.Get("error")
	if state == "" || (code == "" && idpErr == "") {
		writeError(w, http.StatusBadRequest, "URL must contain 'code' and 'state' parameters")
		return
	}
	if idpErr != "" {
		writeError(w, http.StatusBadRequest, "authorization server returned: "+idpErr+" "+q.Get("error_description"))
		return
	}
	p, err := s.oauth.LoadPending(r.Context(), state)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	rec, err := s.oauth.ExchangeCode(r.Context(), p.UpstreamName, code, p.CodeVerifier)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	_ = s.oauth.DeletePending(r.Context(), state)
	go s.upstreams.ReconnectAfterAuth(context.Background(), p.UpstreamName)
	_ = s.audit.Write(r.Context(), audit.Event{
		EventType:     "oauth.success",
		ResultSummary: p.UpstreamName + " (paste)",
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"upstream":          p.UpstreamName,
		"access_expires_at": nullableMS(rec.AccessExpiresAt),
	})
}

// oauthReauth wipes the current token and forces a fresh flow.
func (s *Server) oauthReauth(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	if _, err := s.requireUser(r); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if _, err := s.upstreams.Get(r.Context(), name); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	// Mark the existing token as needing reauth so the live connection
	// is dropped — operator will re-run /oauth/begin afterwards.
	s.oauth.MarkReauthExternal(r.Context(), name, "operator-initiated reauth")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// oauthDisconnect revokes the token (best-effort) and removes the
// client + token rows.
func (s *Server) oauthDisconnect(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "DELETE only")
		return
	}
	if _, err := s.requireUser(r); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if err := s.oauth.Disconnect(r.Context(), name); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.audit.Write(r.Context(), audit.Event{
		EventType:     "oauth.disconnect",
		ResultSummary: name,
	})
	go s.upstreams.ReconnectAfterAuth(context.Background(), name)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// oauthPAT stores a personal access token / API key in lieu of running
// the OAuth dance. Body: {"token":"…"}.
func (s *Server) oauthPAT(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	if _, err := s.requireUser(r); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var body struct {
		Token string `json:"token"`
	}
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(body.Token) == "" {
		writeError(w, http.StatusBadRequest, "token is required")
		return
	}
	// PAT mode wants a placeholder client record so the upstream is
	// recognized as "auth-managed". Fill in zeros if we don't already
	// have a discovered client.
	if existing, _ := s.oauth.GetClient(r.Context(), name); existing == nil {
		_ = s.oauth.PutClient(r.Context(), oauth.ClientRecord{
			UpstreamName:            name,
			Issuer:                  "(personal-access-token)",
			AuthorizationEndpoint:   "(pat)",
			TokenEndpoint:           "(pat)",
			ClientID:                "(pat)",
			RedirectURI:             "(pat)",
			TokenEndpointAuthMethod: "none",
		})
	}
	if err := s.oauth.PutPAT(r.Context(), name, body.Token); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.audit.Write(r.Context(), audit.Event{
		EventType:     "oauth.pat",
		ResultSummary: name,
	})
	go s.upstreams.ReconnectAfterAuth(context.Background(), name)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// oauthStatus returns "what's the auth situation for this upstream."
func (s *Server) oauthStatus(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	if _, err := s.requireUser(r); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	cli, _ := s.oauth.GetClient(r.Context(), name)
	tok, _ := s.oauth.GetToken(r.Context(), name)
	out := map[string]any{
		"upstream":   name,
		"has_client": cli != nil,
		"has_token":  tok != nil,
	}
	if cli != nil {
		out["issuer"] = cli.Issuer
		out["scopes"] = cli.Scopes
		out["redirect_uri"] = cli.RedirectURI
		out["device_supported"] = cli.DeviceAuthorizationEndpoint != ""
		out["client_id"] = cli.ClientID
		if len(cli.ExtraAuthorizeParams) > 0 {
			out["extra_authorize_params"] = cli.ExtraAuthorizeParams
		}
	}
	if tok != nil {
		out["state"] = tok.State
		out["scope_granted"] = tok.Scope
		out["is_pat"] = tok.IsPAT
		out["last_error"] = tok.LastError
		if !tok.AccessExpiresAt.IsZero() {
			out["access_expires_at"] = tok.AccessExpiresAt.UnixMilli()
		}
		if !tok.LastRefreshAt.IsZero() {
			out["last_refresh_at"] = tok.LastRefreshAt.UnixMilli()
		}
		out["refresh_failures"] = tok.RefreshFailures
	}
	writeJSON(w, http.StatusOK, out)
}

// dispatchOAuth is called from serversItem when the subpath under
// /v1/servers/{name}/ starts with "oauth". It routes the trailing
// segment to the appropriate handler.
func (s *Server) dispatchOAuth(w http.ResponseWriter, r *http.Request, name, subpath string) bool {
	if s.oauth == nil {
		writeError(w, http.StatusServiceUnavailable, "oauth service not wired")
		return true
	}
	switch subpath {
	case "oauth":
		// status (GET) or disconnect (DELETE)
		switch r.Method {
		case http.MethodGet:
			s.oauthStatus(w, r, name)
		case http.MethodDelete:
			s.oauthDisconnect(w, r, name)
		default:
			writeError(w, http.StatusMethodNotAllowed, "GET or DELETE")
		}
		return true
	case "oauth/discover":
		s.oauthDiscover(w, r, name)
		return true
	case "oauth/manual-client":
		s.oauthManualClient(w, r, name)
		return true
	case "oauth/begin":
		s.oauthBegin(w, r, name)
		return true
	case "oauth/device-begin":
		s.oauthDeviceBegin(w, r, name)
		return true
	case "oauth/device-poll":
		s.oauthDevicePoll(w, r, name)
		return true
	case "oauth/reauth":
		s.oauthReauth(w, r, name)
		return true
	case "oauth/pat":
		s.oauthPAT(w, r, name)
		return true
	}
	return false
}

// oauthRedirectURI computes the callback URL based on PublicURL when set,
// or the request's host when running locally. When PublicURL is set the
// IdP must accept it during DCR — if the dashboard is on Tailnet only,
// PublicURL should be the Tailnet hostname so the IdP redirect still
// works for browsers on the same network. Operators on a different
// network use Mode B (paste).
func (s *Server) oauthRedirectURI(r *http.Request) string {
	base := strings.TrimRight(s.security.PublicURL, "/")
	if base == "" {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		base = scheme + "://" + r.Host
	}
	return base + "/v1/mcp-oauth/callback"
}

func pickAuthMethod(md *oauth.ASMetadata) string {
	if md == nil {
		return "none"
	}
	for _, m := range md.TokenEndpointAuthMethodsSupp {
		if m == "none" {
			return "none"
		}
	}
	for _, m := range md.TokenEndpointAuthMethodsSupp {
		if m == "client_secret_basic" {
			return "client_secret_basic"
		}
	}
	for _, m := range md.TokenEndpointAuthMethodsSupp {
		if m == "client_secret_post" {
			return "client_secret_post"
		}
	}
	return "none"
}

func pickScopes(md *oauth.ASMetadata, requested []string) []string {
	if len(requested) > 0 {
		return requested
	}
	// If the IdP advertises offline_access, prefer it so we get a
	// refresh_token. We pass everything advertised — IdPs ignore unknown
	// scopes by spec.
	out := append([]string{}, md.ScopesSupported...)
	hasOffline := false
	for _, s := range out {
		if s == "offline_access" {
			hasOffline = true
		}
	}
	if !hasOffline {
		out = append(out, "offline_access")
	}
	return out
}

func nullableMS(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UnixMilli()
}

func oauthSuccessHTML(name string, expires time.Time) string {
	exp := ""
	if !expires.IsZero() {
		exp = "<p>Access token expires " + expires.UTC().Format(time.RFC1123) + ".</p>"
	}
	return `<!doctype html><html><head><meta charset="utf-8"><title>Authorized — toolyard</title>
<style>body{font:14px system-ui,sans-serif;color:#222;background:#f6f8fa;display:flex;align-items:center;justify-content:center;height:100vh;margin:0}div{background:#fff;border-radius:8px;padding:32px;box-shadow:0 4px 12px rgba(0,0,0,0.08);max-width:400px}h1{margin:0 0 8px;font-size:18px;color:#0a7}</style>
</head><body><div>
<h1>✓ Authorized</h1>
<p><strong>` + htmlEscape(name) + `</strong> is now connected to toolyard. You can close this tab.</p>` + exp + `
<p style="color:#888;font-size:12px;margin-top:24px">If the dashboard tab does not auto-update, reload it.</p>
</div></body></html>`
}

func oauthHTMLError(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusBadRequest)
	_, _ = w.Write([]byte(`<!doctype html><html><head><meta charset="utf-8"><title>OAuth error — toolyard</title>
<style>body{font:14px system-ui,sans-serif;color:#222;background:#fff5f5;display:flex;align-items:center;justify-content:center;height:100vh;margin:0}div{background:#fff;border:1px solid #f99;border-radius:8px;padding:32px;max-width:480px}h1{margin:0 0 8px;font-size:18px;color:#c33}code{background:#fee;padding:2px 4px;border-radius:3px}</style>
</head><body><div>
<h1>Authorization failed</h1>
<p>` + htmlEscape(msg) + `</p>
<p style="color:#888;font-size:12px;margin-top:24px">Return to the dashboard and try again. If your browser cannot reach the dashboard's host, choose <em>"Different browser → paste URL back"</em> next time.</p>
</div></body></html>`))
}

func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", `'`, "&#39;")
	return r.Replace(s)
}
