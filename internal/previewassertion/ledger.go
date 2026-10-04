package previewassertion

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// SQLLedger permanently binds one enrolled agent ID to its operator-verified
// immutable tuple. It stores no credentials, assertions, or proof contents.
type SQLLedger struct{ DB *sql.DB }

// CommittedBefore requires an original pre-approval commitment. It never creates
// one for a historical approval from an unrelated generic upstream.
func (l *SQLLedger) CommittedBefore(ctx context.Context, agentID string, createdAtMillis int64) error {
	if l == nil || l.DB == nil || createdAtMillis <= 0 || ctx.Err() != nil {
		return ErrIdentity
	}
	var recorded int64
	if l.DB.QueryRowContext(ctx, `SELECT recorded_at FROM bks_preview_enrollment_ledger WHERE agent_id=?`, agentID).Scan(&recorded) != nil || recorded <= 0 || recorded > createdAtMillis {
		return ErrIdentity
	}
	return nil
}

// CurrentPrincipal uses a strict join intentionally specific to preview calls.
// Ordinary Toolyard authentication compatibility remains unchanged.
func CurrentPrincipal(ctx context.Context, db *sql.DB, id string) (Principal, error) {
	if db == nil {
		return Principal{}, ErrIdentity
	}
	return currentPrincipal(ctx, db, id)
}

type principalQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func currentPrincipal(ctx context.Context, q principalQuerier, id string) (Principal, error) {
	var p Principal
	err := q.QueryRowContext(ctx, `SELECT a.id, a.owner_user
		FROM agents a INNER JOIN users u ON u.id = a.owner_user
		WHERE a.id = ? AND a.kind = 'agent' AND COALESCE(a.disabled,0) = 0
		AND a.token_hash IS NOT NULL AND a.token_hash <> ''
		AND a.enroll_code IS NULL AND u.status = 'active'`, id).Scan(&p.ID, &p.OwnerUserID)
	if err != nil {
		return Principal{}, ErrIdentity
	}
	return p, nil
}

func (l *SQLLedger) Commit(ctx context.Context, b Binding) error {
	if l == nil || l.DB == nil || ctx.Err() != nil || !validateBinding(b, b.IssuedAt) {
		return ErrIdentity
	}
	// WAL's default NORMAL synchronization can lose a recently accepted row
	// after an OS/power restart. Pin and verify FULL on this exact connection
	// before the immutable commitment transaction. Never modify store.Open.
	conn, err := l.DB.Conn(ctx)
	if err != nil {
		return ErrIdentity
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, `PRAGMA synchronous=FULL`); err != nil {
		return ErrIdentity
	}
	var synchronous int
	if conn.QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&synchronous) != nil || synchronous != 2 {
		return ErrIdentity
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return ErrIdentity
	}
	defer tx.Rollback()
	// Do not record a registry entry for an absent/blocked/incomplete principal.
	// Check the principal inside the same transaction as the first commitment.
	p, err := currentPrincipal(ctx, tx, b.PrincipalID)
	if err != nil || p.OwnerUserID != b.OwnerUserID {
		return ErrIdentity
	}
	var owner, sid, proof, mode, approver string
	err = tx.QueryRowContext(ctx, `SELECT owner_user_id, session_id, proof_sha256, credential_mode, approved_by
		FROM bks_preview_enrollment_ledger WHERE agent_id = ?`, b.PrincipalID).Scan(&owner, &sid, &proof, &mode, &approver)
	switch {
	case err == nil:
		if owner != b.OwnerUserID || sid != b.SessionID || proof != b.ProofSHA256 || mode != b.CredentialMode || approver != b.ApprovedBy {
			return ErrIdentity
		}
	case errors.Is(err, sql.ErrNoRows):
		_, err = tx.ExecContext(ctx, `INSERT INTO bks_preview_enrollment_ledger
			(agent_id,owner_user_id,session_id,proof_sha256,credential_mode,approved_by,recorded_at)
			VALUES(?,?,?,?,?,?,?)`, b.PrincipalID, b.OwnerUserID, b.SessionID, b.ProofSHA256, b.CredentialMode, b.ApprovedBy, time.Now().UnixMilli())
		if err != nil {
			return ErrIdentity
		}
	default:
		return ErrIdentity
	}
	if tx.Commit() != nil {
		return ErrIdentity
	}
	return nil
}
