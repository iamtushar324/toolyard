// Package upstreams persists MCP-server configurations and reconciles them
// with the running gateway: connect-on-add, disconnect-on-remove, retry on
// reconnect, all from the dashboard.
package upstreams

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/gateway"
	"github.com/tusharbhardwaj/toolyard/internal/secrets"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

var (
	ErrInvalid     = errors.New("invalid server config")
	ErrAlreadyHere = errors.New("server name already exists")
	ErrNotFound    = errors.New("server not found")
	ErrReserved    = errors.New("server name is reserved")
)

// IdentityForwarding is a server's per-person identity setting.
type IdentityForwarding struct {
	// Header carries the caller's raw identity key on tools/call only,
	// e.g. "x-bk-bifrost-vk" for BkCoreServices and BkDocsServices.
	Header string `json:"header"`
	// Register marks the server where toolyard registers each key's
	// fingerprint (it exposes upsert-/delete-bifrost-virtual-key-actor).
	Register bool `json:"register,omitempty"`
}

// Server is one persisted upstream config and its current connection status.
type Server struct {
	Name      string            `json:"name"`
	Transport string            `json:"transport"`
	Command   string            `json:"command,omitempty"`
	Args      []string          `json:"args,omitempty"`
	URL       string            `json:"url,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	// Headers are static HTTP headers for http transports, persisted in
	// headers_json. Values may be secret:// refs (resolved at dial time)
	// and are merged with OAuth-provided headers — OAuth wins on a clash
	// (e.g. Authorization).
	Headers map[string]string `json:"headers,omitempty"`
	// Identity, when set on an http server, forwards the caller's
	// per-person identity key on every tool call. Persisted in
	// identity_json.
	Identity *IdentityForwarding `json:"identity,omitempty"`
	// AuthMode says who signs in to the server: AuthShared (one account for
	// everyone, the default) or AuthPerUser (each person connects their own
	// account from My connections; an agent's calls use its owner's token
	// and never a shared one). http servers only.
	AuthMode string `json:"auth_mode"`
	// AssertionProfile selects a service-owned assertion credential profile.
	// It uses per_user authorization without an OAuth token or caller SID.
	AssertionProfile string `json:"assertion_profile,omitempty"`
	Enabled          bool   `json:"enabled"`
	LastStatus       string `json:"last_status,omitempty"`
	LastError        string `json:"last_error,omitempty"`
	ToolCount        int    `json:"tool_count"`
	CreatedAt        int64  `json:"created_at"`
	UpdatedAt        int64  `json:"updated_at"`
	// EnvPlaintextKeys is populated only by Masked(): the env keys whose
	// values were masked (i.e. plaintext, not secret:// refs) so the UI can
	// offer a "convert to secret" action. Never persisted.
	EnvPlaintextKeys []string `json:"env_plaintext_keys,omitempty"`
	// OAuthReset is set only on the Server returned by Update, when the
	// edit moved the url to another origin and the server's OAuth client
	// and tokens were dropped as a result: the operator has to authorise
	// it again. Never persisted.
	OAuthReset bool `json:"oauth_reset,omitempty"`
}

// AuthMode values.
const (
	AuthShared  = "shared"
	AuthPerUser = "per_user"
)

// Policy gates which upstream configurations are admissible. Used to
// implement -no-stdio-upstreams and -upstream-env-denylist on the public
// deployment so a compromised dashboard session can't spawn arbitrary
// subprocesses or hijack the loader via LD_PRELOAD.
type Policy struct {
	AllowStdio  bool
	EnvDenylist []string // case-insensitive prefix or exact match
}

// HeaderProvider lets the OAuth service hand the upstream package a
// closure that returns "Authorization: Bearer <live-token>" headers per
// request. Decoupled so we don't import oauth here.
type HeaderProvider interface {
	HeaderFunc(upstream string) func(ctx context.Context) map[string]string
	// HasClient reports whether the upstream holds OAuth state (a client
	// registration, or the placeholder client a stored PAT gets).
	HasClient(ctx context.Context, upstream string) (bool, error)
	// Disconnect revokes (best-effort) and deletes the upstream's OAuth
	// client and tokens. Update calls it before moving a server to another
	// origin so a bearer minted for one host is never sent to another.
	Disconnect(ctx context.Context, upstream string) error
}

// PerUserAuth is the per-user token store a per_user server needs: who has
// connected it (gateway.PerUserAuth) and each person's bearer header.
// *oauth.Service satisfies it.
type PerUserAuth interface {
	gateway.PerUserAuth
	// UserHeaderFunc returns the per-request header function for upstream,
	// given the user whose connection the request belongs to.
	UserHeaderFunc(upstream string) func(ctx context.Context, userID string) map[string]string
}

// SecretResolver is the secrets-broker dependency. Decoupled via interface so
// internal/upstreams doesn't import internal/secrets. Implemented by
// *internal/secrets.Service.
type SecretResolver interface {
	// ResolveMap returns a copy of in with every secret:// ref replaced by
	// its decrypted value; non-refs pass through. Errors name the ref.
	ResolveMap(ctx context.Context, in map[string]string) (map[string]string, error)
	// Exists reports whether a stored secret with this name is present.
	Exists(ctx context.Context, name string) (bool, error)
}

// Service owns the upstream_servers table and keeps the live gateway in sync.
type Service struct {
	db         *store.DB
	gw         *gateway.Gateway
	policy     Policy
	auth       HeaderProvider // optional
	perUser    PerUserAuth    // optional; without it per_user servers cannot connect
	secrets    SecretResolver // optional
	assertions func(context.Context, Server) (gateway.TrustedUpstream, error)

	mu sync.Mutex // serializes connect/disconnect side-effects
}

func New(db *store.DB, gw *gateway.Gateway) *Service {
	return &Service{db: db, gw: gw, policy: Policy{AllowStdio: true}}
}

// SetPolicy installs admission rules. Idempotent.
func (s *Service) SetPolicy(p Policy) { s.policy = p }

// SetAuth installs the OAuth header provider. Calling this with nil
// disables the wiring (useful for tests).
func (s *Service) SetAuth(a HeaderProvider) { s.auth = a }

// SetPerUserAuth installs the per-user token store. nil leaves per_user
// servers unable to connect ("per-user sign-in is not wired").
func (s *Service) SetPerUserAuth(a PerUserAuth) { s.perUser = a }

// SetSecrets installs the secrets broker so secret:// refs in env/headers
// resolve at dial time. Nil disables the wiring (refs would then fail the
// dial as missing secrets).
func (s *Service) SetSecrets(r SecretResolver) { s.secrets = r }

// SetAssertionProvider installs the private service-owned profile resolver.
// A nil provider refuses assertion activation without affecting other servers.
func (s *Service) SetAssertionProvider(p func(context.Context, Server) (gateway.TrustedUpstream, error)) {
	s.assertions = p
}

// envDenied returns the first env key in the supplied map that the policy
// forbids. Empty string means clean.
func (p Policy) envDenied(env map[string]string) string {
	if len(env) == 0 || len(p.EnvDenylist) == 0 {
		return ""
	}
	for k := range env {
		ku := strings.ToUpper(k)
		for _, deny := range p.EnvDenylist {
			du := strings.ToUpper(deny)
			if ku == du || strings.HasPrefix(ku, du) {
				return k
			}
		}
	}
	return ""
}

func (s *Service) toCfg(srv Server) gateway.UpstreamConfig {
	cfg := gateway.UpstreamConfig{
		Name:      srv.Name,
		Transport: srv.Transport,
		Command:   srv.Command,
		Args:      srv.Args,
		URL:       srv.URL,
		Env:       srv.Env,
	}
	if srv.AssertionProfile != "" {
		// The dedicated provider replaces every ordinary credential source.
		// connect attaches Trusted; never attach OAuth/static header closures.
		cfg.PerUser = true
		return cfg
	}
	isHTTP := cfg.Transport == "github" || cfg.Transport == "http" || cfg.Transport == "streamable-http" || cfg.Transport == ""

	// stdio: resolve secret:// refs in Env at dial time via EnvFunc. The
	// closure captures a copy of srv.Env so a later edit doesn't race.
	if cfg.Transport == "stdio" && s.secrets != nil {
		envCopy := copyMap(srv.Env)
		resolver := s.secrets
		cfg.EnvFunc = func(ctx context.Context) (map[string]string, error) {
			return resolver.ResolveMap(ctx, envCopy)
		}
	}

	// http: compose static (secret-resolved) headers with the OAuth bearer
	// header, then the caller's identity key. OAuth wins on a clash (e.g.
	// Authorization) so a stored bearer always takes precedence over a
	// hand-set header; the identity header wins over both.
	//
	// On a per_user server the bearer is the one of the user the gateway
	// names on the request context (its connection's owner): the shared
	// token is never consulted, so such a server can never act as the
	// shared account by accident.
	if isHTTP {
		name := srv.Name
		staticHeaders := copyMap(srv.Headers)
		resolver := s.secrets
		var oauthFn func(ctx context.Context) map[string]string
		perUser := srv.AuthMode == AuthPerUser
		switch {
		case perUser:
			cfg.PerUser = true
			if s.perUser != nil {
				cfg.PerUserAuth = s.perUser
				userFn := s.perUser.UserHeaderFunc(srv.Name)
				oauthFn = func(ctx context.Context) map[string]string {
					uid, _ := gateway.UpstreamUser(ctx)
					return userFn(ctx, uid)
				}
			}
		case s.auth != nil:
			oauthFn = s.auth.HeaderFunc(srv.Name)
		}
		var identityHeader string
		if srv.Identity != nil {
			identityHeader = srv.Identity.Header
		}
		cfg.IdentityHeader = identityHeader
		if len(staticHeaders) > 0 || oauthFn != nil || identityHeader != "" {
			cfg.HeaderFunc = func(ctx context.Context) map[string]string {
				out := map[string]string{}
				for k, v := range staticHeaders {
					if !secrets.IsRef(v) {
						out[k] = v
						continue
					}
					// A ref that doesn't resolve is dropped for this
					// request, never sent as the literal "secret://NAME".
					resolved, err := resolveHeaderRef(ctx, resolver, k, v)
					if err != nil {
						log.Printf("upstream %q: header %q dropped: %v", name, k, err)
						continue
					}
					out[k] = resolved
				}
				if perUser {
					// Nothing static may stand in for the person. validate
					// refuses a static Authorization on a per_user server;
					// this keeps it out even on a row saved before that
					// rule, in any letter case (Header.Set would merge
					// them, so map order must not decide). With no bearer
					// for the person the request carries no Authorization
					// at all, and the gateway refuses the call before
					// sending it (bearerOnFile).
					for k := range out {
						if strings.EqualFold(k, "Authorization") {
							delete(out, k)
						}
					}
				}
				if oauthFn != nil {
					for k, v := range oauthFn(ctx) {
						out[k] = v // OAuth wins
					}
				}
				// The identity header carries the caller's key or nothing
				// at all. Whatever static config or OAuth put under that
				// name (in any letter case) is discarded, so a fixed
				// identity can't be pinned on a forwarding server; a
				// request without a key (initialize, tools/list, a
				// reconnect) goes out under the shared credentials only.
				if identityHeader != "" {
					for k := range out {
						if strings.EqualFold(k, identityHeader) {
							delete(out, k)
						}
					}
					if key, ok := gateway.ForwardedKey(ctx); ok {
						out[identityHeader] = key
					}
				}
				return out
			}
		}
	}
	return cfg
}

// resolveHeaderRef resolves one secret:// header value through the broker.
// It fails when no broker is wired or the secret can't be produced; the
// error names the ref, never a value.
func resolveHeaderRef(ctx context.Context, r SecretResolver, key, ref string) (string, error) {
	if r == nil {
		return "", fmt.Errorf("%s: secrets broker not wired", ref)
	}
	out, err := r.ResolveMap(ctx, map[string]string{key: ref})
	if err != nil {
		return "", err
	}
	return out[key], nil
}

func copyMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func validate(srv Server) error {
	if strings.TrimSpace(srv.Name) == "" {
		return fmt.Errorf("%w: name required", ErrInvalid)
	}
	if strings.ContainsAny(srv.Name, " \t\n.") {
		return fmt.Errorf("%w: name must not contain spaces or dots", ErrInvalid)
	}
	// Names the gateway owns: its synthetic upstreams and built-in groups
	// (a server by one of these names would share their prefix and access
	// group) and toolyard, the code-mode server for every internal tool.
	// Built-ins (mempalace, notes, skills) are registered by startup, not
	// from the dashboard.
	switch srv.Name {
	case "builtin", "fixture", "memory", "tools", "mempalace", "notes", "skills",
		"inbox", "session", "lake", "events", "policies", "servers", "audit", "access", "toolyard", "connections":
		return ErrReserved
	}
	switch srv.Transport {
	case "github":
		if srv.URL != "https://api.github.com" || srv.AuthMode != AuthPerUser || len(srv.Headers) != 0 || len(srv.Env) != 0 || srv.Command != "" || len(srv.Args) != 0 || srv.Identity != nil {
			return fmt.Errorf("%w: GitHub requires per-user OAuth, the fixed GitHub API URL, and no shared credentials", ErrInvalid)
		}
	case "stdio":
		if srv.Command == "" {
			return fmt.Errorf("%w: stdio requires command", ErrInvalid)
		}
	case "http", "streamable-http", "":
		if srv.URL == "" {
			return fmt.Errorf("%w: http requires url", ErrInvalid)
		}
		if srv.Transport == "" {
			srv.Transport = "http"
		}
	default:
		return fmt.Errorf("%w: unsupported transport %q", ErrInvalid, srv.Transport)
	}
	if err := rejectMasked("header", srv.Headers); err != nil {
		return err
	}
	if err := rejectMasked("env", srv.Env); err != nil {
		return err
	}
	if err := validateAuthMode(srv); err != nil {
		return err
	}
	if err := validateAssertionProfile(srv); err != nil {
		return err
	}
	return validateIdentity(srv)
}

var assertionProfileRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,63}$`)

