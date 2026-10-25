// Package passkey lets the owner register passkeys (Face ID / Touch ID /
// security keys, via WebAuthn) and confirm high-risk inbox approvals with
// one. It implements inbox.PasskeyGate.
//
// Ceremonies are short-lived and single-use. Each one remembers the
// relying-party ID and origin it was started from (the dashboard's own
// origin), so toolyard works behind any hostname without configuration.
// They are rows of passkey_ceremonies, not process memory, so a ceremony
// started on one replica can finish on another (TEC-1630).
// An assertion's challenge embeds a hash of its purpose — for a decision,
// inbox.DecisionDigest — so the authenticator's signature covers exactly
// what the owner confirmed.
package passkey

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/tusharbhardwaj/toolyard/internal/inbox"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// CeremonyTTL is how long a started registration or assertion stays valid.
const CeremonyTTL = 3 * time.Minute

// Errors.
var (
	ErrNoCeremony = errors.New("that confirmation expired or was already used; try again")
	ErrOrigin     = errors.New("passkeys need the dashboard's own https (or localhost) origin")
	ErrNone       = errors.New("no passkey registered")
	ErrPurpose    = errors.New("this confirmation was made for something else")
	ErrClone      = errors.New("this passkey's signature counter went backwards; it may have been cloned")
)

// Passkey is one registered credential, without key material.
type Passkey struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	CreatedAt  int64  `json:"created_at"`
	LastUsedAt int64  `json:"last_used_at,omitempty"`
}

// User is the owner the ceremony is for.
type User struct {
	ID   string
	Name string
}

type ceremony struct {
	kind    string // register | assert
	userID  string
	purpose string
	session webauthn.SessionData
	rpID    string
	origin  string
	exp     time.Time
}

// Service stores passkeys and runs WebAuthn ceremonies.
type Service struct {
	db  *store.DB
	now func() time.Time
}

// maxCeremonies bounds the started ceremonies kept at once (runaway
// protection); past it the oldest go.
const maxCeremonies = 256

// New creates the service.
func New(db *store.DB) *Service {
	return &Service{db: db, now: time.Now}
}

// SetClock replaces the clock (tests).
func (s *Service) SetClock(now func() time.Time) { s.now = now }

// RelyingParty derives the WebAuthn relying-party ID and origin from the
// Origin header of a dashboard request. The origin must be https, or
// http on localhost (browsers only allow WebAuthn in secure contexts), and
// its host must be one of hosts (the request's Host, a proxy's
// X-Forwarded-Host, or the configured public URL's host) when any are given.
func RelyingParty(originHeader string, hosts ...string) (rpID, origin string, err error) {
	u, err := url.Parse(strings.TrimSpace(originHeader))
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return "", "", ErrOrigin
	}
	ok := true
	for i, h := range hosts {
		if i == 0 {
			ok = false
		}
		if h != "" && strings.EqualFold(u.Host, h) {
			ok = true
			break
		}
	}
	if !ok {
		return "", "", ErrOrigin
	}
	name := u.Hostname()
	if u.Scheme == "http" && name != "localhost" {
		return "", "", ErrOrigin
	}
	return name, u.Scheme + "://" + u.Host, nil
}

type waUser struct {
	id    []byte
	name  string
	creds []webauthn.Credential
}

func (u *waUser) WebAuthnID() []byte                         { return u.id }
func (u *waUser) WebAuthnName() string                       { return u.name }
func (u *waUser) WebAuthnDisplayName() string                { return u.name }
func (u *waUser) WebAuthnCredentials() []webauthn.Credential { return u.creds }

func newWA(rpID, origin string) (*webauthn.WebAuthn, error) {
	return webauthn.New(&webauthn.Config{
		RPID:          rpID,
		RPDisplayName: "toolyard",
		RPOrigins:     []string{origin},
		Timeouts: webauthn.TimeoutsConfig{
			Login:        webauthn.TimeoutConfig{Enforce: true, Timeout: CeremonyTTL},
			Registration: webauthn.TimeoutConfig{Enforce: true, Timeout: CeremonyTTL},
		},
	})
}

