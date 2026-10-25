package mcpauth

// clients.go knows who a client is: one registered through RFC 7591
// dynamic client registration (DCR), or one identified by its client ID
// metadata document (CIMD: the client_id is an https URL serving the
// client's own registration as JSON). Both end up as a mcp_oauth_clients
// row, the second as a cached copy that is fetched again when it expires.

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Client kinds.
const (
	ClientKindDCR  = "dcr"
	ClientKindCIMD = "cimd"
)

// Token endpoint authentication methods this server supports.
const (
	AuthNone        = "none"
	AuthSecretPost  = "client_secret_post"
	AuthSecretBasic = "client_secret_basic"
)

// Registration and document limits.
const (
	maxRegistrationBody = 16 << 10
	maxRedirectURIs     = 10
	maxRedirectURILen   = 2000
	maxClientNameLen    = 100
	maxCIMDBody         = 64 << 10
	cimdFetchTimeout    = 8 * time.Second
	cimdDefaultTTL      = time.Hour
	cimdMinTTL          = 5 * time.Minute
	cimdMaxTTL          = 24 * time.Hour
)

// Client is a registered or document-identified client.
type Client struct {
	ID           string   `json:"client_id"`
	Kind         string   `json:"kind"`
	Name         string   `json:"client_name"`
	URI          string   `json:"client_uri,omitempty"`
	LogoURI      string   `json:"logo_uri,omitempty"`
	RedirectURIs []string `json:"redirect_uris"`
	AuthMethod   string   `json:"token_endpoint_auth_method"`
	CreatedAt    int64    `json:"created_at"`
	secretHash   string
	expiresAt    int64
}

// DisplayName is how the client is shown: its name, else the host of its
// metadata document, else "Unnamed client".
func (c *Client) DisplayName() string {
	if c.Name != "" {
		return c.Name
	}
	if c.Kind == ClientKindCIMD {
		if u, err := url.Parse(c.ID); err == nil && u.Host != "" {
			return u.Host
		}
	}
	return "Unnamed client"
}

// Public reports whether the client has no secret (PKCE is its proof).
func (c *Client) Public() bool { return c.AuthMethod == AuthNone }

// ---- redirect URIs -------------------------------------------------------------

// privateSchemeRE is a private-use URI scheme a native app may redirect to
// (RFC 8252 section 7.1), e.g. "cursor" or "com.example.app".
var privateSchemeRE = regexp.MustCompile(`^[a-z][a-z0-9+.-]{1,62}$`)

// deniedSchemes can never be redirect targets: they run code, read local
// files or are plain-text network schemes.
var deniedSchemes = map[string]bool{
	"javascript": true, "data": true, "file": true, "vbscript": true, "about": true, "blob": true,
	"filesystem": true, "ftp": true, "ws": true, "wss": true, "mailto": true, "tel": true, "sms": true,
	"view-source": true, "chrome": true, "intent": true,
}

// checkRedirectURI validates one redirect URI: https anywhere, http only
// on the loopback interface (native apps, RFC 8252 section 7.3), or a
// private-use scheme; never a fragment or user info.
func checkRedirectURI(raw string) error {
	if raw == "" || len(raw) > maxRedirectURILen {
		return errors.New("redirect URI is empty or too long")
	}
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() {
		return fmt.Errorf("redirect URI %q is not an absolute URL", raw)
	}
	if u.Fragment != "" || strings.Contains(raw, "#") {
		return fmt.Errorf("redirect URI %q has a fragment", raw)
	}
	if u.User != nil {
		return fmt.Errorf("redirect URI %q has user info", raw)
	}
	for _, r := range raw {
		if r < 0x21 || r == 0x7f || r > unicode.MaxASCII {
			return fmt.Errorf("redirect URI %q has spaces or non-ASCII characters", raw)
		}
	}
	switch scheme := strings.ToLower(u.Scheme); {
	case scheme == "https":
		if u.Host == "" {
			return fmt.Errorf("redirect URI %q has no host", raw)
		}
	case scheme == "http":
		if !isLoopbackHost(u.Hostname()) {
			return fmt.Errorf("redirect URI %q uses http on a host other than the loopback interface; use https", raw)
		}
	case deniedSchemes[scheme] || !privateSchemeRE.MatchString(scheme):
		return fmt.Errorf("redirect URI %q uses a scheme toolyard does not redirect to", raw)
	}
	return nil
}

