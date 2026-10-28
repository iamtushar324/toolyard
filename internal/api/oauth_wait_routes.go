package api

// oauth_wait_routes.go is the waiting stop page of toolyard's OAuth server
// for MCP clients (mcpauth/wait.go). The page /oauth/authorize stops on, in
// the window where the client started (often a pop-up the client opened),
// waits by itself while the person answers in another browser or Chrome
// profile through the copied consent link. After a yes it shows "Approved
// in another browser" and a Continue button; it never moves on by itself.
//
//	POST /oauth/wait           {"request", "waiter"} -> {"status": pending |
//	                           approved | denied | expired | used}, and the
//	                           client's name when approved. A missing or
//	                           wrong waiter gets the answer of an unknown
//	                           request. The page's script asks every 2 s.
//	POST /oauth/wait/continue  (the Continue form) checks Sec-Fetch-Site
//	                           when the browser sends it and the nonce
//	                           against the SameSite=Strict cookie the stop
//	                           page set, uses the request's approval once
//	                           (the return link then refuses as used, and
//	                           the other way round), has the code minted now
//	                           and sends the browser to the client's
//	                           redirect URI with a 303, as the return link
//	                           does.
//	GET  /oauth/wait.js        the page's script (CSP script-src 'self').
//
// Only the stop page carries the waiter secret, in its body (a hidden form
// field), never in a link: the consent link the person copies has the
// request id alone. The stop page's cookie (SameSite=Strict, its value
// bound to the request by a MAC under the session key) also tells the
// consent screen that this browser rendered the stop page, so it needs no
// confirmation code there (mcpauth/code.go). The secret travels in POST
// bodies only, so it stays out of access logs. Without JavaScript the page
// works as before: copy the consent link, and the return page in the other
// browser gives the return link. The routes are outside /v1, so HardenAPI and RoleGuard do not
// apply; the Origin check of EnforceOriginOnMutations does, and per-address
// limits bound the tries.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/mcpauth"
)

// waitNoncePrefix names the stop page's nonce cookies, one per request.
const waitNoncePrefix = "toolyard_oauth_wait_"

// waitCookiePath is the stop page cookie's path (without HTTPS; behind
// HTTPS it is __Host- with path /): it reaches /oauth/wait/continue and
// /oauth/consent.
const waitCookiePath = "/oauth/"

// stopPageMACLabel separates the stop page nonce's MAC from other uses of
// the session key.
const stopPageMACLabel = "toolyard.oauth-stop-page.v1|"

// The page asks every waitPollEvery, for at most waitPollFor (the request's
// lifetime and a margin: after that the server answers expired anyway).
// waitPollPerIP allows four waiting pages per address at once.
const (
	waitPollEvery = 2 * time.Second
	waitPollFor   = mcpauth.RequestTTL + 30*time.Second
	waitPollPerIP = 1200
)

// oauthWaitScriptTag loads the waiting page's script (waitScript).
const oauthWaitScriptTag = `<script src="/oauth/wait.js" defer></script>`

// waitNonceName is the nonce cookie for one authorization request.
func waitNonceName(requestID string) string {
	sum := sha256.Sum256([]byte(requestID))
	return waitNoncePrefix + hex.EncodeToString(sum[:8])
}

// stopPageNonce is a fresh nonce for request requestID's stop page:
// random, with a MAC under the session key that binds it to the request.
func (s *Server) stopPageNonce(requestID string) (string, error) {
	n, err := randomNonce()
	if err != nil {
		return "", err
	}
	return n + "." + s.stopPageMAC(requestID, n), nil
}

