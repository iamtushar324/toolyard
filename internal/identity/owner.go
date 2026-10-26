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
//   - for an install that had users before 0072 and no owner email
//     configured, EnsureOwner at startup: the oldest active admin. With an
//     owner email configured, startup never picks anyone (a stored email
//     may be unverified); the configured person's next verified sign-in
//     claims the role, through the link to the setup account or as a
//     returning user.
//
// The configured owner email only seeds the owner. Once there is one, the
// stored owner is what counts (OwnerSignInEmail for personal sign-in).
// While an owner email is configured and that person has not signed in
// with a verified address yet, the role stays vacant: no fallback hands it
// to someone else.
//
// Every owner change and every automatic promotion writes an audit row in
// the same transaction (auditTx): user.owner and user.role, by the person
// or by one of the system actors below. These rows go straight into
// audit_events (they hold ids, usernames and counts, nothing to redact):
// GET /v1/audit and the exports list them, the live audit stream does not
// push them. The one exception to "always an
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

// ActiveOwnerOrAdmin is who acts for the install: the owner while they are
// active, else the oldest active admin (no owner yet, or a blocked owner,
// such as one who left the organisation). ErrNoUser when nobody active is
// an admin. Owner notifications (push, the Inbox, events,
// reauthorisation) and the skills hub's fallback reviewer go to them, so a
// blocked owner never gets them.
func (s *Service) ActiveOwnerOrAdmin(ctx context.Context) (*User, error) {
	u, err := s.Owner(ctx)
	if err == nil && u.Status == StatusActive {
		return u, nil
	}
	if err != nil && !errors.Is(err, ErrNoUser) {
		return nil, err
	}
	u, err = scanUser(s.db.QueryRowContext(ctx,
		`SELECT `+userColumns+` FROM users u WHERE u.role = ? AND u.status = ? ORDER BY u.created_at, u.id LIMIT 1`,
		RoleAdmin, StatusActive))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoUser
	}
	if err != nil {
		return nil, err
	}
	return u, nil
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
// oldest active admin, when no owner email is configured (ownerEmail, the
// -owner-email). With an owner email it picks nobody: a stored email may
// never have been verified, so that person's next verified sign-in claims
// the role instead (UpsertClerkUser). An install with an owner is left
// alone, and so is one without an active admin (the next bootstrap
// sign-in claims the role). It returns the owner, or ErrNoUser when there
// still is none, and audits a claim as system:startup. Every replica may
// run it at startup at the same time: the claim is conditional and the
// unique index refuses a second owner.
func (s *Service) EnsureOwner(ctx context.Context, ownerEmail string) (*User, error) {
	if u, err := s.Owner(ctx); !errors.Is(err, ErrNoUser) {
		return u, err
	}
	if strings.TrimSpace(ownerEmail) != "" {
		return nil, ErrNoUser
	}
	var id string
	err := s.db.QueryRowContext(ctx,
		`SELECT id FROM users WHERE role = ? AND status = ? ORDER BY created_at, id LIMIT 1`,
		RoleAdmin, StatusActive).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoUser
	}
	if err != nil {
		return nil, err
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
	_, err := s.TransferOwnershipWith(ctx, fromID, toID, TransferOptions{})
	return err
}

// TransferOptions are the extra revocations a transfer makes in its own
// transaction.
type TransferOptions struct {
	// RevokePrevious disables the previous owner's enrolled agents and
	// revokes their operator tokens.
	RevokePrevious bool
	// RevokeOtherOperatorTokens revokes every operator token that is not
	// the new owner's (personal sign-in, where only the owner signs in).
	RevokeOtherOperatorTokens bool
}

// TransferResult counts what a transfer revoked.
type TransferResult struct {
	AgentsDisabled        int64
	OperatorTokensRevoked int64
}

