package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// State labels for oauth_tokens.state.
const (
	StateUnauthorized = "unauthorized"
	StateActive       = "active"
	StateNeedsReauth  = "needs_reauth"
	StateRefreshing   = "refreshing"
)

// Mode tags for oauth_pending.mode.
const (
	ModeCallback = "callback" // Mode A — same-origin redirect to /v1/mcp-oauth/callback
	ModePaste    = "paste"    // Mode B — user pastes the post-redirect URL into the dashboard
	ModeDevice   = "device"   // Mode C — RFC 8628 device flow
)

// PendingTTL is how long we keep a started OAuth flow alive. Shorter than
// the "1h" most IdPs allow — operator inattention is a CSRF risk.
const PendingTTL = 10 * time.Minute

// Common errors.
var (
	ErrPendingNotFound = errors.New("oauth: state not found or expired")
	ErrClientNotFound  = errors.New("oauth: client not registered")
	ErrTokenNotFound   = errors.New("oauth: no token stored")
	ErrNeedsReauth     = errors.New("oauth: upstream needs reauthorization")
	ErrFlowMismatch    = errors.New("oauth: pending flow does not match")
)

// ASMetadata captures the subset of RFC 8414 / OIDC discovery we use.
type ASMetadata struct {
	Issuer                       string   `json:"issuer"`
	AuthorizationEndpoint        string   `json:"authorization_endpoint"`
	TokenEndpoint                string   `json:"token_endpoint"`
	RegistrationEndpoint         string   `json:"registration_endpoint,omitempty"`
	RevocationEndpoint           string   `json:"revocation_endpoint,omitempty"`
	DeviceAuthorizationEndpoint  string   `json:"device_authorization_endpoint,omitempty"`
	ScopesSupported              []string `json:"scopes_supported,omitempty"`
	GrantTypesSupported          []string `json:"grant_types_supported,omitempty"`
	TokenEndpointAuthMethodsSupp []string `json:"token_endpoint_auth_methods_supported,omitempty"`
	CodeChallengeMethodsSupp     []string `json:"code_challenge_methods_supported,omitempty"`
}

// PRMetadata is the protected-resource metadata (RFC 9728).
type PRMetadata struct {
	Resource             string   `json:"resource"`
	AuthorizationServers []string `json:"authorization_servers"`
	ScopesSupported      []string `json:"scopes_supported,omitempty"`
}

// ClientRecord is the per-upstream OAuth client persisted to oauth_clients.
type ClientRecord struct {
	UpstreamName                string
	Issuer                      string
	AuthorizationEndpoint       string
	TokenEndpoint               string
	RegistrationEndpoint        string
	RevocationEndpoint          string
	DeviceAuthorizationEndpoint string
	ClientID                    string
	ClientSecret                string // decrypted; stored encrypted
	RedirectURI                 string
	Scopes                      []string
	TokenEndpointAuthMethod     string
	Metadata                    *ASMetadata
}

// TokenRecord is the persisted token row, decrypted.
type TokenRecord struct {
	UpstreamName     string
	AccessToken      string
	RefreshToken     string
	TokenType        string
	Scope            string
	ObtainedAt       time.Time
	AccessExpiresAt  time.Time // zero means no expiry (PAT)
	RefreshExpiresAt time.Time
	LastRefreshAt    time.Time
	RefreshFailures  int
	State            string
	LastError        string
	IsPAT            bool
}

// PendingRecord is one in-flight OAuth dance.
type PendingRecord struct {
	State           string
	UpstreamName    string
	UserID          string
	Mode            string
	CodeVerifier    string
	DeviceCode      string
	UserCode        string
	IntervalS       int
	VerificationURI string
	ExpiresAt       time.Time
	CreatedAt       time.Time
}

// EventBus is the minimum the OAuth service needs from the realtime hub.
// Decoupled so we don't pull realtime into this package.
type EventBus interface {
	Publish(eventType string, data any)
}

// Notifier is the minimum we need from the push service.
type Notifier interface {
	Notify(ctx context.Context, userID string, payload any) error
}

// IdentityResolver lets the service ask "who's the primary user?" so the
// refresher can fire push notifications without holding a request scope.
type IdentityResolver interface {
	PrimaryUserID(ctx context.Context) (string, error)
}

