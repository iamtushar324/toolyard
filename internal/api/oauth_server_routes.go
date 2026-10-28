package api

// oauth_server_routes.go mounts toolyard's OAuth 2.1 authorization server
// for MCP clients (internal/mcpauth) and the Connected apps API.
//
//   - The metadata documents, registration, token and revocation endpoints
//     need no session; mcpauth serves them.
//   - GET /oauth/authorize checks the request and stores it, then stops on
//     a page of our own: "Continue in this browser" links to the consent
//     screen, and "Copy link for another browser" shows that link to open
//     in another browser or Chrome profile (any browser where the person
//     signs in to toolyard). It never redirects there by itself. The
//     dashboard's session cookie is SameSite=Strict behind HTTPS, so a
//     cross-site navigation from the client arrives without it; the click
//     on our own page is a same-site navigation, which carries it. The page
//     also waits (oauth_wait_routes.go): after the person allows the app in
//     another browser, it offers Continue in this window, where the client
//     started.
//   - /oauth/consent shows the client, where it will send the person back
//     to, and lets them choose a nickname and the tool scope. The POST is a
//     plain form: a nonce cookie (SameSite=Strict, HttpOnly) proves it came
//     from the page we rendered, and the Origin check of
//     EnforceOriginOnMutations still applies. It stops again: allow sends
//     the browser (303) to the return link (oauth_return_routes.go), which
//     mints the code when it is used, in this browser or another; deny
//     shows a page whose links tell the client.
//   - /v1/me/apps... lists, renames, re-scopes and revokes connected apps;
//     /v1/me/agent-scopes sets the same per-agent scope on any agent.
//     Members see their own; admins may see and change everyone's.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/access"
	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/gateway"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/mcpauth"
)

// mcpOAuthProtocolPath reports the cookie-less OAuth endpoints (register,
// token, revoke): MCP clients call them from servers and native apps that
// send no Origin, and they read no cookie, so the CSRF header and Origin
// checks do not apply (security.go).
func mcpOAuthProtocolPath(path string) bool { return mcpauth.ProtocolPath(path) }

type mcpOAuthHandlers struct {
	s   *Server
	svc *mcpauth.Service
}

// MCPOAuthRoutes mounts the authorization server and the Connected apps
// API on mux. main.go calls it after Routes with the service it built; a
// nil service mounts nothing (every path then 404s as before).
func (s *Server) MCPOAuthRoutes(mux *http.ServeMux, svc *mcpauth.Service) {
	if svc == nil {
		return
	}
	h := &mcpOAuthHandlers{s: s, svc: svc}
	s.mcpAuth = svc
	// String literals throughout: role_guard_test.go finds the routes by
	// scanning for them. The paths match mcpauth's constants.
	mux.HandleFunc("/.well-known/oauth-protected-resource", svc.HandleProtectedResource)
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", svc.HandleProtectedResource)
	mux.HandleFunc("/.well-known/oauth-authorization-server", svc.HandleAuthorizationServer)
	mux.HandleFunc("/oauth/register", func(w http.ResponseWriter, r *http.Request) {
		svc.HandleRegister(w, r, s.security.ClientIP(r))
	})
	mux.HandleFunc("/oauth/token", svc.HandleToken)
	mux.HandleFunc("/oauth/revoke", svc.HandleRevoke)
	mux.HandleFunc("/oauth/authorize", h.authorize)
	mux.HandleFunc("/oauth/consent", h.consent)
	mux.HandleFunc("/oauth/return/", h.returnLink)
	mux.HandleFunc("/oauth/copy.js", h.copyScript)
	mux.HandleFunc("/oauth/wait", h.waitPoll)
	mux.HandleFunc("/oauth/wait/continue", h.waitContinue)
	mux.HandleFunc("/oauth/wait.js", h.waitScript)
	// Connected apps and per-agent scopes (member routes in role_guard.go).
	mux.HandleFunc("/v1/me/apps", h.listApps)
	mux.HandleFunc("/v1/me/apps/config", h.appsConfig)
	mux.HandleFunc("/v1/me/apps/rename", h.renameApp)
	mux.HandleFunc("/v1/me/apps/scope", h.scopeApp)
	mux.HandleFunc("/v1/me/apps/revoke", h.revokeApp)
	mux.HandleFunc("/v1/me/apps/catalog", h.catalog)
	mux.HandleFunc("/v1/me/apps/audit", h.appAudit)
	mux.HandleFunc("/v1/me/agent-scopes", h.agentScope)
}

// ---- authorize and consent -----------------------------------------------------

