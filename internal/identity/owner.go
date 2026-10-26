package identity

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

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
// While an owner email is configured and nobody with that address exists
// yet, the role stays vacant: no fallback hands it to someone else.
//
// Every owner change and every automatic promotion writes an audit row in
// the same transaction (auditTx): user.owner and user.role, by the person
// or by one of the system actors below. The one exception to "always an
// active admin" is SuspendDepartedOwner: the Clerk sync blocks an owner who
// left the organisation; a host operator then moves the role with
// `toolyard owner transfer` (ForceTransferOwnership).

// System actors in the audit rows of automatic owner and role changes.
const (
	ActorSetup     = "system:setup"      // the first account, /v1/auth/setup
	ActorSignIn    = "system:sign-in"    // a Clerk sign-in claims or bootstraps
	ActorStartup   = "system:startup"    // EnsureOwner and RepromoteOwner
	ActorCLI       = "system:cli"        // toolyard owner transfer on the host
	ActorClerkSync = "system:clerk-sync" // the offboarding sync
)

var (
	// ErrOwner: the owner cannot be demoted or blocked; transfer the
	// owner role to someone else first.
	ErrOwner = errors.New("the owner cannot be demoted or blocked; transfer ownership first")
	// ErrNotOwner: only the owner can transfer the owner role.
	ErrNotOwner = errors.New("only the owner can transfer ownership")
	// ErrAlreadyOwner: the transfer names the current owner.
	ErrAlreadyOwner = errors.New("that user is already the owner")
)