func validateAssertionProfile(srv Server) error {
	if srv.AssertionProfile == "" {
		return nil
	}
	if !assertionProfileRE.MatchString(srv.AssertionProfile) || srv.Transport != "http" || srv.AuthMode != AuthPerUser ||
		len(srv.Headers) != 0 || len(srv.Env) != 0 || srv.Command != "" || len(srv.Args) != 0 || srv.Identity != nil {
		return fmt.Errorf("%w: assertion_profile requires a safe profile ID, http, per_user, and no credential or process overrides", ErrInvalid)
	}
	u, err := url.Parse(srv.URL)
	if err != nil || u.Scheme != "http" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%w: assertion profile endpoint must be a plain http URL without user information, query or fragment", ErrInvalid)
	}
	return nil
}

// validateAuthMode checks the sign-in mode: shared (or unset) is always
// fine; per_user needs an http transport, since it works through the
// OAuth bearer on each person's connection, and no static Authorization
// header, which would otherwise be sent as the person.
func validateAuthMode(srv Server) error {
	switch srv.AuthMode {
	case "", AuthShared:
		return nil
	case AuthPerUser:
		if srv.Transport == "stdio" {
			return fmt.Errorf("%w: per-user sign-in needs an http transport", ErrInvalid)
		}
		for k := range srv.Headers {
			if strings.EqualFold(k, "Authorization") {
				return fmt.Errorf("%w: header %q cannot be set on a server where each person signs in; the person's own token is the Authorization", ErrInvalid, k)
			}
		}
		return nil
	}
	return fmt.Errorf("%w: auth_mode must be %q or %q", ErrInvalid, AuthShared, AuthPerUser)
}

