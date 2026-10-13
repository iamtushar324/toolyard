package identity

import (
	"context"
	"database/sql"
)

// revokeLocalConnectionsTx persists the revocation at the identity mutation,
// including when every local client is offline throughout a disable/restore.
// The predicate is chosen only by the two owner/status handlers below. This
// does not change ordinary agent re-enable behavior or federation credentials.
func revokeLocalConnectionsTx(ctx context.Context, tx *sql.Tx, predicate string, args []any, now int64) error {
	_, err := tx.ExecContext(ctx, `UPDATE local_connections SET status='revoked',credential_enc='' WHERE `+predicate, args...)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE inbox_grants SET status='revoked',revoked_at=? WHERE status='active' AND agent_id IN (SELECT agent_id FROM local_connections WHERE `+predicate+`)`, append([]any{now}, args...)...)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE callback_receivers SET status='removed' WHERE agent_id IN (SELECT agent_id FROM local_connections WHERE `+predicate+`)`, args...)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE callback_outbox SET status='failed',terminal_reason='connection_revoked' WHERE status IN ('pending','delivering') AND receiver_id IN (SELECT id FROM callback_receivers WHERE agent_id IN (SELECT agent_id FROM local_connections WHERE `+predicate+`))`, args...)
	if err != nil {
		return err
	}
	hostPredicate := predicate
	hostArgs := args
	if predicate == `user_id=? AND (parent_agent_id=? OR agent_id=?)` {
		hostPredicate = `user_id=? AND agent_id=?`
		hostArgs = []any{args[0], args[2]}
	}
	return revokeHostConnectionsTx(ctx, tx, hostPredicate, hostArgs, now)
}

// Host authorization never reappears after account or agent disable/restore.
func revokeHostConnectionsTx(ctx context.Context, tx *sql.Tx, predicate string, args []any, now int64) error {
	if _, err := tx.ExecContext(ctx, `UPDATE host_connections SET status='revoked',credential_enc='',updated_at=? WHERE `+predicate, append([]any{now}, args...)...); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE inbox_grants SET status='revoked',revoked_at=? WHERE status='active' AND agent_id IN(SELECT agent_id FROM host_connections WHERE `+predicate+`)`, append([]any{now}, args...)...); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE callback_receivers SET status='removed' WHERE agent_id IN(SELECT agent_id FROM host_connections WHERE `+predicate+`)`, args...); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE callback_outbox SET status='failed',terminal_reason='connection_revoked' WHERE status IN('pending','delivering') AND receiver_id IN(SELECT id FROM callback_receivers WHERE agent_id IN(SELECT agent_id FROM host_connections WHERE `+predicate+`))`, args...)
	return err
}