func (h *mcpOAuthHandlers) authorize(w http.ResponseWriter, r *http.Request) {
	if !h.svc.Enabled() {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	req, aerr := h.svc.BeginAuthorize(r.Context(), r.URL.Query(), h.s.security.ClientIP(r))
	if aerr != nil {
		if loc := aerr.Location(); loc != "" {
			http.Redirect(w, r, loc, http.StatusFound)
			return
		}
		status := aerr.Status
		if status == 0 {
			status = http.StatusBadRequest
		}
		writeConnectPage(w, status, "Cannot connect this app",
			`<h1>Toolyard cannot connect this app</h1><p>`+htmlEscape(aerr.Description)+`</p><p class="meta">Error: `+htmlEscape(aerr.Code)+`</p>`)
		return
	}
	// Stop here, signed in or not, so the person can choose the browser.
	// The consent screen asks for sign-in when this browser has no session
	// (or did not send it: the cookie is SameSite=Strict and this request
	// came from the client's site; the click below is same-site).
	next := mcpauth.ConsentPath + "?request=" + url.QueryEscape(req.ID)
	client := htmlEscape(req.Client.DisplayName())
	minutes := strconv.Itoa(int(mcpauth.RequestTTL / time.Minute))
	// The page waits for the answer from another browser (waitSection). Its
	// Continue form posts to /oauth/wait/continue, which answers with a 303
	// to the client: browsers apply form-action to that redirect, so the CSP
	// allows the client's redirect target. The nonce cookie, SameSite=Strict,
	// proves the form came from this page in this browser.
	nonce, err := h.s.stopPageNonce(req.ID)
	if err != nil {
		writeConnectPage(w, http.StatusInternalServerError, "Something went wrong", "<h1>Something went wrong</h1><p>Try again in a moment.</p>")
		return
	}
	h.setWaitNonce(w, r, req.ID, nonce)
	w.Header().Set("Content-Security-Policy", consentCSP(req.RedirectURI, ""))
	// A form POST under no-referrer sends Origin: null, which the Origin
	// check refuses; same-origin keeps the page's own origin on its form.
	w.Header().Set("Referrer-Policy", "same-origin")
	writeOAuthPage(w, http.StatusOK, "Connect "+req.Client.DisplayName(), oauthCopyScriptTag+oauthWaitScriptTag,
		`<h1>`+client+` wants to connect to your toolyard</h1>`+
			`<p class="meta">That name is the app's own claim. The next page shows who the app is, where toolyard sends you back to, and what it asks for.</p>`+
			`<p><a class="btn primary" href="`+htmlEscape(next)+`">Continue in this browser</a></p>`+
			`<h2>Copy link for another browser</h2>`+
			`<p>To answer in another browser or Chrome profile, open this link there and sign in to toolyard. It works for `+minutes+` minutes.</p>`+
			oauthCopyField("oauth-consent-link", h.svc.Issuer()+next, "Link for another browser")+
			// The confirmation code (mcpauth/code.go): an allow in another
			// browser needs it typed. It is only ever in this page's body.
			`<p class="oauth-code-line">Your code: <strong class="oauth-code">`+htmlEscape(mcpauth.FormatCode(req.Code))+`</strong>. `+
			`Type it on the consent page in the other browser.</p>`+
			h.waitSection(req, nonce))
}

// oauthCopyScriptTag loads the copy buttons' script. The pages' CSP
// allows scripts from 'self' only, and this one is ours (copyScript).
const oauthCopyScriptTag = `<script src="/oauth/copy.js" defer></script>`

// oauthCopyField is a read-only field that shows link and a Copy button
// for it. Without the script the field still shows the link, to select
// and copy by hand.
func oauthCopyField(id, link, label string) string {
	return `<p class="row"><input id="` + id + `" class="oauth-copy-field" type="text" readonly size="60" value="` + htmlEscape(link) +
		`" aria-label="` + htmlEscape(label) + `"> <button class="copy-btn" type="button" data-copy="` + id + `">Copy link</button></p>`
}

// oauthCopyScript wires the Copy buttons of the OAuth pages: a button with
// data-copy copies the field it names, and a click in a field selects it.
const oauthCopyScript = `// toolyard OAuth pages: Copy buttons (oauth_server_routes.go).
(function () {
  'use strict';
  document.addEventListener('click', function (ev) {
    var t = ev.target;
    if (t && t.classList && t.classList.contains('oauth-copy-field')) { t.select(); return; }
    var btn = t && t.closest ? t.closest('button[data-copy]') : null;
    if (!btn) return;
    var field = document.getElementById(btn.getAttribute('data-copy'));
    if (!field) return;
    field.focus();
    field.select();
    var copied = function () { btn.textContent = 'Copied'; btn.classList.add('copied'); };
    var fallback = function () { try { if (document.execCommand('copy')) copied(); } catch (_) {} };
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(field.value).then(copied, fallback);
    } else {
      fallback();
    }
  });
})();
`

// copyScript serves oauthCopyScript.
func (h *mcpOAuthHandlers) copyScript(w http.ResponseWriter, r *http.Request) {
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
	_, _ = io.WriteString(w, oauthCopyScript)
}

// writeOAuthPage is writeConnectPage with extra head elements.
func writeOAuthPage(w http.ResponseWriter, status int, title, head, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">` +
		head + `<title>` + htmlEscape(title) + ` — toolyard</title><link rel="stylesheet" href="/style.css"></head><body><main>` + body + `</main></body></html>`))
}

// consentNonceName is the nonce cookie for one authorization request.
func consentNonceName(requestID string) string {
	sum := sha256.Sum256([]byte(requestID))
	return "ty_oauth_" + hex.EncodeToString(sum[:8])
}

