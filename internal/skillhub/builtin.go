package skillhub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/goroutines"
	"github.com/tusharbhardwaj/toolyard/internal/skillhub/builtin"
	"github.com/tusharbhardwaj/toolyard/internal/store"
)

// The built-in source: the toolyard skills compiled into the binary
// (package builtin), served as the read-only source "toolyard" while the
// hub is on (Options.BuiltinSkills). Its versions go through the same
// discovery, validation and version pipeline as a GitHub source's (file
// checks, discover, insertVersion, activate, retain), so digests, search,
// grep, listings, the MCP Skills extension, bundles, the per-node index and
// the cluster messages work unchanged; it never calls GitHub.
//
// The version's "commit" is builtin.Digest of the embedded tree. At start
// each node compares the active version's commit with its own tree; when
// they differ it takes the source's sync lock, looks again, and makes its
// tree live: an existing version of that tree (a downgrade, or a replica
// that won the race) or a new one. Two replicas starting together
// therefore make one version, and while replicas of two toolyard versions
// run side by side the one that started last decides (no flapping). Every
// builtinCheckEvery a node retries a start that failed and reinstalls a
// source that went missing. An older version follows the normal
// retention.
//
// Admins can change who sees the source and where its skills run (the
// sharing fields and the per-skill runs override); everything about what
// it fetches, its webhook, its versions and a delete are refused with
// ErrConflict, and agents cannot propose changes to it.

// Source kinds.
const (
	// KindGitHub is a source read from a GitHub repository.
	KindGitHub = "github"
	// KindBuiltin is the built-in source: skills compiled into toolyard.
	KindBuiltin = "builtin"
)

const (
	// BuiltinSlug is the built-in source's slug, reserved: no other source
	// may take it.
	BuiltinSlug = "toolyard"
	// BuiltinSourceID is the built-in source's fixed ID, the same on every
	// replica, so they share its sync lock before the row exists.
	BuiltinSourceID = "src_toolyard_builtin"
	// BuiltinGuideSkill is the skill the server instructions point new
	// agents at.
	BuiltinGuideSkill = "toolyard-basics"

	// The built-in source's fixed repository settings. They make the
	// version label "toolyard.builtin/skills@<digest12>", and they point
	// at nothing GitHub can ever serve, so a toolyard build that does not
	// know the builtin kind (a rollback, an older replica) cannot fetch
	// anything for the row either:
	//   - builtinOwner holds a dot: GitHub user and organisation names
	//     take only letters, digits and single hyphens (managed users add
	//     one underscore), never a dot, so no account can own this repository;
	//   - the mode is pinned to builtinPin, the all-zero SHA, which names
	//     no commit, so such a build never asks for a branch head;
	//   - auth none, no token, no webhook secret, and a next check in year
	//     9999 (builtinNever), so no poller finds it due.
	// builtinBranch keeps the settings valid (a source names a branch).
	// builtinRow puts these back if an older build's admin changed them.
	builtinOwner  = "toolyard.builtin"
	builtinRepo   = "skills"
	builtinBranch = "builtin"
	builtinPin    = "0000000000000000000000000000000000000000"
	// builtinCreator is the source's created_by: toolyard itself, not a
	// user (change proposals are refused for the source).
	builtinCreator = "toolyard"

	goBuiltin = "skillhub-builtin"
	// builtinCheckEvery is how often a node compares the live built-in
	// version with its own tree (a replica of another toolyard version may
	// have made its own live).
	builtinCheckEvery = 5 * time.Minute
	// builtinRetry is the first wait after a failed install.
	builtinRetry = 15 * time.Second
	// builtinTimeout bounds one install.
	builtinTimeout = time.Minute
	// builtinNever is the next_check_at the source keeps (9999-12-31): a
	// poller of any build, this one's or an older one's, never finds it
	// due, so nothing ever reads GitHub for it.
	builtinNever int64 = 253402300799999
)