// isLoopbackHost: localhost or a loopback IP literal.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// redirectMatches reports whether given is one of registered: an exact
// match, or for an http loopback URI the same URI on another port (RFC
// 8252 section 7.3: native apps pick a free port at run time).
func redirectMatches(registered []string, given string) bool {
	for _, r := range registered {
		if r == given {
			return true
		}
		if loopbackSameButPort(r, given) {
			return true
		}
	}
	return false
}

func loopbackSameButPort(a, b string) bool {
	ua, err1 := url.Parse(a)
	ub, err2 := url.Parse(b)
	if err1 != nil || err2 != nil || ua.Scheme != "http" || ub.Scheme != "http" {
		return false
	}
	if !isLoopbackHost(ua.Hostname()) || !strings.EqualFold(ua.Hostname(), ub.Hostname()) {
		return false
	}
	return ua.EscapedPath() == ub.EscapedPath() && ua.RawQuery == ub.RawQuery && ub.User == nil && ub.Fragment == ""
}

// redirectHostAllowed applies the admin's redirect host allow list.
func (s *Service) redirectHostAllowed(raw string) bool {
	list := s.RedirectHosts()
	if len(list) == 0 {
		return true
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	scheme, host := strings.ToLower(u.Scheme), strings.ToLower(u.Hostname())
	for _, e := range list {
		switch {
		case strings.HasSuffix(e, ":"):
			if scheme+":" == e {
				return true
			}
		case strings.HasPrefix(e, "*."):
			if strings.HasSuffix(host, e[1:]) && host != e[2:] {
				return true
			}
		default:
			if host == e && (scheme == "https" || scheme == "http") {
				return true
			}
		}
	}
	return false
}

// RedirectHost is the part of a redirect URI the consent screen shows
// prominently: the host, or "scheme:" for a private-use scheme.
func RedirectHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		return u.Host
	}
	if u.Host != "" {
		return u.Scheme + "://" + u.Host
	}
	return u.Scheme + ":"
}

// CSPSource is the Content-Security-Policy source expression that matches
// a redirect URI, for the consent page's form-action: the origin for http
// and https, the scheme for a private-use scheme.
func CSPSource(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	switch scheme := strings.ToLower(u.Scheme); scheme {
	case "http", "https":
		return scheme + "://" + u.Host
	default:
		if privateSchemeRE.MatchString(scheme) {
			return scheme + ":"
		}
	}
	return ""
}

// cleanText strips control characters and caps a display string.
func cleanText(v string, max int) string {
	v = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, strings.TrimSpace(v))
	if r := []rune(v); len(r) > max {
		v = string(r[:max])
	}
	return strings.TrimSpace(v)
}

// httpsURLOrEmpty keeps an optional https URL of at most 500 characters.
func httpsURLOrEmpty(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || len(v) > 500 {
		return ""
	}
	u, err := url.Parse(v)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return ""
	}
	return u.String()
}

// ---- dynamic client registration (RFC 7591) ----------------------------------

type registrationRequest struct {
	RedirectURIs            []string `json:"redirect_uris"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
	ClientName              string   `json:"client_name"`
	ClientURI               string   `json:"client_uri"`
	LogoURI                 string   `json:"logo_uri"`
	Scope                   string   `json:"scope"`
	SoftwareID              string   `json:"software_id"`
	SoftwareVersion         string   `json:"software_version"`
}

// RegistrationResponse is the RFC 7591 client information response.
type RegistrationResponse struct {
	ClientID                string   `json:"client_id"`
	ClientIDIssuedAt        int64    `json:"client_id_issued_at"`
	ClientSecret            string   `json:"client_secret,omitempty"`
	ClientSecretExpiresAt   *int64   `json:"client_secret_expires_at,omitempty"`
	ClientName              string   `json:"client_name,omitempty"`
	ClientURI               string   `json:"client_uri,omitempty"`
	LogoURI                 string   `json:"logo_uri,omitempty"`
	RedirectURIs            []string `json:"redirect_uris"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	Scope                   string   `json:"scope,omitempty"`
}

func invalidRedirect(desc string) *Error {
	return oauthErr(http.StatusBadRequest, "invalid_redirect_uri", desc)
}

func invalidMetadata(desc string) *Error {
	return oauthErr(http.StatusBadRequest, "invalid_client_metadata", desc)
}

