package federation

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tusharbhardwaj/toolyard/internal/identity"
)

type LocalBinding struct {
	EnvironmentID   string `json:"environment_id"`
	LocalUserID     string `json:"local_user_id"`
	InstanceID      string `json:"instance_id"`
	ExpectedVersion int    `json:"expected_version"`
	Reconnect       bool   `json:"reconnect,omitempty"`
}

func localAAD(env, user string) []byte { return []byte("local|" + env + "|" + user) }
func tokenDigest(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
func validLocalID(id string) bool {
	return id != "" && len(id) <= 128 && !strings.ContainsAny(id, "|\x00\r\n")
}

// ConnectLocal exchanges an existing account key; it never provisions a user or
// treats the local user id as an independently verified hosted identity.
func (s *Service) ConnectLocal(ctx context.Context, bootstrap string, b LocalBinding) (*Credential, error) {
	if !validLocalID(b.EnvironmentID) || !validLocalID(b.LocalUserID) || b.InstanceID != s.InstanceID || b.ExpectedVersion != 0 {
		return nil, ErrUnauthorized
	}
	parent, err := s.VerifyAgent(ctx, bootstrap)
	if err != nil {
		return nil, ErrUnauthorized
	}
	u, err := s.identity.GetUserByID(ctx, parent.Owner)
	if err != nil || u.Status != identity.StatusActive {
		return nil, ErrRevoked
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var derived int
	if err = tx.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM local_connections WHERE agent_id=?)+(SELECT count(*) FROM federation_connections WHERE agent_id=?)`, parent.ID, parent.ID).Scan(&derived); err != nil {
		return nil, err
	}
	if derived != 0 {
		return nil, ErrUnauthorized
	}
	var parentHash string
	if err = tx.QueryRowContext(ctx, `SELECT a.token_hash FROM agents a JOIN users u ON u.id=a.owner_user WHERE a.id=? AND a.disabled=0 AND u.status='active'`, parent.ID).Scan(&parentHash); err != nil || parentHash != tokenDigest(bootstrap) {
		return nil, ErrRevoked
	}
	var enc, agent, status, owner, parentID, boundHash string
	var version, generation int
	var expires int64
	err = tx.QueryRowContext(ctx, `SELECT credential_enc,agent_id,status,user_id,parent_agent_id,parent_token_hash,version,expires_at,connection_generation FROM local_connections WHERE environment_id=? AND local_user_id=?`, b.EnvironmentID, b.LocalUserID).Scan(&enc, &agent, &status, &owner, &parentID, &boundHash, &version, &expires, &generation)
	exists := err == nil
	if exists {
		if owner != u.ID {
			return nil, ErrConflict
		}
		var currentHash string
		if err = tx.QueryRowContext(ctx, `SELECT token_hash FROM agents WHERE id=? AND disabled=0 AND owner_user=?`, agent, owner).Scan(&currentHash); err != nil {
			return nil, ErrRevoked
		}
		if status == "active" && parentID == parent.ID && boundHash == parentHash && expires > s.now().UnixMilli() {
			raw, e := s.cipher.Open(enc, localAAD(b.EnvironmentID, b.LocalUserID))
			if e != nil {
				return nil, e
			}
			if currentHash == tokenDigest(string(raw)) {
				return &Credential{Token: string(raw), AgentID: agent, UserID: owner, Email: u.Email, Version: version, ExpiresAt: time.UnixMilli(expires).UTC().Format(time.RFC3339), InstanceID: s.InstanceID, Generation: generation}, nil
			}
		}
		if !b.Reconnect {
			if status != "active" || expires <= s.now().UnixMilli() {
				return nil, ErrRevoked
			}
			return nil, ErrConflict
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	var bound int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM local_connections WHERE environment_id=? AND parent_agent_id=? AND local_user_id<>?`, b.EnvironmentID, parent.ID, b.LocalUserID).Scan(&bound); err != nil {
		return nil, err
	}
	if bound != 0 {
		return nil, ErrConflict
	}
	if !exists {
		agent = "ag_" + uuid.NewString()
	}
	token, hash, err := identity.NewAgentToken(agent)
	if err != nil {
		return nil, err
	}
	enc, err = s.cipher.Seal([]byte(token), localAAD(b.EnvironmentID, b.LocalUserID))
	if err != nil {
		return nil, err
	}
	now := s.now().UnixMilli()
	expires = s.now().Add(30 * 24 * time.Hour).UnixMilli()
	if exists {
		// Only the explicit human bootstrap path can create a new lifecycle. Old
		// permission and callback registrations stay terminal after reconnection.
		if err = s.revokeLocalResourcesTx(ctx, tx, agent); err != nil {
			return nil, err
		}
		_, err = tx.ExecContext(ctx, `UPDATE agents SET token_hash=?,prev_token_hash=NULL,prev_token_expires=NULL,last_seen=?,enroll_code=NULL,enroll_expires=NULL WHERE id=? AND disabled=0 AND owner_user=?`, hash, now, agent, u.ID)
		if err != nil {
			return nil, err
		}
		_, err = tx.ExecContext(ctx, `UPDATE local_connections SET parent_agent_id=?,parent_token_hash=?,credential_enc=?,credential_hash=?,version=version+1,connection_generation=connection_generation+1,status='active',expires_at=? WHERE environment_id=? AND local_user_id=?`, parent.ID, parentHash, enc, hash, expires, b.EnvironmentID, b.LocalUserID)
	} else {
		_, err = tx.ExecContext(ctx, `INSERT INTO agents(id,name,owner_user,token_hash,last_seen,created_at) VALUES(?,?,?,?,?,?)`, agent, "T3 local connection", u.ID, hash, now, now)
		if err != nil {
			return nil, err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO local_connections(environment_id,local_user_id,user_id,parent_agent_id,parent_token_hash,agent_id,credential_enc,credential_hash,version,expires_at,connection_generation) VALUES(?,?,?,?,?,?,?,?,1,?,1)`, b.EnvironmentID, b.LocalUserID, u.ID, parent.ID, parentHash, agent, enc, hash, expires)
	}
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return &Credential{Token: token, AgentID: agent, UserID: u.ID, Email: u.Email, Version: version + 1, ExpiresAt: time.UnixMilli(expires).UTC().Format(time.RFC3339), InstanceID: s.InstanceID, Generation: generation + 1}, nil
}

func (s *Service) localValid(ctx context.Context, agent string, allowExpired bool) error {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM local_connections c JOIN agents p ON p.id=c.parent_agent_id JOIN agents a ON a.id=c.agent_id JOIN users u ON u.id=c.user_id WHERE c.agent_id=? AND c.status='active' AND (c.expires_at>? OR ?=1) AND p.disabled=0 AND p.owner_user=c.user_id AND p.token_hash=c.parent_token_hash AND a.disabled=0 AND a.owner_user=c.user_id AND a.token_hash=c.credential_hash AND u.status='active'`, agent, s.now().UnixMilli(), allowExpired).Scan(&n)
	if err != nil {
		return err
	}
	if n != 1 {
		// An expired credential can renew. Parent/account/key revocation is
		// terminal once observed, including after account reactivation.
		if e := s.invalidateLocal(ctx, agent); e != nil {
			return e
		}
		return ErrRevoked
	}
	return nil
}
func (s *Service) LocalEnvironment(ctx context.Context, agent, env string) bool {
	if s.localValid(ctx, agent, false) != nil {
		return false
	}
	var got string
	err := s.db.QueryRowContext(ctx, `SELECT environment_id FROM local_connections WHERE agent_id=?`, agent).Scan(&got)
	return err == nil && got == env
}
func (s *Service) RenewLocal(ctx context.Context, token string, expected int) (*Credential, error) {
	a, err := s.identity.VerifyAgentToken(ctx, token)
	if err != nil {
		return nil, ErrUnauthorized
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err = s.localValid(ctx, a.ID, true); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var active int
	err = tx.QueryRowContext(ctx, `SELECT count(*) FROM local_connections c JOIN agents p ON p.id=c.parent_agent_id JOIN agents a ON a.id=c.agent_id JOIN users u ON u.id=c.user_id WHERE c.agent_id=? AND c.status='active' AND p.disabled=0 AND p.owner_user=c.user_id AND p.token_hash=c.parent_token_hash AND a.disabled=0 AND a.owner_user=c.user_id AND a.token_hash=c.credential_hash AND u.status='active'`, a.ID).Scan(&active)
	if err != nil {
		return nil, err
	}
	if active != 1 {
		return nil, ErrRevoked
	}
	var env, user, enc string
	var version, generation int
	var expires int64
	if err = tx.QueryRowContext(ctx, `SELECT environment_id,local_user_id,credential_enc,version,expires_at,connection_generation FROM local_connections WHERE agent_id=? AND status='active'`, a.ID).Scan(&env, &user, &enc, &version, &expires, &generation); err != nil {
		return nil, ErrRevoked
	}
	if expected < 1 || expected > version {
		return nil, ErrConflict
	}
	raw, err := s.cipher.Open(enc, localAAD(env, user))
	if err != nil {
		return nil, err
	}
	current := string(raw)
	if expires > s.now().Add(24*time.Hour).UnixMilli() || expected < version {
		if expires <= s.now().UnixMilli() {
			return nil, ErrConflict
		}
		return &Credential{Token: current, AgentID: a.ID, UserID: a.Owner, Email: a.OwnerEmail, Version: version, ExpiresAt: time.UnixMilli(expires).UTC().Format(time.RFC3339), InstanceID: s.InstanceID, Generation: generation}, nil
	}
	// Renewal advances expiry/version without changing the bearer. A lost
	// response or server restart can recover with the persisted credential at
	// any later time; only an explicit new lifecycle rotates the secret.
	expires = s.now().Add(30 * 24 * time.Hour).UnixMilli()
	res, err := tx.ExecContext(ctx, `UPDATE local_connections SET version=version+1,expires_at=? WHERE agent_id=? AND version=? AND status='active'`, expires, a.ID, version)
	if err != nil {
		return nil, err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return nil, ErrConflict
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return &Credential{Token: current, AgentID: a.ID, UserID: a.Owner, Email: a.OwnerEmail, Version: version + 1, ExpiresAt: time.UnixMilli(expires).UTC().Format(time.RFC3339), InstanceID: s.InstanceID, Generation: generation}, nil
}
func (s *Service) RevokeLocal(ctx context.Context, token string) error {
	a, err := s.identity.VerifyAgentToken(ctx, token)
	if err != nil {
		return ErrUnauthorized
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE local_connections SET status='revoked',credential_enc='' WHERE agent_id=? AND user_id=?`, a.ID, a.Owner)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return ErrUnauthorized
	}
	_, err = tx.ExecContext(ctx, `UPDATE inbox_grants SET status='revoked',revoked_at=? WHERE agent_id=? AND status='active'`, s.now().UnixMilli(), a.ID)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE callback_receivers SET status='removed' WHERE agent_id=?`, a.ID)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE callback_outbox SET status='failed',terminal_reason='connection_revoked' WHERE status IN ('pending','delivering') AND receiver_id IN (SELECT id FROM callback_receivers WHERE agent_id=?)`, a.ID)
	if err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Service) LocalHandoff(ctx context.Context, token string) (string, time.Time, error) {
	a, err := s.VerifyAgent(ctx, token)
	if err != nil {
		return "", time.Time{}, ErrUnauthorized
	}
	if err = s.localValid(ctx, a.ID, false); err != nil {
		return "", time.Time{}, err
	}
	raw := make([]byte, 32)
	if _, err = rand.Read(raw); err != nil {
		return "", time.Time{}, err
	}
	code := base64.RawURLEncoding.EncodeToString(raw)
	expiry := s.now().Add(time.Minute)
	_, err = s.db.ExecContext(ctx, `INSERT INTO local_handoffs(code_hash,agent_id,user_id,expires_at) VALUES(?,?,?,?)`, tokenDigest(code), a.ID, a.Owner, expiry.UnixMilli())
	return code, expiry, err
}
func (s *Service) ConsumeLocalHandoff(ctx context.Context, code string) (string, error) {
	if len(code) > 128 {
		return "", ErrUnauthorized
	}
	var agent string
	if err := s.db.QueryRowContext(ctx, `SELECT agent_id FROM local_handoffs WHERE code_hash=?`, tokenDigest(code)).Scan(&agent); err != nil {
		return "", ErrUnauthorized
	}
	if err := s.localValid(ctx, agent, false); err != nil {
		return "", err
	}
	var user string
	err := s.db.QueryRowContext(ctx, `UPDATE local_handoffs SET consumed_at=? WHERE code_hash=? AND consumed_at IS NULL AND expires_at>? AND EXISTS (SELECT 1 FROM local_connections c JOIN agents p ON p.id=c.parent_agent_id JOIN agents a ON a.id=c.agent_id JOIN users u ON u.id=c.user_id WHERE c.agent_id=local_handoffs.agent_id AND c.status='active' AND c.expires_at>? AND p.disabled=0 AND p.token_hash=c.parent_token_hash AND a.disabled=0 AND a.owner_user=c.user_id AND a.token_hash=c.credential_hash AND u.status='active') RETURNING user_id`, s.now().UnixMilli(), tokenDigest(code), s.now().UnixMilli(), s.now().UnixMilli()).Scan(&user)
	if err != nil {
		return "", ErrUnauthorized
	}
	return user, nil
}

func (s *Service) invalidateLocal(ctx context.Context, agent string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `UPDATE local_connections SET status='revoked',credential_enc='' WHERE agent_id=? AND status='active' AND NOT EXISTS (SELECT 1 FROM agents p JOIN agents a ON a.id=local_connections.agent_id JOIN users u ON u.id=local_connections.user_id WHERE p.id=local_connections.parent_agent_id AND p.disabled=0 AND p.owner_user=local_connections.user_id AND p.token_hash=local_connections.parent_token_hash AND a.disabled=0 AND a.owner_user=local_connections.user_id AND a.token_hash=local_connections.credential_hash AND u.status='active')`, agent)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE inbox_grants SET status='revoked',revoked_at=? WHERE agent_id=? AND status='active' AND EXISTS (SELECT 1 FROM local_connections WHERE agent_id=? AND status='revoked')`, s.now().UnixMilli(), agent, agent)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE callback_receivers SET status='removed' WHERE agent_id=? AND EXISTS (SELECT 1 FROM local_connections WHERE agent_id=? AND status='revoked')`, agent, agent)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE callback_outbox SET status='failed',terminal_reason='connection_revoked' WHERE status IN ('pending','delivering') AND receiver_id IN (SELECT id FROM callback_receivers WHERE agent_id=? AND status='removed')`, agent)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Service) revokeLocalResourcesTx(ctx context.Context, tx *sql.Tx, agent string) error {
	_, err := tx.ExecContext(ctx, `UPDATE inbox_grants SET status='revoked',revoked_at=? WHERE agent_id=? AND status='active'`, s.now().UnixMilli(), agent)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE callback_receivers SET status='removed' WHERE agent_id=?`, agent)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE callback_outbox SET status='failed',terminal_reason='connection_revoked' WHERE status IN ('pending','delivering') AND receiver_id IN (SELECT id FROM callback_receivers WHERE agent_id=?)`, agent)
	return err
}