func (h *mcpOAuthHandlers) consent(w http.ResponseWriter, r *http.Request) {
	if !h.svc.Enabled() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	// A form POST under no-referrer sends Origin: null, which the Origin
	// check refuses; keep the page's own origin on its form.
	w.Header().Set("Referrer-Policy", "same-origin")
	if !h.svc.AllowIP(r.Context(), "consent", h.s.security.ClientIP(r), 120, 10*time.Minute) {
		writeConnectPage(w, http.StatusTooManyRequests, "Too many requests", "<h1>Too many requests</h1><p>Wait a few minutes and try again.</p>")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "GET or POST")
		return
	}
	id := r.URL.Query().Get("request")
	pending, err := h.svc.PendingRequest(r.Context(), id)
	if err != nil {
		var oe *mcpauth.Error
		switch {
		case errors.Is(err, mcpauth.ErrRequestNotFound):
			writeConnectPage(w, http.StatusBadRequest, "Sign-in expired",
				"<h1>This sign-in request expired or was already answered</h1><p>Go back to your app and connect toolyard again.</p>")
		case errors.As(err, &oe):
			writeConnectPage(w, http.StatusBadRequest, "Cannot connect this app",
				"<h1>Toolyard cannot connect this app</h1><p>"+htmlEscape(oe.Description)+"</p>")
		default:
			writeConnectPage(w, http.StatusInternalServerError, "Something went wrong", "<h1>Something went wrong</h1><p>Try again in a moment.</p>")
		}
		return
	}
	u, ok := h.s.sessionUser(r)
	if !ok || u.Status != identity.StatusActive {
		login := "/login?oauth_request=" + url.QueryEscape(pending.ID)
		writeConnectPage(w, http.StatusUnauthorized, "Sign in to toolyard",
			`<h1>Sign in to toolyard</h1><p><strong>`+htmlEscape(pending.Client.DisplayName())+`</strong> wants to connect to your toolyard. Sign in, and you come back here to decide what it may use.</p>`+
				`<p><a class="btn primary" href="`+htmlEscape(login)+`">Sign in to toolyard</a></p>`)
		return
	}
	if r.Method == http.MethodPost {
		h.consentSubmit(w, r, pending, u)
		return
	}
	h.renderConsent(w, r, pending, u, "", nil)
}

// scopeGroup is one server of the person's catalog on the consent screen
// and in the scope editor.
type scopeGroup struct {
	Name  string            `json:"name"`
	Tools []scopeGroupEntry `json:"tools"`
}

