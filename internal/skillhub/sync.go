package skillhub

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"sync"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/goroutines"
)

// trigger says what started a sync.
type trigger int

const (
	triggerPoll    trigger = iota // the leader's schedule
	triggerManual                 // SyncNow
	triggerWebhook                // a GitHub push
)

// syncRequest is one request to sync a source.
type syncRequest struct {
	tr trigger
	// sha is the commit a signed push webhook named: the sync serves it
	// without asking GitHub for the branch head (which GitHub may answer
	// from a cache up to a minute old).
	sha string
}

// headCacheAge is how old a branch head GitHub may serve from its cache
// for a public repository (Cache-Control max-age=60), with a margin.
const headCacheAge = 90 * time.Second

// pollConcurrency is how many due sources the leader syncs at once.
const pollConcurrency = 3

// syncOutcome is what a check learned about the branch head, recorded
// whatever the sync's result, and the source's updated_at the record
// expects (it moves with the sync's own activation; an admin's edit
// during the sync moves it too, and the record then yields to the edit).
type syncOutcome struct {
	headSHA   string
	headETag  string
	updatedAt int64
}

// errSourceChanged is a sync's answer when an admin changed what the
// source fetches, or made another version live, while the sync ran: the
// sync's version is not made live, and the next check builds again.
var errSourceChanged = errors.New("skillhub: the source changed while it was syncing")

// SyncNow checks the source at once on this node and syncs a new commit.
// It asks for the branch head without the stored ETag and asks caches not
// to answer, so a push a moment ago is seen. A sync failure is reported in
// the result's Error and also returned.
func (s *Service) SyncNow(ctx context.Context, id string) (*SyncResult, error) {
	if id == BuiltinSourceID {
		// The built-in source never reads GitHub: "Sync now" installs
		// this binary's skills if they are not live.
		return s.ensureBuiltin(ctx)
	}
	return s.syncSource(ctx, id, syncRequest{tr: triggerManual})
}

// syncSource runs one sync of a source under its cluster lock, so two
// syncs of one source never overlap on any node.
func (s *Service) syncSource(ctx context.Context, id string, req syncRequest) (*SyncResult, error) {
	lctx, unlock, err := s.cluster.LockCtx(ctx, "skillhub:sync:"+id)
	if err != nil {
		return nil, fmt.Errorf("skillhub: lock source %s: %w", id, err)
	}
	defer unlock()
	row, err := s.getSourceRow(lctx, s.db, id)
	if err != nil {
		return nil, err
	}
	if row.IsBuiltin() {
		// Never synced from GitHub (builtin.go installs it).
		return &SyncResult{SourceID: id, SHA: row.ActiveSHA, Unchanged: true}, nil
	}
	if req.tr == triggerPoll && (!row.Enabled || (row.NextCheckAt != nil && row.NextCheckAt.After(s.now()))) {
		// Another node, a webhook or "Sync now" checked it meanwhile.
		return &SyncResult{SourceID: id, SHA: row.ActiveSHA, Unchanged: true}, nil
	}
	res, out, serr := s.syncLocked(lctx, row, req)
	if res == nil {
		res = &SyncResult{SourceID: id}
	}
	if serr != nil && errors.Is(lctx.Err(), context.Canceled) {
		// The leader lease or the source's lock was lost, or the gateway
		// is stopping: that is not the source's failure, so nothing is
		// recorded and no backoff starts.
		res.Error = "the sync was interrupted"
		return res, serr
	}
	if serr != nil {
		res.Error = cleanMessage(serr.Error())
	}
	s.recordCheck(context.WithoutCancel(lctx), row, out, serr)
	if serr != nil {
		s.log.Warn("skillhub: sync failed", "source", row.Slug, "repo", row.Owner+"/"+row.Repo, "err", res.Error)
		return res, serr
	}
	if res.Activated || res.Staged {
		s.log.Info("skillhub: synced", "source", row.Slug, "repo", RepoLabel(row.Owner, row.Repo, res.SHA),
			"activated", res.Activated, "staged", res.Staged, "full", res.Full)
	}
	return res, nil
}

