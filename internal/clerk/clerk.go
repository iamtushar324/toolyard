// Package clerk verifies Clerk session tokens and checks organisation
// membership through the Clerk Backend API. It is the server half of the
// dashboard's "Sign in with Google" flow: the browser obtains a short-lived
// session JWT from Clerk and POSTs it to toolyard, which verifies the
// signature against the instance's JWKS and then confirms the user belongs
// to the configured organisation before issuing its own session.
//
// Same instance and organisation as bkt3, so "internal user" means exactly
// one thing across both apps.
package clerk

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

var (
	// ErrInvalidToken: the session token failed verification (bad signature,
	// expired, wrong issuer/audience, unknown key, malformed).
	ErrInvalidToken = errors.New("clerk: invalid session token")
	// ErrNotMember: the Clerk user is real but not in the configured org.
	ErrNotMember = errors.New("clerk: not an organization member")
	// ErrUnavailable: the JWKS or Backend API could not be reached or
	// answered with an error. Callers must fail closed without blocking
	// anyone (a Clerk outage is not an offboarding).
	ErrUnavailable = errors.New("clerk: service unavailable")
)

const (
	defaultAPIBase = "https://api.clerk.com/v1"
	// jwksRefetchMinInterval bounds how often an unknown kid may trigger a
	// JWKS round trip after a successful fetch, so a flood of forged tokens
	// can't turn into a flood of requests to Clerk.
	jwksRefetchMinInterval = time.Minute
	// jwksFailureBackoff is how long a failed JWKS fetch is trusted to
	// still be failing: verifications that need the keys answer
	// ErrUnavailable meanwhile instead of hammering Clerk, then retry.
	jwksFailureBackoff = 5 * time.Second
	// tokenLeeway absorbs clock skew between us and Clerk. Session tokens
	// live about 60 s, so keep it small.
	tokenLeeway = 10 * time.Second
	// connectMaxTokenAge bounds how long ago (by iat, plus tokenLeeway) a
	// token VerifySessionTokenFor accepts was issued: the connect flow
	// sends a freshly minted token, and anything older is a replay.
	connectMaxTokenAge = 2 * time.Minute
	// membershipPageSize is the page size for a user's own memberships (a
	// person is in a handful of orgs at most).
	membershipPageSize = 100
	// membersPageSize is the Backend API maximum for listing an org's
	// members.
	membersPageSize = 500
	maxResponseBody = 8 << 20
)

// hostRE accepts a bare DNS name with at least one dot: no scheme, path,
// port or userinfo, which is all a publishable key should ever encode.
var hostRE = regexp.MustCompile(`^(?i:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)(?:\.(?i:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?))+$`)

// FrontendAPI decodes the Frontend API hostname embedded in a publishable
// key: base64 of "<host>$" after the pk_live_ / pk_test_ prefix.
func FrontendAPI(publishableKey string) (string, error) {
	var enc string
	switch {
	case strings.HasPrefix(publishableKey, "pk_live_"):
		enc = strings.TrimPrefix(publishableKey, "pk_live_")
	case strings.HasPrefix(publishableKey, "pk_test_"):
		enc = strings.TrimPrefix(publishableKey, "pk_test_")
	default:
		return "", errors.New("clerk: publishable key must start with pk_live_ or pk_test_")
	}
	if enc == "" {
		return "", errors.New("clerk: publishable key has no payload")
	}
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		raw, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(enc, "="))
	}
	if err != nil {
		return "", fmt.Errorf("clerk: publishable key is not base64: %w", err)
	}
	host := strings.TrimSuffix(string(raw), "$")
	if !hostRE.MatchString(host) {
		return "", fmt.Errorf("clerk: publishable key does not encode a hostname (%q)", host)
	}
	return strings.ToLower(host), nil
}