// EmailEqual compares two email addresses ASCII-only and case-insensitively,
// ignoring surrounding spaces: only A-Z fold to a-z, every other byte must
// match exactly, so Unicode case folding (U+212A KELVIN SIGN for k, say)
// can never make a different address match. An empty address matches
// nothing.
func EmailEqual(a, b string) bool {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	if a == "" || len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

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

// EnsureOwner gives an install that has users but no owner one. With an
// owner email configured (the -owner-email, ownerEmail) it is the active
// user with that address (made an admin if they were a member); when
// nobody has it yet the role stays vacant, for that person's first sign-in
// to claim. Without an owner email it is the oldest active admin. An
// install with an owner is left alone, and so is one without users or
// without an active admin (the next bootstrap sign-in claims the role). It
// returns the owner, or ErrNoUser when there still is none, and audits a
// claim as system:startup. Every replica may run it at startup at the same
// time: the claim is conditional and the unique index refuses a second
// owner.
func (s *Service) EnsureOwner(ctx context.Context, ownerEmail string) (*User, error) {
	if u, err := s.Owner(ctx); !errors.Is(err, ErrNoUser) {
		return u, err
	}
	id, err := s.ownerCandidate(ctx, strings.TrimSpace(ownerEmail))
	if err != nil {
		return nil, err
	}
	if id == "" {
		return nil, ErrNoUser
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := claimOwnerTx(ctx, tx, id, time.Now().UnixMilli(), ActorStartup, "startup"); err != nil {
		if store.IsUniqueViolation(err) {
			_ = tx.Rollback()
			return s.Owner(ctx)
		}
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		if store.IsUniqueViolation(err) {
			return s.Owner(ctx)
		}
		return nil, err
	}
	return s.Owner(ctx)
}

// ownerCandidate is who EnsureOwner makes the owner, "" for nobody.
func (s *Service) ownerCandidate(ctx context.Context, ownerEmail string) (string, error) {
	if ownerEmail != "" {
		// Compared in Go (EmailEqual), not with SQL LOWER, so the rule is
		// the same on both engines and ASCII-only.
		rows, err := s.db.QueryContext(ctx,
			`SELECT id, email FROM users WHERE status = ? AND email IS NOT NULL AND email != '' ORDER BY created_at, id`,
			StatusActive)
		if err != nil {
			return "", err
		}
		defer rows.Close()
		for rows.Next() {
			var id, email string
			if err := rows.Scan(&id, &email); err != nil {
				return "", err
			}
			if EmailEqual(email, ownerEmail) {
				return id, nil
			}
		}
		return "", rows.Err()
	}
	var id string
	err := s.db.QueryRowContext(ctx,
		`SELECT id FROM users WHERE role = ? AND status = ? ORDER BY created_at, id LIMIT 1`,
		RoleAdmin, StatusActive).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// RepromoteOwner makes a stored owner who is not an admin one again: an
// older binary, running beside this one in a rolling deploy, knows nothing
// of the owner and may have demoted them. It audits the change as
// system:startup and reports whether it made one. A blocked owner stays
// blocked: a block is deliberate (the Clerk sync blocks an owner who left
// the organisation); the caller warns and a host operator moves the role
// (ForceTransferOwnership).
func (s *Service) RepromoteOwner(ctx context.Context) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	var id, username, role string
	err = tx.QueryRowContext(ctx, `SELECT id, username, role FROM users WHERE is_owner = 1`+s.db.Dialect.ForUpdate()).Scan(&id, &username, &role)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if role == RoleAdmin {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE users SET role = ?, updated_at = ? WHERE id = ? AND is_owner = 1`,
		RoleAdmin, time.Now().UnixMilli(), id); err != nil {
		return false, err
	}
	if err := auditTx(ctx, tx, "user.role", ActorStartup, "owner_repromoted",
		fmt.Sprintf("%s role=admin (the owner is always an admin)", username), map[string]any{"user_id": id}); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// TransferOwnership hands the owner role from fromID, who must be the
// owner, to toID, who must be another active user: toID becomes the owner
// and an admin; fromID stays an admin (the new owner may demote them
// later). ErrNotOwner when fromID is not the owner (also when a concurrent
// transfer moved the role first), ErrAlreadyOwner when toID is fromID,
// ErrNoUser for an unknown toID and ErrUserBlocked for a blocked one. It
// audits the move as user:<fromID> in the same transaction (auditOwnerMove);
// the caller checks who may ask (the owner in a browser session).
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
		`SELECT id, username, role, COALESCE(is_owner,0), status FROM users WHERE id IN (?, ?) ORDER BY id`+s.db.Dialect.ForUpdate(),
		fromID, toID)
	if err != nil {
		return err
	}
	fromOwner, toFound, toStatus := false, false, ""
	var fromName, toName, toRole string
	for rows.Next() {
		var id, username, role, status string
		var owner int64
		if err := rows.Scan(&id, &username, &role, &owner, &status); err != nil {
			rows.Close()
			return err
		}
		switch id {
		case fromID:
			fromOwner, fromName = owner == 1, username
		case toID:
			toFound, toStatus, toName, toRole = true, status, username, role
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
	if err := auditOwnerMove(ctx, tx, "user:"+fromID, "transfer", fromID, fromName, toID, toName, toRole); err != nil {
		return err
	}
	return tx.Commit()
}

// auditOwnerMove writes the user.owner row of a move of the owner role
// (both user ids in the summary and the arguments), and a user.role row
// when the new owner was not an admin before.
func auditOwnerMove(ctx context.Context, tx *sql.Tx, actorID, reason, fromID, fromName, toID, toName, toPrevRole string) error {
	from := "nobody"
	if fromID != "" {
		from = fmt.Sprintf("%s (%s)", fromName, fromID)
	}
	if err := auditTx(ctx, tx, "user.owner", actorID, reason,
		fmt.Sprintf("owner %s -> %s (%s)", from, toName, toID),
		map[string]any{"from_user_id": fromID, "to_user_id": toID}); err != nil {
		return err
	}
	if toPrevRole != RoleAdmin {
		return auditTx(ctx, tx, "user.role", actorID, reason,
			fmt.Sprintf("%s role=admin (the new owner)", toName), map[string]any{"user_id": toID})
	}
	return nil
}

// ForceTransferOwnership is the host operator's break-glass (`toolyard
// owner transfer`): it makes toID the owner and an admin whoever owns the
// role now, even a blocked or departed owner, and audits the move as
// actorID (system:cli). The previous owner keeps their role and status.
// toID must be active unless unblock is set, which re-activates them as
// part of the move (a lone admin who was blocked when they left the
// organisation and came back). It returns the previous owner's id ("" when
// there was none).
func (s *Service) ForceTransferOwnership(ctx context.Context, toID, actorID string, unblock bool) (string, error) {
	if toID == "" {
		return "", ErrNoUser
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx,
		`SELECT id, username, role, COALESCE(is_owner,0), status FROM users WHERE is_owner = 1 OR id = ? ORDER BY id`+s.db.Dialect.ForUpdate(),
		toID)
	if err != nil {
		return "", err
	}
	var fromID, fromName, toName, toRole, toStatus string
	toFound := false
	for rows.Next() {
		var id, username, role, status string
		var owner int64
		if err := rows.Scan(&id, &username, &role, &owner, &status); err != nil {
			rows.Close()
			return "", err
		}
		if owner == 1 {
			fromID, fromName = id, username
		}
		if id == toID {
			toFound, toName, toRole, toStatus = true, username, role, status
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return "", err
	}
	rows.Close()
	if !toFound {
		return "", ErrNoUser
	}
	if toStatus != StatusActive && !unblock {
		return "", ErrUserBlocked
	}
	if fromID == toID && toStatus == StatusActive && toRole == RoleAdmin {
		return "", ErrAlreadyOwner
	}
	now := time.Now().UnixMilli()
	if toStatus != StatusActive {
		if _, err := tx.ExecContext(ctx, `UPDATE users SET status = ?, blocked_reason = NULL, updated_at = ? WHERE id = ?`,
			StatusActive, now, toID); err != nil {
			return "", err
		}
		if err := auditTx(ctx, tx, "user.status", actorID, "break_glass",
			fmt.Sprintf("%s status=active", toName), map[string]any{"user_id": toID}); err != nil {
			return "", err
		}
	}
	if fromID != toID {
		if _, err := tx.ExecContext(ctx, `UPDATE users SET is_owner = 0, updated_at = ? WHERE is_owner = 1`, now); err != nil {
			return "", err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE users SET is_owner = 1, role = ?, updated_at = ? WHERE id = ?`,
		RoleAdmin, now, toID); err != nil {
		return "", err
	}
	if fromID != toID {
		if err := auditOwnerMove(ctx, tx, actorID, "break_glass", fromID, fromName, toID, toName, toRole); err != nil {
			return "", err
		}
	} else if toRole != RoleAdmin {
		if err := auditTx(ctx, tx, "user.role", actorID, "break_glass",
			fmt.Sprintf("%s role=admin (the owner)", toName), map[string]any{"user_id": toID}); err != nil {
			return "", err
		}
	}
	return fromID, tx.Commit()
}

// SuspendDepartedOwner is what the Clerk sync does to an owner who left
// the organisation: block them (reason left_org), which ends their browser
// sessions and stops their agent and operator tokens, then also disable
// their agents and revoke their operator tokens for good, all audited as
// system:clerk-sync. The row and the owner role stay; an admin unblocks a
// mistaken departure in the dashboard, and a host operator moves the role
// with ForceTransferOwnership. It reports whether it changed anything (an
// owner already blocked is left alone).
func (s *Service) SuspendDepartedOwner(ctx context.Context, id string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	var username string
	err = tx.QueryRowContext(ctx, `SELECT username FROM users WHERE id = ? AND is_owner = 1 AND status = ?`+s.db.Dialect.ForUpdate(),
		id, StatusActive).Scan(&username)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	now := time.Now().UnixMilli()
	if _, err := tx.ExecContext(ctx, `UPDATE users SET status = ?, blocked_reason = 'left_org', updated_at = ? WHERE id = ?`,
		StatusBlocked, now, id); err != nil {
		return false, err
	}
	if err := revokeAllForUser(ctx, tx, id, now); err != nil {
		return false, err
	}
	if err := revokeLocalConnectionsTx(ctx, tx, `user_id=?`, []any{id}, now); err != nil {
		return false, err
	}
	agents, tokens, err := revokeCredentialsTx(ctx, tx, id, now)
	if err != nil {
		return false, err
	}
	if err := auditTx(ctx, tx, "user.status", ActorClerkSync, "left_org",
		fmt.Sprintf("%s status=blocked: the owner left the organisation; sessions ended, %d agents disabled, %d operator tokens revoked; still the owner until `toolyard owner transfer`", username, agents, tokens),
		map[string]any{"user_id": id, "agents_disabled": agents, "operator_tokens_revoked": tokens}); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// RevokeUserCredentials disables id's agents and revokes their operator
// tokens, audited as actorID with reason. A transfer in personal sign-in
// mode offers it for the previous owner, who can no longer sign in.
func (s *Service) RevokeUserCredentials(ctx context.Context, id, actorID, reason string) (agents, tokens int64, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = tx.Rollback() }()
	var username string
	if err := tx.QueryRowContext(ctx, `SELECT username FROM users WHERE id = ?`, id).Scan(&username); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, 0, ErrNoUser
		}
		return 0, 0, err
	}
	now := time.Now().UnixMilli()
	if agents, tokens, err = revokeCredentialsTx(ctx, tx, id, now); err != nil {
		return 0, 0, err
	}
	if err := revokeLocalConnectionsTx(ctx, tx, `user_id=?`, []any{id}, now); err != nil {
		return 0, 0, err
	}
	if err := auditTx(ctx, tx, "user.credentials_revoked", actorID, reason,
		fmt.Sprintf("%s: %d agents disabled, %d operator tokens revoked", username, agents, tokens),
		map[string]any{"user_id": id, "agents_disabled": agents, "operator_tokens_revoked": tokens}); err != nil {
		return 0, 0, err
	}
	return agents, tokens, tx.Commit()
}

// revokeCredentialsTx disables uid's enrolled agents (OAuth clients' agents
// included; the identity agent stays with identitykeys and stops with the
// owner's status) and revokes their operator tokens.
func revokeCredentialsTx(ctx context.Context, tx *sql.Tx, uid string, now int64) (agents, tokens int64, err error) {
	res, err := tx.ExecContext(ctx, `UPDATE agents SET disabled = 1 WHERE owner_user = ? AND kind = ? AND COALESCE(disabled,0) = 0`,
		uid, AgentKindAgent)
	if err != nil {
		return 0, 0, err
	}
	if agents, err = res.RowsAffected(); err != nil {
		return 0, 0, err
	}
	res, err = tx.ExecContext(ctx, `UPDATE operator_tokens SET revoked_at = ? WHERE user_id = ? AND revoked_at IS NULL`, now, uid)
	if err != nil {
		return 0, 0, err
	}
	if tokens, err = res.RowsAffected(); err != nil {
		return 0, 0, err
	}
	return agents, tokens, nil
}

// AgentName is the name of the agent agentID, whoever owns it, or "" when
// there is no such agent.
func (s *Service) AgentName(ctx context.Context, agentID string) string {
	var name string
	if err := s.db.QueryRowContext(ctx, `SELECT name FROM agents WHERE id = ?`, agentID).Scan(&name); err != nil {
		return ""
	}
	return name
}

// auditTx writes one audit row (the audit_events table audit.Logger
// writes) in the caller's transaction, so an owner or role change and its
// record commit together or not at all, as the federation and OAuth
// services do for theirs.
func auditTx(ctx context.Context, tx *sql.Tx, eventType, actorID, reason, summary string, args map[string]any) error {
	var body any
	if len(args) > 0 {
		b, err := json.Marshal(args)
		if err != nil {
			return err
		}
		body = string(b)
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO audit_events(id, ts, agent_id, event_type, reason, result_summary, arguments) VALUES(?,?,?,?,?,?,?)`,
		uuid.NewString(), time.Now().UnixMilli(), actorID, eventType, nullStr(reason), nullStr(summary), body)
	return err
}

// claimOwnerTx makes id the owner and an admin, in tx, when the install
// has no owner and id is active, and audits it as actorID (user.owner, and
// user.role when id was not an admin); otherwise it changes nothing and
// reports false. A concurrent claim that committed first makes this a
// unique violation, which the caller's retry turns into "there is an
// owner".
func claimOwnerTx(ctx context.Context, tx *sql.Tx, id string, now int64, actorID, reason string) (bool, error) {
	var username, role string
	if err := tx.QueryRowContext(ctx, `SELECT username, role FROM users WHERE id = ?`, id).Scan(&username, &role); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE users SET is_owner = 1, role = ?, updated_at = ?
         WHERE id = ? AND status = ? AND NOT EXISTS (SELECT 1 FROM users o WHERE o.is_owner = 1)`,
		RoleAdmin, now, id, StatusActive)
	if err != nil {
		return false, err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return false, err
	}
	return true, auditOwnerMove(ctx, tx, actorID, reason, "", "", id, username, role)
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
