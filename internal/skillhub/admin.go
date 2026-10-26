package skillhub

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// Ceilings on what an admin may set.
const (
	maxPollSeconds   = 86400
	ceilFileBytes    = 16 << 20
	ceilTotalBytes   = 1 << 30
	ceilSkills       = 20000
	ceilFiles        = 200000
	maxMembers       = 1000
	maxSampleSkills  = 20
	defaultVersions  = 20
	maxVersionsList  = 100
	maxWebhookBody   = 25 << 20
	webhookSecretLen = 32
)

// Inspect reads what toolyard can see of a repository: owner, name,
// visibility, default branch and branches, and the branch and folder of a
// tree or blob URL (a branch name with "/" is matched against the branch
// list). Without a token, a private or missing repository answers with
// NeedsToken set.
func (s *Service) Inspect(ctx context.Context, rawURL, token string) (*RepoInfo, error) {
	return s.inspect(ctx, rawURL, token, false)
}

// InspectAnonymous is Inspect with no token at all, not even the default
// one: what a skill contributor without their own credential may read.
// A private repository answers with NeedsToken set.
func (s *Service) InspectAnonymous(ctx context.Context, rawURL string) (*RepoInfo, error) {
	return s.inspect(ctx, rawURL, "", true)
}

func (s *Service) inspect(ctx context.Context, rawURL, token string, anonymous bool) (*RepoInfo, error) {
	ref, rest, err := parseRepoURL(rawURL)
	if err != nil {
		return nil, err
	}
	token = strings.TrimSpace(token)
	gh := s.newClient(token)
	if token == "" && !anonymous {
		gh = s.publicClient()
	}
	repo, err := gh.repo(ctx, ref.Owner, ref.Repo)
	if isGHStatus(err, 404) && token == "" {
		// Private or missing: the dashboard asks for a token.
		return &RepoInfo{RepoRef: ref, NeedsToken: true}, nil
	}
	if err != nil {
		return nil, s.inspectError(err, ref, token != "")
	}
	info := &RepoInfo{RepoRef: ref, Private: repo.Private, DefaultBranch: repo.DefaultBranch, HTMLURL: repo.HTMLURL}
	if repo.Owner.Login != "" {
		info.Owner = repo.Owner.Login
	}
	if repo.Name != "" {
		info.Repo = repo.Name
	}
	info.Branches, info.BranchesMore, err = gh.branches(ctx, info.Owner, info.Repo)
	if err != nil {
		return nil, s.inspectError(err, ref, token != "")
	}
	info.Branch, info.Path = "", ""
	if rest != "" {
		info.Branch, info.Path = splitTreeRest(rest, info.Branches)
	}
	return info, nil
}

// inspectError explains a failed repository read.
func (s *Service) inspectError(err error, ref RepoRef, withToken bool) error {
	switch {
	case isGHStatus(err, 404) && !withToken:
		return fmt.Errorf("%w (%s/%s)", ErrNeedsToken, ref.Owner, ref.Repo)
	case isGHStatus(err, 404):
		return fmt.Errorf("%w: %s/%s is missing or the token cannot read it", ErrNeedsToken, ref.Owner, ref.Repo)
	case isGHStatus(err, 401):
		return fmt.Errorf("%w: GitHub rejected the token (the one given, or the default token in Skills, GitHub access)", ErrInvalid)
	}
	return err
}