// Config builds a Client. SecretKey, PublishableKey, OrganizationID and
// AuthorizedParties are required. The remaining fields have defaults and
// exist so tests can point the client at an httptest server.
type Config struct {
	SecretKey      string
	PublishableKey string
	OrganizationID string
	// AllowedGoogleEmail selects personal access instead of organization membership.
	AllowedGoogleEmail string
	// AuthorizedParties is the set of browser origins whose tokens we
	// accept (the azp claim). In production this is the dashboard's
	// public URL origin.
	AuthorizedParties []string
	HTTPClient        *http.Client
	// APIBase is the Backend API root. Default https://api.clerk.com/v1.
	APIBase string
	// JWKSURL and Issuer default to https://<frontend-api>/.well-known/jwks.json
	// and https://<frontend-api>.
	JWKSURL string
	Issuer  string
}

// Client talks to one Clerk instance.
type Client struct {
	secretKey          string
	publishableKey     string
	frontendAPI        string
	orgID              string
	allowedGoogleEmail string
	azp                map[string]bool
	http               *http.Client
	apiBase            string
	jwksURL            string
	issuer             string
	now                func() time.Time

	mu          sync.Mutex
	keys        map[string]*rsa.PublicKey
	lastSuccess time.Time // last JWKS fetch that returned keys; zero before the first
	lastFailure time.Time // last JWKS fetch that failed; zero when the last attempt succeeded
}

// New validates cfg and returns a Client. No network call is made here; the
// JWKS is fetched lazily on the first verification.
func New(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.SecretKey) == "" {
		return nil, errors.New("clerk: secret key is required")
	}
	if strings.TrimSpace(cfg.OrganizationID) == "" && strings.TrimSpace(cfg.AllowedGoogleEmail) == "" {
		return nil, errors.New("clerk: organization id is required")
	}
	fapi, err := FrontendAPI(cfg.PublishableKey)
	if err != nil {
		return nil, err
	}
	azp := partySet(cfg.AuthorizedParties)
	if len(azp) == 0 {
		return nil, errors.New("clerk: at least one authorized party (the dashboard origin) is required")
	}
	c := &Client{
		secretKey:          cfg.SecretKey,
		publishableKey:     cfg.PublishableKey,
		frontendAPI:        fapi,
		orgID:              cfg.OrganizationID,
		allowedGoogleEmail: strings.TrimSpace(cfg.AllowedGoogleEmail),
		azp:                azp,
		http:               cfg.HTTPClient,
		apiBase:            strings.TrimRight(cfg.APIBase, "/"),
		jwksURL:            cfg.JWKSURL,
		issuer:             cfg.Issuer,
		now:                time.Now,
		keys:               map[string]*rsa.PublicKey{},
	}
	if c.http == nil {
		c.http = &http.Client{Timeout: 10 * time.Second}
	}
	if c.apiBase == "" {
		c.apiBase = defaultAPIBase
	}
	if c.jwksURL == "" {
		c.jwksURL = "https://" + fapi + "/.well-known/jwks.json"
	}
	if c.issuer == "" {
		c.issuer = "https://" + fapi
	}
	return c, nil
}

// PublishableKey is what the browser needs to load Clerk.
func (c *Client) PublishableKey() string { return c.publishableKey }

// FrontendAPI is the instance hostname (for CSP and for the browser).
func (c *Client) FrontendAPI() string { return c.frontendAPI }

// Claims is the verified subset of a Clerk session token we care about.
type Claims struct {
	Subject         string // Clerk user id (user_…)
	SessionID       string // Clerk session id (sess_…)
	AuthorizedParty string // browser origin the token was minted for
}

type sessionClaims struct {
	SessionID       string `json:"sid"`
	AuthorizedParty string `json:"azp"`
	// Aud is the raw aud claim, kept only so VerifySessionTokenFor can
	// notice one is present (Clerk session tokens carry none; JWT
	// templates usually do). It shadows RegisteredClaims.Audience, which
	// nothing here checks: no parser option asks for an audience.
	Aud json.RawMessage `json:"aud,omitempty"`
	jwt.RegisteredClaims
}

// NormalizeOrigin is the form an azp origin is compared in: trimmed,
// lowercase, without a trailing slash.
func NormalizeOrigin(origin string) string {
	return strings.ToLower(strings.TrimRight(strings.TrimSpace(origin), "/"))
}

// partySet is the normalised set of origins; empty entries are dropped.
func partySet(origins []string) map[string]bool {
	set := map[string]bool{}
	for _, o := range origins {
		if o = NormalizeOrigin(o); o != "" {
			set[o] = true
		}
	}
	return set
}

