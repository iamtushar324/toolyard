package api

// oauth_return_routes.go is the return link of toolyard's OAuth server for
// MCP clients (mcpauth/return.go): /oauth/return/{ticket}. After the person
// allows an app on the consent screen, the browser comes here (303) instead
// of going to the client, so it shows the same page as any browser the
// person opens the link in: the one where they started, or another
// browser or Chrome profile where the client waits.
//
//	GET   "Approved. Return to <client>": where the code goes, whose
//	      toolyard the app connects to, a "Continue in this browser"
//	      button, and the link itself to copy. It changes nothing, so a link
//	      previewer or a drive-by fetch cannot use the ticket up, and sets a
//	      SameSite=Strict nonce cookie whose value the form carries.
//	POST  (the button) checks Sec-Fetch-Site when the browser sends it and
//	      the nonce against its cookie, uses the ticket once, has the
//	      authorization code minted now (its 60 seconds start here) and
//	      sends the browser to the client's redirect URI with a 303.
//	HEAD  answers headers only and never uses the ticket.
//
// No toolyard session is needed (the browser where the client waits may
// not be signed in to toolyard): the ticket is the capability. It can only
// send a code to the client's registered redirect URI, and the client's
// state and PKCE verifier, which never left the client, protect the code.
//
// The page's Content-Security-Policy widens form-action to the client's
// redirect target: browsers apply the page's form-action to the redirect
// the POST answers with. The route is outside /v1, so HardenAPI and
// RoleGuard do not apply; the Origin check of EnforceOriginOnMutations
// does, and the per-address limit below bounds the tries.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/mcpauth"
)

// returnNoncePrefix names the return page's nonce cookies, one per ticket.
const returnNoncePrefix = "toolyard_oauth_return_"

// returnNonceName is the nonce cookie for one ticket.
func returnNonceName(ticket string) string {
	sum := sha256.Sum256([]byte(ticket))
	return returnNoncePrefix + hex.EncodeToString(sum[:8])
}

// returnLink serves /oauth/return/{ticket}.
func (h *mcpOAuthHandlers) returnLink(w http.ResponseWriter, r *http.Request) {
	if !h.svc.Enabled() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	// The ticket is in the path: no Referer leaves this origin. A form
	// POST under no-referrer sends Origin: null, which the Origin check
	// refuses; same-origin keeps the page's own origin on its form.
	w.Header().Set("Referrer-Policy", "same-origin")
	if !h.svc.AllowIP(r.Context(), "oauth_return", h.s.security.ClientIP(r), 60, 10*time.Minute) {
		writeConnectPage(w, http.StatusTooManyRequests, "Too many requests", "<h1>Too many requests</h1><p>Wait a few minutes and try again.</p>")
		return
	}
	ticket := strings.TrimPrefix(r.URL.Path, mcpauth.ReturnPath)
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		h.returnConfirm(w, r, ticket)
	case http.MethodPost:
		h.returnContinue(w, r, ticket)
	default:
		w.Header().Set("Allow", "GET, HEAD, POST")
		writeError(w, http.StatusMethodNotAllowed, "GET or POST")
	}
}

// returnConfirm (GET) shows the return page. It uses nothing up.
func (h *mcpOAuthHandlers) returnConfirm(w http.ResponseWriter, r *http.Request, ticket string) {
	info, err := h.svc.ReturnTicket(r.Context(), ticket)
	if err != nil {
		writeReturnRefusal(w, info, err)
		return
	}
	nonce, err := randomNonce()
	if err != nil {
		writeConnectPage(w, http.StatusInternalServerError, "Something went wrong", "<h1>Something went wrong</h1><p>Try again in a moment.</p>")
		return
	}
	// Behind HTTPS the cookie is __Host- (see hostCookie): only this host
	// can have set it.
	name, path := h.s.hostCookie(r, returnNonceName(ticket), mcpauth.ReturnPath)
	http.SetCookie(w, &http.Cookie{Name: name, Value: nonce, Path: path, HttpOnly: true,
		Secure: h.s.security.IsBehindHTTPS(r), SameSite: http.SameSiteStrictMode, MaxAge: int(mcpauth.ReturnTTL / time.Second)})
	w.Header().Set("Content-Security-Policy", consentCSP(info.RedirectURI, info.Client.LogoURI))

	e := htmlEscape
	client := e(info.Client.DisplayName())
	var b strings.Builder
	b.WriteString(`<h1>Approved. Return to ` + client + `</h1>`)
	if info.Client.LogoURI != "" {
		b.WriteString(`<p><img src="` + e(info.Client.LogoURI) + `" alt="" width="48" height="48" referrerpolicy="no-referrer"></p>`)
	}
	b.WriteString(`<p>You allowed <strong>` + client + `</strong> to use the toolyard of <strong>` + e(info.Account) + `</strong>, as <strong>` +
		e(info.Nickname) + `</strong>. To finish, go back to it in the browser where you started.</p>`)
	b.WriteString(`<div class="card"><p>Toolyard sends you back to</p><p><strong>` + e(mcpauth.RedirectHost(info.RedirectURI)) + `</strong></p>` +
		`<p class="meta">` + e(info.RedirectURI) + `</p></div>`)
	b.WriteString(`<form method="post" action="` + e(mcpauth.ReturnPath+ticket) + `">` +
		`<input type="hidden" name="nonce" value="` + e(nonce) + `">` +
		`<p><button class="primary" type="submit">Continue in this browser</button></p></form>`)
	b.WriteString(`<h2>Copy return link</h2>` +
		`<p>Did ` + client + ` start in another browser or Chrome profile? Open this link there instead. ` +
		`It works once, within ` + strconv.Itoa(int(mcpauth.ReturnTTL/time.Minute)) + ` minutes of your approval, and needs no toolyard sign-in.</p>`)
	b.WriteString(oauthCopyField("oauth-return-link", h.svc.Issuer()+mcpauth.ReturnPath+ticket, "Return link"))
	writeOAuthPage(w, http.StatusOK, "Return to "+info.Client.DisplayName(), oauthCopyScriptTag, b.String())
}