type scopeGroupEntry struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// ownerCatalog is what userID may use, grouped by server, without the
// always-on groups (every agent keeps those).
func (s *Server) ownerCatalog(r *http.Request, userID string) []scopeGroup {
	if s.gateway == nil {
		return nil
	}
	entries := s.gateway.CatalogFor(gateway.WithAgentID(r.Context(), "dashboard:"+userID))
	by := map[string][]scopeGroupEntry{}
	for _, e := range entries {
		g := access.GroupOf(e.Upstream, e.Name)
		if access.AlwaysOn(g) {
			continue
		}
		desc := strings.TrimSpace(e.Description)
		if r := []rune(desc); len(r) > 160 {
			desc = string(r[:160]) + "…"
		}
		by[g] = append(by[g], scopeGroupEntry{Name: e.Name, Description: desc})
	}
	out := make([]scopeGroup, 0, len(by))
	for g, tools := range by {
		sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
		out = append(out, scopeGroup{Name: g, Tools: tools})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// scopeFromForm builds the agent scope from the consent form, keeping
// only servers and tools the person may use.
func scopeFromForm(form url.Values, groups []scopeGroup) access.AgentScope {
	known := map[string]bool{}
	toolGroup := map[string]string{}
	for _, g := range groups {
		known[g.Name] = true
		for _, t := range g.Tools {
			toolGroup[t.Name] = g.Name
		}
	}
	sc := access.AgentScope{Mode: access.ScopeModeAll}
	if form.Get("scope_mode") == access.ScopeModeListed {
		sc.Mode = access.ScopeModeListed
	}
	servers := func(key string) []string {
		var out []string
		for _, v := range form[key] {
			if known[v] {
				out = append(out, v)
			}
		}
		return out
	}
	tools := func(key string) []access.ToolRef {
		var out []access.ToolRef
		for _, v := range form[key] {
			if g, ok := toolGroup[v]; ok {
				out = append(out, access.ToolRef{Server: g, Tool: v})
			}
		}
		return out
	}
	sc.DenyServers, sc.DenyTools = servers("deny_server"), tools("deny_tool")
	if sc.Mode == access.ScopeModeListed {
		sc.AllowServers, sc.AllowTools = servers("allow_server"), tools("allow_tool")
	}
	return sc
}

// consentDraft is what the form showed or the person typed.
type consentDraft struct {
	nickname      string
	scope         access.AgentScope
	mayManage     bool
	signOutOthers bool
	// connected: the person already has an active grant for this client
	// (another device), so the page offers to sign its sessions out.
	connected bool
}

func (h *mcpOAuthHandlers) consentSubmit(w http.ResponseWriter, r *http.Request, pending *mcpauth.AuthorizeRequest, u *identity.User) {
	r.Body = http.MaxBytesReader(w, r.Body, 512<<10)
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" {
		writeError(w, http.StatusForbidden, "same_origin_required")
		return
	}
	if r.ParseForm() != nil {
		writeError(w, http.StatusBadRequest, "invalid_form")
		return
	}
	nonceName, noncePath := h.s.hostCookie(r, consentNonceName(pending.ID), mcpauth.ConsentPath)
	cookie, err := r.Cookie(nonceName)
	nonce := r.PostForm.Get("nonce")
	if err != nil || nonce == "" || !hmac.Equal([]byte(cookie.Value), []byte(nonce+"."+u.ID)) {
		writeError(w, http.StatusForbidden, "consent_nonce_invalid")
		return
	}
	clearNonce := func() {
		http.SetCookie(w, &http.Cookie{Name: nonceName, Value: "", Path: noncePath, MaxAge: -1, HttpOnly: true,
			Secure: h.s.security.IsBehindHTTPS(r), SameSite: http.SameSiteStrictMode})
	}
	var loc, ticket string
	verdict := r.PostForm.Get("verdict")
	switch verdict {
	case "deny":
		loc, err = h.svc.Deny(r.Context(), pending.ID, u)
	case "allow":
		groups := h.s.ownerCatalog(r, u.ID)
		draft := &consentDraft{nickname: r.PostForm.Get("nickname"), scope: scopeFromForm(r.PostForm, groups),
			mayManage: u.Role == identity.RoleAdmin && r.PostForm.Get("may_manage") == "1", signOutOthers: r.PostForm.Get("sign_out_others") == "1"}
		if prev, err := h.svc.Existing(r.Context(), u.ID, pending.Client.ID); err == nil && prev != nil {
			draft.connected = !prev.Revoked
			// The consent screen does not show the tool list (the Connected
			// apps page sets it), so connecting again keeps the one chosen.
			draft.scope.ToolList = prev.Scope.ToolList
		}
		// A browser that did not render this request's stop page must type
		// its confirmation code; the stop page's cookie says which.
		ticket, err = h.svc.Approve(r.Context(), pending.ID, u, mcpauth.Consent{Nickname: draft.nickname, Scope: draft.scope,
			MayManage: draft.mayManage, SignOutOthers: draft.signOutOthers,
			SameBrowser: h.renderedStopPage(r, pending.ID), Code: r.PostForm.Get("code")})
		if errors.Is(err, mcpauth.ErrInvalidNickname) || errors.Is(err, access.ErrInvalidScope) {
			h.renderConsent(w, r, pending, u, err.Error(), draft)
			return
		}
		if errors.Is(err, mcpauth.ErrCodeWrong) {
			h.renderConsent(w, r, pending, u, "That code is not right. Type the code shown in the window where you started this connection.", draft)
			return
		}
		if errors.Is(err, mcpauth.ErrCodeLocked) {
			clearNonce()
			client := htmlEscape(pending.Client.DisplayName())
			writeConnectPage(w, http.StatusForbidden, "Sign-in ended",
				"<h1>Too many wrong codes</h1><p>A wrong code was typed "+strconv.Itoa(mcpauth.MaxCodeAttempts)+
					" times, so this sign-in ended. Nothing was allowed.</p><p>Start again from "+client+": connect toolyard there once more.</p>")
			return
		}
	default:
		writeError(w, http.StatusBadRequest, "verdict_required")
		return
	}
	if errors.Is(err, mcpauth.ErrRequestNotFound) {
		clearNonce()
		writeConnectPage(w, http.StatusBadRequest, "Sign-in expired",
			"<h1>This sign-in request expired or was already answered</h1><p>Go back to your app and connect toolyard again.</p>")
		return
	}
	if err != nil {
		writeConnectPage(w, http.StatusInternalServerError, "Something went wrong", "<h1>Something went wrong</h1><p>Nothing was allowed. Try again in a moment.</p>")
		return
	}
	clearNonce()
	if verdict == "deny" {
		h.renderDenied(w, pending, loc)
		return
	}
	// Never straight to the client: to the return link, a page of our own
	// (303, so a reload shows the page again instead of posting twice).
	http.Redirect(w, r, mcpauth.ReturnPath+ticket, http.StatusSeeOther)
}

// renderDenied stops after a deny too: loc, the client's redirect URI with
// error=access_denied, carries no code and no secret, so it is shown as is,
// to follow here or copy to the browser where the client waits.
func (h *mcpOAuthHandlers) renderDenied(w http.ResponseWriter, pending *mcpauth.AuthorizeRequest, loc string) {
	client := htmlEscape(pending.Client.DisplayName())
	writeOAuthPage(w, http.StatusOK, "Not allowed", oauthCopyScriptTag,
		`<h1>You did not allow `+client+`</h1>`+
			`<p>Nothing was connected. Tell `+client+` so it stops waiting: toolyard sends you back to <strong>`+htmlEscape(mcpauth.RedirectHost(pending.RedirectURI))+`</strong>.</p>`+
			`<p><a class="btn primary" href="`+htmlEscape(loc)+`">Continue in this browser</a></p>`+
			`<h2>Copy link for another browser</h2>`+
			`<p>Did `+client+` start in another browser or Chrome profile? Open this link there instead.</p>`+
			oauthCopyField("oauth-deny-link", loc, "Link for another browser"))
}

// consentCSP is an OAuth page's Content-Security-Policy: the strict one,
// with the client's redirect target allowed as a form-action when redirect
// is set (browsers apply form-action to the redirect that follows the
// POST; the return page's Continue needs it, the consent screen does not)
// and its logo's host allowed for images.
func consentCSP(redirect, logo string) string {
	csp := strictCSP
	safe := func(v string) bool { return v != "" && !strings.ContainsAny(v, " ;,'\"\t\r\n") }
	if src := mcpauth.CSPSource(redirect); safe(src) {
		csp = strings.Replace(csp, "form-action 'self' https://github.com", "form-action 'self' "+src, 1)
	} else {
		csp = strings.Replace(csp, "form-action 'self' https://github.com", "form-action 'self'", 1)
	}
	if u, err := url.Parse(logo); logo != "" && err == nil && u.Scheme == "https" && safe(u.Host) {
		csp = strings.Replace(csp, "img-src 'self' data: https://img.clerk.com", "img-src 'self' data: https://img.clerk.com https://"+u.Host, 1)
	}
	return csp
}

func (h *mcpOAuthHandlers) renderConsent(w http.ResponseWriter, r *http.Request, pending *mcpauth.AuthorizeRequest, u *identity.User, errMsg string, draft *consentDraft) {
	client := pending.Client
	if draft == nil {
		draft = &consentDraft{nickname: mcpauth.DefaultNickname(client.DisplayName()), scope: access.AgentScope{Mode: access.ScopeModeAll}}
		if prev, err := h.svc.Existing(r.Context(), u.ID, client.ID); err == nil && prev != nil {
			draft.nickname, draft.scope, draft.connected = prev.Nickname, prev.Scope, !prev.Revoked
		}
	}
	groups := h.s.ownerCatalog(r, u.ID)
	nonce, err := randomNonce()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "nonce_failed")
		return
	}
	name, path := h.s.hostCookie(r, consentNonceName(pending.ID), mcpauth.ConsentPath)
	http.SetCookie(w, &http.Cookie{Name: name, Value: nonce + "." + u.ID, Path: path, HttpOnly: true,
		Secure: h.s.security.IsBehindHTTPS(r), SameSite: http.SameSiteStrictMode, MaxAge: int(mcpauth.RequestTTL / time.Second)})
	// The form posts here and is answered with a page of our own (the
	// return link or the deny page), never the client's redirect URI, so
	// form-action stays 'self'.
	w.Header().Set("Content-Security-Policy", consentCSP("", client.LogoURI))

	var b strings.Builder
	e := htmlEscape
	b.WriteString(`<h1>Allow this app to use your toolyard?</h1>`)
	if client.LogoURI != "" {
		b.WriteString(`<p><img src="` + e(client.LogoURI) + `" alt="" width="48" height="48" referrerpolicy="no-referrer"></p>`)
	}
	b.WriteString(`<p>Signed in as <strong>` + e(u.Label()) + `</strong>`)
	if u.Email != "" {
		b.WriteString(` (` + e(u.Email) + `)`)
	}
	b.WriteString(`.</p><div class="card">`)
	// Who the app is: only a metadata document's host is checked; the
	// name is always the app's own claim.
	if host := mcpauth.ClientHost(client); host != "" {
		b.WriteString(`<p>Identified by <strong>` + e(host) + `</strong></p>` +
			`<p class="meta">Its client metadata document is ` + e(client.ID) + `. The name it gives itself (not checked): ` + e(client.DisplayName()) + `.</p>`)
		if mcpauth.RedirectLeavesClientHost(client, pending.RedirectURI) {
			b.WriteString(`<p class="err" role="alert">Warning: this app is identified by ` + e(host) + ` but sends you back to ` +
				e(mcpauth.RedirectHost(pending.RedirectURI)) + `. Allow it only if you expect that.</p>`)
		}
	} else {
		b.WriteString(`<p><strong>Self-registered app</strong></p>` +
			`<p class="meta">The name it gives itself (not checked): ` + e(client.DisplayName()) + `. Toolyard cannot check who made it: check the address below before you allow it.</p>`)
	}
	b.WriteString(`<p>After you answer, toolyard gives you a link back to</p><p><strong>` + e(mcpauth.RedirectHost(pending.RedirectURI)) + `</strong></p>` +
		`<p class="meta">` + e(pending.RedirectURI) + `</p></div>`)
	b.WriteString(`<p>It asks to use toolyard's tools (` + e(pending.Scope) + `). It will act as an agent you own, named by the nickname below. ` +
		`Toolyard records every call it makes, and calls that need approval still come to your Inbox. You can rename it, limit it or revoke it later on the Connected apps page.</p>`)
	// After an allow, the window where the connection started can finish
	// it by itself (oauth_wait_routes.go), wherever that window is.
	b.WriteString(`<p class="meta">Allow it only if you started this connection yourself, just now: after you allow it, the window where it started can finish it.</p>`)
	if errMsg != "" {
		b.WriteString(`<p class="err" role="alert">` + e(errMsg) + `</p>`)
	}
	b.WriteString(`<form method="post" action="` + e(mcpauth.ConsentPath+"?request="+url.QueryEscape(pending.ID)) + `">`)
	b.WriteString(`<input type="hidden" name="nonce" value="` + e(nonce) + `">`)
	// A browser that did not render the stop page types its confirmation
	// code (mcpauth/code.go). The field never shows what was typed before.
	// A request stored before codes existed needs none.
	if pending.HasCode && !h.renderedStopPage(r, pending.ID) {
		b.WriteString(`<fieldset><legend>Code from the window where you started</legend>` +
			`<p>Type the code shown in the window where you started this connection. If you did not start a connection yourself just now, do not type a code: close this page.</p>` +
			`<p><label for="code">Code from the window where you started</label><br><input id="code" name="code" required maxlength="16" placeholder="XXX-XXX" ` +
			`autocomplete="off" autocapitalize="characters" spellcheck="false"></p></fieldset>`)
	}
	b.WriteString(`<p><label for="nickname">Nickname for this connection</label><br><input id="nickname" name="nickname" value="` + e(draft.nickname) +
		`" maxlength="64" required></p><p class="meta">Letters, digits, spaces and . _ : ( ) - only.</p>`)
	checked := func(ok bool) string {
		if ok {
			return ` checked`
		}
		return ``
	}
	listed := draft.scope.Mode == access.ScopeModeListed
	b.WriteString(`<fieldset><legend>What may it use?</legend>` +
		`<p><label><input type="radio" name="scope_mode" value="all"` + checked(!listed) + `> Everything you can use</label></p>` +
		`<p><label><input type="radio" name="scope_mode" value="listed"` + checked(listed) + `> Only the servers and tools I allow below</label></p></fieldset>`)
	if len(groups) > 0 {
		allowS, denyS := setOf(draft.scope.AllowServers), setOf(draft.scope.DenyServers)
		allowT, denyT := map[string]bool{}, map[string]bool{}
		for _, t := range draft.scope.AllowTools {
			allowT[t.Tool] = true
		}
		for _, t := range draft.scope.DenyTools {
			denyT[t.Tool] = true
		}
		b.WriteString(`<fieldset><legend>Servers and tools</legend><p class="meta">"Allow" counts when you pick "Only the servers and tools I allow". "Block" always applies.</p>`)
		for _, g := range groups {
			b.WriteString(`<details><summary><strong>` + e(g.Name) + `</strong> · ` + strconv.Itoa(len(g.Tools)) + ` tools</summary>`)
			b.WriteString(`<p><label><input type="checkbox" name="allow_server" value="` + e(g.Name) + `"` + checked(allowS[g.Name]) + `> Allow every tool of ` + e(g.Name) + `</label> ` +
				`<label><input type="checkbox" name="deny_server" value="` + e(g.Name) + `"` + checked(denyS[g.Name]) + `> Block ` + e(g.Name) + `</label></p><ul>`)
			for _, t := range g.Tools {
				b.WriteString(`<li><code>` + e(t.Name) + `</code> <label><input type="checkbox" name="allow_tool" value="` + e(t.Name) + `"` + checked(allowT[t.Name]) + `> Allow</label> ` +
					`<label><input type="checkbox" name="deny_tool" value="` + e(t.Name) + `"` + checked(denyT[t.Name]) + `> Block</label></li>`)
			}
			b.WriteString(`</ul></details>`)
		}
		b.WriteString(`</fieldset>`)
	}
	if draft.connected {
		b.WriteString(`<fieldset><legend>Other sessions</legend><p><label><input type="checkbox" name="sign_out_others" value="1"` + checked(draft.signOutOthers) +
			`> Sign out this app's other sessions</label></p>` +
			`<p class="meta">You already connected this app, maybe on another device. Leave this off to keep those sessions working.</p></fieldset>`)
	}
	if u.Role == identity.RoleAdmin {
		b.WriteString(`<fieldset><legend>Admin control</legend><p><label><input type="checkbox" name="may_manage" value="1"` + checked(draft.mayManage) +
			`> May manage toolyard (policies, servers, shared connections)</label></p>` +
			`<p class="meta">Leave this off unless the app must change toolyard itself. Off, it uses tools like any agent of yours but cannot change policies, reconnect servers or sign in shared connections, although you are an admin.</p></fieldset>`)
	}
	b.WriteString(`<p class="row"><button class="primary" type="submit" name="verdict" value="allow">Allow</button> ` +
		`<button type="submit" name="verdict" value="deny" formnovalidate>Deny</button></p></form>`)
	status := http.StatusOK
	if errMsg != "" {
		status = http.StatusBadRequest
	}
	writeConnectPage(w, status, "Allow "+client.DisplayName(), b.String())
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// oauthAgentDeleted runs the OAuth revocation follow-up (cancel what the
// agent had pending) after the generic agent route deleted an agent that
// was an OAuth client; identity.DeleteAgent already revoked its grant.
func (s *Server) oauthAgentDeleted(ctx context.Context, agentID string) {
	if s.mcpAuth != nil {
		s.mcpAuth.AgentDeleted(ctx, agentID)
	}
}