// Service is the top-level OAuth coordinator.
type Service struct {
	db       *store.DB
	cipher   *Cipher
	httpc    *http.Client
	bus      EventBus
	notifier Notifier
	idents   IdentityResolver

	mu      sync.RWMutex
	bearers map[string]*atomic.Value // upstream -> *atomic.Value holding string

	// lastAttempt tracks the last time the refresher attempted a refresh
	// for an upstream (success OR failure). Unlike last_refresh_at, which
	// only advances on success, this lets backoff space out retries across
	// an IdP outage instead of firing every tick.
	lastAttempt map[string]time.Time

	// reauthHook is invoked once per upstream when state flips to
	// needs_reauth so the upstreams.Service can drop the dead connection.
	reauthHook func(upstream string)
}

// New wires the service. dataDir is where we read/create oauth.key.
func New(db *store.DB, cipher *Cipher, bus EventBus, notifier Notifier, idents IdentityResolver) *Service {
	return &Service{
		db:       db,
		cipher:   cipher,
		bus:      bus,
		notifier: notifier,
		idents:   idents,
		httpc: &http.Client{
			Timeout: 30 * time.Second,
		},
		bearers:     map[string]*atomic.Value{},
		lastAttempt: map[string]time.Time{},
	}
}

// SetReauthHook installs the callback fired when a token becomes invalid.
func (s *Service) SetReauthHook(fn func(upstream string)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reauthHook = fn
}

// SetHTTPClient lets tests inject a transport pointed at an httptest.Server.
func (s *Service) SetHTTPClient(c *http.Client) { s.httpc = c }

