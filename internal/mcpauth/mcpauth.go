// Package mcpauth makes toolyard an OAuth 2.1 authorization server for its
// own /mcp endpoint, so MCP clients (ChatGPT, Claude.ai, Claude Code,
// Codex, Cursor) can add toolyard by URL and sign the person in through
// their browser instead of pasting an agent token.
//
// What it implements (docs/design/v0.1.md, section C):
//
//   - RFC 9728 protected-resource metadata and RFC 8414 authorization
//     server metadata;
//   - RFC 7591 dynamic client registration, and client ID metadata
//     documents (an https client_id that names the client's own JSON
//     document, fetched without leaving the public internet);
//   - the authorization code grant with PKCE S256 required and RFC 8707
//     resource indicators (every token is for this toolyard's /mcp only);
//   - refresh tokens that rotate on every use: a refresh token used twice
//     revokes the whole grant; RFC 7009 revocation.
//
// Each grant is one person's authorization of one client and runs as an
// agent that person owns (Agent.Kind "oauth_client"), named by the
// nickname they chose on the consent screen and limited by the tool scope
// they chose there (access.AgentScope). Approval behaviour stays toolyard's
// normal policy. Every piece of state (clients, requests, codes, tokens,
// rate limits) lives in the database, so several replicas can share one
// Postgres database; codes and refresh tokens are claimed with
// UPDATE ... WHERE used_at IS NULL RETURNING.
//
// The consent screen needs the dashboard's session cookie, so its HTTP
// handlers live in internal/api (oauth_server_routes.go); this package
// serves the endpoints that need no session (http.go).
package mcpauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/access"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// AgentKindOAuthClient is the agent kind every grant's agent reports
// (identity.Agent.Kind, actor.Raiser.AgentKind and audit_events.agent_kind).
// The agents row itself keeps the stored kind "agent": its CHECK constraint
// predates OAuth, and the grant row is what marks the agent as a client.
const AgentKindOAuthClient = "oauth_client"

// Paths this package and internal/api serve.
const (
	ProtectedResourcePath    = "/.well-known/oauth-protected-resource"
	AuthorizationServerPath  = "/.well-known/oauth-authorization-server"
	AuthorizePath            = "/oauth/authorize"
	ConsentPath              = "/oauth/consent"
	TokenPath                = "/oauth/token"
	RegisterPath             = "/oauth/register"
	RevokePath               = "/oauth/revoke"
	resourcePath             = "/mcp"
	protectedResourceMCPPath = ProtectedResourcePath + resourcePath
)

// ProtectedResourcePaths are the paths that serve the RFC 9728 document:
// the root form and the form with the resource's path appended.
var ProtectedResourcePaths = []string{ProtectedResourcePath, protectedResourceMCPPath}

// Settings keys (system_settings). An admin changes them with PATCH
// /v1/settings or on the Connected apps page.
const (
	// SettingEnabled turns OAuth for MCP clients on or off. Default on
	// whenever -public-url is set; without a public URL there is no issuer
	// and it is always off.
	SettingEnabled = "mcp_oauth_enabled"
	// SettingRedirectHosts is an optional allow list of client redirect
	// hosts (comma or space separated): "claude.ai", "*.example.com",
	// "localhost", or "cursor:" for a private-use URI scheme. Empty allows
	// every valid redirect URI.
	SettingRedirectHosts = "mcp_oauth_redirect_hosts"
	// SettingAccessTTLMinutes is the access token lifetime (5-1440, default 60).
	SettingAccessTTLMinutes = "mcp_oauth_access_ttl_minutes"
	// SettingRefreshTTLDays is the refresh token lifetime, renewed on every
	// refresh (1-365, default 30).
	SettingRefreshTTLDays = "mcp_oauth_refresh_ttl_days"
	// SettingMaxSessionDays is the absolute limit from consent: no token
	// of a grant outlives it, however often it is refreshed (1-365,
	// default 90). The person then signs in again.
	SettingMaxSessionDays = "mcp_oauth_max_session_days"
	// SettingCustomSchemes lists the private-use redirect schemes allowed
	// without a reverse-DNS name (comma separated). Default:
	// DefaultCustomSchemes.
	SettingCustomSchemes = "mcp_oauth_custom_schemes"
)