// builtinState is the Service's built-in source state.
type builtinState struct {
	on bool
	mu sync.Mutex
	// load returns the tree; nil is builtin.Files (tests replace it).
	load func() (map[string][]byte, error)
	tree *builtinTree
	// installedAt is when the version this node confirmed live (its own
	// tree, installed or found) went live; zero until then.
	installedAt time.Time
	// lastErr is the last install error logged by the background check,
	// so a lasting one is logged once.
	lastErr string
}

// builtinTree is the embedded files and their digest.
type builtinTree struct {
	files  map[string][]byte
	digest string
}

// IsBuiltin reports whether src is the built-in source.
func (src *Source) IsBuiltin() bool { return src != nil && src.Kind == KindBuiltin }

// reservedSlug reports a slug no admin or agent may give a source.
func reservedSlug(slug string) bool { return slug == BuiltinSlug }

// BuiltinGuide is the skill reference the server instructions point new
// agents at ("toolyard/toolyard-basics"), or "" when the built-in skills
// are off or this node does not serve them under their slug (another
// source held it first). Start installs them before the gateway asks.
func (s *Service) BuiltinGuide() string {
	if !s.bi.on {
		return ""
	}
	if si := s.idx.slug(BuiltinSlug); si == nil || si.src.ID != BuiltinSourceID || si.versionID == "" {
		return ""
	}
	return BuiltinSlug + "/" + BuiltinGuideSkill
}

// builtinInstalledAt is when the version this node confirmed went live.
func (s *Service) builtinInstalledAt() time.Time {
	s.bi.mu.Lock()
	defer s.bi.mu.Unlock()
	return s.bi.installedAt
}

// builtinTree returns the tree this binary serves.
func (s *Service) builtinTree() (*builtinTree, error) {
	s.bi.mu.Lock()
	defer s.bi.mu.Unlock()
	if s.bi.tree != nil {
		return s.bi.tree, nil
	}
	load := s.bi.load
	if load == nil {
		load = builtin.Files
	}
	files, err := load()
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, errors.New("skillhub: this build has no built-in skills")
	}
	s.bi.tree = &builtinTree{files: files, digest: builtin.Digest(files)}
	return s.bi.tree, nil
}

// startBuiltin installs the built-in source (when on) and starts the
// node's periodic check. A failed install is logged and retried in the
// background; the rest of the hub serves meanwhile.
func (s *Service) startBuiltin(ctx context.Context) {
	if !s.bi.on {
		return
	}
	ictx, cancel := context.WithTimeout(ctx, builtinTimeout)
	_, err := s.ensureBuiltin(ictx)
	cancel()
	if err != nil && ctx.Err() == nil {
		s.bi.lastErr = cleanMessage(err.Error())
		s.log.Warn("skillhub: installing the built-in skills failed; retrying in the background", "err", s.bi.lastErr)
	}
	goroutines.Go(goBuiltin, func() { s.runBuiltin(ctx, err != nil) })
}

// runBuiltin checks the built-in source every builtinCheckEvery (sooner
// after a failure) until ctx ends.
func (s *Service) runBuiltin(ctx context.Context, failed bool) {
	wait := builtinCheckEvery
	if failed {
		wait = builtinRetry
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		cctx, cancel := context.WithTimeout(ctx, builtinTimeout)
		err := s.checkBuiltin(cctx)
		cancel()
		if ctx.Err() != nil {
			return
		}
		switch {
		case err == nil:
			if s.bi.lastErr != "" {
				s.log.Info("skillhub: the built-in skills are installed")
			}
			s.bi.lastErr = ""
			wait = builtinCheckEvery
		default:
			if msg := cleanMessage(err.Error()); msg != s.bi.lastErr {
				s.bi.lastErr = msg
				s.log.Warn("skillhub: installing the built-in skills failed; retrying", "err", msg)
			}
			wait = min(max(wait*2, builtinRetry), builtinCheckEvery)
		}
		t.Reset(wait)
	}
}

