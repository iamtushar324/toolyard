// Package federation binds server-owned connections to a registered environment,
// verified Toolyard instance and independently verified user subject.
package federation

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
	"github.com/tusharbhardwaj/toolyard/internal/sealbox"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

const Protocol = "toolyard-federation-v1"

var ErrUnauthorized = errors.New("federation assertion or trust is invalid")
var ErrRevoked = errors.New("connection_revoked")
var ErrUnavailable = errors.New("membership verification unavailable")
var ErrTrustRevoked = errors.New("trust_revoked")
var ErrNotMember = errors.New("not_org_member")
var ErrConflict = errors.New("federation trust or credential version changed")

type Profile struct{ Email, Name, Avatar string }
type Service struct {
	db         *store.DB
	identity   *identity.Service
	cipher     *sealbox.Cipher
	InstanceID string
	// Membership obtains the current organisation membership from the IdP,
	// independently of claims in the environment's signed assertion.
	Membership func(context.Context, string) (Profile, error)
	now        func() time.Time
	mu         sync.Mutex
}

func New(ctx context.Context, db *store.DB, id *identity.Service, cipher *sealbox.Cipher) (*Service, error) {
	s := &Service{db: db, identity: id, cipher: cipher, now: time.Now}
	_, err := db.ExecContext(ctx, `INSERT INTO federation_instance(id) SELECT ? WHERE NOT EXISTS (SELECT 1 FROM federation_instance)`, "tyi_"+uuid.NewString())
	if err != nil {
		return nil, err
	}
	err = db.QueryRowContext(ctx, `SELECT id FROM federation_instance LIMIT 1`).Scan(&s.InstanceID)
	return s, err
}

type Trust struct {
	Issuer    string `json:"issuer"`
	PublicKey string `json:"public_key"`
	Kid       string `json:"kid"`
	Origin    string `json:"origin"`
}