// prepareSource turns an admin's input into a source row and the token it
// reads with. It reads the repository, so a source that cannot be read is
// refused before it is stored.
func (s *Service) prepareSource(ctx context.Context, by string, in SourceInput) (*sourceRow, *ghClient, error) {
	ref, rest, err := parseRepoURL(in.URL)
	if err != nil {
		return nil, nil, err
	}
	token := strings.TrimSpace(in.Token)
	auth := in.Auth
	if auth == "" {
		auth = AuthNone
		if token != "" {
			auth = AuthToken
		}
	}
	var gh *ghClient
	switch auth {
	case AuthNone:
		if token != "" {
			return nil, nil, fmt.Errorf("%w: a token was given but auth is none", ErrInvalid)
		}
		gh = s.publicClient()
		if in.PendingApproval {
			// A skill contributor never reads with the admins' default
			// token: only a public repository is readable without their
			// own token or GitHub connection.
			gh = s.newClient("")
		}
	case AuthToken:
		if token == "" {
			return nil, nil, fmt.Errorf("%w: auth token needs a token", ErrInvalid)
		}
		gh = s.newClient(token)
	case AuthGitHub:
		if gh, err = s.userClient(ctx, by); err != nil {
			return nil, nil, err
		}
	default:
		return nil, nil, fmt.Errorf("%w: auth %q: use none, token or github", ErrInvalid, in.Auth)
	}
	repo, err := gh.repo(ctx, ref.Owner, ref.Repo)
	if err != nil {
		return nil, nil, s.inspectError(err, ref, auth != AuthNone)
	}
	r := &sourceRow{Source: Source{Kind: "github", Owner: ref.Owner, Repo: ref.Repo, Auth: auth, CreatedBy: by}}
	if repo.Owner.Login != "" {
		r.Owner = repo.Owner.Login
	}
	if repo.Name != "" {
		r.Repo = repo.Name
	}
	r.Branch, r.Path = strings.TrimSpace(in.Branch), in.Path
	if rest != "" && (r.Branch == "" || strings.TrimSpace(r.Path) == "") {
		branches, _, err := gh.branches(ctx, r.Owner, r.Repo)
		if err != nil {
			return nil, nil, s.inspectError(err, ref, auth != AuthNone)
		}
		b, p := splitTreeRest(rest, branches)
		if r.Branch == "" {
			r.Branch = b
		}
		if strings.TrimSpace(r.Path) == "" && r.Branch == b {
			r.Path = p
		}
	}
	if r.Branch == "" {
		r.Branch = repo.DefaultBranch
	}
	if err := s.applyInput(ctx, r, in); err != nil {
		return nil, nil, err
	}
	return r, gh, nil
}

// applyInput validates and fills in everything but the repository and
// credential.
func (s *Service) applyInput(ctx context.Context, r *sourceRow, in SourceInput) error {
	var err error
	if r.Path, err = cleanFolder(r.Path); err != nil {
		return err
	}
	if r.Branch == "" {
		return fmt.Errorf("%w: a branch is required", ErrInvalid)
	}
	if r.Exclude, err = validateGlobs(in.Exclude); err != nil {
		return err
	}
	r.Mode = in.Mode
	if r.Mode == "" {
		r.Mode = ModeReview
		if s.TrustedOwner(ctx, r.Owner) {
			r.Mode = ModeAuto
		}
	}
	r.PinnedSHA = strings.ToLower(strings.TrimSpace(in.PinnedSHA))
	r.PollSeconds = in.PollSeconds
	r.Enabled = in.Enabled == nil || *in.Enabled
	r.Visibility, r.Members, r.Blocked, r.DefaultRuns = in.Visibility, in.Members, in.Blocked, in.DefaultRuns
	if in.Limits != nil {
		r.Limits = *in.Limits
	}
	if in.PendingApproval {
		// A skill contributor's source: review, or auto for a repository
		// owner an admin trusts (the owner GitHub answered with, not the
		// one in the URL); never pinned, never other limits.
		r.Approval = ApprovalPending
		switch {
		case r.Mode == ModePinned:
			return fmt.Errorf("%w: only an admin can pin a source", ErrInvalid)
		case r.Mode == ModeAuto && !s.TrustedOwner(ctx, r.Owner):
			return fmt.Errorf("%w: automatic updates are for trusted repository owners only; %s is not one", ErrInvalid, r.Owner)
		case in.Limits != nil:
			return fmt.Errorf("%w: only an admin can set a source's limits", ErrInvalid)
		}
	}
	return normalizeSource(r)
}