// DefaultCustomSchemes are the redirect schemes of well-known native MCP
// clients that do not use a reverse-DNS scheme (RFC 8252 section 7.1).
const DefaultCustomSchemes = "cursor,vscode,vscode-insiders,windsurf,zed"

// Lifetimes and limits.
const (
	DefaultAccessTTL  = time.Hour
	DefaultRefreshTTL = 30 * 24 * time.Hour
	DefaultMaxSession = 90 * 24 * time.Hour
	// refreshReuseGrace: a refresh token presented again this soon after
	// its use, while its successor is still unused, is refused without
	// revoking the grant (a client retry, or two of its requests racing).
	refreshReuseGrace = 10 * time.Second
	// CodeTTL: an authorization code must be exchanged within a minute.
	CodeTTL = 60 * time.Second
	// RequestTTL: the person has ten minutes to sign in and answer.
	RequestTTL = 10 * time.Minute
	// lastUsedEvery throttles the last-used write on the hot /mcp path.
	lastUsedEvery = time.Minute
)

// Scopes this server knows. Unknown scopes a client asks for are ignored,
// not refused: tool access is chosen on the consent screen, not by scope.
const (
	ScopeMCP           = "mcp"
	ScopeOfflineAccess = "offline_access"
)

// Token and identifier prefixes.
const (
	accessPrefix  = "tyat_"
	refreshPrefix = "tyrt_"
	codePrefix    = "tyoc_"
	secretPrefix  = "tycs_"
	clientPrefix  = "mcpc_"
)

// SettingsReader is what the service reads from settings.Service.
type SettingsReader interface {
	GetBoolDefault(key string, def bool) bool
	GetString(key, fallback string) string
	GetInt(key string, fallback int) int
}

// Options configures New.
type Options struct {
	DB *store.DB
	// PublicURL is the toolyard origin clients reach (-public-url): the
	// issuer, and with /mcp the protected resource. Empty turns the
	// service off.
	PublicURL string
	// Settings, when set, supplies the instance switch, the redirect host
	// allow list and the token lifetimes. nil uses the defaults.
	Settings SettingsReader
	// Access, when set, has its cached agent limits dropped after a grant's
	// scope changes. nil leaves them to expire (5 seconds).
	Access *access.Service
	// Now is the clock (tests).
	Now func() time.Time
	// Resolver looks up a metadata document's host. Default: the system
	// resolver.
	Resolver func(ctx context.Context, host string) ([]net.IPAddr, error)
	// AllowPrivateCIMD lets client metadata documents live on loopback and
	// private addresses and on any port. Tests only: in production a
	// document must be on the public internet, on port 443.
	AllowPrivateCIMD bool
	// CIMDRootCAs replaces the system roots for document fetches. Tests only.
	CIMDRootCAs *x509.CertPool
	// OnRevoke, when set, runs after a grant is revoked (by its owner, by
	// its client, or because a token was used twice) with the grant's
	// agent, after the agent was disabled: main.go cancels the agent's
	// pending approvals and Inbox requests there.
	OnRevoke func(ctx context.Context, agentID string)
}

// Service is the authorization server.
type Service struct {
	db               *store.DB
	publicURL        string
	settings         SettingsReader
	access           *access.Service
	now              func() time.Time
	resolver         func(ctx context.Context, host string) ([]net.IPAddr, error)
	allowPrivateCIMD bool
	cimdRoots        *x509.CertPool
	onRevoke         func(ctx context.Context, agentID string)
	mem              memLimiter
	flightMu         sync.Mutex
	flights          map[string]*flight
}

