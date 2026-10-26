package skillhub

// Sharing controls and skill contributors (docs/skills-hub.md "Sharing" and
// "Contributors").
//
// Who sees a skill is decided in one place, srcIndex.skillVisible
// (index.go), which every read path asks; this file stores what it reads:
// each skill's own sharing setting (skill_sharing, migration 0073), a
// contributor source's approval (skill_sources.approval) and the list of
// skill contributors (skill_contributors, migration 0074). Every change
// moves the source's updated_at and is published on the cluster topic, so
// every node's index reloads it within moments.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	_ SharingAdmin      = (*Service)(nil)
	_ ContributorsAdmin = (*Service)(nil)
)

// maxApprovalNote caps a rejection's reason.
const maxApprovalNote = 2000

// skillShares reads a source's per-skill sharing settings, by skill path.
func (s *Service) skillShares(ctx context.Context, sourceID string) (map[string]SkillSharing, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT skill_path, mode, members_json, blocked_json FROM skill_sharing WHERE source_id = ?`, sourceID)
	if err != nil {
		return nil, fmt.Errorf("skillhub: read skill sharing: %w", err)
	}
	defer rows.Close()
	out := map[string]SkillSharing{}
	for rows.Next() {
		var p, mode, members, blocked string
		if err := rows.Scan(&p, &mode, &members, &blocked); err != nil {
			return nil, fmt.Errorf("skillhub: read skill sharing: %w", err)
		}
		sh := SkillSharing{Mode: Visibility(mode)}
		_ = json.Unmarshal([]byte(members), &sh.Members)
		_ = json.Unmarshal([]byte(blocked), &sh.Blocked)
		out[p] = sh
	}
	return out, rows.Err()
}

// normalizeSharing checks a skill's sharing setting: mode inherit ("" or
// "inherit"), everyone, members or nobody; members only with mode members.
func normalizeSharing(sh SkillSharing) (SkillSharing, error) {
	switch strings.TrimSpace(string(sh.Mode)) {
	case "", "inherit":
		sh.Mode = VisibleInherit
	case string(VisibleEveryone), string(VisibleMembers), string(VisibleNobody):
		sh.Mode = Visibility(strings.TrimSpace(string(sh.Mode)))
	default:
		return sh, fmt.Errorf("%w: mode %q: use inherit, everyone, members or nobody", ErrInvalid, sh.Mode)
	}
	var err error
	if sh.Members, err = cleanUserList(sh.Members, "members"); err != nil {
		return sh, err
	}
	if sh.Blocked, err = cleanUserList(sh.Blocked, "blocked"); err != nil {
		return sh, err
	}
	if sh.Mode != VisibleMembers {
		sh.Members = nil
	}
	if len(sh.Members) == 0 {
		sh.Members = nil
	}
	if len(sh.Blocked) == 0 {
		sh.Blocked = nil
	}
	return sh, nil
}

// SetSkillSharing sets the sharing of the skill at skillPath of a source,
// kept by source and path so it survives new versions. A zero setting (mode
// inherit, nobody blocked) clears it. Setting one needs the skill in the
// active version; clearing works for any path. It returns the setting it
// replaced (zero when there was none).
func (s *Service) SetSkillSharing(ctx context.Context, by, id, skillPath string, sh SkillSharing) (SkillSharing, error) {
	r, err := s.getSourceRow(ctx, s.db, id)
	if err != nil {
		return SkillSharing{}, err
	}
	skillPath = strings.Trim(strings.TrimSpace(skillPath), "/")
	if sh, err = normalizeSharing(sh); err != nil {
		return SkillSharing{}, err
	}
	shares, err := s.skillShares(ctx, id)
	if err != nil {
		return SkillSharing{}, err
	}
	before := shares[skillPath]
	if !sh.IsZero() {
		var n int64
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM skill_entries WHERE version_id = ? AND path = ?`,
			r.ActiveVersionID, skillPath).Scan(&n); err != nil {
			return SkillSharing{}, fmt.Errorf("skillhub: set sharing: %w", err)
		}
		if n == 0 {
			return SkillSharing{}, fmt.Errorf("%w: source %s serves no skill at %q", ErrNotFound, r.Slug, skillPath)
		}
	}
	now := s.now().UnixMilli()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SkillSharing{}, fmt.Errorf("skillhub: set sharing: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if sh.IsZero() {
		_, err = tx.ExecContext(ctx, `DELETE FROM skill_sharing WHERE source_id = ? AND skill_path = ?`, id, skillPath)
	} else {
		members, _ := json.Marshal(nonNil(sh.Members))
		blocked, _ := json.Marshal(nonNil(sh.Blocked))
		_, err = tx.ExecContext(ctx, `INSERT INTO skill_sharing (source_id, skill_path, mode, members_json, blocked_json, set_by, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT (source_id, skill_path) DO UPDATE SET mode = excluded.mode,
			members_json = excluded.members_json, blocked_json = excluded.blocked_json, set_by = excluded.set_by,
			updated_at = excluded.updated_at`, id, skillPath, string(sh.Mode), string(members), string(blocked), by, now)
	}
	if err != nil {
		return SkillSharing{}, fmt.Errorf("skillhub: set sharing: %w", err)
	}
	// The source's updated_at tells every node's index to reload it; it
	// always moves forward, even past a write in the same millisecond.
	if _, err := tx.ExecContext(ctx, `UPDATE skill_sources SET updated_at = `+bumpUpdatedAt+` WHERE id = ?`, now, now, id); err != nil {
		return SkillSharing{}, fmt.Errorf("skillhub: set sharing: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return SkillSharing{}, fmt.Errorf("skillhub: set sharing: %w", err)
	}
	s.log.Info("skillhub: skill sharing changed", "source", id, "skill", skillPath, "mode", string(sh.Mode), "by", by)
	s.changed(ctx, id)
	return before, nil
}