// TailnetPartyPrefix marks an authorized-party entry that admits a whole
// Tailscale tailnet instead of one origin: "tailnet:<tailnet>.ts.net"
// accepts any http or https origin, on any port, whose host is a name
// under that tailnet or a Tailscale address (100.64.0.0/10,
// fd7a:115c:a1e0::/48). Traffic inside a tailnet is encrypted by
// WireGuard, so plain http is accepted there.
const TailnetPartyPrefix = "tailnet:"

var (
	tailnetV4 = mustCIDR("100.64.0.0/10")
	tailnetV6 = mustCIDR("fd7a:115c:a1e0::/48")
)

func mustCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return n
}

// parties is an authorized-party matcher: exact origins, whole tailnets,
// and domain wildcards ("https://*.example.com": any host under
// example.com, at any depth and on any port, with that scheme only).
type parties struct {
	exact     map[string]bool
	tailnets  []string // lowercase tailnet DNS suffixes, e.g. "tailab6257.ts.net"
	wildcards []wildcardParty
}

type wildcardParty struct{ scheme, suffix string }

// parseWildcardParty reads "scheme://*.domain" (no port, path or
// credentials); ok is false for anything else.
func parseWildcardParty(e string) (wildcardParty, bool) {
	scheme, rest, ok := strings.Cut(e, "://")
	if !ok || (scheme != "https" && scheme != "http") {
		return wildcardParty{}, false
	}
	suffix, ok := strings.CutPrefix(rest, "*.")
	if !ok || suffix == "" || strings.ContainsAny(suffix, "/:@?#*") {
		return wildcardParty{}, false
	}
	return wildcardParty{scheme: scheme, suffix: suffix}, true
}

func newParties(entries []string) parties {
	p := parties{exact: map[string]bool{}}
	for _, e := range entries {
		e = NormalizeOrigin(e)
		if suffix, ok := strings.CutPrefix(e, TailnetPartyPrefix); ok {
			if suffix = strings.Trim(suffix, "."); suffix != "" {
				p.tailnets = append(p.tailnets, suffix)
			}
			continue
		}
		if w, ok := parseWildcardParty(e); ok {
			p.wildcards = append(p.wildcards, w)
			continue
		}
		if e != "" {
			p.exact[e] = true
		}
	}
	return p
}

// allows reports whether a normalised azp origin is admitted.
func (p parties) allows(azp string) bool {
	if azp == "" {
		return false
	}
	if p.exact[azp] {
		return true
	}
	if len(p.tailnets) == 0 && len(p.wildcards) == 0 {
		return false
	}
	u, err := url.Parse(azp)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.Trim(u.Path, "/") != "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if ip := net.ParseIP(host); ip != nil {
		return len(p.tailnets) > 0 && (tailnetV4.Contains(ip) || tailnetV6.Contains(ip))
	}
	for _, w := range p.wildcards {
		if u.Scheme == w.scheme && strings.HasSuffix(host, "."+w.suffix) {
			return true
		}
	}
	for _, suffix := range p.tailnets {
		if strings.HasSuffix(host, "."+suffix) {
			return true
		}
	}
	return false
}

// VerifySessionToken checks signature (RS256 against the instance JWKS),
// exp/nbf, iss, azp and sub. Anything short of a fully valid token is
// ErrInvalidToken; a JWKS fetch failure is ErrUnavailable. The azp must be
// one of the dashboard origins the Client was built with.
func (c *Client) VerifySessionToken(ctx context.Context, token string) (Claims, error) {
	claims, _, err := c.verify(ctx, token, func(azp string) bool { return c.azp[azp] })
	return claims, err
}

