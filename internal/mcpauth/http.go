package mcpauth

// http.go serves the endpoints that need no dashboard session: the two
// metadata documents, registration, the token endpoint and revocation.
// They read no cookie, so they answer any origin (CORS *) and are exempt
// from the dashboard's Origin check; client credentials, PKCE and the
// single-use codes are their protection.

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
)

// maxFormBody bounds a token or revocation request.
const maxFormBody = 64 << 10

// ProtocolPath reports whether path is one of the cookie-less protocol
// endpoints this file serves with POST (registration, token, revocation).
func ProtocolPath(path string) bool {
	return path == RegisterPath || path == TokenPath || path == RevokePath
}

// cors answers a preflight and sets the CORS headers; it reports whether
// the request was a preflight (already answered).
func cors(w http.ResponseWriter, r *http.Request, methods string) bool {
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Allow-Methods", methods+", OPTIONS")
	h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, MCP-Protocol-Version")
	h.Set("Access-Control-Max-Age", "600")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	return false
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeOAuthError(w http.ResponseWriter, e *Error, basic bool) {
	if e.Status == http.StatusUnauthorized && basic {
		w.Header().Set("WWW-Authenticate", `Basic realm="toolyard"`)
	}
	status := e.Status
	if status == 0 {
		status = http.StatusBadRequest
	}
	writeJSON(w, status, e)
}

// ProtectedResourceMetadata is the RFC 9728 document.
func (s *Service) ProtectedResourceMetadata() map[string]any {
	return map[string]any{
		"resource":                 s.Resource(),
		"authorization_servers":    []string{s.Issuer()},
		"scopes_supported":         []string{ScopeMCP, ScopeOfflineAccess},
		"bearer_methods_supported": []string{"header"},
		"resource_name":            "toolyard",
	}
}

// AuthorizationServerMetadata is the RFC 8414 document.
func (s *Service) AuthorizationServerMetadata() map[string]any {
	methods := []string{AuthNone, AuthSecretPost, AuthSecretBasic}
	return map[string]any{
		"issuer":                                         s.Issuer(),
		"authorization_endpoint":                         s.Issuer() + AuthorizePath,
		"token_endpoint":                                 s.Issuer() + TokenPath,
		"registration_endpoint":                          s.Issuer() + RegisterPath,
		"revocation_endpoint":                            s.Issuer() + RevokePath,
		"response_types_supported":                       []string{"code"},
		"response_modes_supported":                       []string{"query"},
		"grant_types_supported":                          []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":               []string{"S256"},
		"token_endpoint_auth_methods_supported":          methods,
		"revocation_endpoint_auth_methods_supported":     methods,
		"scopes_supported":                               []string{ScopeMCP, ScopeOfflineAccess},
		"client_id_metadata_document_supported":          true,
		"authorization_response_iss_parameter_supported": true,
	}
}

// HandleProtectedResource serves GET /.well-known/oauth-protected-resource
// (and its /mcp form).
func (s *Service) HandleProtectedResource(w http.ResponseWriter, r *http.Request) {
	s.serveMetadata(w, r, s.ProtectedResourceMetadata)
}

// HandleAuthorizationServer serves GET /.well-known/oauth-authorization-server.
func (s *Service) HandleAuthorizationServer(w http.ResponseWriter, r *http.Request) {
	s.serveMetadata(w, r, s.AuthorizationServerMetadata)
}

func (s *Service) serveMetadata(w http.ResponseWriter, r *http.Request, doc func() map[string]any) {
	if !s.Enabled() {
		http.NotFound(w, r)
		return
	}
	if cors(w, r, "GET") {
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD, OPTIONS")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_ = json.NewEncoder(w).Encode(doc())
	}
}

// HandleRegister serves POST /oauth/register (RFC 7591). ip is the
// caller's address as the dashboard's proxy settings see it.
func (s *Service) HandleRegister(w http.ResponseWriter, r *http.Request, ip string) {
	if !s.Enabled() {
		http.NotFound(w, r)
		return
	}
	if cors(w, r, "POST") {
		return
	}
	if r.Method != http.MethodPost {
		writeOAuthError(w, oauthErr(http.StatusMethodNotAllowed, "invalid_request", "POST a JSON object of client metadata"), false)
		return
	}
	key := rateKey(ip)
	if !s.allow(r.Context(), "register:"+key, limitFor(key, registerPerIP), registerWindow) ||
		!s.allow(r.Context(), "register:global", registerGlobal, registerWindow) {
		w.Header().Set("Retry-After", "600")
		writeOAuthError(w, oauthErr(http.StatusTooManyRequests, "temporarily_unavailable", "too many registrations; try again later"), false)
		return
	}
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		writeOAuthError(w, invalidMetadata("Content-Type must be application/json"), false)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRegistrationBody+1))
	if err != nil || len(body) > maxRegistrationBody {
		writeOAuthError(w, invalidMetadata("the registration body is too large"), false)
		return
	}
	resp, oerr := s.Register(r.Context(), body, ip)
	if oerr != nil {
		writeOAuthError(w, oerr, false)
		return
	}
	writeJSON(w, http.StatusCreated, resp)
}