// New returns the service. It starts nothing; call Run for the sweeper.
func New(o Options) *Service {
	s := &Service{
		db:               o.DB,
		publicURL:        strings.TrimRight(strings.TrimSpace(o.PublicURL), "/"),
		settings:         o.Settings,
		access:           o.Access,
		now:              o.Now,
		resolver:         o.Resolver,
		allowPrivateCIMD: o.AllowPrivateCIMD,
		cimdRoots:        o.CIMDRootCAs,
		onRevoke:         o.OnRevoke,
		flights:          map[string]*flight{},
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.resolver == nil {
		s.resolver = net.DefaultResolver.LookupIPAddr
	}
	return s
}

// Enabled reports whether OAuth for MCP clients is on: a public URL is
// set and the instance switch is not off.
func (s *Service) Enabled() bool {
	if s == nil || s.publicURL == "" {
		return false
	}
	if s.settings == nil {
		return true
	}
	return s.settings.GetBoolDefault(SettingEnabled, true)
}

// Issuer is the authorization server's issuer identifier.
func (s *Service) Issuer() string { return s.publicURL }

// Resource is the protected resource every token is for: <public-url>/mcp.
func (s *Service) Resource() string { return s.publicURL + resourcePath }

// ResourceMetadataURL is the RFC 9728 document a 401 from /mcp points at,
// or "" while the service is off.
func (s *Service) ResourceMetadataURL() string {
	if !s.Enabled() {
		return ""
	}
	return s.publicURL + ProtectedResourcePath
}

// AccessTTL is the configured access token lifetime.
func (s *Service) AccessTTL() time.Duration {
	if s.settings == nil {
		return DefaultAccessTTL
	}
	return time.Duration(clamp(s.settings.GetInt(SettingAccessTTLMinutes, 60), 5, 1440)) * time.Minute
}

// RefreshTTL is the configured refresh token lifetime.
func (s *Service) RefreshTTL() time.Duration {
	if s.settings == nil {
		return DefaultRefreshTTL
	}
	return time.Duration(clamp(s.settings.GetInt(SettingRefreshTTLDays, 30), 1, 365)) * 24 * time.Hour
}

// MaxSession is the configured absolute session limit.
func (s *Service) MaxSession() time.Duration {
	if s.settings == nil {
		return DefaultMaxSession
	}
	return time.Duration(clamp(s.settings.GetInt(SettingMaxSessionDays, 90), 1, 365)) * 24 * time.Hour
}

// CustomSchemes is the set of private-use redirect schemes allowed without
// a reverse-DNS name.
func (s *Service) CustomSchemes() map[string]bool {
	raw := DefaultCustomSchemes
	if s.settings != nil {
		raw = s.settings.GetString(SettingCustomSchemes, DefaultCustomSchemes)
	}
	out := map[string]bool{}
	for _, f := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' || r == ';' }) {
		if f = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(f), ":")); f != "" {
			out[f] = true
		}
	}
	return out
}

// RedirectHosts is the configured redirect host allow list (lower case),
// empty when every valid redirect URI is allowed.
func (s *Service) RedirectHosts() []string {
	if s.settings == nil {
		return nil
	}
	raw := s.settings.GetString(SettingRedirectHosts, "")
	var out []string
	for _, f := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' || r == ';' }) {
		if f = strings.ToLower(strings.TrimSpace(f)); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// IsAccessToken reports whether token has the form of an access token this
// server issues, so the /mcp guard knows which check to run.
func IsAccessToken(token string) bool { return strings.HasPrefix(token, accessPrefix) }

// ---- errors ------------------------------------------------------------------

// Error is an OAuth error response (RFC 6749 section 5.2 and friends).
type Error struct {
	Code        string `json:"error"`
	Description string `json:"error_description,omitempty"`
	Status      int    `json:"-"`
}

func (e *Error) Error() string {
	if e.Description == "" {
		return e.Code
	}
	return e.Code + ": " + e.Description
}

func oauthErr(status int, code, desc string) *Error {
	return &Error{Code: code, Description: desc, Status: status}
}

func invalidRequest(desc string) *Error {
	return oauthErr(http.StatusBadRequest, "invalid_request", desc)
}
func invalidGrant(desc string) *Error { return oauthErr(http.StatusBadRequest, "invalid_grant", desc) }
func invalidClient(desc string) *Error {
	return oauthErr(http.StatusUnauthorized, "invalid_client", desc)
}
func serverError() *Error {
	return oauthErr(http.StatusInternalServerError, "server_error", "toolyard could not complete the request; try again")
}

// Errors the consent flow returns.
var (
	// ErrRequestNotFound: the authorization request is unknown, expired or
	// already answered.
	ErrRequestNotFound = errors.New("mcpauth: authorization request expired or already answered")
	// ErrInvalidNickname: the nickname is empty or has characters an agent
	// name may not have.
	ErrInvalidNickname = errors.New("mcpauth: nickname must be 1-64 characters of letters, digits, spaces and . _ : ( ) -")
	// ErrAppNotFound: no such connected app.
	ErrAppNotFound = errors.New("mcpauth: connected app not found")
)

// ---- helpers -----------------------------------------------------------------

// newSecret returns prefix + 32 random bytes in base64url.
func newSecret(prefix string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// hashSecret is the stored form of a code, token or client secret.
func hashSecret(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])
}

