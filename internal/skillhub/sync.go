package skillhub

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
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

// syncOutcome is what a check learned about the branch head, recorded
// whatever the sync's result.
type syncOutcome struct {
	headSHA  string
	headETag string
}

// SyncNow checks the source at once on this node and syncs a new commit.
// A sync failure is reported in the result's Error and also returned.
func (s *Service) SyncNow(ctx context.Context, id string) (*SyncResult, error) {
	return s.syncSource(ctx, id, triggerManual)
}

// syncSource runs one sync of a source under its cluster lock, so two
// syncs of one source never overlap on any node.
func (s *Service) syncSource(ctx context.Context, id string, tr trigger) (*SyncResult, error) {
	lctx, unlock, err := s.cluster.LockCtx(ctx, "skillhub:sync:"+id)
	if err != nil {
		return nil, fmt.Errorf("skillhub: lock source %s: %w", id, err)
	}
	defer unlock()
	row, err := s.getSourceRow(lctx, s.db, id)
	if err != nil {
		return nil, err
	}
	if tr == triggerPoll && (!row.Enabled || (row.NextCheckAt != nil && row.NextCheckAt.After(s.now()))) {
		// Another node, a webhook or "Sync now" checked it meanwhile.
		return &SyncResult{SourceID: id, SHA: row.ActiveSHA, Unchanged: true}, nil
	}
	res, out, serr := s.syncLocked(lctx, row, tr)
	if res == nil {
		res = &SyncResult{SourceID: id}
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
func (s *Service) syncLocked(ctx context.Context, row *sourceRow, tr trigger) (*SyncResult, *syncOutcome, error) {
	gh, err := s.clientFor(ctx, row)
	if err != nil {
		return nil, nil, err
	}
	target, out, err := s.target(ctx, gh, row)
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
		return s.syncKnown(ctx, row, known, tr, res, out)
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
		return res, out, err
	}
	res.VersionID, res.Full, res.Diff = v.ID, full, diff
	if row.Mode == ModeReview {
		if err := s.stage(ctx, row, v); err != nil {
			return res, out, err
		}
		res.Staged = true
	} else {
		if _, err := s.activate(ctx, "", row.ID, v.ID, false); err != nil {
			return res, out, err
		}
		res.Activated = true
		s.changed(ctx, row.ID)
	}
	if err := s.retain(ctx, row.ID); err != nil {
		s.log.Warn("skillhub: retention failed", "source", row.Slug, "err", err.Error())
	}
	return res, out, nil
}

// syncKnown handles a commit that already has a version.
func (s *Service) syncKnown(ctx context.Context, row *sourceRow, known *versionRow, tr trigger, res *SyncResult, out *syncOutcome) (*SyncResult, *syncOutcome, error) {
	switch {
	case known.Status == VersionActive:
		res.Unchanged = true
	case known.Status == VersionFailed && tr == triggerManual:
		// "Sync now" tries a failed commit again.
		if err := s.dropVersion(ctx, known.ID); err != nil {
			return res, out, err
		}
		return s.syncLocked(ctx, row, triggerPoll)
	case known.Status == VersionFailed:
		res.Unchanged = true
		return res, out, errors.New(known.Error)
	case row.Mode == ModePinned, row.Mode == ModeAuto && known.Status == VersionStaged:
		// A pinned source serves its commit whatever happened to it; an
		// auto source serves a version staged before it left review.
		if _, err := s.activate(ctx, "", row.ID, known.ID, row.Mode == ModePinned); err != nil {
			return res, out, err
		}
		res.Activated = true
		s.changed(ctx, row.ID)
	default:
		// Staged and waiting for review, retired by a rollback, or
		// rejected: a new commit is needed to change anything.
		res.Unchanged = true
	}
	return res, out, nil
}

// dropVersion deletes a version that serves nothing (a failed one).
func (s *Service) dropVersion(ctx context.Context, versionID string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM skill_versions WHERE id = ? AND status = ?`, versionID, string(VersionFailed)); err != nil {
		return fmt.Errorf("skillhub: drop failed version: %w", err)
	}
	return nil
}

// target returns the commit to serve: the pinned SHA, or the branch head
// (asked with the stored ETag, so an unchanged branch costs no rate
// limit).
func (s *Service) target(ctx context.Context, gh *ghClient, row *sourceRow) (string, *syncOutcome, error) {
	if row.Mode == ModePinned {
		if !shaRe.MatchString(row.PinnedSHA) {
			return "", nil, fmt.Errorf("%w: a pinned source needs a full commit SHA", ErrInvalid)
		}
		return row.PinnedSHA, &syncOutcome{headSHA: row.headSHA, headETag: row.headETag}, nil
	}
	etag := row.headETag
	if row.headSHA == "" {
		etag = ""
	}
	sha, newETag, notModified, err := gh.head(ctx, row.Owner, row.Repo, row.Branch, etag)
	if err != nil {
		return "", nil, s.repoError(err, row)
	}
	if notModified {
		return row.headSHA, &syncOutcome{headSHA: row.headSHA, headETag: row.headETag}, nil
	}
	return sha, &syncOutcome{headSHA: sha, headETag: newETag}, nil
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
// an unsafe archive), not to the network: it is recorded as a failed
// version.
func isBuildFailure(err error) bool {
	return errors.Is(err, ErrLimit) || errors.Is(err, errUnsafeArchive)
}

// stage keeps a version staged for review and tells the review hook.
func (s *Service) stage(ctx context.Context, row *sourceRow, v *versionRow) error {
	if err := s.setPending(ctx, row.ID, v.ID); err != nil {
		return err
	}
	h := s.reviewHook()
	if h == nil {
		return nil
	}
	src := row.Source
	src.PendingVersionID = v.ID
	if err := h.PendingVersion(context.WithoutCancel(ctx), src, v.Version); err != nil {
		s.log.Warn("skillhub: the review hook failed; the version stays staged", "source", row.Slug, "version", v.ID, "err", err.Error())
	}
	return nil
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
	return v, nil
}

// buildIncremental updates the base version with the files the commits
// since it changed. It reports false (download everything) when the
// change is too large, rewrites history, or adds or removes a skill.
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
		if !safeRepoPath(f.Filename) || (f.PreviousFilename != "" && !safeRepoPath(f.PreviousFilename)) {
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
// reports false when the file must come from a full download (over the
// contents API's 1 MB inline limit but within the source's file limit).
func (s *Service) fetchChanged(ctx context.Context, gh *ghClient, row *sourceRow, fs *fileSet, rel, target string) (bool, error) {
	ct, data, err := gh.contents(ctx, row.Owner, row.Repo, repoPath(row.Path, rel), target)
	if err != nil {
		return false, err
	}
	switch {
	case ct.Type == "symlink":
		fs.omit(rel, "symlink", "omitted: links are not followed")
		return true, nil
	case ct.Type != "file":
		fs.omit(rel, "special", "omitted: not a regular file")
		return true, nil
	case ct.Size > fs.lim.fileBytes:
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
	return s.newClient(""), nil
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
// It also purges unreferenced blobs every hour.
func (s *Service) RunPoller(ctx context.Context) {
	t := time.NewTicker(pollTick)
	defer t.Stop()
	var lastPurge time.Time
	for {
		s.pollDue(ctx)
		if s.now().Sub(lastPurge) >= purgeEvery {
			if err := s.purgeBlobs(ctx); err != nil && ctx.Err() == nil {
				s.log.Warn("skillhub: purge failed", "err", err.Error())
			}
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

// pollDue syncs, one at a time, the sources whose check is due.
func (s *Service) pollDue(ctx context.Context) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM skill_sources WHERE enabled = 1
		AND (next_check_at IS NULL OR next_check_at <= ?) ORDER BY next_check_at LIMIT 50`, s.now().UnixMilli())
	if err != nil {
		if ctx.Err() == nil {
			s.log.Warn("skillhub: listing due sources failed", "err", err.Error())
		}
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		sctx, cancel := context.WithTimeout(ctx, syncTimeout)
		_, _ = s.syncSource(sctx, id, triggerPoll) // logged and recorded inside
		cancel()
	}
}

// requestSync starts a background sync of a source on this node. While
// one waits or runs, another request makes it run once more afterwards,
// so the newest push is never missed and requests do not pile up.
func (s *Service) requestSync(id string, tr trigger) {
	s.qmu.Lock()
	if s.queued[id] > 0 {
		s.queued[id] = 2
		s.qmu.Unlock()
		return
	}
	s.queued[id] = 1
	s.qmu.Unlock()
	ctx := s.baseCtx()
	s.bg.Add(1)
	goroutines.Go(goSyncAsync, func() {
		defer s.bg.Done()
		for {
			sctx, cancel := context.WithTimeout(ctx, syncTimeout)
			_, _ = s.syncSource(sctx, id, tr) // logged and recorded inside
			cancel()
			s.qmu.Lock()
			if s.queued[id] == 2 && ctx.Err() == nil {
				s.queued[id] = 1
				s.qmu.Unlock()
				continue
			}
			delete(s.queued, id)
			s.qmu.Unlock()
			return
		}
	})
}
