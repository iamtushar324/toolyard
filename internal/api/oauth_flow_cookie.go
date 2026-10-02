package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"

	"github.com/tusharbhardwaj/toolyard/internal/oauth"
)

// The OAuth flow cookie ties an authorization flow to the browser that
// started it.
//
// The provider sends the browser back to /v1/mcp-oauth/callback from its
// own site, and behind HTTPS the session cookie is SameSite=Strict, so it
// does not come along: the callback cannot see who the browser is. The
// begin response therefore sets a second, short-lived cookie that a
// cross-site top-level GET does carry (SameSite=Lax), scoped to the OAuth
// return routes only, whose value is an HMAC over (state, user). The
// callback recomputes it from the state in the URL and the user on the
// pending row: a match proves this is the browser that started that very
// flow, for that very person. The value carries no token and no user id.
//
// One cookie per flow (the name carries a prefix of the state), so two
// flows in flight in one browser do not clobber each other. The cookie
// lives as long as the pending row and is cleared when the callback runs.

const (
	oauthFlowCookiePrefix = "toolyard_oauth_flow_"
	oauthFlowCookiePath   = "/v1/mcp-oauth/"
	oauthFlowMACLabel     = "toolyard.oauth-flow.v1|"
	oauthFlowCookieState  = 16 // chars of the state carried in the cookie name
)

// oauthFlowCookieName is the cookie for one flow. The state is base64url,
// so every character is valid in a cookie name.
func oauthFlowCookieName(state string) string {
	if len(state) > oauthFlowCookieState {
		state = state[:oauthFlowCookieState]
	}
	return oauthFlowCookiePrefix + state
}

// oauthFlowMAC is the cookie value: HMAC-SHA256 under the session key with
// its own label, over the state and the user the flow belongs to.
func (s *Server) oauthFlowMAC(state, userID string) string {
	m := hmac.New(sha256.New, s.sessionKey)
	m.Write([]byte(oauthFlowMACLabel + state + "|" + userID))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// setOAuthFlowCookie binds the flow identified by state to the browser
// that made this request, as userID.
func (s *Server) setOAuthFlowCookie(w http.ResponseWriter, r *http.Request, state, userID string) {
	http.SetCookie(w, &http.Cookie{
		Name:     oauthFlowCookieName(state),
		Value:    s.oauthFlowMAC(state, userID),
		Path:     oauthFlowCookiePath,
		HttpOnly: true,
		Secure:   s.security.IsBehindHTTPS(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(oauth.PendingTTL.Seconds()),
	})
}

// oauthFlowCookieValid reports whether the request carries the flow
// cookie for (state, userID).
func (s *Server) oauthFlowCookieValid(r *http.Request, state, userID string) bool {
	c, err := r.Cookie(oauthFlowCookieName(state))
	if err != nil || c.Value == "" {
		return false
	}
	return hmac.Equal([]byte(c.Value), []byte(s.oauthFlowMAC(state, userID)))
}

// clearOAuthFlowCookie removes the flow cookie once the callback has run,
// whatever the outcome.
func (s *Server) clearOAuthFlowCookie(w http.ResponseWriter, r *http.Request, state string) {
	http.SetCookie(w, &http.Cookie{
		Name:     oauthFlowCookieName(state),
		Value:    "",
		Path:     oauthFlowCookiePath,
		HttpOnly: true,
		Secure:   s.security.IsBehindHTTPS(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}