func setOf(v []string) map[string]bool {
	m := make(map[string]bool, len(v))
	for _, s := range v {
		m[s] = true
	}
	return m
}

// ---- Connected apps API ----------------------------------------------------------

// appFor loads a connected app the caller may manage: their own, or any
// for an admin. Anything else answers 404, like an unknown id.
func (h *mcpOAuthHandlers) appFor(w http.ResponseWriter, r *http.Request, id string) (*mcpauth.App, *identity.User, bool) {
	u, err := h.s.requireUserFull(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return nil, nil, false
	}
	app, err := h.svc.App(r.Context(), id)
	if err != nil || (app.OwnerID != u.ID && u.Role != identity.RoleAdmin) {
		writeError(w, http.StatusNotFound, "app_not_found")
		return nil, nil, false
	}
	return app, u, true
}

func (h *mcpOAuthHandlers) listApps(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	u, err := h.s.requireUserFull(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	owner := u.ID
	if r.URL.Query().Get("all") == "true" {
		if u.Role != identity.RoleAdmin {
			writeError(w, http.StatusForbidden, "admin_only")
			return
		}
		owner = ""
	}
	apps, err := h.svc.ListApps(r.Context(), owner)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "apps_list_failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"apps": apps})
}

// appsConfig tells the page how to connect a client, and an admin the
// instance settings.
func (h *mcpOAuthHandlers) appsConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	u, err := h.s.requireUserFull(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	out := map[string]any{
		"enabled":    h.svc.Enabled(),
		"public_url": h.svc.Issuer() != "",
		"mcp_url":    h.svc.Resource(),
	}
	if u.Role == identity.RoleAdmin {
		out["settings"] = map[string]any{
			mcpauth.SettingEnabled:          h.svc.Enabled(),
			mcpauth.SettingRedirectHosts:    strings.Join(h.svc.RedirectHosts(), ", "),
			mcpauth.SettingAccessTTLMinutes: int(h.svc.AccessTTL() / time.Minute),
			mcpauth.SettingRefreshTTLDays:   int(h.svc.RefreshTTL() / (24 * time.Hour)),
			mcpauth.SettingMaxSessionDays:   int(h.svc.MaxSession() / (24 * time.Hour)),
			mcpauth.SettingCustomSchemes:    strings.Join(sortedKeys(h.svc.CustomSchemes()), ", "),
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *mcpOAuthHandlers) renameApp(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var body struct {
		ID       string `json:"id"`
		Nickname string `json:"nickname"`
	}
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body")
		return
	}
	app, u, ok := h.appFor(w, r, body.ID)
	if !ok {
		return
	}
	if err := h.svc.RenameApp(r.Context(), app, body.Nickname, u); err != nil {
		if errors.Is(err, mcpauth.ErrInvalidNickname) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "rename_failed")
		return
	}
	h.writeApp(w, r, app.ID)
}

func (h *mcpOAuthHandlers) scopeApp(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var body struct {
		ID        string             `json:"id"`
		Scope     *access.AgentScope `json:"scope"`
		MayManage *bool              `json:"may_manage"`
	}
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body")
		return
	}
	app, u, ok := h.appFor(w, r, body.ID)
	if !ok {
		return
	}
	// Only the app's owner, while an admin, may let it manage toolyard (an
	// admin may not give another person's app admin control); anyone who
	// manages the app may take that away.
	if body.MayManage != nil && *body.MayManage && (app.OwnerID != u.ID || u.Role != identity.RoleAdmin) {
		writeError(w, http.StatusForbidden, "owner_admin_only")
		return
	}
	if body.Scope != nil {
		if _, err := h.svc.SetAppScope(r.Context(), app, *body.Scope, u); err != nil {
			writeScopeError(w, err)
			return
		}
	}
	if body.MayManage != nil && *body.MayManage != app.MayManage {
		if err := h.svc.SetAppManage(r.Context(), app, *body.MayManage, u); err != nil {
			if errors.Is(err, mcpauth.ErrManageOwnerOnly) {
				writeError(w, http.StatusForbidden, "owner_admin_only")
				return
			}
			writeError(w, http.StatusInternalServerError, "manage_failed")
			return
		}
	}
	h.writeApp(w, r, app.ID)
}