// HasClient reports whether the upstream has a registered OAuth client.
func (s *Service) HasClient(ctx context.Context, upstream string) (bool, error) {
	row := s.db.QueryRowContext(ctx, `SELECT 1 FROM oauth_clients WHERE upstream_name = ?`, upstream)
	var x int
	if err := row.Scan(&x); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// HeaderFunc returns a transport.HTTPHeaderFunc-compatible closure that
// stamps Authorization: Bearer with the live token. Empty when there is
// no token (the upstream connect attempt will surface a clear 401).
func (s *Service) HeaderFunc(upstream string) func(ctx context.Context) map[string]string {
	return func(_ context.Context) map[string]string {
		tok := s.currentBearer(upstream)
		if tok == "" {
			return nil
		}
		return map[string]string{"Authorization": "Bearer " + tok}
	}
}

// currentBearer fetches the cached access token for an upstream. The
// refresher writes it; readers read without a lock via atomic.Value.
func (s *Service) currentBearer(upstream string) string {
	s.mu.RLock()
	v, ok := s.bearers[upstream]
	s.mu.RUnlock()
	if !ok {
		// Cold path — populate from DB.
		s.cacheBearerFromDB(upstream)
		s.mu.RLock()
		v, ok = s.bearers[upstream]
		s.mu.RUnlock()
		if !ok {
			return ""
		}
	}
	if s, ok := v.Load().(string); ok {
		return s
	}
	return ""
}

func (s *Service) setBearer(upstream, tok string) {
	s.mu.Lock()
	v := s.bearers[upstream]
	if v == nil {
		v = &atomic.Value{}
		s.bearers[upstream] = v
	}
	s.mu.Unlock()
	v.Store(tok)
}

// cacheBearerFromDB pre-populates the in-memory bearer cache from the
// oauth_tokens row. Used on startup and on cold reads.
func (s *Service) cacheBearerFromDB(upstream string) {
	tok, err := s.GetToken(context.Background(), upstream)
	if err != nil || tok == nil {
		return
	}
	s.setBearer(upstream, tok.AccessToken)
}

// PrimeBearers walks every upstream with a stored token and warms the
// in-memory cache. Called once at startup.
func (s *Service) PrimeBearers(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT upstream_name FROM oauth_tokens`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return err
		}
		names = append(names, n)
	}
	for _, n := range names {
		s.cacheBearerFromDB(n)
	}
	return rows.Err()
}

// ---------------------------------------------------------------------------
// Discovery & Dynamic Client Registration
// ---------------------------------------------------------------------------

// Discover walks the cascade documented in plan §7. Given the MCP base
// URL, it tries the protected-resource hint, then well-known endpoints
// on the resource origin, then OIDC. Returns ASMetadata or an error.
func (s *Service) Discover(ctx context.Context, mcpURL string) (*ASMetadata, error) {
	u, err := url.Parse(mcpURL)
	if err != nil {
		return nil, err
	}
	// 1) Protected-resource hint.
	if pr, err := s.fetchPR(ctx, u); err == nil && pr != nil && len(pr.AuthorizationServers) > 0 {
		for _, asURL := range pr.AuthorizationServers {
			if md, err := s.fetchAS(ctx, asURL); err == nil && md != nil && md.AuthorizationEndpoint != "" {
				return md, nil
			}
		}
	}
	// 2) AS metadata at the resource origin.
	asURL := u.Scheme + "://" + u.Host
	if md, err := s.fetchAS(ctx, asURL); err == nil && md != nil && md.AuthorizationEndpoint != "" {
		return md, nil
	}
	return nil, fmt.Errorf("oauth: could not discover AS metadata for %s", mcpURL)
}

func (s *Service) fetchPR(ctx context.Context, u *url.URL) (*PRMetadata, error) {
	candidates := []string{
		u.Scheme + "://" + u.Host + "/.well-known/oauth-protected-resource",
		u.Scheme + "://" + u.Host + strings.TrimRight(u.Path, "/") + "/.well-known/oauth-protected-resource",
	}
	for _, c := range candidates {
		if md, err := s.getJSON(ctx, c); err == nil {
			out := &PRMetadata{}
			if json.Unmarshal(md, out) == nil && len(out.AuthorizationServers) > 0 {
				return out, nil
			}
		}
	}
	return nil, errors.New("no protected-resource metadata")
}

func (s *Service) fetchAS(ctx context.Context, asURL string) (*ASMetadata, error) {
	candidates := []string{
		strings.TrimRight(asURL, "/") + "/.well-known/oauth-authorization-server",
		strings.TrimRight(asURL, "/") + "/.well-known/openid-configuration",
	}
	for _, c := range candidates {
		body, err := s.getJSON(ctx, c)
		if err != nil {
			continue
		}
		out := &ASMetadata{}
		if err := json.Unmarshal(body, out); err == nil && out.AuthorizationEndpoint != "" && out.TokenEndpoint != "" {
			return out, nil
		}
	}
	return nil, errors.New("no AS metadata")
}

func (s *Service) getJSON(ctx context.Context, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := s.httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("GET %s: %s", u, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 256<<10))
}

// Register attempts Dynamic Client Registration (RFC 7591). If the AS
// doesn't expose a registration endpoint, the caller must persist a
// pre-issued client_id (and optional client_secret) themselves via
// PutClient.
func (s *Service) Register(ctx context.Context, md *ASMetadata, redirectURI string, scopes []string) (clientID, clientSecret, authMethod string, err error) {
	if md.RegistrationEndpoint == "" {
		return "", "", "", errors.New("oauth: AS does not expose registration_endpoint; supply client_id manually")
	}
	body := map[string]any{
		"client_name":                "toolyard",
		"redirect_uris":              []string{redirectURI},
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none", // PKCE-only public client
		"scope":                      strings.Join(scopes, " "),
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, md.RegistrationEndpoint, strings.NewReader(string(raw)))
	if err != nil {
		return "", "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := s.httpc.Do(req)
	if err != nil {
		return "", "", "", err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode/100 != 2 {
		return "", "", "", fmt.Errorf("oauth register %s: %s — %s", md.RegistrationEndpoint, resp.Status, string(rb))
	}
	var out struct {
		ClientID                string `json:"client_id"`
		ClientSecret            string `json:"client_secret"`
		TokenEndpointAuthMethod string `json:"token_endpoint_auth_method"`
	}
	if err := json.Unmarshal(rb, &out); err != nil {
		return "", "", "", err
	}
	method := out.TokenEndpointAuthMethod
	if method == "" {
		if out.ClientSecret != "" {
			method = "client_secret_post"
		} else {
			method = "none"
		}
	}
	return out.ClientID, out.ClientSecret, method, nil
}

// PutClient upserts the per-upstream OAuth client record.
func (s *Service) PutClient(ctx context.Context, rec ClientRecord) error {
	secEnc, err := s.cipher.Seal([]byte(rec.ClientSecret), AAD(rec.UpstreamName, "client_secret"))
	if err != nil {
		return err
	}
	mdJSON := ""
	if rec.Metadata != nil {
		b, _ := json.Marshal(rec.Metadata)
		mdJSON = string(b)
	}
	now := time.Now().UnixMilli()
	scopes := strings.Join(rec.Scopes, " ")
	_, err = s.db.ExecContext(ctx, `
        INSERT INTO oauth_clients(upstream_name, issuer, authorization_endpoint, token_endpoint,
            registration_endpoint, revocation_endpoint, device_authorization_endpoint,
            client_id, client_secret_enc, redirect_uri, scopes,
            token_endpoint_auth_method, metadata_json, registered_at, updated_at)
        VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
        ON CONFLICT(upstream_name) DO UPDATE SET
            issuer = excluded.issuer,
            authorization_endpoint = excluded.authorization_endpoint,
            token_endpoint = excluded.token_endpoint,
            registration_endpoint = excluded.registration_endpoint,
            revocation_endpoint = excluded.revocation_endpoint,
            device_authorization_endpoint = excluded.device_authorization_endpoint,
            client_id = excluded.client_id,
            client_secret_enc = excluded.client_secret_enc,
            redirect_uri = excluded.redirect_uri,
            scopes = excluded.scopes,
            token_endpoint_auth_method = excluded.token_endpoint_auth_method,
            metadata_json = excluded.metadata_json,
            updated_at = excluded.updated_at
    `,
		rec.UpstreamName, rec.Issuer, rec.AuthorizationEndpoint, rec.TokenEndpoint,
		nullStr(rec.RegistrationEndpoint), nullStr(rec.RevocationEndpoint),
		nullStr(rec.DeviceAuthorizationEndpoint),
		rec.ClientID, nullStr(secEnc), rec.RedirectURI, scopes,
		rec.TokenEndpointAuthMethod, nullStr(mdJSON), now, now)
	return err
}

// GetClient loads the per-upstream OAuth client.
func (s *Service) GetClient(ctx context.Context, upstream string) (*ClientRecord, error) {
	row := s.db.QueryRowContext(ctx, `
        SELECT upstream_name, issuer, authorization_endpoint, token_endpoint,
               COALESCE(registration_endpoint,''), COALESCE(revocation_endpoint,''),
               COALESCE(device_authorization_endpoint,''),
               client_id, COALESCE(client_secret_enc,''), redirect_uri, scopes,
               token_endpoint_auth_method, COALESCE(metadata_json,'')
        FROM oauth_clients WHERE upstream_name = ?
    `, upstream)
	var rec ClientRecord
	var secEnc, mdJSON, scopes string
	if err := row.Scan(&rec.UpstreamName, &rec.Issuer, &rec.AuthorizationEndpoint, &rec.TokenEndpoint,
		&rec.RegistrationEndpoint, &rec.RevocationEndpoint, &rec.DeviceAuthorizationEndpoint,
		&rec.ClientID, &secEnc, &rec.RedirectURI, &scopes, &rec.TokenEndpointAuthMethod, &mdJSON); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrClientNotFound
		}
		return nil, err
	}
	if secEnc != "" {
		pt, err := s.cipher.Open(secEnc, AAD(upstream, "client_secret"))
		if err != nil {
			return nil, err
		}
		rec.ClientSecret = string(pt)
	}
	if scopes != "" {
		rec.Scopes = strings.Fields(scopes)
	}
	if mdJSON != "" {
		md := &ASMetadata{}
		if json.Unmarshal([]byte(mdJSON), md) == nil {
			rec.Metadata = md
		}
	}
	return &rec, nil
}

// DeleteClient drops the OAuth client + token rows for an upstream. Used
// when the upstream itself is removed.
func (s *Service) DeleteClient(ctx context.Context, upstream string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM oauth_clients WHERE upstream_name = ?`, upstream)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM oauth_tokens WHERE upstream_name = ?`, upstream)
	s.mu.Lock()
	delete(s.bearers, upstream)
	s.mu.Unlock()
	return err
}

// ---------------------------------------------------------------------------
// Pending OAuth flows (state -> verifier mapping)
// ---------------------------------------------------------------------------

// BeginCallback creates a pending row for Mode A or Mode B (both use
// PKCE; B just receives the code via a paste form rather than a direct
// browser redirect). Returns the authorization URL the dashboard should
// open.
func (s *Service) BeginCallback(ctx context.Context, upstream, userID, mode string, scopes []string) (authURL, state string, err error) {
	if mode != ModeCallback && mode != ModePaste {
		return "", "", fmt.Errorf("oauth: bad mode %q", mode)
	}
	cli, err := s.GetClient(ctx, upstream)
	if err != nil {
		return "", "", err
	}
	state, err = randomString(32)
	if err != nil {
		return "", "", err
	}
	verifier, err := randomString(48)
	if err != nil {
		return "", "", err
	}
	challenge := codeChallenge(verifier)
	now := time.Now()
	_, err = s.db.ExecContext(ctx, `
        INSERT INTO oauth_pending(state, upstream_name, user_id, mode, code_verifier,
            expires_at, created_at)
        VALUES(?,?,?,?,?,?,?)
    `, state, upstream, userID, mode, verifier, now.Add(PendingTTL).UnixMilli(), now.UnixMilli())
	if err != nil {
		return "", "", err
	}

	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", cli.ClientID)
	q.Set("redirect_uri", cli.RedirectURI)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("state", state)
	if len(scopes) == 0 {
		scopes = cli.Scopes
	}
	if len(scopes) > 0 {
		q.Set("scope", strings.Join(scopes, " "))
	}
	// Encourage IdPs to issue a refresh_token without forcing reconsent
	// on every connect — both keys are commonly accepted, harmless when ignored.
	q.Set("access_type", "offline")
	q.Set("prompt", "consent")

	sep := "?"
	if strings.Contains(cli.AuthorizationEndpoint, "?") {
		sep = "&"
	}
	authURL = cli.AuthorizationEndpoint + sep + q.Encode()
	return authURL, state, nil
}

// LoadPending fetches a pending row by state. Returns ErrPendingNotFound
// if missing or expired.
func (s *Service) LoadPending(ctx context.Context, state string) (*PendingRecord, error) {
	row := s.db.QueryRowContext(ctx, `
        SELECT state, upstream_name, user_id, mode, COALESCE(code_verifier,''),
               COALESCE(device_code,''), COALESCE(user_code,''),
               COALESCE(interval_s,0), COALESCE(verification_uri,''),
               expires_at, created_at
        FROM oauth_pending WHERE state = ?
    `, state)
	var p PendingRecord
	var exp, created int64
	if err := row.Scan(&p.State, &p.UpstreamName, &p.UserID, &p.Mode, &p.CodeVerifier,
		&p.DeviceCode, &p.UserCode, &p.IntervalS, &p.VerificationURI, &exp, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrPendingNotFound
		}
		return nil, err
	}
	p.ExpiresAt = time.UnixMilli(exp)
	p.CreatedAt = time.UnixMilli(created)
	if time.Now().After(p.ExpiresAt) {
		// Expired — clean up and report as not found so the API layer
		// returns a uniform 400 either way.
		_, _ = s.db.ExecContext(ctx, `DELETE FROM oauth_pending WHERE state = ?`, state)
		return nil, ErrPendingNotFound
	}
	return &p, nil
}

// DeletePending removes a pending row. Called on success and on hard error.
func (s *Service) DeletePending(ctx context.Context, state string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM oauth_pending WHERE state = ?`, state)
	return err
}