// checkBuiltin installs this node's tree when the source does not serve
// it: one small read when it does.
//
// Once this node has confirmed its tree, it leaves a different live
// version alone when that went live later: a replica that started after
// this one made its own skills live, and the newest start decides.
func (s *Service) checkBuiltin(ctx context.Context) error {
	tree, err := s.builtinTree()
	if err != nil {
		return err
	}
	row, err := s.getSourceRow(ctx, s.db, BuiltinSourceID)
	switch {
	case err != nil && !isNotFound(err):
		return err
	case err == nil && row.ActiveSHA == tree.digest:
		if row.LastError != "" {
			s.recordBuiltinCheck(ctx, nil) // an old failure, healed since
		}
		return nil
	case err == nil && s.newerBuiltinLive(ctx, row):
		return nil
	}
	_, err = s.ensureBuiltin(ctx)
	return err
}

// newerBuiltinLive reports that the live built-in version went live after
// the one this node confirmed.
func (s *Service) newerBuiltinLive(ctx context.Context, row *sourceRow) bool {
	installed := s.builtinInstalledAt()
	if installed.IsZero() || row.ActiveVersionID == "" {
		return false
	}
	v, err := s.getVersionRow(ctx, s.db, row.ID, row.ActiveVersionID)
	return err == nil && v.ActivatedAt != nil && v.ActivatedAt.After(installed)
}

// ensureBuiltin makes this binary's tree the built-in source's active
// version, under the source's sync lock: it creates the source when it is
// missing, reuses a version of the same tree, or builds a new one. It is
// also "Sync now" for the built-in source.
func (s *Service) ensureBuiltin(ctx context.Context) (*SyncResult, error) {
	tree, err := s.builtinTree()
	if err != nil {
		return nil, err
	}
	lctx, unlock, err := s.cluster.LockCtx(ctx, "skillhub:sync:"+BuiltinSourceID)
	if err != nil {
		return nil, fmt.Errorf("skillhub: lock the built-in source: %w", err)
	}
	defer unlock()
	row, err := s.builtinRow(lctx)
	if err != nil {
		return nil, err
	}
	res := &SyncResult{SourceID: row.ID, SHA: tree.digest, Full: true}
	err = s.installBuiltin(lctx, row, tree, res)
	if err != nil && lctx.Err() != nil {
		// The lock was lost or the node is stopping: not the source's
		// failure, so nothing is recorded.
		res.Error = "the install was interrupted"
		return res, err
	}
	s.recordBuiltinCheck(context.WithoutCancel(lctx), err)
	if err != nil {
		res.Error = cleanMessage(err.Error())
		return res, err
	}
	if v, verr := s.getVersionRow(lctx, s.db, row.ID, res.VersionID); verr == nil && v.ActivatedAt != nil {
		s.bi.mu.Lock()
		s.bi.installedAt = *v.ActivatedAt
		s.bi.mu.Unlock()
	}
	return res, nil
}

// installBuiltin is ensureBuiltin's work; the caller holds the lock.
func (s *Service) installBuiltin(ctx context.Context, row *sourceRow, tree *builtinTree, res *SyncResult) error {
	if row.ActiveSHA == tree.digest {
		// Another replica made it live: serve it here too, in case its
		// cluster message has not arrived (one small read when it has).
		res.Unchanged, res.VersionID = true, row.ActiveVersionID
		if err := s.reloadSource(ctx, row.ID); err != nil {
			s.log.Warn("skillhub: reloading the built-in source failed", "err", err.Error())
		}
		return nil
	}
	cfg := row.configHash()
	known, err := s.findVersion(ctx, row.ID, tree.digest, cfg)
	if err != nil {
		return err
	}
	versionID := ""
	if known != nil && known.Status != VersionFailed {
		versionID = known.ID
	} else {
		b, err := buildBuiltin(tree.files, row)
		if err != nil {
			return err
		}
		diff, err := s.diffAgainstActive(ctx, row, tree.digest, b)
		if err != nil {
			return err
		}
		v, err := s.insertVersion(ctx, newVersion{src: row, sha: tree.digest, config: cfg, b: b, full: true, diff: diff})
		if err != nil {
			return err
		}
		versionID, res.Diff = v.ID, diff
	}
	res.VersionID = versionID
	st := stateOf(row)
	changed, _, err := s.activate(ctx, activation{sourceID: row.ID, versionID: versionID, fromSync: true, allowRejected: true, expect: &st})
	if err != nil {
		return err
	}
	res.Activated = true
	if changed {
		s.log.Info("skillhub: built-in skills installed", "repo", RepoLabel(row.Owner, row.Repo, tree.digest), "version", versionID)
		s.changed(ctx, row.ID)
	}
	if err := s.retain(ctx, row.ID); err != nil {
		s.log.Warn("skillhub: retention failed", "source", row.Slug, "err", err.Error())
	}
	return nil
}

