package mcpauth

// authorize.go is the authorization request and the person's answer: the
// request is checked and stored (BeginAuthorize), the consent screen reads
// it back (PendingRequest), and Approve or Deny claims it once, creates or
// reuses the grant and its agent, and returns where to send the browser.

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/tusharbhardwaj/toolyard/internal/access"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// pkceRE: an S256 code challenge (43 base64url characters) or a code
// verifier (43-128 unreserved characters), RFC 7636 section 4.
var (
	challengeRE = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)
	verifierRE  = regexp.MustCompile(`^[A-Za-z0-9._~-]{43,128}$`)
	requestIDRE = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)
	// nicknameRE is the agent name rule (identity's agentNameRE).
	nicknameRE = regexp.MustCompile(`^[A-Za-z0-9 ._:()-]{1,64}$`)
)

// AuthorizeRequest is a checked authorization request waiting for consent.
type AuthorizeRequest struct {
	ID            string
	Client        *Client
	RedirectURI   string
	State         string
	CodeChallenge string
	Resource      string
	Scope         string
	ExpiresAt     int64
}

// AuthorizeError is a failed authorization request. With RedirectURI set
// it goes back to the client (RFC 6749 section 4.1.2.1); without, the
// client or its redirect URI could not be trusted and the person sees it.
type AuthorizeError struct {
	*Error
	RedirectURI string
	State       string
	issuer      string
}

// Location is where to send the browser for a redirectable error, "" when
// the person must see it instead.
func (e *AuthorizeError) Location() string {
	if e.RedirectURI == "" {
		return ""
	}
	return withParams(e.RedirectURI, map[string]string{"error": e.Code, "error_description": e.Description, "state": e.State, "iss": e.issuer})
}