// bumpUpdatedAt is an updated_at value (arguments: now, now) that always
// moves forward: now, or one past the stored value when that is not
// older. Every node's index reloads a source whose updated_at moved.
const bumpUpdatedAt = `CASE WHEN updated_at >= ? THEN updated_at + 1 ELSE ? END`

// DecideApproval approves or rejects a skill contributor's source that
// waits for approval (or was rejected: an admin may still approve it).
// Approve makes the source live: approval "approved" and the source
// enabled. Its waiting version (review mode) goes live with it only when
// that version is at sha, the commit the approver saw on the approval
// question; any other waiting version becomes a normal review question
// for an admin, so nothing goes live that the approver did not see.
// Reject disables the source, with note as the reason. A source that
// needs no approval, or one already approved, is ErrConflict.
func (s *Service) DecideApproval(ctx context.Context, by, id string, approve bool, note, sha string) (*Source, error) {
	r, err := s.getSourceRow(ctx, s.db, id)
	if err != nil {
		return nil, err
	}
	switch {
	case r.Approval == ApprovalNone:
		return nil, fmt.Errorf("%w: source %s was added by an admin: there is nothing to approve", ErrConflict, r.Slug)
	case r.Approval == ApprovalApproved:
		return nil, fmt.Errorf("%w: source %s is approved already", ErrConflict, r.Slug)
	case r.Approval == ApprovalRejected && !approve:
		return nil, fmt.Errorf("%w: source %s is rejected already", ErrConflict, r.Slug)
	}
	note = strings.TrimSpace(note)
	if len(note) > maxApprovalNote {
		note = note[:maxApprovalNote]
	}
	now := s.now().UnixMilli()
	var res sql.Result
	if approve {
		res, err = s.db.ExecContext(ctx, `UPDATE skill_sources SET approval = ?, approval_note = '', approval_by = ?, enabled = 1,
			updated_at = `+bumpUpdatedAt+` WHERE id = ? AND approval = ?`, string(ApprovalApproved), by, now, now, id, string(r.Approval))
	} else {
		res, err = s.db.ExecContext(ctx, `UPDATE skill_sources SET approval = ?, approval_note = ?, approval_by = ?, enabled = 0,
			updated_at = `+bumpUpdatedAt+` WHERE id = ? AND approval = ?`, string(ApprovalRejected), note, by, now, now, id, string(r.Approval))
	}
	if err != nil {
		return nil, fmt.Errorf("skillhub: decide approval: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, fmt.Errorf("%w: source %s was decided meanwhile", ErrConflict, r.Slug)
	}
	s.log.Info("skillhub: contributor source decided", "source", id, "approved", approve, "by", by)
	if approve && r.PendingVersionID != "" {
		s.approveWaitingVersion(ctx, by, r, sha)
	}
	if approve && !r.Enabled {
		s.pokePoller()
	}
	s.changed(ctx, id)
	return s.GetSource(ctx, id)
}

// approveWaitingVersion settles an approved source's waiting version: live
// when it is at sha (the commit the approver saw), else a normal review
// question, which the source no longer skips now that it is approved.
func (s *Service) approveWaitingVersion(ctx context.Context, by string, r *sourceRow, sha string) {
	v, err := s.getVersionRow(ctx, s.db, r.ID, r.PendingVersionID)
	if err != nil {
		s.log.Warn("skillhub: reading an approved source's waiting version failed", "source", r.ID, "err", err.Error())
		return
	}
	if sha != "" && strings.EqualFold(v.SHA, sha) {
		if err := s.Activate(ctx, by, r.ID, v.ID); err != nil && !errors.Is(err, ErrConflict) {
			s.log.Warn("skillhub: making an approved source's waiting version live failed", "source", r.ID, "err", err.Error())
		}
		return
	}
	src := r.Source
	src.Approval = ApprovalApproved
	s.notifyReview(ctx, &sourceRow{Source: src}, v)
}

// Contributors lists the skill contributors, oldest first.
func (s *Service) Contributors(ctx context.Context) ([]Contributor, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT user_id, added_by, added_at FROM skill_contributors ORDER BY added_at, user_id`)
	if err != nil {
		return nil, fmt.Errorf("skillhub: list contributors: %w", err)
	}
	defer rows.Close()
	out := []Contributor{}
	for rows.Next() {
		var c Contributor
		var at int64
		if err := rows.Scan(&c.UserID, &c.AddedBy, &at); err != nil {
			return nil, fmt.Errorf("skillhub: list contributors: %w", err)
		}
		c.AddedAt = time.UnixMilli(at).UTC()
		out = append(out, c)
	}
	return out, rows.Err()
}

// IsContributor reports whether userID is a skill contributor.
func (s *Service) IsContributor(ctx context.Context, userID string) (bool, error) {
	if strings.TrimSpace(userID) == "" {
		return false, nil
	}
	var n int64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM skill_contributors WHERE user_id = ?`, userID).Scan(&n); err != nil {
		return false, fmt.Errorf("skillhub: read contributors: %w", err)
	}
	return n > 0, nil
}

// SetContributor adds (on) or removes userID as a skill contributor. Both
// are idempotent. Removing one keeps their sources: only admins can change
// them then.
func (s *Service) SetContributor(ctx context.Context, by, userID string, on bool) error {
	userID = strings.TrimSpace(userID)
	if userID == "" || len(userID) > 200 {
		return fmt.Errorf("%w: a user ID is required", ErrInvalid)
	}
	var err error
	if on {
		_, err = s.db.ExecContext(ctx, `INSERT INTO skill_contributors (user_id, added_by, added_at) VALUES (?, ?, ?)
			ON CONFLICT (user_id) DO NOTHING`, userID, by, s.now().UnixMilli())
	} else {
		_, err = s.db.ExecContext(ctx, `DELETE FROM skill_contributors WHERE user_id = ?`, userID)
	}
	if err != nil {
		return fmt.Errorf("skillhub: set contributor: %w", err)
	}
	s.log.Info("skillhub: skill contributor changed", "user", userID, "on", on, "by", by)
	return nil
}
