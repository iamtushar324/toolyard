package oauth

// connect_tickets.go: one-time connect links.
//
// An agent whose owner has not signed in to a server cannot do the signing
// in itself, and the person should not have to leave the chat to find the
// right dashboard page. So the gateway mints a ticket for (user, server,
// purpose) and shows the person a link carrying it. Opening the link
// redeems the ticket once, starts the ordinary OAuth flow for that very
// user and server in that browser (the flow cookie binds the return), and
// forwards the browser to the provider. Only the ticket's sha256 is stored;
// the plaintext exists in the link the agent shows and nowhere else.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"time"
)

// ConnectLinkPath is the public route a connect link opens:
// <public URL>/v1/connect/link/<ticket>. The API serves it; the gateway
// builds links with it.
const ConnectLinkPath = "/v1/connect/link/"

// ConnectTicketTTL is how long a connect link can be opened. The flow it
// starts then has PendingTTL of its own.
const ConnectTicketTTL = 10 * time.Minute

// Connect purposes: whose token the link's sign-in produces.
const (
	// ConnectPurposePerUser: the person's own token on a per_user server.
	ConnectPurposePerUser = "per_user"
	// ConnectPurposeShared: the shared token of a shared OAuth server; only
	// an admin is given such a link.
	ConnectPurposeShared = "shared"
)

// Ticket errors. Redeem tells them apart so the audit row can say why a
// link was refused; the page the person sees is the same for all three.
var (
	ErrTicketUnknown = errors.New("oauth: connect link not found")
	ErrTicketUsed    = errors.New("oauth: connect link already used")
	ErrTicketExpired = errors.New("oauth: connect link expired")
)

// MaxLiveConnectTickets caps the unused, unexpired tickets one (user,
// server) can have at once: every refusal mints one, so an agent retrying
// in a loop would otherwise fill the table with links nobody opens.
const MaxLiveConnectTickets = 20

// ErrTooManyTickets is returned by IssueConnectTicket at that cap.
var ErrTooManyTickets = errors.New("oauth: too many connect links are already outstanding for this person and server; use one of them, or wait a few minutes")

// ErrAccountMismatch is returned by ExchangeCodeForUserChecked when the
// caller's accept hook rejected the account the provider signed in: nothing
// was stored.
var ErrAccountMismatch = errors.New("oauth: the provider account is not this person's")

// ConnectTicket is a redeemed ticket's row, without the ticket itself.
type ConnectTicket struct {
	UserID    string
	Upstream  string
	Purpose   string
	AgentID   string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// hashConnectTicket is what a ticket is stored as.
func hashConnectTicket(ticket string) string {
	sum := sha256.Sum256([]byte(ticket))
	return hex.EncodeToString(sum[:])
}

// IssueConnectTicket mints a ticket for userID to connect upstream for
// purpose, noting the agent that asked. The plaintext is returned once;
// only its hash is stored. The link is valid for ConnectTicketTTL and can
// be redeemed once.
func (s *Service) IssueConnectTicket(ctx context.Context, userID, upstream, purpose, agentID string) (string, error) {
	if userID == "" || upstream == "" {
		return "", errors.New("oauth: a connect ticket needs a user and a server")
	}
	if purpose != ConnectPurposePerUser && purpose != ConnectPurposeShared {
		return "", fmt.Errorf("oauth: bad connect purpose %q", purpose)
	}
	ticket, err := randomString(32)
	if err != nil {
		return "", err
	}
	now := time.Now()
	// The cap and the insert are one statement, so two refusals racing
	// cannot both slip under it.
	res, err := s.db.ExecContext(ctx, `
        INSERT INTO connect_tickets(id_hash, user_id, upstream, purpose, agent_id, created_at, expires_at)
        SELECT ?,?,?,?,?,?,?
        WHERE (SELECT count(*) FROM connect_tickets
               WHERE user_id = ? AND upstream = ? AND used_at IS NULL AND expires_at > ?) < ?`,
		hashConnectTicket(ticket), userID, upstream, purpose, nullStr(agentID),
		now.UnixMilli(), now.Add(ConnectTicketTTL).UnixMilli(),
		userID, upstream, now.UnixMilli(), MaxLiveConnectTickets)
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return "", ErrTooManyTickets
	}
	return ticket, nil
}

