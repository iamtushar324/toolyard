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

// MaxPendingContributorSources caps the sources one skill contributor may
// have waiting for approval at once. CreateSource enforces it under a
// cluster lock per contributor, so parallel creates (on any replica)
// cannot pass it.
const MaxPendingContributorSources = 3

// pendingSourcesOf counts the sources userID added that wait for approval.
func (s *Service) pendingSourcesOf(ctx context.Context, userID string) (int, error) {
	var n int64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM skill_sources WHERE created_by = ? AND approval = ?`,
		userID, string(ApprovalPending)).Scan(&n); err != nil {
		return 0, fmt.Errorf("skillhub: count waiting sources: %w", err)
	}
	return int(n), nil
}

// TooManyPendingMessage is the refusal of a contributor's create past
// MaxPendingContributorSources.
func TooManyPendingMessage(n int) string {
	return fmt.Sprintf("you already have %d sources waiting for an admin's approval; wait for a decision, or delete one", n)
}

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
// waits for approval (or was rejected: an admin may still approve it on
// the Skills page). Approve makes the source live (approval "approved",
// the source enabled), bound to sha, the commit the approver saw
// (approveSource). Reject disables the source, with note as the reason.
// A source that needs no approval, or one already approved, is
// ErrConflict.
func (s *Service) DecideApproval(ctx context.Context, by, id string, approve bool, note, sha string) (*Source, error) {
	return s.decideApproval(ctx, by, id, nil, approve, note, ApprovalBinding{SHA: sha})
}

// DecideApprovalFrom is DecideApproval for a source still in approval
// state from: any other state is ErrConflict. The Inbox decides with
// ApprovalPending, so a late answer to the question never approves a
// source an admin rejected on the Skills page meanwhile, and with the
// question's binding (b), so it applies only to the waiting version, or
// the settings, the question showed.
func (s *Service) DecideApprovalFrom(ctx context.Context, by, id string, from Approval, approve bool, note string, b ApprovalBinding) (*Source, error) {
	return s.decideApproval(ctx, by, id, &from, approve, note, b)
}

func (s *Service) decideApproval(ctx context.Context, by, id string, from *Approval, approve bool, note string, b ApprovalBinding) (*Source, error) {
	r, err := s.getSourceRow(ctx, s.db, id)
	if err != nil {
		return nil, err
	}
	switch {
	case r.Approval == ApprovalNone:
		return nil, fmt.Errorf("%w: source %s was added by an admin: there is nothing to approve", ErrConflict, r.Slug)
	case from != nil && r.Approval != *from:
		return nil, fmt.Errorf("%w: source %s is %s now, not %s: it was decided meanwhile", ErrConflict, r.Slug, r.Approval, *from)
	case r.Approval == ApprovalApproved:
		return nil, fmt.Errorf("%w: source %s is approved already", ErrConflict, r.Slug)
	case r.Approval == ApprovalRejected && !approve:
		return nil, fmt.Errorf("%w: source %s is rejected already", ErrConflict, r.Slug)
	}
	if approve {
		b.SHA = strings.ToLower(strings.TrimSpace(b.SHA))
		return s.approveSource(ctx, by, r, b)
	}
	note = strings.TrimSpace(note)
	if len(note) > maxApprovalNote {
		note = note[:maxApprovalNote]
	}
	now := s.now().UnixMilli()
	res, err := s.db.ExecContext(ctx, `UPDATE skill_sources SET approval = ?, approval_note = ?, approval_by = ?, enabled = 0,
		updated_at = `+bumpUpdatedAt+` WHERE id = ? AND approval = ?`, string(ApprovalRejected), note, by, now, now, id, string(r.Approval))
	if err != nil {
		return nil, fmt.Errorf("skillhub: decide approval: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, fmt.Errorf("%w: source %s was decided meanwhile", ErrConflict, r.Slug)
	}
	s.log.Info("skillhub: contributor source decided", "source", id, "approved", false, "by", by)
	s.changed(ctx, id)
	return s.GetSource(ctx, id)
}

// approveSource approves a contributor's source, bound to b, what the
// approver saw (on the Inbox question, or on the source page): the commit
// b.SHA and, when b names them, the waiting version b.Version and the
// settings b.Config. Nothing goes live that the approver did not see.
//
//   - A waiting version that matches b (built with the source's current
//     settings) is made live first, while agents still see nothing of the
//     source; the approval then serves exactly it.
//   - Another waiting version of a review-mode source becomes a normal
//     review question once the source is approved: only an admin makes it
//     live. Of an automatic or pinned source, the next check would make
//     it live: the approval is refused (ErrConflict).
//   - Otherwise a live version that does not match b is refused, and so
//     is an automatic or pinned source that synced nothing yet, or whose
//     live version was built with settings it no longer has (its next
//     sync would make an unseen version live).
//
// The approval is stored only while the source still has the live and
// waiting versions this checked, so a sync in between cannot slip in a
// commit nobody saw: that is ErrConflict too, and the admin looks again.
func (s *Service) approveSource(ctx context.Context, by string, r *sourceRow, b ApprovalBinding) (*Source, error) {
	sha := b.SHA
	version := func(id string) (*versionRow, error) {
		if id == "" {
			return nil, nil
		}
		v, err := s.getVersionRow(ctx, s.db, r.ID, id)
		if errors.Is(err, ErrNotFound) {
			return nil, nil
		}
		return v, err
	}
	waiting, err := version(r.PendingVersionID)
	if err != nil {
		return nil, err
	}
	live, err := version(r.ActiveVersionID)
	if err != nil {
		return nil, err
	}
	var head *versionRow
	if waiting == nil && r.Mode != ModeReview {
		if head, err = s.nextVersion(ctx, r); err != nil {
			return nil, err
		}
	}
	// bound reports a version that is what the approver saw.
	bound := func(v *versionRow) bool {
		return v != nil && sha != "" && strings.EqualFold(v.SHA, sha) && (b.Version == "" || b.Version == v.ID) &&
			(b.Config == "" || b.Config == v.config)
	}
	match := waiting != nil && waiting.config == r.configHash() && bound(waiting)
	switch {
	case match:
	case waiting != nil && r.Mode != ModeReview && strings.EqualFold(waiting.SHA, sha):
		return nil, fmt.Errorf("%w: source %s would serve commit %s with other settings (folder, excludes or limits) than the approval showed; look at it on the Skills page and approve that",
			ErrConflict, r.Slug, reviewShort(waiting.SHA))
	case waiting != nil && r.Mode != ModeReview:
		return nil, fmt.Errorf("%w: source %s would serve commit %s, not %s, the commit that was approved; look at it on the Skills page and approve that",
			ErrConflict, r.Slug, reviewShort(waiting.SHA), approvedLabel(sha))
	case r.ActiveVersionID != "" && !strings.EqualFold(r.ActiveSHA, sha):
		return nil, fmt.Errorf("%w: source %s serves commit %s, not %s, the commit that was approved; look at it on the Skills page and approve that",
			ErrConflict, r.Slug, reviewShort(r.ActiveSHA), approvedLabel(sha))
	case live != nil && !bound(live):
		return nil, fmt.Errorf("%w: source %s serves commit %s with other settings than the approval showed; look at it on the Skills page and approve that",
			ErrConflict, r.Slug, reviewShort(live.SHA))
	case waiting == nil && r.Mode != ModeReview:
		if err := liveApproval(r, live, head); err != nil {
			return nil, err
		}
	}
	active, pending := r.ActiveVersionID, r.PendingVersionID
	if match {
		if err := s.Activate(ctx, by, r.ID, waiting.ID); err != nil {
			return nil, fmt.Errorf("skillhub: approve source %s: make commit %s live: %w", r.Slug, reviewShort(waiting.SHA), err)
		}
		active, pending = waiting.ID, ""
	}
	now := s.now().UnixMilli()
	res, err := s.db.ExecContext(ctx, `UPDATE skill_sources SET approval = ?, approval_note = '', approval_by = ?, enabled = 1,
		updated_at = `+bumpUpdatedAt+` WHERE id = ? AND approval = ? AND active_version_id = ? AND pending_version_id = ?`,
		string(ApprovalApproved), by, now, now, r.ID, string(r.Approval), active, pending)
	if err != nil {
		return nil, fmt.Errorf("skillhub: decide approval: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, fmt.Errorf("%w: source %s was decided, or synced another commit, meanwhile; look again", ErrConflict, r.Slug)
	}
	s.log.Info("skillhub: contributor source decided", "source", r.ID, "approved", true, "by", by, "sha", sha)
	if waiting != nil && !match {
		// Review mode, another commit: a normal review question, which
		// the source no longer skips now that it is approved.
		src := r.Source
		src.Approval = ApprovalApproved
		s.notifyReview(ctx, &sourceRow{Source: src}, waiting)
	}
	if !r.Enabled {
		s.pokePoller()
	}
	s.changed(ctx, r.ID)
	return s.GetSource(ctx, r.ID)
}

// nextVersion is the version of what a contributor's automatic or pinned
// source would serve next with its current settings: its pinned commit,
// else the branch head it last saw (none since its branch, pin or settings
// changed: every such change clears head_sha, and only a check of the
// current branch records it again). nil when that version does not exist
// (yet).
func (s *Service) nextVersion(ctx context.Context, r *sourceRow) (*versionRow, error) {
	target := r.headSHA
	if r.Mode == ModePinned {
		target = r.PinnedSHA
	}
	if target == "" {
		return nil, nil
	}
	return s.findVersion(ctx, r.ID, target, r.configHash())
}

// liveApproval checks the approval of a contributor's automatic or pinned
// source that has nothing waiting, whose live version (nil: none) would
// stay live. After the approval its syncs make versions of its current
// branch (or pin) and settings live without asking, so the live version
// must be what it would serve next (next, nextVersion), or that version
// must exist as retired or rejected, which no sync makes live (an
// automatic source only), with the live one built with the current
// settings. Otherwise the refusal says when approving works: after the
// next sync (it builds, or stages again, what it would serve), or after a
// new commit or a settings change (the version of the head failed, or was
// rejected or retired with no current live version).
func liveApproval(r *sourceRow, live, next *versionRow) error {
	cfg := r.configHash()
	current := live != nil && live.config == cfg
	target := r.headSHA
	if r.Mode == ModePinned {
		target = r.PinnedSHA
	}
	switch {
	case target == "" || next == nil || next.Status == VersionStaged:
		return fmt.Errorf("%w: source %s has no version of its current branch and settings to approve yet; approve it after its next sync", ErrConflict, r.Slug)
	case current && strings.EqualFold(live.SHA, target):
		return nil
	case next.Status == VersionFailed:
		return fmt.Errorf("%w: the version of source %s's commit %s failed to build; approve it after a new commit or a settings change", ErrConflict, r.Slug, reviewShort(target))
	case current && r.Mode == ModeAuto && (next.Status == VersionRetired || next.Status == VersionRejected):
		return nil
	}
	return fmt.Errorf("%w: source %s has no version of its current branch and settings to approve: the one of commit %s is %s; approve it after a new commit or a settings change",
		ErrConflict, r.Slug, reviewShort(target), next.Status)
}

// approvedLabel names the commit an approval was given for.
func approvedLabel(sha string) string {
	if sha == "" {
		return "none (no commit was named)"
	}
	return reviewShort(sha)
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
