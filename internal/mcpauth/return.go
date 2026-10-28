package mcpauth

// return.go is the return link. The person's yes (Approve) does not send
// the browser back to the client with a code: it stores a one-time return
// ticket, and the consent screen shows its link (ReturnPath + ticket). The
// person can allow the app in one browser or Chrome profile and open the
// link in the one where the client waits. The link works once, for
// ReturnTTL; the authorization code is minted only when it is used
// (RedeemReturn), so the code's 60 seconds (CodeTTL) start then.
//
// The ticket is a capability, like the code it stands for: whoever holds
// it can have a code sent to the client's registered redirect URI, and
// nowhere else. No toolyard session is needed to use it. The client's
// state and PKCE verifier stay with the client instance that started the
// request, so a code delivered to another browser's copy of the client is
// useless. Only the ticket's SHA-256 is stored.

import (
	"context"
	"database/sql"
	"errors"
	"regexp"

	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// returnTicketRE: returnPrefix and 32 random bytes in base64url.
var returnTicketRE = regexp.MustCompile(`^` + returnPrefix + `[A-Za-z0-9_-]{43}$`)

// ValidReturnTicket reports whether ticket has the form of a return ticket.
func ValidReturnTicket(ticket string) bool { return returnTicketRE.MatchString(ticket) }

// ReturnInfo is what the return page shows about a ticket.
type ReturnInfo struct {
	Client      *Client
	RedirectURI string
	// Nickname is the name of the agent the app runs as.
	Nickname string
	// Account is whose toolyard the app connects to: the email of the
	// person who allowed it, else their name.
	Account   string
	ExpiresAt int64
}

// insertReturnTicketTx stores a new return ticket for an approved request
// in the caller's transaction and returns the raw ticket. consentedAt is
// the consent's time: the ticket expires ReturnTTL after it.
func (s *Service) insertReturnTicketTx(ctx context.Context, q store.Querier, grantID, userID string, req *claimedRequest, consentedAt int64) (string, error) {
	ticket, err := newSecret(returnPrefix)
	if err != nil {
		return "", err
	}
	_, err = q.ExecContext(ctx,
		`INSERT INTO mcp_oauth_return_tickets(ticket_hash, grant_id, user_id, client_id, redirect_uri, state, code_challenge, resource, scope, created_at, expires_at)
         VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		hashSecret(ticket), grantID, userID, req.clientID, req.redirect, req.state, req.challenge, req.resource, req.scope,
		consentedAt, consentedAt+ReturnTTL.Milliseconds())
	return ticket, err
}

// ReturnTicket reads a ticket for the return page without using it. For a
// used or expired ticket, or one whose connection was revoked since, it
// returns the info next to ErrReturnUsed, ErrReturnExpired or
// ErrReturnRevoked, so the page can name the app; for an unknown one, nil
// and ErrReturnUnknown.
func (s *Service) ReturnTicket(ctx context.Context, ticket string) (*ReturnInfo, error) {
	if !ValidReturnTicket(ticket) {
		return nil, ErrReturnUnknown
	}
	var info ReturnInfo
	var clientID, grantID string
	var used sql.NullInt64
	var u identity.User
	err := s.db.QueryRowContext(ctx,
		`SELECT t.client_id, t.grant_id, t.redirect_uri, t.expires_at, t.used_at, COALESCE(a.name, ''),
                COALESCE(u.email, ''), COALESCE(u.display_name, ''), COALESCE(u.username, '')
         FROM mcp_oauth_return_tickets t
         LEFT JOIN mcp_oauth_grants g ON g.id = t.grant_id
         LEFT JOIN agents a ON a.id = g.agent_id
         LEFT JOIN users u ON u.id = t.user_id
         WHERE t.ticket_hash = ?`, hashSecret(ticket)).
		Scan(&clientID, &grantID, &info.RedirectURI, &info.ExpiresAt, &used, &info.Nickname, &u.Email, &u.DisplayName, &u.Username)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrReturnUnknown
	}
	if err != nil {
		return nil, err
	}
	info.Account = u.Email
	if info.Account == "" {
		info.Account = u.Label()
	}
	c, cerr := s.Client(ctx, clientID)
	if cerr != nil {
		return nil, cerr
	}
	info.Client = c
	switch {
	case used.Valid:
		return &info, ErrReturnUsed
	case info.ExpiresAt <= s.now().UnixMilli():
		return &info, ErrReturnExpired
	}
	g, err := s.grantStateTx(ctx, s.db, grantID)
	if err != nil {
		return nil, err
	}
	if !g.usable() {
		return &info, ErrReturnRevoked
	}
	return &info, nil
}

// RedeemReturn uses a return ticket, once: it mints the authorization code
// now and returns the redirect that carries it, with the request's state
// and the issuer, to the client's registered redirect URI, as Approve did
// before return links. Of two uses racing, exactly one gets the code. A
// used, expired or unknown ticket returns ErrReturnUsed, ErrReturnExpired
// or ErrReturnUnknown. A ticket whose connection was revoked since the
// consent is used up and returns ErrReturnRevoked.
func (s *Service) RedeemReturn(ctx context.Context, ticket string) (string, error) {
	if !ValidReturnTicket(ticket) {
		return "", ErrReturnUnknown
	}
	hash := hashSecret(ticket)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	// The grant first, as every transaction on a grant and its codes takes
	// it (lockGrantOf), so a consent or a revocation racing this one cannot
	// deadlock with it.
	if err := s.lockGrantOf(ctx, tx, `SELECT grant_id FROM mcp_oauth_return_tickets WHERE ticket_hash = ?`, hash); err != nil {
		return "", err
	}
	now := s.now()
	var grantID, clientID, redirect, state, challenge, resource, scope string
	var consentedAt int64
	err = tx.QueryRowContext(ctx,
		`UPDATE mcp_oauth_return_tickets SET used_at = ? WHERE ticket_hash = ? AND used_at IS NULL AND expires_at > ?
         RETURNING grant_id, client_id, redirect_uri, state, code_challenge, resource, scope, created_at`,
		now.UnixMilli(), hash, now.UnixMilli()).
		Scan(&grantID, &clientID, &redirect, &state, &challenge, &resource, &scope, &consentedAt)
	if errors.Is(err, sql.ErrNoRows) {
		var used sql.NullInt64
		if e := tx.QueryRowContext(ctx, `SELECT used_at FROM mcp_oauth_return_tickets WHERE ticket_hash = ?`, hash).Scan(&used); e != nil {
			return "", ErrReturnUnknown
		}
		if used.Valid {
			return "", ErrReturnUsed
		}
		return "", ErrReturnExpired
	}
	if err != nil {
		return "", err
	}
	g, err := s.grantStateTx(ctx, tx, grantID)
	if err != nil {
		return "", err
	}
	if !g.usable() {
		// The ticket stays used: it can never become good again.
		if err = tx.Commit(); err != nil {
			return "", err
		}
		return "", ErrReturnRevoked
	}
	code, err := newSecret(codePrefix)
	if err != nil {
		return "", err
	}
	// created_at is the consent's time, not now: the token endpoint takes
	// it as the sign-in time (auth_time), which the absolute session limit
	// counts from. The code's own lifetime starts now.
	if _, err = tx.ExecContext(ctx,
		`INSERT INTO mcp_oauth_codes(code_hash, grant_id, client_id, redirect_uri, code_challenge, resource, scope, family_id, created_at, expires_at)
         VALUES(?,?,?,?,?,?,?,?,?,?)`,
		hashSecret(code), grantID, clientID, redirect, challenge, resource, scope, newFamily(),
		consentedAt, now.Add(CodeTTL).UnixMilli()); err != nil {
		return "", err
	}
	if err = tx.Commit(); err != nil {
		return "", err
	}
	return withParams(redirect, map[string]string{"code": code, "state": state, "iss": s.Issuer()}), nil
}