// loadConnectTicket reads a ticket's row by hash, with its used_at.
func (s *Service) loadConnectTicket(ctx context.Context, hash string) (*ConnectTicket, sql.NullInt64, error) {
	var t ConnectTicket
	var created, exp int64
	var usedAt sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT user_id, upstream, purpose, COALESCE(agent_id,''), created_at, expires_at, used_at FROM connect_tickets WHERE id_hash = ?`, hash).
		Scan(&t.UserID, &t.Upstream, &t.Purpose, &t.AgentID, &created, &exp, &usedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, usedAt, ErrTicketUnknown
	}
	if err != nil {
		return nil, usedAt, err
	}
	t.CreatedAt = time.UnixMilli(created)
	t.ExpiresAt = time.UnixMilli(exp)
	return &t, usedAt, nil
}

// PeekConnectTicket reads a live ticket's row without redeeming it: the
// confirm page shows whose link it is before anything happens. Unknown,
// used and expired tickets are told apart as RedeemConnectTicket does.
func (s *Service) PeekConnectTicket(ctx context.Context, ticket string) (*ConnectTicket, error) {
	if ticket == "" {
		return nil, ErrTicketUnknown
	}
	t, usedAt, err := s.loadConnectTicket(ctx, hashConnectTicket(ticket))
	switch {
	case err != nil:
		return nil, err
	case usedAt.Valid:
		return nil, ErrTicketUsed
	case !time.Now().Before(t.ExpiresAt):
		return nil, ErrTicketExpired
	}
	return t, nil
}

// RedeemConnectTicket marks the ticket used and returns its row. A ticket
// is redeemed at most once: the conditional UPDATE is the whole check, so
// two browsers submitting the same link race for one row. Unknown, already
// used and expired tickets are told apart (ErrTicketUnknown, ErrTicketUsed,
// ErrTicketExpired).
func (s *Service) RedeemConnectTicket(ctx context.Context, ticket string) (*ConnectTicket, error) {
	if ticket == "" {
		return nil, ErrTicketUnknown
	}
	h := hashConnectTicket(ticket)
	now := time.Now().UnixMilli()
	res, err := s.db.ExecContext(ctx,
		`UPDATE connect_tickets SET used_at = ? WHERE id_hash = ? AND used_at IS NULL AND expires_at > ?`, now, h, now)
	if err != nil {
		return nil, err
	}
	t, usedAt, err := s.loadConnectTicket(ctx, h)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return t, nil
	}
	switch {
	case usedAt.Valid && usedAt.Int64 < now:
		return nil, ErrTicketUsed
	case t.ExpiresAt.UnixMilli() <= now:
		return nil, ErrTicketExpired
	default:
		// Redeemed between the UPDATE and the read.
		return nil, ErrTicketUsed
	}
}

// ConsumeConnectTickets marks every live ticket for (userID, upstream)
// used: the person just connected, so the links still floating in chat
// must not start another sign-in. Returns how many were closed.
func (s *Service) ConsumeConnectTickets(ctx context.Context, userID, upstream string) (int, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE connect_tickets SET used_at = ? WHERE user_id = ? AND upstream = ? AND used_at IS NULL`,
		time.Now().UnixMilli(), userID, upstream)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// PurgeExpiredConnectTickets drops tickets past their expiry (used or
