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
	"fmt"
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

// setInboxIfPending records a row's Inbox item while the row is still
// pending, and reports whether it did: a row closed while its item was
// being created (another check replaced it, the source was decided) keeps
// no item, and the caller withdraws the item.
func (st proposalStore) setInboxIfPending(ctx context.Context, id, inboxID string) (bool, error) {
	res, err := st.db.ExecContext(ctx, `UPDATE skill_proposals SET inbox_id = ?, updated_at = ? WHERE id = ? AND status = ?`,
		inboxID, st.now().UnixMilli(), id, proposalPending)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
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

// lockedApproval runs decide in one transaction that holds the row of
// source sourceID (FOR UPDATE on Postgres; SQLite has one writer), with
// the source as it is now, its waiting and live versions, the version it
// would serve next with its current settings (nextVersion; each nil when
// none) and its open approval rows (pending, or approved and being
// applied). A row decide returns is inserted in the same transaction and
// returned: two askers on any replica never both add a question, and none
// adds one that shows the source otherwise than it is. decide must not use the
// database (on SQLite the transaction holds the only connection). A
// source that is gone is ErrNotFound.
func (st proposalStore) lockedApproval(ctx context.Context, sourceID string,
	decide func(src *sourceRow, waiting, live, next *versionRow, open []proposalRow) *proposalRow) (*proposalRow, error) {
	tx, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var id string
	err = tx.QueryRowContext(ctx, `SELECT id FROM skill_sources WHERE id = ?`+st.db.Dialect.ForUpdate(), sourceID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: source %s", ErrNotFound, sourceID)
	}
	if err != nil {
		return nil, err
	}
	src, err := scanSource(tx.QueryRowContext(ctx, `SELECT `+sourceColumns+sourceFrom+` WHERE s.id = ?`, sourceID))
	if err != nil {
		return nil, err
	}
	version := func(id string) (*versionRow, error) {
		if id == "" {
			return nil, nil
		}
		v, err := scanVersion(tx.QueryRowContext(ctx, `SELECT `+versionColumns+` FROM skill_versions WHERE id = ? AND source_id = ?`, id, sourceID))
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return v, err
	}
	waiting, err := version(src.PendingVersionID)
	if err != nil {
		return nil, err
	}
	live, err := version(src.ActiveVersionID)
	if err != nil {
		return nil, err
	}
	var next *versionRow
	if target := src.headSHA; target != "" || src.Mode == ModePinned {
		if src.Mode == ModePinned {
			target = src.PinnedSHA
		}
		next, err = scanVersion(tx.QueryRowContext(ctx, `SELECT `+versionColumns+` FROM skill_versions
			WHERE source_id = ? AND sha = ? AND config = ? ORDER BY created_at DESC, id DESC LIMIT 1`, sourceID, target, src.configHash()))
		if errors.Is(err, sql.ErrNoRows) {
			next, err = nil, nil
		}
		if err != nil {
			return nil, err
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+proposalColumns+` FROM skill_proposals WHERE kind = ? AND source_id = ? AND status IN (?, ?) ORDER BY created_at`,
		proposalKindApproval, sourceID, proposalPending, proposalApproved)
	if err != nil {
		return nil, err
	}
	open, err := collectProposals(rows)
	if err != nil {
		return nil, err
	}
	row := decide(src, waiting, live, next, open)
	if row == nil {
		return nil, nil
	}
	now := st.now().UnixMilli()
	row.CreatedAt, row.UpdatedAt = now, now
	if row.Input == "" {
		row.Input = "{}"
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO skill_proposals(`+proposalColumns+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		row.ID, row.Kind, row.Status, row.AgentID, row.UserID, row.SourceID, row.VersionID, row.InboxID,
		row.Input, row.Reason, row.Result, row.Error, row.DecidedBy, row.CreatedAt, row.UpdatedAt); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return row, nil
}

// latestForSource is the newest row of one kind about a source, any
// status, or nil.
func (st proposalStore) latestForSource(ctx context.Context, kind, sourceID string) (*proposalRow, error) {
	r, err := scanProposal(st.db.QueryRowContext(ctx, `SELECT `+proposalColumns+` FROM skill_proposals WHERE kind = ? AND source_id = ?
		ORDER BY created_at DESC, id DESC LIMIT 1`, kind, sourceID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

// pendingForSource lists the pending rows of one kind about a source.
func (st proposalStore) pendingForSource(ctx context.Context, kind, sourceID string) ([]proposalRow, error) {
	rows, err := st.db.QueryContext(ctx, `SELECT `+proposalColumns+` FROM skill_proposals WHERE kind = ? AND source_id = ? AND status = ? ORDER BY created_at`,
		kind, sourceID, proposalPending)
	if err != nil {
		return nil, err
	}
	return collectProposals(rows)
}

// staleApproved lists rows of one kind still approved (being applied)
// whose last update is older than before: an approval whose process
// stopped before it finished.
func (st proposalStore) staleApproved(ctx context.Context, kind string, before int64, limit int) ([]proposalRow, error) {
	rows, err := st.db.QueryContext(ctx, `SELECT `+proposalColumns+` FROM skill_proposals WHERE kind = ? AND status = ? AND updated_at < ? ORDER BY updated_at LIMIT ?`,
		kind, proposalApproved, before, limit)
	if err != nil {
		return nil, err
	}
	return collectProposals(rows)
}

// claim moves a row's updated_at to now when it is still in status with
// updatedAt (compare-and-swap), and reports whether this caller won: of
// two replicas resuming the same row, one goes on.
func (st proposalStore) claim(ctx context.Context, id, status string, updatedAt int64) (bool, error) {
	res, err := st.db.ExecContext(ctx, `UPDATE skill_proposals SET updated_at = ? WHERE id = ? AND status = ? AND updated_at = ?`,
		max(st.now().UnixMilli(), updatedAt+1), id, status, updatedAt)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// collectProposals reads and closes a row set.
func collectProposals(rows *sql.Rows) ([]proposalRow, error) {
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