// normalizeSource checks a source's settings and fills in defaults.
func normalizeSource(r *sourceRow) error {
	if !validBranch(r.Branch) {
		return fmt.Errorf("%w: branch %q is not a valid branch name", ErrInvalid, r.Branch)
	}
	switch r.Mode {
	case ModeAuto, ModeReview:
	case ModePinned:
		if !shaRe.MatchString(r.PinnedSHA) {
			return fmt.Errorf("%w: mode pinned needs pinned_sha, a full 40-character commit SHA", ErrInvalid)
		}
	default:
		return fmt.Errorf("%w: mode %q: use auto, review or pinned", ErrInvalid, r.Mode)
	}
	if r.PinnedSHA != "" && !shaRe.MatchString(r.PinnedSHA) {
		return fmt.Errorf("%w: pinned_sha must be a full commit SHA", ErrInvalid)
	}
	switch {
	case r.PollSeconds == 0:
		r.PollSeconds = DefaultPollSeconds
	case r.PollSeconds < MinPollSeconds:
		r.PollSeconds = MinPollSeconds
	case r.PollSeconds > maxPollSeconds:
		r.PollSeconds = maxPollSeconds
	}
	switch r.Visibility {
	case "":
		r.Visibility = VisibleEveryone
	case VisibleEveryone, VisibleMembers, VisibleNobody:
	default:
		return fmt.Errorf("%w: visibility %q: use everyone, members or nobody", ErrInvalid, r.Visibility)
	}
	var err error
	if r.Members, err = cleanUserList(r.Members, "members"); err != nil {
		return err
	}
	if r.Blocked, err = cleanUserList(r.Blocked, "blocked"); err != nil {
		return err
	}
	switch r.DefaultRuns {
	case "":
		r.DefaultRuns = RunsAnywhere
	case RunsAnywhere, RunsHost:
	default:
		return fmt.Errorf("%w: default_runs %q: use anywhere or host", ErrInvalid, r.DefaultRuns)
	}
	l := r.Limits
	if l.MaxFileBytes < 0 || l.MaxTotalBytes < 0 || l.MaxSkills < 0 || l.MaxFiles < 0 ||
		l.MaxFileBytes > ceilFileBytes || l.MaxTotalBytes > ceilTotalBytes || l.MaxSkills > ceilSkills || l.MaxFiles > ceilFiles {
		return fmt.Errorf("%w: limits must be between 0 (the default) and %d bytes per file, %d bytes in total, %d skills, %d files",
			ErrInvalid, ceilFileBytes, ceilTotalBytes, ceilSkills, ceilFiles)
	}
	return nil
}

// cleanUserList trims, de-duplicates and caps a list of user IDs.
func cleanUserList(ids []string, field string) ([]string, error) {
	out := uniqueStrings(trimAll(ids))
	if len(out) > maxMembers {
		return nil, fmt.Errorf("%w: %s: at most %d people", ErrInvalid, field, maxMembers)
	}
	return out, nil
}

