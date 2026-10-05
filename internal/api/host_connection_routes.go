package api

import (
	"crypto/hmac"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/identity"
)

const hostAuthorizePath = "/connections/authorize"

func hostProtocolPath(path string) bool {
	switch path {
	case "/v1/connections/host/begin", "/v1/connections/host/poll", "/v1/connections/host/cancel", "/v1/connections/host/renew", "/v1/connections/host/revoke", "/v1/connections/host/handoff":
		return true
	}
	return false
}
func (s *Server) hostConnection(w http.ResponseWriter, r *http.Request) {
	if s.federation == nil {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, 405, "POST only")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	r.Body = http.MaxBytesReader(w, r.Body, 12<<10)
	action := strings.TrimPrefix(r.URL.Path, "/v1/connections/host/")
	if action == "begin" || action == "poll" || action == "cancel" {
		var b struct {
			Proof string `json:"proof"`
		}
		if decode(r, &b) != nil {
			writeError(w, 400, "invalid_request")
			return
		}
		if action == "begin" {
			h, e := s.federation.BeginHost(r.Context(), b.Proof)
			if e != nil {
				federationError(w, e)
				return
			}
			writeJSON(w, 200, map[string]string{"request_id": h.RequestID, "authorization_url": strings.TrimRight(s.security.PublicURL, "/") + hostAuthorizePath + "?request=" + url.QueryEscape(h.AuthorizationRef), "expires_at": h.ExpiresAt, "status": h.Status})
			return
		}
		h, e := s.federation.PollHost(r.Context(), b.Proof, action == "cancel")
		if e != nil {
			federationError(w, e)
			return
		}
		writeJSON(w, 200, h)
		return
	}
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		writeError(w, 401, "agent_token_required")
		return
	}
	token := strings.TrimPrefix(auth, "Bearer ")
	switch action {
	case "renew":
		var b struct {
			ExpectedVersion int `json:"expected_version"`
		}
		if decode(r, &b) != nil {
			writeError(w, 400, "invalid_request")
			return
		}
		c, e := s.federation.RenewHost(r.Context(), token, b.ExpectedVersion)
		if e != nil {
			federationError(w, e)
			return
		}
		writeJSON(w, 200, c)
	case "revoke":
		if e := s.federation.RevokeHost(r.Context(), token); e != nil {
			federationError(w, e)
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	case "handoff":
		code, expiry, e := s.federation.HostHandoff(r.Context(), token)
		if e != nil {
			federationError(w, e)
			return
		}
		writeJSON(w, 200, map[string]string{"url": strings.TrimRight(s.security.PublicURL, "/") + "/connections/host/handoff?code=" + url.QueryEscape(code), "expires_at": expiry.UTC().Format(time.RFC3339)})
	default:
		http.NotFound(w, r)
	}
}
func (s *Server) hostAuthorization(w http.ResponseWriter, r *http.Request) {
	if s.federation == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if !s.unauthLimit.AllowN("host-consent-page:"+s.security.ClientIP(r), 60, time.Minute) {
		writeError(w, 429, "too_many_requests")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		writeError(w, 405, "GET or POST")
		return
	}
	ref := r.URL.Query().Get("request")
	h, e := s.federation.InspectHostRequest(r.Context(), ref)
	if e != nil {
		writeError(w, 404, "authorization_not_found")
		return
	}
	u, ok := s.sessionUser(r)
	if !ok || u.Status != identity.StatusActive {
		writeConnectPage(w, 401, "Sign in to Toolyard", `<h1>Sign in to Toolyard</h1><p>Sign in with the account that will own this server agent. You will return to this request after sign-in.</p><p><a href="/login?host_request=`+url.QueryEscape(ref)+`">Sign in to Toolyard</a></p>`)
		return
	}
	if h.LocalUserID != "local-user" && (u.ClerkUserID == "" || u.ClerkUserID != h.LocalUserID) {
		writeConnectPage(w, 403, "Wrong account", "<h1>This request is for another user</h1><p>The server user does not match your Toolyard account. No access changed.</p>")
		return
	}
	if r.Method == http.MethodPost {
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		if r.Header.Get("Sec-Fetch-Site") != "" && r.Header.Get("Sec-Fetch-Site") != "same-origin" {
			writeError(w, 403, "same_origin_required")
			return
		}
		if r.ParseForm() != nil {
			writeError(w, 400, "invalid_form")
			return
		}
		nonceName, noncePath := s.hostCookie(r, "ty_host_"+h.RequestID, hostAuthorizePath)
		cookie, e := r.Cookie(nonceName)
		nonce := r.PostForm.Get("nonce")
		if e != nil || nonce == "" || !hmac.Equal([]byte(cookie.Value), []byte(nonce+"."+u.ID)) {
			writeError(w, 403, "authorization_nonce_invalid")
			return
		}
		verdict := r.PostForm.Get("verdict")
		if verdict != "accept" && verdict != "reject" {
			writeError(w, 400, "verdict_required")
			return
		}
		h, e = s.federation.DecideHost(r.Context(), ref, u.ID, verdict == "accept")
		if e != nil {
			federationError(w, e)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: nonceName, Value: "", Path: noncePath, MaxAge: -1, HttpOnly: true, Secure: s.security.IsBehindHTTPS(r), SameSite: http.SameSiteStrictMode})
	}
	if h.Status != "pending" {
		message := "No access was granted."
		if h.Status == "approved" {
			message = "Toolyard assigned this agent to your account. The server can now save its connection. Restricted calls still require an Inbox decision."
		}
		writeConnectPage(w, 200, "Server authorization: "+h.Status, "<h1>"+htmlEscape(h.Status)+"</h1><p>"+htmlEscape(message)+"</p><a href=\"/#agents\">View your agents</a>")
		return
	}
	nonce, e := randomNonce()
	if e != nil {
		writeError(w, 500, "nonce_failed")
		return
	}
	name, path := s.hostCookie(r, "ty_host_"+h.RequestID, hostAuthorizePath)
	http.SetCookie(w, &http.Cookie{Name: name, Value: nonce + "." + u.ID, Path: path, HttpOnly: true, Secure: s.security.IsBehindHTTPS(r), SameSite: http.SameSiteStrictMode, MaxAge: 600})
	body := `<h1>Allow this server to use Toolyard?</h1><p>Toolyard will create a dedicated agent owned by <strong>` + htmlEscape(u.Label()) + `</strong> (` + htmlEscape(u.Email) + `). Its credential stays on the destination server.</p><dl><dt>Server label (reported by the server)</dt><dd>` + htmlEscape(h.HostName) + `</dd><dt>Platform (reported by the server)</dt><dd>` + htmlEscape(h.Platform) + `</dd><dt>Environment ID</dt><dd>` + htmlEscape(h.EnvironmentID) + `</dd><dt>Server user</dt><dd>` + htmlEscape(h.LocalUserID) + `</dd><dt>Server key fingerprint</dt><dd style="overflow-wrap:anywhere">` + htmlEscape(h.Fingerprint) + `</dd><dt>Request expiry</dt><dd>` + htmlEscape(h.ExpiresAt) + `</dd></dl><p>Authorize only a server you selected in BKT3. Anyone who can use its local profile can use this connection. Host labels do not verify hardware.</p><p>Restricted calls still require your Inbox decision. You can revoke the server in Toolyard.</p><form method="post" action="` + hostAuthorizePath + `?request=` + htmlEscape(ref) + `"><input type="hidden" name="nonce" value="` + htmlEscape(nonce) + `"><button name="verdict" value="accept">Allow this server</button> <button name="verdict" value="reject">Reject</button></form>`
	writeConnectPage(w, 200, "Authorize a BKT3 server", body)
}
func (s *Server) hostManagement(w http.ResponseWriter, r *http.Request) {
	if s.federation == nil {
		http.NotFound(w, r)
		return
	}
	u, ok := s.sessionUser(r)
	if !ok {
		writeError(w, 401, "authentication_required")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.Path == "/v1/connections/hosts" && r.Method == http.MethodGet {
		all := r.URL.Query().Get("all") == "true"
		if all && u.Role != identity.RoleAdmin {
			writeError(w, 403, "admin_required")
			return
		}
		hosts, e := s.federation.ListHosts(r.Context(), u.ID, all)
		if e != nil {
			writeError(w, 500, "host_list_failed")
			return
		}
		writeJSON(w, 200, map[string]any{"hosts": hosts})
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/v1/connections/hosts/")
	agent, action, found := strings.Cut(path, "/")
	if r.Method != http.MethodPost || !found || action != "revoke" {
		writeError(w, 405, "POST revoke only")
		return
	}
	if e := s.federation.RevokeOwnedHost(r.Context(), u.ID, agent); e != nil {
		federationError(w, e)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
func (s *Server) hostHandoff(w http.ResponseWriter, r *http.Request) {
	if s.federation == nil || r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	user, e := s.federation.ConsumeHostHandoff(r.Context(), r.URL.Query().Get("code"))
	if e != nil {
		writeError(w, 401, "handoff_expired_or_used")
		return
	}
	if s.issueSession(w, r, user) != nil {
		writeError(w, 500, "session_failed")
		return
	}
	http.Redirect(w, r, "/#inbox", http.StatusSeeOther)
}