// VerifySessionTokenFor is VerifySessionToken against a caller-given set
// of allowed azp origins instead of the dashboard's own: a server-to-server
// flow (POST /v1/connect/t3) accepts tokens another Clerk-backed app on the
// same instance minted in its browser. The dashboard's origins are never
// accepted here, whatever the caller passes, so a token that would open a
// dashboard session can't be replayed into this flow; and VerifySessionToken
// keeps refusing these origins. Same JWKS cache and refetch limits.
//
// Because the answer here is a long-lived credential, the token must also
// look like a fresh Clerk session token: a non-empty sid, an iat no more
// than connectMaxTokenAge ago and not in the future (both give or take
// tokenLeeway), and no aud claim at all.
func (c *Client) VerifySessionTokenFor(ctx context.Context, token string, authorizedParties []string) (Claims, error) {
	allowed := newParties(authorizedParties)
	// The dashboard's own origins never pass here, even when a tailnet
	// entry would cover them.
	claims, sc, err := c.verify(ctx, token, func(azp string) bool { return !c.azp[azp] && allowed.allows(azp) })
	if err != nil {
		return Claims{}, err
	}
	if strings.TrimSpace(sc.SessionID) == "" {
		return Claims{}, fmt.Errorf("%w: missing sid", ErrInvalidToken)
	}
	if len(sc.Aud) > 0 {
		return Claims{}, fmt.Errorf("%w: aud present (not a session token)", ErrInvalidToken)
	}
	if sc.IssuedAt == nil {
		return Claims{}, fmt.Errorf("%w: missing iat", ErrInvalidToken)
	}
	now, iat := c.now(), sc.IssuedAt.Time
	if iat.After(now.Add(tokenLeeway)) {
		return Claims{}, fmt.Errorf("%w: iat in the future", ErrInvalidToken)
	}
	if now.Sub(iat) > connectMaxTokenAge+tokenLeeway {
		return Claims{}, fmt.Errorf("%w: issued %s ago, more than %s", ErrInvalidToken, now.Sub(iat).Round(time.Second), connectMaxTokenAge)
	}
	return claims, nil
}

// verify is the shared check; allowed decides whether the token's
// (normalised) azp is admitted. It also returns the parsed claims for
// VerifySessionTokenFor's extra rules.
func (c *Client) verify(ctx context.Context, token string, allowed func(azp string) bool) (Claims, *sessionClaims, error) {
	token = strings.TrimSpace(token)
	if token == "" || len(token) > 16<<10 {
		return Claims{}, nil, ErrInvalidToken
	}
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{jwt.SigningMethodRS256.Alg()}),
		jwt.WithLeeway(tokenLeeway),
		jwt.WithIssuer(c.issuer),
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(func() time.Time { return c.now() }),
	)
	var unavailable error
	parsed, err := parser.ParseWithClaims(token, &sessionClaims{}, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		if kid == "" {
			return nil, errors.New("missing kid")
		}
		key, kerr := c.keyFor(ctx, kid)
		if errors.Is(kerr, ErrUnavailable) {
			unavailable = kerr
		}
		return key, kerr
	})
	if unavailable != nil {
		return Claims{}, nil, unavailable
	}
	if err != nil || !parsed.Valid {
		return Claims{}, nil, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}
	sc, ok := parsed.Claims.(*sessionClaims)
	if !ok {
		return Claims{}, nil, ErrInvalidToken
	}
	sub := strings.TrimSpace(sc.Subject)
	if sub == "" {
		return Claims{}, nil, fmt.Errorf("%w: missing sub", ErrInvalidToken)
	}
	// Clerk sets azp to the Origin that requested the token. bkt3 skips
	// this check; we don't — a token minted for another Clerk-backed app
	// on the same instance must not open a toolyard session.
	azp := NormalizeOrigin(sc.AuthorizedParty)
	if azp == "" || !allowed(azp) {
		return Claims{}, nil, fmt.Errorf("%w: azp %q not authorized", ErrInvalidToken, sc.AuthorizedParty)
	}
	return Claims{Subject: sub, SessionID: sc.SessionID, AuthorizedParty: azp}, sc, nil
}