// TransferOwnershipWith is TransferOwnership plus the revocations in opts,
// all in one transaction with the move and its audit rows.
func (s *Service) TransferOwnershipWith(ctx context.Context, fromID, toID string, opts TransferOptions) (TransferResult, error) {
	var out TransferResult
	if fromID == "" || toID == "" {
		return out, ErrNoUser
	}
	if fromID == toID {
		return out, ErrAlreadyOwner
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
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
		return out, err
	}
	fromOwner, toFound, toStatus := false, false, ""
	var fromName, toName, toRole string
	for rows.Next() {
		var id, username, role, status string
		var owner int64
		if err := rows.Scan(&id, &username, &role, &owner, &status); err != nil {
			rows.Close()
			return out, err
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
		return out, err
	}
	rows.Close()
	if !fromOwner {
		return out, ErrNotOwner
	}
	if !toFound {
		return out, ErrNoUser
	}
	if toStatus != StatusActive {
		return out, ErrUserBlocked
	}
	now := time.Now().UnixMilli()
	// Clear first: the unique index allows one owner at any moment.
	res, err := tx.ExecContext(ctx, `UPDATE users SET is_owner = 0, updated_at = ? WHERE id = ? AND is_owner = 1`, now, fromID)
	if err != nil {
		return out, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return out, err
	} else if n != 1 {
		return out, ErrNotOwner
	}
	res, err = tx.ExecContext(ctx, `UPDATE users SET is_owner = 1, role = ?, updated_at = ? WHERE id = ? AND status = ?`,
		RoleAdmin, now, toID, StatusActive)
	if err != nil {
		return out, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return out, err
	} else if n != 1 {
		return out, ErrUserBlocked
	}
	if err := auditOwnerMove(ctx, tx, "user:"+fromID, "transfer", fromID, fromName, toID, toName, toRole); err != nil {
		return out, err
	}
	if opts.RevokePrevious {
		agents, tokens, err := revokeCredentialsTx(ctx, tx, fromID, now)
		if err != nil {
			return out, err
		}
		if err := revokeLocalConnectionsTx(ctx, tx, `user_id=?`, []any{fromID}, now); err != nil {
			return out, err
		}
		out.AgentsDisabled, out.OperatorTokensRevoked = agents, tokens
		if err := auditTx(ctx, tx, "user.credentials_revoked", "user:"+fromID, "previous_owner",
			fmt.Sprintf("%s: %d agents disabled, %d operator tokens revoked", fromName, agents, tokens),
			map[string]any{"user_id": fromID, "agents_disabled": agents, "operator_tokens_revoked": tokens}); err != nil {
			return out, err
		}
	}
	if opts.RevokeOtherOperatorTokens {
		res, err := tx.ExecContext(ctx, `UPDATE operator_tokens SET revoked_at = ? WHERE user_id <> ? AND revoked_at IS NULL`, now, toID)
		if err != nil {
			return out, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return out, err
		}
		out.OperatorTokensRevoked += n
		if n > 0 {
			if err := auditTx(ctx, tx, "operator_token.revoke", "user:"+fromID, "personal_owner_transfer",
				fmt.Sprintf("%d operator tokens of people other than the new owner %s revoked", n, toName),
				map[string]any{"owner_user_id": toID, "operator_tokens_revoked": n}); err != nil {
				return out, err
			}
		}
	}
	return out, tx.Commit()
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
// sessions and stops their agent and operator tokens, then disable their
// agents, revoke their operator tokens and delete their push subscriptions
// (a notification tap needs no session), all audited as
// system:clerk-sync. The row and the owner role stay; an admin unblocks a
// mistaken departure in the dashboard, and a host operator moves the role
// with ForceTransferOwnership. It reports whether it changed anything (an
// owner already blocked is left alone). ErrNotOwner: id is active but no
// longer the owner (ownership moved since the caller looked), so the
// caller blocks them as anyone else. ErrLastAdmin: nobody else is an
// active admin, so it changes nothing (a partial member list must not lock
// everyone out); the caller warns.
func (s *Service) SuspendDepartedOwner(ctx context.Context, id string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	var username, status string
	var owner int64
	err = tx.QueryRowContext(ctx, `SELECT username, status, COALESCE(is_owner,0) FROM users WHERE id = ?`+s.db.Dialect.ForUpdate(),
		id).Scan(&username, &status, &owner)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrNoUser
	}
	if err != nil {
		return false, err
	}
	if status != StatusActive {
		return false, nil
	}
	if owner != 1 {
		return false, ErrNotOwner
	}
	if err := ensureOtherActiveAdmin(ctx, tx, s.db.Dialect, id); err != nil {
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
	res, err := tx.ExecContext(ctx, `DELETE FROM push_subscriptions WHERE user_id = ?`, id)
	if err != nil {
		return false, err
	}
	pushes, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if err := auditTx(ctx, tx, "user.status", ActorClerkSync, "left_org",
		fmt.Sprintf("%s status=blocked: the owner left the organisation; sessions ended, %d agents disabled, %d operator tokens revoked, %d push subscriptions deleted; still the owner until `toolyard owner transfer`", username, agents, tokens, pushes),
		map[string]any{"user_id": id, "agents_disabled": agents, "operator_tokens_revoked": tokens, "push_subscriptions_deleted": pushes}); err != nil {
		return false, err
	}
	return true, tx.Commit()
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

// IsReservedAgentName reports a name that reads as toolyard itself
// ("toolyard", "Toolyard system", "toolyard:inbox", …): only its letters
// and digits, lower-cased, starting with "toolyard". The Inbox names
// toolyard as the sender of its own items, so no agent may take such a
// name (CreateEnrollment, CreateAgentWithToken), and AgentName marks one
// that already has it.
func IsReservedAgentName(name string) bool {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return strings.HasPrefix(b.String(), "toolyard")
}

// AgentName is how an agent is named on Inbox items and proposals: its
// name, whoever owns it, with the owner's name after it when the owner is
// not the install's owner (so another person's agent never passes for one
// of the owner's, or for toolyard), and quoted as "agent ..." when the name
// reads as toolyard (agents enrolled by other routes: an OAuth client, a
// host). "" when there is no such agent.
func (s *Service) AgentName(ctx context.Context, agentID string) string {
	var name, ownerLabel string
	var owner int64
	err := s.db.QueryRowContext(ctx,
		`SELECT a.name, COALESCE(NULLIF(u.display_name,''), u.username, ''), COALESCE(u.is_owner,0)
         FROM agents a LEFT JOIN users u ON u.id = a.owner_user WHERE a.id = ?`, agentID).Scan(&name, &ownerLabel, &owner)
	if err != nil {
		return ""
	}
	if IsReservedAgentName(name) {
		name = fmt.Sprintf("agent %q", name)
	}
	if owner != 1 && ownerLabel != "" {
		name += " · " + ownerLabel
	}
	return name
}

// ErrAmbiguousUser: a user reference names more than one account.
var ErrAmbiguousUser = errors.New("more than one user matches; use the user id")

// ResolveUserRef finds the one user a host command names: by exact id,
// else exact username, else email (EmailEqual: ASCII-only, ignoring case).
// ErrNoUser when nothing matches, ErrAmbiguousUser when more than one
// account has that email.
func (s *Service) ResolveUserRef(ctx context.Context, ref string) (*User, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, ErrNoUser
	}
	if u, err := s.GetUserByID(ctx, ref); err == nil {
		return u, nil
	} else if !errors.Is(err, ErrNoUser) {
		return nil, err
	}
	u, err := scanUser(s.db.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users u WHERE u.username = ?`, ref))
	if err == nil {
		return u, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT u.id, COALESCE(u.email,'') FROM users u WHERE u.email IS NOT NULL AND u.email != '' ORDER BY u.created_at, u.id`)
	if err != nil {
		return nil, err
	}
	var match []string
	for rows.Next() {
		var id, email string
		if err := rows.Scan(&id, &email); err != nil {
			rows.Close()
			return nil, err
		}
		if EmailEqual(email, ref) {
			match = append(match, id)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	switch len(match) {
	case 0:
		return nil, ErrNoUser
	case 1:
		return s.GetUserByID(ctx, match[0])
	default:
		return nil, ErrAmbiguousUser
	}
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
