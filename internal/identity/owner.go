package identity

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// The owner is the one person in charge of an install. users.is_owner marks
// them (migration 0072) and a partial unique index keeps it to one row.
// The owner is always an active admin: SetRole and SetStatus refuse to
// demote or block them (ErrOwner), and only they can hand the role to
// another active user (TransferOwnership), who becomes an admin too. The
// previous owner stays an admin until someone demotes them.
//
// Who becomes the owner, on an install that has none:
//   - the first account /v1/auth/setup creates (CreateUser);
//   - the first verified Clerk sign-in (UpsertClerkUser): with an owner
//     email configured (-owner-email), only that person, on their first
//     sign-in; without one, whoever the sign-in makes the first admin;
//   - for an install that had users before 0072, EnsureOwner at startup:
//     the active user with the configured owner email, else the oldest
//     active admin.
//
// The configured owner email only seeds the owner. Once there is one, the
// stored owner is what counts (OwnerSignInEmail for personal sign-in).

var (
	// ErrOwner: the owner cannot be demoted or blocked; transfer the
	// owner role to someone else first.
	ErrOwner = errors.New("the owner cannot be demoted or blocked; transfer ownership first")
	// ErrNotOwner: only the owner can transfer the owner role.
	ErrNotOwner = errors.New("only the owner can transfer ownership")
	// ErrAlreadyOwner: the transfer names the current owner.
	ErrAlreadyOwner = errors.New("that user is already the owner")
)

// SetOwnerEmail records the configured -owner-email. On an install without
// an owner, only a sign-in with this address claims the role, through
// every route that creates users (a caller that passes no owner email to
// UpsertClerkUser included). Call it once at startup, before serving.
func (s *Service) SetOwnerEmail(email string) { s.ownerEmail = strings.TrimSpace(email) }

// Owner returns the owner, or ErrNoUser when the install has none yet.
func (s *Service) Owner(ctx context.Context) (*User, error) {
	u, err := scanUser(s.db.QueryRowContext(ctx,
		`SELECT `+userColumns+` FROM users u WHERE u.is_owner = 1`))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoUser
	}
	if err != nil {
		return nil, err
	}
	return u, nil
}

// OwnerOrPrimary is the person in charge: the owner, else (an install
// with users but no owner yet) the oldest account, as before 0072. Owner
// notifications (push, the Inbox, events, reauthorisation) go to them.
func (s *Service) OwnerOrPrimary(ctx context.Context) (*User, error) {
	u, err := s.Owner(ctx)
	if errors.Is(err, ErrNoUser) {
		return s.PrimaryUser(ctx)
	}
	return u, err
}

// OwnerSignInEmail is the one Google address personal sign-in
// (-clerk-owner-only) admits: the owner's email, else seed (the configured
// -owner-email) while there is no owner or the owner has no email on file
// (a password account that the first Google sign-in with seed links to).
// After a transfer it is the new owner's address, so the new owner can
// sign in and the previous one no longer can.
func (s *Service) OwnerSignInEmail(ctx context.Context, seed string) (string, error) {
	seed = strings.TrimSpace(seed)
	u, err := s.Owner(ctx)
	if errors.Is(err, ErrNoUser) {
		return seed, nil
	}
	if err != nil {
		return "", err
	}
	if e := strings.TrimSpace(u.Email); e != "" {
		return e, nil
	}
	return seed, nil
}