// keyFor returns the RSA public key for kid, fetching the JWKS on first use
// and refetching at most once a minute when the kid is unknown (Clerk
// rotates keys rarely; a burst of forged tokens must not turn into a burst
// of requests to Clerk). Only a successful fetch starts that minute: after
// a failed one we don't know whether the kid is bad or Clerk is down, so
// the answer is ErrUnavailable and the fetch is retried after a short
// backoff. Keys already cached keep verifying throughout.
func (c *Client) keyFor(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if k, ok := c.keys[kid]; ok {
		return k, nil
	}
	now := c.now()
	if !c.lastFailure.IsZero() && now.Sub(c.lastFailure) < jwksFailureBackoff {
		return nil, fmt.Errorf("%w: jwks fetch failed recently; retrying shortly", ErrUnavailable)
	}
	if c.lastFailure.IsZero() && !c.lastSuccess.IsZero() && now.Sub(c.lastSuccess) < jwksRefetchMinInterval {
		return nil, fmt.Errorf("%w: unknown kid %q", ErrInvalidToken, kid)
	}
	keys, err := c.fetchJWKS(ctx)
	if err != nil {
		c.lastFailure = now
		return nil, err
	}
	c.lastFailure = time.Time{}
	c.lastSuccess = now
	c.keys = keys
	if k, ok := c.keys[kid]; ok {
		return k, nil
	}
	return nil, fmt.Errorf("%w: unknown kid %q", ErrInvalidToken, kid)
}

type jwks struct {
	Keys []struct {
		Kty string `json:"kty"`
		Kid string `json:"kid"`
		Use string `json:"use"`
		Alg string `json:"alg"`
		N   string `json:"n"`
		E   string `json:"e"`
	} `json:"keys"`
}

// fetchJWKS downloads the key set and parses each RSA key's n/e. Keys of
// other types, or marked for anything but signing, are skipped.
func (c *Client) fetchJWKS(ctx context.Context) (map[string]*rsa.PublicKey, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.jwksURL, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	req.Header.Set("Accept", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: jwks: %v", ErrUnavailable, err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, maxResponseBody))
	if err != nil {
		return nil, fmt.Errorf("%w: jwks: %v", ErrUnavailable, err)
	}
	if res.StatusCode/100 != 2 {
		return nil, fmt.Errorf("%w: jwks: HTTP %d", ErrUnavailable, res.StatusCode)
	}
	var set jwks
	if err := json.Unmarshal(body, &set); err != nil {
		return nil, fmt.Errorf("%w: jwks: %v", ErrUnavailable, err)
	}
	out := make(map[string]*rsa.PublicKey, len(set.Keys))
	for _, k := range set.Keys {
		if k.Kty != "RSA" || k.Kid == "" || (k.Use != "" && k.Use != "sig") || (k.Alg != "" && k.Alg != "RS256") {
			continue
		}
		pub, err := rsaFromJWK(k.N, k.E)
		if err != nil {
			continue
		}
		out[k.Kid] = pub
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: jwks: no usable RSA signing keys", ErrUnavailable)
	}
	return out, nil
}

func rsaFromJWK(n, e string) (*rsa.PublicKey, error) {
	nb, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(n, "="))
	if err != nil {
		return nil, err
	}
	eb, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(e, "="))
	if err != nil {
		return nil, err
	}
	if len(nb) == 0 || len(eb) == 0 || len(eb) > 8 {
		return nil, errors.New("bad n/e")
	}
	exp := new(big.Int).SetBytes(eb)
	if !exp.IsInt64() || exp.Int64() < 3 || exp.Int64() > int64(^uint32(0)) {
		return nil, errors.New("bad exponent")
	}
	mod := new(big.Int).SetBytes(nb)
	if mod.BitLen() < 2048 {
		return nil, errors.New("modulus too small")
	}
	return &rsa.PublicKey{N: mod, E: int(exp.Int64())}, nil
}

// Member is one organisation membership as the Backend API reports it.
type Member struct {
	IsMember  bool
	Role      string // e.g. org:admin, org:member
	Email     string // public_user_data.identifier
	FirstName string
	LastName  string
	ImageURL  string
}