func validOrigin(raw string) bool {
	u, e := url.Parse(raw)
	return e == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && (u.Path == "" || u.Path == "/")
}
func (s *Service) Register(ctx context.Context, t Trust, adminID string) error {
	key, err := base64.StdEncoding.DecodeString(t.PublicKey)
	if err != nil {
		key, err = base64.RawStdEncoding.DecodeString(t.PublicKey)
	}
	if err != nil || len(key) != ed25519.PublicKeySize || t.Issuer == "" || len(t.Issuer) > 128 || t.Kid == "" || !validOrigin(t.Origin) {
		return ErrUnauthorized
	}
	t.PublicKey = base64.StdEncoding.EncodeToString(key)
	t.Origin = strings.TrimRight(t.Origin, "/")
	u, err := s.identity.GetUserByID(ctx, adminID)
	if err != nil || u.Role != identity.RoleAdmin || u.Status != identity.StatusActive {
		return ErrUnauthorized
	}
	var old Trust
	var status string
	err = s.db.QueryRowContext(ctx, `SELECT issuer,public_key,kid,origin,status FROM federation_issuers WHERE issuer=?`, t.Issuer).Scan(&old.Issuer, &old.PublicKey, &old.Kid, &old.Origin, &status)
	if err == nil {
		if old != t {
			return ErrConflict
		}
		if status == "active" {
			return nil
		}
		// Only fresh, independent administrator registration revives an environment.
		// Individual user revocations remain terminal.
		tx, e := s.db.BeginTx(ctx, nil)
		if e != nil {
			return e
		}
		defer tx.Rollback()
		if _, e = tx.ExecContext(ctx, `UPDATE federation_issuers SET status='active',registered_by=? WHERE issuer=?`, adminID, t.Issuer); e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, `UPDATE federation_connections SET status='reconnect' WHERE issuer=? AND status='environment_removed'`, t.Issuer); e != nil {
			return e
		}
		return tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO federation_issuers(issuer,public_key,kid,origin,registered_by,created_at) VALUES(?,?,?,?,?,?)`, t.Issuer, t.PublicKey, t.Kid, t.Origin, adminID, s.now().UnixMilli())
	return err
}

type Claims struct{ jwt.RegisteredClaims }
type Principal struct {
	Issuer, Subject, Origin string
	User                    *identity.User
}

func (s *Service) Verify(ctx context.Context, assertion string) (*Principal, error) {
	return s.verify(ctx, assertion, false)
}

// VerifyRevocation permits an idempotent cleanup retry after trust removal.
// Removed trust cannot connect, hand off, or create receivers.
func (s *Service) VerifyRevocation(ctx context.Context, assertion string) (*Principal, error) {
	return s.verify(ctx, assertion, true)
}
func (s *Service) verify(ctx context.Context, assertion string, cleanup bool) (*Principal, error) {
	if len(assertion) > 16384 || s.Membership == nil {
		return nil, ErrUnauthorized
	}
	var c Claims
	var origin string
	token, err := jwt.ParseWithClaims(assertion, &c, func(t *jwt.Token) (any, error) {
		if t.Method != jwt.SigningMethodEdDSA {
			return nil, ErrUnauthorized
		}
		claims, ok := t.Claims.(*Claims)
		if !ok || claims.Issuer == "" {
			return nil, ErrUnauthorized
		}
		var publicKey, kid string
		e := s.db.QueryRowContext(ctx, `SELECT public_key,kid,origin FROM federation_issuers WHERE issuer=? AND (status='active' OR ?=1)`, claims.Issuer, cleanup).Scan(&publicKey, &kid, &origin)
		if e != nil {
			return nil, ErrTrustRevoked
		}
		if t.Header["kid"] != kid {
			return nil, ErrUnauthorized
		}
		raw, e := base64.StdEncoding.DecodeString(publicKey)
		if e != nil {
			return nil, ErrUnauthorized
		}
		return ed25519.PublicKey(raw), nil
	}, jwt.WithValidMethods([]string{"EdDSA"}), jwt.WithAudience(s.InstanceID), jwt.WithExpirationRequired(), jwt.WithIssuedAt(), jwt.WithTimeFunc(s.now))
	if errors.Is(err, ErrTrustRevoked) {
		return nil, ErrTrustRevoked
	}
	if err != nil || !token.Valid || c.Subject == "" || c.ID == "" || c.IssuedAt == nil || c.ExpiresAt == nil || c.ExpiresAt.Sub(c.IssuedAt.Time) > 2*time.Minute || c.IssuedAt.Before(s.now().Add(-2*time.Minute)) {
		return nil, ErrUnauthorized
	}
	// Unique jti persists before using the assertion, even if a later network
	// failure occurs. Retries sign a new assertion; permission does not replay.
	_, err = s.db.ExecContext(ctx, `INSERT INTO federation_replays(issuer,jti,expires_at) VALUES(?,?,?)`, c.Issuer, c.ID, c.ExpiresAt.UnixMilli())
	if err != nil {
		return nil, ErrUnauthorized
	}
	profile, err := s.Membership(ctx, c.Subject)
	if err != nil {
		return nil, err
	}
	// New users are members. This path never assigns an administrator role or
	// links an owner by email. Returning identity is matched by verified subject.
	u, err := s.identity.UpsertClerkUser(ctx, identity.ClerkProfile{ClerkUserID: c.Subject, Email: profile.Email, DisplayName: profile.Name, AvatarURL: profile.Avatar}, "")
	if err != nil {
		return nil, err
	}
	if u.Status != identity.StatusActive {
		return nil, ErrRevoked
	}
	return &Principal{Issuer: c.Issuer, Subject: c.Subject, Origin: origin, User: u}, nil
}

type Credential struct {
	Token     string `json:"token"`
	Email     string `json:"email"`
	AgentID   string `json:"agent_id"`
	UserID    string `json:"user_id"`
	ExpiresAt string `json:"expires_at"`
	Version   int    `json:"credential_version"`
}

func (s *Service) Connect(ctx context.Context, p *Principal, expectedVersion int) (*Credential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var accountStatus string
	if err = tx.QueryRowContext(ctx, `SELECT status FROM users WHERE id=?`, p.User.ID).Scan(&accountStatus); err != nil || accountStatus != identity.StatusActive {
		return nil, ErrRevoked
	}
	var trust string
	if err = tx.QueryRowContext(ctx, `SELECT status FROM federation_issuers WHERE issuer=?`, p.Issuer).Scan(&trust); err != nil || trust != "active" {
		return nil, ErrRevoked
	}
	var enc, agent, status, userID string
	var version int
	var expires int64
	err = tx.QueryRowContext(ctx, `SELECT user_id,agent_id,credential_enc,version,status,expires_at FROM federation_connections WHERE issuer=? AND subject=?`, p.Issuer, p.Subject).Scan(&userID, &agent, &enc, &version, &status, &expires)
	exists := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if exists {
		if userID != p.User.ID || (status != "active" && status != "reconnect") {
			return nil, ErrRevoked
		}
		var disabled int
		var owner string
		if err = tx.QueryRowContext(ctx, `SELECT disabled,owner_user FROM agents WHERE id=?`, agent).Scan(&disabled, &owner); err != nil || disabled != 0 || owner != userID {
			return nil, ErrRevoked
		}
		if status == "active" {
			raw, e := s.cipher.Open(enc, []byte("federation|"+p.Issuer+"|"+p.Subject))
			if e != nil {
				return nil, e
			}
			token := string(raw)
			// Check the current hash in the same transaction. External rotation is revocation.
			sum := sha256.Sum256([]byte(token))
			var stored string
			if e = tx.QueryRowContext(ctx, `SELECT token_hash FROM agents WHERE id=?`, agent).Scan(&stored); e != nil || stored != hex.EncodeToString(sum[:]) {
				return nil, ErrRevoked
			}
			if expectedVersion > version {
				return nil, ErrConflict
			}
			if expires > s.now().Add(24*time.Hour).UnixMilli() || expectedVersion < version {
				if expires <= s.now().UnixMilli() {
					return nil, ErrConflict
				}
				return &Credential{Token: token, AgentID: agent, UserID: userID, Email: p.User.Email, ExpiresAt: time.UnixMilli(expires).UTC().Format(time.RFC3339), Version: version}, nil
			}
			if expectedVersion != version {
				return nil, ErrConflict
			}
		} else if expectedVersion != 0 && expectedVersion != version {
			return nil, ErrConflict
		}
	} else {
		if expectedVersion != 0 {
			return nil, ErrConflict
		}
		agent, userID = "ag_"+uuid.NewString(), p.User.ID
	}
	token, hash, err := identity.NewAgentToken(agent)
	if err != nil {
		return nil, err
	}
	expires = s.now().Add(30 * 24 * time.Hour).UnixMilli()
	enc, err = s.cipher.Seal([]byte(token), []byte("federation|"+p.Issuer+"|"+p.Subject))
	if err != nil {
		return nil, err
	}
	now := s.now().UnixMilli()
	if exists {
		res, e := tx.ExecContext(ctx, `UPDATE federation_connections SET credential_enc=?,version=version+1,expires_at=?,status='active' WHERE issuer=? AND subject=? AND version=? AND status=?`, enc, expires, p.Issuer, p.Subject, version, status)
		if e != nil {
			return nil, e
		}
		n, e := res.RowsAffected()
		if e != nil || n != 1 {
			return nil, ErrConflict
		}
		_, err = tx.ExecContext(ctx, `UPDATE agents SET prev_token_hash=CASE WHEN ?='active' THEN token_hash ELSE NULL END,prev_token_expires=CASE WHEN ?='active' THEN ? ELSE NULL END,token_hash=?,last_seen=?,enroll_code=NULL,enroll_expires=NULL WHERE id=? AND disabled=0`, status, status, s.now().Add(identity.DefaultRotateGrace).UnixMilli(), hash, now, agent)
	} else {
		envHash := sha256.Sum256([]byte(p.Issuer))
		_, err = tx.ExecContext(ctx, `INSERT INTO agents(id,name,owner_user,token_hash,last_seen,created_at) VALUES(?,?,?,?,?,?)`, agent, "T3 "+hex.EncodeToString(envHash[:8]), userID, hash, now, now)
		if err == nil {
			_, err = tx.ExecContext(ctx, `INSERT INTO federation_connections(issuer,subject,user_id,agent_id,credential_enc,version,expires_at) VALUES(?,?,?,?,?,1,?)`, p.Issuer, p.Subject, userID, agent, enc, expires)
		}
	}
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return &Credential{Token: token, AgentID: agent, UserID: userID, Email: p.User.Email, ExpiresAt: time.UnixMilli(expires).UTC().Format(time.RFC3339), Version: version + 1}, nil
}

// VerifyAgent enforces federation revocation and expiry at every MCP ingress.
// Ordinary non-federated agents retain their existing authentication path.
func (s *Service) VerifyAgent(ctx context.Context, token string) (*identity.Agent, error) {
	a, err := s.identity.VerifyAgentToken(ctx, token)
	if err != nil {
		return nil, err
	}
	var status, trust string
	var expiry int64
	err = s.db.QueryRowContext(ctx, `SELECT c.status,c.expires_at,i.status FROM federation_connections c JOIN federation_issuers i ON i.issuer=c.issuer WHERE c.agent_id=?`, a.ID).Scan(&status, &expiry, &trust)
	if errors.Is(err, sql.ErrNoRows) {
		return a, nil
	}
	if err != nil || status != "active" || trust != "active" || expiry <= s.now().UnixMilli() {
		return nil, identity.ErrAgentTokenInvalid
	}
	return a, nil
}
func (s *Service) PrivateReceiverAllowed(ctx context.Context, agentID, issuer, destination string) bool {
	u, err := url.Parse(destination)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Fragment != "" || u.RawQuery != "" || !regexp.MustCompile(`^/api/session-webhooks/[A-Za-z0-9_-]{1,120}$`).MatchString(u.Path) || u.RawPath != "" {
		return false
	}
	var origin string
	err = s.db.QueryRowContext(ctx, `SELECT i.origin FROM federation_connections c JOIN federation_issuers i ON i.issuer=c.issuer WHERE c.agent_id=? AND c.issuer=? AND c.status='active' AND i.status='active' AND c.expires_at>?`, agentID, issuer, s.now().UnixMilli()).Scan(&origin)
	return err == nil && origin == u.Scheme+"://"+u.Host
}

func (s *Service) Revoke(ctx context.Context, p *Principal, scope string) error {
	if scope != "user" && scope != "environment" {
		return ErrUnauthorized
	}
	if scope == "environment" && p.User.Role != identity.RoleAdmin {
		return ErrUnauthorized
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	predicate := `issuer=?`
	args := []any{p.Issuer}
	if scope == "user" {
		predicate += ` AND subject=?`
		args = append(args, p.Subject)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE federation_issuers SET status='removed' WHERE issuer=?`, p.Issuer)
		if err != nil {
			return err
		}
	}
	connectionStatus := "revoked"
	if scope == "environment" {
		connectionStatus = "environment_removed"
		predicate += ` AND status != 'revoked'`
	}
	_, err = tx.ExecContext(ctx, `UPDATE federation_connections SET status=?,credential_enc='' WHERE `+predicate, append([]any{connectionStatus}, args...)...)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE inbox_grants SET status='revoked',revoked_at=? WHERE status='active' AND agent_id IN (SELECT agent_id FROM federation_connections WHERE `+predicate+`)`, append([]any{s.now().UnixMilli()}, args...)...)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE callback_receivers SET status='removed' WHERE agent_id IN (SELECT agent_id FROM federation_connections WHERE `+predicate+`)`, args...)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE callback_outbox SET status='failed',terminal_reason='connection_revoked' WHERE status IN ('pending','delivering') AND receiver_id IN (SELECT id FROM callback_receivers WHERE status='removed')`)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Service) Handoff(ctx context.Context, p *Principal, returnURL string) (string, time.Time, error) {
	// Only the registered environment origin is a valid return destination.
	u, err := url.Parse(returnURL)
	if err != nil || u.User != nil || u.Fragment != "" || u.Scheme+"://"+u.Host != p.Origin {
		return "", time.Time{}, ErrUnauthorized
	}
	var status string
	err = s.db.QueryRowContext(ctx, `SELECT c.status FROM federation_connections c JOIN agents a ON a.id=c.agent_id JOIN users u ON u.id=c.user_id WHERE c.issuer=? AND c.subject=? AND c.expires_at>? AND a.disabled=0 AND u.status='active'`, p.Issuer, p.Subject, s.now().UnixMilli()).Scan(&status)
	if err != nil || status != "active" {
		return "", time.Time{}, ErrRevoked
	}
	codeRaw := make([]byte, 32)
	if _, err = rand.Read(codeRaw); err != nil {
		return "", time.Time{}, err
	}
	code := base64.RawURLEncoding.EncodeToString(codeRaw)
	sum := sha256.Sum256([]byte(code))
	expires := s.now().Add(time.Minute)
	_, err = s.db.ExecContext(ctx, `INSERT INTO federation_handoffs(code_hash,issuer,user_id,return_url,expires_at) VALUES(?,?,?,?,?)`, hex.EncodeToString(sum[:]), p.Issuer, p.User.ID, returnURL, expires.UnixMilli())
	return code, expires, err
}
func (s *Service) ConsumeHandoff(ctx context.Context, code string) (string, error) {
	if len(code) > 128 {
		return "", ErrUnauthorized
	}
	sum := sha256.Sum256([]byte(code))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var userID string
	err = tx.QueryRowContext(ctx, `UPDATE federation_handoffs SET consumed_at=? WHERE code_hash=? AND consumed_at IS NULL AND expires_at>? AND issuer IN (SELECT issuer FROM federation_issuers WHERE status='active') AND EXISTS (SELECT 1 FROM federation_connections c JOIN agents a ON a.id=c.agent_id WHERE c.issuer=federation_handoffs.issuer AND c.user_id=federation_handoffs.user_id AND c.status='active' AND c.expires_at>federation_handoffs.expires_at AND a.disabled=0) AND user_id IN (SELECT id FROM users WHERE status='active') RETURNING user_id`, s.now().UnixMilli(), hex.EncodeToString(sum[:]), s.now().UnixMilli()).Scan(&userID)
	if err != nil {
		return "", ErrUnauthorized
	}
	if err = tx.Commit(); err != nil {
		return "", err
	}
	return userID, nil
}