func trimAll(xs []string) []string {
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

// Preview is a dry run of a source: the commit it would serve, the skills
// found, warnings, a sample and sizes. Nothing is stored.
func (s *Service) Preview(ctx context.Context, by string, in SourceInput) (*Preview, error) {
	r, gh, err := s.prepareSource(ctx, by, in)
	if err != nil {
		return nil, err
	}
	r.Slug = strings.TrimSpace(in.Slug)
	if r.Slug == "" {
		r.Slug = slugify(r.Repo)
	}
	if reservedSlug(r.Slug) && strings.TrimSpace(in.Slug) == "" {
		r.Slug = slugCandidate(r.Slug, 2)
	}
	return s.previewRow(ctx, r, gh)
}

// PreviewSource is a dry run of a stored source with its own settings and
// credential (anonymous for a contributor's source without one). Nothing
// is stored.
func (s *Service) PreviewSource(ctx context.Context, id string) (*Preview, error) {
	r, err := s.getSourceRow(ctx, s.db, id)
	if err != nil {
		return nil, err
	}
	gh, err := s.clientFor(ctx, r)
	if err != nil {
		return nil, err
	}
	return s.previewRow(ctx, r, gh)
}

// previewRow is the dry run of r with gh: the commit it would serve, the
// skills found, warnings, a sample and sizes.
func (s *Service) previewRow(ctx context.Context, r *sourceRow, gh *ghClient) (*Preview, error) {
	var err error
	sha := r.PinnedSHA
	if r.Mode != ModePinned {
		if sha, _, _, err = gh.head(ctx, r.Owner, r.Repo, r.Branch, "", true); err != nil {
			return nil, s.repoError(err, r)
		}
	}
	ex, err := newExcluder(r.Exclude)
	if err != nil {
		return nil, err
	}
	fs, err := s.download(ctx, gh, r, sha, ex, r.Limits.effective())
	if err != nil {
		return nil, err
	}
	b, err := discover(fs, r.Path, r.rootName(), nil)
	if err != nil {
		return nil, err
	}
	p := &Preview{
		Source: r.Source, SHA: sha, SkillCount: len(b.skills), FileCount: len(b.files),
		TotalBytes: b.totalBytes, Warnings: nonNilWarnings(b.warnings), Sample: []SkillSummary{},
	}
	label := RepoLabel(r.Owner, r.Repo, sha)
	for _, sk := range b.skills {
		if len(p.Sample) == maxSampleSkills {
			break
		}
		runs := effectiveRuns("", sk.meta.runs, r.DefaultRuns)
		p.Sample = append(p.Sample, SkillSummary{
			URI: SkillURI(r.Slug, sk.path), Name: sk.meta.name, Description: sk.meta.description,
			Source: r.Slug, Path: sk.path, Repo: label, Commit: sha, Runs: runs,
		})
	}
	return p, nil
}

// CreateSource stores a new source. Its first check is due at once: the
// leader's poll loop runs it within moments, or call SyncNow for a sync on
// this node. A skill contributor's source (PendingApproval) is refused
// with ErrConflict when they already have MaxPendingContributorSources
// waiting: the count and the insert run under a cluster lock per
// contributor, so parallel creates on any replica cannot pass the cap.
func (s *Service) CreateSource(ctx context.Context, by string, in SourceInput) (*Source, error) {
	r, gh, err := s.prepareSource(ctx, by, in)
	if err != nil {
		return nil, err
	}
	r.ID = newID("src_")
	if gh.token != "" && r.Auth == AuthToken {
		if r.tokenSealed, err = s.seal(gh.token, tokenAAD(r.ID)); err != nil {
			return nil, err
		}
	}
	if in.PendingApproval {
		lctx, unlock, err := s.cluster.LockCtx(ctx, "skillhub:contributor:"+by)
		if err != nil {
			return nil, fmt.Errorf("skillhub: lock the sources of %s: %w", by, err)
		}
		defer unlock()
		ctx = lctx
		n, err := s.pendingSourcesOf(ctx, by)
		if err != nil {
			return nil, err
		}
		if n >= MaxPendingContributorSources {
			return nil, fmt.Errorf("%w: %s", ErrConflict, TooManyPendingMessage(n))
		}
	}
	if err := s.insertWithSlug(ctx, r, strings.TrimSpace(in.Slug)); err != nil {
		return nil, err
	}
	s.pokePoller()
	return s.GetSource(ctx, r.ID)
}

// insertWithSlug stores the source under the given slug, or the first
// free default one.
func (s *Service) insertWithSlug(ctx context.Context, r *sourceRow, slug string) error {
	if slug != "" {
		if len(slug) > maxSlugLen || !slugRe.MatchString(slug) {
			return fmt.Errorf("%w: slug %q: lowercase letters, digits and single hyphens, at most %d characters", ErrInvalid, slug, maxSlugLen)
		}
		if reservedSlug(slug) {
			return fmt.Errorf("%w: slug %q is reserved for toolyard's built-in skills; choose another", ErrConflict, slug)
		}
		r.Slug = slug
		if err := s.insertSource(ctx, r); err != nil {
			if store.IsUniqueViolation(err) {
				return fmt.Errorf("%w: slug %q is taken", ErrConflict, slug)
			}
			return fmt.Errorf("skillhub: store source: %w", err)
		}
		return nil
	}
	base := slugify(r.Repo)
	for n := 1; n <= 100; n++ {
		r.Slug = slugCandidate(base, n)
		if reservedSlug(r.Slug) {
			continue
		}
		err := s.insertSource(ctx, r)
		if err == nil {
			return nil
		}
		if !store.IsUniqueViolation(err) {
			return fmt.Errorf("skillhub: store source: %w", err)
		}
	}
	return fmt.Errorf("%w: no free slug for %q; give one", ErrConflict, base)
}

// seal seals a secret with secrets.key.
func (s *Service) seal(secret string, aad []byte) (string, error) {
	if s.cipher == nil {
		return "", errors.New("skillhub: this server has no cipher to store secrets")
	}
	sealed, err := s.cipher.Seal([]byte(secret), aad)
	if err != nil {
		return "", fmt.Errorf("skillhub: seal secret: %w", err)
	}
	return sealed, nil
}

// maxUpdateAttempts bounds UpdateSource's read-modify-write rounds when
// other writes (a sync's activation, a sharing change) move the source in
// between.
const maxUpdateAttempts = 5

// updateGuard is the part of a source the caller of UpdateSource decided
// on (who may change what, which open questions to close): a round after a
// write in between goes on only while it is unchanged.
type updateGuard struct {
	approval             Approval
	hidden, enabled      bool
	mode                 UpdateMode
	branch, path, pinned string
	exclude, limits      string
	auth                 AuthMode
	token                string // sealed
}

func guardOf(r *sourceRow) updateGuard {
	ex, _ := json.Marshal(nonNil(r.Exclude))
	lim, _ := json.Marshal(r.Limits)
	return updateGuard{approval: r.Approval, hidden: r.Hidden, enabled: r.Enabled, mode: r.Mode, branch: r.Branch, path: r.Path,
		pinned: r.PinnedSHA, exclude: string(ex), limits: string(lim), auth: r.Auth, token: r.tokenSealed}
}

// UpdateSource changes a source's settings. A change to what is fetched
// (branch, folder, excludes, limits, mode, pinned commit, credential)
// makes its next check due at once.
//
// A contributor's source that is not approved forgets its waiting version
// when its branch, pinned commit, folder, excludes or limits change.
//
// Only the fields the update names are written (the hide lock, for one,
// changes only when the update names it: the admin hide path), and only
// while the source is still as read (its updated_at); a write in between
// makes it read again, at most maxUpdateAttempts times. When the approval,
// the hide lock, the paused state, the mode, what is fetched (branch,
// folder, excludes, pinned commit, limits) or the credential (read access,
// token) changed in between, it answers ErrConflict instead: the caller's
// checks read the old source. With up.IfUpdatedAt set, the first read must
// be the source the caller checked (that updated_at), else ErrConflict.
func (s *Service) UpdateSource(ctx context.Context, by, id string, up SourceUpdate) (*Source, error) {
	var first *updateGuard
	due := false
	for attempt := 1; ; attempt++ {
		r, err := s.getSourceRow(ctx, s.db, id)
		if err != nil {
			return nil, err
		}
		g := guardOf(r)
		switch {
		case first == nil && !up.IfUpdatedAt.IsZero() && r.UpdatedAt.UnixMilli() != up.IfUpdatedAt.UnixMilli():
			return nil, fmt.Errorf("%w: source %s changed since it was read; look again and retry", ErrConflict, r.Slug)
		case first == nil:
			first = &g
		case g != *first:
			return nil, fmt.Errorf("%w: source %s changed meanwhile (its approval, hide lock, paused state, mode, what it fetches or its credential); look again and retry", ErrConflict, r.Slug)
		}
		read, fetched := r.UpdatedAt.UnixMilli(), fetchKey(r)
		if due, err = s.applyUpdate(r, up); err != nil {
			return nil, err
		}
		// A contributor's source that is not approved forgets its waiting
		// version when what it fetches changed: built from the old branch
		// or settings, it can no longer be shown or approved (the next
		// check stages the new one, and finds an older one again when the
		// settings come back). Also when this read saw none: a sync may
		// stage one (setPending moves no updated_at) before this write.
		dropWaiting := !r.Approval.Live() && fetchKey(r) != fetched
		ok, err := s.updateSourceSettings(ctx, r, up, read, due, dropWaiting)
		if err != nil {
			return nil, err
		}
		if ok {
			break
		}
		if attempt == maxUpdateAttempts {
			return nil, fmt.Errorf("%w: source %s keeps changing; try again", ErrConflict, r.Slug)
		}
	}
	s.changed(ctx, id)
	if due {
		s.pokePoller()
	}
	return s.GetSource(ctx, id)
}

// fetchKey is what a source's versions are built from: its branch, pinned
// commit and settings (configHash).
func fetchKey(r *sourceRow) string {
	return r.Branch + "\n" + r.PinnedSHA + "\n" + r.configHash()
}

// applyUpdate applies an update to r as read and validates the result. It
// reports whether what is fetched changed (the next check is due at once).
func (s *Service) applyUpdate(r *sourceRow, up SourceUpdate) (bool, error) {
	if err := refuseBuiltinUpdate(r, up); err != nil {
		return false, err
	}
	due := false
	set := func(dst *string, v *string) {
		if v != nil && strings.TrimSpace(*v) != *dst {
			*dst = strings.TrimSpace(*v)
			due = true
		}
	}
	set(&r.Branch, up.Branch)
	if up.Path != nil {
		p, err := cleanFolder(*up.Path)
		if err != nil {
			return false, err
		}
		due = due || p != r.Path
		r.Path = p
	}
	if up.Exclude != nil {
		ex, err := validateGlobs(*up.Exclude)
		if err != nil {
			return false, err
		}
		r.Exclude, due = ex, true
	}
	if up.Mode != nil && *up.Mode != r.Mode {
		r.Mode, due = *up.Mode, true
	}
	if up.PinnedSHA != nil {
		pin := strings.ToLower(strings.TrimSpace(*up.PinnedSHA))
		due = due || pin != r.PinnedSHA
		r.PinnedSHA = pin
	}
	if up.Limits != nil {
		r.Limits, due = *up.Limits, true
	}
	if err := s.updateCredential(r, up, &due); err != nil {
		return false, err
	}
	if up.PollSeconds != nil {
		r.PollSeconds = *up.PollSeconds
	}
	if up.Enabled != nil {
		due = due || (*up.Enabled && !r.Enabled)
		r.Enabled = *up.Enabled
	}
	if up.Visibility != nil {
		r.Visibility = *up.Visibility
	}
	if up.Members != nil {
		r.Members = *up.Members
	}
	if up.Blocked != nil {
		r.Blocked = *up.Blocked
	}
	if up.Hidden != nil {
		r.Hidden = *up.Hidden
	}
	if up.DefaultRuns != nil {
		r.DefaultRuns = *up.DefaultRuns
	}
	if r.Branch == "" {
		return false, fmt.Errorf("%w: a branch is required", ErrInvalid)
	}
	if err := normalizeSource(r); err != nil {
		return false, err
	}
	return due, nil
}

// updateCredential applies an update's auth and token fields.
func (s *Service) updateCredential(r *sourceRow, up SourceUpdate, due *bool) error {
	if up.Auth != nil && *up.Auth != r.Auth {
		switch *up.Auth {
		case AuthNone, AuthToken, AuthGitHub:
		default:
			return fmt.Errorf("%w: auth %q: use none, token or github", ErrInvalid, *up.Auth)
		}
		r.Auth, *due = *up.Auth, true
	}
	if up.ClearToken {
		r.tokenSealed, *due = "", true
	}
	if up.Token != nil && strings.TrimSpace(*up.Token) != "" {
		sealed, err := s.seal(strings.TrimSpace(*up.Token), tokenAAD(r.ID))
		if err != nil {
			return err
		}
		r.tokenSealed, *due = sealed, true
		if up.Auth == nil && r.Auth == AuthNone {
			r.Auth = AuthToken
		}
	}
	if r.Auth == AuthToken && r.tokenSealed == "" {
		return fmt.Errorf("%w: auth token needs a token", ErrInvalid)
	}
	if r.Auth == AuthGitHub && s.tokenSource() == nil {
		return fmt.Errorf("%w: reading with a GitHub connection is not available on this server", ErrInvalid)
	}
	return nil
}

// DeleteSource deletes a source with its versions, usage and overrides.
// File contents no other version uses are purged.
func (s *Service) DeleteSource(ctx context.Context, by, id string) error {
	if err := refuseBuiltin(id, "it cannot be deleted (hide it from everyone instead)"); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("skillhub: delete source: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	// The source row goes first: a sync storing a version holds it (FOR
	// UPDATE), so its version is committed before the rest is deleted, or
	// it finds the source gone.
	res, err := tx.ExecContext(ctx, `DELETE FROM skill_sources WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("skillhub: delete source: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: source %s", ErrNotFound, id)
	}
	for _, q := range []string{
		`DELETE FROM skill_files WHERE version_id IN (SELECT id FROM skill_versions WHERE source_id = ?)`,
		`DELETE FROM skill_entries WHERE version_id IN (SELECT id FROM skill_versions WHERE source_id = ?)`,
		`DELETE FROM skill_versions WHERE source_id = ?`,
		`DELETE FROM skill_usage WHERE source_id = ?`,
		`DELETE FROM skill_runs_overrides WHERE source_id = ?`,
		`DELETE FROM skill_sharing WHERE source_id = ?`,
	} {
		if _, err := tx.ExecContext(ctx, q, id); err != nil {
			return fmt.Errorf("skillhub: delete source: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("skillhub: delete source: %w", err)
	}
	s.log.Info("skillhub: source deleted", "source", id, "by", by)
	s.changed(ctx, id)
	if err := s.purgeBlobs(ctx); err != nil {
		s.log.Warn("skillhub: purge after a delete failed", "err", err.Error())
	}
	return nil
}

// GetSource returns one source.
func (s *Service) GetSource(ctx context.Context, id string) (*Source, error) {
	r, err := s.getSourceRow(ctx, s.db, id)
	if err != nil {
		return nil, err
	}
	src := r.Source
	return &src, nil
}

// ListSources returns every source, by slug.
func (s *Service) ListSources(ctx context.Context) ([]Source, error) {
	rows, err := s.listSourceRows(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Source, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Source)
	}
	return out, nil
}

// Versions lists a source's versions, newest first.
func (s *Service) Versions(ctx context.Context, id string, limit int) ([]Version, error) {
	if _, err := s.getSourceRow(ctx, s.db, id); err != nil {
		return nil, err
	}
	switch {
	case limit <= 0:
		limit = defaultVersions
	case limit > maxVersionsList:
		limit = maxVersionsList
	}
	return s.listVersions(ctx, id, limit)
}

// GetVersion returns one version of a source.
func (s *Service) GetVersion(ctx context.Context, id, versionID string) (*Version, error) {
	v, err := s.getVersionRow(ctx, s.db, id, versionID)
	if err != nil {
		return nil, err
	}
	return &v.Version, nil
}

// Activate makes a version active: the staged version waiting for review
// (an approval), or a retired one (a rollback). A staged version that is
// not the one waiting for review (a newer commit replaced it) is refused
// with ErrConflict. Every node serves the version within moments.
func (s *Service) Activate(ctx context.Context, by, id, versionID string) error {
	if err := refuseBuiltin(id, "it serves the skills of the running toolyard version"); err != nil {
		return err
	}
	changed, _, err := s.activate(ctx, activation{by: by, sourceID: id, versionID: versionID})
	if err != nil {
		return err
	}
	if changed {
		s.log.Info("skillhub: version activated", "source", id, "version", versionID, "by", by)
		s.changed(ctx, id)
	}
	return nil
}

// Reject marks a staged version rejected.
func (s *Service) Reject(ctx context.Context, by, id, versionID, note string) error {
	if err := refuseBuiltin(id, "it has no versions to review"); err != nil {
		return err
	}
	v, err := s.getVersionRow(ctx, s.db, id, versionID)
	if err != nil {
		return err
	}
	if v.Status != VersionStaged {
		return fmt.Errorf("%w: version %s is %s; only a staged version can be rejected", ErrConflict, versionID, v.Status)
	}
	if len(note) > 2000 {
		note = note[:2000]
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("skillhub: reject: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `UPDATE skill_versions SET status = ?, decided_by = ?, note = ? WHERE id = ? AND source_id = ? AND status = ?`,
		string(VersionRejected), by, note, versionID, id, string(VersionStaged))
	if err != nil {
		return fmt.Errorf("skillhub: reject: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: version %s is no longer staged", ErrConflict, versionID)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE skill_sources SET pending_version_id = '' WHERE id = ? AND pending_version_id = ?`, id, versionID); err != nil {
		return fmt.Errorf("skillhub: reject: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("skillhub: reject: %w", err)
	}
	s.log.Info("skillhub: version rejected", "source", id, "version", versionID, "by", by)
	return nil
}

// Skills lists the skills of a source's active version with where they
// run and their reads in the last 30 days.
func (s *Service) Skills(ctx context.Context, id string) ([]SkillUsage, error) {
	r, err := s.getSourceRow(ctx, s.db, id)
	if err != nil {
		return nil, err
	}
	out := []SkillUsage{}
	if r.ActiveVersionID == "" {
		return out, nil
	}
	if err := s.flushUsage(ctx); err != nil {
		s.log.Warn("skillhub: writing read counts failed", "err", err.Error())
	}
	entries, err := s.versionEntries(ctx, r.ActiveVersionID)
	if err != nil {
		return nil, err
	}
	overrides, err := s.runsOverrides(ctx, id)
	if err != nil {
		return nil, err
	}
	shares, err := s.skillShares(ctx, id)
	if err != nil {
		return nil, err
	}
	reads, err := s.reads30d(ctx, id)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		u := SkillUsage{
			Path: e.path, Name: e.name, Runs: effectiveRuns(overrides[e.path], Runs(e.runs), r.DefaultRuns),
			RunsOverride: overrides[e.path], Reads30d: reads[e.path],
		}
		if sh, ok := shares[e.path]; ok {
			u.Sharing = &sh
		}
		out = append(out, u)
	}
	return out, nil
}

// SetRuns overrides where one skill of the active version runs; "" clears
// the override.
func (s *Service) SetRuns(ctx context.Context, by, id, skillPath string, runs Runs) error {
	r, err := s.getSourceRow(ctx, s.db, id)
	if err != nil {
		return err
	}
	skillPath = strings.Trim(strings.TrimSpace(skillPath), "/")
	switch runs {
	case "", RunsAnywhere, RunsHost:
	default:
		return fmt.Errorf("%w: runs %q: use anywhere, host or empty", ErrInvalid, runs)
	}
	if runs != "" {
		var n int64
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM skill_entries WHERE version_id = ? AND path = ?`,
			r.ActiveVersionID, skillPath).Scan(&n); err != nil {
			return fmt.Errorf("skillhub: set runs: %w", err)
		}
		if n == 0 {
			return fmt.Errorf("%w: source %s serves no skill at %q", ErrNotFound, r.Slug, skillPath)
		}
	}
	now := s.now().UnixMilli()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("skillhub: set runs: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if runs == "" {
		_, err = tx.ExecContext(ctx, `DELETE FROM skill_runs_overrides WHERE source_id = ? AND skill_path = ?`, id, skillPath)
	} else {
		_, err = tx.ExecContext(ctx, `INSERT INTO skill_runs_overrides (source_id, skill_path, runs, set_by, updated_at)
			VALUES (?, ?, ?, ?, ?) ON CONFLICT (source_id, skill_path) DO UPDATE SET runs = excluded.runs,
			set_by = excluded.set_by, updated_at = excluded.updated_at`, id, skillPath, string(runs), by, now)
	}
	if err != nil {
		return fmt.Errorf("skillhub: set runs: %w", err)
	}
	// The source's updated_at tells every node's index to reload it; it
	// always moves forward, even past a write made after r was read.
	if _, err := tx.ExecContext(ctx, `UPDATE skill_sources SET updated_at = `+bumpUpdatedAt+` WHERE id = ?`, now, now, id); err != nil {
		return fmt.Errorf("skillhub: set runs: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("skillhub: set runs: %w", err)
	}
	s.changed(ctx, id)
	return nil
}

// RotateWebhookSecret makes a new webhook secret, stores it sealed and
// returns it once.
func (s *Service) RotateWebhookSecret(ctx context.Context, by, id string) (string, error) {
	if err := refuseBuiltin(id, "it has no repository and takes no webhook"); err != nil {
		return "", err
	}
	if _, err := s.getSourceRow(ctx, s.db, id); err != nil {
		return "", err
	}
	var b [webhookSecretLen]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("skillhub: make secret: %w", err)
	}
	secret := hex.EncodeToString(b[:])
	sealed, err := s.seal(secret, webhookAAD(id))
	if err != nil {
		return "", err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE skill_sources SET webhook_secret_sealed = ? WHERE id = ?`, sealed, id); err != nil {
		return "", fmt.Errorf("skillhub: store webhook secret: %w", err)
	}
	s.log.Info("skillhub: webhook secret rotated", "source", id, "by", by)
	return secret, nil
}

// HandleWebhook takes a GitHub webhook delivery for a source. The
// X-Hub-Signature-256 header must be the HMAC-SHA256 of the body with the
// source's secret. "ping" is accepted; a "push" to the source's branch
// starts a sync on this node (in the background: GitHub waits 10 seconds
// at most). Other events are ignored.
func (s *Service) HandleWebhook(ctx context.Context, id, event string, body []byte, signature string) error {
	if len(body) > maxWebhookBody {
		return fmt.Errorf("%w: webhook body too large", ErrInvalid)
	}
	r, err := s.getSourceRow(ctx, s.db, id)
	if err != nil {
		return err
	}
	if r.webhookSealed == "" || s.cipher == nil {
		return ErrBadSignature
	}
	secret, err := s.cipher.Open(r.webhookSealed, webhookAAD(id))
	if err != nil || !validSignature(secret, body, signature) {
		return ErrBadSignature
	}
	switch event {
	case "ping":
		return nil
	case "push":
	default:
		return nil
	}
	var push struct {
		Ref     string `json:"ref"`
		After   string `json:"after"`
		Deleted bool   `json:"deleted"`
	}
	if err := json.Unmarshal(body, &push); err != nil {
		return fmt.Errorf("%w: the push payload is not JSON", ErrInvalid)
	}
	after := strings.ToLower(strings.TrimSpace(push.After))
	if push.Ref != "refs/heads/"+r.Branch || push.Deleted || !r.Enabled || r.Mode == ModePinned ||
		strings.Trim(after, "0") == "" {
		// Another branch, a deleted branch (after is all zeros), a paused
		// source, or a pinned one.
		return nil
	}
	req := syncRequest{tr: triggerWebhook}
	if shaRe.MatchString(after) {
		// The signature proves the body: serve the pushed commit itself,
		// without asking for the branch head (GitHub may answer that from
		// a cache up to a minute old).
		req.sha = after
	}
	s.requestSync(id, req)
	return nil
}

// validSignature checks "sha256=<hex>" against the body's HMAC-SHA256, in
// constant time.
func validSignature(secret, body []byte, signature string) bool {
	sig, ok := strings.CutPrefix(strings.TrimSpace(signature), "sha256=")
	if !ok {
		return false
	}
	got, err := hex.DecodeString(sig)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}
