package skillhub

// The skill_proposals table (migration 0070): one row per skills hub
// question that waits in the Inbox (an agent's source proposal, a review
// of a staged version), with its outcome. Status changes are conditional
// UPDATEs, so a decision is applied once even when two paths (the Inbox
// handler and a reconcile) reach it together.

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// Proposal kinds and statuses as stored.
const (
	proposalKindSource = "source"
	proposalKindReview = "review"

	proposalPending   = "pending"
	proposalApproved  = "approved" // approved, being applied
	proposalDone      = "done"
	proposalRejected  = "rejected"
	proposalFailed    = "failed"
	proposalCancelled = "cancelled"
)

// proposalRow is one skill_proposals row.
type proposalRow struct {
	ID        string
	Kind      string
	Status    string
	AgentID   string
	UserID    string
	SourceID  string
	VersionID string
	InboxID   string
	Input     string
	Reason    string
	Result    string
	Error     string
	DecidedBy string
	CreatedAt int64
	UpdatedAt int64
}

// proposalUpdate is what a status change writes besides the status.
// Empty fields stay as they are.
type proposalUpdate struct {
	Result    string
	Error     string
	DecidedBy string
}

type proposalStore struct {
	db  *store.DB
	now func() time.Time
}

// proposalNewID returns "skp_" and 16 random hex characters.
func proposalNewID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("skillhub: crypto/rand failed: " + err.Error())
	}
	return "skp_" + hex.EncodeToString(b[:])
}

const proposalColumns = `id, kind, status, agent_id, user_id, source_id, version_id, inbox_id, input, reason, result, error, decided_by, created_at, updated_at`

func (st proposalStore) insert(ctx context.Context, row *proposalRow) error {
	now := st.now().UnixMilli()
	row.CreatedAt, row.UpdatedAt = now, now
	if row.Input == "" {
		row.Input = "{}"
	}
	_, err := st.db.ExecContext(ctx, `INSERT INTO skill_proposals(`+proposalColumns+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		row.ID, row.Kind, row.Status, row.AgentID, row.UserID, row.SourceID, row.VersionID, row.InboxID,
		row.Input, row.Reason, row.Result, row.Error, row.DecidedBy, row.CreatedAt, row.UpdatedAt)
	return err
}

func scanProposal(sc interface{ Scan(...any) error }) (*proposalRow, error) {
	var r proposalRow
	if err := sc.Scan(&r.ID, &r.Kind, &r.Status, &r.AgentID, &r.UserID, &r.SourceID, &r.VersionID, &r.InboxID,
		&r.Input, &r.Reason, &r.Result, &r.Error, &r.DecidedBy, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return nil, err
	}
	return &r, nil
}

// get loads one row; ErrNotFound when there is none.
func (st proposalStore) get(ctx context.Context, id string) (*proposalRow, error) {
	r, err := scanProposal(st.db.QueryRowContext(ctx, `SELECT `+proposalColumns+` FROM skill_proposals WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return r, err
}

func (st proposalStore) setInbox(ctx context.Context, id, inboxID string) error {
	_, err := st.db.ExecContext(ctx, `UPDATE skill_proposals SET inbox_id = ?, updated_at = ? WHERE id = ?`, inboxID, st.now().UnixMilli(), id)
	return err
}

// move changes a row's status from one of from to to, and reports whether
// it did (false: the row was not in one of those statuses, for example a
// decision another path already applied).
func (st proposalStore) move(ctx context.Context, id string, from []string, to string, upd proposalUpdate) (bool, error) {
	now := st.now().UnixMilli()
	q := `UPDATE skill_proposals SET status = ?, updated_at = ?`
	args := []any{to, now}
	if upd.Result != "" {
		q += `, result = ?`
		args = append(args, upd.Result)
	}
	if upd.Error != "" {
		q += `, error = ?`
		args = append(args, upd.Error)
	}
	if upd.DecidedBy != "" {
		q += `, decided_by = ?, decided_at = ?`
		args = append(args, upd.DecidedBy, now)
	}
	q += ` WHERE id = ? AND status IN (` + strings.TrimSuffix(strings.Repeat("?,", len(from)), ",") + `)`
	args = append(args, id)
	for _, f := range from {
		args = append(args, f)
	}
	res, err := st.db.ExecContext(ctx, q, args...)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// pendingReviews lists a source's pending review rows; versionID "" means
// every version.
func (st proposalStore) pendingReviews(ctx context.Context, sourceID, versionID string) ([]proposalRow, error) {
	q := `SELECT ` + proposalColumns + ` FROM skill_proposals WHERE source_id = ? AND kind = ? AND status = ?`
	args := []any{sourceID, proposalKindReview, proposalPending}
	if versionID != "" {
		q += ` AND version_id = ?`
		args = append(args, versionID)
	}
	rows, err := st.db.QueryContext(ctx, q+` ORDER BY created_at`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []proposalRow
	for rows.Next() {
		r, err := scanProposal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// countPending counts an agent's pending rows of one kind.
func (st proposalStore) countPending(ctx context.Context, agentID, kind string) (int, error) {
	var n int
	err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM skill_proposals WHERE agent_id = ? AND kind = ? AND status = ?`,
		agentID, kind, proposalPending).Scan(&n)
	return n, err
}

// pendingRows lists pending rows of one kind, oldest first: one agent's
// (agentID set) or everyone's, at most limit.
func (st proposalStore) pendingRows(ctx context.Context, kind, agentID string, limit int) ([]proposalRow, error) {
	q := `SELECT ` + proposalColumns + ` FROM skill_proposals WHERE kind = ? AND status = ?`
	args := []any{kind, proposalPending}
	if agentID != "" {
		q += ` AND agent_id = ?`
		args = append(args, agentID)
	}
	q += ` ORDER BY created_at LIMIT ?`
	args = append(args, limit)
	rows, err := st.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []proposalRow
	for rows.Next() {
		r, err := scanProposal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// openReviewFor reports whether a version already has a review row that is
// waiting (pending) or being applied (approved).
func (st proposalStore) openReviewFor(ctx context.Context, sourceID, versionID string) (bool, error) {
	var n int
	err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM skill_proposals WHERE kind = ? AND source_id = ? AND version_id = ? AND status IN (?, ?)`,
		proposalKindReview, sourceID, versionID, proposalPending, proposalApproved).Scan(&n)
	return n > 0, err
}

// remove deletes a row (one whose Inbox item was never created).
func (st proposalStore) remove(ctx context.Context, id string) error {
	_, err := st.db.ExecContext(ctx, `DELETE FROM skill_proposals WHERE id = ?`, id)
	return err
}