// validateClientMetadata checks the fields DCR and metadata documents
// share. It returns the auth method to store.
func (s *Service) validateClientMetadata(redirects []string, method string, grants, responses []string, allowSecret bool) (string, *Error) {
	if len(redirects) == 0 {
		return "", invalidRedirect("redirect_uris is required")
	}
	if len(redirects) > maxRedirectURIs {
		return "", invalidRedirect(fmt.Sprintf("at most %d redirect_uris", maxRedirectURIs))
	}
	for _, r := range redirects {
		if err := checkRedirectURI(r); err != nil {
			return "", invalidRedirect(err.Error())
		}
		if !s.redirectHostAllowed(r) {
			return "", invalidRedirect(fmt.Sprintf("redirect URI %q is not on this toolyard's list of allowed client redirect hosts", r))
		}
	}
	for _, g := range grants {
		if g != "authorization_code" && g != "refresh_token" {
			return "", invalidMetadata(fmt.Sprintf("grant type %q is not supported; use authorization_code and refresh_token", g))
		}
	}
	for _, rt := range responses {
		if rt != "code" {
			return "", invalidMetadata(fmt.Sprintf("response type %q is not supported; use code", rt))
		}
	}
	switch method {
	case AuthNone:
	case AuthSecretPost, AuthSecretBasic:
		if !allowSecret {
			return "", invalidMetadata("a client metadata document client cannot use a client secret; use token_endpoint_auth_method none")
		}
	case "private_key_jwt":
		return "", invalidMetadata("token_endpoint_auth_method private_key_jwt is not supported; use none")
	default:
		return "", invalidMetadata(fmt.Sprintf("token_endpoint_auth_method %q is not supported", method))
	}
	return method, nil
}

// Register handles one RFC 7591 registration. ip is the caller's address,
// already rate limited by the HTTP handler.
func (s *Service) Register(ctx context.Context, body []byte, ip string) (*RegistrationResponse, *Error) {
	var req registrationRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, invalidMetadata("the body must be a JSON object of client metadata")
	}
	method := strings.TrimSpace(req.TokenEndpointAuthMethod)
	if method == "" {
		// RFC 7591 section 2: the default is client_secret_basic.
		method = AuthSecretBasic
	}
	method, oerr := s.validateClientMetadata(req.RedirectURIs, method, req.GrantTypes, req.ResponseTypes, true)
	if oerr != nil {
		return nil, oerr
	}
	id, err := newSecret(clientPrefix)
	if err != nil {
		return nil, serverError()
	}
	id = id[:len(clientPrefix)+22] // 16 random bytes are plenty for an identifier
	var secret, secretHash string
	if method != AuthNone {
		if secret, err = newSecret(secretPrefix); err != nil {
			return nil, serverError()
		}
		secretHash = hashSecret(secret)
	}
	name := cleanText(req.ClientName, maxClientNameLen)
	clientURI, logo := httpsURLOrEmpty(req.ClientURI), httpsURLOrEmpty(req.LogoURI)
	redirects, _ := json.Marshal(req.RedirectURIs)
	meta, _ := json.Marshal(map[string]string{"software_id": cleanText(req.SoftwareID, 200), "software_version": cleanText(req.SoftwareVersion, 100), "scope": cleanText(req.Scope, 500)})
	now := s.now()
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO mcp_oauth_clients(client_id, kind, client_name, client_uri, logo_uri, redirect_uris, token_endpoint_auth_method,
             secret_hash, metadata, registered_ip, created_at, updated_at)
         VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, ClientKindDCR, name, nullStr(clientURI), nullStr(logo), string(redirects), method,
		nullStr(secretHash), string(meta), nullStr(ip), now.UnixMilli(), now.UnixMilli()); err != nil {
		return nil, serverError()
	}
	resp := &RegistrationResponse{
		ClientID: id, ClientIDIssuedAt: now.Unix(), ClientSecret: secret,
		ClientName: name, ClientURI: clientURI, LogoURI: logo, RedirectURIs: req.RedirectURIs,
		GrantTypes: []string{"authorization_code", "refresh_token"}, ResponseTypes: []string{"code"},
		TokenEndpointAuthMethod: method, Scope: grantedScope(req.Scope),
	}
	if secret != "" {
		var never int64
		resp.ClientSecretExpiresAt = &never
	}
	return resp, nil
}

// ---- lookup ------------------------------------------------------------------

const clientColumns = `client_id, kind, client_name, COALESCE(client_uri,''), COALESCE(logo_uri,''), redirect_uris,
	token_endpoint_auth_method, COALESCE(secret_hash,''), created_at, COALESCE(expires_at,0)`