// withParams adds params (empty values left out) to a redirect URI's query.
func withParams(raw string, params map[string]string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	for k, v := range params {
		if v != "" {
			q.Set(k, v)
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// Request limits: per client address (IPv6 by /64) and network (IPv4 /24,
// IPv6 /48) everywhere, and global caps only on work that costs something
// beyond one row that expires in minutes: registering a client and
// fetching a metadata document.
var (
	authorizePerIP   = 120
	authorizeWindow  = 10 * time.Minute
	registerPerIP    = 60
	registerGlobal   = 300
	registerWindow   = time.Hour
	cimdFetchGlobal  = 600
	maxUnusedClients = 5000
	// unusedClientTTL: a registration nobody authorized within a day is
	// swept, so the cap on unused registrations frees up within a day.
	unusedClientTTL = 24 * time.Hour
)

// trustedRedirect reports whether an error may go straight back to a
// redirect URI before the person answered: when the admin's redirect host
// allow list names it, or when it is on the host of the client's own
// metadata document. A self-registered client's redirect URI is never
// trusted this way, since anyone can register one.
func (s *Service) trustedRedirect(c *Client, redirect string) bool {
	if len(s.RedirectHosts()) > 0 && s.redirectHostAllowed(redirect) {
		return true
	}
	host := ClientHost(c)
	u, err := url.Parse(redirect)
	if host == "" || err != nil || u.Scheme != "https" {
		return false
	}
	r := strings.ToLower(u.Hostname())
	return r == host || strings.HasSuffix(r, "."+host)
}

// BeginAuthorize checks an authorization request (the query of GET
// /oauth/authorize) and stores it for the consent screen.
//
// Errors before the person answers are shown to the person, except that
// one goes back to the client once the client, its redirect URI and
// response_type=code are all valid and the redirect URI is trusted
// (trustedRedirect). After the person answers, allow and deny redirect.
func (s *Service) BeginAuthorize(ctx context.Context, q url.Values, ip string) (*AuthorizeRequest, *AuthorizeError) {
	show := func(status int, code, desc string) *AuthorizeError {
		return &AuthorizeError{Error: oauthErr(status, code, desc)}
	}
	// Per address and per network only: an authorization for a known
	// client is cheap, so no global cap that a flood could use up for
	// everyone. Fetching a document and registering a client have theirs.
	if !s.allowAll(ctx, ipLimits("authorize", ip, authorizePerIP, authorizeWindow)...) {
		return nil, show(http.StatusTooManyRequests, "temporarily_unavailable", "Too many sign-in attempts. Wait a few minutes and try again.")
	}
	for _, k := range []string{"client_id", "redirect_uri", "response_type", "state", "code_challenge", "code_challenge_method", "resource", "scope"} {
		if len(q[k]) > 1 {
			return nil, show(http.StatusBadRequest, "invalid_request", "The sign-in link repeats the "+k+" parameter.")
		}
	}
	clientID := q.Get("client_id")
	if clientID == "" {
		return nil, show(http.StatusBadRequest, "invalid_request", "The sign-in link has no client_id. Start the connection again from your app.")
	}
	client, cerr := s.ResolveClient(ctx, clientID, ip)
	if cerr != nil {
		status := http.StatusBadRequest
		if cerr.Status == http.StatusTooManyRequests {
			status = cerr.Status
		}
		return nil, show(status, cerr.Code, "Toolyard cannot use this app: "+cerr.Description)
	}
	redirect := q.Get("redirect_uri")
	if redirect == "" {
		return nil, show(http.StatusBadRequest, "invalid_request", "The sign-in link has no redirect_uri.")
	}
	if !redirectMatches(client.RedirectURIs, redirect) {
		return nil, show(http.StatusBadRequest, "invalid_request", "The app asked to return to an address it did not register, so toolyard will not send you there.")
	}
	if !s.redirectHostAllowed(redirect) {
		return nil, show(http.StatusForbidden, "access_denied", "This toolyard's administrator does not allow apps that return to "+RedirectHost(redirect)+".")
	}
	if q.Get("response_type") != "code" {
		return nil, show(http.StatusBadRequest, "unsupported_response_type", "The app asked for something other than an authorization code (response_type must be code).")
	}
	// Before the person answers, an error goes back to the client only
	// when its redirect URI is trusted (s.trustedRedirect); otherwise the
	// person sees it: anyone can register a client with someone else's
	// redirect URI, and a broken request must not bounce a browser there
	// (RFC 9700 section 4.11.2). state is optional: PKCE is required, and
	// it is what protects the code (OAuth 2.1).
	state := q.Get("state")
	trusted := s.trustedRedirect(client, redirect)
	back := func(code, desc string) *AuthorizeError {
		if !trusted {
			return show(http.StatusBadRequest, code, "The app sent an invalid sign-in request: "+desc+".")
		}
		return &AuthorizeError{Error: oauthErr(http.StatusBadRequest, code, desc), RedirectURI: redirect, State: state, issuer: s.Issuer()}
	}
	if len(state) > 1000 {
		return nil, back("invalid_request", "state is too long")
	}
	challenge := q.Get("code_challenge")
	if challenge == "" {
		return nil, back("invalid_request", "PKCE is required: send code_challenge with code_challenge_method S256")
	}
	if q.Get("code_challenge_method") != "S256" {
		return nil, back("invalid_request", "code_challenge_method must be S256")
	}
	if !challengeRE.MatchString(challenge) {
		return nil, back("invalid_request", "code_challenge must be the base64url SHA-256 of the code verifier (43 characters)")
	}
	// A client that sends no resource indicator gets toolyard's only
	// resource, /mcp; one that sends another resource is refused.
	resource := q.Get("resource")
	if resource == "" {
		resource = s.Resource()
	}
	if !s.sameResource(resource) {
		return nil, back("invalid_target", "resource must be "+s.Resource())
	}
	id, err := requestID()
	if err != nil {
		return nil, show(http.StatusInternalServerError, "server_error", "Toolyard could not start the sign-in. Try again.")
	}
	now := s.now()
	req := &AuthorizeRequest{ID: id, Client: client, RedirectURI: redirect, State: state, CodeChallenge: challenge,
		Resource: s.Resource(), Scope: grantedScope(q.Get("scope")), ExpiresAt: now.Add(RequestTTL).UnixMilli()}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO mcp_oauth_requests(id, client_id, redirect_uri, state, code_challenge, resource, scope, created_at, expires_at)
         VALUES(?,?,?,?,?,?,?,?,?)`,
		req.ID, client.ID, redirect, state, challenge, req.Resource, req.Scope, now.UnixMilli(), req.ExpiresAt); err != nil {
		return nil, show(http.StatusInternalServerError, "server_error", "Toolyard could not start the sign-in. Try again.")
	}
	return req, nil
}

// requestID is 32 random bytes in base64url: 43 characters, the form the
// login page carries back after sign-in.
func requestID() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// ValidRequestID reports whether id has the form of a request id.
func ValidRequestID(id string) bool { return requestIDRE.MatchString(id) }

// PendingRequest returns the unanswered, unexpired request id names, with
// its client.
func (s *Service) PendingRequest(ctx context.Context, id string) (*AuthorizeRequest, error) {
	if !ValidRequestID(id) {
		return nil, ErrRequestNotFound
	}
	var r AuthorizeRequest
	var clientID string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, client_id, redirect_uri, state, code_challenge, resource, scope, expires_at
         FROM mcp_oauth_requests WHERE id = ? AND used_at IS NULL AND expires_at > ?`, id, s.now().UnixMilli()).
		Scan(&r.ID, &clientID, &r.RedirectURI, &r.State, &r.CodeChallenge, &r.Resource, &r.Scope, &r.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrRequestNotFound
	}
	if err != nil {
		return nil, err
	}
	c, cerr := s.Client(ctx, clientID)
	if cerr != nil {
		return nil, cerr
	}
	r.Client = c
	return &r, nil
}

// ExistingGrant is what a person already allowed a client: the nickname
// and scope the consent screen starts from when they connect it again.
type ExistingGrant struct {
	GrantID   string
	AgentID   string
	Nickname  string
	Scope     access.AgentScope
	Revoked   bool
	MayManage bool
}

// Existing returns userID's grant for clientID, or nil when there is none.
func (s *Service) Existing(ctx context.Context, userID, clientID string) (*ExistingGrant, error) {
	var g ExistingGrant
	var revoked sql.NullInt64
	var manage int
	err := s.db.QueryRowContext(ctx,
		`SELECT g.id, g.agent_id, COALESCE(a.name, ''), g.revoked_at, g.may_manage FROM mcp_oauth_grants g LEFT JOIN agents a ON a.id = g.agent_id
         WHERE g.user_id = ? AND g.client_id = ?`, userID, clientID).Scan(&g.GrantID, &g.AgentID, &g.Nickname, &revoked, &manage)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	g.Revoked, g.MayManage = revoked.Valid, manage != 0
	if g.Scope, err = access.ReadAgentScope(ctx, s.db, g.AgentID); err != nil {
		return nil, err
	}
	return &g, nil
}

// Consent is what the person chose on the consent screen.
type Consent struct {
	Nickname string
	Scope    access.AgentScope
	// MayManage lets the client manage toolyard (policies, servers, shared
	// connections). Only an admin can give it; it is ignored otherwise.
	MayManage bool
}

// DefaultNickname turns a client name into a valid agent name.
func DefaultNickname(clientName string) string {
	var b strings.Builder
	for _, r := range clientName {
		if r < 128 && nicknameRE.MatchString(string(r)) {
			b.WriteRune(r)
		} else {
			b.WriteRune(' ')
		}
	}
	out := strings.Join(strings.Fields(b.String()), " ")
	if len(out) > 64 {
		out = strings.TrimSpace(out[:64])
	}
	if out == "" {
		out = "MCP client"
	}
	return out
}

// CheckNickname returns the trimmed nickname or ErrInvalidNickname.
func CheckNickname(nickname string) (string, error) {
	nickname = strings.TrimSpace(nickname)
	if !nicknameRE.MatchString(nickname) {
		return "", ErrInvalidNickname
	}
	return nickname, nil
}

// claimedRequest is a request row Approve or Deny claimed.
type claimedRequest struct {
	clientID, redirect, state, challenge, resource, scope string
}

// claimRequest marks the request answered by userID, once: of two answers
// racing, exactly one gets the row.
func (s *Service) claimRequest(ctx context.Context, q store.Querier, id, userID string) (*claimedRequest, error) {
	if !ValidRequestID(id) {
		return nil, ErrRequestNotFound
	}
	var c claimedRequest
	now := s.now().UnixMilli()
	err := q.QueryRowContext(ctx,
		`UPDATE mcp_oauth_requests SET used_at = ?, user_id = ? WHERE id = ? AND used_at IS NULL AND expires_at > ?
         RETURNING client_id, redirect_uri, state, code_challenge, resource, scope`, now, userID, id, now).
		Scan(&c.clientID, &c.redirect, &c.state, &c.challenge, &c.resource, &c.scope)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrRequestNotFound
	}
	return &c, err
}

// Approve answers request id with the person's yes: it creates the grant
// and its agent (or reuses the person's earlier grant for this client,
// renamed and re-enabled), sets the agent's tool scope, issues a code, and
// returns the redirect that carries the code back to the client.
func (s *Service) Approve(ctx context.Context, id string, u *identity.User, c Consent) (string, error) {
	nickname, err := CheckNickname(c.Nickname)
	if err != nil {
		return "", err
	}
	scope, err := c.Scope.Normalize()
	if err != nil {
		return "", err
	}
	// Make sure the client row is current before the transaction: a
	// metadata document client may need a fetch, which must not run while
	// the transaction holds the database.
	pending, err := s.PendingRequest(ctx, id)
	if err != nil {
		return "", err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	req, err := s.claimRequest(ctx, tx, id, u.ID)
	if err != nil {
		return "", err
	}
	now := s.now().UnixMilli()
	manage := 0
	if c.MayManage && u.Role == identity.RoleAdmin {
		manage = 1
	}
	var grantID, agentID string
	err = tx.QueryRowContext(ctx, `SELECT id, agent_id FROM mcp_oauth_grants WHERE user_id = ? AND client_id = ?`+s.db.Dialect.ForUpdate(),
		u.ID, req.clientID).Scan(&grantID, &agentID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		grantID, agentID = "grt_"+uuid.NewString(), identity.NewAgentID()
		if _, err = tx.ExecContext(ctx, `INSERT INTO agents(id, name, owner_user, token_hash, last_seen, created_at) VALUES(?,?,?,'',?,?)`,
			agentID, nickname, u.ID, now, now); err != nil {
			return "", err
		}
		if _, err = tx.ExecContext(ctx,
			`INSERT INTO mcp_oauth_grants(id, client_id, user_id, agent_id, scope, resource, created_at, updated_at, may_manage)
             VALUES(?,?,?,?,?,?,?,?,?)`, grantID, req.clientID, u.ID, agentID, req.scope, req.resource, now, now, manage); err != nil {
			return "", err
		}
	case err != nil:
		return "", err
	default:
		res, err := tx.ExecContext(ctx, `UPDATE agents SET name = ?, disabled = 0 WHERE id = ? AND owner_user = ?`, nickname, agentID, u.ID)
		if err != nil {
			return "", err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			// The agent was deleted; the grant kept its id (the tombstone),
			// so the client comes back as the same agent.
			if _, err = tx.ExecContext(ctx, `INSERT INTO agents(id, name, owner_user, token_hash, last_seen, created_at) VALUES(?,?,?,'',?,?)`,
				agentID, nickname, u.ID, now, now); err != nil {
				return "", err
			}
		}
		// A new consent replaces the grant's earlier sign-ins: every token
		// chain and unused code issued before it stops, whether the grant
		// was revoked, its agent was deleted, or it was still active. Only
		// the code issued below is valid afterwards.
		if _, err = tx.ExecContext(ctx, `UPDATE mcp_oauth_tokens SET revoked_at = ? WHERE grant_id = ? AND revoked_at IS NULL`, now, grantID); err != nil {
			return "", err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE mcp_oauth_codes SET used_at = ? WHERE grant_id = ? AND used_at IS NULL`, now, grantID); err != nil {
			return "", err
		}
		if _, err = tx.ExecContext(ctx,
			`UPDATE mcp_oauth_grants SET scope = ?, resource = ?, updated_at = ?, revoked_at = NULL, revoked_reason = NULL, may_manage = ? WHERE id = ?`,
			req.scope, req.resource, now, manage, grantID); err != nil {
			return "", err
		}
	}
	if _, err = access.WriteAgentScope(ctx, tx, agentID, scope, u.ID); err != nil {
		return "", err
	}
	code, err := newSecret(codePrefix)
	if err != nil {
		return "", err
	}
	if _, err = tx.ExecContext(ctx,
		`INSERT INTO mcp_oauth_codes(code_hash, grant_id, client_id, redirect_uri, code_challenge, resource, scope, family_id, created_at, expires_at)
         VALUES(?,?,?,?,?,?,?,?,?,?)`,
		hashSecret(code), grantID, req.clientID, req.redirect, req.challenge, req.resource, req.scope, newFamily(),
		now, s.now().Add(CodeTTL).UnixMilli()); err != nil {
		return "", err
	}
	if err = s.auditTx(ctx, tx, auditRow{event: "mcp_oauth.authorized", agentID: agentID, nickname: nickname, owner: u, decider: u,
		client: pending.Client, grantID: grantID, details: map[string]any{
			"redirect_host": RedirectHost(req.redirect), "scope": req.scope, "tool_scope": scope.Summary(), "may_manage": manage == 1,
		}}); err != nil {
		return "", err
	}
	if err = tx.Commit(); err != nil {
		return "", err
	}
	if s.access != nil {
		s.access.InvalidateAgent(agentID)
	}
	return withParams(req.redirect, map[string]string{"code": code, "state": req.state, "iss": s.Issuer()}), nil
}

// Deny answers request id with the person's no and returns the redirect
// that tells the client (error access_denied).
func (s *Service) Deny(ctx context.Context, id string, u *identity.User) (string, error) {
	pending, err := s.PendingRequest(ctx, id)
	if err != nil {
		return "", err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	req, err := s.claimRequest(ctx, tx, id, u.ID)
	if err != nil {
		return "", err
	}
	if err = s.auditTx(ctx, tx, auditRow{event: "mcp_oauth.denied", owner: u, decider: u, client: pending.Client,
		details: map[string]any{"redirect_host": RedirectHost(req.redirect)}}); err != nil {
		return "", err
	}
	if err = tx.Commit(); err != nil {
		return "", err
	}
	return withParams(req.redirect, map[string]string{"error": "access_denied", "error_description": "The person did not allow this app.", "state": req.state, "iss": s.Issuer()}), nil
}

// ---- audit ---------------------------------------------------------------------

// auditRow is one lifecycle row (authorized, denied, renamed, scope
// changed, revoked, reuse detected) in audit_events.
type auditRow struct {
	event    string
	agentID  string
	nickname string
	owner    *identity.User
	decider  *identity.User
	client   *Client
	clientID string
	grantID  string
	details  map[string]any
}

// via is the path the change came in by: the dashboard when a person
// made it, the OAuth endpoints otherwise (a client's revocation, a reused
// refresh token).
func (r auditRow) via() string {
	if r.decider != nil {
		return "dashboard"
	}
	return "oauth"
}

// auditTx writes r in the caller's transaction. The OAuth client id goes
// in the arguments JSON next to the details; the client's display name in
// client_name; the grant id is the decider_ref (the instrument).
func (s *Service) auditTx(ctx context.Context, q store.Querier, r auditRow) error {
	args := map[string]any{}
	for k, v := range r.details {
		args[k] = v
	}
	clientName, clientID := "", r.clientID
	if r.client != nil {
		clientID = r.client.ID
		args["oauth_client_kind"] = r.client.Kind
		clientName = r.client.DisplayName()
	}
	if clientID != "" {
		args["oauth_client_id"] = clientID
	}
	if r.grantID != "" {
		args["grant_id"] = r.grantID
	}
	body, _ := json.Marshal(args)
	var ownerID, ownerEmail, ownerName, decID, decEmail, decName, via string
	if r.owner != nil {
		ownerID, ownerEmail, ownerName = r.owner.ID, r.owner.Email, r.owner.Label()
	}
	if r.decider != nil {
		decID, decEmail, decName, via = r.decider.ID, r.decider.Email, r.decider.Label(), "dashboard"
	}
	_, err := q.ExecContext(ctx,
		`INSERT INTO audit_events(id, ts, event_type, agent_id, agent_name, agent_kind, owner_user_id, owner_email, owner_name,
             arguments, client_kind, client_name, via, decided_by_user_id, decided_by_email, decided_by_name, decided_via, decider_ref,
             oauth_client_id)
         VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		"ev_"+uuid.NewString(), s.now().UnixMilli(), r.event, nullStr(r.agentID), nullStr(r.nickname), AgentKindOAuthClient,
		nullStr(ownerID), nullStr(ownerEmail), nullStr(ownerName), string(body), nil, nullStr(clientName), r.via(),
		nullStr(decID), nullStr(decEmail), nullStr(decName), nullStr(via), nullStr(r.grantID), nullStr(clientID))
	return err
}
