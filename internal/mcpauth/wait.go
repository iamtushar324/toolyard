package mcpauth

// wait.go is the waiting stop page. The page /oauth/authorize stops on, in
// the browser window where the client started (often a pop-up the client
// opened), also waits: it asks Wait every few seconds whether the person
// answered, in this browser or in another one where they opened the copied
// consent link. After a yes it offers Continue (RedeemWait), which uses the
// SAME approval as the return link (return.go), once: whichever of the two
// is used first gets the code, and the other is refused as used. The code
// is then sent from the window where the client started, so a client that
// keeps its OAuth state in that window finds it.
//
// Only the stop page knows the request's waiter secret: BeginAuthorize
// returns it once, and only its SHA-256 is stored (mcp_oauth_requests.
// waiter_hash). The consent link the person copies carries the request id
// alone, so whoever sees that link can neither watch the request nor finish
// it here. A missing or wrong waiter gets the same answer as an unknown
// request. The secret never goes in a URL: the page sends it in a POST body.

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"regexp"
)

// Statuses Wait reports.
const (
	// WaitPending: the person has not answered yet.
	WaitPending = "pending"
	// WaitApproved: the person allowed the app, and the approval is unused
	// and unexpired.
	WaitApproved = "approved"
	// WaitDenied: the person did not allow the app.
	WaitDenied = "denied"
	// WaitExpired: the request or its approval expired, a newer approval
	// replaced it, or the request is unknown (or the waiter is wrong).
	WaitExpired = "expired"
	// WaitUsed: the approval was used, by its return link or by the
	// waiting page.
	WaitUsed = "used"
)

// Verdicts claimRequest records in mcp_oauth_requests.verdict.
const (
	verdictAllow = "allow"
	verdictDeny  = "deny"
)

// waiterRE: waiterPrefix and 32 random bytes in base64url.
var waiterRE = regexp.MustCompile(`^` + waiterPrefix + `[A-Za-z0-9_-]{43}$`)

// ValidWaiter reports whether waiter has the form of a waiter secret.
func ValidWaiter(waiter string) bool { return waiterRE.MatchString(waiter) }

// WaitInfo is what the waiting page learns about its request.
type WaitInfo struct {
	Status string
	// Client is the app, set only when the waiter matched the request.
	Client *Client
}

// waitState is a request's state as its waiter sees it.
type waitState struct {
	status string
	// known: the waiter matched a stored request.
	known    bool
	clientID string
	// ticketHash is the approval's return ticket, when approved.
	ticketHash string
}

// waitStateOf reads request requestID for the holder of waiter. It changes
// nothing. An unknown request, a malformed request id or waiter, and a
// waiter that does not match all give the same unknown state.
func (s *Service) waitStateOf(ctx context.Context, requestID, waiter string) (*waitState, error) {
	unknown := &waitState{status: WaitExpired}
	if !ValidRequestID(requestID) || !ValidWaiter(waiter) {
		return unknown, nil
	}
	var stored, verdict sql.NullString
	var used sql.NullInt64
	var clientID string
	var expires int64
	err := s.db.QueryRowContext(ctx,
		`SELECT waiter_hash, verdict, client_id, used_at, expires_at FROM mcp_oauth_requests WHERE id = ?`, requestID).
		Scan(&stored, &verdict, &clientID, &used, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return unknown, nil
	}
	if err != nil {
		return nil, err
	}
	if !stored.Valid || subtle.ConstantTimeCompare([]byte(hashSecret(waiter)), []byte(stored.String)) != 1 {
		return unknown, nil
	}
	st := &waitState{known: true, clientID: clientID}
	now := s.now().UnixMilli()
	switch {
	case !used.Valid && expires > now:
		st.status = WaitPending
	case !used.Valid:
		st.status = WaitExpired
	case verdict.String == verdictDeny:
		st.status = WaitDenied
	case verdict.String != verdictAllow:
		// Answered by an older binary, which records no verdict and links
		// no return ticket to the request: this page cannot finish it, so
		// it keeps waiting while the person uses that binary's return link.
		st.status = WaitPending
	default:
		var hash string
		var ticketExpires int64
		var ticketUsed sql.NullInt64
		err := s.db.QueryRowContext(ctx,
			`SELECT ticket_hash, expires_at, used_at FROM mcp_oauth_return_tickets WHERE request_id = ?`, requestID).
			Scan(&hash, &ticketExpires, &ticketUsed)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			st.status = WaitExpired // swept an hour after it expired
		case err != nil:
			return nil, err
		case ticketUsed.Valid:
			st.status = WaitUsed
		case ticketExpires <= now:
			st.status = WaitExpired
		default:
			st.status, st.ticketHash = WaitApproved, hash
		}
	}
	return st, nil
}

// Wait reports the state of request requestID to the waiting page that
// holds its waiter secret, with the app when the waiter matched. It changes
// nothing. A missing or wrong waiter, or an unknown request, is reported as
// WaitExpired with no app: the same answer, so it tells nothing. A
// connection revoked after the approval still reports WaitApproved; its
// Continue (RedeemWait) then refuses with ErrReturnRevoked.
func (s *Service) Wait(ctx context.Context, requestID, waiter string) (*WaitInfo, error) {
	st, err := s.waitStateOf(ctx, requestID, waiter)
	if err != nil {
		return nil, err
	}
	info := &WaitInfo{Status: st.status}
	if st.known {
		if c, cerr := s.Client(ctx, st.clientID); cerr == nil {
			info.Client = c
		}
	}
	return info, nil
}

// RedeemWait is the waiting page's Continue: it uses the approval of
// request requestID, once, exactly as the return link does (RedeemReturn):
// the two share the approval, so whichever is used first gets the code and
// the other gets ErrReturnUsed. It mints the code now and returns the
// redirect that carries it, with the request's state and the issuer, to
// the client's registered redirect URI.
//
// Before the person answered it returns ErrWaitPending; after a no,
// ErrWaitDenied; for a missing or wrong waiter or an unknown request,
// ErrWaitUnknown; for an approval that was used, expired (or replaced by a
// newer one) or revoked since, ErrReturnUsed, ErrReturnExpired or
// ErrReturnRevoked.
func (s *Service) RedeemWait(ctx context.Context, requestID, waiter string) (string, error) {
	st, err := s.waitStateOf(ctx, requestID, waiter)
	if err != nil {
		return "", err
	}
	switch {
	case !st.known:
		return "", ErrWaitUnknown
	case st.status == WaitApproved:
		// The claim is atomic: if the return link was used since
		// waitStateOf read the ticket, redeemTicket refuses as used.
		return s.redeemTicket(ctx, st.ticketHash)
	case st.status == WaitPending:
		return "", ErrWaitPending
	case st.status == WaitDenied:
		return "", ErrWaitDenied
	case st.status == WaitUsed:
		return "", ErrReturnUsed
	default:
		return "", ErrReturnExpired
	}
}