func (h *mcpOAuthHandlers) revokeApp(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var body struct {
		ID string `json:"id"`
	}
	if err := decode(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body")
		return
	}
	app, u, ok := h.appFor(w, r, body.ID)
	if !ok {
		return
	}
	if err := h.svc.RevokeApp(r.Context(), app, u); err != nil {
		writeError(w, http.StatusInternalServerError, "revoke_failed")
		return
	}
	if h.s.access != nil {
		h.s.access.InvalidateAgent(app.AgentID)
	}
	h.writeApp(w, r, app.ID)
}

func (h *mcpOAuthHandlers) writeApp(w http.ResponseWriter, r *http.Request, id string) {
	app, err := h.svc.App(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "app_read_failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"app": app})
}

// agentOwnerFor resolves which person's catalog and scope a request is
// about: an app id (?id=) or an agent id (?agent_id=), which the caller
// must own unless they are an admin.
func (h *mcpOAuthHandlers) agentOwnerFor(w http.ResponseWriter, r *http.Request, appID, agentID string) (agent, owner string, u *identity.User, ok bool) {
	u, err := h.s.requireUserFull(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return "", "", nil, false
	}
	switch {
	case appID != "":
		app, err := h.svc.App(r.Context(), appID)
		if err != nil {
			writeError(w, http.StatusNotFound, "app_not_found")
			return "", "", nil, false
		}
		agent, owner = app.AgentID, app.OwnerID
	case strings.HasPrefix(agentID, "ag_") && h.s.access != nil:
		o, err := h.s.access.OwnerUser(r.Context(), agentID)
		if err != nil || o == "" {
			writeError(w, http.StatusNotFound, "agent_not_found")
			return "", "", nil, false
		}
		agent, owner = agentID, o
	default:
		writeError(w, http.StatusBadRequest, "id or agent_id required")
		return "", "", nil, false
	}
	if owner != u.ID && u.Role != identity.RoleAdmin {
		writeError(w, http.StatusNotFound, "agent_not_found")
		return "", "", nil, false
	}
	return agent, owner, u, true
}

// catalog lists what the owner of an app or agent may use, by server, for
// the scope editor. Without an id it is the caller's own catalog.
func (h *mcpOAuthHandlers) catalog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	q := r.URL.Query()
	var owner string
	if q.Get("id") == "" && q.Get("agent_id") == "" {
		u, err := h.s.requireUserFull(r)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		owner = u.ID
	} else {
		var ok bool
		if _, owner, _, ok = h.agentOwnerFor(w, r, q.Get("id"), q.Get("agent_id")); !ok {
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"servers": h.s.ownerCatalog(r, owner), "always_on": access.AlwaysOnGroups()})
}

