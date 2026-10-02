package oauth

// user_tokens.go is the per-user side of OAuth: on a per_user upstream each
// toolyard user connects their own account, toolyard keeps every person's
// token refreshed, and an agent's calls run with the token of the person
// who owns the agent. The OAuth client stays one per upstream; only the
// token rows (oauth_user_tokens) are per person.

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Connection states reported to the dashboard for one (upstream, user).
// StateActive and StateNeedsReauth come from the row; the two below are
// derived.
const (
	// ConnNeedsSignIn: the user has no token on this upstream yet.
	ConnNeedsSignIn = "needs_signin"
	// ConnExpired: the token ran out and cannot be refreshed (no refresh
	// token, or the refresh token itself expired); a new sign-in is needed.
	ConnExpired = "expired"
	// ConnConnected: a usable token is on file.
	ConnConnected = "connected"
)

// ErrNoUserToken is returned when a per-user operation names a (upstream,
// user) that has no token row.
var ErrNoUserToken = errors.New("oauth: user has not connected this server")

// ErrSuperseded means a refresh found the row changed (a reconnect or a
// disconnect) between its read and its write, so it wrote nothing.
var ErrSuperseded = errors.New("oauth: token row changed under the refresh; nothing written")

// UserTokenRecord is one person's token row on a per_user upstream,
// decrypted.
type UserTokenRecord struct {
	UpstreamName     string
	UserID           string
	AccessToken      string
	RefreshToken     string
	TokenType        string
	Scope            string
	AccountLabel     string
	ObtainedAt       time.Time
	AccessExpiresAt  time.Time
	RefreshExpiresAt time.Time
	LastRefreshAt    time.Time
	RefreshFailures  int
	State            string
	LastError        string
	// refreshEnc is the refresh_token_enc ciphertext as read; a refresh
	// writes back only while the row still carries it.
	refreshEnc string
}

// lockUser serialises the read-then-write operations on one person's row
// (exchange, refresh, disconnect, a 401 mark). The returned func unlocks.
func (s *Service) lockUser(upstream, userID string) func() {
	k := userKey(upstream, userID)
	s.mu.Lock()
	m := s.userLocks[k]
	if m == nil {
		m = &sync.Mutex{}
		s.userLocks[k] = m
	}
	s.mu.Unlock()
	m.Lock()
	return m.Unlock
}

// UserConnection is what the API shows about one (upstream, user): the
// state and its timestamps, never a token.
type UserConnection struct {
	UpstreamName    string    `json:"server"`
	UserID          string    `json:"user_id"`
	State           string    `json:"state"`
	AccountLabel    string    `json:"account_label,omitempty"`
	ConnectedAt     time.Time `json:"-"`
	AccessExpiresAt time.Time `json:"-"`
	LastRefreshAt   time.Time `json:"-"`
	RefreshFailures int       `json:"refresh_failures,omitempty"`
	LastError       string    `json:"last_error,omitempty"`
}

// UserAAD binds a per-user token envelope to its (upstream, user, kind), so
// a row copied onto another user's row (or onto the shared row) fails to
// open. The prefix differs from the shared AAD so no upstream name can make
// the two coincide.
func UserAAD(upstream, userID, kind string) []byte {
	return []byte("toolyard.oauth.user.v1|" + upstream + "|" + userID + "|" + kind)
}

// userKey is the in-memory key for one (upstream, user). Upstream names
// never contain NUL, so it cannot collide with a shared upstream key.
func userKey(upstream, userID string) string { return upstream + "\x00" + userID }

// ---------------------------------------------------------------------------
// Bearer cache and header function
// ---------------------------------------------------------------------------

// UserHeaderFunc returns the per-request header function for a per_user
// upstream: Authorization: Bearer <that user's live token>, or nothing
// when the user has no usable token (the dial then fails with a clear
// 401 rather than running as somebody else).
func (s *Service) UserHeaderFunc(upstream string) func(ctx context.Context, userID string) map[string]string {
	return func(_ context.Context, userID string) map[string]string {
		if userID == "" {
			return nil
		}
		tok := s.currentUserBearer(upstream, userID)
		if tok == "" {
			return nil
		}
		return map[string]string{"Authorization": "Bearer " + tok}
	}
}