// returnContinue (POST) uses the ticket and sends the browser to the
// client with the code.
func (h *mcpOAuthHandlers) returnContinue(w http.ResponseWriter, r *http.Request, ticket string) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	// The form came from this origin's return page: the browser says so
	// (Sec-Fetch-Site), and the nonce it carries is the one that page's
	// response set as a cookie.
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" {
		writeConnectPage(w, http.StatusForbidden, "Open the link again",
			"<h1>Open the return link again</h1><p>This button only works on toolyard's own return page. Nothing was sent to the app.</p>")
		return
	}
	if r.ParseForm() != nil {
		writeConnectPage(w, http.StatusBadRequest, "Open the link again",
			"<h1>Open the return link again</h1><p>The form could not be read. Nothing was sent to the app.</p>")
		return
	}
	name, path := h.s.hostCookie(r, returnNonceName(ticket), mcpauth.ReturnPath)
	cookie, err := r.Cookie(name)
	nonce := r.PostForm.Get("nonce")
	if err != nil || nonce == "" || !hmac.Equal([]byte(cookie.Value), []byte(nonce)) {
		writeConnectPage(w, http.StatusForbidden, "Open the link again",
			"<h1>Open the return link again</h1><p>This page is stale or did not come from toolyard's return page. Nothing was sent to the app.</p>")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: path, MaxAge: -1, HttpOnly: true,
		Secure: h.s.security.IsBehindHTTPS(r), SameSite: http.SameSiteStrictMode})
	loc, err := h.svc.RedeemReturn(r.Context(), ticket)
	if err != nil {
		info, _ := h.svc.ReturnTicket(r.Context(), ticket)
		writeReturnRefusal(w, info, err)
		return
	}
	http.Redirect(w, r, loc, http.StatusSeeOther)
}

// writeReturnRefusal is the page for a return link that cannot be used.
// info, when known, names the app.
func writeReturnRefusal(w http.ResponseWriter, info *mcpauth.ReturnInfo, err error) {
	client := "your app"
	if info != nil && info.Client != nil {
		client = info.Client.DisplayName()
	}
	again := "<p>Start again from " + htmlEscape(client) + ": connect toolyard there once more.</p>"
	switch {
	case errors.Is(err, mcpauth.ErrReturnUsed):
		writeConnectPage(w, http.StatusGone, "Return link already used",
			"<h1>This return link was already used</h1><p>A return link works once. If "+htmlEscape(client)+" is still waiting, it did not get the sign-in.</p>"+again)
	case errors.Is(err, mcpauth.ErrReturnExpired):
		writeConnectPage(w, http.StatusGone, "Return link expired",
			"<h1>This return link expired</h1><p>A return link works for "+strconv.Itoa(int(mcpauth.ReturnTTL/time.Minute))+
				" minutes after you allow the app, and only the newest one works.</p>"+again)
	case errors.Is(err, mcpauth.ErrReturnRevoked):
		writeConnectPage(w, http.StatusGone, "Connection revoked",
			"<h1>This connection was revoked</h1><p>It was revoked, or its owner can no longer sign in, after you allowed it. Nothing was sent to the app.</p>"+again)
	case errors.Is(err, mcpauth.ErrReturnUnknown):
		writeConnectPage(w, http.StatusNotFound, "Return link not valid",
			"<h1>This return link is not valid</h1><p>Check that you copied all of it. An old link stops existing an hour after it expires.</p>"+again)
	default:
		writeConnectPage(w, http.StatusInternalServerError, "Something went wrong",
			"<h1>Something went wrong</h1><p>Nothing was sent to the app. Try again in a moment.</p>")
	}
}