// syncLocked is one sync; the caller holds the source's lock.
func (s *Service) syncLocked(ctx context.Context, row *sourceRow, req syncRequest) (*SyncResult, *syncOutcome, error) {
	out := &syncOutcome{headSHA: row.headSHA, headETag: row.headETag, updatedAt: row.UpdatedAt.UnixMilli()}
	gh, err := s.clientFor(ctx, row)
	if err != nil {
		return nil, out, err
	}
	target, err := s.target(ctx, gh, row, req, out)
	if err != nil {
		return nil, out, err
	}
	res := &SyncResult{SourceID: row.ID, SHA: target}
	cfg := row.configHash()
	known, err := s.findVersion(ctx, row.ID, target, cfg)
	if err != nil {
		return nil, out, err
	}
	if known != nil {
		res.VersionID = known.ID
		return s.syncKnown(ctx, row, known, req, res, out)
	}
	b, full, err := s.build(ctx, gh, row, target)
	if err != nil {
		if isBuildFailure(err) {
			s.recordFailedVersion(context.WithoutCancel(ctx), row, target, cfg, err)
		}
		return res, out, err
	}
	diff, err := s.diffAgainstActive(ctx, row, target, b)
	if err != nil {
		return res, out, err
	}
	v, err := s.insertVersion(ctx, newVersion{src: row, sha: target, config: cfg, b: b, full: full, diff: diff})
	if err != nil {
		if isBuildFailure(err) {
			s.recordFailedVersion(context.WithoutCancel(ctx), row, target, cfg, err)
		}
		return res, out, err
	}
	res.VersionID, res.Full, res.Diff = v.ID, full, diff
	if row.Mode == ModeReview || !row.Approval.Live() {
		// Review mode waits for an admin; so does every new commit of a
		// skill contributor's source that is not approved, whatever its
		// mode: approving it makes live only the commit the approver saw
		// (approveSource), and the approval question follows the waiting
		// version (notifyReview).
		err = s.stage(ctx, row, v)
		res.Staged = err == nil
	} else {
		err = s.activateFromSync(ctx, row, v.ID, out)
		res.Activated = err == nil
	}
	if errors.Is(err, errSourceChanged) {
		// Built with settings that are no longer the source's: drop it.
		s.log.Info("skillhub: the source changed during the sync; the next check builds again", "source", row.Slug)
		s.discardVersion(context.WithoutCancel(ctx), v.ID)
		return res, out, nil
	}
	if err != nil {
		return res, out, err
	}
	if err := s.retain(ctx, row.ID); err != nil {
		s.log.Warn("skillhub: retention failed", "source", row.Slug, "err", err.Error())
	}
	return res, out, nil
}