// EnsureOwner gives an install that has users but no owner one: the
// active user whose email is ownerEmail (the configured -owner-email; made
// an admin if they were a member), else the oldest active admin. An
// install with an owner is left alone, and so is one without users or
// without an active admin (the next bootstrap sign-in claims the role).
// It returns the owner, or ErrNoUser when there still is none. Every
// replica may run it at startup at the same time: the claim is
// conditional and the unique index refuses a second owner.
func (s *Service) EnsureOwner(ctx context.Context, ownerEmail string) (*User, error) {
	if u, err := s.Owner(ctx); !errors.Is(err, ErrNoUser) {
		return u, err
	}
	var id string
	if ownerEmail = strings.TrimSpace(ownerEmail); ownerEmail != "" {
		err := s.db.QueryRowContext(ctx,
			`SELECT id FROM users WHERE LOWER(email) = LOWER(?) AND status = ? ORDER BY created_at, id LIMIT 1`,
			ownerEmail, StatusActive).Scan(&id)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}
	if id == "" {
		err := s.db.QueryRowContext(ctx,
			`SELECT id FROM users WHERE role = ? AND status = ? ORDER BY created_at, id LIMIT 1`,
			RoleAdmin, StatusActive).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNoUser
		}
		if err != nil {
			return nil, err
		}
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE users SET is_owner = 1, role = ?, updated_at = ?
         WHERE id = ? AND status = ? AND NOT EXISTS (SELECT 1 FROM users o WHERE o.is_owner = 1)`,
		RoleAdmin, time.Now().UnixMilli(), id, StatusActive); err != nil && !store.IsUniqueViolation(err) {
		return nil, err
	}
	return s.Owner(ctx)
}

// TransferOwnership hands the owner role from fromID, who must be the
// owner, to toID, who must be another active user: toID becomes the owner
// and an admin; fromID stays an admin (the new owner may demote them
// later). ErrNotOwner when fromID is not the owner (also when a concurrent
// transfer moved the role first), ErrAlreadyOwner when toID is fromID,
// ErrNoUser for an unknown toID and ErrUserBlocked for a blocked one. The
// caller checks who may ask (the owner in a browser session) and audits.
func (s *Service) TransferOwnership(ctx context.Context, fromID, toID string) error {
	if fromID == "" || toID == "" {
		return ErrNoUser
	}
	if fromID == toID {
		return ErrAlreadyOwner
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// Both rows locked in id order, the order ensureOtherActiveAdmin locks
	// admins in, so a transfer and a concurrent demotion or block wait for
	// each other instead of deadlocking (Postgres; SQLite's single
	// connection already serialises them).
	rows, err := tx.QueryContext(ctx,
		`SELECT id, COALESCE(is_owner,0), status FROM users WHERE id IN (?, ?) ORDER BY id`+s.db.Dialect.ForUpdate(),
		fromID, toID)
	if err != nil {
		return err
	}
	fromOwner, toFound, toStatus := false, false, ""
	for rows.Next() {
		var id, status string
		var owner int64
		if err := rows.Scan(&id, &owner, &status); err != nil {
			rows.Close()
			return err
		}
		switch id {
		case fromID:
			fromOwner = owner == 1
		case toID:
			toFound, toStatus = true, status
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if !fromOwner {
		return ErrNotOwner
	}
	if !toFound {
		return ErrNoUser
	}
	if toStatus != StatusActive {
		return ErrUserBlocked
	}
	now := time.Now().UnixMilli()
	// Clear first: the unique index allows one owner at any moment.
	res, err := tx.ExecContext(ctx, `UPDATE users SET is_owner = 0, updated_at = ? WHERE id = ? AND is_owner = 1`, now, fromID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return ErrNotOwner
	}
	res, err = tx.ExecContext(ctx, `UPDATE users SET is_owner = 1, role = ?, updated_at = ? WHERE id = ? AND status = ?`,
		RoleAdmin, now, toID, StatusActive)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return ErrUserBlocked
	}
	return tx.Commit()
}

// claimOwnerTx makes id the owner and an admin, in tx, when the install
// has no owner and id is active; otherwise it changes nothing. A
// concurrent claim that committed first makes this a unique violation,
// which the caller's retry turns into "there is an owner".
func claimOwnerTx(ctx context.Context, tx *sql.Tx, id string, now int64) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE users SET is_owner = 1, role = ?, updated_at = ?
         WHERE id = ? AND status = ? AND NOT EXISTS (SELECT 1 FROM users o WHERE o.is_owner = 1)`,
		RoleAdmin, now, id, StatusActive)
	return err
}

// noOwner reports whether the install has no owner.
func noOwner(ctx context.Context, tx *sql.Tx) (bool, error) {
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM users WHERE is_owner = 1`).Scan(&n); err != nil {
		return false, err
	}
	return n == 0, nil
}

// noUsers reports whether the store has no users at all.
func noUsers(ctx context.Context, tx *sql.Tx) (bool, error) {
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&n); err != nil {
		return false, err
	}
	return n == 0, nil
}