// appAudit lists an app's (or agent's) recent audit rows.
func (h *mcpOAuthHandlers) appAudit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	q := r.URL.Query()
	agent, _, _, ok := h.agentOwnerFor(w, r, q.Get("id"), q.Get("agent_id"))
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if h.s.audit == nil {
		writeJSON(w, http.StatusOK, map[string]any{"events": []audit.Event{}})
		return
	}
	events, err := h.s.audit.Query(r.Context(), audit.Filter{AgentID: agent, Limit: limit, Before: qInt(r, "before")})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "audit_failed")
		return
	}
	if events == nil {
		events = []audit.Event{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"agent_id": agent, "events": events})
}

// agentScope reads (GET ?agent_id=) or replaces (POST {agent_id, scope})
// the per-agent scope of any agent the caller may manage. For an OAuth
// app the Connected apps routes say the same with an audit row. The scope
// carries the agent's tool list too (tool_list: small, the default, or
// full), under the same checks: a POST without it sets small.
func (h *mcpOAuthHandlers) agentScope(w http.ResponseWriter, r *http.Request) {
	if h.s.access == nil {
		writeError(w, http.StatusNotFound, "access_unavailable")
		return
	}
	switch r.Method {
	case http.MethodGet:
		agent, _, _, ok := h.agentOwnerFor(w, r, "", r.URL.Query().Get("agent_id"))
		if !ok {
			return
		}
		sc, err := h.s.access.AgentScope(r.Context(), agent)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "scope_read_failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"agent_id": agent, "scope": sc, "scope_summary": sc.Summary()})
	case http.MethodPost:
		var body struct {
			AgentID string            `json:"agent_id"`
			Scope   access.AgentScope `json:"scope"`
		}
		if err := decode(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_body")
			return
		}
		agent, _, u, ok := h.agentOwnerFor(w, r, "", body.AgentID)
		if !ok {
			return
		}
		if app, err := h.svc.AppByAgent(r.Context(), agent); err == nil {
			if _, err := h.svc.SetAppScope(r.Context(), app, body.Scope, u); err != nil {
				writeScopeError(w, err)
				return
			}
		} else {
			before, _ := h.s.access.AgentScope(r.Context(), agent)
			if _, err := h.s.access.SetAgentScope(r.Context(), agent, body.Scope, u.ID); err != nil {
				writeScopeError(w, err)
				return
			}
			after, _ := h.s.access.AgentScope(r.Context(), agent)
			if h.s.audit != nil {
				_ = h.s.audit.Write(r.Context(), audit.Event{EventType: "agent.scope_changed", AgentID: agent,
					ResultSummary: after.Summary(), Reason: "previous: " + before.Summary(),
					DecidedByUserID: u.ID, DecidedByEmail: u.Email, DecidedByName: u.Label(), DecidedVia: "dashboard"})
			}
		}
		sc, err := h.s.access.AgentScope(r.Context(), agent)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "scope_read_failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"agent_id": agent, "scope": sc, "scope_summary": sc.Summary()})
	default:
		writeError(w, http.StatusMethodNotAllowed, "GET or POST")
	}
}

func writeScopeError(w http.ResponseWriter, err error) {
	if errors.Is(err, access.ErrInvalidScope) {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeError(w, http.StatusInternalServerError, "scope_failed")
}