// not); a used ticket's row is only needed until then. Run from the
// refresher's tick.
func (s *Service) PurgeExpiredConnectTickets(ctx context.Context) (int, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM connect_tickets WHERE expires_at < ?`, time.Now().UnixMilli())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// BeginFromTicket starts the flow a redeemed connect link asked for, for
// userID: their own token when perUser, the server's shared token
// otherwise. The flow is browser-bound (the API sets the flow cookie on the
// browser that redeemed the link) and marked as ticket-started, so the
// callback applies the account rule and shows the "back to your agent"
// page; openerVerified says that browser held a toolyard session for
// userID. Returns the provider's authorize URL and the state.
func (s *Service) BeginFromTicket(ctx context.Context, upstream, userID string, perUser, openerVerified bool) (authURL, state string, err error) {
	if userID == "" {
		return "", "", errors.New("oauth: a connect link flow needs a user")
	}
	return s.beginFlow(ctx, upstream, userID, ModeCallback, nil,
		pendingFlags{perUser: perUser, browserBound: true, viaTicket: true, openerVerified: openerVerified})
}

// ExchangeCodeChecked is ExchangeCode with a look at the token before it
// replaces the shared row: accept sees the account label on file (empty
// when none) and the record the provider returned, and may refuse it. On
// refusal nothing changes, the new token is revoked at the provider, and
// the error is returned wrapped in ErrAccountMismatch.
func (s *Service) ExchangeCodeChecked(ctx context.Context, upstream, code, verifier string,
	accept func(prevLabel string, rec *TokenRecord) error) (*TokenRecord, error) {
	return s.exchangeCode(ctx, upstream, code, verifier, accept)
}

// ExchangeCodeForUserChecked is ExchangeCodeForUser with a look at the
// token before it is stored: accept sees the record the provider returned
// (account label included) and may refuse it. On refusal nothing is
// stored, the just-issued token is revoked at the provider when it has a
// revocation endpoint, and the error is returned wrapped in
// ErrAccountMismatch.
func (s *Service) ExchangeCodeForUserChecked(ctx context.Context, upstream, userID, code, verifier string,
	accept func(rec *UserTokenRecord) error) (*UserTokenRecord, error) {
	return s.exchangeCodeForUser(ctx, upstream, userID, code, verifier, accept)
}

// exchangeCodeForUser swaps the code for tokens, runs accept when given,
// then stores the row and warms the bearer cache.
func (s *Service) exchangeCodeForUser(ctx context.Context, upstream, userID, code, verifier string,
	accept func(rec *UserTokenRecord) error) (*UserTokenRecord, error) {
	defer s.lockUser(upstream, userID)()
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
	rec := userTokenRecordFromResponse(upstream, userID, tr)
	if accept != nil {
		if aerr := accept(rec); aerr != nil {
			s.revoke(ctx, cli, rec.RefreshToken, rec.AccessToken)
			return nil, fmt.Errorf("%w: %v", ErrAccountMismatch, aerr)
		}
	}
	if err := s.PutUserToken(ctx, rec); err != nil {
		return nil, err
	}
	s.setUserBearer(upstream, userID, rec.AccessToken)
	if s.bus != nil {
		s.bus.Publish("mcp_oauth_user_done", map[string]any{
			"upstream":      upstream,
			"user_id":       userID,
			"account_label": rec.AccountLabel,
		})
	}
	return rec, nil
}

// ---------------------------------------------------------------------------
// What the agent-facing connection tools read
// ---------------------------------------------------------------------------

// OAuthServer is one server with an OAuth client, as the connection tools
// list them: its name, who signs in to it, and whether it is enabled.
type OAuthServer struct {
	Name     string
	AuthMode string
	Enabled  bool
}

// OAuthServers lists every server that holds an OAuth client (discovery or
// a manual client, the PAT placeholder included), joined with the server
// row for its auth mode. The rows are read fully before anything else
// runs on the database.
func (s *Service) OAuthServers(ctx context.Context) ([]OAuthServer, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT c.upstream_name, COALESCE(u.auth_mode,'shared'), u.enabled
        FROM oauth_clients c JOIN upstream_servers u ON u.name = c.upstream_name
        ORDER BY c.upstream_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OAuthServer
	for rows.Next() {
		var o OAuthServer
		var enabled int
		if err := rows.Scan(&o.Name, &o.AuthMode, &enabled); err != nil {
			return nil, err
		}
		o.Enabled = enabled != 0
		out = append(out, o)
	}
	return out, rows.Err()
}

// SharedConnection is a shared server's sign-in as the connection tools see
// it. No token value is ever part of it.
type SharedConnection struct {
	// HasToken: a token row exists (whatever its state).
	HasToken bool
	// State is the token row's state (StateActive, StateNeedsReauth, ...),
	// "" without a row.
	State string
	// AccountLabel names the account the token belongs to, when the
	// provider said.
	AccountLabel string
	// IsPAT: the token is a pasted personal access token.
	IsPAT bool
	// CanAuthorize: the client has a real authorization endpoint, so a
	// browser sign-in can replace the token. False for the placeholder
	// client a PAT gets.
	CanAuthorize bool
}

// Usable reports whether calls can go out with this token now.
func (c *SharedConnection) Usable() bool {
	return c != nil && c.HasToken && (c.State == StateActive || c.State == StateRefreshing)
}

// SharedConnection reports a shared server's OAuth sign-in: nil when the
// server has no OAuth client at all (it is not an OAuth server).
func (s *Service) SharedConnection(ctx context.Context, upstream string) (*SharedConnection, error) {
	var c SharedConnection
	var authEndpoint string
	var hasToken, pat int
	err := s.db.QueryRowContext(ctx, `
        SELECT c.authorization_endpoint, t.upstream_name IS NOT NULL,
               COALESCE(t.state,''), COALESCE(t.account_label,''), COALESCE(t.is_pat,0)
        FROM oauth_clients c LEFT JOIN oauth_tokens t ON t.upstream_name = c.upstream_name
        WHERE c.upstream_name = ?`, upstream).Scan(&authEndpoint, &hasToken, &c.State, &c.AccountLabel, &pat)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c.HasToken = hasToken == 1
	c.IsPAT = pat == 1
	c.CanAuthorize = authEndpoint != "" && authEndpoint != "(pat)"
	return &c, nil
}

// UserConnectionOf reports one person's sign-in to a per_user server: nil
// when they have never connected it.
func (s *Service) UserConnectionOf(ctx context.Context, upstream, userID string) (*UserConnection, error) {
	list, err := s.listUserConnections(ctx, `upstream_name = ? AND user_id = ?`, upstream, userID)
	if err != nil {
		return nil, err
	}
	if len(list) != 1 {
		return nil, nil
	}
	return &list[0], nil
}