// normalizeAuthMode fills the default so rows and configs always carry an
// explicit mode.
func normalizeAuthMode(mode string) string {
	if mode == "" {
		return AuthShared
	}
	return mode
}

// rejectMasked refuses the placeholder Masked() puts in place of a value.
// A client that edits the masked GET view and sends it back would
// otherwise overwrite a real credential with dots.
func rejectMasked(kind string, m map[string]string) error {
	for k, v := range m {
		if v == MaskedValue {
			return fmt.Errorf("%w: %s %q: masked value — send the real value, a secret:// reference, or leave the key out", ErrInvalid, kind, k)
		}
	}
	return nil
}

// sameOrigin reports whether two urls share scheme and host (with port).
// Anything that doesn't parse counts as a different origin, so an odd url
// errs on the side of resetting OAuth.
func sameOrigin(a, b string) bool {
	ua, errA := url.Parse(a)
	ub, errB := url.Parse(b)
	if errA != nil || errB != nil {
		return false
	}
	return strings.EqualFold(ua.Scheme, ub.Scheme) && strings.EqualFold(ua.Host, ub.Host)
}

// identityHeaderDenylist names headers that can never carry an identity
// key: they belong to the HTTP transport, the MCP session or another
// credential. Lower-case; compared case-insensitively.
var identityHeaderDenylist = map[string]bool{
	"authorization":        true,
	"cookie":               true,
	"host":                 true,
	"content-type":         true,
	"content-length":       true,
	"accept":               true,
	"mcp-session-id":       true,
	"mcp-protocol-version": true,
	"last-event-id":        true,
}