func scanClient(row interface{ Scan(...any) error }) (*Client, error) {
	var c Client
	var redirects string
	if err := row.Scan(&c.ID, &c.Kind, &c.Name, &c.URI, &c.LogoURI, &redirects, &c.AuthMethod, &c.secretHash, &c.CreatedAt, &c.expiresAt); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(redirects), &c.RedirectURIs); err != nil {
		return nil, err
	}
	return &c, nil
}

// Client returns the client clientID names: a registered client, or the
// one its metadata document describes (fetched when not cached or when the
// cached copy expired). An unknown client is invalid_client.
func (s *Service) Client(ctx context.Context, clientID string) (*Client, *Error) {
	if clientID == "" || len(clientID) > maxRedirectURILen {
		return nil, invalidClient("unknown client_id")
	}
	if strings.HasPrefix(clientID, "https://") {
		return s.cimdClient(ctx, clientID)
	}
	c, err := scanClient(s.db.QueryRowContext(ctx, `SELECT `+clientColumns+` FROM mcp_oauth_clients WHERE client_id = ? AND kind = ?`, clientID, ClientKindDCR))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, invalidClient("unknown client_id; register the client first")
	}
	if err != nil {
		return nil, serverError()
	}
	return c, nil
}

// checkSecret reports whether secret is c's client secret.
func (c *Client) checkSecret(secret string) bool {
	if c.secretHash == "" || secret == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(c.secretHash), []byte(hashSecret(secret))) == 1
}

// ---- client ID metadata documents --------------------------------------------

// cimdDocument is the subset of a client metadata document we read.
type cimdDocument struct {
	ClientID                string          `json:"client_id"`
	ClientName              string          `json:"client_name"`
	ClientURI               string          `json:"client_uri"`
	LogoURI                 string          `json:"logo_uri"`
	RedirectURIs            []string        `json:"redirect_uris"`
	TokenEndpointAuthMethod string          `json:"token_endpoint_auth_method"`
	GrantTypes              []string        `json:"grant_types"`
	ResponseTypes           []string        `json:"response_types"`
	ClientSecret            json.RawMessage `json:"client_secret"`
	ClientSecretExpiresAt   json.RawMessage `json:"client_secret_expires_at"`
}

// checkCIMDURL validates a metadata document client_id: https, a host, a
// path other than "/", no fragment, no user info, no dot segments.
func checkCIMDURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" || strings.Contains(raw, "#") {
		return errors.New("client_id URL must be https with a host and no user info or fragment")
	}
	if u.Path == "" || u.Path == "/" {
		return errors.New("client_id URL must have a path")
	}
	for _, seg := range strings.Split(u.Path, "/") {
		if seg == "." || seg == ".." {
			return errors.New("client_id URL must not have dot segments")
		}
	}
	return nil
}