// PurgeExpiredPending drops anything past expires_at. Run from a tick.
func (s *Service) PurgeExpiredPending(ctx context.Context) (int, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM oauth_pending WHERE expires_at < ?`, time.Now().UnixMilli())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// ---------------------------------------------------------------------------
// Token exchange & refresh
// ---------------------------------------------------------------------------

type tokenResponse struct {
	AccessToken      string `json:"access_token"`
	TokenType        string `json:"token_type"`
	ExpiresIn        int    `json:"expires_in"`
	RefreshToken     string `json:"refresh_token"`
	RefreshExpiresIn int    `json:"refresh_expires_in"`
	Scope            string `json:"scope"`
	IDToken          string `json:"id_token"`
	Error            string `json:"error"`
	ErrorDesc        string `json:"error_description"`
}

// ExchangeCode performs the authorization-code -> tokens swap. On success
// the encrypted token row is persisted and the in-memory bearer is updated.
func (s *Service) ExchangeCode(ctx context.Context, upstream, code, verifier string) (*TokenRecord, error) {
	cli, err := s.GetClient(ctx, upstream)
	if err != nil {
		return nil, err
	}
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", cli.RedirectURI)
	form.Set("client_id", cli.ClientID)
	form.Set("code_verifier", verifier)
	if cli.TokenEndpointAuthMethod == "client_secret_post" && cli.ClientSecret != "" {
		form.Set("client_secret", cli.ClientSecret)
	}
	tr, err := s.postToken(ctx, cli, form)
	if err != nil {
		return nil, err
	}
	rec := tokenRecordFromResponse(upstream, tr)
	if err := s.PutToken(ctx, rec); err != nil {
		return nil, err
	}
	s.setBearer(upstream, rec.AccessToken)
	if s.bus != nil {
		s.bus.Publish("mcp_oauth_done", map[string]any{
			"upstream": upstream,
			"state":    rec.State,
		})
	}
	return rec, nil
}

// Refresh swaps a refresh_token for a fresh access_token. On rotation the
// new refresh_token replaces the old.
func (s *Service) Refresh(ctx context.Context, upstream string) (*TokenRecord, error) {
	cli, err := s.GetClient(ctx, upstream)
	if err != nil {
		return nil, err
	}
	cur, err := s.GetToken(ctx, upstream)
	if err != nil {
		return nil, err
	}
	if cur == nil || cur.IsPAT || cur.RefreshToken == "" {
		return nil, errors.New("oauth: no refresh token available")
	}
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", cur.RefreshToken)
	form.Set("client_id", cli.ClientID)
	if cli.TokenEndpointAuthMethod == "client_secret_post" && cli.ClientSecret != "" {
		form.Set("client_secret", cli.ClientSecret)
	}
	tr, err := s.postToken(ctx, cli, form)
	if err != nil {
		// Surface invalid_grant as a hard reauth signal.
		if strings.Contains(err.Error(), "invalid_grant") {
			s.markNeedsReauth(ctx, upstream, err.Error())
			return nil, ErrNeedsReauth
		}
		return nil, err
	}
	rec := tokenRecordFromResponse(upstream, tr)
	// Some IdPs don't rotate refresh tokens — keep the existing one.
	if rec.RefreshToken == "" {
		rec.RefreshToken = cur.RefreshToken
		rec.RefreshExpiresAt = cur.RefreshExpiresAt
	}
	rec.LastRefreshAt = time.Now()
	if err := s.PutToken(ctx, rec); err != nil {
		return nil, err
	}
	s.setBearer(upstream, rec.AccessToken)
	if s.bus != nil {
		s.bus.Publish("mcp_oauth_refreshed", map[string]any{
			"upstream":          upstream,
			"access_expires_at": rec.AccessExpiresAt.UnixMilli(),
		})
	}
	return rec, nil
}

func (s *Service) postToken(ctx context.Context, cli *ClientRecord, form url.Values) (*tokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cli.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if cli.TokenEndpointAuthMethod == "client_secret_basic" && cli.ClientSecret != "" {
		req.SetBasicAuth(cli.ClientID, cli.ClientSecret)
	}
	resp, err := s.httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	tr := &tokenResponse{}
	if json.Unmarshal(body, tr) != nil && len(body) > 0 {
		return nil, fmt.Errorf("oauth token endpoint: %s — %s", resp.Status, string(body))
	}
	if tr.Error != "" {
		return nil, fmt.Errorf("oauth: %s — %s", tr.Error, tr.ErrorDesc)
	}
	if resp.StatusCode/100 != 2 || tr.AccessToken == "" {
		return nil, fmt.Errorf("oauth token endpoint: %s — %s", resp.Status, string(body))
	}
	return tr, nil
}

func tokenRecordFromResponse(upstream string, tr *tokenResponse) *TokenRecord {
	now := time.Now()
	rec := &TokenRecord{
		UpstreamName: upstream,
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		TokenType:    tr.TokenType,
		Scope:        tr.Scope,
		ObtainedAt:   now,
		State:        StateActive,
	}
	if tr.ExpiresIn > 0 {
		rec.AccessExpiresAt = now.Add(time.Duration(tr.ExpiresIn) * time.Second)
	}
	if tr.RefreshExpiresIn > 0 {
		rec.RefreshExpiresAt = now.Add(time.Duration(tr.RefreshExpiresIn) * time.Second)
	}
	return rec
}

// PutToken upserts the encrypted token row.
func (s *Service) PutToken(ctx context.Context, rec *TokenRecord) error {
	atEnc, err := s.cipher.Seal([]byte(rec.AccessToken), AAD(rec.UpstreamName, "access"))
	if err != nil {
		return err
	}
	rtEnc, err := s.cipher.Seal([]byte(rec.RefreshToken), AAD(rec.UpstreamName, "refresh"))
	if err != nil {
		return err
	}
	state := rec.State
	if state == "" {
		state = StateActive
	}
	pat := 0
	if rec.IsPAT {
		pat = 1
	}
	_, err = s.db.ExecContext(ctx, `
        INSERT INTO oauth_tokens(upstream_name, access_token_enc, refresh_token_enc,
            token_type, scope, obtained_at, access_expires_at, refresh_expires_at,
            last_refresh_at, refresh_failures, state, last_error, is_pat)
        VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)
        ON CONFLICT(upstream_name) DO UPDATE SET
            access_token_enc = excluded.access_token_enc,
            refresh_token_enc = excluded.refresh_token_enc,
            token_type = excluded.token_type,
            scope = excluded.scope,
            obtained_at = excluded.obtained_at,
            access_expires_at = excluded.access_expires_at,
            refresh_expires_at = excluded.refresh_expires_at,
            last_refresh_at = excluded.last_refresh_at,
            refresh_failures = excluded.refresh_failures,
            state = excluded.state,
            last_error = excluded.last_error,
            is_pat = excluded.is_pat
    `,
		rec.UpstreamName, nullStr(atEnc), nullStr(rtEnc),
		nullStr(rec.TokenType), nullStr(rec.Scope),
		nullTimeMS(rec.ObtainedAt), nullTimeMS(rec.AccessExpiresAt), nullTimeMS(rec.RefreshExpiresAt),
		nullTimeMS(rec.LastRefreshAt), rec.RefreshFailures, state, nullStr(rec.LastError), pat)
	return err
}

// GetToken returns the decrypted token row (or nil when missing).
func (s *Service) GetToken(ctx context.Context, upstream string) (*TokenRecord, error) {
	row := s.db.QueryRowContext(ctx, `
        SELECT upstream_name, COALESCE(access_token_enc,''), COALESCE(refresh_token_enc,''),
               COALESCE(token_type,''), COALESCE(scope,''),
               COALESCE(obtained_at,0), COALESCE(access_expires_at,0),
               COALESCE(refresh_expires_at,0), COALESCE(last_refresh_at,0),
               refresh_failures, state, COALESCE(last_error,''), is_pat
        FROM oauth_tokens WHERE upstream_name = ?
    `, upstream)
	var rec TokenRecord
	var atEnc, rtEnc string
	var ob, ax, rx, lr int64
	var pat int
	if err := row.Scan(&rec.UpstreamName, &atEnc, &rtEnc, &rec.TokenType, &rec.Scope,
		&ob, &ax, &rx, &lr, &rec.RefreshFailures, &rec.State, &rec.LastError, &pat); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if atEnc != "" {
		pt, err := s.cipher.Open(atEnc, AAD(upstream, "access"))
		if err != nil {
			return nil, err
		}
		rec.AccessToken = string(pt)
	}
	if rtEnc != "" {
		pt, err := s.cipher.Open(rtEnc, AAD(upstream, "refresh"))
		if err != nil {
			return nil, err
		}
		rec.RefreshToken = string(pt)
	}
	if ob > 0 {
		rec.ObtainedAt = time.UnixMilli(ob)
	}
	if ax > 0 {
		rec.AccessExpiresAt = time.UnixMilli(ax)
	}
	if rx > 0 {
		rec.RefreshExpiresAt = time.UnixMilli(rx)
	}
	if lr > 0 {
		rec.LastRefreshAt = time.UnixMilli(lr)
	}
	rec.IsPAT = pat == 1
	return &rec, nil
}

// PutPAT stores a personal access token / API key as the access_token
// with no refresh metadata. The refresher ignores PAT rows.
func (s *Service) PutPAT(ctx context.Context, upstream, pat string) error {
	rec := &TokenRecord{
		UpstreamName: upstream,
		AccessToken:  pat,
		TokenType:    "Bearer",
		ObtainedAt:   time.Now(),
		State:        StateActive,
		IsPAT:        true,
	}
	if err := s.PutToken(ctx, rec); err != nil {
		return err
	}
	s.setBearer(upstream, pat)
	if s.bus != nil {
		s.bus.Publish("mcp_oauth_done", map[string]any{"upstream": upstream})
	}
	return nil
}

// markNeedsReauth flips the token row to needs_reauth, fires SSE + push.
func (s *Service) markNeedsReauth(ctx context.Context, upstream, msg string) {
	_, _ = s.db.ExecContext(ctx, `
        UPDATE oauth_tokens
        SET state = ?, last_error = ?, refresh_failures = refresh_failures + 1
        WHERE upstream_name = ?`, StateNeedsReauth, msg, upstream)
	s.setBearer(upstream, "")
	s.mu.RLock()
	hook := s.reauthHook
	s.mu.RUnlock()
	if hook != nil {
		go hook(upstream)
	}
	if s.bus != nil {
		s.bus.Publish("mcp_oauth_needs_reauth", map[string]any{
			"upstream": upstream,
			"error":    msg,
		})
	}
	if s.notifier != nil && s.idents != nil {
		uid, err := s.idents.PrimaryUserID(ctx)
		if err == nil {
			_ = s.notifier.Notify(ctx, uid, map[string]any{
				"title": "toolyard: " + upstream + " needs reauthorization",
				"body":  "Open the dashboard and reauthorize.",
				"url":   "/?reauth=" + upstream,
			})
		}
	}
}

// MarkReauthExternal is the public entry point used by upstreams.Service
// when the streamable-http connect fails with 401. Idempotent.
func (s *Service) MarkReauthExternal(ctx context.Context, upstream, msg string) {
	s.markNeedsReauth(ctx, upstream, msg)
}

// Disconnect optionally calls the revocation endpoint, then deletes the
// token + client rows. Used by /v1/upstreams/{id}/oauth DELETE.
func (s *Service) Disconnect(ctx context.Context, upstream string) error {
	cli, _ := s.GetClient(ctx, upstream)
	tok, _ := s.GetToken(ctx, upstream)
	if cli != nil && cli.RevocationEndpoint != "" && tok != nil && (tok.RefreshToken != "" || tok.AccessToken != "") {
		form := url.Values{}
		token := tok.RefreshToken
		hint := "refresh_token"
		if token == "" {
			token = tok.AccessToken
			hint = "access_token"
		}
		form.Set("token", token)
		form.Set("token_type_hint", hint)
		form.Set("client_id", cli.ClientID)
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, cli.RevocationEndpoint,
			strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		// Best-effort.
		if resp, err := s.httpc.Do(req); err == nil {
			resp.Body.Close()
		}
	}
	return s.DeleteClient(ctx, upstream)
}

// ---------------------------------------------------------------------------
// Device flow (RFC 8628)
// ---------------------------------------------------------------------------

// DeviceAuthResponse is the IdP's reply to /device_authorization.
type DeviceAuthResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// BeginDevice initiates RFC 8628 with the IdP and persists a pending row.
// The polling is run by PollDevice on the same state token.
func (s *Service) BeginDevice(ctx context.Context, upstream, userID string, scopes []string) (*PendingRecord, error) {
	cli, err := s.GetClient(ctx, upstream)
	if err != nil {
		return nil, err
	}
	if cli.DeviceAuthorizationEndpoint == "" {
		return nil, errors.New("oauth: AS does not support device flow")
	}
	form := url.Values{}
	form.Set("client_id", cli.ClientID)
	if len(scopes) == 0 {
		scopes = cli.Scopes
	}
	if len(scopes) > 0 {
		form.Set("scope", strings.Join(scopes, " "))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cli.DeviceAuthorizationEndpoint,
		strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := s.httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("device authorization: %s — %s", resp.Status, string(body))
	}
	var dr DeviceAuthResponse
	if err := json.Unmarshal(body, &dr); err != nil {
		return nil, err
	}
	state, err := randomString(32)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	expires := now.Add(time.Duration(maxInt(dr.ExpiresIn, 600)) * time.Second)
	if expires.Sub(now) > 30*time.Minute {
		expires = now.Add(30 * time.Minute)
	}
	verification := dr.VerificationURIComplete
	if verification == "" {
		verification = dr.VerificationURI
	}
	interval := dr.Interval
	if interval <= 0 {
		interval = 5
	}
	_, err = s.db.ExecContext(ctx, `
        INSERT INTO oauth_pending(state, upstream_name, user_id, mode, code_verifier,
            device_code, user_code, interval_s, verification_uri, expires_at, created_at)
        VALUES(?,?,?,?,?,?,?,?,?,?,?)
    `, state, upstream, userID, ModeDevice, "",
		dr.DeviceCode, dr.UserCode, interval, verification, expires.UnixMilli(), now.UnixMilli())
	if err != nil {
		return nil, err
	}
	return &PendingRecord{
		State:           state,
		UpstreamName:    upstream,
		UserID:          userID,
		Mode:            ModeDevice,
		DeviceCode:      dr.DeviceCode,
		UserCode:        dr.UserCode,
		IntervalS:       interval,
		VerificationURI: verification,
		ExpiresAt:       expires,
		CreatedAt:       now,
	}, nil
}

// PollDevice attempts a single poll against the AS's token endpoint with
// device_code grant. Returns (record, nil) on success, (nil, nil) on
// authorization_pending, error otherwise.
func (s *Service) PollDevice(ctx context.Context, state string) (*TokenRecord, error) {
	p, err := s.LoadPending(ctx, state)
	if err != nil {
		return nil, err
	}
	if p.Mode != ModeDevice {
		return nil, ErrFlowMismatch
	}
	cli, err := s.GetClient(ctx, p.UpstreamName)
	if err != nil {
		return nil, err
	}
	form := url.Values{}
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:device_code")
	form.Set("device_code", p.DeviceCode)
	form.Set("client_id", cli.ClientID)
	tr, err := s.postToken(ctx, cli, form)
	if err != nil {
		// authorization_pending and slow_down are normal mid-poll signals.
		if strings.Contains(err.Error(), "authorization_pending") || strings.Contains(err.Error(), "slow_down") {
			return nil, nil
		}
		return nil, err
	}
	rec := tokenRecordFromResponse(p.UpstreamName, tr)
	if err := s.PutToken(ctx, rec); err != nil {
		return nil, err
	}
	s.setBearer(p.UpstreamName, rec.AccessToken)
	_ = s.DeletePending(ctx, state)
	if s.bus != nil {
		s.bus.Publish("mcp_oauth_done", map[string]any{"upstream": p.UpstreamName})
	}
	return rec, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func randomString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func codeChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullTimeMS(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UnixMilli()
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