// pkceS256 is the S256 code challenge of verifier (RFC 7636 section 4.2).
func pkceS256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// rateKey is the rate-limit key for a client address: the address, an
// IPv6 address's /64 (one host usually owns the whole /64), or "unknown"
// when the address is missing or unreadable.
func rateKey(ip string) string {
	addr, err := netip.ParseAddr(strings.TrimSpace(ip))
	if err != nil {
		return "unknown"
	}
	addr = addr.Unmap().WithZone("")
	if addr.Is6() {
		p, err := addr.Prefix(64)
		if err != nil {
			return "unknown"
		}
		return p.String()
	}
	return addr.String()
}

// netKey is the network of a client address for the second rate-limit
// tier: an IPv4 /24 or an IPv6 /48; "" when the address is unknown (the
// strict "unknown" bucket already covers it).
func netKey(ip string) string {
	addr, err := netip.ParseAddr(strings.TrimSpace(ip))
	if err != nil {
		return ""
	}
	addr = addr.Unmap().WithZone("")
	bits := 24
	if addr.Is6() {
		bits = 48
	}
	p, err := addr.Prefix(bits)
	if err != nil {
		return ""
	}
	return p.String()
}

// limitFor is max for a known address and a strict quarter of it for the
// shared "unknown" bucket.
func limitFor(key string, max int) int {
	if key == "unknown" {
		if max /= 4; max < 1 {
			max = 1
		}
	}
	return max
}

func nullStr(v string) any {
	if v == "" {
		return nil
	}
	return v
}

// grantedScope is the scope a client gets for what it asked: always "mcp",
// plus "offline_access" when asked. Anything else is ignored.
func grantedScope(requested string) string {
	out := []string{ScopeMCP}
	for _, sc := range strings.Fields(requested) {
		if sc == ScopeOfflineAccess {
			out = append(out, ScopeOfflineAccess)
			break
		}
	}
	return strings.Join(out, " ")
}

// scopeSubset reports whether every scope in want is in have.
func scopeSubset(want, have string) bool {
	set := map[string]bool{}
	for _, sc := range strings.Fields(have) {
		set[sc] = true
	}
	for _, sc := range strings.Fields(want) {
		if !set[sc] {
			return false
		}
	}
	return true
}

// sameResource compares a resource indicator with ours: scheme and host
// case-insensitively, and a trailing slash ignored.
func (s *Service) sameResource(given string) bool {
	norm := func(v string) string {
		v = strings.TrimRight(strings.TrimSpace(v), "/")
		if i := strings.Index(v, "://"); i > 0 {
			rest := v[i+3:]
			host, path, _ := strings.Cut(rest, "/")
			v = strings.ToLower(v[:i]) + "://" + strings.ToLower(host)
			if path != "" {
				v += "/" + path
			}
		}
		return v
	}
	return given != "" && norm(given) == norm(s.Resource())
}
