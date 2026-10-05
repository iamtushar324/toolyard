package federation

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
)

type HostClaims struct {
	jwt.RegisteredClaims
	Action    string `json:"action"`
	RequestID string `json:"request_id"`
	PublicKey string `json:"public_key,omitempty"`
	HostName  string `json:"host_name,omitempty"`
	Platform  string `json:"platform,omitempty"`
}
type HostRequest struct {
	RequestID        string `json:"request_id"`
	AuthorizationRef string `json:"-"`
	EnvironmentID    string `json:"environment_id"`
	LocalUserID      string `json:"local_user_id"`
	PublicKey        string `json:"-"`
	HostName         string `json:"host_name"`
	Platform         string `json:"platform"`
	Fingerprint      string `json:"key_fingerprint"`
	Status           string `json:"status"`
	OwnerID          string `json:"owner_user_id,omitempty"`
	AgentID          string `json:"agent_id,omitempty"`
	ExpiresAt        string `json:"expires_at"`
	expires          int64
	Generation       int
}
type HostResult struct {
	Status           string `json:"status"`
	RequestID        string `json:"request_id"`
	AuthorizationURL string `json:"authorization_url,omitempty"`
	ExpiresAt        string `json:"expires_at"`
	*Credential
}
type HostConnection struct {
	EnvironmentID string `json:"environment_id"`
	LocalUserID   string `json:"local_user_id"`
	HostName      string `json:"host_name"`
	Platform      string `json:"platform"`
	Fingerprint   string `json:"key_fingerprint"`
	OwnerID       string `json:"owner_user_id"`
	OwnerName     string `json:"owner_name"`
	OwnerEmail    string `json:"owner_email"`
	AgentID       string `json:"agent_id"`
	Status        string `json:"status"`
	ExpiresAt     string `json:"expires_at"`
	CreatedAt     int64  `json:"created_at"`
	LastUsedAt    int64  `json:"last_used_at"`
}