func (s *Server) stopPageMAC(requestID, n string) string {
	m := hmac.New(sha256.New, s.sessionKey)
	m.Write([]byte(stopPageMACLabel + requestID + "|" + n))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// renderedStopPage reports whether this browser rendered request
// requestID's stop page: it sends that page's cookie (SameSite=Strict, so
// only on a same-site request) with a value whose MAC binds it to this
// request. The consent screen then needs no confirmation code.
func (h *mcpOAuthHandlers) renderedStopPage(r *http.Request, requestID string) bool {
	name, _ := h.s.hostCookie(r, waitNonceName(requestID), waitCookiePath)
	c, err := r.Cookie(name)
	if err != nil {
		return false
	}
	n, mac, ok := strings.Cut(c.Value, ".")
	return ok && n != "" && hmac.Equal([]byte(mac), []byte(h.s.stopPageMAC(requestID, n)))
}

// setWaitNonce sets the stop page's nonce cookie for requestID. It lives
// as long as the request and its approval can (RequestTTL + ReturnTTL).
// Behind HTTPS it is __Host- (see hostCookie).
func (h *mcpOAuthHandlers) setWaitNonce(w http.ResponseWriter, r *http.Request, requestID, nonce string) {
	name, path := h.s.hostCookie(r, waitNonceName(requestID), waitCookiePath)
	http.SetCookie(w, &http.Cookie{Name: name, Value: nonce, Path: path, HttpOnly: true,
		Secure: h.s.security.IsBehindHTTPS(r), SameSite: http.SameSiteStrictMode,
		MaxAge: int((mcpauth.RequestTTL + mcpauth.ReturnTTL) / time.Second)})
}

// waitSection is the stop page's waiting part: one block per state, all
// hidden until the script (waitScript) shows the one the server reports.
// The Continue form carries the request id, the waiter secret and the
// nonce; nothing else on the page has the waiter.
func (h *mcpOAuthHandlers) waitSection(req *mcpauth.AuthorizeRequest, nonce string) string {
	e := htmlEscape
	client := e(req.Client.DisplayName())
	host := e(mcpauth.RedirectHost(req.RedirectURI))
	again := `<p>Start again from ` + client + `: connect toolyard there once more.</p>`
	var b strings.Builder
	b.WriteString(`<div id="oauth-wait" data-every-ms="` + strconv.Itoa(int(waitPollEvery/time.Millisecond)) +
		`" data-wait-ms="` + strconv.Itoa(int(waitPollFor/time.Millisecond)) + `">`)
	b.WriteString(`<div data-wait-state="pending" hidden>` +
		`<p>Keep this window open: after you allow the app in the other browser, this window continues by itself.</p>` +
		`<p class="meta" role="status">Waiting for your answer…</p></div>`)
	b.WriteString(`<div data-wait-state="approved" hidden><h2>Approved in another browser</h2>` +
		`<p>You allowed <strong>` + client + `</strong> in another browser. Continue to finish here, in the window where it waits: toolyard sends you back to <strong>` +
		host + `</strong>.</p>` +
		`<form id="oauth-wait-form" method="post" action="` + mcpauth.WaitContinuePath + `">` +
		`<input type="hidden" name="request" value="` + e(req.ID) + `">` +
		`<input type="hidden" name="waiter" value="` + e(req.Waiter) + `">` +
		`<input type="hidden" name="nonce" value="` + e(nonce) + `">` +
		`<p><button class="primary" type="submit">Continue</button></p></form></div>`)
	b.WriteString(`<div data-wait-state="denied" hidden><h2>You did not allow ` + client + `</h2>` +
		`<p>Nothing was connected. Tell ` + client + ` so it stops waiting: toolyard sends you back to <strong>` + host + `</strong>.</p>` +
		`<p><a class="btn primary" href="` + e(h.svc.DenyLocation(req)) + `">Continue</a></p></div>`)
	b.WriteString(`<div data-wait-state="expired" hidden><h2>This sign-in expired</h2>` +
		`<p>It was not finished within ` + strconv.Itoa(int(mcpauth.RequestTTL/time.Minute)) + ` minutes, a newer approval replaced it, or a wrong code was typed too often.</p>` + again + `</div>`)
	b.WriteString(`<div data-wait-state="used" hidden><h2>This sign-in was already finished</h2>` +
		`<p>Its return link was used, in this browser or another. If ` + client + ` is still waiting, it did not get the sign-in.</p>` + again + `</div>`)
	b.WriteString(`<noscript><p>After you allow the app, toolyard gives you a link back to this browser, where ` + client + ` waits.</p></noscript>`)
	b.WriteString(`</div>`)
	return b.String()
}

// waitPoll serves POST /oauth/wait: the state of one request for the
// page that holds its waiter.
func (h *mcpOAuthHandlers) waitPoll(w http.ResponseWriter, r *http.Request) {
	if !h.svc.Enabled() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	if !h.svc.AllowIP(r.Context(), "oauth_wait", h.s.security.ClientIP(r), waitPollPerIP, 10*time.Minute) {
		writeError(w, http.StatusTooManyRequests, "rate_limited")
		return
	}
	// Only the stop page's own script asks.
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" {
		writeError(w, http.StatusForbidden, "same_origin_required")
		return
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "application/json required")
		return
	}
	var body struct {
		Request string `json:"request"`
		Waiter  string `json:"waiter"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<10)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body")
		return
	}
	info, err := h.svc.Wait(r.Context(), body.Request, body.Waiter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "wait_failed")
		return
	}
	out := map[string]string{"status": info.Status}
	if info.Status == mcpauth.WaitApproved && info.Client != nil {
		out["client"] = info.Client.DisplayName()
	}
	writeJSON(w, http.StatusOK, out)
}

// waitContinue serves POST /oauth/wait/continue: the waiting page's
// Continue uses the approval and sends the browser to the client.
func (h *mcpOAuthHandlers) waitContinue(w http.ResponseWriter, r *http.Request) {
	if !h.svc.Enabled() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "same-origin")
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	if !h.svc.AllowIP(r.Context(), "oauth_wait_continue", h.s.security.ClientIP(r), 60, 10*time.Minute) {
		writeConnectPage(w, http.StatusTooManyRequests, "Too many requests", "<h1>Too many requests</h1><p>Wait a few minutes and try again.</p>")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	// The form came from this origin's stop page: the browser says so
	// (Sec-Fetch-Site), and the nonce it carries is the one that page's
	// response set as a cookie.
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" {
		writeConnectPage(w, http.StatusForbidden, "Start again",
			"<h1>This button only works on toolyard's own page</h1><p>Nothing was sent to the app. Start again from your app.</p>")
		return
	}
	if r.ParseForm() != nil {
		writeConnectPage(w, http.StatusBadRequest, "Start again",
			"<h1>The form could not be read</h1><p>Nothing was sent to the app. Start again from your app.</p>")
		return
	}
	id, waiter, nonce := r.PostForm.Get("request"), r.PostForm.Get("waiter"), r.PostForm.Get("nonce")
	if !mcpauth.ValidRequestID(id) {
		writeWaitRefusal(w, nil, mcpauth.ErrWaitUnknown)
		return
	}
	name, path := h.s.hostCookie(r, waitNonceName(id), waitCookiePath)
	cookie, err := r.Cookie(name)
	if err != nil || nonce == "" || !hmac.Equal([]byte(cookie.Value), []byte(nonce)) {
		writeConnectPage(w, http.StatusForbidden, "Start again",
			"<h1>This page is stale or did not come from toolyard</h1><p>Nothing was sent to the app. Use the return link from the other browser, or start again from your app.</p>")
		return
	}
	loc, err := h.svc.RedeemWait(r.Context(), id, waiter)
	// The nonce is spent once the approval is used or can never be: keep
	// it only while the request waits for an answer, or after an error the
	// person may retry.
	if err == nil || isFinalWaitRefusal(err) {
		http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: path, MaxAge: -1, HttpOnly: true,
			Secure: h.s.security.IsBehindHTTPS(r), SameSite: http.SameSiteStrictMode})
	}
	if err != nil {
		var client *mcpauth.Client
		if info, ierr := h.svc.Wait(r.Context(), id, waiter); ierr == nil {
			client = info.Client
		}
		writeWaitRefusal(w, client, err)
		return
	}
	http.Redirect(w, r, loc, http.StatusSeeOther)
}

// isFinalWaitRefusal reports a refusal no later Continue can turn into a
// code.
func isFinalWaitRefusal(err error) bool {
	for _, e := range []error{mcpauth.ErrWaitUnknown, mcpauth.ErrWaitDenied, mcpauth.ErrReturnUsed, mcpauth.ErrReturnExpired,
		mcpauth.ErrReturnRevoked, mcpauth.ErrReturnUnknown} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

// writeWaitRefusal is the page for a Continue that cannot be used. client,
// when known, names the app.
func writeWaitRefusal(w http.ResponseWriter, client *mcpauth.Client, err error) {
	name := "your app"
	if client != nil {
		name = client.DisplayName()
	}
	app := htmlEscape(name)
	again := "<p>Start again from " + app + ": connect toolyard there once more.</p>"
	switch {
	case errors.Is(err, mcpauth.ErrWaitPending):
		writeConnectPage(w, http.StatusConflict, "Not allowed yet",
			"<h1>"+app+" is not allowed yet</h1><p>Allow it in the other browser first. Then go back and press Continue again. Nothing was sent to the app.</p>")
	case errors.Is(err, mcpauth.ErrWaitDenied):
		writeConnectPage(w, http.StatusForbidden, "Not allowed",
			"<h1>You did not allow "+app+"</h1><p>Nothing was connected.</p>"+again)
	case errors.Is(err, mcpauth.ErrReturnUsed):
		writeConnectPage(w, http.StatusGone, "Already finished",
			"<h1>This sign-in was already finished</h1><p>Its return link was used first, in this browser or another: a sign-in finishes once. If "+
				app+" is still waiting, it did not get the sign-in.</p>"+again)
	case errors.Is(err, mcpauth.ErrReturnExpired):
		writeConnectPage(w, http.StatusGone, "Sign-in expired",
			"<h1>This sign-in expired</h1><p>After you allow the app, this window can finish within "+strconv.Itoa(int(mcpauth.ReturnTTL/time.Minute))+
				" minutes, and only for the newest approval.</p>"+again)
	case errors.Is(err, mcpauth.ErrReturnRevoked):
		writeConnectPage(w, http.StatusGone, "Connection revoked",
			"<h1>This connection was revoked</h1><p>It was revoked, or its owner can no longer sign in, after you allowed it. Nothing was sent to the app.</p>"+again)
	case errors.Is(err, mcpauth.ErrWaitUnknown), errors.Is(err, mcpauth.ErrReturnUnknown):
		writeConnectPage(w, http.StatusNotFound, "Page not valid",
			"<h1>This page is not valid</h1><p>It did not come from toolyard, or its sign-in ended more than an hour ago. Nothing was sent to the app.</p>"+again)
	default:
		writeConnectPage(w, http.StatusInternalServerError, "Something went wrong",
			"<h1>Something went wrong</h1><p>Nothing was sent to the app. Go back and try again in a moment.</p>")
	}
}

// oauthWaitScript runs the waiting page: it asks /oauth/wait every few
// seconds (at once when the page becomes visible again: background tabs
// slow timers down) and shows the block of the state the server reports.
// It never submits the Continue form itself: the person always presses it.
const oauthWaitScript = `// toolyard OAuth stop page: wait for the answer from another browser (oauth_wait_routes.go).
(function () {
  'use strict';
  var box = document.getElementById('oauth-wait');
  var form = document.getElementById('oauth-wait-form');
  if (!box || !form || !window.fetch || !window.JSON) return;
  var request = form.elements.namedItem('request').value, waiter = form.elements.namedItem('waiter').value;
  var every = parseInt(box.getAttribute('data-every-ms'), 10) || 2000;
  var deadline = Date.now() + (parseInt(box.getAttribute('data-wait-ms'), 10) || 630000);
  var done = false, busy = false, timer = null;
  var show = function (state) {
    var parts = box.querySelectorAll('[data-wait-state]');
    for (var i = 0; i < parts.length; i++) parts[i].hidden = parts[i].getAttribute('data-wait-state') !== state;
  };
  var finish = function (state) {
    done = true;
    if (timer) { clearTimeout(timer); timer = null; }
    show(state);
  };
  var schedule = function () { if (!done && !timer) timer = setTimeout(poll, every); };
  var poll = function () {
    timer = null;
    if (done || busy) return;
    if (Date.now() > deadline) { finish('expired'); return; }
    busy = true;
    fetch('/oauth/wait', {
      method: 'POST', credentials: 'omit', cache: 'no-store',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ request: request, waiter: waiter })
    }).then(function (r) { return r.ok ? r.json() : null; }).then(function (out) {
      busy = false;
      var st = out && out.status;
      if (st === 'approved' || st === 'denied' || st === 'expired' || st === 'used') { finish(st); return; }
      schedule();
    }, function () { busy = false; schedule(); });
  };
  document.addEventListener('visibilitychange', function () {
    if (document.hidden || done || busy) return;
    if (timer) { clearTimeout(timer); timer = null; }
    poll();
  });
  show('pending');
  poll();
})();
`

// waitScript serves oauthWaitScript.
func (h *mcpOAuthHandlers) waitScript(w http.ResponseWriter, r *http.Request) {
	if !h.svc.Enabled() {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = io.WriteString(w, oauthWaitScript)
}