// readForm parses a form-encoded POST body (never the query string, which
// must not carry credentials) and the client's authentication.
func readForm(w http.ResponseWriter, r *http.Request) (url.Values, ClientAuth, *Error) {
	if r.Method != http.MethodPost {
		return nil, ClientAuth{}, oauthErr(http.StatusMethodNotAllowed, "invalid_request", "use POST")
	}
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/x-www-form-urlencoded" {
		return nil, ClientAuth{}, invalidRequest("Content-Type must be application/x-www-form-urlencoded")
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBody)
	if err := r.ParseForm(); err != nil {
		return nil, ClientAuth{}, invalidRequest("the form body could not be read")
	}
	form := r.PostForm
	for k, v := range form {
		if len(v) > 1 {
			return nil, ClientAuth{}, invalidRequest("parameter " + k + " is repeated")
		}
	}
	ca := ClientAuth{ClientID: form.Get("client_id"), Secret: form.Get("client_secret")}
	if auth := r.Header.Get("Authorization"); auth != "" {
		id, secret, ok := basicAuth(auth)
		if !ok {
			return nil, ClientAuth{}, invalidClient("the Authorization header is not valid client_secret_basic")
		}
		if (ca.ClientID != "" && ca.ClientID != id) || ca.Secret != "" {
			return nil, ClientAuth{}, invalidRequest("use one client authentication method")
		}
		ca = ClientAuth{ClientID: id, Secret: secret, Basic: true}
	}
	return form, ca, nil
}

// basicAuth decodes client_secret_basic: base64 of the form-encoded id and
// secret joined by a colon (RFC 6749 section 2.3.1).
func basicAuth(header string) (id, secret string, ok bool) {
	const prefix = "Basic "
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", "", false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(header[len(prefix):]))
	if err != nil {
		return "", "", false
	}
	rawID, rawSecret, found := strings.Cut(string(raw), ":")
	if !found {
		return "", "", false
	}
	if id, err = url.QueryUnescape(rawID); err != nil {
		return "", "", false
	}
	if secret, err = url.QueryUnescape(rawSecret); err != nil {
		return "", "", false
	}
	return id, secret, id != ""
}

// HandleToken serves POST /oauth/token.
func (s *Service) HandleToken(w http.ResponseWriter, r *http.Request) {
	if !s.Enabled() {
		http.NotFound(w, r)
		return
	}
	if cors(w, r, "POST") {
		return
	}
	form, ca, oerr := readForm(w, r)
	if oerr != nil {
		writeOAuthError(w, oerr, r.Header.Get("Authorization") != "")
		return
	}
	resp, oerr := s.Exchange(r.Context(), form, ca)
	if oerr != nil {
		writeOAuthError(w, oerr, ca.Basic)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// HandleRevoke serves POST /oauth/revoke (RFC 7009).
func (s *Service) HandleRevoke(w http.ResponseWriter, r *http.Request) {
	if !s.Enabled() {
		http.NotFound(w, r)
		return
	}
	if cors(w, r, "POST") {
		return
	}
	form, ca, oerr := readForm(w, r)
	if oerr != nil {
		writeOAuthError(w, oerr, r.Header.Get("Authorization") != "")
		return
	}
	if oerr := s.Revoke(r.Context(), form.Get("token"), ca); oerr != nil {
		writeOAuthError(w, oerr, ca.Basic)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
}
