// Package upstreams persists MCP-server configurations and reconciles them
// with the running gateway: connect-on-add, disconnect-on-remove, retry on
// reconnect, all from the dashboard.
package upstreams

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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
	Headers    map[string]string `json:"headers,omitempty"`
	Enabled    bool              `json:"enabled"`
	LastStatus string            `json:"last_status,omitempty"`
	LastError  string            `json:"last_error,omitempty"`
	ToolCount  int               `json:"tool_count"`
	CreatedAt  int64             `json:"created_at"`
	UpdatedAt  int64             `json:"updated_at"`
	// EnvPlaintextKeys is populated only by Masked(): the env keys whose
	// values were masked (i.e. plaintext, not secret:// refs) so the UI can
	// offer a "convert to secret" action. Never persisted.
	EnvPlaintextKeys []string `json:"env_plaintext_keys,omitempty"`
}

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
	HasClient(ctx context.Context, upstream string) (bool, error)
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
	db      *store.DB
	gw      *gateway.Gateway
	policy  Policy
	auth    HeaderProvider // optional
	secrets SecretResolver // optional

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

// SetSecrets installs the secrets broker so secret:// refs in env/headers
// resolve at dial time. Nil disables the wiring (refs would then fail the
// dial as missing secrets).
func (s *Service) SetSecrets(r SecretResolver) { s.secrets = r }

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
	isHTTP := cfg.Transport == "http" || cfg.Transport == "streamable-http" || cfg.Transport == ""

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
	// header. OAuth wins on a clash (e.g. Authorization) so a stored bearer
	// always takes precedence over a hand-set header.
	if isHTTP {
		staticHeaders := copyMap(srv.Headers)
		resolver := s.secrets
		var oauthFn func(ctx context.Context) map[string]string
		if s.auth != nil {
			oauthFn = s.auth.HeaderFunc(srv.Name)
		}
		if len(staticHeaders) > 0 || oauthFn != nil {
			cfg.HeaderFunc = func(ctx context.Context) map[string]string {
				out := map[string]string{}
				if len(staticHeaders) > 0 {
					resolved := staticHeaders
					if resolver != nil {
						if r, err := resolver.ResolveMap(ctx, staticHeaders); err == nil {
							resolved = r
						}
					}
					for k, v := range resolved {
						out[k] = v
					}
				}
				if oauthFn != nil {
					for k, v := range oauthFn(ctx) {
						out[k] = v // OAuth wins
					}
				}
				return out
			}
		}
	}
	return cfg
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
	switch srv.Name {
	case "builtin", "fixture", "memory", "tools", "mempalace", "notes", "skills":
		return ErrReserved
	}
	switch srv.Transport {
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
	return nil
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
		if !srv.Enabled {
			continue
		}
		if err := s.connect(ctx, srv); err != nil {
			s.recordStatus(ctx, srv.Name, "", err.Error(), 0)
		}
	}
	return nil
}