// validHeaderToken reports whether name is an RFC 7230 token, the only
// thing an HTTP header field name may be.
func validHeaderToken(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0:
		default:
			return false
		}
	}
	return true
}

// validateIdentity checks a server's identity-forwarding setting: http
// transports only, a real header name that isn't reserved, and no static
// header of the same name, so an admin can't pin a fixed identity on a
// server that is meant to act as the caller.
func validateIdentity(srv Server) error {
	id := srv.Identity
	if id == nil {
		return nil
	}
	if srv.Transport == "stdio" {
		return fmt.Errorf("%w: identity forwarding needs an http transport", ErrInvalid)
	}
	if !validHeaderToken(id.Header) {
		return fmt.Errorf("%w: identity header %q is not a valid HTTP header name", ErrInvalid, id.Header)
	}
	if identityHeaderDenylist[strings.ToLower(id.Header)] {
		return fmt.Errorf("%w: identity header %q is reserved", ErrInvalid, id.Header)
	}
	for k := range srv.Headers {
		if strings.EqualFold(k, id.Header) {
			return fmt.Errorf("%w: static header %q clashes with the identity header; the caller's key must not be pinned", ErrInvalid, k)
		}
	}
	return nil
}

// identityJSON is the identity_json column value: NULL when forwarding is
// off, the JSON object otherwise.
func identityJSON(id *IdentityForwarding) any {
	if id == nil {
		return nil
	}
	b, _ := json.Marshal(id)
	return string(b)
}

// decodeServerJSON fills the JSON-encoded columns of a scanned row.
func decodeServerJSON(srv *Server, argsRaw, envRaw, headersRaw, identityRaw string) {
	if argsRaw != "" {
		_ = json.Unmarshal([]byte(argsRaw), &srv.Args)
	}
	if envRaw != "" {
		_ = json.Unmarshal([]byte(envRaw), &srv.Env)
	}
	if headersRaw != "" {
		_ = json.Unmarshal([]byte(headersRaw), &srv.Headers)
	}
	if identityRaw != "" && identityRaw != "null" {
		var id IdentityForwarding
		if err := json.Unmarshal([]byte(identityRaw), &id); err == nil {
			srv.Identity = &id
		}
	}
}

// LoadAll reads all enabled servers from the DB and connects them in
// parallel. Connection errors are recorded against last_status / last_error
// so the dashboard can show why something didn't come up.
func (s *Service) LoadAll(ctx context.Context) error {
	servers, err := s.list(ctx)
	if err != nil {
		return err
	}
	for _, srv := range servers {
		if !srv.Enabled || srv.AssertionProfile != "" {
			continue
		}
		// A stdio row saved before -no-stdio-upstreams was turned on must
		// not start either. Built-ins that are switched on re-attach through
		// UpsertBuiltin right after this, which overwrites the status.
		if s.stdioRefused(srv) {
			s.recordStatus(ctx, srv.Name, "", errStdioDisabled.Error(), 0)
			continue
		}
		if err := s.connect(ctx, srv); err != nil {
			s.recordStatus(ctx, srv.Name, "", err.Error(), 0)
		}
	}
	return nil
}

// LoadAssertions registers enabled assertion catalogs from trusted local
// profiles. Call this before the deferred approval sweep. It performs no
// network discovery; normal upstreams remain on the separate LoadAll path.
func (s *Service) LoadAssertions(ctx context.Context) error {
	servers, err := s.list(ctx)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, srv := range servers {
		if !srv.Enabled || srv.AssertionProfile == "" {
			continue
		}
		if err := s.connect(ctx, srv); err != nil {
			s.recordStatus(ctx, srv.Name, "", "trusted assertion profile unavailable", 0)
		}
	}
	return nil
}

// AssertEnabled checks the persisted connector before each assertion POST.
// Removal, disablement or any immutable configuration mismatch refuses it.
func (s *Service) AssertEnabled(ctx context.Context, name, profile, endpoint string) error {
	srv, err := s.get(ctx, name)
	if err != nil || !srv.Enabled || profile == "" || srv.AssertionProfile != profile || srv.URL != endpoint ||
		srv.AuthMode != AuthPerUser || srv.Transport != "http" || validate(*srv) != nil {
		return errors.New("trusted assertion connector disabled or changed")
	}
	return nil
}

func (s *Service) list(ctx context.Context) ([]Server, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, transport, COALESCE(command,''), COALESCE(args_json,''),
            COALESCE(url,''), COALESCE(env_json,''), COALESCE(headers_json,''),
            COALESCE(identity_json,''), COALESCE(auth_mode,'shared'), COALESCE(assertion_profile,''), enabled,
            COALESCE(last_status,''), COALESCE(last_error,''), tool_count,
            created_at, updated_at
         FROM upstream_servers ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Server
	for rows.Next() {
		var srv Server
		var argsRaw, envRaw, headersRaw, identityRaw string
		var enabled int
		if err := rows.Scan(&srv.Name, &srv.Transport, &srv.Command, &argsRaw,
			&srv.URL, &envRaw, &headersRaw, &identityRaw, &srv.AuthMode, &srv.AssertionProfile, &enabled, &srv.LastStatus, &srv.LastError,
			&srv.ToolCount, &srv.CreatedAt, &srv.UpdatedAt); err != nil {
			return nil, err
		}
		srv.Enabled = enabled != 0
		decodeServerJSON(&srv, argsRaw, envRaw, headersRaw, identityRaw)
		out = append(out, srv)
	}
	return out, rows.Err()
}

// List returns the persisted upstream configs.
func (s *Service) List(ctx context.Context) ([]Server, error) { return s.list(ctx) }

// Get returns one upstream server by name. Returns ErrNotFound when missing.
func (s *Service) Get(ctx context.Context, name string) (*Server, error) {
	return s.get(ctx, name)
}