// syncKnown handles a commit that already has a version.
func (s *Service) syncKnown(ctx context.Context, row *sourceRow, known *versionRow, req syncRequest, res *SyncResult, out *syncOutcome) (*SyncResult, *syncOutcome, error) {
	headMoved := res.SHA != row.headSHA
	activate := false
	switch {
	case known.Status == VersionActive:
	case known.Status == VersionFailed && req.tr == triggerManual:
		// "Sync now" tries a failed commit again.
		if err := s.dropVersion(ctx, known.ID); err != nil {
			return res, out, err
		}
		return s.syncLocked(ctx, row, syncRequest{tr: triggerPoll, sha: res.SHA})
	case known.Status == VersionFailed:
		res.Unchanged = true
		return res, out, errors.New(known.Error)
	case !row.Approval.Live():
		// A skill contributor's source that is not approved makes no
		// commit live, whatever its mode (as review mode: a new commit
		// waits, syncLocked). The waiting version tells the hook again,
		// so an approval question that shows another commit is asked
		// again for this one.
		if known.Status == VersionStaged && known.ID == row.PendingVersionID {
			s.notifyReview(ctx, row, known)
		}
	case row.Mode == ModePinned:
		// A pinned source serves its commit whatever happened to it.
		activate = true
	case row.Mode == ModeAuto && known.Status == VersionStaged:
		// A version staged before the source left review.
		activate = true
	case row.Mode == ModeAuto && known.Status == VersionRetired && headMoved:
		// The branch moved back to an older commit (a force-push or a
		// revert to it): serve it. While the head stays where it is, a
		// rollback by an admin holds. A head read from GitHub's cache
		// may lag the version made live a moment ago: wait for a fresh
		// one rather than flap back.
		activate = req.tr != triggerPoll || !s.activatedRecently(ctx, row)
		if !activate {
			// Do not record the possibly stale head either.
			out.headSHA, out.headETag = row.headSHA, row.headETag
		}
	case row.Mode == ModeReview && known.Status == VersionStaged && known.ID == row.PendingVersionID:
		// Still waiting for review: tell the hook again (it adds nothing
		// when the review item exists), so a hook call that failed is
		// retried.
		s.notifyReview(ctx, row, known)
	}
	if !activate {
		res.Unchanged = true
		return res, out, nil
	}
	err := s.activateFromSync(ctx, row, known.ID, out)
	switch {
	case errors.Is(err, errSourceChanged):
		res.Unchanged = true
		return res, out, nil
	case err != nil:
		return res, out, err
	}
	res.Activated = true
	return res, out, nil
}

// activatedRecently reports that the source's active version went live
// within headCacheAge.
func (s *Service) activatedRecently(ctx context.Context, row *sourceRow) bool {
	if row.ActiveVersionID == "" {
		return false
	}
	v, err := s.getVersionRow(ctx, s.db, row.ID, row.ActiveVersionID)
	if err != nil || v.ActivatedAt == nil {
		return false
	}
	return s.now().Sub(*v.ActivatedAt) < headCacheAge
}

// activateFromSync makes a version live for a sync, unless the source
// changed since the sync read it (errSourceChanged).
func (s *Service) activateFromSync(ctx context.Context, row *sourceRow, versionID string, out *syncOutcome) error {
	st := stateOf(row)
	changed, updated, err := s.activate(ctx, activation{
		sourceID: row.ID, versionID: versionID, fromSync: true,
		allowRejected: row.Mode == ModePinned, expect: &st,
	})
	if err != nil {
		return err
	}
	if changed {
		out.updatedAt = updated
		s.changed(ctx, row.ID)
	}
	return nil
}