func (s *Service) list(ctx context.Context) ([]Server, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, transport, COALESCE(command,''), COALESCE(args_json,''),
            COALESCE(url,''), COALESCE(env_json,''), COALESCE(headers_json,''), enabled,
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
		var argsRaw, envRaw, headersRaw string
		var enabled int
		if err := rows.Scan(&srv.Name, &srv.Transport, &srv.Command, &argsRaw,
			&srv.URL, &envRaw, &headersRaw, &enabled, &srv.LastStatus, &srv.LastError,
			&srv.ToolCount, &srv.CreatedAt, &srv.UpdatedAt); err != nil {
			return nil, err
		}
		srv.Enabled = enabled != 0
		if argsRaw != "" {
			_ = json.Unmarshal([]byte(argsRaw), &srv.Args)
		}
		if envRaw != "" {
			_ = json.Unmarshal([]byte(envRaw), &srv.Env)
		}
		if headersRaw != "" {
			_ = json.Unmarshal([]byte(headersRaw), &srv.Headers)
		}
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
	if srv.Transport == "stdio" && !s.policy.AllowStdio {
		return nil, fmt.Errorf("%w: stdio upstreams disabled by -no-stdio-upstreams", ErrInvalid)
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

	argsBlob, _ := json.Marshal(srv.Args)
	envBlob, _ := json.Marshal(srv.Env)
	headersBlob, _ := json.Marshal(srv.Headers)

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO upstream_servers(name, transport, command, args_json, url,
            env_json, headers_json, enabled, created_at, updated_at)
         VALUES(?,?,?,?,?,?,?,?,?,?)`,
		srv.Name, srv.Transport, nullStr(srv.Command), string(argsBlob),
		nullStr(srv.URL), string(envBlob), string(headersBlob), 1, now, now)
	if err != nil {
		// SQLite reports unique constraint as "UNIQUE constraint failed".
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return nil, ErrAlreadyHere
		}
		return nil, err
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
	if !srv.Enabled {
		return
	}
	s.mu.Lock()
	_ = s.gw.RemoveUpstream(name)
	if err := s.connect(ctx, *srv); err != nil {
		s.recordStatus(ctx, name, "", err.Error(), 0)
	}
	s.mu.Unlock()
}

// Reconnect drops any existing connection and re-tries.
func (s *Service) Reconnect(ctx context.Context, name string) (*Server, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	srv, err := s.get(ctx, name)
	if err != nil {
		return nil, err
	}
	_ = s.gw.RemoveUpstream(name)
	if err := s.connect(ctx, *srv); err != nil {
		s.recordStatus(ctx, name, "", err.Error(), 0)
		final, _ := s.get(ctx, name)
		return final, err
	}
	return s.get(ctx, name)
}

// connect attaches the upstream to the gateway and records status. Caller
// holds s.mu.
func (s *Service) connect(ctx context.Context, srv Server) error {
	if err := s.gw.AddUpstream(ctx, s.toCfg(srv)); err != nil {
		return err
	}
	s.recordStatus(ctx, srv.Name, "ok", "", s.gw.UpstreamToolCount(srv.Name))
	s.gw.NotifyToolListChanged()
	return nil
}

func (s *Service) get(ctx context.Context, name string) (*Server, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT name, transport, COALESCE(command,''), COALESCE(args_json,''),
            COALESCE(url,''), COALESCE(env_json,''), COALESCE(headers_json,''), enabled,
            COALESCE(last_status,''), COALESCE(last_error,''), tool_count,
            created_at, updated_at
         FROM upstream_servers WHERE name = ?`, name)
	var srv Server
	var argsRaw, envRaw, headersRaw string
	var enabled int
	if err := row.Scan(&srv.Name, &srv.Transport, &srv.Command, &argsRaw,
		&srv.URL, &envRaw, &headersRaw, &enabled, &srv.LastStatus, &srv.LastError,
		&srv.ToolCount, &srv.CreatedAt, &srv.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	srv.Enabled = enabled != 0
	if argsRaw != "" {
		_ = json.Unmarshal([]byte(argsRaw), &srv.Args)
	}
	if envRaw != "" {
		_ = json.Unmarshal([]byte(envRaw), &srv.Env)
	}
	if headersRaw != "" {
		_ = json.Unmarshal([]byte(headersRaw), &srv.Headers)
	}
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
            env_json, headers_json, enabled, created_at, updated_at)
         VALUES(?,?,?,?,?,?,?,?,?,?)
         ON CONFLICT(name) DO UPDATE SET
            transport = excluded.transport,
            command   = excluded.command,
            args_json = excluded.args_json,
            url       = excluded.url,
            env_json  = excluded.env_json,
            headers_json = excluded.headers_json,
            enabled   = 1,
            updated_at = excluded.updated_at`,
		srv.Name, srv.Transport, nullStr(srv.Command), string(argsBlob),
		nullStr(srv.URL), string(envBlob), string(headersBlob), 1, srv.CreatedAt, now)
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
			name, ok := secrets.ParseRef(v)
			if !ok {
				return fmt.Errorf("%w: %s %q has malformed secret ref %q", ErrInvalid, kind, k, v)
			}
			exists, err := s.secrets.Exists(ctx, name)
			if err != nil {
				return err
			}
			if !exists {
				return fmt.Errorf("%w: %s %q references unknown secret %q", ErrInvalid, kind, k, name)
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
			masked[k] = "•••"
			plaintext = append(plaintext, k)
		}
	}
	sort.Strings(plaintext)
	return masked, plaintext
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