func (s *Service) cimdClient(ctx context.Context, clientID string) (*Client, *Error) {
	if err := checkCIMDURL(clientID); err != nil {
		return nil, invalidClient(err.Error())
	}
	cached, err := scanClient(s.db.QueryRowContext(ctx, `SELECT `+clientColumns+` FROM mcp_oauth_clients WHERE client_id = ? AND kind = ?`, clientID, ClientKindCIMD))
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, serverError()
	}
	if cached != nil && cached.expiresAt > s.now().UnixMilli() {
		return cached, nil
	}
	doc, ttl, ferr := s.fetchMetadataDocument(ctx, clientID)
	if ferr != nil {
		return nil, invalidClient("could not read the client metadata document: " + ferr.Error())
	}
	if doc.ClientID != clientID {
		return nil, invalidClient("the client metadata document's client_id does not equal its URL")
	}
	if len(doc.ClientSecret) > 0 || len(doc.ClientSecretExpiresAt) > 0 {
		return nil, invalidClient("a client metadata document must not carry a client secret")
	}
	method := strings.TrimSpace(doc.TokenEndpointAuthMethod)
	if method == "" {
		method = AuthNone
	}
	if _, oerr := s.validateClientMetadata(doc.RedirectURIs, method, doc.GrantTypes, doc.ResponseTypes, false); oerr != nil {
		return nil, invalidClient("client metadata document: " + oerr.Description)
	}
	c := &Client{
		ID: clientID, Kind: ClientKindCIMD, Name: cleanText(doc.ClientName, maxClientNameLen),
		URI: httpsURLOrEmpty(doc.ClientURI), LogoURI: httpsURLOrEmpty(doc.LogoURI),
		RedirectURIs: doc.RedirectURIs, AuthMethod: AuthNone,
	}
	now := s.now()
	c.CreatedAt, c.expiresAt = now.UnixMilli(), now.Add(ttl).UnixMilli()
	redirects, _ := json.Marshal(c.RedirectURIs)
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO mcp_oauth_clients(client_id, kind, client_name, client_uri, logo_uri, redirect_uris, token_endpoint_auth_method,
             created_at, updated_at, expires_at)
         VALUES(?,?,?,?,?,?,?,?,?,?)
         ON CONFLICT(client_id) DO UPDATE SET client_name = excluded.client_name, client_uri = excluded.client_uri,
             logo_uri = excluded.logo_uri, redirect_uris = excluded.redirect_uris,
             token_endpoint_auth_method = excluded.token_endpoint_auth_method,
             updated_at = excluded.updated_at, expires_at = excluded.expires_at`,
		c.ID, ClientKindCIMD, c.Name, nullStr(c.URI), nullStr(c.LogoURI), string(redirects), c.AuthMethod,
		c.CreatedAt, c.CreatedAt, c.expiresAt); err != nil {
		return nil, serverError()
	}
	if cached != nil {
		c.CreatedAt = cached.CreatedAt
	}
	return c, nil
}

// privateIP reports an address a metadata document may not live on.
func privateIP(ip net.IP) bool {
	return ip.IsPrivate() || ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || !ip.IsGlobalUnicast()
}

// fetchMetadataDocument GETs a client metadata document without leaving
// the public internet: every address of the host must be public (unless
// AllowPrivateCIMD), the connection goes to the address that was checked
// (no second DNS answer can move it), TLS still verifies the host name,
// redirects are not followed, and the body and time are bounded.
func (s *Service) fetchMetadataDocument(ctx context.Context, raw string) (*cimdDocument, time.Duration, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, 0, err
	}
	host, port := u.Hostname(), u.Port()
	if port == "" {
		port = "443"
	}
	ctx, cancel := context.WithTimeout(ctx, cimdFetchTimeout)
	defer cancel()
	var ips []net.IPAddr
	if ip := net.ParseIP(host); ip != nil {
		ips = []net.IPAddr{{IP: ip}}
	} else if ips, err = s.resolver(ctx, host); err != nil || len(ips) == 0 {
		return nil, 0, errors.New("its host does not resolve")
	}
	for _, a := range ips {
		if privateIP(a.IP) && !s.allowPrivateCIMD {
			return nil, 0, errors.New("its host is not on the public internet")
		}
	}
	pinned := net.JoinHostPort(ips[0].IP.String(), port)
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	transport := &http.Transport{
		Proxy: nil, // the checked address is the trust decision; a proxy would make its own
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "tcp", pinned)
		},
		TLSClientConfig:        &tls.Config{ServerName: host, RootCAs: s.cimdRoots, MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:    5 * time.Second,
		ResponseHeaderTimeout:  5 * time.Second,
		MaxResponseHeaderBytes: 16 << 10,
		DisableKeepAlives:      true,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "toolyard-oauth/1")
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, errors.New("the request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("it answered HTTP %d (redirects are not followed)", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxCIMDBody+1))
	if err != nil {
		return nil, 0, errors.New("reading it failed")
	}
	if len(body) > maxCIMDBody {
		return nil, 0, fmt.Errorf("it is larger than %d bytes", maxCIMDBody)
	}
	var doc cimdDocument
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, 0, errors.New("it is not a JSON object")
	}
	return &doc, cacheTTL(resp.Header.Get("Cache-Control")), nil
}

// cacheTTL reads max-age from a Cache-Control header, bounded to
// [cimdMinTTL, cimdMaxTTL]; no-store or no max-age gives the default.
func cacheTTL(header string) time.Duration {
	ttl := cimdDefaultTTL
	for _, d := range strings.Split(header, ",") {
		d = strings.ToLower(strings.TrimSpace(d))
		if v, ok := strings.CutPrefix(d, "max-age="); ok {
			if n, err := strconv.Atoi(strings.Trim(v, `"`)); err == nil && n >= 0 {
				ttl = time.Duration(n) * time.Second
			}
		}
	}
	if ttl < cimdMinTTL {
		ttl = cimdMinTTL
	}
	if ttl > cimdMaxTTL {
		ttl = cimdMaxTTL
	}
	return ttl
}
