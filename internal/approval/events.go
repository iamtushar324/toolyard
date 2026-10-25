package approval

import (
	"context"
	"database/sql"

	"github.com/tusharbhardwaj/toolyard/internal/logx"
)

// Webhook events of legacy deferred approvals, keyed by approval ID.
const (
	// EventApprovalDecided: the approval left pending (allowed or denied).
	EventApprovalDecided = "approval.decided"
	// EventApprovalExecuted: the allowed call ran and its result was
	// recorded (or recorded as outcome_unknown).
	EventApprovalExecuted = "approval.executed"
)

// EventSink queues the webhook events of approvals (internal/callbacks).
// SubscribeApproval runs once a request exists: ref is the call's
// _callback_ref, "" for the agent's default receiver. ApprovalEventTx runs
// inside the transaction that records the decision or the result, so a
// subscriber hears of it at least once.
type EventSink interface {
	SubscribeApproval(ctx context.Context, req *Request, ref string) error
	ApprovalEventTx(ctx context.Context, tx *sql.Tx, req *Request, eventType string) error
}

// SetEventSink installs the webhook event sink. Pass nil to stop emitting.
func (b *Bus) SetEventSink(s EventSink) { b.events = s }

// subscribe lets the request's receivers follow it. A refused callback
// reference does not stop the call (its deferred answer and
// tools.poll_approval still work): it is logged and returned to the caller
// in req.CallbackWarning, which the gateway adds to the deferred answer.
func (b *Bus) subscribe(ctx context.Context, req *Request, ref string) {
	if b.events == nil || req == nil {
		return
	}
	if err := b.events.SubscribeApproval(ctx, req, ref); err != nil {
		logx.For("approval").Warn("approval callback subscription refused", "approval_id", req.ID, "err", err.Error())
		if ref != "" {
			req.CallbackWarning = "_callback_ref was not registered for this approval: " + err.Error() + ". No approval.decided or approval.executed callback will arrive; poll with tools.poll_approval."
		}
	}
}

// decideTx applies a decision UPDATE and queues approval.decided in one
// transaction, and returns the rows it decided (0: no longer pending).
func (b *Bus) decideTx(ctx context.Context, id, update string, updateArgs []any) (int64, error) {
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, update, updateArgs...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if n == 1 {
		if err := b.emitDecidedTx(ctx, tx, id); err != nil {
			return 0, err
		}
	}
	return n, tx.Commit()
}

// emitDecidedTx queues approval.decided for the receivers that follow id,
// inside the decision's transaction.
func (b *Bus) emitDecidedTx(ctx context.Context, tx *sql.Tx, id string) error {
	if b.events == nil {
		return nil
	}
	decided, err := getTx(ctx, tx, id)
	if err != nil {
		return err
	}
	return b.events.ApprovalEventTx(ctx, tx, decided, EventApprovalDecided)
}

// getTx reads one approval inside tx (a decision or result transaction
// that must not use the pool: SQLite has one connection).
func getTx(ctx context.Context, tx *sql.Tx, id string) (*Request, error) {
	return scanRequest(tx.QueryRowContext(ctx, `SELECT `+requestSelectColumns+` FROM approval_requests WHERE id = ?`, id))
}