// membershipList mirrors the Backend API's OrganizationMemberships schema:
// GET /v1/users/{id}/organization_memberships and
// GET /v1/organizations/{id}/memberships both return it.
type membershipList struct {
	Data []struct {
		Role         string `json:"role"`
		Organization struct {
			ID string `json:"id"`
		} `json:"organization"`
		PublicUserData *struct {
			UserID     string  `json:"user_id"`
			Identifier *string `json:"identifier"`
			FirstName  *string `json:"first_name"`
			LastName   *string `json:"last_name"`
			ImageURL   string  `json:"image_url"`
		} `json:"public_user_data"`
	} `json:"data"`
	TotalCount int `json:"total_count"`
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// OrgMembership reports whether clerkUserID belongs to the configured
// organisation, with the profile Clerk publishes for that membership.
// ErrNotMember when they don't (or the user id is unknown), ErrUnavailable
// when Clerk can't be asked.
func (c *Client) OrgMembership(ctx context.Context, clerkUserID string) (Member, error) {
	if c.allowedGoogleEmail != "" {
		return c.googleOwner(ctx, clerkUserID)
	}
	clerkUserID = strings.TrimSpace(clerkUserID)
	if clerkUserID == "" {
		return Member{}, ErrNotMember
	}
	path := "/users/" + url.PathEscape(clerkUserID) + "/organization_memberships"
	// Advance by the rows actually returned, not the requested page size,
	// so a server-side cap below our limit can't make us skip entries.
	for offset := 0; ; {
		page, status, err := c.getMemberships(ctx, path, membershipPageSize, offset)
		if err != nil {
			return Member{}, err
		}
		if status == http.StatusNotFound {
			return Member{}, ErrNotMember
		}
		for _, m := range page.Data {
			if m.Organization.ID != c.orgID {
				continue
			}
			out := Member{IsMember: true, Role: m.Role}
			if p := m.PublicUserData; p != nil {
				out.Email = deref(p.Identifier)
				out.FirstName = deref(p.FirstName)
				out.LastName = deref(p.LastName)
				out.ImageURL = p.ImageURL
			}
			return out, nil
		}
		offset += len(page.Data)
		if len(page.Data) == 0 || offset >= page.TotalCount {
			return Member{}, ErrNotMember
		}
	}
}

// OrgMembers lists every member of the configured organisation keyed by
// Clerk user id. Used by the hourly offboarding sync; a member row without
// public_user_data (Clerk omits it for deprovisioned users) is skipped.
func (c *Client) OrgMembers(ctx context.Context) (map[string]Member, error) {
	path := "/organizations/" + url.PathEscape(c.orgID) + "/memberships"
	out := map[string]Member{}
	for offset := 0; ; {
		page, status, err := c.getMemberships(ctx, path, membersPageSize, offset)
		if err != nil {
			return nil, err
		}
		if status == http.StatusNotFound {
			return nil, fmt.Errorf("%w: organization %s not found", ErrUnavailable, c.orgID)
		}
		for _, m := range page.Data {
			p := m.PublicUserData
			if p == nil || p.UserID == "" {
				continue
			}
			out[p.UserID] = Member{
				IsMember:  true,
				Role:      m.Role,
				Email:     deref(p.Identifier),
				FirstName: deref(p.FirstName),
				LastName:  deref(p.LastName),
				ImageURL:  p.ImageURL,
			}
		}
		offset += len(page.Data)
		if len(page.Data) == 0 || offset >= page.TotalCount {
			return out, nil
		}
	}
}

// getMemberships performs one paginated Backend API GET. A 404 is returned
// to the caller as the status (it means "no such user/org", not an
// outage); every other non-2xx is ErrUnavailable.
func (c *Client) getMemberships(ctx context.Context, path string, limit, offset int) (*membershipList, int, error) {
	q := url.Values{}
	q.Set("limit", strconv.Itoa(limit))
	q.Set("offset", strconv.Itoa(offset))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.apiBase+path+"?"+q.Encode(), nil)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.secretKey)
	req.Header.Set("Accept", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, maxResponseBody))
	if err != nil {
		return nil, 0, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if res.StatusCode == http.StatusNotFound {
		return &membershipList{}, http.StatusNotFound, nil
	}
	if res.StatusCode/100 != 2 {
		return nil, res.StatusCode, fmt.Errorf("%w: GET %s: HTTP %d", ErrUnavailable, path, res.StatusCode)
	}
	var page membershipList
	if err := json.Unmarshal(body, &page); err != nil {
		return nil, res.StatusCode, fmt.Errorf("%w: GET %s: %v", ErrUnavailable, path, err)
	}
	return &page, res.StatusCode, nil
}