// buildBuiltin checks the embedded files like a repository's (built-in
// excludes, text only, storable paths, no secrets, the source's limits)
// and finds their skills.
func buildBuiltin(files map[string][]byte, row *sourceRow) (*built, error) {
	ex, err := newExcluder(row.Exclude)
	if err != nil {
		return nil, err
	}
	fs := newFileSet(row.Limits.effective())
	for _, p := range sortedPaths(files) {
		if !safeRepoPath(p) {
			return nil, fmt.Errorf("%w: built-in file %q", errUnsafeArchive, p)
		}
		if ex.excluded(p) {
			fs.exclude(p)
			continue
		}
		if err := fs.add(p, files[p]); err != nil {
			return nil, err
		}
	}
	return discover(fs, row.Path, row.rootName(), nil)
}

// builtinRow reads the built-in source, creating it when it is missing.
func (s *Service) builtinRow(ctx context.Context) (*sourceRow, error) {
	row, err := s.getSourceRow(ctx, s.db, BuiltinSourceID)
	switch {
	case err == nil && builtinFixed(row):
		return row, nil
	case err == nil:
		return s.fixBuiltinRow(ctx, row)
	case !isNotFound(err):
		return nil, err
	}
	r := &sourceRow{Source: Source{
		ID: BuiltinSourceID, Slug: BuiltinSlug, Kind: KindBuiltin, Owner: builtinOwner, Repo: builtinRepo, Branch: builtinBranch,
		Mode: ModePinned, PinnedSHA: builtinPin, Auth: AuthNone, PollSeconds: DefaultPollSeconds, Enabled: true,
		Visibility: VisibleEveryone, DefaultRuns: RunsAnywhere, CreatedBy: builtinCreator,
	}}
	if err := s.insertSource(ctx, r); err != nil {
		if !store.IsUniqueViolation(err) {
			return nil, fmt.Errorf("skillhub: store the built-in source: %w", err)
		}
		if row, gerr := s.getSourceRow(ctx, s.db, BuiltinSourceID); gerr == nil {
			return row, nil
		}
		return nil, fmt.Errorf("%w: another skill source already uses the slug %q, which is reserved for toolyard's built-in skills; delete that source and add its repository again under another slug",
			ErrConflict, BuiltinSlug)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE skill_sources SET next_check_at = ? WHERE id = ?`, builtinNever, BuiltinSourceID); err != nil {
		return nil, fmt.Errorf("skillhub: store the built-in source: %w", err)
	}
	return s.getSourceRow(ctx, s.db, BuiltinSourceID)
}

// builtinFixed reports whether a row holds the built-in source's fixed
// settings (sharing and runs are the admins' to change).
func builtinFixed(r *sourceRow) bool {
	return r.Kind == KindBuiltin && r.Owner == builtinOwner && r.Repo == builtinRepo && r.Branch == builtinBranch &&
		r.Path == "" && len(r.Exclude) == 0 && r.Mode == ModePinned && r.PinnedSHA == builtinPin && r.Auth == AuthNone &&
		!r.HasToken && !r.HasWebhookSecret && r.Enabled && r.PollSeconds == DefaultPollSeconds && r.Limits == (Limits{}) &&
		r.NextCheckAt != nil && r.NextCheckAt.UnixMilli() == builtinNever
}

// fixBuiltinRow puts the fixed settings back (a row made by an earlier
// build of this branch, or changed by a build that does not know the
// builtin kind), keeping who sees it and where its skills run. The
// source's updated_at moves, so every node reloads it.
func (s *Service) fixBuiltinRow(ctx context.Context, row *sourceRow) (*sourceRow, error) {
	now := max(s.now().UnixMilli(), row.UpdatedAt.UnixMilli()+1)
	if _, err := s.db.ExecContext(ctx, `UPDATE skill_sources SET kind = ?, owner = ?, repo = ?, branch = ?, path = '', exclude_json = '[]',
		mode = ?, pinned_sha = ?, auth = ?, token_sealed = '', webhook_secret_sealed = '', poll_seconds = ?, enabled = 1,
		limits_json = '{}', next_check_at = ?, updated_at = ? WHERE id = ?`,
		KindBuiltin, builtinOwner, builtinRepo, builtinBranch, string(ModePinned), builtinPin, string(AuthNone),
		int64(DefaultPollSeconds), builtinNever, now, BuiltinSourceID); err != nil {
		return nil, fmt.Errorf("skillhub: restore the built-in source's settings: %w", err)
	}
	s.log.Info("skillhub: restored the built-in source's fixed settings")
	s.changed(ctx, BuiltinSourceID)
	return s.getSourceRow(ctx, s.db, BuiltinSourceID)
}

// recordBuiltinCheck records an install's outcome on the source, for the
// dashboard. It never schedules a check: the poller leaves the built-in
// source alone (kind, and a next check that never comes).
func (s *Service) recordBuiltinCheck(ctx context.Context, cause error) {
	msg := ""
	if cause != nil {
		msg = cleanMessage(cause.Error())
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE skill_sources SET last_checked_at = ?, next_check_at = ?, last_error = ?,
		failures = 0 WHERE id = ? AND kind = ?`, s.now().UnixMilli(), builtinNever, msg, BuiltinSourceID, KindBuiltin); err != nil {
		s.log.Warn("skillhub: recording the built-in source's check failed", "err", err.Error())
	}
}