// Add inserts a new upstream and tries to connect. On a connection failure the
// row stays around (so the dashboard can show the error and the operator can
// edit + retry); the call returns the connection error for the API layer.
func (s *Service) Add(ctx context.Context, srv Server) (*Server, error) {
	if err := validate(srv); err != nil {
		return nil, err
	}
	if s.stdioRefused(srv) {
		return nil, errStdioDisabled
	}
	if denied := s.policy.envDenied(srv.Env); denied != "" {
		return nil, fmt.Errorf("%w: env key %q is on the denylist", ErrInvalid, denied)
	}
	if err := s.validateSecretRefs(ctx, srv); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UnixMilli()
	srv.CreatedAt = now
	srv.UpdatedAt = now
	srv.Enabled = true
	if srv.AssertionProfile != "" {
		srv.Enabled = false
	}
	srv.AuthMode = normalizeAuthMode(srv.AuthMode)

	argsBlob, _ := json.Marshal(srv.Args)
	envBlob, _ := json.Marshal(srv.Env)
	headersBlob, _ := json.Marshal(srv.Headers)

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO upstream_servers(name, transport, command, args_json, url,
            env_json, headers_json, identity_json, auth_mode, assertion_profile, enabled, created_at, updated_at)
         VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		srv.Name, srv.Transport, nullStr(srv.Command), string(argsBlob),
		nullStr(srv.URL), string(envBlob), string(headersBlob), identityJSON(srv.Identity), srv.AuthMode, nullStr(srv.AssertionProfile), boolInt(srv.Enabled), now, now)
	if err != nil {
		// SQLite reports unique constraint as "UNIQUE constraint failed".
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return nil, ErrAlreadyHere
		}
		return nil, err
	}

	if !srv.Enabled {
		s.recordStatus(ctx, srv.Name, "disabled", "", 0)
		return s.get(ctx, srv.Name)
	}
	if err := s.connect(ctx, srv); err != nil {
		s.recordStatus(ctx, srv.Name, "", err.Error(), 0)
		final, _ := s.get(ctx, srv.Name)
		return final, err
	}
	final, _ := s.get(ctx, srv.Name)
	return final, nil
}