func (s *Service) currentUserBearer(upstream, userID string) string {
	k := userKey(upstream, userID)
	s.mu.RLock()
	v, ok := s.userBearers[k]
	s.mu.RUnlock()
	if !ok {
		s.cacheUserBearerFromDB(upstream, userID)
		s.mu.RLock()
		v, ok = s.userBearers[k]
		s.mu.RUnlock()
		if !ok {
			return ""
		}
	}
	if tok, ok := v.Load().(string); ok {
		return tok
	}
	return ""
}

func (s *Service) setUserBearer(upstream, userID, tok string) {
	k := userKey(upstream, userID)
	s.mu.Lock()
	v := s.userBearers[k]
	if v == nil {
		v = &atomic.Value{}
		s.userBearers[k] = v
	}
	s.mu.Unlock()
	v.Store(tok)
}

func (s *Service) dropUserBearer(upstream, userID string) {
	s.mu.Lock()
	delete(s.userBearers, userKey(upstream, userID))
	s.mu.Unlock()
}

// cacheUserBearerFromDB warms the cache for one (upstream, user). A row
// that is not active (needs_reauth) caches an empty bearer, so the header
// function sends nothing for it.
func (s *Service) cacheUserBearerFromDB(upstream, userID string) {
	tok, err := s.GetUserToken(context.Background(), upstream, userID)
	if err != nil || tok == nil {
		return
	}
	if tok.State != StateActive {
		s.setUserBearer(upstream, userID, "")
		return
	}
	s.setUserBearer(upstream, userID, tok.AccessToken)
}

// ---------------------------------------------------------------------------
// Rows
// ---------------------------------------------------------------------------

// PutUserToken upserts one person's encrypted token row.
func (s *Service) PutUserToken(ctx context.Context, rec *UserTokenRecord) error {
	atEnc, err := s.cipher.Seal([]byte(rec.AccessToken), UserAAD(rec.UpstreamName, rec.UserID, "access"))
	if err != nil {
		return err
	}
	rtEnc, err := s.cipher.Seal([]byte(rec.RefreshToken), UserAAD(rec.UpstreamName, rec.UserID, "refresh"))
	if err != nil {
		return err
	}
	state := rec.State
	if state == "" {
		state = StateActive
	}
	_, err = s.db.ExecContext(ctx, `
        INSERT INTO oauth_user_tokens(upstream_name, user_id, access_token_enc, refresh_token_enc,
            token_type, scope, account_label, obtained_at, access_expires_at, refresh_expires_at,
            last_refresh_at, refresh_failures, state, last_error)
        VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)
        ON CONFLICT(upstream_name, user_id) DO UPDATE SET
            access_token_enc = excluded.access_token_enc,
            refresh_token_enc = excluded.refresh_token_enc,
            token_type = excluded.token_type,
            scope = excluded.scope,
            account_label = excluded.account_label,
            obtained_at = excluded.obtained_at,
            access_expires_at = excluded.access_expires_at,
            refresh_expires_at = excluded.refresh_expires_at,
            last_refresh_at = excluded.last_refresh_at,
            refresh_failures = excluded.refresh_failures,
            state = excluded.state,
            last_error = excluded.last_error
    `,
		rec.UpstreamName, rec.UserID, nullStr(atEnc), nullStr(rtEnc),
		nullStr(rec.TokenType), nullStr(rec.Scope), nullStr(rec.AccountLabel),
		nullTimeMS(rec.ObtainedAt), nullTimeMS(rec.AccessExpiresAt), nullTimeMS(rec.RefreshExpiresAt),
		nullTimeMS(rec.LastRefreshAt), rec.RefreshFailures, state, nullStr(rec.LastError))
	return err
}

const userTokenColumns = `upstream_name, user_id, COALESCE(access_token_enc,''), COALESCE(refresh_token_enc,''),
               COALESCE(token_type,''), COALESCE(scope,''), COALESCE(account_label,''),
               COALESCE(obtained_at,0), COALESCE(access_expires_at,0),
               COALESCE(refresh_expires_at,0), COALESCE(last_refresh_at,0),
               refresh_failures, state, COALESCE(last_error,'')`

type rowScanner interface {
	Scan(dest ...any) error
}