// dropVersion deletes a version that serves nothing (a failed one).
func (s *Service) dropVersion(ctx context.Context, versionID string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM skill_versions WHERE id = ? AND status = ?`, versionID, string(VersionFailed)); err != nil {
		return fmt.Errorf("skillhub: drop failed version: %w", err)
	}
	return nil
}

// target returns the commit to serve: the pinned SHA, the commit a push
// webhook named, or the branch head. The poller asks with the stored
// ETag, so an unchanged branch costs no rate limit; "Sync now" asks fresh.
func (s *Service) target(ctx context.Context, gh *ghClient, row *sourceRow, req syncRequest, out *syncOutcome) (string, error) {
	if row.Mode == ModePinned {
		if !shaRe.MatchString(row.PinnedSHA) {
			return "", fmt.Errorf("%w: a pinned source needs a full commit SHA", ErrInvalid)
		}
		return row.PinnedSHA, nil
	}
	fresh := req.tr == triggerManual
	if req.sha != "" {
		if s.pushedCommitOK(ctx, gh, row, req.sha) {
			out.headSHA = req.sha
			return req.sha, nil
		}
		// An old or out-of-order delivery (GitHub's "Redeliver"): ask for
		// the branch head instead, past GitHub's cache.
		fresh = true
	}
	etag := ""
	if !fresh && row.headSHA != "" {
		etag = row.headETag
	}
	sha, newETag, notModified, err := gh.head(ctx, row.Owner, row.Repo, row.Branch, etag, fresh)
	if err != nil {
		return "", s.repoError(err, row)
	}
	if notModified {
		return row.headSHA, nil
	}
	out.headSHA, out.headETag = sha, newETag
	return sha, nil
}

// pushedCommitOK reports whether a push webhook's commit may be served
// without reading the branch head: it is the head this source last saw,
// or it descends from the active commit (one comparison, status ahead or
// identical). An old or out-of-order delivery names an older commit:
// served as it is, it would make a retired version live again, or rebuild
// a pruned one, until the next poll.
func (s *Service) pushedCommitOK(ctx context.Context, gh *ghClient, row *sourceRow, sha string) bool {
	switch {
	case sha == row.headSHA || sha == row.ActiveSHA:
		return true
	case row.ActiveSHA == "":
		return false
	}
	cmp, err := gh.compare(ctx, row.Owner, row.Repo, row.ActiveSHA, sha)
	if err != nil {
		s.log.Info("skillhub: could not compare a pushed commit with the active one; reading the branch head", "source", row.Slug, "err", cleanMessage(err.Error()))
		return false
	}
	return cmp.Status == "ahead" || cmp.Status == "identical"
}

// repoError explains a GitHub failure on a source's repository.
func (s *Service) repoError(err error, row *sourceRow) error {
	switch {
	case isGHStatus(err, 404) && row.Auth == AuthNone:
		return fmt.Errorf("%w (%s/%s@%s)", ErrNeedsToken, row.Owner, row.Repo, row.Branch)
	case isGHStatus(err, 404, 422):
		return fmt.Errorf("%w: %s/%s has no branch %q, or the credential cannot read it: %v", ErrNotFound, row.Owner, row.Repo, row.Branch, err)
	case isGHStatus(err, 401):
		return fmt.Errorf("%w: GitHub rejected the source's credential", ErrInvalid)
	}
	return err
}

// isBuildFailure reports a failure that belongs to the commit (a limit,
// an unsafe archive, content the database refuses), not to the network:
// it is recorded as a failed version, so the commit is not downloaded
// again until "Sync now" or a settings change.
func isBuildFailure(err error) bool {
	return errors.Is(err, ErrLimit) || errors.Is(err, errUnsafeArchive) || errors.Is(err, errContent)
}

// stage makes a version the one waiting for review, unless the source
// changed since the sync read it (errSourceChanged), and tells the review
// hook.
func (s *Service) stage(ctx context.Context, row *sourceRow, v *versionRow) error {
	if err := s.setPending(ctx, row, v.ID); err != nil {
		return err
	}
	src := row.Source
	src.PendingVersionID = v.ID
	s.notifyReview(ctx, &sourceRow{Source: src}, v)
	return nil
}

// notifyReview calls the review hook for a pending version. A failure is
// logged: the next check of the source calls it again. A contributor's
// source that waits for approval gets no review question: the approval
// question covers its waiting version (approving it makes that version
// live, approveSource), so the hook's ApprovalHook part is told instead,
// and asks that question again when it shows another commit. A rejected
// source asks nothing.
func (s *Service) notifyReview(ctx context.Context, row *sourceRow, v *versionRow) {
	h := s.reviewHook()
	if h == nil {
		return
	}
	src := row.Source
	src.PendingVersionID = v.ID
	var err error
	switch {
	case row.Approval.Live():
		err = h.PendingVersion(context.WithoutCancel(ctx), src, v.Version)
	case row.Approval == ApprovalPending:
		ah, ok := h.(ApprovalHook)
		if !ok {
			return
		}
		err = ah.PendingApprovalVersion(context.WithoutCancel(ctx), src, v.Version)
	default:
		return
	}
	if err != nil {
		s.log.Warn("skillhub: the review hook failed; it is called again at the next check", "source", row.Slug, "version", v.ID, "err", err.Error())
	}
}

// build makes a version of the commit: incrementally from the active
// version when that is possible, else from a full download.
func (s *Service) build(ctx context.Context, gh *ghClient, row *sourceRow, target string) (*built, bool, error) {
	ex, err := newExcluder(row.Exclude)
	if err != nil {
		return nil, false, err
	}
	lim := row.Limits.effective()
	if base, err := s.incrementalBase(ctx, row); err == nil && base != nil {
		b, ok, err := s.buildIncremental(ctx, gh, row, base, target, ex, lim)
		switch {
		case err != nil && isBuildFailure(err):
			return nil, false, err
		case err != nil:
			s.log.Info("skillhub: incremental update failed; downloading the commit", "source", row.Slug, "err", err.Error())
		case ok:
			return b, false, nil
		}
	}
	fs, err := s.download(ctx, gh, row, target, ex, lim)
	if err != nil {
		return nil, false, err
	}
	b, err := discover(fs, row.Path, row.rootName(), nil)
	return b, true, err
}

// download reads the commit's archive.
func (s *Service) download(ctx context.Context, gh *ghClient, row *sourceRow, sha string, ex *excluder, lim effLimits) (*fileSet, error) {
	var fs *fileSet
	err := gh.archive(ctx, row.Owner, row.Repo, sha, func(r io.Reader) error {
		var rerr error
		fs, rerr = readArchive(r, row.Path, ex, lim)
		return rerr
	})
	if err != nil {
		return nil, s.repoError(err, row)
	}
	return fs, nil
}

// incrementalBase is the active version when it was built with the
// current settings.
func (s *Service) incrementalBase(ctx context.Context, row *sourceRow) (*versionRow, error) {
	if row.ActiveVersionID == "" {
		return nil, nil
	}
	v, err := s.getVersionRow(ctx, s.db, row.ID, row.ActiveVersionID)
	if err != nil || v.config != row.configHash() {
		return nil, err
	}
	for _, w := range v.Warnings {
		if w.Code == "too_many_warnings" || w.Code == "too_many_omitted" {
			// Its list of left-out files is cut short.
			return nil, nil
		}
	}
	return v, nil
}

// buildIncremental updates the base version with the files the commits
// since it changed. It reports false (download everything) when the
// change is too large, rewrites history, adds or removes a skill, or
// touches a .gitattributes file.
//
// The archive and the contents API can differ: `git archive` honours the
// export-ignore and export-subst attributes, the contents API serves the
// raw blob. A change to .gitattributes therefore always downloads the
// archive; a changed file that is export-ignored stays served until the
// next full download.
func (s *Service) buildIncremental(ctx context.Context, gh *ghClient, row *sourceRow, base *versionRow, target string, ex *excluder, lim effLimits) (*built, bool, error) {
	cmp, err := gh.compare(ctx, row.Owner, row.Repo, base.SHA, target)
	if err != nil {
		return nil, false, err
	}
	if (cmp.Status != "ahead" && cmp.Status != "identical") || len(cmp.Files) >= maxCompareFiles {
		return nil, false, nil
	}
	type change struct{ rel, status string }
	var changes []change
	for _, f := range cmp.Files {
		if !safeRepoPath(f.Filename) || (f.PreviousFilename != "" && !safeRepoPath(f.PreviousFilename)) ||
			path.Base(f.Filename) == ".gitattributes" || path.Base(f.PreviousFilename) == ".gitattributes" {
			return nil, false, nil
		}
		if f.Status == "renamed" && f.PreviousFilename != "" {
			if rel, ok := underFolder(f.PreviousFilename, row.Path); ok && rel != "" {
				changes = append(changes, change{rel, "removed"})
			}
		}
		if rel, ok := underFolder(f.Filename, row.Path); ok && rel != "" {
			changes = append(changes, change{rel, f.Status})
		}
	}
	if len(changes) > maxIncrementalFiles {
		return nil, false, nil
	}
	fs, skillDirs, err := s.baseFileSet(ctx, base, row.Path, lim)
	if err != nil {
		return nil, false, err
	}
	for _, c := range changes {
		if c.status == "unchanged" {
			continue
		}
		if path.Base(c.rel) == skillFile && (c.status == "removed" || !skillDirs[skillDir(c.rel)] ||
			(c.status != "modified" && c.status != "changed")) {
			// A skill may appear or go away: only a full download sees
			// every file of its folder.
			return nil, false, nil
		}
		if c.status == "removed" {
			fs.remove(c.rel)
			continue
		}
		if _, inSkill := innermostSkill(c.rel, skillDirs); !inSkill {
			continue
		}
		fs.remove(c.rel)
		if ex.excluded(c.rel) {
			fs.exclude(c.rel)
			continue
		}
		ok, err := s.fetchChanged(ctx, gh, row, fs, c.rel, target)
		if err != nil || !ok {
			return nil, false, err
		}
	}
	contents, err := s.loadSkillMDs(ctx, fs)
	if err != nil {
		return nil, false, err
	}
	b, err := discover(fs, row.Path, row.rootName(), contents)
	return b, err == nil, err
}

// fetchChanged reads one changed file at the target commit into fs. It
// reports false when the file must come from a full download: it is not a
// plain file at the path asked for (the contents API answers for a
// symbolic link with its target's content and path, which toolyard never
// follows), or it is over the contents API's 1 MB inline limit but within
// the source's file limit.
func (s *Service) fetchChanged(ctx context.Context, gh *ghClient, row *sourceRow, fs *fileSet, rel, target string) (bool, error) {
	asked := repoPath(row.Path, rel)
	ct, data, err := gh.contents(ctx, row.Owner, row.Repo, asked, target)
	if err != nil {
		return false, err
	}
	switch {
	case ct.Type != "file" || ct.Path != asked:
		return false, nil
	case ct.Size > fs.lim.fileBytes:
		if err := fs.count(); err != nil {
			return false, err
		}
		fs.omit(rel, "too_large", fmt.Sprintf("omitted: %d bytes, over the %d-byte file limit", ct.Size, fs.lim.fileBytes))
		return true, nil
	case data == nil:
		return false, nil
	}
	return true, fs.add(rel, data)
}

// baseFileSet is the base version's files (contents left in the blob
// store) and its file warnings, with its skill folders.
func (s *Service) baseFileSet(ctx context.Context, base *versionRow, folder string, lim effLimits) (*fileSet, map[string]bool, error) {
	files, err := s.versionFiles(ctx, base.ID)
	if err != nil {
		return nil, nil, err
	}
	entries, err := s.versionEntries(ctx, base.ID)
	if err != nil {
		return nil, nil, err
	}
	fs := newFileSet(lim)
	for _, f := range files {
		fs.files[f.path] = &candidate{path: f.path, sha: f.sha, size: f.size, media: f.media}
		fs.bytes += f.size
	}
	fs.scanned = fs.bytes
	for _, w := range base.Warnings {
		if !fileLevelCodes[w.Code] {
			continue
		}
		rel, ok := underFolder(w.Path, folder)
		if !ok || rel == "" {
			continue
		}
		if w.Code == "excluded" {
			fs.exclude(rel)
			continue
		}
		w.Path = rel
		fs.omitted[rel] = w
	}
	fs.entries = len(fs.files) + len(fs.omitted)
	dirs := make(map[string]bool, len(entries))
	for _, e := range entries {
		dirs[e.path] = true
	}
	return fs, dirs, nil
}

// loadSkillMDs reads the stored SKILL.md files a file set points at.
func (s *Service) loadSkillMDs(ctx context.Context, fs *fileSet) (map[string][]byte, error) {
	var shas []string
	for p, c := range fs.files {
		if c.data == nil && path.Base(p) == skillFile {
			shas = append(shas, c.sha)
		}
	}
	return s.loadBlobs(ctx, shas)
}

// diffAgainstActive compares a built version with the active one.
func (s *Service) diffAgainstActive(ctx context.Context, row *sourceRow, sha string, b *built) (*Diff, error) {
	d := &Diff{ToSHA: sha, Added: []string{}, Changed: []string{}, Removed: []string{}}
	old := map[string]storedEntry{}
	if row.ActiveVersionID != "" {
		d.FromSHA = row.ActiveSHA
		entries, err := s.versionEntries(ctx, row.ActiveVersionID)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			old[e.path] = e
		}
	}
	for _, sk := range b.skills {
		o, ok := old[sk.path]
		switch {
		case !ok:
			d.Added = append(d.Added, sk.path)
		case o.skillMD != sk.skillMD.sha || o.filesDigest != sk.filesDigest:
			d.Changed = append(d.Changed, sk.path)
		}
		delete(old, sk.path)
	}
	for _, p := range sortedPaths(old) {
		d.Removed = append(d.Removed, p)
	}
	return d, nil
}

// clientFor returns a GitHub client with the source's credential.
func (s *Service) clientFor(ctx context.Context, row *sourceRow) (*ghClient, error) {
	switch row.Auth {
	case AuthToken:
		if row.tokenSealed == "" {
			return nil, fmt.Errorf("%w: the source reads with a token but has none", ErrInvalid)
		}
		if s.cipher == nil {
			return nil, errors.New("skillhub: no cipher to open the source's token")
		}
		tok, err := s.cipher.Open(row.tokenSealed, tokenAAD(row.ID))
		if err != nil {
			return nil, errors.New("skillhub: the source's stored token cannot be opened (was secrets.key replaced?)")
		}
		return s.newClient(string(tok)), nil
	case AuthGitHub:
		return s.userClient(ctx, row.CreatedBy)
	}
	if row.Approval != ApprovalNone {
		// A skill contributor's source without its own credential reads
		// anonymously: the admins' default token could read private
		// repositories the contributor cannot.
		return s.newClient(""), nil
	}
	return s.publicClient(), nil
}

// userClient reads with a user's toolyard GitHub connection.
func (s *Service) userClient(ctx context.Context, userID string) (*ghClient, error) {
	ts := s.tokenSource()
	if ts == nil {
		return nil, fmt.Errorf("%w: reading with a GitHub connection is not available on this server", ErrInvalid)
	}
	tok, err := ts(ctx, userID)
	if err != nil || tok == "" {
		return nil, fmt.Errorf("%w: the source creator's GitHub connection cannot be used; connect GitHub again or give the source a token", ErrNeedsToken)
	}
	return s.newClient(tok), nil
}

func tokenAAD(id string) []byte   { return []byte("skillhub-token|" + id) }
func webhookAAD(id string) []byte { return []byte("skillhub-webhook|" + id) }

// RunPoller checks every enabled source whose poll interval has passed,
// until ctx ends. Run it on one node at a time (a cluster leader lease).
// Every hour it also purges unreferenced blobs and read counts older than
// usageKeepDays.
func (s *Service) RunPoller(ctx context.Context) {
	t := time.NewTicker(pollTick)
	defer t.Stop()
	var lastPurge time.Time
	for {
		s.pollDue(ctx)
		if s.now().Sub(lastPurge) >= purgeEvery {
			s.housekeeping(ctx)
			lastPurge = s.now()
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.wake:
		}
	}
}

// housekeeping purges what no version and no report needs any more.
func (s *Service) housekeeping(ctx context.Context) {
	if err := s.purgeBlobs(ctx); err != nil && ctx.Err() == nil {
		s.log.Warn("skillhub: purge failed", "err", err.Error())
	}
	if err := s.pruneUsage(ctx); err != nil && ctx.Err() == nil {
		s.log.Warn("skillhub: pruning read counts failed", "err", err.Error())
	}
}

// pollDue syncs the sources whose check is due, pollConcurrency at a time,
// so one slow source does not hold up the others.
func (s *Service) pollDue(ctx context.Context) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, auth, approval FROM skill_sources WHERE enabled = 1 AND kind <> ?
		AND (next_check_at IS NULL OR next_check_at <= ?) ORDER BY next_check_at LIMIT 50`, KindBuiltin, s.now().UnixMilli())
	if err != nil {
		if ctx.Err() == nil {
			s.log.Warn("skillhub: listing due sources failed", "err", err.Error())
		}
		return
	}
	// While GitHub's rate limit holds back the credential of the sources
	// without their own token (anonymous, or the default token), those
	// wait for it together; sources with their own token go on.
	publicPaused, paused := s.limits.blockedUntil(s.publicClient().key)
	// Contributors' sources without their own credential read anonymously
	// (clientFor), under the anonymous limit.
	_, anonPaused := s.limits.blockedUntil(s.newClient("").key)
	var ids []string
	skipped := 0
	for rows.Next() {
		var id, auth, approval string
		if rows.Scan(&id, &auth, &approval) != nil {
			continue
		}
		if AuthMode(auth) == AuthNone && ((Approval(approval) == ApprovalNone && paused) || (Approval(approval) != ApprovalNone && anonPaused)) {
			skipped++
			continue
		}
		ids = append(ids, id)
	}
	rows.Close()
	if skipped > 0 && !publicPaused.Equal(s.lastPublicPause) {
		s.lastPublicPause = publicPaused
		s.log.Warn("skillhub: GitHub's rate limit holds back the sources without their own token; they are checked again when it ends",
			"until", publicPaused.UTC().Format(time.RFC3339), "sources", skipped)
	}
	slots := make(chan struct{}, pollConcurrency)
	var wg sync.WaitGroup
	for _, id := range ids {
		select {
		case <-ctx.Done():
		case slots <- struct{}{}:
		}
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		goroutines.Go(goPoll, func() {
			defer func() { <-slots; wg.Done() }()
			sctx, cancel := context.WithTimeout(ctx, syncTimeout)
			defer cancel()
			_, _ = s.syncSource(sctx, id, syncRequest{tr: triggerPoll}) // logged and recorded inside
		})
	}
	wg.Wait()
}

// pendingSync is a background sync of one source that waits or runs.
type pendingSync struct {
	again bool        // run once more when this run ends
	req   syncRequest // what the next run serves
}

// requestSync starts a background sync of a source on this node. While
// one waits or runs, another request makes it run once more afterwards
// (with the newest request's commit), so the newest push is never missed
// and requests do not pile up.
func (s *Service) requestSync(id string, req syncRequest) {
	s.qmu.Lock()
	if p := s.queued[id]; p != nil {
		p.again, p.req = true, req
		s.qmu.Unlock()
		return
	}
	s.queued[id] = &pendingSync{}
	s.qmu.Unlock()
	ctx := s.baseCtx()
	s.bg.Add(1)
	goroutines.Go(goSyncAsync, func() {
		defer s.bg.Done()
		for {
			sctx, cancel := context.WithTimeout(ctx, syncTimeout)
			_, _ = s.syncSource(sctx, id, req) // logged and recorded inside
			cancel()
			s.qmu.Lock()
			if p := s.queued[id]; p.again && ctx.Err() == nil {
				p.again, req = false, p.req
				s.qmu.Unlock()
				continue
			}
			delete(s.queued, id)
			s.qmu.Unlock()
			return
		}
	})
}