// Remove disconnects the upstream and deletes the row.
func (s *Service) Remove(ctx context.Context, name string) error {
	if isReservedBuiltin(name) {
		return ErrReserved
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.gw.RemoveUpstream(name); err != nil &&
		!errors.Is(err, gateway.ErrUpstreamNotFound) {
		return err
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM upstream_servers WHERE name = ?`, name)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	s.gw.NotifyToolListChanged()
	return nil
}

// ReconnectAfterAuth is called after the OAuth flow completes for an
// upstream so the live gateway picks up the freshly-stored bearer
// without an operator click. Best-effort — errors are logged via the
// status row.
func (s *Service) ReconnectAfterAuth(ctx context.Context, name string) {
	srv, err := s.get(ctx, name)
	if err != nil {
		return
	}
	if !srv.Enabled || srv.AssertionProfile != "" {
		return
	}
	s.mu.Lock()
	_ = s.gw.RemoveUpstream(name)
	if err := s.connect(ctx, *srv); err != nil {
		s.recordStatus(ctx, name, "", err.Error(), 0)
	}
	s.mu.Unlock()
}

// ReconnectAfterUserAuth is called after one person connects a per_user
// upstream. Other people's connections are left alone; what changes is
// that an upstream nobody had connected yet now gets its tool list (over
// this person's session). The person's own connection opens on their
// first call. Best-effort, as ReconnectAfterAuth.
func (s *Service) ReconnectAfterUserAuth(ctx context.Context, name, userID string) {
	srv, err := s.get(ctx, name)
	if err != nil || !srv.Enabled || srv.AuthMode != AuthPerUser || srv.AssertionProfile != "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	err = s.gw.RefreshPerUser(ctx, name)
	switch {
	case errors.Is(err, gateway.ErrUpstreamNotFound):
		// Not registered (an earlier connect failed outright): a full
		// connect registers it and loads the tools.
		if err := s.connect(ctx, *srv); err != nil {
			s.recordStatus(ctx, name, "", err.Error(), 0)
		}
	case errors.Is(err, gateway.ErrWaitingForSignIn):
		s.recordStatus(ctx, name, gateway.StatusWaitingSignIn, "", 0)
	case err != nil:
		s.recordStatus(ctx, name, "", err.Error(), 0)
	default:
		s.recordStatus(ctx, name, "ok", "", s.gw.UpstreamToolCount(name))
		s.gw.NotifyToolListChanged()
	}
}

// DropUserConnection closes one person's connection to a per_user
// upstream: after they disconnect, or after their token was rejected.
func (s *Service) DropUserConnection(name, userID string) {
	s.gw.DropUserConnection(name, userID)
}

// Reconnect drops any existing connection and re-tries.
func (s *Service) Reconnect(ctx context.Context, name string) (*Server, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	srv, err := s.get(ctx, name)
	if err != nil {
		return nil, err
	}
	if srv.AssertionProfile != "" && !srv.Enabled {
		return srv, nil
	}
	// Built-ins run a command the binary chose, not dashboard input.
	if s.stdioRefused(*srv) && !isReservedBuiltin(name) {
		return nil, errStdioDisabled
	}
	_ = s.gw.RemoveUpstream(name)
	if err := s.connect(ctx, *srv); err != nil {
		s.recordStatus(ctx, name, "", err.Error(), 0)
		final, _ := s.get(ctx, name)
		return final, err
	}
	return s.get(ctx, name)
}

var errStdioDisabled = fmt.Errorf("%w: stdio upstreams disabled by -no-stdio-upstreams", ErrInvalid)

// stdioRefused reports whether -no-stdio-upstreams forbids starting srv.
func (s *Service) stdioRefused(srv Server) bool {
	return srv.Transport == "stdio" && !s.policy.AllowStdio
}

// connect attaches the upstream to the gateway and records status. Caller
// holds s.mu. A per_user upstream nobody has connected yet is registered
// without tools and recorded as waiting for a first sign-in; that is not
// a failure.
func (s *Service) connect(ctx context.Context, srv Server) error {
	cfg := s.toCfg(srv)
	if srv.AssertionProfile != "" {
		trusted, err := s.assertionConfig(ctx, srv)
		if err != nil {
			return err
		}
		cfg.Trusted = trusted
	}
	err := s.gw.AddUpstream(ctx, cfg)
	if errors.Is(err, gateway.ErrWaitingForSignIn) {
		s.recordStatus(ctx, srv.Name, gateway.StatusWaitingSignIn, "", 0)
		s.gw.NotifyToolListChanged()
		return nil
	}
	if err != nil {
		return err
	}
	s.recordStatus(ctx, srv.Name, "ok", "", s.gw.UpstreamToolCount(srv.Name))
	s.gw.NotifyToolListChanged()
	return nil
}

func (s *Service) assertionConfig(ctx context.Context, srv Server) (gateway.TrustedUpstream, error) {
	if validate(srv) != nil || s.assertions == nil {
		return nil, fmt.Errorf("%w: trusted assertion profile unavailable", ErrInvalid)
	}
	trusted, err := s.assertions(ctx, srv)
	if err != nil || trusted == nil {
		return nil, fmt.Errorf("%w: trusted assertion profile unavailable", ErrInvalid)
	}
	return trusted, nil
}

func (s *Service) get(ctx context.Context, name string) (*Server, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT name, transport, COALESCE(command,''), COALESCE(args_json,''),
            COALESCE(url,''), COALESCE(env_json,''), COALESCE(headers_json,''),
            COALESCE(identity_json,''), COALESCE(auth_mode,'shared'), COALESCE(assertion_profile,''), enabled,
            COALESCE(last_status,''), COALESCE(last_error,''), tool_count,
            created_at, updated_at
         FROM upstream_servers WHERE name = ?`, name)
	var srv Server
	var argsRaw, envRaw, headersRaw, identityRaw string
	var enabled int
	if err := row.Scan(&srv.Name, &srv.Transport, &srv.Command, &argsRaw,
		&srv.URL, &envRaw, &headersRaw, &identityRaw, &srv.AuthMode, &srv.AssertionProfile, &enabled, &srv.LastStatus, &srv.LastError,
		&srv.ToolCount, &srv.CreatedAt, &srv.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	srv.Enabled = enabled != 0
	decodeServerJSON(&srv, argsRaw, envRaw, headersRaw, identityRaw)
	return &srv, nil
}

func (s *Service) recordStatus(ctx context.Context, name, status, errMsg string, toolCount int) {
	_, _ = s.db.ExecContext(ctx,
		`UPDATE upstream_servers SET last_status = ?, last_error = ?, tool_count = ?, updated_at = ?
         WHERE name = ?`,
		nullStr(status), nullStr(errMsg), toolCount, time.Now().UnixMilli(), name)
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

// isReservedBuiltin reports whether name refers to a toolyard-managed
// upstream that the dashboard shouldn't allow operators to delete. Built-in
// upstreams (e.g. "mempalace") are managed by the gateway's startup wiring;
// removing them from the DB doesn't unregister the live tools, and the next
// boot would simply re-insert the row anyway.
func isReservedBuiltin(name string) bool {
	switch name {
	case "mempalace", "notes":
		return true
	}
	return false
}

// UpsertBuiltin persists a toolyard-managed upstream config and connects it.
// It bypasses the AllowStdio policy check (which exists to fence
// dashboard-driven user input) and the reserved-name validation — built-ins
// are part of the binary's startup contract, not user-supplied. If a row by
// the same name already exists the config is updated in place; otherwise a
// new row is inserted. Either way the upstream is (re)connected and an
// tools/list_changed notification fires so live MCP clients pick the new
// tools up immediately.
//
// Connection errors leave the row in place with last_error populated so the
// dashboard can show the failure; the operator may retry via the standard
// reconnect endpoint.
func (s *Service) UpsertBuiltin(ctx context.Context, srv Server) (*Server, error) {
	if srv.AssertionProfile != "" {
		return nil, fmt.Errorf("%w: assertion profiles are dashboard-managed upstreams", ErrInvalid)
	}
	if strings.TrimSpace(srv.Name) == "" {
		return nil, fmt.Errorf("%w: name required", ErrInvalid)
	}
	if !isReservedBuiltin(srv.Name) {
		return nil, fmt.Errorf("%w: UpsertBuiltin is only for reserved built-in names", ErrInvalid)
	}
	// Built-ins still get the env-denylist check — even toolyard's own
	// startup wiring shouldn't accidentally pass LD_PRELOAD.
	if denied := s.policy.envDenied(srv.Env); denied != "" {
		return nil, fmt.Errorf("%w: env key %q is on the denylist", ErrInvalid, denied)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UnixMilli()
	srv.UpdatedAt = now
	if srv.CreatedAt == 0 {
		srv.CreatedAt = now
	}
	srv.Enabled = true

	argsBlob, _ := json.Marshal(srv.Args)
	envBlob, _ := json.Marshal(srv.Env)
	headersBlob, _ := json.Marshal(srv.Headers)

	// INSERT … ON CONFLICT keeps the existing created_at while updating the
	// rest. SQLite's "excluded" pseudo-table refers to the would-be-inserted row.
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO upstream_servers(name, transport, command, args_json, url,
            env_json, headers_json, identity_json, assertion_profile, enabled, created_at, updated_at)
         VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
         ON CONFLICT(name) DO UPDATE SET
            transport = excluded.transport,
            command   = excluded.command,
            args_json = excluded.args_json,
            url       = excluded.url,
            env_json  = excluded.env_json,
            headers_json = excluded.headers_json,
            identity_json = excluded.identity_json,
            assertion_profile = excluded.assertion_profile,
            enabled   = 1,
            updated_at = excluded.updated_at`,
		srv.Name, srv.Transport, nullStr(srv.Command), string(argsBlob),
		nullStr(srv.URL), string(envBlob), string(headersBlob), identityJSON(srv.Identity), nullStr(srv.AssertionProfile), 1, srv.CreatedAt, now)
	if err != nil {
		return nil, err
	}

	// If something was already connected under this name, drop it first so
	// we pick up command/args changes across restarts.
	_ = s.gw.RemoveUpstream(srv.Name)

	if err := s.connect(ctx, srv); err != nil {
		s.recordStatus(ctx, srv.Name, "", err.Error(), 0)
		final, _ := s.get(ctx, srv.Name)
		return final, err
	}
	return s.get(ctx, srv.Name)
}

// validateSecretRefs checks every secret:// reference in the server's env and
// headers: shape must be valid and the named secret must already exist, so a
// typo fails at save time rather than silently failing the dial later. A nil
// secrets resolver means refs are permitted unchecked (the dial will still
// fail loudly if the broker isn't wired).
func (s *Service) validateSecretRefs(ctx context.Context, srv Server) error {
	if s.secrets == nil {
		return nil
	}
	check := func(kind string, m map[string]string) error {
		for k, v := range m {
			if !secrets.IsRef(v) {
				continue
			}
			names, ok := secrets.Refs(v)
			if !ok {
				return fmt.Errorf("%w: %s %q has a malformed secret ref (use secret://NAME or ${secret://NAME})", ErrInvalid, kind, k)
			}
			for _, name := range names {
				exists, err := s.secrets.Exists(ctx, name)
				if err != nil {
					return err
				}
				if !exists {
					return fmt.Errorf("%w: %s %q references unknown secret %q (request it with POST /v1/secrets {\"name\":%q,\"request\":true})", ErrInvalid, kind, k, name, name)
				}
			}
		}
		return nil
	}
	if err := check("env", srv.Env); err != nil {
		return err
	}
	return check("header", srv.Headers)
}

// Masked returns a copy of srv safe to serialize to the dashboard / API:
// secret:// refs pass through verbatim (they're not sensitive), but every
// other non-empty env / header value is replaced with a bullet placeholder.
// EnvPlaintextKeys lists the keys whose value was masked so the UI can offer
// a "convert to secret" action.
func Masked(srv Server) Server {
	out := srv
	out.Env, out.EnvPlaintextKeys = maskValues(srv.Env)
	out.Headers, _ = maskValues(srv.Headers)
	return out
}

// MaskedValue stands in for every plaintext env / header value in API
// responses. It is also refused on save (see rejectMasked).
const MaskedValue = "•••"

func maskValues(in map[string]string) (map[string]string, []string) {
	if len(in) == 0 {
		return in, nil
	}
	masked := make(map[string]string, len(in))
	var plaintext []string
	for k, v := range in {
		switch {
		case v == "":
			masked[k] = ""
		case secrets.IsRef(v):
			masked[k] = v
		default:
			masked[k] = MaskedValue
			plaintext = append(plaintext, k)
		}
	}
	sort.Strings(plaintext)
	return masked, plaintext
}

// Patch is a partial edit for Update. A nil pointer leaves that field as it
// is; Headers and Env replace the whole map when present ({} clears it).
// Identity is tri-state so the dashboard can send "identity": null to turn
// forwarding off. Transport, command and args are not editable: changing
// the process an upstream runs is a remove-and-add.
//
// AuthMode switches who signs in. Neither direction drops any token:
// shared -> per_user keeps the shared token row (nothing on a per_user
// server ever uses it, and the admin can disconnect it from Auth…), and
// per_user -> shared keeps every person's row (unused and no longer
// refreshed while the server is shared, back in use if it is switched
// again). Correctness only needs the live connections replaced, which
// Update does by reconnecting.
type Patch struct {
	URL              *string            `json:"url"`
	Headers          *map[string]string `json:"headers"`
	Env              *map[string]string `json:"env"`
	Identity         OptionalIdentity   `json:"identity"`
	AuthMode         *string            `json:"auth_mode"`
	AssertionProfile *string            `json:"assertion_profile"`
	Enabled          *bool              `json:"enabled"`
}

// OptionalIdentity tells "not in the body" (Set false) apart from an
// explicit null (Set true, Value nil) and an object (Set true, Value set).
type OptionalIdentity struct {
	Set   bool
	Value *IdentityForwarding
}

// UnmarshalJSON runs for a present field only, including a JSON null,
// which is what makes the tri-state work.
func (o *OptionalIdentity) UnmarshalJSON(b []byte) error {
	o.Set = true
	o.Value = nil
	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var v IdentityForwarding
	if err := dec.Decode(&v); err != nil {
		return fmt.Errorf("identity: %w", err)
	}
	o.Value = &v
	return nil
}

// Update applies patch to a saved server and reconnects it in place. The
// row is updated, never deleted and re-inserted, so the server's OAuth
// client and tokens (which cascade on delete) survive an edit. Validation
// runs on the merged config, so a patch can't sneak an identity header
// past an existing static header or vice versa. On a connect failure the
// new config is still saved and the error recorded on the row, as in Add.
func (s *Service) Update(ctx context.Context, name string, patch Patch) (*Server, error) {
	if isReservedBuiltin(name) {
		return nil, ErrReserved
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	cur, err := s.get(ctx, name)
	if err != nil {
		return nil, err
	}
	next := *cur
	if patch.AssertionProfile != nil && *patch.AssertionProfile != cur.AssertionProfile {
		return nil, fmt.Errorf("%w: assertion_profile is immutable; remove and add a separate connector", ErrInvalid)
	}
	if cur.AssertionProfile != "" && patch.URL != nil && strings.TrimSpace(*patch.URL) != cur.URL {
		return nil, fmt.Errorf("%w: assertion profile endpoint is immutable", ErrInvalid)
	}
	if patch.URL != nil {
		next.URL = strings.TrimSpace(*patch.URL)
	}
	if patch.Headers != nil {
		next.Headers = copyMap(*patch.Headers)
	}
	if patch.Env != nil {
		next.Env = copyMap(*patch.Env)
	}
	if patch.Identity.Set {
		next.Identity = patch.Identity.Value
	}
	if patch.AuthMode != nil {
		next.AuthMode = normalizeAuthMode(strings.TrimSpace(*patch.AuthMode))
	}
	if patch.Enabled != nil {
		next.Enabled = *patch.Enabled
	}
	if err := validate(next); err != nil {
		return nil, err
	}
	if next.Enabled && s.stdioRefused(next) {
		return nil, errStdioDisabled
	}
	if denied := s.policy.envDenied(next.Env); denied != "" {
		return nil, fmt.Errorf("%w: env key %q is on the denylist", ErrInvalid, denied)
	}
	if err := s.validateSecretRefs(ctx, next); err != nil {
		return nil, err
	}
	if next.AssertionProfile != "" && next.Enabled {
		if _, err := s.assertionConfig(ctx, next); err != nil {
			return nil, err
		}
	}

	// A bearer belongs to the host it was issued for. Before the url moves
	// to another origin, drop the server's OAuth client and tokens (the
	// header func is keyed by server name and would otherwise send the old
	// token to the new host on the first initialize). This happens before
	// the row changes, so a failed reset leaves the server as it was.
	oauthReset := false
	if patch.URL != nil && s.auth != nil && !sameOrigin(cur.URL, next.URL) {
		has, err := s.auth.HasClient(ctx, name)
		if err != nil {
			return nil, err
		}
		if has {
			if err := s.auth.Disconnect(ctx, name); err != nil {
				return nil, fmt.Errorf("reset oauth for %s before url change: %w", name, err)
			}
			oauthReset = true
		}
	}

	envBlob, _ := json.Marshal(next.Env)
	headersBlob, _ := json.Marshal(next.Headers)
	enabled := 0
	if next.Enabled {
		enabled = 1
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE upstream_servers SET url=?, env_json=?, headers_json=?, identity_json=?, auth_mode=?, assertion_profile=?, enabled=?, updated_at=?
         WHERE name=?`,
		nullStr(next.URL), string(envBlob), string(headersBlob), identityJSON(next.Identity),
		normalizeAuthMode(next.AuthMode), nullStr(next.AssertionProfile), enabled, time.Now().UnixMilli(), name); err != nil {
		return nil, err
	}

	// Reconnect in place so the live upstream runs the new config. A
	// disabled server is just taken down; LoadAll skips it on restart.
	_ = s.gw.RemoveUpstream(name)
	var connectErr error
	if !next.Enabled {
		s.recordStatus(ctx, name, "disabled", "", 0)
		s.gw.NotifyToolListChanged()
	} else if connectErr = s.connect(ctx, next); connectErr != nil {
		s.recordStatus(ctx, name, "", connectErr.Error(), 0)
	}
	final, err := s.get(ctx, name)
	if err != nil {
		return nil, err
	}
	final.OAuthReset = oauthReset
	return final, connectErr
}

// ConvertEnvToSecret extracts the plaintext value of env[envKey] on the named
// server, creates a secret from it via create, rewrites the env entry to a
// secret:// ref, persists, and reconnects so the live upstream picks up the
// ref. create is supplied by the caller (the API layer) so this package stays
// decoupled from internal/secrets' write side.
func (s *Service) ConvertEnvToSecret(ctx context.Context, serverName, envKey, secretName string,
	create func(ctx context.Context, name, value, description string) error) (*Server, error) {
	if !secrets.ValidName(secretName) {
		return nil, fmt.Errorf("%w: secret name must match ^[A-Z][A-Z0-9_]{0,63}$", ErrInvalid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	srv, err := s.get(ctx, serverName)
	if err != nil {
		return nil, err
	}
	val, ok := srv.Env[envKey]
	if !ok || val == "" {
		return nil, fmt.Errorf("%w: env key %q not set on %q", ErrInvalid, envKey, serverName)
	}
	if secrets.IsRef(val) {
		return nil, fmt.Errorf("%w: env key %q is already a secret reference", ErrInvalid, envKey)
	}
	if err := create(ctx, secretName, val, "converted from "+serverName+" env "+envKey); err != nil {
		return nil, err
	}
	srv.Env[envKey] = secrets.RefPrefix + secretName
	envBlob, _ := json.Marshal(srv.Env)
	if _, err := s.db.ExecContext(ctx,
		`UPDATE upstream_servers SET env_json=?, updated_at=? WHERE name=?`,
		string(envBlob), time.Now().UnixMilli(), serverName); err != nil {
		return nil, err
	}
	// Reconnect so the running upstream re-dials with the ref (resolved at
	// dial time). Best-effort: a connect failure is recorded on the row.
	_ = s.gw.RemoveUpstream(serverName)
	if err := s.connect(ctx, *srv); err != nil {
		s.recordStatus(ctx, serverName, "", err.Error(), 0)
		final, _ := s.get(ctx, serverName)
		return final, err
	}
	return s.get(ctx, serverName)
}

// SortedNames is a small helper for the API layer.
func SortedNames(servers []Server) []string {
	out := make([]string, 0, len(servers))
	for _, s := range servers {
		out = append(out, s.Name)
	}
	sort.Strings(out)
	return out
}