// scanUserToken reads userTokenColumns and decrypts the two token
// envelopes under the row's own AAD.
func (s *Service) scanUserToken(row rowScanner) (*UserTokenRecord, error) {
	var rec UserTokenRecord
	var atEnc, rtEnc string
	var ob, ax, rx, lr int64
	if err := row.Scan(&rec.UpstreamName, &rec.UserID, &atEnc, &rtEnc, &rec.TokenType, &rec.Scope, &rec.AccountLabel,
		&ob, &ax, &rx, &lr, &rec.RefreshFailures, &rec.State, &rec.LastError); err != nil {
		return nil, err
	}
	if atEnc != "" {
		pt, err := s.cipher.Open(atEnc, UserAAD(rec.UpstreamName, rec.UserID, "access"))
		if err != nil {
			return nil, err
		}
		rec.AccessToken = string(pt)
	}
	if rtEnc != "" {
		pt, err := s.cipher.Open(rtEnc, UserAAD(rec.UpstreamName, rec.UserID, "refresh"))
		if err != nil {
			return nil, err
		}
		rec.RefreshToken = string(pt)
		rec.refreshEnc = rtEnc
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
	return &rec, nil
}

// GetUserToken returns one person's decrypted token row, or nil when the
// user has not connected the upstream.
func (s *Service) GetUserToken(ctx context.Context, upstream, userID string) (*UserTokenRecord, error) {
	rec, err := s.scanUserToken(s.db.QueryRowContext(ctx,
		`SELECT `+userTokenColumns+` FROM oauth_user_tokens WHERE upstream_name = ? AND user_id = ?`, upstream, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return rec, err
}

// connectionOf derives the dashboard state of a row: the row's own state
// when it says needs_reauth, expired when the token ran out with no way
// to refresh it, else connected.
func connectionOf(rec *UserTokenRecord, now time.Time) UserConnection {
	c := UserConnection{
		UpstreamName:    rec.UpstreamName,
		UserID:          rec.UserID,
		State:           ConnConnected,
		AccountLabel:    rec.AccountLabel,
		ConnectedAt:     rec.ObtainedAt,
		AccessExpiresAt: rec.AccessExpiresAt,
		LastRefreshAt:   rec.LastRefreshAt,
		RefreshFailures: rec.RefreshFailures,
		LastError:       rec.LastError,
	}
	switch {
	case rec.State == StateNeedsReauth:
		c.State = StateNeedsReauth
	case !rec.RefreshExpiresAt.IsZero() && now.After(rec.RefreshExpiresAt):
		c.State = ConnExpired
	case rec.RefreshToken == "" && !rec.AccessExpiresAt.IsZero() && now.After(rec.AccessExpiresAt):
		c.State = ConnExpired
	}
	return c
}

// listUserTokens scans every row matching where, decrypting none of the
// tokens (the state columns are enough for a listing). The rows are read
// fully before anything else runs on the database.
func (s *Service) listUserConnections(ctx context.Context, where string, args ...any) ([]UserConnection, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT upstream_name, user_id, COALESCE(account_label,''), COALESCE(obtained_at,0),
               COALESCE(access_expires_at,0), COALESCE(refresh_expires_at,0), COALESCE(last_refresh_at,0),
               refresh_failures, state, COALESCE(last_error,''), refresh_token_enc IS NOT NULL
        FROM oauth_user_tokens WHERE `+where+` ORDER BY upstream_name, user_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	now := time.Now()
	var out []UserConnection
	for rows.Next() {
		var rec UserTokenRecord
		var ob, ax, rx, lr int64
		var hasRefresh bool
		if err := rows.Scan(&rec.UpstreamName, &rec.UserID, &rec.AccountLabel, &ob, &ax, &rx, &lr,
			&rec.RefreshFailures, &rec.State, &rec.LastError, &hasRefresh); err != nil {
			return nil, err
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
		if hasRefresh {
			rec.RefreshToken = "present" // connectionOf only asks whether one exists
		}
		out = append(out, connectionOf(&rec, now))
	}
	return out, rows.Err()
}

// ListUserConnections reports every person connected to one upstream, for
// the admin's "who signs in" view.
func (s *Service) ListUserConnections(ctx context.Context, upstream string) ([]UserConnection, error) {
	return s.listUserConnections(ctx, `upstream_name = ?`, upstream)
}

// UserConnections reports one person's connections across every upstream,
// keyed by upstream name, for "My connections".
func (s *Service) UserConnections(ctx context.Context, userID string) (map[string]UserConnection, error) {
	list, err := s.listUserConnections(ctx, `user_id = ?`, userID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]UserConnection, len(list))
	for _, c := range list {
		out[c.UpstreamName] = c
	}
	return out, nil
}

// ConnectedUsers lists the users who hold a usable token on upstream (the
// ones the gateway may open a connection for).
func (s *Service) ConnectedUsers(ctx context.Context, upstream string) ([]string, error) {
	list, err := s.listUserConnections(ctx, `upstream_name = ?`, upstream)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, c := range list {
		if c.State == ConnConnected {
			out = append(out, c.UserID)
		}
	}
	return out, nil
}

// UserConnected reports whether one user holds a usable token on upstream.
func (s *Service) UserConnected(ctx context.Context, upstream, userID string) (bool, error) {
	list, err := s.listUserConnections(ctx, `upstream_name = ? AND user_id = ?`, upstream, userID)
	if err != nil {
		return false, err
	}
	return len(list) == 1 && list[0].State == ConnConnected, nil
}

// deleteUserTokens drops every person's row on an upstream and their
// cached bearers. Called from DeleteClient.
func (s *Service) deleteUserTokens(ctx context.Context, upstream string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM oauth_user_tokens WHERE upstream_name = ?`, upstream)
	prefix := upstream + "\x00"
	s.mu.Lock()
	for k := range s.userBearers {
		if strings.HasPrefix(k, prefix) {
			delete(s.userBearers, k)
		}
	}
	s.mu.Unlock()
	return err
}

// ---------------------------------------------------------------------------
// Flow: begin, exchange, refresh, reauth, disconnect
// ---------------------------------------------------------------------------

// BeginForUser starts a Mode A flow whose token will belong to userID.
// Returns the authorization URL the person's browser should open. The flow
// is always browser-bound: a per-user token is stored only for a browser
// the API can tie to the person.
func (s *Service) BeginForUser(ctx context.Context, upstream, userID string) (authURL, state string, err error) {
	if strings.TrimSpace(userID) == "" {
		return "", "", errors.New("oauth: per-user flow needs a user")
	}
	return s.beginFlow(ctx, upstream, userID, ModeCallback, nil, pendingFlags{perUser: true, browserBound: true})
}

// ExchangeCodeForUser swaps the authorization code for tokens and stores
// them on userID's row. The caller has already checked that the browser
// completing the flow is the person's.
func (s *Service) ExchangeCodeForUser(ctx context.Context, upstream, userID, code, verifier string) (*UserTokenRecord, error) {
	return s.exchangeCodeForUser(ctx, upstream, userID, code, verifier, nil)
}

// RefreshUser swaps one person's refresh_token for a fresh access_token.
// invalid_grant marks that person's row needs_reauth and returns
// ErrNeedsReauth; a row that changed meanwhile is ErrSuperseded.
func (s *Service) RefreshUser(ctx context.Context, upstream, userID string) (*UserTokenRecord, error) {
	defer s.lockUser(upstream, userID)()
	cur, err := s.GetUserToken(ctx, upstream, userID)
	if err != nil {
		return nil, err
	}
	if cur == nil {
		return nil, ErrNoUserToken
	}
	if cur.RefreshToken == "" {
		return nil, errors.New("oauth: no refresh token available")
	}
	return s.refreshUserLocked(ctx, upstream, userID, cur)
}

// RefreshUserToken is what the gateway calls when the upstream answered a
// per-user call with 401: the access token may simply have expired. It
// refreshes once; nil means a new token is in place and the call can be
// retried. The row is marked needs_reauth only when there is nothing to
// refresh with (no refresh token, or the IdP says invalid_grant); a
// transient IdP failure leaves it alone and is returned as is.
func (s *Service) RefreshUserToken(ctx context.Context, upstream, userID string) error {
	defer s.lockUser(upstream, userID)()
	cur, err := s.GetUserToken(ctx, upstream, userID)
	if err != nil {
		return err
	}
	if cur == nil {
		return ErrNoUserToken
	}
	if cur.State == StateNeedsReauth {
		return ErrNeedsReauth
	}
	if cur.RefreshToken == "" {
		s.markUserNeedsReauth(ctx, upstream, userID, "upstream rejected the token (401) and there is no refresh token")
		return ErrNeedsReauth
	}
	_, err = s.refreshUserLocked(ctx, upstream, userID, cur)
	return err
}

// FreshenUserToken runs before the gateway dials a person's connection: an
// access token whose expiry has already passed is refreshed first, so a
// restart longer than the token's lifetime does not turn into a 401 (and
// a needless re-auth) for everyone. A token still within its lifetime is
// left to the refresher's schedule. ErrNeedsReauth means the person must
// sign in again; any other error is transient.
func (s *Service) FreshenUserToken(ctx context.Context, upstream, userID string) error {
	defer s.lockUser(upstream, userID)()
	cur, err := s.GetUserToken(ctx, upstream, userID)
	if err != nil {
		return err
	}
	if cur == nil {
		return ErrNoUserToken
	}
	if cur.State == StateNeedsReauth {
		return ErrNeedsReauth
	}
	if cur.AccessExpiresAt.IsZero() || time.Now().Before(cur.AccessExpiresAt) {
		return nil
	}
	if cur.RefreshToken == "" {
		s.markUserNeedsReauth(ctx, upstream, userID, "access token expired and there is no refresh token")
		return ErrNeedsReauth
	}
	_, err = s.refreshUserLocked(ctx, upstream, userID, cur)
	return err
}

// refreshUserLocked is the refresh itself; the caller holds the user's
// lock and has read cur. The write goes through only while the row still
// carries the refresh token that was read, so a disconnect or a fresh
// sign-in that happened while the provider was answering is never
// overwritten.
func (s *Service) refreshUserLocked(ctx context.Context, upstream, userID string, cur *UserTokenRecord) (*UserTokenRecord, error) {
	cli, err := s.GetClient(ctx, upstream)
	if err != nil {
		return nil, err
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
		if strings.Contains(err.Error(), "invalid_grant") {
			s.markUserNeedsReauth(ctx, upstream, userID, err.Error())
			return nil, ErrNeedsReauth
		}
		return nil, err
	}
	rec := userTokenRecordFromResponse(upstream, userID, tr)
	if rec.RefreshToken == "" {
		rec.RefreshToken = cur.RefreshToken
		rec.RefreshExpiresAt = cur.RefreshExpiresAt
	}
	if rec.AccountLabel == "" {
		rec.AccountLabel = cur.AccountLabel
	}
	rec.LastRefreshAt = time.Now()
	if err := s.updateUserTokenIfUnchanged(ctx, rec, cur.refreshEnc); err != nil {
		return nil, err
	}
	s.setUserBearer(upstream, userID, rec.AccessToken)
	return rec, nil
}

// updateUserTokenIfUnchanged writes a refreshed token over the existing
// row only while that row still carries refreshEnc, the refresh token
// ciphertext the refresh started from. No row updated means the row was
// deleted or replaced meanwhile: ErrSuperseded, nothing written, cache
// untouched.
func (s *Service) updateUserTokenIfUnchanged(ctx context.Context, rec *UserTokenRecord, refreshEnc string) error {
	atEnc, err := s.cipher.Seal([]byte(rec.AccessToken), UserAAD(rec.UpstreamName, rec.UserID, "access"))
	if err != nil {
		return err
	}
	rtEnc, err := s.cipher.Seal([]byte(rec.RefreshToken), UserAAD(rec.UpstreamName, rec.UserID, "refresh"))
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `
        UPDATE oauth_user_tokens
        SET access_token_enc = ?, refresh_token_enc = ?, token_type = ?, scope = ?, account_label = ?,
            obtained_at = ?, access_expires_at = ?, refresh_expires_at = ?, last_refresh_at = ?,
            refresh_failures = 0, state = ?, last_error = NULL
        WHERE upstream_name = ? AND user_id = ? AND refresh_token_enc = ?`,
		nullStr(atEnc), nullStr(rtEnc), nullStr(rec.TokenType), nullStr(rec.Scope), nullStr(rec.AccountLabel),
		nullTimeMS(rec.ObtainedAt), nullTimeMS(rec.AccessExpiresAt), nullTimeMS(rec.RefreshExpiresAt),
		nullTimeMS(rec.LastRefreshAt), StateActive,
		rec.UpstreamName, rec.UserID, refreshEnc)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrSuperseded
	}
	return nil
}

func userTokenRecordFromResponse(upstream, userID string, tr *tokenResponse) *UserTokenRecord {
	now := time.Now()
	rec := &UserTokenRecord{
		UpstreamName: upstream,
		UserID:       userID,
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		TokenType:    tr.TokenType,
		Scope:        tr.Scope,
		AccountLabel: accountLabelFromIDToken(tr.IDToken),
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

// markUserNeedsReauth flips one person's row to needs_reauth, drops their
// cached bearer, asks the gateway to close their connection, and tells
// that person (SSE + push) to reconnect from My connections. Nobody else
// is notified.
func (s *Service) markUserNeedsReauth(ctx context.Context, upstream, userID, msg string) {
	_, _ = s.db.ExecContext(ctx, `
        UPDATE oauth_user_tokens
        SET state = ?, last_error = ?, refresh_failures = refresh_failures + 1
        WHERE upstream_name = ? AND user_id = ?`, StateNeedsReauth, msg, upstream, userID)
	s.setUserBearer(upstream, userID, "")
	s.mu.RLock()
	hook := s.userReauthHook
	s.mu.RUnlock()
	if hook != nil {
		go hook(upstream, userID)
	}
	if s.bus != nil {
		s.bus.Publish("mcp_oauth_user_needs_reauth", map[string]any{
			"upstream": upstream,
			"user_id":  userID,
			"error":    msg,
		})
	}
	if s.notifier != nil {
		_ = s.notifier.Notify(ctx, userID, map[string]any{
			"title": "toolyard: " + upstream + " needs you to sign in again",
			"body":  "Open My connections and reconnect " + upstream + ".",
			"url":   "/#connections",
		})
	}
}

// MarkUserUnauthorized is the public entry point for the gateway when the
// upstream still answers 401 after a refresh: that person's token is dead
// as far as the upstream is concerned. Idempotent; a user with no row is a
// no-op.
func (s *Service) MarkUserUnauthorized(ctx context.Context, upstream, userID, msg string) {
	defer s.lockUser(upstream, userID)()
	if tok, err := s.GetUserToken(ctx, upstream, userID); err != nil || tok == nil || tok.State == StateNeedsReauth {
		return
	}
	s.markUserNeedsReauth(ctx, upstream, userID, msg)
}

// DisconnectUser revokes one person's token at the IdP (best-effort, when
// it has a revocation endpoint) and deletes their row. The client record
// and everyone else's rows stay. It takes the person's lock, so a refresh
// that is waiting on the provider finishes (and then finds its write
// superseded) before the row goes.
func (s *Service) DisconnectUser(ctx context.Context, upstream, userID string) error {
	defer s.lockUser(upstream, userID)()
	tok, err := s.GetUserToken(ctx, upstream, userID)
	if err != nil {
		return err
	}
	if tok == nil {
		return ErrNoUserToken
	}
	cli, _ := s.GetClient(ctx, upstream)
	s.revoke(ctx, cli, tok.RefreshToken, tok.AccessToken)
	if _, err := s.db.ExecContext(ctx, `DELETE FROM oauth_user_tokens WHERE upstream_name = ? AND user_id = ?`, upstream, userID); err != nil {
		return err
	}
	s.dropUserBearer(upstream, userID)
	return nil
}

// accountLabelFromIDToken is the best-effort account name: the email claim
// of an OIDC id_token when the IdP sent one. The token is only decoded,
// never verified; the label is display-only and nothing trusts it.
func accountLabelFromIDToken(idToken string) string {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return ""
	}
	var claims struct {
		Email string `json:"email"`
	}
	if json.Unmarshal(raw, &claims) != nil {
		return ""
	}
	email := strings.TrimSpace(claims.Email)
	if len(email) > 254 {
		return ""
	}
	return email
}