func hostAAD(env, user, key string) []byte { return []byte("host|" + env + "|" + user + "|" + key) }
func fingerprint(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}
func hostText(v string, max int) bool {
	if strings.TrimSpace(v) != v || v == "" || len(v) > max {
		return false
	}
	for _, r := range v {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return false
		}
	}
	return true
}
func decodeHostKey(raw string) ([]byte, error) {
	b, e := base64.StdEncoding.DecodeString(raw)
	if e != nil {
		b, e = base64.RawStdEncoding.DecodeString(raw)
	}
	if e != nil || len(b) != ed25519.PublicKeySize {
		return nil, ErrUnauthorized
	}
	return b, nil
}
func (s *Service) hostRequest(ctx context.Context, ref string, byRef bool) (*HostRequest, error) {
	q := `SELECT request_id,authorization_ref,environment_id,local_user_id,public_key,host_name,platform,status,COALESCE(owner_user_id,''),COALESCE(agent_id,''),expires_at,COALESCE(connection_generation,0) FROM host_connection_requests WHERE request_id=?`
	if byRef {
		q = strings.Replace(q, "request_id=?", "authorization_ref=?", 1)
	}
	h := new(HostRequest)
	err := s.db.QueryRowContext(ctx, q, ref).Scan(&h.RequestID, &h.AuthorizationRef, &h.EnvironmentID, &h.LocalUserID, &h.PublicKey, &h.HostName, &h.Platform, &h.Status, &h.OwnerID, &h.AgentID, &h.expires, &h.Generation)
	if err != nil {
		return nil, ErrUnauthorized
	}
	if h.expires <= s.now().UnixMilli() && h.Status == "pending" {
		h.Status = "expired"
	}
	h.ExpiresAt = time.UnixMilli(h.expires).UTC().Format(time.RFC3339)
	h.Fingerprint = fingerprint(h.PublicKey)
	return h, nil
}
func (s *Service) InspectHostRequest(ctx context.Context, ref string) (*HostRequest, error) {
	if len(ref) != 43 {
		return nil, ErrUnauthorized
	}
	return s.hostRequest(ctx, ref, true)
}
func (s *Service) verifyHostProof(ctx context.Context, raw, action string) (*HostClaims, error) {
	if len(raw) > 8192 {
		return nil, ErrUnauthorized
	}
	c := new(HostClaims)
	if _, _, e := jwt.NewParser().ParseUnverified(raw, c); e != nil {
		return nil, ErrUnauthorized
	}
	key := c.PublicKey
	if action != "begin" {
		var env, subject, boundKey string
		err := s.db.QueryRowContext(ctx, `SELECT environment_id,local_user_id,public_key FROM host_connection_requests WHERE request_id=?`, c.RequestID).Scan(&env, &subject, &boundKey)
		if errors.Is(err, sql.ErrNoRows) && action == "cancel" {
			key = c.PublicKey
		} else {
			if err != nil || env != c.Issuer || subject != c.Subject {
				return nil, ErrUnauthorized
			}
			key = boundKey
		}
	}
	pub, e := decodeHostKey(key)
	if e != nil {
		return nil, e
	}
	_, e = jwt.ParseWithClaims(raw, c, func(t *jwt.Token) (any, error) { return ed25519.PublicKey(pub), nil }, jwt.WithValidMethods([]string{"EdDSA"}), jwt.WithAudience(s.InstanceID), jwt.WithExpirationRequired(), jwt.WithIssuedAt(), jwt.WithTimeFunc(s.now))
	if e != nil || len(c.Audience) != 1 || c.Action != action || !validLocalID(c.Issuer) || !validLocalID(c.Subject) || c.ID == "" || len(c.ID) > 128 || c.IssuedAt == nil || c.ExpiresAt == nil || c.ExpiresAt.Time.Sub(c.IssuedAt.Time) > 120*time.Second || c.IssuedAt.Time.Before(s.now().Add(-120*time.Second)) {
		return nil, ErrUnauthorized
	}
	if _, e = uuid.Parse(c.RequestID); e != nil {
		return nil, ErrUnauthorized
	}
	c.PublicKey = base64.StdEncoding.EncodeToString(pub)
	if action == "begin" && (!hostText(c.HostName, 120) || !hostText(c.Platform, 40)) {
		return nil, ErrUnauthorized
	}
	return c, nil
}
func (s *Service) hostReplayTx(ctx context.Context, tx *sql.Tx, c *HostClaims) error {
	if _, e := tx.ExecContext(ctx, `DELETE FROM host_proof_replays WHERE expires_at<?`, s.now().Add(-time.Minute).UnixMilli()); e != nil {
		return e
	}
	_, e := tx.ExecContext(ctx, `INSERT INTO host_proof_replays(environment_id,public_key,jti,expires_at) VALUES(?,?,?,?)`, c.Issuer, c.PublicKey, c.ID, c.ExpiresAt.UnixMilli())
	if e != nil {
		return ErrUnauthorized
	}
	return nil
}
func (s *Service) hostAuditTx(ctx context.Context, tx *sql.Tx, event string, h *HostRequest, u *identity.User, agent string) error {
	body, _ := json.Marshal(map[string]string{"request_id": h.RequestID, "environment_id": h.EnvironmentID, "local_user_id": h.LocalUserID, "key_fingerprint": h.Fingerprint, "host_name": h.HostName, "platform": h.Platform})
	owner, email, name := "", "", ""
	if u != nil {
		owner, email, name = u.ID, u.Email, u.Label()
	}
	deciderID, deciderEmail, deciderName, via := "", "", "", "host-service"
	if event == "host.authorization_approved" || event == "host.authorization_rejected" || event == "host.connection_revoked_by_owner" {
		deciderID, deciderEmail, deciderName, via = owner, email, name, "dashboard"
	}
	_, e := tx.ExecContext(ctx, `INSERT INTO audit_events(id,ts,event_type,agent_id,agent_name,agent_kind,owner_user_id,owner_email,owner_name,arguments,decided_by_user_id,decided_by_email,decided_by_name,decided_via,decider_ref) VALUES(?,?,?,?,?,'agent',?,?,?,?,?,?,?,?,?)`, uuid.NewString(), s.now().UnixMilli(), event, agent, h.HostName+" · "+name, owner, email, name, string(body), deciderID, deciderEmail, deciderName, via, h.RequestID)
	return e
}
func (s *Service) BeginHost(ctx context.Context, proof string) (*HostRequest, error) {
	c, e := s.verifyHostProof(ctx, proof, "begin")
	if e != nil {
		return nil, e
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	if e = s.hostReplayTx(ctx, tx, c); e != nil {
		return nil, e
	}
	content, _ := json.Marshal([]string{c.Issuer, c.Subject, c.PublicKey, c.HostName, c.Platform})
	digest := tokenDigest(string(content))
	var old string
	e = tx.QueryRowContext(ctx, `SELECT content_hash FROM host_connection_requests WHERE request_id=?`, c.RequestID).Scan(&old)
	if e == nil {
		if old != digest {
			return nil, ErrConflict
		}
		if e = tx.Commit(); e != nil {
			return nil, e
		}
		return s.hostRequest(ctx, c.RequestID, false)
	}
	if !errors.Is(e, sql.ErrNoRows) {
		return nil, e
	}
	var key string
	e = tx.QueryRowContext(ctx, `SELECT public_key FROM host_connections WHERE environment_id=? AND local_user_id=?`, c.Issuer, c.Subject).Scan(&key)
	if e == nil && key != c.PublicKey {
		return nil, ErrConflict
	}
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return nil, e
	}
	var count int
	if e = tx.QueryRowContext(ctx, `SELECT count(*) FROM host_connection_requests WHERE environment_id=? AND local_user_id=? AND status='pending' AND expires_at>?`, c.Issuer, c.Subject, s.now().UnixMilli()).Scan(&count); e != nil {
		return nil, e
	}
	if count >= 3 {
		return nil, ErrConflict
	}
	raw := make([]byte, 32)
	if _, e = rand.Read(raw); e != nil {
		return nil, e
	}
	h := &HostRequest{RequestID: c.RequestID, AuthorizationRef: base64.RawURLEncoding.EncodeToString(raw), EnvironmentID: c.Issuer, LocalUserID: c.Subject, PublicKey: c.PublicKey, HostName: c.HostName, Platform: c.Platform, Fingerprint: fingerprint(c.PublicKey), Status: "pending", expires: s.now().Add(10 * time.Minute).UnixMilli()}
	_, e = tx.ExecContext(ctx, `INSERT INTO host_connection_requests(request_id,authorization_ref,environment_id,local_user_id,public_key,host_name,platform,content_hash,created_at,expires_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, h.RequestID, h.AuthorizationRef, h.EnvironmentID, h.LocalUserID, h.PublicKey, h.HostName, h.Platform, digest, s.now().UnixMilli(), h.expires)
	if e != nil {
		return nil, e
	}
	if e = s.hostAuditTx(ctx, tx, "host.authorization_started", h, nil, ""); e != nil {
		return nil, e
	}
	if e = tx.Commit(); e != nil {
		return nil, e
	}
	h.ExpiresAt = time.UnixMilli(h.expires).UTC().Format(time.RFC3339)
	return h, nil
}
func (s *Service) DecideHost(ctx context.Context, ref, owner string, accept bool) (*HostRequest, error) {
	u, e := s.identity.GetUserByID(ctx, owner)
	if e != nil || u.Status != identity.StatusActive {
		return nil, ErrUnauthorized
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	h, e := s.InspectHostRequest(ctx, ref)
	if e != nil {
		return nil, e
	}
	// Local identity has exactly one authenticated owner profile. Hosted subjects
	// must match the browser account's independently verified Clerk subject.
	if h.LocalUserID != "local-user" && (u.ClerkUserID == "" || u.ClerkUserID != h.LocalUserID) {
		return nil, ErrUnauthorized
	}
	verdict := "rejected"
	if accept {
		verdict = "approved"
	}
	if h.Status != "pending" {
		if h.Status == verdict && h.OwnerID == owner {
			return h, nil
		}
		return nil, ErrConflict
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	var active int
	if e = tx.QueryRowContext(ctx, `SELECT count(*) FROM users WHERE id=? AND status='active'`, owner).Scan(&active); e != nil {
		return nil, e
	}
	if active != 1 {
		return nil, ErrRevoked
	}
	var agent string
	if accept {
		var key, status, existingOwner string
		var expires int64
		e = tx.QueryRowContext(ctx, `SELECT c.agent_id,c.public_key,CASE WHEN a.disabled=0 AND a.owner_user=c.user_id AND a.token_hash=c.credential_hash THEN c.status ELSE 'invalid' END,c.user_id,c.expires_at FROM host_connections c LEFT JOIN agents a ON a.id=c.agent_id WHERE c.environment_id=? AND c.local_user_id=?`, h.EnvironmentID, h.LocalUserID).Scan(&agent, &key, &status, &existingOwner, &expires)
		exists := e == nil
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return nil, e
		}
		if exists && (key != h.PublicKey || existingOwner != owner) {
			return nil, ErrConflict
		}
		if !exists || status != "active" || expires <= s.now().UnixMilli() {
			if exists {
				if e = s.revokeLocalResourcesTx(ctx, tx, agent); e != nil {
					return nil, e
				}
			} else {
				agent = "ag_" + uuid.NewString()
			}
			token, hash, e := identity.NewAgentToken(agent)
			if e != nil {
				return nil, e
			}
			enc, e := s.cipher.Seal([]byte(token), hostAAD(h.EnvironmentID, h.LocalUserID, h.PublicKey))
			if e != nil {
				return nil, e
			}
			now := s.now().UnixMilli()
			lease := s.now().Add(30 * 24 * time.Hour).UnixMilli()
			if exists {
				res, err := tx.ExecContext(ctx, `UPDATE agents SET name=?,token_hash=?,prev_token_hash=NULL,prev_token_expires=NULL,disabled=0 WHERE id=? AND owner_user=?`, h.HostName+" · "+u.Label(), hash, agent, owner)
				if err != nil {
					return nil, err
				}
				n, _ := res.RowsAffected()
				if n == 0 {
					if _, err = tx.ExecContext(ctx, `INSERT INTO agents(id,name,owner_user,token_hash,last_seen,created_at) VALUES(?,?,?,?,?,?)`, agent, h.HostName+" · "+u.Label(), owner, hash, now, now); err != nil {
						return nil, err
					}
				}
				_, e = tx.ExecContext(ctx, `UPDATE host_connections SET credential_enc=?,credential_hash=?,version=version+1,connection_generation=connection_generation+1,status='active',expires_at=?,updated_at=?,host_name=?,platform=? WHERE agent_id=?`, enc, hash, lease, now, h.HostName, h.Platform, agent)
			} else {
				_, e = tx.ExecContext(ctx, `INSERT INTO agents(id,name,owner_user,token_hash,last_seen,created_at) VALUES(?,?,?,?,?,?)`, agent, h.HostName+" · "+u.Label(), owner, hash, now, now)
				if e != nil {
					return nil, e
				}
				_, e = tx.ExecContext(ctx, `INSERT INTO host_connections(environment_id,local_user_id,public_key,host_name,platform,user_id,agent_id,credential_enc,credential_hash,expires_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, h.EnvironmentID, h.LocalUserID, h.PublicKey, h.HostName, h.Platform, owner, agent, enc, hash, lease, now, now)
			}
			if e != nil {
				return nil, e
			}
		}
	}
	var generation int
	if accept {
		if e = tx.QueryRowContext(ctx, `SELECT connection_generation FROM host_connections WHERE agent_id=?`, agent).Scan(&generation); e != nil {
			return nil, e
		}
	}
	res, e := tx.ExecContext(ctx, `UPDATE host_connection_requests SET status=?,owner_user_id=?,agent_id=?,decided_at=?,connection_generation=? WHERE request_id=? AND status='pending' AND expires_at>?`, verdict, owner, agent, s.now().UnixMilli(), generation, h.RequestID, s.now().UnixMilli())
	if e != nil {
		return nil, e
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return nil, ErrConflict
	}
	if e = s.hostAuditTx(ctx, tx, "host.authorization_"+verdict, h, u, agent); e != nil {
		return nil, e
	}
	if e = tx.Commit(); e != nil {
		return nil, e
	}
	return s.hostRequest(ctx, h.RequestID, false)
}
func (s *Service) PollHost(ctx context.Context, proof string, cancel bool) (*HostResult, error) {
	action := "poll"
	if cancel {
		action = "cancel"
	}
	c, e := s.verifyHostProof(ctx, proof, action)
	if e != nil {
		return nil, e
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	h, e := s.hostRequest(ctx, c.RequestID, false)
	if e != nil && cancel {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return nil, err
		}
		defer tx.Rollback()
		if err = s.hostReplayTx(ctx, tx, c); err != nil {
			return nil, err
		}
		raw := make([]byte, 32)
		if _, err = rand.Read(raw); err != nil {
			return nil, err
		}
		content, _ := json.Marshal([]string{c.Issuer, c.Subject, c.PublicKey, c.HostName, c.Platform})
		h = &HostRequest{RequestID: c.RequestID, AuthorizationRef: base64.RawURLEncoding.EncodeToString(raw), EnvironmentID: c.Issuer, LocalUserID: c.Subject, PublicKey: c.PublicKey, HostName: c.HostName, Platform: c.Platform, Fingerprint: fingerprint(c.PublicKey), Status: "cancelled", expires: s.now().Add(10 * time.Minute).UnixMilli()}
		_, err = tx.ExecContext(ctx, `INSERT INTO host_connection_requests(request_id,authorization_ref,environment_id,local_user_id,public_key,host_name,platform,content_hash,status,created_at,expires_at) VALUES(?,?,?,?,?,?,?,?,'cancelled',?,?)`, h.RequestID, h.AuthorizationRef, h.EnvironmentID, h.LocalUserID, h.PublicKey, h.HostName, h.Platform, tokenDigest(string(content)), s.now().UnixMilli(), h.expires)
		if err != nil {
			return nil, err
		}
		if err = s.hostAuditTx(ctx, tx, "host.authorization_cancelled", h, nil, ""); err != nil {
			return nil, err
		}
		if err = tx.Commit(); err != nil {
			return nil, err
		}
		return &HostResult{Status: "cancelled", RequestID: h.RequestID, ExpiresAt: time.UnixMilli(h.expires).UTC().Format(time.RFC3339)}, nil
	}
	if e != nil {
		return nil, e
	}
	if h.PublicKey != c.PublicKey || h.EnvironmentID != c.Issuer || h.LocalUserID != c.Subject {
		return nil, ErrUnauthorized
	}
	var u *identity.User
	if h.Status == "approved" {
		u, e = s.identity.GetUserByID(ctx, h.OwnerID)
		if e != nil || u.Status != identity.StatusActive {
			return nil, ErrRevoked
		}
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	if e = s.hostReplayTx(ctx, tx, c); e != nil {
		return nil, e
	}
	if cancel {
		if h.Status == "pending" {
			if e = s.hostAuditTx(ctx, tx, "host.authorization_cancelled", h, nil, ""); e != nil {
				return nil, e
			}
		}
		if h.Status == "approved" {
			var generation int
			if e = tx.QueryRowContext(ctx, `SELECT connection_generation FROM host_connections WHERE agent_id=?`, h.AgentID).Scan(&generation); e != nil {
				return nil, e
			}
			if generation == h.Generation {
				if _, e = tx.ExecContext(ctx, `UPDATE host_connections SET status='revoked',credential_enc='',updated_at=? WHERE agent_id=?`, s.now().UnixMilli(), h.AgentID); e != nil {
					return nil, e
				}
				if e = s.revokeLocalResourcesTx(ctx, tx, h.AgentID); e != nil {
					return nil, e
				}
			}
			if e = s.hostAuditTx(ctx, tx, "host.authorization_cancelled", h, u, h.AgentID); e != nil {
				return nil, e
			}
		}
		_, e = tx.ExecContext(ctx, `UPDATE host_connection_requests SET status='cancelled' WHERE request_id=? AND status IN ('pending','approved')`, c.RequestID)
	} else {
		_, e = tx.ExecContext(ctx, `UPDATE host_connection_requests SET status='expired' WHERE request_id=? AND status='pending' AND expires_at<=?`, c.RequestID, s.now().UnixMilli())
	}
	if e != nil {
		return nil, e
	}
	// Read the transaction's state rather than an earlier connection snapshot.
	if e = tx.QueryRowContext(ctx, `SELECT status FROM host_connection_requests WHERE request_id=?`, c.RequestID).Scan(&h.Status); e != nil {
		return nil, e
	}
	out := &HostResult{Status: h.Status, RequestID: h.RequestID, ExpiresAt: h.ExpiresAt}
	if h.Status == "approved" {
		credential, e := s.hostCredentialTx(ctx, tx, h.AgentID, false)
		if e != nil {
			return nil, e
		}
		if credential.Generation != h.Generation {
			return nil, ErrRevoked
		}
		out.Credential = credential
		out.ExpiresAt = credential.ExpiresAt
		res, e := tx.ExecContext(ctx, `UPDATE host_connection_requests SET claimed_at=? WHERE request_id=? AND claimed_at IS NULL`, s.now().UnixMilli(), h.RequestID)
		if e != nil {
			return nil, e
		}
		n, _ := res.RowsAffected()
		if n == 1 {
			if e = s.hostAuditTx(ctx, tx, "host.credential_claimed", h, u, h.AgentID); e != nil {
				return nil, e
			}
		}
	}
	if e = tx.Commit(); e != nil {
		return nil, e
	}
	return out, nil
}
func (s *Service) hostCredentialTx(ctx context.Context, tx *sql.Tx, agent string, allowExpired bool) (*Credential, error) {
	var env, user, key, enc, hash, owner, email, status string
	var expires int64
	var version, generation int
	e := tx.QueryRowContext(ctx, `SELECT c.environment_id,c.local_user_id,c.public_key,c.credential_enc,c.credential_hash,c.user_id,COALESCE(u.email,''),c.status,c.expires_at,c.version,c.connection_generation FROM host_connections c JOIN agents a ON a.id=c.agent_id JOIN users u ON u.id=c.user_id WHERE c.agent_id=? AND a.disabled=0 AND a.owner_user=c.user_id AND a.token_hash=c.credential_hash AND u.status='active'`, agent).Scan(&env, &user, &key, &enc, &hash, &owner, &email, &status, &expires, &version, &generation)
	if e != nil || status != "active" || (!allowExpired && expires <= s.now().UnixMilli()) {
		return nil, ErrRevoked
	}
	raw, e := s.cipher.Open(enc, hostAAD(env, user, key))
	if e != nil {
		return nil, e
	}
	if tokenDigest(string(raw)) != hash {
		return nil, ErrRevoked
	}
	return &Credential{Token: string(raw), AgentID: agent, UserID: owner, Email: email, Version: version, Generation: generation, InstanceID: s.InstanceID, ExpiresAt: time.UnixMilli(expires).UTC().Format(time.RFC3339)}, nil
}
func (s *Service) hostValid(ctx context.Context, agent string, allowExpired bool) error {
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	_, e = s.hostCredentialTx(ctx, tx, agent, allowExpired)
	return e
}
func (s *Service) RenewHost(ctx context.Context, token string, expected int) (*Credential, error) {
	a, e := s.identity.VerifyAgentToken(ctx, token)
	if e != nil {
		return nil, ErrUnauthorized
	}
	u, e := s.identity.GetUserByID(ctx, a.Owner)
	if e != nil {
		return nil, e
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	c, e := s.hostCredentialTx(ctx, tx, a.ID, true)
	if e != nil {
		return nil, e
	}
	if expected < 1 || expected > c.Version {
		return nil, ErrConflict
	}
	exp, _ := time.Parse(time.RFC3339, c.ExpiresAt)
	if exp.Before(s.now().Add(24*time.Hour)) && (expected == c.Version || exp.Before(s.now())) {
		expiry := s.now().Add(30 * 24 * time.Hour)
		res, e := tx.ExecContext(ctx, `UPDATE host_connections SET version=version+1,expires_at=?,updated_at=? WHERE agent_id=? AND version=? AND status='active'`, expiry.UnixMilli(), s.now().UnixMilli(), a.ID, c.Version)
		if e != nil {
			return nil, e
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			return nil, ErrConflict
		}
		c.Version++
		c.ExpiresAt = expiry.UTC().Format(time.RFC3339)
		h, e := s.hostByAgentTx(ctx, tx, a.ID)
		if e != nil {
			return nil, e
		}
		if e = s.hostAuditTx(ctx, tx, "host.credential_renewed", h, u, a.ID); e != nil {
			return nil, e
		}
	}
	if e = tx.Commit(); e != nil {
		return nil, e
	}
	return c, nil
}
func (s *Service) hostByAgentTx(ctx context.Context, tx *sql.Tx, agent string) (*HostRequest, error) {
	h := new(HostRequest)
	e := tx.QueryRowContext(ctx, `SELECT environment_id,local_user_id,public_key,host_name,platform,user_id FROM host_connections WHERE agent_id=?`, agent).Scan(&h.EnvironmentID, &h.LocalUserID, &h.PublicKey, &h.HostName, &h.Platform, &h.OwnerID)
	h.Fingerprint = fingerprint(h.PublicKey)
	return h, e
}
func (s *Service) RevokeHost(ctx context.Context, token string) error {
	agent, _, ok := strings.Cut(token, ".")
	if !ok || len(agent) > 128 {
		return ErrUnauthorized
	}
	var owner string
	if e := s.db.QueryRowContext(ctx, `SELECT user_id FROM host_connections WHERE agent_id=? AND credential_hash=?`, agent, tokenDigest(token)).Scan(&owner); e != nil {
		return ErrUnauthorized
	}
	return s.revokeHost(ctx, owner, agent, false, tokenDigest(token))
}
func (s *Service) RevokeOwnedHost(ctx context.Context, owner, agent string) error {
	return s.revokeHost(ctx, owner, agent, true, "")
}
func (s *Service) revokeHost(ctx context.Context, owner, agent string, byOwner bool, expectedHash string) error {
	u, e := s.identity.GetUserByID(ctx, owner)
	if (e != nil || u.Status != identity.StatusActive) && byOwner {
		return ErrUnauthorized
	}
	if u == nil {
		u = &identity.User{ID: owner}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	h, e := s.hostByAgentTx(ctx, tx, agent)
	if e != nil || h.OwnerID != owner {
		return ErrUnauthorized
	}
	var status, hash string
	if e = tx.QueryRowContext(ctx, `SELECT status,credential_hash FROM host_connections WHERE agent_id=?`, agent).Scan(&status, &hash); e != nil {
		return e
	}
	if !byOwner && hash != expectedHash {
		return ErrUnauthorized
	}
	_, e = tx.ExecContext(ctx, `UPDATE host_connections SET status='revoked',credential_enc='',updated_at=? WHERE agent_id=?`, s.now().UnixMilli(), agent)
	if e != nil {
		return e
	}
	if e = s.revokeLocalResourcesTx(ctx, tx, agent); e != nil {
		return e
	}
	if status != "revoked" {
		event := "host.connection_revoked"
		if byOwner {
			event = "host.connection_revoked_by_owner"
		}
		if e = s.hostAuditTx(ctx, tx, event, h, u, agent); e != nil {
			return e
		}
	}
	return tx.Commit()
}
func (s *Service) ListHosts(ctx context.Context, owner string, all bool) ([]HostConnection, error) {
	query := `SELECT c.environment_id,c.local_user_id,c.host_name,c.platform,c.public_key,c.user_id,COALESCE(u.display_name,u.username),COALESCE(u.email,''),c.agent_id,c.status,c.expires_at,c.created_at,COALESCE(a.last_seen,0) FROM host_connections c JOIN users u ON u.id=c.user_id LEFT JOIN agents a ON a.id=c.agent_id`
	args := []any{}
	if !all {
		query += ` WHERE c.user_id=?`
		args = append(args, owner)
	}
	query += ` ORDER BY c.created_at DESC LIMIT 500`
	rows, e := s.db.QueryContext(ctx, query, args...)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []HostConnection{}
	for rows.Next() {
		var h HostConnection
		var key string
		var expires int64
		if e = rows.Scan(&h.EnvironmentID, &h.LocalUserID, &h.HostName, &h.Platform, &key, &h.OwnerID, &h.OwnerName, &h.OwnerEmail, &h.AgentID, &h.Status, &expires, &h.CreatedAt, &h.LastUsedAt); e != nil {
			return nil, e
		}
		h.Fingerprint = fingerprint(key)
		h.ExpiresAt = time.UnixMilli(expires).UTC().Format(time.RFC3339)
		if h.Status == "active" && expires <= s.now().UnixMilli() {
			h.Status = "expired"
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
func (s *Service) HostHandoff(ctx context.Context, token string) (string, time.Time, error) {
	a, e := s.VerifyAgent(ctx, token)
	if e != nil {
		return "", time.Time{}, ErrUnauthorized
	}
	if e = s.hostValid(ctx, a.ID, false); e != nil {
		return "", time.Time{}, e
	}
	raw := make([]byte, 32)
	if _, e = rand.Read(raw); e != nil {
		return "", time.Time{}, e
	}
	code := base64.RawURLEncoding.EncodeToString(raw)
	expiry := s.now().Add(time.Minute)
	_, e = s.db.ExecContext(ctx, `INSERT INTO host_handoffs(code_hash,agent_id,user_id,expires_at) VALUES(?,?,?,?)`, tokenDigest(code), a.ID, a.Owner, expiry.UnixMilli())
	return code, expiry, e
}
func (s *Service) ConsumeHostHandoff(ctx context.Context, code string) (string, error) {
	if len(code) != 43 {
		return "", ErrUnauthorized
	}
	var user string
	e := s.db.QueryRowContext(ctx, `UPDATE host_handoffs SET consumed_at=? WHERE code_hash=? AND consumed_at IS NULL AND expires_at>? AND EXISTS(SELECT 1 FROM host_connections c JOIN agents a ON a.id=c.agent_id JOIN users u ON u.id=c.user_id WHERE c.agent_id=host_handoffs.agent_id AND c.status='active' AND c.expires_at>? AND c.credential_hash=a.token_hash AND a.disabled=0 AND a.owner_user=c.user_id AND u.status='active') RETURNING user_id`, s.now().UnixMilli(), tokenDigest(code), s.now().UnixMilli(), s.now().UnixMilli()).Scan(&user)
	if e != nil {
		return "", ErrUnauthorized
	}
	return user, nil
}