// errBuiltinReadOnly explains a refused change to the built-in source.
func errBuiltinReadOnly(what string) error {
	return fmt.Errorf("%w: the built-in toolyard source ships with toolyard: %s; admins can change only who sees it and where its skills run",
		ErrConflict, what)
}

// refuseBuiltin refuses an action on the built-in source.
func refuseBuiltin(id, what string) error {
	if id != BuiltinSourceID {
		return nil
	}
	return errBuiltinReadOnly(what)
}

// refuseBuiltinUpdate refuses an update of the built-in source that
// changes anything but its sharing and where its skills run.
func refuseBuiltinUpdate(r *sourceRow, up SourceUpdate) error {
	if !r.IsBuiltin() {
		return nil
	}
	var fields []string
	add := func(changed bool, name string) {
		if changed {
			fields = append(fields, name)
		}
	}
	add(up.Branch != nil && strings.TrimSpace(*up.Branch) != r.Branch, "branch")
	add(up.Path != nil && strings.Trim(strings.TrimSpace(*up.Path), "/") != r.Path, "folder")
	add(up.Exclude != nil && !sameJSON(nonNil(*up.Exclude), nonNil(r.Exclude)), "exclude globs")
	add(up.Mode != nil && *up.Mode != r.Mode, "update mode")
	add(up.PinnedSHA != nil && strings.TrimSpace(*up.PinnedSHA) != r.PinnedSHA, "pinned commit")
	add(up.Auth != nil && *up.Auth != r.Auth, "read access")
	add((up.Token != nil && strings.TrimSpace(*up.Token) != "") || up.ClearToken, "token")
	add(up.PollSeconds != nil && *up.PollSeconds != r.PollSeconds, "check interval")
	add(up.Limits != nil && !sameJSON(*up.Limits, r.Limits), "limits")
	add(up.Enabled != nil && *up.Enabled != r.Enabled, "paused state")
	if len(fields) == 0 {
		return nil
	}
	return errBuiltinReadOnly("its " + strings.Join(fields, ", ") + " cannot change")
}

func sameJSON(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