func (s *Service) user(ctx context.Context, u User) (*waUser, error) {
	creds, err := s.credentials(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	name := u.Name
	if name == "" {
		name = "owner"
	}
	return &waUser{id: []byte(u.ID), name: name, creds: creds}, nil
}

func (s *Service) credentials(ctx context.Context, userID string) ([]webauthn.Credential, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT credential FROM inbox_passkeys WHERE user_id = ? ORDER BY created_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []webauthn.Credential
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var c webauthn.Credential
		if err := json.Unmarshal([]byte(raw), &c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// put stores a started ceremony and returns its ID. Expired ceremonies
// are deleted first, and only the newest maxCeremonies are kept.
func (s *Service) put(ctx context.Context, c *ceremony) (string, error) {
	b := make([]byte, 18)
	_, _ = rand.Read(b)
	id := base64.RawURLEncoding.EncodeToString(b)
	sess, err := json.Marshal(c.session)
	if err != nil {
		return "", err
	}
	now := s.now().UnixMilli()
	if _, err := s.db.ExecContext(ctx, `DELETE FROM passkey_ceremonies WHERE expires_at < ?`, now); err != nil {
		return "", err
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO passkey_ceremonies(id, kind, user_id, purpose, session, rp_id, origin, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, id, c.kind, c.userID, c.purpose, string(sess), c.rpID, c.origin, now, c.exp.UnixMilli()); err != nil {
		return "", err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM passkey_ceremonies WHERE id NOT IN
		(SELECT id FROM passkey_ceremonies ORDER BY created_at DESC, id DESC LIMIT ?)`, maxCeremonies); err != nil {
		return "", err
	}
	return id, nil
}

// take removes and returns a ceremony (single use): one statement reads
// and deletes it, so two finishes of the same ceremony, on one replica or
// two, never both get it.
func (s *Service) take(ctx context.Context, id, kind string) (*ceremony, error) {
	var c ceremony
	var sess string
	var exp int64
	err := s.db.QueryRowContext(ctx, `DELETE FROM passkey_ceremonies WHERE id = ?
		RETURNING kind, user_id, purpose, session, rp_id, origin, expires_at`, id).
		Scan(&c.kind, &c.userID, &c.purpose, &sess, &c.rpID, &c.origin, &exp)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoCeremony
	}
	if err != nil {
		return nil, err
	}
	c.exp = time.UnixMilli(exp)
	if c.kind != kind || s.now().After(c.exp) {
		return nil, ErrNoCeremony
	}
	if err := json.Unmarshal([]byte(sess), &c.session); err != nil {
		return nil, ErrNoCeremony
	}
	return &c, nil
}

// BeginRegistration starts adding a passkey. It returns the ceremony ID
// and the options for navigator.credentials.create.
func (s *Service) BeginRegistration(ctx context.Context, u User, originHeader string, hosts ...string) (string, *protocol.CredentialCreation, error) {
	rpID, origin, err := RelyingParty(originHeader, hosts...)
	if err != nil {
		return "", nil, err
	}
	wa, err := newWA(rpID, origin)
	if err != nil {
		return "", nil, err
	}
	wu, err := s.user(ctx, u)
	if err != nil {
		return "", nil, err
	}
	excl := make([]protocol.CredentialDescriptor, 0, len(wu.creds))
	for _, c := range wu.creds {
		excl = append(excl, c.Descriptor())
	}
	opts, sess, err := wa.BeginRegistration(wu,
		webauthn.WithAuthenticatorSelection(protocol.AuthenticatorSelection{
			ResidentKey:      protocol.ResidentKeyRequirementPreferred,
			UserVerification: protocol.VerificationRequired,
		}),
		webauthn.WithExclusions(excl),
		webauthn.WithConveyancePreference(protocol.PreferNoAttestation))
	if err != nil {
		return "", nil, err
	}
	id, err := s.put(ctx, &ceremony{kind: "register", userID: u.ID, session: *sess, rpID: rpID, origin: origin, exp: s.now().Add(CeremonyTTL)})
	if err != nil {
		return "", nil, err
	}
	return id, opts, nil
}

// FinishRegistration verifies the browser's response and stores the passkey.
func (s *Service) FinishRegistration(ctx context.Context, u User, ceremonyID, name string, response []byte) (*Passkey, error) {
	c, err := s.take(ctx, ceremonyID, "register")
	if err != nil {
		return nil, err
	}
	if c.userID != u.ID {
		return nil, ErrNoCeremony
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = "Passkey"
	}
	if utf8.RuneCountInString(name) > 60 {
		name = string([]rune(name)[:60])
	}
	wa, err := newWA(c.rpID, c.origin)
	if err != nil {
		return nil, err
	}
	wu, err := s.user(ctx, u)
	if err != nil {
		return nil, err
	}
	parsed, err := protocol.ParseCredentialCreationResponseBody(bytes.NewReader(response))
	if err != nil {
		return nil, fmt.Errorf("couldn't read the passkey response: %w", err)
	}
	cred, err := wa.CreateCredential(wu, c.session, parsed)
	if err != nil {
		return nil, fmt.Errorf("the passkey couldn't be verified: %w", err)
	}
	if !cred.Flags.UserVerified {
		return nil, errors.New("the passkey didn't verify you (Face ID, Touch ID or PIN); try another authenticator")
	}
	raw, err := json.Marshal(cred)
	if err != nil {
		return nil, err
	}
	pk := &Passkey{ID: base64.RawURLEncoding.EncodeToString(cred.ID), Name: name, CreatedAt: s.now().UnixMilli()}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO inbox_passkeys(id, user_id, name, credential, created_at) VALUES (?,?,?,?,?)`,
		pk.ID, u.ID, pk.Name, string(raw), pk.CreatedAt); err != nil {
		return nil, err
	}
	return pk, nil
}

// purposeTag is the challenge suffix that commits to a purpose.
func purposeTag(purpose string) []byte {
	sum := sha256.Sum256([]byte("toolyard-passkey-v1\n" + purpose))
	return sum[:16]
}

// BeginAssertion starts a confirmation for purpose ("decide:<digest>" or
// "remove:<passkey id>"). It returns the ceremony ID and the options for
// navigator.credentials.get.
func (s *Service) BeginAssertion(ctx context.Context, u User, purpose, originHeader string, hosts ...string) (string, *protocol.CredentialAssertion, error) {
	rpID, origin, err := RelyingParty(originHeader, hosts...)
	if err != nil {
		return "", nil, err
	}
	wa, err := newWA(rpID, origin)
	if err != nil {
		return "", nil, err
	}
	wu, err := s.user(ctx, u)
	if err != nil {
		return "", nil, err
	}
	if len(wu.creds) == 0 {
		return "", nil, ErrNone
	}
	challenge := make([]byte, 32)
	_, _ = rand.Read(challenge[:16])
	copy(challenge[16:], purposeTag(purpose))
	opts, sess, err := wa.BeginLogin(wu, webauthn.WithUserVerification(protocol.VerificationRequired), webauthn.WithChallenge(challenge))
	if err != nil {
		return "", nil, err
	}
	id, err := s.put(ctx, &ceremony{kind: "assert", userID: u.ID, purpose: purpose, session: *sess, rpID: rpID, origin: origin, exp: s.now().Add(CeremonyTTL)})
	if err != nil {
		return "", nil, err
	}
	return id, opts, nil
}

// FinishAssertion verifies a confirmation made for purpose.
func (s *Service) FinishAssertion(ctx context.Context, ceremonyID, purpose string, response []byte) (string, error) {
	return s.finishAssertion(ctx, ceremonyID, purpose, response, "")
}
func (s *Service) finishAssertion(ctx context.Context, ceremonyID, purpose string, response []byte, expectedUser string) (string, error) {
	c, err := s.take(ctx, ceremonyID, "assert")
	if err != nil {
		return "", err
	}
	if expectedUser != "" && c.userID != expectedUser {
		return "", ErrPurpose
	}
	if subtle.ConstantTimeCompare([]byte(c.purpose), []byte(purpose)) != 1 {
		return "", ErrPurpose
	}
	ch, err := base64.RawURLEncoding.DecodeString(c.session.Challenge)
	if err != nil || len(ch) != 32 || subtle.ConstantTimeCompare(ch[16:], purposeTag(purpose)) != 1 {
		return "", ErrPurpose
	}
	wa, err := newWA(c.rpID, c.origin)
	if err != nil {
		return "", err
	}
	var name string
	_ = s.db.QueryRowContext(ctx, `SELECT COALESCE(username,'') FROM users WHERE id = ?`, c.userID).Scan(&name)
	wu, err := s.user(ctx, User{ID: c.userID, Name: name})
	if err != nil {
		return "", err
	}
	parsed, err := protocol.ParseCredentialRequestResponseBody(bytes.NewReader(response))
	if err != nil {
		return "", fmt.Errorf("couldn't read the passkey response: %w", err)
	}
	cred, err := wa.ValidateLogin(wu, c.session, parsed)
	if err != nil {
		return "", err
	}
	if cred.Authenticator.CloneWarning {
		return "", ErrClone
	}
	if !cred.Flags.UserVerified {
		return "", errors.New("the passkey didn't verify you")
	}
	raw, err := json.Marshal(cred)
	if err != nil {
		return "", err
	}
	id := base64.RawURLEncoding.EncodeToString(cred.ID)
	if _, err := s.db.ExecContext(ctx, `UPDATE inbox_passkeys SET credential = ?, last_used_at = ? WHERE id = ? AND user_id = ?`,
		string(raw), s.now().UnixMilli(), id, c.userID); err != nil {
		return "", err
	}
	return id, nil
}

// List returns the owner's passkeys.
func (s *Service) List(ctx context.Context, userID string) ([]Passkey, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, created_at, COALESCE(last_used_at, 0) FROM inbox_passkeys WHERE user_id = ? ORDER BY created_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Passkey{}
	for rows.Next() {
		var p Passkey
		if err := rows.Scan(&p.ID, &p.Name, &p.CreatedAt, &p.LastUsedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Remove deletes a passkey. Callers must first verify an assertion with
// purpose "remove:<id>".
func (s *Service) Remove(ctx context.Context, userID, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM inbox_passkeys WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// Reset deletes every passkey (the operator's recovery path when the
// owner has lost all their devices; see -inbox-reset-passkeys).
func (s *Service) Reset(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM inbox_passkeys`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Enabled implements inbox.PasskeyGate: any passkey registered.
func (s *Service) Enabled(ctx context.Context) bool {
	on, err := s.CheckEnabled(ctx)
	return err != nil || on
}

func (s *Service) CheckEnabled(ctx context.Context) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM inbox_passkeys`).Scan(&n)
	return n > 0, err
}

// VerifyCredential implements inbox.PasskeyGate: it checks the assertion
// and returns the id of the credential that signed it.
func (s *Service) VerifyCredential(ctx context.Context, digest string, a *inbox.PasskeyAssertion) (string, error) {
	if a == nil {
		return "", inbox.ErrPasskeyRequired
	}
	return s.FinishAssertion(ctx, a.SessionID, "decide:"+digest, a.Response)
}

// Verify is VerifyCredential for callers that only need the verdict.
func (s *Service) Verify(ctx context.Context, digest string, a *inbox.PasskeyAssertion) error {
	_, err := s.VerifyCredential(ctx, digest, a)
	return err
}

var _ inbox.PasskeyGate = (*Service)(nil)

func (s *Service) CheckEnabledFor(ctx context.Context, userID string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM inbox_passkeys WHERE user_id=?`, userID).Scan(&n)
	return n > 0, err
}
func (s *Service) VerifyCredentialFor(ctx context.Context, userID, digest string, a *inbox.PasskeyAssertion) (string, error) {
	if a == nil {
		return "", inbox.ErrPasskeyRequired
	}
	return s.finishAssertion(ctx, a.SessionID, "decide:"+digest, a.Response, userID)
}
